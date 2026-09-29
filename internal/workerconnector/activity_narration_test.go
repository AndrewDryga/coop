package workerconnector

import (
	"encoding/json"
	"strings"
	"testing"
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
