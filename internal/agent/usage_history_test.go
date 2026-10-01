package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageNativeHistory(t *testing.T) {
	for _, tc := range []struct {
		provider, data                   string
		input, read, write, hour, output int64
	}{
		{"claude", `
{"type":"assistant","timestamp":"2026-09-30T10:00:00Z","requestId":"r","message":{"id":"m","model":"claude-sonnet-4-6","usage":{"input_tokens":20,"cache_read_input_tokens":100,"cache_creation_input_tokens":30,"output_tokens":1}}}
{"type":"assistant","timestamp":"2026-09-30T10:00:01Z","requestId":"r","message":{"id":"m","model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":20,"cache_read_input_tokens":100,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":20},"output_tokens":40}}}
`, 20, 100, 10, 20, 40},
		{"codex", `
{"timestamp":"2026-09-30T10:00:00Z","type":"turn_context","payload":{"turn_id":"t","model":"gpt-6.1-sol"}}
{"timestamp":"2026-09-30T10:00:01Z","type":"token_usage_record","payload":{"turn_id":"t","response_id":"r","usage":{"input_tokens":100,"cached_input_tokens":40,"cache_write_input_tokens":60,"output_tokens":10,"reasoning_output_tokens":5,"total_tokens":110}}}
{"timestamp":"2026-09-30T10:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":40,"cache_write_input_tokens":60,"output_tokens":10},"last_token_usage":{"input_tokens":100,"cached_input_tokens":40,"cache_write_input_tokens":60,"output_tokens":10}}}}
`, 0, 40, 60, 0, 10},
		{"gemini", `
{"sessionId":"s","projectHash":"p"}
{"id":"m","timestamp":"2026-09-30T10:00:00Z","type":"gemini","model":"gemini-2.5-pro","tokens":null}
{"$set":{"messages":[{"id":"m","timestamp":"2026-09-30T10:00:00Z","type":"gemini","model":"gemini-2.5-pro","tokens":{"input":100,"cached":60,"output":20,"thoughts":5,"tool":0,"total":125}}]}}
{"$rewindTo":"m"}
`, 40, 60, 0, 0, 25},
		{"grok", `{"sessionId":"s","session":{"inputTokens":9999},"turns":[{"endedAt":"2026-09-30T10:00:00Z","inputTokens":9999,"outputTokens":9999,"modelUsage":{"grok-4.3":{"inputTokens":1000,"cachedReadTokens":600,"cacheCreationTokens":100,"outputTokens":200,"reasoningTokens":50,"modelCalls":1}}}]}`, 300, 600, 100, 0, 200},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			ag, _ := Get(tc.provider)
			history, err := ag.Usage().ParseHistory(strings.NewReader(strings.TrimSpace(tc.data)))
			if err != nil || len(history.Events) != 1 {
				t.Fatalf("events=%d error=%v", len(history.Events), err)
			}
			e := history.Events[0]
			if e.Input != tc.input || e.Read != tc.read || e.Write != tc.write || e.Write1h != tc.hour || e.Output != tc.output {
				t.Fatalf("event=%+v", e)
			}
		})
	}
}

func TestUsageCodexLegacySnapshots(t *testing.T) {
	data := `
{"timestamp":"2026-08-01T10:00:00Z","type":"turn_context","payload":{"turn_id":"old","model":"gpt-5.3-codex"}}
{"timestamp":"2026-08-01T10:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10},"last_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10}}}}
{"timestamp":"2026-09-30T10:00:00Z","type":"turn_context","payload":{"turn_id":"new","model":"gpt-5.3-codex"}}
{"timestamp":"2026-09-30T10:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":150,"cached_input_tokens":50,"output_tokens":20},"last_token_usage":{"input_tokens":50,"cached_input_tokens":10,"output_tokens":10}}}}
{"timestamp":"2026-09-30T10:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":150,"cached_input_tokens":50,"output_tokens":20,"total_tokens":999999},"last_token_usage":{"input_tokens":0,"cached_input_tokens":0,"output_tokens":0,"total_tokens":999999}}}}
`
	history, err := codexUsageHistory(strings.NewReader(strings.TrimSpace(data)))
	if err != nil || len(history.Events) != 2 {
		t.Fatalf("events=%d error=%v", len(history.Events), err)
	}
	newest := history.Events[1]
	if newest.Input != 40 || newest.Read != 10 || newest.Output != 10 {
		t.Fatalf("cumulative history charged lifetime/fictional tokens: %+v", newest)
	}
}

func TestUsageHistoryScanExcludesChildrenAndLinks(t *testing.T) {
	profile := t.TempDir()
	parent := filepath.Join(profile, "sessions", "bucket", "parent")
	child := filepath.Join(profile, "sessions", "bucket", "child")
	data := `{"turns":[{"endedAt":"2026-09-30T10:00:00Z","modelUsage":{"grok-4.3":{"inputTokens":100,"outputTokens":20,"modelCalls":1}}}]}`
	mustWriteSessionHistory(t, filepath.Join(parent, "summary.json"), `{"session_kind":"interactive","parent_session_id":"older"}`)
	mustWriteSessionHistory(t, filepath.Join(parent, "usage.json"), data)
	mustWriteSessionHistory(t, filepath.Join(child, "summary.json"), `{"session_kind":"subagent_fork","parent_session_id":"parent"}`)
	mustWriteSessionHistory(t, filepath.Join(child, "usage.json"), data)
	outside := t.TempDir()
	mustWriteSessionHistory(t, filepath.Join(outside, "summary.json"), `{"session_kind":"interactive"}`)
	mustWriteSessionHistory(t, filepath.Join(outside, "usage.json"), data)
	if err := os.Symlink(outside, filepath.Join(profile, "sessions", "linked")); err != nil {
		t.Fatal(err)
	}
	history := ReadUsageHistory(context.Background(), profile, (grokAgent{}).Usage())
	if len(history.Events) != 1 || !history.Available {
		t.Fatalf("parent/child counted twice: %+v", history)
	}
	if err := os.Remove(filepath.Join(parent, "usage.json")); err != nil {
		t.Fatal(err)
	}
	history = ReadUsageHistory(context.Background(), profile, (grokAgent{}).Usage())
	if len(history.Events) != 1 || !history.Available || !history.Partial {
		t.Fatalf("orphaned child usage disappeared: %+v", history)
	}
}
