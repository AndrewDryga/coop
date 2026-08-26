package tasks

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
)

// EnterRefAuthorityWindow is the compare-and-swap the work loop, signing, fork-land, and the
// audit-reopen completion path all share: acquire the lock, then re-read HEAD as the FIRST action
// inside it and compare against the value validation is about to trust.
//
// internal/cli/refauthority_test.go keeps four sibling tests (TestCmdTasksDoneTakesRefAuthority,
// TestFastForwardParentTakesRefAuthority, TestSignUnpushedTakesRefAuthority,
// TestReconcileQueueAfterMergeTakesRefAuthority) — those prove cli's own staying ref-touching
// mutators (fork_merge.go's fastForwardParent, sign.go's signUnpushed) take this same lock, so they
// stay where those mutators live. These twelve tests were one file before the extraction; this
// split follows Risk 5 of this task's spec.md exactly.

func TestEnterRefAuthorityWindowMatchesLiveHead(t *testing.T) {
	repo, run := gitRepo(t)
	run("commit", "-q", "--allow-empty", "-m", "base")
	head := gitOut(repo, "rev-parse", "HEAD")

	cfg := &config.Config{ConfigDir: t.TempDir()}
	release, live, err := EnterRefAuthorityWindow(cfg, repo, head, nil)
	if err != nil || release == nil || live != head {
		t.Fatalf("EnterRefAuthorityWindow(matching) = (release=%v, live=%q, err=%v), want a granted window on %q", release != nil, live, err, head)
	}
	release()

	// The window must be fully released: a second entry succeeds immediately.
	release2, _, err := EnterRefAuthorityWindow(cfg, repo, head, nil)
	if err != nil {
		t.Fatalf("EnterRefAuthorityWindow after release = %v, want the worktree free again", err)
	}
	release2()
}

func TestEnterRefAuthorityWindowFailsClosedOnHeadMismatch(t *testing.T) {
	repo, run := gitRepo(t)
	run("commit", "-q", "--allow-empty", "-m", "base")
	validated := gitOut(repo, "rev-parse", "HEAD")
	// HEAD moves before the window is entered — exactly the old validation-to-consumption seam.
	run("commit", "-q", "--allow-empty", "-m", "moved")
	live := gitOut(repo, "rev-parse", "HEAD")

	cfg := &config.Config{ConfigDir: t.TempDir()}
	release, got, err := EnterRefAuthorityWindow(cfg, repo, validated, nil)
	if release != nil {
		t.Fatal("a HEAD mismatch must not hand back a held lock")
	}
	if !errors.Is(err, ErrRefAuthorityMoved) {
		t.Fatalf("EnterRefAuthorityWindow(mismatch) err = %v, want ErrRefAuthorityMoved", err)
	}
	if got != live {
		t.Fatalf("EnterRefAuthorityWindow(mismatch) live = %q, want the actual current HEAD %q", got, live)
	}

	// Failing closed must not leak the lock: a fresh window on the same worktree is immediately
	// available (using the now-live HEAD as its own validated value).
	release2, _, err := EnterRefAuthorityWindow(cfg, repo, live, nil)
	if err != nil {
		t.Fatalf("EnterRefAuthorityWindow after a failed-closed mismatch = %v, want the worktree still free", err)
	}
	release2()
}

func TestEnterRefAuthorityWindowFailsClosedWhenContended(t *testing.T) {
	repo, run := gitRepo(t)
	run("commit", "-q", "--allow-empty", "-m", "base")
	head := gitOut(repo, "rev-parse", "HEAD")
	cfg := &config.Config{ConfigDir: t.TempDir()}

	holderRelease, err := LockRefAuthority(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer holderRelease()

	release, live, err := EnterRefAuthorityWindow(cfg, repo, head, nil)
	if release != nil || live != "" || err == nil || errors.Is(err, ErrRefAuthorityMoved) {
		t.Fatalf("EnterRefAuthorityWindow(contended) = (release=%v, live=%q, err=%v), want a plain lock-acquire failure", release != nil, live, err)
	}
}

// The parallel-controller contract: validate() and consume() for a DIFFERENT worktree must never be
// slowed by this one's window.
func TestEnterRefAuthorityWindowKeepsSeparateWorktreesParallel(t *testing.T) {
	repoA, runA := gitRepo(t)
	runA("commit", "-q", "--allow-empty", "-m", "a")
	repoB, runB := gitRepo(t)
	runB("commit", "-q", "--allow-empty", "-m", "b")

	cfg := &config.Config{ConfigDir: t.TempDir()}
	releaseA, _, err := EnterRefAuthorityWindow(cfg, repoA, gitOut(repoA, "rev-parse", "HEAD"), nil)
	if err != nil {
		t.Fatalf("window A = %v, want it granted", err)
	}
	defer releaseA()
	releaseB, _, err := EnterRefAuthorityWindow(cfg, repoB, gitOut(repoB, "rev-parse", "HEAD"), nil)
	if err != nil {
		t.Fatalf("window B = %v, want a concurrent fork's worktree to stay parallel", err)
	}
	defer releaseB()
}

// TestRefAuthorityWindowRejectsConcurrentHeadMove is the coordinated multi-process regression: the
// completing controller pauses at the exact old validation-to-consumption seam (the beforeWindow
// test hook — a synchronization point, not a sleep), a SECOND, REAL OS process (a plain `git
// commit`, standing in for "an interactive coop run, a host-side signing rewrite, or a human
// committing" — the task's own three named threats) moves HEAD, then the controller proceeds into
// the window. It must fail closed exactly the way the work loop does: RestoreRefAuthorityFailure +
// RefAuthorityFailureError are the same functions commands.go calls, so this proves the window's
// real production contract, not a reimplementation of it.
func TestRefAuthorityWindowRejectsConcurrentHeadMove(t *testing.T) {
	repo, run := gitRepo(t)
	run("commit", "-q", "--allow-empty", "-m", "base")

	root := filepath.Join(repo, TasksRoot)
	id := "2026-08-09-race-me"
	item := taskForLease(t, root, StateInProgress, id)
	const generation = "original-generation"
	if err := WriteAuditReopenRecord(root, testAuditReopenRecord(id, generation)); err != nil {
		t.Fatal(err)
	}
	// Simulate the agent's box work: a commit bound to the task, then the folder already moved to
	// done — exactly the state the host observes when it reaches the ref-authority window.
	run("commit", "-q", "--allow-empty", "-m", "implement\n\n"+CoopTaskTrailer+": "+id)
	if err := MoveTaskDir(root, item, StateDone); err != nil {
		t.Fatal(err)
	}
	headAfter := gitOut(repo, "rev-parse", "HEAD")

	cfg := &config.Config{ConfigDir: t.TempDir()}
	var raced string
	beforeWindow := func(repo, validated string) {
		// The pause IS the synchronization point: this hook runs synchronously, before the lock is
		// even attempted, so there is nothing racy about when the second process's commit lands
		// relative to the compare that follows.
		run("commit", "-q", "--allow-empty", "-m", "a concurrent host-side commit")
		raced = gitOut(repo, "rev-parse", "HEAD")
	}

	release, live, err := EnterRefAuthorityWindow(cfg, repo, headAfter, beforeWindow)
	if release != nil {
		t.Fatal("a concurrent HEAD move must not hand back a held ref-authority lock")
	}
	if !errors.Is(err, ErrRefAuthorityMoved) {
		t.Fatalf("EnterRefAuthorityWindow err = %v, want ErrRefAuthorityMoved", err)
	}
	if raced == "" || live != raced || live == headAfter {
		t.Fatalf("live HEAD = %q, want the concurrent commit %q (validated was %q)", live, raced, headAfter)
	}

	// Drive the SAME restore/error path commands.go uses on this exact failure.
	current, ok := CurrentTask(root, id)
	if !ok || current.State != StateDone {
		t.Fatalf("task state before restore = %+v, %v; want it still done (untouched by the failed window)", current, ok)
	}
	reason := fmt.Sprintf("HEAD moved from the validated %s to %s before task authority could be consumed — another process changed this checkout during completion", headAfter, live)
	restoreErr := RestoreRefAuthorityFailure(QueuedTask{Root: root, Item: current}, reason)
	finalErr := RefAuthorityFailureError(id, reason, restoreErr)
	if finalErr == nil || !strings.Contains(finalErr.Error(), id) || !strings.Contains(finalErr.Error(), "completion rejected") {
		t.Fatalf("RefAuthorityFailureError = %v, want an actionable per-task refusal", finalErr)
	}

	// The task is actionable again, with no partial authority: no receipt, and the audit-reopen
	// generation this window never reached is exactly as it was.
	restored, ok := CurrentTask(root, id)
	if !ok || restored.State != StateInProgress {
		t.Fatalf("restored task = %+v, %v; want it back in in_progress", restored, ok)
	}
	if taskCompletionRecorded(root, restored) {
		t.Fatal("a receipt was written for a completion the ref-authority window rejected")
	}
	record, ok, err := ReadAuditReopenRecord(root, id)
	if err != nil || !ok || record.Generation != generation {
		t.Fatalf("audit reopen record after the rejected window = %+v, ok=%v, err=%v; want generation %q untouched", record, ok, err, generation)
	}
}

// refAuthorityProcess drives a real OS process through the TestRefAuthorityHelperProcess entry
// point below — the same re-exec-the-test-binary idiom lease_test.go uses (startLeaseProcess/
// leaseProcess) for a genuinely multi-process regression.
type refAuthorityProcess struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Reader
}

func startRefAuthorityProcess(t *testing.T, configDir, repo string) *refAuthorityProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRefAuthorityHelperProcess$")
	cmd.Env = append(os.Environ(),
		"COOP_REFAUTH_HELPER=1",
		"COOP_REFAUTH_CONFIGDIR="+configDir,
		"COOP_REFAUTH_REPO="+repo,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return &refAuthorityProcess{cmd: cmd, stdin: stdin, out: bufio.NewReader(stdout)}
}

func (p *refAuthorityProcess) readLine(t *testing.T) string {
	t.Helper()
	line := make(chan string, 1)
	go func() {
		s, _ := p.out.ReadString('\n')
		line <- strings.TrimSpace(s)
	}()
	select {
	case got := <-line:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("ref authority helper did not report a result")
		return ""
	}
}

// TestRefAuthorityHelperProcess is not a test on its own: run normally (no env var) it does
// nothing. Re-exec'd with COOP_REFAUTH_HELPER set, it acquires ref authority, reports readiness,
// then blocks on stdin so the driving test controls exactly when it releases — or kills it to
// simulate a crash while the lock is held.
func TestRefAuthorityHelperProcess(t *testing.T) {
	if os.Getenv("COOP_REFAUTH_HELPER") == "" {
		return
	}
	cfg := &config.Config{ConfigDir: os.Getenv("COOP_REFAUTH_CONFIGDIR")}
	release, err := LockRefAuthority(cfg, os.Getenv("COOP_REFAUTH_REPO"))
	if err != nil {
		fmt.Printf("ERROR %v\n", err)
		return
	}
	fmt.Println("LOCKED")
	_, _ = io.Copy(io.Discard, os.Stdin) // blocks until the parent closes stdin or kills this process
	release()
	fmt.Println("RELEASED")
}

// A real second process holding ref authority blocks a concurrent acquire on the same worktree —
// the mutual exclusion proven across an actual process boundary, not just goroutines sharing one.
func TestLockRefAuthorityBlocksAcrossRealProcesses(t *testing.T) {
	configDir, repo := t.TempDir(), t.TempDir()
	cfg := &config.Config{ConfigDir: configDir}

	holder := startRefAuthorityProcess(t, configDir, repo)
	if got := holder.readLine(t); got != "LOCKED" {
		t.Fatalf("holder reported %q, want LOCKED", got)
	}

	if _, err := LockRefAuthority(cfg, repo); err == nil {
		t.Fatal("acquired ref authority while a separate process held it")
	}

	if err := holder.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if got := holder.readLine(t); got != "RELEASED" {
		t.Fatalf("holder reported %q, want RELEASED", got)
	}
	if err := holder.cmd.Wait(); err != nil {
		t.Fatal(err)
	}

	release, err := LockRefAuthority(cfg, repo)
	if err != nil {
		t.Fatalf("LockRefAuthority after the holder released = %v, want it granted", err)
	}
	release()
}

// Crash-in-window recovery: a controller killed while holding ref authority must not strand it.
// flock is bound to the process's open file description, so the kernel releases it the instant the
// process dies — the next run acquires it immediately, and since nothing between lock-acquisition
// and consumption ran before the kill, the task carries no partial authority to clean up.
func TestLockRefAuthorityRecoversAfterHolderIsKilled(t *testing.T) {
	configDir, repo := t.TempDir(), t.TempDir()
	cfg := &config.Config{ConfigDir: configDir}

	holder := startRefAuthorityProcess(t, configDir, repo)
	if got := holder.readLine(t); got != "LOCKED" {
		t.Fatalf("holder reported %q, want LOCKED", got)
	}

	if err := holder.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := holder.cmd.Wait(); err == nil {
		t.Fatal("killed helper process reported a clean exit")
	}

	release, err := LockRefAuthority(cfg, repo)
	if err != nil {
		t.Fatalf("LockRefAuthority after the holder was killed = %v, want it granted (flock dies with the process)", err)
	}
	release()
}

// lockRefAuthority is the compare-and-swap window's mutual exclusion: acquire, contended acquire
// (naming the holder), and release — the same shape as internal/cli's lockLoopCheckout tests, since
// the spec mirrors its keying exactly. A stuck holder must fail fast (non-blocking), never wedge the
// caller — see the contended case below. Moved from fork_loop_test.go with the function.
func TestLockRefAuthorityRefusesConcurrentHolder(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	repo := t.TempDir()

	release, err := LockRefAuthority(cfg, repo)
	if err != nil {
		t.Fatalf("first LockRefAuthority(%q) = %v, want it granted", repo, err)
	}

	if _, err = LockRefAuthority(cfg, repo); err == nil {
		t.Fatal("a second holder acquired the same worktree's ref authority")
	} else if !strings.Contains(err.Error(), "validating or consuming a completion") {
		t.Errorf("contended LockRefAuthority error = %q, want it to say what's in progress", err)
	} else if !strings.Contains(err.Error(), fmt.Sprintf("pid %d", os.Getpid())) {
		t.Errorf("contended LockRefAuthority error = %q, want it to name the holding pid", err)
	}

	// Releasing hands the worktree to the next contender — the window must not park it forever.
	release()
	release2, err := LockRefAuthority(cfg, repo)
	if err != nil {
		t.Fatalf("LockRefAuthority after release = %v, want the worktree free again", err)
	}
	release2()
}

// The parallel-fork property matters here too: a coordinated regression that pins a worktree's ref
// authority must never block a DIFFERENT fork's completion.
func TestLockRefAuthorityKeepsSeparateWorktreesParallel(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}

	releaseA, err := LockRefAuthority(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("lock on worktree A = %v, want it granted", err)
	}
	defer releaseA()

	releaseB, err := LockRefAuthority(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("lock on worktree B = %v, want concurrent forks to stay parallel", err)
	}
	defer releaseB()
}

// Never keyed on the repo name: two distinct absolute paths that just happen to share a basename
// must not collide (the failure mode a name-based key would have).
func TestLockRefAuthorityKeyedOnResolvedPathNotName(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	base := t.TempDir()
	a := filepath.Join(base, "one", "repo")
	b := filepath.Join(base, "two", "repo")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	releaseA, err := LockRefAuthority(cfg, a)
	if err != nil {
		t.Fatalf("lock on %q = %v, want it granted", a, err)
	}
	defer releaseA()
	releaseB, err := LockRefAuthority(cfg, b)
	if err != nil {
		t.Fatalf("lock on %q = %v, want a same-named-but-different worktree to stay parallel", b, err)
	}
	defer releaseB()
}

// refAuthorityIsCurrent is what makes the lock proof against a purge or a careless `rm -rf .locks`:
// flock binds a process to an INODE, never the name every other controller opens, so a record
// removed and recreated between open and flock must never leave two controllers each holding an
// "exclusive" lock on a different inode. Mirrors lease_test.go's identical race on the task-lease
// registry (TestLeaseAuthorityLockRechecksInodeIdentity / replaceLeaseAuthorityRecord). Moved from
// fork_loop_test.go with the function.
func TestRefAuthorityIsCurrentDetectsSwappedInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ref-test.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if current, err := refAuthorityIsCurrent(f, path); err != nil || !current {
		t.Fatalf("freshly opened lock current = (%v, %v), want (true, nil)", current, err)
	}

	// This process still holds an fd on the original inode, so the kernel cannot recycle its
	// number — the swap below is guaranteed to produce a different identity.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	swapped, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	swapped.Close()
	if current, err := refAuthorityIsCurrent(f, path); err != nil || current {
		t.Fatalf("swapped-inode lock current = (%v, %v), want (false, nil)", current, err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if current, err := refAuthorityIsCurrent(f, path); err != nil || current {
		t.Fatalf("unlinked lock current = (%v, %v), want (false, nil)", current, err)
	}
}
