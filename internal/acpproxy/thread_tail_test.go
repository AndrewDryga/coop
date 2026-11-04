package acpproxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/threadtext"
)

func tailChunk(session, kind, text string) []byte {
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
		"sessionId": session, "update": map[string]any{"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": text}},
	}})
	return append(line, '\n')
}

// tailText returns the text of the only update in line.
func tailText(t *testing.T, line []byte) string {
	t.Helper()
	var m struct {
		Params struct {
			Update struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("%v: %s", err, line)
	}
	return m.Params.Update.Content.Text
}

// A Coop message opens with a rule exactly when an editor would run it on from agent text.
func TestThreadTailSetsANoticeApartFromAgentText(t *testing.T) {
	agent := tailChunk("S", "agent_message_chunk", "the reply")
	for _, tc := range []struct {
		name   string
		before func(*threadTail)
		rule   bool
	}{
		{"an empty thread", func(*threadTail) {}, false},
		{"after agent text", func(w *threadTail) { _, _ = w.Write(agent) }, true},
		{"after another notice", func(w *threadTail) { _, _ = w.Write(SessionMessageLine("S", "first")) }, true},
		{"after an empty agent chunk", func(w *threadTail) { _, _ = w.Write(tailChunk("S", "agent_message_chunk", " \n")) }, false},
		{"after a replayed person's message", func(w *threadTail) {
			_, _ = w.Write(agent)
			_, _ = w.Write(tailChunk("S", "user_message_chunk", "hi"))
		}, false},
		{"after a tool call", func(w *threadTail) {
			_, _ = w.Write(agent)
			_, _ = w.Write(tailChunk("S", "tool_call", ""))
		}, false},
		{"after settings", func(w *threadTail) {
			_, _ = w.Write(agent)
			_, _ = w.Write(tailChunk("S", "config_option_update", ""))
		}, true},
		{"after the person sends a message, or reopens the thread", func(w *threadTail) {
			_, _ = w.Write(agent)
			w.reset("S")
		}, false},
		{"after agent text in another thread", func(w *threadTail) { _, _ = w.Write(tailChunk("other", "agent_message_chunk", "x")) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			tail := newThreadTail(&out)
			tc.before(tail)
			out.Reset()
			if _, err := tail.Write(SessionMessageLine("S", "**Notice.**")); err != nil {
				t.Fatal(err)
			}
			text := tailText(t, out.Bytes())
			threadtext.Check(t, text)
			if got := strings.HasPrefix(text, "\n\n---\n\n**Notice.**"); got != tc.rule {
				t.Fatalf("rule = %v, want %v: %q", got, tc.rule, text)
			}
			if !tc.rule && !strings.HasPrefix(text, "\n\n**Notice.**") {
				t.Fatalf("text changed: %q", text)
			}
		})
	}
}

// Only Coop's own lines change, and a Write of several lines keeps them in order. A box's chunk that looks
// like one is left alone, as is what is not a session update.
func TestThreadTailLeavesEverythingElseAlone(t *testing.T) {
	var out bytes.Buffer
	tail := newThreadTail(&out)
	agent := tailChunk("S", "agent_message_chunk", "\n\nfirst")
	answer := []byte(`{"jsonrpc":"2.0","id":1,"result":{"text":"\n\nx"}}` + "\n")
	broken := []byte("{not json \"session/update\"\n")
	lines := bytes.Join([][]byte{agent, agent, answer, broken}, nil)
	if n, err := tail.Write(lines); err != nil || n != len(lines) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !bytes.Equal(out.Bytes(), lines) {
		t.Fatalf("got %s\nwant %s", out.Bytes(), lines)
	}
	out.Reset()
	notice := SessionMessageLine("S", "said")
	together := append(append([]byte(nil), notice...), agent...)
	if _, err := tail.Write(together); err != nil {
		t.Fatal(err)
	}
	got := bytes.SplitAfter(out.Bytes(), []byte("\n"))
	if text := tailText(t, got[0]); text != "\n\n---\n\nsaid\n\n" {
		t.Fatalf("the notice = %q", text)
	}
	if !bytes.Equal(got[1], agent) {
		t.Fatalf("the agent's line = %s", got[1])
	}
}

// Through the real proxy: what the hook says about a reopen comes before the request goes on, and a
// later message, from wherever it comes, is set apart from the reply above it.
func TestProxySpeaksFirstOnAReopenAndSetsNoticesApart(t *testing.T) {
	notice := SessionMessageLine("S1", "late")
	hooks := &Hooks{
		FromEditor: func(line []byte) (bool, []byte, []byte, bool) {
			if parse(line).Method == "session/load" {
				return false, SessionMessageLine("S1", "top"), nil, false
			}
			return false, nil, nil, false
		},
		ToEditor: func(line []byte) ([]byte, bool) {
			if bytes.Contains(line, []byte("the reply")) {
				return append(append([]byte(nil), line...), notice...), false
			}
			return line, false
		},
	}
	h := newProxyHarness(t, 1, hooks)
	h.initialize(0)
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":2,"method":"session/load","params":{"sessionId":"S1","cwd":"/w","mcpServers":[]}}`)
	if top := readLine(t, h.clientOut); tailText(t, top) != "\n\ntop\n\n" {
		t.Fatalf("before the load is forwarded, the thread heard %s", top)
	}
	if forwarded := parse(readLine(t, h.childIn[0])); forwarded.Method != "session/load" {
		t.Fatalf("the box got %q", forwarded.Method)
	}
	writeLine(t, h.children[0].outW, string(bytes.TrimSpace(tailChunk("S1", "user_message_chunk", "hi"))))
	readLine(t, h.clientOut)
	writeLine(t, h.children[0].outW, string(bytes.TrimSpace(tailChunk("S1", "agent_message_chunk", "the reply"))))
	readLine(t, h.clientOut)
	if late := readLine(t, h.clientOut); tailText(t, late) != "\n\n---\n\nlate\n\n" {
		t.Fatalf("the late message = %s", late)
	}
}
