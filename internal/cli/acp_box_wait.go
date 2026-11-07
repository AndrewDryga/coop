package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/acpproxy"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
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
	// acpLaunchedEnv names the file an inner child creates when its box starts (acpReportLaunch).
	acpLaunchedEnv = "COOP_ACP_LAUNCHED"
	// acpBoxWaitMoveDeadline is how long a session waits for a holding child to finish what the editor
	// asked of it before replacing it anyway. A handshake takes seconds, so this is a hung adapter or a
	// sign-in nobody finished; a person finishing one has this long.
	acpBoxWaitMoveDeadline = 2 * time.Minute
	// acpLaunchWaitEnv names the descriptor an inner child tells its supervisor on that it waits for the
	// service launch lock. Its stdout is the editor's wire and its stderr carries what the box writes,
	// and neither may decide what a session does next.
	acpLaunchWaitEnv = "COOP_ACP_LAUNCH_WAIT_FD"
	// acpBoxReadyNotice is what the editor shows once the session runs on the project's own box.
	acpBoxReadyNotice = "This project's box is ready, and this session moved onto it."
	// acpBoxNotStarting titles what a thread hears while something has to settle before a box can start.
	acpBoxNotStarting = "This project's box cannot start yet."
	// acpBrokenFileReasonLimit is how much of why the project's file does not load a thread shows, in
	// characters: a yaml error or two, with their line numbers.
	acpBrokenFileReasonLimit = 600
)

// errACPBoxWaiting keeps the warm pool empty while the session's boxes cannot start: nothing it parks
// could serve a switch.
var errACPBoxWaiting = errors.New("the project's box is not ready")

// acpHoldingChild reports that this process is an editor session's child that only holds prompts. Only
// a supervisor sets the flag: its children's environment is cleaned of an inherited one.
func acpHoldingChild() bool {
	return os.Getenv("COOP_ACP_INNER") != "" && os.Getenv(acpHandshakeEnv) == "1"
}

// acpReportLaunch makes a filtered child tell its supervisor when its box is about to start: the proxy
// allows a box that has started a minute to answer, and everything before this point much longer. A
// supervisor sets the path, and a failed write only leaves the box under the longer limit.
func acpReportLaunch(spec *box.RunSpec) {
	path := os.Getenv(acpLaunchedEnv)
	if os.Getenv("COOP_ACP_INNER") == "" || path == "" {
		return
	}
	spec.OnRuntimeLaunch = func() { _ = os.WriteFile(path, nil, 0o600) }
}

// acpBoxWait keeps an editor connected while its filtered project box cannot start. The session runs
// a child that only answers the handshake (acpproxy.Child.HoldPrompts) and shows why it waits. Files a
// person already approved rebuild in the background; anything else waits for that person, and the
// session notices their `coop build`, their `coop approve` or their fixed project file by itself. Once
// the box can start, the holding child is replaced and any held prompt goes out, with no reconnect. An
// open or offline session has one thing to wait for: other boxes that keep its own from starting the
// project's services.
//
// Concurrency: the session's factory and its poll loop both call refresh, one at a time; checking is
// taken before the network refresh's admitting. Fixed sentences only reach the editor, except a
// services refusal, whose text is Coop's own words and validated names, and why a project's file does
// not load, escaped and cut short. The whole reason goes to the agent log.
type acpBoxWait struct {
	check   func(context.Context) error         // box.EditorBoxReadiness under the session's current capture
	rebuild func(context.Context, string) error // box.RebuildApprovedProject
	stamp   func() string                       // box.EditorBoxStamp: when to ask again
	// prepareBuild saves the build files for review (box.PrepareFilteredProjectBuild), and buildReviewed
	// builds such a snapshot. A thread offers them only in a filtered session (heldCommand).
	prepareBuild  func(context.Context) (acpPreparedBuild, error)
	buildReviewed func(context.Context, acpPreparedBuild) error
	holding       func() bool  // the live child only holds prompts
	serving       func() bool  // the live child runs turns and has answered initialize
	move          func() bool  // replace it, once it owes the editor nothing
	force         func() bool  // replace it anyway
	publish       func(string) // what threads hear now; "" when nothing waits
	notify        func(string) // shown once, with the next session update
	// announce is publish for a wait that can begin with every thread idle: each open one hears it now.
	announce func(string)
	lockBusy func() error // box.EditorServiceLaunchBusy: the lock alone, which costs a few stats
	replace  func() bool  // replace a child that has answered nothing, since it waits to start
	log      func(error)  // the agent log
	opened   func()       // the session's own box answered: the warm pool may fill
	yield    func()       // stop the session's parked boxes: one of its own needs the lock they hold
	// unblocked runs when the wait clears, before the box moved onto starts. Nil if nothing depends on that.
	unblocked func()
	// filtered: a real box that keeps failing also holds prompts. Others keep the proxy's rapid-fail rule.
	filtered bool
	now      func() time.Time // the clock; tests set it

	ctx    context.Context // ends the rebuild and the poll loop
	cancel context.CancelFunc
	kicks  chan struct{}
	done   sync.WaitGroup

	checking sync.Mutex // one check at a time; taken before mu

	moveWanted time.Time // when the poll loop first could not move; only the poll loop touches it

	mu           sync.Mutex
	blocked      error                 // why a real box cannot start now; nil when it can
	checked      time.Time             // when the question was last asked, whatever the answer
	building     bool                  // a rebuild is running
	failed       bool                  // the rebuild failed: what it could not do needs a person
	startFailed  bool                  // a real box could not start, and none has run since
	failedHow    acpproxy.ChildFailure // how the last one failed
	retrying     bool                  // a real box is being tried again; startFailed clears once it keeps running
	retriedAt    time.Time             // when that started
	moving       bool                  // real boxes can start, and the session's own has not answered yet
	said         string                // what publish was last told
	seen         string                // the stamp blocked was answered under
	onHold       bool                  // the last child handed to the proxy holds prompts
	closed       bool                  // no rebuild may start now
	offer        *acpBuildOffer        // the snapshot a thread's /build saved, and its code
	reviewFailed bool                  // the last build of a reviewed snapshot failed
	reviewing    bool                  // the running build is a reviewed snapshot's, not an approved rebuild

	live          *acpproxy.Child          // the last child handed to the proxy
	launchWaits   map[*acpproxy.Child]bool // real children that say they wait for the lock now, parked or not
	launchWaitNew bool                     // the live one said so, and the poll loop has not acted on it

	closing sync.Once
}

// newACPBoxWait wires a session's wait to its supervisor. spec names the box the session's boxes would
// run: its project, workspace, agent and supervisor.
func (a *app) newACPBoxWait(ctrl *acpctl.Control, restarter *acpproxy.Restarter, spec box.RunSpec, opened, yield func()) *acpBoxWait {
	ctx, cancel := context.WithCancel(context.Background())
	return &acpBoxWait{
		filtered: a.acpCapture != nil,
		check: func(ctx context.Context) error {
			if err := a.acpRefresh.readmitHeld(); err != nil {
				return err
			}
			capture := a.currentACPCapture()
			if capture == nil {
				return box.EditorUnfilteredBoxReadiness(ctx, a.cfg, a.rt, spec)
			}
			return box.EditorBoxReadiness(ctx, a.cfg, a.rt, capture, spec)
		},
		lockBusy: func() error { return box.EditorServiceLaunchBusy(spec) },
		rebuild: func(ctx context.Context, dfRel string) error {
			build := spec
			build.Ctx, build.Quiet, build.Stderr = ctx, true, os.Stderr // stdout is the editor's wire
			return box.RebuildApprovedProject(a.cfg, a.rt, build, dfRel)
		},
		stamp: func() string { return box.EditorBoxStamp(a.currentACPCapture(), spec) },
		prepareBuild: func(ctx context.Context) (acpPreparedBuild, error) {
			return box.PrepareFilteredProjectBuild(ctx, a.cfg, spec)
		},
		buildReviewed: func(ctx context.Context, prepared acpPreparedBuild) error {
			build, ok := prepared.(*box.PreparedProjectBuild)
			if !ok {
				return errors.New("the reviewed project build is no longer available")
			}
			rt, err := a.rt.FreezeCompose(ctx)
			if err != nil {
				return err
			}
			return build.Build(ctx, rt, a.cfg, os.Stderr, os.Stderr) // stdout is the editor's wire
		},
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
		announce: func(text string) {
			ctrl.SetBoxWait(text)
			restarter.TellOpen(text)
		},
		replace: restarter.ReplaceUnstarted,
		notify:  ctrl.Notify,
		log:     func(err error) { ui.Warn("%s", safeErrorText(err)) }, // a project's file can be quoted in it
		opened:  opened,
		yield:   yield,
		now:     time.Now,
		ctx:     ctx, cancel: cancel, kicks: make(chan struct{}, 1),
	}
}

// blockedNow is the last answer: a child spawned now holds prompts if real boxes cannot start, or the
// last one could not and it is not time to try another.
func (w *acpBoxWait) blockedNow() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.blocked != nil || w.startFailed && !w.retrying
}

// waitingNow says the session has not yet seen a real box keep running. The warm pool stays empty: a
// box parked now would be one more that cannot serve a switch, a retry costs one box, not a fan-out,
// and a box being moved onto must reach a project's services before any other (confirmMove).
func (w *acpBoxWait) waitingNow() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.blocked != nil || w.startFailed || w.moving
}

// childFailed is the proxy saying a real box could not start: it exited by itself, or the proxy stopped
// it for not answering. The next child holds prompts. Only a child that keeps running ends this, so a
// retry that fails the same way again says nothing new.
func (w *acpBoxWait) childFailed(how acpproxy.ChildFailure) {
	if w == nil {
		return
	}
	w.mu.Lock()
	first := !w.startFailed || w.failedHow != how
	w.startFailed, w.failedHow, w.retrying, w.moving, w.checked = true, how, false, false, w.now() // confirmStart opens the pool now
	text := w.said
	if w.blocked == nil {
		text = acpBoxFailedText(how)
	}
	repeat := text == w.said
	w.said = text
	w.mu.Unlock()
	if first {
		w.log(acpBoxFailedLog(how))
	}
	if !repeat {
		w.publish(text)
	}
}

// childFailedHook is childFailed for the proxy, which counts every failure itself with no wait. That
// includes an open or offline session, whose wait is only for the lock.
func (w *acpBoxWait) childFailedHook() func(acpproxy.ChildFailure) {
	if w == nil || !w.filtered {
		return nil
	}
	return w.childFailed
}

// acpBoxFailedText is what a thread hears while real boxes keep failing this way. A child that exits
// already wrote why to the agent log, whatever the reason: Coop's refusal, a broken sidecar, a Dockerfile
// that no longer fits. So this need not, and need not know every reason. A child that does not answer
// wrote nothing, so the thread hears what Coop saw, and since such a try costs up to the time it waited,
// no number of seconds between tries.
func acpBoxFailedText(how acpproxy.ChildFailure) string {
	const keepsTrying = "Coop keeps trying, and retries right away when the project changes. Your messages go out once it is ready."
	switch how {
	case acpproxy.ChildSilent:
		return acpctl.ProblemText(acpctl.Problem{Title: "This project's box did not answer.",
			Detail: "It started, but the agent inside said nothing for a minute, so Coop stopped it. Anything it printed is in the agent log. " + keepsTrying})
	case acpproxy.ChildNotReady:
		return acpctl.ProblemText(acpctl.Problem{Title: "This project's box took too long to start.",
			Detail: "Coop stopped it after 5 minutes of getting ready. It may have been waiting for another box in this project, pulling images or starting services. " + keepsTrying})
	}
	return acpctl.ProblemText(acpctl.Problem{Title: "Coop could not start this project's box.",
		Detail: "The reason is in the agent log. Coop tries again every 30 seconds and when the project changes. Your messages go out once it is ready."})
}

// acpBoxFailedLog is what the agent log hears the first time.
func acpBoxFailedLog(how acpproxy.ChildFailure) error {
	const waits = "Messages are held while Coop keeps trying"
	switch how {
	case acpproxy.ChildSilent:
		return errors.New("this project's box started, but the agent inside did not answer the editor for a minute, so it was stopped. " + waits)
	case acpproxy.ChildNotReady:
		return errors.New("this project's box was still getting ready after 5 minutes, so it was stopped. It may have been waiting for another box in this project, pulling images or starting services. " + waits)
	}
	return errors.New("this project's box exited as it started; the reason should be above. Messages are held, and Coop tries again every 30 seconds and when the project changes")
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

// confirmMove opens the warm pool once the box the session waited for has answered. Any sooner, a warm
// box can reach the service launch lock first and hold it shared while it stays parked, and the
// session's own box then waits on that lock with its messages unsent.
func (w *acpBoxWait) confirmMove() {
	w.mu.Lock()
	due := w.moving && w.blocked == nil
	w.mu.Unlock()
	if !due || !w.serving() {
		return
	}
	w.mu.Lock()
	moved := w.moving && w.blocked == nil
	if moved {
		w.moving = false
	}
	w.mu.Unlock()
	if moved {
		w.opened()
	}
}

// awaitAnswer keeps the warm pool shut until the box just spawned has answered, as a move does. It is
// for a project with services: that box may have to start them, and a warm box must not get there first.
func (w *acpBoxWait) awaitAnswer() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.moving = true
}

// warmTaken is awaitAnswer for a parked box an open session just took, in a project with services. One
// that joined them has started, answers at once, and the refill joins too. One that has not started
// could not join and waits on the lock: its neighbours may hold it and never stop by themselves, and a
// parked box does not ask (box.RunSpec.OnOwnBoxesHoldLaunch), so they stop here. Either way the refill
// waits for the answer, so it cannot get to the lock first.
func (w *acpBoxWait) warmTaken(child *acpproxy.Child) {
	if w == nil {
		return
	}
	w.awaitAnswer()
	if child.Launched != nil && !child.Launched() {
		go w.yield()
	}
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
	var busy *box.ServiceLaunchBusy
	if err != nil && !errors.As(err, &required) && !errors.As(err, &services) && !errors.As(err, &busy) && acpHeldAnswer(err) == nil && !errors.Is(err, box.ErrProjectNotAdmitted) {
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
	w.moving = w.moving || cleared
	if blocked == nil || required == nil || !required.Approved {
		w.failed = false
	}
	if required != nil && required.Approved && !w.building && !w.failed && !w.closed {
		w.building = true
		w.done.Add(1)
		go w.runRebuild(required.Dockerfile)
	}
	text := acpBoxWaitText(blocked, required, w.failed)
	// Build files a person can still review and build. Refused ones are not: the same files would be
	// refused again, so there is nothing to offer and any saved copy of them is let go.
	buildable := required != nil && !required.Approved && !required.Refused
	switch {
	case text == "" && w.startFailed:
		text = acpBoxFailedText(w.failedHow)
	case buildable && w.building && w.reviewing:
		text = acpReviewedBuildText
	case buildable && w.building:
		text = acpRebuildingText
	case buildable && w.filtered && w.prepareBuild != nil:
		text = acpBuildableWaitText(w.reviewFailed, required.BuildCommand())
	}
	var stale *acpBuildOffer
	if !buildable {
		stale, w.offer, w.reviewFailed = w.offer, nil, false
	}
	repeat := text == w.said
	w.said = text
	w.mu.Unlock()
	if stale != nil {
		_ = stale.prepared.Close()
	}
	if changed {
		if required != nil && required.Approved {
			w.log(fmt.Errorf("rebuilding this project's box from the files you approved: %s", required.Reason))
		} else {
			w.log(blocked)
		}
	}
	var busy *box.ServiceLaunchBusy
	switch {
	case repeat:
	case errors.As(blocked, &busy):
		w.announce(text)
	default:
		w.publish(text)
	}
	// A session on hold for the lock has no use for its parked boxes, which a spawn would refuse anyway, and
	// they hold what it waits for. Two sessions that kept theirs would wait on each other for good.
	if errors.As(blocked, &busy) {
		go w.yield()
	}
	if cleared && w.unblocked != nil {
		w.unblocked()
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
	const waits = "Your messages go out once it is ready."
	var broken *box.BrokenProjectFile
	var pending *networkstate.PendingApproval
	var services *box.ServicesNeedRules
	var busy *box.ServiceLaunchBusy
	switch {
	case blocked == nil:
		return ""
	case errors.As(blocked, &broken):
		return acpctl.ProblemText(acpctl.Problem{Title: "This project's " + project.File + " has a problem.",
			Detail: "Nothing in that file applies, so no agent runs here yet. Fix the file, and this session moves on by itself. " + waits}) +
			"\n\nWhat it says: " + acpctl.MarkdownText(brokenFileReason(broken))
	case errors.Is(blocked, box.ErrProjectNotAdmitted):
		return acpBoxFailedText(acpproxy.ChildExited)
	case errors.As(blocked, &busy):
		title, details := busy.Say()
		return "**" + title + ".**\n\n" + strings.Join(details, "\n\n") + "\n\nYour messages go out once this session's box is ready."
	case errors.As(blocked, &pending):
		return acpctl.ProblemText(acpctl.Problem{Title: "This project's network access needs your approval.",
			Detail:  "No agent runs here until you approve it. " + waits,
			Command: "coop approve", Purpose: "in this project, on the host"})
	case errors.Is(blocked, errACPNetworkModeChanged):
		return acpctl.ProblemText(acpctl.Problem{Title: "Reconnect your editor.",
			Detail: "This session cannot switch to the network access this project now uses, so no agent runs here. A message you sent stays unsent: send it again after you reconnect."})
	case required != nil && required.Refused:
		// The reason `coop run` gives. It can quote the project's files: line breaks read as spaces
		// here, and ProblemText makes the rest literal.
		why := strings.TrimSuffix(strings.Join(strings.Fields(required.Reason), " "), ".")
		return acpctl.ProblemText(acpctl.Problem{Title: "Coop refused the last build of this project's box.",
			Detail:  "Why: " + why + ". After you fix it, build it again. " + waits,
			Command: required.BuildCommand(), Purpose: "in this project, on the host"})
	case required != nil && failed:
		return acpctl.ProblemText(acpctl.Problem{Title: "Coop could not rebuild this project's box.",
			Detail:  "The reason is in the agent log. Fix it, then build it. " + waits,
			Command: required.BuildCommand(), Purpose: "in this project, on the host"})
	case required != nil && required.Approved:
		return acpRebuildingText
	case required != nil:
		return acpctl.ProblemText(acpctl.Problem{Title: "This project's box needs a build.",
			Detail:  "Nobody approved its build files as they are now, and a build runs them with open networking. Review the Dockerfile and the files it copies. " + waits,
			Command: required.BuildCommand(), Purpose: "in this project, on the host"})
	case errors.As(blocked, &services):
		return acpServicesWaitText(services, waits)
	default: // refresh lets nothing else through, and only Coop's own words may reach a thread
		return acpctl.ProblemText(acpctl.Problem{Title: acpBoxNotStarting, Detail: "The reason is in the agent log. " + waits})
	}
}

// acpRebuildingText is what threads hear while Coop rebuilds from build files a person approved before.
const acpRebuildingText = "**Rebuilding this project's box.**\n\nYour approved build files did not change, so this needs nothing from you. Your messages go out once it is ready."

// acpServicesWaitText shows a services refusal so that it reads whole at any panel width. An editor does
// not wrap a code block, so a sentence in one runs off the panel's edge. Coop knows the rules and the
// real decision is the review, so a thread sends the person to `coop approve` rather than to their file.
func acpServicesWaitText(services *box.ServicesNeedRules, waits string) string {
	blocks := []string{acpctl.ProblemText(acpctl.Problem{Title: acpBoxNotStarting, Detail: acpSentence(services.Reason) + " " + waits})}
	if services.Rules != "" {
		intro := "Run this in this project, on the host, to see the rules to add:"
		if len(services.Offer()) > 0 {
			intro = "Run this in this project, on the host. It lists the services and offers to add their rules:"
		}
		blocks = append(blocks, intro, acpctl.CodeBlock("coop approve"))
	}
	return strings.Join(append(blocks, acpctl.MarkdownText(acpSentence(services.TurnOff))), "\n\n")
}

// acpSentence makes a part of a refusal stand on its own: a thread has no "cannot start because" for it
// to follow.
func acpSentence(part string) string {
	if part == "" {
		return ""
	}
	return strings.ToUpper(part[:1]) + strings.TrimSuffix(part[1:], ".") + "."
}

// brokenFileReason is what a thread shows of why the project's file does not load. An error can quote
// the file, which anyone who can edit the project wrote and which can be a megabyte, so it is cut short
// here, and goes whole to the agent log. yaml's errors span lines, which MarkdownText would show as
// escape codes, so it is one line of words; the caller makes those literal with MarkdownText.
func brokenFileReason(err error) string {
	reason := []rune(strings.Join(strings.Fields(safeErrorText(err)), " "))
	if len(reason) > acpBrokenFileReasonLimit {
		return string(reason[:acpBrokenFileReasonLimit]) + "…"
	}
	return string(reason)
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
	w.heedLaunchWait()
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
	w.confirmMove()
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
	w.live = child
	waits := w.launchWaits[child] // a parked box handed over while it waits: now it is this session's wait
	w.launchWaitNew = w.launchWaitNew || waits
	w.mu.Unlock()
	if moved {
		w.notify(acpBoxReadyNotice)
	}
	if waits {
		w.kick()
	}
}

// follow hears what child says about the service launch lock until it exits. Only the session's own
// child waits for anyone: a parked box's wait is nobody's news until the session takes that box.
func (w *acpBoxWait) follow(child *acpproxy.Child, word io.ReadCloser) {
	go func() {
		defer func() { _ = word.Close() }()
		b := make([]byte, 1)
		for {
			_, err := word.Read(b)
			if err == nil && b[0] == acpLaunchOwnBoxes {
				go w.yield()
				continue
			}
			waiting := err == nil && b[0] == acpLaunchWaiting
			w.mu.Lock()
			if w.launchWaits == nil {
				w.launchWaits = map[*acpproxy.Child]bool{}
			}
			if waiting {
				w.launchWaits[child] = true
			} else {
				delete(w.launchWaits, child)
			}
			heed := waiting && child == w.live
			w.launchWaitNew = w.launchWaitNew || heed
			w.mu.Unlock()
			if heed {
				w.kick()
			}
			if err != nil {
				return
			}
		}
	}()
}

// heedLaunchWait acts on the session's child saying it waits for the lock, however it came to: the check
// before it started saw a free lock, or the box was parked. It answers nothing while it waits, so with no
// thread yet the editor shows no more than a spinner, and a replacement would end the session once
// acpproxy's replay grace ran out. So the threads hear why and a child that holds prompts takes over,
// and poll moves off it when the lock comes free. If the lock is free again or only the session's own
// boxes hold it, it has nobody to wait for that a thread could do anything about, and it keeps trying.
func (w *acpBoxWait) heedLaunchWait() {
	w.mu.Lock()
	heard := w.launchWaitNew
	w.launchWaitNew = false
	w.mu.Unlock()
	if !heard {
		return
	}
	w.checking.Lock()
	defer w.checking.Unlock()
	stamp := w.stamp()
	var busy *box.ServiceLaunchBusy
	if !errors.As(w.lockBusy(), &busy) {
		// What it said no longer shields it from the first-answer limit: a wait on the session's own
		// parked boxes never ends by itself, and with no thread told, the limit is what ends it.
		w.mu.Lock()
		waits := w.launchWaits[w.live]
		delete(w.launchWaits, w.live)
		w.mu.Unlock()
		if waits {
			go w.yield() // it was parked beside the box it now replaces, and its neighbours hold the lock
		}
		return
	}
	w.settle(busy, nil, stamp)
	w.replace()
}

// waitsForLock reports that child said it waits for the lock, and nothing has answered that yet.
func (w *acpBoxWait) waitsForLock(child *acpproxy.Child) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.launchWaits[child]
}

// What a child says on acpLaunchWaitEnv: one byte for each change.
const (
	acpLaunchWaiting  = 'w'
	acpLaunchDone     = 'd'
	acpLaunchOwnBoxes = 'o' // the session's parked boxes are all that keep it out
)

// acpLaunchWaitReporter is how an inner child tells its supervisor about the lock, nil if it did not ask:
// box.RunSpec.OnServiceLaunchWait and box.RunSpec.OnOwnBoxesHoldLaunch.
func acpLaunchWaitReporter() (func(waiting bool), func()) {
	fd, err := strconv.Atoi(os.Getenv(acpLaunchWaitEnv))
	if err != nil || fd < 3 {
		return nil, nil
	}
	syscall.CloseOnExec(fd) // docker has no use for it, and would hold it open after this child is gone
	// Write by number, never through an *os.File: the descriptor is inherited for the process's life,
	// and a file wrapping it would close it from its finalizer once these funcs are dropped, closing
	// whatever file has reused that number by then.
	word := func(b byte) {
		for {
			// A supervisor that stopped listening is stopping this child too, so other errors are dropped.
			if _, err := syscall.Write(fd, []byte{b}); err != syscall.EINTR {
				return
			}
		}
	}
	return func(waiting bool) {
		if waiting {
			word(acpLaunchWaiting)
		} else {
			word(acpLaunchDone)
		}
	}, func() { word(acpLaunchOwnBoxes) }
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
		w.dropOffer()
	})
}
