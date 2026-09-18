package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestClaudeToolResultsOnlyEndAcceptedMatchingStarts(t *testing.T) {
	var out, tail bytes.Buffer
	rec := &activityRecorder{}
	d := newStreamDecoder(&out, &tail, "claude", "", "")
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"known","name":"Bash","input":{"command":"make check"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"still-open","name":"Read","input":{"file_path":"result.png"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"unnamed","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"unknown","is_error":true,"content":"permission denied"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"unnamed","content":"ignored"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"known","content":"ok"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"known","content":"duplicate"}]}}`,
	}
	feedActivityLines(t, d, rec, lines)
	want := []string{"progress", "tool_start:known", "progress", "tool_start:still-open", "tool_end:known"}
	if !slices.Equal(rec.events, want) {
		t.Fatalf("Claude paired activity = %v, want %v", rec.events, want)
	}
	if !strings.Contains(out.String(), "permission denied") {
		t.Fatalf("unknown-ID tool failure was hidden instead of merely withheld from activity:\n%s", out.String())
	}
	if _, tracked := d.tool.byID["still-open"]; !tracked {
		t.Fatal("unrelated accepted tool was closed")
	}
	if _, tracked := d.tool.byID["unnamed"]; tracked {
		t.Fatal("unnamed tool was tracked as an accepted lifecycle start")
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

// Grok's pinned client opens every tool under a toolCallId and closes it on ACP's completed, failed
// or cancelled status — a non-zero shell exit included, and a call that arrives already finished.
// Null and in-progress updates are progress inside the open tool; an update for an id nothing
// opened, a second close, and the other providers' tool shapes are nothing in a grok stream.
func TestGrokStreamActivity(t *testing.T) {
	var out, tail bytes.Buffer
	rec := &activityRecorder{}
	lines := []string{
		`{"type":"thought","data":"plan"}`,
		`{"type":"text","data":"working"}`,
		`{"type":"tool_call","toolCallId":"c1","toolName":"run_terminal_command","kind":"execute","status":"pending","rawInput":{"command":"make check"}}`,
		`{"type":"tool_call_update","toolCallId":"c1","status":null,"content":[]}`,
		`{"type":"tool_call_update","toolCallId":"c1","status":"in_progress","rawOutput":{"type":"Bash","exit_code":0}}`,
		`{"type":"tool_call_update","toolCallId":"c1","status":"completed","rawOutput":{"type":"Bash","exit_code":2}}`,
		`{"type":"tool_call_update","toolCallId":"c1","status":"completed"}`,
		`{"type":"tool_call_update","toolCallId":"ghost","status":"completed"}`,
		`{"type":"tool_call","toolCallId":"c2","toolName":"read_file","kind":"read","status":"pending","rawInput":{"target_file":"/repo/a.go"}}`,
		`{"type":"tool_call_update","toolCallId":"c2","status":"failed","content":[{"type":"content","content":{"type":"text","text":"no such file"}}]}`,
		`{"type":"tool_call","toolCallId":"c3","toolName":"read_file","kind":"read","status":"completed","rawInput":{"target_file":"/repo/b.go"}}`,
		`{"type":"tool_call","toolCallId":"c4","toolName":"run_terminal_command","kind":"execute","status":"pending","rawInput":{"command":"sleep 60"}}`,
		`{"type":"tool_call_update","toolCallId":"c4","status":"cancelled"}`,
		`{"type":"tool_use","tool_name":"run_shell_command","tool_id":"g1","parameters":{"command":"make check"}}`,
		`{"type":"tool_result","tool_id":"g1","status":"success"}`,
		`{"type":"item.started","item":{"id":"x1","type":"command_execution","command":"make check"}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"make check"}}]}}`,
		`{"type":"usage","usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"type":"end","num_turns":1,"usage":{"input_tokens":1,"output_tokens":1}}`,
	}
	feedActivityLines(t, newGrokStreamDecoder(&out, &tail, "grok", "", "/repo", "m"), rec, lines)
	want := []string{"progress", "progress", "tool_start:c1", "tool_end:c1", "tool_start:c2", "tool_end:c2",
		"tool_start:c3", "tool_end:c3", "tool_start:c4", "tool_end:c4", "terminal"}
	if !slices.Equal(rec.events, want) {
		t.Errorf("grok activity = %v, want %v", rec.events, want)
	}
}

// A tool the stream opened and never closed is still open when the attempt ends: the watchdog's
// tool cap, not the decoder, bounds it.
func TestGrokStreamAbandonedToolStaysOpenUntilTheAttemptEnds(t *testing.T) {
	var out, tail bytes.Buffer
	rec := &activityRecorder{}
	lines := []string{
		`{"type":"tool_call","toolCallId":"c1","toolName":"run_terminal_command","kind":"execute","status":"pending","rawInput":{"command":"sleep 3600"}}`,
		`{"type":"tool_call_update","toolCallId":"c1","status":"in_progress","rawOutput":{"type":"Bash","exit_code":0}}`,
		`{"type":"end","num_turns":1,"usage":{"input_tokens":1,"output_tokens":1}}`,
	}
	feedActivityLines(t, newGrokStreamDecoder(&out, &tail, "grok", "", "/repo", "m"), rec, lines)
	if want := []string{"tool_start:c1", "terminal"}; !slices.Equal(rec.events, want) {
		t.Errorf("abandoned grok tool activity = %v, want %v", rec.events, want)
	}
}

// Every provider's EMPTY-but-recognized envelopes. Each one parses, carries a type the adapter
// knows, and proves nothing: no content, no ID, nothing that can be paired or shown. Coming off
// the box's own stdout they are the cheapest possible deadline reset, so none of them may produce
// activity. (Bootstrap and terminal events are absent on purpose: bootstrap arms no clock at all,
// and a terminal event ends the attempt rather than extending it.)
func TestEmptyRecognizedEnvelopesNeverResetActivity(t *testing.T) {
	var out, tail bytes.Buffer
	cases := []struct {
		name    string
		decoder iterationStreamDecoder
		lines   []string
	}{
		{"claude", newStreamDecoder(&out, &tail, "claude", "", ""), []string{
			`{"type":"assistant"}`,
			`{"type":"assistant","message":{}}`,
			`{"type":"assistant","message":{"content":[]}}`,
			`{"type":"assistant","message":{"content":[{}]}}`,
			`{"type":"assistant","message":{"content":[{"type":"text","text":""}]}}`,
			`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":""}]}}`,
			`{"type":"assistant","message":{"content":[{"type":"tool_use"}]}}`,
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1"}]}}`,
			`{"type":"user","message":{"content":[{"type":"tool_result"}]}}`,
			`{"type":"user","message":{"content":[{"type":"tool_result","is_error":true}]}}`,
		}},
		{"codex", newCodexStreamDecoder(&out, &tail, "codex", "", "", "m"), []string{
			`{"type":"item.started","item":{}}`,
			`{"type":"item.started","item":{"type":"reasoning"}}`,
			`{"type":"item.started","item":{"type":"command_execution","command":"make check"}}`,
			`{"type":"item.updated","item":{"type":"agent_message"}}`,
			`{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`,
			`{"type":"item.completed","item":{"type":"mcp_tool_call"}}`,
		}},
		{"gemini", newGeminiStreamDecoder(&out, &tail, "gemini", "", "", "m"), []string{
			`{"type":"message","role":"assistant"}`,
			`{"type":"message","role":"assistant","content":""}`,
			`{"type":"tool_use"}`,
			`{"type":"tool_use","tool_name":"run_shell_command","parameters":{"command":"make check"}}`,
			`{"type":"tool_result"}`,
			`{"type":"tool_result","status":"success"}`,
		}},
		{"grok", newGrokStreamDecoder(&out, &tail, "grok", "", "", "m"), []string{
			`{"type":"thought"}`,
			`{"type":"thought","data":""}`,
			`{"type":"text"}`,
			`{"type":"text","data":""}`,
			`{"type":"tool_call"}`,
			`{"type":"tool_call","toolName":"run_terminal_command","kind":"execute","rawInput":{"command":"make check"}}`,
			`{"type":"tool_call_update"}`,
			`{"type":"tool_call_update","toolCallId":"c1","status":"completed"}`,
			`{"type":"usage","usage":{"input_tokens":1}}`,
			`{"type":"available_commands","tools":["read_file"]}`,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &activityRecorder{}
			feedActivityLines(t, c.decoder, rec, c.lines)
			if len(rec.events) != 0 {
				t.Errorf("empty %s envelopes produced activity: %v", c.name, rec.events)
			}
		})
	}
}

// IDs come off the box's stdout, so unique ones are free to invent. A forged flood IS accepted as
// activity — content-bearing events are indistinguishable from honest ones, which is exactly why
// the watchdog carries a non-resettable attempt ceiling — but it may not grow the host's
// per-attempt state past the cap while doing it.
func TestForgedStreamIDsCannotGrowDecoderState(t *testing.T) {
	const forged = 5_000
	var out, tail bytes.Buffer
	claude := newStreamDecoder(&out, &tail, "claude", "", "")
	codexTools := newCodexStreamDecoder(&out, &tail, "codex", "", "", "m")
	codexShown := newCodexStreamDecoder(&out, &tail, "codex", "", "", "m")
	gemini := newGeminiStreamDecoder(&out, &tail, "gemini", "", "", "m")
	grok := newGrokStreamDecoder(&out, &tail, "grok", "", "", "m")
	cases := []struct {
		name    string
		decoder iterationStreamDecoder
		line    func(i int) string
		tracked func() int
	}{
		{"claude tool_use", claude, func(i int) string {
			return fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"forged-%d","name":"Bash","input":{"command":"make check"}}]}}`, i)
		}, func() int { return len(claude.tool.byID) }},
		{"codex command_execution", codexTools, func(i int) string {
			return fmt.Sprintf(`{"type":"item.started","item":{"id":"forged-%d","type":"command_execution","command":"make check"}}`, i)
		}, func() int { return len(codexTools.tool.byID) }},
		{"codex web_search", codexShown, func(i int) string {
			return fmt.Sprintf(`{"type":"item.started","item":{"id":"forged-%d","type":"web_search","query":"q"}}`, i)
		}, func() int { return len(codexShown.shown.byID) }},
		{"gemini tool_use", gemini, func(i int) string {
			return fmt.Sprintf(`{"type":"tool_use","tool_name":"run_shell_command","tool_id":"forged-%d","parameters":{"command":"make check"}}`, i)
		}, func() int { return len(gemini.tool.byID) }},
		{"grok tool_call", grok, func(i int) string {
			return fmt.Sprintf(`{"type":"tool_call","toolCallId":"forged-%d","toolName":"run_terminal_command","kind":"execute","status":"pending","rawInput":{"command":"make check"}}`, i)
		}, func() int { return max(len(grok.tool.byID), len(grok.commands.byID)) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &activityRecorder{}
			c.decoder.setActivity(rec)
			for i := range forged {
				_, _ = c.decoder.Write([]byte(c.line(i) + "\n"))
			}
			c.decoder.flush()
			if got := c.tracked(); got > maxStreamTrackedIDs {
				t.Errorf("%d forged IDs grew tracked state to %d, want at most %d", forged, got, maxStreamTrackedIDs)
			}
			if len(rec.events) < forged {
				t.Errorf("forged flood produced %d activity events, want at least %d — this test's whole point is that validation does NOT stop forgery", len(rec.events), forged)
			}
		})
	}
}

// A well-formed stream must not leak either: every tool that reports its result is forgotten, so
// a long attempt's tracked state tracks OPEN tools, not the tools it has ever run.
func TestCompletedToolsLeaveNoDecoderState(t *testing.T) {
	var out, tail bytes.Buffer
	claude := newStreamDecoder(&out, &tail, "claude", "", "")
	rec := &activityRecorder{}
	claude.setActivity(rec)
	var lines []string
	for i := range 1_000 {
		lines = append(lines,
			fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t%d","name":"Bash","input":{"command":"make check"}}]}}`, i),
			fmt.Sprintf(`{"type":"user","message":{"content":[{"tool_use_id":"t%d","type":"tool_result","content":"ok"}]}}`, i))
	}
	feedActivityLines(t, claude, rec, lines)
	if len(claude.tool.byID) != 0 {
		t.Errorf("1000 completed tools left %d labels behind", len(claude.tool.byID))
	}
}

func TestClaudeLargeImageResultClosesOnlyItsMatchingTool(t *testing.T) {
	var out, tail, diagnostic bytes.Buffer
	rec := &activityRecorder{}
	d := newStreamDecoder(&out, &tail, "claude", "", "")
	d.diagnostic = &diagnostic
	d.setActivity(rec)
	start := `{"type":"assistant","message":{"content":[` +
		`{"type":"tool_use","id":"image-read","name":"Read","input":{"file_path":"capture.png"}},` +
		`{"type":"tool_use","id":"still-open","name":"Bash","input":{"command":"make check"}}]}}`
	_, _ = d.Write([]byte(start + "\n"))

	imageData := strings.Repeat("A", 630648)
	image := map[string]any{"type": "image", "source": map[string]any{
		"type": "base64", "media_type": "image/png", "data": imageData,
	}}
	result := map[string]any{"type": "tool_result", "tool_use_id": "image-read", "content": []any{image}}
	frame, err := json.Marshal(map[string]any{
		"type": "user", "message": map[string]any{"content": []any{result}}, "toolUseResult": image,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(frame) <= maxStreamEventBytes || len(frame) > maxClaudeStreamEventBytes {
		t.Fatalf("Claude image fixture size = %d, want (%d, %d]", len(frame), maxStreamEventBytes, maxClaudeStreamEventBytes)
	}
	for len(frame) > 0 {
		n := min(8191, len(frame))
		_, _ = d.Write(frame[:n])
		frame = frame[n:]
	}
	_, _ = d.Write([]byte("\n"))
	d.flush()

	for _, want := range []string{"tool_start:image-read", "tool_start:still-open", "tool_end:image-read"} {
		if !slices.Contains(rec.events, want) {
			t.Fatalf("large image lifecycle missing %q: %v", want, rec.events)
		}
	}
	if slices.Contains(rec.events, "tool_end:still-open") {
		t.Fatalf("large image result closed another tool: %v", rec.events)
	}
	if _, tracked := d.tool.byID["image-read"]; tracked {
		t.Fatal("completed image tool label remained tracked")
	}
	if _, tracked := d.tool.byID["still-open"]; !tracked {
		t.Fatal("unrelated open tool label was lost")
	}
	combined := out.String() + tail.String() + diagnostic.String()
	if strings.Contains(combined, imageData[:256]) || strings.Contains(combined, "provider stream event exceeded") {
		t.Fatalf("image bytes leaked or valid result was omitted: output bytes=%d", len(combined))
	}
}

func TestClaudeLargeMalformedAndOverCapEventsDoNotCloseTools(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame []byte
	}{
		{
			name: "malformed within Claude envelope",
			frame: []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"open","content":"` +
				strings.Repeat("A", maxStreamEventBytes+1) + `"}]}`),
		},
		{
			name:  "beyond Claude envelope",
			frame: bytes.Repeat([]byte("x"), maxClaudeStreamEventBytes+1),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, tail bytes.Buffer
			rec := &activityRecorder{}
			d := newStreamDecoder(&out, &tail, "claude", "", "")
			d.setActivity(rec)
			_, _ = d.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"open","name":"Read","input":{"file_path":"capture.png"}}]}}` + "\n"))
			before := len(rec.events)
			for len(tc.frame) > 0 {
				n := min(8191, len(tc.frame))
				_, _ = d.Write(tc.frame[:n])
				tc.frame = tc.frame[n:]
			}
			_, _ = d.Write([]byte("\n"))
			if len(rec.events) != before || slices.Contains(rec.events, "tool_end:open") {
				t.Fatalf("invalid large event changed lifecycle: %v", rec.events)
			}
			// Dropping or rejecting one frame must not poison later ordinary decoding.
			_, _ = d.Write([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"open","content":"ok"}]}}` + "\n"))
			d.flush()
			if !slices.Contains(rec.events, "tool_end:open") {
				t.Fatalf("ordinary result after invalid large event did not close tool: %v", rec.events)
			}
			if len(out.String()+tail.String()) > 512 {
				t.Fatalf("invalid large event produced unbounded output: %d bytes", len(out.String()+tail.String()))
			}
		})
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
