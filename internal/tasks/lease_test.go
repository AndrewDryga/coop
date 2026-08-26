package tasks

import (
	"bufio"
	"encoding/json"
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

func testLeaseOwner() TaskLeaseOwner {
	return TaskLeaseOwner{
		RunID: "test-run", PID: 4242, Provider: "codex", Target: "codex:gpt-test@work",
		Now: func() time.Time { return time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC) },
	}
}

func taskCompletionReceipt(root string, task Item) (leaseCompletionReceipt, bool) {
	receipt, ok, _ := inspectTaskCompletionReceipt(root, task)
	return receipt, ok
}

func taskCompletionRecorded(root string, task Item) bool {
	_, ok := taskCompletionReceipt(root, task)
	return ok
}

// taskOwned reads a task's durable claim record for a test assertion, failing the test outright on
// a read error (a corrupt/mismatched record must never be silently treated as "no owner" — see
// readTaskOwnerRecord) rather than being mistaken for the common "never claimed" case.
func taskOwned(t *testing.T, root, id string) (TaskOwnerRecord, bool) {
	t.Helper()
	rec, ok, err := ReadTaskOwnerRecord(root, id)
	if err != nil {
		t.Fatalf("readTaskOwnerRecord(%s): %v", id, err)
	}
	return rec, ok
}

func taskForLease(t *testing.T, root, state, id string) Item {
	t.Helper()
	writeTaskFile(t, filepath.Join(root, state, id, "task.md"), "# "+id+"\n")
	item, ok := CurrentTask(root, id)
	if !ok {
		t.Fatalf("could not read task %s", id)
	}
	return item
}

func testAuditReopenRecord(id, generation string) AuditReopenRecord {
	return AuditReopenRecord{
		Version: auditReopenVersion, Generation: generation, TaskID: id,
		BaselineHead: strings.Repeat("a", 40),
		Subject:      AuditReopenCommit{TaskID: id, ChangeTree: "subject-tree"},
		History: []AuditReopenCommit{{
			TaskID: "descendant", ChangeTree: "descendant-tree",
		}},
	}
}

func writeRawAuditReopenRecord(t *testing.T, root, id string, body []byte) []byte {
	t.Helper()
	name, err := auditReopenRecordName(root, id)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	body = append(append([]byte(nil), body...), '\n')
	if err := AtomicWriteTaskFile(registry, name, body); err != nil {
		t.Fatal(err)
	}
	return body
}

func leaseAuthorityBytes(t *testing.T, root, id string) []byte {
	t.Helper()
	authority, err := OpenLeaseAuthority(root, id, false)
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	if _, err := authority.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(authority)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func seedCompletionReceiptBytes(t *testing.T, root string, task Item) []byte {
	t.Helper()
	authority, err := lockLeaseAuthority(root, task.ID, true, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := completionReceiptFor(task.Dir)
	if err != nil {
		_ = unlockLeaseFile(authority)
		t.Fatal(err)
	}
	receipt.Nonce = "0123456789abcdef0123456789abcdef"
	if err := writeLeaseCompletionReceiptValue(authority, receipt); err != nil {
		_ = unlockLeaseFile(authority)
		t.Fatal(err)
	}
	if err := unlockLeaseFile(authority); err != nil {
		t.Fatal(err)
	}
	return leaseAuthorityBytes(t, root, task.ID)
}

func TestAuditReopenRecordVersionsFailClosed(t *testing.T) {
	t.Run("new record requires baseline and non-nil history", func(t *testing.T) {
		record := testAuditReopenRecord("new", "generation")
		record.History = nil
		if validateAuditReopenRecord(record, record.TaskID) == nil {
			t.Fatal("new record with nil history was accepted")
		}
		record.History = []AuditReopenCommit{}
		record.BaselineHead = ""
		if validateAuditReopenRecord(record, record.TaskID) == nil {
			t.Fatal("new record without baseline HEAD was accepted")
		}
	})
	t.Run("unsupported versions cannot authorize lifecycle changes", func(t *testing.T) {
		for _, version := range []int{1, 2, 5} {
			t.Run(fmt.Sprintf("version=%d", version), func(t *testing.T) {
				root := t.TempDir()
				task := taskForLease(t, root, StateInProgress, fmt.Sprintf("unsupported-%d", version))
				var body []byte
				if version <= 2 {
					pending := ""
					if version == 2 {
						pending = `,"unblock_pending":true`
					}
					body = []byte(fmt.Sprintf(
						`{"version":%d,"generation":"unsupported-generation","task_id":%q,"subject":{"task_id":%q,"change_tree":"subject-tree"},"descendants":[]%s}`,
						version, task.ID, task.ID, pending,
					))
				} else {
					record := testAuditReopenRecord(task.ID, "unsupported-generation")
					record.Version = version
					var err error
					body, err = json.Marshal(record)
					if err != nil {
						t.Fatal(err)
					}
				}
				written := writeRawAuditReopenRecord(t, root, task.ID, body)
				receiptBytes := seedCompletionReceiptBytes(t, root, task)
				if _, ok, err := ReadAuditReopenRecord(root, task.ID); err == nil || ok ||
					!strings.Contains(err.Error(), fmt.Sprintf("unsupported audit reopen record version %d", version)) {
					t.Fatalf("unsupported read = ok=%v err=%v", ok, err)
				}
				lease, _, err := TryTaskLease(root, task, testLeaseOwner())
				if lease != nil || err == nil ||
					!strings.Contains(err.Error(), fmt.Sprintf("unsupported audit reopen record version %d", version)) {
					t.Fatalf("unsupported lease = %#v, %v", lease, err)
				}
				if got := leaseAuthorityBytes(t, root, task.ID); !slices.Equal(got, receiptBytes) {
					t.Fatalf("unsupported lease changed completion receipt: %q, want %q", got, receiptBytes)
				}
				if code, err := tasksFolderMove(root, []string{task.ID}, StateDone, "done", "completed"); code != -1 || err == nil || !strings.Contains(err.Error(), fmt.Sprintf("unsupported audit reopen record version %d", version)) {
					t.Fatalf("unsupported completion = code %d err=%v", code, err)
				}
				if current, ok := CurrentTask(root, task.ID); !ok || current.State != StateInProgress {
					t.Fatalf("unsupported authority moved task: %#v ok=%v", current, ok)
				}
				name, _ := auditReopenRecordName(root, task.ID)
				registry, openErr := OpenLeaseAuthorityRoot()
				if openErr != nil {
					t.Fatal(openErr)
				}
				got, readErr := ReadTaskMetadataFile(registry, name)
				_ = registry.Close()
				if readErr != nil || !slices.Equal(got, written) {
					t.Fatalf("unsupported authority changed: %q err=%v", got, readErr)
				}
			})
		}
	})
	t.Run("removed fields fail closed", func(t *testing.T) {
		root := t.TempDir()
		task := taskForLease(t, root, StateInProgress, "unknown-field")
		record := testAuditReopenRecord(task.ID, "unknown-field-generation")
		body, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		body = []byte(strings.Replace(string(body), `"version":3`, `"version":3,"descendants":[]`, 1))
		writeRawAuditReopenRecord(t, root, task.ID, body)
		if _, ok, err := ReadAuditReopenRecord(root, task.ID); err == nil || ok ||
			!strings.Contains(err.Error(), `unknown field "descendants"`) {
			t.Fatalf("removed field read = ok=%v err=%v", ok, err)
		}
		if lease, _, err := TryTaskLease(root, task, testLeaseOwner()); lease != nil || err == nil ||
			!strings.Contains(err.Error(), `unknown field "descendants"`) {
			t.Fatalf("removed field lease = %#v err=%v", lease, err)
		}
	})
	t.Run("unsupported blocked authority cannot unblock", func(t *testing.T) {
		root := t.TempDir()
		task := taskForLease(t, root, StateBlocked, "unsupported-blocked")
		decision := filepath.Join(task.Dir, "decision.md")
		writeTaskFile(t, decision, "# Decision\n\n**Resolution:** <!-- unresolved -->\n")
		beforeDecision := readFileString(decision)
		record := testAuditReopenRecord(task.ID, "unsupported-blocked-generation")
		record.Version = 1
		body, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		written := writeRawAuditReopenRecord(t, root, task.ID, body)
		code, err := tasksFolderUnblock(root, []string{task.ID, "must not be recorded"})
		if code != -1 || err == nil || !strings.Contains(err.Error(), "unsupported audit reopen record version 1") {
			t.Fatalf("unsupported unblock = code %d err=%v", code, err)
		}
		if current, ok := CurrentTask(root, task.ID); !ok || current.State != StateBlocked {
			t.Fatalf("unsupported unblock moved task: %#v ok=%v", current, ok)
		}
		if after := readFileString(decision); after != beforeDecision {
			t.Fatalf("unsupported unblock changed decision:\n%s", after)
		}
		name, _ := auditReopenRecordName(root, task.ID)
		registry, openErr := OpenLeaseAuthorityRoot()
		if openErr != nil {
			t.Fatal(openErr)
		}
		got, readErr := ReadTaskMetadataFile(registry, name)
		_ = registry.Close()
		if readErr != nil || !slices.Equal(got, written) {
			t.Fatalf("unsupported unblock changed authority: %q err=%v", got, readErr)
		}
	})
	t.Run("pending authority names activation before completion", func(t *testing.T) {
		repo, git := gitRepo(t)
		t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
		git("commit", "-q", "--allow-empty", "-m", "base")
		git("commit", "-q", "--allow-empty", "-m", "pending implementation\n\nCoop-Task: pending")
		root := filepath.Join(repo, TasksRoot)
		task := taskForLease(t, root, StateTodo, "pending")
		record, err := CaptureAuditReopen(repo, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		record.Version = auditReopenPendingVersion
		record.UnblockPending = true
		if err := WriteAuditReopenRecord(root, record); err != nil {
			t.Fatal(err)
		}
		receiptBytes := seedCompletionReceiptBytes(t, root, task)
		lease, _, leaseErr := TryTaskLease(root, task, testLeaseOwner())
		if lease != nil || leaseErr == nil || !strings.Contains(leaseErr.Error(), "non-authorizing pending audit unblock") {
			t.Fatalf("pending lease = %#v err=%v", lease, leaseErr)
		}
		if got := leaseAuthorityBytes(t, root, task.ID); !slices.Equal(got, receiptBytes) {
			t.Fatalf("pending lease changed completion receipt: %q, want %q", got, receiptBytes)
		}
		err = CompleteTrustedTask(root, task)
		if err == nil || !strings.Contains(err.Error(), "pending audit authority") ||
			!strings.Contains(err.Error(), "coop tasks unblock "+task.ID) {
			t.Fatalf("pending completion error = %v", err)
		}
		code, err := tasksFolderMove(root, []string{task.ID}, StateDone, "done", "completed")
		if code != -1 || err == nil ||
			!strings.Contains(err.Error(), "coop tasks unblock "+task.ID) ||
			strings.Contains(err.Error(), "retry: coop tasks done") {
			t.Fatalf("pending tasks done recovery = code %d err %v", code, err)
		}
		current, ok := CurrentTask(root, task.ID)
		if !ok || current.State != StateTodo {
			t.Fatalf("pending completion moved task: %#v, ok=%v", current, ok)
		}
	})
}

func TestTaskLeaseAuditReopenAuthorityIsScopedConsumedAndNotReusable(t *testing.T) {
	root := t.TempDir()
	first := taskForLease(t, root, StateInProgress, "first")
	second := taskForLease(t, root, StateInProgress, "second")
	record := testAuditReopenRecord(first.ID, "generation-one")
	if err := WriteAuditReopenRecord(root, record); err != nil {
		t.Fatal(err)
	}
	// Provider-writable task prose cannot redirect the host record to another task.
	writeTaskFile(t, filepath.Join(second.Dir, "audit-reopen.json"), `{"generation":"generation-one"}`)

	firstLease, _, err := TryTaskLease(root, first, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	if firstLease.Reopen == nil || firstLease.Reopen.Generation != record.Generation {
		t.Fatalf("first lease reopen = %#v, want %q", firstLease.Reopen, record.Generation)
	}
	if err := MoveTaskDir(root, first, StateDone); err != nil {
		t.Fatal(err)
	}
	done, _ := CurrentTask(root, first.ID)
	if err := FinalizeQueuedCompletion(QueuedTask{Root: root, Item: done}); err != nil {
		t.Fatal(err)
	}
	if err := firstLease.MarkCompleted(done.Dir); err != nil {
		t.Fatal(err)
	}
	receipt, ok := readLeaseCompletionReceipt(firstLease.authority, done.Dir)
	if !ok || receipt.AuditReopenGeneration != record.Generation {
		t.Fatalf("completion receipt = %#v, ok=%v", receipt, ok)
	}
	if err := firstLease.ConsumeAuditReopen(); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadAuditReopenRecord(root, first.ID); err != nil || ok {
		t.Fatalf("accepted generation remained reusable: ok=%v err=%v", ok, err)
	}
	if err := firstLease.Release(); err != nil {
		t.Fatal(err)
	}

	secondLease, _, err := TryTaskLease(root, second, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	if secondLease.Reopen != nil {
		t.Fatalf("task-local forgery redirected authority: %#v", secondLease.Reopen)
	}
	if err := secondLease.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditReopenRecordReplacementPreservesHostGeneration(t *testing.T) {
	root := t.TempDir()
	id := "replace-audit-baseline"
	original := testAuditReopenRecord(id, "generation-original")
	if err := WriteAuditReopenRecord(root, original); err != nil {
		t.Fatal(err)
	}
	rebased := original
	rebased.Subject.ChangeTree = "rewritten-subject-tree"
	if err := replaceAuditReopenRecordIfMatches(root, original, rebased); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := ReadAuditReopenRecord(root, id); err != nil || !ok || !reflect.DeepEqual(got, rebased) {
		t.Fatalf("rebased record = %#v, ok=%v err=%v", got, ok, err)
	}

	replacement := rebased
	replacement.Generation = "generation-replaced"
	if err := WriteAuditReopenRecord(root, replacement); err != nil {
		t.Fatal(err)
	}
	secondRebase := rebased
	secondRebase.Subject.ChangeTree = "second-rewrite"
	if err := replaceAuditReopenRecordIfMatches(root, rebased, secondRebase); err == nil {
		t.Fatal("authority replacement was accepted")
	}
	if got, ok, err := ReadAuditReopenRecord(root, id); err != nil || !ok || !reflect.DeepEqual(got, replacement) {
		t.Fatalf("rejected replacement changed host authority: got=%#v ok=%v err=%v", got, ok, err)
	}
}

func TestInterruptedAcceptedCompletionConsumesAuditReopenGeneration(t *testing.T) {
	root := t.TempDir()
	task := taskForLease(t, root, StateInProgress, "interrupted")
	record := testAuditReopenRecord(task.ID, "generation-crash")
	if err := WriteAuditReopenRecord(root, record); err != nil {
		t.Fatal(err)
	}
	lease, _, err := TryTaskLease(root, task, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	if err := MoveTaskDir(root, task, StateDone); err != nil {
		t.Fatal(err)
	}
	done, _ := CurrentTask(root, task.ID)
	if err := FinalizeQueuedCompletion(QueuedTask{Root: root, Item: done}); err != nil {
		t.Fatal(err)
	}
	if err := lease.MarkCompleted(done.Dir); err != nil {
		t.Fatal(err)
	}
	// Simulate process death after the receipt is durable but before generation consumption and
	// ordinary lease cleanup: release the kernel descriptors while retaining crash metadata.
	lease.Quiesce()
	if err := unlockLeaseFile(lease.authority); err != nil {
		t.Fatal(err)
	}
	lease.authority = nil

	if err := ReconcileInterruptedCompletions([]string{root}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadAuditReopenRecord(root, task.ID); err != nil || ok {
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
	root := filepath.Join(repo, TasksRoot)
	task := taskForLease(t, root, StateInProgress, "manual")
	record, err := CaptureAuditReopen(repo, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteAuditReopenRecord(root, record); err != nil {
		t.Fatal(err)
	}
	if err := CompleteTrustedTask(root, task); err != nil {
		t.Fatal(err)
	}
	done, _ := CurrentTask(root, task.ID)
	receipt, ok := taskCompletionReceipt(root, done)
	if !ok || receipt.AuditReopenGeneration != record.Generation {
		t.Fatalf("manual completion receipt = %#v, ok=%v", receipt, ok)
	}
	if _, ok, err := ReadAuditReopenRecord(root, task.ID); err != nil || ok {
		t.Fatalf("manual completion retained generation: ok=%v err=%v", ok, err)
	}
}

// TestTaskOwnerRecordRoundTripsMismatchAndCorruption covers the owner record's registry I/O in
// isolation from any CLI verb: a clean round trip, a record whose TaskID doesn't match the id being
// read (mirroring the audit-reopen record's own id-binding check), and a corrupt body — both of the
// latter two must fail closed (an error), never silently read back as "no owner".
func TestTaskOwnerRecordRoundTripsMismatchAndCorruption(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		root := t.TempDir()
		want := TaskOwnerRecord{
			Version: taskOwnerRecordVersion, TaskID: "round-trip", Source: taskOwnerSourceInteractiveClaim,
			User: "ada", Host: "workstation", ClaimedAt: time.Now().Truncate(time.Second),
		}
		if err := writeTaskOwnerRecord(root, want); err != nil {
			t.Fatal(err)
		}
		got, ok, err := ReadTaskOwnerRecord(root, "round-trip")
		if err != nil || !ok || !got.ClaimedAt.Equal(want.ClaimedAt) ||
			got.Version != want.Version || got.TaskID != want.TaskID || got.Source != want.Source ||
			got.User != want.User || got.Host != want.Host {
			t.Fatalf("round trip = %#v, ok=%v err=%v, want %#v", got, ok, err, want)
		}
		if err := removeTaskOwnerRecord(root, "round-trip"); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := ReadTaskOwnerRecord(root, "round-trip"); err != nil || ok {
			t.Fatalf("after remove: ok=%v err=%v, want absent", ok, err)
		}
		// Removing an already-absent record is a no-op, not an error — most lifecycle transitions
		// (block/unblock/done on a loop-owned task) legitimately have none to clear.
		if err := removeTaskOwnerRecord(root, "round-trip"); err != nil {
			t.Fatalf("idempotent remove: %v", err)
		}
	})

	t.Run("missing record reads as absent, not an error", func(t *testing.T) {
		root := t.TempDir()
		if _, ok, err := ReadTaskOwnerRecord(root, "never-claimed"); err != nil || ok {
			t.Fatalf("never-written record = ok=%v err=%v, want (false, nil)", ok, err)
		}
	})

	t.Run("mismatched task id is rejected, not silently absent", func(t *testing.T) {
		root := t.TempDir()
		record := TaskOwnerRecord{
			Version: taskOwnerRecordVersion, TaskID: "task-a", Source: taskOwnerSourceInteractiveClaim,
			User: "ada", Host: "workstation", ClaimedAt: time.Now(),
		}
		if err := writeTaskOwnerRecord(root, record); err != nil {
			t.Fatal(err)
		}
		// Write task-a's already-validated bytes directly under task-b's registry name — provider-
		// writable task prose must never be able to redirect a claim onto a different task's record
		// (same threat TestTaskLeaseAuditReopenAuthorityIsScopedConsumedAndNotReusable covers for
		// audit-reopen authority).
		name, err := taskOwnerRecordName(root, "task-b")
		if err != nil {
			t.Fatal(err)
		}
		registry, err := OpenLeaseAuthorityRoot()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(record)
		if err := AtomicWriteTaskFile(registry, name, append(data, '\n')); err != nil {
			t.Fatal(err)
		}
		_ = registry.Close()
		if _, ok, err := ReadTaskOwnerRecord(root, "task-b"); err == nil || ok {
			t.Fatalf("mismatched-id record = ok=%v err=%v, want a validation error", ok, err)
		}
	})

	t.Run("corrupt body fails closed", func(t *testing.T) {
		root := t.TempDir()
		name, err := taskOwnerRecordName(root, "corrupt")
		if err != nil {
			t.Fatal(err)
		}
		registry, err := OpenLeaseAuthorityRoot()
		if err != nil {
			t.Fatal(err)
		}
		if err := AtomicWriteTaskFile(registry, name, []byte("not json\n")); err != nil {
			t.Fatal(err)
		}
		_ = registry.Close()
		if _, ok, err := ReadTaskOwnerRecord(root, "corrupt"); err == nil || ok {
			t.Fatalf("corrupt record = ok=%v err=%v, want an error", ok, err)
		}
	})

	t.Run("write validates before touching disk", func(t *testing.T) {
		root := t.TempDir()
		bad := TaskOwnerRecord{Version: taskOwnerRecordVersion, TaskID: "bad", Source: ""} // no source/user/host/time
		if err := writeTaskOwnerRecord(root, bad); err == nil {
			t.Fatal("write accepted an incomplete owner record")
		}
		if _, ok, err := ReadTaskOwnerRecord(root, "bad"); err != nil || ok {
			t.Fatalf("rejected write still landed: ok=%v err=%v", ok, err)
		}
	})
}

func TestTaskLeaseWritesHostHeartbeatAndReleases(t *testing.T) {
	root, id := t.TempDir(), "resume-me"
	item := taskForLease(t, root, StateInProgress, id)
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	owner := testLeaseOwner()
	owner.Now = func() time.Time { return now }
	lease, _, err := TryTaskLease(root, item, owner)
	if err != nil {
		t.Fatal(err)
	}

	meta, ok := readLeaseAuthorityMetadata(root, id)
	if !ok || meta.RunID != owner.RunID || meta.ControllerPID != owner.PID || meta.Provider != owner.Provider || meta.Target != owner.Target {
		t.Fatalf("lease metadata = %+v, ok=%v", meta, ok)
	}
	if !meta.AcquiredAt.Equal(now) || !meta.HeartbeatAt.Equal(now) {
		t.Fatalf("initial metadata timestamps = %+v, want %s", meta, now)
	}

	now = now.Add(10 * time.Second)
	if err := MoveTaskDir(root, item, StateBlocked); err != nil {
		t.Fatal(err)
	}
	if err := lease.refresh(); err != nil {
		t.Fatal(err)
	}
	if got, ok := readLeaseAuthorityMetadata(root, id); !ok || !got.HeartbeatAt.Equal(now) {
		t.Fatalf("host heartbeat after task rename = %+v, ok=%v", got, ok)
	}
	for _, name := range []string{"lease.lock", "lease.json"} {
		if pathExists(filepath.Join(item.Dir, "tmp", name)) ||
			pathExists(filepath.Join(root, StateBlocked, id, "tmp", name)) {
			t.Fatalf("task-local lease mirror %s was created", name)
		}
	}
	doneItem, ok := CurrentTask(root, id)
	if !ok {
		t.Fatal("moved task disappeared before completion")
	}
	if err := MoveTaskDir(root, doneItem, StateDone); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if _, ok := readLeaseAuthorityMetadata(root, id); ok {
		t.Fatal("normal release left host lease metadata behind")
	}
}

func TestTaskLeaseHeartbeatTickerRefreshesMetadata(t *testing.T) {
	root, id := t.TempDir(), "heartbeat"
	item := taskForLease(t, root, StateInProgress, id)
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
	lease, _, err := TryTaskLease(root, item, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Release() })

	now = now.Add(leaseHeartbeatInterval)
	ticks <- now
	deadline := time.Now().Add(time.Second)
	for {
		meta, ok := readLeaseAuthorityMetadata(root, id)
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
	item := taskForLease(t, root, StateInProgress, id)
	ticks := make(chan time.Time, 1)
	owner := testLeaseOwner()
	owner.Ticker = func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }
	lease, _, err := TryTaskLease(root, item, owner)
	if err != nil {
		t.Fatal(err)
	}
	lease.Quiesce()
	before, ok := readLeaseAuthorityMetadata(root, id)
	if !ok {
		t.Fatal("quiesced lease lost host metadata")
	}
	if got := observeTaskLease(item, owner.now()); got.State == leaseUnleased {
		t.Fatal("quiesce released the authoritative task lock")
	}
	ticks <- owner.now().Add(leaseHeartbeatInterval)
	if after, ok := readLeaseAuthorityMetadata(root, id); !ok || !after.HeartbeatAt.Equal(before.HeartbeatAt) {
		t.Fatalf("quiesced heartbeat changed metadata: before=%+v after=%+v ok=%v", before, after, ok)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestTaskLeaseIgnoresProviderControlledTmp(t *testing.T) {
	root, id := t.TempDir(), "provider-tmp"
	item := taskForLease(t, root, StateInProgress, id)
	tmp := filepath.Join(item.Dir, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lease.lock", "lease.json"} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("provider-controlled\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lease, _, err := TryTaskLease(root, item, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	second, observed, err := TryTaskLease(root, item, TaskLeaseOwner{
		RunID: "second", PID: 4343, Provider: "claude", Target: "claude:test",
	})
	if err != nil || second != nil || observed.State == leaseUnleased {
		t.Fatalf("provider-local files affected host contention: lease=%v observed=%+v err=%v", second, observed, err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if got := observeTaskLease(item, testLeaseOwner().now()); got.State != leaseUnleased {
		t.Fatalf("provider-local files made released task look leased: %+v", got)
	}
	third, observed, err := TryTaskLease(root, item, TaskLeaseOwner{
		RunID: "third", PID: 4545, Provider: "gemini", Target: "gemini:test",
	})
	if err != nil || third == nil || observed.State != leaseUnleased {
		t.Fatalf("provider-local files blocked host reacquisition: lease=%v observed=%+v err=%v", third, observed, err)
	}
	if err := third.Release(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lease.lock", "lease.json"} {
		data, readErr := os.ReadFile(filepath.Join(tmp, name))
		if readErr != nil || string(data) != "provider-controlled\n" {
			t.Fatalf("provider-local file %s changed to %q, err=%v", name, data, readErr)
		}
	}
}

func TestTaskLeaseObservationUsesLockNotHeartbeat(t *testing.T) {
	root, id := t.TempDir(), "locked"
	item := taskForLease(t, root, StateInProgress, id)
	now := time.Date(2026, 7, 14, 12, 1, 0, 0, time.UTC)
	owner := testLeaseOwner()
	owner.Now = func() time.Time { return now }
	lease, _, err := TryTaskLease(root, item, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Release() })

	if got := observeTaskLease(item, now); got.State != leaseBusy || got.Provider != "codex" {
		t.Errorf("fresh held lease = %+v, want busy codex", got)
	}
	lease.meta.HeartbeatAt = now.Add(-leaseStaleAfter - time.Second)
	if err := writeLeaseAuthorityMetadata(root, id, lease.meta); err != nil {
		t.Fatal(err)
	}
	if got := observeTaskLease(item, now); got.State != leaseStalled || got.Provider != "codex" {
		t.Errorf("stale held lease = %+v, want stalled codex", got)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if got := observeTaskLease(item, now); got.State != leaseUnleased {
		t.Errorf("released lease = %+v, want unleased", got)
	}
}

func TestTaskLeaseAdoptionIgnoresMetadataPIDWhenLockIsFree(t *testing.T) {
	root, id := t.TempDir(), "pid-reused"
	taskForLease(t, root, StateInProgress, id)
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
	authority, err := OpenLeaseAuthority(root, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeLeaseAuthorityMetadata(root, id, stale); err != nil {
		t.Fatal(err)
	}

	owner := testLeaseOwner()
	owner.RunID = "new-run"
	owner.Now = time.Now
	assignment, err := assignLoopTask([]string{root}, owner)
	if err != nil || assignment.Outcome != assignmentReady || assignment.Task.Item.ID != id {
		t.Fatalf("PID-reuse adoption = %+v, err=%v", assignment, err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAssignLoopTaskSkipsForeignLeaseAndFallsBackToTodo(t *testing.T) {
	root := t.TempDir()
	busy := taskForLease(t, root, StateInProgress, "a-busy")
	taskForLease(t, root, StateTodo, "b-todo")
	foreign, _, err := TryTaskLease(root, busy, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = foreign.Release() })

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
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}

	onlyBusy := t.TempDir()
	item := taskForLease(t, onlyBusy, StateInProgress, "busy")
	foreignOnly, _, err := TryTaskLease(onlyBusy, item, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	defer foreignOnly.Release()
	assignment, err = assignLoopTask([]string{onlyBusy}, owner)
	if err != nil || assignment.Outcome != AssignmentUnavailable || assignment.Busy.Busy != 1 {
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
		_ = assignment.Lease.Release()
	case AssignmentUnavailable:
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
		if state == StateTodo && pathExists(filepath.Join(root, StateTodo, id)) {
			t.Fatal("losing todo contender recreated the task's old state path")
		}
		items := ReadTaskTree(root)
		if len(items) != 1 || items[0].ID != id || items[0].State != StateInProgress {
			t.Fatalf("simultaneous %s claim left queue %+v, want one in-progress task", state, items)
		}
		first.release(t)
		second.release(t)
	}
	t.Run("simultaneous todo claim", func(t *testing.T) { runRace(t, StateTodo) })
	t.Run("simultaneous in-progress adoption", func(t *testing.T) { runRace(t, StateInProgress) })

	t.Run("dead owner is adopted immediately", func(t *testing.T) {
		root, id := t.TempDir(), "recover"
		taskForLease(t, root, StateInProgress, id)
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
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("stale heartbeat with a live lock stays stalled", func(t *testing.T) {
		root, id := t.TempDir(), "stalled"
		item := taskForLease(t, root, StateInProgress, id)
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
		if err != nil || assignment.Outcome != AssignmentUnavailable || assignment.Busy.Stalled != 1 {
			t.Fatalf("stalled lock assignment = %+v, err=%v", assignment, err)
		}
		holder.release(t)
	})

	t.Run("two tasks let two controllers win", func(t *testing.T) {
		root := t.TempDir()
		taskForLease(t, root, StateTodo, "a")
		taskForLease(t, root, StateTodo, "b")
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
	t.Setenv(TestLeaseAuthorityRootEnv, "")
	dir, err := leaseAuthorityRoot()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".local", "state", "coop", "task-leases", LeaseAuthorityVersion)
	if dir != want {
		t.Fatalf("authority root = %q, want %q", dir, want)
	}
	sessions, err := defaultSessionStateRootForTest()
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := legacyLeaseAuthorityRoot()
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
	if legacy != filepath.Join(cache, "coop", "task-leases", LeaseAuthorityVersion) {
		t.Fatalf("legacy root = %q, want the old cache path under %q", legacy, cache)
	}
	if dir == legacy || strings.HasPrefix(dir, cache+string(filepath.Separator)) {
		t.Fatalf("authority root %q still resolves inside the OS cache dir %q", dir, cache)
	}
}

func TestLeaseAuthorityRefusesPopulatedLegacyRootWithoutMutation(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "state", "coop", "task-leases", LeaseAuthorityVersion)
	legacy := filepath.Join(base, "cache", "coop", "task-leases", LeaseAuthorityVersion)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(legacy, "receipt.lock")
	want := []byte("durable receipt bytes\n")
	if err := os.WriteFile(record, want, 0o600); err != nil {
		t.Fatal(err)
	}

	registry, err := openLeaseAuthorityRootAt(dir, func() (string, error) { return legacy, nil })
	if registry != nil || err == nil {
		t.Fatalf("open populated legacy root = (%v, %v), want refusal", registry, err)
	}
	for _, text := range []string{legacy, dir, "not empty", "stop all older Coop processes", "migrate the whole directory"} {
		if !strings.Contains(err.Error(), text) {
			t.Fatalf("refusal %q does not contain %q", err, text)
		}
	}
	if pathExists(filepath.Dir(dir)) {
		t.Fatalf("refusal created durable parent %s", filepath.Dir(dir))
	}
	for _, artifact := range []string{".adopt.lock", ".v1.adopting"} {
		if pathExists(filepath.Join(filepath.Dir(dir), artifact)) {
			t.Fatalf("refusal created retired adoption artifact %s", artifact)
		}
	}
	if got, readErr := os.ReadFile(record); readErr != nil || !slices.Equal(got, want) {
		t.Fatalf("legacy record after refusal = %q, err=%v; want exact %q", got, readErr, want)
	}
}

func TestLeaseAuthorityRefusesEveryKindOfLegacyEntry(t *testing.T) {
	cases := map[string]func(*testing.T, string){
		"dotfile": func(t *testing.T, legacy string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(legacy, ".partial"), []byte("debris"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"subdirectory": func(t *testing.T, legacy string) {
			t.Helper()
			if err := os.Mkdir(filepath.Join(legacy, "unexpected"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, legacy string) {
			t.Helper()
			if err := os.Symlink(t.TempDir(), filepath.Join(legacy, "pointer")); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(t *testing.T, legacy string) {
			t.Helper()
			if err := syscall.Mkfifo(filepath.Join(legacy, "pipe"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "state", "task-leases", LeaseAuthorityVersion)
			legacy := filepath.Join(base, "cache", "task-leases", LeaseAuthorityVersion)
			if err := os.MkdirAll(legacy, 0o700); err != nil {
				t.Fatal(err)
			}
			seed(t, legacy)

			registry, err := openLeaseAuthorityRootAt(dir, func() (string, error) { return legacy, nil })
			if registry != nil || err == nil || !strings.Contains(err.Error(), "not empty") {
				t.Fatalf("open legacy %s entry = (%v, %v), want not-empty refusal", name, registry, err)
			}
			if pathExists(filepath.Dir(dir)) {
				t.Fatalf("refusal created durable parent %s", filepath.Dir(dir))
			}
		})
	}
}

func TestLeaseAuthorityRefusesMalformedLegacyRoot(t *testing.T) {
	for _, kind := range []string{"regular-file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "state", "task-leases", LeaseAuthorityVersion)
			legacy := filepath.Join(base, "cache", "task-leases", LeaseAuthorityVersion)
			if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
				t.Fatal(err)
			}
			if kind == "regular-file" {
				if err := os.WriteFile(legacy, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(t.TempDir(), legacy); err != nil {
				t.Fatal(err)
			}

			registry, err := openLeaseAuthorityRootAt(dir, func() (string, error) { return legacy, nil })
			if registry != nil || err == nil || !strings.Contains(err.Error(), "real directory") {
				t.Fatalf("open malformed legacy %s = (%v, %v), want refusal", kind, registry, err)
			}
			if pathExists(filepath.Dir(dir)) {
				t.Fatalf("refusal created durable parent %s", filepath.Dir(dir))
			}
		})
	}
}

func TestLeaseAuthorityRefusesUnreadableLegacyRoot(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "state", "task-leases", LeaseAuthorityVersion)
	legacy := filepath.Join(base, "cache", "task-leases", LeaseAuthorityVersion)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(legacy, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(legacy, 0o700) })
	if _, err := os.ReadDir(legacy); err == nil {
		t.Skip("test user can read a mode-000 directory")
	}

	registry, err := openLeaseAuthorityRootAt(dir, func() (string, error) { return legacy, nil })
	if registry != nil || err == nil || !strings.Contains(err.Error(), legacy) {
		t.Fatalf("open unreadable legacy root = (%v, %v), want refusal", registry, err)
	}
	if pathExists(filepath.Dir(dir)) {
		t.Fatalf("refusal created durable parent %s", filepath.Dir(dir))
	}
}

func TestLeaseAuthorityRefusesLegacyResolverFailure(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "state", "task-leases", LeaseAuthorityVersion)
	wantErr := errors.New("cache root unavailable")
	registry, err := openLeaseAuthorityRootAt(dir, func() (string, error) { return "", wantErr })
	if registry != nil || !errors.Is(err, wantErr) {
		t.Fatalf("open with legacy resolver failure = (%v, %v), want wrapped %v", registry, err, wantErr)
	}
	for _, text := range []string{dir, "OS cache", "stop all older Coop processes", "migrate"} {
		if !strings.Contains(err.Error(), text) {
			t.Fatalf("resolver refusal %q does not contain %q", err, text)
		}
	}
	if pathExists(filepath.Dir(dir)) {
		t.Fatalf("resolver refusal created durable parent %s", filepath.Dir(dir))
	}
}

func TestLeaseAuthorityFreshRootAllowsMissingOrEmptyLegacy(t *testing.T) {
	for _, kind := range []string{"missing", "empty"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "state", "task-leases", LeaseAuthorityVersion)
			legacy := filepath.Join(base, "cache", "task-leases", LeaseAuthorityVersion)
			if kind == "empty" {
				if err := os.MkdirAll(legacy, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			registry, err := openLeaseAuthorityRootAt(dir, func() (string, error) { return legacy, nil })
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.Close(); err != nil {
				t.Fatal(err)
			}
			if !pathExists(dir) {
				t.Fatalf("fresh install did not create %s", dir)
			}
			if kind == "missing" && pathExists(filepath.Dir(legacy)) {
				t.Fatalf("fresh install created retired cache parent %s", filepath.Dir(legacy))
			}
			if kind == "empty" {
				entries, readErr := os.ReadDir(legacy)
				if readErr != nil || len(entries) != 0 {
					t.Fatalf("empty legacy root after open = %v, err=%v", entries, readErr)
				}
			}
		})
	}
}

func TestLeaseAuthorityExistingDurableRootNeverResolvesLegacy(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "state", "task-leases", LeaseAuthorityVersion)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "kept.json")
	want := []byte("current authority\n")
	if err := os.WriteFile(record, want, 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	registry, err := openLeaseAuthorityRootAt(dir, func() (string, error) {
		called = true
		return "", errors.New("must not resolve retired state")
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("existing current authority resolved the retired cache path")
	}
	if got, readErr := os.ReadFile(record); readErr != nil || !slices.Equal(got, want) {
		t.Fatalf("current record after open = %q, err=%v; want exact %q", got, readErr, want)
	}
}

func TestLeaseAuthorityConcurrentFreshOpensConverge(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "state", "task-leases", LeaseAuthorityVersion)
	legacy := filepath.Join(base, "cache", "task-leases", LeaseAuthorityVersion)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	const openers = 8
	errs := make([]error, openers)
	var wg sync.WaitGroup
	for i := range openers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			registry, err := openLeaseAuthorityRootAt(dir, func() (string, error) { return legacy, nil })
			if err == nil {
				err = registry.Close()
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("opener %d = %v", i, err)
		}
	}
	if !pathExists(dir) {
		t.Fatalf("concurrent opens did not create %s", dir)
	}
}

// replaceLeaseAuthorityRecord unlinks a record and recreates its name. The caller still holds an fd
// on the original inode, so the kernel cannot recycle that inode number — the swap is guaranteed to
// produce a different identity, which is exactly the purge-underfoot race the recheck must catch.
func replaceLeaseAuthorityRecord(t *testing.T, name string) {
	t.Helper()
	path := filepath.Join(os.Getenv(TestLeaseAuthorityRootEnv), name)
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
	task := taskForLease(t, root, StateTodo, "identity")
	key, err := LeaseAuthorityKey(root, task.ID)
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
		named, err := os.Lstat(filepath.Join(os.Getenv(TestLeaseAuthorityRootEnv), name))
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
			return os.Remove(filepath.Join(os.Getenv(TestLeaseAuthorityRootEnv), name))
		})
		if file != nil || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("removed record = %v, %v; want os.ErrNotExist", file, err)
		}
	})
}
