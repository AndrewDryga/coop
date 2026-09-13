package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The error text is from the observed native event. The command removes the
// unrelated host path and database environment; only its display length matters.
func TestClaudeFailureCauseNativeExample(t *testing.T) {
	const path = "portal/.agent/tasks/10_in_progress/2026-09-11-rename-the-without-operator-input-recovery-test/tmp/focused.log"
	command := "./run test portal apps/emisar_web/test/emisar_web/mcp/mcp_runbook_recovery_tools_test.exs:2849 > " + path + " 2>&1; tail -15 " + path
	output := "Exit code 1\n/bin/bash: line 1: " + path + ": No such file or directory\nfocused exit 1\ntail: cannot open '" + path + "' for reading: No such file or directory"
	quotedCommand, _ := json.Marshal(command)
	quotedOutput, _ := json.Marshal(output)
	events := fmt.Sprintf("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"id\":\"observed\",\"name\":\"Bash\",\"input\":{\"command\":%s}}]}}\n{\"type\":\"user\",\"message\":{\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"observed\",\"is_error\":true,\"content\":%s}]}}\n", quotedCommand, quotedOutput)
	for _, width := range []int{0, 38, 80} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			var out, tail bytes.Buffer
			d := newStreamDecoder(&out, &tail, "claude", "personal", "")
			if width > 0 {
				d.setDisplayWidth(func() int { return width })
			}
			_, _ = d.Write([]byte(events))
			d.flush()
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			failure := lines[len(lines)-1]
			t.Logf("width=%d failure=%q selected existing diagnostic=%q", width, failure, commandFailureDiagnostic(output))
			if !strings.Contains(failure, "cannot open") && !strings.Contains(failure, "No such file") {
				t.Errorf("failure line hides the available filesystem cause: %q", failure)
			}
			if tail.Len() != 0 {
				t.Fatalf("tool output leaked into provider-limit tail: %q", tail.String())
			}
		})
	}
}

func TestClaudeToolFailureDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name, label string
		content     any
		want        string
	}{
		{"exit then cause", "Bash make check", "Exit code 1\nerror: missing directory", "error: missing directory"},
		{"ANSI header", "Bash make check", "\x1b[31m\x1b[0m\n\x1b[31mExit code 1\x1b[0m\n\x1b[31merror:\tdenied\x1b[0m\x00", "error: denied"},
		{"empty", "Bash make check", "\n\x1b[31m\x1b[0m", ""},
		{"weak only", "Bash make check", "Exit code 1", "Exit code 1"},
		{"MCP useful first", "mcp__tasks__update", "Missing required field: next_action\nRetry with all fields", "Missing required field: next_action"},
		{"MCP weak metadata", "mcp__tasks__update", "Error\nRequest id abc123", "Error"},
		{"MCP metadata-only tail", "mcp__tasks__update", "Error\n" + strings.Repeat("Request id abc123\n", diagnosticTailMax+1), "Error"},
		{"MCP keyword metadata", "mcp__tasks__update", "Error\nrequest_error_id: 123", "Error"},
		{"MCP hyphenated metadata", "mcp__tasks__update", "Error\nrequest-error-id: 123", "Error"},
		{"consult useful first", "consult → thinker", "Consultation deadline elapsed\nRetry later", "Consultation deadline elapsed"},
		{"block array", "Bash make check", []map[string]any{{"type": "text", "text": "Exit code 1"}, {"type": "text", "text": "fatal: permission denied"}}, "fatal: permission denied"},
		{"unpaired result", "", "Exit code 1\ncannot open file", "cannot open file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, tail, classification bytes.Buffer
			d := newStreamDecoder(&out, &tail, "claude", "personal", "")
			d.diagnostic = &classification
			d.tool.set("tool", tc.label)
			content, err := json.Marshal(tc.content)
			if err != nil {
				t.Fatal(err)
			}
			event := []byte(fmt.Sprintf("{\"type\":\"user\",\"message\":{\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"tool\",\"is_error\":true,\"content\":%s}]}}\n", content))
			original := bytes.Clone(event)
			_, _ = d.Write(event)
			d.flush()
			want := "  ✗"
			if tc.label != "" {
				want += " " + tc.label
			}
			if tc.want != "" {
				want += ": " + tc.want
			}
			if got := out.String(); got != want+"\n" {
				t.Fatalf("failure output = %q, want %q", got, want+"\n")
			}
			if !bytes.Equal(event, original) || tail.Len() != 0 || classification.Len() != 0 {
				t.Fatal("display rewrote provider input or classified tool output")
			}
			out.Reset()
			_, _ = d.Write(bytes.Replace(event, []byte(`"is_error":true`), []byte(`"is_error":false`), 1))
			d.flush()
			if out.Len() != 0 {
				t.Fatalf("successful tool result became noisy: %q", out.String())
			}
		})
	}
}

func TestFailureLineSharesLiveWidthWithCause(t *testing.T) {
	d := newNDJSONDecoder(nil, nil, nil)
	for _, width := range []int{8, 15, 25, 38, 80, 180} {
		for _, suffix := range []string{"", " (exit 7)"} {
			d.setDisplayWidth(func() int { return width })
			got := d.streamFailureLine("Bash "+strings.Repeat("long command ", 30), suffix, "error: cannot open file", streamToolTextWidth)
			if !strings.HasPrefix(got, "  ✗") || len([]rune(got)) > width-1 {
				t.Fatalf("width %d suffix %q: %q", width, suffix, got)
			}
			if width >= 38 && (!strings.Contains(got, "Bash") || !strings.Contains(got, "error:") || !strings.Contains(got, strings.TrimSpace(suffix))) {
				t.Fatalf("width %d lost tool, cause or exit: %q", width, got)
			}
		}
	}
}
