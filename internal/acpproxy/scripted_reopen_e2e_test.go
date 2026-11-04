package acpproxy_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestScriptedACPReopenedThreadReadsLikeALiveOne is a thread carried from claude to codex, reopened by
// a fresh coop acp that never saw the text it injected. Codex replays that text as the person's first
// message, split across chunks. The thread shows only what they typed, the agent did get the context,
// and each open hears the start message once, before the replay.
func TestScriptedACPReopenedThreadReadsLikeALiveOne(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := scriptedTempDir(t)
	coopBin := filepath.Join(tmp, "coop")
	fixtureBin := filepath.Join(tmp, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A preset that does not load is something every session's start message names, on a box with no network.
	broken := filepath.Join(repo, ".agent", "presets", "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "preset.yaml"), []byte("not: [valid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initialize := map[string]any{"method": "initialize", "result": map[string]any{
		"protocolVersion": 1, "agentCapabilities": map[string]any{"sessionCapabilities": map[string]any{"load": map[string]any{}, "close": map[string]any{}, "delete": map[string]any{}}}, "authMethods": []any{},
	}}
	planPath := filepath.Join(tmp, "plan.json")
	writePlan := func(codexReopen []any) {
		t.Helper()
		plan, err := json.Marshal(map[string]any{"providers": map[string]any{
			"claude": []any{
				[]any{
					initialize,
					map[string]any{"method": "session/new", "result": map[string]any{"sessionId": "A1", "configOptions": []any{}}},
					map[string]any{"method": "session/set_config_option", "result": map[string]any{"configOptions": []any{}}},
					map[string]any{"method": "session/prompt", "result": map[string]any{"stopReason": "end_turn"}},
				},
				[]any{initialize},
				[]any{initialize},
			},
			"codex": []any{
				[]any{
					initialize,
					map[string]any{"method": "session/new", "result": map[string]any{"sessionId": "C1", "configOptions": []any{}}},
					map[string]any{"method": "session/prompt", "result": map[string]any{"stopReason": "end_turn"}},
				},
				codexReopen,
				codexReopen,
			},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(planPath, plan, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writePlan([]any{initialize})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	first := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, planPath, "claude", "claude", "codex")
	if _, err := first.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, first.stderr.String())
	}
	if _, err := first.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}}); err != nil {
		t.Fatalf("session/new: %v\nstderr:\n%s", err, first.stderr.String())
	}
	text := func(words string) []any { return []any{map[string]any{"type": "text", "text": words}} }
	if _, err := first.client.req(ctx, "session/prompt", map[string]any{"sessionId": "A1", "prompt": text("earlier question")}); err != nil {
		t.Fatalf("first turn: %v\nstderr:\n%s", err, first.stderr.String())
	}
	switchScriptedProvider(t, ctx, first, "A1", "codex")
	if _, err := first.client.req(ctx, "session/prompt", map[string]any{"sessionId": "A1", "prompt": text("hi")}); err != nil {
		t.Fatalf("carried turn: %v\nstderr:\n%s", err, first.stderr.String())
	}
	first.stop(t)

	// The agent got the context, in the bytes the provider will later hand back.
	block := carriedBlockFromWire(t, filepath.Join(tmp, "fixture-state", "codex-0", "wire.jsonl"))
	if !strings.Contains(block, "earlier question") || !strings.HasSuffix(block, "--- end of carried context ---") {
		t.Fatalf("carried block = %q", block)
	}

	chunk := func(kind, words string) json.RawMessage {
		event, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
			"sessionId": "C1", "update": map[string]any{"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": words}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return event
	}
	load := map[string]any{
		"method": "session/load",
		"params": map[string]any{"cwd": repo, "mcpServers": []any{}, "sessionId": "C1"},
		"result": map[string]any{"sessionId": "C1", "configOptions": []any{}},
		"events": []any{
			chunk("user_message_chunk", block[:17]),
			chunk("user_message_chunk", block[17:len(block)-3]),
			chunk("user_message_chunk", block[len(block)-3:]+"hi"),
			chunk("agent_message_chunk", "the answer"),
		},
	}
	writePlan([]any{initialize, load})

	// Each open is a new coop acp, which has no memory of the block or of an earlier start message.
	for open := range 2 {
		proc := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, planPath, "claude", "claude", "codex")
		if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
			t.Fatalf("open %d initialize: %v\nstderr:\n%s", open, err, proc.stderr.String())
		}
		mark := proc.client.mark()
		response, err := proc.client.req(ctx, "session/load", map[string]any{"cwd": repo, "mcpServers": []any{}, "sessionId": "A1"})
		if err != nil || response["error"] != nil {
			t.Fatalf("open %d: %v %v\nstderr:\n%s\nwire:\n%s", open, err, response["error"], proc.stderr.String(), wireDump(proc.client.transcript()))
		}
		isNotice := func(frame wireFrame) bool { return strings.Contains(string(frame.Raw), "missing from the toolbar") }
		if _, _, err := proc.client.await(ctx, mark, isNotice); err != nil {
			t.Fatalf("open %d heard no start message: %v\nwire:\n%s", open, err, wireDump(proc.client.transcript()))
		}
		proc.stop(t)
		var thread []string
		for _, frame := range proc.client.transcript()[mark:] {
			params, _ := frame.Msg["params"].(map[string]any)
			update, _ := params["update"].(map[string]any)
			content, _ := update["content"].(map[string]any)
			switch words, _ := content["text"].(string); {
			case isNotice(frame):
				if params["sessionId"] != "A1" {
					t.Fatalf("open %d: start message in session %v", open, params["sessionId"])
				}
				thread = append(thread, "start message")
			case frame.Msg["method"] == nil && frame.Msg["id"] != nil:
				thread = append(thread, "answer")
			case words != "":
				thread = append(thread, update["sessionUpdate"].(string)+": "+words)
			}
		}
		got := strings.Join(thread, " | ")
		if want := "start message | user_message_chunk: hi | agent_message_chunk: the answer | answer"; got != want {
			t.Fatalf("open %d: the thread read\n%s\nwant\n%s", open, got, want)
		}
	}
}

// carriedBlockFromWire returns the first text block of the one prompt a box received.
func carriedBlockFromWire(t *testing.T, path string) string {
	t.Helper()
	wire, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(wire), "\n") {
		var entry struct {
			Frame struct {
				Method string `json:"method"`
				Params struct {
					Prompt []struct {
						Text string `json:"text"`
					} `json:"prompt"`
				} `json:"params"`
			} `json:"frame"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Frame.Method == "session/prompt" && len(entry.Frame.Params.Prompt) > 0 {
			return entry.Frame.Params.Prompt[0].Text
		}
	}
	t.Fatalf("no prompt in %s:\n%s", path, wire)
	return ""
}
