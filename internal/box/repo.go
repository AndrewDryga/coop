package box

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
)

// ResolveRepo returns the repository to operate on: the override if set, else the repository that
// FindRepo finds, else the working directory.
func ResolveRepo(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	repo, ok, err := FindRepo()
	if err != nil || ok {
		return repo, err
	}
	return syscall.Getwd()
}

// FindRepo returns the nearest folder at or above the working directory that holds a .git; ok is
// false when none does. It never asks git: git's answer follows core.worktree and other settings in
// .git/config, which a box can write, so a planted core.worktree would have the next launch mount
// whatever it names, the user's home folder say, as the workspace. Like git, it refuses a
// repository another user owns. It also never climbs to a repository at the home folder or above
// it: a box that deletes its own .git moves the walk up to whatever repository holds it, a home
// folder kept in Git among them, and only the override can name one of those.
func FindRepo() (repo string, ok bool, err error) {
	// The kernel's name for the working directory, never $PWD's: on a case-insensitive disk $PWD
	// keeps the case the user typed, while everything keyed by the repository's path (leases,
	// Compose names, the case-sensitive mount in the box) needs the one spelling git reports.
	wd, err := syscall.Getwd()
	if err != nil {
		return "", false, fmt.Errorf("find the working folder: %w", err)
	}
	root, ok, err := forkspace.RepositoryRoot(wd)
	if err != nil || !ok {
		return "", false, err
	}
	if err := ownedRepository(root); err != nil {
		return "", false, err
	}
	if home, above := homeOrAbove(root); above {
		return "", false, fmt.Errorf("the nearest Git repository to %s is %s, at your home folder %s or above it, and Coop never "+
			"uses one of those as a project: run `git init` in your project folder, or set COOP_REPO to the folder to use", wd, root, home)
	}
	return root, true, nil
}

// ownedRepository refuses, as git does, a repository whose folder or .git another user owns: on a
// shared host anyone can create /tmp/.git, and on macOS a hard link to one of your own gitfiles
// shows as yours, so the folder holding it has to be yours too.
func ownedRepository(root string) error {
	for _, path := range []string{root, filepath.Join(root, ".git")} {
		if info, err := os.Lstat(path); err == nil {
			if st, isUnix := info.Sys().(*syscall.Stat_t); isUnix && int(st.Uid) != os.Getuid() {
				return fmt.Errorf("the Git repository at %s belongs to another user, so Coop does not use it as a project: "+
					"set COOP_REPO to the folder to use", root)
			}
		}
	}
	return nil
}

// homeOrAbove reports whether dir is the user's home folder or a folder above it, and names the
// home folder. It compares folders, not spellings, so neither a case-insensitive disk nor a linked
// home lets a repository at the home folder through. A HOME that is unset or relative falls back to
// the account's home folder.
func homeOrAbove(dir string) (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		u, err := user.Current()
		if err != nil || u.HomeDir == "" {
			return "", false
		}
		home = u.HomeDir
	}
	target, err := os.Stat(dir)
	if err != nil {
		return home, false
	}
	walk := home
	if real, err := filepath.EvalSymlinks(home); err == nil {
		walk = real
	}
	for ; ; walk = filepath.Dir(walk) {
		if info, err := os.Stat(walk); err == nil && os.SameFile(info, target) {
			return home, true
		}
		if filepath.Dir(walk) == walk {
			return home, false
		}
	}
}

// ServicesProject is the deterministic per-REPO base name: a lowercased, sanitized basename.
// ImageForRepo adds a native Linux user suffix for project images; Compose names stay repo-based.
func ServicesProject(repo string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(filepath.Base(repo)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return "coop-" + b.String()
}

// ComposeProject is the per-WORKSPACE compose project (and network) name: the sanitized basename
// plus a short hash of the workspace's CANONICAL path. Distinct per checkout, so a fork and its
// parent — or two clones — never share one compose project (and its volumes). The path is
// canonicalized (symlinks resolved, e.g. macOS /var→/private/var) so the SAME physical workspace
// always yields the SAME name: its sidecar volumes persist across every run. Distinct from the
// image base name (ServicesProject); ImageForRepo adds a user suffix on native Linux.
func ComposeProject(workspacePath string) string {
	canon := canonicalWorkspace(workspacePath)
	sum := sha256.Sum256([]byte(canon))
	return ServicesProject(canon) + "-" + hex.EncodeToString(sum[:])[:8]
}

// ComposeProjectFor returns the development project's historical name when owner is empty, and
// a private Compose project for one logical loop/session otherwise. The owner is hashed rather
// than embedded because run identifiers are host bookkeeping, not public Docker names.
func ComposeProjectFor(workspacePath, owner string) string {
	base := ComposeProject(workspacePath)
	if owner == "" {
		return base
	}
	sum := sha256.Sum256([]byte(owner))
	return base + "-run-" + hex.EncodeToString(sum[:])[:8]
}

func servicePortScope(workspacePath, owner string) string {
	if owner == "" {
		return canonicalWorkspace(workspacePath)
	}
	return canonicalWorkspace(workspacePath) + "#coop-service-owner:" + owner
}

// canonicalWorkspace resolves a workspace path to a stable canonical form (symlinks followed,
// cleaned) so the same physical checkout always hashes identically — the invariant behind stable
// per-workspace ports AND stable sidecar volumes. Falls back to a plain clean if the path can't be
// resolved (e.g. doesn't exist yet).
func canonicalWorkspace(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(r)
	}
	return filepath.Clean(path)
}

// ComposeFile returns the sibling-services compose file, or "" if it's absent or empty (a zero-byte
// file declares no services). It completes the config-source/runtime-identity split: the relative
// PATH (box.compose, else .agent/compose.yml) is trusted config, read from policyRepo, while the
// FILE itself is read from the workspace at that path — so a fork uses the parent's committed choice
// of WHERE the compose file lives, but its OWN copy of the file. For a plain repo pass repo twice.
func ComposeFile(workspace, policyRepo string) string {
	return ComposeFileAt(workspace, project.ComposePath(policyRepo))
}

// ComposeFileAt returns the tracked compose file selected by a trusted project policy. The
// relative path has already been validated by project.Load; this helper keeps review-specific
// dependency stacks bound to the disposable candidate rather than the parent checkout.
func ComposeFileAt(workspace, relativePath string) string {
	f := filepath.Join(workspace, filepath.FromSlash(relativePath))
	if fi, err := os.Stat(f); err == nil && !fi.IsDir() && fi.Size() > 0 {
		return f
	}
	return ""
}

// fileExists reports whether path is an existing regular file (not a directory).
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
