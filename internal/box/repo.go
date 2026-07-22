package box

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/project"
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

// ComposeFileRel is the DEFAULT repo-relative compose path (project.DefaultCompose) — the scaffold
// write location under .agent/. At runtime ComposeFile honors box.compose from project.yaml,
// falling back here.
const ComposeFileRel = project.DefaultCompose

// ComposeFile returns the repo's sibling-services compose file (box.compose, else .agent/compose.yml),
// or "" if it's absent or empty — a zero-byte file declares no services, so auto-running
// `compose up` would error. The path is validated in-repo by project.Load.
func ComposeFile(repo string) string {
	f := filepath.Join(repo, filepath.FromSlash(project.ComposePath(repo)))
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
