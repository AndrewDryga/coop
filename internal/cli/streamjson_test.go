package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestStreamDecoder(t *testing.T) {
	// Representative events from a real `claude -p --output-format stream-json --verbose` run.
	lines := []string{
		`{"type":"system","subtype":"init","model":"claude-opus-4-8"}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1781877000,"rateLimitType":"five_hour"}}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"secret reasoning"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"echo hi","description":"x"}}]}}`,
		`{"type":"user","message":{"content":[{"tool_use_id":"t1","type":"tool_result","content":"429 too many requests","is_error":false}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"working on task 9"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":".agent/TASKS.test.md"}}]}}`,
		`{"type":"user","message":{"content":[{"tool_use_id":"t2","type":"tool_result","content":"could not find string to replace","is_error":true}]}}`,
		`not valid json`,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":2,"duration_ms":8269,"total_cost_usd":0.1117,"result":"done"}`,
	}
	var out, tail bytes.Buffer
	d := newStreamDecoder(&out, &tail)
	// Feed in two chunks split mid-line to exercise the partial-line buffer.
	blob := strings.Join(lines, "\n") + "\n"
	cut := len(blob) / 2
	_, _ = d.Write([]byte(blob[:cut]))
	_, _ = d.Write([]byte(blob[cut:]))
	d.flush()

	o := out.String()
	for _, want := range []string{"⚙ Bash", "echo hi", "working on task 9", "✎ Edit", ".agent/TASKS.test.md", "✗", "could not find string", "· 2 turns", "$0.11", "not valid json"} {
		if !strings.Contains(o, want) {
			t.Errorf("rendered output missing %q\n--- got ---\n%s", want, o)
		}
	}
	// Events are decoded, not dumped as raw JSON; thinking is hidden.
	if strings.Contains(o, `"type":"assistant"`) {
		t.Errorf("raw JSON leaked into output:\n%s", o)
	}
	if strings.Contains(o, "secret reasoning") {
		t.Errorf("thinking should be hidden:\n%s", o)
	}

	tl := tail.String()
	// Assistant text and the final result reach the limit-detection tail...
	for _, want := range []string{"working on task 9", "done"} {
		if !strings.Contains(tl, want) {
			t.Errorf("tail missing %q\n--- tail ---\n%s", want, tl)
		}
	}
	// ...but tool output does NOT — otherwise a tool printing "429" would false-trip the
	// rate-limit detector.
	if strings.Contains(tl, "429") {
		t.Errorf("tool output leaked into limit-detection tail:\n%s", tl)
	}
}

func TestStreamDecoderRateLimit(t *testing.T) {
	now := time.Now()
	// A blocking rate_limit_event is translated into the text detectLimit understands, with the
	// reset epoch, so the loop waits until then instead of failing the run.
	var out, tail bytes.Buffer
	d := newStreamDecoder(&out, &tail)
	_, _ = d.Write([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1781877000,"rateLimitType":"five_hour"}}` + "\n"))
	if !strings.Contains(tail.String(), "usage limit reached|1781877000") {
		t.Fatalf("blocking limit not written to tail: %q", tail.String())
	}
	if hint := detectLimit(tail.String(), now); !hint.limited || !hint.resetAt.Equal(time.Unix(1781877000, 0)) {
		t.Errorf("detectLimit on translated notice = %+v, want limited at 1781877000", hint)
	}
	if !strings.Contains(out.String(), "rate limited") {
		t.Errorf("blocking limit should render to the user: %q", out.String())
	}

	// Informational statuses every run emits must not trip the detector.
	for _, st := range []string{"allowed", "warning", "queued"} {
		var o, tl bytes.Buffer
		nd := newStreamDecoder(&o, &tl)
		_, _ = nd.Write([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"` + st + `","resetsAt":1781877000,"rateLimitType":"five_hour"}}` + "\n"))
		if detectLimit(tl.String(), now).limited {
			t.Errorf("status %q should not trip the limit detector (tail=%q)", st, tl.String())
		}
	}
}

func TestBlockingLimitStatus(t *testing.T) {
	for _, s := range []string{"blocked", "rejected", "exhausted", "throttled"} {
		if !blockingLimitStatus(s) {
			t.Errorf("%q should be blocking", s)
		}
	}
	for _, s := range []string{"allowed", "warning", "queued", ""} {
		if blockingLimitStatus(s) {
			t.Errorf("%q should not be blocking", s)
		}
	}
}
