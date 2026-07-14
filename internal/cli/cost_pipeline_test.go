package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// TestCostPipelineE2E exercises the whole cost/token pipeline end to end — a real-shaped claude
// result event → the stream decoder's tally → a stage record → the telemetry file on disk → the
// read-back → the aggregation → the closing digest — for a two-model (claude work + codex signoff)
// run. It covers every link except the live box subprocess, which only pipes the bytes the decoder
// already parses correctly (proven on real output by the live check-secrets run and
// TestStreamDecoderResultUsage; codex's turn.completed usage shape was verified against a live box).
func TestCostPipelineE2E(t *testing.T) {
	repo, run := t.TempDir(), "e2e"

	// 1. A claude work stage: feed a real-shaped claude result event through the decoder and take
	//    the tally it captured — exactly what runIteration hands recordStage.
	var out, tail bytes.Buffer
	dec := newStreamDecoder(&out, &tail, "claude", "personal_backup", "")
	_, _ = dec.Write([]byte(`{"type":"result","subtype":"success","num_turns":42,"duration_ms":600000,"total_cost_usd":12.31,"usage":{"input_tokens":40000,"cache_creation_input_tokens":200000,"cache_read_input_tokens":1000000,"output_tokens":48000}}` + "\n"))
	dec.flush()
	if dec.last == nil {
		t.Fatal("decoder captured no tally from the result event")
	}
	work := buildStageRecord(run, "work", "test", agents.Target{Provider: "claude", Model: "claude-fable-5"},
		time.Now(), time.Now(), 0, 0, 0, "h0", "h1", taskCounts{}, []string{"my-task"}, nil)
	work.CostUSD, work.InTok, work.OutTok = dec.last.CostUSD, dec.last.InTok, dec.last.OutTok
	if err := appendStageRecord(repo, run, work); err != nil {
		t.Fatal(err)
	}

	// 2. A codex signoff stage on a different model, with its own cost.
	sign := buildStageRecord(run, "signoff", "test", agents.Target{Provider: "codex", Model: "gpt-5.6-terra"},
		time.Now(), time.Now(), 0, 0, 0, "h1", "h1", taskCounts{}, nil, nil)
	sign.CostUSD, sign.InTok, sign.OutTok = 4.20, 90000, 3000
	if err := appendStageRecord(repo, run, sign); err != nil {
		t.Fatal(err)
	}

	// 3. Read the run back, aggregate, and render the digest the user sees.
	rc := costFromRecords(readStageRecords(repo, run))
	cs := loopChangeSet{
		tasks:      []taskChanges{{id: "my-task", commits: []commitInfo{{"a1", "do the thing"}}, files: []string{"internal/box/x.go"}}},
		subsystems: []string{"internal/box"},
	}
	d := cs.humanDigest(newLoopHealth(), nil, rc)
	t.Logf("rendered closing digest:\n%s", d)

	// Per-task cost (claude work, 1.24M in), run total (12.31+4.20), and the two-model split.
	for _, want := range []string{
		"do the thing", "$12.31", // the shipped task carries its cost
		"Cost:", "$16.51", "1.3M in / 51.0k out", // run total across both stages
		"by model:", "claude:claude-fable-5 $12.31", "codex:gpt-5.6-terra $4.20", // per-model
	} {
		if !strings.Contains(d, want) {
			t.Errorf("closing digest missing %q:\n%s", want, d)
		}
	}
}
