package loop

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/AndrewDryga/coop/internal/tasks"
)

// TestRunIterationStopsBeforeLaunchOnCompletionWindowSetupFailure and (in loop_test.go)
// TestLoopRejectsActionableDuplicateIDsAcrossQueues live HERE, not in internal/tasks, despite testing
// a completion-window/task-queue setup failure: their subject under test — runIteration/Run — is this
// package's own orchestration, which internal/tasks does not own and must not import back (see
// internal/importdag_test.go's invariant 1). They reach into internal/tasks only for exported setup
// primitives, the same shape internal/tasks's own refauthority_test.go staying tests use.
func TestRunIterationStopsBeforeLaunchOnCompletionWindowSetupFailure(t *testing.T) {
	root := t.TempDir()
	indexName, err := tasks.CompletionWindowIndexName(root)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tasks.OpenLeaseAuthorityRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.AtomicWriteTaskFile(registry, indexName, []byte("{not-json\n")); err != nil {
		_ = registry.Close()
		t.Fatal(err)
	}
	_ = registry.Close()

	c := &Control{}
	code, output, usage, classification, windows, err := c.runIteration(
		context.Background(), t.TempDir(), "must-not-launch", "codex", "", []string{"must-not-launch"},
		false, []string{root}, completionWindowStrict, nil, true, io.Discard, nil, "setup failure", "",
	)
	if code != 1 || !errors.Is(err, tasks.ErrCompletionWindowSetup) || windows != nil || output != "" || usage != nil {
		t.Fatalf("setup-failed iteration = code %d output %q usage %#v windows %#v err %v", code, output, usage, windows, err)
	}
	if classification.outcome != "process_failure" {
		t.Fatalf("setup-failed iteration outcome = %q, want process_failure", classification.outcome)
	}
}
