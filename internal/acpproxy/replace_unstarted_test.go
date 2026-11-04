package acpproxy

import (
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// A real child waiting to start has answered nothing, so nothing is lost replacing it. The editor's
// initialize gets the replacement's answer, and a person's wait is no failed start.
func TestReplaceUnstartedAnswersInitializeFromTheReplacement(t *testing.T) {
	h := newFailingHarness(t, realChild(), holdingChild())
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	readLine(t, h.childIn[0]) // it is still waiting to start, so it never answers
	if !h.r.ReplaceUnstarted() {
		t.Fatal("the replacement was refused")
	}
	frame := parse(readLine(t, h.childIn[1]))
	if frame.Method != "initialize" {
		t.Fatalf("the replacement got %q", frame.Method)
	}
	writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":`+string(frame.ID)+`,"result":{"protocolVersion":1}}`)
	if answer := parse(readLine(t, h.clientOut)); string(answer.ID) != "1" || len(answer.Result) == 0 {
		t.Fatalf("the editor got %+v", answer)
	}
	h.newSession(1, "S1")
	if !h.r.Holding() || h.failed.Load() != 0 {
		t.Fatalf("holding=%v, %d failed starts", h.r.Holding(), h.failed.Load())
	}
	_ = h.shutdown()
}

// A replacement stuck the same way is still replaying when its word arrives, with threads open: left
// alone it would end the session when the replay's grace ran out.
func TestReplaceUnstartedSupersedesAReplacementStillReplaying(t *testing.T) {
	h := newFailingHarness(t, realChild(), realChild(), holdingChild())
	h.initialize(0)
	h.newSession(0, "S1")
	if !h.r.RestartIfIdle() {
		t.Fatal("the switch was refused")
	}
	readLine(t, h.childIn[1]) // the replay's initialize, which it never answers
	if !h.r.ReplaceUnstarted() {
		t.Fatal("the replacement was refused")
	}
	go drainReader(h.clientOut) // the end of the replay tells the thread its settings
	h.answerReplay(2)
	wait.For(t, "the session to reach the holding child", h.r.Holding)
	if h.failed.Load() != 0 {
		t.Fatalf("%d failed starts", h.failed.Load())
	}
	_ = h.shutdown()
}

// It never touches a child that answered: a word that arrives late must not end a running session.
func TestReplaceUnstartedLeavesAChildThatAnswered(t *testing.T) {
	var unbound *Restarter
	if unbound.ReplaceUnstarted() {
		t.Fatal("an unbound restarter replaced something")
	}
	unbound.TellOpen("nobody hears this")
	real := newFailingHarness(t, realChild())
	real.initialize(0)
	if real.r.ReplaceUnstarted() {
		t.Fatal("replaced a child that answered initialize")
	}
	_ = real.shutdown()
	holding := newFailingHarness(t, holdingChild())
	if holding.r.ReplaceUnstarted() {
		t.Fatal("replaced the child that only holds prompts")
	}
	_ = holding.shutdown()
}

// A wait can begin with every thread idle, so each open one hears it now, not when it next sends.
func TestTellOpenReachesEveryOpenThread(t *testing.T) {
	h := newFailingHarness(t, realChild())
	h.initialize(0)
	h.newSession(0, "S1")
	h.newSession(0, "S2")
	h.newSession(0, "S3")
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":9,"method":"session/close","params":{"sessionId":"S2"}}`)
	readLine(t, h.childIn[0])
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":9,"result":{}}`)
	readLine(t, h.clientOut)
	go h.r.TellOpen("a box is in the way") // the write finishes only as the editor's end reads it
	for _, session := range []string{"S1", "S3"} {
		line := string(readLine(t, h.clientOut))
		if !strings.Contains(line, `"sessionId":"`+session+`"`) || !strings.Contains(line, "a box is in the way") {
			t.Fatalf("thread %s heard %s", session, line)
		}
	}
	_ = h.shutdown()
}
