package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// acpRefreshInterval is how often a filtered editor session looks for an approval change — one small
// owner-private read when nothing changed; it admits again only once every thread is idle.
const acpRefreshInterval = 2 * time.Second

// acpRefreshSettle is how long a prepared replacement must keep running before the box it replaces is
// stopped: about as long as a filtered box takes to answer its first request (~6 s, measured for the
// warm pool). One that survives it got through the gateway, container and adapter start — the steps a
// new policy can break — so the switch rarely hands the session a box that is still failing. There is
// no way back after the switch on purpose: going back to the rules the old box ran would widen a
// session whose new approval narrowed them, and an agent can make its own box exit. A prompt sent
// before the switch runs on the old box; the editor is told once the session moved.
const acpRefreshSettle = 6 * time.Second

var (
	// errACPNetworkModeChanged is an approval that no longer resolves to filtered networking: a running
	// filtered session cannot become an open or offline one by replacing its boxes.
	errACPNetworkModeChanged = errors.New("this project's network access mode changed")
	// errACPApprovalWithdrawn is a refusal after `coop net forget`.
	errACPApprovalWithdrawn = errors.New("this project's network approval was withdrawn")
	// errACPReplacementStopped is a prepared replacement that exited before it could take over.
	errACPReplacementStopped = errors.New("the replacement box stopped while starting")
)

// acpRefreshMoved is what the editor shows once the live box runs a newly adopted policy. Replacing
// the box stops whatever the agent left running in the old one, so the notice says so.
const acpRefreshMoved = "Coop moved this session to a new box with the network rules you approved. Anything still running in the old box was stopped."

// acpRefreshNotice is what the editor shows when a network approval change could not be applied.
// Fixed sentences only: an admission error can name hosts and paths, and those stay in the agent log.
func acpRefreshNotice(err error) string {
	var pending *networkstate.PendingApproval
	switch {
	case errors.Is(err, errACPNetworkModeChanged):
		return "Coop kept this session's network rules: this project's network access changed. Reconnect the editor to use it."
	case errors.Is(err, errACPApprovalWithdrawn):
		return "Coop kept this session's network rules, but this project's approval was withdrawn. Reconnect the editor to apply the withdrawal."
	case errors.As(err, &pending):
		return "Coop kept this session's network rules: this project's requested access needs review. Run coop approve to apply it."
	case errors.Is(err, box.ErrNetworkSetupRequired):
		return "Coop kept this session's network rules: this host's network setup is out of date. Run coop net setup, then reconnect the editor."
	default:
		return "Coop kept this session's network rules: the new rules could not be applied. The reason is in the agent log; reconnect the editor to try again."
	}
}

// acpAdmit runs a filtered `coop acp` supervisor's startup admission again (see acpReadmit).
type acpAdmit func(context.Context) (*box.CapturedEgress, error)

// admitNetworkFunc is box.AdmitNetwork's shape, for the app's admission test seam.
type admitNetworkFunc func(*config.Config, runtime.Runtime, box.RunSpec, box.NetworkAdmission) (*box.CapturedEgress, error)

// admitACPNetwork is box.AdmitNetwork unless a test replaced it: a filtered admission cannot succeed
// without Docker, and a test of what a supervisor admits needs one that does.
func (a *app) admitACPNetwork(cfg *config.Config, spec box.RunSpec, options box.NetworkAdmission) (*box.CapturedEgress, error) {
	if a.admitNetwork != nil {
		return a.admitNetwork(cfg, a.rt, spec, options)
	}
	return box.AdmitNetwork(cfg, a.rt, spec, options)
}

// acpReadmit admits this session's network policy again exactly as its supervisor did at startup —
// the spec and options frozen then (lead, preset, flags and the provider/account scope, never derived
// again from presets an agent can edit) on the configuration as it stood before that admission —
// except that it never sets the host up. The project's request is read again, as in any admission:
// that is how an edit nobody approved is refused.
func (a *app) acpReadmit(ctx context.Context, cfg *config.Config, spec box.RunSpec, options box.NetworkAdmission) (*box.CapturedEgress, error) {
	spec.Ctx, spec.Peers = ctx, slices.Clone(spec.Peers)
	options.ExistingQualification = true
	return a.admitACPNetwork(cfg.Clone(), spec, options)
}

// acpSpawn is one selection a box is started for: SpawnTarget's answer.
type acpSpawn struct {
	target agents.Target
	preset string
	ok     bool // false: the selection was unrecognizable, and the box runs on the launch arguments alone
}

func (s acpSpawn) same(o acpSpawn) bool {
	return s.ok == o.ok && s.preset == o.preset && s.target.String() == o.target.String()
}

// acpNetworkRefresh moves a running filtered `coop acp` session onto a newly approved network policy
// without the editor reconnecting. A box's policy never changes: when the host's approval generation
// moves, the session admits again under the provider/account scope it froze at startup and REPLACES
// its boxes — parked ones at once, the live one through a restart the proxy accepts only while every
// thread is idle, after its replacement has started and kept running. A box starting anyway (a
// respawn, a switch) starts under the newest approval. A refusal keeps the rules the live box runs
// and is reported.
//
// Concurrency: check, swap, prepare and fail run on the refresh loop alone, so at most one
// replacement is ever prepared; take and started run in the proxy's factory; adopt runs in both, one
// at a time. Lock order: admitting, then mu. Every child the factory returns passes through started.
type acpNetworkRefresh struct {
	generation func() (uint64, error)                  // the host's approval generation
	admit      func() (*box.CapturedEgress, error)     // the startup admission again; existing host setup only
	peek       func() (acpSpawn, bool)                 // the selection the next spawn would use; false: none to prepare for
	spawn      func(acpSpawn) (*acpproxy.Child, error) // a parked replacement under the current capture
	idle       func() bool                             // nothing in flight: a cheap look
	restart    func() bool                             // restart the live child while still idle
	drain      func()                                  // stop parked boxes of a replaced policy, then refill
	log        func(error)                             // the agent log gets the whole reason
	notify     func(string)                            // the editor gets fixed sentences
	now        func() time.Time                        // the clock; tests set it
	stop       context.CancelFunc                      // ends admit's and spawn's context
	stopped    <-chan struct{}                         // closed by stop: the loop ends

	admitting sync.Mutex // one admission at a time: the refresh loop and a respawn both adopt

	mu          sync.Mutex
	startup     *box.CapturedEgress   // admitted at startup; closed by its admitter
	capture     *box.CapturedEgress   // what a box started now proves
	created     []*box.CapturedEgress // captures this refresh admitted, closed with the session
	seen        uint64                // the generation capture reflects, or the last one tried
	unreadable  bool                  // the generation could not be read last time (traced once)
	running     string                // the policy the live box proved; "" before the first
	prepared    *acpproxy.Child       // a started replacement waiting for a quiet moment
	preparedFor acpSpawn
	preparedAt  time.Time
	failed      string // a policy whose replacement failed, until the generation moves again
	shown       string // the last notice of a failure, not repeated while nothing else changed
	closed      bool
}

// newACPNetworkRefresh wires a filtered `coop acp` session's refresh to its supervisor; nil for an
// open or offline session. Nothing runs until start.
func (a *app) newACPNetworkRefresh(ctrl *acpctl.Control, restarter *acpproxy.Restarter, drain func(),
	spawn func(context.Context, acpSpawn) (*acpproxy.Child, error)) *acpNetworkRefresh {
	startup, readmit := a.acpCapture, a.acpAdmission
	if startup == nil || startup.Store == nil || readmit == nil {
		return nil
	}
	ctx, stop := context.WithCancel(context.Background())
	return &acpNetworkRefresh{
		generation: startup.Store.ApprovalGeneration,
		admit: func() (*box.CapturedEgress, error) {
			capture, err := readmit(ctx)
			// After `coop net forget` admission names the review a project still asking for rules
			// needs; the editor is told about the withdrawal instead.
			if err != nil {
				if withdrawn, werr := startup.Store.Withdrawn(startup.Project); werr == nil && withdrawn {
					err = fmt.Errorf("%w: %w", errACPApprovalWithdrawn, err)
				}
			}
			return capture, err
		},
		peek: func() (acpSpawn, bool) {
			target, preset, ok := ctrl.PeekSpawnTarget()
			return acpSpawn{target: target, preset: preset, ok: ok}, ok
		},
		spawn:   func(want acpSpawn) (*acpproxy.Child, error) { return spawn(ctx, want) },
		idle:    restarter.Idle,
		restart: restarter.RestartIfIdle,
		drain:   drain,
		log:     func(err error) { ui.Warn("kept this session's network rules: %v", err) },
		notify:  ctrl.Notify,
		now:     time.Now,
		stop:    stop,
		stopped: ctx.Done(),
		startup: startup, capture: startup, seen: startup.ApprovalGeneration,
	}
}

// start runs the refresh — after every turn end and on an idle tick — until the returned close, which
// also stops a replacement still waiting and releases the captures the refresh admitted. close may be
// called more than once, from any goroutine. A nil refresh starts nothing.
func (r *acpNetworkRefresh) start(ctrl *acpctl.Control) func() {
	if r == nil {
		return func() {}
	}
	kicks := make(chan struct{}, 1)
	ctrl.OnTurnEnd(func() {
		select {
		case kicks <- struct{}{}:
		default:
		}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.run(kicks)
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.stop()
			<-done
			r.close()
		})
	}
}

// current is the capture a box started now proves.
func (r *acpNetworkRefresh) current() *box.CapturedEgress {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.capture
}

func (r *acpNetworkRefresh) run(kicks <-chan struct{}) {
	ticker := time.NewTicker(acpRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopped:
			return
		case <-ticker.C:
		case <-kicks:
		}
		r.check()
	}
}

// check adopts a changed approval once every thread is idle, then moves the live box onto the current
// policy if it still runs an older one.
func (r *acpNetworkRefresh) check() {
	if r.moved() && r.idle() {
		r.adopt()
	}
	r.swap()
}

// moved reports whether the approval generation changed since the one last seen. An unreadable
// generation tells nothing, so nothing is refreshed; `coop approve` repairs it on the next change.
func (r *acpNetworkRefresh) moved() bool {
	generation, err := r.generation()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		if !r.unreadable {
			acpproxy.Trace("network refresh: approval generation unreadable: %v", err)
		}
		r.unreadable = true
		return false
	}
	r.unreadable = false
	return generation != r.seen
}

// adopt admits again when the approval generation moved. A new capture then serves every box started
// from now on, and parked boxes of the old one are replaced; a refusal keeps the rules the live box
// runs (refuse). A respawn calls it too: that box is starting anyway, so it starts under the newest
// approval, even mid-turn.
func (r *acpNetworkRefresh) adopt() {
	if r == nil {
		return
	}
	r.admitting.Lock()
	defer r.admitting.Unlock()
	generation, err := r.generation()
	if err != nil {
		return // moved() traces it; a respawn simply starts under the current capture
	}
	r.mu.Lock()
	unchanged := generation == r.seen || r.closed
	r.mu.Unlock()
	if unchanged {
		return
	}
	capture, err := r.admit()
	if err == nil && capture == nil {
		err = errACPNetworkModeChanged
	}
	if err != nil {
		r.refuse(generation, err)
		return
	}
	r.mu.Lock()
	r.seen = generation
	r.failed = "" // a new approval may make a replacement that failed before start
	installed := !r.closed && capture.Fingerprint != r.capture.Fingerprint
	if installed {
		r.created = append(r.created, capture)
		r.capture = capture
	}
	r.mu.Unlock()
	if !installed { // another project's approval moved the generation, or the session is ending
		_ = capture.Close()
		return
	}
	acpproxy.Trace("network refresh: admitted policy %s", capture.Fingerprint)
	r.drain()
}

// refuse keeps the rules the live box runs. A capture adopted earlier but not running yet is dropped
// with the replacement prepared for it: after a withdrawal, or any later refusal, nothing may move the
// session onto rules admission no longer grants.
func (r *acpNetworkRefresh) refuse(generation uint64, err error) {
	r.mu.Lock()
	r.seen = generation
	prepared := r.prepared
	r.prepared = nil
	live := r.captureLocked(r.running)
	reverted := live != nil && live != r.capture
	if reverted {
		r.capture = live
	}
	closed := r.closed
	r.mu.Unlock()
	if prepared != nil {
		prepared.Stop()
	}
	if reverted {
		acpproxy.Trace("network refresh: back to policy %s", live.Fingerprint)
		r.drain()
	}
	if !closed && !errors.Is(err, context.Canceled) {
		r.report(err)
	}
}

// captureLocked is the capture a policy was admitted under: the startup one or one this refresh
// admitted. Called with mu held.
func (r *acpNetworkRefresh) captureLocked(policy string) *box.CapturedEgress {
	if r.startup.Fingerprint == policy {
		return r.startup
	}
	for _, capture := range r.created {
		if capture.Fingerprint == policy {
			return capture
		}
	}
	return nil
}

// swap moves the live box onto the current policy. It prepares the replacement first, parked, for the
// selection the next spawn would use, and asks the proxy to restart only once that replacement has
// kept running for acpRefreshSettle, still fits that selection, and every thread is idle; the factory
// then hands it over.
func (r *acpNetworkRefresh) swap() {
	want, known := r.peek()
	r.mu.Lock()
	policy := r.capture.Fingerprint
	// Nothing switches while the generation cannot be read: a withdrawal may be what it would say.
	due := !r.closed && !r.unreadable && known && r.running != "" && r.running != policy && r.failed != policy
	var stale *acpproxy.Child
	if r.prepared != nil && (!due || r.prepared.Policy != policy || !r.preparedFor.same(want)) {
		stale, r.prepared = r.prepared, nil
	}
	prepared, preparedAt := r.prepared, r.preparedAt
	r.mu.Unlock()
	if stale != nil {
		stale.Stop()
	}
	if !due {
		return
	}
	if prepared == nil {
		if r.idle() {
			r.prepare(want, policy)
		}
		return
	}
	if exited(prepared) {
		r.fail(policy, prepared, errACPReplacementStopped)
		return
	}
	if r.now().Sub(preparedAt) >= acpRefreshSettle && r.restart() {
		acpproxy.Trace("network refresh: replacing the idle box for %s", want.target.String())
	}
}

// prepare starts a parked replacement for want under the current policy.
func (r *acpNetworkRefresh) prepare(want acpSpawn, policy string) {
	child, err := r.spawn(want)
	if errors.Is(err, errACPAccountCooling) || errors.Is(err, context.Canceled) {
		return // not yet, or the session is ending
	}
	if err != nil {
		r.fail(policy, nil, err)
		return
	}
	r.mu.Lock()
	fits := !r.closed && r.prepared == nil && child.Policy == r.capture.Fingerprint && r.running != "" && r.running != child.Policy
	if fits {
		r.prepared, r.preparedFor, r.preparedAt = child, want, r.now()
	}
	r.mu.Unlock()
	if !fits {
		child.Stop()
	}
}

// fail keeps the live box: this policy's replacement is not tried again until the generation moves,
// and the reason is reported. A replacement the factory took or dropped meanwhile is no longer the
// refresh's: stopping it could stop the live box, and reporting it would announce a failure that did
// not happen.
func (r *acpNetworkRefresh) fail(policy string, child *acpproxy.Child, err error) {
	r.mu.Lock()
	if child != nil && r.prepared != child {
		r.mu.Unlock()
		return
	}
	r.failed = policy
	r.prepared = nil
	r.mu.Unlock()
	if child != nil {
		child.Stop()
	}
	r.report(err)
}

// take hands the factory the prepared replacement when it was started for exactly this selection
// under the current policy. A prepared box that no longer fits is stopped: nothing else can use it.
func (r *acpNetworkRefresh) take(spawn acpSpawn) *acpproxy.Child {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	child := r.prepared
	fits := child != nil && child.Policy == r.capture.Fingerprint && r.preparedFor.same(spawn)
	r.prepared = nil
	r.mu.Unlock()
	if child == nil {
		return nil
	}
	if !fits || exited(child) {
		child.Stop()
		return nil
	}
	if child.SetActive != nil {
		child.SetActive(true)
	}
	return child
}

// started records the box the factory hands the proxy, and tells the editor once when it is the first
// under a newly adopted policy.
func (r *acpNetworkRefresh) started(child *acpproxy.Child) {
	if r == nil {
		return
	}
	r.mu.Lock()
	moved := r.running != "" && child.Policy != r.running
	if moved {
		r.shown = ""
	}
	r.running = child.Policy
	r.mu.Unlock()
	if moved {
		r.notify(acpRefreshMoved)
	}
}

// report gives the agent log the whole reason and the editor one fixed sentence. Every approval on the
// host moves the one generation, so a refusal can repeat for another project's change; the same
// notice is not shown again until something else was.
func (r *acpNetworkRefresh) report(err error) {
	r.log(err)
	notice := acpRefreshNotice(err)
	r.mu.Lock()
	repeat := notice == r.shown
	r.shown = notice
	r.mu.Unlock()
	if !repeat {
		r.notify(notice)
	}
}

// close stops a replacement still waiting and releases every capture this refresh admitted; the
// startup capture belongs to its admitter. Call it once the refresh loop has ended.
func (r *acpNetworkRefresh) close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closed = true
	prepared, created := r.prepared, r.created
	r.prepared, r.created = nil, nil
	r.mu.Unlock()
	if prepared != nil {
		prepared.Stop()
	}
	for _, capture := range created {
		_ = capture.Close()
	}
}

func exited(child *acpproxy.Child) bool {
	if child.WaitDone == nil {
		return false
	}
	select {
	case <-child.WaitDone:
		return true
	default:
		return false
	}
}
