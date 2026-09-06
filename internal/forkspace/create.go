package forkspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Setup creates the clone and its branch (the git half of `coop fork <name>`, with no
// agent run — so the lifecycle is testable without a container).
func Setup(repo, name string) (string, error) {
	return SetupContext(context.Background(), repo, name)
}

func SetupContext(ctx context.Context, repo, name string) (ws string, err error) {
	ws = Workspace(repo, name)
	if err := os.MkdirAll(Home(repo), 0o755); err != nil {
		return ws, err
	}
	if err := GitCloneContext(ctx, repo, ws); err != nil {
		return ws, fmt.Errorf("couldn't clone the repo into the fork workspace: %w", err)
	}
	complete := false
	defer func() {
		if complete {
			return
		}
		if cleanupErr := os.RemoveAll(ws); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove incomplete fork workspace %s: %w", ws, cleanupErr))
		}
	}()
	checkoutErr := gitCheckoutNewBranchContext(ctx, ws, name)
	if ctx.Err() != nil {
		return ws, errors.Join(ctx.Err(), checkoutErr)
	}
	branch, branchErr := gitOutputContext(ctx, ws, "symbolic-ref", "--quiet", "--short", "HEAD")
	if branchErr != nil {
		return ws, fmt.Errorf("verify fork branch %q: %w", name, branchErr)
	}
	if branch != name {
		if checkoutErr != nil {
			return ws, fmt.Errorf("check out fork branch %q: %w", name, checkoutErr)
		}
		return ws, fmt.Errorf("check out fork branch %q: HEAD is on %q", name, branch)
	}
	if err := propagateGitEnvContext(ctx, repo, ws); err != nil {
		return ws, err
	}
	if err := Exclude(ws, ".coop/"); err != nil { // trusted setup only; never re-open agent-writable .git metadata later
		return ws, fmt.Errorf("exclude fork bookkeeping: %w", err)
	}
	complete = true
	return ws, nil
}

// propagateGitEnvContext carries the parent's git environment into a fresh fork. A clone
// keeps no local identity and the box has no ambient ~/.gitconfig, so without this an
// agent couldn't commit and the user's global ignores wouldn't apply:
//   - user.name / user.email — so the agent's commits have an author;
//   - the global gitignore (core.excludesfile) content into .git/info/exclude — git's
//     local, uncommitted ignore file, so no host config path dangles inside the box.
func propagateGitEnvContext(ctx context.Context, repo, ws string) error {
	if err := PropagateGitIdentityContext(ctx, repo, ws); err != nil {
		return err
	}
	// Signing materials (key + format) travel to the fork so commits can be signed
	// with your key when they're rebased on land — on the host, where the key lives.
	// commit.gpgsign is deliberately NOT copied: the keyless box must commit unsigned.
	for _, k := range []string{"user.signingkey", "gpg.format"} {
		v, ok, err := gitConfigContext(ctx, repo, k)
		if err != nil {
			return fmt.Errorf("read parent Git %s: %w", k, err)
		}
		if ok && v != "" {
			if err := GitRefCommand(ctx, ws, "config", k, v).Run(); err != nil { // the fork's own config: the real git dir
				return fmt.Errorf("set fork Git %s: %w", k, err)
			}
		}
	}
	// Read core.excludesfile from your GLOBAL config, never the agent-writable repo: a poisoned
	// repo could otherwise point it at a host secret (e.g. ~/.ssh/id_rsa) and we'd copy that file's
	// content into the fork the agent reads. `--path` expands a leading ~ in the configured path.
	if gi := gitGlobalOutContext(ctx, "--path", "core.excludesfile"); gi != "" {
		if data, err := os.ReadFile(gi); err == nil && len(data) > 0 {
			excl := filepath.Join(ws, ".git", "info", "exclude")
			if err := appendFile(excl, append([]byte("\n# carried from your global core.excludesfile\n"), data...)); err != nil {
				return fmt.Errorf("carry global Git excludes into fork: %w", err)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// PropagateGitIdentity gives a clone the trusted parent's resolved commit identity. Git clone does
// not copy local config, and a preview rebase must work even when the host has no global identity.
func PropagateGitIdentity(repo, ws string) error {
	return PropagateGitIdentityContext(context.Background(), repo, ws)
}

func PropagateGitIdentityContext(ctx context.Context, repo, ws string) error {
	for _, key := range []string{"user.email", "user.name"} {
		value, ok, err := gitConfigContext(ctx, repo, key)
		if err != nil {
			return fmt.Errorf("read parent Git %s: %w", key, err)
		}
		if ok && value != "" {
			if err := GitRefCommand(ctx, ws, "config", key, value).Run(); err != nil {
				return fmt.Errorf("set fork Git %s: %w", key, err)
			}
		}
	}
	return nil
}

// Exclude appends a pattern to the fork's local .git/info/exclude (git's uncommitted
// ignore file) if absent, so coop's per-fork bookkeeping never shows in a review diff or
// lands on merge.
func Exclude(ws, pattern string) error {
	excl := filepath.Join(ws, ".git", "info", "exclude")
	if data, err := os.ReadFile(excl); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == pattern {
				return nil
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return appendFile(excl, []byte("\n# coop: per-fork state, never committed\n"+pattern+"\n"))
}

func appendFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	return errors.Join(writeErr, f.Close())
}

// Destroy removes a fork's workspace and its review/<name> ref, then prunes an empty forks home.
// Best-effort on the ref so it works for partially-built forks.
//
// The fork's sibling SERVICES are the caller's job, before this (internal/cli's destroyFork):
// teardown is driven by the fork's own compose file, so once the workspace is deleted there is
// nothing left to drive it.
func Destroy(repo, name string) error {
	_ = GitRefCommand(context.Background(), repo, "branch", "-q", "-D", "review/"+name).Run() // a packed ref: the real git dir, never the view
	if err := os.RemoveAll(Workspace(repo, name)); err != nil {
		return err
	}
	if entries, _ := os.ReadDir(Home(repo)); len(entries) == 0 {
		_ = os.Remove(Home(repo))
	}
	return nil
}
