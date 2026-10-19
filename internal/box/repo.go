package box

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/fsidentity"
	"github.com/AndrewDryga/coop/internal/shadowpath"
)

// ResolveRepo returns the repository to operate on: the override if set, else the repository that
// FindRepo finds, else the working directory.
//
// The override is followed through links to the folder they name. The secret scan walks the
// repository from its root and never follows a link, so a linked override would be mounted whole
// with no secret hidden; and the working directory never names a link, so everything keyed by the
// repository's path would see two projects. For the same reason the override takes the spelling the
// disk stores: on a disk that ignores letter case (a Mac's), COOP_REPO=~/projects/APP names the
// folder the working directory calls ~/Projects/app, and task leases, fork records and network
// approvals must not split between the two. An override that cannot be resolved, one that does not
// exist say, is kept as given, for the command that uses it to report.
//
// A link may not take the override to the home folder or a folder above it. A box that can write
// the folder above the override, a checkout kept inside the one it works in say, could swap the
// override for such a link, and the next launch would mount all the user's files read-write with
// only the secret names hidden. An override spelled as HOME spells the home folder, or as a folder
// above it, still names that folder when a link in that spelling changes its path, as /home does on
// some Linux systems: such a link sits above the home folder, and a box that could change it could
// already write the home folder.
func ResolveRepo(override string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		resolved := canonicalWorkspace(abs)
		if stored, err := fsidentity.StoredPath(resolved); err == nil {
			resolved = stored
		}
		if resolved != abs {
			if home, above := shadowpath.HomeOrAbove(resolved); above {
				if rel, err := filepath.Rel(abs, home); err != nil || !filepath.IsLocal(rel) {
					return "", fmt.Errorf("COOP_REPO %s leads through a link to %s, your home folder or above. "+
						"Remove the link, or set COOP_REPO to the folder itself", abs, resolved)
				}
			}
		}
		return resolved, nil
	}
	repo, ok, err := FindRepo()
	if err != nil || ok {
		return repo, err
	}
	// The kernel's name for the working folder, as in FindRepo: a shell that entered a folder through
	// a link a box planted is in the folder the link leads to, which may be the home folder.
	wd, err := syscall.Getwd()
	if err != nil {
		return "", fmt.Errorf("find the working folder: %w", err)
	}
	if home, above := shadowpath.HomeOrAbove(wd); above {
		return "", fmt.Errorf("the working folder %s is your home folder %s or above it, and %s", wd, home, homeProjectRefusal)
	}
	return wd, nil
}

// homeProjectRefusal ends both refusals of a home folder, or a folder above it, as the project.
const homeProjectRefusal = "Coop never uses one of those as a project: run `git init` in your project folder, or set COOP_REPO to the folder to use"

// RepoRelative is path relative to repo, matching folders rather than spellings. repo is the path
// ResolveRepo gives, which normally holds no link, while a path the user hands Coop, the shell's
// working directory or an absolute COOP_TASKS, may reach the same folder through one. The lexical
// answer stands when it already lies inside repo, or when no folder at or above path is the
// repository.
func RepoRelative(repo, path string) (string, error) {
	rel, err := filepath.Rel(repo, path)
	if err != nil || filepath.IsLocal(rel) {
		return rel, err
	}
	if dir, ok := folderAtOrAbove(path, repo); ok {
		return filepath.Rel(dir, path)
	}
	return rel, nil
}

// SameFolder reports whether a and b name one folder, however each is spelled: through a link, or in
// another case on a case-insensitive disk. A path Coop recorded keeps the spelling it was written
// in, so it is matched to the one Coop derives now this way, never as a string.
func SameFolder(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

// folderAtOrAbove returns the nearest folder at or above path that is the folder target names. It
// compares folders, not spellings, so neither a link nor a case-insensitive disk hides a match.
func folderAtOrAbove(path, target string) (string, bool) {
	want, err := os.Stat(target)
	if err != nil {
		return "", false
	}
	for dir := path; ; dir = filepath.Dir(dir) {
		if info, err := os.Stat(dir); err == nil && os.SameFile(info, want) {
			return dir, true
		}
		if filepath.Dir(dir) == dir {
			return "", false
		}
	}
}

// FindRepo returns the nearest folder at or above the working directory that holds a .git; ok is
// false when none does. It never asks git: git's answer follows core.worktree and other settings in
// .git/config, which a box can write, so a planted core.worktree would have the next launch mount
// whatever it names, the user's home folder say, as the workspace. Like git, it refuses a
// repository another user owns. It also never climbs to a repository at the home folder or above
// it: a box that deletes its own .git moves the walk up to whatever repository holds it, a home
// folder kept in Git among them, and only the override can name one of those. The nearest .git
// wins even when a box planted it inside the real project; the .coopignore rules of the folders
// above still hide what they name there (shadowpath).
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
	if home, above := shadowpath.HomeOrAbove(root); above {
		return "", false, fmt.Errorf("the nearest Git repository to %s is %s, at your home folder %s or above it, and %s", wd, root, home, homeProjectRefusal)
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
