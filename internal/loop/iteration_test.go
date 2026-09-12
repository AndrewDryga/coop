package loop

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
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
		false, false, []string{root}, completionWindowStrict, nil, true, io.Discard, nil, "setup failure", "", nil,
	)
	if code != 1 || !errors.Is(err, tasks.ErrCompletionWindowSetup) || windows != nil || output != "" || usage != nil {
		t.Fatalf("setup-failed iteration = code %d output %q usage %#v windows %#v err %v", code, output, usage, windows, err)
	}
	if classification.outcome != "process_failure" {
		t.Fatalf("setup-failed iteration outcome = %q, want process_failure", classification.outcome)
	}
}

func TestLoopIterationOptsIntoNarrationButNotProjectPortPublication(t *testing.T) {
	root := t.TempDir()
	var launched box.RunSpec
	c := &Control{
		cfg: &config.Config{}, net: newNetworkLog(),
		boxRun: func(spec box.RunSpec) (int, error) { launched = spec; return 0, nil },
	}
	_, _, _, _, windows, err := c.runIteration(
		context.Background(), t.TempDir(), "image", "codex", "", []string{"true"},
		false, false, []string{root}, completionWindowStrict, nil, true, io.Discard, nil, "test", "", nil,
	)
	if windows != nil {
		_ = windows.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	if !launched.Batch || !launched.LoopPresentation || launched.Serve {
		t.Fatalf("loop launch flags = Batch %t LoopPresentation %t Serve %t", launched.Batch, launched.LoopPresentation, launched.Serve)
	}
}

func TestLoopIdentityIsCapturedButRawTraceStaysRaw(t *testing.T) {
	repo, root := t.TempDir(), t.TempDir()
	events := "{\"type\":\"system\",\"subtype\":\"init\",\"model\":\"claude-opus-5\"}\n" +
		"{\"type\":\"result\",\"subtype\":\"success\",\"num_turns\":1}\n"
	var forkLog bytes.Buffer
	c := &Control{
		cfg: &config.Config{StreamTrace: true}, net: newNetworkLog(), runID: "run",
		boxRun: func(spec box.RunSpec) (int, error) {
			_, err := io.WriteString(spec.Stdout, events)
			return 0, err
		},
	}
	_, _, _, _, windows, err := c.runIteration(
		context.Background(), repo, "image", "claude", "", []string{"claude"},
		true, true, []string{root}, completionWindowStrict, nil, true, &forkLog, nil, "test", "", nil,
	)
	if windows != nil {
		_ = windows.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	traceDir := filepath.Join(repo, ".agent", "runs", "run.streams")
	raw, err := os.ReadFile(filepath.Join(traceDir, "01-claude.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != events {
		t.Fatalf("raw trace = %q, want untouched provider events", raw)
	}
	rendered, err := os.ReadFile(filepath.Join(traceDir, "01-claude.out"))
	if err != nil {
		t.Fatal(err)
	}
	for label, output := range map[string]string{"fork log": forkLog.String(), "rendered trace": string(rendered)} {
		if strings.Count(output, "Starting claude:claude-opus-5") != 1 || strings.Contains(output, "· using") {
			t.Fatalf("%s did not capture exactly one replacement identity: %q", label, output)
		}
	}
}
