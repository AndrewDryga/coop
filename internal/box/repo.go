package box

import (
	"crypto/sha256"
	"encoding/hex"
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

// ServicesProject is the deterministic per-REPO name: a lowercased, sanitized basename. It is the
// per-project IMAGE tag — legitimately shared across clones of the same repo (same Dockerfile →
// same image), so it stays basename-only.
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
// image tag (ServicesProject), which stays repo-based.
func ComposeProject(workspacePath string) string {
	canon := canonicalWorkspace(workspacePath)
	sum := sha256.Sum256([]byte(canon))
	return ServicesProject(canon) + "-" + hex.EncodeToString(sum[:])[:8]
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

// ComposeFileRel is the DEFAULT repo-relative compose path (project.DefaultCompose) — the scaffold
// write location under .agent/. At runtime ComposeFile honors box.compose from project.yaml,
// falling back here.
const ComposeFileRel = project.DefaultCompose

// ComposeFile returns the sibling-services compose file, or "" if it's absent or empty (a zero-byte
// file declares no services). It completes the config-source/runtime-identity split: the relative
// PATH (box.compose, else .agent/compose.yml) is trusted config, read from policyRepo, while the
// FILE itself is read from the workspace at that path — so a fork uses the parent's committed choice
// of WHERE the compose file lives, but its OWN copy of the file. For a plain repo pass repo twice.
func ComposeFile(workspace, policyRepo string) string {
	f := filepath.Join(workspace, filepath.FromSlash(project.ComposePath(policyRepo)))
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
