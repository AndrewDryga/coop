package acpproxy

import (
	"bufio"
	"bytes"
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
	go func() { done <- Run(context.Background(), clientInR, clientOutW, factory, nil) }()

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

	// authenticate → part of the setup handshake that must be replayed on a restart.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":"a","method":"authenticate","params":{"methodId":"x"}}`)
	if m := method(t, readLine(t, childIn1)); m != "authenticate" {
		t.Fatalf("child1 frame = %q, want authenticate", m)
	}
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":"a","result":{}}`)
	readLine(t, clientOut) // authenticate response forwarded

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
	var sawInit, sawAuth, sawLoad bool
	for i := 0; i < 3; i++ {
		line := readLine(t, childIn2)
		h := parse(line)
		switch h.Method {
		case "initialize":
			sawInit = true
		case "authenticate":
			sawAuth = true
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
	if !sawInit || !sawAuth || !sawLoad {
		t.Fatalf("replay missing init=%v auth=%v load=%v", sawInit, sawAuth, sawLoad)
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

// TestProxyAutoReplyAnswersChildRequest: an agent→editor request the hook claims (a permission ask)
// is answered straight to the child and NOT forwarded to the editor; other traffic still flows.
func TestProxyAutoReplyAnswersChildRequest(t *testing.T) {
	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()
	c1 := newFakeChild()
	queue := make(chan *fakeChild, 1)
	queue <- c1
	factory := func(context.Context) (*Child, error) { return (<-queue).child(), nil }

	hooks := &Hooks{AutoReply: func(line []byte) ([]byte, bool) {
		h := parse(line)
		if h.Method != "session/request_permission" {
			return nil, true
		}
		return []byte(`{"jsonrpc":"2.0","id":` + string(h.ID) + `,"result":{"outcome":{"outcome":"selected","optionId":"ok"}}}` + "\n"), false
	}}

	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), clientInR, clientOutW, factory, hooks) }()

	childIn := bufio.NewReader(c1.inR)
	clientOut := bufio.NewReader(clientOutR)

	// Child asks for permission → coop answers the child directly (id 7), editor never sees the ask.
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{"options":[{"optionId":"ok","kind":"allow_once"}]}}`)
	reply := readLine(t, childIn)
	if h := parse(reply); !h.isResponse() || string(trimQuotes(h.ID)) != "7" || !strings.Contains(string(reply), "selected") {
		t.Fatalf("child did not get an id-7 allow response, got %s", reply)
	}

	// A normal notification IS forwarded — proving the stream is live and only the permission was pulled.
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S1"}}`)
	if m := method(t, readLine(t, clientOut)); m != "session/update" {
		t.Fatalf("editor should see the update after the swallowed permission, got %q", m)
	}

	clientInW.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after client close")
	}
}

// TestProxyToEditorRestart: a ToEditor hook that returns restart=true (coop auto-rotating on a
// rate limit) forwards the line to the editor AND respawns the box, replaying the handshake.
func TestProxyToEditorRestart(t *testing.T) {
	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()
	c1, c2 := newFakeChild(), newFakeChild()
	queue := make(chan *fakeChild, 2)
	queue <- c1
	queue <- c2
	factory := func(context.Context) (*Child, error) { return (<-queue).child(), nil }

	hooks := &Hooks{ToEditor: func(line []byte) ([]byte, bool) {
		return line, bytes.Contains(line, []byte("ratelimited")) // forward always; restart on the limit line
	}}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), clientInR, clientOutW, factory, hooks) }()

	childIn1 := bufio.NewReader(c1.inR)
	childIn2 := bufio.NewReader(c2.inR)
	clientOut := bufio.NewReader(clientOutR)

	// Bring child1 up with initialize, so a restart has a handshake to replay (our sync point).
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if m := method(t, readLine(t, childIn1)); m != "initialize" {
		t.Fatalf("child1 frame = %q, want initialize", m)
	}
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	readLine(t, clientOut)

	// child1 emits a rate-limit error: ToEditor forwards it to the editor and flags a restart.
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":2,"error":{"message":"ratelimited"}}`)
	if id := idStr(t, readLine(t, clientOut)); id != "2" {
		t.Fatalf("the flagged line must still forward to the editor, got id %q", id)
	}

	// The proxy respawned child2 and replayed initialize — read it, answer its synthetic id so replay
	// completes and child2 goes live (proves the ToEditor restart swapped the box).
	replayed := readLine(t, childIn2)
	h := parse(replayed)
	if h.Method != "initialize" {
		t.Fatalf("expected initialize replayed on child2, got %q", h.Method)
	}
	writeLine(t, c2.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, string(h.ID)))

	clientInW.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after client close")
	}
}

// TestProxyReplayWarnsOnFailedLoad: if a restored session's session/load comes back an error on the
// restarted box (e.g. its transcript wasn't on the shared store for the new credential), the loss is
// surfaced on warnOut instead of vanishing silently.
func TestProxyReplayWarnsOnFailedLoad(t *testing.T) {
	var buf bytes.Buffer
	old := warnOut
	warnOut = &buf
	defer func() { warnOut = old }()

	p := &proxy{
		out:      io.Discard,
		sessions: map[string]json.RawMessage{"S1": json.RawMessage(`{"cwd":"/w"}`)},
		newReqs:  map[string]json.RawMessage{},
		pending:  map[string]bool{},
	}
	fc := newFakeChild()
	br := bufio.NewReader(fc.outR)
	// Read the synthetic session/load coop writes, answer it with an error.
	go func() {
		r := bufio.NewReader(fc.inR)
		line, _ := r.ReadBytes('\n')
		h := parse(line)
		writeLine(t, fc.outW, `{"jsonrpc":"2.0","id":`+string(h.ID)+`,"error":{"code":-1,"message":"no such session"}}`)
	}()
	if err := p.replay(fc.child(), br); err != nil {
		t.Fatalf("replay returned %v", err)
	}
	if !strings.Contains(buf.String(), "did not reload") || !strings.Contains(buf.String(), "S1") {
		t.Errorf("expected a lost-session warning naming S1, got: %q", buf.String())
	}
}

// TestProxyReplayTimeoutFailsPending: a restarted child that never answers the replay handshake
// (a hang, not a crash) must not freeze the editor — replay times out, the in-flight request is
// failed back, and Run gives up with an error instead of blocking forever.
func TestProxyReplayTimeoutFailsPending(t *testing.T) {
	orig1, orig2 := replayStartupGrace, replayIdleTimeout
	replayStartupGrace, replayIdleTimeout = 150*time.Millisecond, 150*time.Millisecond
	defer func() { replayStartupGrace, replayIdleTimeout = orig1, orig2 }()

	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()

	c1, c2 := newFakeChild(), newFakeChild()
	queue := make(chan *fakeChild, 2)
	queue <- c1
	queue <- c2
	factory := func(context.Context) (*Child, error) { return (<-queue).child(), nil }

	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), clientInR, clientOutW, factory, nil) }()

	childIn1 := bufio.NewReader(c1.inR)
	clientOut := bufio.NewReader(clientOutR)

	// Bring child1 up with one in-flight request, then kill it.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	readLine(t, childIn1)
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	readLine(t, clientOut)
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{}}`)
	readLine(t, childIn1)
	c1.outW.Close() // child1 dies

	// child2 receives the replayed initialize but NEVER answers (a hang). Drain its input so the
	// replay writer isn't blocked, then let the timeout fire.
	childIn2 := bufio.NewReader(c2.inR)
	go func() {
		for {
			if _, err := childIn2.ReadBytes('\n'); err != nil {
				return
			}
		}
	}()

	// The in-flight request is failed back to the editor (not left hanging)...
	if id := idStr(t, readLine(t, clientOut)); id != "2" {
		t.Fatalf("expected error response for the in-flight id 2, got id %q", id)
	}
	// ...and Run gives up rather than blocking forever on the hung child.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a non-nil error when replay times out")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not give up on a hung replay")
	}
}

// TestFailAllPendingClearsNewReqs: a session/new in flight when the child dies must not leak its
// newReqs entry (else it lingers forever and could bind a sessionId on a stale id).
func TestFailAllPendingClearsNewReqs(t *testing.T) {
	p := &proxy{
		out:      io.Discard,
		sessions: map[string]json.RawMessage{},
		newReqs:  map[string]json.RawMessage{`"7"`: json.RawMessage(`{"cwd":"/w"}`)},
		pending:  map[string]bool{`"7"`: true},
	}
	p.failAllPending()
	if len(p.newReqs) != 0 {
		t.Errorf("failAllPending should clear newReqs, got %v", p.newReqs)
	}
	if len(p.pending) != 0 {
		t.Errorf("failAllPending should clear pending, got %v", p.pending)
	}
}

// TestSwapChildPublishesAndFailsAtomically guards the swap-window double-reply: a swap must
// publish the new child AND fail the in-flight requests as one step. With the two split, a request
// arriving in the gap was routed to the now-live child AND failed — a duplicate response and a
// re-run prompt. Here an in-flight id is failed exactly once, the new child goes live, and a
// request that lands after the swap reaches the live child and is NOT failed.
func TestSwapChildPublishesAndFailsAtomically(t *testing.T) {
	var out bytes.Buffer
	p := &proxy{
		out:      &out,
		sessions: map[string]json.RawMessage{},
		newReqs:  map[string]json.RawMessage{`"1"`: json.RawMessage(`{"cwd":"/w"}`)},
		pending:  map[string]bool{`"1"`: true},
	}
	fc := newFakeChild()
	c := fc.child()
	p.swapChild(c)

	if p.child != c {
		t.Error("swapChild did not publish the new child as live")
	}
	if len(p.pending) != 0 || len(p.newReqs) != 0 {
		t.Errorf("swapChild should clear pending+newReqs, got pending=%v newReqs=%v", p.pending, p.newReqs)
	}
	if got := out.String(); strings.Count(got, `"id":"1"`) != 1 {
		t.Errorf("in-flight id 1 should be failed exactly once, got: %q", got)
	}

	// A request arriving after the swap routes to the live child and is not failed.
	go p.fromClient([]byte(`{"jsonrpc":"2.0","id":"2","method":"session/prompt","params":{}}` + "\n"))
	if got := readLine(t, bufio.NewReader(fc.inR)); !strings.Contains(string(got), `"id":"2"`) {
		t.Errorf("post-swap request should reach the live child, got: %q", got)
	}
	if strings.Contains(out.String(), `"id":"2"`) {
		t.Error("post-swap request must NOT be failed — the live child answers it")
	}
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
	go func() { done <- Run(context.Background(), clientInR, io.Discard, factory, nil) }()
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
