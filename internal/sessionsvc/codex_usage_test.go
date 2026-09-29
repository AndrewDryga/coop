package sessionsvc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
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
	if want := (codexTokenUsage{Input: 1_020_300, Cached: 1_005_440, Output: 1_900, Reasoning: 99}); whole != want {
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
	if want := (codexTokenUsage{Input: 1_790_611, Cached: 1_705_600, Output: 4_889, Reasoning: 69}); whole != want {
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

func TestCodexWholeTurnUsageTrustsARolloutOnlyOnceItHoldsTheReportedCall(t *testing.T) {
	profile := t.TempDir()
	native := "01a0e6b5-bd79-73d0-a1a5-2ccb9618e573"
	dir := filepath.Join(profile, "sessions", "2026", "09", "28")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-09-28T06-31-05-"+native+".jsonl"), codexFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	// What codex-acp reported for the turn: its last call, input net of cache.
	reported := session.Usage{InputTokens: 106_085 - 104_704, CachedInputTokens: 104_704, OutputTokens: 358}

	got := codexWholeTurnUsage(context.Background(), profile, native, reported)
	want := session.Usage{InputTokens: 1_020_300 - 1_005_440, CachedInputTokens: 1_005_440, OutputTokens: 1_900, ReasoningTokens: 99}
	if got != want {
		t.Fatalf("whole turn %+v, want %+v", got, want)
	}

	// A rollout that does not yet hold the reported call is behind the
	// adapter: what the adapter reported stands.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	later := session.Usage{InputTokens: 900, CachedInputTokens: 107_000, OutputTokens: 42}
	if got := codexWholeTurnUsage(ctx, profile, native, later); got != later {
		t.Fatalf("an unconfirmed rollout replaced the report: %+v", got)
	}

	// No rollout for the session, or no profile: the report stands.
	if got := codexWholeTurnUsage(context.Background(), profile, "01a0e6b5-0000-0000-0000-000000000000", reported); got != reported {
		t.Fatalf("a missing rollout changed the report: %+v", got)
	}
	if got := codexWholeTurnUsage(context.Background(), "", native, reported); got != reported {
		t.Fatalf("no profile changed the report: %+v", got)
	}
}
