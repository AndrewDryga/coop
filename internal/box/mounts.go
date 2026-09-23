package box

import (
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
// .git is skipped.
//
// A path is shadowed when its basename matches SecretGlobs (unless AllowGlobs whitelists
// it — templates and public CA bundles stay visible by default), OR a .coopignore — in the
// repo root or any ancestor directory of the path — matches it (its basename patterns apply
// anywhere in that directory's subtree; its path patterns are relative to that directory). An
// explicit .coopignore match is authoritative: it re-hides even an AllowGlobs-whitelisted name.
//
// Its only input is the repo tree plus the .coopignore files in it (no container
// runtime, no temp files), so it can be exhaustively unit-tested — this is the
// function that must never let a secret leak.
func ComputeMounts(repo, workdir string) ([]Mount, error) {
	shadowed := NewShadowDecider(repo)
	mounts := []Mount{{Kind: Bind, Source: repo, Target: workdir}}
	err := filepath.WalkDir(repo, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == repo {
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
		if !shadowed(relSlash) {
			return nil
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
// it), or a .coopignore in the root or an ancestor directory matches it (AllowGlobs does NOT
// override an explicit .coopignore — it's the user's final say). Each directory's .coopignore
// is loaded once into the closure's cache. ComputeMounts and check-secrets share this
// visibility decision; the scanner still checks hidden commit candidates because
// hiding a file from the box does not prevent committing it. A hidden directory
// hides every descendant, including names an allow rule would otherwise rescue.
func NewShadowDecider(repo string) func(relSlash string) bool {
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
