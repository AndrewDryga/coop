package box

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/fsfilter"
	"github.com/AndrewDryga/coop/internal/shadowpath"
)

func captureRememberedFilesystemSnapshot(ctx context.Context, root string, profile fsfilter.Profile, grants []string, exclude string) (snapshot *filesystemSnapshot, err error) {
	err = withHiddenFilesLock(ctx, root, func() error {
		var captureErr error
		snapshot, captureErr = captureFilesystemSnapshot(root, profile, grants, nil, exclude)
		if captureErr != nil {
			return captureErr
		}
		return snapshot.remember()
	})
	return snapshot, err
}

func (s *filesystemSnapshot) protectedPaths() []string {
	wanted := map[string]bool{".": true}
	paths := slices.Clone(s.policy.Grants)
	for _, entry := range s.policy.Entries {
		paths = append(paths, entry.Path)
	}
	for _, name := range paths {
		for p := name; p != "."; p = path.Dir(p) {
			wanted[p] = true
		}
	}
	return sortedKeys(wanted)
}

func (s *filesystemSnapshot) digest() (string, error) {
	type object struct {
		Path     string
		Dev, Ino uint64
		Mode     fs.FileMode
	}
	var objects []object
	for _, name := range s.protectedPaths() {
		value := s.objects[name]
		objects = append(objects, object{name, value.id.dev, value.id.ino, value.mode})
	}
	data, err := json.Marshal(struct {
		Root    string
		Policy  fsfilter.Policy
		Objects []object
	}{s.root, s.policy, objects})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// The host inventory only groups aliases on the host. Guest inode keys are
// obtained separately from the loader's held descriptors, never translated.
type filesystemObject struct {
	id   fileIdentity
	mode fs.FileMode
}

type filesystemSnapshot struct {
	root      string
	policy    fsfilter.Policy
	objects   map[string]filesystemObject
	protected map[fileIdentity]uint32
	origins   rememberedHidden
}

// Count a hidden directory once, as the mount backend does, while including
// protected aliases outside it. Entries are sorted with parents before children.
func (s *filesystemSnapshot) secretCount() int {
	secret := map[string]bool{}
	count := 0
	for _, entry := range s.policy.Entries {
		if entry.Flags&fsfilter.Secret != 0 {
			secret[entry.Path] = true
			if !secret[path.Dir(entry.Path)] {
				count++
			}
		}
	}
	return count
}

// captureFilesystemSnapshot uses the existing decider and walks even hidden
// directories: an already-existing hardlink outside a hidden directory must
// inherit its protection. A later trusted-host reclassification needs admission.
func captureFilesystemSnapshot(root string, profile fsfilter.Profile, grants []string, retained map[fileIdentity]uint32, exclude string) (*filesystemSnapshot, error) {
	s := &filesystemSnapshot{root: root, policy: fsfilter.Policy{Profile: profile, Grants: slices.Clone(grants)}, objects: map[string]filesystemObject{}, protected: maps.Clone(retained), origins: rememberedHidden{}}
	before := loadHiddenFiles(root)
	if s.protected == nil {
		s.protected = map[fileIdentity]uint32{}
	}
	inventory, err := filesystemObjects(root, exclude)
	if err != nil {
		return nil, err
	}
	s.objects = inventory.objects
	flags, err := filesystemNamedFlags(root, inventory, before)
	if err != nil {
		return nil, err
	}
	for _, rel := range inventory.paths {
		if f := flags[rel]; f != 0 {
			id := s.objects[rel].id
			s.protected[id] |= f
			if f&fsfilter.Secret != 0 && s.origins[id] == "" {
				s.origins[id] = rel
				if earlier, ok := before[id]; ok {
					s.origins[id] = earlier
				}
			}
		}
	}
	for rel, object := range s.objects {
		if f := s.protected[object.id]; f != 0 {
			flags[rel] |= f
		}
	}
	// Grants are object authority. Policy files and directories cannot acquire it,
	// including when a requested grant aliases a protected policy file.
	for _, grant := range grants {
		object, ok := s.objects[grant]
		if !ok || object.mode != 0 || flags[grant]&fsfilter.ReadOnly != 0 {
			return nil, fmt.Errorf("ineligible filesystem read grant %q", grant)
		}
	}
	for rel, f := range flags {
		if f != 0 && rel != "." {
			s.policy.Entries = append(s.policy.Entries, fsfilter.Entry{Path: rel, Flags: f})
		}
	}
	slices.SortFunc(s.policy.Entries, func(a, b fsfilter.Entry) int { return strings.Compare(a.Path, b.Path) })
	slices.Sort(s.policy.Grants)
	if _, err := s.policy.Compile(1); err != nil {
		return nil, err
	}
	return s, nil
}

// Classify names in the same order as mount discovery. Identity collection is
// separate so final validation can reuse the already-pinned object authority.
func filesystemNamedFlags(root string, inventory *filesystemInventory, before rememberedHidden) (map[string]uint32, error) {
	flags := map[string]uint32{}
	suppressed := map[string]bool{}
	decide := shadowpath.NewInventoryDecider(root, slices.Values(inventory.paths))
	for _, rel := range inventory.paths {
		object := inventory.objects[rel]
		id := object.id
		directory := object.mode.IsDir()
		var f uint32
		if rel != "." {
			// Lexical order visits parents first; inherited directory protection therefore
			// includes every descendant without reinterpreting an allow pattern.
			parent := path.Dir(rel)
			f = flags[parent] & fsfilter.Secret
			skip := suppressed[parent] || directory && path.Base(rel) == ".git"
			origin, remembered := before[id]
			moved := remembered && origin != rel
			if !skip {
				switch {
				case object.mode.IsRegular() && IsCoopignoreName(path.Base(rel)) && isCoopignore(filepath.Join(root, rel)):
					f |= fsfilter.ReadOnly
				default:
					hidden, err := decide(rel)
					if err != nil {
						return nil, err
					}
					if hidden {
						f |= fsfilter.Secret
					}
				}
			}
			// Mount discovery skips .git and hidden descendants, but a remembered
			// object remains secret at an existing alias in either location.
			if moved {
				f |= fsfilter.Secret
			}
			if directory {
				suppressed[rel] = skip || f&fsfilter.Secret != 0
			}
		}
		if f != 0 {
			flags[rel] = f
		}
	}
	return flags, nil
}

// Revalidate before the first workload instruction. Public content edits are
// irrelevant, but root/ancestor replacement, policy changes, new secrets and
// changing alias or grant identities require a new admission.
func (s *filesystemSnapshot) revalidate(exclude string) error {
	// Only the qualified APFS backend permits this optimization. In particular,
	// Linux leaf bind mounts need a device check on every leaf.
	if !filesystemProfileCandidate(s.root) || !maps.Equal(loadHiddenFiles(s.root), s.origins) {
		return s.revalidateFull(exclude)
	}
	inventory, err := inventoryFilesystem(s.root, exclude, false)
	if err != nil {
		return err
	}
	for _, rel := range s.protectedPaths() {
		info, err := os.Lstat(filepath.Join(s.root, rel))
		if err != nil {
			return err
		}
		id, ok := identityOf(info)
		object := filesystemObject{id: id, mode: info.Mode().Type()}
		listed, present := inventory.objects[rel]
		if !ok || !present || object != s.objects[rel] || listed.mode != object.mode {
			return fmt.Errorf("protected filesystem object %q changed during admission; retry the launch", rel)
		}
		inventory.objects[rel] = object
	}
	flags, err := filesystemNamedFlags(s.root, inventory, s.origins)
	if err != nil {
		return err
	}
	expected := make(map[string]uint32, len(s.policy.Entries))
	for _, entry := range s.policy.Entries {
		expected[entry.Path] = entry.Flags
	}
	for rel, flag := range flags {
		if flag&^expected[rel] != 0 {
			return errors.New("protected filesystem paths changed during admission; retry the launch")
		}
	}
	// Another admission can remember a newly classified inode while this walk
	// runs. Only a full identity census can find its ordinary-looking aliases.
	if !maps.Equal(loadHiddenFiles(s.root), s.origins) {
		return s.revalidateFull(exclude)
	}
	return nil
}

func (s *filesystemSnapshot) revalidateFull(exclude string) error {
	next, err := captureFilesystemSnapshot(s.root, s.policy.Profile, s.policy.Grants, s.protected, exclude)
	if err != nil {
		return err
	}
	if !slices.Equal(s.policy.Entries, next.policy.Entries) {
		return errors.New("protected filesystem paths changed during admission; retry the launch")
	}
	for _, p := range s.protectedPaths() {
		if s.objects[p] != next.objects[p] {
			return fmt.Errorf("protected filesystem object %q changed during admission; retry the launch", p)
		}
	}
	return nil
}

func (s *filesystemSnapshot) remember() error {
	record := hiddenFilesRecord{Version: hiddenFilesVersion}
	seen := map[fileIdentity]bool{}
	for _, entry := range s.policy.Entries {
		if entry.Flags&fsfilter.Secret == 0 {
			continue
		}
		id := s.objects[entry.Path].id
		if seen[id] {
			continue
		}
		origin := s.origins[id]
		if origin == "" {
			return errors.New("protected filesystem object lost its policy origin")
		}
		seen[id] = true
		record.Files = append(record.Files, hiddenFile{Dev: id.dev, Ino: id.ino, Path: origin})
	}
	// Persist captured identities, not a fresh stat of paths the host may have
	// atomically replaced. An alias must retain its original policy-hidden path.
	return writeHiddenFiles(s.root, record)
}
