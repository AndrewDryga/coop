package tasks

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/ui"
)

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Make COOP_EGRESS fail closed!": "make-coop-egress-fail-closed",
		"  Trim --- dashes  ":           "trim-dashes",
		"123 Go":                        "123-go",
		// Unicode letters/digits survive instead of being dropped to "" — a non-Latin title
		// gets a real slug, and a mixed one keeps both scripts.
		"Привет мир":  "привет-мир",
		"Café déjà":   "café-déjà",
		"Fix Привет!": "fix-привет",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
	// A long title is hard-capped to a clean ASCII slug — no "…" ellipsis in a path,
	// no dangling dash, ≤ 48 runes.
	long := slugify("Folder-mode queue split: distribute task folders across forks and worktrees")
	if n := len([]rune(long)); n > 48 {
		t.Errorf("long slug %q is %d runes, want ≤ 48", long, n)
	}
	if strings.ContainsRune(long, '…') {
		t.Errorf("long slug must not contain an ellipsis: %q", long)
	}
	if strings.HasPrefix(long, "-") || strings.HasSuffix(long, "-") {
		t.Errorf("long slug has a dangling dash: %q", long)
	}
	if !strings.HasPrefix(long, "folder-mode-queue-split") {
		t.Errorf("long slug lost its prefix: %q", long)
	}
}

func TestFindTask(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-01-alpha", "task.md"), "# a\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-01-alpine", "task.md"), "# b\n")
	if _, err := FindTask(root, "2026-01-01-alpha"); err != nil {
		t.Errorf("exact match: %v", err)
	}
	if _, err := FindTask(root, "alpine"); err != nil {
		t.Errorf("unique substring 'alpine': %v", err)
	}
	if _, err := FindTask(root, "alp"); err == nil {
		t.Errorf("ambiguous 'alp' should error")
	}
	if _, err := FindTask(root, "zzz"); err == nil {
		t.Errorf("missing 'zzz' should error")
	}
	// An empty fragment must error, not substring-match every task.
	if _, err := FindTask(root, ""); err == nil {
		t.Errorf("empty id should error, not match everything")
	}
}

// `coop tasks path <id>` prints the resolved folder (reusing findTask) so a hook or human can
// `cat "$(coop tasks path <id>)/task.md"`; absent/ambiguous ids error like the other id commands.
func TestTasksFolderPath(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, StateTodo, "2026-01-01-alpha")
	writeTaskFile(t, filepath.Join(dir, "task.md"), "# a\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-01-alpine", "task.md"), "# b\n")

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code, err := tasksFolderPath(root, []string{"alpha"}) // 'alpha' is a unique substring (alpine lacks it)
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if code != 0 || err != nil {
		t.Fatalf("tasks path alpha = (%d, %v), want (0, nil)", code, err)
	}
	if got := strings.TrimSpace(string(out)); got != dir {
		t.Errorf("printed %q, want the task's dir %q", got, dir)
	}
	if code, err := tasksFolderPath(root, []string{"alp"}); code == 0 || err == nil { // ambiguous
		t.Errorf("ambiguous 'alp' = (%d, %v), want an error", code, err)
	}
	if code, err := tasksFolderPath(root, []string{"zzz"}); code == 0 || err == nil { // absent
		t.Errorf("absent 'zzz' = (%d, %v), want an error", code, err)
	}
	if code, _ := tasksFolderPath(root, nil); code != 2 { // no id → usage
		t.Errorf("no id = %d, want 2 (usage)", code)
	}
}

func TestTasksFolderLifecycle(t *testing.T) {
	root := t.TempDir()

	if code, err := tasksFolderAdd(root, []string{"Make egress fail closed"}, StateTodo, "tasks add"); code != 0 || err != nil {
		t.Fatalf("add: code=%d err=%v", code, err)
	}
	items := ReadTaskTree(root)
	if len(items) != 1 || items[0].State != StateTodo {
		t.Fatalf("after add: %+v", items)
	}
	id := items[0].ID
	if !strings.HasSuffix(id, "-make-egress-fail-closed") {
		t.Errorf("id slug = %q", id)
	}

	// claim via a substring of the id
	if code, err := tasksFolderMove(root, []string{"egress"}, StateInProgress, "claim", "claimed"); code != 0 || err != nil {
		t.Fatalf("claim: code=%d err=%v", code, err)
	}
	if got := ReadTaskTree(root)[0].State; got != StateInProgress {
		t.Fatalf("after claim, state = %s", got)
	}
	if rec, owned := taskOwned(t, root, id); !owned {
		t.Fatal("claim must write a durable owner record")
	} else if rec.Source != taskOwnerSourceInteractiveClaim || rec.TaskID != id || rec.User == "" || rec.Host == "" || rec.ClaimedAt.IsZero() {
		t.Errorf("claim owner record = %+v, want a fully populated interactive-claim record", rec)
	}
	tmpFile := filepath.Join(root, StateInProgress, id, "tmp", "scratch.patch")
	artifact := filepath.Join(root, StateInProgress, id, "artifacts", "evidence.txt")
	writeTaskFile(t, tmpFile, "resume me\n")
	writeTaskFile(t, artifact, "keep me\n")
	// An interrupted iteration resumes by claiming the already-in-progress task; non-done moves
	// must retain both its disposable scratch and durable evidence.
	if code, err := tasksFolderMove(root, []string{id}, StateInProgress, "claim", "claimed"); code != 0 || err != nil {
		t.Fatalf("resume claim: code=%d err=%v", code, err)
	}
	if !fileExists(tmpFile) {
		t.Fatal("an interrupted in-progress task lost its tmp")
	}
	if _, owned := taskOwned(t, root, id); !owned {
		t.Error("re-claiming an already in-progress task must still hold the owner record")
	}

	// block → moves to blocked/ and writes decision.md
	if code, err := tasksFolderBlock(root, []string{id}); code != 0 || err != nil {
		t.Fatalf("block: code=%d err=%v", code, err)
	}
	bt := ReadTaskTree(root)[0]
	if bt.State != StateBlocked || !bt.HasDecision {
		t.Fatalf("after block: %+v", bt)
	}
	if !fileExists(filepath.Join(root, StateBlocked, id, "decision.md")) {
		t.Error("decision.md not created on block")
	}
	if !fileExists(filepath.Join(root, StateBlocked, id, "tmp", "scratch.patch")) {
		t.Error("blocking a task must retain its tmp")
	}
	if _, owned := taskOwned(t, root, id); owned {
		t.Error("block must clear the owner record")
	}

	// unblock WITH an answer → todo (available again; the in_progress lock is taken by claim), the
	// resolved decision.md rides along. (A no-answer unblock of an unresolved decision is refused —
	// covered by TestUnblockRequiresResolution.)
	if code, err := tasksFolderUnblock(root, []string{id, "A — go with it"}); code != 0 || err != nil {
		t.Fatalf("unblock: code=%d err=%v", code, err)
	}
	if ReadTaskTree(root)[0].State != StateTodo {
		t.Fatal("after unblock, not back in todo")
	}
	if !fileExists(filepath.Join(root, StateTodo, id, "tmp", "scratch.patch")) {
		t.Error("unblocking a task must retain its tmp")
	}
	if _, owned := taskOwned(t, root, id); owned {
		t.Error("unblock must leave the owner record cleared (idempotent — block already cleared it)")
	}
	// unblocking a non-blocked task is an error (it's in todo now), not a silent reopen.
	if code, err := tasksFolderUnblock(root, []string{id}); code == 0 || err == nil {
		t.Errorf("unblock of a non-blocked task should error, got (%d, %v)", code, err)
	}

	if code, err := tasksFolderMove(root, []string{id}, StateInProgress, "claim", "claimed"); code != 0 || err != nil {
		t.Fatalf("reclaim: code=%d err=%v", code, err)
	}
	if !fileExists(filepath.Join(root, StateInProgress, id, "tmp", "scratch.patch")) {
		t.Error("reclaiming a task must retain its tmp")
	}
	if _, owned := taskOwned(t, root, id); !owned {
		t.Error("reclaiming from todo must write a fresh owner record")
	}

	// done → done/ and removes only tmp; durable artifacts survive for review/archive.
	if code, err := tasksFolderMove(root, []string{id}, StateDone, "done", "done"); code != 0 || err != nil {
		t.Fatalf("done: code=%d err=%v", code, err)
	}
	if ReadTaskTree(root)[0].State != StateDone {
		t.Fatal("after done, not done")
	}
	if pathExists(filepath.Join(root, StateDone, id, "tmp")) {
		t.Error("done must remove the completed task's tmp")
	}
	if !fileExists(filepath.Join(root, StateDone, id, "artifacts", "evidence.txt")) {
		t.Error("done must retain durable artifacts")
	}
	if _, owned := taskOwned(t, root, id); owned {
		t.Error("done must clear the owner record")
	}
	completedState := readFileString(filepath.Join(root, StateDone, id, "state.md"))
	if !strings.Contains(completedState, "**Status:** complete") || !strings.Contains(completedState, "**Next action:** none") {
		t.Errorf("done must finalize lifecycle-owned state fields:\n%s", completedState)
	}
	doneItem := Item{ID: id, Dir: filepath.Join(root, StateDone, id), State: StateDone}
	if !taskCompletionRecorded(root, doneItem) {
		t.Error("done must record host-only completion evidence")
	}

	// Review reopens are ordinary non-done moves: scratch created for the next attempt survives
	// the move, then the next successful completion removes it.
	reopenedTmp := filepath.Join(root, StateDone, id, "tmp", "review-notes.txt")
	writeTaskFile(t, reopenedTmp, "resume review fix\n")
	if code, err := tasksFolderMove(root, []string{id}, StateInProgress, "claim", "claimed"); code != 0 || err != nil {
		t.Fatalf("review reopen: code=%d err=%v", code, err)
	}
	if !fileExists(filepath.Join(root, StateInProgress, id, "tmp", "review-notes.txt")) {
		t.Error("review reopen must retain tmp")
	}
	if _, owned := taskOwned(t, root, id); !owned {
		t.Error("claiming a review reopen must write a fresh owner record")
	}
	authority, err := OpenLeaseAuthority(root, id, false)
	if err != nil {
		t.Fatal(err)
	}
	info, err := authority.Stat()
	_ = authority.Close()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Error("review reopen retained stale host-only completion evidence")
	}
	reopenedItem, ok := CurrentTask(root, id)
	if !ok {
		t.Fatal("reopened task disappeared")
	}
	if err := MoveTaskDir(root, reopenedItem, StateDone); err != nil {
		t.Fatal(err)
	}
	if taskCompletionRecorded(root, doneItem) {
		t.Error("same-inode completion after reopen matched the cleared stale receipt")
	}
	if code, err := tasksFolderMove(root, []string{id}, StateDone, "done", "done"); code != 0 || err != nil {
		t.Fatalf("done after review reopen: code=%d err=%v", code, err)
	}
	if pathExists(filepath.Join(root, StateDone, id, "tmp")) {
		t.Error("done after review reopen must remove tmp")
	}
	if _, owned := taskOwned(t, root, id); owned {
		t.Error("done after review reopen must clear the owner record")
	}
	if !taskCompletionRecorded(root, doneItem) {
		t.Error("done after review reopen must refresh host-only completion evidence")
	}

	// no-op move when already in the target state
	if code, _ := tasksFolderMove(root, []string{id}, StateDone, "done", "done"); code != 0 {
		t.Errorf("re-done should be a no-op (code 0), got %d", code)
	}

	// remove deletes the folder (a manual, by-id removal); --yes skips the gate in this non-TTY test
	if code, err := tasksFolderRemove(root, []string{id, "--yes"}); code != 0 || err != nil {
		t.Fatalf("remove: code=%d err=%v", code, err)
	}
	if len(ReadTaskTree(root)) != 0 {
		t.Fatal("after remove, tree not empty")
	}
}

// TestTasksFolderRelease covers `coop tasks release <id>` — the explicit hand-back for a human
// claim — end to end: usage, the state guard, the "nothing to release" soft no-op on an unclaimed
// in-progress task (e.g. one the loop itself adopted), and the substantive clear-without-moving case.
func TestTasksFolderRelease(t *testing.T) {
	t.Run("usage error with no id", func(t *testing.T) {
		root := t.TempDir()
		if code, err := tasksFolderRelease(root, nil); code != 2 || err == nil {
			t.Errorf("release with no id = code %d err %v, want a usage error", code, err)
		}
	})

	t.Run("refuses a task that is not in progress", func(t *testing.T) {
		root := t.TempDir()
		writeTaskFile(t, filepath.Join(root, StateTodo, "still-todo", "task.md"), "# Todo\n")
		code, err := tasksFolderRelease(root, []string{"still-todo"})
		if code != 1 || err == nil || !strings.Contains(err.Error(), "not in progress") {
			t.Fatalf("release of a todo task = code %d err %v, want a state error", code, err)
		}
	})

	t.Run("nothing to release is a soft note, not an error", func(t *testing.T) {
		root := t.TempDir()
		// Simulate a loop adoption directly (moveTaskDir, never claim): in progress, no record —
		// exactly what the loop's own todo->in_progress move produces.
		writeTaskFile(t, filepath.Join(root, StateInProgress, "loop-owned", "task.md"), "# Loop\n")
		code, err := tasksFolderRelease(root, []string{"loop-owned"})
		if code != 0 || err != nil {
			t.Fatalf("release of an unclaimed in-progress task = code %d err %v, want a clean no-op", code, err)
		}
		if !pathExists(filepath.Join(root, StateInProgress, "loop-owned")) {
			t.Fatal("release must never move the folder, claimed or not")
		}
	})

	t.Run("clears an existing claim and leaves the folder in place", func(t *testing.T) {
		root := t.TempDir()
		writeTaskFile(t, filepath.Join(root, StateTodo, "claimed", "task.md"), "# Claimed\n")
		if code, err := tasksFolderMove(root, []string{"claimed"}, StateInProgress, "claim", "claimed"); code != 0 || err != nil {
			t.Fatalf("claim: code=%d err=%v", code, err)
		}
		code, err := tasksFolderRelease(root, []string{"claimed"})
		if code != 0 || err != nil {
			t.Fatalf("release: code=%d err=%v", code, err)
		}
		if _, owned := taskOwned(t, root, "claimed"); owned {
			t.Error("release must clear the owner record")
		}
		if !pathExists(filepath.Join(root, StateInProgress, "claimed")) {
			t.Fatal("release must leave the task in 10_in_progress/")
		}
	})
}

// TestCmdTasksFolderReleaseDispatch proves "release" is wired end to end through the dispatcher —
// not just callable as a bare function: it validates args like every other structured subcommand and
// is a recognized verb for completion and the unknown-subcommand suggester.
func TestCmdTasksFolderReleaseDispatch(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "dispatch-me", "task.md"), "# Dispatch\n")
	if code, err := CmdTasksFolder("", root, []string{"claim", "dispatch-me"}); code != 0 || err != nil {
		t.Fatalf("claim via cmdTasksFolder: code=%d err=%v", code, err)
	}
	if code, err := CmdTasksFolder("", root, []string{"release", "dispatch-me"}); code != 0 || err != nil {
		t.Fatalf("release via cmdTasksFolder: code=%d err=%v", code, err)
	}
	if _, owned := taskOwned(t, root, "dispatch-me"); owned {
		t.Error("release via cmdTasksFolder must clear the owner record")
	}
	// The verb participates in flag/positional validation like every other structured subcommand.
	if code, err := CmdTasksFolder("", root, []string{"release", "dispatch-me", "extra"}); code != 2 || err == nil {
		t.Errorf("release with a stray extra arg = code %d err %v, want a usage error", code, err)
	}
	if !slices.Contains(TasksVerbs, "release") {
		t.Error("release must be a recognized tasks verb (drives completion + unknownErr suggestions)")
	}
}

func TestTaskTmpCleanupIsContained(t *testing.T) {
	base := t.TempDir()
	taskDir := filepath.Join(base, "task")
	outside := filepath.Join(base, "outside")
	writeTaskFile(t, filepath.Join(taskDir, "task.md"), "# task\n")
	writeTaskFile(t, filepath.Join(outside, "keep.txt"), "keep\n")

	if _, err := taskLocalPath(taskDir, "../outside"); err == nil {
		t.Error("taskLocalPath accepted parent traversal")
	}
	if _, err := taskLocalPath(taskDir, outside); err == nil {
		t.Error("taskLocalPath accepted an absolute path")
	}

	// A tmp symlink is unlinked, never followed into its target.
	if err := os.Symlink(outside, filepath.Join(taskDir, "tmp")); err != nil {
		t.Fatal(err)
	}
	if err := removeTaskTmp(taskDir); err != nil {
		t.Fatalf("remove symlink tmp: %v", err)
	}
	if !fileExists(filepath.Join(outside, "keep.txt")) || pathExists(filepath.Join(taskDir, "tmp")) {
		t.Error("tmp symlink cleanup escaped the task or left the link behind")
	}

	// Nor may the task folder itself be a symlink: following it would move the deletion boundary.
	writeTaskFile(t, filepath.Join(taskDir, "tmp", "keep.txt"), "keep\n")
	linkedTask := filepath.Join(base, "linked-task")
	if err := os.Symlink(taskDir, linkedTask); err != nil {
		t.Fatal(err)
	}
	if err := removeTaskTmp(linkedTask); err == nil {
		t.Error("cleanup accepted a symlinked task folder")
	}
	if !fileExists(filepath.Join(taskDir, "tmp", "keep.txt")) {
		t.Error("rejected task-folder symlink still deleted its target")
	}

	// Symlinks nested inside a real tmp tree are removed as links; their external targets survive.
	if err := os.Symlink(outside, filepath.Join(taskDir, "tmp", "outside-link")); err != nil {
		t.Fatal(err)
	}
	if err := removeTaskTmp(taskDir); err != nil {
		t.Fatalf("remove real tmp containing a symlink: %v", err)
	}
	if !fileExists(filepath.Join(outside, "keep.txt")) {
		t.Error("nested tmp symlink cleanup escaped into its target")
	}
}

func TestTasksDoneSurfacesTmpCleanupFailure(t *testing.T) {
	root := t.TempDir()
	id := "2026-01-01-cleanup-fails"
	writeTaskFile(t, filepath.Join(root, StateInProgress, id, "task.md"), "# cleanup fails\n")
	writeTaskFile(t, filepath.Join(root, StateInProgress, id, "tmp", "scratch"), "keep until cleaned\n")

	oldCleaner := taskTmpCleaner
	taskTmpCleaner = func(string) error { return errors.New("injected cleanup failure") }
	t.Cleanup(func() { taskTmpCleaner = oldCleaner })
	code, err := tasksFolderMove(root, []string{id}, StateDone, "done", "done")
	if code == 0 || err == nil || !strings.Contains(err.Error(), "injected cleanup failure") ||
		!strings.Contains(err.Error(), "retry: coop tasks done "+id) {
		t.Fatalf("done cleanup failure = (%d, %v), want loud retryable failure", code, err)
	}
	if got := ReadTaskTree(root)[0].State; got != StateDone {
		t.Fatalf("successful folder transition should remain observable for retry, got %s", got)
	}
	if !fileExists(filepath.Join(root, StateDone, id, "tmp", "scratch")) {
		t.Error("injected cleanup failure unexpectedly removed tmp")
	}

	// `done` on an already-done task retries cleanup, so the surfaced failure is recoverable.
	taskTmpCleaner = oldCleaner
	if code, err := tasksFolderMove(root, []string{id}, StateDone, "done", "done"); code != 0 || err != nil {
		t.Fatalf("retry done cleanup: code=%d err=%v", code, err)
	}
	if pathExists(filepath.Join(root, StateDone, id, "tmp")) {
		t.Error("retry did not clean tmp")
	}
}

func TestNormalizeCompletedTaskStatePreservesAgentFields(t *testing.T) {
	taskDir := t.TempDir()
	statePath := filepath.Join(taskDir, "state.md")
	original := "# State — useful title\n\nintro remains\n**Status:** ready to commit\n**Done so far:** implemented the denial path\n**Next action:** commit and archive\n**Traps:** preserve this warning\nfooter remains\n"
	writeTaskFile(t, statePath, original)

	if err := normalizeCompletedTaskState("task-id", taskDir); err != nil {
		t.Fatal(err)
	}
	got := readFileString(statePath)
	for _, want := range []string{
		"# State — useful title", "intro remains", "**Status:** complete",
		"**Done so far:** implemented the denial path", "**Next action:** none",
		"**Traps:** preserve this warning", "footer remains",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("normalized state missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ready to commit") || strings.Contains(got, "commit and archive") {
		t.Errorf("normalized state retained stale lifecycle values:\n%s", got)
	}
	firstInfo, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := normalizeCompletedTaskState("task-id", taskDir); err != nil {
		t.Fatal(err)
	}
	if second := readFileString(statePath); second != got {
		t.Errorf("state finalization is not idempotent:\nfirst: %q\nsecond: %q", got, second)
	}
	secondInfo, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(firstInfo, secondInfo) {
		t.Error("already-final state was rewritten instead of remaining a filesystem no-op")
	}
}

func TestNormalizeCompletedTaskStateRepairsMissingAndMalformed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		done string
		trap string
	}{
		{name: "missing", done: "—", trap: "—"},
		{name: "duplicate lifecycle field", body: "**Status:** stale\n**Status:** also stale\n**Done so far:** retain me\n**Traps:** retain trap\n", done: "retain me", trap: "retain trap"},
		{name: "missing summary field", body: "**Status:** stale\n**Next action:** archive\n**Traps:** retain trap\n", done: "—", trap: "retain trap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			taskDir := t.TempDir()
			if tc.body != "" {
				writeTaskFile(t, filepath.Join(taskDir, "state.md"), tc.body)
			}
			if err := normalizeCompletedTaskState("task-id", taskDir); err != nil {
				t.Fatal(err)
			}
			got := readFileString(filepath.Join(taskDir, "state.md"))
			for _, want := range []string{"# State — task-id", "**Status:** complete", "**Done so far:** " + tc.done, "**Next action:** none", "**Traps:** " + tc.trap} {
				if !strings.Contains(got, want) {
					t.Errorf("repaired state missing %q:\n%s", want, got)
				}
			}
			if strings.Count(got, taskStateStatus) != 1 || strings.Count(got, taskStateNext) != 1 {
				t.Errorf("repaired state is still structurally ambiguous:\n%s", got)
			}
		})
	}
}

func TestNormalizeCompletedTaskStateRejectsSymlinkedState(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside-state")
	want := "outside state sentinel\n"
	writeTaskFile(t, outside, want)
	taskDir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(taskDir, "state.md")); err != nil {
		t.Fatal(err)
	}
	if err := normalizeCompletedTaskState("task-id", taskDir); err == nil || !strings.Contains(err.Error(), "single-link regular file") {
		t.Fatalf("symlinked state error = %v", err)
	}
	if got := readFileString(outside); got != want {
		t.Fatalf("outside state changed to %q", got)
	}
}

func TestTasksDoneRetriesStateFinalizationFailure(t *testing.T) {
	root := t.TempDir()
	id := "2026-01-01-state-fails"
	taskDir := filepath.Join(root, StateInProgress, id)
	writeTaskFile(t, filepath.Join(taskDir, "task.md"), "# state fails\n")
	writeTaskFile(t, filepath.Join(taskDir, "tmp", "scratch"), "retain until state is safe\n")
	if err := os.MkdirAll(filepath.Join(taskDir, "state.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	code, err := tasksFolderMove(root, []string{id}, StateDone, "done", "done")
	if code == 0 || err == nil || !strings.Contains(err.Error(), "state finalization failed") ||
		!strings.Contains(err.Error(), "retry: coop tasks done "+id) {
		t.Fatalf("done state failure = (%d, %v), want loud retryable failure", code, err)
	}
	doneDir := filepath.Join(root, StateDone, id)
	if !fileExists(filepath.Join(doneDir, "tmp", "scratch")) {
		t.Fatal("state failure must retain tmp for diagnosis and retry")
	}
	if err := os.RemoveAll(filepath.Join(doneDir, "state.md")); err != nil {
		t.Fatal(err)
	}
	if code, err := tasksFolderMove(root, []string{id}, StateDone, "done", "done"); code != 0 || err != nil {
		t.Fatalf("retry done after clearing state obstruction: code=%d err=%v", code, err)
	}
	if pathExists(filepath.Join(doneDir, "tmp")) {
		t.Fatal("successful retry did not remove tmp")
	}
	state := readFileString(filepath.Join(doneDir, "state.md"))
	if !strings.Contains(state, "**Status:** complete") || !strings.Contains(state, "**Next action:** none") {
		t.Errorf("successful retry did not create final state:\n%s", state)
	}
}

// moveTaskDir reports an actionable error, not a raw ENOENT, when the task's source folder
// vanished under it — a concurrent move to a different state won the race.
func TestMoveTaskDirSourceVanished(t *testing.T) {
	root := t.TempDir()
	ti := Item{ID: "2026-01-01-x", State: StateTodo, Dir: filepath.Join(root, StateTodo, "2026-01-01-x")}
	err := MoveTaskDir(root, ti, StateInProgress) // source never created → vanished
	if err == nil || !strings.Contains(err.Error(), "changed state under us") {
		t.Errorf("moveTaskDir with a vanished source = %v, want an actionable 'changed state' error", err)
	}
}

// Without --yes and no TTY (the test env), a destructive rm refuses and preserves the target — and
// names WHAT it would remove (the resolved id, or the --all-done count) so it isn't a blind delete.
func TestTasksRemoveGate(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-01-keep", "task.md"), "# keep\n")
	// by-id (substring match): refuses, task survives, error names the resolved id.
	code, err := tasksFolderRemove(root, []string{"keep"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "2026-01-01-keep") {
		t.Fatalf("rm without --yes = (%d, %v), want (2, a refusal naming the resolved id)", code, err)
	}
	if len(ReadTaskTree(root)) != 1 {
		t.Fatal("a refused rm must not delete the task")
	}
	// --all-done: refuses with the blast-radius count; the done task survives.
	writeTaskFile(t, filepath.Join(root, StateDone, "2026-01-02-done", "task.md"), "# done\n")
	code, err = tasksFolderRemove(root, []string{"--all-done"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "1 done task") {
		t.Fatalf("rm --all-done without --yes = (%d, %v), want (2, a refusal naming the count)", code, err)
	}
	if countDone(root) != 1 {
		t.Error("a refused --all-done must not delete anything")
	}
}

func TestTasksRemovePurgesStaleRunRecords(t *testing.T) {
	root := t.TempDir()
	task := taskForLease(t, root, StateInProgress, "2026-01-01-cancelled")
	lease, _, err := TryTaskLease(root, task, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	if err := MoveTaskDir(root, task, StateDone); err != nil {
		t.Fatal(err)
	}
	done, ok := CurrentTask(root, task.ID)
	if !ok {
		t.Fatal("completed task disappeared")
	}
	if err := lease.MarkCompleted(done.Dir); err != nil {
		t.Fatal(err)
	}
	// Simulate a controller dying after durable completion but before releasing its claim.
	lease.Quiesce()
	if err := unlockLeaseFile(lease.authority); err != nil {
		t.Fatal(err)
	}
	lease.authority = nil
	windows, err := BeginReviewCompletionWindows([]string{root}, []string{task.ID})
	if err != nil {
		t.Fatal(err)
	}
	windowID := windows.windows[0].id
	if err := windows.Abandon(); err != nil {
		t.Fatal(err)
	}
	indexFile, index, err := lockCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	record := index.Windows[windowID]
	record.AllowedDoneDeparture = task.ID
	record.AllowedDoneDepartures = []string{task.ID, "keep-other-task"}
	record.BaselineMutations = []string{task.ID, "keep-other-task"}
	record.RecoveredDepartures = []string{task.ID, "keep-other-task"}
	record.WorkSubject = task.ID
	index.Windows[windowID] = record
	writeErr := writeCompletionWindowIndex(root, index)
	if unlockErr := unlockLeaseFile(indexFile); unlockErr != nil {
		writeErr = errors.Join(writeErr, unlockErr)
	}
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if err := WriteAuditReopenRecord(root, testAuditReopenRecord(task.ID, "cancelled-generation")); err != nil {
		t.Fatal(err)
	}
	if err := writeTrustedDoneDeparture(root, trustedDoneDeparture{
		Version: trustedDoneDepartureVersion, TaskID: task.ID, Nonces: []string{strings.Repeat("0", 32)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := claimTaskOwnerRecord(root, task.ID); err != nil {
		t.Fatal(err)
	}

	if code, err := tasksFolderRemove(root, []string{task.ID, "--yes"}); code != 0 || err != nil {
		t.Fatalf("rm cancelled task = (%d, %v), want (0, nil)", code, err)
	}
	if _, ok := CurrentTask(root, task.ID); ok {
		t.Fatal("removed task survived in a lifecycle state")
	}
	index, err = ReadCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	record = index.Windows[windowID]
	if _, ok := record.Baseline[task.ID]; ok {
		t.Fatal("removed task survived in the stale completion-window baseline")
	}
	if record.AllowedDoneDeparture != "" ||
		len(record.AllowedDoneDepartures) != 1 || record.AllowedDoneDepartures[0] != "keep-other-task" ||
		len(record.BaselineMutations) != 1 || record.BaselineMutations[0] != "keep-other-task" ||
		len(record.RecoveredDepartures) != 1 || record.RecoveredDepartures[0] != "keep-other-task" ||
		len(record.ReviewSubjects) != 0 || !record.ReviewSubjectScoped || record.WorkSubject != "" {
		t.Fatalf("removed task survived in completion-window policy fields: %#v", record)
	}
	authority, err := OpenLeaseAuthority(root, task.ID, false)
	if err != nil {
		t.Fatalf("removed task lost its persistent authority inode: %v", err)
	}
	lockErr := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	info, statErr := authority.Stat()
	closeErr := unlockLeaseFile(authority)
	var size int64 = -1
	if info != nil {
		size = info.Size()
	}
	if lockErr != nil || statErr != nil || closeErr != nil || size != 0 {
		t.Fatalf(
			"removed task authority = size %d, lock %v, stat %v, close %v; want empty unlocked persistent inode",
			size, lockErr, statErr, closeErr,
		)
	}
	if leaseAuthorityMetadataExists(root, task.ID) {
		t.Fatal("removed task retained stale controller metadata")
	}
	if auditReopenRecordExists(root, task.ID) {
		t.Fatal("removed task retained stale audit-reopen authority")
	}
	if _, owned, err := ReadTaskOwnerRecord(root, task.ID); err != nil || owned {
		t.Fatalf("removed task retained its owner record: owned=%v err=%v", owned, err)
	}
	if _, ok, err := readTrustedDoneDeparture(root, task.ID); err != nil || ok {
		t.Fatalf("removed task trusted departure = ok %v, err %v; want absent", ok, err)
	}
	concurrent := taskForLease(t, root, StateInProgress, "2026-01-02-concurrent")
	concurrentLease, _, err := TryTaskLease(root, concurrent, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	if err := MoveTaskDir(root, concurrent, StateDone); err != nil {
		t.Fatal(err)
	}
	concurrentDone, ok := CurrentTask(root, concurrent.ID)
	if !ok {
		t.Fatal("concurrently completed task disappeared")
	}
	if err := concurrentLease.MarkCompleted(concurrentDone.Dir); err != nil {
		t.Fatal(err)
	}
	if err := concurrentLease.Release(); err != nil {
		t.Fatal(err)
	}
	recovered, err := ReconcileCompletionWindowsWithActivity([]string{root})
	if err != nil {
		t.Fatalf("restart reconciliation after subject deletion: %v", err)
	}
	if !slices.Equal(recovered, []string{concurrent.ID}) {
		t.Fatalf("restart recovered concurrent completions %v, want [%s]", recovered, concurrent.ID)
	}
}

func TestTasksRemoveRefusesLiveLeaseBeforePurging(t *testing.T) {
	root := t.TempDir()
	task := taskForLease(t, root, StateInProgress, "2026-01-01-live")
	lease, _, err := TryTaskLease(root, task, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	if err := MoveTaskDir(root, task, StateDone); err != nil {
		t.Fatal(err)
	}
	done, ok := CurrentTask(root, task.ID)
	if !ok {
		t.Fatal("completed live task disappeared")
	}
	if err := lease.MarkCompleted(done.Dir); err != nil {
		t.Fatal(err)
	}
	windows, err := BeginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	windowID := windows.windows[0].id
	if err := windows.Abandon(); err != nil {
		t.Fatal(err)
	}

	if code, err := tasksFolderRemove(root, []string{task.ID, "--yes"}); code != -1 || err == nil ||
		!strings.Contains(err.Error(), "still leased by a live controller") {
		t.Fatalf("rm live task = (%d, %v), want refusal", code, err)
	}
	if _, ok := CurrentTask(root, task.ID); !ok {
		t.Fatal("refused rm deleted the live task")
	}
	index, err := ReadCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.Windows[windowID].Baseline[task.ID]; !ok {
		t.Fatal("refused rm partially purged the completion-window baseline")
	}
	if !leaseAuthorityMetadataExists(root, task.ID) {
		t.Fatal("refused rm removed the active controller's metadata")
	}

	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if code, err := tasksFolderRemove(root, []string{task.ID, "--yes"}); code != 0 || err != nil {
		t.Fatalf("rm after lease release = (%d, %v), want success", code, err)
	}
}

// `coop tasks clear` is the bulk-delete idiom: it clears the done archive (= `rm --all-done`),
// gated the same way as rm — refuses without --yes in a non-TTY, deletes with it.
func TestTasksClear(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateDone, "d1", "task.md"), "# d\n")
	if code, err := CmdTasksFolder("", root, []string{"clear"}); code != 2 || err == nil {
		t.Fatalf("tasks clear without --yes = (%d, %v), want (2, gated)", code, err)
	}
	if countDone(root) != 1 {
		t.Error("a refused clear must not delete the done task")
	}
	if code, err := CmdTasksFolder("", root, []string{"clear", "--yes"}); code != 0 || err != nil {
		t.Fatalf("tasks clear --yes = (%d, %v), want (0, nil)", code, err)
	}
	if countDone(root) != 0 {
		t.Error("clear --yes should empty the done archive")
	}
}

func TestTasksFolderRemoveAllDone(t *testing.T) {
	root := t.TempDir()
	// two done tasks, one todo and one in_progress that must SURVIVE --all-done
	writeTaskFile(t, filepath.Join(root, StateDone, "2026-01-01-a", "task.md"), "# a\n")
	writeTaskFile(t, filepath.Join(root, StateDone, "2026-01-02-b", "task.md"), "# b\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-03-c", "task.md"), "# c\n")
	writeTaskFile(t, filepath.Join(root, StateInProgress, "2026-01-04-d", "task.md"), "# d\n")
	windows, err := BeginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	windowID := windows.windows[0].id
	if err := windows.Abandon(); err != nil {
		t.Fatal(err)
	}

	if code, err := tasksFolderRemove(root, []string{"--all-done", "--yes"}); code != 0 || err != nil {
		t.Fatalf("rm --all-done: code=%d err=%v", code, err)
	}
	items := ReadTaskTree(root)
	if len(items) != 2 {
		t.Fatalf("after --all-done, want 2 tasks left (todo+in_progress), got %d", len(items))
	}
	for _, it := range items {
		if it.State == StateDone {
			t.Errorf("a done task survived --all-done: %s", it.ID)
		}
	}
	index, err := ReadCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(index.Windows[windowID].Baseline); got != 0 {
		t.Fatalf("rm --all-done retained %d archived task(s) in the stale completion-window baseline", got)
	}
	if err := ReconcileCompletionWindows([]string{root}); err != nil {
		t.Fatalf("restart reconciliation after rm --all-done: %v", err)
	}
	// A second run is a clean no-op (nothing done left), not an error.
	if code, err := tasksFolderRemove(root, []string{"--all-done"}); code != 0 || err != nil {
		t.Errorf("rm --all-done with no done tasks should be a no-op, got (%d, %v)", code, err)
	}
	// Bare `rm` (no id, no flag) is a usage error.
	if code, _ := tasksFolderRemove(root, nil); code != 2 {
		t.Errorf("rm with no args should be a usage error (2), got %d", code)
	}
}

func TestTasksFolderRemoveAllDoneStopsAtLiveLease(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateDone, "2026-01-01-a", "task.md"), "# a\n")
	busy := taskForLease(t, root, StateInProgress, "2026-01-02-b")
	lease, _, err := TryTaskLease(root, busy, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	if err := MoveTaskDir(root, busy, StateDone); err != nil {
		t.Fatal(err)
	}
	done, ok := CurrentTask(root, busy.ID)
	if !ok {
		t.Fatal("busy completed task disappeared")
	}
	if err := lease.MarkCompleted(done.Dir); err != nil {
		t.Fatal(err)
	}

	code, err := tasksFolderRemove(root, []string{"--all-done", "--yes"})
	if code != -1 || err == nil {
		t.Fatalf("rm --all-done with a later live lease = (%d, %v), want failure", code, err)
	}
	for _, want := range []string{
		busy.ID,
		"1 task removed before stop",
		"coop tasks rm --all-done --yes",
		"still leased by a live controller",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("bulk failure missing %q:\n%s", want, err)
		}
	}
	if _, ok := CurrentTask(root, "2026-01-01-a"); ok {
		t.Fatal("bulk failure restored an earlier successfully removed task")
	}
	if current, ok := CurrentTask(root, busy.ID); !ok || current.State != StateDone {
		t.Fatal("bulk failure removed or moved the busy task")
	}

	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if code, err := tasksFolderRemove(root, []string{"--all-done", "--yes"}); code != 0 || err != nil {
		t.Fatalf("bulk retry after lease release = (%d, %v), want success", code, err)
	}
	if _, ok := CurrentTask(root, busy.ID); ok {
		t.Fatal("bulk retry left the released task")
	}
}

func TestCmdTasksFolderDispatch(t *testing.T) {
	root := t.TempDir()
	// no sub-command (empty rest) must not panic and should list cleanly
	if code, err := CmdTasksFolder(root, root, nil); code != 0 || err != nil {
		t.Fatalf("cmdTasksFolder(nil): code=%d err=%v", code, err)
	}
	if code, err := CmdTasksFolder(root, root, []string{}); code != 0 || err != nil {
		t.Fatalf("cmdTasksFolder([]): code=%d err=%v", code, err)
	}
	// add then list through the dispatcher
	if code, err := CmdTasksFolder(root, root, []string{"add", "Hello world"}); code != 0 || err != nil {
		t.Fatalf("add via dispatch: code=%d err=%v", code, err)
	}
	if code, err := CmdTasksFolder(root, root, []string{"ls"}); code != 0 || err != nil {
		t.Fatalf("ls via dispatch: code=%d err=%v", code, err)
	}
	if code, _ := CmdTasksFolder(root, root, []string{"bogus"}); code != 2 {
		t.Errorf("unknown sub should return code 2, got %d", code)
	}
}

func TestTasksFolderSplitCommand(t *testing.T) {
	repo := t.TempDir()
	root := filepath.Join(repo, ".agent", "tasks")
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-01-a", "task.md"), "# a\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-02-b", "task.md"), "# b\n")
	var code int
	var err error
	out := captureStderr(t, func() { code, err = tasksFolderSplit(repo, root, []string{"2"}) })
	if code != 0 || err != nil {
		t.Fatalf("split 2: code=%d err=%v", code, err)
	}
	if !IsTaskDir(filepath.Join(repo, ".agent", "tasks.slice1")) || !IsTaskDir(filepath.Join(repo, ".agent", "tasks.slice2")) {
		t.Error("split did not create both slice dirs")
	}
	for _, want := range []string{
		"coop fork slice1 <target|preset> --loop -d --tasks .agent/tasks.slice1",
		"coop fork slice2 <target|preset> --loop -d --tasks .agent/tasks.slice2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("split output missing %q:\n%s", want, out)
		}
	}
	if got := strings.Count(out, "run: coop fork slice"); got != 2 {
		t.Errorf("split output has %d direct fork commands, want one per written slice (2):\n%s", got, out)
	}
	if code, _ := tasksFolderSplit(repo, root, []string{"0"}); code != 2 {
		t.Errorf("split 0 should be a usage error (2), got %d", code)
	}
	if code, _ := tasksFolderSplit(repo, root, nil); code != 2 {
		t.Errorf("split with no n should be a usage error (2), got %d", code)
	}
}

// The tasks unknown-subcommand suggester and isTasksSubcommand share one source (tasksVerbs), so the
// flagship `watch` is suggestable and every verb+alias is recognized — no drift between the two.
func TestTasksVerbsIncludeWatch(t *testing.T) {
	// a mistype of watch suggests it — only possible if watch is in the derived candidate list.
	if err := unknownErr("tasks command", "watxh", TasksVerbs); !strings.Contains(err.Error(), `did you mean "watch"`) {
		t.Errorf("expected a watch suggestion, got: %v", err)
	}
	for _, s := range []string{"watch", "ls", "rm", "clear", "decisions"} {
		if !isTasksSubcommand(s) {
			t.Errorf("isTasksSubcommand(%q) = false, want true", s)
		}
	}
	if isTasksSubcommand("bogus") {
		t.Error("isTasksSubcommand(bogus) = true, want false")
	}
	// v3 keeps no compat aliases: start→claim, list→ls, remove→rm are all retired.
	for _, s := range []string{"start", "list", "remove"} {
		if isTasksSubcommand(s) {
			t.Errorf("isTasksSubcommand(%q) = true, want false (retired in v3)", s)
		}
	}
}

func TestTasksFolderLint(t *testing.T) {
	// findings: blocked-without-decision, todo-with-decision, status field, missing acceptance
	root := t.TempDir()
	if err := ScaffoldStateDirs(root); err != nil { // isolate the content findings from the missing-state-dir check
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(root, StateBlocked, "b1", "task.md"), "---\ntitle: B\n---\n# B\n**Acceptance criteria:** x\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "t1", "task.md"), "---\ntitle: T\nstatus: todo\n---\n# T\nno accept here\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "t2", "task.md"), "# T2\n**Acceptance criteria:** ok\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "t2", "decision.md"), "# Decision: ?\n")
	if code, err := tasksFolderLint(root); err != nil || code != 1 {
		t.Fatalf("lint with findings: code=%d err=%v (want 1)", code, err)
	}

	// clean tree — a complete task carries all three sections (Context / Acceptance criteria / Approach).
	clean := t.TempDir()
	if err := ScaffoldStateDirs(clean); err != nil { // a real queue has all four state dirs (lint flags a tree missing any)
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(clean, StateTodo, "ok", "task.md"), "---\ntitle: OK\n---\n# OK\n**Context:** why\n**Acceptance criteria:** the gate is green\n**Approach:** do it\n")
	writeTaskFile(t, filepath.Join(clean, StateBlocked, "bk", "task.md"), "# BK\n**Context:** c\n**Acceptance criteria:** y\n**Approach:** a\n")
	writeTaskFile(t, filepath.Join(clean, StateBlocked, "bk", "decision.md"), "# Decision: which?\n**Recommendation:** A\n")
	if code, err := tasksFolderLint(clean); err != nil || code != 0 {
		t.Fatalf("clean lint: code=%d err=%v (want 0)", code, err)
	}
}

// A queue missing any state dir is a corruption trap: the in-box "move a folder between states"
// protocol would rename a task into the nonexistent dir (see scaffoldStateDirs). lint flags it (exit
// 1); scaffolding the four makes it clean.
func TestTasksFolderLintFlagsMissingStateDir(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "t1", "task.md"),
		"# T\n**Context:** c\n**Acceptance criteria:** the gate is green\n**Approach:** a\n")
	// only 00_todo exists (a hand-made or pre-fix tree) — the other three are missing.
	if code, err := tasksFolderLint(root); err != nil || code != 1 {
		t.Fatalf("lint of a queue missing state dirs: code=%d err=%v (want 1)", code, err)
	}
	if err := ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	if code, err := tasksFolderLint(root); code != 0 || err != nil {
		t.Errorf("after scaffolding all four state dirs, lint should be clean: code=%d err=%v", code, err)
	}
}

// A task id copied into TWO state dirs (cp instead of a coop move) is deliberately masked by
// readTaskTree's torn-read dedup, so `coop tasks` and the loop stay quiet — lint is the surface
// that flags the persistent duplicate. readTaskTree itself must keep showing exactly one.
func TestTasksFolderLintFlagsDuplicateIDs(t *testing.T) {
	root := t.TempDir()
	if err := ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	task := "# D\n**Context:** c\n**Acceptance criteria:** the gate is green\n**Approach:** a\n"
	writeTaskFile(t, filepath.Join(root, StateTodo, "dup", "task.md"), task)
	writeTaskFile(t, filepath.Join(root, StateDone, "dup", "task.md"), task)
	if got := len(ReadTaskTree(root)); got != 1 {
		t.Fatalf("readTaskTree sees %d items, want 1 — the dedup must keep masking the hot path", got)
	}
	if code, err := tasksFolderLint(root); err != nil || code != 1 {
		t.Fatalf("lint of a duplicated id: code=%d err=%v (want 1)", code, err)
	}
	if err := os.RemoveAll(filepath.Join(root, StateDone, "dup")); err != nil {
		t.Fatal(err)
	}
	if code, err := tasksFolderLint(root); code != 0 || err != nil {
		t.Errorf("after removing the stale copy, lint should be clean: code=%d err=%v", code, err)
	}
}

// `coop tasks add` seeds self-documenting task.md + log.md + state.md (but not decision.md,
// which would make a todo task lint-dirty), and the result is lint-clean out of the box.
func TestTasksFolderAddSeedsSelfDocumentingFiles(t *testing.T) {
	root := t.TempDir()
	if code, err := tasksFolderAdd(root, []string{"make egress fail closed"}, StateTodo, "tasks add"); code != 0 || err != nil {
		t.Fatalf("add: code=%d err=%v", code, err)
	}
	items := ReadTaskTree(root)
	if len(items) != 1 {
		t.Fatalf("want 1 task, got %d", len(items))
	}
	dir := filepath.Join(root, StateTodo, items[0].ID)

	for _, f := range []string{"task.md", "log.md", "state.md"} {
		if !fileExists(filepath.Join(dir, f)) {
			t.Errorf("add should seed %s", f)
		}
		if body := readFileString(filepath.Join(dir, f)); !strings.Contains(body, "<!--") {
			t.Errorf("%s should open with an explanatory header comment", f)
		}
	}
	if fileExists(filepath.Join(dir, "decision.md")) {
		t.Error("add must NOT seed decision.md — a todo task carrying one is a lint error")
	}
	// A freshly-added task is lint-clean (all sections present, no decision in todo, no status field).
	if code, err := tasksFolderLint(root); code != 0 || err != nil {
		t.Errorf("a freshly-added task should be lint-clean, got code=%d err=%v", code, err)
	}
}

// taskBody with no values reproduces the scaffold body byte-for-byte (the single shape source stays
// stable), and taskShapeIssues flags a body missing a section but not the all-sections scaffold.
func TestTaskBodyScaffoldStable(t *testing.T) {
	want := "**Context:** <the problem, why it matters, and where in the code it lives>\n\n" +
		"**Acceptance criteria:** <the gate green + the behaviour/test that proves it's done>\n\n" +
		"**Approach:** <the boring plan; when it outgrows ~a screen, move it into spec.md>\n\n" +
		"## Subtasks\n" +
		"- [ ] <first small, end-to-end, testable step — check off once the gate is green>\n"
	if got := taskBody(nil, nil); got != want {
		t.Errorf("scaffold body drifted from the single source:\ngot:  %q\nwant: %q", got, want)
	}
	if issues := taskShapeIssues(taskBody(nil, nil)); len(issues) != 0 {
		t.Errorf("scaffold has all sections present, want no issues, got %v", issues)
	}
	if issues := taskShapeIssues("# t\n**Acceptance criteria:** x\n"); len(issues) != 2 { // missing Context + Approach
		t.Errorf("body missing Context+Approach should yield 2 issues, got %v", issues)
	}
}

// `coop tasks add` with structured flags creates a FILLED, lint-clean task in one call; partial flags
// are all-or-nothing (no folder created); with no flags it's the placeholder scaffold.
func TestTasksFolderAddStructuredFlags(t *testing.T) {
	root := t.TempDir()
	code, err := tasksFolderAdd(root, []string{"wire", "auth",
		"--context", "the login retries loop",
		"--acceptance", "gate green + a retry test",
		"--approach", "cap attempts at 3",
		"--subtask", "add the cap", "--subtask", "test the failure path"}, StateTodo, "tasks add")
	if code != 0 || err != nil {
		t.Fatalf("structured add: code=%d err=%v", code, err)
	}
	items := ReadTaskTree(root)
	if len(items) != 1 {
		t.Fatalf("want 1 task, got %d", len(items))
	}
	body := readFileString(filepath.Join(items[0].Dir, "task.md"))
	for _, want := range []string{
		"# wire auth", "**Context:** the login retries loop",
		"**Acceptance criteria:** gate green + a retry test", "**Approach:** cap attempts at 3",
		"- [ ] add the cap", "- [ ] test the failure path",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("structured body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<the problem") {
		t.Errorf("a fully-flagged task should carry no placeholders:\n%s", body)
	}
	if code, err := tasksFolderLint(root); code != 0 || err != nil {
		t.Errorf("structured task should be lint-clean, got code=%d err=%v", code, err)
	}
	// Partial flags → refused (exit 2), and NOTHING created.
	root2 := t.TempDir()
	if code, _ := tasksFolderAdd(root2, []string{"half", "--context", "only this"}, StateTodo, "tasks add"); code != 2 {
		t.Errorf("partial structured flags should be a usage error (2), got %d", code)
	}
	if len(ReadTaskTree(root2)) != 0 {
		t.Error("a refused structured add must not create a task folder")
	}
}

// A REPEATED section flag must accumulate, not silently last-wins — a paste with several
// --acceptance clauses used to keep only the last, dropping the rest (real data loss).
func TestTasksFolderAddRepeatedSectionFlag(t *testing.T) {
	root := t.TempDir()
	code, err := tasksFolderAdd(root, []string{"harden", "the", "endpoint",
		"--context", "the MCP surface needs certifying",
		"--acceptance", "OAuth is fail-closed: token must carry mcp scope",
		"--acceptance", "Streamable HTTP method/header/origin validated",
		"--acceptance", "risk-tier mapping explicit and tested",
		"--approach", "start with failing tests",
		"--subtask", "oauth tests"}, StateTodo, "tasks add")
	if code != 0 || err != nil {
		t.Fatalf("repeated-flag add: code=%d err=%v", code, err)
	}
	body := readFileString(filepath.Join(ReadTaskTree(root)[0].Dir, "task.md"))
	// ALL three acceptance clauses survive (not just the last), under the one heading.
	for _, want := range []string{
		"OAuth is fail-closed: token must carry mcp scope",
		"Streamable HTTP method/header/origin validated",
		"risk-tier mapping explicit and tested",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("repeated --acceptance dropped a clause %q:\n%s", want, body)
		}
	}
	// They accumulate under a SINGLE Acceptance heading (not one heading per flag).
	if n := strings.Count(body, "**Acceptance criteria:**"); n != 1 {
		t.Errorf("want one Acceptance heading, got %d", n)
	}
}

// `coop tasks block` writes a decision.md that's self-documenting and easy for a human to
// answer: the structured sections, a HUMAN reply marker, and the exact unblock command.
func TestValidateArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		flags   []string
		maxPos  int
		wantErr bool
	}{
		{"id positional", []string{"my-task"}, nil, 1, false},
		{"allowed flag, no positional", []string{"--all"}, []string{"--all"}, 0, false},
		{"unknown flag", []string{"--bogus"}, []string{"--all"}, 0, true},
		{"flag where none allowed", []string{"--all"}, nil, 1, true},
		{"too many positionals", []string{"a", "b"}, nil, 1, true},
		{"allowed flag counts as 0 positionals", []string{"--all-done"}, []string{"--all-done"}, 1, false},
		{"nothing", nil, []string{"--all"}, 0, false},
	}
	for _, tc := range cases {
		if err := validateArgs("tasks x", tc.args, tc.flags, tc.maxPos); (err != nil) != tc.wantErr {
			t.Errorf("%s: validateArgs err=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}

// `coop tasks ls` caps the (only-growing) done archive so live work isn't buried; --all shows all.
func TestTasksFolderListSubtaskLegend(t *testing.T) {
	// A task WITH subtasks → the [n/m] marker AND a one-line legend explaining it.
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-01-a", "task.md"), "# A\n\n## Subtasks\n- [ ] one\n- [x] two\n")
	out := captureStdout(t, func() { _, _ = tasksFolderList(root, false) })
	if !strings.Contains(out, "[1/2]") {
		t.Errorf("expected the [1/2] subtask marker:\n%s", out)
	}
	if !strings.Contains(out, "= subtasks") {
		t.Errorf("a task with subtasks should show the legend:\n%s", out)
	}
	// A task WITHOUT subtasks → no legend, so the common listing stays uncluttered.
	bare := t.TempDir()
	writeTaskFile(t, filepath.Join(bare, StateTodo, "2026-01-01-b", "task.md"), "# B\n\nno checkboxes here\n")
	out2 := captureStdout(t, func() { _, _ = tasksFolderList(bare, false) })
	if strings.Contains(out2, "= subtasks") {
		t.Errorf("a subtask-free listing must not show the legend:\n%s", out2)
	}
}

// `coop tasks ls` tags an in-progress row with who claimed it (tag-exceptions-not-every-row: only
// the exceptional, claimed row) instead of its usual lease label — which would otherwise read the
// misleading "unleased" for a task nobody need be actively holding a lock on to own.
func TestTasksFolderListShowsOwner(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateInProgress, "2026-01-01-unowned", "task.md"), "# Unowned\n")
	unowned := captureStdout(t, func() { _, _ = tasksFolderList(root, false) })
	if !strings.Contains(unowned, "unleased") {
		t.Errorf("an in-progress task with no lease and no claim should show \"unleased\":\n%s", unowned)
	}
	if strings.Contains(unowned, "claimed by") {
		t.Errorf("an unclaimed task must not show a claimed-by tag:\n%s", unowned)
	}

	if code, err := tasksFolderMove(root, []string{"2026-01-01-unowned"}, StateInProgress, "claim", "claimed"); code != 0 || err != nil {
		t.Fatalf("claim: code=%d err=%v", code, err)
	}
	owned := captureStdout(t, func() { _, _ = tasksFolderList(root, false) })
	rec, ok := taskOwned(t, root, "2026-01-01-unowned")
	if !ok {
		t.Fatal("expected an owner record after claim")
	}
	if !strings.Contains(owned, "claimed by "+rec.User) {
		t.Errorf("a claimed task should show \"claimed by %s\":\n%s", rec.User, owned)
	}
	if strings.Contains(owned, "unleased") {
		t.Errorf("a claimed task must not also show the lease label \"unleased\":\n%s", owned)
	}
}

func TestTasksFolderListCapsDone(t *testing.T) {
	root := t.TempDir()
	for i := 1; i <= 7; i++ {
		writeTaskFile(t, filepath.Join(root, StateDone, fmt.Sprintf("2026-01-%02d-done%d", i, i), "task.md"), fmt.Sprintf("# Done task %d\n", i))
	}
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-02-01-live", "task.md"), "# Live work\n")

	capped := captureStdout(t, func() { _, _ = tasksFolderList(root, false) })
	if !strings.Contains(capped, "+2 earlier") { // 7 done, cap 5 → 2 elided
		t.Errorf("default ls should cap done with '+2 earlier':\n%s", capped)
	}
	if strings.Contains(capped, "Done task 1") || strings.Contains(capped, "Done task 2") { // oldest hidden
		t.Errorf("the 2 oldest done should be elided:\n%s", capped)
	}
	if !strings.Contains(capped, "Done task 7") || !strings.Contains(capped, "Live work") {
		t.Errorf("recent done + live work must still show:\n%s", capped)
	}
	all := captureStdout(t, func() { _, _ = tasksFolderList(root, true) })
	if !strings.Contains(all, "Done task 1") || strings.Contains(all, "earlier") {
		t.Errorf("--all should show every done with no elision:\n%s", all)
	}
}

func TestFileURI(t *testing.T) {
	// Absolute path → file:// URL with unsafe characters percent-encoded (a space here).
	if got, want := fileURI("/Users/x/a b"), "file:///Users/x/a%20b"; got != want {
		t.Errorf("fileURI = %q, want %q", got, want)
	}
}

// `coop tasks ls --blocked` (and the other state flags) narrows the listing to those states, with
// a footer that echoes only what's shown; a filter matching nothing says so plainly.
func TestTasksFolderListStateFilter(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-01-todoA", "task.md"), "# Todo A\n")
	writeTaskFile(t, filepath.Join(root, StateBlocked, "2026-01-02-blockedB", "task.md"), "# Blocked B\n")
	writeTaskFile(t, filepath.Join(root, StateDone, "2026-01-03-doneC", "task.md"), "# Done C\n")

	blocked := captureStdout(t, func() { _, _ = tasksFolderList(root, false, StateBlocked) })
	if !strings.Contains(blocked, "Blocked B") || strings.Contains(blocked, "Todo A") || strings.Contains(blocked, "Done C") {
		t.Errorf("--blocked should show only the blocked task:\n%s", blocked)
	}
	if strings.Contains(blocked, "in progress") { // a hidden state must not leak into the footer summary
		t.Errorf("filtered summary must list only the shown state:\n%s", blocked)
	}

	// A union of flags shows every named state, and nothing else.
	union := captureStdout(t, func() { _, _ = tasksFolderList(root, false, StateTodo, StateDone) })
	if !strings.Contains(union, "Todo A") || !strings.Contains(union, "Done C") || strings.Contains(union, "Blocked B") {
		t.Errorf("--todo --done union wrong:\n%s", union)
	}

	// A filter that matches nothing prints a plain note instead of an empty block + summary.
	only := t.TempDir()
	writeTaskFile(t, filepath.Join(only, StateTodo, "2026-01-01-x", "task.md"), "# X\n")
	none := captureStdout(t, func() { _, _ = tasksFolderList(only, false, StateBlocked) })
	if !strings.Contains(none, "no blocked tasks") {
		t.Errorf("an empty filter should note 'no blocked tasks':\n%s", none)
	}

	// The flags pass validation via the real dispatch, and a typo is still rejected with a hint.
	if code, err := CmdTasksFolder(root, root, []string{"ls", "--blocked", "--todo"}); code != 0 || err != nil {
		t.Errorf("ls --blocked --todo: got (%d, %v), want (0, nil)", code, err)
	}
	if code, err := CmdTasksFolder(root, root, []string{"ls", "--blockd"}); code != 2 || err == nil {
		t.Errorf("ls --blockd (typo): got (%d, %v), want (2, err)", code, err)
	}
}

// In an umbrella project the filter threads through the multi-queue roll-up: --blocked shows the
// blocked work in every subproject's queue and hides the rest; a bad flag is rejected there too.
func TestTasksListAllStateFilter(t *testing.T) {
	repo := t.TempDir()
	writeTaskFile(t, filepath.Join(repo, "a", ".agent", "tasks", StateBlocked, "2026-01-01-ablk", "task.md"), "# A blocked\n")
	writeTaskFile(t, filepath.Join(repo, "a", ".agent", "tasks", StateTodo, "2026-01-02-atodo", "task.md"), "# A todo\n")
	writeTaskFile(t, filepath.Join(repo, "b", ".agent", "tasks", StateBlocked, "2026-01-03-bblk", "task.md"), "# B blocked\n")
	rels := []string{filepath.Join("a", ".agent", "tasks"), filepath.Join("b", ".agent", "tasks")}

	out := captureStdout(t, func() { _, _ = tasksListAll(repo, rels, []string{"--blocked"}) })
	if !strings.Contains(out, "A blocked") || !strings.Contains(out, "B blocked") {
		t.Errorf("umbrella --blocked should show blocked work in every queue:\n%s", out)
	}
	if strings.Contains(out, "A todo") {
		t.Errorf("umbrella --blocked must hide todo work:\n%s", out)
	}
	if code, err := tasksListAll(repo, rels, []string{"--blockd"}); code != 2 || err == nil {
		t.Errorf("umbrella ls with a bad flag: got (%d, %v), want (2, err)", code, err)
	}
}

// unblock must not drop a task into todo with an UNRESOLVED decision.md — that's the exact state
// lint rejects ("unresolved decision.md but is todo"). With no inline answer and a placeholder
// Resolution it refuses (task stays blocked); an inline answer resolves it and unblocks lint-clean.
func TestUnblockRequiresResolution(t *testing.T) {
	root := t.TempDir()
	if err := ScaffoldStateDirs(root); err != nil { // a real queue has all four state dirs (lint flags a tree missing any)
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-01-pick", "task.md"),
		"# Pick a backend\n\n**Context:** need a datastore\n**Acceptance criteria:** one is chosen and why is noted\n**Approach:** compare options\n")
	if code, err := tasksFolderBlock(root, []string{"pick"}); code != 0 || err != nil {
		t.Fatalf("block: %d %v", code, err)
	}
	// No answer + placeholder Resolution → refuse, stay blocked (don't create a lint-rejected todo).
	if code, err := tasksFolderUnblock(root, []string{"pick"}); code != 2 || err == nil {
		t.Fatalf("unblock with no resolution: got (%d, %v), want (2, err)", code, err)
	}
	if ReadTaskTree(root)[0].State != StateBlocked {
		t.Fatal("a refused unblock must leave the task blocked")
	}
	// With an inline answer → resolves the decision and unblocks to todo.
	if code, err := tasksFolderUnblock(root, []string{"pick", "Postgres"}); code != 0 || err != nil {
		t.Fatalf("unblock with answer: %d %v", code, err)
	}
	tk := ReadTaskTree(root)[0]
	if tk.State != StateTodo {
		t.Fatalf("after answered unblock, state=%s want todo", tk.State)
	}
	if !decisionResolved(filepath.Join(tk.Dir, "decision.md")) {
		t.Error("decision.md should be resolved after an inline answer")
	}
	if code, _ := tasksFolderLint(root); code != 0 {
		t.Error("an answered-unblock task must be lint-clean")
	}
}

func TestTasksFolderBlockSeedsHumanReplyDecision(t *testing.T) {
	root := t.TempDir()
	if code, err := tasksFolderAdd(root, []string{"pick the database"}, StateTodo, "tasks add"); code != 0 || err != nil {
		t.Fatalf("add: code=%d err=%v", code, err)
	}
	id := ReadTaskTree(root)[0].ID
	if code, err := tasksFolderBlock(root, []string{id}); code != 0 || err != nil {
		t.Fatalf("block: code=%d err=%v", code, err)
	}
	dec := readFileString(filepath.Join(root, StateBlocked, id, "decision.md"))
	for _, want := range []string{
		"# Decision:", "**The decision:**", "**Options:**", "**Recommendation:**",
		"**Resolution:**", "HUMAN:", "coop tasks unblock " + id,
	} {
		if !strings.Contains(dec, want) {
			t.Errorf("decision.md missing %q:\n%s", want, dec)
		}
	}
}

// `coop tasks unblock <id> <answer>` records the answer into decision.md's Resolution (replacing
// the HUMAN placeholder) and moves the task to in_progress — deciding it in one command. The rest
// of the decision.md survives the edit and the updated file rides along to the new state.
func TestTasksFolderUnblockRecordsInlineAnswer(t *testing.T) {
	root := t.TempDir()
	if code, err := tasksFolderAdd(root, []string{"pick the db"}, StateTodo, "tasks add"); code != 0 || err != nil {
		t.Fatalf("add: code=%d err=%v", code, err)
	}
	id := ReadTaskTree(root)[0].ID
	if code, err := tasksFolderBlock(root, []string{id}); code != 0 || err != nil {
		t.Fatalf("block: code=%d err=%v", code, err)
	}
	if code, err := tasksFolderUnblock(root, []string{id, "B", "—", "go", "SQLite"}); code != 0 || err != nil {
		t.Fatalf("unblock+answer: code=%d err=%v", code, err)
	}
	if ReadTaskTree(root)[0].State != StateTodo {
		t.Fatal("after unblock, not back in todo")
	}
	dec := readFileString(filepath.Join(root, StateTodo, id, "decision.md"))
	if !strings.Contains(dec, "**Resolution:** B — go SQLite\n") {
		t.Errorf("answer not recorded into Resolution:\n%s", dec)
	}
	if strings.Contains(dec, "your answer") {
		t.Errorf("inline answer should replace the placeholder, not leave it:\n%s", dec)
	}
	for _, want := range []string{"# Decision:", "**Options:**", "**Recommendation:**"} {
		if !strings.Contains(dec, want) {
			t.Errorf("decision.md lost %q after recording the answer:\n%s", want, dec)
		}
	}
	// the resolved decision.md riding along must NOT make the todo task lint-dirty
	if code, err := tasksFolderLint(root); code != 0 || err != nil {
		t.Errorf("unblocked task with a resolved decision should lint clean, got code=%d err=%v", code, err)
	}
}

func TestStripHTMLComments(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"a <!-- x --> b", "a  b"},
		{"<!-- only -->", ""},
		{"line1\n<!-- multi\nline -->\nline2", "line1\n\nline2"},
		{"text <!-- unterminated", "text "},
		{"no comment", "no comment"},
	} {
		if got := stripHTMLComments(c.in); got != c.want {
			t.Errorf("stripHTMLComments(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// runDecisionBrowser: :n skips the first decision, then a typed answer resolves the second;
// answering the last one auto-finishes. The answered task moves to todo with its recorded answer;
// the skipped one stays blocked. I/O is injected so no real terminal is needed.
func TestRunDecisionBrowser(t *testing.T) {
	root := t.TempDir()
	for _, title := range []string{"alpha", "beta"} {
		if code, err := tasksFolderAdd(root, []string{title}, StateTodo, "tasks add"); code != 0 || err != nil {
			t.Fatalf("add %s: code=%d err=%v", title, code, err)
		}
	}
	for _, it := range ReadTaskTree(root) {
		if code, err := tasksFolderBlock(root, []string{it.ID}); code != 0 || err != nil {
			t.Fatalf("block %s: code=%d err=%v", it.ID, code, err)
		}
	}
	var decisions []Item
	for _, it := range ReadTaskTree(root) {
		if it.State == StateBlocked {
			decisions = append(decisions, it)
		}
	}
	if len(decisions) != 2 {
		t.Fatalf("want 2 blocked decisions, got %d", len(decisions))
	}
	in := strings.NewReader(":n\nSQLite it is\n")
	var out bytes.Buffer
	if code, err := runDecisionBrowser(decisionRefs(root, "", decisions), in, &out); code != 0 || err != nil {
		t.Fatalf("browser: code=%d err=%v", code, err)
	}
	if strings.Contains(out.String(), " · · ") {
		t.Errorf("single-queue browser must not render an empty queue label:\n%s", out.String())
	}
	if a, _ := FindTask(root, decisions[0].ID); a.State != StateBlocked {
		t.Errorf("skipped decision should stay blocked, got %s", a.State)
	}
	b, err := FindTask(root, decisions[1].ID)
	if err != nil || b.State != StateTodo {
		t.Fatalf("answered decision should be in todo, got %v (err %v)", b.State, err)
	}
	if dec := readFileString(filepath.Join(b.Dir, "decision.md")); !strings.Contains(dec, "**Resolution:** SQLite it is") {
		t.Errorf("answer not recorded into the answered decision:\n%s", dec)
	}
	if !strings.Contains(out.String(), "decision 1 of 2") {
		t.Errorf("browser output missing the position header:\n%s", out.String())
	}
}

// runDecisionBrowser: :d deletes (drops) the current decision's task after a y confirm read from
// the browser's own scanner — an unrecoverable folder removal, not a "done" move. Deleting both
// decisions empties the queue and finishes; the removed folders are gone from disk.
func TestRunDecisionBrowserDelete(t *testing.T) {
	root := t.TempDir()
	for _, title := range []string{"alpha", "beta"} {
		if code, err := tasksFolderAdd(root, []string{title}, StateTodo, "tasks add"); code != 0 || err != nil {
			t.Fatalf("add %s: code=%d err=%v", title, code, err)
		}
	}
	for _, it := range ReadTaskTree(root) {
		if code, err := tasksFolderBlock(root, []string{it.ID}); code != 0 || err != nil {
			t.Fatalf("block %s: code=%d err=%v", it.ID, code, err)
		}
	}
	var decisions []Item
	for _, it := range ReadTaskTree(root) {
		if it.State == StateBlocked {
			decisions = append(decisions, it)
		}
	}
	if len(decisions) != 2 {
		t.Fatalf("want 2 blocked decisions, got %d", len(decisions))
	}
	const windowID = "decision-delete-window"
	indexFile, index, err := lockCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	index.Windows[windowID] = CompletionWindowRecord{
		Baseline: map[string]CompletionFingerprint{
			decisions[0].ID: {},
			decisions[1].ID: {},
		},
		ReviewWindow:   true,
		ReviewSubjects: []string{decisions[0].ID, decisions[1].ID},
	}
	writeErr := writeCompletionWindowIndex(root, index)
	if unlockErr := unlockLeaseFile(indexFile); unlockErr != nil {
		writeErr = errors.Join(writeErr, unlockErr)
	}
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	in := strings.NewReader(":d\ny\n:d\ny\n") // delete both, each behind a y confirm
	var out bytes.Buffer
	if code, err := runDecisionBrowser(decisionRefs(root, "", decisions), in, &out); code != 0 || err != nil {
		t.Fatalf("browser: code=%d err=%v", code, err)
	}
	for _, d := range decisions {
		if got, err := FindTask(root, d.ID); err == nil {
			t.Errorf(":d should DELETE %s, but it still exists as %s", d.ID, got.State)
		}
		if _, err := os.Stat(d.Dir); !os.IsNotExist(err) {
			t.Errorf(":d should remove %s from disk (stat err=%v)", d.Dir, err)
		}
	}
	if !strings.Contains(out.String(), "this can't be undone") {
		t.Errorf("delete confirm should warn it can't be undone:\n%s", out.String())
	}
	index, err = ReadCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	record := index.Windows[windowID]
	if len(record.Baseline) != 0 || len(record.ReviewSubjects) != 0 {
		t.Fatalf(":d retained deleted tasks in its stale completion window: %#v", record)
	}
	if err := ReconcileCompletionWindows([]string{root}); err != nil {
		t.Fatalf("restart reconciliation after :d: %v", err)
	}
}

// runDecisionBrowser: a declined :d confirm (a bare Enter defaults to No) is a safe no-op — the
// task stays blocked on disk and the browser stays on it.
func TestRunDecisionBrowserDeleteDeclined(t *testing.T) {
	root := t.TempDir()
	if code, err := tasksFolderAdd(root, []string{"alpha"}, StateTodo, "tasks add"); code != 0 || err != nil {
		t.Fatalf("add: code=%d err=%v", code, err)
	}
	it := ReadTaskTree(root)[0]
	if code, err := tasksFolderBlock(root, []string{it.ID}); code != 0 || err != nil {
		t.Fatalf("block: code=%d err=%v", code, err)
	}
	var decisions []Item
	for _, d := range ReadTaskTree(root) {
		if d.State == StateBlocked {
			decisions = append(decisions, d)
		}
	}
	in := strings.NewReader(":d\n\n:q\n") // :d, then a bare Enter declines (default No), then quit
	var out bytes.Buffer
	if code, err := runDecisionBrowser(decisionRefs(root, "", decisions), in, &out); code != 0 || err != nil {
		t.Fatalf("browser: code=%d err=%v", code, err)
	}
	got, err := FindTask(root, decisions[0].ID)
	if err != nil || got.State != StateBlocked {
		t.Fatalf("declined :d should leave the task blocked, got %v (err %v)", got.State, err)
	}
}

// TestRunDecisionBrowserSpansQueues: one browser session walks decisions from SEVERAL queues —
// each ref carries its own root (the answer moves the task within the right queue) and a label
// naming the queue in the header, so a monorepo answers everything in one sitting.
func TestRunDecisionBrowserSpansQueues(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	var refs []decisionRef
	for _, q := range []struct{ root, label, title string }{
		{rootA, "a/.agent/tasks", "alpha"},
		{rootB, "b/.agent/tasks", "beta"},
	} {
		if code, err := tasksFolderAdd(q.root, []string{q.title}, StateTodo, "tasks add"); code != 0 || err != nil {
			t.Fatalf("add %s: code=%d err=%v", q.title, code, err)
		}
		it := ReadTaskTree(q.root)[0]
		if code, err := tasksFolderBlock(q.root, []string{it.ID}); code != 0 || err != nil {
			t.Fatalf("block %s: code=%d err=%v", it.ID, code, err)
		}
		refs = append(refs, decisionRef{root: q.root, label: q.label, id: it.ID})
	}
	in := strings.NewReader("go with A\ngo with B\n")
	var out bytes.Buffer
	if code, err := runDecisionBrowser(refs, in, &out); code != 0 || err != nil {
		t.Fatalf("browser: code=%d err=%v", code, err)
	}
	for i, root := range []string{rootA, rootB} {
		it, err := FindTask(root, refs[i].id)
		if err != nil || it.State != StateTodo {
			t.Errorf("queue %d: answered decision should be in todo, got %v (err %v)", i, it.State, err)
		}
	}
	for _, label := range []string{"a/.agent/tasks · ", "b/.agent/tasks · "} {
		if !strings.Contains(out.String(), label) {
			t.Errorf("browser header missing queue label %q:\n%s", label, out.String())
		}
	}
}

func TestDecisionsUnknownFlag(t *testing.T) {
	if code, err := tasksFolderDecisions(t.TempDir(), []string{"--bogus"}); code != 2 || err == nil {
		t.Errorf("unknown decisions flag should be a usage error (2), got (%d, %v)", code, err)
	}
}

// An id is a unique handle: re-adding a title whose id already exists in ANY state (e.g. a
// shipped task in 99_done/) must be rejected, not create a second folder that shadows the first.
func TestTasksFolderAddRejectsCrossStateCollision(t *testing.T) {
	root := t.TempDir()
	if code, err := tasksFolderAdd(root, []string{"redo me"}, StateTodo, "tasks add"); code != 0 || err != nil {
		t.Fatalf("add: code=%d err=%v", code, err)
	}
	id := ReadTaskTree(root)[0].ID
	if code, err := tasksFolderMove(root, []string{id}, StateDone, "done", "done"); code != 0 || err != nil {
		t.Fatalf("done: code=%d err=%v", code, err)
	}
	// Same title → same id, but it now lives in 99_done/ — the re-add must fail.
	if code, err := tasksFolderAdd(root, []string{"redo me"}, StateTodo, "tasks add"); code == 0 || err == nil {
		t.Fatalf("re-add of a shipped id should be rejected, got (%d, %v)", code, err)
	}
	items := ReadTaskTree(root)
	if len(items) != 1 || items[0].State != StateDone {
		t.Fatalf("collision must not create a duplicate id: %+v", items)
	}
}

// A move onto a destination that already holds the same id (a torn move / stray duplicate across
// states) must be a clean, actionable error — not a raw os.Rename "file exists" that strands the task.
func TestMoveTaskDirRefusesDuplicateDest(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateInProgress, "2026-01-01-x", "task.md"), "# x\n")
	writeTaskFile(t, filepath.Join(root, StateDone, "2026-01-01-x", "task.md"), "# x\n")
	// `done` resolves the in_progress copy (read-side dedup keeps earliest); moving it onto the
	// existing 99_done copy must surface a clean "already exists", not crash or strand.
	code, err := tasksFolderMove(root, []string{"2026-01-01-x"}, StateDone, "done", "done")
	if code == 0 || err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("move onto a duplicate dest = (%d, %v), want a clean 'already exists' error", code, err)
	}
}

// The multi-queue decisions roll-up: queues with no open decision are skipped (no bare
// banner over nothing), a blocked task's decision prints under its queue's banner, an
// all-clear across queues exits 0 with no stdout listing, and an unknown flag exits 2.
func TestTasksDecisionsRollup(t *testing.T) {
	repo := t.TempDir()
	rels := []string{"svc-a/.agent/tasks", "svc-b/.agent/tasks"}

	// Nothing exists yet: all-clear, exit 0, nothing on stdout (the note goes to stderr).
	out := captureStdout(t, func() {
		if code, err := tasksDecisionsAll(repo, rels, nil); code != 0 || err != nil {
			t.Errorf("empty rollup: code=%d err=%v", code, err)
		}
	})
	if strings.TrimSpace(out) != "" {
		t.Errorf("all-clear rollup should print no stdout listing, got:\n%s", out)
	}

	// svc-a has only a todo (no decisions); svc-b has a blocked task with a decision.
	writeTaskFile(t, filepath.Join(repo, "svc-a/.agent/tasks", StateTodo, "2026-01-01-plain", "task.md"), "# plain\n")
	writeTaskFile(t, filepath.Join(repo, "svc-b/.agent/tasks", StateBlocked, "2026-01-01-stuck", "task.md"), "# stuck\n")
	writeTaskFile(t, filepath.Join(repo, "svc-b/.agent/tasks", StateBlocked, "2026-01-01-stuck", "decision.md"),
		"# Decision: pick a database?\n\n**Recommendation:** A — boring wins\n")

	out = captureStdout(t, func() {
		if code, err := tasksDecisionsAll(repo, rels, nil); code != 0 || err != nil {
			t.Errorf("rollup: code=%d err=%v", code, err)
		}
	})
	if !strings.Contains(out, "svc-b/.agent/tasks") || !strings.Contains(out, "pick a database?") {
		t.Errorf("rollup should show svc-b's decision under its banner, got:\n%s", out)
	}
	if strings.Contains(out, "svc-a/.agent/tasks") {
		t.Errorf("a queue with no open decision must not print a banner, got:\n%s", out)
	}

	// The main user-facing failure path: an unknown flag is a usage error.
	if code, err := tasksDecisionsAll(repo, rels, []string{"--bogus"}); code != 2 || err == nil {
		t.Errorf("unknown flag = (%d, %v), want (2, error)", code, err)
	}
}

// decisionDivider is the interactive browser's between-decisions border. No-color must keep the
// stable "decision N of M · where" label (the roll-up/browser tests match on it and a pipe stays
// plain); the label and location always survive so a redirect is still readable.
func TestDecisionDividerPlain(t *testing.T) {
	got := decisionDivider(ui.Palette{}, 2, 7, "runner · 2026-01-02-foo")
	want := "── decision 2 of 7 · runner · 2026-01-02-foo ──"
	if got != want {
		t.Errorf("plain divider = %q, want %q", got, want)
	}
}
