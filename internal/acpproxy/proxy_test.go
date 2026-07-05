package acpproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// TestProxyResumePromptReinjectsAfterRestart: after a restart replays a session, a non-nil
// ResumePrompt is fed through the client path — reaching the new box remapped + tracked — and its
// response reaches the editor, completing the turn transparently (coop's rate-limit auto-resend).
func TestProxyResumePromptReinjectsAfterRestart(t *testing.T) {
	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()
	c1, c2 := newFakeChild(), newFakeChild()
	queue := make(chan *fakeChild, 2)
	queue <- c1
	queue <- c2
	factory := func(context.Context) (*Child, error) { return (<-queue).child(), nil }

	resumed := false
	hooks := &Hooks{ResumePrompt: func(sid string) []byte {
		if sid == "S1" && !resumed {
			resumed = true
			return []byte(`{"jsonrpc":"2.0","id":"resume-9","method":"session/prompt","params":{"sessionId":"S1","prompt":[{"type":"text","text":"again"}]}}` + "\n")
		}
		return nil
	}}

	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), clientInR, clientOutW, factory, hooks) }()

	childIn1 := bufio.NewReader(c1.inR)
	childIn2 := bufio.NewReader(c2.inR)
	clientOut := bufio.NewReader(clientOutR)

	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	readLine(t, childIn1)
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	readLine(t, clientOut)

	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w"}}`)
	readLine(t, childIn1)
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"S1"}}`)
	readLine(t, clientOut)

	// A prompt turns the session, so the restart reloads it (rather than re-creating it).
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1"}}`)
	readLine(t, childIn1)

	// child1 dies → replay initialize + session/load(S1) on child2.
	c1.outW.Close()
	for {
		h := parse(readLine(t, childIn2))
		writeLine(t, c2.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, string(h.ID)))
		if h.Method == "session/load" {
			break
		}
	}

	// swapChild fails the in-flight id 3 → drain it first (it's written before the resume is queued).
	if id := idStr(t, readLine(t, clientOut)); id != "3" {
		t.Fatalf("expected in-flight id 3 failed on swap, got %q", id)
	}

	// After the swap, ResumePrompt fires: the prompt is re-injected to the new box.
	fr := parse(readLine(t, childIn2))
	if fr.Method != "session/prompt" {
		t.Fatalf("resume frame = %q, want session/prompt", fr.Method)
	}
	if sid := sessionID(fr.Params); sid != "S1" {
		t.Fatalf("resumed prompt sessionId = %q, want S1", sid)
	}
	if string(trimQuotes(fr.ID)) != "resume-9" {
		t.Fatalf("resumed prompt id = %s, want resume-9", fr.ID)
	}

	// Its response reaches the editor — the turn completes.
	writeLine(t, c2.outW, `{"jsonrpc":"2.0","id":"resume-9","result":{"stopReason":"end_turn"}}`)
	line := readLine(t, clientOut)
	if !strings.Contains(string(line), "resume-9") || !strings.Contains(string(line), "result") {
		t.Fatalf("resumed prompt response never reached the editor, got %s", line)
	}

	clientInW.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after client close")
	}
}

// TestProxyReplayForwardsConfigUpdate: the editor never sees the replayed session/load result, so the
// proxy forwards its configOptions as a config_option_update — through ToEditor, so coop's toolbar
// rewrite applies — bringing the toolbar up to the restarted box's truth (e.g. a preset's model).
func TestProxyReplayForwardsConfigUpdate(t *testing.T) {
	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()
	c1, c2 := newFakeChild(), newFakeChild()
	queue := make(chan *fakeChild, 2)
	queue <- c1
	queue <- c2
	factory := func(context.Context) (*Child, error) { return (<-queue).child(), nil }

	sawUpdate := false
	hooks := &Hooks{ToEditor: func(line []byte) ([]byte, bool) {
		if bytes.Contains(line, []byte("config_option_update")) {
			sawUpdate = true // coop's rewrite gets its look at the synthesized update
		}
		return line, false
	}}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), clientInR, clientOutW, factory, hooks) }()

	childIn1 := bufio.NewReader(c1.inR)
	childIn2 := bufio.NewReader(c2.inR)
	clientOut := bufio.NewReader(clientOutR)

	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	readLine(t, childIn1)
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	readLine(t, clientOut)

	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w"}}`)
	readLine(t, childIn1)
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"S1"}}`)
	readLine(t, clientOut)

	// A prompt turns the session so the restart reloads it.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1"}}`)
	readLine(t, childIn1)

	// child1 dies → replay on child2; the load result carries the box's REAL config.
	c1.outW.Close()
	for {
		h := parse(readLine(t, childIn2))
		if h.Method == "session/load" {
			writeLine(t, c2.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"configOptions":[{"id":"model","currentValue":"claude-fable-5"}]}}`, string(h.ID)))
			break
		}
		writeLine(t, c2.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, string(h.ID)))
	}

	// The in-flight id 3 fails on swap, then the config update reaches the editor.
	if id := idStr(t, readLine(t, clientOut)); id != "3" {
		t.Fatalf("expected the in-flight id 3 failed first, got %q", id)
	}
	upd := readLine(t, clientOut)
	if !strings.Contains(string(upd), "config_option_update") || !strings.Contains(string(upd), "claude-fable-5") {
		t.Fatalf("expected a config_option_update with the box's model, got %s", upd)
	}
	if sid := sessionID(parse(upd).Params); sid != "S1" {
		t.Fatalf("config update sessionId = %q, want S1", sid)
	}
	if !sawUpdate {
		t.Error("the synthesized update must pass through ToEditor (coop's toolbar rewrite)")
	}

	clientInW.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after client close")
	}
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
		out:       io.Discard,
		sessions:  map[string]*sess{"S1": {params: json.RawMessage(`{"cwd":"/w"}`), adapterID: "S1", turned: true}},
		byAdapter: map[string]string{},
		newReqs:   map[string]json.RawMessage{},
		pending:   map[string]bool{},
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
		out:       io.Discard,
		sessions:  map[string]*sess{},
		byAdapter: map[string]string{},
		newReqs:   map[string]json.RawMessage{`"7"`: json.RawMessage(`{"cwd":"/w"}`)},
		pending:   map[string]bool{`"7"`: true},
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
		out:       &out,
		sessions:  map[string]*sess{},
		byAdapter: map[string]string{},
		newReqs:   map[string]json.RawMessage{`"1"`: json.RawMessage(`{"cwd":"/w"}`)},
		pending:   map[string]bool{`"1"`: true},
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

// TestWithSessionID: the sessionId rewrite replaces params.sessionId, preserves the rest, and leaves
// a line with no params untouched.
func TestWithSessionID(t *testing.T) {
	out := withSessionID([]byte(`{"jsonrpc":"2.0","method":"session/prompt","params":{"sessionId":"A","foo":1}}`+"\n"), "B")
	if sid := sessionID(parse(out).Params); sid != "B" {
		t.Errorf("sessionId = %q, want B", sid)
	}
	if !bytes.Contains(out, []byte(`"foo":1`)) {
		t.Errorf("other params dropped: %s", out)
	}
	noParams := []byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")
	if got := withSessionID(noParams, "B"); !bytes.Equal(got, noParams) {
		t.Errorf("a line with no params was changed: %s", got)
	}
}

// TestProxyRecreatesTurnlessSession: a coop-driven switch (or box death) BEFORE the first prompt
// re-creates the turn-less session on the new box under a fresh id and remaps it, so the editor's
// original id keeps working — no "Session not found". (A session that HAS turned is reloaded instead;
// that path is covered by TestProxyPassthroughAndReplayOnRestart, whose in-flight prompt marks it.)
func TestProxyRecreatesTurnlessSession(t *testing.T) {
	orig1, orig2 := replayStartupGrace, replayIdleTimeout
	replayStartupGrace, replayIdleTimeout = 2*time.Second, 2*time.Second
	defer func() { replayStartupGrace, replayIdleTimeout = orig1, orig2 }()

	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()
	c1, c2 := newFakeChild(), newFakeChild()
	queue := make(chan *fakeChild, 2)
	queue <- c1
	queue <- c2
	factory := func(context.Context) (*Child, error) { return (<-queue).child(), nil }

	// A coop-driven switch: an editor line carrying "__switch__" is handled here and restarts the box.
	hooks := &Hooks{FromEditor: func(line []byte) (bool, []byte, bool) {
		if bytes.Contains(line, []byte("__switch__")) {
			return true, nil, true
		}
		return false, nil, false
	}}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), clientInR, clientOutW, factory, hooks) }()

	childIn1 := bufio.NewReader(c1.inR)
	childIn2 := bufio.NewReader(c2.inR)
	clientOut := bufio.NewReader(clientOutR)

	// initialize.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	readLine(t, childIn1)
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	readLine(t, clientOut)

	// session/new → box assigns S1. NO prompt follows, so the session is turn-less.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w","mcpServers":[]}}`)
	readLine(t, childIn1)
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"S1"}}`)
	if id := idStr(t, readLine(t, clientOut)); id != "2" {
		t.Fatalf("session/new response id = %q, want 2", id)
	}

	// An in-flight (non-prompt) request so the swap has something to fail — our sync point for "the
	// new box is live" — without marking the session turned.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":3,"method":"session/set_config_option","params":{"sessionId":"S1","configId":"model","value":"x"}}`)
	readLine(t, childIn1) // reaches child1, which never answers it

	// The switch, before any prompt → restart.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":4,"method":"session/set_config_option","params":{"configId":"__switch__"}}`)

	// child2 replay: initialize (setup) + session/new (re-create the turn-less S1, NOT session/load).
	reNew := false
	for i := 0; i < 2; i++ {
		fr := parse(readLine(t, childIn2))
		switch fr.Method {
		case "initialize":
			writeLine(t, c2.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, string(fr.ID)))
		case "session/new":
			reNew = true
			writeLine(t, c2.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"S2","configOptions":[{"id":"model","currentValue":"m1"}]}}`, string(fr.ID)))
		case "session/load":
			t.Fatal("a turn-less session must be re-created (session/new), not session/load-ed")
		default:
			t.Fatalf("unexpected replay frame: %s", fr.Method)
		}
	}
	if !reNew {
		t.Fatal("turn-less session was not re-created on the new box")
	}

	// swapChild fails the in-flight id 3 → our signal that child2 is now live.
	if id := idStr(t, readLine(t, clientOut)); id != "3" {
		t.Fatalf("expected in-flight id 3 to be failed on swap, got %q", id)
	}

	// The re-created session's config is forwarded under the EDITOR's id (S1, not the box's S2).
	upd := parse(readLine(t, clientOut))
	if sid := sessionID(upd.Params); sid != "S1" {
		t.Fatalf("config update sessionId = %q, want the editor's S1", sid)
	}

	// The editor's FIRST prompt still uses the ORIGINAL id S1; it must reach the box remapped to S2.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":5,"method":"session/prompt","params":{"sessionId":"S1"}}`)
	fwd := parse(readLine(t, childIn2))
	if fwd.Method != "session/prompt" {
		t.Fatalf("post-restart frame = %q, want session/prompt", fwd.Method)
	}
	if sid := sessionID(fwd.Params); sid != "S2" {
		t.Fatalf("editor→box prompt sessionId = %q, want S2 (remapped from S1)", sid)
	}

	// A box notification tagged with S2 must come back to the editor tagged with the original S1.
	writeLine(t, c2.outW, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S2","update":{}}}`)
	upd = parse(readLine(t, clientOut))
	if sid := sessionID(upd.Params); sid != "S1" {
		t.Fatalf("box→editor update sessionId = %q, want S1 (the editor's id)", sid)
	}

	clientInW.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after client close")
	}
}

// TestProxyDisconnectDuringReplayReturnsCleanly: if the editor disconnects WHILE a restart's replay
// is in flight (the new box not yet published), Run must still tear down and return — not orphan the
// new box on an Out that never closes and block forever. (Prod is reaped by signal ctx-cancel; the
// bug shows as a hang only with a non-cancelable ctx, but the leak is real either way.)
func TestProxyDisconnectDuringReplayReturnsCleanly(t *testing.T) {
	orig1, orig2 := replayStartupGrace, replayIdleTimeout
	replayStartupGrace, replayIdleTimeout = 3*time.Second, 3*time.Second
	defer func() { replayStartupGrace, replayIdleTimeout = orig1, orig2 }()

	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()
	c1, c2 := newFakeChild(), newFakeChild()
	queue := make(chan *fakeChild, 2)
	queue <- c1
	queue <- c2
	factory := func(context.Context) (*Child, error) { return (<-queue).child(), nil }

	hooks := &Hooks{FromEditor: func(line []byte) (bool, []byte, bool) {
		if bytes.Contains(line, []byte("__switch__")) {
			return true, nil, true
		}
		return false, nil, false
	}}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), clientInR, clientOutW, factory, hooks) }()

	childIn1 := bufio.NewReader(c1.inR)
	childIn2 := bufio.NewReader(c2.inR)
	clientOut := bufio.NewReader(clientOutR)

	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	readLine(t, childIn1)
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	readLine(t, clientOut)

	// An intentional restart: replay(child2) begins and blocks waiting for the initialize response.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":2,"method":"session/set_config_option","params":{"configId":"__switch__"}}`)
	fr := parse(readLine(t, childIn2))
	if fr.Method != "initialize" {
		t.Fatalf("replay frame = %q, want initialize", fr.Method)
	}

	// Editor disconnects mid-replay; let the disconnect tear down the (old) child BEFORE replay
	// publishes the new one — the exact window where the new box could be orphaned.
	clientInW.Close()
	time.Sleep(50 * time.Millisecond)
	writeLine(t, c2.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, string(fr.ID)))

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after a mid-replay disconnect (the new box was orphaned)")
	}
}

// resetTrace clears the package-level trace state so each trace test is isolated (and restores it
// after). White-box: the tracer is package-global, like warnOut.
func resetTrace(t *testing.T) {
	t.Helper()
	clear := func() {
		traceMu.Lock()
		if c, ok := traceOut.(io.Closer); ok {
			c.Close()
		}
		traceOut, traceGaveUp, traceLastAt, traceWritten = nil, false, time.Time{}, 0
		traceMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// TestTraceWritesWhenEnabled: with COOP_ACP_TRACE set, the wire + events land in
// <config>/acp-trace-<pid>.log.
func TestTraceWritesWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir) // traces land under <dir>/coop, not the real config
	t.Setenv("COOP_ACP_TRACE", "1")
	resetTrace(t)

	traceLine("editor→box", []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new"}`+"\n"))
	Trace("restart requested")

	path := filepath.Join(dir, "coop", fmt.Sprintf("acp-trace-%d.log", os.Getpid()))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("trace file not written: %v", err)
	}
	for _, want := range []string{"editor→box", "session/new", "restart requested"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("trace missing %q:\n%s", want, b)
		}
	}
}

// TestTraceEnabledBySentinel: the sentinel file turns tracing on with no env var (so it works on an
// already-running server).
func TestTraceEnabledBySentinel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("COOP_ACP_TRACE", "") // env OFF; only the sentinel enables it
	if err := os.MkdirAll(filepath.Join(dir, "coop"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "coop", traceSentinel), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	resetTrace(t)

	Trace("via sentinel")
	path := filepath.Join(dir, "coop", fmt.Sprintf("acp-trace-%d.log", os.Getpid()))
	if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), "via sentinel") {
		t.Fatalf("sentinel did not enable tracing (err=%v): %s", err, b)
	}
}

// TestTraceOffByDefault: with neither the env var nor the sentinel, tracing writes nothing.
func TestTraceOffByDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir) // fresh config dir, no sentinel
	t.Setenv("COOP_ACP_TRACE", "")
	resetTrace(t)

	traceLine("editor→box", []byte("{}\n"))
	Trace("nope")

	if got, _ := filepath.Glob(filepath.Join(dir, "coop", "acp-trace-*.log")); len(got) > 0 {
		t.Errorf("tracing wrote while off: %v", got)
	}
}

// TestTraceRotatesAtCap: the primary log never exceeds the byte cap; overflow rolls into a .1 backup,
// so a long-running server's trace stays bounded (~2× the cap).
func TestTraceRotatesAtCap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("COOP_ACP_TRACE", "1")
	resetTrace(t)
	old := traceMaxBytes
	traceMaxBytes = 300
	defer func() { traceMaxBytes = old }()

	for i := 0; i < 80; i++ {
		Trace("line %02d — filler to push past the tiny test cap xxxxxxxxxx", i)
	}
	base := filepath.Join(dir, "coop", fmt.Sprintf("acp-trace-%d.log", os.Getpid()))
	fi, err := os.Stat(base)
	if err != nil {
		t.Fatalf("primary log: %v", err)
	}
	if fi.Size() > traceMaxBytes {
		t.Errorf("primary log %d bytes exceeds cap %d — rotation didn't bound it", fi.Size(), traceMaxBytes)
	}
	if _, err := os.Stat(base + ".1"); err != nil {
		t.Errorf("expected a .1 backup after rotation: %v", err)
	}
}

// TestTracePrunesOldFiles: opening a new trace prunes old per-pid logs to the newest traceKeepFiles,
// but never removes a log whose pid is still running.
func TestTracePrunesOldFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("COOP_ACP_TRACE", "1")
	cdir := filepath.Join(dir, "coop")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Five stale logs from dead pids (huge, non-existent), oldest→newest by mtime, each with a .1.
	base := time.Now().Add(-time.Hour)
	stale := []int{2000000001, 2000000002, 2000000003, 2000000004, 2000000005}
	for i, pid := range stale {
		p := filepath.Join(cdir, fmt.Sprintf("acp-trace-%d.log", pid))
		if err := os.WriteFile(p, []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p+".1", []byte("older\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := base.Add(time.Duration(i) * time.Minute)
		os.Chtimes(p, mt, mt)
	}
	old := traceKeepFiles
	traceKeepFiles = 3
	defer func() { traceKeepFiles = old }()
	resetTrace(t)

	Trace("hi") // opens our log and prunes

	if _, err := os.Stat(filepath.Join(cdir, fmt.Sprintf("acp-trace-%d.log", os.Getpid()))); err != nil {
		t.Fatalf("our own log was removed: %v", err)
	}
	remaining := 0
	for _, pid := range stale {
		if _, err := os.Stat(filepath.Join(cdir, fmt.Sprintf("acp-trace-%d.log", pid))); err == nil {
			remaining++
		}
	}
	if remaining != traceKeepFiles-1 {
		t.Errorf("kept %d stale logs, want %d (newest, minus our own slot)", remaining, traceKeepFiles-1)
	}
	// The oldest three (and their .1) should be gone; the newest two survive.
	for _, pid := range stale[:3] {
		if _, err := os.Stat(filepath.Join(cdir, fmt.Sprintf("acp-trace-%d.log.1", pid))); err == nil {
			t.Errorf("stale backup for pid %d not pruned", pid)
		}
	}
}
