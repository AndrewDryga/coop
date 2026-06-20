package cli

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

const sampleQueue = `# .agent/TASKS.md — the work queue.
# [ ] todo   [w] claimed   [x] done   [B] blocked

## Example

- [E] an example, never counted
  - **Context:** illustrative only.

## Active

- [x] shipped one
  - **Context:** done already.
- [w] in flight
  - **Context:** being worked.
  - **Likely files:** a.go
  - **Implementation direction:** do the thing.
  - **Acceptance checks:** the gate.
- [ ] a well-shaped todo
  - **Context:** why.
  - **Likely files:** b.go
  - **Implementation direction:** boring approach.
  - **Acceptance checks:** test passes.
- [ ] a bare todo with no body
`

func TestParseTasksBlocksAndSections(t *testing.T) {
	tasks := parseTasks(sampleQueue)
	if len(tasks) != 5 { // E + x + w + 2×[ ]
		t.Fatalf("parsed %d tasks, want 5", len(tasks))
	}
	// The [E] example keeps its section; the bare body line is captured.
	if tasks[0].State != "E" || tasks[0].Section != "Example" {
		t.Errorf("task0 = %+v, want state E in section Example", tasks[0])
	}
	// A task's block carries its indented body, not just the title.
	var wip task
	for _, tk := range tasks {
		if tk.State == "w" {
			wip = tk
		}
	}
	if wip.Section != "Active" {
		t.Errorf("[w] task section = %q, want Active", wip.Section)
	}
	if !strings.Contains(wip.block(), "**Likely files:** a.go") {
		t.Errorf("[w] block lost its body:\n%s", wip.block())
	}
}

func TestSplitOpenTaskBlocksCarriesBodies(t *testing.T) {
	buckets := splitOpenTaskBlocks(sampleQueue, 2)
	// Two open [ ] tasks → one per bucket (round-robin), each a whole block.
	if len(buckets[0]) != 1 || len(buckets[1]) != 1 {
		t.Fatalf("bucket sizes = %d,%d, want 1,1", len(buckets[0]), len(buckets[1]))
	}
	all := strings.Join(append(buckets[0], buckets[1]...), "\n")
	if !strings.Contains(all, "**Implementation direction:** boring approach.") {
		t.Errorf("split dropped the task body:\n%s", all)
	}
	if strings.Contains(all, "[E]") || strings.Contains(all, "[x]") || strings.Contains(all, "[w]") {
		t.Errorf("split included a non-open task:\n%s", all)
	}
}

func TestTasksLint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "TASKS.md")
	// A stale [w], an unchecked task wrongly under ## Example, a malformed marker,
	// and a non-self-contained todo.
	queue := "## Example\n\n- [ ] sneaky real task in the example section\n" +
		"  - **Context:** x\n  - **Likely files:** y\n  - **Implementation direction:** z\n  - **Acceptance checks:** w\n\n" +
		"## Active\n\n- [w] half-done claim\n  - **Context:** c\n  - **Likely files:** f\n  - **Implementation direction:** d\n  - **Acceptance checks:** a\n" +
		"- [y] malformed marker here\n" +
		"- [ ] no body so not self-contained\n"
	if err := os.WriteFile(path, []byte(queue), 0o644); err != nil {
		t.Fatal(err)
	}
	code, err := tasksLint(path)
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Errorf("lint exit = %d, want 1 (findings present)", code)
	}
}

func TestTasksLintClean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "TASKS.md")
	clean := "## Active\n\n- [ ] a good task\n" +
		"  - **Context:** why it matters.\n  - **Likely files:** x.go\n" +
		"  - **Implementation direction:** the boring way.\n  - **Acceptance checks:** gate green.\n" +
		"- [x] already shipped, body not required\n"
	if err := os.WriteFile(path, []byte(clean), 0o644); err != nil {
		t.Fatal(err)
	}
	code, err := tasksLint(path)
	if err != nil || code != 0 {
		t.Errorf("clean queue lint = (%d, %v), want (0, nil)", code, err)
	}
}

func TestTasksAdd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "TASKS.md")
	if err := os.WriteFile(path, []byte("## Active\n\n- [x] existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, err := tasksAdd(path, []string{"do", "a", "new", "thing"}); err != nil || code != 0 {
		t.Fatalf("tasksAdd = (%d, %v)", code, err)
	}
	got := readFileString(path)
	if !strings.Contains(got, "- [ ] do a new thing") {
		t.Errorf("added task missing:\n%s", got)
	}
	// The appended task is an empty skeleton, so lint now flags it unshaped (exit 1) — the nudge
	// to fill it in, matching what `coop tasks add` tells you to do.
	if code, _ := tasksLint(path); code != 1 {
		t.Errorf("freshly added empty stub should lint unshaped (exit 1), got %d:\n%s", code, got)
	}
}

func TestExtractTasksFlags(t *testing.T) {
	flags, rest, err := extractTasksFlags([]string{"--tasks", "a", "list", "--tasks=b", "--debug"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(flags, []string{"a", "b"}) {
		t.Errorf("flags = %v, want [a b]", flags)
	}
	if !slices.Equal(rest, []string{"list", "--debug"}) {
		t.Errorf("rest = %v, want [list --debug]", rest)
	}

	// A value-bearing flag with no value is an error, not a silently-dropped flag.
	for _, bad := range [][]string{{"--tasks"}, {"list", "--tasks"}, {"--tasks="}, {"--tasks", "--debug"}} {
		if _, _, err := extractTasksFlags(bad); err == nil {
			t.Errorf("extractTasksFlags(%v) should error on a missing value", bad)
		}
	}
}

// TestFlagValue covers the shared value-bearing-flag parser: both forms, and the missing-value
// cases (trailing flag, flag-as-value, empty =).
func TestFlagValue(t *testing.T) {
	ok := func(args []string, wantVal string, wantN int) {
		t.Helper()
		v, n, found, err := flagValue(args, 0, "--f")
		if !found || err != nil || v != wantVal || n != wantN {
			t.Errorf("flagValue(%v) = (%q,%d,%v,%v), want (%q,%d,true,nil)", args, v, n, found, err, wantVal, wantN)
		}
	}
	ok([]string{"--f", "x"}, "x", 2)
	ok([]string{"--f=x"}, "x", 1)
	for _, bad := range [][]string{{"--f"}, {"--f", "-g"}, {"--f="}} {
		if _, _, found, err := flagValue(bad, 0, "--f"); !found || err == nil {
			t.Errorf("flagValue(%v) should be found with an error", bad)
		}
	}
	if _, _, found, _ := flagValue([]string{"other"}, 0, "--f"); found {
		t.Error("flagValue should not match an unrelated token")
	}
}

func TestTaskQueues(t *testing.T) {
	repo := t.TempDir()
	cfg := &config.Config{TasksFiles: []string{".agent/TASKS.md"}}

	// No flags → the configured default.
	if got, err := taskQueues(cfg, repo, nil); err != nil || !slices.Equal(got, []string{".agent/TASKS.md"}) {
		t.Fatalf("default = %v err %v", got, err)
	}
	// Relative flags → repo-relative, untouched.
	got, err := taskQueues(cfg, repo, []string{"portal/.agent/TASKS.md", "runner/.agent/TASKS.md"})
	if err != nil || !slices.Equal(got, []string{"portal/.agent/TASKS.md", "runner/.agent/TASKS.md"}) {
		t.Fatalf("relative = %v err %v", got, err)
	}
	// An absolute path inside the repo is relativized.
	abs := filepath.Join(repo, "mcp", ".agent", "TASKS.md")
	if got, err := taskQueues(cfg, repo, []string{abs}); err != nil || !slices.Equal(got, []string{filepath.Join("mcp", ".agent", "TASKS.md")}) {
		t.Fatalf("absolute = %v err %v", got, err)
	}
	// A path escaping the repo is rejected.
	if _, err := taskQueues(cfg, repo, []string{"../outside/TASKS.md"}); err == nil {
		t.Error("a path escaping the repo should error")
	}
}

func TestCmdTasksMultiAndArity(t *testing.T) {
	repo := t.TempDir()
	mk := func(rel string) {
		t.Helper()
		full := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(sampleQueue), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("portal/.agent/TASKS.md")
	mk("runner/.agent/TASKS.md")
	a := &app{cfg: &config.Config{RepoOverride: repo, TasksFiles: []string{".agent/TASKS.md"}}}

	// add and split target a single file — reject more than one --tasks.
	for _, sub := range []string{"add", "split"} {
		args := []string{"--tasks", "portal/.agent/TASKS.md", "--tasks", "runner/.agent/TASKS.md", sub, "x"}
		if code, err := a.cmdTasks(args); code != 2 || err == nil {
			t.Errorf("%s with two --tasks: code=%d err=%v, want 2 + error", sub, code, err)
		}
	}

	// list spans both files, each under its path header.
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code, err := a.cmdTasks([]string{"--tasks", "portal/.agent/TASKS.md", "--tasks", "runner/.agent/TASKS.md", "list"})
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if code != 0 || err != nil {
		t.Fatalf("list: code=%d err=%v", code, err)
	}
	for _, want := range []string{"portal/.agent/TASKS.md", "runner/.agent/TASKS.md"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("list output missing header %q:\n%s", want, out)
		}
	}
}

// lint must flag a task whose required sections are present but empty (the `coop tasks add` stub),
// while a filled task lints clean and the [E] example is exempt.
func TestTasksLintEmptySections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "TASKS.md")
	stub := "## Active\n\n- [ ] do a thing\n  - **Context:**\n  - **Likely files:**\n  - **Implementation direction:**\n  - **Acceptance checks:**\n"
	if err := os.WriteFile(path, []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _ := tasksLint(path); code != 1 {
		t.Errorf("empty stub: tasksLint = %d, want 1 (unshaped)", code)
	}
	filled := "## Active\n\n- [ ] do a thing\n  - **Context:** the problem and where it lives.\n  - **Likely files:** foo.go\n  - **Implementation direction:** the boring approach; what to avoid.\n  - **Acceptance checks:** make check green; a test proves it.\n"
	if err := os.WriteFile(path, []byte(filled), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, err := tasksLint(path); code != 0 || err != nil {
		t.Errorf("filled task: tasksLint = (%d, %v), want (0, nil)", code, err)
	}
}

// emptySections must not be fooled by the multi-word "Acceptance checks:" label (the word
// "checks" sits before the colon, so it isn't content).
func TestEmptySectionsAcceptanceLabel(t *testing.T) {
	tk := task{Lines: strings.Split("- [ ] x\n  - **Context:** c\n  - **Likely files:** f\n  - **Implementation direction:** d\n  - **Acceptance checks:**", "\n")}
	got := emptySections(tk)
	if len(got) != 1 || got[0] != "Acceptance" {
		t.Errorf("emptySections = %v, want [Acceptance] (the label word 'checks' is not content)", got)
	}
}

// `coop tasks split N` says the source is unchanged (so the loop isn't pointed at both source and
// slices, double-running tasks) and warns when there are fewer open tasks than requested slices.
func TestTasksSplitMessages(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, ".agent", "TASKS.md")
	if err := os.WriteFile(path, []byte("## Active\n\n- [ ] only one\n  - **Context:** c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	code, err := tasksSplit(repo, path, []string{"2"}) // 1 open task, asked for 2 slices
	_ = w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	if code != 0 || err != nil {
		t.Fatalf("tasksSplit = (%d, %v)", code, err)
	}
	s := string(out)
	if !strings.Contains(s, "only 1 open task") || !strings.Contains(s, "not the 2 requested") {
		t.Errorf("missing under-split warning:\n%s", s)
	}
	if !strings.Contains(s, "is unchanged") || !strings.Contains(s, "runs twice") {
		t.Errorf("missing original-vs-slices guidance:\n%s", s)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "- [ ] only one") {
		t.Error("source TASKS.md must be left unchanged")
	}
}
