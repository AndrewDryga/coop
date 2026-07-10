package box

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// MountKind distinguishes the three ways a path enters (or is blocked from) the box.
type MountKind int

const (
	// Bind binds a host path to a box path (the repo at the workdir).
	Bind MountKind = iota
	// DirDecoy overlays an empty read-only directory, shadowing a secret directory.
	DirDecoy
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
// mount is always the repo bind; each later mount shadows a secret (DirDecoy for a
// directory, Decoy for a file). Secret directories are not descended into, so a
// shadowed dir hides all of its contents at once. The repo's .git is skipped.
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
		if !shadowed(relSlash) {
			return nil
		}
		target := workdir + "/" + relSlash
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

// ComposeDecoyMounts returns a read-only file decoy for every sibling-services compose
// path (composeFileRels), applied UNCONDITIONALLY — whether or not one exists on the host.
//
// This is the compose auto-up gate. coop auto-runs a compose file on the HOST docker daemon
// on every networked launch (EnsureServices → `compose up`), so an in-box agent that could
// write one would get host-root via `privileged: true` + a host bind mount. The agent's only
// write channel to that host path is the read-write repo bind, so shadowing the path
// read-only in the box removes the channel: the host can then only ever run a compose file
// authored OUTSIDE the box — by the human on the host, which IS the authorization. Same trust
// model as the secret decoys in ComputeMounts, and an allowlist-by-construction (the agent
// can't write the file) rather than a deny-list that must enumerate every dangerous directive.
//
// Kept separate from ComputeMounts so it doesn't inflate that function's secret ShadowCount,
// and unconditional (not gated on existence) because the threat is the agent CREATING a file
// that isn't there yet — Docker creates the mountpoint if absent.
func ComposeDecoyMounts(workdir string) []Mount {
	mounts := make([]Mount, 0, len(composeFileRels))
	for _, rel := range composeFileRels {
		mounts = append(mounts, Mount{Kind: Decoy, Target: workdir + "/" + rel, RO: true})
	}
	return mounts
}

// NewShadowDecider returns a predicate reporting whether a repo-relative slash path is
// shadowed from the box: its basename matches SecretGlobs (and AllowGlobs doesn't whitelist
// it), or a .coopignore in the root or an ancestor directory matches it (AllowGlobs does NOT
// override an explicit .coopignore — it's the user's final say). Each directory's .coopignore
// is loaded once into the closure's cache. ComputeMounts (the
// mount plan) and `coop check-secrets` (the scanner) share this single rule so "what the
// box can see" can never drift between them — scanning a path the box hides is pointless,
// and a secret that IS shadowed is already protected.
func NewShadowDecider(repo string) func(relSlash string) bool {
	cache := map[string]UserGlobs{} // dir (slash-rel, "" = root) → its .coopignore, loaded once
	loadDir := func(dirRel string) UserGlobs {
		if g, ok := cache[dirRel]; ok {
			return g
		}
		g := LoadUserGlobs(filepath.Join(repo, filepath.FromSlash(dirRel)))
		cache[dirRel] = g
		return g
	}
	return func(relSlash string) bool {
		name := relSlash
		if i := strings.LastIndexByte(relSlash, '/'); i >= 0 {
			name = relSlash[i+1:]
		}
		// A secret-named file stays visible only if an allow rule rescues it: an EXACT known-public
		// name (a CA bundle, which overrides even *.pem), or a template/sample SUFFIX — but a
		// suffix can never un-shadow a private-key pattern, so id_rsa.example stays shadowed. Allow
		// rules override only built-in SecretGlobs false positives, never an explicit .coopignore
		// (the user's authoritative hide rule).
		// Match the built-in denylist case-insensitively so a case variant (.ENV, ID_RSA, *.PEM)
		// can't slip past — important on a case-insensitive host FS (macOS/Windows). The user's
		// .coopignore stays case-sensitive (their patterns, their casing).
		lname := strings.ToLower(name)
		allowed := matchesAny(lname, AllowGlobs) ||
			(matchesAny(lname, allowTemplateGlobs) && !matchesAny(lname, hardSecretGlobs))
		byDefault := matchesAny(lname, SecretGlobs) && !allowed
		return byDefault || shadowedByCoopignore(relSlash, loadDir)
	}
}

// shadowedByCoopignore reports whether the repo-relative slash path is shadowed by a
// .coopignore in the root or any ancestor directory of the path: each directory's
// basename patterns match anywhere in its subtree, and its path patterns are matched
// against the path relative to that directory (so sub/.coopignore's "config/x" means
// sub/config/x). loadDir caches the per-directory globs.
func shadowedByCoopignore(relSlash string, loadDir func(string) UserGlobs) bool {
	base := relSlash
	if i := strings.LastIndexByte(relSlash, '/'); i >= 0 {
		base = relSlash[i+1:]
	}
	dir, remaining := "", relSlash
	for {
		g := loadDir(dir)
		if matchesAny(base, g.Base) || matchesPath(remaining, g.Path) {
			return true
		}
		i := strings.IndexByte(remaining, '/')
		if i < 0 {
			return false
		}
		if dir == "" {
			dir = remaining[:i]
		} else {
			dir += "/" + remaining[:i]
		}
		remaining = remaining[i+1:]
	}
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
			// repo bind by destination on every runtime, so the repo bind can't re-cover
			// it. (podman applies --tmpfs in a separate pass, which re-exposed the dir.)
			args = append(args, "-v", decoyDir+":"+m.Target+":ro")
		case Decoy:
			args = append(args, "-v", decoyFile+":"+m.Target+":ro")
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

// matchesPath reports whether a repo-relative slash path matches any of the path
// patterns (filepath.Match semantics: `*` does not cross `/`, no `**`).
func matchesPath(relSlash string, globs []string) bool {
	for _, g := range globs {
		if ok, _ := filepath.Match(g, relSlash); ok {
			return true
		}
	}
	return false
}
