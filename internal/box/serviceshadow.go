package box

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// serviceShadowOverride extends the primary box's secret shadowing to sibling services. A
// validated compose file may bind any path inside the repo into a sidecar — the repo root, a
// directory, or a single file — and Compose would hand the sidecar the raw host files, including
// every .env, key, and .coopignore'd path the box itself only ever sees as an empty decoy. The
// override written here applies the SAME shadow decision (NewShadowDecider: SecretGlobs, AllowGlobs,
// .coopignore) to each bind: a bind whose resolved source is itself a secret is replaced by a decoy
// at the same target (Compose merges volumes by target, so the override wins), and a directory bind
// gets a decoy over every shadowed descendant, pruning shadowed directories whole. Symlink aliases
// are resolved before deciding, so `./alias -> .env` is shadowed like `.env`. Named volumes and
// tmpfs entries carry no host source and are untouched.
//
// Returns the override path and true when at least one decoy was needed; decoy sources live in
// dir, the private per-start temp dir, beside the frozen compose snapshot.
func serviceShadowOverride(repo, composeFile string, data []byte, dir string) (string, bool, error) {
	decoys, _, err := serviceShadowPlan(repo, composeFile, data)
	if err != nil {
		return "", false, err
	}
	return writeServiceShadowOverride(decoys, dir)
}

// serviceDecoy is one read-only override mount. Most entries are empty decoys; bindSource marks
// a readable .coopignore overlay that secret approval must not remove.
type serviceDecoy struct {
	target     string
	dir        bool
	source     string
	bindSource string // non-empty for a readable, read-only policy bind
}

// serviceShadowPlan decides, per service, which bind targets get a decoy, and lists the
// repo-relative sources being hidden (sorted, unique) — what a human must see before approving
// the file (ReviewServiceSecrets) and what the auto-up warning names.
func serviceShadowPlan(repo, composeFile string, data []byte) (map[string][]serviceDecoy, []string, error) {
	var doc composeDoc
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&doc); err != nil {
		return nil, nil, err
	}
	realRepo, err := resolveExisting(repo)
	if err != nil {
		return nil, nil, err
	}
	composeDir := filepath.Dir(composeFile)
	shadowed := NewShadowDecider(realRepo)
	decoys := map[string][]serviceDecoy{}
	hidden := map[string]bool{}
	names := make([]string, 0, len(doc.Services))
	for name := range doc.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, entry := range doc.Services[name].Volumes {
			source, target := bindSourceTarget(entry)
			if source == "" || target == "" || !looksLikePath(source) {
				continue
			}
			abs := source
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(composeDir, source)
			}
			real, err := resolveExisting(abs)
			if err != nil {
				return nil, nil, err
			}
			rel, err := filepath.Rel(realRepo, real)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue // validated binds never leave the repo; a stray one is not ours to project
			}
			info, err := os.Stat(real)
			if err != nil {
				continue // a source that does not exist yet has nothing to hide
			}
			if rel != "." && shadowed(filepath.ToSlash(rel)) {
				decoys[name] = append(decoys[name], serviceDecoy{target: target, dir: info.IsDir(), source: filepath.ToSlash(rel)})
				hidden[filepath.ToSlash(rel)] = true
				continue
			}
			if !info.IsDir() {
				if filepath.Base(real) == CoopIgnoreFile && info.Mode().IsRegular() {
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
				if d.Name() == CoopIgnoreFile && d.Type().IsRegular() {
					decoys[name] = append(decoys[name], serviceDecoy{target: target + "/" + filepath.ToSlash(under), source: filepath.ToSlash(relRepo), bindSource: p})
					return nil
				}
				if !shadowed(filepath.ToSlash(relRepo)) {
					return nil
				}
				decoys[name] = append(decoys[name], serviceDecoy{target: target + "/" + filepath.ToSlash(under), dir: d.IsDir(), source: filepath.ToSlash(relRepo)})
				hidden[filepath.ToSlash(relRepo)] = true
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			})
			if err != nil {
				return nil, nil, err
			}
		}
	}
	paths := make([]string, 0, len(hidden))
	for p := range hidden {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return decoys, paths, nil
}

// keepDecoysOutside drops the decoys whose source a human approved, and reports the sources still
// hidden. An approval names exact files, so a secret that appears LATER under an approved
// directory bind keeps its decoy — the human never saw it.
func keepDecoysOutside(decoys map[string][]serviceDecoy, approved []string) (map[string][]serviceDecoy, []string) {
	allow := make(map[string]bool, len(approved))
	for _, p := range approved {
		allow[p] = true
	}
	kept := map[string][]serviceDecoy{}
	hidden := map[string]bool{}
	for name, list := range decoys {
		for _, d := range list {
			if d.bindSource == "" && allow[d.source] {
				continue
			}
			kept[name] = append(kept[name], d)
			if d.bindSource == "" {
				hidden[d.source] = true
			}
		}
	}
	paths := make([]string, 0, len(hidden))
	for p := range hidden {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return kept, paths
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

func writeServiceShadowOverride(decoys map[string][]serviceDecoy, dir string) (string, bool, error) {
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
				source = d.bindSource
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
