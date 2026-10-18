package loop

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/procharness"

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
	completeTrustedTask   = tasks.CompleteTrustedTask
	readAuditReopenRecord = tasks.ReadAuditReopenRecord
)

// openLeaseAuthority opens the task's lease authority file, which a test locks to stand in for
// another controller's lease.
func openLeaseAuthority(root, id string, create bool) (*os.File, error) {
	key, err := tasks.LeaseAuthorityKey(root, id)
	if err != nil {
		return nil, err
	}
	registry, err := tasks.OpenLeaseAuthorityRoot()
	if err != nil {
		return nil, err
	}
	defer registry.Close()
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	return registry.OpenFile(key+".lock", flags, 0o600)
}

func queueProgress(hosts []string) (tasks.TaskCounts, string) {
	counts, active, err := tasks.QueueProgress(hosts)
	if err != nil {
		panic(err)
	}
	return counts, active
}

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
	item, ok, err := tasks.CurrentTask(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("could not read task %s", id)
	}
	return item
}

// captureStderr returns whatever fn writes to os.Stderr (ui.Info/ui.Warn go there).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	out, err := procharness.CaptureOutput(fn, &os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// rts builds a claude rotation over the named accounts, one rung each.
func rts(creds ...string) *ladder.Rotation {
	ts := make([]agents.Target, len(creds))
	for i, c := range creds {
		ts[i] = agents.Target{Provider: "claude", Accounts: []string{c}}
	}
	return ladder.NewRotation(ts)
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
