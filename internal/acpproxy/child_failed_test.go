package acpproxy

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// failingHarness runs a proxy whose caller can hold prompts, over the children given, counting each
// time a real one could not start.
type failingHarness struct {
	*holdingHarness
	failed atomic.Int32
}

func newFailingHarness(t *testing.T, children ...*fakeChild) *failingHarness {
	t.Helper()
	f := &failingHarness{}
	r := &Restarter{}
	hooks := &Hooks{PromptHeld: func(string) []byte { return []byte(heldStatus) }}
	opts := RunOpts{Restarter: r, ChildFailed: func() { f.failed.Add(1) }}
	f.holdingHarness = &holdingHarness{proxyHarness: newProxyHarnessChildren(t, children, hooks, opts), r: r, editor: make(chan string, 32)}
	return f
}

func realChild() *fakeChild {
	child := newFakeChild()
	child.provider = "codex"
	return child
}

func holdingChild() *fakeChild {
	child := realChild()
	child.holdPrompts = true
	return child
}

// The editor asked the first child, which refused to start. The replacement's answer is the editor's
// answer: an error to initialize reads as a dead server, the very thing a holding child is there for.
func TestEditorsInitializeSurvivesARealChildThatCannotStart(t *testing.T) {
	h := newFailingHarness(t, realChild(), holdingChild())
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if frame := parse(readLine(t, h.childIn[0])); frame.Method != "initialize" {
		t.Fatalf("the first child got %q", frame.Method)
	}
	h.children[0].outW.Close()
	replay := parse(readLine(t, h.childIn[1]))
	if replay.Method != "initialize" {
		t.Fatalf("the holding child got %q", replay.Method)
	}
	if h.failed.Load() != 1 {
		t.Fatalf("%d failed starts reported before the next child began", h.failed.Load())
	}
	writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":`+string(replay.ID)+`,"result":{"protocolVersion":1}}`)
	answer := parse(readLine(t, h.clientOut))
	if string(answer.ID) != "1" || !strings.Contains(string(answer.Result), "protocolVersion") || len(answer.Error) != 0 {
		t.Fatalf("the editor got %+v, want the holding child's answer under its own id", answer)
	}
	h.newSession(1, "S1")
	if !h.r.Holding() {
		t.Fatal("the session is not on the holding child")
	}
	_ = h.shutdown()
}

// A replacement can die as it replays, and the question still waits for one that answers.
func TestEditorsInitializeSurvivesAReplacementThatDiesToo(t *testing.T) {
	h := newFailingHarness(t, realChild(), holdingChild(), holdingChild())
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	readLine(t, h.childIn[0])
	h.children[0].outW.Close()
	readLine(t, h.childIn[1])
	h.children[1].outW.Close()
	replay := parse(readLine(t, h.childIn[2]))
	writeLine(t, h.children[2].outW, `{"jsonrpc":"2.0","id":`+string(replay.ID)+`,"result":{"protocolVersion":1}}`)
	if answer := parse(readLine(t, h.clientOut)); string(answer.ID) != "1" || len(answer.Result) == 0 {
		t.Fatalf("the editor got %+v, want the answer of the child that lived", answer)
	}
	_ = h.shutdown()
}

// The other order: the first child failed before the editor asked, and the replacement is still
// starting. The question waits for it, so a slow editor is not told the server failed.
func TestEditorsInitializeDuringAReplacementGetsItsAnswer(t *testing.T) {
	block := make(chan struct{})
	first, second := realChild(), holdingChild()
	r := &Restarter{}
	failed := make(chan struct{})
	h := newProxyHarnessChildren(t, []*fakeChild{first, second}, nil, RunOpts{Restarter: r, ChildFailed: func() { close(failed); <-block }})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(block) }) })
	first.outW.Close()
	<-failed
	// The factory will not be asked for the replacement until block closes: the child is gone and the
	// session is restarting, which is the window in question.
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	wait.For(t, "the question to be held", func() bool {
		p := r.bound()
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.restartHeld) == 1
	})
	release.Do(func() { close(block) })
	frame := parse(readLine(t, h.childIn[1]))
	if frame.Method != "initialize" || string(frame.ID) != "1" {
		t.Fatalf("the replacement got %q (id %s), want the editor's own question", frame.Method, frame.ID)
	}
	writeLine(t, second.outW, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`)
	if answer := parse(readLine(t, h.clientOut)); string(answer.ID) != "1" || len(answer.Result) == 0 {
		t.Fatalf("the editor got %+v", answer)
	}
	_ = h.shutdown()
}

// A real box that keeps refusing is the caller's to report, at its own pace, however often it happens.
// Only a child that holds prompts failing means the session cannot stay connected, and then every
// open thread hears why it ended, since a stopped Docker looks like this.
func TestOnlyChildrenThatHoldPromptsCountTowardTheCap(t *testing.T) {
	var children []*fakeChild
	for range maxRapidFails + 3 {
		children = append(children, realChild())
	}
	children = append(children, holdingChild()) // this one serves, so a thread is open, and is the first to count
	for range maxRapidFails - 1 {
		children = append(children, holdingChild())
	}
	h := newFailingHarness(t, children...)
	real := maxRapidFails + 3
	for i := range real {
		h.children[i].outW.Close()
	}
	wait.For(t, "every real child to fail", func() bool { return h.failed.Load() == int32(real) })
	wait.For(t, "the holding child to go live", h.r.Holding)
	h.initialize(real)
	h.newSession(real, "S1")
	h.readEditor()
	h.children[real].outW.Close()
	for i := real + 1; i < len(children); i++ {
		if frame := parse(readLine(t, h.childIn[i])); frame.Method != "initialize" {
			t.Fatalf("child %d got %q", i, frame.Method)
		}
		h.children[i].outW.Close()
	}
	h.expectEditor("Coop could not start any box for this project")
	select {
	case err := <-h.done:
		if err == nil || !strings.Contains(err.Error(), "could not start 5 times") {
			t.Fatalf("the session ended with %v", err)
		}
	case <-time.After(wait.Deadline):
		t.Fatal("the session did not give up on children that hold prompts")
	}
	if h.failed.Load() != int32(real) {
		t.Fatalf("%d failed starts, want only the %d real children's", h.failed.Load(), real)
	}
}

// A slow refusal is still a refusal: a holding child that never answered counts however long it took
// to die, or a refusal that takes a few seconds would respawn forever with no cap at all.
func TestAHoldingChildThatNeverAnsweredCountsHoweverLongItLived(t *testing.T) {
	healthy := minHealthy
	minHealthy = 0
	t.Cleanup(func() { minHealthy = healthy })
	var children []*fakeChild
	for range maxRapidFails {
		children = append(children, holdingChild())
	}
	h := newFailingHarness(t, children...)
	for i := range children {
		h.children[i].outW.Close()
	}
	select {
	case err := <-h.done:
		if err == nil {
			t.Fatal("the session ended with no reason")
		}
	case <-time.After(wait.Deadline):
		t.Fatal("slowly failing children never reached the cap")
	}
}

// A child that answered and then ran for a while is not one that could not start, and its replacement
// is a real box again.
func TestARealChildThatServedThenDiedIsNotAFailedStart(t *testing.T) {
	window := ChildStartWindow
	ChildStartWindow = 0
	t.Cleanup(func() { ChildStartWindow = window })
	h := newFailingHarness(t, realChild(), realChild())
	h.initialize(0)
	h.children[0].outW.Close()
	h.answerInitializeReplay(1)
	if h.failed.Load() != 0 {
		t.Fatalf("%d failed starts for a child that served", h.failed.Load())
	}
	_ = h.shutdown()
}

// Dying right after answering is still failing to start: an adapter that cannot live in this box often
// gets that far.
func TestARealChildThatAnsweredAndDiedAtOnceIsAFailedStart(t *testing.T) {
	h := newFailingHarness(t, realChild(), holdingChild())
	h.initialize(0)
	h.children[0].outW.Close()
	h.answerInitializeReplay(1)
	if h.failed.Load() != 1 {
		t.Fatalf("%d failed starts", h.failed.Load())
	}
	_ = h.shutdown()
}

func (h *failingHarness) answerInitializeReplay(child int) {
	h.t.Helper()
	frame := parse(readLine(h.t, h.childIn[child]))
	if frame.Method != "initialize" {
		h.t.Fatalf("child %d got %q", child, frame.Method)
	}
	writeLine(h.t, h.children[child].outW, `{"jsonrpc":"2.0","id":`+string(frame.ID)+`,"result":{}}`)
}

// A switch, a move or a shutdown is Coop stopping the child, whatever its age.
func TestAnIntentionalStopIsNotAFailedStart(t *testing.T) {
	t.Run("a switch", func(t *testing.T) {
		h := newFailingHarness(t, realChild(), realChild())
		h.initialize(0)
		if !h.r.RestartIfIdle() {
			t.Fatal("the idle child was not restarted")
		}
		h.answerInitializeReplay(1)
		if h.failed.Load() != 0 {
			t.Fatalf("%d failed starts", h.failed.Load())
		}
		_ = h.shutdown()
	})
	t.Run("a move", func(t *testing.T) {
		h := newFailingHarness(t, holdingChild(), realChild())
		h.initialize(0)
		if !h.r.MoveOffHolding() {
			t.Fatal("the move was refused")
		}
		h.answerInitializeReplay(1)
		if h.failed.Load() != 0 {
			t.Fatalf("%d failed starts", h.failed.Load())
		}
		_ = h.shutdown()
	})
	t.Run("the editor leaving", func(t *testing.T) {
		h := newFailingHarness(t, realChild())
		h.initialize(0)
		_ = h.shutdown()
		if h.failed.Load() != 0 {
			t.Fatalf("%d failed starts", h.failed.Load())
		}
	})
}

// A session with no caller to hold prompts, an open or offline one, keeps the old rule: any child that
// dies at once counts. TestProxyGivesUpOnRapidFailures holds it to the cap; this holds it to its words.
func TestWithNoWayToHoldEveryChildStillCounts(t *testing.T) {
	spawns := 0
	factory := func(context.Context) (*Child, error) {
		spawns++
		pr, pw := io.Pipe()
		pw.Close()
		_, inW := io.Pipe()
		return &Child{In: inW, Out: pr, Stop: func() { inW.Close() }}, nil
	}
	clientInR, _ := io.Pipe()
	err := RunWith(t.Context(), clientInR, io.Discard, factory, nil, RunOpts{})
	if err == nil || !strings.Contains(err.Error(), "agent exited 5 times") || spawns != maxRapidFails {
		t.Fatalf("%d spawns, err %v", spawns, err)
	}
}

// The move has a deadline, and this is what it does at the end of it: a request a holding child never
// answered gets the usual error, a held prompt goes to the replacement, and the editor is not stuck.
func TestForceOffHoldingMovesPastWhatTheChildStillOwes(t *testing.T) {
	h := newHoldingHarness(t, nil, nil)
	h.initialize(0)
	h.newSession(0, "S1")
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":9,"method":"session/set_mode","params":{"sessionId":"S1","modeId":"x"}}`)
	readLine(t, h.childIn[0]) // it never answers
	h.readEditor()
	h.prompt("3")
	h.expectEditor("waiting for the box")
	if h.r.MoveOffHolding() {
		t.Fatal("the ordinary move went past what the child still owes")
	}
	if !h.r.ForceOffHolding() {
		t.Fatal("the forced move was refused")
	}
	h.answerReplay(1)
	if line := h.expectEditor(`"id":9`); !strings.Contains(line, "error") {
		t.Fatalf("the unanswered request got %s", line)
	}
	if frame := parse(readLine(t, h.childIn[1])); frame.Method != "session/prompt" || string(frame.ID) != "3" {
		t.Fatalf("the replacement got %q (id %s), want the held prompt", frame.Method, frame.ID)
	}
	if h.r.ForceOffHolding() {
		t.Fatal("a box that runs turns was forced off")
	}
	_ = h.shutdown()
}

// A holding child that never answered initialize at all: the editor's question still gets an answer.
func TestForceOffHoldingAnswersInitializeFromTheReplacement(t *testing.T) {
	h := newHoldingHarness(t, nil, nil)
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	readLine(t, h.childIn[0]) // it hangs
	if !h.r.ForceOffHolding() {
		t.Fatal("the forced move was refused")
	}
	frame := parse(readLine(t, h.childIn[1]))
	writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":`+string(frame.ID)+`,"result":{"protocolVersion":1}}`)
	if answer := parse(readLine(t, h.clientOut)); string(answer.ID) != "1" || len(answer.Result) == 0 {
		t.Fatalf("the editor got %+v", answer)
	}
	_ = h.shutdown()
}

// Serving is what proves a retried box worked: it runs turns and has answered.
func TestServingIsARealChildThatAnswered(t *testing.T) {
	h := newFailingHarness(t, holdingChild(), realChild())
	h.initialize(0)
	if h.r.Serving() {
		t.Fatal("a child that holds prompts counted as serving")
	}
	if !h.r.MoveOffHolding() {
		t.Fatal("the move was refused")
	}
	if h.r.Serving() {
		t.Fatal("a child that has not answered yet counted as serving")
	}
	h.answerInitializeReplay(1)
	wait.For(t, "the real child to serve", h.r.Serving)
	_ = h.shutdown()
	var unbound *Restarter
	if unbound.Serving() || unbound.ForceOffHolding() {
		t.Fatal("an unbound restarter reported something")
	}
}
