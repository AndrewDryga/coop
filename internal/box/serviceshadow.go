package box

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"go.yaml.in/yaml/v3"
)

// serviceDecoy is one read-only override mount. Most entries are empty decoys; bindSource marks
// a readable .coopignore overlay that secret approval must not remove.
type serviceDecoy struct {
	target     string
	dir        bool
	source     string
	bindSource string // non-empty for a readable, read-only policy bind
	writable   bool   // the original bind or its ancestor lets the service modify this source
}

// serviceCoverLimit bounds the spelling covers one service gets: each is a bind mount, and 8,192
// of them start a container in about 2.3 s on OrbStack (spellings.go).
const serviceCoverLimit = 8192

// serviceShadowPlan decides, per service, which bind targets get a decoy, and lists the
// repo-relative sources being hidden (sorted, unique) — what a human must see before approving
// the file (ReviewServiceSecrets) and what the auto-up warning names. On a disk that ignores
// letter case, a hidden file or .coopignore inside a folder bind also gets a cover for every other
// spelling of its name, as a box's does (ComputeMounts); uncovered lists those it could not cover.
func serviceShadowPlan(repo, composeFile string, data []byte) (decoys map[string][]serviceDecoy, hiddenPaths, uncovered []string, err error) {
	var doc composeDoc
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&doc); err != nil {
		return nil, nil, nil, err
	}
	realRepo, err := resolveExisting(repo)
	if err != nil {
		return nil, nil, nil, err
	}
	composeDir := filepath.Dir(composeFile)
	shadowed := NewShadowDecider(realRepo)
	decoys = map[string][]serviceDecoy{}
	hidden := map[string]bool{}
	missed := map[string]bool{}
	names := make([]string, 0, len(doc.Services))
	for name := range doc.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		covers := 0
		// spelled adds decoy and a copy of it at every other spelling of its name. A folder needs
		// none: Linux keeps one entry per folder, so CONFIG/ lands on config/'s cover.
		spelled := func(decoy serviceDecoy, p string) {
			decoys[name] = append(decoys[name], decoy)
			if !foldsCase(p) {
				return
			}
			spellings, ok := otherSpellings(filepath.Base(p))
			if !ok || covers+len(spellings) > serviceCoverLimit {
				missed[decoy.source] = true
				return
			}
			covers += len(spellings)
			for _, spelling := range spellings {
				cover := decoy
				cover.target = path.Join(path.Dir(decoy.target), spelling)
				decoys[name] = append(decoys[name], cover)
			}
		}
		for _, entry := range doc.Services[name].Volumes {
			source, target := bindSourceTarget(entry)
			if target == "" || !serviceVolumeIsBind(entry, source) {
				continue
			}
			abs := source
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(composeDir, source)
			}
			real, err := resolveExisting(abs)
			if err != nil {
				return nil, nil, nil, err
			}
			rel, err := filepath.Rel(realRepo, real)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue // validated binds never leave the repo; a stray one is not ours to project
			}
			info, err := os.Stat(real)
			if err != nil {
				continue // a source that does not exist yet has nothing to hide
			}
			_, writable := serviceVolumeTargetAccess(entry)
			if rel != "." {
				secret, err := shadowed(filepath.ToSlash(rel))
				if err != nil {
					return nil, nil, nil, err
				}
				if secret {
					decoys[name] = append(decoys[name], serviceDecoy{target: target, dir: info.IsDir(), source: filepath.ToSlash(rel), writable: writable})
					hidden[filepath.ToSlash(rel)] = true
					continue
				}
			}
			if !info.IsDir() {
				if info.Mode().IsRegular() && isCoopignore(real) {
					decoys[name] = append(decoys[name], serviceDecoy{target: target, source: filepath.ToSlash(rel), bindSource: real})
				}
				continue
			}
			err = filepath.WalkDir(real, func(p string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if p == real {
					return nil
				}
				if d.IsDir() && d.Name() == ".git" {
					return fs.SkipDir
				}
				relRepo, err := filepath.Rel(realRepo, p)
				if err != nil {
					return err
				}
				under, err := filepath.Rel(real, p)
				if err != nil {
					return err
				}
				if d.Type().IsRegular() && isCoopignore(p) {
					spelled(serviceDecoy{target: target + "/" + filepath.ToSlash(under), source: filepath.ToSlash(relRepo), bindSource: p}, p)
					return nil
				}
				if secret, err := shadowed(filepath.ToSlash(relRepo)); err != nil || !secret {
					return err
				}
				hidden[filepath.ToSlash(relRepo)] = true
				decoy := serviceDecoy{target: target + "/" + filepath.ToSlash(under), dir: d.IsDir(), source: filepath.ToSlash(relRepo), writable: writable}
				if d.IsDir() {
					decoys[name] = append(decoys[name], decoy)
					return fs.SkipDir
				}
				spelled(decoy, p)
				return nil
			})
			if err != nil {
				return nil, nil, nil, err
			}
		}
	}
	return decoys, sortedKeys(hidden), sortedKeys(missed), nil
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// CaseExposed names the hidden files this review's services could still read under another letter
// case: on a disk that ignores case, the names too long to cover under every spelling
// (serviceShadowPlan). coop up refuses them unless the owner accepted that risk.
func (r *ServiceStartReview) CaseExposed() ([]string, error) {
	abs, err := filepath.Abs(r.file)
	if err != nil {
		return nil, err
	}
	_, _, uncovered, err := serviceShadowPlan(r.workspace, abs, r.data)
	return uncovered, err
}

// keepDecoysOutside drops the decoys whose source a human approved, and reports the sources still
// hidden. An approval names exact files, so a secret that appears LATER under an approved
// directory bind keeps its decoy — the human never saw it.
func keepDecoysOutside(decoys map[string][]serviceDecoy, approved []string) (map[string][]serviceDecoy, []string) {
	allow := make(map[string]bool, len(approved))
	for _, p := range approved {
		allow[p] = true
	}
	unsafe := make(map[string]bool)
	for _, list := range decoys {
		for _, d := range list {
			if d.dir || d.writable {
				unsafe[d.source] = true
			}
		}
	}
	kept := map[string][]serviceDecoy{}
	hidden := map[string]bool{}
	for name, list := range decoys {
		for _, d := range list {
			if d.bindSource == "" && allow[d.source] && !unsafe[d.source] {
				continue
			}
			kept[name] = append(kept[name], d)
			if d.bindSource == "" {
				hidden[d.source] = true
			}
		}
	}
	return kept, sortedKeys(hidden)
}

// writeServiceShadowOverride writes the override that mounts an empty decoy over every planned
// target. The override itself is per-start (dir is the private snapshot dir, gone when the command
// returns), but its decoy SOURCES are not: `compose up -d` leaves the sidecars running long after
// coop exits, and a bind whose source coop deleted is a mount the container can no longer trust —
// on a restart the runtime resolves it to whatever it invents. So the decoys live in coop's own
// state directory, created once and shared: they are empty and read-only, so sharing them costs
// nothing. (The primary box's decoy stays a temp file: that container dies inside the same Run.)
// serviceDecoyPaths returns the shared empty file and empty directory sidecars mount in place of a
// hidden path, creating them if this host has none yet. Anything unexpected at either path (a
// non-empty file, a file where the directory belongs) is replaced rather than trusted.
func serviceDecoyPaths() (file, dir string, err error) {
	root, err := serviceStateRoot("decoys")
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", "", err
	}
	file, dir = filepath.Join(root, "file"), filepath.Join(root, "dir")
	if info, statErr := os.Stat(file); statErr != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		if err := os.RemoveAll(file); err != nil {
			return "", "", err
		}
		handle, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o400)
		if err != nil && !errors.Is(err, fs.ErrExist) { // another coop created it in the meantime
			return "", "", err
		}
		if err == nil {
			if err := handle.Close(); err != nil {
				return "", "", err
			}
		}
	}
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		if err := os.RemoveAll(dir); err != nil {
			return "", "", err
		}
		if err := os.Mkdir(dir, 0o500); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", "", err
		}
	}
	return file, dir, nil
}

func writeServiceShadowOverride(repo string, decoys map[string][]serviceDecoy, dir string) (string, bool, error) {
	if len(decoys) == 0 {
		return "", false, nil
	}
	names := make([]string, 0, len(decoys))
	for name := range decoys {
		names = append(names, name)
	}
	sort.Strings(names)
	decoyFile, decoyDir, err := serviceDecoyPaths()
	if err != nil {
		return "", false, err
	}
	var sources *repositorySources
	defer func() {
		if sources != nil {
			_ = sources.Close()
		}
	}()
	// A .coopignore's covers share its snapshot: one read and one stored copy per file.
	policies := map[string]string{}
	var b strings.Builder
	b.WriteString("services:\n")
	for _, name := range names {
		list := decoys[name]
		if len(list) == 0 {
			continue
		}
		sort.Slice(list, func(i, j int) bool { return list[i].target < list[j].target })
		fmt.Fprintf(&b, "  %s:\n    volumes:\n", name)
		for _, d := range list {
			source := decoyFile
			if d.bindSource != "" {
				if source = policies[d.source]; source == "" {
					if sources == nil {
						sources, err = openRepositorySources(repo)
						if err != nil {
							return "", false, err
						}
					}
					policy, readErr := sources.readRegularFileNoFollow(filepath.FromSlash(d.source), coopIgnoreSnapshotLimit)
					if readErr != nil {
						return "", false, fmt.Errorf("snapshot service policy %s: %w", d.source, readErr)
					}
					source, err = durableServicePolicySnapshot(policy)
					if err != nil {
						return "", false, err
					}
					policies[d.source] = source
				}
			} else if d.dir {
				source = decoyDir
			}
			fmt.Fprintf(&b, "      - type: bind\n        source: %q\n        target: %q\n        read_only: true\n", source, d.target)
		}
	}
	path := filepath.Join(dir, "coop-compose-override-shadow.yml")
	if err := os.WriteFile(path, []byte(b.String()), 0o400); err != nil {
		return "", false, err
	}
	return path, true, nil
}

// A sidecar may restart long after the per-command Compose snapshot is removed. Its .coopignore
// bind therefore needs an immutable, host-private source that survives the command lifetime.
func durableServicePolicySnapshot(data []byte) (string, error) {
	root, err := serviceStateRoot("policies")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	path := filepath.Join(root, hex.EncodeToString(sum[:])+".coopignore")
	if existing, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existing, data) {
			return path, nil
		}
		return "", errors.New("stored service policy snapshot changed")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	f, temporary, err := forkspace.CreateTempIn(dir, ".policy-")
	if err != nil {
		return "", err
	}
	defer dir.Remove(temporary)
	defer f.Close()
	if err := f.Chmod(0o400); err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := forkspace.PublishNoReplace(dir, temporary, filepath.Base(path)); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		existing, err := dir.ReadFile(filepath.Base(path))
		if err != nil {
			return "", err
		}
		if !bytes.Equal(existing, data) {
			return "", errors.New("stored service policy snapshot changed")
		}
	}
	return path, nil
}

// bindSourceTarget reads a volume entry's host source and container target in both the short
// "source:target[:mode]" form and the long {type, source, target} form; "" when the entry has no
// host source (an anonymous or named volume, a tmpfs).
func bindSourceTarget(entry any) (source, target string) {
	switch v := entry.(type) {
	case string:
		parts := strings.SplitN(v, ":", 3)
		if len(parts) < 2 {
			return "", ""
		}
		return parts[0], parts[1]
	case map[string]any:
		if t, _ := v["type"].(string); t != "" && t != "bind" {
			return "", ""
		}
		source, _ = v["source"].(string)
		target, _ = v["target"].(string)
		return source, target
	}
	return "", ""
}
