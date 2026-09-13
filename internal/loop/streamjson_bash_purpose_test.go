package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestBashPurposeDisplay(t *testing.T) {
	for _, tc := range []struct{ description, want string }{
		{"Capture the before screenshot", "Capture the before screenshot — export MIX_ENV=dev"},
		{"\n Capture the before screenshot\nsecond line", "Capture the before screenshot — export MIX_ENV=dev"},
		{"\x1b[31mCapture\x1b[0m\tbefore\x00", "Capture before — export MIX_ENV=dev"},
		{"", "export MIX_ENV=dev"}, {"\x1b[31m\x00\x1b[0m", "export MIX_ENV=dev"},
		{"Run command", "export MIX_ENV=dev"}, {"Execute shell command.", "export MIX_ENV=dev"},
		{"export MIX_ENV=dev", "export MIX_ENV=dev"},
	} {
		var out, tail bytes.Buffer
		d := newStreamDecoder(&out, &tail, "claude", "", "")
		got := d.streamBashToolLine("export MIX_ENV=dev", tc.description)
		if got != "⚙ Bash "+tc.want {
			t.Errorf("description %q: %q, want %q", tc.description, got, tc.want)
		}
	}
}

func TestBashPurposeWidthsAndFailureIdentity(t *testing.T) {
	command := "export MIX_ENV=test; ./run gate portal > task/tmp/check.log 2>&1"
	description := "Capture the before screenshot of the approval requirements rail"
	input, _ := json.Marshal(toolInput{Command: command, Description: description})
	event := fmt.Sprintf("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"id\":\"purpose\",\"name\":\"Bash\",\"input\":%s}]}}\n", input)
	for _, width := range []int{0, 20, 38, 40, 80, 120} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			var out, tail bytes.Buffer
			d := newStreamDecoder(&out, &tail, "claude", "", "")
			if width > 0 {
				d.setDisplayWidth(func() int { return width })
			}
			_, _ = d.Write([]byte(event))
			d.flush()
			line := strings.TrimSpace(out.String())
			t.Logf("columns=%d: %s", width, line)
			if !strings.Contains(line, "Bash") || !strings.Contains(line, "export") {
				t.Fatalf("lost real command identity: %s", line)
			}
			if width >= 40 || width == 0 {
				if !strings.Contains(line, "Capture") || !strings.Contains(line, " — ") {
					t.Fatalf("useful purpose absent: %s", line)
				}
			}
			if width > 0 && len([]rune(line)) > width-1 {
				t.Fatalf("row wrapped: %q", line)
			}
			if width == 0 && len([]rune(line)) > streamToolTextWidth+len([]rune("⚙ Bash ")) {
				t.Fatalf("static row exceeded cap: %q", line)
			}
			out.Reset()
			_, _ = d.Write([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"purpose","is_error":true,"content":"Exit code 1\nerror: screenshot failed"}]}}` + "\n"))
			d.flush()
			failure := strings.TrimSpace(out.String())
			if strings.Contains(failure, "Capture") || !strings.Contains(failure, "Bash") {
				t.Fatalf("purpose replaced failure identity: %s", failure)
			}
			if width >= 80 || width == 0 {
				if !strings.Contains(failure, "export") || !strings.Contains(failure, "screenshot failed") {
					t.Fatalf("lost command/cause: %s", failure)
				}
			}
			if tail.Len() != 0 {
				t.Fatalf("description reached rate-limit detector: %s", tail.String())
			}
		})
	}
}

func TestBashPurposePreservesSpecialLabels(t *testing.T) {
	for _, command := range []string{"coop-consult thinker --fresh review", "coop-delegate fast task", "mkdir -p .agent/tasks/00_todo/2026-09-12-a-task"} {
		var out, tail bytes.Buffer
		d := newStreamDecoder(&out, &tail, "claude", "", "")
		input, _ := json.Marshal(toolInput{Command: command, Description: "DESCRIPTION_MUST_NOT_REPLACE_CLASSIFICATION"})
		event := fmt.Sprintf("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"id\":\"special\",\"name\":\"Bash\",\"input\":%s}]}}\n", input)
		_, _ = d.Write([]byte(event))
		d.flush()
		if strings.Contains(out.String(), "DESCRIPTION") || strings.Contains(out.String(), "Bash") {
			t.Fatalf("special tool lost classification: %s", out.String())
		}
	}
}

func TestBashPurposeResizeAndSuccessfulResult(t *testing.T) {
	var out, tail bytes.Buffer
	d := newStreamDecoder(&out, &tail, "claude", "", "")
	width := 80
	d.setDisplayWidth(func() int { return width })
	input, _ := json.Marshal(toolInput{Command: "T=task/tmp; identify before.png after.png", Description: "Check screenshot dimensions"})
	event := []byte(fmt.Sprintf("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"id\":\"screenshot\",\"name\":\"Bash\",\"input\":%s}]}}\n", input))
	original := bytes.Clone(event)
	for _, columns := range []int{80, 40} {
		width = columns
		out.Reset()
		_, _ = d.Write(event)
		d.flush()
		line := strings.TrimSpace(out.String())
		if !strings.Contains(line, "Check screenshot") || !strings.Contains(line, "T=") || len([]rune(line)) > width-1 {
			t.Fatalf("resize to %d lost purpose/command or wrapped: %s", width, line)
		}
		if !bytes.Equal(event, original) {
			t.Fatal("display mutated provider evidence")
		}
		out.Reset()
		_, _ = d.Write([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"screenshot","content":"success"}]}}` + "\n"))
		d.flush()
		if out.Len() != 0 || tail.Len() != 0 {
			t.Fatalf("successful tool invented narration: %q", out.String())
		}
	}
}
