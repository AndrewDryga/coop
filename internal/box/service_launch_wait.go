package box

import (
	"context"
	"errors"
	"fmt"
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

// LockServiceLaunch takes the project's service launch lock exclusively for a command a person
// watches, and says so if it has to wait.
func LockServiceLaunch(ctx context.Context, repo string) (func(), error) {
	return lockServiceLaunch(ctx, repo, true, RunSpec{}, nil)
}

// lockServiceLaunch is forkspace.LockServiceLaunch for anyone a person is waiting on. A running box
// holds the shared side for its whole life, so the exclusive side can wait for hours, with no limit:
// whoever has to wait says for what, what to do about it, and when the wait ends. spec names the box
// that waits, if one does: its own record is not what it waits for, and a supervisor may want to know.
func lockServiceLaunch(ctx context.Context, repo string, exclusive bool, spec RunSpec, sections *launchSections) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	first, cancel := context.WithTimeout(ctx, serviceLaunchWaitNotice)
	unlock, err := forkspace.LockServiceLaunch(first, repo, exclusive)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return unlock, err
	}
	// Only a service launch waits for running boxes. The shared side waits for whoever is starting
	// services, or for a command such as `coop up`, which keeps no record.
	busy, own := &ServiceLaunchBusy{}, false
	if exclusive {
		busy, own = serviceLaunchHolders(repo, spec.activityID, spec.ActivitySource)
	}
	sections.serviceLaunchWait(busy, exclusive)
	// The session's own boxes are not something to ask a person to close.
	tell := spec.OnServiceLaunchWait != nil && exclusive && !own
	if tell {
		spec.OnServiceLaunchWait(true)
	}
	if exclusive && own && spec.OnOwnBoxesHoldLaunch != nil && spec.ActivityRole != forkspace.ExecutionRoleWarm {
		spec.OnOwnBoxesHoldLaunch()
	}
	unlock, err = forkspace.LockServiceLaunch(ctx, repo, exclusive)
	if err != nil {
		return nil, err
	}
	if tell {
		spec.OnServiceLaunchWait(false)
	}
	sections.serviceLaunchWaitOver()
	return unlock, nil
}

// lockServiceStart is lockServiceLaunch for a box about to start its services. A supervisor stops a box
// that waits as the ordinary way to stop waiting, so that reads as a cancelled setup, not a failure.
func lockServiceStart(ctx context.Context, repo string, spec RunSpec, sections *launchSections) (func(), error) {
	unlock, err := lockServiceLaunch(ctx, repo, true, spec, sections)
	switch {
	case err == nil:
		return unlock, nil
	case errors.Is(err, context.Canceled):
		return nil, setupCanceled{step: "starting services", cause: err}
	}
	return nil, fmt.Errorf("wait for a safe service launch: %w", err)
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

// serviceLaunchHolders says who keeps a launch out of the lock, as far as the host's records can
// tell: a running box other than except and the boxes of the launch's own session (source). A
// controller only supervises, so its boxes speak for it, and of a session's own boxes only a parked
// one can hold the lock: the one that waits does not, nor does one that only holds prompts. own reports
// that parked boxes of its own session are all there is. An unreadable record names nobody; the wait
// is still real.
func serviceLaunchHolders(repo, except, source string) (busy *ServiceLaunchBusy, own bool) {
	live, err := LiveBoxes(repo, except)
	if err != nil {
		return &ServiceLaunchBusy{}, false
	}
	var others []forkspace.ExecutionObservation
	for _, observation := range live {
		switch record := observation.Record; {
		case record.Role == forkspace.ExecutionRoleController || record.Role == forkspace.ExecutionRoleDetachedWorker:
		case source != "" && record.SourceID == source:
			own = own || record.Role == forkspace.ExecutionRoleWarm
		default:
			others = append(others, observation)
		}
	}
	return &ServiceLaunchBusy{Boxes: liveBoxNames(others)}, own && len(others) == 0
}

// EditorServiceLaunchBusy asks whether a box of this editor session (spec.SupervisorID) would wait for
// the lock now. It returns *ServiceLaunchBusy, or nil when the lock is free or only the session's parked
// boxes hold it: those never stop by themselves and nobody can close them, so a thread is not told to.
func EditorServiceLaunchBusy(spec RunSpec) error {
	repo, _, err := forkspace.ResolveProjectBinding(projectPolicyRepo(spec))
	if err != nil {
		return err
	}
	taken, err := forkspace.ServiceLaunchTaken(repo)
	if err != nil || !taken {
		return err
	}
	busy, own := serviceLaunchHolders(repo, "", spec.SupervisorID)
	if own {
		return nil
	}
	return busy
}

// EditorUnfilteredBoxReadiness is EditorBoxReadiness for an open or offline session: a box of its own
// has nothing to wait for but the lock, and that only if it starts the project's services.
func EditorUnfilteredBoxReadiness(cfg *config.Config, rt runtime.Runtime, spec RunSpec) error {
	p, err := project.Load(projectPolicyRepo(spec))
	if err != nil {
		return err
	}
	if ComposeFileAt(spec.Repo, p.ComposeRel()) == "" || !EditorStartsServices(cfg, rt, p) {
		return nil
	}
	return EditorServiceLaunchBusy(spec)
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
