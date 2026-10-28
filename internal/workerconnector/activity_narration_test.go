package workerconnector

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// Ryker's timeline showed "Run command — the worker reported that a command
// ran, not which one" for every shell step of every turn, and nothing of what
// the model was thinking, because this boundary let only a tool's kind cross
// (Andrew, 2026-09-28: "workers must report commands, thinking and everything
// else"). The shapes below are the ones the live worker's Codex sessions record.
func TestPublicActivityNarratesCommandsTheirOutputAndThoughts(t *testing.T) {
	started := decodeNarration(t, "tool.started", `{"tool_call_id":"exec-1","kind":"execute","title":"Run git status","input":{"command":"git status --short","cwd":"/workspace"}}`)
	if started["title"] != "Run git status" {
		t.Fatalf("lost the title: %v", started)
	}
	if input, _ := started["input"].(map[string]any); input["command"] != "git status --short" || input["cwd"] != "/workspace" {
		t.Fatalf("lost the command: %v", started)
	}

	completed := decodeNarration(t, "tool.completed", `{"tool_call_id":"exec-1","kind":"execute","status":"completed","title":"Run git status","input":{"command":"git status --short","cwd":"/workspace"},"output":{"formatted_output":" M lib/a.go\n","exit_code":0},"content":[{"type":"terminal","terminalId":"t-1"}]}`)
	output, _ := completed["output"].(map[string]any)
	if output["formatted_output"] != " M lib/a.go\n" || output["exit_code"] != float64(0) {
		t.Fatalf("lost what the command printed: %v", completed)
	}

	for _, kind := range []string{"model.thought", "model.progress"} {
		if !operatorActivityEvent(kind) {
			t.Fatalf("%s does not reach the controller", kind)
		}
		if narrated := decodeNarration(t, kind, `{"text":"**Checking the deploy script**"}`); narrated["text"] != "**Checking the deploy script**" {
			t.Fatalf("%s lost its text: %v", kind, narrated)
		}
	}

	plan := decodeNarration(t, "model.plan", `{"entries":[{"content":"Read the deploy script","status":"completed","priority":"high"},{"content":"Fix the readiness probe","status":"in_progress"}]}`)
	entries, _ := plan["entries"].([]any)
	if plan["step_count"] != float64(2) || len(entries) != 2 || entries[1].(map[string]any)["content"] != "Fix the readiness probe" {
		t.Fatalf("lost the plan: %v", plan)
	}
}

// A command line or its output can carry a token: such a field is withheld
// whole, and the event says which field and why. A withheld tool input still
// names the tool, which the controller needs to match its own tool calls.
func TestPublicActivityWithholdsAFieldThatCarriesALikelySecret(t *testing.T) {
	token := "ghp_" + strings.Repeat("aB7C", 9)
	raw := `{"tool_call_id":"exec-2","kind":"execute","title":"Run curl","input":{"command":"curl -H 'Authorization: token ` + token + `' https://api.github.com/user"},"status":"completed","output":{"formatted_output":"{}","exit_code":0}}`
	for _, kind := range []string{"tool.started", "tool.completed"} {
		payload, ok := publicActivityPayload(kind, json.RawMessage(raw))
		if !ok || strings.Contains(string(payload), token) || strings.Contains(string(payload), "curl -H") {
			t.Fatalf("a likely secret crossed: %s", payload)
		}
		var narrated map[string]any
		_ = json.Unmarshal(payload, &narrated)
		withheld, _ := narrated["withheld"].(map[string]any)
		if withheld["input"] != "likely GitHub token" || narrated["title"] != "Run curl" {
			t.Fatalf("did not say what was withheld: %s", payload)
		}
	}

	mcp := decodeNarration(t, "tool.started", `{"tool_call_id":"mcp-1","kind":"other","input":{"server":"controller-tools","tool":"record_finding","arguments":{"what":"`+token+`"}}}`)
	if input, _ := mcp["input"].(map[string]any); input["server"] != "controller-tools" || input["tool"] != "record_finding" || input["arguments"] != nil {
		t.Fatalf("a withheld input lost its tool's name: %v", mcp)
	}

	thought := decodeNarration(t, "model.thought", `{"text":"The key is `+token+`"}`)
	if _, ok := thought["text"]; ok || thought["withheld"].(map[string]any)["text"] != "likely GitHub token" {
		t.Fatalf("a thought with a likely secret crossed: %v", thought)
	}
}

// A poll stops at the first event that does not fit its 512 KiB, so an event
// as large as a command's whole output would hold its session's stream still.
func TestPublicActivityBoundsWhatACommandPrinted(t *testing.T) {
	printed := strings.Repeat("line of build output\n", 20_000)
	encoded, _ := json.Marshal(map[string]any{
		"tool_call_id": "exec-3", "kind": "execute", "status": "completed", "title": "Run make",
		"input": map[string]any{"command": "make"}, "output": map[string]any{"formatted_output": printed, "exit_code": 2},
		"terminal_exit": map[string]any{"exit_code": 2, "signal": "SIGTERM", "ignored": "private"},
	})
	payload, ok := publicActivityPayload("tool.completed", encoded)
	if !ok || len(payload) > maximumNarrationEventBytes {
		t.Fatalf("an event of %d bytes crossed", len(payload))
	}
	var narrated map[string]any
	_ = json.Unmarshal(payload, &narrated)
	output, _ := narrated["output"].(map[string]any)
	if output["truncated"] != true || !strings.HasPrefix(output["preview"].(string), `{"exit_code":2,"formatted_output":"line of build output\n`) {
		t.Fatalf("a long output did not cross as a marked preview: %.300s", payload)
	}
	if terminal, _ := narrated["terminal_exit"].(map[string]any); len(terminal) != 2 || terminal["exit_code"] != float64(2) || terminal["signal"] != "SIGTERM" {
		t.Fatalf("truncated output lost typed terminal exit: %.300s", payload)
	}
}

func TestPublicActivityBoundsFinalEncoding(t *testing.T) {
	for _, text := range []string{strings.Repeat("<", 16_000), strings.Repeat("x", 16_300)} {
		encoded, err := encodeWireJSON(map[string]any{
			"tool_call_id": "bounded", "kind": "execute", "status": "completed",
			"input":  map[string]any{"server": "controller", "tool": "inspect", "arguments": text},
			"output": text, "content": text, "locations": text,
			"title":         strings.Repeat("<", maximumNarrationTitleBytes),
			"terminal_exit": map[string]any{"exit_code": 2},
		})
		if err != nil {
			t.Fatal(err)
		}
		payload, ok := publicActivityPayload("tool.completed", encoded)
		if !ok || len(payload) > maximumNarrationEventBytes {
			t.Fatalf("final encoding = %d bytes, accepted=%v; want bounded event", len(payload), ok)
		}
		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatal(err)
		}
		input, _ := event["input"].(map[string]any)
		terminal, _ := event["terminal_exit"].(map[string]any)
		if input["server"] != "controller" || input["tool"] != "inspect" || terminal["exit_code"] != float64(2) {
			t.Fatalf("bounded event lost tool identity or terminal status: %.500s", payload)
		}
		if text[0] == 'x' && event["withheld"] == nil {
			t.Fatal("oversized aggregate lost its withheld explanation")
		}
		if text[0] == '<' && event["withheld"] != nil {
			t.Fatal("bounded HTML-rich evidence was unnecessarily withheld")
		}
	}
}

func TestPublicActivityBoundsToolNamesAlongsidePathEvidence(t *testing.T) {
	paths := make([]any, 16)
	for i := range paths {
		paths[i] = map[string]any{"source": fmt.Sprintf("/locations/%d/path", i), "scope": "project", "path": fmt.Sprintf("%02d", i) + strings.Repeat("\x01", 510)}
	}
	raw, err := encodeWireJSON(map[string]any{
		"tool_call_id": "bounded-names", "kind": "execute", "status": "completed",
		"input":         map[string]any{"server": strings.Repeat("x", 16_300), "tool": "inspect"},
		"path_context":  map[string]any{"basis": "lexical", "paths": paths},
		"terminal_exit": map[string]any{"exit_code": 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := publicActivityPayload("tool.completed", raw)
	if !ok || len(payload) > maximumNarrationEventBytes {
		t.Fatalf("bounded event = %d bytes, accepted=%v", len(payload), ok)
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	input := event["input"].(map[string]any)
	if input["server"] != nil || input["tool"] != "inspect" || event["withheld"].(map[string]any)["input"] != "too large" {
		t.Fatalf("oversized name not withheld: %.500s", payload)
	}
	if event["tool_call_id"] != "bounded-names" || event["terminal_exit"].(map[string]any)["exit_code"] != float64(2) || len(event["path_context"].(map[string]any)["paths"].([]any)) != len(paths) {
		t.Fatal("bounded event lost tool, terminal, or path evidence")
	}
}

func TestPublicActivityBoundsNestedTextWithoutLosingEvidence(t *testing.T) {
	formatting := strings.Repeat("\x1b[0m", 256)
	entries := make([]any, maximumPlanSteps)
	for i := range entries {
		entries[i] = map[string]any{"content": formatting, "status": "completed", "priority": "high"}
	}
	text := strings.Repeat(formatting, 16)
	part := map[string]any{"kind": "text", "text": text, "sha256": sha256Text(text), "bytes": len(text)}
	for kind, source := range map[string]map[string]any{
		"model.plan":  {"entries": entries},
		"prompt.sent": {"version": 1, "state": "sent", "text_sha256": sha256Text(text + text), "text_bytes": len(text) * 2, "parts": []any{part, part}, "blocks": []any{}},
	} {
		t.Run(kind, func(t *testing.T) {
			raw, err := encodeWireJSON(source)
			if err != nil {
				t.Fatal(err)
			}
			payload, ok := publicActivityPayload(kind, raw)
			if !ok || len(payload) > maximumNarrationEventBytes {
				t.Fatalf("%s payload = %d bytes, accepted=%v", kind, len(payload), ok)
			}
			var value map[string]any
			if err := json.Unmarshal(payload, &value); err != nil {
				t.Fatal(err)
			}
			withheld := 0
			if kind == "model.plan" {
				steps := value["entries"].([]any)
				if len(steps) != len(entries) || value["step_count"] != float64(len(entries)) {
					t.Fatalf("lost plan steps: %v", value)
				}
				for _, raw := range steps {
					step := raw.(map[string]any)
					if step["status"] != "completed" || step["priority"] != "high" {
						t.Fatalf("lost plan evidence: %v", step)
					}
					if step["withheld"] == "too large" {
						withheld++
					} else if step["content"] != formatting {
						t.Fatal("changed retained plan text")
					}
				}
			} else {
				parts := value["parts"].([]any)
				if len(parts) != 2 || value["state"] != "sent" || value["text_sha256"] != sha256Text(text+text) {
					t.Fatal("lost prompt identity")
				}
				for _, raw := range parts {
					part := raw.(map[string]any)
					if part["sha256"] != sha256Text(text) || part["bytes"] != float64(len(text)) {
						t.Fatal("lost prompt part identity")
					}
					if part["kind"] == "withheld" && part["reason"] == "too large" {
						withheld++
					} else if part["text"] != text {
						t.Fatal("changed retained prompt text")
					}
				}
			}
			if withheld == 0 {
				t.Fatal("oversized nested text did not record withholding")
			}
		})
	}
}

func TestActivityEnvelopeKeepsBoundedWireEncoding(t *testing.T) {
	now := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)
	raw, err := encodeWireJSON(map[string]any{"tool_call_id": "html", "output": strings.Repeat("<", 16_000)})
	if err != nil {
		t.Fatal(err)
	}
	api := &eventAPI{events: []workerproto.SessionEvent{{ID: "event-1", SessionID: "coop-session-1", Sequence: 1, Type: "tool.completed", Version: 1, OccurredAt: now, Payload: raw}}}
	executor, err := NewExecutor(ExecutorConfig{API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.journal.bindEventStream(createCommand(now.Add(time.Minute)), "coop-session-1"); err != nil {
		t.Fatal(err)
	}
	batches, err := executor.collectActivity(context.Background(), maximumPollBytes)
	if err != nil || len(batches) != 1 || len(batches[0].Events) != 1 {
		t.Fatalf("activity = %+v, %v", batches, err)
	}
	envelope := batches[0].Events[0].Payload
	if len(envelope) > maximumNarrationEventBytes || strings.Contains(string(envelope), `\u003c`) {
		t.Fatalf("event envelope expanded bounded payload to %d bytes", len(envelope))
	}
	wire, err := encodeWireJSON(batches)
	if err != nil || strings.Contains(string(wire), `\u003c`) {
		t.Fatalf("poll encoding expanded activity: %v", err)
	}
}

func TestPublicActivityTerminalExitRejectsUntrustedFields(t *testing.T) {
	value := decodeNarration(t, "tool.completed", `{"terminal_exit":{"exit_code":1.5,"signal":"`+"ghp_"+strings.Repeat("aB7C", 9)+`","ignored":"private"}}`)
	if value["terminal_exit"] != nil || value["withheld"].(map[string]any)["terminal_exit"] != "likely GitHub token" {
		t.Fatalf("untrusted terminal metadata crossed: %v", value)
	}
}

func decodeNarration(t *testing.T, kind, raw string) map[string]any {
	t.Helper()
	payload, ok := publicActivityPayload(kind, json.RawMessage(raw))
	if !ok {
		t.Fatalf("rejected a valid %s", kind)
	}
	var value map[string]any
	if err := json.Unmarshal(payload, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

// The daemon's real narrated events (sessionsvc/testdata/narrated_activity.json, which the daemon's
// own test pins) name the checkout by its absolute path in a thought, a plan step, a read's title,
// input and location, and a command and its output. The controller gets each of them relative to
// the checkout, and never the root itself.
func TestDaemonActivityCrossesWithoutTheCheckoutRoot(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "sessionsvc", "testdata", "narrated_activity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var events []struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &events); err != nil || len(events) == 0 {
		t.Fatalf("decode the daemon's events: %v", err)
	}
	crossed := []string{}
	for _, event := range events {
		payload, ok := publicActivityPayload(event.Type, event.Payload)
		if !ok {
			t.Fatalf("rejected the daemon's %s event", event.Type)
		}
		if strings.Contains(string(payload), "/home/dev") {
			t.Errorf("%s carried the checkout root to the controller: %s", event.Type, payload)
		}
		crossed = append(crossed, string(payload))
	}
	all := strings.Join(crossed, "\n")
	for _, relative := range []string{"The failing test is lib/a_test.go.", "Fix lib/a.go", "Read lib/a.go", "go test lib", "FAIL: lib/a_test.go:12"} {
		if !strings.Contains(all, relative) {
			t.Errorf("lost %q; it should cross relative to the checkout:\n%s", relative, all)
		}
	}
}
