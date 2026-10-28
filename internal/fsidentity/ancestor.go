package fsidentity

import (
	"errors"
	"os"
	"path/filepath"
)

// AliasesAncestor compares the source inode with every existing ancestor of path.
// Callers separately check lexical overlap, including paths that do not exist yet.
func AliasesAncestor(root, path string) (bool, error) {
	rootInfo, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil && os.SameFile(rootInfo, info) {
			return true, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, nil
		}
	}
}
