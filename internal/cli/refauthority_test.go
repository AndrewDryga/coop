package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// enterRefAuthorityWindow is the compare-and-swap the work loop, signing, fork-land, and the
// audit-reopen completion path all share: acquire the lock, then re-read HEAD as the FIRST action
// inside it and compare against the value validation is about to trust.

func TestEnterRefAuthorityWindowMatchesLiveHead(t *testing.T) {
	repo, run := gitrepo.New(t)
	run("commit", "-q", "--allow-empty", "-m", "base")
	head := gitOut(repo, "rev-parse", "HEAD")

	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	release, live, err := a.enterRefAuthorityWindow(repo, head)
	if err != nil || release == nil || live != head {
		t.Fatalf("enterRefAuthorityWindow(matching) = (release=%v, live=%q, err=%v), want a granted window on %q", release != nil, live, err, head)
	}
	release()

	// The window must be fully released: a second entry succeeds immediately.
	release2, _, err := a.enterRefAuthorityWindow(repo, head)
	if err != nil {
		t.Fatalf("enterRefAuthorityWindow after release = %v, want the worktree free again", err)
	}
	release2()
}

func TestEnterRefAuthorityWindowFailsClosedOnHeadMismatch(t *testing.T) {
	repo, run := gitrepo.New(t)
	run("commit", "-q", "--allow-empty", "-m", "base")
	validated := gitOut(repo, "rev-parse", "HEAD")
	// HEAD moves before the window is entered — exactly the old validation-to-consumption seam.
	run("commit", "-q", "--allow-empty", "-m", "moved")
	live := gitOut(repo, "rev-parse", "HEAD")

	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	release, got, err := a.enterRefAuthorityWindow(repo, validated)
	if release != nil {
		t.Fatal("a HEAD mismatch must not hand back a held lock")
	}
	if !errors.Is(err, errRefAuthorityMoved) {
		t.Fatalf("enterRefAuthorityWindow(mismatch) err = %v, want errRefAuthorityMoved", err)
	}
	if got != live {
		t.Fatalf("enterRefAuthorityWindow(mismatch) live = %q, want the actual current HEAD %q", got, live)
	}

	// Failing closed must not leak the lock: a fresh window on the same worktree is immediately
	// available (using the now-live HEAD as its own validated value).
	release2, _, err := a.enterRefAuthorityWindow(repo, live)
	if err != nil {
		t.Fatalf("enterRefAuthorityWindow after a failed-closed mismatch = %v, want the worktree still free", err)
	}
	release2()
}

func TestEnterRefAuthorityWindowFailsClosedWhenContended(t *testing.T) {
	repo, run := gitrepo.New(t)
	run("commit", "-q", "--allow-empty", "-m", "base")
	head := gitOut(repo, "rev-parse", "HEAD")
	cfg := &config.Config{ConfigDir: t.TempDir()}

	holderRelease, err := lockRefAuthority(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer holderRelease()

	a := &app{cfg: cfg}
	release, live, err := a.enterRefAuthorityWindow(repo, head)
	if release != nil || live != "" || err == nil || errors.Is(err, errRefAuthorityMoved) {
		t.Fatalf("enterRefAuthorityWindow(contended) = (release=%v, live=%q, err=%v), want a plain lock-acquire failure", release != nil, live, err)
	}
}

// The parallel-controller contract: validate() and consume() for a DIFFERENT worktree must never be
// slowed by this one's window.
func TestEnterRefAuthorityWindowKeepsSeparateWorktreesParallel(t *testing.T) {
	repoA, runA := gitrepo.New(t)
	runA("commit", "-q", "--allow-empty", "-m", "a")
	repoB, runB := gitrepo.New(t)
	runB("commit", "-q", "--allow-empty", "-m", "b")

	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	releaseA, _, err := a.enterRefAuthorityWindow(repoA, gitOut(repoA, "rev-parse", "HEAD"))
	if err != nil {
		t.Fatalf("window A = %v, want it granted", err)
	}
	defer releaseA()
	releaseB, _, err := a.enterRefAuthorityWindow(repoB, gitOut(repoB, "rev-parse", "HEAD"))
	if err != nil {
		t.Fatalf("window B = %v, want a concurrent fork's worktree to stay parallel", err)
	}
	defer releaseB()
}

// TestRefAuthorityWindowRejectsConcurrentHeadMove is the coordinated multi-process regression: the
// completing controller pauses at the exact old validation-to-consumption seam (the
// beforeRefAuthorityWindow test hook — a synchronization point, not a sleep), a SECOND, REAL OS
// process (a plain `git commit`, standing in for "an interactive coop run, a host-side signing
// rewrite, or a human committing" — the task's own three named threats) moves HEAD, then the
// controller proceeds into the window. It must fail closed exactly the way the work loop does:
// restoreRefAuthorityFailure + refAuthorityFailureError are the same functions commands.go calls, so
// this proves the window's real production contract, not a reimplementation of it.
//
// This test cannot pass against the pre-fix tree: enterRefAuthorityWindow, restoreRefAuthorityFailure,
// and the beforeRefAuthorityWindow hook did not exist before this change, so main both lacks the
// mechanism this test exercises and offered no seam to interpose the race deterministically.
func TestRefAuthorityWindowRejectsConcurrentHeadMove(t *testing.T) {
	repo, run := gitrepo.New(t)
	run("commit", "-q", "--allow-empty", "-m", "base")

	root := filepath.Join(repo, tasksRoot)
	id := "2026-08-09-race-me"
	item := taskForLease(t, root, stateInProgress, id)
	const generation = "original-generation"
	if err := writeAuditReopenRecord(root, testAuditReopenRecord(id, generation)); err != nil {
		t.Fatal(err)
	}
	// Simulate the agent's box work: a commit bound to the task, then the folder already moved to
	// done — exactly the state the host observes when it reaches the ref-authority window.
	run("commit", "-q", "--allow-empty", "-m", "implement\n\n"+coopTaskTrailer+": "+id)
	if err := moveTaskDir(root, item, stateDone); err != nil {
		t.Fatal(err)
	}
	headAfter := gitOut(repo, "rev-parse", "HEAD")

	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	var raced string
	a.beforeRefAuthorityWindow = func(repo, validated string) {
		// The pause IS the synchronization point: this hook runs synchronously, before the lock is
		// even attempted, so there is nothing racy about when the second process's commit lands
		// relative to the compare that follows.
		run("commit", "-q", "--allow-empty", "-m", "a concurrent host-side commit")
		raced = gitOut(repo, "rev-parse", "HEAD")
	}

	release, live, err := a.enterRefAuthorityWindow(repo, headAfter)
	if release != nil {
		t.Fatal("a concurrent HEAD move must not hand back a held ref-authority lock")
	}
	if !errors.Is(err, errRefAuthorityMoved) {
		t.Fatalf("enterRefAuthorityWindow err = %v, want errRefAuthorityMoved", err)
	}
	if raced == "" || live != raced || live == headAfter {
		t.Fatalf("live HEAD = %q, want the concurrent commit %q (validated was %q)", live, raced, headAfter)
	}

	// Drive the SAME restore/error path commands.go uses on this exact failure.
	current, ok := currentTask(root, id)
	if !ok || current.State != stateDone {
		t.Fatalf("task state before restore = %+v, %v; want it still done (untouched by the failed window)", current, ok)
	}
	reason := fmt.Sprintf("HEAD moved from the validated %s to %s before task authority could be consumed — another process changed this checkout during completion", headAfter, live)
	restoreErr := restoreRefAuthorityFailure(queuedTask{Root: root, Item: current}, reason)
	finalErr := refAuthorityFailureError(id, reason, restoreErr)
	if finalErr == nil || !strings.Contains(finalErr.Error(), id) || !strings.Contains(finalErr.Error(), "completion rejected") {
		t.Fatalf("refAuthorityFailureError = %v, want an actionable per-task refusal", finalErr)
	}

	// The task is actionable again, with no partial authority: no receipt, and the audit-reopen
	// generation this window never reached is exactly as it was.
	restored, ok := currentTask(root, id)
	if !ok || restored.State != stateInProgress {
		t.Fatalf("restored task = %+v, %v; want it back in in_progress", restored, ok)
	}
	if taskCompletionRecorded(root, restored) {
		t.Fatal("a receipt was written for a completion the ref-authority window rejected")
	}
	record, ok, err := readAuditReopenRecord(root, id)
	if err != nil || !ok || record.Generation != generation {
		t.Fatalf("audit reopen record after the rejected window = %+v, ok=%v, err=%v; want generation %q untouched", record, ok, err, generation)
	}
}

// TestCmdTasksDoneTakesRefAuthority proves the wrap in cmdTasks (tasks.go) is real, not decorative:
// completeTrustedTask's audit-reopen branch shares the loop's validate-then-consume shape, so
// `coop tasks done` must take the same lock a running loop's window holds — never able to complete
// mid-window and never able to make that window refuse.
func TestCmdTasksDoneTakesRefAuthority(t *testing.T) {
	repo := t.TempDir()
	a := appFor(repo)
	root := filepath.Join(repo, tasksRoot)
	id := "2026-08-09-contended-done"
	taskForLease(t, root, stateInProgress, id)

	resolved, err := filepath.Abs(repo)
	if err != nil {
		t.Fatal(err)
	}
	release, err := lockRefAuthority(a.cfg, resolved)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	code, err := a.cmdTasks([]string{"done", id})
	if err == nil || !strings.Contains(err.Error(), "ref authority") {
		t.Fatalf("cmdTasks(done) while ref authority is held = (%d, %v), want a ref-authority refusal", code, err)
	}
	if current, ok := currentTask(root, id); !ok || current.State != stateInProgress {
		t.Fatalf("task state after the refused done = %+v, %v; want it untouched", current, ok)
	}
}

// TestFastForwardParentTakesRefAuthority proves fork-merge's landing step (the parent-checkout
// mutation, not the fork's own isolated rebase) shares the same lock, so a fork can never land in
// the middle of a loop's validate→consume window on the parent it's landing onto.
func TestFastForwardParentTakesRefAuthority(t *testing.T) {
	repo, run := gitrepo.New(t)
	run("commit", "-q", "--allow-empty", "-m", "base")
	run("branch", "candidate") // gitFetchInto(repo, repo, "candidate") must succeed before the lock is even reached
	a := appFor(repo)

	resolved, err := filepath.Abs(repo)
	if err != nil {
		t.Fatal(err)
	}
	release, err := lockRefAuthority(a.cfg, resolved)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if err := a.fastForwardParent(repo, repo, "candidate"); err == nil || !strings.Contains(err.Error(), "ref authority") {
		t.Fatalf("fastForwardParent while ref authority is held = %v, want a ref-authority refusal", err)
	}
}

// TestSignUnpushedTakesRefAuthority proves signUnpushed's ref-mutating tail (the update-ref compare-
// and-swap) shares the lock: a signing rewrite can never land inside another controller's
// validate→consume window, and vice versa, so coop's own signing sweep can never trip its own
// refusal.
func TestSignUnpushedTakesRefAuthority(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	keyDir := t.TempDir()
	key := filepath.Join(keyDir, "sk")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-f", key, "-N", "", "-C", "coop-test").CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	globalCfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalCfg, []byte("[commit]\n\tgpgsign = true\n[gpg]\n\tformat = ssh\n[user]\n\tsigningkey = "+key+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))

	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "T")
	run("config", "commit.gpgsign", "false")
	run("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	run("commit", "-q", "--allow-empty", "-m", "unpushed")

	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	resolved, err := filepath.Abs(repo)
	if err != nil {
		t.Fatal(err)
	}
	release, err := lockRefAuthority(a.cfg, resolved)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := a.signUnpushed(repo, base); err == nil || !strings.Contains(err.Error(), "ref authority") {
		t.Fatalf("signUnpushed while ref authority is held = %v, want a ref-authority refusal", err)
	}
}

// TestReconcileQueueAfterMergeTakesRefAuthority proves the fork-merge reconciliation path shares the
// lock: completeTrustedTask's audit-reopen branch has the same validate-then-consume shape as the
// work loop's own completion, so closing a landed task here must be exclusive with a running loop's
// window on the same parent checkout too.
func TestReconcileQueueAfterMergeTakesRefAuthority(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, run := gitrepo.New(t)
	writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, "landed", "task.md"), "# landed\n")
	if err := os.WriteFile(filepath.Join(repo, "code.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "seed queue")
	beforeLand := gitOut(repo, "rev-parse", "HEAD")
	run("commit", "-q", "--allow-empty", "-m", "landed work\n\n"+coopTaskTrailer+": landed")

	a := &app{cfg: &config.Config{ConfigDir: t.TempDir(), TasksFiles: []string{tasksRoot}}}
	resolved, err := filepath.Abs(repo)
	if err != nil {
		t.Fatal(err)
	}
	release, err := lockRefAuthority(a.cfg, resolved)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if err := a.reconcileQueueAfterMerge(repo, "fork1", beforeLand+"..HEAD"); err == nil || !strings.Contains(err.Error(), "ref authority") {
		t.Fatalf("reconcileQueueAfterMerge while ref authority is held = %v, want a ref-authority refusal", err)
	}
	if current, ok := currentTask(filepath.Join(repo, tasksRoot), "landed"); !ok || current.State != stateTodo {
		t.Fatalf("landed task state after the refused reconcile = %+v, %v; want it untouched", current, ok)
	}
}

// refAuthorityProcess drives a real OS process through the TestRefAuthorityHelperProcess entry
// point below — the same re-exec-the-test-binary idiom tasklease_test.go uses
// (startLeaseProcess/leaseProcess) for a genuinely multi-process regression.
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
	release, err := lockRefAuthority(cfg, os.Getenv("COOP_REFAUTH_REPO"))
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

	if _, err := lockRefAuthority(cfg, repo); err == nil {
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

	release, err := lockRefAuthority(cfg, repo)
	if err != nil {
		t.Fatalf("lockRefAuthority after the holder released = %v, want it granted", err)
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

	release, err := lockRefAuthority(cfg, repo)
	if err != nil {
		t.Fatalf("lockRefAuthority after the holder was killed = %v, want it granted (flock dies with the process)", err)
	}
	release()
}
