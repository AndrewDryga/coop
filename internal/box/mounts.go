package box

import (
	"io/fs"
	"path/filepath"
)

// MountKind distinguishes the three ways a path enters (or is blocked from) the box.
type MountKind int

const (
	// Bind binds a host path to a box path (the repo at the workdir).
	Bind MountKind = iota
	// Tmpfs overlays an empty in-memory dir, shadowing a secret directory.
	Tmpfs
	// Decoy overlays an empty read-only file, shadowing a secret file.
	Decoy
)

// Mount is one entry in the container's filesystem plan.
type Mount struct {
	Kind   MountKind
	Source string // host path (Bind only)
	Target string // path inside the box
	RO     bool   // read-only bind
}

// ComputeMounts is the security core: it returns the mounts that bind the repo
// into the box at workdir and shadow every secret path beneath it. The first
// mount is always the repo bind; each later mount shadows a secret (Tmpfs for a
// directory, Decoy for a file). Secret directories are not descended into, so a
// shadowed dir hides all of its contents at once. The repo's .git is skipped.
//
// It is pure (no container runtime, no temp files) so it can be exhaustively
// unit-tested — this is the function that must never let a secret leak.
func ComputeMounts(repo, workdir string) ([]Mount, error) {
	mounts := []Mount{{Kind: Bind, Source: repo, Target: workdir}}
	err := filepath.WalkDir(repo, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == repo {
			return nil // never shadow the repo root itself
		}
		name := d.Name()
		if d.IsDir() && name == ".git" {
			return fs.SkipDir
		}
		if !matchesAny(name, SecretGlobs) || matchesAny(name, AllowGlobs) {
			return nil
		}
		rel, err := filepath.Rel(repo, path)
		if err != nil {
			return err
		}
		target := workdir + "/" + filepath.ToSlash(rel)
		if d.IsDir() {
			mounts = append(mounts, Mount{Kind: Tmpfs, Target: target})
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

// ShadowCount is the number of secret paths shadowed (everything but the bind).
func ShadowCount(mounts []Mount) int {
	n := 0
	for _, m := range mounts {
		if m.Kind != Bind {
			n++
		}
	}
	return n
}

// RenderMounts turns a mount plan into container-runtime arguments. decoy is the
// shared empty read-only file used to shadow secret files.
func RenderMounts(mounts []Mount, decoy string) []string {
	var args []string
	for _, m := range mounts {
		switch m.Kind {
		case Bind:
			spec := m.Source + ":" + m.Target
			if m.RO {
				spec += ":ro"
			}
			args = append(args, "-v", spec)
		case Tmpfs:
			args = append(args, "--tmpfs", m.Target)
		case Decoy:
			args = append(args, "-v", decoy+":"+m.Target+":ro")
		}
	}
	return args
}

func matchesAny(name string, globs []string) bool {
	for _, g := range globs {
		if ok, _ := filepath.Match(g, name); ok {
			return true
		}
	}
	return false
}
