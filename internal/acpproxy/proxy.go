// Package acpproxy keeps an editor's ACP (Agent Client Protocol) connection alive
// across a restart of the agent's container.
//
// `coop acp` normally runs the agent adapter in a box and ties its own lifetime to
// the box: when the container dies (a rebuild, an OOM, Docker restarting), the
// adapter's stdio hits EOF, `coop acp` exits, and the editor reports the server as
// crashed — and editors keep one server process per agent, so it stays dead until
// the whole editor restarts.
//
// The proxy sits between the editor (clientIn/clientOut) and a Child produced by a
// Factory. It speaks just enough ACP — newline-delimited JSON-RPC 2.0 — to record
// the editor's setup handshake (initialize, authenticate) and which sessions are
// live. If the child exits while the editor is still connected, it starts a new
// child, replays that setup handshake and a `session/load` for each live session
// (the conversation persists on the mounted home, and authenticate reloads the
// on-disk token, exactly as on an editor restart), fails any in-flight request so
// the editor isn't left hanging, and resumes forwarding. The editor never sees EOF.
package acpproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Child is one run of the agent adapter (one container). The proxy writes ACP to In
// and reads ACP from Out; Stop force-terminates it.
type Child struct {
	In   io.WriteCloser
	Out  io.Reader
	Stop func()
}

// Factory starts a fresh child. ctx is cancelled when the proxy is shutting down.
type Factory func(ctx context.Context) (*Child, error)

const replayPrefix = "coop-acp-" // id namespace for our synthetic replay requests

const readBuf = 1 << 20 // ACP messages can carry file contents; read generously.

const (
	minHealthy    = 2 * time.Second // a child living less than this counts as a rapid failure
	maxRapidFails = 5               // give up after this many rapid failures in a row
)

// replayStartupGrace bounds how long the FIRST replayed response may take — a restarted box can be
// pulling an image or provisioning a .tool-versions toolchain via asdf (minutes on a cold run), so
// it's deliberately generous. It's the "ready" gate: once the agent answers once, the box is up and
// replayIdleTimeout takes over. replayIdleTimeout bounds the gap between replay responses thereafter
// — so a hang mid-replay (e.g. the agent makes a client request session/load never gets a reply to)
// fails fast instead of freezing the editor. Vars, not consts, so tests can shrink them.
var (
	replayStartupGrace = 5 * time.Minute
	replayIdleTimeout  = 60 * time.Second
)

// errReplayTimeout means a restarted child stopped responding during replay (a hang, not a
// crash). The caller tears the child down and gives up, rather than leaving the editor frozen.
var errReplayTimeout = errors.New("acpproxy: timed out waiting for the restarted agent")

// Run proxies ACP between the editor and children from factory until the editor
// closes clientIn (clean shutdown) or ctx is cancelled. A child that exits while the
// editor is still connected is transparently replaced.
//
// Single-use contract: Run spawns an editor→child reader goroutine that blocks on
// clientIn until EOF. Run does not own clientIn (it's passed in) and can't interrupt a
// blocked Read, so on a ctx-cancel return that goroutine lives until the editor closes
// clientIn. That's fine for the one intended caller — the `coop acp` process, where
// clientIn is os.Stdin and process exit reaps it — but it means Run is single-use per
// clientIn, not a reusable in-process library: don't call it in a loop expecting the
// reader to stop when ctx cancels. Close clientIn to release the goroutine.
func Run(ctx context.Context, clientIn io.Reader, clientOut io.Writer, factory Factory) error {
	p := &proxy{
		out:      clientOut,
		sessions: map[string]json.RawMessage{},
		newReqs:  map[string]json.RawMessage{},
		pending:  map[string]bool{},
	}

	child, err := factory(ctx)
	if err != nil {
		return err
	}
	reader := bufio.NewReaderSize(child.Out, readBuf)
	p.setChild(child)

	clientGone := make(chan struct{})
	go func() {
		// Editor → child.
		_ = readLines(bufio.NewReaderSize(clientIn, readBuf), func(line []byte) error {
			p.fromClient(line)
			return nil
		})
		// Mark shutdown BEFORE killing the child: otherwise the child's death unblocks
		// pumpChild, the loop hits its default branch and respawns a box that then has
		// no client and leaks. With clientGone already closed, the loop exits instead.
		close(clientGone)
		p.shutdownChild()
	}()

	// On shutdown (signal / ctx cancel) stop the current child, so pumpChild unblocks,
	// the loop exits, and the box is torn down even without a client stdin EOF.
	go func() { <-ctx.Done(); p.shutdownChild() }()

	rapid := 0
	for {
		start := time.Now()
		p.pumpChild(reader) // returns when this child's Out closes
		select {
		case <-clientGone:
			child.Stop()
			return nil
		case <-ctx.Done():
			child.Stop()
			return ctx.Err()
		default:
		}
		// A child that dies almost immediately is broken (no runtime, bad auth, a
		// failing image) — respawning in a tight loop would be a fork bomb, so back
		// off and eventually give up, letting the editor see the failure.
		if time.Since(start) < minHealthy {
			rapid++
			if rapid >= maxRapidFails {
				child.Stop()
				p.failAllPending()
				return fmt.Errorf("acpproxy: agent exited %d times within %s of starting; giving up", rapid, minHealthy)
			}
			time.Sleep(time.Duration(rapid) * 200 * time.Millisecond)
		} else {
			rapid = 0
		}
		// The child died while the editor is still here: replace it transparently.
		next, err := factory(ctx)
		if err != nil {
			p.failAllPending()
			return err
		}
		nr := bufio.NewReaderSize(next.Out, readBuf)
		// replay publishes next as the live child (setChild) before failing pending. A replay
		// timeout (the restarted agent hung) is a bounded failure: stop it and give up so the
		// editor isn't frozen waiting — better a clean "server exited" than an infinite hang.
		if err := p.replay(next, nr); err != nil {
			next.Stop()
			p.failAllPending()
			return err
		}
		child.Stop() // release the dead child's pipes + cidfile dir before swapping in the new one
		child, reader = next, nr
	}
}

type proxy struct {
	out io.Writer

	mu       sync.Mutex
	child    *Child
	setup    [][]byte                   // editor's pre-session setup (initialize, authenticate), in order
	sessions map[string]json.RawMessage // sessionId -> session/new params (for session/load)
	newReqs  map[string]json.RawMessage // pending session/new request id -> its params
	pending  map[string]bool            // editor request id -> awaiting a response
}

func (p *proxy) setChild(c *Child) {
	p.mu.Lock()
	p.child = c
	p.mu.Unlock()
}

func (p *proxy) shutdownChild() {
	p.mu.Lock()
	c := p.child
	p.mu.Unlock()
	if c != nil {
		c.Stop()
	}
}

// fromClient records replay-relevant state, then forwards the line to the current
// child. A write to a momentarily-dead child during a swap is dropped; that request
// is covered by failAllPending on the swap.
func (p *proxy) fromClient(line []byte) {
	h := parse(line)
	p.mu.Lock()
	if h.isRequest() {
		switch h.Method {
		case "initialize", "authenticate":
			// The pre-session handshake the editor ran to bring the agent up. A fresh
			// agent needs the same — notably authenticate, which loads the on-disk
			// token (without it the new agent reports "authentication required").
			p.setup = append(p.setup, clone(line))
		case "session/new":
			p.newReqs[string(h.ID)] = h.Params
		case "session/load":
			if sid := sessionID(h.Params); sid != "" {
				p.sessions[sid] = h.Params
			}
		}
		p.pending[string(h.ID)] = true
	}
	var w io.Writer
	if p.child != nil {
		w = p.child.In
	}
	p.mu.Unlock()
	if w != nil {
		_, _ = w.Write(line)
	}
}

// pumpChild forwards child→editor for one child generation, learning session ids from
// session/new responses and clearing pending requests. Returns on the child's EOF.
func (p *proxy) pumpChild(br *bufio.Reader) {
	_ = readLines(br, func(line []byte) error {
		h := parse(line)
		if h.isResponse() {
			id := string(h.ID)
			p.mu.Lock()
			if params, ok := p.newReqs[id]; ok {
				delete(p.newReqs, id)
				if sid := sessionID(h.Result); sid != "" {
					p.sessions[sid] = params
				}
			}
			delete(p.pending, id)
			p.mu.Unlock()
		}
		_, _ = p.out.Write(line)
		return nil
	})
}

// replay brings a freshly-started child up to the editor's current view: re-send
// initialize and a session/load for each live session, consuming their responses
// (and any chatter emitted while loading) so the editor doesn't see them; then fail
// any requests that were in flight when the old child died.
func (p *proxy) replay(c *Child, br *bufio.Reader) error {
	p.mu.Lock()
	setup := make([][]byte, len(p.setup))
	copy(setup, p.setup)
	sessions := make(map[string]json.RawMessage, len(p.sessions))
	for k, v := range p.sessions {
		sessions[k] = v
	}
	p.mu.Unlock()

	var msgs [][]byte
	expect := map[string]bool{}
	for i, line := range setup {
		id := fmt.Sprintf("%ssetup-%d", replayPrefix, i)
		if msg := withID(line, id); msg != nil {
			msgs = append(msgs, msg)
			expect[id] = true
		}
	}
	for sid, params := range sessions {
		id := replayPrefix + "load-" + sid
		if msg := loadRequest(id, sid, params); msg != nil {
			msgs = append(msgs, msg)
			expect[id] = true
		}
	}

	// Write the synthetic requests concurrently with reading their responses, so a
	// full stdin buffer can't deadlock against the child's pending replies.
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for _, m := range msgs {
			if _, err := c.In.Write(m); err != nil {
				return
			}
		}
	}()
	// Bounded read: allow a generous wait for the FIRST response (the box may still be starting),
	// then a tighter idle bound once the agent has answered and is provably up. A child EOF here
	// means it died during replay — break and let Run respawn it; a timeout means it hung — abort.
	timeout := replayStartupGrace
	for len(expect) > 0 {
		line, err := readLineCtx(br, timeout)
		if len(line) > 0 {
			if h := parse(line); h.isResponse() {
				delete(expect, string(trimQuotes(h.ID)))
			}
		}
		if errors.Is(err, errReplayTimeout) {
			return err
		}
		if err != nil {
			break // child EOF: it died mid-replay; fall through, Run's loop respawns it
		}
		timeout = replayIdleTimeout
	}
	<-sent // replay writer done: no concurrent write to c.In
	p.swapChild(c)
	return nil
}

// swapChild publishes c as the live child AND fails the requests that were in flight, in one
// critical section. The two MUST be atomic: a request that arrives during the swap is then
// either (a) seen before the lock — added to pending, failed here, and (since p.child was still
// the dead child) written there and dropped, so the editor's retry is the only delivery; or
// (b) seen after the lock — routed to the live child and NOT in the failed snapshot. Splitting
// setChild and failAllPending left a window where a request could be BOTH routed to the live
// child AND failed — a duplicate response and a re-executed prompt. (Run publishes the initial
// child via setChild; only swaps come through here.)
func (p *proxy) swapChild(c *Child) {
	p.mu.Lock()
	ids := make([]string, 0, len(p.pending))
	for id := range p.pending {
		ids = append(ids, id)
		delete(p.newReqs, id) // a session/new in flight at the swap never gets a response
	}
	p.pending = map[string]bool{}
	p.child = c
	p.mu.Unlock()
	for _, id := range ids {
		_, _ = p.out.Write(errorResponse(id))
	}
}

// readLineCtx reads one newline-terminated line from br, returning errReplayTimeout if d elapses
// first. On a timeout the blocked read is abandoned: its goroutine sends into a buffered channel and
// exits once the child is stopped (which EOFs the reader), so the caller MUST tear the child down
// after a timeout — it must not keep reading br. Used only during replay, where reads are sequential.
func readLineCtx(br *bufio.Reader, d time.Duration) ([]byte, error) {
	type res struct {
		line []byte
		err  error
	}
	ch := make(chan res, 1)
	go func() { line, err := br.ReadBytes('\n'); ch <- res{line, err} }()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.line, r.err
	case <-t.C:
		return nil, errReplayTimeout
	}
}

// failAllPending answers every in-flight editor request with a JSON-RPC error so the
// editor retries instead of hanging on a response the dead child will never send.
func (p *proxy) failAllPending() {
	p.mu.Lock()
	ids := make([]string, 0, len(p.pending))
	for id := range p.pending {
		ids = append(ids, id)
		// A session/new in flight when the child died will never get a response — drop its
		// newReqs entry too, or it leaks (and could wrongly bind a sessionId on a stale id).
		delete(p.newReqs, id)
	}
	p.pending = map[string]bool{}
	p.mu.Unlock()
	for _, id := range ids {
		_, _ = p.out.Write(errorResponse(id))
	}
}

// --- JSON-RPC helpers --------------------------------------------------------

type header struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

func parse(line []byte) header {
	var h header
	_ = json.Unmarshal(line, &h)
	return h
}

func (h header) isRequest() bool  { return h.Method != "" && len(h.ID) > 0 }
func (h header) isResponse() bool { return h.Method == "" && len(h.ID) > 0 }

func sessionID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.SessionID
}

// withID re-stamps a request line with a new (string) id.
func withID(line []byte, id string) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(line, &m) != nil {
		return nil
	}
	idJSON, _ := json.Marshal(id)
	m["id"] = idJSON
	out, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return append(out, '\n')
}

// loadRequest builds a session/load request reusing the original session/new params
// (cwd, mcpServers, …) with the sessionId set.
func loadRequest(id, sid string, params json.RawMessage) []byte {
	p := map[string]json.RawMessage{}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	sidJSON, _ := json.Marshal(sid)
	p["sessionId"] = sidJSON
	paramsJSON, _ := json.Marshal(p)
	idJSON, _ := json.Marshal(id)
	msg := map[string]json.RawMessage{
		"jsonrpc": json.RawMessage(`"2.0"`),
		"id":      idJSON,
		"method":  json.RawMessage(`"session/load"`),
		"params":  paramsJSON,
	}
	out, err := json.Marshal(msg)
	if err != nil {
		return nil
	}
	return append(out, '\n')
}

func errorResponse(id string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"error":{"code":-32000,"message":"coop: agent restarted, please retry"}}` + "\n")
}

func trimQuotes(raw json.RawMessage) []byte {
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return raw[1 : len(raw)-1]
	}
	return raw
}

func clone(b []byte) []byte { return append([]byte(nil), b...) }

// readLines calls fn for each newline-terminated message read from br.
func readLines(br *bufio.Reader, fn func([]byte) error) error {
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if e := fn(clone(line)); e != nil {
				return e
			}
		}
		if err != nil {
			return err
		}
	}
}
