package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// The decoder→watchdog activity contract: only valid adapter-recognized semantic events reach
// the sink; malformed, unknown, raw, and repaint-only output never does.

func feedActivityLines(t *testing.T, d iterationStreamDecoder, rec *activityRecorder, lines []string) {
	t.Helper()
	d.setActivity(rec)
	blob := strings.Join(lines, "\n") + "\n"
	cut := len(blob) / 2 // split mid-line: partial buffering must not double-report events
	_, _ = d.Write([]byte(blob[:cut]))
	_, _ = d.Write([]byte(blob[cut:]))
	d.flush()
}

func TestClaudeStreamActivity(t *testing.T) {
	var out, tail bytes.Buffer
	rec := &activityRecorder{}
	lines := []string{
		`{"type":"system","subtype":"init","model":"claude-fable-5"}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"reasoning"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"make check"}}]}}`,
		`not valid json`,
		`{"type":not-json}`,
		`{"type":"session_state_changed"}`,
		`{"type":"user","message":{"content":[{"tool_use_id":"t1","type":"tool_result","content":"ok","is_error":false}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":1}`,
	}
	feedActivityLines(t, newStreamDecoder(&out, &tail, "claude", "", ""), rec, lines)
	want := []string{"bootstrap", "progress", "progress", "tool_start:t1", "tool_end:t1", "progress", "terminal"}
	if !slices.Equal(rec.events, want) {
		t.Errorf("claude activity = %v, want %v", rec.events, want)
	}
}

func TestCodexStreamActivity(t *testing.T) {
	var out, tail bytes.Buffer
	rec := &activityRecorder{}
	lines := []string{
		`{"type":"thread.started","thread_id":"x"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"id":"r1","type":"reasoning"}}`,
		`{"type":"item.completed","item":{"id":"r1","type":"reasoning"}}`,
		`{"type":"item.started","item":{"id":"c1","type":"command_execution","command":"make check"}}`,
		`{"type":"item.updated","item":{"id":"c1","type":"command_execution"}}`,
		`{"type":"item.completed","item":{"id":"c1","type":"command_execution","exit_code":0}}`,
		`{"type":"item.started","item":{"id":"w1","type":"web_search","query":"docs"}}`,
		`{"type":"item.completed","item":{"id":"m1","type":"agent_message","text":"done"}}`,
		`{"type":"item.completed","item":{"id":"u1","type":"banana"}}`,
		`{"type":"future.event"}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`,
	}
	feedActivityLines(t, newCodexStreamDecoder(&out, &tail, "codex", "", "", "m"), rec, lines)
	want := []string{
		"bootstrap", "progress", "progress", "tool_start:c1", "progress", "tool_end:c1",
		"progress", "progress", "terminal",
	}
	if !slices.Equal(rec.events, want) {
		t.Errorf("codex activity = %v, want %v", rec.events, want)
	}
}

func TestGeminiStreamActivity(t *testing.T) {
	var out, tail bytes.Buffer
	rec := &activityRecorder{}
	lines := []string{
		`{"type":"init"}`,
		`{"type":"message","role":"user","content":"the whole prompt echo"}`,
		`{"type":"message","role":"assistant","content":"working"}`,
		`{"type":"tool_use","tool_name":"run_shell_command","tool_id":"g1","parameters":{"command":"make check"}}`,
		`{"type":"tool_result","tool_id":"g1","status":"success"}`,
		`{"type":"retry"}`,
		`{"type":"result","status":"success","stats":{"input_tokens":1,"output_tokens":1,"duration_ms":5}}`,
	}
	feedActivityLines(t, newGeminiStreamDecoder(&out, &tail, "gemini", "", "", "m"), rec, lines)
	want := []string{"bootstrap", "progress", "tool_start:g1", "tool_end:g1", "terminal"}
	if !slices.Equal(rec.events, want) {
		t.Errorf("gemini activity = %v, want %v", rec.events, want)
	}
}

func TestGrokStreamActivity(t *testing.T) {
	var out, tail bytes.Buffer
	rec := &activityRecorder{}
	lines := []string{
		`{"type":"thought","data":"hidden reasoning"}`,
		`{"type":"text","data":"working"}`,
		`{"type":"tool_call","data":"unrecognized"}`,
		`{"type":"end","num_turns":1,"usage":{"input_tokens":1,"output_tokens":1}}`,
	}
	feedActivityLines(t, newGrokStreamDecoder(&out, &tail, "grok", "", "", "m"), rec, lines)
	want := []string{"progress", "progress", "terminal"}
	if !slices.Equal(rec.events, want) {
		t.Errorf("grok activity = %v, want %v", rec.events, want)
	}
}

func TestRawAndRepaintOutputNeverResetsActivity(t *testing.T) {
	var out, tail bytes.Buffer
	rec := &activityRecorder{}
	d := newStreamDecoder(&out, &tail, "claude", "", "")
	d.setActivity(rec)
	// Raw prose, ANSI repaints, a malformed event, and an oversized event: none are
	// adapter-recognized semantic activity.
	_, _ = d.Write([]byte("plain diagnostic line\n"))
	_, _ = d.Write([]byte("\x1b[2K\x1b[1Gredraw\n"))
	_, _ = d.Write([]byte(`{"type":"assistant","message":`))
	_, _ = d.Write([]byte("\n"))
	_, _ = d.Write(append(bytes.Repeat([]byte("x"), maxStreamEventBytes+1), '\n'))
	d.flush()
	if len(rec.events) != 0 {
		t.Errorf("untrusted output produced activity: %v", rec.events)
	}
}
