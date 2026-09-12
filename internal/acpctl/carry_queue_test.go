package acpctl

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestACPGrokRunningPromptCarryEcho(t *testing.T) {
	c := newTestControl(t)
	preamble := "[coop] This thread continues a conversation\n\n--- conversation so far ---\nuser: remembered token\n"
	c.echoPreamble["s1"] = preamble
	queue := func(running string, entries []any) []byte {
		line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "_x.ai/queue/changed", "params": map[string]any{
			"sessionId": "s1", "runningText": running, "entries": entries, "version": 4, "meta": "keep",
		}})
		return append(line, '\n')
	}
	for _, tc := range []struct {
		name, running, want string
		entries             []any
	}{
		{"running", "before " + preamble + " after", "before  after", []any{}},
		{"running only", preamble, "", []any{}},
		{"queued only", "real active prompt", "real active prompt", []any{map[string]any{"kind": "prompt", "text": preamble}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, matched := c.filterPreambleEcho(queue(tc.running, tc.entries))
			if !matched || len(out) == 0 || strings.Contains(string(out), "[coop] This thread continues") {
				t.Fatalf("carry echo was not removed without dropping queue state: matched=%v out=%s", matched, out)
			}
			var message struct {
				Params struct {
					Running string `json:"runningText"`
					Entries []any  `json:"entries"`
					Version int    `json:"version"`
					Meta    string `json:"meta"`
				} `json:"params"`
			}
			if err := json.Unmarshal(out, &message); err != nil {
				t.Fatal(err)
			}
			p := message.Params
			if p.Running != tc.want || p.Entries == nil || len(p.Entries) != 0 || p.Version != 4 || p.Meta != "keep" {
				t.Fatalf("queue state changed: %+v", p)
			}
		})
	}
	nearMiss := queue(strings.Replace(preamble, "[coop]", "[user]", 1), []any{})
	if out, matched := c.filterPreambleEcho(nearMiss); matched || string(out) != string(nearMiss) {
		t.Fatal("unrelated running text was filtered")
	}
	c.promptSession["1"] = "s1"
	c.clearPreambleOnTerminal([]byte(`{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`))
	late := queue(preamble, []any{})
	if out, matched := c.filterPreambleEcho(late); matched || string(out) != string(late) {
		t.Fatal("completed prompt retained a stale queue filter")
	}
}
