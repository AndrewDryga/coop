package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture is the live worker's rollout of 2026-09-28 06:31 (native session
// 01a0e6b5-bd79-73d0-a1a5-2ccb9618e573), reduced to its task and token_count
// events: a task of 23 calls in a fresh process, then one of 10 calls in the
// same process, whose running total carried the first task's.
const codexRolloutFixture = "testdata/codex-rollout-warm-tasks.jsonl"

func codexFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(codexRolloutFixture)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// codex-acp answered each of these tasks with its last call alone: 93k input
// and 737 output tokens for a turn that used 1.79M and 4,889.
func TestCodexRolloutCountsEveryCallOfTheLastTask(t *testing.T) {
	data := codexFixture(t)

	whole, last, ok := codexRolloutTurn(bytes.NewReader(data))
	if !ok {
		t.Fatal("no turn in the rollout")
	}
	if want := (TurnTokens{Input: 1_020_300, Cached: 1_005_440, Output: 1_900, Reasoning: 99}); whole != want {
		t.Fatalf("the warm task counted %+v, want %+v", whole, want)
	}
	if last.Input != 106_085 || last.Output != 358 {
		t.Fatalf("last call %+v", last)
	}

	// The same process's first task alone: a fresh total, counted whole.
	lines := strings.SplitAfter(string(data), "\n")
	second := 0
	for index, line := range lines {
		if strings.Contains(line, `"task_started"`) && index > 1 {
			second = index
		}
	}
	whole, last, ok = codexRolloutTurn(strings.NewReader(strings.Join(lines[:second], "")))
	if !ok {
		t.Fatal("no first task")
	}
	if want := (TurnTokens{Input: 1_790_611, Cached: 1_705_600, Output: 4_889, Reasoning: 69}); whole != want {
		t.Fatalf("the fresh task counted %+v, want %+v", whole, want)
	}
	if last.Input != 92_975 || last.Output != 737 {
		t.Fatalf("last call %+v", last)
	}
}

// A tool's output can be far longer than a line buffer; it must not end the
// count or hide the calls after it.
func TestCodexRolloutSkipsALongLine(t *testing.T) {
	data := codexFixture(t)
	long := `{"type":"response_item","payload":{"type":"function_call_output","output":"` +
		strings.Repeat("x", 300_000) + `"}}` + "\n"
	cut := bytes.LastIndex(data, []byte(`"token_count"`))
	cut = bytes.LastIndexByte(data[:cut], '\n') + 1
	withLong := append(append(append([]byte{}, data[:cut]...), long...), data[cut:]...)

	whole, _, ok := codexRolloutTurn(bytes.NewReader(withLong))
	if !ok || whole.Input != 1_020_300 || whole.Output != 1_900 {
		t.Fatalf("a long line changed the count: %+v %v", whole, ok)
	}
}

// Only the selected complete home is searched, independent of the active account.
func TestCodexFindsASessionsRolloutInItsNativeHome(t *testing.T) {
	root := filepath.Join(t.TempDir(), "codex", "acp-homes", "repository-key", "home")
	native := "01a0e6b5-bd79-73d0-a1a5-2ccb9618e573"
	dir := filepath.Join(root, "sessions", "2026", "09", "28")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-09-28T06-31-05-"+native+".jsonl"), codexFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	record, ok := Agent(codexAgent{}).(TurnRecord)
	if !ok {
		t.Fatal("codex keeps no turn record")
	}
	whole, last, ok := record.LastTurnTokens(root, native)
	if !ok || whole.Input != 1_020_300 || last.Input != 106_085 {
		t.Fatalf("read %+v %+v %v", whole, last, ok)
	}
	for _, missing := range [][2]string{
		{filepath.Join(root, "other"), native},
		{root, "01a0e6b5-0000-0000-0000-000000000000"},
		{"", native},
		{"..", native},
		{root, "*"},
	} {
		if _, _, ok := record.LastTurnTokens(missing[0], missing[1]); ok {
			t.Errorf("read a record for %q", missing)
		}
	}
}
