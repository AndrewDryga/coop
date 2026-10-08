package forkspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Setup creates the clone and its branch (the git half of `coop fork <name>`, with no
// agent run — so the lifecycle is testable without a container).
func Setup(repo, name string) (string, error) {
	return SetupContext(context.Background(), repo, name)
}

func SetupContext(ctx context.Context, repo, name string) (ws string, err error) {
	return setupContext(ctx, repo, name, "", false)
}

// SetupPinnedContext creates a session workspace at an already-validated commit without using
// Git's racy local hardlink clone optimization. Ordinary forks keep SetupContext's faster clone.
func SetupPinnedContext(ctx context.Context, repo, name, commit string) (ws string, err error) {
	if !validPinnedCommit(commit) {
		return "", errors.New("invalid pinned commit")
	}
	return setupContext(ctx, repo, name, commit, false)
}

// SetupIsolatedContext additionally materializes independently owned unchanged
// submodules and LFS payloads, without running checkout drivers from host config.
func SetupIsolatedContext(ctx context.Context, repo, name, commit string) (string, error) {
	if err := QualifyDefaultLFSStorage(ctx, repo); err != nil {
		return "", err
	}
	if !validPinnedCommit(commit) {
		return "", errors.New("invalid isolated commit")
	}
	return setupContext(ctx, repo, name, commit, true)
}

func setupContext(ctx context.Context, repo, name, commit string, isolated bool) (ws string, err error) {
	ws = Workspace(repo, name)
	// Production callers hold the fork lifecycle lock. Refuse before cloning so an interrupted
	// session discard cannot be hidden by a new, unanchored workspace at the same public name.
	// EnsureGenerationLocked repeats the check at the authority-publication boundary.
	if ValidExistingName(name) {
		if err := RequireForkNameAvailable(repo, name); err != nil {
			return ws, err
		}
	}
	if err := ensureForkHome(repo); err != nil {
		return ws, err
	}
	clone := GitCloneContext
	if commit != "" {
		clone = func(ctx context.Context, src, dst string) error {
			return GitClonePinnedContext(ctx, src, dst, commit)
		}
	}
	if err := clone(ctx, repo, ws); err != nil {
		return ws, fmt.Errorf("couldn't clone the repo into the fork workspace: %w", err)
	}
	complete := false
	defer func() {
		if complete {
			return
		}
		if cleanupErr := os.RemoveAll(ws); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove incomplete fork workspace %s: %w", ws, cleanupErr))
		} else if syncErr := confirmForkDirectoryEntry(ws); syncErr != nil {
			err = errors.Join(err, fmt.Errorf("confirm removal of incomplete fork workspace %s: %w", ws, syncErr))
		}
	}()
	// The clone's directory name is the workspace users and later generation authority rely on.
	// Sync its parent before returning or publishing an identity; on failure the defer removes the
	// volatile clone and confirms that absence so a retry starts from one unambiguous state.
	if err := confirmForkDirectoryEntry(ws); err != nil {
		return ws, fmt.Errorf("confirm fork workspace creation: %w", err)
	}
	var checkoutErr error
	if isolated {
		checkoutErr = CheckoutIndependent(ctx, ws, commit, name)
	} else if commit == "" {
		checkoutErr = gitCheckoutNewBranchContext(ctx, ws, name)
	} else if checkoutErr = GitRefCommand(ctx, ws, "update-ref", "refs/heads/"+name, commit).Run(); checkoutErr == nil {
		checkoutErr = GitSwitchBranch(ctx, ws, name)
	}
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
	if isolated {
		if checkoutErr != nil {
			return ws, fmt.Errorf("isolated checkout: %w", checkoutErr)
		}
		if err := MaterializeIndependentTree(ctx, repo, ws, commit, commit); err != nil {
			return ws, fmt.Errorf("isolated dependencies: %w", err)
		}
		if err := ValidateIndependentGit(ws); err != nil {
			return ws, err
		}
	}
	if err := Exclude(ws, ".coop/"); err != nil { // trusted setup only; never re-open agent-writable .git metadata later
		return ws, fmt.Errorf("exclude fork bookkeeping: %w", err)
	}
	if err := Exclude(ws, "/"+GenerationMarkerName); err != nil {
		return ws, fmt.Errorf("exclude fork identity marker: %w", err)
	}
	complete = true
	return ws, nil
}

func validPinnedCommit(commit string) bool {
	if len(commit) != 40 && len(commit) != 64 {
		return false
	}
	for _, r := range commit {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
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
			// Pinned clones disable templates, so Git has not created info/ yet.
			if err := os.MkdirAll(filepath.Dir(excl), 0o755); err != nil {
				return fmt.Errorf("create fork Git excludes directory: %w", err)
			}
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
	identity, exists, err := ReadGeneration(repo, name)
	if err != nil {
		return err
	}
	isolated := false
	if exists {
		isolated, err = IsolatedGeneration(repo, identity)
		if err != nil {
			return err
		}
	}
	if !isolated {
		_ = GitRefCommand(context.Background(), repo, "branch", "-q", "-D", "review/"+name).Run() // ordinary forks own this parent ref; isolated forks do not
	}
	if err := os.RemoveAll(Workspace(repo, name)); err != nil {
		return err
	}
	if err := confirmForkDirectoryEntry(Workspace(repo, name)); err != nil {
		return fmt.Errorf("confirm fork workspace removal: %w", err)
	}
	home := Home(repo)
	if entries, _ := os.ReadDir(home); len(entries) == 0 {
		if err := os.Remove(home); err == nil {
			if err := confirmForkDirectoryEntry(home); err != nil {
				return fmt.Errorf("confirm empty fork home removal: %w", err)
			}
		}
	}
	return nil
}
