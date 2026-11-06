package tasks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
)

// The hint after a scaffolded add shows the command that fills the task in, with the task's own
// title and placeholders, never a canned example about some other task.
func TestAddHintUsesPlaceholders(t *testing.T) {
	out := captureStderr(t, func() {
		if code, err := tasksFolderAdd(t.TempDir(), []string{"Rotate the signing key"}, StateTodo, "tasks add"); code != 0 || err != nil {
			t.Fatalf("add = (%d, %v)", code, err)
		}
	})
	want := "  coop tasks add \"Rotate the signing key\" \\\n" +
		"    --context \"<context>\" \\\n    --acceptance \"<acceptance>\" \\\n    --approach \"<approach>\" \\\n    --subtask \"<subtask>\"\n"
	if !strings.Contains(out, want) || strings.Contains(out, "expired session") {
		t.Fatalf("add hint =\n%s\nwant it to contain\n%s", out, want)
	}
}

// A filter that matches nothing names the state the way a person writes it.
func TestEmptyFilterSaysInProgress(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-01-x", "task.md"), "# X\n")
	if out := captureStdout(t, func() { _, _ = tasksFolderList(root, false, StateInProgress) }); out != "No in-progress tasks.\n" {
		t.Fatalf("empty in-progress filter = %q", out)
	}
}

// With no task yet but ideas saved in the backlog, the listing says so and how to see them.
func TestEmptyListingNamesSavedIdeas(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"2026-01-01-idea", "2026-01-02-other"} {
		writeTaskFile(t, filepath.Join(root, StateBacklog, id, "task.md"), "# "+id+"\n")
	}
	want := "No tasks yet. The backlog holds 2 ideas.\n\n" +
		"  Create one: coop tasks add \"Describe the work\"\n" +
		"  See ideas:  coop backlog\n"
	if out := captureStdout(t, func() { _, _ = tasksFolderList(root, false) }); out != want {
		t.Fatalf("empty listing with a backlog =\n%q\nwant\n%q", out, want)
	}
}

// claim, block and unblock each leave a line in log.md, as park does, so the journal shows who took
// the task, what it waited on and how that was answered.
func TestClaimBlockAndUnblockJournalInTheLog(t *testing.T) {
	root := t.TempDir()
	if code, err := tasksFolderAdd(root, []string{"Pick storage", "--context", "c", "--acceptance", "a", "--approach", "p"}, StateTodo, "tasks add"); code != 0 || err != nil {
		t.Fatalf("add = (%d, %v)", code, err)
	}
	id := mustReadTaskTree(t, root)[0].ID
	steps := []struct {
		run  func() (int, error)
		want string
	}{
		{func() (int, error) {
			return tasksFolderMoveWith(root, []string{id}, StateInProgress, "claim", "claimed", claimOptions{actor: ClaimActor{Label: "codex"}})
		}, "- Claimed by codex\n"},
		{func() (int, error) {
			return tasksFolderBlock(root, []string{id, "--question", "Which store?", "--why", "W", "--option", "A — disk", "--recommendation", "A"})
		}, "- Blocked: Which store?\n"},
		{func() (int, error) { return tasksFolderUnblock("", root, []string{id, "Use", "disk"}) }, "- Unblocked: Use disk\n"},
	}
	for _, step := range steps {
		if code, err := step.run(); code != 0 || err != nil {
			t.Fatalf("step for %q = (%d, %v)", step.want, code, err)
		}
		dir := mustReadTaskTree(t, root)[0].Dir
		if log := readFileString(filepath.Join(dir, "log.md")); !strings.Contains(log, step.want) {
			t.Fatalf("log.md lacks %q:\n%s", step.want, log)
		}
	}
}

// The lease names the command it runs the way a shell would read it back, and the line that ends
// it pairs with the line that took it. A lease is not a claim, so the end says the task stays
// claimed, and never "Released", which reads as coop tasks release.
func TestLeaseOutputQuotesTheCommandAndPairsItsLines(t *testing.T) {
	t.Setenv("USER", "alice")
	root := t.TempDir()
	inProgressTask(t, root, "cmd")
	const stays = "  The task stays in progress, claimed by alice.\n"
	for _, tc := range []struct {
		command []string
		code    int
		end     string
	}{
		{[]string{"sh", "-c", "echo inside-lease; exit 0"}, 0, "✓ Lease on cmd ended: the command finished\n" + stays},
		{[]string{"sh", "-c", "exit 3"}, 3, "Lease on cmd ended: the command exited with status 3\n" + stays},
	} {
		out := captureStderr(t, func() {
			code, err := holdTaskLease(context.Background(), root, leaseRequest{id: "cmd", actor: ClaimActor{Label: "codex"}, command: tc.command}, time.Millisecond)
			if code != tc.code || err != nil {
				t.Fatalf("lease = (%d, %v), want exit %d", code, err, tc.code)
			}
		})
		start := "✓ Leased cmd as codex\n  Running: sh -c '" + tc.command[2] + "'\n"
		if !strings.HasPrefix(out, start) || !strings.HasSuffix(out, tc.end) {
			t.Fatalf("lease output =\n%s\nwant it to start with\n%sand end with\n%s", out, start, tc.end)
		}
	}
}

func TestShellJoinQuotesOnlyWhatAShellWouldSplit(t *testing.T) {
	for argv, want := range map[string]string{
		"make\x00check":            "make check",
		"go\x00test\x00./...":      "go test ./...",
		"sh\x00-c\x00echo it's ok": `sh -c 'echo it'\''s ok'`,
		"printf\x00":               "printf ''",
	} {
		if got := shellJoin(strings.Split(argv, "\x00")); got != want {
			t.Errorf("shellJoin(%q) = %s, want %s", argv, got, want)
		}
	}
}

// Without color the board's bars would be solid blocks with nothing to tell their states apart, so
// a pipe gets the counts alone; they and the JSON name states as a person reads them and as the
// JSON counts already do, never by folder.
func TestTaskWatchWithoutColorAndJSONUseStateNames(t *testing.T) {
	items := []Item{{ID: "a", Title: "Wire auth", State: StateInProgress}, {ID: "b", Title: "Ship", State: StateDone}}
	c, _ := TaskTreeCounts(items)
	frame := tasksWatchFrame(boardRead{sources: []watchSource{{label: ".agent/tasks", counts: c}, {label: "web/.agent/tasks", counts: c}}, merged: merge(items)}, 0, 80, 0)
	joined := strings.Join(frame, "\n")
	if strings.ContainsAny(joined, "█░") || !strings.Contains(frame[0], "1 stopped") || strings.Contains(joined, "in_progress") {
		t.Fatalf("a colorless board draws bars or folder names:\n%s", joined)
	}

	repo := t.TempDir()
	root := filepath.Join(repo, ".agent", "tasks")
	writeTaskFile(t, filepath.Join(root, StateInProgress, "2026-01-01-wire", "task.md"), "# Wire auth\n")
	data, err := json.Marshal(ReadProjectSnapshot(repo, []string{root}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"state":"in_progress"`) || strings.Contains(string(data), "10_in_progress\"") {
		t.Fatalf("watch --json names a task's state by its folder:\n%s", data)
	}
}

// In an umbrella project, add run inside a subproject's folder files into that subproject's queue;
// at the top it still asks which project.
func TestUmbrellaAddUsesTheSubprojectItRunsIn(t *testing.T) {
	repo := gitFolder(t)
	writeTaskFile(t, filepath.Join(repo, ".agent", "project.yaml"), "subprojects:\n  - web\n  - api\n")
	if err := os.MkdirAll(filepath.Join(repo, "web", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	if code, err := CmdTasks(Host{}, cfg, []string{"add", "Fix the form"}); code != 2 || err == nil || !strings.Contains(err.Error(), "--project") {
		t.Fatalf("add at the umbrella's top = (%d, %v), want the project choice", code, err)
	}
	t.Chdir(filepath.Join(repo, "web", "src"))
	if code, err := CmdTasks(Host{}, cfg, []string{"add", "Fix the form"}); code != 0 || err != nil {
		t.Fatalf("add inside web = (%d, %v)", code, err)
	}
	if items := mustReadTaskTree(t, filepath.Join(repo, "web", ".agent", "tasks")); len(items) != 1 || items[0].Title != "Fix the form" {
		t.Fatalf("web's queue = %+v", items)
	}
}

// Several queues list one after another with one blank line between them, each under its path
// and nothing else: no markdown-style "#", no repeated "Tasks" heading.
func TestMultiQueueListingSeparatesQueuesWithOneBlankLine(t *testing.T) {
	repo := t.TempDir()
	writeTaskFile(t, filepath.Join(repo, "a", ".agent", "tasks", StateTodo, "2026-01-01-a", "task.md"), "# A todo\n")
	writeTaskFile(t, filepath.Join(repo, "b", ".agent", "tasks", StateBlocked, "2026-01-02-b", "task.md"), "# B blocked\n")
	rels := []string{filepath.Join("a", ".agent", "tasks"), filepath.Join("b", ".agent", "tasks"), filepath.Join("c", ".agent", "tasks")}
	want := "a/.agent/tasks\n\nTODO · 1\n  A todo\n    2026-01-01-a\n\n" +
		"b/.agent/tasks\n\nBLOCKED · 1\n  B blocked\n    Needs your answer\n    2026-01-02-b\n\nAnswer blocked tasks: coop tasks decisions -i\n\n" +
		"c/.agent/tasks\n  (no task queue here yet)\n"
	if out := captureStdout(t, func() { _, _ = tasksListAll(repo, rels, nil) }); out != want {
		t.Fatalf("multi-queue listing =\n%q\nwant\n%q", out, want)
	}
}
