package forkspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Setup creates the clone and its branch (the git half of `coop fork <name>`, with no
// agent run — so the lifecycle is testable without a container).
func Setup(repo, name string) (string, error) {
	ws := Workspace(repo, name)
	if err := os.MkdirAll(Home(repo), 0o755); err != nil {
		return ws, err
	}
	if err := GitClone(repo, ws); err != nil {
		return ws, fmt.Errorf("couldn't clone the repo into the fork workspace: %w", err)
	}
	_ = gitCheckoutNewBranch(ws, name) // branch may already exist in origin; fine
	propagateGitEnv(repo, ws)
	Exclude(ws, ".coop/") // trusted setup only; never re-open agent-writable .git metadata later
	return ws, nil
}

// propagateGitEnv carries the parent's git environment into a fresh fork. A clone
// keeps no local identity and the box has no ambient ~/.gitconfig, so without this an
// agent couldn't commit and the user's global ignores wouldn't apply:
//   - user.name / user.email — so the agent's commits have an author;
//   - the global gitignore (core.excludesfile) content into .git/info/exclude — git's
//     local, uncommitted ignore file, so no host config path dangles inside the box.
func propagateGitEnv(repo, ws string) {
	PropagateGitIdentity(repo, ws)
	// Signing materials (key + format) travel to the fork so commits can be signed
	// with your key when they're rebased on land — on the host, where the key lives.
	// commit.gpgsign is deliberately NOT copied: the keyless box must commit unsigned.
	for _, k := range []string{"user.signingkey", "gpg.format"} {
		if v := gitOut(repo, "config", "--get", k); v != "" {
			_ = gitRun(ws, "config", k, v)
		}
	}
	// Read core.excludesfile from your GLOBAL config, never the agent-writable repo: a poisoned
	// repo could otherwise point it at a host secret (e.g. ~/.ssh/id_rsa) and we'd copy that file's
	// content into the fork the agent reads. `--path` expands a leading ~ in the configured path.
	if gi := gitGlobalOut("--path", "core.excludesfile"); gi != "" {
		if data, err := os.ReadFile(gi); err == nil && len(data) > 0 {
			excl := filepath.Join(ws, ".git", "info", "exclude")
			if f, err := os.OpenFile(excl, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				_, _ = f.WriteString("\n# carried from your global core.excludesfile\n")
				_, _ = f.Write(data)
				_ = f.Close()
			}
		}
	}
}

// PropagateGitIdentity gives a clone the trusted parent's resolved commit identity. Git clone does
// not copy local config, and a preview rebase must work even when the host has no global identity.
func PropagateGitIdentity(repo, ws string) {
	if email := gitOut(repo, "config", "user.email"); email != "" {
		_ = gitRun(ws, "config", "user.email", email)
	}
	if name := gitOut(repo, "config", "user.name"); name != "" {
		_ = gitRun(ws, "config", "user.name", name)
	}
}

// Exclude appends a pattern to the fork's local .git/info/exclude (git's uncommitted
// ignore file) if absent, so coop's per-fork bookkeeping never shows in a review diff or
// lands on merge.
func Exclude(ws, pattern string) {
	excl := filepath.Join(ws, ".git", "info", "exclude")
	if data, err := os.ReadFile(excl); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == pattern {
				return
			}
		}
	}
	if f, err := os.OpenFile(excl, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.WriteString("\n# coop: per-fork state, never committed\n" + pattern + "\n")
		_ = f.Close()
	}
}

// Destroy removes a fork's workspace and its review/<name> ref, then prunes an empty forks home.
// Best-effort on the ref so it works for partially-built forks.
//
// The fork's sibling SERVICES are the caller's job, before this (internal/cli's destroyFork):
// teardown is driven by the fork's own compose file, so once the workspace is deleted there is
// nothing left to drive it.
func Destroy(repo, name string) error {
	_ = gitRun(repo, "branch", "-q", "-D", "review/"+name)
	if err := os.RemoveAll(Workspace(repo, name)); err != nil {
		return err
	}
	if entries, _ := os.ReadDir(Home(repo)); len(entries) == 0 {
		_ = os.Remove(Home(repo))
	}
	return nil
}
