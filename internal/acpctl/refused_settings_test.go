package acpctl

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	"github.com/AndrewDryga/coop/internal/config"
)

// codexOptions is the part of a codex-acp session/new answer that Coop reads: its model and effort menus.
const codexOptions = `[{"id":"model","name":"Model","type":"select","currentValue":%q,"options":[{"value":"gpt-6.1-sol","name":"6.1 Sol"},{"value":"gpt-6-luna","name":"6 Luna"}]},` +
	`{"id":"reasoning_effort","name":"Reasoning effort","type":"select","currentValue":%q,"options":[{"value":"low","name":"Low"},{"value":"medium","name":"Medium"},{"value":"high","name":"High"}]}]`

// codexSession is codex-acp as an editor session sees it. It answers like the adapter does (a setting it
// does not know, or a value it does not list, is -32602 "Invalid params"; a cancelled turn ends
// "cancelled"), and it keeps what it was sent, in order.
type codexSession struct {
	mu            sync.Mutex
	received      []string
	promptSeen    chan struct{}
	model, effort string
}

func (s *codexSession) saw() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.received)
}

func (s *codexSession) options() string {
	return fmt.Sprintf(codexOptions, s.model, s.effort)
}

func (s *codexSession) serve(in io.Reader, out io.Writer) {
	lines := bufio.NewScanner(in)
	lines.Buffer(nil, 1<<20)
	var open json.RawMessage // the prompt that is running
	prompts := 0
	answer := func(id json.RawMessage, result string) {
		fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", id, result)
	}
	for lines.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ConfigID string `json:"configId"`
				Value    string `json:"value"`
				Prompt   []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			} `json:"params"`
		}
		if json.Unmarshal(lines.Bytes(), &m) != nil {
			continue
		}
		summary := m.Method
		switch m.Method {
		case "session/set_config_option":
			summary += " " + m.Params.ConfigID + "=" + m.Params.Value
		case "session/prompt":
			summary += " " + m.Params.Prompt[0].Text
		}
		s.mu.Lock()
		s.received = append(s.received, summary)
		s.mu.Unlock()
		switch m.Method {
		case "initialize":
			answer(m.ID, `{"protocolVersion":1,"agentInfo":{"name":"@agentclientprotocol/codex-acp","version":"2.6.0"},"agentCapabilities":{},"authMethods":[]}`)
		case "session/new":
			answer(m.ID, fmt.Sprintf(`{"sessionId":"S","configOptions":%s}`, s.options()))
		case "session/set_config_option":
			known := map[string][]string{"model": {"gpt-6.1-sol", "gpt-6-luna"}, "reasoning_effort": {"low", "medium", "high"}}
			if !slices.Contains(known[m.Params.ConfigID], m.Params.Value) {
				fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"Invalid params"}}`+"\n", m.ID)
				continue
			}
			if m.Params.ConfigID == "model" {
				s.model = m.Params.Value
			} else {
				s.effort = m.Params.Value
			}
			answer(m.ID, fmt.Sprintf(`{"configOptions":%s}`, s.options()))
		case "session/prompt":
			if prompts++; prompts > 1 {
				// The turn after the cancel: the agent works, with the tool it was given.
				fmt.Fprintln(out, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S","update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Read file notes.txt","kind":"read","status":"pending"}}}`)
				answer(m.ID, `{"stopReason":"end_turn"}`)
				continue
			}
			open = m.ID // the first turn runs until the editor cancels it
			close(s.promptSeen)
		case "session/cancel":
			if open != nil {
				answer(open, `{"stopReason":"cancelled"}`)
				open = nil
			}
		}
	}
}

// An editor at the protocol: it sends lines and waits for the answer to one request, or for an update.
type scriptedEditor struct {
	t       *testing.T
	in      io.Writer
	mu      sync.Mutex
	lines   []string
	changed chan struct{}
}

func (e *scriptedEditor) send(line string) {
	e.t.Helper()
	if _, err := fmt.Fprintln(e.in, line); err != nil {
		e.t.Fatal(err)
	}
}

func (e *scriptedEditor) read(r io.Reader) {
	lines := bufio.NewScanner(r)
	lines.Buffer(nil, 1<<20)
	for lines.Scan() {
		e.mu.Lock()
		e.lines = append(e.lines, lines.Text())
		e.mu.Unlock()
		select {
		case e.changed <- struct{}{}:
		default:
		}
	}
}

// wait returns the first line, from index `from` on, that holds `want`.
func (e *scriptedEditor) wait(want string, from int) (line string, at int) {
	e.t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		e.mu.Lock()
		for i := from; i < len(e.lines); i++ {
			if strings.Contains(e.lines[i], want) {
				line, at = e.lines[i], i
				e.mu.Unlock()
				return line, at
			}
		}
		e.mu.Unlock()
		select {
		case <-e.changed:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			e.t.Fatalf("timed out waiting for %s; the editor has: %q", want, e.snapshot())
		}
	}
}

func (e *scriptedEditor) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.lines)
}

// settings is what a list of target-setting requests asks for, without their request ids.
func settings(t *testing.T, requests [][]byte) []string {
	t.Helper()
	var out []string
	for _, request := range requests {
		var m struct {
			Method string `json:"method"`
			Params struct {
				ConfigID string `json:"configId"`
				Value    string `json:"value"`
			} `json:"params"`
		}
		if err := json.Unmarshal(request, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m.Method+" "+m.Params.ConfigID+"="+m.Params.Value)
	}
	return out
}

// QA saw a Codex editor session answer "I can't read files" with no tool call after two refused setting
// changes and a cancelled turn (2026-11-06). Replaying that sequence through the real proxy and control
// shows what Coop owns of it: the adapter is sent exactly what the editor sent, in that order. The idle
// cancel never reaches it, the refusals are the adapter's own answers, nothing restarts or re-creates the
// session, and a refused setting changes nothing Coop would apply on a restart. Codex's own request trace
// showed the tools in that turn's request too, and a cancel with no refusal gives the same answer, so there
// was nothing here to restore.
func TestRefusedSettingsAndACancelReachTheAdapterAsTheEditorSentThem(t *testing.T) {
	c := New(&config.Config{ConfigDir: t.TempDir()}, "codex", "gpt-6-luna", "", t.TempDir(),
		Selection{Provider: "codex", Account: "personal"}, nil, nil,
		withTestAccounts(testHost(), map[string][]string{"codex": {"personal", "emisar"}}))
	adapter := &codexSession{promptSeen: make(chan struct{}), model: "gpt-6.1-sol", effort: "low"}
	spawned := 0
	factory := func(context.Context) (*acpproxy.Child, error) {
		spawned++
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		go adapter.serve(inR, outW)
		return &acpproxy.Child{In: inW, Out: outR, Provider: "codex", Account: "personal", Stop: func() { outW.Close(); inR.Close() }}, nil
	}
	editorR, editorW := io.Pipe()
	clientR, clientW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- acpproxy.RunWith(context.Background(), editorR, clientW, factory, c.Hooks(), acpproxy.RunOpts{})
		clientW.Close()
	}()
	editor := &scriptedEditor{t: t, in: editorW, changed: make(chan struct{}, 1)}
	go editor.read(clientR)

	editor.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	editor.wait(`"id":1,`, 0)
	editor.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w","mcpServers":[]}}`)
	_, answered := editor.wait(`"id":2,`, 0)
	editor.wait(`config_option_update`, answered) // Coop's own model setting has been applied
	before := settings(t, c.sessionReady("S"))

	setting := func(id int, configID, value string) string {
		editor.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/set_config_option","params":{"sessionId":"S","configId":%q,"value":%q}}`, id, configID, value))
		line, _ := editor.wait(fmt.Sprintf(`"id":%d,`, id), 0)
		return line
	}
	const invalid = `"error":{"code":-32602,"message":"Invalid params"}`
	if line := setting(3, "reasoning_effort", "bogus-effort"); !strings.Contains(line, invalid) {
		t.Errorf("the refused effort reached the editor as %s, want the adapter's own error", line)
	}
	if line := setting(4, "nope_option", "x"); !strings.Contains(line, invalid) {
		t.Errorf("the unknown option reached the editor as %s, want the adapter's own error", line)
	}
	if got := settings(t, c.sessionReady("S")); !slices.Equal(got, before) {
		t.Errorf("a refused setting changed the target Coop applies on a restart: %q, was %q", got, before)
	}
	editor.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"S"}}`) // nothing is running
	if line := setting(5, "reasoning_effort", "low"); strings.Contains(line, `"error"`) {
		t.Errorf("a valid effort after the refusals was refused: %s", line)
	}
	editor.send(`{"jsonrpc":"2.0","id":6,"method":"session/prompt","params":{"sessionId":"S","prompt":[{"type":"text","text":"Reply with exactly: OK"}]}}`)
	select {
	case <-adapter.promptSeen:
	case <-time.After(30 * time.Second):
		t.Fatal("the first prompt never reached the adapter")
	}
	editor.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"S"}}`)
	if line, _ := editor.wait(`"id":6,`, 0); !strings.Contains(line, `"stopReason":"cancelled"`) {
		t.Errorf("the cancelled turn ended as %s", line)
	}
	editor.send(`{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{"sessionId":"S","prompt":[{"type":"text","text":"Read notes.txt"}]}}`)
	reply, at := editor.wait(`"id":7,`, 0)
	if !strings.Contains(reply, `"stopReason":"end_turn"`) {
		t.Errorf("the next turn ended as %s", reply)
	}
	if _, tool := editor.wait(`"sessionUpdate":"tool_call"`, 0); tool > at {
		t.Errorf("the tool call came after the turn's answer: %q", editor.snapshot())
	}

	want := []string{
		"initialize", "session/new",
		"session/set_config_option model=gpt-6-luna", // Coop's own, once, when the session began
		"session/set_config_option reasoning_effort=bogus-effort",
		"session/set_config_option nope_option=x",
		"session/set_config_option reasoning_effort=low",
		"session/prompt Reply with exactly: OK",
		"session/cancel",
		"session/prompt Read notes.txt",
	}
	if got := adapter.saw(); !slices.Equal(got, want) {
		t.Errorf("the adapter was sent\n  %q\nwant exactly what the editor sent, with Coop's one model setting\n  %q", got, want)
	}
	// A restart would apply the model and the effort the adapter accepted, and nothing it refused.
	if got, want := settings(t, c.sessionReady("S")), append(slices.Clone(before), "session/set_config_option reasoning_effort=low"); !slices.Equal(got, want) {
		t.Errorf("the target Coop applies on a restart is %q, want %q", got, want)
	}
	if spawned != 1 {
		t.Errorf("the box was started %d times, want once", spawned)
	}
	editorW.Close()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the proxy did not end")
	}
}
