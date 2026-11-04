package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/acpproxy"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/ui"
)

const (
	// acpBoxWaitInterval is how often a waiting session looks for a sign that something changed: a
	// few stats. It is also how often a session whose box is ready tries to move off a holding child.
	acpBoxWaitInterval = 2 * time.Second
	// acpBoxWaitRecheck is how often a waiting session asks the whole question anyway. Undoing an edit
	// to a copied file leaves no cheaper sign, and the question costs a large project about a second.
	acpBoxWaitRecheck = 30 * time.Second
	// acpBoxWaitFresh is how old an answer a spawn may use, so a session that just asked does not
	// read the project's build files again to start its first box.
	acpBoxWaitFresh = 2 * time.Second
	// acpHandshakeEnv tells an inner child to run box.RunSpec.HandshakeOnly.
	acpHandshakeEnv = "COOP_ACP_HANDSHAKE_BOX"
	// acpBoxWaitMoveDeadline is how long a session waits for a holding child to finish what the editor
	// asked of it before replacing it anyway. A handshake takes seconds, so this is a hung adapter or a
	// sign-in nobody finished; a person finishing one has this long.
	acpBoxWaitMoveDeadline = 2 * time.Minute
	// acpBoxReadyNotice is what the editor shows once the session runs on the project's own box.
	acpBoxReadyNotice = "This project's box is ready, and this session moved onto it."
	// acpBoxStartFailedText is what a thread hears while real boxes keep exiting as they start, whatever
	// the reason: Coop's refusal, a broken sidecar, a Dockerfile that no longer fits. The child that
	// failed already wrote why to the agent log, so this need not, and need not know every reason.
	acpBoxStartFailedText = "⚠ **Coop could not start this project's box.**\n\nThe reason is in the agent log. Coop tries again every 30 seconds and whenever the project changes. Your messages wait here and go out once it is ready."
)

// errACPBoxWaiting keeps the warm pool empty while the session's boxes cannot start: nothing it parks
// could serve a switch.
var errACPBoxWaiting = errors.New("the project's box is not ready")

// acpHoldingChild reports that this process is an editor session's child that only holds prompts. Only
// a supervisor sets the flag: its children's environment is cleaned of an inherited one.
func acpHoldingChild() bool {
	return os.Getenv("COOP_ACP_INNER") != "" && os.Getenv(acpHandshakeEnv) == "1"
}

// acpBoxWait keeps an editor connected while its filtered project box cannot start. The session runs
// a child that only answers the handshake (acpproxy.Child.HoldPrompts) and shows why it waits. Files a
// person already approved rebuild in the background; anything else waits for that person, and the
// session notices their `coop build` or `coop approve` by itself. Once the box can start, the holding
// child is replaced and any held prompt goes out, with no reconnect.
//
// Concurrency: the session's factory and its poll loop both call refresh, one at a time; checking is
// taken before the network refresh's admitting. Fixed sentences only reach the editor, except a
// services refusal, whose text is Coop's own words and validated names. The whole reason goes to the
// agent log.
type acpBoxWait struct {
	check   func(context.Context) error         // box.EditorBoxReadiness under the session's current capture
	rebuild func(context.Context, string) error // box.RebuildApprovedProject
	stamp   func() string                       // box.EditorBoxStamp: when to ask again
	holding func() bool                         // the live child only holds prompts
	serving func() bool                         // the live child runs turns and has answered initialize
	move    func() bool                         // replace it, once it owes the editor nothing
	force   func() bool                         // replace it anyway
	publish func(string)                        // what threads hear now; "" when nothing waits
	notify  func(string)                        // shown once, with the next session update
	log     func(error)                         // the agent log
	opened  func()                              // real boxes can start again: the warm pool may fill
	now     func() time.Time                    // the clock; tests set it

	ctx    context.Context // ends the rebuild and the poll loop
	cancel context.CancelFunc
	kicks  chan struct{}
	done   sync.WaitGroup

	checking sync.Mutex // one check at a time; taken before mu

	moveWanted time.Time // when the poll loop first could not move; only the poll loop touches it

	mu          sync.Mutex
	blocked     error     // why a real box cannot start now; nil when it can
	checked     time.Time // when the question was last asked, whatever the answer
	building    bool      // a rebuild is running
	failed      bool      // the rebuild failed: what it could not do needs a person
	startFailed bool      // a real box exited as it started, and none has run since
	retrying    bool      // a real box is being tried again; startFailed clears once it keeps running
	retriedAt   time.Time // when that started
	said        string    // what publish was last told
	seen        string    // the stamp blocked was answered under
	onHold      bool      // the last child handed to the proxy holds prompts
	closed      bool      // no rebuild may start now

	closing sync.Once
}

// newACPBoxWait wires a filtered session's wait to its supervisor; nil for an open or offline session.
// spec names the box the session's boxes would run: its project, workspace and agent.
func (a *app) newACPBoxWait(ctrl *acpctl.Control, restarter *acpproxy.Restarter, spec box.RunSpec, opened func()) *acpBoxWait {
	if a.acpCapture == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &acpBoxWait{
		check: func(ctx context.Context) error {
			if err := a.acpRefresh.readmitHeld(); err != nil {
				return err
			}
			return box.EditorBoxReadiness(ctx, a.cfg, a.rt, a.currentACPCapture(), spec)
		},
		rebuild: func(ctx context.Context, dfRel string) error {
			build := spec
			build.Ctx, build.Quiet, build.Stderr = ctx, true, os.Stderr // stdout is the editor's wire
			return box.RebuildApprovedProject(a.cfg, a.rt, build, dfRel)
		},
		stamp:   func() string { return box.EditorBoxStamp(a.currentACPCapture(), spec) },
		holding: restarter.Holding,
		serving: restarter.Serving,
		move:    restarter.MoveOffHolding,
		force:   restarter.ForceOffHolding,
		publish: func(text string) {
			ctrl.SetBoxWait(text)
			if text != "" {
				restarter.TellHeld()
			}
		},
		notify: ctrl.Notify,
		log:    func(err error) { ui.Warn("%v", err) },
		opened: opened,
		now:    time.Now,
		ctx:    ctx, cancel: cancel, kicks: make(chan struct{}, 1),
	}
}

// blockedNow is the last answer: a child spawned now holds prompts if real boxes cannot start, or the
// last one exited as it started and it is not time to try another.
func (w *acpBoxWait) blockedNow() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.blocked != nil || w.startFailed && !w.retrying
}

// waitingNow says the session has not yet seen a real box keep running. The warm pool stays empty: a
// box parked now would be one more that cannot serve a switch, and a retry costs one box, not a fan-out.
func (w *acpBoxWait) waitingNow() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.blocked != nil || w.startFailed
}

// childFailed is the proxy saying a real box exited by itself as it started. Whatever the reason, it
// is in the agent log, and the next child holds prompts. Only a child that keeps running ends this,
// so a retry that fails again says nothing new.
func (w *acpBoxWait) childFailed() {
	if w == nil {
		return
	}
	w.mu.Lock()
	first := !w.startFailed
	w.startFailed, w.retrying, w.checked = true, false, w.now()
	text := w.said
	if w.blocked == nil {
		text = acpBoxStartFailedText
	}
	repeat := text == w.said
	w.said = text
	w.mu.Unlock()
	if first {
		w.log(errors.New("this project's box exited as it started, and the reason should be above. This session waits on a box that only holds messages, and tries again every 30 seconds and when the project changes"))
	}
	if !repeat {
		w.publish(text)
	}
}

// childFailedHook is childFailed for the proxy, which counts every failure itself with no wait.
func (w *acpBoxWait) childFailedHook() func() {
	if w == nil {
		return nil
	}
	return w.childFailed
}

// retryFailedStart lets the next spawn be a real box again, if nothing else stands in its way.
func (w *acpBoxWait) retryFailedStart() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.startFailed && !w.retrying && w.blocked == nil {
		w.retrying, w.retriedAt = true, w.now()
	}
}

// confirmStart ends a failed start once the box being tried again has answered and outlived the window
// in which an exit would still count as failing to start.
func (w *acpBoxWait) confirmStart() {
	w.mu.Lock()
	due := w.retrying && w.now().Sub(w.retriedAt) >= acpproxy.ChildStartWindow
	w.mu.Unlock()
	if !due || !w.serving() {
		return
	}
	w.mu.Lock()
	if !w.retrying { // it failed after all, a moment ago
		w.mu.Unlock()
		return
	}
	w.startFailed, w.retrying = false, false
	said := w.said
	w.said = ""
	w.mu.Unlock()
	if said != "" {
		w.publish("")
	}
	w.opened()
	w.notify(acpBoxReadyNotice)
}

// refresh asks whether a real box can start now, and acts on the answer. An error is a failure to
// look, which proves nothing: the last answer stands, and a child's own launch proofs still decide.
func (w *acpBoxWait) refresh(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.checking.Lock()
	defer w.checking.Unlock()
	stamp := w.stamp()
	err := w.check(ctx)
	var required *box.ProjectBuildRequired
	var services *box.ServicesNeedRules
	var pending *networkstate.PendingApproval
	if err != nil && !errors.As(err, &required) && !errors.As(err, &services) && !errors.As(err, &pending) && !errors.Is(err, errACPNetworkModeChanged) {
		w.mu.Lock()
		w.checked, w.seen = w.now(), stamp // not again until something changes: it costs a large project a second
		w.mu.Unlock()
		return err
	}
	w.settle(err, required, stamp)
	return nil
}

// refreshStale is refresh for a spawn: an answer this recent is still good.
func (w *acpBoxWait) refreshStale(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	fresh := !w.checked.IsZero() && w.now().Sub(w.checked) < acpBoxWaitFresh
	w.mu.Unlock()
	if fresh {
		return nil
	}
	return w.refresh(ctx)
}

func (w *acpBoxWait) settle(blocked error, required *box.ProjectBuildRequired, stamp string) {
	w.mu.Lock()
	cleared := w.blocked != nil && blocked == nil && !w.startFailed
	changed := blocked != nil && (w.blocked == nil || w.blocked.Error() != blocked.Error())
	w.blocked, w.checked, w.seen = blocked, w.now(), stamp
	if blocked == nil || required == nil || !required.Approved {
		w.failed = false
	}
	if required != nil && required.Approved && !w.building && !w.failed && !w.closed {
		w.building = true
		w.done.Add(1)
		go w.runRebuild(required.Dockerfile)
	}
	text := acpBoxWaitText(blocked, required, w.failed)
	if text == "" && w.startFailed {
		text = acpBoxStartFailedText
	}
	repeat := text == w.said
	w.said = text
	w.mu.Unlock()
	if changed {
		if required != nil && required.Approved {
			w.log(fmt.Errorf("rebuilding this project's box from the files you approved: %s", required.Reason))
		} else {
			w.log(blocked)
		}
	}
	if !repeat {
		w.publish(text)
	}
	if cleared {
		w.opened()
	}
}

// runRebuild runs the one background rebuild. Files that changed since the check are a new question
// for the next poll, and a session stopping is nobody's failure.
func (w *acpBoxWait) runRebuild(dfRel string) {
	defer w.done.Done()
	err := w.rebuild(w.ctx, dfRel)
	var changed *box.ProjectBuildRequired
	failed := err != nil && !errors.As(err, &changed) && w.ctx.Err() == nil
	if failed {
		w.log(err)
	}
	w.mu.Lock()
	w.building, w.failed = false, failed
	w.seen = "" // ask again at once
	w.mu.Unlock()
	w.kick()
}

// acpBoxWaitText is what a thread hears while its message waits. required is the miss when it is a
// project build; a services refusal carries its own remedy.
func acpBoxWaitText(blocked error, required *box.ProjectBuildRequired, failed bool) string {
	const waits = "Your messages wait here and go out once it is ready."
	var pending *networkstate.PendingApproval
	switch {
	case blocked == nil:
		return ""
	case errors.As(blocked, &pending):
		return acpctl.ProblemText(acpctl.Problem{Title: "This project's network access needs your approval.",
			Detail:  "Nobody approved what it asks for as it is now, so no agent runs here yet. " + waits,
			Command: "coop approve", Purpose: "in this project, on the host"})
	case errors.Is(blocked, errACPNetworkModeChanged):
		return acpctl.ProblemText(acpctl.Problem{Title: "Reconnect your editor.",
			Detail: "You approved network access this session cannot switch to, so no agent runs here. A message you sent stays unsent: send it again after you reconnect."})
	case required != nil && failed:
		return acpctl.ProblemText(acpctl.Problem{Title: "Coop could not rebuild this project's box.",
			Detail:  "The reason is in the agent log. Fix it, then build it. " + waits,
			Command: "coop build --egress filtered", Purpose: "in this project, on the host"})
	case required != nil && required.Approved:
		return "**Rebuilding this project's box.**\n\nYour approved build files did not change, so this needs nothing from you. " + waits
	case required != nil:
		return acpctl.ProblemText(acpctl.Problem{Title: "This project's box needs a build.",
			Detail:  "You have not approved its build files as they are now, and a build runs them with open networking, so Coop waits for you. " + waits + " Review the Dockerfile and the files it copies.",
			Command: "coop build --egress filtered", Purpose: "in this project, on the host"})
	default:
		return "⚠ **This project's box cannot start yet.**\n\n" + waits + "\n\n" + acpctl.CodeBlock(blocked.Error())
	}
}

func (w *acpBoxWait) kick() {
	select {
	case w.kicks <- struct{}{}:
	default:
	}
}

// poll asks again when a sign says something changed or it has been a while, and moves off a holding
// child once a real box can start. A failed start gets that same question, and its answer only says
// nothing else stands in the way: the next real box's own proofs are the only way to know.
func (w *acpBoxWait) poll() {
	w.mu.Lock()
	waiting, building, seen, checked := w.blocked != nil || w.startFailed && !w.retrying, w.building, w.seen, w.checked
	w.mu.Unlock()
	if waiting && !building && (w.stamp() != seen || w.now().Sub(checked) >= acpBoxWaitRecheck) {
		if err := w.refresh(w.ctx); err != nil && w.ctx.Err() == nil {
			acpproxy.Trace("box wait: could not look: %v", err)
		}
		w.retryFailedStart()
	}
	w.confirmStart()
	if w.blockedNow() || !w.holding() {
		w.moveWanted = time.Time{}
		return
	}
	switch now := w.now(); {
	case w.move():
		w.moveWanted = time.Time{}
	case w.moveWanted.IsZero():
		w.moveWanted = now
	case now.Sub(w.moveWanted) >= acpBoxWaitMoveDeadline:
		w.log(errors.New("the box holding this session's messages did not finish what the editor asked of it, so it was replaced anyway"))
		w.force()
		w.moveWanted = time.Time{}
	}
}

// started tells the editor once a real box has replaced a holding child, which is when that is true:
// a move asks for one, and the box it gets may turn out to be another holding child.
func (w *acpBoxWait) started(child *acpproxy.Child) {
	if w == nil {
		return
	}
	w.mu.Lock()
	moved := w.onHold && !child.HoldPrompts && !w.startFailed // after a failed start, confirmStart says so
	w.onHold = child.HoldPrompts
	w.mu.Unlock()
	if moved {
		w.notify(acpBoxReadyNotice)
	}
}

// start runs the poll loop until close. A nil wait starts nothing.
func (w *acpBoxWait) start() {
	if w == nil {
		return
	}
	w.done.Add(1)
	go func() {
		defer w.done.Done()
		ticker := time.NewTicker(acpBoxWaitInterval)
		defer ticker.Stop()
		for {
			select {
			case <-w.ctx.Done():
				return
			case <-ticker.C:
			case <-w.kicks:
			}
			w.poll()
		}
	}()
}

// close stops the poll loop and any rebuild and waits for them: a build runs in its own process group
// and must not outlive the session. Safe to call more than once, from any goroutine, on a nil wait.
func (w *acpBoxWait) close() {
	if w == nil {
		return
	}
	w.closing.Do(func() {
		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()
		w.cancel()
		w.done.Wait()
	})
}
