package box

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ResolveRepo returns the repo root to operate on: the override if set, else the
// git top-level of the working directory, else the working directory itself.
func ResolveRepo(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		if top := strings.TrimSpace(string(out)); top != "" {
			return top, nil
		}
	}
	return os.Getwd()
}

// ServicesProject is the deterministic compose project name for a repo: a
// lowercased, sanitized basename. It also doubles as the per-project image tag.
func ServicesProject(repo string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(filepath.Base(repo)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return "coop-" + b.String()
}

// composeFileRels are the repo-relative (slash-form) paths coop recognizes as a
// sibling-services compose file, in priority order. It is the single source of truth:
// ComposeFile picks the first that exists to auto-run on the HOST daemon, and
// ComposeDecoyMounts shadows exactly these paths read-only in the box so an in-box agent
// can never author one. Keep the two uses in sync by editing only this list.
var composeFileRels = []string{
	"compose.agent.yml",
	".agent/compose.yml",
}

// ComposeFile returns the repo's sibling-services compose file, or "" if none. A zero-byte
// file counts as none: it declares no services, so auto-running `compose up` on it only errors
// — and coop's own compose decoy can leave an empty mountpoint behind (see composeDecoyStrays),
// which must never be mistaken for a real compose file.
func ComposeFile(repo string) string {
	for _, rel := range composeFileRels {
		f := filepath.Join(repo, filepath.FromSlash(rel))
		if fi, err := os.Stat(f); err == nil && !fi.IsDir() && fi.Size() > 0 {
			return f
		}
	}
	return ""
}

// composeDecoyStrays returns the host paths, under repo, of the compose decoys that do not
// exist yet. ComposeDecoyMounts shadows every composeFileRels path read-only in the box, and
// Docker materializes a bind-mount target that's missing; because those targets sit inside the
// read-write repo bind, an absent one lands in the repo on the host and outlives the container
// — a stray empty compose.agent.yml. box.Run snapshots these before the run and removes the
// empty ones after (removeEmptyStrays), so a launch leaves none behind. A path that already
// exists is a real compose file and is not returned.
func composeDecoyStrays(repo string) []string {
	var strays []string
	for _, rel := range composeFileRels {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if _, err := os.Stat(p); os.IsNotExist(err) {
			strays = append(strays, p)
		}
	}
	return strays
}

// removeComposeStrays deletes, under repo, each snapshot path Docker left as an empty mountpoint
// for a compose decoy, plus any directory it had to create for one (e.g. .agent/ for
// .agent/compose.yml) that is now empty. A path that gained content — a real compose file
// authored on the host meanwhile — is left untouched, and so is any directory that still holds
// other files (os.Remove only clears an empty one). snapshot is the pre-run composeDecoyStrays.
func removeComposeStrays(repo string, snapshot []string) {
	sep := string(filepath.Separator)
	for _, p := range snapshot {
		if fi, err := os.Stat(p); err != nil || fi.IsDir() || fi.Size() != 0 {
			continue // gone, a dir, or a real (non-empty) compose file — leave it
		}
		if os.Remove(p) != nil {
			continue
		}
		// Prune now-empty ancestor dirs coop caused, stopping at the repo root and at the first
		// directory that still has entries (a real .agent/ with tasks fails os.Remove and stays).
		for d := filepath.Dir(p); d != repo && strings.HasPrefix(d, repo+sep); d = filepath.Dir(d) {
			if os.Remove(d) != nil {
				break
			}
		}
	}
}

// fileExists reports whether path is an existing regular file (not a directory).
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
