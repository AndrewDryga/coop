package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Native Claude JSON retained in the archived egress task's
// artifacts/2026-09-08-fable-consult.json; its log records the actual CLI call.
// Only unrelated metadata and review prose are removed. Token/cost fields are
// copied unchanged, including thinking nested within (not added to) output.
const capturedClaudeConsult = `{"type":"result","subtype":"success","is_error":false,"result":"CAPTURED_REPLY","total_cost_usd":0.7052490000000001,"usage":{"input_tokens":2,"cache_creation_input_tokens":7678,"cache_read_input_tokens":0,"output_tokens":10963,"output_tokens_details":{"thinking_tokens":7581}}}`

func TestClaudeConsultCapturedResult(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	cmd := exec.Command("sh", "-c", (claudeAgent{}).ShellPrelude()+"\nclaude_text")
	cmd.Stdin = strings.NewReader(capturedClaudeConsult)
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "CAPTURED_REPLY\n" {
		t.Fatalf("captured reply = %q, error %v", out, err)
	}
}

func TestClaudePeerRowShell(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	for _, tc := range []struct {
		name, stream, runID string
		wantRow, wantCost   bool
		wantIn              int
	}{
		{"captured", capturedClaudeConsult, "run", true, true, 7680},
		{"cache read", strings.Replace(capturedClaudeConsult, `"cache_read_input_tokens":0`, `"cache_read_input_tokens":11`, 1), "run", true, true, 7691},
		{"no run", capturedClaudeConsult, "", false, false, 0},
		{"invalid run", capturedClaudeConsult, "../outside", false, false, 0},
		{"no usage", `{"type":"result","is_error":false,"result":"VALID"}`, "run", false, false, 0},
		{"no cost", strings.Replace(capturedClaudeConsult, `"total_cost_usd":0.7052490000000001,`, "", 1), "run", true, false, 7680},
		{"negative cost", strings.Replace(capturedClaudeConsult, "0.7052490000000001", "-1", 1), "run", true, false, 7680},
		{"invalid cost", strings.Replace(capturedClaudeConsult, "0.7052490000000001", `"not money"`, 1), "run", true, false, 7680},
		{"invalid usage", strings.Replace(capturedClaudeConsult, `"input_tokens":2`, `"input_tokens":-1`, 1), "run", false, false, 0},
		{"fractional tokens", strings.Replace(capturedClaudeConsult, `"input_tokens":2`, `"input_tokens":1.5`, 1), "run", false, false, 0},
		{"error result", strings.Replace(capturedClaudeConsult, `"is_error":false`, `"is_error":true`, 1), "run", false, false, 0},
		{"multiple results", capturedClaudeConsult + "\n" + capturedClaudeConsult, "run", false, false, 0},
		{"malformed", "not-json", "run", false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			runs := filepath.Join(dir, ".agent", "runs")
			if err := os.MkdirAll(runs, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(runs, "run.peers.jsonl")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", (claudeAgent{}).ShellPrelude()+"\nclaude_peer_row thinker captured-model")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "COOP_RUN_ID="+tc.runID)
			cmd.Stdin = strings.NewReader(tc.stream)
			if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
				t.Fatalf("best-effort telemetry changed consult status/output: %v, %q", err, out)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wantRow {
				if len(data) != 0 {
					t.Fatalf("unexpected row: %s", data)
				}
				return
			}
			var row map[string]any
			if err := json.Unmarshal(data, &row); err != nil {
				t.Fatalf("invalid row %q: %v", data, err)
			}
			if row["provider"] != "claude" || row["run"] != "run" || row["role"] != "thinker" || row["model"] != "captured-model" || row["in"] != float64(tc.wantIn) || row["out"] != float64(10963) {
				t.Fatalf("incorrect usage/identity: %s", data)
			}
			cost, exists := row["cost"]
			if exists != tc.wantCost || (tc.wantCost && cost != 0.7052490000000001) {
				t.Fatalf("reported cost = %v (present %v), want present %v", cost, exists, tc.wantCost)
			}
		})
	}
}
