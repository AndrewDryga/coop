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
// child, replays the setup handshake, then loads each native session owned by that
// provider or creates a replacement for a provider switch. It fails any in-flight
// request so the editor isn't left hanging and resumes forwarding. The editor never
// sees EOF.
package acpproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Child is one run of the agent adapter (one container). The proxy writes ACP to In
// and reads ACP from Out; Stop force-terminates it.
type Child struct {
	In       io.WriteCloser
	Out      io.Reader
	Stop     func()
	Provider string // native session ids are scoped to this provider
	Account  string // successful authentication is scoped to this concrete credential too
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
	// ReplayFailure gives the control layer a synthetic replay error before the proxy swallows it.
	// handled=true means the error is target health (for example auth_required): restart requests a
	// different child, while out carries an actionable error whose message can be shown in-session.
	ReplayFailure func(line []byte) (out []byte, restart, handled bool)
	// SessionReady is called with a sessionId once a session is (re)established — a fresh session/new
	// OR a replayed session/load after a box restart — so coop can force per-session state that the
	// adapter resets each launch (yolo mode, coop's model). It returns lines to inject to the ADAPTER;
	// their responses are swallowed (the editor never sent them). Injected requests must use IDs from
	// InjectPrefix so the proxy can recognize and swallow their responses.
	SessionReady func(sessionID string) [][]byte
	// InjectedResponse observes a response together with the synthetic request that caused it before
	// the proxy swallows the response. It may return an editor-facing notification, used to surface a
	// rejected target setting instead of silently running the wrong model or effort.
	InjectedResponse func(request, response []byte) []byte
	// ChildReset tells the control layer that the current child generation ended, so request-scoped
	// state that cannot survive a restart can be discarded.
	ChildReset func()
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
	// PromptForwarded observes a session/prompt only after it has cleared target-setting gating and
	// reload admission and entered pending bookkeeping. synthetic distinguishes a replay resume from
	// a real editor prompt. It is the safe point for request correlation and assistant-turn ownership.
	PromptForwarded func(line []byte, synthetic bool)
	// AutoReply lets coop answer an agent→editor REQUEST itself instead of bothering the editor — the
	// yolo mechanism: coop approves every session/request_permission (the box is the sandbox) so no
	// provider ever shows a permission prompt, uniformly, whatever each adapter's own settings are.
	// Called for every agent→editor request. Return reply != nil to write it back to the ADAPTER;
	// forward=false to NOT pass the original request on to the editor. (nil, true) is pass-through.
	AutoReply func(line []byte) (reply []byte, forward bool)
	// ResumePrompt is called once per live session right after a restart's replay re-establishes it, so
	// coop can transparently re-send a prompt that failed on the old box (the turn that tripped a
	// rate-limit rotation / wait). A non-nil return is fed through the common box path — remapped to
	// the box id, registered as pending, and its response forwarded to the editor — but is NOT passed
	// through FromEditor again: it is synthetic traffic, not a second user prompt. Return nil for a
	// session with nothing to resume. The returned line MUST carry the editor's session id.
	ResumePrompt func(sessionID string) []byte
	// SessionRecreated is called (before ResumePrompt) when a session that HAD a conversation could
	// not be reloaded after a restart and was re-created fresh instead — a provider switch (the new
	// agent can't read the old one's transcript), or a genuinely lost store. coop uses it to carry
	// the conversation best-effort: the next prompt into that session gets a history preamble.
	SessionRecreated func(sessionID string)
	// SessionClosed is called after session/close deactivates a native session. Durable identity and
	// carry history remain available for a later session/resume, while request-scoped state is dropped.
	SessionClosed func(sessionID string)
	// SessionEnded is called after session/delete removes the proxy's durable identity, so the owner
	// can release its per-session caches and carry history too.
	SessionEnded func(sessionID string)
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
	forceReplyTimeout  = 30 * time.Second
)

// errReplayTimeout means a restarted child stopped responding during replay (a hang, not a
// crash). The caller tears the child down and gives up, rather than leaving the editor frozen.
var (
	errReplayTimeout    = errors.New("acpproxy: timed out waiting for the restarted agent")
	errReplaySuperseded = errors.New("acpproxy: replacement superseded by a newer restart")
)

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
		out:            clientOut,
		hooks:          hooks,
		authentication: map[authenticationScope]authenticationState{},
		setupReqs:      map[string]setupRequest{},
		authPending:    map[authenticationScope]string{},
		sessions:       map[string]*sess{},
		byAdapter:      map[string]string{},
		sessionReqs:    map[string]sessionRequest{},
		pending:        map[string]bool{},
		retired:        map[string]bool{},
		unavailable:    map[string]bool{},
		reactivating:   map[string]string{},
		injected:       map[string][]byte{},
		forceByID:      map[string]*forceChain{},
		forceBySess:    map[string]*forceChain{},
		forceFailed:    map[string]string{},
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
	// start rather than exiting: new threads must still work. A controller-driven restart during
	// replay is different: retain the restored state and negotiate the newly-selected target.
	if opts.Resume != nil {
		var rerr error
		for {
			epoch := p.currentRestartEpoch()
			rerr = p.replayAt(child, reader, epoch)
			if !errors.Is(rerr, errReplaySuperseded) {
				break
			}
			child.Stop()
			if p.reloading.Load() {
				return &reloadError{Snap: p.snapshot()}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			p.clearSupersededIntent(epoch)
			child, err = factory(ctx)
			if err != nil {
				return err
			}
			reader = bufio.NewReaderSize(child.Out, readBuf)
			p.setChild(child)
		}
		if rerr != nil {
			Trace("resume replay failed (%v) — starting fresh", rerr)
			// The first box may be HUNG (errReplayTimeout leaves it live by contract — the caller
			// stops it), and replay's reader goroutine may still be blocked on it. Pumping this child
			// would freeze the editor and race the reader, so stop it, drop the restored state, and
			// spawn a clean first child so new threads still work.
			child.Stop()
			p.mu.Lock()
			p.setup = nil
			p.authentication = map[authenticationScope]authenticationState{}
			p.setupReqs = map[string]setupRequest{}
			p.authPending = map[authenticationScope]string{}
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
	if opts.Reload != nil {
		go func() {
			select {
			case <-opts.Reload:
				p.beginReload()
			case <-ctx.Done():
			}
		}()
	}

	rapid := 0
	for {
		start := time.Now()
		p.pumpChild(child, reader) // returns when this child's Out closes
		p.retireChild(child)
		p.resetForceState()
		// A reload was requested: hand the snapshot back to the caller (which re-execs the binary),
		// NOT counting it as a failure and NOT failing pending — the editor's transport survives.
		if p.reloading.Load() {
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
		var next *Child
		var nr *bufio.Reader
		for {
			epoch := p.currentRestartEpoch()
			next, err = factory(ctx)
			if err != nil {
				// A SIGHUP racing a failed respawn: re-exec (carry the sessions forward) rather than exit
				// on the editor — the reload is the intent, the respawn failure is incidental.
				if p.reloading.Load() {
					return &reloadError{Snap: p.snapshot()}
				}
				p.failAllPending()
				return err
			}
			nr = bufio.NewReaderSize(next.Out, readBuf)
			// A selection can change while this candidate is still replaying. In that case the replay
			// is deliberately abandoned and factory is consulted again for the newest selection.
			rerr := p.replayAt(next, nr, epoch)
			if errors.Is(rerr, errReplaySuperseded) {
				next.Stop()
				if p.reloading.Load() {
					return &reloadError{Snap: p.snapshot()}
				}
				select {
				case <-clientGone:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				p.clearSupersededIntent(epoch)
				continue
			}
			if rerr != nil {
				next.Stop()
				if p.reloading.Load() { // same race, one step later — prefer the reload over a give-up
					return &reloadError{Snap: p.snapshot()}
				}
				p.failAllPending()
				return rerr
			}
			break
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
	provider  string          // the provider that owns adapterID; native ids never cross this boundary
	closed    bool            // close releases active resources but preserves identity for an explicit resume
	turned    bool            // a session/prompt has run (or it was resumed via session/load), so a
	// transcript exists and it can be session/load-ed back after a restart; false = re-create instead
}

// sessionRequest is a session mutation staged behind one editor request. None of these changes are
// durable until the active child returns success; otherwise a failed load/prompt/close can create a
// phantom replay candidate or delete a still-live one.
type sessionRequest struct {
	method     string
	editorID   string
	adapterID  string
	provider   string
	params     json.RawMessage
	generation uint64
}

// setupRequest stages provider-owned handshake state behind the child response. Initialize is
// reusable transport setup as soon as the editor sends it; authenticate is retained only after the
// provider accepts it, otherwise a failed login would poison every later restart.
type setupRequest struct {
	method     string
	provider   string
	account    string
	methodID   string
	line       []byte
	generation uint64
}

type authenticationScope struct {
	provider string
	account  string
}

type authenticationState struct {
	methodID string
	line     []byte
}

type replayBinding struct {
	adapterID string
	provider  string
	turned    bool
}

// forceChain serializes provider-owned session settings. JSON-RPC requests may execute
// concurrently, so writing model then effort is insufficient when changing model resets effort;
// the next request is written only after the prior response succeeds. Prompts wait behind the chain.
type forceChain struct {
	session  string
	requests [][]byte
	next     int
	activeID string
	held     []clientLine
	timer    *time.Timer
}

type clientOrigin uint8

const (
	originEditor clientOrigin = iota
	originSynthetic
)

// clientLine retains whether a prompt really came from the editor while it waits behind a target
// settings chain. Releasing a synthetic resume through FromEditor would record it as a second user
// message and recursively wrap its already-carried context.
type clientLine struct {
	line   []byte
	origin clientOrigin
}

type proxy struct {
	out io.Writer

	controlMu      sync.Mutex // serializes controller hooks with restart decisions and resume admission
	mu             sync.Mutex
	child          *Child
	candidate      *Child // replacement being replayed before it becomes authoritative
	generation     uint64
	restartEpoch   uint64                                      // increments for every selection/restart request, even during replay
	shuttingDown   bool                                        // editor gone / ctx cancelled: a concurrent swap must stop the child it publishes
	setup          [][]byte                                    // the editor's initialize request (slice shape preserves old snapshots)
	authentication map[authenticationScope]authenticationState // one chosen method per provider account
	setupReqs      map[string]setupRequest                     // request id -> handshake state committed only on success
	authPending    map[authenticationScope]string              // provider account -> serialized authenticate/logout request id
	initResult     json.RawMessage                             // active child's fresh initialize result (capabilities included)
	authMethods    map[string]bool                             // methods advertised by the active child's initialize result
	initGeneration uint64                                      // generation initResult/authMethods belong to
	sessions       map[string]*sess                            // editor sessionId -> session state
	byAdapter      map[string]string                           // adapter sessionId -> editor sessionId, only for re-created (diverged) sessions
	sessionReqs    map[string]sessionRequest                   // request id -> mutation committed only on success
	pending        map[string]bool                             // editor request id -> awaiting a response
	retired        map[string]bool                             // native ids retired in this child generation; drop late output
	unavailable    map[string]bool                             // editor sessions that did not establish on the active child
	reactivating   map[string]string                           // retired native id -> pending load/resume request that may stream history
	injected       map[string][]byte                           // synthetic request id -> request, for response diagnostics
	forceByID      map[string]*forceChain                      // active setting request id -> its ordered chain
	forceBySess    map[string]*forceChain                      // adapter session id -> active setting chain
	forceFailed    map[string]string                           // adapter session id -> setting failure; prompts fail loudly

	hooks       *Hooks      // coop's control layer (nil → pure pass-through)
	intentional atomic.Bool // set before a coop-driven restart so the loop doesn't count it as a failure
	reloading   atomic.Bool // reject requests after the reload boundary instead of losing them to exec
}

// Snapshot is the proxy's re-establishable session state, carried across a supervisor re-exec (a
// SIGHUP reload) so the editor's live threads survive the binary swap. Native identity is part of
// the snapshot: a fresh child of the same provider can load it, while another provider must create
// its own native session without probing a foreign id.
type Snapshot struct {
	Setup          [][]byte             `json:"setup"`                    // initialize only; old snapshots may contain unsafe global authenticate lines
	Authentication []AuthenticationSnap `json:"authentication,omitempty"` // successful requests, scoped to provider + advertised method
	Sessions       []SessionSnap        `json:"sessions"`                 // one per live editor session
}

// AuthenticationSnap is one successful provider-owned authenticate request retained across a
// supervisor re-exec. MethodID is explicit so restore never has to trust malformed request JSON.
type AuthenticationSnap struct {
	Provider string          `json:"provider"`
	Account  string          `json:"account"`
	MethodID string          `json:"method_id"`
	Request  json.RawMessage `json:"request"`
}

// SessionSnap is one session flattened for serialization.
type SessionSnap struct {
	EditorID  string          `json:"editor_id"`
	AdapterID string          `json:"adapter_id,omitempty"`
	Provider  string          `json:"provider,omitempty"`
	Params    json.RawMessage `json:"params"`
	Closed    bool            `json:"closed,omitempty"`
	Turned    bool            `json:"turned"`
}

// snapshot copies the proxy's setup + sessions into a serializable Snapshot, under the lock.
func (p *proxy) snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap := Snapshot{}
	for _, line := range p.setup {
		if parse(line).Method == "initialize" {
			snap.Setup = [][]byte{clone(line)}
			break
		}
	}
	scopes := make([]authenticationScope, 0, len(p.authentication))
	for scope := range p.authentication {
		scopes = append(scopes, scope)
	}
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].provider == scopes[j].provider {
			return scopes[i].account < scopes[j].account
		}
		return scopes[i].provider < scopes[j].provider
	})
	for _, scope := range scopes {
		state := p.authentication[scope]
		snap.Authentication = append(snap.Authentication, AuthenticationSnap{
			Provider: scope.provider, Account: scope.account, MethodID: state.methodID, Request: clone(state.line),
		})
	}
	for id, s := range p.sessions {
		snap.Sessions = append(snap.Sessions, SessionSnap{
			EditorID: id, AdapterID: s.adapterID, Provider: s.provider, Params: s.params, Closed: s.closed, Turned: s.turned,
		})
	}
	return snap
}

// restore seeds a fresh proxy's setup + sessions from a Snapshot. Old snapshots did not carry the
// native id/provider; a provider-aware child re-creates those rather than guessing ownership.
func (p *proxy) restore(snap Snapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setup = nil
	for _, l := range snap.Setup {
		if parse(l).Method == "initialize" {
			p.setup = [][]byte{clone(l)}
			break
		}
	}
	if p.authentication == nil {
		p.authentication = map[authenticationScope]authenticationState{}
	}
	for _, a := range snap.Authentication {
		request := parse(a.Request)
		if a.Provider == "" || a.Account == "" || a.MethodID == "" || request.Method != "authenticate" ||
			a.MethodID != authenticationMethodID(request.Params) {
			continue
		}
		p.authentication[authenticationScope{a.Provider, a.Account}] = authenticationState{a.MethodID, clone(a.Request)}
	}
	for _, s := range snap.Sessions {
		adapterID := s.AdapterID
		if adapterID == "" {
			adapterID = s.EditorID
		}
		p.sessions[s.EditorID] = &sess{
			params: s.Params, adapterID: adapterID, provider: s.Provider, closed: s.Closed, turned: s.Turned,
		}
		if adapterID != s.EditorID {
			p.byAdapter[adapterID] = s.EditorID
		}
	}
}

func (p *proxy) setChild(c *Child) {
	p.mu.Lock()
	p.child = c
	p.generation++
	p.initResult = nil
	p.authMethods = nil
	p.initGeneration = 0
	p.authPending = map[authenticationScope]string{}
	p.retired = map[string]bool{}
	p.unavailable = map[string]bool{}
	p.reactivating = map[string]string{}
	p.mu.Unlock()
}

func (p *proxy) retireChild(c *Child) {
	p.mu.Lock()
	if p.child == c {
		p.child = nil
		p.generation++
	}
	p.mu.Unlock()
}

func (p *proxy) shutdownChild() {
	p.mu.Lock()
	p.shuttingDown = true
	c, candidate := p.child, p.candidate
	p.mu.Unlock()
	if c != nil {
		c.Stop()
	}
	if candidate != nil && candidate != c {
		candidate.Stop()
	}
}

// beginReload closes request admission and fails every already-admitted request under the same
// lock, then stops the child. Nothing can cross the snapshot/exec boundary without a response.
func (p *proxy) beginReload() {
	p.mu.Lock()
	p.reloading.Store(true)
	ids := make([]string, 0, len(p.pending))
	for id := range p.pending {
		ids = append(ids, id)
		delete(p.sessionReqs, id)
		delete(p.setupReqs, id)
	}
	p.pending = map[string]bool{}
	p.authPending = map[authenticationScope]string{}
	p.reactivating = map[string]string{}
	c, candidate := p.child, p.candidate
	if c != nil {
		p.child = nil
		p.generation++
	}
	p.candidate = nil
	p.mu.Unlock()
	for _, id := range ids {
		_, _ = p.out.Write(errorResponse(id))
	}
	if c != nil {
		c.Stop()
	}
	if candidate != nil && candidate != c {
		candidate.Stop()
	}
}

// fromClient records replay-relevant state, then forwards the line to the current
// child. A write to a momentarily-dead child during a swap is dropped; that request
// is covered by failAllPending on the swap.
func (p *proxy) fromClient(line []byte) {
	p.forwardClient(line, originEditor)
}

// forwardClient is the common editor/synthetic request path. Synthetic replay resumes deliberately
// skip Hooks.FromEditor, but retain every protocol responsibility below it: settings gating, session
// id remapping, pending tracking, reload admission, and child delivery.
func (p *proxy) forwardClient(line []byte, origin clientOrigin) {
	if origin == originEditor {
		p.controlMu.Lock()
		defer p.controlMu.Unlock()
	}
	if origin == originEditor {
		traceLine("editor→box", line)
	} else {
		traceLine("coop→box(resume)", line)
	}
	original := parse(line)
	if p.reloading.Load() && original.isRequest() {
		_, _ = p.out.Write(errorResponse(string(original.ID)))
		return
	}
	if p.gatePrompt(line, origin) {
		return
	}
	// coop's control layer gets first look: it may handle an editor request itself (a coop-owned
	// config option like the credential/preset selector) — not forwarding it to the adapter, replying
	// to the editor directly, and optionally restarting the box on a new credential/preset.
	if origin == originEditor && p.hooks != nil && p.hooks.FromEditor != nil {
		handled, resp, toAdapter, restart := p.hooks.FromEditor(line)
		if handled {
			Trace("coop handled the editor line itself (restart=%v)", restart)
			// A translated adapter request (e.g. a synthesized model set → session/set_model). Write it
			// to the CURRENT child; a write to a momentarily-dead child during a swap is dropped, exactly
			// like fromClient's forward.
			if len(toAdapter) > 0 {
				p.mu.Lock()
				if p.reloading.Load() {
					p.mu.Unlock()
					_, _ = p.out.Write(errorResponse(string(original.ID)))
					return
				}
				c := p.child
				p.trackInjectedLocked(toAdapter)
				p.mu.Unlock()
				if c != nil {
					traceLine("editor→box(coop)", toAdapter)
					_, _ = c.In.Write(toAdapter)
				}
			}
			if len(resp) > 0 {
				traceLine("box→editor(coop)", resp)
				_, _ = p.out.Write(resp)
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
	if p.reloading.Load() && h.isRequest() {
		p.mu.Unlock()
		_, _ = p.out.Write(errorResponse(string(h.ID)))
		return
	}
	if p.sessionReqs == nil {
		p.sessionReqs = map[string]sessionRequest{}
	}
	if p.setupReqs == nil {
		p.setupReqs = map[string]setupRequest{}
	}
	if h.isRequest() {
		provider := ""
		account := ""
		if p.child != nil {
			provider = p.child.Provider
			account = p.child.Account
		}
		adapterID := sid
		sessionProvider := ""
		s := p.sessions[sid]
		if s != nil {
			adapterID = s.adapterID
			sessionProvider = s.provider
		}
		unavailable := sid != "" && p.unavailable[sid]
		foreign := sid != "" && sessionProvider != "" && provider != "" && sessionProvider != provider
		childMissing := p.child == nil
		// A failed replay or provider switch must not trap a thread forever. Close can always retire
		// the proxy's active view locally; delete can always discard an identity that cannot safely be
		// sent to this provider. Both operations remain idempotent from the editor's perspective.
		if sid != "" && h.Method == "session/close" && (unavailable || foreign || childMissing || (s != nil && s.closed)) {
			closed := p.closeSessionLocked(sid, adapterID, false)
			held := p.dropSessionForceLocked(adapterID)
			p.mu.Unlock()
			p.failHeldPrompts(held)
			if closed && p.hooks != nil && p.hooks.SessionClosed != nil {
				p.hooks.SessionClosed(sid)
			}
			_, _ = p.out.Write(successResponse(string(h.ID)))
			return
		}
		if sid != "" && h.Method == "session/delete" && (unavailable || foreign || childMissing) {
			var cleanup []byte
			cleanupChild := p.child
			cleanupGeneration := p.generation
			if unavailable && !foreign && !childMissing && sessionProvider != "" && sessionProvider == provider {
				cleanup = withID(withSessionID(line, adapterID), fmt.Sprintf(
					"%sdelete-%d-%s", InjectPrefix, p.generation, trimQuotes(h.ID),
				))
			}
			ended := p.deleteSessionLocked(sid, adapterID, false)
			held := p.dropSessionForceLocked(adapterID)
			p.mu.Unlock()
			p.failHeldPrompts(held)
			if ended && p.hooks != nil && p.hooks.SessionEnded != nil {
				p.hooks.SessionEnded(sid)
			}
			_, _ = p.out.Write(successResponse(string(h.ID)))
			if len(cleanup) > 0 {
				p.mu.Lock()
				active := p.child == cleanupChild && p.generation == cleanupGeneration
				p.mu.Unlock()
				if active {
					_, _ = cleanupChild.In.Write(cleanup)
				}
			}
			return
		}
		if childMissing {
			p.mu.Unlock()
			if origin == originEditor {
				_, _ = p.out.Write(sessionUnavailableResponse(string(h.ID), sid, provider))
			}
			return
		}
		reactivation := h.Method == "session/load" || h.Method == "session/resume"
		closed := s != nil && s.closed && !reactivation && h.Method != "session/delete"
		if sid != "" && ((unavailable && !reactivation) || foreign || closed) {
			p.mu.Unlock()
			_, _ = p.out.Write(sessionUnavailableResponse(string(h.ID), sid, provider))
			return
		}
		switch h.Method {
		case "initialize":
			// Initialize parameters describe the editor and can be reused for every child, but each
			// child's RESPONSE is fresh capability truth. Replace instead of append so a duplicate
			// editor initialize cannot grow an invalid replay tape.
			p.setup = [][]byte{clone(line)}
			p.setupReqs[string(h.ID)] = setupRequest{
				method: h.Method, provider: provider, account: account, line: clone(line), generation: p.generation,
			}
		case "authenticate":
			methodID := authenticationMethodID(h.Params)
			// ACP requires methodId to be advertised by THIS initialize response. The editor's
			// original method menu cannot change after a provider switch, so reject a stale click
			// locally instead of invoking an unrelated provider method.
			if p.initGeneration != p.generation || !p.authMethods[methodID] {
				p.mu.Unlock()
				_, _ = p.out.Write(authenticationUnavailableResponse(string(h.ID), provider, methodID))
				return
			}
			scope := authenticationScope{provider, account}
			if p.authPending == nil {
				p.authPending = map[authenticationScope]string{}
			}
			if p.authPending[scope] != "" {
				p.mu.Unlock()
				_, _ = p.out.Write(authenticationBusyResponse(string(h.ID), provider, account))
				return
			}
			p.authPending[scope] = string(h.ID)
			p.setupReqs[string(h.ID)] = setupRequest{
				method: h.Method, provider: provider, account: account, methodID: methodID, line: clone(line),
				generation: p.generation,
			}
		case "logout":
			scope := authenticationScope{provider, account}
			if p.authPending == nil {
				p.authPending = map[authenticationScope]string{}
			}
			if p.authPending[scope] != "" {
				p.mu.Unlock()
				_, _ = p.out.Write(authenticationBusyResponse(string(h.ID), provider, account))
				return
			}
			p.authPending[scope] = string(h.ID)
			p.setupReqs[string(h.ID)] = setupRequest{
				method: h.Method, provider: provider, account: account, generation: p.generation,
			}
		case "session/new":
			p.sessionReqs[string(h.ID)] = sessionRequest{
				method: h.Method, provider: provider, params: h.Params, generation: p.generation,
			}
		case "session/load", "session/resume":
			if sid != "" {
				if s != nil && s.closed {
					if p.reactivating == nil {
						p.reactivating = map[string]string{}
					}
					p.reactivating[adapterID] = string(h.ID)
				}
				p.sessionReqs[string(h.ID)] = sessionRequest{
					method: h.Method, editorID: sid, adapterID: adapterID, provider: provider, params: h.Params,
					generation: p.generation,
				}
			}
		case "session/prompt":
			if sid != "" {
				p.sessionReqs[string(h.ID)] = sessionRequest{
					method: h.Method, editorID: sid, adapterID: adapterID, provider: provider, generation: p.generation,
				}
			}
		case "session/close", "session/delete":
			if sid != "" {
				p.sessionReqs[string(h.ID)] = sessionRequest{
					method: h.Method, editorID: sid, adapterID: adapterID, provider: provider, generation: p.generation,
				}
			}
		}
		p.pending[string(h.ID)] = true
	}
	if h.Method == "session/prompt" && origin == originEditor && p.hooks != nil && p.hooks.PromptForwarded != nil {
		p.hooks.PromptForwarded(line, origin == originSynthetic)
	}
	// Translate the editor's session id to the box's, if this session was re-created under a new one
	// (turn-less at a restart). Normal case: adapterID == sid, so no rewrite.
	fwd := line
	if s := p.sessions[sid]; sid != "" && s != nil && s.adapterID != sid {
		fwd = withSessionID(line, s.adapterID)
	}
	var writeChild *Child
	writeGeneration := p.generation
	if p.child != nil {
		writeChild = p.child
	}
	p.mu.Unlock()
	wrote := false
	if writeChild != nil {
		_, err := writeChild.In.Write(fwd)
		wrote = err == nil
	}
	if wrote && h.Method == "session/prompt" && origin == originSynthetic && p.hooks != nil && p.hooks.PromptForwarded != nil {
		// ResumePrompt is a retryable peek. Consume it only after the line reached the still-current
		// child. Holding p.mu makes this atomic with triggerRestart: a later switch re-arms the active
		// turn, while an earlier switch fails this validation and leaves the resend pending.
		p.controlMu.Lock()
		p.mu.Lock()
		if p.child == writeChild && p.generation == writeGeneration {
			p.hooks.PromptForwarded(line, true)
		}
		p.mu.Unlock()
		p.controlMu.Unlock()
	}
}

// pumpChild forwards child→editor for one child generation, learning session ids from
// session/new responses and clearing pending requests. Returns on the child's EOF.
func (p *proxy) pumpChild(child *Child, br *bufio.Reader) {
	p.mu.Lock()
	generation := p.generation
	p.mu.Unlock()
	_ = readLines(br, func(line []byte) error {
		p.mu.Lock()
		active := p.child == child && p.generation == generation
		p.mu.Unlock()
		if !active {
			Trace("dropping output from a retired child generation")
			return nil
		}
		traceLine("box→editor", line)
		// Translate the box's session id back to the one the editor knows, so coop's hooks and the
		// editor always see the editor's id (a no-op unless a session was re-created under a new one).
		line = p.remapToEditor(line)
		if len(line) == 0 {
			Trace("dropping output for a retired native session")
			return nil
		}
		h := parse(line)
		// Force-setting responses may synchronously release an editor prompt through forwardClient.
		// That path acquires controlMu itself, so keep these generation-checked synthetic responses out
		// of the controller response critical section. Normal responses remain serialized end to end.
		injectedResponse := h.isResponse() && strings.HasPrefix(string(trimQuotes(h.ID)), InjectPrefix)
		responseControl := h.isResponse() && !injectedResponse
		if responseControl {
			p.controlMu.Lock()
			defer p.controlMu.Unlock()
		}
		if !h.isResponse() {
			p.mu.Lock()
			active := p.child == child && p.generation == generation
			p.mu.Unlock()
			if !active {
				Trace("dropping request/notification from a retired child generation")
				return nil
			}
		}
		// coop answers some agent→editor requests itself (session/request_permission → always allow, so
		// the editor never prompts). The reply goes to THIS child; the request is not forwarded.
		if h.isRequest() && p.hooks != nil && p.hooks.AutoReply != nil {
			if reply, forward := p.hooks.AutoReply(line); reply != nil || !forward {
				if len(reply) > 0 {
					p.mu.Lock()
					active := p.child == child && p.generation == generation
					p.mu.Unlock()
					if active {
						_, _ = child.In.Write(reply)
					}
				}
				if !forward {
					return nil
				}
			}
		}
		if h.isResponse() {
			// Swallow responses to coop's injected force-sets — the editor never sent them.
			if injectedResponse {
				p.handleInjectedResponseFrom(line, child, generation)
				return nil
			}
			id := string(h.ID)
			p.mu.Lock()
			// The first generation check is only an optimistic fast path. A restart can retire the
			// child while remapping/parsing this line, so response mutation must revalidate under the
			// same lock that owns pending and session state.
			if p.child != child || p.generation != generation {
				p.mu.Unlock()
				Trace("dropping response from a retired child generation")
				return nil
			}
			op, wasSession := p.sessionReqs[id]
			delete(p.sessionReqs, id)
			setupOp, wasSetup := p.setupReqs[id]
			delete(p.setupReqs, id)
			if wasSetup && (setupOp.method == "authenticate" || setupOp.method == "logout") {
				scope := authenticationScope{setupOp.provider, setupOp.account}
				if p.authPending[scope] == id {
					delete(p.authPending, scope)
				}
			}
			delete(p.pending, id)
			if (op.method == "session/load" || op.method == "session/resume") && p.reactivating[op.adapterID] == id {
				delete(p.reactivating, op.adapterID)
			}
			success := responseSucceeded(h)
			if wasSetup && setupOp.generation == generation && success {
				switch setupOp.method {
				case "initialize":
					p.initResult = clone(h.Result)
					p.authMethods = authenticationMethodIDs(h.Result)
					p.initGeneration = generation
				case "authenticate":
					if setupOp.methodID != "" {
						if p.authentication == nil {
							p.authentication = map[authenticationScope]authenticationState{}
						}
						scope := authenticationScope{setupOp.provider, setupOp.account}
						p.authentication[scope] = authenticationState{setupOp.methodID, clone(setupOp.line)}
					}
				case "logout":
					scope := authenticationScope{setupOp.provider, setupOp.account}
					delete(p.authentication, scope)
				}
			}
			readyID := ""
			closedID := ""
			endedID := ""
			var held []clientLine
			if wasSession && op.generation == generation && success {
				switch op.method {
				case "session/new":
					if sid := sessionID(h.Result); sid != "" {
						p.bindSessionLocked(sid, sid, op.provider, op.params, false)
						readyID = sid
					}
				case "session/load", "session/resume":
					p.bindSessionLocked(op.editorID, op.adapterID, op.provider, op.params, true)
					readyID = op.adapterID
				case "session/prompt":
					if s := p.sessions[op.editorID]; s != nil && s.adapterID == op.adapterID {
						s.turned = true
					}
				case "session/close":
					if p.closeSessionLocked(op.editorID, op.adapterID, true) {
						closedID = op.editorID
						held = p.dropSessionForceLocked(op.adapterID)
					}
				case "session/delete":
					if p.deleteSessionLocked(op.editorID, op.adapterID, true) {
						endedID = op.editorID
						held = p.dropSessionForceLocked(op.adapterID)
					}
				}
			}
			p.mu.Unlock()
			p.failHeldPrompts(held)
			// A fresh session/new just completed: force coop's per-session state (yolo, model) on the
			// adapter, which resets it every launch.
			if readyID != "" {
				p.forceSessionFor(child, generation, readyID)
			}
			if closedID != "" && p.hooks != nil && p.hooks.SessionClosed != nil {
				p.hooks.SessionClosed(closedID)
			}
			if endedID != "" && p.hooks != nil && p.hooks.SessionEnded != nil {
				p.hooks.SessionEnded(endedID)
			}
		}
		if !h.isResponse() {
			p.mu.Lock()
			active := p.child == child && p.generation == generation
			p.mu.Unlock()
			if !active {
				Trace("dropping notification from a retired child generation")
				return nil
			}
		}
		if !responseControl {
			p.controlMu.Lock()
		}
		out, restart := line, false
		if p.hooks != nil && p.hooks.ToEditor != nil {
			out, restart = p.hooks.ToEditor(line)
		}
		if !h.isResponse() {
			p.mu.Lock()
			active := p.child == child && p.generation == generation
			p.mu.Unlock()
			if !active {
				if !responseControl {
					p.controlMu.Unlock()
				}
				Trace("dropping hook output from a retired child generation")
				return nil
			}
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
		if !responseControl {
			p.controlMu.Unlock()
		}
		return nil
	})
}

// forceSession starts an acknowledgement-gated settings chain for one established session. The
// chain is serialized because model changes can reset effort, and prompts wait until it completes.
func (p *proxy) forceSession(sid string) {
	p.mu.Lock()
	child, generation := p.child, p.generation
	p.mu.Unlock()
	p.forceSessionFor(child, generation, sid)
}

func (p *proxy) forceSessionFor(child *Child, generation uint64, sid string) {
	if p.hooks == nil || p.hooks.SessionReady == nil {
		return
	}
	requests := p.hooks.SessionReady(sid)
	p.mu.Lock()
	if p.child != child || p.generation != generation {
		p.mu.Unlock()
		Trace("discarding target settings for a retired child generation")
		return
	}
	msg, c, held := p.installForceLocked(sid, requests)
	p.mu.Unlock()
	for _, prompt := range held {
		p.forwardClient(prompt.line, prompt.origin)
	}
	p.writeForce(c, msg)
}

// installForceLocked installs a gate before any setting is written. p.mu must be held; callers may
// therefore install replay gates atomically with publishing the new child.
func (p *proxy) installForceLocked(sid string, requests [][]byte) (msg []byte, c *Child, held []clientLine) {
	delete(p.forceFailed, sid)
	if old := p.forceBySess[sid]; old != nil {
		if old.timer != nil {
			old.timer.Stop()
		}
		delete(p.forceByID, old.activeID)
		delete(p.injected, old.activeID)
		held = append(held, old.held...)
	}
	if len(requests) == 0 {
		delete(p.forceBySess, sid)
		return nil, p.child, held
	}
	chain := &forceChain{session: sid, requests: requests, held: held}
	p.forceBySess[sid] = chain
	msg, c = p.nextForceLocked(chain)
	return msg, c, nil
}

// dropSessionForceLocked removes one session's target gate during close/delete. p.mu must be held.
func (p *proxy) dropSessionForceLocked(sid string) []clientLine {
	delete(p.forceFailed, sid)
	chain := p.forceBySess[sid]
	if chain == nil {
		return nil
	}
	if chain.timer != nil {
		chain.timer.Stop()
	}
	delete(p.forceByID, chain.activeID)
	delete(p.injected, chain.activeID)
	delete(p.forceBySess, sid)
	return append([]clientLine(nil), chain.held...)
}

func (p *proxy) failHeldPrompts(held []clientLine) {
	for _, prompt := range held {
		_, _ = p.out.Write(errorResponse(string(parse(prompt.line).ID)))
	}
}

// gatePrompt holds an editor prompt while its adapter session is still applying target settings,
// or rejects it after a setting failure. Held prompts have not entered normal bookkeeping yet.
func (p *proxy) gatePrompt(line []byte, origin clientOrigin) bool {
	h := parse(line)
	if h.Method != "session/prompt" || len(h.ID) == 0 {
		return false
	}
	editorID := sessionID(h.Params)
	if editorID == "" {
		return false
	}
	p.mu.Lock()
	adapterID := editorID
	if s := p.sessions[editorID]; s != nil && s.adapterID != "" {
		adapterID = s.adapterID
	}
	if chain := p.forceBySess[adapterID]; chain != nil {
		chain.held = append(chain.held, clientLine{line: clone(line), origin: origin})
		p.mu.Unlock()
		Trace("holding prompt for session %s until ACP target settings apply", editorID)
		return true
	}
	failure := p.forceFailed[adapterID]
	p.mu.Unlock()
	if failure == "" {
		return false
	}
	_, _ = p.out.Write(targetErrorResponse(string(h.ID), failure))
	return true
}

// nextForceLocked registers and returns the next request in chain. p.mu must be held.
func (p *proxy) nextForceLocked(chain *forceChain) ([]byte, *Child) {
	if chain.next >= len(chain.requests) {
		return nil, p.child
	}
	msg := chain.requests[chain.next]
	chain.next++
	id := string(trimQuotes(parse(msg).ID))
	chain.activeID = id
	p.forceByID[id] = chain
	p.injected[id] = clone(msg)
	timeout := forceReplyTimeout
	chain.timer = time.AfterFunc(timeout, func() {
		p.failForceRequest(id, fmt.Sprintf("timed out after %s", timeout))
	})
	return msg, p.child
}

func (p *proxy) writeForce(c *Child, msg []byte) {
	if len(msg) == 0 {
		return
	}
	id := string(trimQuotes(parse(msg).ID))
	if c == nil {
		p.failForceRequest(id, "adapter is unavailable")
		return
	}
	if _, err := c.In.Write(msg); err != nil {
		p.failForceRequest(id, "write failed: "+err.Error())
	}
}

func (p *proxy) failForceRequest(id, detail string) {
	line, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32001, "message": "ACP target setting " + detail},
	})
	p.handleInjectedResponse(append(line, '\n'))
}

// trackInjectedLocked remembers a translated synthetic request so a rejected response can identify
// the setting that failed. p.mu must be held.
func (p *proxy) trackInjectedLocked(line []byte) {
	h := parse(line)
	id := string(trimQuotes(h.ID))
	if strings.HasPrefix(id, InjectPrefix) {
		p.injected[id] = clone(line)
	}
}

// handleInjectedResponse advances a settings chain only after success. A failure leaves the session
// blocked and fails held/future prompts loudly; a successful final response releases held prompts.
func (p *proxy) handleInjectedResponse(line []byte) {
	p.handleInjectedResponseFrom(line, nil, 0)
}

// handleInjectedResponseFrom binds a child-originated synthetic response to the generation that
// emitted it. Local timeout failures pass child=nil and apply to the current force chain.
func (p *proxy) handleInjectedResponseFrom(line []byte, child *Child, generation uint64) {
	h := parse(line)
	id := string(trimQuotes(h.ID))
	p.mu.Lock()
	if child != nil && (p.child != child || p.generation != generation) {
		p.mu.Unlock()
		Trace("dropping injected response from a retired child generation")
		return
	}
	request := p.injected[id]
	delete(p.injected, id)
	chain := p.forceByID[id]
	delete(p.forceByID, id)
	var next []byte
	var c *Child
	var held []clientLine
	failure := ""
	if chain != nil && chain.activeID == id {
		if chain.timer != nil {
			chain.timer.Stop()
			chain.timer = nil
		}
		chain.activeID = ""
		if len(h.Error) > 0 && string(h.Error) != "null" {
			failure = errorMessage(h.Error)
			p.forceFailed[chain.session] = failure
			delete(p.forceBySess, chain.session)
			held = append(held, chain.held...)
		} else if chain.next < len(chain.requests) {
			next, c = p.nextForceLocked(chain)
		} else {
			delete(p.forceBySess, chain.session)
			held = append(held, chain.held...)
		}
	}
	p.mu.Unlock()

	if len(request) > 0 && p.hooks != nil && p.hooks.InjectedResponse != nil {
		if notice := p.hooks.InjectedResponse(request, line); len(notice) > 0 {
			notice = p.remapToEditor(notice)
			_, _ = p.out.Write(notice)
		}
	}
	if failure != "" {
		for _, prompt := range held {
			_, _ = p.out.Write(targetErrorResponse(string(parse(prompt.line).ID), failure))
		}
		return
	}
	if len(next) > 0 {
		p.writeForce(c, next)
		return
	}
	for _, prompt := range held {
		p.forwardClient(prompt.line, prompt.origin)
	}
}

// resetForceState drops one child's synthetic request state. Prompts held behind a child that died
// receive the normal retry error rather than hanging outside pending bookkeeping.
func (p *proxy) resetForceState() {
	p.mu.Lock()
	var held []clientLine
	for _, chain := range p.forceBySess {
		if chain.timer != nil {
			chain.timer.Stop()
		}
		held = append(held, chain.held...)
	}
	p.injected = map[string][]byte{}
	p.forceByID = map[string]*forceChain{}
	p.forceBySess = map[string]*forceChain{}
	p.forceFailed = map[string]string{}
	p.mu.Unlock()
	if p.hooks != nil && p.hooks.ChildReset != nil {
		p.hooks.ChildReset()
	}
	for _, prompt := range held {
		_, _ = p.out.Write(errorResponse(string(parse(prompt.line).ID)))
	}
}

// triggerRestart tears the current child down so pumpChild returns and Run's loop respawns it — a
// coop-driven switch (e.g. a new credential/preset), flagged intentional so it's not a rapid-fail.
func (p *proxy) triggerRestart() {
	Trace("restart requested (coop switch / rotate)")
	p.intentional.Store(true)
	p.mu.Lock()
	p.restartEpoch++
	c, candidate := p.child, p.candidate
	if c != nil {
		p.child = nil
		p.generation++
	}
	p.candidate = nil
	p.mu.Unlock()
	if c != nil {
		c.Stop()
	}
	if candidate != nil && candidate != c {
		candidate.Stop()
	}
}

func (p *proxy) currentRestartEpoch() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.restartEpoch
}

func (p *proxy) replayChildActive(c *Child, epoch uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.child == c && p.restartEpoch == epoch && !p.reloading.Load() && !p.shuttingDown
}

// clearSupersededIntent consumes the restart that invalidated expected. A later restart cannot be
// lost: it either already advanced the epoch (and is also represented by the next candidate) or
// runs after this critical section and sets intentional again for the live child it stops.
func (p *proxy) clearSupersededIntent(expected uint64) {
	p.mu.Lock()
	if p.restartEpoch != expected {
		p.intentional.Store(false)
	}
	p.mu.Unlock()
}

// bindSessionLocked installs both directions of one successful native binding. p.mu must be held.
func (p *proxy) bindSessionLocked(editorID, adapterID, provider string, params json.RawMessage, turned bool) {
	if p.sessions == nil {
		p.sessions = map[string]*sess{}
	}
	if p.byAdapter == nil {
		p.byAdapter = map[string]string{}
	}
	if p.retired == nil {
		p.retired = map[string]bool{}
	}
	if adapterID == "" {
		adapterID = editorID
	}
	if old := p.sessions[editorID]; old != nil {
		if old.adapterID != editorID {
			delete(p.byAdapter, old.adapterID)
		}
		if old.adapterID != "" && old.adapterID != adapterID {
			p.retired[old.adapterID] = true
		}
	}
	delete(p.retired, adapterID)
	delete(p.unavailable, editorID)
	delete(p.reactivating, adapterID)
	p.sessions[editorID] = &sess{params: clone(params), adapterID: adapterID, provider: provider, turned: turned}
	if adapterID != editorID {
		p.byAdapter[adapterID] = editorID
	}
}

// closeSessionLocked releases one active native session without discarding its durable identity.
// An explicit load/resume can later reactivate the same provider-owned id. p.mu must be held.
func (p *proxy) closeSessionLocked(editorID, adapterID string, retire bool) bool {
	if p.retired == nil {
		p.retired = map[string]bool{}
	}
	s := p.sessions[editorID]
	if s == nil || s.adapterID != adapterID || s.closed {
		return false
	}
	s.closed = true
	if retire {
		p.retired[s.adapterID] = true
		delete(p.unavailable, editorID)
	}
	delete(p.reactivating, s.adapterID)
	return true
}

// deleteSessionLocked retires and removes a durable binding without letting an older response
// remove a newer binding for the same stable editor id. p.mu must be held.
func (p *proxy) deleteSessionLocked(editorID, adapterID string, retire bool) bool {
	if p.retired == nil {
		p.retired = map[string]bool{}
	}
	s := p.sessions[editorID]
	if s == nil || s.adapterID != adapterID {
		return false
	}
	if s.adapterID != editorID {
		delete(p.byAdapter, s.adapterID)
	}
	if retire {
		p.retired[s.adapterID] = true
	}
	delete(p.reactivating, s.adapterID)
	delete(p.sessions, editorID)
	delete(p.unavailable, editorID)
	return true
}

// remapToEditor rewrites a box→editor line's sessionId from the box's id back to the editor's, when
// this session was re-created under a new box id. A no-op (no parse) in the normal case, so it's cheap
// on the streaming hot path.
func (p *proxy) remapToEditor(line []byte) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.byAdapter) == 0 && len(p.retired) == 0 {
		return line
	}
	aid := sessionID(parse(line).Params)
	if aid == "" {
		return line
	}
	if p.retired[aid] && p.reactivating[aid] == "" {
		return nil
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
	return p.replayAt(c, br, p.currentRestartEpoch())
}

func (p *proxy) replayAt(c *Child, br *bufio.Reader, epoch uint64) error {
	type snap struct {
		editorID, adapterID, provider string
		params                        json.RawMessage
		turned                        bool
	}
	p.mu.Lock()
	if p.restartEpoch != epoch {
		p.mu.Unlock()
		return errReplaySuperseded
	}
	p.candidate = c
	var initialize []byte
	for _, line := range p.setup {
		if parse(line).Method == "initialize" {
			initialize = clone(line)
			break
		}
	}
	providerAuth, hasProviderAuth := p.authentication[authenticationScope{c.Provider, c.Account}]
	providerAuth.line = clone(providerAuth.line)
	snaps := make([]snap, 0, len(p.sessions))
	for eid, s := range p.sessions {
		if s.closed {
			continue
		}
		snaps = append(snaps, snap{eid, s.adapterID, s.provider, s.params, s.turned})
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.candidate == c {
			p.candidate = nil
		}
		p.mu.Unlock()
	}()

	Trace("replay: negotiating %s and restoring %d session(s) on the restarted box", c.Provider, len(snaps))
	var sessionMsgs [][]byte
	for _, s := range snaps {
		// Native ids belong to one provider. Load only when the replacement child is that provider (or
		// when both sides are provider-agnostic legacy callers); otherwise create directly and let Coop
		// carry context without first sending a known-foreign id.
		canLoad := s.turned && (c.Provider == "" || (s.provider != "" && s.provider == c.Provider))
		if canLoad {
			id := replayPrefix + "load-" + s.editorID
			if msg := loadRequest(id, s.adapterID, s.params); msg != nil {
				sessionMsgs = append(sessionMsgs, msg)
			}
		} else {
			id := replayPrefix + "new-" + s.editorID
			if msg := newRequest(id, s.params); msg != nil {
				sessionMsgs = append(sessionMsgs, msg)
			}
		}
	}
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
	ready := map[string]bool{} // editor sessions successfully established on this child
	bindings := map[string]replayBinding{}
	var recreated []string
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
	initID := replayPrefix + "initialize"
	authReplayID := replayPrefix + "authenticate"
	var freshInitResult json.RawMessage
	replayRecovery := func(line []byte) (message string, handled bool, err error) {
		if p.hooks == nil || p.hooks.ReplayFailure == nil {
			return "", false, nil
		}
		p.controlMu.Lock()
		p.mu.Lock()
		current := p.restartEpoch == epoch && p.candidate == c && !p.reloading.Load() && !p.shuttingDown
		p.mu.Unlock()
		if !current {
			p.controlMu.Unlock()
			return "", false, errReplaySuperseded
		}
		out, restart, handled := p.hooks.ReplayFailure(line)
		if restart {
			p.triggerRestart()
		}
		p.controlMu.Unlock()
		if restart {
			return "", true, errReplaySuperseded
		}
		if handled && len(out) > 0 {
			message = errorMessage(parse(out).Error)
		}
		return message, handled, nil
	}
	process := func(expect map[string]bool) error {
		for len(expect) > 0 {
			line, err := readLineCtx(br, timeout)
			if len(line) > 0 {
				if h := parse(line); h.isResponse() {
					id := string(trimQuotes(h.ID))
					recoveryMessage, recoveryHandled := "", false
					if !responseSucceeded(h) {
						var recoveryErr error
						recoveryMessage, recoveryHandled, recoveryErr = replayRecovery(line)
						if recoveryErr != nil {
							return recoveryErr
						}
					}
					if id == initID {
						if !responseSucceeded(h) {
							message := errorMessage(h.Error)
							if recoveryMessage != "" {
								message = recoveryMessage
							}
							return fmt.Errorf("replay initialize failed for provider %s: %s", c.Provider, message)
						}
						freshInitResult = clone(h.Result)
					} else if id == authReplayID && !responseSucceeded(h) {
						// A stored method can expire between children. Retire it immediately so later
						// restarts do not loop; session setup then exposes normal auth_required recovery.
						p.mu.Lock()
						delete(p.authentication, authenticationScope{c.Provider, c.Account})
						p.mu.Unlock()
						message := errorMessage(h.Error)
						if recoveryMessage != "" {
							message = recoveryMessage
						}
						Trace("replay: authentication failed for %s@%s and was retired: %s", c.Provider, c.Account, message)
					} else if eid, ok := strings.CutPrefix(id, replayPrefix+"new-"); ok {
						// A re-created session — turn-less in round one, or a turned one whose reload
						// failed in round two: bind the editor's id to the fresh box id so both directions
						// translate from here on. If even session/new failed, the box is broken.
						if newID := sessionID(h.Result); newID != "" {
							bindings[eid] = replayBinding{adapterID: newID, provider: c.Provider}
							ready[eid] = true
							configUpdates = append(configUpdates, configUpdate{eid, resultConfigOptions(h.Result)})
							// A TURNED session landed here only because its conversation didn't carry —
							// tell coop, so it can inject its best-effort history preamble.
							if snapByEditor[eid].turned {
								recreated = append(recreated, eid)
							}
						} else if !responseSucceeded(h) {
							message := errorMessage(h.Error)
							if recoveryMessage != "" {
								message = recoveryMessage
							}
							fmt.Fprintf(warnOut, "coop acp: session %s could not be re-created after the box restarted: %s\n", eid, message)
							Trace("replay: session %s re-create failed: %s", eid, message)
							failures = append(failures, replayFailure{eid, message})
						}
					} else if eid, ok := strings.CutPrefix(id, replayPrefix+"load-"); ok {
						// A session that DID have a transcript failed to reload — a provider switch (the
						// new agent can't read the old one's store), or a genuinely lost transcript.
						// Re-create it fresh in a second round; coop carries the conversation best-effort.
						if !responseSucceeded(h) && recoveryHandled {
							message := recoveryMessage
							if message == "" {
								message = errorMessage(h.Error)
							}
							failures = append(failures, replayFailure{eid, message})
						} else if !responseSucceeded(h) {
							fmt.Fprintf(warnOut, "coop acp: session %s did not reload after the box restarted; re-creating it fresh (context carried best-effort): %s\n", eid, h.Error)
							Trace("replay: session %s did NOT reload — re-creating: %s", eid, h.Error)
							recreate = append(recreate, eid)
						} else {
							provider := c.Provider
							if provider == "" {
								provider = snapByEditor[eid].provider
							}
							bindings[eid] = replayBinding{
								adapterID: snapByEditor[eid].adapterID, provider: provider, turned: true,
							}
							ready[eid] = true
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
	// Run replay in protocol phases. Initialize must finish before its authMethods can select compatible
	// provider-owned authentication, and authentication must finish before a session can rely on it.
	// Each phase writes concurrently with reads so a large session batch cannot fill the child's stdin.
	sendPhase := func(msgs [][]byte) error {
		if len(msgs) == 0 || childEOF {
			return nil
		}
		expect := map[string]bool{}
		for _, msg := range msgs {
			if h := parse(msg); h.isRequest() {
				expect[string(trimQuotes(h.ID))] = true
			}
		}
		sent := make(chan struct{})
		go func() {
			defer close(sent)
			for _, msg := range msgs {
				if _, err := c.In.Write(msg); err != nil {
					return
				}
			}
		}()
		err := process(expect)
		<-sent
		return err
	}
	if len(initialize) > 0 {
		if err := sendPhase([][]byte{withID(initialize, initID)}); err != nil {
			return err
		}
	}
	freshAuthMethods := authenticationMethodIDs(freshInitResult)
	var authMsgs [][]byte
	if hasProviderAuth && freshAuthMethods[providerAuth.methodID] {
		authMsgs = append(authMsgs, withID(providerAuth.line, authReplayID))
	} else if hasProviderAuth {
		Trace("replay: %s@%s no longer advertises auth method %q; not replaying it", c.Provider, c.Account, providerAuth.methodID)
	}
	if err := sendPhase(authMsgs); err != nil {
		return err
	}
	if err := sendPhase(sessionMsgs); err != nil {
		return err
	}
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
	if childEOF {
		// None of this candidate child's bindings are authoritative. Its EOF will make the main loop
		// start another child and replay the last committed identities again.
		bindings = map[string]replayBinding{}
		recreated = nil
		ready = map[string]bool{}
		configUpdates = nil
		failures = nil
		freshInitResult = nil
		freshAuthMethods = nil
	}
	p.mu.Lock()
	current := p.restartEpoch == epoch && p.candidate == c && !p.reloading.Load() && !p.shuttingDown
	p.mu.Unlock()
	if !current {
		return errReplaySuperseded
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
		for eid := range ready {
			if line := p.hooks.ResumePrompt(eid); len(line) > 0 {
				resumes = append(resumes, line)
				if id := parse(line).ID; len(id) > 0 {
					keep[string(id)] = true
				}
			}
		}
	}
	forces := map[string][][]byte{}
	if p.hooks != nil && p.hooks.SessionReady != nil {
		for eid := range ready {
			aid := bindings[eid].adapterID
			if aid != "" {
				forces[aid] = p.hooks.SessionReady(aid)
			}
		}
	}
	if !p.swapChildAt(c, keep, forces, bindings, freshInitResult, freshAuthMethods, epoch, true) {
		return errReplaySuperseded
	}
	for _, eid := range recreated {
		if !p.replayChildActive(c, epoch) {
			return errReplaySuperseded
		}
		if p.hooks != nil && p.hooks.SessionRecreated != nil {
			p.hooks.SessionRecreated(eid)
		}
	}
	// Tell the editor what the restarted box's sessions ACTUALLY look like (the model in force after a
	// credential/preset switch, say) — it never saw the load/new results. Synthesized on the editor
	// side of the boundary with the editor's ids, and run through ToEditor so coop's toolbar rewrite
	// (drop mode, prepend coop_setup, refresh its cache) applies. The restart flag is ignored: a
	// config notification can't be a rate-limit error.
	for _, cu := range configUpdates {
		if !p.replayChildActive(c, epoch) {
			return errReplaySuperseded
		}
		if len(cu.options) == 0 {
			continue
		}
		line := configOptionUpdateLine(cu.editorID, cu.options)
		if p.hooks != nil && p.hooks.ToEditor != nil {
			line, _ = p.hooks.ToEditor(line)
		}
		if !p.replayChildActive(c, epoch) {
			return errReplaySuperseded
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
		if !p.replayChildActive(c, epoch) {
			return errReplaySuperseded
		}
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
		if !p.replayChildActive(c, epoch) {
			return errReplaySuperseded
		}
		p.forwardClient(line, originSynthetic)
	}
	if !p.replayChildActive(c, epoch) {
		return errReplaySuperseded
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
func (p *proxy) swapChild(c *Child, keep map[string]bool, forces map[string][][]byte, bindings map[string]replayBinding) {
	_ = p.swapChildAt(c, keep, forces, bindings, nil, nil, p.currentRestartEpoch(), false)
}

func (p *proxy) swapChildAt(
	c *Child,
	keep map[string]bool,
	forces map[string][][]byte,
	bindings map[string]replayBinding,
	initResult json.RawMessage,
	authMethods map[string]bool,
	epoch uint64,
	requireCandidate bool,
) bool {
	p.mu.Lock()
	if p.restartEpoch != epoch || (requireCandidate && p.candidate != c) || p.reloading.Load() {
		p.mu.Unlock()
		return false
	}
	ids := make([]string, 0, len(p.pending))
	for id := range p.pending {
		// Even a kept prompt is re-admitted below. Its mutation must be replaced with the new
		// generation's staged request before a response with the reused JSON-RPC id can arrive.
		delete(p.sessionReqs, id)
		delete(p.setupReqs, id)
		if keep[id] {
			continue
		}
		ids = append(ids, id)
	}
	p.pending = map[string]bool{}
	p.authPending = map[authenticationScope]string{}
	p.retired = map[string]bool{}
	p.reactivating = map[string]string{}
	if bindings != nil {
		p.unavailable = map[string]bool{}
		for eid, s := range p.sessions {
			if !s.closed {
				p.unavailable[eid] = true
			}
		}
		for eid, binding := range bindings {
			if s := p.sessions[eid]; s != nil {
				p.bindSessionLocked(eid, binding.adapterID, binding.provider, s.params, binding.turned)
			}
		}
	}
	p.child = c
	if p.candidate == c {
		p.candidate = nil
	}
	p.generation++
	p.initResult = clone(initResult)
	p.authMethods = cloneBoolMap(authMethods)
	p.initGeneration = p.generation
	var forceWrites [][]byte
	var released []clientLine
	for sid, requests := range forces {
		msg, _, held := p.installForceLocked(sid, requests)
		if len(msg) > 0 {
			forceWrites = append(forceWrites, msg)
		}
		released = append(released, held...)
	}
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
	if !down {
		for _, msg := range forceWrites {
			p.writeForce(c, msg)
		}
		for _, prompt := range released {
			p.forwardClient(prompt.line, prompt.origin)
		}
	}
	return true
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
		// A staged session mutation on a dead child will never get a response. Drop it so a
		// later generation reusing the JSON-RPC id cannot commit stale state.
		delete(p.sessionReqs, id)
		delete(p.setupReqs, id)
	}
	p.pending = map[string]bool{}
	p.authPending = map[authenticationScope]string{}
	p.reactivating = map[string]string{}
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

func responseSucceeded(h header) bool {
	return len(h.Error) == 0 || string(h.Error) == "null"
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

func authenticationMethodID(raw json.RawMessage) string {
	var v struct {
		MethodID string `json:"methodId"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.MethodID
}

// authenticationMethodIDs extracts the only method ids valid for authenticate on one initialized
// child. ACP explicitly scopes methodId to the initialize response, so absent/malformed means none.
func authenticationMethodIDs(result json.RawMessage) map[string]bool {
	var v struct {
		AuthMethods []struct {
			ID string `json:"id"`
		} `json:"authMethods"`
	}
	_ = json.Unmarshal(result, &v)
	methods := make(map[string]bool, len(v.AuthMethods))
	for _, method := range v.AuthMethods {
		if method.ID != "" {
			methods[method.ID] = true
		}
	}
	return methods
}

func cloneBoolMap(src map[string]bool) map[string]bool {
	if src == nil {
		return nil
	}
	dst := make(map[string]bool, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
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

func successResponse(id string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"result":{}}` + "\n")
}

func sessionUnavailableResponse(id, sessionID, provider string) []byte {
	message, _ := json.Marshal(fmt.Sprintf(
		"coop: session %s is not available on provider %s; switch back or start a new thread", sessionID, provider,
	))
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"error":{"code":-32002,"message":` + string(message) + `}}` + "\n")
}

func authenticationUnavailableResponse(id, provider, methodID string) []byte {
	message, _ := json.Marshal(fmt.Sprintf(
		"coop: authentication method %q is not advertised by provider %s; use that provider's Coop account selector or run coop login %s@account",
		methodID, provider, provider,
	))
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"error":{"code":-32602,"message":` + string(message) + `}}` + "\n")
}

func authenticationBusyResponse(id, provider, account string) []byte {
	message, _ := json.Marshal(fmt.Sprintf(
		"coop: authentication is already in progress for %s@%s; wait for it to finish before retrying",
		provider, account,
	))
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"error":{"code":-32003,"message":` + string(message) + `}}` + "\n")
}

func targetErrorResponse(id, detail string) []byte {
	message, _ := json.Marshal("coop: ACP target settings failed: " + detail + "; switch the target or retry the session")
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"error":{"code":-32001,"message":` + string(message) + `}}` + "\n")
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
