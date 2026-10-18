package tasks

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
)

// LockRefAuthority makes the short validate→finalize→consume window exclusive PER WORKTREE: from the
// moment a completion's HEAD is trusted through the moment task authority is actually consumed for
// it (a receipt, an audit-reopen generation), nothing else may move that ref. An interactive `coop
// run`, a host-side signing rewrite (signUnpushed), a fork land (fastForwardParent), a host-driven
// completion of audit-reopened work (completeTrustedTask via `coop tasks done`), or a human commit
// can move HEAD. Coop's own mutators take this same lock for their ref-touching moments, so Coop
// cannot trigger its own refusal. It is keyed like lockLoopCheckout by resolved worktree path,
// letting parallel forks remain independent (isolate the state, don't serialize the workers).
func LockRefAuthority(cfg *config.Config, repo string) (func(), error) {
	checkout := repo
	if resolved, err := filepath.EvalSymlinks(checkout); err == nil {
		checkout = resolved
	} else if absolute, absErr := filepath.Abs(checkout); absErr == nil {
		checkout = absolute
	}
	release, holder, err := cfg.TryLock("ref", checkout)
	if errors.Is(err, config.ErrLockHeld) {
		if holder != "" {
			holder = " (" + holder + ")"
		}
		// Fail fast rather than block the loop forever.
		return nil, fmt.Errorf("another coop process is validating or consuming a completion in %s%s — wait for it to finish, then retry", checkout, holder)
	}
	return release, err
}

// ErrRefAuthorityMoved reports that HEAD moved between the value validation is about to trust and
// the first read inside the ref-authority window — the compare-and-swap failing closed. A sentinel
// (not just a formatted error) so callers can tell this apart from a lock-acquire failure and build
// their own message with the observed/expected SHAs.
var ErrRefAuthorityMoved = errors.New("ref authority: HEAD moved before validation could be trusted")

// EnterRefAuthorityWindow acquires the per-worktree ref-authority lock and, as the FIRST action
// inside it, re-reads HEAD and compares it against headAfter — the value a completion was validated
// against. A mismatch (or an unreadable HEAD) fails closed and releases the lock itself before
// returning, so a caller that gets an error never holds anything and never has to release. On success
// the caller owns the returned release func and must call it exactly once on every subsequent exit
// from the window — validation and consumption both happen inside this same held lock, which is what
// makes the compare meaningful: once it succeeds, nothing else can move HEAD until release runs.
//
// beforeWindow is a test seam: when non-nil it fires just before the lock is taken, so a test can
// interpose a concurrent HEAD move at the exact old validation-to-consumption gap. Production callers
// pass nil.
func EnterRefAuthorityWindow(cfg *config.Config, repo, headAfter string, beforeWindow func(repo, headAfter string)) (release func(), liveHead string, err error) {
	if beforeWindow != nil {
		beforeWindow(repo, headAfter) // test seam: a concurrent process moves HEAD here
	}
	release, err = LockRefAuthority(cfg, repo)
	if err != nil {
		return nil, "", fmt.Errorf("acquire ref authority for %s: %w", repo, err)
	}
	liveHead, err = forkspace.GitOutErr(repo, "rev-parse", "HEAD")
	if err != nil {
		release()
		return nil, "", fmt.Errorf("re-read HEAD of %s inside the ref authority window: %w", repo, err)
	}
	if liveHead != headAfter {
		release()
		return nil, liveHead, ErrRefAuthorityMoved
	}
	return release, liveHead, nil
}
