package acpproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeChild backs a Child with in-memory pipes: the test reads inR to see what the
// proxy sent the child, and writes outW to simulate the child's replies.
type fakeChild struct {
	inR, outR *io.PipeReader
	inW, outW *io.PipeWriter
}

type blockingWriteCloser struct {
	entered chan struct{}
	release chan struct{}
}

func (w *blockingWriteCloser) Write(p []byte) (int, error) {
	select {
	case <-w.entered:
	default:
		close(w.entered)
	}
	<-w.release
	return len(p), nil
}

func (*blockingWriteCloser) Close() error { return nil }

type failingWriteCloser struct{ err error }

func (w failingWriteCloser) Write([]byte) (int, error) { return 0, w.err }
func (failingWriteCloser) Close() error                { return nil }

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

func writeLineAsync(w io.Writer, s string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := io.WriteString(w, s+"\n")
		done <- err
	}()
	return done
}

func TestProxyTargetSettingsAreAcknowledgementGated(t *testing.T) {
	modelID, effortID := InjectPrefix+"model", InjectPrefix+"effort"
	h := newProxyHarness(t, 1, &Hooks{SessionReady: func(sid string) [][]byte {
		return [][]byte{
			[]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"session/set_config_option","params":{"sessionId":%q,"configId":"model","value":"m"}}`+"\n", modelID, sid)),
			[]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"session/set_config_option","params":{"sessionId":%q,"configId":"effort","value":"xhigh"}}`+"\n", effortID, sid)),
		}
	}})
	c := h.children[0]

	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w"}}`)
	readLine(t, h.childIn[0])
	wrote := writeLineAsync(c.outW, `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"S1"}}`)
	if line := readLine(t, h.childIn[0]); string(trimQuotes(parse(line).ID)) != modelID {
		t.Fatalf("first target request = %s, want model", line)
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if id := idStr(t, readLine(t, h.clientOut)); id != "2" {
		t.Fatalf("session/new response id = %q", id)
	}

	// The editor can prompt as soon as it sees session/new, but the prompt must wait behind both
	// setting acknowledgements. If it overtakes, the next child frame below exposes the regression.
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}`)
	writeLine(t, c.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":{"configOptions":[]}}`, modelID))
	if line := readLine(t, h.childIn[0]); string(trimQuotes(parse(line).ID)) != effortID {
		t.Fatalf("frame after model acknowledgement = %s, want effort before prompt", line)
	}
	writeLine(t, c.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":{"configOptions":[]}}`, effortID))
	if line := readLine(t, h.childIn[0]); method(t, line) != "session/prompt" || idStr(t, line) != "3" {
		t.Fatalf("frame after effort acknowledgement = %s, want held prompt", line)
	}
	writeLine(t, c.outW, `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`)
	readLine(t, h.clientOut)
	_ = h.shutdown()
}

func TestProxyTargetSettingFailureBlocksPrompt(t *testing.T) {
	modelID, effortID := InjectPrefix+"model", InjectPrefix+"effort"
	hooks := &Hooks{
		SessionReady: func(sid string) [][]byte {
			return [][]byte{
				[]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"session/set_config_option","params":{"sessionId":%q,"configId":"model","value":"bad"}}`+"\n", modelID, sid)),
				[]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"session/set_config_option","params":{"sessionId":%q,"configId":"effort","value":"xhigh"}}`+"\n", effortID, sid)),
			}
		},
		InjectedResponse: func(request, response []byte) []byte {
			if len(parse(response).Error) == 0 {
				return nil
			}
			return sessionNoticeLine(sessionID(parse(request).Params), "target rejected")
		},
	}
	h := newProxyHarness(t, 1, hooks)
	c := h.children[0]

	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w"}}`)
	readLine(t, h.childIn[0])
	wrote := writeLineAsync(c.outW, `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"S1"}}`)
	readLine(t, h.childIn[0]) // model request
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	readLine(t, h.clientOut) // session/new response
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}`)
	writeLine(t, c.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"error":{"code":-32602,"message":"unsupported model"}}`, modelID))
	first, second := readLine(t, h.clientOut), readLine(t, h.clientOut)
	joined := string(first) + string(second)
	if !strings.Contains(joined, "target rejected") || !strings.Contains(joined, `"sessionId":"S1"`) ||
		!strings.Contains(joined, `"id":3`) || !strings.Contains(joined, "ACP target settings failed") {
		t.Fatalf("setting notice and held-prompt failure were incomplete:\n%s", joined)
	}
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}`)
	if response := readLine(t, h.clientOut); idStr(t, response) != "4" || !strings.Contains(string(response), "unsupported model") {
		t.Fatalf("future prompt must stay blocked after target failure: %s", response)
	}
	_ = h.shutdown()
}

func TestProxyTargetSettingTimeoutBlocksPrompt(t *testing.T) {
	original := forceReplyTimeout
	forceReplyTimeout = 50 * time.Millisecond
	defer func() { forceReplyTimeout = original }()
	id := InjectPrefix + "timeout"
	forwarded := 0
	h := newProxyHarness(t, 1, &Hooks{
		SessionReady: func(sid string) [][]byte {
			return [][]byte{[]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"session/set_config_option","params":{"sessionId":%q,"configId":"model","value":"m"}}`+"\n", id, sid))}
		},
		PromptForwarded: func([]byte, bool) { forwarded++ },
	})
	c := h.children[0]
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w"}}`)
	readLine(t, h.childIn[0])
	wrote := writeLineAsync(c.outW, `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"S1"}}`)
	readLine(t, h.childIn[0]) // setting request, intentionally never answered
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	readLine(t, h.clientOut)
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}`)
	if response := readLine(t, h.clientOut); idStr(t, response) != "3" || !strings.Contains(string(response), "timed out") {
		t.Fatalf("timed-out setting did not fail the held prompt: %s", response)
	}
	if forwarded != 0 {
		t.Fatalf("timed-out held prompt was admitted %d times, want zero", forwarded)
	}
	_ = h.shutdown()
}

func TestProxyTargetSettingWriteFailureBlocksPrompt(t *testing.T) {
	var out bytes.Buffer
	id := InjectPrefix + "write"
	p := &proxy{
		out:   &out,
		child: &Child{In: failingWriteCloser{err: errors.New("closed input")}},
		hooks: &Hooks{SessionReady: func(sid string) [][]byte {
			return [][]byte{[]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"session/set_config_option","params":{"sessionId":%q,"configId":"model","value":"m"}}`, id, sid))}
		}},
		sessions:    map[string]*sess{"S1": {adapterID: "S1"}},
		byAdapter:   map[string]string{},
		sessionReqs: map[string]sessionRequest{},
		pending:     map[string]bool{},
		injected:    map[string][]byte{},
		forceByID:   map[string]*forceChain{},
		forceBySess: map[string]*forceChain{},
		forceFailed: map[string]string{},
	}
	p.forceSession("S1")
	p.fromClient([]byte(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}` + "\n"))
	if got := out.String(); !strings.Contains(got, `"id":4`) || !strings.Contains(got, "write failed: closed input") {
		t.Fatalf("setting write failure did not block the prompt: %s", got)
	}
}

func TestProxyReplayPublishesChildWithTargetGate(t *testing.T) {
	var out bytes.Buffer
	writer := &blockingWriteCloser{entered: make(chan struct{}), release: make(chan struct{})}
	c := &Child{In: writer, Stop: func() {}}
	id := InjectPrefix + "replay"
	p := &proxy{
		out:         &out,
		child:       &Child{In: failingWriteCloser{err: errors.New("old")}},
		sessions:    map[string]*sess{"S1": {adapterID: "N2"}},
		byAdapter:   map[string]string{"N2": "S1"},
		sessionReqs: map[string]sessionRequest{},
		pending:     map[string]bool{},
		injected:    map[string][]byte{},
		forceByID:   map[string]*forceChain{},
		forceBySess: map[string]*forceChain{},
		forceFailed: map[string]string{},
	}
	request := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"session/set_config_option","params":{"sessionId":"N2","configId":"model","value":"m"}}`+"\n", id))
	done := make(chan struct{})
	go func() {
		p.swapChild(c, nil, map[string][][]byte{"N2": {request}}, nil)
		close(done)
	}()
	<-writer.entered // child is published and its first setting write is blocked
	p.fromClient([]byte(`{"jsonrpc":"2.0","id":5,"method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}` + "\n"))
	p.mu.Lock()
	held := len(p.forceBySess["N2"].held)
	p.mu.Unlock()
	if held != 1 {
		t.Fatalf("prompt raced replay publication instead of entering the preinstalled gate (held=%d)", held)
	}
	close(writer.release)
	<-done
	p.resetForceState()
}

func TestProxyInjectedFailureNoticeRemapsAdapterSession(t *testing.T) {
	var out bytes.Buffer
	id := InjectPrefix + "remap"
	request := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"session/set_config_option","params":{"sessionId":"N2","configId":"model","value":"bad"}}`, id))
	p := &proxy{
		out:         &out,
		hooks:       &Hooks{InjectedResponse: func([]byte, []byte) []byte { return sessionNoticeLine("N2", "rejected") }},
		sessions:    map[string]*sess{"S1": {adapterID: "N2"}},
		byAdapter:   map[string]string{"N2": "S1"},
		injected:    map[string][]byte{id: request},
		forceByID:   map[string]*forceChain{},
		forceBySess: map[string]*forceChain{},
		forceFailed: map[string]string{},
	}
	p.handleInjectedResponse([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"error":{"code":-32602,"message":"bad"}}`, id)))
	if got := out.String(); !strings.Contains(got, `"sessionId":"S1"`) || strings.Contains(got, `"sessionId":"N2"`) {
		t.Fatalf("injected failure notice was not remapped to the editor session: %s", got)
	}
	before := out.Len()
	p.handleInjectedResponse([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"error":{"code":-32602,"message":"late"}}`, id)))
	if out.Len() != before {
		t.Fatalf("stale injected response produced a second notice: %s", out.String()[before:])
	}
}

func TestProxySessionLoadAppliesTargetBeforeReturning(t *testing.T) {
	id := InjectPrefix + "load-model"
	h := newProxyHarness(t, 1, &Hooks{SessionReady: func(sid string) [][]byte {
		return [][]byte{[]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"session/set_config_option","params":{"sessionId":%q,"configId":"model","value":"m"}}`+"\n", id, sid))}
	}})
	c := h.children[0]
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":7,"method":"session/load","params":{"cwd":"/w","sessionId":"S1"}}`)
	readLine(t, h.childIn[0])
	wrote := writeLineAsync(c.outW, `{"jsonrpc":"2.0","id":7,"result":{"configOptions":[]}}`)
	if setting := readLine(t, h.childIn[0]); string(trimQuotes(parse(setting).ID)) != id || !strings.Contains(string(setting), `"sessionId":"S1"`) {
		t.Fatalf("session/load target setting = %s", setting)
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if response := readLine(t, h.clientOut); idStr(t, response) != "7" {
		t.Fatalf("session/load response = %s", response)
	}
	writeLine(t, c.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":{}}`, id))
	_ = h.shutdown()
}

func TestProxyRejectsRequestsDuringReloadHandoff(t *testing.T) {
	var out bytes.Buffer
	p := &proxy{
		out:     &out,
		pending: map[string]bool{"9": true},
		sessionReqs: map[string]sessionRequest{
			"9": {method: "session/new", params: json.RawMessage(`{"cwd":"/w"}`)},
		},
	}
	p.beginReload()
	p.fromClient([]byte(`{"jsonrpc":"2.0","id":10,"method":"session/new","params":{"cwd":"/w"}}` + "\n"))
	if got := out.String(); !strings.Contains(got, `"id":9`) || !strings.Contains(got, `"id":10`) || strings.Count(got, "agent restarted, please retry") != 2 {
		t.Fatalf("request raced with reload without a retry response: %s", got)
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

	// A completed prompt makes the session reloadable under response-gated lifecycle state.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1"}}`)
	if m := method(t, readLine(t, childIn1)); m != "session/prompt" {
		t.Fatalf("child1 frame = %q, want session/prompt", m)
	}
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`)
	readLine(t, clientOut)
	// A later request is still in flight when this child dies.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"S1"}}`)
	readLine(t, childIn1)

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

	// The in-flight request (id 4) is failed back to the client so it isn't hung.
	errLine := readLine(t, clientOut)
	if id := idStr(t, errLine); id != "4" || !strings.Contains(string(errLine), `"error"`) {
		t.Fatalf("expected error response for id 4, got %s", errLine)
	}

	// Forwarding resumed: a new request reaches child2.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":5,"method":"session/prompt","params":{}}`)
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
	editorPrompts := 0
	forwardedPrompts := 0
	readyCalls := 0
	resumeAdmitted := make(chan struct{})
	hooks := &Hooks{
		FromEditor: func(line []byte) (bool, []byte, []byte, bool) {
			if parse(line).Method == "session/prompt" {
				editorPrompts++
			}
			return false, nil, nil, false
		},
		PromptForwarded: func(line []byte, _ bool) {
			if parse(line).Method == "session/prompt" {
				forwardedPrompts++
				if forwardedPrompts == 2 {
					close(resumeAdmitted)
				}
			}
		},
		SessionReady: func(sid string) [][]byte {
			readyCalls++
			if readyCalls == 2 {
				return [][]byte{[]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":"coop-inject-resume-setting","method":"session/set_config_option","params":{"sessionId":%q,"configId":"model","value":"target"}}`+"\n", sid))}
			}
			return nil
		},
		ResumePrompt: func(sid string) []byte {
			if sid == "S1" && !resumed {
				resumed = true
				return []byte(`{"jsonrpc":"2.0","id":"resume-9","method":"session/prompt","params":{"sessionId":"S1","prompt":[{"type":"text","text":"again"}]}}` + "\n")
			}
			return nil
		},
	}
	h := newProxyHarness(t, 2, hooks)
	c1, c2 := h.children[0], h.children[1]
	clientInW, clientOut := h.clientIn, h.clientOut
	childIn1, childIn2 := h.childIn[0], h.childIn[1]

	h.initialize(0)
	h.newSession(0, "S1")

	// The first prompt is still in flight when the child dies, so it has not yet established a
	// reloadable transcript. Replay creates a fresh native session before resending the prompt.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1"}}`)
	readLine(t, childIn1)

	// child1 dies → replay initialize + session/new on child2.
	c1.outW.Close()
	for {
		h := parse(readLine(t, childIn2))
		result := `{}`
		if h.Method == "session/new" {
			result = `{"sessionId":"S1b"}`
		}
		writeLine(t, c2.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, string(h.ID), result))
		if h.Method == "session/new" {
			break
		}
	}

	// swapChild fails the in-flight id 3 → drain it first (it's written before the resume is queued).
	if id := idStr(t, readLine(t, clientOut)); id != "3" {
		t.Fatalf("expected in-flight id 3 failed on swap, got %q", id)
	}

	// The replay installs target settings before publishing the child. The synthetic resume is held
	// behind that chain and has neither re-entered FromEditor nor been admitted as pending yet.
	settingLine := readLine(t, childIn2)
	setting := parse(settingLine)
	if setting.Method != "session/set_config_option" || string(trimQuotes(setting.ID)) != "coop-inject-resume-setting" {
		t.Fatalf("frame before resume = %s, want target setting", settingLine)
	}
	if editorPrompts != 1 || forwardedPrompts != 1 {
		t.Fatalf("before setting ack: FromEditor=%d admitted=%d, want 1/1", editorPrompts, forwardedPrompts)
	}
	writeLine(t, c2.outW, `{"jsonrpc":"2.0","id":"coop-inject-resume-setting","result":{}}`)

	// After the swap, ResumePrompt fires: the prompt is re-injected to the new box.
	fr := parse(readLine(t, childIn2))
	if fr.Method != "session/prompt" {
		t.Fatalf("resume frame = %q, want session/prompt", fr.Method)
	}
	if sid := sessionID(fr.Params); sid != "S1b" {
		t.Fatalf("resumed prompt sessionId = %q, want fresh native S1b", sid)
	}
	if string(trimQuotes(fr.ID)) != "resume-9" {
		t.Fatalf("resumed prompt id = %s, want resume-9", fr.ID)
	}
	if editorPrompts != 1 {
		t.Fatalf("FromEditor saw %d prompts, want only the real editor prompt", editorPrompts)
	}
	select {
	case <-resumeAdmitted:
	case <-time.After(time.Second):
		t.Fatal("synthetic prompt was written but never admitted")
	}
	if forwardedPrompts != 2 {
		t.Fatalf("admitted prompts = %d, want real + synthetic after setting success", forwardedPrompts)
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

	// child1 dies before the first prompt succeeds → replay initialize + session/new on child2.
	h.children[0].outW.Close()
	for {
		fr := parse(readLine(t, h.childIn[1]))
		result := `{}`
		if fr.Method == "session/new" {
			result = `{"sessionId":"S1b"}`
		}
		writeLine(t, c2.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, string(fr.ID), result))
		if fr.Method == "session/new" {
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
	if sid := sessionID(fr.Params); sid != "S1b" {
		t.Fatalf("resumed prompt sessionId = %q, want fresh native S1b", sid)
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

	// A successful prompt makes the session reloadable; a later prompt stays in flight.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1"}}`)
	readLine(t, childIn1)
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`)
	readLine(t, clientOut)
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"S1"}}`)
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

	// The first thing the editor sees post-swap is the failed in-flight id 4 — NOT the re-stream.
	errLine := readLine(t, clientOut)
	if id := idStr(t, errLine); id != "4" || strings.Contains(string(errLine), "replayed-history") {
		t.Fatalf("first post-swap frame must be the failed id 4, got: %s", errLine)
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

	// A successful prompt makes the session reloadable; a later prompt remains in flight.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1"}}`)
	readLine(t, childIn1)
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`)
	readLine(t, clientOut)
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"S1"}}`)
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

	// The in-flight id 4 fails on swap, then the config update reaches the editor.
	if id := idStr(t, readLine(t, clientOut)); id != "4" {
		t.Fatalf("expected the in-flight id 4 failed first, got %q", id)
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
		out:         io.Discard,
		sessions:    map[string]*sess{"S1": {params: json.RawMessage(`{"cwd":"/w"}`), adapterID: "S1", turned: true}},
		byAdapter:   map[string]string{},
		sessionReqs: map[string]sessionRequest{},
		pending:     map[string]bool{},
		hooks:       &Hooks{SessionRecreated: func(sid string) { recreated = append(recreated, sid) }},
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

func TestProxyReplayCommitsBindingsOnlyAfterCandidateCompletes(t *testing.T) {
	var recreated atomic.Int32
	p := &proxy{
		out: io.Discard,
		sessions: map[string]*sess{
			"E1": {params: json.RawMessage(`{"cwd":"/one"}`), adapterID: "A1", provider: "claude", turned: true},
			"E2": {params: json.RawMessage(`{"cwd":"/two"}`), adapterID: "A2", provider: "claude", turned: true},
		},
		byAdapter: map[string]string{}, retired: map[string]bool{}, sessionReqs: map[string]sessionRequest{}, pending: map[string]bool{},
		hooks: &Hooks{SessionRecreated: func(string) { recreated.Add(1) }},
	}
	failed := newFakeChild()
	failedChild := failed.child()
	failedChild.Provider = "codex"
	go func() {
		r := bufio.NewReader(failed.inR)
		line, _ := r.ReadBytes('\n')
		h := parse(line)
		eid, _ := strings.CutPrefix(string(trimQuotes(h.ID)), replayPrefix+"new-")
		writeLine(t, failed.outW, `{"jsonrpc":"2.0","id":`+string(h.ID)+`,"result":{"sessionId":"abandoned-`+eid+`"}}`)
		_, _ = r.ReadBytes('\n')
		failed.outW.Close() // the second session never establishes: abandon the whole candidate
	}()
	if err := p.replay(failedChild, bufio.NewReader(failed.outR)); err != nil {
		t.Fatalf("failed candidate replay returned %v", err)
	}
	if p.sessions["E1"].provider != "claude" || p.sessions["E2"].provider != "claude" ||
		strings.HasPrefix(p.sessions["E1"].adapterID, "abandoned-") || strings.HasPrefix(p.sessions["E2"].adapterID, "abandoned-") {
		t.Fatalf("partial candidate bindings escaped: E1=%+v E2=%+v", p.sessions["E1"], p.sessions["E2"])
	}
	if recreated.Load() != 0 {
		t.Fatalf("partial candidate fired SessionRecreated %d times", recreated.Load())
	}

	good := newFakeChild()
	goodChild := good.child()
	goodChild.Provider = "codex"
	go func() {
		r := bufio.NewReader(good.inR)
		for i := 0; i < 2; i++ {
			line, _ := r.ReadBytes('\n')
			h := parse(line)
			eid, _ := strings.CutPrefix(string(trimQuotes(h.ID)), replayPrefix+"new-")
			writeLine(t, good.outW, `{"jsonrpc":"2.0","id":`+string(h.ID)+`,"result":{"sessionId":"C-`+eid+`"}}`)
		}
	}()
	if err := p.replay(goodChild, bufio.NewReader(good.outR)); err != nil {
		t.Fatalf("complete candidate replay returned %v", err)
	}
	if p.sessions["E1"].adapterID != "C-E1" || p.sessions["E2"].adapterID != "C-E2" ||
		p.sessions["E1"].provider != "codex" || p.sessions["E2"].provider != "codex" {
		t.Fatalf("complete candidate did not commit atomically: E1=%+v E2=%+v", p.sessions["E1"], p.sessions["E2"])
	}
	if recreated.Load() != 2 {
		t.Fatalf("complete candidate fired SessionRecreated %d times, want 2", recreated.Load())
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
	var editor bytes.Buffer
	p := &proxy{
		out:         &editor,
		sessions:    map[string]*sess{"S1": {params: json.RawMessage(`{"cwd":"/w"}`), adapterID: "S1", turned: true}},
		byAdapter:   map[string]string{},
		sessionReqs: map[string]sessionRequest{},
		pending:     map[string]bool{},
		hooks:       &Hooks{SessionRecreated: func(sid string) { recreated = append(recreated, sid) }},
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
	if !p.unavailable["S1"] {
		t.Fatal("a session that failed load and re-create remained routable on the new child")
	}
	if len(recreated) != 0 {
		t.Errorf("SessionRecreated fired %v for a failed re-create, want none", recreated)
	}
	if !strings.Contains(buf.String(), "could not be re-created") {
		t.Errorf("expected the re-create failure warning, got: %q", buf.String())
	}

	// A failed replay cannot make the editor thread immortal. Close is handled locally and retains
	// the unbound marker so a subsequent delete is local too.
	p.fromClient([]byte(`{"jsonrpc":"2.0","id":9,"method":"session/close","params":{"sessionId":"S1"}}` + "\n"))
	if s := p.sessions["S1"]; s == nil || !s.closed || !p.unavailable["S1"] {
		t.Fatalf("local close after failed replay left session active/unavailable: %+v unavailable=%v", s, p.unavailable)
	}
	if !strings.Contains(editor.String(), `"id":9,"result":{}`) {
		t.Fatalf("local close did not answer the editor: %q", editor.String())
	}
	p.fromClient([]byte(`{"jsonrpc":"2.0","id":10,"method":"session/delete","params":{"sessionId":"S1"}}` + "\n"))
	if p.sessions["S1"] != nil || p.unavailable["S1"] {
		t.Fatalf("delete after local close retained failed identity: sessions=%v unavailable=%v", p.sessions, p.unavailable)
	}
	if !strings.Contains(editor.String(), `"id":10,"result":{}`) {
		t.Fatalf("delete after local close did not answer the editor: %q", editor.String())
	}
}

func TestProxyDeletesUnavailableForeignSessionLocally(t *testing.T) {
	var out bytes.Buffer
	p := &proxy{
		out:         &out,
		child:       &Child{Provider: "codex"},
		sessions:    map[string]*sess{"S1": {adapterID: "CLAUDE-NATIVE", provider: "claude", turned: true}},
		byAdapter:   map[string]string{"CLAUDE-NATIVE": "S1"},
		unavailable: map[string]bool{"S1": true},
		retired:     map[string]bool{},
		sessionReqs: map[string]sessionRequest{},
		pending:     map[string]bool{},
	}
	p.fromClient([]byte(`{"jsonrpc":"2.0","id":10,"method":"session/delete","params":{"sessionId":"S1"}}` + "\n"))
	if p.sessions["S1"] != nil || p.byAdapter["CLAUDE-NATIVE"] != "" || p.unavailable["S1"] {
		t.Fatalf("local delete retained foreign unavailable identity: sessions=%v byAdapter=%v unavailable=%v", p.sessions, p.byAdapter, p.unavailable)
	}
	if p.retired["CLAUDE-NATIVE"] {
		t.Fatal("foreign native id was retired against the current provider generation")
	}
	if !strings.Contains(out.String(), `"id":10,"result":{}`) {
		t.Fatalf("local delete did not answer the editor: %q", out.String())
	}
}

func TestProxyBestEffortDeletesUnavailableSameProviderSession(t *testing.T) {
	fc := newFakeChild()
	child := fc.child()
	child.Provider = "codex"
	var out bytes.Buffer
	p := &proxy{
		out:         &out,
		child:       child,
		generation:  3,
		sessions:    map[string]*sess{"S1": {adapterID: "CODEX-NATIVE", provider: "codex", turned: true}},
		byAdapter:   map[string]string{"CODEX-NATIVE": "S1"},
		unavailable: map[string]bool{"S1": true},
		retired:     map[string]bool{},
		sessionReqs: map[string]sessionRequest{},
		pending:     map[string]bool{},
	}
	done := make(chan struct{})
	go func() {
		p.fromClient([]byte(`{"jsonrpc":"2.0","id":11,"method":"session/delete","params":{"sessionId":"S1"}}` + "\n"))
		close(done)
	}()
	cleanup := parse(readLine(t, bufio.NewReader(fc.inR)))
	<-done
	if cleanup.Method != "session/delete" || sessionID(cleanup.Params) != "CODEX-NATIVE" ||
		!strings.HasPrefix(string(trimQuotes(cleanup.ID)), InjectPrefix+"delete-") {
		t.Fatalf("best-effort provider cleanup = method %q id %s params %s", cleanup.Method, cleanup.ID, cleanup.Params)
	}
	if p.sessions["S1"] != nil || !strings.Contains(out.String(), `"id":11,"result":{}`) {
		t.Fatalf("same-provider unavailable delete did not complete locally: sessions=%v out=%q", p.sessions, out.String())
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

// TestFailAllPendingClearsSessionReqs: a session mutation in flight when the child dies must not
// leak into the next generation, where its JSON-RPC id may be reused.
func TestFailAllPendingClearsSessionReqs(t *testing.T) {
	p := &proxy{
		out:         io.Discard,
		sessions:    map[string]*sess{},
		byAdapter:   map[string]string{},
		sessionReqs: map[string]sessionRequest{`"7"`: {method: "session/new", params: json.RawMessage(`{"cwd":"/w"}`)}},
		pending:     map[string]bool{`"7"`: true},
	}
	p.failAllPending()
	if len(p.sessionReqs) != 0 {
		t.Errorf("failAllPending should clear staged session requests, got %v", p.sessionReqs)
	}
	if len(p.pending) != 0 {
		t.Errorf("failAllPending should clear pending, got %v", p.pending)
	}
}

func TestSwapChildDropsStagedMutationForKeptResumeID(t *testing.T) {
	p := &proxy{
		out:         io.Discard,
		sessions:    map[string]*sess{},
		byAdapter:   map[string]string{},
		sessionReqs: map[string]sessionRequest{`"7"`: {method: "session/prompt", editorID: "S1", generation: 1}},
		pending:     map[string]bool{`"7"`: true},
	}
	fc := newFakeChild()
	p.swapChild(fc.child(), map[string]bool{`"7"`: true}, nil, nil)
	if len(p.sessionReqs) != 0 || len(p.pending) != 0 {
		t.Fatalf("kept resume retained old-generation mutation: requests=%v pending=%v", p.sessionReqs, p.pending)
	}
}

func TestProxyRejectsStagedSessionMutationFromAnotherGeneration(t *testing.T) {
	fc := newFakeChild()
	child := fc.child()
	var out bytes.Buffer
	p := &proxy{
		out:         &out,
		child:       child,
		generation:  2,
		sessions:    map[string]*sess{},
		byAdapter:   map[string]string{},
		sessionReqs: map[string]sessionRequest{`7`: {method: "session/new", provider: "codex", generation: 1}},
		pending:     map[string]bool{`7`: true},
		retired:     map[string]bool{},
	}
	done := make(chan struct{})
	go func() {
		p.pumpChild(child, bufio.NewReader(fc.outR))
		close(done)
	}()
	writeLine(t, fc.outW, `{"jsonrpc":"2.0","id":7,"result":{"sessionId":"STALE"}}`)
	fc.outW.Close()
	<-done
	if p.sessions["STALE"] != nil || len(p.sessionReqs) != 0 || len(p.pending) != 0 {
		t.Fatalf("stale staged mutation committed across generations: sessions=%v requests=%v pending=%v", p.sessions, p.sessionReqs, p.pending)
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
		out:         &out,
		sessions:    map[string]*sess{},
		byAdapter:   map[string]string{},
		sessionReqs: map[string]sessionRequest{`"1"`: {method: "session/new", params: json.RawMessage(`{"cwd":"/w"}`)}},
		pending:     map[string]bool{`"1"`: true},
	}
	fc := newFakeChild()
	c := fc.child()
	p.swapChild(c, nil, nil, nil)

	if p.child != c {
		t.Error("swapChild did not publish the new child as live")
	}
	if len(p.pending) != 0 || len(p.sessionReqs) != 0 {
		t.Errorf("swapChild should clear pending+sessionReqs, got pending=%v sessionReqs=%v", p.pending, p.sessionReqs)
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
	clientInW := h.clientIn
	childIn2 := h.childIn[1]

	h.initialize(0)

	// An intentional restart: replay(child2) begins and blocks waiting for the initialize response.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":2,"method":"session/set_config_option","params":{"configId":"__switch__"}}`)
	fr := parse(readLine(t, childIn2))
	if fr.Method != "initialize" {
		t.Fatalf("replay frame = %q, want initialize", fr.Method)
	}

	// Editor disconnects mid-replay. The unpublished candidate is stopped immediately; no synthetic
	// response is needed to let replay finish.
	clientInW.Close()

	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after a mid-replay disconnect (the new box was orphaned)")
	}
}

func TestProxyNewerSwitchSupersedesCandidateReplay(t *testing.T) {
	orig1, orig2 := replayStartupGrace, replayIdleTimeout
	replayStartupGrace, replayIdleTimeout = 2*time.Second, 2*time.Second
	defer func() { replayStartupGrace, replayIdleTimeout = orig1, orig2 }()

	hooks := &Hooks{FromEditor: func(line []byte) (bool, []byte, []byte, bool) {
		if bytes.Contains(line, []byte("__switch__")) {
			return true, successResponse(string(parse(line).ID)), nil, true
		}
		return false, nil, nil, false
	}}
	h := newProxyHarness(t, 3, hooks)
	h.initialize(0)

	// Leave one request pending on child 1 so its eventual failure is the publication barrier.
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":2,"method":"session/set_config_option","params":{"configId":"pending"}}`)
	readLine(t, h.childIn[0])
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":3,"method":"session/set_config_option","params":{"configId":"__switch__"}}`)
	readLine(t, h.clientOut)

	firstReplay := parse(readLine(t, h.childIn[1]))
	if firstReplay.Method != "initialize" {
		t.Fatalf("candidate replay frame = %q, want initialize", firstReplay.Method)
	}
	// Switch again while child 2 is waiting on replay. Child 2 must never become authoritative.
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":4,"method":"session/set_config_option","params":{"configId":"__switch__"}}`)
	readLine(t, h.clientOut)
	latestReplay := parse(readLine(t, h.childIn[2]))
	if latestReplay.Method != "initialize" {
		t.Fatalf("latest replay frame = %q, want initialize", latestReplay.Method)
	}
	writeLine(t, h.children[2].outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, latestReplay.ID))
	if id := idStr(t, readLine(t, h.clientOut)); id != "2" {
		t.Fatalf("final swap failed id %q, want pending id 2", id)
	}

	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":5,"method":"session/new","params":{"cwd":"/w"}}`)
	if got := idStr(t, readLine(t, h.childIn[2])); got != "5" {
		t.Fatalf("post-switch request reached id %q on latest child, want 5", got)
	}
	h.shutdown()
}

func TestProxySupersededPublishedReplayKeepsResumePending(t *testing.T) {
	var resumePending atomic.Bool
	resumePending.Store(true)
	recreated := make(chan struct{})
	release := make(chan struct{})
	var blocked atomic.Bool
	hooks := &Hooks{
		ResumePrompt: func(sid string) []byte {
			if sid == "E1" && resumePending.Load() {
				return []byte(`{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{"sessionId":"E1","prompt":[]}}` + "\n")
			}
			return nil
		},
		PromptForwarded: func(_ []byte, synthetic bool) {
			if synthetic {
				resumePending.Store(false)
			}
		},
		SessionRecreated: func(string) {
			if blocked.CompareAndSwap(false, true) {
				close(recreated)
				<-release
			}
		},
	}
	p := &proxy{
		out: io.Discard, hooks: hooks,
		sessions: map[string]*sess{
			"E1": {params: json.RawMessage(`{"cwd":"/w"}`), adapterID: "CLAUDE-NATIVE", provider: "claude", turned: true},
		},
		byAdapter:   map[string]string{"CLAUDE-NATIVE": "E1"},
		sessionReqs: map[string]sessionRequest{`7`: {method: "session/prompt", editorID: "E1"}},
		pending:     map[string]bool{`7`: true},
		retired:     map[string]bool{}, unavailable: map[string]bool{}, reactivating: map[string]string{},
		injected: map[string][]byte{}, forceByID: map[string]*forceChain{}, forceBySess: map[string]*forceChain{}, forceFailed: map[string]string{},
	}

	first := newFakeChild()
	firstChild := first.child()
	firstChild.Provider = "codex"
	go func() {
		line, _ := bufio.NewReader(first.inR).ReadBytes('\n')
		h := parse(line)
		writeLine(t, first.outW, `{"jsonrpc":"2.0","id":`+string(h.ID)+`,"result":{"sessionId":"CODEX-NATIVE"}}`)
	}()
	firstDone := make(chan error, 1)
	go func() { firstDone <- p.replayAt(firstChild, bufio.NewReader(first.outR), 0) }()
	<-recreated // first candidate is published, but its replay tail has not sent the resume
	p.triggerRestart()
	close(release)
	if err := <-firstDone; !errors.Is(err, errReplaySuperseded) {
		t.Fatalf("published replay after a newer switch returned %v, want superseded", err)
	}
	if !resumePending.Load() {
		t.Fatal("superseded replay consumed its one-shot resume")
	}

	latest := newFakeChild()
	latestChild := latest.child()
	latestChild.Provider = "grok"
	seenPrompt := make(chan header, 1)
	go func() {
		r := bufio.NewReader(latest.inR)
		line, _ := r.ReadBytes('\n')
		h := parse(line)
		writeLine(t, latest.outW, `{"jsonrpc":"2.0","id":`+string(h.ID)+`,"result":{"sessionId":"GROK-NATIVE"}}`)
		line, _ = r.ReadBytes('\n')
		seenPrompt <- parse(line)
	}()
	if err := p.replayAt(latestChild, bufio.NewReader(latest.outR), p.currentRestartEpoch()); err != nil {
		t.Fatalf("latest replay: %v", err)
	}
	prompt := <-seenPrompt
	if prompt.Method != "session/prompt" || sessionID(prompt.Params) != "GROK-NATIVE" {
		t.Fatalf("latest candidate did not receive retained resume: method=%q params=%s", prompt.Method, prompt.Params)
	}
	if resumePending.Load() {
		t.Fatal("resume remained pending after reaching the latest child")
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
		out:         &editor,
		sessions:    map[string]*sess{"S1": {params: json.RawMessage(`{"cwd":"/w"}`), adapterID: "S1", turned: true}},
		byAdapter:   map[string]string{},
		sessionReqs: map[string]sessionRequest{},
		pending:     map[string]bool{},
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

// TestProxySnapshotRestore: the full editor/native/provider identity survives a supervisor re-exec,
// so the next child can decide whether the native id belongs to it before choosing load vs new.
func TestProxySnapshotRestore(t *testing.T) {
	src := &proxy{
		out:         io.Discard,
		setup:       [][]byte{[]byte(`{"method":"initialize"}`), []byte(`{"method":"authenticate"}`)},
		sessions:    map[string]*sess{"S1": {params: json.RawMessage(`{"cwd":"/a"}`), adapterID: "adapterX", provider: "codex", turned: true}, "S2": {params: json.RawMessage(`{"cwd":"/b"}`), adapterID: "S2", provider: "claude", closed: true, turned: false}},
		byAdapter:   map[string]string{},
		sessionReqs: map[string]sessionRequest{},
		pending:     map[string]bool{},
	}
	snap := src.snapshot()
	if len(snap.Setup) != 2 || string(snap.Setup[0]) != `{"method":"initialize"}` {
		t.Errorf("setup not snapshotted: %v", snap.Setup)
	}
	dst := &proxy{out: io.Discard, sessions: map[string]*sess{}, byAdapter: map[string]string{}, sessionReqs: map[string]sessionRequest{}, pending: map[string]bool{}}
	dst.restore(snap)
	if len(dst.setup) != 2 || string(dst.setup[1]) != `{"method":"authenticate"}` {
		t.Errorf("setup not restored: %v", dst.setup)
	}
	s1 := dst.sessions["S1"]
	if s1 == nil || !s1.turned || string(s1.params) != `{"cwd":"/a"}` || s1.adapterID != "adapterX" || s1.provider != "codex" {
		t.Errorf("S1 native identity not restored: %+v", s1)
	}
	if s2 := dst.sessions["S2"]; s2 == nil || s2.turned || !s2.closed {
		t.Errorf("S2's closed/turned state must survive: %+v", s2)
	}
}

func TestProxyCommitsSessionLifecycleOnlyAfterSuccess(t *testing.T) {
	fc := newFakeChild()
	child := fc.child()
	child.Provider = "codex"
	outR, outW := io.Pipe()
	var closed, ended []string
	p := &proxy{
		out: outW, child: child,
		hooks: &Hooks{
			SessionClosed: func(sid string) { closed = append(closed, sid) },
			SessionEnded:  func(sid string) { ended = append(ended, sid) },
		},
		sessions: map[string]*sess{
			"S1": {params: json.RawMessage(`{"cwd":"/w"}`), adapterID: "N1", provider: "codex"},
			"S2": {params: json.RawMessage(`{"cwd":"/w"}`), adapterID: "N2", provider: "codex", turned: true},
		},
		byAdapter:   map[string]string{"N1": "S1", "N2": "S2"},
		sessionReqs: map[string]sessionRequest{},
		pending:     map[string]bool{},
		retired:     map[string]bool{},
		injected:    map[string][]byte{},
		forceByID:   map[string]*forceChain{},
		forceBySess: map[string]*forceChain{},
		forceFailed: map[string]string{},
	}
	done := make(chan struct{})
	go func() {
		p.pumpChild(child, bufio.NewReader(fc.outR))
		close(done)
	}()
	childIn := bufio.NewReader(fc.inR)
	editorOut := bufio.NewReader(outR)
	roundTrip := func(request, response string) []byte {
		t.Helper()
		go p.fromClient([]byte(request + "\n"))
		forwarded := readLine(t, childIn)
		writeLine(t, fc.outW, response)
		readLine(t, editorOut)
		return forwarded
	}

	roundTrip(`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"missing","cwd":"/w"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"missing"}}`)
	if p.sessions["missing"] != nil {
		t.Fatal("failed session/load created a replayable session")
	}
	roundTrip(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}`,
		`{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"failed"}}`)
	if p.sessions["S1"].turned {
		t.Fatal("failed prompt marked the session as having a transcript")
	}
	roundTrip(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}`,
		`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`)
	if !p.sessions["S1"].turned {
		t.Fatal("successful prompt did not mark the session reloadable")
	}
	if got := sessionID(parse(roundTrip(`{"jsonrpc":"2.0","id":4,"method":"session/close","params":{"sessionId":"S1"}}`,
		`{"jsonrpc":"2.0","id":4,"error":{"code":-32603,"message":"busy"}}`)).Params); got != "N1" {
		t.Fatalf("close forwarded native id %q, want N1", got)
	}
	if p.sessions["S1"] == nil {
		t.Fatal("failed close removed the session")
	}
	roundTrip(`{"jsonrpc":"2.0","id":5,"method":"session/close","params":{"sessionId":"S1"}}`,
		`{"jsonrpc":"2.0","id":5,"result":{}}`)
	if s := p.sessions["S1"]; s == nil || !s.closed || s.adapterID != "N1" || p.byAdapter["N1"] != "S1" || !p.retired["N1"] {
		t.Fatalf("successful close did not deactivate and retain native identity: session=%+v byAdapter=%v retired=%v", s, p.byAdapter, p.retired)
	}
	if late := p.remapToEditor(sessionNoticeLine("N1", "late")); late != nil {
		t.Fatalf("late notification for closed native N1 was not dropped: %s", late)
	}
	go p.fromClient([]byte(`{"jsonrpc":"2.0","id":6,"method":"session/resume","params":{"sessionId":"S1","cwd":"/w"}}` + "\n"))
	resumed := readLine(t, childIn)
	if got := sessionID(parse(resumed).Params); got != "N1" {
		t.Fatalf("resume after close forwarded native id %q, want N1", got)
	}
	writeLine(t, fc.outW, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"N1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"restored history"}}}}`)
	if history := readLine(t, editorOut); sessionID(parse(history).Params) != "S1" || !strings.Contains(string(history), "restored history") {
		t.Fatalf("resume history was dropped before the success response: %s", history)
	}
	writeLine(t, fc.outW, `{"jsonrpc":"2.0","id":6,"result":{"configOptions":[]}}`)
	readLine(t, editorOut)
	if s := p.sessions["S1"]; s == nil || s.closed || s.adapterID != "N1" || p.retired["N1"] {
		t.Fatalf("successful resume did not reactivate the retained identity: %+v retired=%v", s, p.retired)
	}
	roundTrip(`{"jsonrpc":"2.0","id":7,"method":"session/close","params":{"sessionId":"S1"}}`,
		`{"jsonrpc":"2.0","id":7,"result":{}}`)
	roundTrip(`{"jsonrpc":"2.0","id":8,"method":"session/delete","params":{"sessionId":"S2"}}`,
		`{"jsonrpc":"2.0","id":8,"result":{}}`)
	if p.sessions["S2"] != nil || p.byAdapter["N2"] != "" {
		t.Fatalf("successful delete retained session S2: sessions=%v byAdapter=%v", p.sessions, p.byAdapter)
	}
	if !slices.Equal(closed, []string{"S1", "S1"}) || !slices.Equal(ended, []string{"S2"}) {
		t.Fatalf("session lifecycle hooks closed=%v ended=%v, want [S1 S1] and [S2]", closed, ended)
	}
	roundTrip(`{"jsonrpc":"2.0","id":9,"method":"session/resume","params":{"sessionId":"R3","cwd":"/w"}}`,
		`{"jsonrpc":"2.0","id":9,"result":{"configOptions":[]}}`)
	if s := p.sessions["R3"]; s == nil || s.adapterID != "R3" || s.provider != "codex" || !s.turned {
		t.Fatalf("successful resume did not commit native identity: %+v", s)
	}

	fc.outW.Close()
	<-done
}

func TestProxyDropsRetiredChildOutputAndReusedResponseID(t *testing.T) {
	oldFC, nextFC := newFakeChild(), newFakeChild()
	oldChild, nextChild := oldFC.child(), nextFC.child()
	oldChild.Provider, nextChild.Provider = "claude", "codex"
	var out bytes.Buffer
	p := &proxy{
		out: &out, sessions: map[string]*sess{}, byAdapter: map[string]string{}, retired: map[string]bool{},
		sessionReqs: map[string]sessionRequest{}, pending: map[string]bool{}, injected: map[string][]byte{},
		forceByID: map[string]*forceChain{}, forceBySess: map[string]*forceChain{}, forceFailed: map[string]string{},
	}
	p.setChild(oldChild)
	oldDone := make(chan struct{})
	go func() {
		p.pumpChild(oldChild, bufio.NewReader(oldFC.outR))
		close(oldDone)
	}()
	p.swapChild(nextChild, nil, nil, nil)
	nextDone := make(chan struct{})
	go func() {
		p.pumpChild(nextChild, bufio.NewReader(nextFC.outR))
		close(nextDone)
	}()

	go p.fromClient([]byte(`{"jsonrpc":"2.0","id":7,"method":"session/new","params":{"cwd":"/w"}}` + "\n"))
	readLine(t, bufio.NewReader(nextFC.inR))
	writeLine(t, oldFC.outW, `{"jsonrpc":"2.0","id":7,"result":{"sessionId":"STALE"}}`)
	writeLine(t, oldFC.outW, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"STALE","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"late"}}}}`)
	oldFC.outW.Close()
	<-oldDone
	if p.sessionReqs["7"].method != "session/new" || !p.pending["7"] || p.sessions["STALE"] != nil || strings.Contains(out.String(), "STALE") {
		t.Fatalf("retired child mutated the live generation: pending=%v requests=%v sessions=%v out=%q", p.pending, p.sessionReqs, p.sessions, out.String())
	}
	writeLine(t, nextFC.outW, `{"jsonrpc":"2.0","id":7,"result":{"sessionId":"LIVE"}}`)
	nextFC.outW.Close()
	<-nextDone
	if s := p.sessions["LIVE"]; s == nil || s.provider != "codex" {
		t.Fatalf("live generation response did not commit: %+v", s)
	}
}

func TestProxyDropsNotificationRetiredWhileToEditorRuns(t *testing.T) {
	fc := newFakeChild()
	child := fc.child()
	entered := make(chan struct{})
	release := make(chan struct{})
	var out bytes.Buffer
	p := &proxy{
		out: &out, child: child,
		hooks: &Hooks{ToEditor: func(line []byte) ([]byte, bool) {
			close(entered)
			<-release
			return line, false
		}},
		sessions: map[string]*sess{}, byAdapter: map[string]string{}, retired: map[string]bool{},
		unavailable: map[string]bool{}, reactivating: map[string]string{}, sessionReqs: map[string]sessionRequest{}, pending: map[string]bool{},
	}
	done := make(chan struct{})
	go func() {
		p.pumpChild(child, bufio.NewReader(fc.outR))
		close(done)
	}()
	go func() {
		_, _ = fc.outW.Write([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S1","update":{}}}` + "\n"))
	}()
	<-entered
	p.triggerRestart()
	close(release)
	<-done
	if out.Len() != 0 {
		t.Fatalf("notification retired during ToEditor reached the editor: %s", out.String())
	}
}

// drainReader consumes a reader until EOF so a proxy write to the client side can't deadlock a test
// that isn't asserting on client output.
func drainReader(r *bufio.Reader) {
	for {
		if _, err := r.ReadBytes('\n'); err != nil {
			return
		}
	}
}

// TestProxyResumeReplaysOnFirstChild: a resumed RunWith (Resume set) replays initialize +
// session/load for a restored turned session onto its FIRST child — today's first child skips replay.
func TestProxyResumeReplaysOnFirstChild(t *testing.T) {
	c1 := newFakeChild()
	factory := func(context.Context) (*Child, error) {
		child := c1.child()
		child.Provider = "codex"
		return child, nil
	}
	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()
	childIn1 := bufio.NewReader(c1.inR)
	go drainReader(bufio.NewReader(clientOutR)) // replay pushes config_option_update to the client

	snap := &Snapshot{
		Setup:    [][]byte{[]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)},
		Sessions: []SessionSnap{{EditorID: "S1", AdapterID: "N1", Provider: "codex", Params: json.RawMessage(`{"cwd":"/w"}`), Turned: true}},
	}
	done := make(chan error, 1)
	go func() {
		done <- RunWith(context.Background(), clientInR, clientOutW, factory, nil, RunOpts{Resume: snap})
	}()

	var sawInit, sawLoad bool
	for i := 0; i < 2; i++ {
		line := readLine(t, childIn1)
		h := parse(line)
		switch h.Method {
		case "initialize":
			sawInit = true
		case "session/load":
			if sid := sessionID(h.Params); sid != "N1" {
				t.Fatalf("session/load sessionId = %q, want native N1", sid)
			}
			sawLoad = true
		default:
			t.Fatalf("unexpected resume-replay frame: %s", line)
		}
		writeLine(t, c1.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, string(h.ID)))
	}
	if !sawInit || !sawLoad {
		t.Fatalf("resume replay on first child missing init=%v load=%v", sawInit, sawLoad)
	}
	clientInW.Close()
	<-done
}

func TestProxyResumeCreatesDirectlyForAnotherProvider(t *testing.T) {
	c1 := newFakeChild()
	factory := func(context.Context) (*Child, error) {
		child := c1.child()
		child.Provider = "grok"
		return child, nil
	}
	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()
	childIn := bufio.NewReader(c1.inR)
	go drainReader(bufio.NewReader(clientOutR))
	var recreated atomic.Int32
	recreatedCh := make(chan struct{})
	snap := &Snapshot{
		Setup: [][]byte{[]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)},
		Sessions: []SessionSnap{{
			EditorID: "E1", AdapterID: "C9", Provider: "codex", Params: json.RawMessage(`{"cwd":"/w"}`), Turned: true,
		}},
	}
	done := make(chan error, 1)
	go func() {
		done <- RunWith(context.Background(), clientInR, clientOutW, factory, &Hooks{
			SessionRecreated: func(sid string) {
				if sid == "E1" {
					recreated.Add(1)
					close(recreatedCh)
				}
			},
		}, RunOpts{Resume: snap})
	}()

	for i := 0; i < 2; i++ {
		line := readLine(t, childIn)
		h := parse(line)
		switch h.Method {
		case "initialize":
			writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":`+string(h.ID)+`,"result":{}}`)
		case "session/new":
			if strings.Contains(string(line), "C9") || sessionID(h.Params) != "" {
				t.Fatalf("cross-provider replay leaked foreign native id: %s", line)
			}
			writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":`+string(h.ID)+`,"result":{"sessionId":"G2"}}`)
		case "session/load":
			t.Fatalf("cross-provider replay probed foreign native id: %s", line)
		default:
			t.Fatalf("unexpected replay method %q", h.Method)
		}
	}
	<-recreatedCh
	wrote := writeLineAsync(clientInW, `{"jsonrpc":"2.0","id":8,"method":"session/prompt","params":{"sessionId":"E1","prompt":[]}}`)
	if forwarded := readLine(t, childIn); sessionID(parse(forwarded).Params) != "G2" {
		t.Fatalf("stable editor id did not map to Grok native G2: %s", forwarded)
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":8,"result":{"stopReason":"end_turn"}}`)
	if recreated.Load() != 1 {
		t.Fatalf("SessionRecreated fired %d times, want once", recreated.Load())
	}
	clientInW.Close()
	<-done
}

// TestProxyReloadReturnsSnapshot: firing Reload returns a reloadError carrying a snapshot of the
// live state and stops the child — the editor's transport is meant to survive the re-exec.
func TestProxyReloadReturnsSnapshot(t *testing.T) {
	c1 := newFakeChild()
	factory := func(context.Context) (*Child, error) { return c1.child(), nil }
	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()
	clientOut := bufio.NewReader(clientOutR)
	childIn1 := bufio.NewReader(c1.inR)
	reload := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- RunWith(context.Background(), clientInR, clientOutW, factory, nil, RunOpts{Reload: reload})
	}()

	// Establish initialize + a session so the snapshot has state to carry.
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	readLine(t, childIn1)
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	readLine(t, clientOut)
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w"}}`)
	readLine(t, childIn1)
	writeLine(t, c1.outW, `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"S1"}}`)
	readLine(t, clientOut)

	close(reload) // request the reload
	err := <-done
	snap, ok := ReloadSnapshot(err)
	if !ok || snap == nil {
		t.Fatalf("a reload should return a reloadError carrying a snapshot, got %v", err)
	}
	if len(snap.Setup) == 0 {
		t.Errorf("snapshot should carry the setup (initialize): %+v", snap)
	}
	if len(snap.Sessions) != 1 || snap.Sessions[0].EditorID != "S1" {
		t.Errorf("snapshot should carry session S1: %+v", snap.Sessions)
	}
}

// TestProxyResumeReplayTimeoutRespawnsClean: if the FIRST box after a re-exec HANGS during resume
// replay (errReplayTimeout), the proxy must NOT pump the hung child (that freezes the editor and
// races the reader). It stops the hung child, drops the restored sessions, and spawns a clean first
// child so NEW threads still work. Regression for the resume-degrade blocker.
func TestProxyResumeReplayTimeoutRespawnsClean(t *testing.T) {
	defer func(g time.Duration) { replayStartupGrace = g }(replayStartupGrace)
	replayStartupGrace = 80 * time.Millisecond // the hung first child times out fast

	c1 := newFakeChild() // never answers replay → errReplayTimeout
	c2 := newFakeChild() // the clean respawn
	var calls atomic.Int32
	factory := func(context.Context) (*Child, error) {
		if calls.Add(1) == 1 {
			return c1.child(), nil // the hung first box
		}
		return c2.child(), nil // clean respawn (and any teardown re-spawn — never blocks)
	}
	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()
	clientOut := bufio.NewReader(clientOutR)
	go drainReader(bufio.NewReader(c1.inR)) // let replay's writes to the hung child complete

	snap := &Snapshot{
		Setup:    [][]byte{[]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)},
		Sessions: []SessionSnap{{EditorID: "S1", Params: json.RawMessage(`{"cwd":"/w"}`), Turned: true}},
	}
	done := make(chan error, 1)
	go func() {
		done <- RunWith(context.Background(), clientInR, clientOutW, factory, nil, RunOpts{Resume: snap})
	}()

	// The hung child times out; a brand-new session must then land on the CLEAN respawn (c2), proving
	// the proxy didn't freeze on c1 and recovered to a working first child.
	childIn2 := bufio.NewReader(c2.inR)
	writeLine(t, clientInW, `{"jsonrpc":"2.0","id":9,"method":"session/new","params":{"cwd":"/w2"}}`)
	if m := parse(readLine(t, childIn2)).Method; m != "session/new" {
		t.Fatalf("clean respawn (c2) should receive the new session/new, got %q", m)
	}
	writeLine(t, c2.outW, `{"jsonrpc":"2.0","id":9,"result":{"sessionId":"S9"}}`)
	if h := parse(readLine(t, clientOut)); string(h.ID) != "9" { // the editor sees its result — not frozen
		t.Fatalf("editor should see the session/new result (id 9), got id %q", string(h.ID))
	}
	if calls.Load() < 2 {
		t.Fatalf("hung first box should have forced a respawn (>=2 factory calls), got %d", calls.Load())
	}

	clientInW.Close() // client gone → the proxy stops the live child and returns; no c2.outW race
	<-done
}
