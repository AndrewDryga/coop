package acpproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const editorOffersItsComputer = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientInfo":{"name":"zed"},` +
	`"clientCapabilities":{"fs":{"readTextFile":true,"writeTextFile":true},"terminal":true,"elicitation":{"form":{},"url":{}},"_meta":{"terminal_output":true}}}}`

const loginButton = `"authMethods":[{"id":"login","_meta":{"terminal-auth":{"command":"touch","args":["/tmp/forged"]}}}]`

// escaped is r as a JSON escape: the spelling a decoder reads as r and a byte search does not.
func escaped(r rune) string {
	return fmt.Sprintf("%cu%04x", 92, r)
}

// noHostAccessOffered fails unless a box was told about everything but the editor's computer.
func noHostAccessOffered(t *testing.T, line []byte) {
	t.Helper()
	if !bytes.HasSuffix(line, []byte("\n")) {
		t.Errorf("the rewritten initialize lost its line ending, which hangs the session: %q", line)
	}
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ClientInfo   map[string]string          `json:"clientInfo"`
			Capabilities map[string]json.RawMessage `json:"clientCapabilities"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &message); err != nil {
		t.Fatalf("%v: %s", err, line)
	}
	if message.Method != "initialize" || len(message.ID) == 0 || message.Params.ClientInfo["name"] != "zed" {
		t.Errorf("the rest of the question changed: %s", line)
	}
	for _, capability := range []string{"fs", "terminal"} {
		if _, offered := message.Params.Capabilities[capability]; offered {
			t.Errorf("the box was told the editor offers %s: %s", capability, line)
		}
	}
	if string(message.Params.Capabilities["_meta"]) != `{"terminal_output":true}` {
		t.Errorf("a capability with no way out was lost: %s", line)
	}
	if string(message.Params.Capabilities["elicitation"]) != `{"form":{}}` {
		t.Errorf("Claude's question tool needs forms, and a link the box chose must not open: %s", line)
	}
}

func TestFilteredBoxIsNotToldTheEditorCanReachTheHost(t *testing.T) {
	t.Run("the first box and its replacement", func(t *testing.T) {
		h := newProxyHarnessWith(t, 2, nil, RunOpts{NoEditorHostAccess: true})
		writeLine(t, h.clientIn, editorOffersItsComputer)
		noHostAccessOffered(t, readLine(t, h.childIn[0]))
		writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`)
		readLine(t, h.clientOut)
		_ = h.children[0].outW.Close()
		noHostAccessOffered(t, readLine(t, h.childIn[1]))
		_ = h.shutdown()
	})
	t.Run("a session resumed from before this rule", func(t *testing.T) {
		h := newProxyHarnessWith(t, 1, nil, RunOpts{NoEditorHostAccess: true, Resume: &Snapshot{Initialize: []byte(editorOffersItsComputer)}})
		replay := readLine(t, h.childIn[0])
		noHostAccessOffered(t, replay)
		writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":`+string(parse(replay).ID)+`,"result":{"protocolVersion":1}}`)
		_ = h.shutdown()
	})
}

// Anything in a box can write onto the adapter's stdout, so these are what a hostile program sends.
func TestFilteredBoxCannotAskTheEditorForTheHost(t *testing.T) {
	h := newProxyHarnessWith(t, 1, nil, RunOpts{NoEditorHostAccess: true})
	h.initialize(0)
	for _, forged := range []struct{ line, id string }{
		{`{"jsonrpc":"2.0","id":7,"method":"terminal/create","params":{"sessionId":"s","command":"touch","args":["/tmp/forged"]}}`, `7`},
		{`{"jsonrpc":"2.0","id":"a","method":"fs/read_text_file","params":{"sessionId":"s","path":"/etc/passwd"}}`, `"a"`},
		{`{"jsonrpc":"2.0","id":8,"method":"fs/write_text_file","params":{"sessionId":"s","path":"/tmp/forged","content":"x"}}`, `8`},
		{`{"jsonrpc":"2.0","id":9,"method":"terminal\/create","params":{}}`, `9`},
		{`{"jsonrpc":"2.0","id":10,"m` + escaped('e') + `thod":"terminal/create","params":{}}`, `10`},
		{`{"jsonrpc":"2.0","id":11,"method":"Terminal/Create","params":{}}`, `11`},
		// Nothing but what is listed goes through: a method ACP adds later, or an editor's own.
		{`{"jsonrpc":"2.0","id":12,"method":"mcp/connect","params":{}}`, `12`},
		{`{"jsonrpc":"2.0","id":13,"method":"_editor/run","params":{}}`, `13`},
		// A link the box chose, however it spells that.
		{`{"jsonrpc":"2.0","id":14,"method":"elicitation/create","params":{"mode":"url","url":"https://example.org/?data=x","elicitationId":"e"}}`, `14`},
		{`{"jsonrpc":"2.0","id":15,"method":"elicitation/create","params":{"mode":"form","mode":"url","url":"https://example.org/"}}`, `15`},
		{`{"jsonrpc":"2.0","id":16,"method":"elicitation/create","params":{"MODE":"form","mode":"url","url":"https://example.org/"}}`, `16`},
		{`{"jsonrpc":"2.0","id":19,"method":"elicitation/create","params":{"mode":"url","mode":"form","url":"https://example.org/"}}`, `19`},
		{`{"jsonrpc":"2.0","id":20,"method":"elicitation/create","params":{"MODE":"url","mode":"form","url":"https://example.org/"}}`, `20`},
		{`{"jsonrpc":"2.0","id":17,"method":"elicitation/create","params":{"url":"https://example.org/"}}`, `17`},
		{`{"jsonrpc":"2.0","id":18,"method":"elicitation/create"}`, `18`},
	} {
		writeLine(t, h.children[0].outW, forged.line)
		reply := parse(readLine(t, h.childIn[0]))
		if string(reply.ID) != forged.id || !strings.Contains(string(reply.Error), "cannot ask the editor") {
			t.Fatalf("%s was answered with %+v", forged.line, reply)
		}
	}
	allowed := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s"}}`
	dropped := strings.Join([]string{
		`{"jsonrpc":"2.0","method":"terminal/kill","params":{}}`,
		`{"jsonrpc":"2.0","method":"_editor/run","params":{}}`,
		`{"jsonrpc":"2.0","method":"elicitation/complete","params":{"elicitationId":"e"}}`,
		// Parsers read these differently, so no check of one parser could speak for the editor's.
		`{"jsonrpc":"2.0","id":30,"method":"terminal/create","METHOD":"session/update"}`,
		`{"jsonrpc":"2.0","id":21,"METHOD":"session/update","method":"terminal/create"}`,
		`{"jsonrpc":"2.0","id":22,"method":"terminal/create","method":"session/update"}`,
		`{"jsonrpc":"2.0","id":23,"method":"session/update","method":"terminal/create"}`,
		`{"jsonrpc":"2.0","id":24,"mEthod":"terminal/create"}`,
		`{"jsonrpc":"2.0","id":1,"RESULT":{"protocolVersion":1,` + loginButton + `}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,` + loginButton + `},"result":{}}`,
		`{"jsonrpc":"2.0","id":25,"method":5}`,
		`[{"jsonrpc":"2.0","id":26,"method":"terminal/create"}]`,
		`{"jsonrpc":"2.0","method":"session/update"} {"jsonrpc":"2.0","id":27,"method":"terminal/create"}`,
		// A carriage return is whitespace to JSON and the end of a line to some readers.
		"{\"note\":\r{\"jsonrpc\":\"2.0\",\"id\":28,\"method\":\"terminal/create\"}\r}",
		`null`,
		`"terminal/create"`,
		`not json`,
		``,
		allowed,
	}, "\n")
	writeLineAsync(h.children[0].outW, dropped)
	// Lines keep their order, so this is the first thing the editor saw and the first the box was told.
	if got := readLine(t, h.clientOut); strings.TrimSpace(string(got)) != allowed {
		t.Fatalf("the editor got %s, want only %s", got, allowed)
	}
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":"last","method":"terminal/create"}`)
	if reply := parse(readLine(t, h.childIn[0])); string(reply.ID) != `"last"` {
		t.Fatalf("a dropped line got an answer: %+v", reply)
	}
	_ = h.shutdown()
}

func TestFilteredBoxStillSaysAllAnAgentNeeds(t *testing.T) {
	h := newProxyHarnessWith(t, 1, nil, RunOpts{NoEditorHostAccess: true})
	h.initialize(0)
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w"}}`)
	readLine(t, h.childIn[0])
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"s"}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk"}}}`,
		`{"jsonrpc":"2.0","id":"p","method":"session/request_permission","params":{"sessionId":"s","options":[]}}`,
		`{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"no"},"note":"a key an editor ignores"}`,
		// Claude's question tool: all a form can do is ask the person.
		`{"jsonrpc":"2.0","id":"q","method":"elicitation/create","params":{"sessionId":"s","mode":"form","message":"Which one?","requestedSchema":{"type":"object"}}}`,
		"{\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"sessionId\":\"s\"}}\r",
	} {
		writeLine(t, h.children[0].outW, line)
		if got := readLine(t, h.clientOut); strings.TrimSpace(string(got)) != strings.TrimSpace(line) {
			t.Fatalf("the editor got %s, want %s unchanged", got, line)
		}
	}
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":"p","result":{"outcome":{"outcome":"cancelled"}}}`)
	if answer := parse(readLine(t, h.childIn[0])); string(answer.ID) != `"p"` {
		t.Fatalf("the box did not get the editor's answer: %+v", answer)
	}
	_ = h.shutdown()
}

// A login button runs its command on the editor's computer once someone presses it.
func TestFilteredBoxOffersTheEditorNoLoginButton(t *testing.T) {
	noButton := func(t *testing.T, line []byte) {
		t.Helper()
		var message struct {
			Result map[string]json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(line, &message); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		if string(message.Result["authMethods"]) != `[]` || bytes.Contains(line, []byte("terminal-auth")) {
			t.Fatalf("the editor was offered a login: %s", line)
		}
		if !bytes.HasSuffix(line, []byte("\n")) {
			t.Fatalf("the rewritten answer lost its line ending: %q", line)
		}
	}
	for name, result := range map[string]string{
		"as an adapter writes it":     `{"protocolVersion":1,` + loginButton + `}`,
		"with its name in escapes":    `{"protocolVersion":1,` + strings.Replace(loginButton, "authMethods", "auth"+escaped('M')+"ethods", 1) + `}`,
		"after a harmless first copy": `{"protocolVersion":1,"authMethods":[],` + loginButton + `}`,
		"with no protocol version":    `{` + loginButton + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newProxyHarnessWith(t, 1, nil, RunOpts{NoEditorHostAccess: true})
			writeLine(t, h.clientIn, editorOffersItsComputer)
			readLine(t, h.childIn[0])
			writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":1,"result":`+result+`}`)
			noButton(t, readLine(t, h.clientOut))
			_ = h.shutdown()
		})
	}
	t.Run("in the answer of a box that replaced a holding one", func(t *testing.T) {
		restarter := &Restarter{}
		second := realChild()
		h := newProxyHarnessChildren(t, []*fakeChild{holdingChild(), second},
			&Hooks{PromptHeld: func(string) []byte { return []byte(heldStatus) }},
			RunOpts{Restarter: restarter, NoEditorHostAccess: true})
		writeLine(t, h.clientIn, editorOffersItsComputer)
		readLine(t, h.childIn[0]) // it hangs
		if !restarter.ForceOffHolding() {
			t.Fatal("the forced move was refused")
		}
		frame := parse(readLine(t, h.childIn[1]))
		writeLine(t, second.outW, `{"jsonrpc":"2.0","id":`+string(frame.ID)+`,"result":{"protocolVersion":1,`+loginButton+`}}`)
		noButton(t, readLine(t, h.clientOut))
		_ = h.shutdown()
	})
}

// Nobody decided to close this for an open session, whose box can reach the network anyway.
func TestOpenSessionKeepsItsEditorChannel(t *testing.T) {
	h := newProxyHarnessWith(t, 1, nil, RunOpts{})
	writeLine(t, h.clientIn, editorOffersItsComputer)
	if got := readLine(t, h.childIn[0]); strings.TrimSpace(string(got)) != editorOffersItsComputer {
		t.Fatalf("the box got %s, want the editor's own question", got)
	}
	answer := `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,` + loginButton + `}}`
	writeLine(t, h.children[0].outW, answer)
	if got := readLine(t, h.clientOut); strings.TrimSpace(string(got)) != answer {
		t.Fatalf("the editor got %s, want the box's own answer", got)
	}
	request := `{"jsonrpc":"2.0","id":7,"method":"terminal/create","params":{"sessionId":"s","command":"ls"}}`
	writeLine(t, h.children[0].outW, request)
	if got := readLine(t, h.clientOut); strings.TrimSpace(string(got)) != request {
		t.Fatalf("the editor got %s, want %s", got, request)
	}
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":7,"result":{"terminalId":"t"}}`)
	if reply := parse(readLine(t, h.childIn[0])); string(reply.ID) != `7` || reply.Error != nil {
		t.Fatalf("the box got %+v, want the editor's answer", reply)
	}
	_ = h.shutdown()
}
