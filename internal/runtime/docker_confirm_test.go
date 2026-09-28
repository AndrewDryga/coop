package runtime

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// The seven-way check that used to hide behind one message: each condition is named, and each is
// sorted into confirmed / retry-me / no-retry-will-help. This is the classification the flake was
// about — a slow or unsettled probe must be RETRIED, only a gone, restarted or never-started
// container is a truth.
func TestClassifyTerminalNamesAndSortsEveryCondition(t *testing.T) {
	exited := DockerContainer{State: DockerContainerState{Status: "exited", StartedAt: time.Now().UTC(), ExitCode: 3}}
	cases := []struct {
		name    string
		value   DockerContainer
		present bool
		err     error
		want    terminalVerdict
		reason  string
	}{
		{"clean exit", exited, true, nil, terminalConfirmed, ""},
		{"dead is terminal too", DockerContainer{State: DockerContainerState{Status: "dead", StartedAt: time.Now().UTC()}}, true, nil, terminalConfirmed, ""},
		{"inspect did not answer", DockerContainer{}, true, errors.New("i/o timeout"), terminalTransient, "did not answer"},
		{"gone", DockerContainer{}, false, nil, terminalUnrecoverable, "gone"},
		{"restarted", DockerContainer{RestartCount: 1, State: DockerContainerState{Status: "exited", StartedAt: time.Now().UTC()}}, true, nil, terminalUnrecoverable, "restarted"},
		{"never started", DockerContainer{State: DockerContainerState{Status: "created"}}, true, nil, terminalUnrecoverable, "never started"},
		{"still running", DockerContainer{State: DockerContainerState{Status: "running", Running: true, StartedAt: time.Now().UTC()}}, true, nil, terminalTransient, "still running"},
		{"paused", DockerContainer{State: DockerContainerState{Status: "paused", Paused: true, StartedAt: time.Now().UTC()}}, true, nil, terminalTransient, "paused"},
		{"status not yet exited", DockerContainer{State: DockerContainerState{Status: "removing", StartedAt: time.Now().UTC()}}, true, nil, terminalTransient, "status is removing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, reason := classifyTerminal(tc.value, tc.present, tc.err)
			if verdict != tc.want {
				t.Errorf("verdict = %d, want %d", verdict, tc.want)
			}
			if tc.reason != "" && !strings.Contains(reason, tc.reason) {
				t.Errorf("reason %q does not name %q", reason, tc.reason)
			}
			if tc.want == terminalConfirmed && reason != "" {
				t.Errorf("a confirmed exit carries no reason, got %q", reason)
			}
		})
	}
}

// A container caught mid-transition is confirmed on a later poll, not failed on the first — the
// exact flake: a slow host shows running or one inspect times out, then exited a beat later.
func TestConfirmWorkloadExitRetriesTransientObservationsUntilItSettles(t *testing.T) {
	settledExit := 5
	states := []struct {
		value DockerContainer
		err   error
	}{
		{value: DockerContainer{State: DockerContainerState{Status: "running", Running: true, StartedAt: time.Now().UTC()}}},
		{err: errors.New("context deadline exceeded")},
		{value: DockerContainer{State: DockerContainerState{Status: "removing", StartedAt: time.Now().UTC()}}},
		{value: DockerContainer{State: DockerContainerState{Status: "exited", StartedAt: time.Now().UTC(), ExitCode: settledExit}}},
	}
	call := 0
	inspect := func() (DockerContainer, bool, error) {
		v := states[min(call, len(states)-1)]
		call++
		return v.value, v.err == nil, v.err
	}
	tick := make(chan time.Time, len(states))
	for range states {
		tick <- time.Now()
	}
	value, err := confirmWorkloadExit(inspect, tick, neverDeadline())
	if err != nil {
		t.Fatalf("a settling container was not confirmed: %v", err)
	}
	if value.State.ExitCode != settledExit {
		t.Errorf("exit code = %d, want the settled %d", value.State.ExitCode, settledExit)
	}
	if call < len(states) {
		t.Errorf("confirmed after %d polls; it should have retried through the transient states", call)
	}
}

// One representative unrecoverable state fails closed at once; the classifier table covers the
// other states. A persistent inspect error keeps the raw error joined for errors.Is callers.
func TestConfirmWorkloadExitFailsClosedOnAnUnrecoverableStateImmediately(t *testing.T) {
	sentinel := errors.New("daemon gone")
	calls := 0
	inspect := func() (DockerContainer, bool, error) {
		calls++
		return DockerContainer{}, false, nil
	}
	// A tick that always fires and a deadline that never does: only an immediate return
	// keeps this from looping forever, which is the point — an unrecoverable state must not
	// retry. Bounded so a regression is a named failure, not a hung suite.
	result := make(chan error, 1)
	go func() {
		_, err := confirmWorkloadExit(inspect, alwaysTick(), neverDeadline())
		result <- err
	}()
	var err error
	select {
	case err = <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("an unrecoverable state was retried instead of decided at once")
	}
	if err == nil {
		t.Fatal("an unrecoverable state was reported confirmed")
	}
	if !strings.Contains(err.Error(), "without a confirmed workload outcome") || !strings.Contains(err.Error(), "gone") {
		t.Errorf("error %q does not name the failure or its cause", err)
	}
	if calls != 1 {
		t.Errorf("inspected %d times; an unrecoverable state is decided on the first read", calls)
	}

	// The raw inspect error survives to the caller so errors.Is keeps working.
	failing := func() (DockerContainer, bool, error) {
		return DockerContainer{State: DockerContainerState{Status: "running", Running: true, StartedAt: time.Now().UTC()}}, true, sentinel
	}
	_, err = confirmWorkloadExit(failing, alwaysTick(), firedDeadline())
	if !errors.Is(err, sentinel) {
		t.Errorf("the raw inspect error did not survive into %v", err)
	}
}

// A state that stays transient for the whole budget fails closed — 'no answer', not success —
// and the message carries the last condition it saw, never a bare "unconfirmed".
func TestConfirmWorkloadExitFailsClosedWhenNeverTerminalWithinTheBudget(t *testing.T) {
	inspect := func() (DockerContainer, bool, error) {
		return DockerContainer{State: DockerContainerState{Status: "running", Running: true, StartedAt: time.Now().UTC()}}, true, nil
	}
	value, err := confirmWorkloadExit(inspect, alwaysTick(), firedDeadline())
	if err == nil {
		t.Fatal("a never-terminal container was reported confirmed")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("error %q does not name the persisting condition", err)
	}
	if value.State.Status != "running" {
		t.Errorf("the last observed state should travel back, got %q", value.State.Status)
	}
}

// The confirmation path runs end to end through StartAttached: a fixture whose container reads
// running for a moment after the client returns, then exited — the reported flake, reproduced —
// and coop confirms it rather than failing on the first probe.
func TestDockerSettlingWorkloadIsConfirmedNotFailed(t *testing.T) {
	defer swapConfirmExitBudget(2 * time.Second)()
	rt, _ := fixtureDocker(t, dockerFixture{Mode: "settling", Container: dockerFixtureContainer()})
	d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	code, err := d.StartAttached(context.Background(), dockerFixtureRef(), nil, io.Discard, io.Discard, func() error { return nil })
	if err != nil {
		t.Fatalf("a settling container was reported failed: %v", err)
	}
	if code != settlingExitCode {
		t.Errorf("exit code = %d, want the settled %d", code, settlingExitCode)
	}
}

// neverDeadline never fires; firedDeadline is already spent; alwaysTick always fires — the extremes
// a confirmation loop must handle without hanging when the OTHER channel decides the outcome.
func neverDeadline() <-chan struct{} { return make(chan struct{}) }

func firedDeadline() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func alwaysTick() <-chan time.Time {
	ch := make(chan time.Time)
	close(ch)
	return ch
}

// settlingExitCode is the exit code the settling fixture reports once its container leaves the
// brief post-return running window — distinct from the client's own exit so a confused test would
// read the wrong one.
const settlingExitCode = 5

// swapConfirmExitBudget shortens the post-attach confirmation budget for a test that deliberately
// hits it (a container that never settles), and restores it. The var exists exactly so a test need
// not wait the production five seconds.
func swapConfirmExitBudget(d time.Duration) func() {
	previous := confirmExitBudget
	confirmExitBudget = d
	return func() { confirmExitBudget = previous }
}
