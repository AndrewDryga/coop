package acpproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeChild backs a Child with in-memory pipes: the test reads inR to see what the
// proxy sent the child, and writes outW to simulate the child's replies.
type fakeChild struct {
	inR, outR *io.PipeReader
	inW, outW *io.PipeWriter
}

func newFakeChild() *fakeChild {
	f := &fakeChild{}
	f.inR, f.inW = io.Pipe()
	f.outR, f.outW = io.Pipe()
	return f
}

func (f *fakeChild) child() *Child {
	return &Child{In: f.inW, Out: f.outR, Stop: func() { f.outW.Close(); f.inR.Close() }}
}

func readLine(t *testing.T, br *bufio.Reader) []byte {
	t.Helper()
	type res struct {
		line []byte
		err  error
	}
	ch := make(chan res, 1)
	go func() { l, e := br.ReadBytes('\n'); ch <- res{l, e} }()
	select {
	case r := <-ch:
		if len(r.line) == 0 && r.err != nil {
			t.Fatalf("read: %v", r.err)
		}
		return r.line
	case <-time.After(3 * time.Second):
		t.Fatal("timeout reading a line — likely a proxy deadlock")
		return nil
	}
}

func writeLine(t *testing.T, w io.Writer, s string) {
	t.Helper()
	if _, err := io.WriteString(w, s+"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestProxyPassthroughAndReplayOnRestart(t *testing.T) {
	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()

	c1, c2 := newFakeChild(), newFakeChild()
	queue := make(chan *fakeChild, 2)
	queue <- c1
	queue <- c2
	factory := func(context.Context) (*Child, error) { return (<-queue).child(), nil }

	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), clientInR, clientOutW, factory) }()

	childIn1 := bufio.NewReader(c1.inR)
	clientOut := bufio.NewReader(clientOutR)

	// initialize → forwarded to child1, response forwarded back.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"x":1}}`)
	if m := method(t, readLine(t, childIn1)); m != "initialize" {
		t.Fatalf("child1 first frame = %q, want initialize", m)
	}
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	if id := idStr(t, readLine(t, clientOut)); id != "1" {
		t.Fatalf("client got id %q, want 1", id)
	}

	// session/new → learn sessionId S1 from the response.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w","mcpServers":[]}}`)
	if m := method(t, readLine(t, childIn1)); m != "session/new" {
		t.Fatalf("child1 frame = %q, want session/new", m)
	}
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"S1"}}`)
	readLine(t, clientOut) // session/new response forwarded to client

	// An in-flight prompt the dying child never answers.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1"}}`)
	if m := method(t, readLine(t, childIn1)); m != "session/prompt" {
		t.Fatalf("child1 frame = %q, want session/prompt", m)
	}

	// child1 dies.
	c1.outW.Close()

	// The proxy respawns child2 and replays initialize + session/load(S1).
	childIn2 := bufio.NewReader(c2.inR)
	var sawInit, sawLoad bool
	for i := 0; i < 2; i++ {
		line := readLine(t, childIn2)
		h := parse(line)
		switch h.Method {
		case "initialize":
			sawInit = true
		case "session/load":
			if sid := sessionID(h.Params); sid != "S1" {
				t.Fatalf("session/load sessionId = %q, want S1", sid)
			}
			sawLoad = true
		default:
			t.Fatalf("unexpected replay frame: %s", line)
		}
		// Answer the synthetic request so replay completes.
		writeLine(t, c2.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, string(h.ID)))
	}
	if !sawInit || !sawLoad {
		t.Fatalf("replay missing init=%v load=%v", sawInit, sawLoad)
	}

	// The in-flight request (id 3) is failed back to the client so it isn't hung.
	errLine := readLine(t, clientOut)
	if id := idStr(t, errLine); id != "3" || !strings.Contains(string(errLine), `"error"`) {
		t.Fatalf("expected error response for id 3, got %s", errLine)
	}

	// Forwarding resumed: a new request reaches child2.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{}}`)
	if m := method(t, readLine(t, childIn2)); m != "session/prompt" {
		t.Fatalf("post-swap frame = %q, want session/prompt", m)
	}

	// Clean shutdown: closing the client makes Run return without respawning.
	clientInW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on clean client close", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after client close")
	}
}

func method(t *testing.T, line []byte) string { t.Helper(); return parse(line).Method }

func idStr(t *testing.T, line []byte) string {
	t.Helper()
	var m struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("bad json %s: %v", line, err)
	}
	return string(m.ID)
}

func TestProxyGivesUpOnRapidFailures(t *testing.T) {
	clientInR, _ := io.Pipe() // editor never sends or closes; the children just die
	spawns := 0
	factory := func(context.Context) (*Child, error) {
		spawns++
		pr, pw := io.Pipe()
		pw.Close() // Out is immediately EOF: the child "dies" instantly
		_, inW := io.Pipe()
		return &Child{In: inW, Out: pr, Stop: func() { inW.Close() }}, nil
	}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), clientInR, io.Discard, factory) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a give-up error after rapid failures")
		}
		if spawns < maxRapidFails {
			t.Fatalf("gave up after %d spawns, want >= %d", spawns, maxRapidFails)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not give up on rapidly-failing children")
	}
}
