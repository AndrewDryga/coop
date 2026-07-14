package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestIterationCommandStreamFlags(t *testing.T) {
	cases := []struct {
		agent string
		base  []string
		want  []string
	}{
		{
			agent: "claude",
			base:  []string{"claude", "-p", "prompt"},
			want:  []string{"claude", "-p", "prompt", "--output-format", "stream-json", "--verbose"},
		},
		{
			agent: "codex",
			base:  []string{"codex", "exec", "--model", "gpt-5.6", "prompt"},
			want:  []string{"codex", "exec", "--model", "gpt-5.6", "--json", "prompt"},
		},
		{
			agent: "gemini",
			base:  []string{"gemini", "--yolo", "--model", "pro", "-p", "prompt"},
			want:  []string{"gemini", "--yolo", "--model", "pro", "-o", "stream-json", "-p", "prompt"},
		},
		{
			agent: "grok",
			base:  []string{"grok", "--permission-mode", "bypassPermissions", "-p", "prompt"},
			want:  []string{"grok", "--permission-mode", "bypassPermissions", "--output-format", "streaming-json", "-p", "prompt"},
		},
	}
	custom := []string{"codex", "exec", "--json", "stream-json", "streaming-json"}
	for _, c := range cases {
		t.Run(c.agent+"/tty", func(t *testing.T) {
			got, streaming := iterationCommand(c.agent, c.base, nil, true)
			if !slices.Equal(got, c.want) {
				t.Errorf("iterationCommand() = %#v, want %#v", got, c.want)
			}
			if !streaming {
				t.Errorf("TTY built-in command did not enable streaming: %#v", got)
			}
		})
		t.Run(c.agent+"/non-tty", func(t *testing.T) {
			got, streaming := iterationCommand(c.agent, c.base, nil, false)
			if !slices.Equal(got, c.base) {
				t.Errorf("non-TTY command = %#v, want untouched %#v", got, c.base)
			}
			if streaming {
				t.Errorf("non-TTY command unexpectedly streams: %#v", got)
			}
		})
		t.Run(c.agent+"/custom", func(t *testing.T) {
			got, streaming := iterationCommand(c.agent, c.base, custom, true)
			if !slices.Equal(got, custom) {
				t.Errorf("custom command = %#v, want untouched %#v", got, custom)
			}
			if streaming {
				t.Errorf("custom command markers unexpectedly enabled streaming: %#v", got)
			}
		})
	}
}

func TestCodexStreamDecoder(t *testing.T) {
	lines := []string{
		`{"type":"thread.started","thread_id":"..."}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"/bin/bash -lc ls","aggregated_output":"","exit_code":null,"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"/bin/bash -lc ls","aggregated_output":"README.md\n","exit_code":0,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"I'm running the requested command."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":15880,"cached_input_tokens":9984,"output_tokens":5,"reasoning_output_tokens":0}}`,
		`not valid json`,
	}
	var out, tail bytes.Buffer
	d := newCodexStreamDecoder(&out, &tail, "codex", "work", "/repo", "gpt-5.6")
	writeSplit(t, d, strings.Join(lines, "\n")+"\n")
	d.flush()

	o := out.String()
	for _, want := range []string{
		"· using codex model gpt-5.6 profile work",
		"⚙ ls",
		"✦ I'm running the requested command.",
		"· 15.9k/5 tok",
		"not valid json",
	} {
		if !strings.Contains(o, want) {
			t.Errorf("rendered output missing %q\n--- got ---\n%s", want, o)
		}
	}
	if strings.Contains(o, `"type":"item`) {
		t.Errorf("raw Codex events leaked into output:\n%s", o)
	}
	if d.last == nil || d.last.InTok != 15880 || d.last.OutTok != 5 {
		t.Errorf("captured tally = %+v, want in 15880 (without cached subset) out 5", d.last)
	}
	tl := tail.String()
	for _, want := range []string{"I'm running the requested command.", "not valid json"} {
		if !strings.Contains(tl, want) {
			t.Errorf("tail missing %q: %q", want, tl)
		}
	}
	if strings.Contains(tl, "README.md") {
		t.Errorf("tool output leaked into tail: %q", tl)
	}
}

func TestCodexStreamDecoderFailureAndUnknownItem(t *testing.T) {
	lines := []string{
		`{"type":"item.started","item":{"id":"bad","type":"command_execution","command":"/bin/bash -lc cd /repo && make check"}}`,
		`{"type":"item.completed","item":{"id":"bad","type":"command_execution","command":"/bin/bash -lc cd /repo && make check","aggregated_output":"compile failed\nmore detail","exit_code":1}}`,
		`{"type":"item.started","item":{"id":"future","type":"future_item"}}`,
		`{"type":"item.completed","item":{"id":"future","type":"future_item"}}`,
	}
	var out, tail bytes.Buffer
	d := newCodexStreamDecoder(&out, &tail, "codex", "", "/repo", "gpt-5.6")
	_, _ = d.Write([]byte(strings.Join(lines, "\n") + "\n"))
	d.flush()
	for _, want := range []string{"⚙ make check", "  ✗ make check: compile failed", "· future_item"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("rendered output missing %q: %q", want, out.String())
		}
	}
	if strings.Count(out.String(), "· future_item") != 1 {
		t.Errorf("paired unknown item should render once: %q", out.String())
	}
	if tail.Len() != 0 {
		t.Errorf("tool events leaked into tail: %q", tail.String())
	}
}

func TestCodexStreamDecoderNativeActivity(t *testing.T) {
	lines := []string{
		`{"type":"item.completed","item":{"id":"change","type":"file_change","changes":[{"path":"internal/cli/x.go","kind":"update"},{"path":"/repo/internal/cli/y.go","kind":"update"},{"path":" ","kind":"delete"}],"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"empty-change","type":"file_change","changes":[],"status":"completed"}}`,
		`{"type":"item.started","item":{"id":"search","type":"web_search","query":"Codex exec JSON schema","action":{}}}`,
		`{"type":"item.completed","item":{"id":"search","type":"web_search","query":"Codex exec JSON schema","action":{}}}`,
		`{"type":"item.completed","item":{"id":"empty-search","type":"web_search","query":"","action":{}}}`,
		`{"type":"item.started","item":{"id":"spawn","type":"collab_tool_call","tool":"spawn_agent","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"spawn","type":"collab_tool_call","tool":"spawn_agent","status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"send","type":"collab_tool_call","tool":"send_input","status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"wait","type":"collab_tool_call","tool":"wait","status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"close","type":"collab_tool_call","tool":"close_agent","status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"other","type":"collab_tool_call","tool":"review_thread","status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"empty-collab","type":"collab_tool_call","tool":"","status":"completed"}}`,
	}
	var out, tail bytes.Buffer
	d := newCodexStreamDecoder(&out, &tail, "codex", "", "/repo", "gpt-5.6")
	writeSplit(t, d, strings.Join(lines, "\n")+"\n")
	d.flush()

	for _, want := range []string{
		"✎ internal/cli/x.go",
		"✎ internal/cli/y.go",
		"✎ file change",
		"⌕ Codex exec JSON schema",
		"⌕ web search",
		"⇢ spawn agent",
		"⇢ send input",
		"⇢ wait",
		"⇢ close agent",
		"⇢ review thread",
		"⇢ collaboration",
	} {
		if count := strings.Count(out.String(), want); count != 1 {
			t.Errorf("rendered output count for %q = %d, want 1:\n%s", want, count, out.String())
		}
	}
	if strings.Contains(out.String(), "· file_change") || strings.Contains(out.String(), "· web_search") || strings.Contains(out.String(), "· collab_tool_call") {
		t.Errorf("known Codex activity rendered as unknown:\n%s", out.String())
	}
	if tail.Len() != 0 {
		t.Errorf("activity events leaked into tail: %q", tail.String())
	}
}

func TestCodexStreamDecoderSuppressesLivePlanChurn(t *testing.T) {
	lines := []string{
		`{"type":"item.started","item":{"id":"todo","type":"todo_list","items":[{"text":"test","completed":false}]}}`,
		`{"type":"item.updated","item":{"id":"todo","type":"todo_list","items":[{"text":"test","completed":true}]}}`,
		`{"type":"item.completed","item":{"id":"todo","type":"todo_list","items":[{"text":"test","completed":true}]}}`,
		`{"type":"item.updated","item":{"id":"future-update","type":"future_item"}}`,
		`{"type":"item.started","item":{"id":"future","type":"future_item"}}`,
		`{"type":"item.completed","item":{"id":"future","type":"future_item"}}`,
	}
	var out, tail bytes.Buffer
	d := newCodexStreamDecoder(&out, &tail, "codex", "", "/repo", "gpt-5.6")
	_, _ = d.Write([]byte(strings.Join(lines, "\n") + "\n"))
	d.flush()

	if got, want := out.String(), "· future_item\n"; got != want {
		t.Errorf("rendered plan churn = %q, want only future item %q", got, want)
	}
	if tail.Len() != 0 {
		t.Errorf("plan churn leaked into tail: %q", tail.String())
	}
}

func TestCodexStreamDecoderTurnFailureReachesTail(t *testing.T) {
	var out, tail bytes.Buffer
	d := newCodexStreamDecoder(&out, &tail, "codex", "", "", "gpt-5.6")
	_, _ = d.Write([]byte(`{"type":"turn.failed","error":{"message":"usage limit reached"}}` + "\n"))
	if !strings.Contains(out.String(), "✗ usage limit reached") {
		t.Errorf("turn failure not rendered: %q", out.String())
	}
	if tail.String() != "usage limit reached\n" {
		t.Errorf("turn failure tail = %q, want exact message", tail.String())
	}
}

func TestGeminiStreamDecoder(t *testing.T) {
	lines := []string{
		`{"type":"init","timestamp":"...","session_id":"...","model":"auto"}`,
		`{"type":"message","timestamp":"...","role":"user","content":"<the whole prompt echoed>"}`,
		`{"type":"message","timestamp":"...","role":"system","content":"notice"}`,
		`{"type":"message","timestamp":"...","role":"assistant","content":"hi","delta":true}`,
		`{"type":"message","timestamp":"...","role":"assistant","content":" there","delta":true}`,
		`{"type":"tool_use","timestamp":"...","tool_name":"read_file","tool_id":"read_file__46lsuj6s","parameters":{"file_path":"README.md"}}`,
		`{"type":"tool_use","timestamp":"...","tool_name":"run_shell_command","tool_id":"run_shell_command__jhb72zzg","parameters":{"description":"List files...","command":"ls"}}`,
		`{"type":"tool_result","timestamp":"...","tool_id":"read_file__46lsuj6s","status":"success","output":""}`,
		`{"type":"tool_result","timestamp":"...","tool_id":"run_shell_command__jhb72zzg","status":"success","output":"README.md"}`,
		`{"type":"future_event"}`,
		`{"type":"result","timestamp":"...","status":"success","stats":{"total_tokens":24652,"input_tokens":23678,"output_tokens":102,"cached":8114,"input":15564,"duration_ms":10916,"tool_calls":2,"models":{}}}`,
		`not valid json`,
	}
	var out, tail bytes.Buffer
	d := newGeminiStreamDecoder(&out, &tail, "gemini", "personal", "/repo", "gemini-3.5-pro")
	writeSplit(t, d, strings.Join(lines, "\n")+"\n")
	d.flush()

	o := out.String()
	for _, want := range []string{
		"· using gemini model gemini-3.5-pro profile personal",
		"· message system",
		"✦ hi there",
		"▸ README.md",
		"⚙ ls",
		"· future_event",
		"· 11s · 23.7k/102 tok",
		"not valid json",
	} {
		if !strings.Contains(o, want) {
			t.Errorf("rendered output missing %q\n--- got ---\n%s", want, o)
		}
	}
	if strings.Count(o, "✦") != 1 {
		t.Errorf("assistant deltas should coalesce to one line:\n%s", o)
	}
	if strings.Contains(o, "<the whole prompt echoed>") || strings.Contains(tail.String(), "<the whole prompt echoed>") {
		t.Errorf("Gemini user prompt echo was rendered\nout=%q\ntail=%q", o, tail.String())
	}
	if d.last == nil || d.last.DurationMS != 10916 || d.last.InTok != 23678 || d.last.OutTok != 102 {
		t.Errorf("captured tally = %+v, want duration 10916 in 23678 out 102", d.last)
	}
	tl := tail.String()
	for _, want := range []string{"hi there", "not valid json"} {
		if !strings.Contains(tl, want) {
			t.Errorf("tail missing %q: %q", want, tl)
		}
	}
	if strings.Contains(tl, "README.md") {
		t.Errorf("tool output leaked into tail: %q", tl)
	}
}

func TestGeminiStreamDecoderFailures(t *testing.T) {
	lines := []string{
		`{"type":"tool_use","tool_name":"read_file","tool_id":"read","parameters":{"file_path":"README.md"}}`,
		`{"type":"tool_result","tool_id":"read","status":"error","output":"permission denied\nmore"}`,
		`{"type":"error","severity":"error","message":"usage limit reached"}`,
		`{"type":"result","status":"error","error":{"type":"fatal","message":"quota exhausted"},"stats":{"input_tokens":12,"output_tokens":3,"duration_ms":1500}}`,
	}
	var out, tail bytes.Buffer
	d := newGeminiStreamDecoder(&out, &tail, "gemini", "", "/repo", "gemini-3.5-pro")
	_, _ = d.Write([]byte(strings.Join(lines, "\n") + "\n"))
	d.flush()
	for _, want := range []string{"  ✗ read_file README.md: permission denied", "✗ usage limit reached", "· 2s · 12/3 tok", "✗ quota exhausted"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("rendered output missing %q: %q", want, out.String())
		}
	}
	if strings.Contains(tail.String(), "permission denied") {
		t.Errorf("tool failure leaked into tail: %q", tail.String())
	}
	for _, want := range []string{"usage limit reached", "quota exhausted"} {
		if !strings.Contains(tail.String(), want) {
			t.Errorf("terminal error missing from tail: %q", tail.String())
		}
	}
}

func TestGeminiStderrFilter(t *testing.T) {
	blob := strings.Join([]string{
		"before",
		geminiColorWarning,
		"real error",
		geminiYOLOWarning,
		geminiYOLOWarning + " extra",
		"after",
	}, "\n")
	var out bytes.Buffer
	f := newGeminiStderrFilter(&out)
	cut := len(blob) / 2
	if _, err := f.Write([]byte(blob[:cut])); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(blob[cut:])); err != nil {
		t.Fatal(err)
	}
	if err := f.flush(); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{"before", "real error", geminiYOLOWarning + " extra", "after"}, "\n")
	if out.String() != want {
		t.Errorf("filtered stderr = %q, want %q", out.String(), want)
	}

	var final bytes.Buffer
	f = newGeminiStderrFilter(&final)
	_, _ = f.Write([]byte(geminiColorWarning))
	if err := f.flush(); err != nil {
		t.Fatal(err)
	}
	if final.Len() != 0 {
		t.Errorf("final noise line without newline was not dropped: %q", final.String())
	}
}

func TestCodexStderrFilter(t *testing.T) {
	routerLine := "2026-07-13T23:00:00Z ERROR codex_core::tools::router: failed to route tool\n"
	nearMiss := "WARN codex_core::tools::router: retrying\r\n"
	blob := "before\r\n" + routerLine + nearMiss + "real error\nfinal without newline"
	var out bytes.Buffer
	f := newCodexStderrFilter(&out)
	cut := strings.Index(blob, codexRouterError) + len("ERROR codex_core::tools")
	if _, err := f.Write([]byte(blob[:cut])); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(blob[cut:])); err != nil {
		t.Fatal(err)
	}
	if err := f.flush(); err != nil {
		t.Fatal(err)
	}
	want := "before\r\n" + nearMiss + "real error\nfinal without newline"
	if out.String() != want {
		t.Errorf("filtered stderr = %q, want %q", out.String(), want)
	}

	var final bytes.Buffer
	f = newCodexStderrFilter(&final)
	unterminated := "timestamp " + codexRouterError + " final diagnostic"
	cut = strings.Index(unterminated, codexRouterError) + len("ERROR codex_core")
	if _, err := f.Write([]byte(unterminated[:cut])); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(unterminated[cut:])); err != nil {
		t.Fatal(err)
	}
	if err := f.flush(); err != nil {
		t.Fatal(err)
	}
	if final.Len() != 0 {
		t.Errorf("final router error without newline was not dropped: %q", final.String())
	}
}

func TestGrokStreamDecoder(t *testing.T) {
	lines := []string{
		`{"type":"thought","data":"The"}`,
		`{"type":"thought","data":" user"}`,
		`{"type":"text","data":"hi"}`,
		`{"type":"text","data":" there"}`,
		`{"type":"future_event"}`,
		`{"type":"end","stopReason":"EndTurn","sessionId":"...","requestId":"...","usage":{"input_tokens":16016,"cache_read_input_tokens":11264,"output_tokens":125,"reasoning_tokens":62,"total_tokens":27467},"num_turns":1,"modelUsage":{"grok-4.5":{}}}`,
		`not valid json`,
	}
	var out, tail bytes.Buffer
	d := newGrokStreamDecoder(&out, &tail, "grok", "work", "/repo", "grok-4.5")
	writeSplit(t, d, strings.Join(lines, "\n")+"\n")
	d.flush()

	o := out.String()
	for _, want := range []string{
		"· using grok model grok-4.5 profile work",
		"✦ hi there",
		"· future_event",
		"· 1 turns · 27.3k/187 tok",
		"not valid json",
	} {
		if !strings.Contains(o, want) {
			t.Errorf("rendered output missing %q\n--- got ---\n%s", want, o)
		}
	}
	if strings.Contains(o, "The") || strings.Contains(o, " user") {
		t.Errorf("Grok thought deltas should be hidden:\n%s", o)
	}
	if strings.Count(o, "✦") != 1 {
		t.Errorf("text deltas should coalesce to one line:\n%s", o)
	}
	if d.last == nil || d.last.Turns != 1 || d.last.InTok != 27280 || d.last.OutTok != 187 {
		t.Errorf("captured tally = %+v, want turns 1 in 27280 out 187", d.last)
	}
	for _, want := range []string{"hi there", "not valid json"} {
		if !strings.Contains(tail.String(), want) {
			t.Errorf("tail missing %q: %q", want, tail.String())
		}
	}
	if strings.Contains(tail.String(), "The") || strings.Contains(tail.String(), " user") {
		t.Errorf("thought deltas leaked into tail: %q", tail.String())
	}
}

func TestGrokStreamDecoderErrorReachesTail(t *testing.T) {
	line := `{"type":"error","message":"Couldn't start session: usage limit"}`
	var out, tail bytes.Buffer
	d := newGrokStreamDecoder(&out, &tail, "grok", "", "", "grok-4.5")
	_, _ = d.Write([]byte(line + "\n"))
	if !strings.Contains(out.String(), line) {
		t.Errorf("Grok error event not passed through: %q", out.String())
	}
	if tail.String() != line+"\n" {
		t.Errorf("Grok error tail = %q, want raw event", tail.String())
	}
}

func TestBufferedStreamTextFlushesBeforeRawLine(t *testing.T) {
	t.Run("gemini", func(t *testing.T) {
		var out, tail bytes.Buffer
		d := newGeminiStreamDecoder(&out, &tail, "gemini", "", "", "")
		lines := []string{
			`{"type":"message","role":"assistant","content":"hi","delta":true}`,
			`diagnostic`,
			`{"type":"tool_use","tool_name":"run_shell_command","tool_id":"shell","parameters":{"command":"ls"}}`,
		}
		_, _ = d.Write([]byte(strings.Join(lines, "\n") + "\n"))
		if out.String() != "✦ hi\ndiagnostic\n⚙ ls\n" {
			t.Errorf("Gemini raw-line ordering = %q", out.String())
		}
		if tail.String() != "hi\ndiagnostic\n" {
			t.Errorf("Gemini raw-line tail = %q", tail.String())
		}
	})

	t.Run("grok", func(t *testing.T) {
		var out, tail bytes.Buffer
		d := newGrokStreamDecoder(&out, &tail, "grok", "", "", "")
		lines := []string{
			`{"type":"text","data":"hi"}`,
			`diagnostic`,
			`{"type":"end","usage":{},"num_turns":0}`,
		}
		_, _ = d.Write([]byte(strings.Join(lines, "\n") + "\n"))
		want := "· using grok model default\n✦ hi\ndiagnostic\n· 0 turns · 0/0 tok\n"
		if out.String() != want {
			t.Errorf("Grok raw-line ordering = %q, want %q", out.String(), want)
		}
		if tail.String() != "hi\ndiagnostic\n" {
			t.Errorf("Grok raw-line tail = %q", tail.String())
		}
	})
}

func TestIterationStreamDecoderDispatch(t *testing.T) {
	var out, tail bytes.Buffer
	cases := []struct {
		agent string
		check func(iterationStreamDecoder) bool
	}{
		{"claude", func(d iterationStreamDecoder) bool { _, ok := d.(*streamDecoder); return ok }},
		{"codex", func(d iterationStreamDecoder) bool { _, ok := d.(*codexStreamDecoder); return ok }},
		{"gemini", func(d iterationStreamDecoder) bool { _, ok := d.(*geminiStreamDecoder); return ok }},
		{"grok", func(d iterationStreamDecoder) bool { _, ok := d.(*grokStreamDecoder); return ok }},
	}
	for _, c := range cases {
		t.Run(c.agent, func(t *testing.T) {
			d := newIterationStreamDecoder(c.agent, &out, &tail, "", "", "model")
			if !c.check(d) {
				t.Errorf("dispatch for %s returned %T", c.agent, d)
			}
		})
	}
	if d := newIterationStreamDecoder("other", &out, &tail, "", "", "model"); d != nil {
		t.Errorf("unknown provider dispatch returned %T, want nil", d)
	}
}

func TestProviderStreamDecoderDefaultModelLine(t *testing.T) {
	cases := []struct {
		name  string
		write func(*bytes.Buffer)
	}{
		{
			"codex",
			func(out *bytes.Buffer) {
				d := newCodexStreamDecoder(out, nil, "codex", "", "", "")
				_, _ = d.Write([]byte(`{"type":"thread.started"}` + "\n"))
			},
		},
		{
			"gemini",
			func(out *bytes.Buffer) {
				d := newGeminiStreamDecoder(out, nil, "gemini", "", "", "")
				_, _ = d.Write([]byte(`{"type":"init"}` + "\n"))
			},
		},
		{
			"grok",
			func(out *bytes.Buffer) {
				d := newGrokStreamDecoder(out, nil, "grok", "", "", "")
				_, _ = d.Write([]byte(`{"type":"thought","data":"hidden"}` + "\n"))
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			c.write(&out)
			if !strings.Contains(out.String(), "· using "+c.name+" model default") {
				t.Errorf("default model line missing: %q", out.String())
			}
		})
	}
}

func writeSplit(t *testing.T, w interface{ Write([]byte) (int, error) }, blob string) {
	t.Helper()
	cut := len(blob) / 2
	if _, err := w.Write([]byte(blob[:cut])); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(blob[cut:])); err != nil {
		t.Fatal(err)
	}
}
