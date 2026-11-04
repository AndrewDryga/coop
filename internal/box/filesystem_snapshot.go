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

func captureRememberedFilesystemSnapshot(ctx context.Context, root string, profile fsfilter.Profile, grants []string) (snapshot *filesystemSnapshot, err error) {
	err = withHiddenFilesLock(ctx, root, func() error {
		var captureErr error
		snapshot, captureErr = captureFilesystemSnapshot(root, profile, grants, nil)
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
func captureFilesystemSnapshot(root string, profile fsfilter.Profile, grants []string, retained map[fileIdentity]uint32) (*filesystemSnapshot, error) {
	s := &filesystemSnapshot{root: root, policy: fsfilter.Policy{Profile: profile, Grants: slices.Clone(grants)}, objects: map[string]filesystemObject{}, protected: maps.Clone(retained), origins: rememberedHidden{}}
	before := loadHiddenFiles(root)
	if s.protected == nil {
		s.protected = map[fileIdentity]uint32{}
	}
	inventory, err := filesystemObjects(root)
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
			parent, name := ".", rel
			if i := strings.LastIndexByte(rel, '/'); i >= 0 {
				parent, name = rel[:i], rel[i+1:]
			}
			f = flags[parent] & fsfilter.Secret
			skip := suppressed[parent] || directory && name == ".git"
			origin, remembered := before[id]
			moved := remembered && origin != rel
			if !skip {
				switch {
				case object.mode.IsRegular() && IsCoopignoreName(name) && isCoopignore(filepath.Join(root, rel)):
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
func (s *filesystemSnapshot) revalidate() error {
	// Only the qualified APFS backend permits this optimization. In particular,
	// Linux leaf bind mounts need a device check on every leaf.
	if !filesystemProfileCandidate(s.root) || !maps.Equal(loadHiddenFiles(s.root), s.origins) {
		return s.revalidateFull()
	}
	inventory, err := inventoryFilesystem(s.root, false)
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
		return s.revalidateFull()
	}
	return nil
}

func (s *filesystemSnapshot) revalidateFull() error {
	next, err := captureFilesystemSnapshot(s.root, s.policy.Profile, s.policy.Grants, s.protected)
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

// maxHiddenFileCovers bounds the empty files one eBPF launch adds: about 0.2 s of start time, measured
// on a Mac. Past it, or past the command line's budget, a hidden file is still denied by eBPF, which is
// the protection. A cover only lets a tool that opens the file see it empty.
const maxHiddenFileCovers = 1000

// withHiddenFileCovers adds an empty read-only file over each hidden file an admission captured, to the
// binds it was given. eBPF stays the boundary, so one mount per real name is enough: it already denies
// other spellings, hidden directories and files created later. Each root is mounted at the bind whose
// source it is; a root with no bind has nothing to cover.
func (a *filesystemAdmission) withHiddenFileCovers(mounts []Mount) []Mount {
	targets := map[string]string{}
	for _, m := range mounts {
		if m.Kind == Bind {
			targets[m.Source] = m.Target
		}
	}
	covers, bytes := 0, 0
	for _, snapshot := range a.snapshots {
		target, bound := targets[snapshot.root]
		if !bound {
			continue
		}
		secret := map[string]bool{}
		for _, entry := range snapshot.policy.Entries { // sorted with parents first
			if entry.Flags&fsfilter.Secret == 0 {
				continue
			}
			secret[entry.Path] = true
			object, known := snapshot.objects[entry.Path]
			if !known || !object.mode.IsRegular() || secret[path.Dir(entry.Path)] {
				continue // a hidden directory stays denied, with everything inside it
			}
			cover := path.Join(target, entry.Path)
			bytes += len(cover) + coverOverhead
			if covers >= maxHiddenFileCovers || bytes > maxCoverBytes {
				return mounts
			}
			mounts = append(mounts, Mount{Kind: Decoy, Target: cover, RO: true})
			covers++
		}
	}
	return mounts
}
