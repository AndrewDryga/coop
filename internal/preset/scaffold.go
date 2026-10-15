package preset

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// Scaffold writes a starter preset and its prompts into a new .agent/presets/<name> folder and
// returns the preset.yaml path. Every write is an exclusive create inside the repository, so an
// existing destination, even an empty or incomplete folder, belongs to the user and is refused.
// A failed scaffold removes what it created.
func Scaffold(repo, name string) (_ string, err error) {
	if !ValidName(name) {
		return "", fmt.Errorf("invalid preset name %q — a preset is a folder name under %s/ (lowercase, no '/', '..', or leading '-')", name, Dir)
	}
	dir := filepath.Join(filepath.FromSlash(Dir), name)
	files := scaffoldFiles(name)
	// The name is part of the rendered YAML, so the files are validated before any is written.
	rendered := make(map[string][]byte, len(files))
	for _, f := range files {
		rendered[f.rel] = []byte(f.content)
	}
	if _, err := loadPreset(name, filepath.Join(repo, dir), rendered["preset.yaml"], func(rel string) ([]byte, error) {
		if data, ok := rendered[filepath.ToSlash(rel)]; ok {
			return data, nil
		}
		return nil, os.ErrNotExist
	}); err != nil {
		return "", fmt.Errorf("validate starter preset: %w", err)
	}

	root, err := os.OpenRoot(repo)
	if err != nil {
		return "", err
	}
	defer root.Close()
	// Load follows no link on a preset's path, so a preset written through one could not run.
	for _, parent := range []string{".agent", filepath.FromSlash(Dir)} {
		if err := root.Mkdir(parent, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if info, err := root.Lstat(parent); err != nil {
			return "", err
		} else if !info.IsDir() {
			return "", fmt.Errorf("preset directory %s is not a real directory — symlinks are not supported during initialization", filepath.ToSlash(parent))
		}
	}
	// From here every create and removal goes through directory handles, so a link a box swaps into
	// the path cannot move one into a folder it cannot see, such as one hidden by .coopignore.
	presets, err := root.OpenRoot(filepath.FromSlash(Dir))
	if err != nil {
		return "", err
	}
	defer presets.Close()
	if err := presets.Mkdir(name, 0o755); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		exists := &ExistsError{Name: name, Path: filepath.ToSlash(dir)}
		if _, err := presets.Stat(filepath.Join(name, "preset.yaml")); err == nil {
			exists.Complete, exists.Path = true, filepath.ToSlash(filepath.Join(dir, "preset.yaml"))
		}
		return "", exists
	}
	folder, err := presets.OpenRoot(name)
	if err != nil {
		_ = presets.Remove(name)
		return "", err
	}
	defer folder.Close()
	// A failure removes what this call created, newest first. Remove is not recursive, so a folder
	// something else has written into stays.
	var created []string
	defer func() {
		if err != nil {
			for i := len(created) - 1; i >= 0; i-- {
				_ = folder.Remove(created[i])
			}
			_ = presets.Remove(name)
		}
	}()
	if err := folder.Mkdir("roles", 0o755); err != nil {
		return "", err
	}
	created = append(created, "roles")
	for _, f := range files {
		rel := filepath.FromSlash(f.rel)
		file, err := folder.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return "", err
		}
		created = append(created, rel)
		_, writeErr := file.WriteString(f.content)
		if err := errors.Join(writeErr, file.Close()); err != nil {
			return "", err
		}
	}
	return filepath.Join(repo, dir, "preset.yaml"), nil
}

// scaffoldFiles lists what Scaffold writes, in order. preset.yaml comes last, so an interrupted
// scaffold never looks like a complete preset.
func scaffoldFiles(name string) []templateFile {
	return append(slices.Clone(templateFiles), templateFile{"preset.yaml", fmt.Sprintf(Template, name)})
}

// ExistsError refuses a create whose destination is already there. Complete says the folder holds
// a preset a reader can open — so the answer is to show it — while an incomplete folder is
// somebody's half-finished work that coop must never merge into, repair, or overwrite. Path is
// what the reader should open: the preset file, or the folder that is in the way.
type ExistsError struct {
	Name     string
	Path     string
	Complete bool
}

func (e *ExistsError) Error() string {
	if e.Complete {
		return fmt.Sprintf("preset %q already exists (%s)", e.Name, e.Path)
	}
	return fmt.Sprintf("%s already exists but is incomplete", e.Path)
}
