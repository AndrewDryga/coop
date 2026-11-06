package tasks

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

// reanchorFixture is a blocked task an audit reopened at its commit, after which other work landed
// the way it lands on main: one task bound by two commits, one commit closing two tasks, and a commit
// with no task. None of those can join the audit's exact replay, which once kept the task blocked for
// good (2026-11-06).
type reanchorFixture struct {
	repo, root, id string
	subject        string   // the reopened task's own commit
	landed         []string // the commits after the review, oldest first
	task           Item
	record         AuditReopenRecord
	git            func(...string)
}

const reanchorReviewedLater = "reviewed-later-task"

func newReanchorFixture(t *testing.T, withHistory bool) reanchorFixture {
	t.Helper()
	repo, git := gitRepo(t)
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	f := reanchorFixture{repo: repo, id: "reopened-task", git: git}
	git("commit", "-q", "--allow-empty", "-m", "base")
	f.subject = f.commit(t, "a.txt", "A\n", "A implementation\n\nCoop-Task: "+f.id)
	if withHistory {
		f.commit(t, "b.txt", "B\n", "B implementation\n\nCoop-Task: "+reanchorReviewedLater)
	}
	record, err := CaptureAuditReopen(repo, f.id)
	if err != nil {
		t.Fatal(err)
	}
	f.record = record
	f.root = filepath.Join(repo, TasksRoot)
	f.task = taskForLease(t, f.root, StateBlocked, f.id)
	writeTaskFile(t, filepath.Join(f.task.Dir, "decision.md"), "# Decision\n\n**Resolution:** <!-- unresolved -->\n")
	authority, err := OpenLeaseAuthority(f.root, f.id, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if err := WriteAuditReopenRecord(f.root, record); err != nil {
		t.Fatal(err)
	}
	f.landed = []string{
		f.commit(t, "other.txt", "one\n", "Other work\n\nCoop-Task: other-task"),
		f.commit(t, "other.txt", "two\n", "More of the other work\n\nCoop-Task: other-task"),
		f.commit(t, "pair.txt", "pair\n", "Close two tasks\n\nCoop-Task: first-task\nCoop-Task: second-task"),
		f.commit(t, "docs.txt", "docs\n", "docs: a change with no task"),
	}
	if AuditReopenCurrentValid(repo, f.head(), f.id, record) {
		t.Fatal("fixture: the record still holds at HEAD, so an unblock has nothing to move past")
	}
	return f
}

func (f reanchorFixture) commit(t *testing.T, name, body, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.repo, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git("add", name)
	f.git("commit", "-q", "-m", message)
	return f.head()
}

// commitVerbatim commits a change with message exactly as given, comment lines included.
func (f reanchorFixture) commitVerbatim(t *testing.T, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.repo, "named.txt"), []byte(message), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git("add", "named.txt")
	f.git("commit", "-q", "--cleanup=verbatim", "-m", message)
	return f.head()
}

func (f reanchorFixture) head() string { return forkspace.GitOut(f.repo, "rev-parse", "HEAD") }

// resign rewrites HEAD the way host signing does: same change, author and message, a new committer.
func (f reanchorFixture) resign(t *testing.T) string {
	t.Helper()
	f.git("-c", "user.name=Signer", "-c", "user.email=signer@example.com", "commit", "-q", "--amend", "--no-edit")
	return f.head()
}

// unblock runs the person's command with an answer and returns the record it left.
func (f reanchorFixture) unblock(t *testing.T, answer string) AuditReopenRecord {
	t.Helper()
	if code, err := tasksFolderUnblock(f.repo, f.root, []string{f.id, answer}); code != 0 || err != nil {
		t.Fatalf("unblock = code %d, %v", code, err)
	}
	got, ok, err := ReadAuditReopenRecord(f.root, f.id)
	if err != nil || !ok {
		t.Fatalf("read the unblocked record: ok=%v err=%v", ok, err)
	}
	return got
}

// assertRefused checks a refused unblock kept the task, its record and the person's answer.
func (f reanchorFixture) assertRefused(t *testing.T, answer string, err error, wants ...string) {
	t.Helper()
	wants = append(wants, "task remains blocked, and your answer is saved in decision.md",
		"then retry: coop tasks unblock "+f.id)
	for _, want := range wants {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	if err != nil && strings.Contains(err.Error(), "<answer>") {
		t.Errorf("the retry asks for the answer again although it is saved: %v", err)
	}
	if current, ok := mustCurrentTask(t, f.root, f.id); !ok || current.State != StateBlocked {
		t.Fatalf("refused unblock moved the task: %#v ok=%v", current, ok)
	}
	if got, ok, readErr := ReadAuditReopenRecord(f.root, f.id); readErr != nil || !ok || !AuditReopenRecordsEqual(got, f.record) {
		t.Fatalf("refused unblock changed the record: got=%#v ok=%v err=%v", got, ok, readErr)
	}
	if saved, readErr := recordedAnswer(filepath.Join(f.task.Dir, "decision.md")); readErr != nil || saved != answer {
		t.Fatalf("refused unblock answer = %q, %v; want %q saved", saved, readErr, answer)
	}
}

func TestUnblockMovesAuditAuthorityPastUnrelatedCommits(t *testing.T) {
	f := newReanchorFixture(t, false)
	head := f.head()
	got := f.unblock(t, "use quieter runner first")
	want := f.record
	want.ReviewedHead, want.BaselineHead, want.Repaired = f.subject, head, true // docs commit is unbound
	if !AuditReopenRecordsEqual(got, want) {
		t.Fatalf("unblocked record = %#v, want %#v", got, want)
	}
	current, ok := mustCurrentTask(t, f.root, f.id)
	if !ok || current.State != StateTodo {
		t.Fatalf("unblocked task = %#v, ok=%v", current, ok)
	}
	if answer, err := recordedAnswer(filepath.Join(current.Dir, "decision.md")); err != nil || answer != "use quieter runner first" {
		t.Fatalf("saved answer = %q, %v", answer, err)
	}

	// The loop leases the task and starts the rework at HEAD.
	if err := MoveTaskDir(f.root, current, StateInProgress); err != nil {
		t.Fatal(err)
	}
	inProgress, ok := mustCurrentTask(t, f.root, f.id)
	if !ok {
		t.Fatal("claimed task disappeared")
	}
	lease, _, err := TryTaskLease(f.root, inProgress, testLeaseOwner())
	if err != nil || lease == nil || lease.Reopen == nil || !AuditReopenRecordsEqual(*lease.Reopen, want) {
		t.Fatalf("lease = %#v, %v", lease, err)
	}
	if err := ValidateLeasedAuditReopen(f.repo, head, f.id, lease.Reopen); err != nil {
		t.Fatalf("lease start refused the moved record: %v", err)
	}
	if missing, _ := CompletionUnbindableTasks(f.repo, head, head, []string{f.id}, lease.Reopen, nil); len(missing) != 0 {
		t.Fatalf("a re-close without a commit was refused: %v", missing)
	}
	repair := f.commit(t, "a.txt", "A repaired\n", "Repair the audit finding")
	if missing, _ := CompletionUnbindableTasks(f.repo, head, repair, []string{f.id}, lease.Reopen, nil); len(missing) != 0 {
		t.Fatalf("one repair commit on top of HEAD was refused: %v", missing)
	}
	// Parked again after the repair, the record keeps its baseline, so the loop signing the repair
	// right after does not strand it.
	parked, err := rebasedAuditReopenRecord(f.repo, head, repair, f.id, *lease.Reopen)
	if err != nil || parked.BaselineHead != head || parked.ReviewedHead != f.subject ||
		!AuditReopenCurrentValid(f.repo, repair, f.id, parked) {
		t.Fatalf("record after a parked repair = %#v, %v", parked, err)
	}
	signed := f.resign(t)
	if signed == repair || !AuditReopenCurrentValid(f.repo, signed, f.id, parked) {
		t.Fatalf("signing the parked repair stranded its record (repair %s, signed %s)", repair, signed)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}

	// The person can also close it, which spends the generation.
	writeTaskFile(t, filepath.Join(inProgress.Dir, "task.md"), "# Task\n- [x] required checks passed\n")
	if inProgress, ok = mustCurrentTask(t, f.root, f.id); !ok {
		t.Fatal("task disappeared")
	}
	if err := CompleteTrustedTask(f.repo, f.root, inProgress); err != nil {
		t.Fatalf("complete the reworked task: %v", err)
	}
	if _, ok, err := ReadAuditReopenRecord(f.root, f.id); err != nil || ok {
		t.Fatalf("the spent generation remained: ok=%v err=%v", ok, err)
	}
}

// Only bound commits landed: none can be a repair the worker made, so a later re-close without a
// commit still counts as no work.
func TestUnblockPastBoundCommitsOnlyIsNoRepair(t *testing.T) {
	f := newReanchorFixture(t, true)
	f.git("reset", "--hard", "-q", f.landed[2]) // drop the unbound docs commit
	got := f.unblock(t, "go on")
	if got.Repaired || got.ReviewedHead == "" || got.BaselineHead != f.landed[2] {
		t.Fatalf("record = %#v, want it moved to %s and not marked repaired", got, f.landed[2])
	}
	if !slices.Equal(got.History, f.record.History) || got.Subject != f.record.Subject {
		t.Fatalf("moved record changed the reviewed history: %#v", got)
	}
}

func TestUnblockRefusesACommitThatNamesTheReopenedTask(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		withHistory   bool
		want          string
	}{
		{name: "a second binding", message: "Bound again\n\nCoop-Task: reopened-task",
			want: ", made after the review, also carries Coop-Task: reopened-task"},
		{name: "one of two trailers", message: "Two tasks\n\nCoop-Task: other-task\nCoop-Task: reopened-task",
			want: ", made after the review, also carries Coop-Task: reopened-task"},
		{name: "a task the review recorded after it", message: "Again\n\nCoop-Task: " + reanchorReviewedLater, withHistory: true,
			want: "also carries Coop-Task: " + reanchorReviewedLater + ", a task the review recorded after this one"},
		{name: "a binding only Git reads", message: "Folded\n\nCoop-Task:\n  reopened-task",
			want: "the review recorded one commit for task reopened-task, but Git reads "},
		// Git skips trailing comment lines and everything after a scissors line; the raw parse
		// takes the message's last paragraph, which binds nothing.
		{name: "a later task's binding before a comment line", withHistory: true,
			message: "Again\n\nCoop-Task: " + reanchorReviewedLater + "\n\n# note",
			want:    "the review recorded one commit for task " + reanchorReviewedLater + ", but Git reads "},
		{name: "a later task's binding before a scissors line", withHistory: true,
			message: "Again\n\nCoop-Task: " + reanchorReviewedLater + "\n\n# ------------------------ >8 ------------------------\nanything",
			want:    "the review recorded one commit for task " + reanchorReviewedLater + ", but Git reads "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReanchorFixture(t, tc.withHistory)
			named := f.commitVerbatim(t, tc.message)
			f.commit(t, "later.txt", "later\n", "Later work\n\nCoop-Task: later-task")
			code, err := tasksFolderUnblock(f.repo, f.root, []string{f.id, "go on"})
			if code != -1 {
				t.Errorf("unblock code = %d, want -1", code)
			}
			wants := []string{"failed during audit authority validation", tc.want}
			if strings.HasPrefix(tc.want, ", made") || strings.HasPrefix(tc.want, "also") {
				wants = append(wants, "commit "+shortCommit(named))
			} else {
				wants = append(wants, named[:7])
			}
			f.assertRefused(t, "go on", err, wants...)
		})
	}
}

func TestUnblockRefusesARewrittenReviewedCommit(t *testing.T) {
	f := newReanchorFixture(t, false)
	f.git("reset", "--hard", "-q", f.subject+"^")
	f.commit(t, "a.txt", "A rewritten\n", "A implementation\n\nCoop-Task: "+f.id)
	for _, sha := range f.landed {
		f.git("cherry-pick", sha)
	}
	code, err := tasksFolderUnblock(f.repo, f.root, []string{f.id, "go on"})
	if code != -1 {
		t.Errorf("unblock code = %d, want -1", code)
	}
	f.assertRefused(t, "go on", err, "HEAD no longer contains commit "+shortCommit(f.subject)+", where the review left task "+f.id)
}

// The commits a moved record accepted belong to other work, so its authority never covers a rewrite
// of the task's commit: the rewrite check compares only the history the record still replays, and
// it would accept dropping every one of those commits.
func TestMovedAuditAuthorityAllowsNoRewrite(t *testing.T) {
	f := newReanchorFixture(t, false)
	anchor := f.head()
	moved := f.unblock(t, "go on")

	f.git("reset", "--hard", "-q", f.subject+"^")
	dropped := f.commit(t, "a.txt", "A rewritten\n", "A implementation\n\nCoop-Task: "+f.id)
	for _, sha := range f.landed {
		f.git("cherry-pick", sha)
	}
	kept := f.head()
	for name, head := range map[string]string{"drops the landed commits": dropped, "replays them": kept} {
		if auditReopenCompletionValid(f.repo, anchor, head, f.id, moved) {
			t.Errorf("a rewrite that %s was accepted", name)
		}
		if missing, _ := CompletionUnbindableTasks(f.repo, anchor, head, []string{f.id}, &moved, nil); !slices.Equal(missing, []string{f.id}) {
			t.Errorf("completion over a rewrite that %s = %v, want the task refused", name, missing)
		}
		if _, err := rebasedAuditReopenRecord(f.repo, anchor, head, f.id, moved); err == nil {
			t.Errorf("a parked rewrite that %s moved the record", name)
		}
	}
}

func TestMovedAuditAuthorityStillFailsClosed(t *testing.T) {
	t.Run("a later binding of the task", func(t *testing.T) {
		f := newReanchorFixture(t, false)
		moved := f.unblock(t, "go on")
		f.commit(t, "again.txt", "again\n", "Again\n\nCoop-Task: "+f.id)
		if err := ValidateLeasedAuditReopen(f.repo, f.head(), f.id, &moved); err == nil {
			t.Fatal("a second binding after the move was accepted at lease start")
		}
	})
	// The move checked Git's reading once; every later check reads it again, so a commit binding a
	// later task for Git alone, landing after the move, stops the next lease.
	for name, message := range map[string]string{
		"a later task bound for Git alone behind a comment line": "Again\n\nCoop-Task: " + reanchorReviewedLater + "\n\n# note",
		"a later task bound for Git alone behind a scissors line": "Again\n\nCoop-Task: " + reanchorReviewedLater +
			"\n\n# ------------------------ >8 ------------------------\nanything",
	} {
		t.Run(name, func(t *testing.T) {
			f := newReanchorFixture(t, true)
			moved := f.unblock(t, "go on")
			f.commitVerbatim(t, message)
			err := ValidateLeasedAuditReopen(f.repo, f.head(), f.id, &moved)
			if err == nil || !strings.Contains(err.Error(), "the review recorded one commit for task "+reanchorReviewedLater+", but Git reads ") {
				t.Fatalf("lease after a binding only Git reads = %v", err)
			}
		})
	}
	t.Run("HEAD dropped the commit rework starts from", func(t *testing.T) {
		f := newReanchorFixture(t, false)
		moved := f.unblock(t, "go on")
		f.git("reset", "--hard", "-q", f.landed[2])
		f.commit(t, "elsewhere.txt", "elsewhere\n", "Elsewhere")
		err := ValidateLeasedAuditReopen(f.repo, f.head(), f.id, &moved)
		if err == nil || !strings.Contains(err.Error(), "no longer contains commit "+shortCommit(moved.BaselineHead)) {
			t.Fatalf("lease over a history without the moved baseline = %v", err)
		}
	})
	t.Run("a record moved over a commit naming the task", func(t *testing.T) {
		f := newReanchorFixture(t, false)
		named := f.commit(t, "named.txt", "named\n", "Two tasks\n\nCoop-Task: other-task\nCoop-Task: "+f.id)
		forged := f.record
		forged.ReviewedHead, forged.BaselineHead = f.subject, f.commit(t, "after.txt", "after\n", "After")
		_, err := auditReopenCurrentHistory(f.repo, f.head(), f.id, forged)
		if err == nil || !strings.Contains(err.Error(), "commit "+shortCommit(named)+", made after the review, also carries") {
			t.Fatalf("record moved over a commit naming the task = %v", err)
		}
	})
	t.Run("record shape", func(t *testing.T) {
		f := newReanchorFixture(t, false)
		for name, reviewed := range map[string]string{"same as the baseline": f.record.BaselineHead, "not a commit id": "subject"} {
			bad := f.record
			bad.ReviewedHead = reviewed
			if validateAuditReopenRecord(bad, f.id) == nil {
				t.Errorf("a reviewed head %s was accepted", name)
			}
		}
	})
}

// A moved record's baseline is a commit of other work, so the loop signing it, or an amend, can
// rewrite it. The lease then parks the task, and the next unblock moves the record again from the
// reviewed history instead of refusing for good.
func TestUnblockMovesAMovedRecordAgainAfterItsBaselineWasRewritten(t *testing.T) {
	f := newReanchorFixture(t, false)
	moved := f.unblock(t, "go on")
	signed := f.resign(t)
	if err := ValidateLeasedAuditReopen(f.repo, signed, f.id, &moved); err == nil {
		t.Fatal("fixture: the rewritten baseline still validates, so the lease would not park the task")
	}
	todo, ok := mustCurrentTask(t, f.root, f.id)
	if !ok {
		t.Fatal("unblocked task disappeared")
	}
	if err := MoveTaskDir(f.root, todo, StateInProgress); err != nil {
		t.Fatal(err)
	}
	inProgress, _ := mustCurrentTask(t, f.root, f.id)
	if err := ParkStaleAuditReopen(QueuedTask{Root: f.root, Item: inProgress}, moved.BaselineHead); err != nil {
		t.Fatal(err)
	}
	got := f.unblock(t, "go on again")
	if got.ReviewedHead != f.subject || got.BaselineHead != signed || got.Generation != f.record.Generation {
		t.Fatalf("record moved again = %#v, want it at %s from %s", got, signed, f.subject)
	}
}

// The host's own signing pass carries a moved record to the commits it rewrote, so the lease does
// not park the task at all. The baseline follows only a rewrite that kept every accepted commit as
// it was, and only to the rewritten baseline: work landing after it still gets the strict check.
func TestSigningCarriesAMovedRecord(t *testing.T) {
	t.Run("signed", func(t *testing.T) {
		f := newReanchorFixture(t, false)
		moved := f.unblock(t, "go on")
		signed := f.resign(t)
		if err := carryAuditReopen(f.repo, f.root, f.id, moved.BaselineHead, nil); err != nil {
			t.Fatal(err)
		}
		got, ok, err := ReadAuditReopenRecord(f.root, f.id)
		if err != nil || !ok || got.BaselineHead != signed || got.ReviewedHead != f.subject {
			t.Fatalf("carried record = %#v ok=%v err=%v, want it at %s", got, ok, err, signed)
		}
		if err := ValidateLeasedAuditReopen(f.repo, signed, f.id, &got); err != nil {
			t.Fatalf("lease after the signing pass: %v", err)
		}
	})
	t.Run("a commit landed after signing", func(t *testing.T) {
		f := newReanchorFixture(t, false)
		moved := f.unblock(t, "go on")
		signed := f.resign(t)
		late := f.commit(t, "late.txt", "late\n", "Late work\n\nCoop-Task: other-task")
		if err := carryAuditReopen(f.repo, f.root, f.id, moved.BaselineHead, nil); err != nil {
			t.Fatal(err)
		}
		got, ok, err := ReadAuditReopenRecord(f.root, f.id)
		if err != nil || !ok || got.BaselineHead != signed {
			t.Fatalf("carried record = %#v ok=%v err=%v, want it at the signed %s, not past it", got, ok, err, signed)
		}
		// The late commit binds a task already bound twice, which the strict check refuses.
		if err := ValidateLeasedAuditReopen(f.repo, late, f.id, &got); err == nil {
			t.Fatal("the commit that landed after signing was accepted without the strict check")
		}
	})
	t.Run("signing rewrote the baseline and a commit after it", func(t *testing.T) {
		f := newReanchorFixture(t, false)
		moved := f.unblock(t, "go on")
		later := f.commit(t, "fresh.txt", "fresh\n", "Fresh work\n\nCoop-Task: fresh-task")
		f.git("reset", "--hard", "-q", moved.BaselineHead+"^")
		f.git("-c", "user.name=Signer", "-c", "user.email=signer@example.com", "cherry-pick", moved.BaselineHead)
		signedBaseline := f.head()
		f.git("-c", "user.name=Signer", "-c", "user.email=signer@example.com", "cherry-pick", later)
		if err := carryAuditReopen(f.repo, f.root, f.id, later, nil); err != nil {
			t.Fatal(err)
		}
		got, ok, err := ReadAuditReopenRecord(f.root, f.id)
		if err != nil || !ok || got.BaselineHead != signedBaseline {
			t.Fatalf("carried record = %#v ok=%v err=%v, want it at the signed baseline %s", got, ok, err, signedBaseline)
		}
	})
	t.Run("an accepted commit changed", func(t *testing.T) {
		f := newReanchorFixture(t, false)
		moved := f.unblock(t, "go on")
		f.commit(t, "docs.txt", "docs changed\n", "docs: a change with no task")
		f.git("reset", "--soft", "-q", "HEAD~2")
		f.git("commit", "-q", "-m", "docs: a change with no task")
		if err := carryAuditReopen(f.repo, f.root, f.id, moved.BaselineHead, nil); err != nil {
			t.Fatal(err)
		}
		if got, ok, err := ReadAuditReopenRecord(f.root, f.id); err != nil || !ok || !AuditReopenRecordsEqual(got, moved) {
			t.Fatalf("carry moved the record over a changed commit: %#v ok=%v err=%v", got, ok, err)
		}
	})
}

// A failed attempt that reset HEAD to the reviewed history, dropping what the move accepted, and
// committed a repair, is accepted at the next unblock, as the person decides, but the record stays
// moved, so the task never regains the authority to rewrite its commit.
func TestUnblockAfterADroppedMoveStaysMoved(t *testing.T) {
	f := newReanchorFixture(t, false)
	f.unblock(t, "go on")
	todo, ok := mustCurrentTask(t, f.root, f.id)
	if !ok {
		t.Fatal("unblocked task disappeared")
	}
	if err := MoveTaskDir(f.root, todo, StateBlocked); err != nil {
		t.Fatal(err)
	}
	f.git("reset", "--hard", "-q", f.subject)
	repair := f.commit(t, "a.txt", "A repaired\n", "Repair the audit finding")
	got := f.unblock(t, "go on again")
	if got.ReviewedHead != f.subject || got.BaselineHead != repair || !got.Repaired {
		t.Fatalf("record after a dropped move = %#v, want it still moved, at %s", got, repair)
	}
}

// Rework that rewrote the task's commit after a move, dropping the accepted commits, stays refused
// when the task is unblocked again: a moved record accepts no rewrite.
func TestUnblockRefusesARewriteAfterAMove(t *testing.T) {
	f := newReanchorFixture(t, false)
	f.unblock(t, "go on")
	todo, ok := mustCurrentTask(t, f.root, f.id)
	if !ok {
		t.Fatal("unblocked task disappeared")
	}
	if err := MoveTaskDir(f.root, todo, StateBlocked); err != nil {
		t.Fatal(err)
	}
	moved, _, _ := ReadAuditReopenRecord(f.root, f.id)
	f.record = moved // assertRefused compares against the record the refusal must keep
	f.task, _ = mustCurrentTask(t, f.root, f.id)
	f.git("reset", "--hard", "-q", f.subject+"^")
	f.commit(t, "a.txt", "A rewritten\n", "A implementation\n\nCoop-Task: "+f.id)
	code, err := tasksFolderUnblock(f.repo, f.root, []string{f.id, "go on"})
	if code != -1 {
		t.Errorf("unblock code = %d, want -1", code)
	}
	f.assertRefused(t, "go on", err, "HEAD no longer contains commit "+shortCommit(f.subject), "a record moved past other work accepts no rewrite")
}

// An unblock that stopped partway leaves a pending record, in blocked before the folder move or in
// todo after it. More work landing before the person runs unblock again must not strand it: the
// retry moves the record past those commits too.
func TestPendingUnblockFinishesPastUnrelatedCommits(t *testing.T) {
	for _, moved := range []bool{false, true} {
		name := "stopped before the folder move"
		if moved {
			name = "stopped after the folder move"
		}
		t.Run(name, func(t *testing.T) {
			f := newReanchorFixture(t, false)
			transition, err := prepareBlockedAuditReopenUnblock(f.repo, f.root, f.task)
			if err != nil {
				t.Fatal(err)
			}
			if err := transition.markPending(); err != nil {
				_ = transition.finish(nil)
				t.Fatal(err)
			}
			if err := recordResolution(filepath.Join(f.task.Dir, "decision.md"), "go on"); err != nil {
				_ = transition.finish(nil)
				t.Fatal(err)
			}
			if moved {
				if err := MoveTaskDir(f.root, f.task, StateTodo); err != nil {
					_ = transition.finish(nil)
					t.Fatal(err)
				}
			}
			if err := transition.finish(nil); err != nil {
				t.Fatal(err)
			}
			f.commit(t, "pair2.txt", "pair\n", "Close two more\n\nCoop-Task: third-task\nCoop-Task: fourth-task")
			if code, err := tasksFolderUnblock(f.repo, f.root, []string{f.id}); code != 0 || err != nil {
				t.Fatalf("finish the interrupted unblock = code %d, %v", code, err)
			}
			got, ok, err := ReadAuditReopenRecord(f.root, f.id)
			if err != nil || !ok || got.UnblockPending || got.Version != auditReopenVersion ||
				got.ReviewedHead != f.subject || got.BaselineHead != f.head() || got.Generation != f.record.Generation {
				t.Fatalf("finished record = %#v ok=%v err=%v", got, ok, err)
			}
			if current, ok := mustCurrentTask(t, f.root, f.id); !ok || current.State != StateTodo {
				t.Fatalf("finished task = %#v, ok=%v", current, ok)
			}
		})
	}
}
