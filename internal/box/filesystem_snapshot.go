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

// captureFilesystemSnapshot uses the existing decider and walks even hidden
// directories: an already-existing hardlink outside a hidden directory must
// inherit its protection. A later trusted-host reclassification needs admission.
func captureFilesystemSnapshot(root string, profile fsfilter.Profile, grants []string, retained map[fileIdentity]uint32, exclude string) (*filesystemSnapshot, error) {
	s := &filesystemSnapshot{root: root, policy: fsfilter.Policy{Profile: profile, Grants: slices.Clone(grants)}, objects: map[string]filesystemObject{}, protected: maps.Clone(retained), origins: rememberedHidden{}}
	before := loadHiddenFiles(root)
	decide := shadowpath.NewDecider(root)
	if s.protected == nil {
		s.protected = map[fileIdentity]uint32{}
	}
	flags := map[string]uint32{}
	// Discovery has the mount walk's pruning semantics, but inventory still
	// visits hidden descendants and .git to protect their external hardlinks.
	suppressed := map[string]bool{}
	var rootDevice uint64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == exclude {
			return fs.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		id, ok := identityOf(info)
		if !ok {
			return errors.New("filesystem object identity unavailable")
		}
		if rel == "." {
			if !d.IsDir() {
				return fmt.Errorf("filesystem admission root %s is not a real directory", root)
			}
			rootDevice = id.dev
		}
		if id.dev != rootDevice {
			return errors.New("filesystem admission cannot cross a mounted filesystem")
		}
		if len(s.objects) >= 1_000_000 {
			return errors.New("filesystem inventory exceeds admission bounds")
		}
		s.objects[rel] = filesystemObject{id: id, mode: info.Mode().Type()}
		if rel != "." {
			// WalkDir visits parents first; inherited directory protection therefore
			// includes every descendant without reinterpreting an allow pattern.
			flags[rel] |= flags[path.Dir(rel)] & fsfilter.Secret
			skip := suppressed[path.Dir(rel)] || d.IsDir() && d.Name() == ".git"
			origin, remembered := before[id]
			moved := remembered && origin != rel
			if !skip {
				switch {
				case d.Type().IsRegular() && isCoopignore(p):
					flags[rel] |= fsfilter.ReadOnly
				default:
					hidden, err := decide(rel)
					if err != nil {
						return err
					}
					if hidden {
						flags[rel] |= fsfilter.Secret
					}
				}
			}
			// Mount discovery skips .git and hidden descendants, but a remembered
			// object remains secret at an existing alias in either location.
			if moved {
				flags[rel] |= fsfilter.Secret
			}
			if d.IsDir() {
				suppressed[rel] = skip || flags[rel]&fsfilter.Secret != 0
			}
		}
		if f := flags[rel]; f != 0 {
			s.protected[id] |= f
			if f&fsfilter.Secret != 0 && s.origins[id] == "" {
				s.origins[id] = rel
				if earlier, ok := before[id]; ok {
					s.origins[id] = earlier
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for rel, object := range s.objects {
		flags[rel] |= s.protected[object.id]
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

// Revalidate before the first workload instruction. Public content edits are
// irrelevant, but root/ancestor replacement, policy changes, new secrets and
// changing alias or grant identities require a new admission.
func (s *filesystemSnapshot) revalidate(exclude string) error {
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
