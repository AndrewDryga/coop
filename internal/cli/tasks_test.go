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
	// The appended task is well-shaped, so it lints clean.
	if code, _ := tasksLint(path); code != 0 {
		t.Errorf("freshly added task does not lint clean (exit %d):\n%s", code, got)
	}
}

func TestExtractTasksFlags(t *testing.T) {
	flags, rest := extractTasksFlags([]string{"--tasks", "a", "list", "--tasks=b", "--debug"})
	if !slices.Equal(flags, []string{"a", "b"}) {
		t.Errorf("flags = %v, want [a b]", flags)
	}
	if !slices.Equal(rest, []string{"list", "--debug"}) {
		t.Errorf("rest = %v, want [list --debug]", rest)
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
