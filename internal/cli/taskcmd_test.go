package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	// An empty fragment must error, not substring-match every task.
	if _, err := findTask(root, ""); err == nil {
		t.Errorf("empty id should error, not match everything")
	}
}

// `coop tasks path <id>` prints the resolved folder (reusing findTask) so a hook or human can
// `cat "$(coop tasks path <id>)/task.md"`; absent/ambiguous ids error like the other id commands.
func TestTasksFolderPath(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, stateTodo, "2026-01-01-alpha")
	writeTaskFile(t, filepath.Join(dir, "task.md"), "# a\n")
	writeTaskFile(t, filepath.Join(root, stateTodo, "2026-01-01-alpine", "task.md"), "# b\n")

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
	if code, err := tasksFolderMove(root, []string{"egress"}, stateInProgress, "claim", "claimed"); code != 0 || err != nil {
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

	// unblock WITH an answer → todo (available again; the in_progress lock is taken by claim), the
	// resolved decision.md rides along. (A no-answer unblock of an unresolved decision is refused —
	// covered by TestUnblockRequiresResolution.)
	if code, err := tasksFolderUnblock(root, []string{id, "A — go with it"}); code != 0 || err != nil {
		t.Fatalf("unblock: code=%d err=%v", code, err)
	}
	if readTaskTree(root)[0].State != stateTodo {
		t.Fatal("after unblock, not back in todo")
	}
	// unblocking a non-blocked task is an error (it's in todo now), not a silent reopen.
	if code, err := tasksFolderUnblock(root, []string{id}); code == 0 || err == nil {
		t.Errorf("unblock of a non-blocked task should error, got (%d, %v)", code, err)
	}

	// done → done/
	if code, err := tasksFolderMove(root, []string{id}, stateDone, "done", "done"); code != 0 || err != nil {
		t.Fatalf("done: code=%d err=%v", code, err)
	}
	if readTaskTree(root)[0].State != stateDone {
		t.Fatal("after done, not done")
	}

	// no-op move when already in the target state
	if code, _ := tasksFolderMove(root, []string{id}, stateDone, "done", "done"); code != 0 {
		t.Errorf("re-done should be a no-op (code 0), got %d", code)
	}

	// remove deletes the folder (a manual, by-id removal); --yes skips the gate in this non-TTY test
	if code, err := tasksFolderRemove(root, []string{id, "--yes"}); code != 0 || err != nil {
		t.Fatalf("remove: code=%d err=%v", code, err)
	}
	if len(readTaskTree(root)) != 0 {
		t.Fatal("after remove, tree not empty")
	}
}

// moveTaskDir reports an actionable error, not a raw ENOENT, when the task's source folder
// vanished under it — a concurrent move to a different state won the race.
func TestMoveTaskDirSourceVanished(t *testing.T) {
	root := t.TempDir()
	ti := taskItem{ID: "2026-01-01-x", State: stateTodo, Dir: filepath.Join(root, stateTodo, "2026-01-01-x")}
	err := moveTaskDir(root, ti, stateInProgress) // source never created → vanished
	if err == nil || !strings.Contains(err.Error(), "changed state under us") {
		t.Errorf("moveTaskDir with a vanished source = %v, want an actionable 'changed state' error", err)
	}
}

// Without --yes and no TTY (the test env), a destructive rm refuses and preserves the target — and
// names WHAT it would remove (the resolved id, or the --all-done count) so it isn't a blind delete.
func TestTasksRemoveGate(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, stateTodo, "2026-01-01-keep", "task.md"), "# keep\n")
	// by-id (substring match): refuses, task survives, error names the resolved id.
	code, err := tasksFolderRemove(root, []string{"keep"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "2026-01-01-keep") {
		t.Fatalf("rm without --yes = (%d, %v), want (2, a refusal naming the resolved id)", code, err)
	}
	if len(readTaskTree(root)) != 1 {
		t.Fatal("a refused rm must not delete the task")
	}
	// --all-done: refuses with the blast-radius count; the done task survives.
	writeTaskFile(t, filepath.Join(root, stateDone, "2026-01-02-done", "task.md"), "# done\n")
	code, err = tasksFolderRemove(root, []string{"--all-done"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "1 done task") {
		t.Fatalf("rm --all-done without --yes = (%d, %v), want (2, a refusal naming the count)", code, err)
	}
	if countDone(root) != 1 {
		t.Error("a refused --all-done must not delete anything")
	}
}

// `coop tasks clear` is the bulk-delete idiom shared with `coop loop pool clear`: it clears the done
// archive (= `rm --all-done`), gated the same way — refuses without --yes in a non-TTY, deletes with it.
func TestTasksClear(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, stateDone, "d1", "task.md"), "# d\n")
	if code, err := cmdTasksFolder("", root, []string{"clear"}); code != 2 || err == nil {
		t.Fatalf("tasks clear without --yes = (%d, %v), want (2, gated)", code, err)
	}
	if countDone(root) != 1 {
		t.Error("a refused clear must not delete the done task")
	}
	if code, err := cmdTasksFolder("", root, []string{"clear", "--yes"}); code != 0 || err != nil {
		t.Fatalf("tasks clear --yes = (%d, %v), want (0, nil)", code, err)
	}
	if countDone(root) != 0 {
		t.Error("clear --yes should empty the done archive")
	}
}

func TestTasksFolderRemoveAllDone(t *testing.T) {
	root := t.TempDir()
	// two done tasks, one todo and one in_progress that must SURVIVE --all-done
	writeTaskFile(t, filepath.Join(root, stateDone, "2026-01-01-a", "task.md"), "# a\n")
	writeTaskFile(t, filepath.Join(root, stateDone, "2026-01-02-b", "task.md"), "# b\n")
	writeTaskFile(t, filepath.Join(root, stateTodo, "2026-01-03-c", "task.md"), "# c\n")
	writeTaskFile(t, filepath.Join(root, stateInProgress, "2026-01-04-d", "task.md"), "# d\n")

	if code, err := tasksFolderRemove(root, []string{"--all-done", "--yes"}); code != 0 || err != nil {
		t.Fatalf("remove --all-done: code=%d err=%v", code, err)
	}
	items := readTaskTree(root)
	if len(items) != 2 {
		t.Fatalf("after --all-done, want 2 tasks left (todo+in_progress), got %d", len(items))
	}
	for _, it := range items {
		if it.State == stateDone {
			t.Errorf("a done task survived --all-done: %s", it.ID)
		}
	}
	// A second run is a clean no-op (nothing done left), not an error.
	if code, err := tasksFolderRemove(root, []string{"--all-done"}); code != 0 || err != nil {
		t.Errorf("remove --all-done with no done tasks should be a no-op, got (%d, %v)", code, err)
	}
	// Bare `remove` (no id, no flag) is a usage error.
	if code, _ := tasksFolderRemove(root, nil); code != 2 {
		t.Errorf("remove with no args should be a usage error (2), got %d", code)
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
	if !isTaskDir(filepath.Join(repo, ".agent", "tasks.slice1")) || !isTaskDir(filepath.Join(repo, ".agent", "tasks.slice2")) {
		t.Error("split did not create both slice dirs")
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
	if err := unknownErr("tasks command", "watxh", tasksVerbs); !strings.Contains(err.Error(), `did you mean "watch"`) {
		t.Errorf("expected a watch suggestion, got: %v", err)
	}
	for _, s := range []string{"watch", "ls", "list", "remove", "decisions"} {
		if !isTasksSubcommand(s) {
			t.Errorf("isTasksSubcommand(%q) = false, want true", s)
		}
	}
	if isTasksSubcommand("bogus") {
		t.Error("isTasksSubcommand(bogus) = true, want false")
	}
	if isTasksSubcommand("start") { // v3: retired in favor of claim
		t.Error("isTasksSubcommand(start) = true, want false (retired)")
	}
}

func TestTasksFolderLint(t *testing.T) {
	// findings: blocked-without-decision, todo-with-decision, status field, missing acceptance
	root := t.TempDir()
	if err := scaffoldStateDirs(root); err != nil { // isolate the content findings from the missing-state-dir check
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(root, stateBlocked, "b1", "task.md"), "---\ntitle: B\n---\n# B\n**Acceptance criteria:** x\n")
	writeTaskFile(t, filepath.Join(root, stateTodo, "t1", "task.md"), "---\ntitle: T\nstatus: todo\n---\n# T\nno accept here\n")
	writeTaskFile(t, filepath.Join(root, stateTodo, "t2", "task.md"), "# T2\n**Acceptance criteria:** ok\n")
	writeTaskFile(t, filepath.Join(root, stateTodo, "t2", "decision.md"), "# Decision: ?\n")
	if code, err := tasksFolderLint(root); err != nil || code != 1 {
		t.Fatalf("lint with findings: code=%d err=%v (want 1)", code, err)
	}

	// clean tree — a complete task carries all three sections (Context / Acceptance criteria / Approach).
	clean := t.TempDir()
	if err := scaffoldStateDirs(clean); err != nil { // a real queue has all four state dirs (lint flags a tree missing any)
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(clean, stateTodo, "ok", "task.md"), "---\ntitle: OK\n---\n# OK\n**Context:** why\n**Acceptance criteria:** the gate is green\n**Approach:** do it\n")
	writeTaskFile(t, filepath.Join(clean, stateBlocked, "bk", "task.md"), "# BK\n**Context:** c\n**Acceptance criteria:** y\n**Approach:** a\n")
	writeTaskFile(t, filepath.Join(clean, stateBlocked, "bk", "decision.md"), "# Decision: which?\n**Recommendation:** A\n")
	if code, err := tasksFolderLint(clean); err != nil || code != 0 {
		t.Fatalf("clean lint: code=%d err=%v (want 0)", code, err)
	}
}

// A queue missing any state dir is a corruption trap: the in-box "move a folder between states"
// protocol would rename a task into the nonexistent dir (see scaffoldStateDirs). lint flags it (exit
// 1); scaffolding the four makes it clean.
func TestTasksFolderLintFlagsMissingStateDir(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, stateTodo, "t1", "task.md"),
		"# T\n**Context:** c\n**Acceptance criteria:** the gate is green\n**Approach:** a\n")
	// only 00_todo exists (a hand-made or pre-fix tree) — the other three are missing.
	if code, err := tasksFolderLint(root); err != nil || code != 1 {
		t.Fatalf("lint of a queue missing state dirs: code=%d err=%v (want 1)", code, err)
	}
	if err := scaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	if code, err := tasksFolderLint(root); code != 0 || err != nil {
		t.Errorf("after scaffolding all four state dirs, lint should be clean: code=%d err=%v", code, err)
	}
}

// `coop tasks add` seeds self-documenting task.md + log.md + state.md (but not decision.md,
// which would make a todo task lint-dirty), and the result is lint-clean out of the box.
func TestTasksFolderAddSeedsSelfDocumentingFiles(t *testing.T) {
	root := t.TempDir()
	if code, err := tasksFolderAdd(root, []string{"make egress fail closed"}); code != 0 || err != nil {
		t.Fatalf("add: code=%d err=%v", code, err)
	}
	items := readTaskTree(root)
	if len(items) != 1 {
		t.Fatalf("want 1 task, got %d", len(items))
	}
	dir := filepath.Join(root, stateTodo, items[0].ID)

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
		"--subtask", "add the cap", "--subtask", "test the failure path"})
	if code != 0 || err != nil {
		t.Fatalf("structured add: code=%d err=%v", code, err)
	}
	items := readTaskTree(root)
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
	if code, _ := tasksFolderAdd(root2, []string{"half", "--context", "only this"}); code != 2 {
		t.Errorf("partial structured flags should be a usage error (2), got %d", code)
	}
	if len(readTaskTree(root2)) != 0 {
		t.Error("a refused structured add must not create a task folder")
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
	writeTaskFile(t, filepath.Join(root, stateTodo, "2026-01-01-a", "task.md"), "# A\n\n## Subtasks\n- [ ] one\n- [x] two\n")
	out := captureStdout(t, func() { _, _ = tasksFolderList(root, false) })
	if !strings.Contains(out, "[1/2]") {
		t.Errorf("expected the [1/2] subtask marker:\n%s", out)
	}
	if !strings.Contains(out, "= subtasks") {
		t.Errorf("a task with subtasks should show the legend:\n%s", out)
	}
	// A task WITHOUT subtasks → no legend, so the common listing stays uncluttered.
	bare := t.TempDir()
	writeTaskFile(t, filepath.Join(bare, stateTodo, "2026-01-01-b", "task.md"), "# B\n\nno checkboxes here\n")
	out2 := captureStdout(t, func() { _, _ = tasksFolderList(bare, false) })
	if strings.Contains(out2, "= subtasks") {
		t.Errorf("a subtask-free listing must not show the legend:\n%s", out2)
	}
}

func TestTasksFolderListCapsDone(t *testing.T) {
	root := t.TempDir()
	for i := 1; i <= 7; i++ {
		writeTaskFile(t, filepath.Join(root, stateDone, fmt.Sprintf("2026-01-%02d-done%d", i, i), "task.md"), fmt.Sprintf("# Done task %d\n", i))
	}
	writeTaskFile(t, filepath.Join(root, stateTodo, "2026-02-01-live", "task.md"), "# Live work\n")

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

// unblock must not drop a task into todo with an UNRESOLVED decision.md — that's the exact state
// lint rejects ("unresolved decision.md but is todo"). With no inline answer and a placeholder
// Resolution it refuses (task stays blocked); an inline answer resolves it and unblocks lint-clean.
func TestUnblockRequiresResolution(t *testing.T) {
	root := t.TempDir()
	if err := scaffoldStateDirs(root); err != nil { // a real queue has all four state dirs (lint flags a tree missing any)
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(root, stateTodo, "2026-01-01-pick", "task.md"),
		"# Pick a backend\n\n**Context:** need a datastore\n**Acceptance criteria:** one is chosen and why is noted\n**Approach:** compare options\n")
	if code, err := tasksFolderBlock(root, []string{"pick"}); code != 0 || err != nil {
		t.Fatalf("block: %d %v", code, err)
	}
	// No answer + placeholder Resolution → refuse, stay blocked (don't create a lint-rejected todo).
	if code, err := tasksFolderUnblock(root, []string{"pick"}); code != 2 || err == nil {
		t.Fatalf("unblock with no resolution: got (%d, %v), want (2, err)", code, err)
	}
	if readTaskTree(root)[0].State != stateBlocked {
		t.Fatal("a refused unblock must leave the task blocked")
	}
	// With an inline answer → resolves the decision and unblocks to todo.
	if code, err := tasksFolderUnblock(root, []string{"pick", "Postgres"}); code != 0 || err != nil {
		t.Fatalf("unblock with answer: %d %v", code, err)
	}
	tk := readTaskTree(root)[0]
	if tk.State != stateTodo {
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
	if code, err := tasksFolderAdd(root, []string{"pick the database"}); code != 0 || err != nil {
		t.Fatalf("add: code=%d err=%v", code, err)
	}
	id := readTaskTree(root)[0].ID
	if code, err := tasksFolderBlock(root, []string{id}); code != 0 || err != nil {
		t.Fatalf("block: code=%d err=%v", code, err)
	}
	dec := readFileString(filepath.Join(root, stateBlocked, id, "decision.md"))
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
	if code, err := tasksFolderAdd(root, []string{"pick the db"}); code != 0 || err != nil {
		t.Fatalf("add: code=%d err=%v", code, err)
	}
	id := readTaskTree(root)[0].ID
	if code, err := tasksFolderBlock(root, []string{id}); code != 0 || err != nil {
		t.Fatalf("block: code=%d err=%v", code, err)
	}
	if code, err := tasksFolderUnblock(root, []string{id, "B", "—", "go", "SQLite"}); code != 0 || err != nil {
		t.Fatalf("unblock+answer: code=%d err=%v", code, err)
	}
	if readTaskTree(root)[0].State != stateTodo {
		t.Fatal("after unblock, not back in todo")
	}
	dec := readFileString(filepath.Join(root, stateTodo, id, "decision.md"))
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
		if code, err := tasksFolderAdd(root, []string{title}); code != 0 || err != nil {
			t.Fatalf("add %s: code=%d err=%v", title, code, err)
		}
	}
	for _, it := range readTaskTree(root) {
		if code, err := tasksFolderBlock(root, []string{it.ID}); code != 0 || err != nil {
			t.Fatalf("block %s: code=%d err=%v", it.ID, code, err)
		}
	}
	var decisions []taskItem
	for _, it := range readTaskTree(root) {
		if it.State == stateBlocked {
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
	if a, _ := findTask(root, decisions[0].ID); a.State != stateBlocked {
		t.Errorf("skipped decision should stay blocked, got %s", a.State)
	}
	b, err := findTask(root, decisions[1].ID)
	if err != nil || b.State != stateTodo {
		t.Fatalf("answered decision should be in todo, got %v (err %v)", b.State, err)
	}
	if dec := readFileString(filepath.Join(b.Dir, "decision.md")); !strings.Contains(dec, "**Resolution:** SQLite it is") {
		t.Errorf("answer not recorded into the answered decision:\n%s", dec)
	}
	if !strings.Contains(out.String(), "decision 1 of 2") {
		t.Errorf("browser output missing the position header:\n%s", out.String())
	}
}

// runDecisionBrowser: :d marks the current task done (99_done/) — a reason is optional since done is
// terminal. `:d <reason>` records the reason into decision.md first; a bare `:d` just moves it.
func TestRunDecisionBrowserMarkDone(t *testing.T) {
	root := t.TempDir()
	for _, title := range []string{"alpha", "beta"} {
		if code, err := tasksFolderAdd(root, []string{title}); code != 0 || err != nil {
			t.Fatalf("add %s: code=%d err=%v", title, code, err)
		}
	}
	for _, it := range readTaskTree(root) {
		if code, err := tasksFolderBlock(root, []string{it.ID}); code != 0 || err != nil {
			t.Fatalf("block %s: code=%d err=%v", it.ID, code, err)
		}
	}
	var decisions []taskItem
	for _, it := range readTaskTree(root) {
		if it.State == stateBlocked {
			decisions = append(decisions, it)
		}
	}
	if len(decisions) != 2 {
		t.Fatalf("want 2 blocked decisions, got %d", len(decisions))
	}
	in := strings.NewReader(":d already published\n:d\n") // first: reason recorded; second: bare :d
	var out bytes.Buffer
	if code, err := runDecisionBrowser(decisionRefs(root, "", decisions), in, &out); code != 0 || err != nil {
		t.Fatalf("browser: code=%d err=%v", code, err)
	}
	for _, d := range decisions {
		if got, err := findTask(root, d.ID); err != nil || got.State != stateDone {
			t.Errorf(":d should move %s to done, got %v (err %v)", d.ID, got.State, err)
		}
	}
	first, _ := findTask(root, decisions[0].ID)
	if dec := readFileString(filepath.Join(first.Dir, "decision.md")); !strings.Contains(dec, "**Resolution:** already published") {
		t.Errorf(":d <reason> should record the reason first:\n%s", dec)
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
		if code, err := tasksFolderAdd(q.root, []string{q.title}); code != 0 || err != nil {
			t.Fatalf("add %s: code=%d err=%v", q.title, code, err)
		}
		it := readTaskTree(q.root)[0]
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
		it, err := findTask(root, refs[i].id)
		if err != nil || it.State != stateTodo {
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
	if code, err := tasksFolderAdd(root, []string{"redo me"}); code != 0 || err != nil {
		t.Fatalf("add: code=%d err=%v", code, err)
	}
	id := readTaskTree(root)[0].ID
	if code, err := tasksFolderMove(root, []string{id}, stateDone, "done", "done"); code != 0 || err != nil {
		t.Fatalf("done: code=%d err=%v", code, err)
	}
	// Same title → same id, but it now lives in 99_done/ — the re-add must fail.
	if code, err := tasksFolderAdd(root, []string{"redo me"}); code == 0 || err == nil {
		t.Fatalf("re-add of a shipped id should be rejected, got (%d, %v)", code, err)
	}
	items := readTaskTree(root)
	if len(items) != 1 || items[0].State != stateDone {
		t.Fatalf("collision must not create a duplicate id: %+v", items)
	}
}

// A move onto a destination that already holds the same id (a torn move / stray duplicate across
// states) must be a clean, actionable error — not a raw os.Rename "file exists" that strands the task.
func TestMoveTaskDirRefusesDuplicateDest(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, stateInProgress, "2026-01-01-x", "task.md"), "# x\n")
	writeTaskFile(t, filepath.Join(root, stateDone, "2026-01-01-x", "task.md"), "# x\n")
	// `done` resolves the in_progress copy (read-side dedup keeps earliest); moving it onto the
	// existing 99_done copy must surface a clean "already exists", not crash or strand.
	code, err := tasksFolderMove(root, []string{"2026-01-01-x"}, stateDone, "done", "done")
	if code == 0 || err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("move onto a duplicate dest = (%d, %v), want a clean 'already exists' error", code, err)
	}
}
