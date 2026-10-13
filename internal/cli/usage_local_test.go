package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestUsageNativeOnlyAndExactAccountExclusion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("NO_COLOR", "1")
	data := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"requestId":"native","message":{"id":"m","model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":20}}}`, time.Now().UTC().Format(time.RFC3339Nano))
	usageFixtureFile(t, filepath.Join(home, ".claude", "projects", "p", "s.jsonl"), data)
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir(), BoxHome: t.TempDir(), RuntimeName: "must-not-run"}}
	out := captureStdout(t, func() {
		code, err := a.cmdUsage([]string{"claude"})
		if code != 0 || err != nil {
			t.Fatalf("%d %v", code, err)
		}
	})
	if !strings.Contains(out, "Unassigned local usage") || !strings.Contains(out, "Subtotal") || a.rtSet {
		t.Fatal(out)
	}
	if code, err := a.cmdUsage([]string{"claude@personal"}); code != 2 || err == nil {
		t.Fatalf("native history invented account: %d %v", code, err)
	}
}

func TestUsageNativeAndEditorDedupPreservesOwnership(t *testing.T) {
	e := agents.UsageEvent{ID: "request", Model: "gpt-6.1-sol", Time: time.Now(), Input: 100, Output: 10, WriteKnown: true, ContextKnown: true}
	makeRow := func(account string, shared, local bool) usageCredential {
		return usageCredential{provider: "codex", account: account, shared: shared, local: local, history: agents.UsageHistory{Available: true, Events: []agents.UsageEvent{e}}}
	}
	for _, tc := range []struct {
		name  string
		rows  []usageCredential
		owner int
	}{
		{"named", []usageCredential{makeRow("a", false, false), makeRow("editor", true, false), makeRow("native", true, true)}, 0},
		{"shared", []usageCredential{makeRow("editor", true, false), makeRow("native", true, true)}, 0},
		{"ambiguous", []usageCredential{makeRow("a", false, false), makeRow("b", false, false), makeRow("editor", true, false), makeRow("native", true, true)}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := deduplicateUsageCredentials(tc.rows)
			ag, _ := agents.Get("codex")
			priced := 0
			for i := range rows {
				rows[i].value = agents.ValueUsageHistory(rows[i].history, time.Now(), ag.Usage().Price)
				priced += rows[i].value.Priced
				if i != tc.owner && rows[i].value.Priced != 0 {
					t.Fatalf("duplicate owner %d", i)
				}
			}
			if priced != 1 {
				t.Fatalf("priced=%d", priced)
			}
		})
	}
}

func TestUsageNativeCopiesKeepBetterVersionAndDoNotInventSharedZero(t *testing.T) {
	e := agents.UsageEvent{ID: "r", Model: "gpt-6.1-sol", Time: time.Now(), Input: 100, Output: 10, WriteKnown: true, ContextKnown: true, Partial: true}
	final := e
	final.Partial = false
	final.Output = 20
	rows := deduplicateUsageCredentials([]usageCredential{
		{provider: "codex", account: "work", history: agents.UsageHistory{Available: true, Events: []agents.UsageEvent{e}}},
		{provider: "codex", account: "native", shared: true, local: true, history: agents.UsageHistory{Available: true, Events: []agents.UsageEvent{final}}},
		{provider: "claude", account: "native", shared: true, local: true, history: agents.UsageHistory{Available: true, Events: []agents.UsageEvent{final}}},
	})
	ag, _ := agents.Get("codex")
	value := agents.ValueUsageHistory(rows[0].history, time.Now(), ag.Usage().Price)
	if value.Priced != 1 || value.Partial || !rows[0].local || rows[1].history.Available || len(rows[2].history.Events) != 1 {
		t.Fatalf("%+v value=%+v", rows, value)
	}
}
