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

// proxyHarness wires a full Run around in-memory pipes — the prologue every restart-flavored
// proxy test used to copy. The test writes editor frames to clientIn, reads what the editor
// would see from clientOut, and talks as box i via children[i] (whose inR side is pre-wrapped
// in childIn[i]). n children are queued into the factory: child 0 is the first live box, each
// restart consumes the next.
type proxyHarness struct {
	clientIn  *io.PipeWriter
	clientOut *bufio.Reader
	children  []*fakeChild
	childIn   []*bufio.Reader
	done      chan error
	t         *testing.T
}

func newProxyHarness(t *testing.T, n int, hooks *Hooks) *proxyHarness {
	t.Helper()
	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()
	h := &proxyHarness{clientIn: clientInW, clientOut: bufio.NewReader(clientOutR), done: make(chan error, 1), t: t}
	queue := make(chan *fakeChild, n)
	for i := 0; i < n; i++ {
		c := newFakeChild()
		h.children = append(h.children, c)
		h.childIn = append(h.childIn, bufio.NewReader(c.inR))
		queue <- c
	}
	factory := func(context.Context) (*Child, error) { return (<-queue).child(), nil }
	go func() { h.done <- Run(context.Background(), clientInR, clientOutW, factory, hooks) }()
	return h
}

// initialize drives the plain initialize round-trip through child i (id 1, empty params and
// result) — request forwarded to the box, result forwarded back. A test that asserts ON the
// handshake itself (passthrough, the ToEditor restart) keeps its own inline copy instead.
func (h *proxyHarness) initialize(i int) {
	h.t.Helper()
	writeLine(h.t, h.clientIn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	readLine(h.t, h.childIn[i])
	writeLine(h.t, h.children[i].outW, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	readLine(h.t, h.clientOut)
}

// newSession drives the plain session/new round-trip through child i (id 2); the box assigns sid.
func (h *proxyHarness) newSession(i int, sid string) {
	h.t.Helper()
	writeLine(h.t, h.clientIn, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w"}}`)
	readLine(h.t, h.childIn[i])
	writeLine(h.t, h.children[i].outW, `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"`+sid+`"}}`)
	readLine(h.t, h.clientOut)
}

// shutdown closes the editor side and waits for Run to return, failing the test on a hang.
// It returns Run's error so a test can assert on it (or ignore it, as most do).
func (h *proxyHarness) shutdown() error {
	h.t.Helper()
	h.clientIn.Close()
	select {
	case err := <-h.done:
		return err
	case <-time.After(3 * time.Second):
		h.t.Fatal("Run did not return after client close")
		return nil
	}
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
	h := newProxyHarness(t, 2, nil)
	c1, c2 := h.children[0], h.children[1]
	clientInW, clientOut := h.clientIn, h.clientOut
	childIn1 := h.childIn[0]

	// initialize → forwarded to child1, response forwarded back. Inline (not h.initialize):
	// the forwarding assertions ARE this test's subject.
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
	childIn2 := h.childIn[1]
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
	if err := h.shutdown(); err != nil {
		t.Fatalf("Run returned %v, want nil on clean client close", err)
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
	resumed := false
	hooks := &Hooks{ResumePrompt: func(sid string) []byte {
		if sid == "S1" && !resumed {
			resumed = true
			return []byte(`{"jsonrpc":"2.0","id":"resume-9","method":"session/prompt","params":{"sessionId":"S1","prompt":[{"type":"text","text":"again"}]}}` + "\n")
		}
		return nil
	}}
	h := newProxyHarness(t, 2, hooks)
	c1, c2 := h.children[0], h.children[1]
	clientInW, clientOut := h.clientIn, h.clientOut
	childIn1, childIn2 := h.childIn[0], h.childIn[1]

	h.initialize(0)
	h.newSession(0, "S1")

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

	h.shutdown()
}

// TestProxyResumeSparesInFlightPrompt: a prompt still awaiting its response when a manual
// credential/preset switch restarts the box is NOT failed with "agent restarted" — the resume
// re-sends the ORIGINAL line, so the keep-set spares its id at the swap and the new box's answer
// completes the editor's original request. A pending request with no resend still fails fast.
func TestProxyResumeSparesInFlightPrompt(t *testing.T) {
	prompt := `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1"}}`
	resumed := false
	hooks := &Hooks{ResumePrompt: func(sid string) []byte {
		if sid == "S1" && !resumed {
			resumed = true
			return []byte(prompt + "\n")
		}
		return nil
	}}
	h := newProxyHarness(t, 2, hooks)
	c2 := h.children[1]
	h.initialize(0)
	h.newSession(0, "S1")

	// The turn in flight at the switch, plus an unrelated request that nothing re-sends.
	writeLine(t, h.clientIn, prompt)
	readLine(t, h.childIn[0])
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":4,"method":"session/set_config_option","params":{"sessionId":"S1"}}`)
	readLine(t, h.childIn[0])

	// child1 dies → replay initialize + session/load(S1) on child2.
	h.children[0].outW.Close()
	for {
		fr := parse(readLine(t, h.childIn[1]))
		writeLine(t, c2.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, string(fr.ID)))
		if fr.Method == "session/load" {
			break
		}
	}

	// Only the non-resumed id 4 is failed at the swap; the in-flight prompt id 3 is spared.
	if id := idStr(t, readLine(t, h.clientOut)); id != "4" {
		t.Fatalf("expected only the non-resumed id 4 failed on swap, got %q", id)
	}

	// The resend reaches the new box under the editor's original id.
	fr := parse(readLine(t, h.childIn[1]))
	if fr.Method != "session/prompt" || string(fr.ID) != "3" {
		t.Fatalf("resume frame = %s %q, want session/prompt id 3", fr.ID, fr.Method)
	}

	// Its response completes the editor's original request — no -32000 ever sent for id 3.
	writeLine(t, c2.outW, `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`)
	line := readLine(t, h.clientOut)
	if idStr(t, line) != "3" || !strings.Contains(string(line), "end_turn") {
		t.Fatalf("in-flight prompt must complete with the resend's result, got %s", line)
	}

	h.shutdown()
}

// TestProxyReplayDropsHistoryRestream: an adapter answering a replayed session/load may first
// re-stream the stored conversation as session/update notifications. Those must NEVER reach the
// editor — it already shows the conversation, so forwarding would duplicate its view (and any
// dangling user turn the dead box persisted before a rate-limit resend would render twice; coop's
// v3 waiver of that stored duplicate leans on this drop). Post-replay updates still flow.
func TestProxyReplayDropsHistoryRestream(t *testing.T) {
	h := newProxyHarness(t, 2, nil)
	c1, c2 := h.children[0], h.children[1]
	clientInW, clientOut := h.clientIn, h.clientOut
	childIn1, childIn2 := h.childIn[0], h.childIn[1]

	h.initialize(0)
	h.newSession(0, "S1")

	// A prompt turns the session (so the restart reloads it) and stays in flight.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1"}}`)
	readLine(t, childIn1)
	c1.outW.Close() // child1 dies

	// child2 replay: on session/load, re-stream "history" BEFORE answering — like an adapter
	// replaying the stored transcript — then answer the load.
	for {
		fr := parse(readLine(t, childIn2))
		if fr.Method == "session/load" {
			writeLine(t, c2.outW, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S1","update":{"marker":"replayed-history"}}}`)
			writeLine(t, c2.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, string(fr.ID)))
			break
		}
		writeLine(t, c2.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, string(fr.ID)))
	}

	// The first thing the editor sees post-swap is the failed in-flight id 3 — NOT the re-stream.
	errLine := readLine(t, clientOut)
	if id := idStr(t, errLine); id != "3" || strings.Contains(string(errLine), "replayed-history") {
		t.Fatalf("first post-swap frame must be the failed id 3, got: %s", errLine)
	}

	// A LIVE update after the swap still flows — proving the stream is open and only the
	// replay-time re-stream was dropped.
	writeLine(t, c2.outW, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S1","update":{"marker":"live"}}}`)
	live := readLine(t, clientOut)
	if !strings.Contains(string(live), `"live"`) || strings.Contains(string(live), "replayed-history") {
		t.Fatalf("expected the live update next, got: %s", live)
	}

	h.shutdown()
}

// TestProxyReplayForwardsConfigUpdate: the editor never sees the replayed session/load result, so the
// proxy forwards its configOptions as a config_option_update — through ToEditor, so coop's toolbar
// rewrite applies — bringing the toolbar up to the restarted box's truth (e.g. a preset's model).
func TestProxyReplayForwardsConfigUpdate(t *testing.T) {
	sawUpdate := false
	hooks := &Hooks{ToEditor: func(line []byte) ([]byte, bool) {
		if bytes.Contains(line, []byte("config_option_update")) {
			sawUpdate = true // coop's rewrite gets its look at the synthesized update
		}
		return line, false
	}}
	h := newProxyHarness(t, 2, hooks)
	c1, c2 := h.children[0], h.children[1]
	clientInW, clientOut := h.clientIn, h.clientOut
	childIn1, childIn2 := h.childIn[0], h.childIn[1]

	h.initialize(0)
	h.newSession(0, "S1")

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

	h.shutdown()
}

// TestProxyAutoReplyAnswersChildRequest: an agent→editor request the hook claims (a permission ask)
// is answered straight to the child and NOT forwarded to the editor; other traffic still flows.
func TestProxyAutoReplyAnswersChildRequest(t *testing.T) {
	hooks := &Hooks{AutoReply: func(line []byte) ([]byte, bool) {
		h := parse(line)
		if h.Method != "session/request_permission" {
			return nil, true
		}
		return []byte(`{"jsonrpc":"2.0","id":` + string(h.ID) + `,"result":{"outcome":{"outcome":"selected","optionId":"ok"}}}` + "\n"), false
	}}
	h := newProxyHarness(t, 1, hooks)
	c1 := h.children[0]
	childIn, clientOut := h.childIn[0], h.clientOut

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

	h.shutdown()
}

// TestProxyToEditorRestart: a ToEditor hook that returns restart=true (coop auto-rotating on a
// rate limit) forwards the line to the editor AND respawns the box, replaying the handshake.
func TestProxyToEditorRestart(t *testing.T) {
	hooks := &Hooks{ToEditor: func(line []byte) ([]byte, bool) {
		return line, bytes.Contains(line, []byte("ratelimited")) // forward always; restart on the limit line
	}}
	h := newProxyHarness(t, 2, hooks)
	c1, c2 := h.children[0], h.children[1]
	clientInW, clientOut := h.clientIn, h.clientOut
	childIn1, childIn2 := h.childIn[0], h.childIn[1]

	// Bring child1 up with initialize, so a restart has a handshake to replay (our sync point).
	// Inline (not h.initialize): the method assertion and non-empty result are part of the point.
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
	fr := parse(replayed)
	if fr.Method != "initialize" {
		t.Fatalf("expected initialize replayed on child2, got %q", fr.Method)
	}
	writeLine(t, c2.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, string(fr.ID)))

	h.shutdown()
}

// TestProxyReplayWarnsOnFailedLoad: if a restored session's session/load comes back an error on the
// restarted box (e.g. its transcript wasn't on the shared store for the new credential), the loss is
// surfaced on warnOut instead of vanishing silently.
// A turned session whose session/load FAILS after a restart (a provider switch, or a lost
// transcript) is re-created fresh in a second replay round: the editor's id is remapped to the
// new box id, SessionRecreated fires (coop's cue to carry the conversation best-effort), and
// the loss is named on stderr — not a silent dead session.
func TestProxyReplayRecreatesFailedLoad(t *testing.T) {
	var buf bytes.Buffer
	old := warnOut
	warnOut = &buf
	defer func() { warnOut = old }()

	var recreated []string
	p := &proxy{
		out:       io.Discard,
		sessions:  map[string]*sess{"S1": {params: json.RawMessage(`{"cwd":"/w"}`), adapterID: "S1", turned: true}},
		byAdapter: map[string]string{},
		newReqs:   map[string]json.RawMessage{},
		pending:   map[string]bool{},
		hooks:     &Hooks{SessionRecreated: func(sid string) { recreated = append(recreated, sid) }},
	}
	fc := newFakeChild()
	br := bufio.NewReader(fc.outR)
	// Round 1: fail the synthetic session/load. Round 2: answer the re-create session/new with a
	// fresh box id, and confirm it reused the ORIGINAL params (cwd survives the provider switch).
	go func() {
		r := bufio.NewReader(fc.inR)
		line, _ := r.ReadBytes('\n')
		h := parse(line)
		writeLine(t, fc.outW, `{"jsonrpc":"2.0","id":`+string(h.ID)+`,"error":{"code":-1,"message":"no such session"}}`)
		line, _ = r.ReadBytes('\n')
		h = parse(line)
		if m := parse(line).Method; m != "session/new" {
			t.Errorf("round 2 sent %q, want session/new", m)
		}
		if !strings.Contains(string(line), `"cwd":"/w"`) {
			t.Errorf("re-create must reuse the original params, sent: %s", line)
		}
		writeLine(t, fc.outW, `{"jsonrpc":"2.0","id":`+string(h.ID)+`,"result":{"sessionId":"S1b"}}`)
	}()
	if err := p.replay(fc.child(), br); err != nil {
		t.Fatalf("replay returned %v", err)
	}
	if s := p.sessions["S1"]; s == nil || s.adapterID != "S1b" {
		t.Fatalf("editor session S1 must remap to the re-created box id S1b, got %+v", p.sessions["S1"])
	}
	if len(recreated) != 1 || recreated[0] != "S1" {
		t.Errorf("SessionRecreated fired %v, want exactly [S1]", recreated)
	}
	if !strings.Contains(buf.String(), "re-creating it fresh") || !strings.Contains(buf.String(), "S1") {
		t.Errorf("expected a re-create warning naming S1, got: %q", buf.String())
	}

	// A SECOND restart before any prompt ran on S1b: the re-created session has no transcript on
	// the new box, so replay must go straight to session/new — no doomed session/load of an id
	// that was never persisted, and no repeat "did NOT reload" warning.
	buf.Reset()
	fc2 := newFakeChild()
	br2 := bufio.NewReader(fc2.outR)
	go func() {
		r := bufio.NewReader(fc2.inR)
		line, _ := r.ReadBytes('\n')
		h := parse(line)
		if m := h.Method; m != "session/new" {
			t.Errorf("second restart sent %q, want session/new in round one", m)
		}
		writeLine(t, fc2.outW, `{"jsonrpc":"2.0","id":`+string(h.ID)+`,"result":{"sessionId":"S1c"}}`)
	}()
	if err := p.replay(fc2.child(), br2); err != nil {
		t.Fatalf("second replay returned %v", err)
	}
	if s := p.sessions["S1"]; s == nil || s.adapterID != "S1c" {
		t.Fatalf("second restart must remap S1 to S1c, got %+v", p.sessions["S1"])
	}
	if strings.Contains(buf.String(), "did not reload") {
		t.Errorf("second restart must not attempt (and fail) a load, got: %q", buf.String())
	}
}

// If even the second-round session/new fails, the session is genuinely gone — loud warn, no
// remap, no SessionRecreated (there is nothing to carry the conversation INTO).
func TestProxyReplayRecreateFails(t *testing.T) {
	var buf bytes.Buffer
	old := warnOut
	warnOut = &buf
	defer func() { warnOut = old }()

	var recreated []string
	p := &proxy{
		out:       io.Discard,
		sessions:  map[string]*sess{"S1": {params: json.RawMessage(`{"cwd":"/w"}`), adapterID: "S1", turned: true}},
		byAdapter: map[string]string{},
		newReqs:   map[string]json.RawMessage{},
		pending:   map[string]bool{},
		hooks:     &Hooks{SessionRecreated: func(sid string) { recreated = append(recreated, sid) }},
	}
	fc := newFakeChild()
	br := bufio.NewReader(fc.outR)
	go func() {
		r := bufio.NewReader(fc.inR)
		for i := 0; i < 2; i++ { // fail the load, then fail the re-create too
			line, _ := r.ReadBytes('\n')
			h := parse(line)
			writeLine(t, fc.outW, `{"jsonrpc":"2.0","id":`+string(h.ID)+`,"error":{"code":-1,"message":"broken box"}}`)
		}
	}()
	if err := p.replay(fc.child(), br); err != nil {
		t.Fatalf("replay returned %v", err)
	}
	if s := p.sessions["S1"]; s == nil || s.adapterID != "S1" {
		t.Fatalf("a failed re-create must not remap, got %+v", p.sessions["S1"])
	}
	if len(recreated) != 0 {
		t.Errorf("SessionRecreated fired %v for a failed re-create, want none", recreated)
	}
	if !strings.Contains(buf.String(), "could not be re-created") {
		t.Errorf("expected the re-create failure warning, got: %q", buf.String())
	}
}

// TestProxyReplayTimeoutFailsPending: a restarted child that never answers the replay handshake
// (a hang, not a crash) must not freeze the editor — replay times out, the in-flight request is
// failed back, and Run gives up with an error instead of blocking forever.
func TestProxyReplayTimeoutFailsPending(t *testing.T) {
	orig1, orig2 := replayStartupGrace, replayIdleTimeout
	replayStartupGrace, replayIdleTimeout = 150*time.Millisecond, 150*time.Millisecond
	defer func() { replayStartupGrace, replayIdleTimeout = orig1, orig2 }()

	h := newProxyHarness(t, 2, nil)
	c1 := h.children[0]
	clientInW, clientOut := h.clientIn, h.clientOut
	childIn1 := h.childIn[0]

	// Bring child1 up with one in-flight request, then kill it.
	h.initialize(0)
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{}}`)
	readLine(t, childIn1)
	c1.outW.Close() // child1 dies

	// child2 receives the replayed initialize but NEVER answers (a hang). Drain its input so the
	// replay writer isn't blocked, then let the timeout fire.
	childIn2 := h.childIn[1]
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
	case err := <-h.done:
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
	p.swapChild(c, nil)

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

	// A coop-driven switch: an editor line carrying "__switch__" is handled here and restarts the box.
	hooks := &Hooks{FromEditor: func(line []byte) (bool, []byte, []byte, bool) {
		if bytes.Contains(line, []byte("__switch__")) {
			return true, nil, nil, true
		}
		return false, nil, nil, false
	}}
	h := newProxyHarness(t, 2, hooks)
	c1, c2 := h.children[0], h.children[1]
	clientInW, clientOut := h.clientIn, h.clientOut
	childIn1, childIn2 := h.childIn[0], h.childIn[1]

	h.initialize(0)

	// session/new → box assigns S1. NO prompt follows, so the session is turn-less. Inline
	// (not h.newSession): the response-id assertion is this test's own sync point.
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

	h.shutdown()
}

// TestProxyDisconnectDuringReplayReturnsCleanly: if the editor disconnects WHILE a restart's replay
// is in flight (the new box not yet published), Run must still tear down and return — not orphan the
// new box on an Out that never closes and block forever. (Prod is reaped by signal ctx-cancel; the
// bug shows as a hang only with a non-cancelable ctx, but the leak is real either way.)
func TestProxyDisconnectDuringReplayReturnsCleanly(t *testing.T) {
	orig1, orig2 := replayStartupGrace, replayIdleTimeout
	replayStartupGrace, replayIdleTimeout = 3*time.Second, 3*time.Second
	defer func() { replayStartupGrace, replayIdleTimeout = orig1, orig2 }()

	hooks := &Hooks{FromEditor: func(line []byte) (bool, []byte, []byte, bool) {
		if bytes.Contains(line, []byte("__switch__")) {
			return true, nil, nil, true
		}
		return false, nil, nil, false
	}}
	h := newProxyHarness(t, 2, hooks)
	c2 := h.children[1]
	clientInW := h.clientIn
	childIn2 := h.childIn[1]

	h.initialize(0)

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
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after a mid-replay disconnect (the new box was orphaned)")
	}
}

// A FromEditor REWRITE (handled=false, toAdapter set) forwards the rewritten line through the
// normal path: the child sees the new body under the editor's request id, and the response
// routes back to the editor — pending tracking intact (unlike handled-mode injections, whose
// responses are swallowed).
func TestProxyFromEditorRewriteKeepsRequestPath(t *testing.T) {
	hooks := &Hooks{FromEditor: func(line []byte) (bool, []byte, []byte, bool) {
		if parse(line).Method != "session/prompt" {
			return false, nil, nil, false
		}
		rew := []byte(`{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{"sessionId":"S1","prompt":[{"type":"text","text":"PREAMBLE + original"}]}}` + "\n")
		return false, nil, rew, false
	}}
	h := newProxyHarness(t, 1, hooks)
	c1, childIn1 := h.children[0], h.childIn[0]
	h.initialize(0)

	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{"sessionId":"S1","prompt":[{"type":"text","text":"original"}]}}`)
	got := readLine(t, childIn1)
	if !strings.Contains(string(got), "PREAMBLE + original") {
		t.Fatalf("child received %s, want the rewritten prompt", got)
	}
	if id := idStr(t, got); id != "7" {
		t.Fatalf("rewrite must keep the editor's request id, got %q", id)
	}
	// The child's answer to that id reaches the editor — the request stayed a normal pending one.
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":7,"result":{"stopReason":"end_turn"}}`)
	if id := idStr(t, readLine(t, h.clientOut)); id != "7" {
		t.Fatalf("editor got response id %q, want 7", id)
	}
}

// TestProxyReplayFailureIsVisibleInThread: when even the re-create session/new fails (a box that
// can't start — e.g. codex refusing its account's sqlite state held by another box), the failure
// must reach the THREAD as an agent_message_chunk naming the error — not just stderr — so the
// user isn't left with a stripped toolbar and silently dead prompts.
func TestProxyReplayFailureIsVisibleInThread(t *testing.T) {
	var stderrBuf, editor bytes.Buffer
	old := warnOut
	warnOut = &stderrBuf
	defer func() { warnOut = old }()

	p := &proxy{
		out:       &editor,
		sessions:  map[string]*sess{"S1": {params: json.RawMessage(`{"cwd":"/w"}`), adapterID: "S1", turned: true}},
		byAdapter: map[string]string{},
		newReqs:   map[string]json.RawMessage{},
		pending:   map[string]bool{},
	}
	fc := newFakeChild()
	br := bufio.NewReader(fc.outR)
	// Round 1: the session/load fails. Round 2: the re-create session/new fails too — the box
	// is genuinely unable to host the session.
	go func() {
		r := bufio.NewReader(fc.inR)
		line, _ := r.ReadBytes('\n')
		h := parse(line)
		writeLine(t, fc.outW, `{"jsonrpc":"2.0","id":`+string(h.ID)+`,"error":{"code":-1,"message":"no such session"}}`)
		line, _ = r.ReadBytes('\n')
		h = parse(line)
		writeLine(t, fc.outW, `{"jsonrpc":"2.0","id":`+string(h.ID)+`,"error":{"code":1001,"message":"Codex process has exited with code 1"}}`)
	}()
	if err := p.replay(fc.child(), br); err != nil {
		t.Fatalf("replay returned %v", err)
	}
	out := editor.String()
	if !strings.Contains(out, "agent_message_chunk") || !strings.Contains(out, "could not be re-established") {
		t.Errorf("the editor must get an in-thread notice for the dead session, got: %q", out)
	}
	if !strings.Contains(out, "Codex process has exited with code 1") {
		t.Errorf("the notice must carry the box's actual error, got: %q", out)
	}
	if !strings.Contains(out, `"sessionId":"S1"`) {
		t.Errorf("the notice must target the editor's session id, got: %q", out)
	}
}
