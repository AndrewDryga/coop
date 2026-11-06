package box

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	// A test can release it early from a timer, and the cleanup must not release it again.
	release = sync.OnceFunc(release)
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

// recordHolder is recordBox for a box that holds the lock, as a running one does.
func recordHolder(t *testing.T, repo string, spec forkspace.ExecutionSpec) forkspace.ExecutionRecord {
	t.Helper()
	record := recordBox(t, repo, spec)
	t.Cleanup(forkspace.NoteServiceLaunchHeld(repo, record.ID))
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
			recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop})
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
	recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP})
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
	recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop})
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
	busy, own := serviceLaunchHolders(repo, false, self.ID, "session")
	if own || len(busy.Boxes) != 0 {
		t.Fatalf("names %v, own %v, with nothing to close", busy.Boxes, own)
	}
	title, details := busy.Say()
	if title != "Waiting for another Coop command in this project" || strings.Contains(strings.Join(details, " "), "Close") {
		t.Fatalf("with nothing to close it said %q %q", title, details)
	}
	recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionInteractive, SourceID: "another"})
	recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, SourceID: "another session", Role: forkspace.ExecutionRoleActive})
	recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, SourceID: "a third session", Role: forkspace.ExecutionRoleActive})
	busy, own = serviceLaunchHolders(repo, false, self.ID, "session")
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
	warm := recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, SourceID: "session", Role: forkspace.ExecutionRoleWarm})
	if err := EditorServiceLaunchBusy(spec); err != nil {
		t.Fatalf("the session's own parked box read as a wait: %v", err)
	}
	recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop, SourceID: "a loop run"})
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
			err := EditorUnfilteredBoxReadiness(t.Context(), test.cfg, rt, RunSpec{Repo: test.repo})
			var busy *ServiceLaunchBusy
			if errors.As(err, &busy) != test.busy {
				t.Fatalf("readiness = %v, want busy=%v", err, test.busy)
			}
		})
	}
	if err := EditorUnfilteredBoxReadiness(t.Context(), open, rt, RunSpec{Repo: repo}); err != nil {
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
	recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop})
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
			recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, SourceID: "session", Role: forkspace.ExecutionRoleWarm})
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

// Two sessions in one project. A runs with a parked box that holds the lock. B connects, needs the exclusive
// side, and waits on a child that only holds prompts. That child is published in the records, and so is any
// box that waits. Neither holds anything, so A's next box asks its parked box to stop instead of waiting on
// B while B waits on that parked box. Only then can either of them go on.
func TestTwoSessionsThatNeedTheLockDoNotWaitOnEachOther(t *testing.T) {
	noticeLaunchWaitAfter(t, 10*time.Millisecond)
	repo := t.TempDir()
	recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, SourceID: "A", Role: forkspace.ExecutionRoleWarm})
	release := holdLaunchLock(t, repo)
	recordBox(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, SourceID: "B", Role: forkspace.ExecutionRoleActive})

	if err := EditorServiceLaunchBusy(RunSpec{Repo: repo, SupervisorID: "A"}); err != nil {
		t.Fatalf("A's spawn is put on hold by a box that holds nothing: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	spec := RunSpec{ActivitySource: "A", ActivityRole: forkspace.ExecutionRoleActive, OnOwnBoxesHoldLaunch: release,
		OnServiceLaunchWait: func(bool) { t.Error("A's thread was told to close something") }}
	unlock, err := lockServiceLaunch(ctx, repo, true, spec, nil)
	if err != nil {
		t.Fatalf("A's box waited on B, and B on A's parked box: %v", err)
	}
	unlock()
}

// A wait goes on asking who it waits for. A's parked boxes stop and B's box takes the lock first: A's box had
// nothing to say a moment ago, and now its thread has to hear, or it has no message and no shield from the
// first-answer limit. When only parked boxes of A's own are left again, they are asked to stop again.
func TestAWaitAsksAgainWhoHoldsTheLock(t *testing.T) {
	noticeLaunchWaitAfter(t, 10*time.Millisecond)
	repo := t.TempDir()
	parked := recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, SourceID: "A", Role: forkspace.ExecutionRoleWarm})
	release := holdLaunchLock(t, repo)

	events := make(chan string, 16)
	spec := RunSpec{ActivitySource: "A", ActivityRole: forkspace.ExecutionRoleActive, Quiet: true, ForceNoTTY: true,
		OnOwnBoxesHoldLaunch: func() { events <- "parked boxes stop" },
		OnServiceLaunchWait:  func(waiting bool) { events <- fmt.Sprint("waiting ", waiting) }}
	expect := func(want string) {
		t.Helper()
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("heard %q, want %q", got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("heard nothing, want %q", want)
		}
	}
	entered := make(chan func(), 1)
	got := captureStderr(t, func() {
		go func() {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			unlock, err := lockServiceLaunch(ctx, repo, true, spec, newLaunchSections(spec))
			if err != nil {
				t.Error(err)
				events <- "gave up"
				return
			}
			entered <- unlock
		}()
		expect("parked boxes stop")
		// The parked box goes, and in the same moment B's box holds the lock.
		other := recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop, SourceID: "B"})
		if err := forkspace.EndExecution(repo, parked); err != nil {
			t.Fatal(err)
		}
		expect("waiting true")
		// B's box goes, and a new parked box of A's is what is left.
		recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, SourceID: "A", Role: forkspace.ExecutionRoleWarm})
		if err := forkspace.EndExecution(repo, other); err != nil {
			t.Fatal(err)
		}
		expect("waiting false")
		expect("parked boxes stop")
		release()
		select {
		case unlock := <-entered:
			unlock()
		case <-time.After(10 * time.Second):
			t.Fatal("the lock came free and the wait did not end")
		}
	})
	if !strings.Contains(got, "Running now: a loop.") {
		t.Errorf("a person was never told what it waits for now: %q", got)
	}
	if !strings.Contains(got, "The wait is over.") {
		t.Errorf("a person was never told the wait ended: %q", got)
	}
}

// A box that has to wait before it starts its services says so beside its record for coop tasks watch
// for as long as it waits, and no longer once it holds the lock; a box that never waited says nothing.
func TestABoxSaysItWaitsToStartItsServicesOnlyWhileItDoes(t *testing.T) {
	noticeLaunchWaitAfter(t, 20*time.Millisecond)
	repo := t.TempDir()
	recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop})
	record := recordBox(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop})
	waiting := func() bool {
		observations, _ := forkspace.Executions(repo)
		for _, observation := range observations {
			if observation.Record.ID == record.ID {
				return observation.WaitingForServices
			}
		}
		return false
	}
	spec := RunSpec{Quiet: true, ForceNoTTY: true, ActivityRepo: repo, activityID: record.ID}
	unlock, err := lockServiceLaunch(t.Context(), repo, true, spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if waiting() {
		t.Fatal("a box that took the lock at once says it waited")
	}
	release := holdLaunchLock(t, repo)
	entered := make(chan func(), 1)
	captureStderr(t, func() {
		go func() {
			unlock, err := lockServiceLaunch(t.Context(), repo, true, spec, nil)
			if err != nil {
				t.Error(err)
			}
			entered <- unlock
		}()
		for deadline := time.Now().Add(5 * time.Second); !waiting() && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
		if !waiting() {
			t.Error("a box waiting for the lock does not say so")
		}
		release()
		(<-entered)()
	})
	if waiting() {
		t.Error("a box that holds the lock still says it waits")
	}
}

// A box says it holds the lock for as long as it does, and a box stopped by a crash says nothing.
func TestABoxSaysItHoldsTheLockOnlyWhileItDoes(t *testing.T) {
	repo := t.TempDir()
	record := recordBox(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop})
	released := false
	spec := RunSpec{activityID: record.ID}
	unlock := spec.noteLaunchHeld(repo, func() { released = true })
	if !forkspace.ServiceLaunchHeldBy(repo, record.ID) {
		t.Fatal("a box that holds the lock does not say so")
	}
	unlock()
	if !released || forkspace.ServiceLaunchHeldBy(repo, record.ID) {
		t.Fatalf("released=%v, and it still says it holds the lock=%v", released, forkspace.ServiceLaunchHeldBy(repo, record.ID))
	}
	spec.noteLaunchHeld(repo, func() {}) // it crashed here
	if err := forkspace.EndExecution(repo, record); err != nil {
		t.Fatal(err)
	}
	if forkspace.ServiceLaunchHeldBy(repo, record.ID) {
		t.Fatal("the note outlived the record")
	}
}

// A waiting session looks again when the lock changes hands, so a thread hears who it waits for now, and when
// whoever started services has finished, since a box that joins them can go on then.
func TestEditorBoxStampChangesWhenTheLockChangesHands(t *testing.T) {
	repo := t.TempDir()
	spec := RunSpec{Repo: repo, SupervisorID: "session"}
	exclusive, err := forkspace.LockServiceLaunch(t.Context(), repo, true)
	if err != nil {
		t.Fatal(err)
	}
	starting := EditorBoxStamp(nil, spec)
	exclusive()
	release := holdLaunchLock(t, repo)
	defer release()
	shared := EditorBoxStamp(nil, spec)
	if starting == shared {
		t.Fatalf("whoever started services finished, and the stamp stayed %q", shared)
	}
	recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop})
	if named := EditorBoxStamp(nil, spec); named == shared {
		t.Fatalf("a new holder left the stamp at %q", named)
	}
}

// newForkWorkspace makes a real generation of a fork of repo, as coop fork does, and its workspace.
func newForkWorkspace(t *testing.T, repo, name string) (string, forkspace.Identity) {
	t.Helper()
	workspace := forkspace.Workspace(repo, name)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureGenerationLocked(repo, name)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	return workspace, identity
}

// Every workspace starts its services from its own files, so a start waits only for boxes working there.
// Keyed by the parent project, four emisar forks starved each other for hours (2026-11-05): a fork
// restarting its services waited for every sibling's box, and a final review waited for a sibling's gate.
// The main checkout's `coop up` then waited for a fork's whole loop, its workspace lock being the
// project's. A project-wide approval change still waits for every box, through the project lock.
func TestAServiceStartWaitsOnlyForBoxesInItsOwnWorkspace(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	a, _ := newForkWorkspace(t, repo, "a")
	b, _ := newForkWorkspace(t, repo, "b")
	if got := serviceStartRepo(RunSpec{Repo: a, ActivityRepo: repo}, repo); got != a {
		t.Fatalf("a fork's service start is keyed by %q, want its workspace %q", got, a)
	}
	if got := serviceStartRepo(RunSpec{}, repo); got != repo {
		t.Fatalf("a launch with no workspace is keyed by %q, want the project %q", got, repo)
	}
	short := func() context.Context {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		t.Cleanup(cancel)
		return ctx
	}
	taken := func(unlock func(), err error) bool {
		if err != nil {
			return false
		}
		unlock()
		return true
	}
	free := func(dir string) bool { return taken(forkspace.LockServiceLaunch(short(), dir, true)) }
	approvalFree := func() bool { return taken(lockApprovalTransition(short(), repo)) }
	// A box running in fork b, as a sibling loop's box does for its whole life.
	unlock, err := enterAuthorityMountWindow(t.Context(), repo, b, nil, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !free(a) {
		t.Error("fork a's service start waited for a box in fork b")
	}
	if free(b) {
		t.Error("fork b's service start did not wait for the box running in fork b")
	}
	// What the main checkout's `coop up` and a loop box there take to start their services.
	if !taken(LockServiceLaunch(short(), repo)) {
		t.Error("the main checkout's coop up waited for a box in fork b")
	}
	if !taken(lockServiceStart(short(), serviceStartRepo(RunSpec{Repo: repo, ActivityRepo: repo}, repo), RunSpec{}, nil)) {
		t.Error("a main-checkout box's service start waited for a box in fork b")
	}
	if approvalFree() {
		t.Error("a project-wide approval change did not wait for a fork's box")
	}
	unlock()
	if !free(b) || !approvalFree() {
		t.Error("the box's locks were not released")
	}
	// The main checkout's box holds its own workspace's lock and the project lock, no fork's.
	unlock, err = enterAuthorityMountWindow(t.Context(), repo, repo, nil, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !free(a) || free(repo) || approvalFree() {
		t.Error("a main-checkout box held a fork's lock, or not its own or the project's")
	}
	unlock()
}

// A wait names only boxes in its own workspace: another workspace's box does not keep it out, so asking
// a person to close it would be wrong. The project lock names every holder.
func TestAWaitNamesOnlyBoxesInItsWorkspace(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	a, _ := newForkWorkspace(t, repo, "a")
	b, identity := newForkWorkspace(t, repo, "b")
	record, err := forkspace.BeginExecution(repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionForkLoop, Workspace: b, Fork: &identity})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forkspace.EndExecution(repo, record) })
	t.Cleanup(forkspace.NoteServiceLaunchHeld(repo, record.ID))
	recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop})
	for _, tc := range []struct {
		lock    string
		project bool
		want    []string
	}{
		{a, false, nil},
		{b, false, []string{"a loop in fork b"}},
		{repo, false, []string{"a loop"}},
		{repo, true, []string{"a loop", "a loop in fork b"}},
	} {
		busy, _ := serviceLaunchHolders(tc.lock, tc.project, "", "")
		if slices.Sort(busy.Boxes); !slices.Equal(busy.Boxes, tc.want) {
			t.Errorf("a wait on %s (project %v) names %v, want %v", tc.lock, tc.project, busy.Boxes, tc.want)
		}
	}
}

// The main checkout's first service anchor changes what every box in the project may mount, a fork's
// too, so the approval that makes it waits for every box, though `coop up` no longer does just to start
// services. An anchor already made, or a fork's own first one, waits for nobody.
func TestTheMainCheckoutsFirstServiceAnchorWaitsForEveryBox(t *testing.T) {
	pinHome(t)
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	fork, _ := newForkWorkspace(t, repo, "a")
	unlockBox, err := enterAuthorityMountWindow(t.Context(), repo, fork, nil, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { unlockBox() }()
	if unlock, create, err := lockFirstServiceAnchor(fork); err != nil || !create {
		t.Fatalf("a fork's first anchor: create %v, %v", create, err)
	} else {
		unlock()
	}
	made := make(chan error, 1)
	go func() {
		unlock, create, err := lockFirstServiceAnchor(repo)
		if err == nil {
			if !create {
				err = errors.New("the first anchor may not be made")
			}
			unlock()
		}
		made <- err
	}()
	select {
	case err := <-made:
		t.Fatalf("the main checkout's first anchor went ahead while a fork's box ran: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	unlockBox()
	if err := <-made; err != nil {
		t.Fatal(err)
	}
	if _, err := serviceApprovalAnchor(repo, true); err != nil {
		t.Fatal(err)
	}
	unlockBox, err = enterAuthorityMountWindow(t.Context(), repo, fork, nil, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	unlock, create, err := lockFirstServiceAnchor(repo)
	if err != nil || create {
		t.Fatalf("an existing anchor: create %v, %v", create, err)
	}
	unlock()
}

// A fork box starting its services is named by a wait in that fork: its note goes with its record, in the
// project's records, where every wait looks, not with the fork's own.
func TestAForkBoxStartingServicesIsNamedByAWaitThere(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	a, identity := newForkWorkspace(t, repo, "a")
	record, err := forkspace.BeginExecution(repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionForkLoop, Workspace: a, Fork: &identity})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forkspace.EndExecution(repo, record) })
	forget := RunSpec{activityID: record.ID, ActivityRepo: repo}.noteLaunchHeld(a, func() {})
	if busy, _ := serviceLaunchHolders(a, false, "", ""); !slices.Equal(busy.Boxes, []string{"a loop in fork a"}) {
		t.Errorf("a wait in fork a names %v while its box starts services", busy.Boxes)
	}
	forget()
	if busy, _ := serviceLaunchHolders(a, false, "", ""); len(busy.Boxes) != 0 {
		t.Errorf("a wait in fork a names %v once its box let go", busy.Boxes)
	}
}

// A fork's box that waits to start its services says so where `coop fork ls` looks, naming what it
// waits for as its own log does, and stops saying so once the wait ends. Shown only in the log, the
// wait left the fork reading as a running loop (2026-11-05).
func TestAForkBoxWaitingToStartServicesShowsInTheForkListing(t *testing.T) {
	noticeLaunchWaitAfter(t, 10*time.Millisecond)
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	a, identity := newForkWorkspace(t, repo, "a")
	holder, err := forkspace.BeginExecution(repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionForkInteractive, Workspace: a, Fork: &identity})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forkspace.EndExecution(repo, holder) })
	t.Cleanup(forkspace.NoteServiceLaunchHeld(repo, holder.ID))
	release := holdLaunchLock(t, a)
	spec := RunSpec{Repo: a, ActivityRepo: repo, ForkName: "a"}
	started := make(chan error, 1)
	captureStderr(t, func() {
		go func() {
			unlock, err := lockServiceStart(t.Context(), a, spec, nil)
			if err == nil {
				unlock()
			}
			started <- err
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if boxes, waiting := forkspace.LaunchWaits(repo, "a"); waiting {
				if !slices.Equal(boxes, []string{"a box in a terminal in fork a"}) {
					t.Errorf("the waiting fork waits for %v", boxes)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("a fork box waiting to start its services never said so")
			}
			time.Sleep(10 * time.Millisecond)
		}
		release()
		if err := <-started; err != nil {
			t.Fatal(err)
		}
	})
	if boxes, waiting := forkspace.LaunchWaits(repo, "a"); waiting {
		t.Fatalf("the fork still shows a wait for %v after it ended", boxes)
	}
}
