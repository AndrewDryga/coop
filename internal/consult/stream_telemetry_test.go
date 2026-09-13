package consult

import (
	"strings"
	"testing"
)

func TestConsultWrapperStreamingReplyAndTelemetry(t *testing.T) {
	for _, tc := range []struct {
		peer, stream string
		in, out      string
	}{
		{"gemini", `{"type":"message","role":"user","content":"not the reply"}
{"type":"message","role":"assistant","content":"FIRST_","delta":true}
{"type":"message","role":"assistant","content":"SECOND","delta":true}
{"type":"result","status":"success","stats":{"input_tokens":21,"input":14,"cached":7,"output_tokens":5}}`, "21", "5"},
		{"grok", `{"type":"thought","data":"not the reply"}
{"type":"text","data":"FIRST_"}
{"type":"text","data":"SECOND"}
{"type":"end","total_cost_usd":0.25,"usage":{"input_tokens":14,"cache_read_input_tokens":7,"output_tokens":5,"reasoning_tokens":3}}`, "21", "5"},
	} {
		t.Run(tc.peer, func(t *testing.T) {
			for _, sample := range []struct {
				name, stream, suffix string
				code                 int
				reply, row           bool
			}{
				{"accepted", tc.stream, "", 0, true, true},
				{"invalid usage", strings.ReplaceAll(tc.stream, `"output_tokens":5`, `"output_tokens":-1`), "", 0, true, false},
				{"provider failure", tc.stream, "exit 7", 7, false, false},
				{"error event", `{"type":"error","message":"failed"}` + "\n" + tc.stream, "", 1, false, false},
				{"malformed", "not-json", "", 1, false, false},
			} {
				t.Run(sample.name, func(t *testing.T) {
					body := "printf '%s\\n' '" + sample.stream + "'\n" + sample.suffix
					out, code, row, resumable := runConsultWrapperStub(t, "stream-peer", tc.peer, body, "shift 3\nexec \"$@\"", "stream-run")
					if code != sample.code || resumable != sample.reply || (row != "") != sample.row {
						t.Fatalf("exit %d, resumable %v, row %q:\n%s", code, resumable, row, out)
					}
					if sample.reply && (!strings.Contains(out, "FIRST_SECOND") || strings.Contains(out, "not the reply") || strings.Contains(out, `"type":`)) {
						t.Fatalf("reply leaked native envelope or non-answer text: %s", out)
					}
					if sample.row {
						for _, want := range []string{`"provider":"` + tc.peer + `"`, `"role":"stream-peer"`, `"in":` + tc.in, `"out":` + tc.out} {
							if !strings.Contains(row, want) {
								t.Fatalf("missing %s in row %s", want, row)
							}
						}
						if tc.peer == "grok" && !strings.Contains(row, `"cost":0.25`) {
							t.Fatalf("Grok reported cost lost: %s", row)
						}
					}
				})
			}
			t.Run("diagnostic overflow rejects telemetry", func(t *testing.T) {
				t.Setenv("COOP_CONSULT_STREAM_LIMIT_FOR_TEST", "1024")
				body := "printf '%s\\n' '" + tc.stream + "'\n" + `dd if=/dev/zero bs=1025 count=1 2>/dev/null | tr '\000' X >&2`
				out, code, row, resumable := runConsultWrapperStub(t, "stream-rejected", tc.peer, body, "shift 3\nexec \"$@\"", "rejected")
				if code == 0 || row != "" || resumable || !strings.Contains(out, "diagnostics exceeded") {
					t.Fatalf("unaccepted attempt published usage/state: exit %d, row %q, resumable %v:\n%s", code, row, resumable, out)
				}
			})
		})
	}
}
