package tasks

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/config"
)

const refAuthorityIdentityAttempts = 5 // mirrors leaseAuthorityIdentityAttempts (lease.go)

var errRefAuthorityIdentity = errors.New("ref authority lock changed identity while locking")

// LockRefAuthority makes the short validate→finalize→consume window exclusive PER WORKTREE: from the
// moment a completion's HEAD is trusted through the moment task authority is actually consumed for
// it (a receipt, an audit-reopen generation), nothing else may move that ref. An interactive `coop
// run`, a host-side signing rewrite (signUnpushed), a fork land (fastForwardParent), a host-driven
// completion of audit-reopened work (completeTrustedTask, reached via `coop tasks done` / fork-merge
// reconciliation), or a human commit can all move HEAD — every one of coop's own mutators takes THIS
// SAME lock for its own ref-touching moment, so coop can never trigger its own refusal. Keyed
// identically to lockLoopCheckout — the resolved worktree path, never the repo name — so a fork
// fleet stays parallel (isolate the state, don't serialize the fleet).
//
// Unlike lockLoopCheckout's bare flock, this is proved after the fact, the same way
// lockLeaseAuthorityWith proves task-lease authority (lease.go): flock binds a process to an
// INODE, never a name, so a lock file removed and recreated between open and flock (a careless `rm
// -rf .locks`, a future migration) would let two controllers each hold an "exclusive" lock on a
// different inode. fstat(fd) vs a fresh lstat(path), compared with os.SameFile, is the only evidence
// the lock held is the lock every other controller contends for; a mismatch drops it and retries on
// the inode that answers the name now, bounded by refAuthorityIdentityAttempts.
func LockRefAuthority(cfg *config.Config, repo string) (func(), error) {
	// Production always carries a ConfigDir; an in-process test app may not, and joining "" would
	// drop a .locks/ directory into whatever the working directory happens to be.
	base := cfg.ConfigDir
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, ".locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	checkout := repo
	if resolved, err := filepath.EvalSymlinks(checkout); err == nil {
		checkout = resolved
	} else if absolute, absErr := filepath.Abs(checkout); absErr == nil {
		checkout = absolute
	}
	sum := sha256.Sum256([]byte(checkout))
	path := filepath.Join(dir, fmt.Sprintf("ref-%x.lock", sum[:12]))

	for attempt := 0; ; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			// Best effort only: flock is the authority, the file body just names the holder so the
			// operator doesn't have to walk a process tree to find a stuck one (fail fast, don't
			// block the loop forever).
			owner, _ := os.ReadFile(path)
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
				held := ""
				if who := strings.TrimSpace(string(owner)); who != "" {
					held = " (" + who + ")"
				}
				return nil, fmt.Errorf("another coop process is validating or consuming a completion in %s%s — wait for it to finish, then retry", checkout, held)
			}
			return nil, err
		}
		current, err := refAuthorityIsCurrent(f, path)
		if err != nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
			return nil, err
		}
		if !current {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
			if attempt+1 >= refAuthorityIdentityAttempts {
				return nil, fmt.Errorf("%w: %s", errRefAuthorityIdentity, path)
			}
			continue
		}
		if err := f.Truncate(0); err == nil {
			_, _ = f.WriteAt([]byte(fmt.Sprintf("pid %d\n", os.Getpid())), 0)
		}
		return func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		}, nil
	}
}

// refAuthorityIsCurrent proves, with the flock already held, that the locked inode is still the one
// path resolves to — see leaseAuthorityIsCurrent's identical technique in lease.go.
func refAuthorityIsCurrent(f *os.File, path string) (bool, error) {
	named, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // unlinked underfoot: the lock we hold now guards an orphan
	}
	if err != nil {
		return false, err
	}
	locked, err := f.Stat()
	if err != nil {
		return false, err
	}
	return os.SameFile(locked, named), nil
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
	liveHead, err = gitOutErr(repo, "rev-parse", "HEAD")
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
