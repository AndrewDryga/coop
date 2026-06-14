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

// ComposeFile returns the repo's sibling-services compose file, or "" if none.
func ComposeFile(repo string) string {
	for _, f := range []string{
		filepath.Join(repo, "compose.agent.yml"),
		filepath.Join(repo, ".agent", "compose.yml"),
	} {
		if fileExists(f) {
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
