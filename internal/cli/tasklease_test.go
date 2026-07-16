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
	"syscall"
	"testing"
	"time"
)

func testLeaseOwner() taskLeaseOwner {
	return taskLeaseOwner{
		RunID: "test-run", PID: 4242, Provider: "codex", Target: "codex:gpt-test@work",
		Now: func() time.Time { return time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC) },
	}
}

func taskCompletionReceipt(root string, task taskItem) (leaseCompletionReceipt, bool) {
	receipt, ok, _ := inspectTaskCompletionReceipt(root, task)
	return receipt, ok
}

func taskCompletionRecorded(root string, task taskItem) bool {
	_, ok := taskCompletionReceipt(root, task)
	return ok
}

func taskForLease(t *testing.T, root, state, id string) taskItem {
	t.Helper()
	writeTaskFile(t, filepath.Join(root, state, id, "task.md"), "# "+id+"\n")
	item, ok := currentTask(root, id)
	if !ok {
		t.Fatalf("could not read task %s", id)
	}
	return item
}

func TestTaskLeaseWritesRenameSafeHeartbeatAndReleases(t *testing.T) {
	root, id := t.TempDir(), "resume-me"
	item := taskForLease(t, root, stateInProgress, id)
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	owner := testLeaseOwner()
	owner.Now = func() time.Time { return now }
	lease, _, err := tryTaskLease(root, item, owner)
	if err != nil {
		t.Fatal(err)
	}

	metaPath := filepath.Join(item.Dir, "tmp", leaseMetadataName)
	meta, ok := readLeaseMetadata(item.Dir)
	if !ok || meta.RunID != owner.RunID || meta.ControllerPID != owner.PID || meta.Provider != owner.Provider || meta.Target != owner.Target {
		t.Fatalf("lease metadata = %+v, ok=%v", meta, ok)
	}
	if !meta.AcquiredAt.Equal(now) || !meta.HeartbeatAt.Equal(now) {
		t.Fatalf("initial metadata timestamps = %+v, want %s", meta, now)
	}

	now = now.Add(10 * time.Second)
	if err := moveTaskDir(root, item, stateBlocked); err != nil {
		t.Fatal(err)
	}
	if err := lease.refresh(); err != nil {
		t.Fatal(err)
	}
	blockedDir := filepath.Join(root, stateBlocked, id)
	if got, ok := readLeaseMetadata(blockedDir); !ok || !got.HeartbeatAt.Equal(now) {
		t.Fatalf("rename-safe heartbeat = %+v, ok=%v", got, ok)
	}
	if pathExists(metaPath) {
		t.Fatal("heartbeat recreated metadata under the old state path")
	}
	doneItem, ok := currentTask(root, id)
	if !ok {
		t.Fatal("moved task disappeared before completion")
	}
	if err := moveTaskDir(root, doneItem, stateDone); err != nil {
		t.Fatal(err)
	}
	if err := lease.release(); err != nil {
		t.Fatal(err)
	}
	doneDir := filepath.Join(root, stateDone, id)
	if pathExists(filepath.Join(doneDir, "tmp", leaseMetadataName)) {
		t.Fatal("normal release left lease metadata behind")
	}
	if !fileExists(filepath.Join(doneDir, "tmp", leaseLockName)) {
		t.Fatal("normal release must retain the stable lock inode")
	}
	if err := removeTaskTmp(doneDir); err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(doneDir, "tmp")) {
		t.Fatal("done cleanup did not remove the released lease lock")
	}
}

func TestTaskLeaseHeartbeatTickerRefreshesMetadata(t *testing.T) {
	root, id := t.TempDir(), "heartbeat"
	item := taskForLease(t, root, stateInProgress, id)
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	ticks := make(chan time.Time, 1)
	owner := testLeaseOwner()
	owner.Now = func() time.Time { return now }
	owner.Ticker = func(interval time.Duration) (<-chan time.Time, func()) {
		if interval != leaseHeartbeatInterval {
			t.Fatalf("heartbeat interval = %s, want %s", interval, leaseHeartbeatInterval)
		}
		return ticks, func() {}
	}
	lease, _, err := tryTaskLease(root, item, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.release() })

	now = now.Add(leaseHeartbeatInterval)
	ticks <- now
	deadline := time.Now().Add(time.Second)
	for {
		meta, ok := readLeaseMetadata(item.Dir)
		if ok && meta.HeartbeatAt.Equal(now) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("heartbeat metadata was not refreshed to %s", now)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTaskLeaseQuiesceStopsHeartbeatAndRetainsLock(t *testing.T) {
	root, id := t.TempDir(), "quiesced"
	item := taskForLease(t, root, stateInProgress, id)
	ticks := make(chan time.Time, 1)
	owner := testLeaseOwner()
	owner.Ticker = func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }
	lease, _, err := tryTaskLease(root, item, owner)
	if err != nil {
		t.Fatal(err)
	}
	lease.quiesce()
	if got := observeTaskLease(item, owner.now()); got.State == leaseUnleased {
		t.Fatal("quiesce released the authoritative task lock")
	}
	if err := moveTaskDir(root, item, stateDone); err != nil {
		t.Fatal(err)
	}
	doneDir := filepath.Join(root, stateDone, id)
	if err := removeTaskTmp(doneDir); err != nil {
		t.Fatal(err)
	}
	ticks <- owner.now().Add(leaseHeartbeatInterval)
	if pathExists(filepath.Join(doneDir, "tmp")) {
		t.Fatal("heartbeat recreated task metadata after quiesce")
	}
	if err := lease.release(); err != nil {
		t.Fatal(err)
	}
}

func TestTaskLeaseMetadataRejectsProviderControlledTmp(t *testing.T) {
	root, id := t.TempDir(), "swapped-tmp"
	item := taskForLease(t, root, stateInProgress, id)
	lease, _, err := tryTaskLease(root, item, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, leaseMetadataName)
	const want = "outside sentinel\n"
	if err := os.WriteFile(sentinel, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(item.Dir, "tmp")
	if err := os.Rename(tmp, tmp+"-provider"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, tmp); err != nil {
		t.Fatal(err)
	}
	if err := lease.refresh(); err == nil {
		t.Fatal("heartbeat followed a provider-swapped tmp symlink")
	}
	if _, ok := readLeaseMetadata(item.Dir); ok {
		t.Fatal("metadata reader followed a provider-swapped tmp symlink")
	}
	if err := lease.release(); err == nil {
		t.Fatal("lease release silently accepted a provider-swapped tmp symlink")
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != want {
		t.Fatalf("outside metadata changed to %q, %v", got, err)
	}
}

func TestTaskLeaseAuthorityRejectsProviderReplacedRealTmp(t *testing.T) {
	root, id := t.TempDir(), "replaced-real-tmp"
	item := taskForLease(t, root, stateInProgress, id)
	first, _, err := tryTaskLease(root, item, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(item.Dir, "tmp")
	if err := os.Rename(tmp, tmp+"-provider"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, leaseLockName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	current, ok := currentTask(root, id)
	if !ok {
		t.Fatal("task disappeared after tmp replacement")
	}
	second, observed, err := tryTaskLease(root, current, taskLeaseOwner{
		RunID: "second", PID: 4343, Provider: "claude", Target: "claude:test",
	})
	if err != nil || second != nil || observed.State == leaseUnleased {
		t.Fatalf("replacement inode acquired a second lease: lease=%v observed=%+v err=%v", second, observed, err)
	}
	if err := first.release(); err != nil {
		t.Fatal(err)
	}
}

func TestReadLeaseMetadataRejectsSpecialFiles(t *testing.T) {
	root, id := t.TempDir(), "special-metadata"
	item := taskForLease(t, root, stateInProgress, id)
	if _, err := taskLeaseDir(item.Dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(item.Dir, "tmp", leaseMetadataName)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readLeaseMetadata(item.Dir); ok {
		t.Fatal("lease metadata reader accepted a FIFO")
	}
}

func TestTaskLeaseObservationUsesLockNotHeartbeat(t *testing.T) {
	root, id := t.TempDir(), "locked"
	item := taskForLease(t, root, stateInProgress, id)
	now := time.Date(2026, 7, 14, 12, 1, 0, 0, time.UTC)
	owner := testLeaseOwner()
	owner.Now = func() time.Time { return now }
	lease, _, err := tryTaskLease(root, item, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.release() })

	if got := observeTaskLease(item, now); got.State != leaseBusy || got.Provider != "codex" {
		t.Errorf("fresh held lease = %+v, want busy codex", got)
	}
	lease.meta.HeartbeatAt = now.Add(-leaseStaleAfter - time.Second)
	if err := errors.Join(writeLeaseAuthorityMetadata(root, id, lease.meta), writeLeaseMetadata(root, id, lease.meta)); err != nil {
		t.Fatal(err)
	}
	if got := observeTaskLease(item, now); got.State != leaseStalled || got.Provider != "codex" {
		t.Errorf("stale held lease = %+v, want stalled codex", got)
	}
	if err := lease.release(); err != nil {
		t.Fatal(err)
	}
	if got := observeTaskLease(item, now); got.State != leaseUnleased {
		t.Errorf("released lease = %+v, want unleased", got)
	}
}

func TestTaskLeaseAdoptionIgnoresMetadataPIDWhenLockIsFree(t *testing.T) {
	root, id := t.TempDir(), "pid-reused"
	item := taskForLease(t, root, stateInProgress, id)
	now := time.Now()
	stale := taskLeaseMetadata{
		Version:       leaseMetadataVersion,
		RunID:         "dead-run",
		ControllerPID: os.Getpid(), // deliberately live: PID metadata is never authority
		Provider:      "codex",
		Target:        "codex:old",
		AcquiredAt:    now.Add(-time.Hour),
		HeartbeatAt:   now.Add(-time.Hour),
	}
	if _, err := taskLeaseDir(item.Dir); err != nil {
		t.Fatal(err)
	}
	lock, err := openLeaseLock(item.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeLeaseMetadata(root, id, stale); err != nil {
		t.Fatal(err)
	}

	owner := testLeaseOwner()
	owner.RunID = "new-run"
	owner.Now = time.Now
	assignment, err := assignLoopTask([]string{root}, owner)
	if err != nil || assignment.Outcome != assignmentReady || assignment.Task.Item.ID != id {
		t.Fatalf("PID-reuse adoption = %+v, err=%v", assignment, err)
	}
	if err := assignment.Lease.release(); err != nil {
		t.Fatal(err)
	}
}

func TestAssignLoopTaskSkipsForeignLeaseAndFallsBackToTodo(t *testing.T) {
	root := t.TempDir()
	busy := taskForLease(t, root, stateInProgress, "a-busy")
	taskForLease(t, root, stateTodo, "b-todo")
	foreign, _, err := tryTaskLease(root, busy, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = foreign.release() })

	owner := testLeaseOwner()
	owner.RunID = "other-run"
	assignment, err := assignLoopTask([]string{root}, owner)
	if err != nil {
		t.Fatal(err)
	}
	if assignment.Outcome != assignmentReady || assignment.Task.Item.ID != "b-todo" || assignment.Counts.Todo != 0 || assignment.Counts.Doing != 2 {
		t.Fatalf("assignment = %+v, want todo fallback", assignment)
	}
	if assignment.Busy.Busy != 1 || assignment.Busy.Stalled != 0 {
		t.Errorf("busy summary = %+v, want one busy", assignment.Busy)
	}
	if err := assignment.Lease.release(); err != nil {
		t.Fatal(err)
	}

	onlyBusy := t.TempDir()
	item := taskForLease(t, onlyBusy, stateInProgress, "busy")
	foreignOnly, _, err := tryTaskLease(onlyBusy, item, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	defer foreignOnly.release()
	assignment, err = assignLoopTask([]string{onlyBusy}, owner)
	if err != nil || assignment.Outcome != assignmentUnavailable || assignment.Busy.Busy != 1 {
		t.Fatalf("all-foreign assignment = %+v, err=%v", assignment, err)
	}
}

// TestTaskLeaseProcess is a helper process for the race tests below. Keeping the lock in a second
// process verifies kernel flock semantics rather than relying on Go's same-process descriptor rules.
func TestTaskLeaseProcess(t *testing.T) {
	mode := os.Getenv("COOP_LEASE_HELPER")
	if mode == "" {
		return
	}
	root := os.Getenv("COOP_LEASE_ROOT")
	if gate := os.Getenv("COOP_LEASE_GATE"); gate != "" {
		for {
			if _, err := os.Stat(gate); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
	owner := testLeaseOwner()
	owner.RunID, owner.PID = "helper-"+fmt.Sprint(os.Getpid()), os.Getpid()
	if mode == "stale" {
		owner.Now = func() time.Time { return time.Now().Add(-leaseStaleAfter - time.Second) }
	}
	assignment, err := assignLoopTask([]string{root}, owner)
	if err != nil {
		fmt.Printf("ERROR %v\n", err)
		return
	}
	switch assignment.Outcome {
	case assignmentReady:
		fmt.Printf("READY %s\n", assignment.Task.Item.ID)
		_, _ = io.Copy(io.Discard, os.Stdin)
		_ = assignment.Lease.release()
	case assignmentUnavailable:
		fmt.Println("UNAVAILABLE")
	default:
		fmt.Println("DRAINED")
	}
}

type leaseProcess struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Reader
}

func startLeaseProcess(t *testing.T, root, mode, gate string) *leaseProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTaskLeaseProcess$")
	cmd.Env = append(os.Environ(),
		"COOP_LEASE_HELPER="+mode,
		"COOP_LEASE_ROOT="+root,
		"COOP_LEASE_GATE="+gate,
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
	return &leaseProcess{cmd: cmd, stdin: stdin, out: bufio.NewReader(stdout)}
}

func (p *leaseProcess) result(t *testing.T) string {
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
		t.Fatal("lease helper did not report a result")
		return ""
	}
}

func (p *leaseProcess) release(t *testing.T) {
	t.Helper()
	_ = p.stdin.Close()
	if err := p.cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestTaskLeaseProcessRaces(t *testing.T) {
	runRace := func(t *testing.T, state string) {
		root, id := t.TempDir(), "only-task"
		taskForLease(t, root, state, id)
		gate := filepath.Join(root, "start")
		first := startLeaseProcess(t, root, "assign", gate)
		second := startLeaseProcess(t, root, "assign", gate)
		if err := os.WriteFile(gate, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		one, two := first.result(t), second.result(t)
		if got := []string{one, two}; !(strings.HasPrefix(got[0], "READY ") && got[1] == "UNAVAILABLE") && !(strings.HasPrefix(got[1], "READY ") && got[0] == "UNAVAILABLE") {
			t.Fatalf("simultaneous %s claim = %v, want one ready and one unavailable", state, got)
		}
		if state == stateTodo && pathExists(filepath.Join(root, stateTodo, id)) {
			t.Fatal("losing todo contender recreated the task's old state path")
		}
		items := readTaskTree(root)
		if len(items) != 1 || items[0].ID != id || items[0].State != stateInProgress {
			t.Fatalf("simultaneous %s claim left queue %+v, want one in-progress task", state, items)
		}
		first.release(t)
		second.release(t)
	}
	t.Run("simultaneous todo claim", func(t *testing.T) { runRace(t, stateTodo) })
	t.Run("simultaneous in-progress adoption", func(t *testing.T) { runRace(t, stateInProgress) })

	t.Run("dead owner is adopted immediately", func(t *testing.T) {
		root, id := t.TempDir(), "recover"
		taskForLease(t, root, stateInProgress, id)
		owner := startLeaseProcess(t, root, "assign", "")
		if got := owner.result(t); got != "READY "+id {
			t.Fatalf("owner = %q", got)
		}
		if err := owner.cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = owner.cmd.Wait()
		adopter := testLeaseOwner()
		adopter.Now = time.Now
		assignment, err := assignLoopTask([]string{root}, adopter)
		if err != nil || assignment.Outcome != assignmentReady || assignment.Task.Item.ID != id {
			t.Fatalf("immediate adoption = %+v, err=%v", assignment, err)
		}
		if err := assignment.Lease.release(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("stale heartbeat with a live lock stays stalled", func(t *testing.T) {
		root, id := t.TempDir(), "stalled"
		item := taskForLease(t, root, stateInProgress, id)
		holder := startLeaseProcess(t, root, "stale", "")
		if got := holder.result(t); got != "READY "+id {
			t.Fatalf("holder = %q", got)
		}
		if got := observeTaskLease(item, time.Now()); got.State != leaseStalled {
			t.Fatalf("live stale lease = %+v, want stalled", got)
		}
		owner := testLeaseOwner()
		owner.Now = time.Now
		assignment, err := assignLoopTask([]string{root}, owner)
		if err != nil || assignment.Outcome != assignmentUnavailable || assignment.Busy.Stalled != 1 {
			t.Fatalf("stalled lock assignment = %+v, err=%v", assignment, err)
		}
		holder.release(t)
	})

	t.Run("two tasks let two controllers win", func(t *testing.T) {
		root := t.TempDir()
		taskForLease(t, root, stateTodo, "a")
		taskForLease(t, root, stateTodo, "b")
		gate := filepath.Join(root, "start")
		first := startLeaseProcess(t, root, "assign", gate)
		second := startLeaseProcess(t, root, "assign", gate)
		if err := os.WriteFile(gate, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		one, two := first.result(t), second.result(t)
		if (one != "READY a" || two != "READY b") && (one != "READY b" || two != "READY a") {
			t.Fatalf("two-task race = %q, %q; want a and b", one, two)
		}
		first.release(t)
		second.release(t)
	})
}
