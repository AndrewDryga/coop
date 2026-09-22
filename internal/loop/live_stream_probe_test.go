//go:build providerlivee2e

package loop

import (
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

func TestProviderLiveStreamContract(t *testing.T) {
	// The same native shapes exercised by streamjson_activity_test.go, now held to the live
	// qualification's complete-lifecycle requirement rather than only individual callbacks.
	streams := map[string][]string{
		"claude": {
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"true"}}]}}`,
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
			`{"type":"result","subtype":"success","is_error":false,"num_turns":1}`,
		},
		"codex": {
			`{"type":"item.started","item":{"id":"t1","type":"command_execution","command":"true"}}`,
			`{"type":"item.completed","item":{"id":"t1","type":"command_execution","exit_code":0}}`,
			`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		"gemini": {
			`{"type":"tool_use","tool_name":"run_shell_command","tool_id":"t1","parameters":{"command":"true"}}`,
			`{"type":"tool_result","tool_id":"t1","status":"success"}`,
			`{"type":"result","status":"success","stats":{"input_tokens":1,"output_tokens":1}}`,
		},
		"grok": {
			`{"type":"tool_call","toolCallId":"t1","toolName":"run_terminal_command","kind":"execute","status":"pending","rawInput":{"command":"true"}}`,
			`{"type":"tool_call_update","toolCallId":"t1","status":"completed","rawOutput":{"type":"Bash","exit_code":0}}`,
			`{"type":"end","num_turns":1,"usage":{"input_tokens":1,"output_tokens":1}}`,
		},
	}
	for _, provider := range agents.Names() {
		lines, ok := streams[provider]
		if !ok {
			t.Fatalf("missing native lifecycle fixture for %s", provider)
		}
		t.Run(provider, func(t *testing.T) {
			for name, lines := range map[string][]string{
				"valid":              lines,
				"no tool":            {lines[2]},
				"no close":           {lines[0], lines[2]},
				"wrong close":        {lines[0], strings.ReplaceAll(lines[1], "t1", "unopened"), lines[2]},
				"no terminal":        lines[:2],
				"duplicate terminal": {lines[0], lines[1], lines[2], lines[2]},
				"early terminal":     {lines[2], lines[0], lines[1]},
				"malformed":          {lines[0], lines[1], `{"type":not-json}`, lines[2]},
			} {
				t.Run(name, func(t *testing.T) {
					probe := NewLiveStreamProbe(provider)
					body := strings.Join(lines, "\n") // also prove final partial-line flushing
					for _, part := range []string{body[:len(body)/2], body[len(body)/2:]} {
						if _, err := probe.Write([]byte(part)); err != nil {
							t.Fatal(err)
						}
					}
					if err := probe.Verify(); (err == nil) != (name == "valid") {
						t.Fatalf("lifecycle acceptance=%v, error=%v", err == nil, err)
					}
				})
			}
		})
	}
}
