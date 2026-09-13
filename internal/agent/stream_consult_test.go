package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Reduced native fresh/resume captures from 2026-09-13, retained in the peer-usage
// task's artifacts/gemini-native-shape-3.json and grok-native-shape-1.json.
// IDs/metadata are omitted and Grok's redacted text deltas replaced with a marker;
// usage and cost values are unchanged. Neither fixture is a pricing estimate.
const capturedGeminiConsult = `{"type":"init","model":"auto"}
{"type":"message","role":"user","content":"previous question"}
{"type":"message","role":"assistant","content":"COOP_USAGE_PROBE","delta":true}
{"type":"result","status":"success","stats":{"input_tokens":10866,"input":2985,"cached":7881,"output_tokens":7,"total_tokens":10930,"duration_ms":3962,"tool_calls":0}}`

const capturedGrokConsult = `{"type":"available_commands","commands":[],"tools":[]}
{"type":"thought","data":"not reply text"}
{"type":"text","data":"COOP_"}
{"type":"text","data":"USAGE_PROBE"}
{"type":"end","num_turns":1,"total_cost_usd":0.00119612,"usage":{"cache_creation_input_tokens":0,"cache_read_input_tokens":6272,"input_tokens":95,"output_tokens":32,"reasoning_tokens":26,"total_tokens":6399}}`

func TestStreamConsultCapturedReplyAndUsage(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	for _, tc := range []struct {
		provider, stream, terminal string
		in, out                    int
		cost                       float64
	}{
		{"gemini", capturedGeminiConsult, `{"type":"result","status":"success"}`, 10866, 7, 0},
		{"grok", capturedGrokConsult, `{"type":"end"}`, 6367, 32, 0.00119612},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			ag, _ := Get(tc.provider)
			for _, sample := range []struct {
				name, stream string
				accepted     bool
			}{
				{"captured", tc.stream, true},
				{"no reply", tc.terminal, false},
				{"missing terminal", `{"type":"text","data":"partial"}`, false},
				{"multiple terminals", tc.stream + "\n" + tc.terminal, false},
				{"after terminal", tc.stream + "\n{}", false},
				{"error event", `{"type":"error","message":"failed"}` + "\n" + tc.stream, false},
				{"malformed", tc.stream + "\nnot-json", false},
				{"scalar event", "true\n" + tc.stream, false},
			} {
				t.Run(sample.name, func(t *testing.T) {
					cmd := exec.Command("sh", "-c", ag.ShellPrelude()+"\n"+tc.provider+"_text")
					cmd.Stdin = strings.NewReader(sample.stream)
					out, err := cmd.CombinedOutput()
					if (err == nil) != sample.accepted || (sample.accepted && string(out) != "COOP_USAGE_PROBE\n") {
						t.Fatalf("reply = %q, error %v, want accepted %v", out, err, sample.accepted)
					}
				})
			}
			for _, sample := range []struct {
				name, stream, runID string
				row                 bool
			}{
				{"captured", tc.stream, "run", true},
				{"no run", tc.stream, "", false},
				{"invalid run", tc.stream, "../outside", false},
				{"no usage", tc.terminal, "run", false},
				{"malformed", "not-json", "run", false},
				{"negative tokens", strings.ReplaceAll(tc.stream, `"output_tokens":`, `"output_tokens":-`), "run", false},
				{"duplicate terminal", tc.stream + "\n" + tc.terminal, "run", false},
			} {
				t.Run("usage/"+sample.name, func(t *testing.T) {
					dir := t.TempDir()
					runs := filepath.Join(dir, ".agent", "runs")
					if err := os.MkdirAll(runs, 0o755); err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(runs, "run.peers.jsonl")
					if err := os.WriteFile(path, nil, 0o600); err != nil {
						t.Fatal(err)
					}
					cmd := exec.Command("sh", "-c", ag.ShellPrelude()+"\n"+tc.provider+"_peer_row critic captured-model")
					cmd.Dir = dir
					cmd.Env = append(os.Environ(), "COOP_RUN_ID="+sample.runID)
					cmd.Stdin = strings.NewReader(sample.stream)
					if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
						t.Fatalf("telemetry changed output/status: %q, %v", out, err)
					}
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if !sample.row {
						if len(data) != 0 {
							t.Fatalf("unexpected telemetry: %s", data)
						}
						return
					}
					var row map[string]any
					if err := json.Unmarshal(data, &row); err != nil {
						t.Fatal(err)
					}
					if row["provider"] != tc.provider || row["role"] != "critic" || row["model"] != "captured-model" || row["in"] != float64(tc.in) || row["out"] != float64(tc.out) {
						t.Fatalf("incorrect peer row: %s", data)
					}
					if cost, exists := row["cost"]; (tc.cost > 0 && cost != tc.cost) || (tc.cost == 0 && exists) {
						t.Fatalf("wrong or invented cost: %s", data)
					}
				})
			}
		})
	}
}
