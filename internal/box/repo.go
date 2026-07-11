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
// sibling-services compose file, in priority order. ComposeFile picks the first that exists to
// auto-run on the HOST daemon (validated first by box.ValidateComposeFile).
var composeFileRels = []string{
	"compose.agent.yml",
	".agent/compose.yml",
}

// ComposeFile returns the repo's sibling-services compose file, or "" if none. A zero-byte file
// counts as none: it declares no services, so auto-running `compose up` on it would only error.
func ComposeFile(repo string) string {
	for _, rel := range composeFileRels {
		f := filepath.Join(repo, filepath.FromSlash(rel))
		if fi, err := os.Stat(f); err == nil && !fi.IsDir() && fi.Size() > 0 {
			return f
		}
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
