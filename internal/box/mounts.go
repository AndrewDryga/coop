package box

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/config"
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
	// Spellings are the other letter-case spellings of Target's last name, each covered the same
	// way. A disk that ignores letter case (a Mac's) opens the file under any of them, and a cover
	// hides only the spelling it sits on.
	Spellings []string
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
//
// On a disk that ignores letter case (a Mac's), each hidden or policy file's mount also covers
// every other spelling of its name (Spellings), since the box opens the file under any of them. A
// name too long to cover that way comes back in an UncoveredSpellings error together with the
// mounts, for the launch to refuse unless its owner accepted the risk.
func ComputeMounts(repo, workdir string) ([]Mount, error) {
	shadowed := NewShadowDecider(repo)
	mounts := []Mount{{Kind: Bind, Source: repo, Target: workdir}}
	var budget spellingBudget
	var uncovered []string
	// spelled covers the other spellings of the file at p. A folder needs none: Linux keeps one
	// entry per folder, so CONFIG/ lands on config/'s cover.
	spelled := func(m Mount, p, relSlash string) Mount {
		spellings, ok := budget.covers(p, m.Target)
		if !ok {
			uncovered = append(uncovered, relSlash)
		}
		m.Spellings = spellings
		return m
	}
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
		if d.Type().IsRegular() && isCoopignore(p) {
			mounts = append(mounts, spelled(Mount{Kind: Policy, Source: p, Target: target, RO: true}, p, relSlash))
			return nil
		}
		if hidden, err := shadowed(relSlash); err != nil || !hidden {
			return err
		}
		if d.IsDir() {
			mounts = append(mounts, Mount{Kind: DirDecoy, Target: target, RO: true})
			return fs.SkipDir // prune: a shadowed dir hides everything within it
		}
		mounts = append(mounts, spelled(Mount{Kind: Decoy, Target: target, RO: true}, p, relSlash))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(uncovered) > 0 {
		return mounts, &UncoveredSpellings{Paths: uncovered}
	}
	return mounts, nil
}

// launchMounts is ComputeMounts for a launch. Hidden files a box could read under another letter
// case refuse it, unless its owner accepted that risk with COOP_ALLOW_CASE_EXPOSED_SECRETS; then
// they come back to be named at launch.
func launchMounts(cfg *config.Config, repo, workdir string) ([]Mount, []string, error) {
	mounts, err := ComputeMounts(repo, workdir)
	var uncovered *UncoveredSpellings
	if errors.As(err, &uncovered) && cfg.AllowCaseExposedSecrets {
		return mounts, uncovered.Paths, nil
	}
	return mounts, nil, err
}

// isCoopignore reports whether path is its folder's .coopignore as the secret decider reads it: that
// exact name, or another spelling that names the same file, such as .COOPIGNORE on a case-insensitive
// Mac disk. Matching the spelling alone left such a file writable in the box, where a box could
// weaken the rules the next launch reads.
func isCoopignore(path string) bool {
	name := filepath.Base(path)
	if name == CoopIgnoreFile {
		return true
	}
	if !IsCoopignoreName(name) {
		return false
	}
	spelled, err := os.Lstat(path)
	if err != nil {
		return false
	}
	read, err := os.Lstat(filepath.Join(filepath.Dir(path), CoopIgnoreFile))
	return err == nil && os.SameFile(spelled, read)
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
			for _, target := range m.targets() {
				args = append(args, "-v", decoyFile+":"+target+":ro")
			}
		case Policy:
			for _, target := range m.targets() {
				args = append(args, "-v", m.Source+":"+target+":ro")
			}
		}
	}
	return args
}

// targets is m's Target followed by the paths of its other spellings.
func (m Mount) targets() []string {
	targets := []string{m.Target}
	dir := path.Dir(m.Target)
	for _, spelling := range m.Spellings {
		targets = append(targets, path.Join(dir, spelling))
	}
	return targets
}
