package tasks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/ui"
)

// The task-flags feature is gone. Old flags.json files are inert historical data: nothing reads,
// writes or deletes them, so a stale or malformed one can no longer change what the board shows or
// stop a task from completing.
func TestRemovedTaskFlagsRecordsAreInert(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	root := filepath.Join(repo, TasksRoot)
	if err := ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	stale := taskForLease(t, root, StateInProgress, "stale")
	broken := taskForLease(t, root, StateInProgress, "broken")
	plain := taskForLease(t, root, StateInProgress, "plain")
	record := func(dir, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "flags.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	record(stale.Dir, `{"version":1,"host_surfaces":[{"path":".githooks/pre-commit","reason":"runs on git commit","commit":"deadbeef"}],"acknowledged":false}`)
	record(broken.Dir, "{not json")

	items := mustReadTaskTree(t, root)
	if len(items) != 3 {
		t.Fatalf("read %d tasks, want 3", len(items))
	}
	p := ui.For(os.Stdout)
	for _, item := range items {
		if markers := listMarkers(p, item); strings.Contains(markers, "runs on your machine") {
			t.Errorf("list markers for %s = %q; the flag marker is removed", item.ID, markers)
		}
	}
	for _, line := range tasksWatchFrame(nil, []mergedTask{{Item: items[0]}, {Item: items[1]}}, 0, 120) {
		if strings.Contains(line, "runs on your machine") {
			t.Errorf("watch frame carries a flag marker: %q", line)
		}
	}
	snapshot := ReadProjectSnapshot(repo, []string{root})
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "host_surfaces") {
		t.Errorf("the snapshot still carries the feature-only host_surfaces field:\n%s", encoded)
	}

	// Completing every task still works, writes no new record, and leaves the historical files alone.
	for _, task := range []Item{stale, broken, plain} {
		if err := CompleteTrustedTask(root, task); err != nil {
			t.Fatalf("complete %s: %v", task.ID, err)
		}
	}
	for _, id := range []string{"stale", "broken"} {
		if _, err := os.Stat(filepath.Join(root, StateDone, id, "flags.json")); err != nil {
			t.Errorf("%s lost its historical flags.json: %v", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, StateDone, "plain", "flags.json")); !os.IsNotExist(err) {
		t.Errorf("completion wrote a flags record for plain: %v", err)
	}
}

// The commands are gone from the parser and from the one verb list help and completion derive from.
func TestRemovedTaskFlagsCommandIsUnknown(t *testing.T) {
	if slices.Contains(TasksVerbs, "flags") {
		t.Fatal("TasksVerbs still offers 'flags' — completion and the suggester read this list")
	}
	repo := initRepo(t)
	root := filepath.Join(repo, TasksRoot)
	if err := ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	code, err := CmdTasksFolder(repo, root, []string{"flags"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), `Unknown command "coop tasks flags"`) {
		t.Fatalf("coop tasks flags = (%d, %v); want an unknown-command refusal", code, err)
	}
	if code, err := CmdTasksFolder(repo, root, []string{"flags", "--ack"}); code != 2 || err == nil {
		t.Fatalf("coop tasks flags --ack = (%d, %v); want an unknown-command refusal", code, err)
	}
}

// The generated reference and the README lost the row with the command.
func TestRemovedTaskFlagsLeavesNoDocumentation(t *testing.T) {
	for _, rel := range []string{"docs/cli.md", "docs/man/coop.1", "site/llms.txt", "README.md"} {
		data, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if strings.Contains(string(data), "tasks flags") {
			t.Errorf("%s still documents 'coop tasks flags'", rel)
		}
	}
}
