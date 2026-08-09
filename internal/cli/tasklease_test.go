package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
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

func testAuditReopenRecord(id, generation string) auditReopenRecord {
	return auditReopenRecord{
		Version: auditReopenVersion, Generation: generation, TaskID: id,
		BaselineHead: strings.Repeat("a", 40),
		Subject:      auditReopenCommit{TaskID: id, ChangeTree: "subject-tree"},
		History: []auditReopenCommit{{
			TaskID: "descendant", ChangeTree: "descendant-tree",
		}},
	}
}

func testLegacyAuditReopenRecord(id, generation string, pending bool) auditReopenRecord {
	version := auditReopenLegacyVersion
	if pending {
		version = auditReopenLegacyPendingVersion
	}
	return auditReopenRecord{
		Version: version, Generation: generation, TaskID: id, UnblockPending: pending,
		Subject: auditReopenCommit{TaskID: id, ChangeTree: "legacy-subject-tree"},
		Descendants: []auditReopenCommit{{
			TaskID: "legacy-descendant", ChangeTree: "legacy-descendant-tree",
		}},
	}
}

func TestAuditReopenRecordVersionsFailClosed(t *testing.T) {
	t.Run("new record requires baseline and non-nil history", func(t *testing.T) {
		record := testAuditReopenRecord("new", "generation")
		record.History = nil
		if validateAuditReopenRecord(record, record.TaskID) == nil {
			t.Fatal("new record with nil history was accepted")
		}
		record.History = []auditReopenCommit{}
		record.BaselineHead = ""
		if validateAuditReopenRecord(record, record.TaskID) == nil {
			t.Fatal("new record without baseline HEAD was accepted")
		}
	})
	t.Run("legacy versions decode but cannot lease", func(t *testing.T) {
		for _, pending := range []bool{false, true} {
			t.Run(fmt.Sprintf("pending=%v", pending), func(t *testing.T) {
				root := t.TempDir()
				task := taskForLease(t, root, stateInProgress, "legacy")
				record := testLegacyAuditReopenRecord(task.ID, "legacy-generation", pending)
				if err := writeAuditReopenRecord(root, record); err != nil {
					t.Fatal(err)
				}
				got, ok, err := readAuditReopenRecord(root, task.ID)
				if err != nil || !ok || !reflect.DeepEqual(got, record) {
					t.Fatalf("legacy decode = %#v, ok=%v err=%v", got, ok, err)
				}
				lease, _, err := tryTaskLease(root, task, testLeaseOwner())
				if lease != nil || err == nil ||
					!strings.Contains(err.Error(), "legacy audit-reopen authority") ||
					!strings.Contains(err.Error(), "--adopt-audit-head <full-sha>") {
					t.Fatalf("legacy lease = %#v, %v", lease, err)
				}
			})
		}
	})
	t.Run("legacy authority cannot complete", func(t *testing.T) {
		root := t.TempDir()
		task := taskForLease(t, root, stateInProgress, "legacy-complete")
		record := testLegacyAuditReopenRecord(task.ID, "legacy-generation", false)
		if err := writeAuditReopenRecord(root, record); err != nil {
			t.Fatal(err)
		}
		if err := completeTrustedTask(root, task); err == nil ||
			!strings.Contains(err.Error(), "legacy") {
			t.Fatalf("legacy completion error = %v", err)
		}
		code, err := tasksFolderMove(root, []string{task.ID}, stateDone, "done", "completed")
		if code != -1 || err == nil ||
			!strings.Contains(err.Error(), "coop tasks block "+task.ID) ||
			!strings.Contains(err.Error(), "coop tasks unblock "+task.ID+" --adopt-audit-head <full-sha>") ||
			strings.Contains(err.Error(), "retry: coop tasks done") {
			t.Fatalf("legacy tasks done recovery = code %d err %v", code, err)
		}
		current, ok := currentTask(root, task.ID)
		if !ok || current.State != stateInProgress {
			t.Fatalf("legacy completion moved task: %#v, ok=%v", current, ok)
		}
	})
	t.Run("pending authority names activation before completion", func(t *testing.T) {
		repo, git := gitRepo(t)
		t.Setenv(testLeaseAuthorityRootEnv, t.TempDir())
		git("commit", "-q", "--allow-empty", "-m", "base")
		git("commit", "-q", "--allow-empty", "-m", "pending implementation\n\nCoop-Task: pending")
		root := filepath.Join(repo, tasksRoot)
		task := taskForLease(t, root, stateTodo, "pending")
		record, err := captureAuditReopen(repo, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		record.Version = auditReopenPendingVersion
		record.UnblockPending = true
		if err := writeAuditReopenRecord(root, record); err != nil {
			t.Fatal(err)
		}
		err = completeTrustedTask(root, task)
		if err == nil || !strings.Contains(err.Error(), "pending audit authority") ||
			!strings.Contains(err.Error(), "coop tasks unblock "+task.ID) ||
			strings.Contains(err.Error(), "legacy adoption") {
			t.Fatalf("pending completion error = %v", err)
		}
		code, err := tasksFolderMove(root, []string{task.ID}, stateDone, "done", "completed")
		if code != -1 || err == nil ||
			!strings.Contains(err.Error(), "coop tasks unblock "+task.ID) ||
			strings.Contains(err.Error(), "retry: coop tasks done") {
			t.Fatalf("pending tasks done recovery = code %d err %v", code, err)
		}
		current, ok := currentTask(root, task.ID)
		if !ok || current.State != stateTodo {
			t.Fatalf("pending completion moved task: %#v, ok=%v", current, ok)
		}
	})
}

func TestTaskLeaseAuditReopenAuthorityIsScopedConsumedAndNotReusable(t *testing.T) {
	root := t.TempDir()
	first := taskForLease(t, root, stateInProgress, "first")
	second := taskForLease(t, root, stateInProgress, "second")
	record := testAuditReopenRecord(first.ID, "generation-one")
	if err := writeAuditReopenRecord(root, record); err != nil {
		t.Fatal(err)
	}
	// Provider-writable task prose cannot redirect the host record to another task.
	writeTaskFile(t, filepath.Join(second.Dir, "audit-reopen.json"), `{"generation":"generation-one"}`)

	firstLease, _, err := tryTaskLease(root, first, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	if firstLease.reopen == nil || firstLease.reopen.Generation != record.Generation {
		t.Fatalf("first lease reopen = %#v, want %q", firstLease.reopen, record.Generation)
	}
	if err := moveTaskDir(root, first, stateDone); err != nil {
		t.Fatal(err)
	}
	done, _ := currentTask(root, first.ID)
	if err := finalizeQueuedCompletion(queuedTask{Root: root, Item: done}); err != nil {
		t.Fatal(err)
	}
	if err := firstLease.markCompleted(done.Dir); err != nil {
		t.Fatal(err)
	}
	receipt, ok := readLeaseCompletionReceipt(firstLease.authority, done.Dir)
	if !ok || receipt.AuditReopenGeneration != record.Generation {
		t.Fatalf("completion receipt = %#v, ok=%v", receipt, ok)
	}
	if err := firstLease.consumeAuditReopen(); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := readAuditReopenRecord(root, first.ID); err != nil || ok {
		t.Fatalf("accepted generation remained reusable: ok=%v err=%v", ok, err)
	}
	if err := firstLease.release(); err != nil {
		t.Fatal(err)
	}

	secondLease, _, err := tryTaskLease(root, second, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	if secondLease.reopen != nil {
		t.Fatalf("task-local forgery redirected authority: %#v", secondLease.reopen)
	}
	if err := secondLease.release(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditReopenRecordReplacementPreservesHostGeneration(t *testing.T) {
	root := t.TempDir()
	id := "replace-audit-baseline"
	original := testAuditReopenRecord(id, "generation-original")
	if err := writeAuditReopenRecord(root, original); err != nil {
		t.Fatal(err)
	}
	rebased := original
	rebased.Subject.ChangeTree = "rewritten-subject-tree"
	if err := replaceAuditReopenRecordIfMatches(root, original, rebased); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := readAuditReopenRecord(root, id); err != nil || !ok || !reflect.DeepEqual(got, rebased) {
		t.Fatalf("rebased record = %#v, ok=%v err=%v", got, ok, err)
	}

	replacement := rebased
	replacement.Generation = "generation-replaced"
	if err := writeAuditReopenRecord(root, replacement); err != nil {
		t.Fatal(err)
	}
	secondRebase := rebased
	secondRebase.Subject.ChangeTree = "second-rewrite"
	if err := replaceAuditReopenRecordIfMatches(root, rebased, secondRebase); err == nil {
		t.Fatal("authority replacement was accepted")
	}
	if got, ok, err := readAuditReopenRecord(root, id); err != nil || !ok || !reflect.DeepEqual(got, replacement) {
		t.Fatalf("rejected replacement changed host authority: got=%#v ok=%v err=%v", got, ok, err)
	}
}

func TestInterruptedAcceptedCompletionConsumesAuditReopenGeneration(t *testing.T) {
	root := t.TempDir()
	task := taskForLease(t, root, stateInProgress, "interrupted")
	record := testAuditReopenRecord(task.ID, "generation-crash")
	if err := writeAuditReopenRecord(root, record); err != nil {
		t.Fatal(err)
	}
	lease, _, err := tryTaskLease(root, task, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	if err := moveTaskDir(root, task, stateDone); err != nil {
		t.Fatal(err)
	}
	done, _ := currentTask(root, task.ID)
	if err := finalizeQueuedCompletion(queuedTask{Root: root, Item: done}); err != nil {
		t.Fatal(err)
	}
	if err := lease.markCompleted(done.Dir); err != nil {
		t.Fatal(err)
	}
	// Simulate process death after the receipt is durable but before generation consumption and
	// ordinary lease cleanup: release the kernel descriptors while retaining crash metadata.
	lease.quiesce()
	if err := errors.Join(unlockLeaseFile(lease.local), unlockLeaseFile(lease.authority)); err != nil {
		t.Fatal(err)
	}
	lease.local, lease.authority = nil, nil

	if err := reconcileInterruptedCompletions([]string{root}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := readAuditReopenRecord(root, task.ID); err != nil || ok {
		t.Fatalf("crash replay retained accepted generation: ok=%v err=%v", ok, err)
	}
	if !pathExists(done.Dir) {
		t.Fatal("crash replay restored an accepted audit completion")
	}
}

func TestTrustedManualCompletionConsumesAuditReopenGeneration(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	git("commit", "-q", "--allow-empty", "-m", "manual implementation\n\nCoop-Task: manual")
	root := filepath.Join(repo, tasksRoot)
	task := taskForLease(t, root, stateInProgress, "manual")
	record, err := captureAuditReopen(repo, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAuditReopenRecord(root, record); err != nil {
		t.Fatal(err)
	}
	if err := completeTrustedTask(root, task); err != nil {
		t.Fatal(err)
	}
	done, _ := currentTask(root, task.ID)
	receipt, ok := taskCompletionReceipt(root, done)
	if !ok || receipt.AuditReopenGeneration != record.Generation {
		t.Fatalf("manual completion receipt = %#v, ok=%v", receipt, ok)
	}
	if _, ok, err := readAuditReopenRecord(root, task.ID); err != nil || ok {
		t.Fatalf("manual completion retained generation: ok=%v err=%v", ok, err)
	}
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

// The authority registry is durable trust state, so its location is part of the contract: pin the
// resolved paths against the session store's state root rather than re-deriving them at review time.
func TestLeaseAuthorityRootIsDurableStateNotCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(testLeaseAuthorityRootEnv, "")
	dir, legacy, err := leaseAuthorityRoots()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".local", "state", "coop", "task-leases", leaseAuthorityVersion)
	if dir != want {
		t.Fatalf("authority root = %q, want %q", dir, want)
	}
	sessions, err := defaultSessionStateRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got, session := filepath.Dir(filepath.Dir(dir)), filepath.Dir(sessions); got != session {
		t.Fatalf("authority state family = %q, session store uses %q", got, session)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if legacy != filepath.Join(cache, "coop", "task-leases", leaseAuthorityVersion) {
		t.Fatalf("legacy root = %q, want the old cache path under %q", legacy, cache)
	}
	if dir == legacy || strings.HasPrefix(dir, cache+string(filepath.Separator)) {
		t.Fatalf("authority root %q still resolves inside the OS cache dir %q", dir, cache)
	}
}

func snapshotLeaseAuthorityRegistry(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = string(data)
	}
	if len(files) == 0 {
		t.Fatalf("registry %s is empty", dir)
	}
	return files
}

// seedLeaseAuthorityRegistry writes one of every record kind through the real writers, so adoption
// is tested against sha-keyed names the production code produced rather than hand-built fixtures.
func seedLeaseAuthorityRegistry(t *testing.T, root string, task taskItem) (nonce string) {
	t.Helper()
	authority, err := lockLeaseAuthority(root, task.ID, true, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLeaseCompletionReceipt(authority, task.Dir); err != nil {
		t.Fatal(err)
	}
	receipt, ok := readLeaseCompletionReceipt(authority, task.Dir)
	if !ok {
		t.Fatal("seeded completion receipt did not read back")
	}
	if err := unlockLeaseFile(authority); err != nil {
		t.Fatal(err)
	}
	meta := taskLeaseMetadata{Version: leaseMetadataVersion, RunID: "seed-run", ControllerPID: 4242}
	if err := writeLeaseAuthorityMetadata(root, task.ID, meta); err != nil {
		t.Fatal(err)
	}
	if err := writeAuditReopenRecord(root, testAuditReopenRecord(task.ID, "seed-generation")); err != nil {
		t.Fatal(err)
	}
	if err := appendTrustedDoneDeparture(root, task.ID, strings.Repeat("ab", 16)); err != nil {
		t.Fatal(err)
	}
	index := completionWindowIndex{
		Version: completionWindowVersion,
		Windows: map[string]completionWindowRecord{
			"seed-window": {Baseline: map[string]completionFingerprint{}},
		},
	}
	if err := writeCompletionWindowIndex(root, index); err != nil {
		t.Fatal(err)
	}
	return receipt.Nonce
}

func TestLeaseAuthorityAdoptsPopulatedLegacyCacheRootOnce(t *testing.T) {
	base := t.TempDir()
	newRoot := filepath.Join(base, "state", "coop", "task-leases", leaseAuthorityVersion)
	legacyRoot := filepath.Join(base, "cache", "coop", "task-leases", leaseAuthorityVersion)

	// Write the registry exactly as the pre-upgrade binary left it, in the cache location.
	t.Setenv(testLeaseAuthorityRootEnv, legacyRoot)
	if err := os.MkdirAll(legacyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	task := taskForLease(t, root, stateDone, "adopted")
	nonce := seedLeaseAuthorityRegistry(t, root, task)
	before := snapshotLeaseAuthorityRegistry(t, legacyRoot)

	// Upgrade: the durable root is absent and the cache root is populated.
	t.Setenv(testLeaseAuthorityRootEnv, newRoot)
	t.Setenv(testLeaseAuthorityLegacyRootEnv, legacyRoot)

	receipt, ok := taskCompletionReceipt(root, task)
	if !ok || receipt.Nonce != nonce {
		t.Fatalf("receipt after adoption = %#v, ok=%v; want nonce %s", receipt, ok, nonce)
	}
	if pathExists(legacyRoot) {
		t.Fatalf("legacy cache root %s survived adoption", legacyRoot)
	}
	if got := snapshotLeaseAuthorityRegistry(t, newRoot); !reflect.DeepEqual(got, before) {
		t.Fatalf("adopted registry = %v, want byte-identical %v", got, before)
	}
	if record, ok, err := readAuditReopenRecord(root, task.ID); err != nil || !ok ||
		record.Generation != "seed-generation" {
		t.Fatalf("audit reopen after adoption = %#v, ok=%v, err=%v", record, ok, err)
	}
	if departure, ok, err := readTrustedDoneDeparture(root, task.ID); err != nil || !ok ||
		!slices.Contains(departure.Nonces, strings.Repeat("ab", 16)) {
		t.Fatalf("departure after adoption = %#v, ok=%v, err=%v", departure, ok, err)
	}
	if meta, ok := readLeaseAuthorityMetadata(root, task.ID); !ok || meta.RunID != "seed-run" {
		t.Fatalf("authority metadata after adoption = %#v, ok=%v", meta, ok)
	}
	index, err := readCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.Windows["seed-window"]; !ok {
		t.Fatalf("completion window index after adoption = %#v", index)
	}
	// Adoption is one-shot: with the cache root gone, a later run must not resurrect or consult it.
	if _, err := os.Stat(legacyRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat legacy root after adoption = %v", err)
	}
	if got := snapshotLeaseAuthorityRegistry(t, newRoot); !reflect.DeepEqual(got, before) {
		t.Fatalf("registry changed on the second open: %v", got)
	}
}

func TestLeaseAuthorityFreshInstallSkipsAdoption(t *testing.T) {
	base := t.TempDir()
	newRoot := filepath.Join(base, "state", "coop", "task-leases", leaseAuthorityVersion)
	legacyRoot := filepath.Join(base, "cache", "coop", "task-leases", leaseAuthorityVersion)
	t.Setenv(testLeaseAuthorityRootEnv, newRoot)
	t.Setenv(testLeaseAuthorityLegacyRootEnv, legacyRoot)

	registry, err := openLeaseAuthorityRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if !pathExists(newRoot) {
		t.Fatalf("fresh install did not create %s", newRoot)
	}
	entries, err := os.ReadDir(newRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("fresh registry = %v, err=%v; want empty", entries, err)
	}
	// The adoption lock is only ever created by the adoption path, so its absence is the proof
	// that a fresh install never entered it.
	if lock := filepath.Join(filepath.Dir(newRoot), leaseAuthorityAdoptLockName); pathExists(lock) {
		t.Fatalf("fresh install created the adoption lock %s", lock)
	}
	if pathExists(filepath.Dir(legacyRoot)) {
		t.Fatalf("fresh install created the legacy cache tree %s", filepath.Dir(legacyRoot))
	}
}

// The cross-volume fallback cannot be reached with a rename, so exercise the copy directly: it is
// the path that must not lose or truncate a receipt, including when it resumes over crash debris.
func TestLeaseAuthorityCrossVolumeAdoptionCopiesEveryRecord(t *testing.T) {
	base := t.TempDir()
	newRoot := filepath.Join(base, "state", "task-leases", leaseAuthorityVersion)
	legacyRoot := filepath.Join(base, "cache", "task-leases", leaseAuthorityVersion)
	if err := os.MkdirAll(filepath.Join(legacyRoot, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	records := map[string]os.FileMode{"aaa.json": 0o644, "bbb.lock": 0o600}
	for name, perm := range records {
		if err := os.WriteFile(filepath.Join(legacyRoot, name), []byte("record "+name+"\n"), perm); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(legacyRoot, ".aaa.json-9-9-0"), []byte("debris"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Debris from an adoption that crashed mid-copy must not survive into the adopted registry.
	staging := filepath.Join(filepath.Dir(newRoot), "."+filepath.Base(newRoot)+".adopting")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "half-copied.json"), []byte("truncated"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := copyLegacyLeaseAuthorityRoot(newRoot, legacyRoot); err != nil {
		t.Fatal(err)
	}
	if pathExists(legacyRoot) || pathExists(staging) {
		t.Fatalf("legacy %s / staging %s survived the copy", legacyRoot, staging)
	}
	got := snapshotLeaseAuthorityRegistry(t, newRoot)
	want := map[string]string{"aaa.json": "record aaa.json\n", "bbb.lock": "record bbb.lock\n"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("copied registry = %v, want %v", got, want)
	}
	for name, perm := range records {
		info, err := os.Lstat(filepath.Join(newRoot, name))
		if err != nil || info.Mode().Perm() != perm {
			t.Fatalf("copied %s mode = %v, err=%v; want %v", name, info.Mode().Perm(), err, perm)
		}
	}
}

func TestLeaseAuthorityAdoptionIsSerializedAcrossAdopters(t *testing.T) {
	base := t.TempDir()
	newRoot := filepath.Join(base, "state", "task-leases", leaseAuthorityVersion)
	legacyRoot := filepath.Join(base, "cache", "task-leases", leaseAuthorityVersion)
	if err := os.MkdirAll(legacyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyRoot, "receipt.lock"), []byte("receipt\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	const adopters = 8
	errs := make([]error, adopters)
	var wg sync.WaitGroup
	for i := range adopters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = adoptLegacyLeaseAuthorityRoot(newRoot, legacyRoot)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("adopter %d = %v", i, err)
		}
	}
	if pathExists(legacyRoot) {
		t.Fatalf("legacy root %s survived concurrent adoption", legacyRoot)
	}
	got := snapshotLeaseAuthorityRegistry(t, newRoot)
	if !reflect.DeepEqual(got, map[string]string{"receipt.lock": "receipt\n"}) {
		t.Fatalf("concurrently adopted registry = %v", got)
	}
}

// replaceLeaseAuthorityRecord unlinks a record and recreates its name. The caller still holds an fd
// on the original inode, so the kernel cannot recycle that inode number — the swap is guaranteed to
// produce a different identity, which is exactly the purge-underfoot race the recheck must catch.
func replaceLeaseAuthorityRecord(t *testing.T, name string) {
	t.Helper()
	path := filepath.Join(os.Getenv(testLeaseAuthorityRootEnv), name)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseAuthorityLockRechecksInodeIdentity(t *testing.T) {
	root := t.TempDir()
	task := taskForLease(t, root, stateTodo, "identity")
	key, err := leaseAuthorityKey(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	name := key + ".lock"
	flock := func(f *os.File) error {
		return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}

	t.Run("a swapped inode is dropped and the lock retaken on the current one", func(t *testing.T) {
		attempts := 0
		file, err := lockLeaseAuthorityWith(root, task.ID, true, func(f *os.File) error {
			if err := flock(f); err != nil {
				return err
			}
			attempts++
			if attempts == 1 {
				replaceLeaseAuthorityRecord(t, name)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if attempts != 2 {
			t.Fatalf("lock attempts = %d, want 2 (one swap, one retry)", attempts)
		}
		locked, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		named, err := os.Lstat(filepath.Join(os.Getenv(testLeaseAuthorityRootEnv), name))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(locked, named) {
			t.Fatal("returned lock is not held on the inode the registry name resolves to")
		}
		if err := unlockLeaseFile(file); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a name that keeps changing identity fails instead of holding an orphan", func(t *testing.T) {
		attempts := 0
		file, err := lockLeaseAuthorityWith(root, task.ID, true, func(f *os.File) error {
			if err := flock(f); err != nil {
				return err
			}
			attempts++
			replaceLeaseAuthorityRecord(t, name)
			return nil
		})
		if file != nil || !errors.Is(err, errLeaseAuthorityIdentity) {
			t.Fatalf("relentless swap = %v, %v; want errLeaseAuthorityIdentity", file, err)
		}
		if attempts != leaseAuthorityIdentityAttempts {
			t.Fatalf("lock attempts = %d, want the bound %d", attempts, leaseAuthorityIdentityAttempts)
		}
	})

	t.Run("a record removed underfoot surfaces as gone, not as a held lock", func(t *testing.T) {
		file, err := lockLeaseAuthorityWith(root, task.ID, false, func(f *os.File) error {
			if err := flock(f); err != nil {
				return err
			}
			return os.Remove(filepath.Join(os.Getenv(testLeaseAuthorityRootEnv), name))
		})
		if file != nil || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("removed record = %v, %v; want os.ErrNotExist", file, err)
		}
	})
}
