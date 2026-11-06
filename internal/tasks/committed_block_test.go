package tasks

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

// committedBlockFixture is a repository whose task an attempt is about to work, under the loop's
// lease.
type committedBlockFixture struct {
	repo, root, id string
	git            func(...string)
}

func newCommittedBlockFixture(t *testing.T) committedBlockFixture {
	t.Helper()
	repo, git := gitRepo(t)
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	f := committedBlockFixture{repo: repo, root: filepath.Join(repo, TasksRoot), id: "gate-blocked", git: git}
	f.commit(t, "base.txt", "base\n", "base")
	return f
}

func (f committedBlockFixture) commit(t *testing.T, name, body, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.repo, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git("add", name)
	f.git("commit", "-q", "-m", message)
	return f.head()
}

func (f committedBlockFixture) head() string { return forkspace.GitOut(f.repo, "rev-parse", "HEAD") }

// lease starts an attempt on the task, the way the loop does, and returns the commit it starts from.
func (f committedBlockFixture) lease(t *testing.T) (*TaskLease, string) {
	t.Helper()
	task := taskForLease(t, f.root, StateInProgress, f.id)
	lease, _, err := TryTaskLease(f.root, task, testLeaseOwner())
	if err != nil || lease == nil {
		t.Fatalf("lease the task: %v", err)
	}
	return lease, f.head()
}

// block has the attempt block its task for a person with its task tools, and returns the commit HEAD
// named then, as taskmcp's AssignedBlockHead does.
func (f committedBlockFixture) block(t *testing.T) string {
	t.Helper()
	current, ok := mustCurrentTask(t, f.root, f.id)
	if !ok {
		t.Fatal("the leased task disappeared")
	}
	blocked := f.head()
	if err := MoveTaskDir(f.root, current, StateBlocked); err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(f.root, StateBlocked, f.id, "decision.md"), "# Decision\n\n**Resolution:** <!-- unresolved -->\n")
	return blocked
}

// endAttempt has the loop record what the attempt left once the box exits.
func (f committedBlockFixture) endAttempt(t *testing.T, lease *TaskLease, base, blocked string) {
	t.Helper()
	if err := lease.RecordCommittedBlock(f.repo, base, blocked, f.head()); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

// blockAttempt has the attempt block its task for a person and end there.
func (f committedBlockFixture) blockAttempt(t *testing.T, lease *TaskLease, base string) {
	t.Helper()
	f.endAttempt(t, lease, base, f.block(t))
}

func (f committedBlockFixture) noted(t *testing.T) bool {
	t.Helper()
	_, ok, err := readCommittedBlock(f.root, f.id)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// unblock runs coop tasks unblock on the task, with the answer when one is given, and returns what
// it printed.
func (f committedBlockFixture) unblock(t *testing.T, answer ...string) string {
	t.Helper()
	var code int
	var err error
	out := captureStderr(t, func() { code, err = tasksFolderUnblock(f.repo, f.root, append([]string{f.id}, answer...)) })
	if code != 0 || err != nil {
		t.Fatalf("unblock = code %d, %v", code, err)
	}
	return out
}

// granted reports whether an answered unblock gave the task re-close authority, failing the test on
// a registry it cannot read.
func (f committedBlockFixture) granted(t *testing.T) bool {
	t.Helper()
	record, ok, err := ReadAuditReopenRecord(f.root, f.id)
	if err != nil {
		t.Fatal(err)
	}
	return ok && record.Answered
}

// The 2026-11-05 emisar loop: a worker committed its task, then blocked it because a required gate
// could not run in the box. Once the person answered, the next worker found the commit under later
// work and blocked again, since crash recovery's amend is safe only at HEAD.
func TestAnsweredCommittedBlockClosesWithoutANewCommit(t *testing.T) {
	f := newCommittedBlockFixture(t)
	lease, base := f.lease(t)
	f.commit(t, "a.txt", "A\n", "A implementation\n\nCoop-Task: "+f.id)
	f.blockAttempt(t, lease, base)
	if !f.noted(t) {
		t.Fatal("the loop did not note the attempt that committed its task and blocked it")
	}
	// The loop signs the commit right after the attempt, then other work lands on top, one task of it
	// bound twice.
	f.git("-c", "user.name=Signer", "-c", "user.email=signer@example.com", "commit", "-q", "--amend", "--no-edit")
	signed := f.head()
	f.commit(t, "other.txt", "one\n", "Other work\n\nCoop-Task: other-task")
	f.commit(t, "other.txt", "two\n", "More of the other work\n\nCoop-Task: other-task")
	f.commit(t, "docs.txt", "docs\n", "docs: a change with no task")
	head := f.head()

	// A box can write the Resolution, so the loop's pre-flight leaves the task to the person.
	writeTaskFile(t, filepath.Join(f.root, StateBlocked, f.id, "decision.md"), "# Decision\n\n**Resolution:** rerun the gate and close\n")
	if ids, err := UnblockResolved([]string{f.root}); err != nil || len(ids) != 0 {
		t.Fatalf("pre-flight unblocked %v, %v", ids, err)
	}
	if current, ok := mustCurrentTask(t, f.root, f.id); !ok || current.State != StateBlocked {
		t.Fatalf("pre-flight moved the task: %#v", current)
	}
	if _, ok, err := ReadAuditReopenRecord(f.root, f.id); err != nil || ok {
		t.Fatalf("pre-flight granted authority: ok=%v err=%v", ok, err)
	}

	// The person sees which commit their answer lets the next worker close the task over.
	if out := f.unblock(t, "the gate runs now: rerun it and close"); !strings.Contains(out, closesWithoutACommitNote+"\n  "+shortCommit(signed)+" A implementation\n") {
		t.Errorf("the unblock does not name the commit %s:\n%s", shortCommit(signed), out)
	}
	record, ok, err := ReadAuditReopenRecord(f.root, f.id)
	if err != nil || !ok || !record.Answered || !auditReopenRecordActive(record) ||
		record.ReviewedHead != signed || record.BaselineHead != head {
		t.Fatalf("unblock left authority %#v, ok=%v err=%v; want it moved from %s to %s", record, ok, err, signed, head)
	}
	if f.noted(t) {
		t.Fatal("the unblock left the note it answered")
	}
	todo, _ := mustCurrentTask(t, f.root, f.id)
	if log := readFileString(filepath.Join(todo.Dir, "log.md")); !strings.Contains(log, "close this task without a new commit") {
		t.Errorf("the task log does not say the next worker may close it without a commit:\n%s", log)
	}

	// The next worker re-closes it without a commit, and is told how.
	if err := MoveTaskDir(f.root, todo, StateInProgress); err != nil {
		t.Fatal(err)
	}
	inProgress, _ := mustCurrentTask(t, f.root, f.id)
	next, _, err := TryTaskLease(f.root, inProgress, testLeaseOwner())
	if err != nil || next == nil || next.Reopen == nil || !next.Reopen.Answered {
		t.Fatalf("next lease = %#v, %v", next, err)
	}
	defer next.Release()
	if err := ValidateLeasedAuditReopen(f.repo, head, f.id, next.Reopen); err != nil {
		t.Fatalf("the next attempt cannot start: %v", err)
	}
	if missing, _ := CompletionUnbindableTasks(f.repo, head, head, []string{f.id}, next.Reopen, nil); len(missing) != 0 {
		t.Fatalf("a re-close with no new commit was refused: %v", missing)
	}
	resume := ResumePrefixFor(f.repo, f.id, StateInProgress, next.Reopen)
	for _, want := range []string{"answered in its decision.md", "NOT crash recovery", "ZERO new commits", "WITHOUT a Coop-Task or Coop-Recovery trailer"} {
		if !strings.Contains(resume, want) {
			t.Errorf("resume line lacks %q:\n%s", want, resume)
		}
	}
	for _, banned := range []string{"Coop-Recovery: <current UTC timestamp>", "STOP"} {
		if strings.Contains(resume, banned) {
			t.Errorf("resume line keeps crash-recovery guidance %q:\n%s", banned, resume)
		}
	}
}

// Only an answer given to coop tasks unblock spends the note. A box can write decision.md, so a plain
// unblock, over a Resolution the box wrote or with no decision.md at all, leaves the note unused and
// the task under the rule for a bound task, until the person answers on a later unblock.
func TestCommittedBlockNeedsTheAnswerOnUnblock(t *testing.T) {
	for name, decision := range map[string]string{
		"a Resolution the box wrote": "# Decision\n\n**Resolution:** checks only, close\n",
		"no decision.md":             "",
	} {
		t.Run(name, func(t *testing.T) {
			f := newCommittedBlockFixture(t)
			lease, base := f.lease(t)
			f.commit(t, "a.txt", "A\n", "A implementation\n\nCoop-Task: "+f.id)
			f.blockAttempt(t, lease, base)
			f.commit(t, "other.txt", "other\n", "Other work")
			decisionFile := filepath.Join(f.root, StateBlocked, f.id, "decision.md")
			if decision == "" {
				if err := os.Remove(decisionFile); err != nil {
					t.Fatal(err)
				}
			} else {
				writeTaskFile(t, decisionFile, decision)
			}
			if out := f.unblock(t); f.granted(t) || strings.Contains(out, closesWithoutACommitNote) {
				t.Fatalf("a plain unblock granted re-close authority:\n%s", out)
			}
			if !f.noted(t) {
				t.Fatal("a plain unblock spent the note")
			}
			if resume := ResumePrefixFor(f.repo, f.id, StateInProgress, nil); !strings.Contains(resume, "STOP") {
				t.Fatalf("the next worker lost the rule for a bound task:\n%s", resume)
			}
			// That worker blocks again with nothing new, and the person answers this time.
			todo, _ := mustCurrentTask(t, f.root, f.id)
			if err := MoveTaskDir(f.root, todo, StateBlocked); err != nil {
				t.Fatal(err)
			}
			if f.unblock(t, "rerun the gate and close"); !f.granted(t) {
				t.Fatal("the answer did not spend the note a plain unblock left")
			}
		})
	}
}

// An answer typed into coop tasks decisions -i is the person's too, and the page names the commit it
// lets the next worker close the task over.
func TestDecisionBrowserAnswerNamesTheCommittedBlock(t *testing.T) {
	f := newCommittedBlockFixture(t)
	lease, base := f.lease(t)
	bound := f.commit(t, "a.txt", "A\n", "A implementation\n\nCoop-Task: "+f.id)
	f.blockAttempt(t, lease, base)
	var out bytes.Buffer
	if code, err := runDecisionBrowser(f.repo, []decisionRef{{root: f.root, id: f.id}}, strings.NewReader("rerun the gate and close\n"), &out); code != 0 || err != nil {
		t.Fatalf("browser = (%d, %v)", code, err)
	}
	if !f.granted(t) || !strings.Contains(out.String(), closesWithoutACommitNote+"\n    "+shortCommit(bound)+" A implementation\n") {
		t.Fatalf("the answer did not grant re-close authority or name the commit %s:\n%s", shortCommit(bound), out.String())
	}
}

// Only a commit the blocking attempt made, still unchanged, earns the authority: a crash recovery
// that blocks, a commit replaced after the block, or one another session made after the worker
// blocked keeps the rule that a bound task needs new work.
func TestCommittedBlockGrantsNothingForAnotherCommit(t *testing.T) {
	t.Run("committed before the attempt", func(t *testing.T) {
		f := newCommittedBlockFixture(t)
		f.commit(t, "a.txt", "A\n", "A implementation\n\nCoop-Task: "+f.id)
		lease, base := f.lease(t)
		f.commit(t, "a.txt", "A, part two\n", "Finish A")
		f.blockAttempt(t, lease, base)
		if f.noted(t) {
			t.Fatal("the loop noted a commit an earlier attempt made")
		}
		f.unblock(t, "finish it")
		if _, ok, err := ReadAuditReopenRecord(f.root, f.id); err != nil || ok {
			t.Fatalf("the unblock granted authority: ok=%v err=%v", ok, err)
		}
		if resume := ResumePrefixFor(f.repo, f.id, StateInProgress, nil); !strings.Contains(resume, "STOP") {
			t.Fatalf("crash recovery lost its rule:\n%s", resume)
		}
	})
	t.Run("changed after it blocked", func(t *testing.T) {
		f := newCommittedBlockFixture(t)
		lease, base := f.lease(t)
		f.commit(t, "a.txt", "A\n", "A implementation\n\nCoop-Task: "+f.id)
		f.blockAttempt(t, lease, base)
		if !f.noted(t) {
			t.Fatal("the loop did not note the attempt that committed its task and blocked it")
		}
		if err := os.WriteFile(filepath.Join(f.repo, "a.txt"), []byte("something else\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.git("commit", "-q", "-a", "--amend", "--no-edit")
		f.unblock(t, "close it")
		if _, ok, err := ReadAuditReopenRecord(f.root, f.id); err != nil || ok {
			t.Fatalf("the unblock granted authority for a changed commit: ok=%v err=%v", ok, err)
		}
		if f.noted(t) {
			t.Fatal("the unblock left the note it answered")
		}
	})
	// Another box sharing the checkout commits with the task's trailer while its worker blocks it
	// without committing.
	t.Run("made by another session after the block", func(t *testing.T) {
		f := newCommittedBlockFixture(t)
		lease, base := f.lease(t)
		blocked := f.block(t)
		f.commit(t, "a.txt", "planted\n", "Planted by another box\n\nCoop-Task: "+f.id)
		f.endAttempt(t, lease, base, blocked)
		if f.noted(t) {
			t.Fatal("the loop noted a commit made after the worker blocked its task")
		}
		if out := f.unblock(t, "close it"); f.granted(t) || strings.Contains(out, closesWithoutACommitNote) {
			t.Fatalf("the unblock granted re-close authority over the planted commit:\n%s", out)
		}
	})
	t.Run("blocked without its task tools", func(t *testing.T) {
		f := newCommittedBlockFixture(t)
		lease, base := f.lease(t)
		f.commit(t, "a.txt", "A\n", "A implementation\n\nCoop-Task: "+f.id)
		f.block(t)
		f.endAttempt(t, lease, base, "")
		if f.noted(t) {
			t.Fatal("the loop noted a commit with no record of when the task blocked")
		}
	})
}
