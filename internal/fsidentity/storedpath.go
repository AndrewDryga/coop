package fsidentity

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// StoredPath is path with its links resolved and every component in the spelling the disk stores.
// filepath.EvalSymlinks keeps the spelling it was given, so on a disk that ignores letter case (a
// Mac's) ~/projects/APP and ~/Projects/app name one folder in two spellings, and a record keyed by
// the path would get two keys. A component the disk would not open under another case is kept as
// given, so a disk that keeps case pays one lstat per component. A path that does not exist is
// returned absolute and cleaned, as given.
func StoredPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) {
			return filepath.Abs(path)
		}
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	stored := filepath.VolumeName(resolved) + string(filepath.Separator)
	for _, name := range strings.Split(strings.TrimPrefix(resolved, stored), string(filepath.Separator)) {
		if name == "" {
			continue
		}
		spelled, err := storedName(stored, name)
		if err != nil {
			return "", err
		}
		stored = filepath.Join(stored, spelled)
	}
	return stored, nil
}

// storedName is the spelling dir's listing holds for name.
func storedName(dir, name string) (string, error) {
	swapped := swapCase(name)
	if swapped == name {
		return name, nil
	}
	named, err := os.Lstat(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	if other, err := os.Lstat(filepath.Join(dir, swapped)); err != nil || !os.SameFile(named, other) {
		return name, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	if slices.Contains(names, name) {
		return name, nil
	}
	for _, candidate := range names {
		if info, err := os.Lstat(filepath.Join(dir, candidate)); err == nil && os.SameFile(named, info) {
			return candidate, nil
		}
	}
	return name, nil
}

func swapCase(name string) string {
	swapped := []byte(name)
	for i, c := range swapped {
		switch {
		case 'a' <= c && c <= 'z':
			swapped[i] = c - 'a' + 'A'
		case 'A' <= c && c <= 'Z':
			swapped[i] = c - 'A' + 'a'
		}
	}
	return string(swapped)
}
