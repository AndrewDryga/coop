package box

import (
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/shadowpath"
)

// MountKind distinguishes the ways a path enters (or is blocked from) the box.
type MountKind int

const (
	// Bind binds a host path to a box path (the repo at the workdir).
	Bind MountKind = iota
	// DirDecoy overlays an empty read-only directory, shadowing a secret directory.
	DirDecoy
	// Decoy overlays an empty read-only file, shadowing a secret file.
	Decoy
	// Policy overlays a readable .coopignore file as read-only.
	Policy
)

// Mount is one entry in the container's filesystem plan.
type Mount struct {
	Kind   MountKind
	Source string // host path (Bind and Policy only)
	Target string // path inside the box
	RO     bool   // read-only bind
}

// ComputeMounts is the security core: it returns the mounts that bind the repo
// into the box at workdir and shadow every secret path beneath it. The first
// mount is always the repo bind; later mounts shadow a secret (DirDecoy for a
// directory, Decoy for a file) or protect a .coopignore policy file. Secret directories
// are not descended into, so a shadowed dir hides all of its contents at once. The repo's
// .git is skipped. The repo itself must be a folder: a link or a file is refused, because
// the walk would bind it without looking inside.
//
// A path is shadowed when its basename matches SecretGlobs (unless AllowGlobs whitelists
// it — templates and public CA bundles stay visible by default), OR a .coopignore — in the
// repo root, any ancestor directory of the path, or a folder above the repo short of the home
// folder — matches it (its basename patterns apply anywhere in that directory's subtree; its
// path patterns are relative to that directory). An explicit .coopignore match is
// authoritative: it re-hides even an AllowGlobs-whitelisted name. A .coopignore it cannot read
// stops the launch (UnreadableCoopignore) at the first path under it that the other rules leave
// visible, rather than leave what it names visible.
//
// Its only inputs are the repo tree, the .coopignore files in it and in the folders above it
// (shadowpath.enclosingPolicies), and $HOME, which bounds that walk (no container runtime, no
// temp files), so it can be exhaustively unit-tested; the tests pin HOME so a stray .coopignore
// above the machine's temp folder can't change their answers. This is the function that must
// never let a secret leak.
func ComputeMounts(repo, workdir string) ([]Mount, error) {
	shadowed := NewShadowDecider(repo)
	mounts := []Mount{{Kind: Bind, Source: repo, Target: workdir}}
	err := filepath.WalkDir(repo, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == repo {
			// The walk never follows a link, so a linked root would be bound whole with not one
			// secret in it hidden. ResolveRepo follows a linked override; one still a link here
			// is one it could not follow.
			if !d.IsDir() {
				return fmt.Errorf("the repository %s is a link or a file, not a folder, so Coop cannot hide the secrets in it: "+
					"set COOP_REPO to the project folder itself", repo)
			}
			return nil // never shadow the repo root itself
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		rel, err := filepath.Rel(repo, p)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		target := workdir + "/" + relSlash
		if d.Name() == CoopIgnoreFile && d.Type().IsRegular() {
			mounts = append(mounts, Mount{Kind: Policy, Source: p, Target: target, RO: true})
			return nil
		}
		if hidden, err := shadowed(relSlash); err != nil || !hidden {
			return err
		}
		if d.IsDir() {
			mounts = append(mounts, Mount{Kind: DirDecoy, Target: target, RO: true})
			return fs.SkipDir // prune: a shadowed dir hides everything within it
		}
		mounts = append(mounts, Mount{Kind: Decoy, Target: target, RO: true})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return mounts, nil
}

// NewShadowDecider returns a predicate reporting whether a repo-relative slash path is
// shadowed from the box: its basename matches SecretGlobs (and AllowGlobs doesn't whitelist
// it), or a .coopignore in the root, an ancestor directory or a folder above the repo short of
// the home folder matches it (AllowGlobs does NOT override an explicit .coopignore — it's the
// user's final say). Each directory's .coopignore is loaded once into the closure's cache.
// ComputeMounts and check-secrets share this visibility decision; the scanner still checks
// hidden commit candidates because hiding a file from the box does not prevent committing it.
// A hidden directory hides every descendant, including names an allow rule would otherwise
// rescue. A path under a .coopignore coop cannot read comes back hidden with an
// UnreadableCoopignore, which every caller that can refuse returns, unless a readable rule or a
// secret name, its own or a folder's above it, already hides it.
func NewShadowDecider(repo string) func(relSlash string) (bool, error) {
	return shadowpath.NewDecider(repo)
}

// ShadowCount is the number of secret paths shadowed.
func ShadowCount(mounts []Mount) int {
	n := 0
	for _, m := range mounts {
		if m.Kind == Decoy || m.Kind == DirDecoy {
			n++
		}
	}
	return n
}

// RenderMounts turns a mount plan into container-runtime arguments. decoyFile and decoyDir
// are the shared empty read-only file and directory used to shadow secret files and dirs.
func RenderMounts(mounts []Mount, decoyFile, decoyDir string) []string {
	var args []string
	for _, m := range mounts {
		switch m.Kind {
		case Bind:
			spec := m.Source + ":" + m.Target
			if m.RO {
				spec += ":ro"
			}
			args = append(args, "-v", spec)
		case DirDecoy:
			// A read-only empty-dir bind, not --tmpfs: as a -v mount it sorts with the
			// repo bind by destination, so the repo bind can't re-cover it.
			args = append(args, "-v", decoyDir+":"+m.Target+":ro")
		case Decoy:
			args = append(args, "-v", decoyFile+":"+m.Target+":ro")
		case Policy:
			args = append(args, "-v", m.Source+":"+m.Target+":ro")
		}
	}
	return args
}

func matchesAny(name string, globs []string) bool {
	return shadowpath.MatchesAny(name, globs)
}
