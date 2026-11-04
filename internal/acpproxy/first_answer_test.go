package acpproxy

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// shrinkFirstAnswer gives a test its own window and limit for watchFirstAnswer.
func shrinkFirstAnswer(t *testing.T, window, limit time.Duration) {
	t.Helper()
	oldWindow, oldLimit := childAnswerWindow, replayStartupGrace
	childAnswerWindow, replayStartupGrace = window, limit
	t.Cleanup(func() { childAnswerWindow, replayStartupGrace = oldWindow, oldLimit })
}

// startsWhenTold is a real child whose box starts when the test says, so that a test can read what it
// was asked first, with no window racing that read.
func startsWhenTold() (child *fakeChild, start func()) {
	var launched atomic.Bool
	child = realChild()
	child.launched = launched.Load
	return child, func() { launched.Store(true) }
}

// started marks a fake child's box as already running. A child a test does not mark never reports it,
// so only the limit applies to it.
func started(child *fakeChild) *fakeChild {
	child.launched = func() bool { return true }
	return child
}

// The promise: a box that never answers the editor's first request does not leave the editor waiting.
// It has no thread yet, so what it needs is an answer, and the holding child gives it one under the
// editor's own id. The thread it then opens is where the caller says why.
func TestAFirstChildThatNeverAnswersMovesTheSessionToAHoldingChild(t *testing.T) {
	shrinkFirstAnswer(t, 60*time.Millisecond, time.Hour)
	hung, start := startsWhenTold()
	h := newFailingHarness(t, hung, holdingChild())
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if frame := parse(readLine(t, h.childIn[0])); frame.Method != "initialize" {
		t.Fatalf("the first child got %q", frame.Method)
	}
	start()
	replay := parse(readLine(t, h.childIn[1])) // the first child never answered, and was stopped
	if replay.Method != "initialize" {
		t.Fatalf("the holding child got %q", replay.Method)
	}
	if h.failed.Load() != 1 || h.how.Load() != int32(ChildSilent) {
		t.Fatalf("%d failed starts, last how=%d, before the next child began", h.failed.Load(), h.how.Load())
	}
	writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":`+string(replay.ID)+`,"result":{"protocolVersion":1}}`)
	if answer := parse(readLine(t, h.clientOut)); string(answer.ID) != "1" || len(answer.Result) == 0 {
		t.Fatalf("the editor got %+v, want the holding child's answer under its own id", answer)
	}
	h.newSession(1, "S1")
	if !h.r.Holding() {
		t.Fatal("the session is not on the holding child")
	}
	_ = h.shutdown()
}

// A replacement that hangs used to end the session after the long grace, with nothing said. Its threads
// and a prompt waiting on it now move to the holding child, which runs no turn.
func TestAReplacementThatNeverAnswersMovesTheSessionToAHoldingChild(t *testing.T) {
	shrinkFirstAnswer(t, 60*time.Millisecond, time.Hour)
	window := ChildStartWindow
	ChildStartWindow = 0 // the first child served, so it is not a failed start
	t.Cleanup(func() { ChildStartWindow = window })
	hung, start := startsWhenTold()
	h := newFailingHarness(t, realChild(), hung, holdingChild())
	h.initialize(0)
	h.newSession(0, "S1")
	h.readEditor()
	h.children[0].outW.Close()
	if frame := parse(readLine(t, h.childIn[1])); frame.Method != "initialize" {
		t.Fatalf("the replacement got %q", frame.Method)
	}
	h.prompt("3") // it waits for the replacement, and then for whatever replaces that
	h.expectEditor("waiting for the box")
	start()
	h.answerReplay(2)
	wait.For(t, "the holding child to go live", h.r.Holding)
	if h.failed.Load() != 1 || h.how.Load() != int32(ChildSilent) {
		t.Fatalf("%d failed starts, last how=%d", h.failed.Load(), h.how.Load())
	}
	select {
	case err := <-h.done:
		t.Fatalf("the session ended with a hung replacement: %v", err)
	default:
	}
	wait.For(t, "the prompt to wait for the box that replaces this one", func() bool {
		p := h.r.bound()
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.restartHeld) == 1 && !p.restarting
	})
	_ = h.shutdown()
}

// Host work before a box starts is not silence: Compose services took 18 to 76 s on real projects, and an
// image pull or another box's launch lock can take longer. The window opens only once the box has started,
// and the whole of it is the box's, however long getting there took.
func TestTheWindowOpensWhenTheBoxStarts(t *testing.T) {
	const window = 2 * time.Second
	shrinkFirstAnswer(t, window, time.Hour)
	slow, start := startsWhenTold()
	h := newFailingHarness(t, slow)
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	readLine(t, h.childIn[0])
	time.Sleep(window + 200*time.Millisecond)
	if h.failed.Load() != 0 {
		t.Fatal("a box still getting ready was charged with not answering")
	}
	start()
	time.Sleep(200 * time.Millisecond) // several ticks of the watch
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	if answer := parse(readLine(t, h.clientOut)); string(answer.ID) != "1" || len(answer.Error) != 0 {
		t.Fatalf("the editor got %+v", answer)
	}
	if h.failed.Load() != 0 {
		t.Fatal("a box that answered within its window was stopped")
	}
	_ = h.shutdown()
}

// A box that has started answers only to the window, whatever the limit for getting ready says.
func TestOnceABoxHasStartedTheLimitNoLongerApplies(t *testing.T) {
	const limit = 200 * time.Millisecond
	shrinkFirstAnswer(t, 6*time.Second, limit)
	h := newFailingHarness(t, started(realChild()))
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	readLine(t, h.childIn[0])
	time.Sleep(5 * limit)
	if h.failed.Load() != 0 {
		t.Fatal("a box past the limit was stopped inside its window")
	}
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	if answer := parse(readLine(t, h.clientOut)); string(answer.ID) != "1" || len(answer.Error) != 0 {
		t.Fatalf("the editor got %+v", answer)
	}
	_ = h.shutdown()
}

// Getting ready still has a limit, or a wedged Compose would leave the editor waiting for good. So does a
// child that cannot say when its box started.
func TestABoxStillGettingReadyAnswersToTheLimit(t *testing.T) {
	for name, launched := range map[string]func() bool{
		"has not started": func() bool { return false },
		"cannot tell":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			const limit = 200 * time.Millisecond
			shrinkFirstAnswer(t, 6*time.Second, limit)
			slow := realChild()
			slow.launched = launched
			h := newFailingHarness(t, slow, holdingChild())
			asked := time.Now()
			writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
			if frame := parse(readLine(t, h.childIn[1])); frame.Method != "initialize" {
				t.Fatalf("the holding child got %q", frame.Method)
			}
			if took := time.Since(asked); took < limit {
				t.Fatalf("stopped after %s, before the limit", took)
			}
			if h.failed.Load() != 1 || h.how.Load() != int32(ChildNotReady) {
				t.Fatalf("%d failed starts, last how=%d", h.failed.Load(), h.how.Load())
			}
			_ = h.shutdown()
		})
	}
}

// A replacement still getting ready waits for the same limit, and the replay reading from it does not
// give up first and end the session.
func TestAReplacementStillGettingReadyAnswersToTheLimitToo(t *testing.T) {
	const limit = 200 * time.Millisecond
	shrinkFirstAnswer(t, 6*time.Second, limit)
	slow := realChild()
	slow.launched = func() bool { return false }
	h := newFailingHarness(t, started(holdingChild()), slow, holdingChild())
	h.initialize(0)
	moved := time.Now()
	if !h.r.MoveOffHolding() {
		t.Fatal("the move was refused")
	}
	if frame := parse(readLine(t, h.childIn[2])); frame.Method != "initialize" {
		t.Fatalf("the holding child got %q", frame.Method)
	}
	if took := time.Since(moved); took < limit {
		t.Fatalf("stopped after %s, before the limit", took)
	}
	if h.failed.Load() != 1 || h.how.Load() != int32(ChildNotReady) {
		t.Fatalf("%d failed starts, last how=%d", h.failed.Load(), h.how.Load())
	}
	_ = h.shutdown()
}

// A wait for the lock has no limit, so a child that says it is waiting is not late. Only what it spends on
// anything else counts.
func TestAChildWaitingForTheLockIsNotStoppedAtTheLimit(t *testing.T) {
	shrinkFirstAnswer(t, 600*time.Millisecond, 60*time.Millisecond)
	var waiting atomic.Bool
	waiting.Store(true)
	child := realChild()
	child.waitsForLock = waiting.Load
	h := newFailingHarness(t, child, holdingChild())
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	readLine(t, h.childIn[0])
	time.Sleep(300 * time.Millisecond) // several limits
	if h.failed.Load() != 0 {
		t.Fatalf("a child waiting for the lock was stopped: %d failed starts", h.failed.Load())
	}
	// Past its wait it answers to the limit again, counted from the end of the wait.
	waiting.Store(false)
	wait.For(t, "the limit to apply again", func() bool { return h.failed.Load() == 1 })
	h.answerInitializeReplay(1)
	readLine(t, h.clientOut) // the editor gets the holding child's answer
	_ = h.shutdown()
}

// A child with nothing to answer is not silent, however long it waits for an editor that has not asked.
func TestAChildNobodyAskedIsNotStopped(t *testing.T) {
	const window = 100 * time.Millisecond
	shrinkFirstAnswer(t, window, window)
	h := newFailingHarness(t, started(realChild()))
	time.Sleep(5 * window)
	if h.failed.Load() != 0 {
		t.Fatal("a child nobody asked was stopped")
	}
	_ = h.shutdown()
}

// Past its first answer a child owes the watch nothing: a slow session/new is not a failed start.
func TestAChildThatAnsweredIsNotStopped(t *testing.T) {
	const window = 100 * time.Millisecond
	shrinkFirstAnswer(t, window, time.Hour)
	child, start := startsWhenTold()
	h := newFailingHarness(t, child)
	h.initialize(0)
	start()
	time.Sleep(5 * window)
	h.newSession(0, "S1")
	if h.failed.Load() != 0 {
		t.Fatal("a child that answered was stopped")
	}
	_ = h.shutdown()
}

// A holding child that never answers counts toward the cap like one that exits, and the session ends
// with a reason in every open thread instead of nothing at all.
func TestHoldingChildrenThatNeverAnswerEndTheSessionAndSayWhy(t *testing.T) {
	shrinkFirstAnswer(t, 60*time.Millisecond, time.Hour)
	children := []*fakeChild{holdingChild()}
	for range maxRapidFails {
		children = append(children, started(holdingChild()))
	}
	h := newFailingHarness(t, children...)
	h.initialize(0)
	h.newSession(0, "S1")
	h.readEditor()
	h.children[0].outW.Close()
	select {
	case err := <-h.done:
		if err == nil || !strings.Contains(err.Error(), "could not start") {
			t.Fatalf("the session ended with %v", err)
		}
	case <-time.After(wait.Deadline):
		t.Fatal("the session never gave up on holding children that do not answer")
	}
	h.expectEditor("Coop could not start any box for this project")
	if h.failed.Load() != 0 {
		t.Fatalf("%d failed starts for children that only hold prompts", h.failed.Load())
	}
}

// A real box that answers initialize and then goes quiet in replay is hung too. It used to end the
// session; it now counts as a child that could not start.
func TestAChildThatGoesQuietPartwayThroughReplayMovesToAHoldingChild(t *testing.T) {
	idle := replayIdleTimeout
	replayIdleTimeout = 100 * time.Millisecond
	t.Cleanup(func() { replayIdleTimeout = idle })
	window := ChildStartWindow
	ChildStartWindow = 0
	t.Cleanup(func() { ChildStartWindow = window })
	h := newFailingHarness(t, realChild(), realChild(), holdingChild())
	h.initialize(0)
	h.newSession(0, "S1")
	h.readEditor()
	h.children[0].outW.Close()
	replay := parse(readLine(t, h.childIn[1]))
	writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":`+string(replay.ID)+`,"result":{}}`)
	h.answerReplay(2) // the replacement never answered the session/new it was sent next
	wait.For(t, "the holding child to go live", h.r.Holding)
	if h.failed.Load() != 1 || h.how.Load() != int32(ChildSilent) {
		t.Fatalf("%d failed starts, last how=%d", h.failed.Load(), h.how.Load())
	}
	_ = h.shutdown()
}

// A reload's first box can hang as well, and its threads survive that like any other replacement's.
func TestAResumedSessionKeepsItsThreadsWhenItsBoxNeverAnswers(t *testing.T) {
	shrinkFirstAnswer(t, 60*time.Millisecond, time.Hour)
	var failed atomic.Int32
	snap := &Snapshot{
		Initialize: []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`),
		Sessions:   []SessionSnap{{EditorID: "S1", AdapterID: "N1", Provider: "codex", Params: json.RawMessage(`{"cwd":"/w"}`), Turned: true}},
	}
	hung, start := startsWhenTold()
	h := newProxyHarnessChildren(t, []*fakeChild{hung, holdingChild()}, nil,
		RunOpts{Resume: snap, Restarter: &Restarter{}, ChildFailed: func(ChildFailure) { failed.Add(1) }})
	if frame := parse(readLine(t, h.childIn[0])); frame.Method != "initialize" {
		t.Fatalf("the first box got %q", frame.Method)
	}
	start()
	replay := parse(readLine(t, h.childIn[1]))
	writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":`+string(replay.ID)+`,"result":{}}`)
	load := parse(readLine(t, h.childIn[1]))
	if load.Method != "session/load" || sessionID(load.Params) != "N1" {
		t.Fatalf("the holding child got %q for %q, want the restored thread", load.Method, sessionID(load.Params))
	}
	if failed.Load() != 1 {
		t.Fatalf("%d failed starts", failed.Load())
	}
	_ = h.shutdown()
}

// A session with no way to hold, an open or offline one, keeps the old rule: nothing stops its first box,
// and TestProxyReplayTimeoutFailsPending holds its replacement to the long grace.
func TestWithNoWayToHoldNothingWatchesTheFirstAnswer(t *testing.T) {
	const window = 100 * time.Millisecond
	shrinkFirstAnswer(t, window, window)
	h := newProxyHarnessChildren(t, []*fakeChild{started(realChild())}, nil, RunOpts{})
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	readLine(t, h.childIn[0])
	time.Sleep(5 * window)
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	if answer := parse(readLine(t, h.clientOut)); string(answer.ID) != "1" || len(answer.Error) != 0 {
		t.Fatalf("the editor got %+v", answer)
	}
	_ = h.shutdown()
}
