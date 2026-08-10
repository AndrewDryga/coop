package loop

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/tasks"
)

// Test-only fixtures for the engine's own suite: the folder-task state aliases and the seeding
// helpers. internal/cli's integration_test.go and internal/forkctl/testhelpers_test.go keep the
// same set, for the same reason the production leaves in util.go are redeclared — a `writeTaskFile`
// is a fixture, not an API, and exporting one would make a test file a package's dependency.

const (
	stateTodo       = tasks.StateTodo
	stateInProgress = tasks.StateInProgress
	stateBlocked    = tasks.StateBlocked
	stateDone       = tasks.StateDone
)

type taskItem = tasks.Item

var (
	moveTaskDir           = tasks.MoveTaskDir
	queueProgress         = tasks.QueueProgress
	completeTrustedTask   = tasks.CompleteTrustedTask
	readAuditReopenRecord = tasks.ReadAuditReopenRecord
	openLeaseAuthority    = tasks.OpenLeaseAuthority
)

const tasksRoot = tasks.TasksRoot

// writeTaskFile creates path (making its parent dirs) with content — seeds a task folder as a
// fixture without going through the real `coop tasks add` dispatcher.
func writeTaskFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// taskForLease writes a fresh task folder in the given state and reads it back.
func taskForLease(t *testing.T, root, state, id string) tasks.Item {
	t.Helper()
	writeTaskFile(t, filepath.Join(root, state, id, "task.md"), "# "+id+"\n")
	item, ok := tasks.CurrentTask(root, id)
	if !ok {
		t.Fatalf("could not read task %s", id)
	}
	return item
}

// captureStderr returns whatever fn writes to os.Stderr (ui.Info/ui.Warn go there).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	return string(out)
}

// rts builds a claude rotation over the named accounts, one rung each.
func rts(creds ...string) *ladder.Rotation {
	ts := make([]agents.Target, len(creds))
	for i, c := range creds {
		ts[i] = agents.Target{Provider: "claude", Accounts: []string{c}}
	}
	return ladder.NewRotation(ts)
}
