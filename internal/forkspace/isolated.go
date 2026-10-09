package forkspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Refuse redirected strong metadata before any operational view can reconcile
// HEAD. Check containing execution roots too: host calls may start in a child.
func validateIsolatedGitAccess(workspace string) error {
	path, err := filepath.Abs(workspace)
	if err != nil {
		return err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	var metadataRoots []string
	for {
		if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
			metadataRoots = append(metadataRoots, path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(path)
		if strings.HasSuffix(parent, Suffix) && ValidExistingName(filepath.Base(path)) {
			repo := strings.TrimSuffix(parent, Suffix)
			identity, present, err := ReadGeneration(repo, filepath.Base(path))
			if err != nil {
				return err
			}
			isolated := false
			if present {
				isolated, err = IsolatedGeneration(repo, identity)
				if err != nil {
					return err
				}
			}
			// Ordinary/legacy binding migration itself needs host Git. Do not
			// apply strong admission to it or recursively demand a migrated anchor.
			if !isolated {
				path = parent
				continue
			}
			if err := ValidateGenerationWorkspace(repo, identity); err != nil {
				return err
			}
			// Missing root metadata must not fall through to an ancestor repository.
			if err := ValidateIndependentGit(path); err != nil {
				return fmt.Errorf("isolated host Git admission: %w", err)
			}
			for _, root := range metadataRoots {
				if root == path {
					continue
				}
				if err := ValidateIndependentGit(root); err != nil {
					return fmt.Errorf("isolated host Git admission: %w", err)
				}
			}
			return nil
		}
		if parent == path {
			return nil
		}
		path = parent
	}
}

// GitMetadataDirectories resolves the parent's administrative and common stores as data only.
// Linked worktrees and separate gitdirs must be fenced alongside the checkout itself.
func GitMetadataDirectories(workspace string) ([]string, error) {
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, err
	}
	metadata := filepath.Join(workspace, ".git")
	info, err := os.Lstat(metadata)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		data, present, err := readGitPointerFile(metadata)
		if err != nil || !present {
			return nil, errors.Join(errors.New("parent Git directory cannot be inspected"), err)
		}
		line := strings.TrimSpace(string(data))
		if !strings.HasPrefix(line, "gitdir:") {
			return nil, errors.New("parent .git is not a Git directory pointer")
		}
		metadata, err = resolveGitPointer(workspace, []byte(strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))), "Git-directory")
		if err != nil {
			return nil, err
		}
	}
	metadata, err = filepath.EvalSymlinks(metadata)
	if err != nil {
		return nil, err
	}
	data, present, err := readGitPointerFile(filepath.Join(metadata, "commondir"))
	if err != nil {
		return nil, err
	}
	roots := []string{metadata}
	if present {
		common, err := resolveGitPointer(metadata, data, "common-directory")
		if err != nil {
			return nil, err
		}
		if common != metadata {
			roots = append(roots, common)
		}
	}
	for _, root := range roots {
		info, err := os.Lstat(root)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("parent Git metadata root is not a real directory")
		}
	}
	return roots, nil
}

// ValidateIndependentGit refuses metadata redirectors before host capture. Isolated execution
// owns a real .git and object store; a gitfile, alternate, hardlink or symlink cannot restore
// write authority over the parent. Callers must also fence live sandbox writers before capture.
func ValidateIndependentGit(workspace string) error {
	return validateIndependentGit(workspace, filepath.WalkDir)
}

var errIndependentGitChanged = errors.New("isolated Git metadata changed during inspection")

func validateIndependentGit(workspace string, walk func(string, fs.WalkDirFunc) error) error {
	var err error
	for range 3 {
		err = validateIndependentGitOnce(workspace, walk)
		if !errors.Is(err, errIndependentGitChanged) {
			return err
		}
		// Git maintenance can unlink a listed lock before Info. Discard the
		// partial pass, never exempt the path from independent-ownership checks.
	}
	return err
}

func validateIndependentGitOnce(workspace string, walk func(string, fs.WalkDirFunc) error) error {
	metadata := filepath.Join(workspace, ".git")
	objects := filepath.Join(metadata, "objects")
	for _, path := range []string{"commondir", "objects/info/alternates", "objects/info/http-alternates"} {
		if _, err := os.Lstat(filepath.Join(metadata, path)); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(fmt.Errorf("isolated Git cannot use %s", path), err)
		}
	}
	for _, path := range []string{metadata, objects} {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("isolated Git requires its own real directory: %s", path)
		}
	}
	return walk(metadata, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) && path != metadata && path != objects {
			return errors.Join(errIndependentGitChanged, err)
		}
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 {
			return fmt.Errorf("isolated Git metadata is not independently owned: %s", path)
		}
		return nil
	})
}
