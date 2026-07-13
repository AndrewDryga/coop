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
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

// Hooks lets the caller (coop) own the ACP session as it flows through the proxy — without the
// generic proxy knowing anything coop-specific. A nil *Hooks (or nil field) is pure pass-through, so
// existing callers and tests are unaffected. All funcs must be safe for concurrent use.
type Hooks struct {
	// ToEditor rewrites a line going agent→editor (e.g. strip/add/retarget configOptions on a
	// session/new|load|resume result or a ConfigOptionUpdate). Returns the line to forward (nil to
	// drop it) and restart=true to tear down + respawn the box afterward — a coop-driven switch (e.g.
	// auto-rotating to another credential when this one reports a rate limit), replayed like any other
	// restart so the editor never disconnects. Called for every agent→editor line.
	ToEditor func(line []byte) (out []byte, restart bool)
	// SessionReady is called with a sessionId once a session is (re)established — a fresh session/new
	// OR a replayed session/load after a box restart — so coop can force per-session state that the
	// adapter resets each launch (yolo mode, coop's model). It returns lines to inject to the ADAPTER;
	// their responses are swallowed (the editor never sent them). Injected requests must use IDs from
	// InjectPrefix so the proxy can recognize and swallow their responses.
	SessionReady func(sessionID string) [][]byte
	// FromEditor inspects an editor→agent line before it's forwarded. handled=true → the proxy does
	// NOT forward the original line to the adapter (coop handled it); resp (if non-nil) is written back
	// to the editor; toAdapter (if non-nil) is written to the current adapter INSTEAD of the original
	// line — coop's translation of an editor request into the adapter's own protocol (e.g. a synthesized
	// model dropdown's set_config_option → the adapter's session/set_model); its response is swallowed
	// like any injected request, so toAdapter must carry an InjectPrefix id. restart=true → the proxy
	// tears down and respawns the box (a coop-driven switch, e.g. a new credential), NOT counted as a
	// failure, then replays the session so the editor never disconnects.
	// handled=false with a non-nil toAdapter is a REWRITE: the proxy forwards toAdapter through the
	// normal path — same bookkeeping, pending tracking, and response routing as the original line —
	// so it must keep the editor's request id (e.g. a history preamble prepended to a prompt).
	FromEditor func(line []byte) (handled bool, resp []byte, toAdapter []byte, restart bool)
	// AutoReply lets coop answer an agent→editor REQUEST itself instead of bothering the editor — the
	// yolo mechanism: coop approves every session/request_permission (the box is the sandbox) so no
	// provider ever shows a permission prompt, uniformly, whatever each adapter's own settings are.
	// Called for every agent→editor request. Return reply != nil to write it back to the ADAPTER;
	// forward=false to NOT pass the original request on to the editor. (nil, true) is pass-through.
	AutoReply func(line []byte) (reply []byte, forward bool)
	// ResumePrompt is called once per live session right after a restart's replay re-establishes it, so
	// coop can transparently re-send a prompt that failed on the old box (the turn that tripped a
	// rate-limit rotation / wait). A non-nil return is fed through the normal editor→box path — remapped
	// to the box id, registered as pending, its response forwarded to the editor — so the turn the
	// editor still shows as running completes on the new box. Return nil for a session with nothing to
	// resume. The returned line MUST carry the editor's session id (like a real editor prompt).
	ResumePrompt func(sessionID string) []byte
	// SessionRecreated is called (before ResumePrompt) when a session that HAD a conversation could
	// not be reloaded after a restart and was re-created fresh instead — a provider switch (the new
	// agent can't read the old one's transcript), or a genuinely lost store. coop uses it to carry
	// the conversation best-effort: the next prompt into that session gets a history preamble.
	SessionRecreated func(sessionID string)
}

// InjectPrefix namespaces coop's SessionReady-injected request IDs so the proxy swallows their
// responses instead of forwarding them to the editor.
const InjectPrefix = "coop-inject-"

const replayPrefix = "coop-acp-" // id namespace for our synthetic replay requests

// warnOut is where the proxy writes operator warnings (a lost session after a restart). A var, not
// os.Stderr inline, so tests can capture it.
var warnOut io.Writer = os.Stderr

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
// RunOpts carries resume/reload state for a supervisor re-exec (a SIGHUP reload). The zero value is
// a normal start (both nil), so Run is unchanged for callers that don't reload.
type RunOpts struct {
	Resume *Snapshot       // seed + replay this session state on the FIRST child (a resumed start)
	Reload <-chan struct{} // a receive triggers a graceful reload: snapshot + stop child + return reloadError
}

// reloadError is returned by RunWith when a reload fires; it carries the snapshot to hand the
// re-exec'd process. Extract it with ReloadSnapshot.
type reloadError struct{ Snap Snapshot }

func (e *reloadError) Error() string { return "acpproxy: reload requested" }

// ReloadSnapshot returns the snapshot carried by a reload error (and true), else (nil, false).
func ReloadSnapshot(err error) (*Snapshot, bool) {
	var re *reloadError
	if errors.As(err, &re) {
		return &re.Snap, true
	}
	return nil, false
}

// Run proxies the editor to a factory-spawned child, restarting it transparently — a thin wrapper
// over RunWith with no resume/reload (the shape existing callers use).
func Run(ctx context.Context, clientIn io.Reader, clientOut io.Writer, factory Factory, hooks *Hooks) error {
	return RunWith(ctx, clientIn, clientOut, factory, hooks, RunOpts{})
}

// RunWith is Run plus resume/reload: opts.Resume seeds + replays session state onto the first child
// (a resumed start), and a receive on opts.Reload snapshots, stops the child, and returns a
// reloadError carrying the snapshot. Both are guarded by non-nil opts, so the zero value is
// byte-identical to the pre-reload Run.
func RunWith(ctx context.Context, clientIn io.Reader, clientOut io.Writer, factory Factory, hooks *Hooks, opts RunOpts) error {
	p := &proxy{
		out:       clientOut,
		hooks:     hooks,
		sessions:  map[string]*sess{},
		byAdapter: map[string]string{},
		newReqs:   map[string]json.RawMessage{},
		pending:   map[string]bool{},
	}
	// Resume: seed the restored session state before the first child, then replay onto it below.
	if opts.Resume != nil {
		p.restore(*opts.Resume)
	}

	child, err := factory(ctx)
	if err != nil {
		return err
	}
	reader := bufio.NewReaderSize(child.Out, readBuf)
	p.setChild(child)
	// A resumed start replays the restored setup + sessions onto its FIRST child (a normal start
	// skips replay — the first child has nothing to restore). A replay failure degrades to a fresh
	// start rather than exiting: new threads must still work.
	if opts.Resume != nil {
		if rerr := p.replay(child, reader); rerr != nil {
			Trace("resume replay failed (%v) — starting fresh", rerr)
			// The first box may be HUNG (errReplayTimeout leaves it live by contract — the caller
			// stops it), and replay's reader goroutine may still be blocked on it. Pumping this child
			// would freeze the editor and race the reader, so stop it, drop the restored state, and
			// spawn a clean first child so new threads still work.
			child.Stop()
			p.mu.Lock()
			p.setup = nil
			p.sessions = map[string]*sess{}
			p.mu.Unlock()
			child, err = factory(ctx)
			if err != nil {
				return err
			}
			reader = bufio.NewReaderSize(child.Out, readBuf)
			p.setChild(child)
		}
	}

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

	// Reload (SIGHUP re-exec): a receive on opts.Reload snapshots + stops the child so pumpChild
	// unblocks, and the loop returns a reloadError carrying the snapshot. A nil channel never fires,
	// so a normal run is unaffected.
	var reloading atomic.Bool
	if opts.Reload != nil {
		go func() {
			select {
			case <-opts.Reload:
				reloading.Store(true)
				p.shutdownChild()
			case <-ctx.Done():
			}
		}()
	}

	rapid := 0
	for {
		start := time.Now()
		p.pumpChild(reader) // returns when this child's Out closes
		// A reload was requested: hand the snapshot back to the caller (which re-execs the binary),
		// NOT counting it as a failure and NOT failing pending — the editor's transport survives.
		if reloading.Load() {
			snap := p.snapshot()
			child.Stop()
			return &reloadError{Snap: snap}
		}
		select {
		case <-clientGone:
			child.Stop()
			return nil
		case <-ctx.Done():
			child.Stop()
			return ctx.Err()
		default:
		}
		// A coop-driven restart (a credential/preset switch) is intentional, not a failure — never
		// count it toward the rapid-fail cap or back off. Otherwise: a child that dies almost
		// immediately is broken (no runtime, bad auth, a failing image) — respawning in a tight loop
		// would be a fork bomb, so back off and eventually give up, letting the editor see the failure.
		if p.intentional.Swap(false) {
			rapid = 0
		} else if time.Since(start) < minHealthy {
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
			// A SIGHUP racing a failed respawn: re-exec (carry the sessions forward) rather than exit
			// on the editor — the reload is the intent, the respawn failure is incidental.
			if reloading.Load() {
				return &reloadError{Snap: p.snapshot()}
			}
			p.failAllPending()
			return err
		}
		nr := bufio.NewReaderSize(next.Out, readBuf)
		// replay publishes next as the live child (setChild) before failing pending. A replay
		// timeout (the restarted agent hung) is a bounded failure: stop it and give up so the
		// editor isn't frozen waiting — better a clean "server exited" than an infinite hang.
		if err := p.replay(next, nr); err != nil {
			next.Stop()
			if reloading.Load() { // same race, one step later — prefer the reload over a give-up
				return &reloadError{Snap: p.snapshot()}
			}
			p.failAllPending()
			return err
		}
		child.Stop() // release the dead child's pipes + cidfile dir before swapping in the new one
		child, reader = next, nr
	}
}

// sess is one live editor session. The editor knows it by a stable id (the map key in
// proxy.sessions); the box knows it by adapterID, which normally equals the editor id but diverges
// after a restart re-creates a turn-less session under a fresh id (see replay + remapSession).
type sess struct {
	params    json.RawMessage // the original session/new (or session/load) params — cwd, mcpServers
	adapterID string          // the id the current box knows this session by (== editor id until re-created)
	turned    bool            // a session/prompt has run (or it was resumed via session/load), so a
	// transcript exists and it can be session/load-ed back after a restart; false = re-create instead
}

type proxy struct {
	out io.Writer

	mu           sync.Mutex
	child        *Child
	shuttingDown bool                       // editor gone / ctx cancelled: a concurrent swap must stop the child it publishes
	setup        [][]byte                   // editor's pre-session setup (initialize, authenticate), in order
	sessions     map[string]*sess           // editor sessionId -> session state
	byAdapter    map[string]string          // adapter sessionId -> editor sessionId, only for re-created (diverged) sessions
	newReqs      map[string]json.RawMessage // pending session/new request id -> its params
	pending      map[string]bool            // editor request id -> awaiting a response

	hooks       *Hooks      // coop's control layer (nil → pure pass-through)
	intentional atomic.Bool // set before a coop-driven restart so the loop doesn't count it as a failure
}

// Snapshot is the proxy's re-establishable session state, carried across a supervisor re-exec (a
// SIGHUP reload) so the editor's live threads survive the binary swap. adapterID is intentionally
// dropped: on resume the box is fresh, so every session starts with adapterID == editor id and the
// replay re-derives any divergence exactly as it does after a box restart.
type Snapshot struct {
	Setup    [][]byte      `json:"setup"`    // the editor's initialize/authenticate lines, in order
	Sessions []SessionSnap `json:"sessions"` // one per live editor session
}

// SessionSnap is one session flattened for serialization.
type SessionSnap struct {
	EditorID string          `json:"editor_id"`
	Params   json.RawMessage `json:"params"`
	Turned   bool            `json:"turned"`
}

// snapshot copies the proxy's setup + sessions into a serializable Snapshot, under the lock.
func (p *proxy) snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap := Snapshot{Setup: make([][]byte, len(p.setup))}
	for i, l := range p.setup {
		snap.Setup[i] = append([]byte(nil), l...)
	}
	for id, s := range p.sessions {
		snap.Sessions = append(snap.Sessions, SessionSnap{EditorID: id, Params: s.params, Turned: s.turned})
	}
	return snap
}

// restore seeds a fresh proxy's setup + sessions from a Snapshot: adapterID = editorID (the fresh
// box knows nothing yet), so replay's turned-branch decides session/load vs session/new per session.
func (p *proxy) restore(snap Snapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setup = make([][]byte, len(snap.Setup))
	for i, l := range snap.Setup {
		p.setup[i] = append([]byte(nil), l...)
	}
	for _, s := range snap.Sessions {
		p.sessions[s.EditorID] = &sess{params: s.Params, adapterID: s.EditorID, turned: s.Turned}
	}
}

func (p *proxy) setChild(c *Child) {
	p.mu.Lock()
	p.child = c
	p.mu.Unlock()
}

func (p *proxy) shutdownChild() {
	p.mu.Lock()
	p.shuttingDown = true
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
	traceLine("editor→box", line)
	// coop's control layer gets first look: it may handle an editor request itself (a coop-owned
	// config option like the credential/preset selector) — not forwarding it to the adapter, replying
	// to the editor directly, and optionally restarting the box on a new credential/preset.
	if p.hooks != nil && p.hooks.FromEditor != nil {
		handled, resp, toAdapter, restart := p.hooks.FromEditor(line)
		if handled {
			Trace("coop handled the editor line itself (restart=%v)", restart)
			if len(resp) > 0 {
				traceLine("box→editor(coop)", resp)
				_, _ = p.out.Write(resp)
			}
			// A translated adapter request (e.g. a synthesized model set → session/set_model). Write it
			// to the CURRENT child; a write to a momentarily-dead child during a swap is dropped, exactly
			// like fromClient's forward.
			if len(toAdapter) > 0 {
				p.mu.Lock()
				c := p.child
				p.mu.Unlock()
				if c != nil {
					traceLine("editor→box(coop)", toAdapter)
					_, _ = c.In.Write(toAdapter)
				}
			}
			if restart {
				p.triggerRestart()
			}
			return
		}
		if len(toAdapter) > 0 {
			// A rewrite (same request, new body — e.g. a history preamble prepended to a prompt):
			// forward it through the normal path below, so bookkeeping, pending tracking, and the
			// response route exactly as the original would have.
			traceLine("editor→box(coop-rewrite)", toAdapter)
			line = toAdapter
		}
	}
	h := parse(line)
	sid := sessionID(h.Params) // the editor's session id, "" for non-session methods
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
			// A resumed session already has a transcript, so it's reloadable from the start.
			if sid != "" {
				p.sessions[sid] = &sess{params: h.Params, adapterID: sid, turned: true}
			}
		case "session/prompt":
			// The first prompt starts a turn → a transcript now exists, so a restart can
			// session/load it back rather than re-create it.
			if s := p.sessions[sid]; s != nil {
				s.turned = true
			}
		}
		p.pending[string(h.ID)] = true
	}
	// Translate the editor's session id to the box's, if this session was re-created under a new one
	// (turn-less at a restart). Normal case: adapterID == sid, so no rewrite.
	fwd := line
	if s := p.sessions[sid]; sid != "" && s != nil && s.adapterID != sid {
		fwd = withSessionID(line, s.adapterID)
	}
	var w io.Writer
	if p.child != nil {
		w = p.child.In
	}
	p.mu.Unlock()
	if w != nil {
		_, _ = w.Write(fwd)
	}
}

// pumpChild forwards child→editor for one child generation, learning session ids from
// session/new responses and clearing pending requests. Returns on the child's EOF.
func (p *proxy) pumpChild(br *bufio.Reader) {
	_ = readLines(br, func(line []byte) error {
		traceLine("box→editor", line)
		// Translate the box's session id back to the one the editor knows, so coop's hooks and the
		// editor always see the editor's id (a no-op unless a session was re-created under a new one).
		line = p.remapToEditor(line)
		h := parse(line)
		// coop answers some agent→editor requests itself (session/request_permission → always allow, so
		// the editor never prompts). The reply goes to THIS child; the request is not forwarded.
		if h.isRequest() && p.hooks != nil && p.hooks.AutoReply != nil {
			if reply, forward := p.hooks.AutoReply(line); reply != nil || !forward {
				if len(reply) > 0 {
					p.mu.Lock()
					c := p.child
					p.mu.Unlock()
					if c != nil {
						_, _ = c.In.Write(reply)
					}
				}
				if !forward {
					return nil
				}
			}
		}
		if h.isResponse() {
			// Swallow responses to coop's injected force-sets — the editor never sent them.
			if strings.HasPrefix(string(trimQuotes(h.ID)), InjectPrefix) {
				return nil
			}
			id := string(h.ID)
			p.mu.Lock()
			newParams, wasNew := p.newReqs[id]
			if wasNew {
				delete(p.newReqs, id)
				if sid := sessionID(h.Result); sid != "" {
					p.sessions[sid] = &sess{params: newParams, adapterID: sid}
				}
			}
			delete(p.pending, id)
			p.mu.Unlock()
			// A fresh session/new just completed: force coop's per-session state (yolo, model) on the
			// adapter, which resets it every launch.
			if wasNew {
				if sid := sessionID(h.Result); sid != "" {
					p.forceSession(sid)
				}
			}
		}
		out, restart := line, false
		if p.hooks != nil && p.hooks.ToEditor != nil {
			out, restart = p.hooks.ToEditor(line)
		}
		if len(out) > 0 {
			_, _ = p.out.Write(out)
		}
		// A coop-driven restart requested from the wire (e.g. a rate-limit auto-rotation): forward the
		// line first (so the editor's request is answered), then tear the box down — Run respawns it on
		// the newly-selected credential and replays the session, exactly like a manual switch.
		if restart {
			p.triggerRestart()
		}
		return nil
	})
}

// forceSession injects coop's per-session force-sets (yolo mode, coop's model) to the CURRENT
// adapter after a session is established. Their responses are swallowed in pumpChild (InjectPrefix).
func (p *proxy) forceSession(sid string) {
	if p.hooks == nil || p.hooks.SessionReady == nil {
		return
	}
	p.mu.Lock()
	c := p.child
	p.mu.Unlock()
	if c == nil {
		return
	}
	for _, m := range p.hooks.SessionReady(sid) {
		_, _ = c.In.Write(m)
	}
}

// triggerRestart tears the current child down so pumpChild returns and Run's loop respawns it — a
// coop-driven switch (e.g. a new credential/preset), flagged intentional so it's not a rapid-fail.
func (p *proxy) triggerRestart() {
	Trace("restart requested (coop switch / rotate)")
	p.intentional.Store(true)
	p.mu.Lock()
	c := p.child
	p.mu.Unlock()
	if c != nil {
		c.Stop()
	}
}

// remapSession records that the session the editor knows as editorID now lives under a new box id
// (it was re-created during replay because it had no reloadable transcript). Both directions then
// translate between the two ids; the old mapping, if any, is dropped.
func (p *proxy) remapSession(editorID, adapterID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[editorID]
	if s == nil {
		return
	}
	if s.adapterID != editorID {
		delete(p.byAdapter, s.adapterID) // clear the previous divergent mapping
	}
	s.adapterID = adapterID
	if adapterID != editorID {
		p.byAdapter[adapterID] = editorID
	}
	// The fresh box session has no transcript until a prompt runs, so a restart before then must
	// re-create again, not session/load an id that was never persisted (turned means "a transcript
	// exists on the CURRENT box"). Without this, every switch after a re-create burned a doomed
	// load round and warned "did NOT reload" — even same-provider.
	s.turned = false
}

// remapToEditor rewrites a box→editor line's sessionId from the box's id back to the editor's, when
// this session was re-created under a new box id. A no-op (no parse) in the normal case, so it's cheap
// on the streaming hot path.
func (p *proxy) remapToEditor(line []byte) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.byAdapter) == 0 {
		return line
	}
	aid := sessionID(parse(line).Params)
	if aid == "" {
		return line
	}
	if eid, ok := p.byAdapter[aid]; ok {
		return withSessionID(line, eid)
	}
	return line
}

// replay brings a freshly-started child up to the editor's current view: re-send
// initialize and a session/load for each live session, consuming their responses
// (and any chatter emitted while loading) so the editor doesn't see them; then fail
// any requests that were in flight when the old child died.
func (p *proxy) replay(c *Child, br *bufio.Reader) error {
	type snap struct {
		editorID, adapterID string
		params              json.RawMessage
		turned              bool
	}
	p.mu.Lock()
	setup := make([][]byte, len(p.setup))
	copy(setup, p.setup)
	snaps := make([]snap, 0, len(p.sessions))
	for eid, s := range p.sessions {
		snaps = append(snaps, snap{eid, s.adapterID, s.params, s.turned})
	}
	p.mu.Unlock()

	Trace("replay: restoring %d session(s) on the restarted box", len(snaps))
	var msgs [][]byte
	expect := map[string]bool{}
	for i, line := range setup {
		id := fmt.Sprintf("%ssetup-%d", replayPrefix, i)
		if msg := withID(line, id); msg != nil {
			msgs = append(msgs, msg)
			expect[id] = true
		}
	}
	for _, s := range snaps {
		// A session with a transcript is reloaded by its box id; a turn-less one (a fresh session/new
		// never prompted) has nothing to load yet, so re-create it and rebind the editor's id — losing
		// nothing, since there was no conversation.
		if s.turned {
			id := replayPrefix + "load-" + s.editorID
			if msg := loadRequest(id, s.adapterID, s.params); msg != nil {
				msgs = append(msgs, msg)
				expect[id] = true
			}
		} else {
			id := replayPrefix + "new-" + s.editorID
			if msg := newRequest(id, s.params); msg != nil {
				msgs = append(msgs, msg)
				expect[id] = true
			}
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
	// The editor never sees the replayed load/new results (they answer OUR synthetic requests), so it
	// would keep showing the config it knew before the restart — stale after a credential/preset
	// switch (e.g. the model dropdown). Collect each re-established session's configOptions from its
	// result and, once the child is live, forward them as config_option_update notifications: the
	// box's truth, pushed through the normal ToEditor path so coop's toolbar rewrite applies.
	type configUpdate struct {
		editorID string
		options  json.RawMessage
	}
	var configUpdates []configUpdate
	// A session whose re-create FAILED is dead on the new box — collect it and tell the thread
	// after the swap (sessionNoticeLine), so the failure is visible where the user is looking.
	type replayFailure struct {
		editorID string
		msg      string
	}
	var failures []replayFailure

	// Bounded read: allow a generous wait for the FIRST response (the box may still be starting),
	// then a tighter idle bound once the agent has answered and is provably up. A child EOF here
	// means it died during replay — break and let Run respawn it; a timeout means it hung — abort.
	snapByEditor := make(map[string]snap, len(snaps))
	for _, s := range snaps {
		snapByEditor[s.editorID] = s
	}
	var recreate []string // turned sessions whose transcript didn't reload — re-created in a second round
	timeout := replayStartupGrace
	childEOF := false
	process := func(expect map[string]bool) error {
		for len(expect) > 0 {
			line, err := readLineCtx(br, timeout)
			if len(line) > 0 {
				if h := parse(line); h.isResponse() {
					id := string(trimQuotes(h.ID))
					if eid, ok := strings.CutPrefix(id, replayPrefix+"new-"); ok {
						// A re-created session — turn-less in round one, or a turned one whose reload
						// failed in round two: bind the editor's id to the fresh box id so both directions
						// translate from here on. If even session/new failed, the box is broken.
						if newID := sessionID(h.Result); newID != "" {
							p.remapSession(eid, newID)
							configUpdates = append(configUpdates, configUpdate{eid, resultConfigOptions(h.Result)})
							// A TURNED session landed here only because its conversation didn't carry —
							// tell coop, so it can inject its best-effort history preamble.
							if snapByEditor[eid].turned && p.hooks != nil && p.hooks.SessionRecreated != nil {
								p.hooks.SessionRecreated(eid)
							}
						} else if len(h.Error) > 0 {
							fmt.Fprintf(warnOut, "coop acp: session %s could not be re-created after the box restarted: %s\n", eid, h.Error)
							Trace("replay: session %s re-create failed: %s", eid, h.Error)
							failures = append(failures, replayFailure{eid, errorMessage(h.Error)})
						}
					} else if eid, ok := strings.CutPrefix(id, replayPrefix+"load-"); ok {
						// A session that DID have a transcript failed to reload — a provider switch (the
						// new agent can't read the old one's store), or a genuinely lost transcript.
						// Re-create it fresh in a second round; coop carries the conversation best-effort.
						if len(h.Error) > 0 {
							fmt.Fprintf(warnOut, "coop acp: session %s did not reload after the box restarted; re-creating it fresh (context carried best-effort): %s\n", eid, h.Error)
							Trace("replay: session %s did NOT reload — re-creating: %s", eid, h.Error)
							recreate = append(recreate, eid)
						} else {
							configUpdates = append(configUpdates, configUpdate{eid, resultConfigOptions(h.Result)})
						}
					}
					delete(expect, id)
				}
			}
			if errors.Is(err, errReplayTimeout) {
				return err
			}
			if err != nil {
				childEOF = true
				return nil // child EOF: it died mid-replay; fall through, Run's loop respawns it
			}
			timeout = replayIdleTimeout
		}
		return nil
	}
	if err := process(expect); err != nil {
		return err
	}
	<-sent // replay writer done: no concurrent write to c.In
	// Second round: re-create the turned sessions whose reload failed, reusing their original
	// session/new params (cwd, mcpServers). Serialized after the first writer, so no concurrent
	// c.In writes; the shared processor remaps them and fires SessionRecreated.
	if len(recreate) > 0 && !childEOF {
		expect2 := map[string]bool{}
		for _, eid := range recreate {
			id := replayPrefix + "new-" + eid
			if msg := newRequest(id, snapByEditor[eid].params); msg != nil {
				if _, err := c.In.Write(msg); err != nil {
					break
				}
				expect2[id] = true
			}
		}
		if err := process(expect2); err != nil {
			return err
		}
	}
	// Re-force coop's per-session state (yolo, model) on the restarted box for each restored session,
	// keyed by its CURRENT box id (a re-created session now lives under a new one). The fresh adapter
	// reset it; responses are swallowed in pumpChild (InjectPrefix).
	if p.hooks != nil && p.hooks.SessionReady != nil {
		p.mu.Lock()
		aids := make([]string, 0, len(p.sessions))
		for _, s := range p.sessions {
			aids = append(aids, s.adapterID)
		}
		p.mu.Unlock()
		for _, aid := range aids {
			for _, m := range p.hooks.SessionReady(aid) {
				_, _ = c.In.Write(m)
			}
		}
	}
	// A session may have a prompt to re-send transparently — a turn that failed on the old box (a
	// rate-limit rotation/wait) or was in flight when a manual switch killed it. Collect the resends
	// BEFORE the swap so swapChild can spare their still-pending request ids from the fail: a
	// mid-turn switch's prompt is still awaiting its response, and the resend completes the editor's
	// original request on the new box. (A rate-limit resend's id was already consumed by the error
	// response, so sparing it is a no-op there.)
	var resumes [][]byte
	keep := map[string]bool{}
	if p.hooks != nil && p.hooks.ResumePrompt != nil {
		p.mu.Lock()
		eids := make([]string, 0, len(p.sessions))
		for eid := range p.sessions {
			eids = append(eids, eid)
		}
		p.mu.Unlock()
		for _, eid := range eids {
			if line := p.hooks.ResumePrompt(eid); len(line) > 0 {
				resumes = append(resumes, line)
				if id := parse(line).ID; len(id) > 0 {
					keep[string(id)] = true
				}
			}
		}
	}
	p.swapChild(c, keep)
	// Tell the editor what the restarted box's sessions ACTUALLY look like (the model in force after a
	// credential/preset switch, say) — it never saw the load/new results. Synthesized on the editor
	// side of the boundary with the editor's ids, and run through ToEditor so coop's toolbar rewrite
	// (drop mode, prepend coop_setup, refresh its cache) applies. The restart flag is ignored: a
	// config notification can't be a rate-limit error.
	for _, cu := range configUpdates {
		if len(cu.options) == 0 {
			continue
		}
		line := configOptionUpdateLine(cu.editorID, cu.options)
		if p.hooks != nil && p.hooks.ToEditor != nil {
			line, _ = p.hooks.ToEditor(line)
		}
		if len(line) > 0 {
			traceLine("box→editor(replay-config)", line)
			_, _ = p.out.Write(line)
		}
	}
	// A dead session gets a visible verdict in its thread — until now the failure lived only in
	// stderr/trace, so the user saw a stripped toolbar and silently failing prompts (e.g. a codex
	// box that can't start because the account's sqlite state is held by another box).
	for _, f := range failures {
		line := sessionNoticeLine(f.editorID,
			"⚠ coop: this session could not be re-established after the box restarted: "+f.msg+
				"\nSwitch the provider or account back (or close the conflicting session), then send a message to retry.")
		traceLine("box→editor(replay-failed)", line)
		_, _ = p.out.Write(line)
	}
	// Feed the resends through the normal client path AFTER the swap, so each is remapped, tracked
	// as pending, and its response reaches the editor, completing the turn the editor still shows
	// as running.
	for _, line := range resumes {
		p.fromClient(line)
	}
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
//
// keep spares request ids replay is about to re-send transparently (a turn in flight at a manual
// switch): they get the resend's real response instead of an error, so the editor's turn completes
// on the new box. The resend re-registers them as pending via fromClient right after the swap —
// before the new child's pump starts, so no response can race the gap.
func (p *proxy) swapChild(c *Child, keep map[string]bool) {
	p.mu.Lock()
	ids := make([]string, 0, len(p.pending))
	for id := range p.pending {
		if keep[id] {
			continue
		}
		ids = append(ids, id)
		delete(p.newReqs, id) // a session/new in flight at the swap never gets a response
	}
	p.pending = map[string]bool{}
	p.child = c
	down := p.shuttingDown
	p.mu.Unlock()
	// The editor disconnected while replay was in flight: shutdownChild already stopped the OLD child,
	// so stop the one we just published too — otherwise its Out never closes and Run blocks forever in
	// pumpChild. Serialized with shutdownChild under p.mu, so exactly one of them stops this child.
	if down {
		c.Stop()
	}
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

// resultConfigOptions extracts a session/load|new result's configOptions, or nil (an adapter that
// doesn't send them there has nothing to forward).
func resultConfigOptions(result json.RawMessage) json.RawMessage {
	if len(result) == 0 {
		return nil
	}
	var v struct {
		ConfigOptions json.RawMessage `json:"configOptions"`
	}
	_ = json.Unmarshal(result, &v)
	return v.ConfigOptions
}

// configOptionUpdateLine builds the ACP config_option_update notification — the full configOptions
// state for one session — used after a replay to bring the editor's toolbar up to the box's truth.
func configOptionUpdateLine(sid string, options json.RawMessage) []byte {
	upd := map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": sid,
			"update": map[string]any{
				"sessionUpdate": "config_option_update",
				"configOptions": options,
			},
		},
	}
	b, _ := json.Marshal(upd)
	return append(b, '\n')
}

// sessionNoticeLine is a synthetic agent_message_chunk to the editor — how a replay failure
// becomes VISIBLE in the thread, instead of dying in stderr while the toolbar silently keeps
// stale options and every next prompt errors against a session that no longer exists.
func sessionNoticeLine(sid, text string) []byte {
	upd := map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": sid,
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": text},
			},
		},
	}
	b, _ := json.Marshal(upd)
	return append(b, '\n')
}

// errorMessage extracts the human message from a JSON-RPC error object, falling back to the
// raw JSON so a malformed error still surfaces something actionable.
func errorMessage(raw json.RawMessage) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return e.Message
	}
	return string(raw)
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

// newRequest builds a session/new request reusing the original params (cwd, mcpServers) under a
// fresh synthetic id — used to re-create a session that has no reloadable transcript yet.
func newRequest(id string, params json.RawMessage) []byte {
	p := map[string]json.RawMessage{}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	delete(p, "sessionId") // session/new mints a new id; it never carries one
	paramsJSON, _ := json.Marshal(p)
	idJSON, _ := json.Marshal(id)
	msg := map[string]json.RawMessage{
		"jsonrpc": json.RawMessage(`"2.0"`),
		"id":      idJSON,
		"method":  json.RawMessage(`"session/new"`),
		"params":  paramsJSON,
	}
	out, err := json.Marshal(msg)
	if err != nil {
		return nil
	}
	return append(out, '\n')
}

// withSessionID returns line with params.sessionId replaced by sid — both directions of the
// editor↔box id remap use it. Returns the line unchanged if it carries no params object.
func withSessionID(line []byte, sid string) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(line, &m) != nil {
		return line
	}
	raw, ok := m["params"]
	if !ok {
		return line
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(raw, &params) != nil {
		return line
	}
	sidJSON, _ := json.Marshal(sid)
	params["sessionId"] = sidJSON
	pj, err := json.Marshal(params)
	if err != nil {
		return line
	}
	m["params"] = pj
	out, err := json.Marshal(m)
	if err != nil {
		return line
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
