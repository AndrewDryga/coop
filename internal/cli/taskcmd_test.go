package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Make COOP_EGRESS fail closed!": "make-coop-egress-fail-closed",
		"  Trim --- dashes  ":           "trim-dashes",
		"123 Go":                        "123-go",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
	// A long title is hard-capped to a clean ASCII slug — no "…" ellipsis in a path,
	// no dangling dash, ≤ 48 runes.
	long := slugify("Folder-mode fleet split: distribute task folders across forks and worktrees")
	if n := len([]rune(long)); n > 48 {
		t.Errorf("long slug %q is %d runes, want ≤ 48", long, n)
	}
	if strings.ContainsRune(long, '…') {
		t.Errorf("long slug must not contain an ellipsis: %q", long)
	}
	if strings.HasPrefix(long, "-") || strings.HasSuffix(long, "-") {
		t.Errorf("long slug has a dangling dash: %q", long)
	}
	if !strings.HasPrefix(long, "folder-mode-fleet-split") {
		t.Errorf("long slug lost its prefix: %q", long)
	}
}

func TestFindTask(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, stateTodo, "2026-01-01-alpha", "task.md"), "# a\n")
	writeTaskFile(t, filepath.Join(root, stateTodo, "2026-01-01-alpine", "task.md"), "# b\n")
	if _, err := findTask(root, "2026-01-01-alpha"); err != nil {
		t.Errorf("exact match: %v", err)
	}
	if _, err := findTask(root, "alpine"); err != nil {
		t.Errorf("unique substring 'alpine': %v", err)
	}
	if _, err := findTask(root, "alp"); err == nil {
		t.Errorf("ambiguous 'alp' should error")
	}
	if _, err := findTask(root, "zzz"); err == nil {
		t.Errorf("missing 'zzz' should error")
	}
}

func TestTasksFolderLifecycle(t *testing.T) {
	root := t.TempDir()

	if code, err := tasksFolderAdd(root, []string{"Make egress fail closed"}); code != 0 || err != nil {
		t.Fatalf("add: code=%d err=%v", code, err)
	}
	items := readTaskTree(root)
	if len(items) != 1 || items[0].State != stateTodo {
		t.Fatalf("after add: %+v", items)
	}
	id := items[0].ID
	if !strings.HasSuffix(id, "-make-egress-fail-closed") {
		t.Errorf("id slug = %q", id)
	}

	// claim via a substring of the id
	if code, err := tasksFolderMove(root, []string{"egress"}, stateInProgress, "claimed"); code != 0 || err != nil {
		t.Fatalf("claim: code=%d err=%v", code, err)
	}
	if got := readTaskTree(root)[0].State; got != stateInProgress {
		t.Fatalf("after claim, state = %s", got)
	}

	// block → moves to blocked/ and writes decision.md
	if code, err := tasksFolderBlock(root, []string{id}); code != 0 || err != nil {
		t.Fatalf("block: code=%d err=%v", code, err)
	}
	bt := readTaskTree(root)[0]
	if bt.State != stateBlocked || !bt.HasDecision {
		t.Fatalf("after block: %+v", bt)
	}
	if !fileExists(filepath.Join(root, stateBlocked, id, "decision.md")) {
		t.Error("decision.md not created on block")
	}

	// unblock → in_progress (decision.md rides along)
	if code, err := tasksFolderMove(root, []string{id}, stateInProgress, "unblocked"); code != 0 || err != nil {
		t.Fatalf("unblock: code=%d err=%v", code, err)
	}
	if readTaskTree(root)[0].State != stateInProgress {
		t.Fatal("after unblock, not in_progress")
	}

	// done → done/
	if code, err := tasksFolderMove(root, []string{id}, stateDone, "done"); code != 0 || err != nil {
		t.Fatalf("done: code=%d err=%v", code, err)
	}
	if readTaskTree(root)[0].State != stateDone {
		t.Fatal("after done, not done")
	}

	// no-op move when already in the target state
	if code, _ := tasksFolderMove(root, []string{id}, stateDone, "done"); code != 0 {
		t.Errorf("re-done should be a no-op (code 0), got %d", code)
	}

	// drop removes the folder
	if code, err := tasksFolderDrop(root, []string{id}); code != 0 || err != nil {
		t.Fatalf("drop: code=%d err=%v", code, err)
	}
	if len(readTaskTree(root)) != 0 {
		t.Fatal("after drop, tree not empty")
	}
}

func TestCmdTasksFolderDispatch(t *testing.T) {
	root := t.TempDir()
	// no sub-command (empty rest) must not panic and should list cleanly
	if code, err := cmdTasksFolder(root, root, nil); code != 0 || err != nil {
		t.Fatalf("cmdTasksFolder(nil): code=%d err=%v", code, err)
	}
	if code, err := cmdTasksFolder(root, root, []string{}); code != 0 || err != nil {
		t.Fatalf("cmdTasksFolder([]): code=%d err=%v", code, err)
	}
	// add then list through the dispatcher
	if code, err := cmdTasksFolder(root, root, []string{"add", "Hello world"}); code != 0 || err != nil {
		t.Fatalf("add via dispatch: code=%d err=%v", code, err)
	}
	if code, err := cmdTasksFolder(root, root, []string{"list"}); code != 0 || err != nil {
		t.Fatalf("list via dispatch: code=%d err=%v", code, err)
	}
	if code, _ := cmdTasksFolder(root, root, []string{"bogus"}); code != 2 {
		t.Errorf("unknown sub should return code 2, got %d", code)
	}
}

func TestTasksFolderSplitCommand(t *testing.T) {
	repo := t.TempDir()
	root := filepath.Join(repo, ".agent", "tasks")
	writeTaskFile(t, filepath.Join(root, stateTodo, "2026-01-01-a", "task.md"), "# a\n")
	writeTaskFile(t, filepath.Join(root, stateTodo, "2026-01-02-b", "task.md"), "# b\n")
	if code, err := tasksFolderSplit(repo, root, []string{"2"}); code != 0 || err != nil {
		t.Fatalf("split 2: code=%d err=%v", code, err)
	}
	if !isTaskDir(filepath.Join(repo, ".agent", "tasks.1")) || !isTaskDir(filepath.Join(repo, ".agent", "tasks.2")) {
		t.Error("split did not create both slice dirs")
	}
	if code, _ := tasksFolderSplit(repo, root, []string{"0"}); code != 2 {
		t.Errorf("split 0 should be a usage error (2), got %d", code)
	}
	if code, _ := tasksFolderSplit(repo, root, nil); code != 2 {
		t.Errorf("split with no n should be a usage error (2), got %d", code)
	}
}

func TestTasksFolderLint(t *testing.T) {
	// findings: blocked-without-decision, todo-with-decision, status field, missing acceptance
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, stateBlocked, "b1", "task.md"), "---\ntitle: B\n---\n# B\n**Acceptance criteria:** x\n")
	writeTaskFile(t, filepath.Join(root, stateTodo, "t1", "task.md"), "---\ntitle: T\nstatus: todo\n---\n# T\nno accept here\n")
	writeTaskFile(t, filepath.Join(root, stateTodo, "t2", "task.md"), "# T2\n**Acceptance criteria:** ok\n")
	writeTaskFile(t, filepath.Join(root, stateTodo, "t2", "decision.md"), "# Decision: ?\n")
	if code, err := tasksFolderLint(root); err != nil || code != 1 {
		t.Fatalf("lint with findings: code=%d err=%v (want 1)", code, err)
	}

	// clean tree
	clean := t.TempDir()
	writeTaskFile(t, filepath.Join(clean, stateTodo, "ok", "task.md"), "---\ntitle: OK\n---\n# OK\n**Acceptance criteria:** the gate is green\n")
	writeTaskFile(t, filepath.Join(clean, stateBlocked, "bk", "task.md"), "# BK\n**Acceptance criteria:** y\n")
	writeTaskFile(t, filepath.Join(clean, stateBlocked, "bk", "decision.md"), "# Decision: which?\n**Recommendation:** A\n")
	if code, err := tasksFolderLint(clean); err != nil || code != 0 {
		t.Fatalf("clean lint: code=%d err=%v (want 0)", code, err)
	}
}
