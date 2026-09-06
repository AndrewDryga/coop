package box

import (
	"bytes"
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
	var doc composeDoc
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&doc); err != nil {
		return "", false, err
	}
	realRepo, err := resolveExisting(repo)
	if err != nil {
		return "", false, err
	}
	composeDir := filepath.Dir(composeFile)
	shadowed := NewShadowDecider(realRepo)
	type decoy struct {
		target string
		dir    bool
	}
	decoys := map[string][]decoy{}
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
				return "", false, err
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
				decoys[name] = append(decoys[name], decoy{target: target, dir: info.IsDir()})
				continue
			}
			if !info.IsDir() {
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
				if !shadowed(filepath.ToSlash(relRepo)) {
					return nil
				}
				under, err := filepath.Rel(real, p)
				if err != nil {
					return err
				}
				decoys[name] = append(decoys[name], decoy{target: target + "/" + filepath.ToSlash(under), dir: d.IsDir()})
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			})
			if err != nil {
				return "", false, err
			}
		}
	}
	if len(decoys) == 0 {
		return "", false, nil
	}
	decoyFile := filepath.Join(dir, "decoy")
	if err := os.WriteFile(decoyFile, nil, 0o400); err != nil {
		return "", false, err
	}
	decoyDir := filepath.Join(dir, "decoy-dir")
	if err := os.Mkdir(decoyDir, 0o500); err != nil {
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
			if d.dir {
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
