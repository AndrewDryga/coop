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
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":".agent/tasks/10_in_progress/2026-06-26-egress/task.md"}}]}}`,
		`{"type":"user","message":{"content":[{"tool_use_id":"t2","type":"tool_result","content":"could not find string to replace","is_error":true}]}}`,
		`not valid json`,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":2,"duration_ms":8269,"total_cost_usd":0.1117,"result":"done"}`,
	}
	var out, tail bytes.Buffer
	d := newStreamDecoder(&out, &tail, "", "", "")
	// Feed in two chunks split mid-line to exercise the partial-line buffer.
	blob := strings.Join(lines, "\n") + "\n"
	cut := len(blob) / 2
	_, _ = d.Write([]byte(blob[:cut]))
	_, _ = d.Write([]byte(blob[cut:]))
	d.flush()

	o := out.String()
	for _, want := range []string{"· model claude-opus-4-8", "⚙ Bash", "echo hi", "✦ working on task 9", "✎ Edit", ".agent/tasks/10_in_progress/2026-06-26-egress/task.md", "✗", "could not find string", "· 2 turns", "$0.11", "not valid json"} {
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

// A result event with a usage block renders in/out tokens on the closing line and captures the
// cost/turns/token tally on the decoder for the loop's telemetry; a result WITHOUT usage still
// renders cost/turns and captures those with zero tokens (no crash, no "tok" on the line).
func TestStreamDecoderResultUsage(t *testing.T) {
	var out, tail bytes.Buffer
	d := newStreamDecoder(&out, &tail, "", "", "")
	// input 4243 + cache_creation 3630 + cache_read 15197 = 23070 in; 698 out.
	_, _ = d.Write([]byte(`{"type":"result","subtype":"success","num_turns":5,"duration_ms":1000,"total_cost_usd":1.23,"usage":{"input_tokens":4243,"cache_creation_input_tokens":3630,"cache_read_input_tokens":15197,"output_tokens":698}}` + "\n"))
	d.flush()
	o := out.String()
	for _, want := range []string{"· 5 turns", "$1.23", "23.1k/698 tok"} {
		if !strings.Contains(o, want) {
			t.Errorf("result line missing %q: %q", want, o)
		}
	}
	if d.last == nil {
		t.Fatal("decoder did not capture the result tally")
	}
	if d.last.CostUSD != 1.23 || d.last.Turns != 5 || d.last.InTok != 23070 || d.last.OutTok != 698 {
		t.Errorf("captured tally = %+v, want cost 1.23 turns 5 in 23070 out 698", d.last)
	}

	// No usage block: cost/turns still captured, tokens zero, and the line omits "tok".
	var out2, tail2 bytes.Buffer
	d2 := newStreamDecoder(&out2, &tail2, "", "", "")
	_, _ = d2.Write([]byte(`{"type":"result","subtype":"success","num_turns":2,"duration_ms":500,"total_cost_usd":0.05}` + "\n"))
	d2.flush()
	if strings.Contains(out2.String(), "tok") {
		t.Errorf("a no-usage result should omit tokens: %q", out2.String())
	}
	if d2.last == nil || d2.last.CostUSD != 0.05 || d2.last.InTok != 0 {
		t.Errorf("no-usage tally = %+v, want cost 0.05 in 0", d2.last)
	}
}

// The init model line names the agent and profile in play when given them, so a loop iteration
// shows "using claude model … profile personal"; with neither it falls back to "· model …".
func TestStreamDecoderModelLine(t *testing.T) {
	init := `{"type":"system","subtype":"init","model":"claude-opus-4-8[1m]"}` + "\n"
	var out, tail bytes.Buffer
	d := newStreamDecoder(&out, &tail, "claude", "personal", "")
	_, _ = d.Write([]byte(init))
	if got := out.String(); !strings.Contains(got, "· using claude model claude-opus-4-8[1m] profile personal") {
		t.Errorf("with agent+profile, model line = %q", got)
	}
	var out2, tail2 bytes.Buffer
	d2 := newStreamDecoder(&out2, &tail2, "", "", "")
	_, _ = d2.Write([]byte(init))
	if got := out2.String(); !strings.Contains(got, "· model claude-opus-4-8[1m]") {
		t.Errorf("without agent/profile, model line = %q (want the bare fallback)", got)
	}
}

func TestStreamDecoderRateLimit(t *testing.T) {
	now := time.Now()
	// A blocking rate_limit_event is translated into the text detectLimit understands, with the
	// reset epoch, so the loop waits until then instead of failing the run.
	var out, tail bytes.Buffer
	d := newStreamDecoder(&out, &tail, "", "", "")
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
		nd := newStreamDecoder(&o, &tl, "", "", "")
		_, _ = nd.Write([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"` + st + `","resetsAt":1781877000,"rateLimitType":"five_hour"}}` + "\n"))
		if detectLimit(tl.String(), now).limited {
			t.Errorf("status %q should not trip the limit detector (tail=%q)", st, tl.String())
		}
	}
}

func TestStreamDecoderModel(t *testing.T) {
	// The init event names the running model — rendered once, so a loop iteration shows
	// what's actually working. It never reaches the rate-limit tail (it's not human text).
	cases := []struct {
		name, line, want string
	}{
		{"init with model", `{"type":"system","subtype":"init","model":"claude-opus-4-8"}`, "· model claude-opus-4-8"},
		{"init without model", `{"type":"system","subtype":"init"}`, ""},
		{"non-init system", `{"type":"system","subtype":"compact_boundary","model":"x"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, tail bytes.Buffer
			d := newStreamDecoder(&out, &tail, "", "", "")
			_, _ = d.Write([]byte(c.line + "\n"))
			d.flush()
			if c.want == "" {
				if strings.TrimSpace(out.String()) != "" {
					t.Errorf("expected no output, got %q", out.String())
				}
			} else if !strings.Contains(out.String(), c.want) {
				t.Errorf("output %q missing %q", out.String(), c.want)
			}
			if tail.Len() != 0 {
				t.Errorf("system event leaked into rate-limit tail: %q", tail.String())
			}
		})
	}
}

func TestRepoRel(t *testing.T) {
	const root = "/home/u/proj"
	cases := []struct {
		name, root, path, wantRel string
		wantInside                bool
	}{
		{"inside, absolute", root, "/home/u/proj/internal/cli/streamjson.go", "internal/cli/streamjson.go", true},
		{"the repo root itself", root, "/home/u/proj", ".", true},
		{"outside, a sibling", root, "/home/u/other/secret.txt", "/home/u/other/secret.txt", false},
		{"outside, the parent", root, "/home/u", "/home/u", false},
		// A shared string prefix is NOT containment: /home/u/proj-x is outside /home/u/proj.
		{"outside, prefix-not-child", root, "/home/u/proj-x/f", "/home/u/proj-x/f", false},
		{"relative path left as-is", root, "internal/x.go", "internal/x.go", true},
		{"empty root disables", "", "/home/u/proj/x.go", "/home/u/proj/x.go", true},
		{"empty path", root, "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rel, inside := repoRel(c.root, c.path)
			if rel != c.wantRel || inside != c.wantInside {
				t.Errorf("repoRel(%q, %q) = (%q, %v), want (%q, %v)", c.root, c.path, rel, inside, c.wantRel, c.wantInside)
			}
		})
	}
}

// With a repo root set, a file tool inside the tree shows a repo-relative path (no mount
// prefix, no flag); one that escapes it keeps its full path and is flagged with a ⚠.
func TestStreamDecoderRepoPaths(t *testing.T) {
	const root = "/home/u/proj"
	cases := []struct {
		name, event, want string
		warn              bool
	}{
		{
			"in-repo path is relativized, not flagged",
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a","name":"Edit","input":{"file_path":"/home/u/proj/internal/cli/streamjson.go"}}]}}`,
			"internal/cli/streamjson.go", false,
		},
		{
			"out-of-repo path keeps its full path and is flagged",
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"b","name":"Read","input":{"file_path":"/etc/passwd"}}]}}`,
			"/etc/passwd", true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, tail bytes.Buffer
			d := newStreamDecoder(&out, &tail, "claude", "", root)
			_, _ = d.Write([]byte(c.event + "\n"))
			d.flush()
			o := out.String()
			if !strings.Contains(o, c.want) {
				t.Errorf("rendered line missing %q:\n%s", c.want, o)
			}
			if got := strings.Contains(o, "⚠"); got != c.warn {
				t.Errorf("outside-repo ⚠ flag = %v, want %v:\n%s", got, c.warn, o)
			}
			if !c.warn && strings.Contains(o, root) {
				t.Errorf("the repo mount prefix leaked into an in-repo line:\n%s", o)
			}
		})
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
