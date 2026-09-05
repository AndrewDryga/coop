package box

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

// ConfigExposureRoots includes the config tree and exact ACP transcript mount
// sources. Transcript subdirectories may be aliases outside the config tree, and
// another provider using this same config may already have them mounted.
func ConfigExposureRoots(cfg *config.Config) []string {
	if cfg == nil || cfg.ConfigDir == "" {
		return nil
	}
	roots := []string{cfg.ConfigDir}
	for _, name := range agents.Names() {
		ag, _ := agents.Get(name)
		for _, subdir := range ag.ACPSessionDirs() {
			roots = append(roots, filepath.Join(acpSharedDir(cfg, name), subdir))
		}
	}
	return roots
}

// Metadata resolution preserves absolute in-repository links; only root-relative
// names authorize content reads. A later source replacement cannot widen the root.
type repositorySources struct {
	root *os.Root
	path string
}

func openRepositorySources(repo string) (*repositorySources, error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, err
	}
	return &repositorySources{root: root, path: canonical}, nil
}

func (s *repositorySources) resolve(name string) (string, error) {
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("artifact path %q is not repository-relative", name)
	}
	resolved, _, err := resolvePathTrace(filepath.Join(s.path, name))
	if err != nil {
		return "", err
	}
	rel, err := relativeToSourceRoot(s.root, resolved)
	if err != nil {
		return "", fmt.Errorf("artifact %q: %w", name, err)
	}
	return rel, nil
}

// The root's inode, not its path spelling, anchors aliases on case-insensitive
// filesystems. This metadata walk only selects a name; reads still use root.
func relativeToSourceRoot(root *os.Root, path string) (string, error) {
	rootInfo, err := root.Stat(".")
	if err != nil {
		return "", err
	}
	rel := "."
	for {
		info, err := os.Stat(path)
		if err == nil && os.SameFile(rootInfo, info) {
			return rel, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", errors.New("source resolves outside its authorized tree")
		}
		rel = filepath.Join(filepath.Base(path), rel)
		path = parent
	}
}

func (s *repositorySources) exists(name string, wantDir bool) (bool, error) {
	rel, err := s.resolve(name)
	if err != nil {
		return false, err
	}
	info, err := s.root.Stat(rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// This metadata-only read distinguishes an absent optional artifact from a
			// dangling leaf link. It never authorizes an unrooted content read.
			if _, linkErr := os.Lstat(filepath.Join(s.path, name)); errors.Is(linkErr, os.ErrNotExist) {
				return false, nil
			} else if linkErr != nil {
				return false, linkErr
			}
		}
		return false, err
	}
	if wantDir && !info.IsDir() {
		return false, fmt.Errorf("%s is not a directory", name)
	}
	if !wantDir && !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", name)
	}
	return true, nil
}

func (s *repositorySources) openTree(name string) (*os.Root, error) {
	rel, err := s.resolve(name)
	if err != nil {
		return nil, err
	}
	return s.root.OpenRoot(rel)
}

const maxFallbackFileBytes = 1 << 20

func (s *repositorySources) readFile(name string) ([]byte, error) {
	rel, err := s.resolve(name)
	if err != nil {
		return nil, err
	}
	f, err := s.root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFallbackFileBytes+1))
	if err == nil && len(data) > maxFallbackFileBytes {
		err = fmt.Errorf("fallback %s exceeds 1 MiB", name)
	}
	return data, err
}

// Do not embed Root.FS: its optimized ReadDir/ReadFile methods bypass this
// nonblocking open. CopyFS can otherwise hang on a regular-file-to-FIFO race.
type sourceCopyFS struct{ root *os.Root }

func (s sourceCopyFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	f, err := s.root.OpenFile(filepath.FromSlash(name), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.IsDir() && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file or directory", name)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (s sourceCopyFS) Lstat(name string) (fs.FileInfo, error) {
	return s.root.Lstat(filepath.FromSlash(name))
}

func (s sourceCopyFS) ReadLink(name string) (string, error) {
	target, err := s.root.Readlink(filepath.FromSlash(name))
	if err != nil || !filepath.IsAbs(target) {
		return target, err
	}
	// An absolute link inside the source must remain inside its relocated copy.
	resolved, _, err := resolvePathTrace(target)
	if err != nil {
		return "", err
	}
	rel, err := relativeToSourceRoot(s.root, resolved)
	if err != nil {
		return "", fmt.Errorf("source link %q points outside its copied tree", name)
	}
	return filepath.Rel(filepath.Dir(filepath.FromSlash(name)), rel)
}

func copySourceTree(dst string, source *os.Root) error {
	if err := os.CopyFS(dst, sourceCopyFS{root: source}); err != nil {
		return err
	}
	copyRoot, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer copyRoot.Close()
	// Validate the immutable copy, not mutable source link text. Internal links
	// retain their behavior; outward, dangling and cyclic chains are never exposed.
	return fs.WalkDir(copyRoot.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if _, err := copyRoot.Stat(filepath.FromSlash(name)); err != nil {
				return fmt.Errorf("copied link %s does not resolve inside its tree: %w", name, err)
			}
		}
		return nil
	})
}
