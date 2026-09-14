package consult

import (
	"strings"
	"testing"
)

func TestConsultWrapperClaudeJSONReplyAndTelemetry(t *testing.T) {
	const passTimeout = "shift 3\nexec \"$@\""
	const result = `{"type":"result","subtype":"success","is_error":false,"result":"CLAUDE_ANSWER","total_cost_usd":0.25,"usage":{"input_tokens":11,"cache_creation_input_tokens":3,"cache_read_input_tokens":7,"output_tokens":5}}`
	for _, tc := range []struct {
		name, output, suffix  string
		code                  int
		reply, row, resumable bool
	}{
		{"accepted", result, "", 0, true, true, true},
		{"no cost", strings.Replace(result, `"total_cost_usd":0.25,`, "", 1), "", 0, true, true, true},
		{"bad usage", strings.Replace(result, `"input_tokens":11`, `"input_tokens":-1`, 1), "", 0, true, false, true},
		{"provider failure", result, "exit 7", 7, false, false, false},
		{"error object", strings.Replace(result, `"is_error":false`, `"is_error":true`, 1), "", 1, false, false, false},
		{"empty reply", strings.Replace(result, "CLAUDE_ANSWER", " ", 1), "", 1, false, false, false},
		{"malformed", "not-json", "", 1, false, false, false},
		{"multiple objects", result + "\n" + result, "", 1, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "printf '%s\\n' '" + tc.output + "'\n" + tc.suffix
			out, code, row, resumable := runConsultWrapperStub(t, "claude-json", "claude", body, passTimeout, "claude-run")
			if code != tc.code || resumable != tc.resumable || (row != "") != tc.row {
				t.Fatalf("exit %d, resumable %v, telemetry %q; want %d/%v/%v:\n%s", code, resumable, row, tc.code, tc.resumable, tc.row, out)
			}
			if tc.reply && (!strings.Contains(out, "CLAUDE_ANSWER") || strings.Contains(out, `"type":"result"`)) {
				t.Fatalf("lead did not receive only decoded reply: %s", out)
			}
			if tc.row {
				for _, want := range []string{`"provider":"claude"`, `"role":"claude-json"`, `"in":21`, `"out":5`, `"fresh_in":11`, `"cache_write":3`, `"cache_read":7`, `"reported_out":5`} {
					if !strings.Contains(row, want) {
						t.Errorf("row missing %s: %s", want, row)
					}
				}
				if tc.name == "no cost" {
					if strings.Contains(row, `"cost"`) {
						t.Errorf("missing cost was invented: %s", row)
					}
				} else if !strings.Contains(row, `"cost":0.25`) {
					t.Errorf("reported cost lost: %s", row)
				}
			}
		})
	}
}

func TestConsultWrapperClaudeTelemetryWaitsForFullAttemptAcceptance(t *testing.T) {
	t.Setenv("COOP_CONSULT_STREAM_LIMIT_FOR_TEST", "1024")
	body := `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"VALID_REPLY","total_cost_usd":0.25,"usage":{"input_tokens":1,"output_tokens":2}}'
dd if=/dev/zero bs=1025 count=1 2>/dev/null | tr '\000' X >&2`
	out, code, row, resumable := runConsultWrapperStub(t, "claude-rejected", "claude", body, "shift 3\nexec \"$@\"", "rejected")
	if code == 0 || row != "" || resumable || !strings.Contains(out, "diagnostics exceeded") {
		t.Fatalf("rejected full attempt published reply state or usage: exit %d, row %q, resumable %v:\n%s", code, row, resumable, out)
	}
}
