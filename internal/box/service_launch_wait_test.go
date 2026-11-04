package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func noticeLaunchWaitAfter(t *testing.T, d time.Duration) {
	t.Helper()
	previous := serviceLaunchWaitNotice
	serviceLaunchWaitNotice = d
	t.Cleanup(func() { serviceLaunchWaitNotice = previous })
}

// holdLaunchLock holds the shared side, as a running box does, until the returned call.
func holdLaunchLock(t *testing.T, repo string) func() {
	t.Helper()
	release, err := forkspace.LockServiceLaunch(t.Context(), repo, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return release
}

// recordBox publishes a running box in repo, as a launch does before it takes the lock.
func recordBox(t *testing.T, repo string, spec forkspace.ExecutionSpec) forkspace.ExecutionRecord {
	t.Helper()
	spec.Workspace = repo
	record, err := forkspace.BeginExecution(repo, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forkspace.EndExecution(repo, record) })
	return record
}

const waitSaysABox = "Coop goes on by itself once it stops. Close it to go on sooner."

// A box that has to wait says what for and what to do about it, wherever a person is looking, and says
// when the wait ends. It really waits: the lock is never dropped or shortened to say so.
func TestABoxThatHasToWaitSaysSoAndWhenItEnds(t *testing.T) {
	for name, test := range map[string]struct {
		spec RunSpec
		want string
	}{
		"a terminal": {RunSpec{Cmd: []string{"make"}}, "Waiting for another box in this project\n" +
			"  Running now: a loop.\n  " + waitSaysABox + "\n  ✓ The wait is over\n"},
		"a loop": {RunSpec{Cmd: []string{"make"}, Batch: true, LoopPresentation: true}, "Preparing task environment\n" +
			"  Starting services\n  Waiting for another box in this project\n" +
			"    Running now: a loop.\n    " + waitSaysABox + "\n  ✓ The wait is over\n"},
		"a quiet run or an editor child": {RunSpec{Quiet: true, ForceNoTTY: true}, "Waiting for another box in this project.\n" +
			"Running now: a loop.\n" + waitSaysABox + "\nThe wait is over.\n"},
	} {
		t.Run(name, func(t *testing.T) {
			noticeLaunchWaitAfter(t, 20*time.Millisecond)
			repo := t.TempDir()
			recordBox(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop})
			release := holdLaunchLock(t, repo)
			var told []bool
			test.spec.OnServiceLaunchWait = func(waiting bool) { told = append(told, waiting) }
			entered := make(chan func(), 1)
			got := captureStderr(t, func() {
				go func() {
					unlock, err := lockServiceLaunch(t.Context(), repo, true, test.spec, newLaunchSections(test.spec))
					if err != nil {
						t.Error(err)
					}
					entered <- unlock
				}()
				select {
				case <-entered:
					t.Error("took the lock while a running box held it")
				case <-time.After(150 * time.Millisecond):
				}
				release()
				(<-entered)()
			})
			if got != test.want {
				t.Errorf("said %q, want %q", got, test.want)
			}
			if !slices.Equal(told, []bool{true, false}) {
				t.Errorf("the supervisor heard %v, want the start and the end", told)
			}
		})
	}
}

// `coop up`, an approval and the like wait the same way, as commands with nothing of their own running.
func TestACommandThatWaitsForABoxNamesIt(t *testing.T) {
	noticeLaunchWaitAfter(t, 20*time.Millisecond)
	repo := t.TempDir()
	recordBox(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP})
	time.AfterFunc(100*time.Millisecond, holdLaunchLock(t, repo))
	got := captureStderr(t, func() {
		unlock, err := LockServiceLaunch(t.Context(), repo)
		if err != nil {
			t.Fatal(err)
		}
		unlock()
	})
	want := "Waiting for another box in this project.\nRunning now: an editor session.\n" + waitSaysABox + "\nThe wait is over.\n"
	if got != want {
		t.Fatalf("said %q, want %q", got, want)
	}
}

// A person hears about a wait, not about the lock: no wait says nothing, and neither does one that ends
// before it is worth saying.
func TestALaunchThatBarelyWaitsSaysNothing(t *testing.T) {
	noticeLaunchWaitAfter(t, time.Minute)
	repo := t.TempDir()
	spec := RunSpec{OnServiceLaunchWait: func(bool) { t.Error("the supervisor heard of a wait") }}
	got := captureStderr(t, func() {
		unlock, err := lockServiceLaunch(t.Context(), repo, true, spec, newLaunchSections(spec))
		if err != nil {
			t.Fatal(err)
		}
		unlock()
		release := holdLaunchLock(t, repo)
		time.AfterFunc(30*time.Millisecond, release)
		if unlock, err = lockServiceLaunch(t.Context(), repo, true, spec, newLaunchSections(spec)); err != nil {
			t.Fatal(err)
		}
		unlock()
	})
	if got != "" {
		t.Fatalf("said %q", got)
	}
}

// The shared side waits only for whoever is starting services, so it never asks anyone to close a box,
// even with one running, and a supervisor has nothing to act on.
func TestTheSharedSideWaitsForACommandNotABox(t *testing.T) {
	noticeLaunchWaitAfter(t, 10*time.Millisecond)
	repo := t.TempDir()
	recordBox(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop})
	exclusive, err := forkspace.LockServiceLaunch(t.Context(), repo, true)
	if err != nil {
		t.Fatal(err)
	}
	spec := RunSpec{OnServiceLaunchWait: func(bool) { t.Error("the supervisor heard of a short wait") }}
	got := captureStderr(t, func() {
		time.AfterFunc(60*time.Millisecond, exclusive)
		unlock, err := lockServiceLaunch(t.Context(), repo, false, spec, nil)
		if err != nil {
			t.Fatal(err)
		}
		unlock()
	})
	want := "Waiting for another Coop command in this project.\nCoop goes on by itself once it finishes.\nThe wait is over.\n"
	if got != want {
		t.Fatalf("said %q, want %q", got, want)
	}
}

// A box stopped while it waits ends quietly, as any cancelled setup does, and claims no end to the wait.
func TestAStoppedWaitIsACancelledSetup(t *testing.T) {
	noticeLaunchWaitAfter(t, 10*time.Millisecond)
	repo := t.TempDir()
	holdLaunchLock(t, repo)
	ctx, cancel := context.WithCancel(t.Context())
	var told []bool
	spec := RunSpec{OnServiceLaunchWait: func(waiting bool) { told = append(told, waiting); cancel() }}
	var err error
	got := captureStderr(t, func() {
		_, err = lockServiceStart(ctx, repo, spec, nil)
	})
	if !errors.Is(err, context.Canceled) || !Interrupted(err) {
		t.Fatalf("err = %#v, want a cancelled setup", err)
	}
	if strings.Contains(got, "over") || !slices.Equal(told, []bool{true}) {
		t.Fatalf("said %q and told %v", got, told)
	}
}

// A person is asked to close only what closing would help with. A box never waits for itself, a fork
// loop's controller holds nothing, and neither does its own session's child that only holds prompts.
func TestAWaitNamesOnlyWhatClosingWouldEnd(t *testing.T) {
	repo := t.TempDir()
	self := recordBox(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, SourceID: "session", Role: forkspace.ExecutionRoleActive})
	recordBox(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, SourceID: "session", Role: forkspace.ExecutionRoleActive})
	recordBox(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop, SourceID: "somebody", Role: forkspace.ExecutionRoleController})
	busy, own := serviceLaunchHolders(repo, self.ID, "session")
	if own || len(busy.Boxes) != 0 {
		t.Fatalf("names %v, own %v, with nothing to close", busy.Boxes, own)
	}
	title, details := busy.Say()
	if title != "Waiting for another Coop command in this project" || strings.Contains(strings.Join(details, " "), "Close") {
		t.Fatalf("with nothing to close it said %q %q", title, details)
	}
	recordBox(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionInteractive, SourceID: "another"})
	recordBox(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, SourceID: "another session", Role: forkspace.ExecutionRoleActive})
	recordBox(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, SourceID: "a third session", Role: forkspace.ExecutionRoleActive})
	busy, own = serviceLaunchHolders(repo, self.ID, "session")
	if own || !slices.Equal(busy.Boxes, []string{"a box in a terminal", "an editor session"}) {
		t.Fatalf("names %v, own %v", busy.Boxes, own)
	}
	_, details = busy.Say()
	if !strings.Contains(details[1], "they stop") || !strings.Contains(details[1], "Close them") {
		t.Fatalf("two kinds of box were called one: %q", details)
	}
}

// Every kind of box has a name a person knows it by, and a fork says which.
func TestLiveBoxNamesAreCoopsOwnWords(t *testing.T) {
	fork := &forkspace.Identity{Name: "perf"}
	for _, test := range []struct {
		record forkspace.ExecutionRecord
		want   string
	}{
		{forkspace.ExecutionRecord{Kind: forkspace.ExecutionLocalLoop}, "a loop"},
		{forkspace.ExecutionRecord{Kind: forkspace.ExecutionForkLoop, Fork: fork}, "a loop in fork perf"},
		{forkspace.ExecutionRecord{Kind: forkspace.ExecutionInteractive}, "a box in a terminal"},
		{forkspace.ExecutionRecord{Kind: forkspace.ExecutionForkInteractive, Fork: fork}, "a box in a terminal in fork perf"},
		{forkspace.ExecutionRecord{Kind: forkspace.ExecutionACP}, "an editor session"},
		{forkspace.ExecutionRecord{Kind: forkspace.ExecutionForkACP, Fork: fork}, "an editor session in fork perf"},
		{forkspace.ExecutionRecord{Kind: forkspace.ExecutionRemoteSession, Fork: fork}, "a remote session in fork perf"},
		{forkspace.ExecutionRecord{Kind: forkspace.ExecutionReview}, "a review"},
		{forkspace.ExecutionRecord{Kind: forkspace.ExecutionGate}, "a gate"},
		{forkspace.ExecutionRecord{Kind: "from a newer coop"}, "another box"},
	} {
		got := liveBoxNames([]forkspace.ExecutionObservation{{Record: test.record}})
		if !slices.Equal(got, []string{test.want}) {
			t.Errorf("%q is called %q, want %q", test.record.Kind, got, test.want)
		}
	}
}

// A session asks before it starts a box, so a free lock is no wait, another box is, and a parked box of
// its own is nothing a thread could do anything about. The child that holds prompts in the meantime
// must not read as that, or the session would move off it while the other box still runs.
func TestEditorServiceLaunchBusyAsksAboutTheLockAndWhoHoldsIt(t *testing.T) {
	repo := t.TempDir()
	spec := RunSpec{Repo: repo, SupervisorID: "session"}
	if err := EditorServiceLaunchBusy(spec); err != nil {
		t.Fatalf("a free lock read as %v", err)
	}
	var busy *ServiceLaunchBusy
	holding := recordBox(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, SourceID: "session", Role: forkspace.ExecutionRoleActive})
	release := holdLaunchLock(t, repo)
	defer release()
	if err := EditorServiceLaunchBusy(spec); !errors.As(err, &busy) || len(busy.Boxes) != 0 {
		t.Fatalf("a lock nobody names, beside the session's holding child, read as %v", err)
	}
	if err := forkspace.EndExecution(repo, holding); err != nil {
		t.Fatal(err)
	}
	warm := recordBox(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, SourceID: "session", Role: forkspace.ExecutionRoleWarm})
	if err := EditorServiceLaunchBusy(spec); err != nil {
		t.Fatalf("the session's own parked box read as a wait: %v", err)
	}
	recordBox(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop, SourceID: "a loop run"})
	if err := EditorServiceLaunchBusy(spec); !errors.As(err, &busy) || !slices.Equal(busy.Boxes, []string{"a loop"}) {
		t.Fatalf("a loop beside the parked box read as %v", err)
	}
	if err := forkspace.EndExecution(repo, warm); err != nil {
		t.Fatal(err)
	}
	release()
	if err := EditorServiceLaunchBusy(spec); err != nil {
		t.Fatalf("a lock that came free read as %v", err)
	}
}

// An open session's box waits only if it starts the project's services at all.
func TestEditorUnfilteredReadinessAsksOnlyWhenItsBoxStartsServices(t *testing.T) {
	repo, _ := writeCompose(t, "services:\n  db:\n    image: postgres:18\n")
	bare := t.TempDir()
	rt := runtime.Runtime{Name: "docker"}
	open := &config.Config{Egress: "open", AutoUp: true, Network: true}
	for name, test := range map[string]struct {
		repo string
		cfg  *config.Config
		busy bool
	}{
		"services start":       {repo, open, true},
		"no services":          {bare, open, false},
		"services off":         {repo, &config.Config{Egress: "open", AutoUp: false, Network: true}, false},
		"not on their network": {repo, &config.Config{Egress: "open", AutoUp: true, Network: false}, false},
		"offline":              {repo, &config.Config{Egress: "none", AutoUp: true, Network: true}, false},
	} {
		t.Run(name, func(t *testing.T) {
			defer holdLaunchLock(t, test.repo)()
			err := EditorUnfilteredBoxReadiness(test.cfg, rt, RunSpec{Repo: test.repo})
			var busy *ServiceLaunchBusy
			if errors.As(err, &busy) != test.busy {
				t.Fatalf("readiness = %v, want busy=%v", err, test.busy)
			}
		})
	}
	if err := EditorUnfilteredBoxReadiness(open, rt, RunSpec{Repo: repo}); err != nil {
		t.Fatalf("a free lock read as %v", err)
	}
}

// A waiting open session has only the lock to watch, and notices when it comes free.
func TestEditorBoxStampChangesWhenTheLockComesFree(t *testing.T) {
	repo := t.TempDir()
	spec := RunSpec{Repo: repo}
	free := EditorBoxStamp(nil, spec)
	release := holdLaunchLock(t, repo)
	taken := EditorBoxStamp(nil, spec)
	release()
	if free == taken || EditorBoxStamp(nil, spec) != free {
		t.Fatalf("free %q, taken %q, free again %q", free, taken, EditorBoxStamp(nil, spec))
	}
}

// The filtered launch's own start of the services and an open run's both go through the wait.
func TestRunSaysWhenItsServicesWaitForAnotherBox(t *testing.T) {
	noticeLaunchWaitAfter(t, 20*time.Millisecond)
	repo, _ := writeCompose(t, "services:\n  db:\n    image: postgres:18\n    expose: [5432]\n")
	recordBox(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop})
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "open", AutoUp: true}
	release := holdLaunchLock(t, repo)
	var told []bool
	spec := RunSpec{Image: "i", Repo: repo, Cmd: []string{"true"}, Network: true, Batch: true, LoopPresentation: true,
		OnServiceLaunchWait: func(waiting bool) {
			told = append(told, waiting)
			if waiting {
				if data, err := os.ReadFile(recorder); err == nil && strings.Contains(string(data), " up ") {
					t.Errorf("services started before the lock was free:\n%s", data)
				}
				release()
			}
		}}
	var code int
	var err error
	got := captureStderr(t, func() { code, err = Run(cfg, recorderRuntime(t, recorder), spec) })
	if err != nil || code != 0 {
		t.Fatalf("Run = %d, %v\n%s", code, err, got)
	}
	for _, want := range []string{"Starting services\n  Waiting for another box in this project\n", "Running now: a loop.", waitSaysABox, "✓ The wait is over\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("said %q, want %q in it", got, want)
		}
	}
	if !slices.Equal(told, []bool{true, false}) {
		t.Errorf("heard %v", told)
	}
}

// A filtered box's launch goes through the same wait, at its last step before the services start.
func TestFilteredLaunchSaysWhenItsServicesWaitForAnotherBox(t *testing.T) {
	noticeLaunchWaitAfter(t, 10*time.Millisecond)
	f, _ := filteredFixture(t)
	f.preparedServices = &preparedFilteredServices{}
	holdLaunchLock(t, f.record.Project)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	spec := RunSpec{Repo: f.record.Project, OnServiceLaunchWait: func(bool) { cancel() }}
	var err error
	got := captureStderr(t, func() { _, err = f.launch(ctx, spec, nil, nil, nil, nil) })
	if !Interrupted(err) {
		t.Fatalf("the launch ended with %v", err)
	}
	if !strings.Contains(got, "Waiting for another Coop command in this project.") {
		t.Fatalf("said %q", got)
	}
}

// Only the session's own box asks for its parked neighbours to stop. A parked box asking the same would
// have them stop each other.
func TestOnlyTheSessionsOwnBoxAsksParkedBoxesToStop(t *testing.T) {
	for _, tc := range []struct {
		name string
		role forkspace.ExecutionRole
		want int
	}{{"the session's box", forkspace.ExecutionRoleActive, 1}, {"another parked box", forkspace.ExecutionRoleWarm, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			defer func(old time.Duration) { serviceLaunchWaitNotice = old }(serviceLaunchWaitNotice)
			serviceLaunchWaitNotice = time.Millisecond
			repo := t.TempDir()
			recordBox(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, SourceID: "session", Role: forkspace.ExecutionRoleWarm})
			held, err := forkspace.LockServiceLaunch(t.Context(), repo, false)
			if err != nil {
				t.Fatal(err)
			}
			asked := 0
			spec := RunSpec{ActivitySource: "session", ActivityRole: tc.role, OnOwnBoxesHoldLaunch: func() { asked++; held() },
				OnServiceLaunchWait: func(bool) { t.Error("a thread heard about the session's own boxes") }}
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			unlock, err := lockServiceLaunch(ctx, repo, true, spec, nil)
			if err == nil {
				unlock()
			} else {
				held()
			}
			if asked != tc.want {
				t.Fatalf("asked %d times, want %d", asked, tc.want)
			}
		})
	}
}
