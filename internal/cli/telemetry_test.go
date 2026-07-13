package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

func TestBuildStageRecord(t *testing.T) {
	tgt := agents.Target{Provider: "codex", Model: "gpt-5.6-sol", Effort: "xhigh", Accounts: []string{"work"}}
	start := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Second)
	q := taskCounts{Todo: 2, Doing: 1, Done: 5}
	rec := buildStageRecord("run123", "work", "v4.0.0", tgt, start, end, 0, 3, 2, "abc123", "def456", q, []string{"t-1"}, []string{"t-2"})

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
	if len(rec.Finished) != 1 || rec.Finished[0] != "t-1" || len(rec.Untrailered) != 1 || rec.Untrailered[0] != "t-2" {
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
	for _, want := range []string{`"stage":"work"`, `"provider":"codex"`, `"reopened":2`, `"finished":["t-1"]`, `"untrailered":["t-2"]`, `"queue_done":5`} {
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
