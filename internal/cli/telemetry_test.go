package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

func TestBuildStageRecord(t *testing.T) {
	tgt := agents.Target{Provider: "codex", Model: "gpt-5.6-sol", Effort: "xhigh", Accounts: []string{"work"}}
	start := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Second)
	q := taskCounts{Todo: 2, Doing: 1, Done: 5}
	rec := buildStageRecord("run123", "work", "v4.0.0", tgt, start, end, 0, 3, 2, "abc123", "def456", q, []string{"t-1"}, []string{"t-2"}, []string{".claude/settings.json"})

	// The EFFECTIVE (post-rotation) target is flattened onto the record — provider included, so a
	// row shows what RAN, the whole point of the fix behind this telemetry.
	if rec.Provider != "codex" || rec.Model != "gpt-5.6-sol" || rec.Effort != "xhigh" || rec.Account != "work" {
		t.Errorf("target not mapped: %+v", rec)
	}
	if rec.Reopened != 2 || rec.Retries != 3 || rec.QueueTodo != 2 || rec.QueueDoing != 1 || rec.QueueDone != 5 {
		t.Errorf("outcome not mapped: %+v", rec)
	}
	if rec.HeadBefore != "abc123" || rec.HeadAfter != "def456" {
		t.Errorf("heads not mapped: %+v", rec)
	}
	if len(rec.Finished) != 1 || rec.Finished[0] != "t-1" || len(rec.Untrailered) != 1 || rec.Untrailered[0] != "t-2" || len(rec.GateFiles) != 1 || rec.GateFiles[0] != ".claude/settings.json" {
		t.Errorf("task evidence not mapped: %+v", rec)
	}
	if rec.Start != "2026-07-12T10:00:00Z" || rec.End != "2026-07-12T10:01:30Z" {
		t.Errorf("timestamps: %s / %s", rec.Start, rec.End)
	}
	// One JSON object per line, with the load-bearing keys.
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"stage":"work"`, `"provider":"codex"`, `"reopened":2`, `"finished":["t-1"]`, `"untrailered":["t-2"]`, `"gate_files":[".claude/settings.json"]`, `"queue_done":5`} {
		if !strings.Contains(string(line), want) {
			t.Errorf("json missing %s:\n%s", want, line)
		}
	}
}

func TestAppendStageRecord(t *testing.T) {
	repo := t.TempDir()
	rec := stageRecord{Run: "r1", Stage: "work", QueueTodo: 1}
	for i := 0; i < 2; i++ {
		if err := appendStageRecord(repo, "r1", rec); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	data, err := os.ReadFile(filepath.Join(repo, ".agent", "runs", "r1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// Two appends → two JSON-Lines rows, each a parseable object.
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 rows, got %d:\n%s", len(lines), data)
	}
	for _, l := range lines {
		var got stageRecord
		if err := json.Unmarshal([]byte(l), &got); err != nil {
			t.Errorf("row not valid JSON: %v\n%s", err, l)
		}
	}
}

func TestPreparePeerRecordFileIsPrivateAndFailClosed(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	path, err := preparePeerRecordFile(repo, "run1")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ok || stat.Nlink != 1 {
		t.Fatalf("peer record target = mode %s stat %#v", info.Mode(), stat)
	}
	if _, err := preparePeerRecordFile(repo, "run1"); err == nil {
		t.Fatal("existing peer record target was replaced")
	}
	removeEmptyPeerRecordFile(path)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("empty peer record target survived cleanup: %v", err)
	}

	outside := t.TempDir()
	if err := os.Remove(filepath.Join(repo, ".agent", "runs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".agent", "runs")); err != nil {
		t.Fatal(err)
	}
	if _, err := preparePeerRecordFile(repo, "run2"); err == nil {
		t.Fatal("symlinked peer record directory was accepted")
	}
}

func TestCostFromRecords(t *testing.T) {
	recs := []stageRecord{
		{Stage: "work", Provider: "claude", Model: "fable-5", CostUSD: 12.5, InTok: 1000, OutTok: 100, Finished: []string{"a"}},
		{Stage: "signoff", Provider: "codex", Model: "gpt-5.6-terra", CostUSD: 4.0, InTok: 500, OutTok: 50},
		{Stage: "verify", CostUSD: 0}, // no cost — contributes nothing
		{Stage: "work", Provider: "claude", Model: "fable-5", CostUSD: 2.0, InTok: 200, OutTok: 20, Finished: []string{"c", "d"}}, // split evenly
	}
	rc := costFromRecords(recs, []peerRecord{{Provider: "codex", Model: "gpt-5.6-terra", In: 1000, Out: 50}})
	if rc.total.usd != 18.5 {
		t.Errorf("total = %v, want 18.5", rc.total.usd)
	}
	if rc.byTask["a"].usd != 12.5 {
		t.Errorf("task a = %v, want 12.5", rc.byTask["a"].usd)
	}
	if c := rc.byTask["c"]; c.usd != 1.0 || c.inTok != 100 || c.outTok != 10 {
		t.Errorf("split task c = %+v, want usd 1.0 in 100 out 10", c)
	}
	if _, ok := rc.byTask[""]; ok {
		t.Error("a stage with no finished task must not create a task bucket")
	}
	// by-model: claude:fable-5 (12.5+2.0) leads codex:gpt-5.6-terra (4.0), sorted by cost desc.
	if len(rc.byModel) != 2 {
		t.Fatalf("byModel = %+v, want 2 models", rc.byModel)
	}
	if m := rc.byModel[0]; m.model != "claude:fable-5" || m.cost.usd != 14.5 || m.cost.inTok != 1200 {
		t.Errorf("byModel[0] = %+v, want claude:fable-5 usd 14.5 in 1200", m)
	}
	// codex:gpt-5.6-terra = the signoff stage $4.0 (500/50) + a token-only peer consult (1000/50).
	if m := rc.byModel[1]; m.model != "codex:gpt-5.6-terra" || m.cost.usd != 4.0 || m.cost.inTok != 1500 {
		t.Errorf("byModel[1] = %+v, want codex:gpt-5.6-terra usd 4.0 in 1500 (signoff+peer)", rc.byModel[1])
	}
}

// readStageRecords is best-effort: a missing run file yields nil, never an error, so the closing
// summary can't break the run's end.
func TestReadStageRecordsMissing(t *testing.T) {
	if recs := readStageRecords(t.TempDir(), "nope"); recs != nil {
		t.Errorf("missing run file should read nil, got %v", recs)
	}
}

// costForRepo sums every run under a clone (a fork loops across runs) — stage records from all
// *.jsonl plus peer rows from all *.peers.jsonl. A repo with no runs dir yields a zero cost.
func TestCostForRepo(t *testing.T) {
	repo := t.TempDir()
	if err := appendStageRecord(repo, "run1", stageRecord{Stage: "work", Provider: "claude", Model: "fable-5", CostUSD: 3.0, InTok: 100, OutTok: 10, Finished: []string{"t1"}}); err != nil {
		t.Fatal(err)
	}
	if err := appendStageRecord(repo, "run2", stageRecord{Stage: "work", Provider: "claude", Model: "fable-5", CostUSD: 2.0, InTok: 50, OutTok: 5, Finished: []string{"t2"}}); err != nil {
		t.Fatal(err)
	}
	peerLine, _ := json.Marshal(peerRecord{Run: "run2", Provider: "codex", Model: "gpt-5.6-terra", In: 1000, Out: 20})
	if err := os.WriteFile(filepath.Join(repo, ".agent", "runs", "run2.peers.jsonl"), append(peerLine, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	rc := costForRepo(repo)
	if rc.total.usd != 5.0 {
		t.Errorf("total cost = %v, want 5.0 (across both runs)", rc.total.usd)
	}
	if len(rc.byModel) != 2 {
		t.Fatalf("byModel = %+v, want claude + the codex peer", rc.byModel)
	}
	if rc.byModel[0].model != "claude:fable-5" || rc.byModel[0].cost.usd != 5.0 {
		t.Errorf("byModel[0] = %+v, want claude:fable-5 $5.0", rc.byModel[0])
	}
	if rc := costForRepo(t.TempDir()); rc.total.usd != 0 {
		t.Errorf("a clone with no runs dir = %v, want zero cost", rc.total.usd)
	}
}
