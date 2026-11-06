package box

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// serviceLaunchWaitNotice is how long a launch waits on the service launch lock before it says so.
// Another box between its two service steps lets go sooner than this, and that wait is nobody's news.
var serviceLaunchWaitNotice = 2 * time.Second

// LockServiceLaunch takes a workspace's service launch lock exclusively for a command a person
// watches, and says so if it has to wait. Only the boxes working in that workspace hold it.
func LockServiceLaunch(ctx context.Context, repo string) (func(), error) {
	return lockServiceLaunch(ctx, repo, true, RunSpec{}, nil)
}

// lockApprovalTransition is what an approval change that every box's mounts depend on takes, such as a
// project's first anchor: the main checkout's workspace lock, which a box from a build before the
// project lock holds too, and then the project lock, which every box holds, a fork's included. A box
// takes them in that order as well, so neither side can wait on the other for ever.
func lockApprovalTransition(ctx context.Context, repo string) (func(), error) {
	unlockWorkspace, err := lockServiceLaunch(ctx, repo, true, RunSpec{}, nil)
	if err != nil {
		return nil, err
	}
	unlockProject, err := lockProjectLaunch(ctx, repo, true, nil)
	if err != nil {
		unlockWorkspace()
		return nil, err
	}
	return func() {
		unlockProject()
		unlockWorkspace()
	}, nil
}

// lockServiceLaunch is forkspace.LockServiceLaunch for anyone a person is waiting on. A running box
// holds the shared side for its whole life, so the exclusive side can wait for hours, with no limit:
// whoever has to wait says for what, what to do about it, and when the wait ends. spec names the box
// that waits, if one does: its own record is not what it waits for, and a supervisor may want to know.
func lockServiceLaunch(ctx context.Context, repo string, exclusive bool, spec RunSpec, sections *launchSections) (func(), error) {
	return waitForLaunchLock(ctx, repo, false, exclusive, spec, sections)
}

// lockProjectLaunch is lockServiceLaunch for the project lock (forkspace.LockProjectLaunch).
func lockProjectLaunch(ctx context.Context, repo string, exclusive bool, sections *launchSections) (func(), error) {
	return waitForLaunchLock(ctx, repo, true, exclusive, RunSpec{}, sections)
}

func waitForLaunchLock(ctx context.Context, repo string, project, exclusive bool, spec RunSpec, sections *launchSections) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	take := forkspace.LockServiceLaunch
	if project {
		take = forkspace.LockProjectLaunch
	}
	var said *ServiceLaunchBusy
	var told, own bool
	waitOver, forgetWait := func() {}, func() {}
	defer func() {
		waitOver()
		forgetWait()
	}()
	for {
		step, cancel := context.WithTimeout(ctx, serviceLaunchWaitNotice)
		unlock, err := take(step, repo, exclusive)
		cancel()
		switch {
		case err == nil && said == nil:
			return unlock, nil
		case err == nil:
			if told {
				spec.OnServiceLaunchWait(false)
			}
			sections.serviceLaunchWaitOver()
			return unlock, nil
		case !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
			return nil, err
		}
		if said == nil && spec.activityID != "" {
			// coop tasks watch reads it beside the box's record: its task waits to start its services.
			waitOver = forkspace.NoteServiceLaunchWaiting(spec.ActivityRepo, spec.activityID)
		}
		// Only a service launch waits for running boxes. The shared side waits for whoever is starting
		// services, or for a command such as `coop up`, which keeps no record.
		busy, ownNow := &ServiceLaunchBusy{}, false
		if exclusive {
			busy, ownNow = serviceLaunchHolders(repo, project, spec.activityID, spec.ActivitySource)
		}
		// This wait has no limit and who it waits for can change: parked boxes stop and another session's
		// box takes the lock first, or the other way around. So it asks again for as long as it lasts.
		if said == nil || !slices.Equal(said.Boxes, busy.Boxes) {
			sections.serviceLaunchWait(busy, exclusive)
			forgetWait()
			forgetWait = spec.noteLaunchWait(busy)
		}
		first := said == nil
		said = busy
		if !exclusive || !first && own == ownNow {
			continue
		}
		own = ownNow
		// The session's own boxes are not something to ask a person to close.
		if want := !own; spec.OnServiceLaunchWait != nil && told != want {
			told = want
			spec.OnServiceLaunchWait(told)
		}
		if own && spec.OnOwnBoxesHoldLaunch != nil && spec.ActivityRole != forkspace.ExecutionRoleWarm {
			spec.OnOwnBoxesHoldLaunch()
		}
	}
}

// serviceStartRepo keys the service start barrier: the workspace a box's services start from, which its
// agent can change. Compose opens bind sources only there, and a fork has its own workspace and Compose
// project, so a start waits only for boxes that can replace paths in that workspace: a fork's never for
// a sibling fork's boxes, the main checkout's never for a fork's. Keyed by the parent project, four
// emisar forks starved each other for hours, and the main checkout's `coop up` waited for a fork's
// whole loop (2026-11-05). Approval changes take the project lock, which every box also holds
// (enterAuthorityMountWindow, lockApprovalTransition).
func serviceStartRepo(spec RunSpec, projectRepo string) string {
	if spec.Repo != "" {
		return spec.Repo
	}
	return projectRepo
}

// lockServiceStart is lockServiceLaunch for a box about to start its services. A supervisor stops a box
// that waits as the ordinary way to stop waiting, so that reads as a cancelled setup, not a failure.
func lockServiceStart(ctx context.Context, repo string, spec RunSpec, sections *launchSections) (func(), error) {
	unlock, err := lockServiceLaunch(ctx, repo, true, spec, sections)
	switch {
	case err == nil:
		return spec.noteLaunchHeld(repo, unlock), nil
	case errors.Is(err, context.Canceled):
		return nil, setupCanceled{step: "starting services", cause: err}
	}
	return nil, fmt.Errorf("wait for a safe service launch: %w", err)
}

// noteLaunchHeld says this box holds the lock that unlock releases, until it does, so that a wait can name
// it. The note goes with the box's record, in its project's, where a wait on any of the project's locks
// looks: noted with a fork's own records, a fork box starting services was never named. A box with no
// record has nothing to be named by.
func (spec RunSpec) noteLaunchHeld(repo string, unlock func()) func() {
	if spec.activityID == "" {
		return unlock
	}
	if spec.ActivityRepo != "" {
		repo = spec.ActivityRepo
	}
	forget := forkspace.NoteServiceLaunchHeld(repo, spec.activityID)
	return func() {
		forget()
		unlock()
	}
}

// noteLaunchWait says where `coop fork ls` looks that this fork's box waits, and for what, until the
// returned call. A wait shown only in the fork's log left the fork reading as a running loop for as long
// as it lasted (2026-11-05). A box outside a fork has no fork listing to tell.
func (spec RunSpec) noteLaunchWait(busy *ServiceLaunchBusy) func() {
	if spec.ForkName == "" || spec.ActivityRepo == "" {
		return func() {}
	}
	return forkspace.NoteLaunchWait(spec.ActivityRepo, spec.ForkName, busy.Boxes)
}

// ServiceLaunchBusy is a box that would have to wait for the service launch lock: Boxes are what run
// in the project now, in Coop's words, and none are named when only a command holds it. An editor
// session waits for it the way it waits for an approval.
type ServiceLaunchBusy struct{ Boxes []string }

func (e *ServiceLaunchBusy) Error() string {
	title, details := e.Say()
	return strings.ToLower(title[:1]) + title[1:] + ". " + strings.Join(details, " ")
}

// Say is what a person hears, in every place they might be looking: a title, then short sentences.
func (e *ServiceLaunchBusy) Say() (title string, details []string) {
	switch len(e.Boxes) {
	case 0:
		return "Waiting for another Coop command in this project", []string{"Coop goes on by itself once it finishes."}
	case 1:
		return "Waiting for another box in this project", []string{"Running now: " + e.Boxes[0] + ".",
			"Coop goes on by itself once it stops. Close it to go on sooner."}
	}
	return "Waiting for other boxes in this project", []string{"Running now: " + strings.Join(e.Boxes, ", ") + ".",
		"Coop goes on by itself once they stop. Close them to go on sooner."}
}

// serviceLaunchHolders says who keeps a launch out of repo's lock, or out of its project lock (project),
// as far as the host's records can tell: a running box other than except and the boxes of the launch's
// own session (source) that said it holds the lock. A workspace's lock is held only by boxes working
// there, so only those count; every box holds the project lock. A record alone says nothing: a controller only supervises, a box may be waiting for the
// lock too, and a box may only hold prompts, in this session or any other. Counting those made two
// sessions wait on each other. Of a session's own boxes only a parked one counts. own reports that parked
// boxes of its own session are all there is. An unreadable record names nobody; the wait is still real.
func serviceLaunchHolders(repo string, project bool, except, source string) (busy *ServiceLaunchBusy, own bool) {
	authority, _, err := forkspace.ResolveProjectBinding(repo)
	if err != nil {
		return &ServiceLaunchBusy{}, false
	}
	live, err := LiveBoxes(authority, except)
	if err != nil {
		return &ServiceLaunchBusy{}, false
	}
	var others []forkspace.ExecutionObservation
	for _, observation := range live {
		switch record := observation.Record; {
		case !forkspace.ServiceLaunchHeldBy(authority, record.ID):
		case !project && !sameDirectory(record.Workspace, repo):
		case source != "" && record.SourceID == source:
			own = own || record.Role == forkspace.ExecutionRoleWarm
		default:
			others = append(others, observation)
		}
	}
	return &ServiceLaunchBusy{Boxes: liveBoxNames(others)}, own && len(others) == 0
}

// EditorServiceLaunchBusy asks whether a box of this editor session (spec.SupervisorID) that has to start
// the project's services would wait for the lock now. It returns *ServiceLaunchBusy, or nil when the lock
// is free or only the session's parked boxes hold it: those never stop by themselves and nobody can close
// them, so a thread is not told to.
func EditorServiceLaunchBusy(spec RunSpec) error {
	return editorServiceLaunchBusy(spec, nil)
}

// editorServiceLaunchBusy is EditorServiceLaunchBusy for a box that may need only the shared side: joins,
// when set, runs the check its launch will make, under that side. Whoever holds the exclusive side keeps
// that launch out as well, so that wait is a real one. This is only a forecast: a box that turns out not to
// join says it waits (RunSpec.OnServiceLaunchWait).
func editorServiceLaunchBusy(spec RunSpec, joins func() bool) error {
	project, _, err := forkspace.ResolveProjectBinding(projectPolicyRepo(spec))
	if err != nil {
		return err
	}
	repo := serviceStartRepo(spec, project)
	taken, err := forkspace.ServiceLaunchTaken(repo)
	if err != nil || !taken {
		return err
	}
	busy, own := serviceLaunchHolders(repo, false, "", spec.SupervisorID)
	if own {
		return nil
	}
	if joins != nil {
		if unlock, err := tryServiceLaunchShared(repo); err == nil {
			defer unlock()
			if joins() {
				return nil
			}
		}
	}
	return busy
}

// tryServiceLaunchShared takes the shared side only if it needs no wait.
func tryServiceLaunchShared(repo string) (func(), error) {
	once, cancel := context.WithCancel(context.Background())
	cancel() // flockWaiting tries once before it looks at its context
	return forkspace.LockServiceLaunch(once, repo, false)
}

// serviceLaunchStamp changes when a waiting session should ask again about the lock: it came free, whoever
// was starting services finished, so a box could join them, or someone else holds it now.
func serviceLaunchStamp(repo, source string) string {
	taken, _ := forkspace.ServiceLaunchTaken(repo) // a failure to look reads as free: only a hint
	if !taken {
		return "free"
	}
	starting := true
	if unlock, err := tryServiceLaunchShared(repo); err == nil {
		unlock()
		starting = false
	}
	busy, own := serviceLaunchHolders(repo, false, "", source)
	return fmt.Sprint(starting, own, busy.Boxes)
}

// EditorUnfilteredBoxReadiness is EditorBoxReadiness for an open or offline session: a box of its own
// has nothing to wait for but the lock, and that only if it starts the project's services.
func EditorUnfilteredBoxReadiness(ctx context.Context, cfg *config.Config, rt runtime.Runtime, spec RunSpec) error {
	p, err := project.Load(projectPolicyRepo(spec))
	if err != nil {
		return err
	}
	file := ComposeFileAt(spec.Repo, p.ComposeRel())
	if file == "" || !EditorStartsServices(cfg, rt, p) {
		return nil
	}
	return editorServiceLaunchBusy(spec, func() bool {
		child := spec
		child.ActivitySource = spec.SupervisorID
		roots := compositionArtifactExposureRoots(applyProjectPolicy(cfg, p, &child), child)
		_, joined := openSessionServicesRunning(ctx, rt, child, file, child.ActivitySource, roots...)
		return joined
	})
}

// serviceLaunchWait opens the wait where the launch's narration is: a section of its own in a
// terminal, nested under the services a loop is starting, and plain lines for everyone else, since a
// wait with no limit is not routine progress a quiet run may hide. An editor child's plain lines reach
// the agent log, and its thread hears through RunSpec.OnServiceLaunchWait.
func (s *launchSections) serviceLaunchWait(busy *ServiceLaunchBusy, exclusive bool) {
	title, details := busy.Say()
	switch {
	case s == nil || !s.on:
		ui.Note("%s.", title)
		for _, detail := range details {
			ui.Note("%s", detail)
		}
	case s.loop:
		if exclusive {
			s.section("Starting services")
		}
		ui.Note("  %s", title)
		for _, detail := range details {
			loopLaunchDetail(detail, 4)
		}
	default:
		s.section(title)
		for _, detail := range details {
			ui.Note("  %s", detail)
		}
	}
}

func (s *launchSections) serviceLaunchWaitOver() {
	if s == nil || !s.on {
		ui.Note("The wait is over.")
		return
	}
	ui.Pass("The wait is over")
}
