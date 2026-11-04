package acpctl

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Queued notices reach the editor exactly once, with the next session update, ahead of it and in
// that update's session — never on a response without a session — in the order they last
// happened: a text queued again shows once, at the end.
func TestACPNotifyRidesTheNextSessionUpdateOnce(t *testing.T) {
	c := newTestControl(t)
	c.Notify("an older notice")
	c.Notify("Coop could not apply the newly approved network rules.")
	c.Notify("an older notice")
	if out := toEd(c, []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`+"\n")); bytes.Contains(out, []byte("Coop could not")) {
		t.Fatalf("a notice rode a line that names no session: %s", out)
	}
	chunk := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}}}` + "\n")
	out := toEd(c, chunk)
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], `"hello"`) {
		t.Fatalf("want the notice ahead of the agent's own update, got:\n%s", out)
	}
	var notice struct {
		Method string `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				SessionUpdate string `json:"sessionUpdate"`
				Content       struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &notice); err != nil {
		t.Fatal(err)
	}
	if notice.Method != "session/update" || notice.Params.SessionID != "S" || notice.Params.Update.SessionUpdate != "agent_message_chunk" ||
		notice.Params.Update.Content.Text != "\n\nCoop could not apply the newly approved network rules.\n\nan older notice\n\n" {
		t.Fatalf("the notices are not each queued text once, in the order they last happened, in the update's session: %s", lines[0])
	}
	if again := toEd(c, chunk); bytes.Count(again, []byte("\n")) != 1 || bytes.Contains(again, []byte("Coop could not")) {
		t.Fatalf("a notice was shown twice: %s", again)
	}
}

// The turn-end cue fires when the LAST running turn ends — not while another session's prompt is
// still running, and not for a response that ends no turn.
func TestACPOnTurnEndFiresWhenTheLastTurnEnds(t *testing.T) {
	c := newTestControl(t)
	ended := 0
	c.OnTurnEnd(func() { ended++ })
	prompt := func(id, session string) {
		fromEditorPrompt(c, []byte(`{"jsonrpc":"2.0","id":"`+id+`","method":"session/prompt","params":{"sessionId":"`+session+`","prompt":[{"type":"text","text":"hi"}]}}`+"\n"))
	}
	prompt("p1", "S1")
	prompt("p2", "S2")
	toEd(c, []byte(`{"jsonrpc":"2.0","id":"p1","result":{"stopReason":"end_turn"}}`+"\n"))
	if ended != 0 {
		t.Fatal("the turn-end cue fired while another session's turn was still running")
	}
	toEd(c, []byte(`{"jsonrpc":"2.0","id":"other","result":{}}`+"\n"))
	if ended != 0 {
		t.Fatal("a response that ends no turn fired the turn-end cue")
	}
	toEd(c, []byte(`{"jsonrpc":"2.0","id":"p2","error":{"code":-32603,"message":"boom"}}`+"\n"))
	if ended != 1 {
		t.Fatalf("the last turn's end fired the cue %d times, want once", ended)
	}
}
