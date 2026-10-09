package box

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/safefile"
	"github.com/AndrewDryga/coop/internal/shadowpath"
)

// ConfigExposureRoots includes both Coop's host-control home and the potentially relocated
// provider config tree, plus exact ACP transcript mount sources. Transcript subdirectories may be
// aliases outside the config tree, and another provider using this same config may already have
// them mounted. BoxHome remains authority even when COOP_CONFIG_DIR points elsewhere: it contains
// coop.conf, session policy, presets and image bookkeeping that a workload must not rewrite.
func ConfigExposureRoots(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	var roots []string
	add := func(path string) {
		if path != "" && !slices.Contains(roots, path) {
			roots = append(roots, path)
		}
	}
	add(cfg.ConfigDir)
	add(cfg.NativeAuthorityConfig().ConfigDir)
	add(cfg.BoxHome)
	if cfg.ConfigDir == "" {
		return roots
	}
	// Native homes are strict complete directories below ConfigDir; there are
	// no independent transcript mounts or provider-controlled source aliases.
	return roots
}

// Repository-derived copies pin their source directory. A fixed source may be a link into the
// same repository; both its visible path and resolved target must permit every copied byte.
type repositorySources struct {
	dir  *os.File
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
	dir, err := safefile.OpenRoot(canonical)
	if err != nil {
		return nil, err
	}
	return &repositorySources{dir: dir, path: canonical}, nil
}

func (s *repositorySources) Close() error { return s.dir.Close() }

const maxSourceLinks = 16

// resolveSource follows only a fixed source's final link. All ancestors and the final resolved
// path are opened strictly below the pinned repository root. An absolute alias is mapped back by
// the pinned root's file identity before it can authorize a content read.
func (s *repositorySources) resolveSource(name string) (string, error) {
	if !filepath.IsLocal(name) || name == "." {
		return "", fmt.Errorf("source %q is not repository-relative", name)
	}
	current := filepath.Clean(name)
	followed := false
	for range maxSourceLinks {
		parent := filepath.Dir(current)
		var dir *os.File
		var err error
		if parent == "." {
			dir, err = safefile.CloneDir(s.dir)
		} else {
			dir, err = safefile.OpenDir(s.dir, parent)
		}
		if err != nil {
			if followed && errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("source %q has a missing link target", name)
			}
			return "", err
		}
		mode, err := safefile.LstatMode(dir, filepath.Base(current))
		if err != nil {
			_ = dir.Close()
			if followed && errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("source %q has a missing link target", name)
			}
			return "", err
		}
		if mode&os.ModeSymlink == 0 {
			_ = dir.Close()
			return current, nil
		}
		target, err := safefile.ReadLink(dir, filepath.Base(current))
		_ = dir.Close()
		if err != nil {
			return "", err
		}
		followed = true
		if filepath.IsAbs(target) {
			rootInfo, err := s.dir.Stat()
			if err != nil {
				return "", err
			}
			current, err = relativeToPinnedDirectory(rootInfo, target)
			if err != nil {
				return "", fmt.Errorf("source %q links outside the repository: %w", name, err)
			}
		} else {
			current = filepath.Clean(filepath.Join(parent, target))
		}
		if !filepath.IsLocal(current) || current == "." {
			return "", fmt.Errorf("source %q links outside the repository", name)
		}
	}
	return "", fmt.Errorf("source %q has too many symbolic links", name)
}

// sourceVisibility combines the policy on an alias with the policy at its resolved target.
// Directory descendants extend both snapshots from each actual pinned child directory.
type sourceVisibility struct {
	aliasRel, realRel string
	alias, real       *shadowpath.Snapshot
}

func (v *sourceVisibility) Shadowed(realRel string) bool {
	if v.real.Shadowed(realRel) {
		return true
	}
	if v.alias == nil {
		return false
	}
	rel, err := filepath.Rel(filepath.FromSlash(v.realRel), filepath.FromSlash(realRel))
	if err != nil || rel == ".." || !filepath.IsLocal(rel) && rel != "." {
		return true
	}
	return v.alias.Shadowed(filepath.ToSlash(filepath.Join(v.aliasRel, rel)))
}

func (v *sourceVisibility) Extend(realRel string, dir *os.File) (*sourceVisibility, error) {
	real, err := v.real.Extend(realRel, dir)
	if err != nil {
		return nil, err
	}
	if v.alias == nil {
		return &sourceVisibility{realRel: v.realRel, real: real}, nil
	}
	rel, err := filepath.Rel(filepath.FromSlash(v.realRel), filepath.FromSlash(realRel))
	if err != nil || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("source descendant %q is outside %q", realRel, v.realRel)
	}
	alias, err := v.alias.Extend(filepath.ToSlash(filepath.Join(v.aliasRel, rel)), dir)
	if err != nil {
		return nil, err
	}
	return &sourceVisibility{aliasRel: v.aliasRel, realRel: v.realRel, alias: alias, real: real}, nil
}

func (s *repositorySources) visibility(aliasRel, realRel string, real *shadowpath.Snapshot) (*sourceVisibility, error) {
	v := &sourceVisibility{aliasRel: filepath.ToSlash(aliasRel), realRel: filepath.ToSlash(realRel), real: real}
	if filepath.Clean(aliasRel) == filepath.Clean(realRel) {
		return v, nil
	}
	parent := filepath.Dir(aliasRel)
	aliasDir, alias, err := shadowpath.OpenTree(s.dir, parent)
	if err != nil {
		return nil, err
	}
	_ = aliasDir.Close()
	v.alias = alias
	if v.Shadowed(realRel) {
		return nil, fmt.Errorf("repository source %s is hidden by Coop's secret policy", aliasRel)
	}
	return v, nil
}

func readRepositoryRegularFileNoFollow(repo, name string, limit int64) ([]byte, error) {
	sources, err := openRepositorySources(repo)
	if err != nil {
		return nil, err
	}
	defer sources.Close()
	return sources.readRegularFileNoFollow(name, limit)
}

func (s *repositorySources) exists(name string, wantDir bool) (bool, error) {
	resolved, err := s.resolveSource(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var opened *os.File
	if wantDir {
		opened, err = safefile.OpenDir(s.dir, resolved)
	} else {
		opened, err = safefile.OpenRegular(s.dir, resolved)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, opened.Close()
}

func (s *repositorySources) openTree(name string) (*os.File, *sourceVisibility, error) {
	realRel, err := s.resolveSource(name)
	if err != nil {
		return nil, nil, err
	}
	tree, real, err := shadowpath.OpenTree(s.dir, realRel)
	if err != nil {
		return nil, nil, err
	}
	v, err := s.visibility(name, realRel, real)
	if err != nil {
		_ = tree.Close()
		return nil, nil, err
	}
	return tree, v, nil
}

const maxFallbackFileBytes = 1 << 20

// readRegularFileNoFollow snapshots a repository file selected by a prior metadata walk. The
// descriptor-rooted open keeps every intermediate traversal inside the pinned repository, while
// O_NOFOLLOW refuses a swapped final symlink and O_NONBLOCK makes a swapped FIFO/device harmless.
func (s *repositorySources) readRegularFileNoFollow(name string, limit int64) ([]byte, error) {
	return safefile.ReadRegular(s.dir, name, limit)
}

func (s *repositorySources) readFile(name string) ([]byte, error) {
	resolved, err := s.resolveSource(name)
	if err != nil {
		return nil, err
	}
	parent, realPolicy, err := shadowpath.OpenTree(s.dir, filepath.Dir(resolved))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	visibility, err := s.visibility(name, resolved, realPolicy)
	if err != nil {
		return nil, err
	}
	if visibility.Shadowed(filepath.ToSlash(resolved)) {
		return nil, fmt.Errorf("repository source %s is hidden by Coop's secret policy", name)
	}
	return safefile.ReadRegular(parent, filepath.Base(resolved), maxFallbackFileBytes)
}

const (
	maxSourceCopyEntries = 16 << 10
	maxSourceCopyBytes   = 256 << 20
	maxSourceCopyDepth   = 64
)

type sourceCopyBudget struct {
	entries     int
	bytes       int64
	policyBytes int64
}

// copySourceTree copies from pinned directory descriptors. Every actual child is reopened with
// raw no-follow semantics, so a repo writer cannot swap a listed file or directory into a link or
// FIFO. The same shadow predicate as the primary repository mount is applied before any byte is
// read, preventing a real `.env` or .coopignore-hidden file from reappearing in a synthesized home.
func copySourceTree(dst string, source *os.File, policy *sourceVisibility, skip func(string) bool) error {
	sourceRel := filepath.FromSlash(policy.realRel)
	if policy.Shadowed(filepath.ToSlash(sourceRel)) {
		return fmt.Errorf("repository source %s is hidden by Coop's secret policy", filepath.ToSlash(sourceRel))
	}
	copySource, err := safefile.CloneDir(source)
	if err != nil {
		return err
	}
	defer copySource.Close()
	rootInfo, err := copySource.Stat()
	if err != nil {
		return err
	}
	if err := prepareCopyDestination(dst); err != nil {
		return err
	}
	budget := &sourceCopyBudget{}
	budget.policyBytes = policy.real.PolicyBytes()
	if policy.alias != nil {
		budget.policyBytes += policy.alias.PolicyBytes()
	}
	if budget.policyBytes > shadowpath.MaxSnapshotPolicyBytes {
		return fmt.Errorf("repository copy policy exceeds %d bytes", shadowpath.MaxSnapshotPolicyBytes)
	}
	if err := copySourceDirectory(dst, copySource, rootInfo, sourceRel, ".", 0, budget, policy, skip); err != nil {
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

func prepareCopyDestination(dst string) error {
	err := os.Mkdir(dst, 0o700)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	info, statErr := os.Lstat(dst)
	if statErr != nil {
		return statErr
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("copy destination %s is not a real directory", dst)
	}
	entries, readErr := os.ReadDir(dst)
	if readErr != nil {
		return readErr
	}
	if len(entries) != 0 {
		return fmt.Errorf("copy destination %s is not empty", dst)
	}
	return nil
}

func copySourceDirectory(dst string, source *os.File, sourceRoot os.FileInfo, sourceRel, treeRel string,
	depth int, budget *sourceCopyBudget, policy *sourceVisibility, skip func(string) bool,
) error {
	if depth > maxSourceCopyDepth {
		return fmt.Errorf("repository copy exceeds %d directory levels", maxSourceCopyDepth)
	}
	for {
		entries, readErr := source.ReadDir(128)
		for _, entry := range entries {
			name := entry.Name()
			childTreeRel := filepath.Join(treeRel, name)
			if skip != nil && skip(filepath.ToSlash(childTreeRel)) {
				continue
			}
			budget.entries++
			if budget.entries > maxSourceCopyEntries {
				return fmt.Errorf("repository copy exceeds %d entries", maxSourceCopyEntries)
			}
			repoRel := filepath.ToSlash(filepath.Join(sourceRel, childTreeRel))
			if policy.Shadowed(repoRel) {
				continue
			}
			mode, err := safefile.LstatMode(source, name)
			if err != nil {
				return err
			}
			destination := filepath.Join(dst, name)
			switch {
			case mode&os.ModeSymlink != 0:
				target, err := safefile.ReadLink(source, name)
				if err != nil {
					return err
				}
				if filepath.IsAbs(target) {
					within, err := relativeToPinnedDirectory(sourceRoot, target)
					if err != nil {
						return fmt.Errorf("source link %s points outside its copied tree: %w", childTreeRel, err)
					}
					target, err = filepath.Rel(filepath.Dir(childTreeRel), within)
					if err != nil {
						return err
					}
				}
				if err := os.Symlink(target, destination); err != nil {
					return err
				}
			case mode.IsDir():
				child, err := safefile.OpenDir(source, name)
				if err != nil {
					return err
				}
				if err := os.Mkdir(destination, 0o777); err != nil {
					_ = child.Close()
					return err
				}
				childPolicy, policyErr := policy.Extend(repoRel, child)
				if policyErr != nil {
					_ = child.Close()
					return policyErr
				}
				budget.policyBytes += childPolicy.real.PolicyBytes() - policy.real.PolicyBytes()
				if policy.alias != nil {
					budget.policyBytes += childPolicy.alias.PolicyBytes() - policy.alias.PolicyBytes()
				}
				if budget.policyBytes > shadowpath.MaxSnapshotPolicyBytes {
					_ = child.Close()
					return fmt.Errorf("repository copy policy exceeds %d bytes", shadowpath.MaxSnapshotPolicyBytes)
				}
				err = copySourceDirectory(destination, child, sourceRoot, sourceRel, childTreeRel,
					depth+1, budget, childPolicy, skip)
				err = errors.Join(err, child.Close())
				if err != nil {
					return err
				}
			case mode.IsRegular():
				in, err := safefile.OpenRegular(source, name)
				if err != nil {
					return err
				}
				perm := os.FileMode(0o666) | mode.Perm()&0o111
				out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
				if err != nil {
					_ = in.Close()
					return err
				}
				remaining := int64(maxSourceCopyBytes) - budget.bytes
				written, copyErr := io.Copy(out, io.LimitReader(in, remaining+1))
				budget.bytes += written
				err = errors.Join(copyErr, out.Close(), in.Close())
				if budget.bytes > maxSourceCopyBytes {
					err = errors.Join(err, fmt.Errorf("repository copy exceeds %d bytes", maxSourceCopyBytes))
				}
				if err != nil {
					return err
				}
			default:
				return fmt.Errorf("repository source %s is not a regular file, directory, or symbolic link", repoRel)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func relativeToPinnedDirectory(root os.FileInfo, path string) (string, error) {
	rel := "."
	for {
		info, err := os.Stat(path)
		if err == nil && os.SameFile(root, info) {
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
