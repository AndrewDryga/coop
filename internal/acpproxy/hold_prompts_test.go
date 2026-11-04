package acpproxy

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

const heldStatus = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"waiting for the box"}}}}` + "\n"

// holdingHarness runs a proxy whose first child only answers the handshake, and whose second is a
// real box. The editor's output is read by a goroutine, so a test can wait for a frame it expects.
type holdingHarness struct {
	*proxyHarness
	r      *Restarter
	editor chan string
}

func newHoldingHarness(t *testing.T, hooks *Hooks, second *fakeChild) *holdingHarness {
	t.Helper()
	first := newFakeChild()
	first.provider, first.holdPrompts = "codex", true
	if second == nil {
		second = newFakeChild()
		second.provider = "codex"
	}
	if hooks == nil {
		hooks = &Hooks{}
	}
	if hooks.PromptHeld == nil {
		hooks.PromptHeld = func(string) []byte { return []byte(heldStatus) }
	}
	r := &Restarter{}
	h := &holdingHarness{proxyHarness: newProxyHarnessChildren(t, []*fakeChild{first, second}, hooks, RunOpts{Restarter: r}), r: r, editor: make(chan string, 32)}
	return h
}

func (h *holdingHarness) readEditor() {
	go func() {
		for {
			line, err := h.clientOut.ReadBytes('\n')
			if err != nil {
				close(h.editor)
				return
			}
			h.editor <- string(line)
		}
	}()
}

func (h *holdingHarness) expectEditor(want string) string {
	h.t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case line, ok := <-h.editor:
			if !ok {
				h.t.Fatalf("the editor's stream ended before %q", want)
			}
			if strings.Contains(line, want) {
				return line
			}
		case <-timeout:
			h.t.Fatalf("the editor never got %q", want)
		}
	}
}

func (h *holdingHarness) prompt(id string) {
	h.t.Helper()
	writeLine(h.t, h.clientIn, `{"jsonrpc":"2.0","id":`+id+`,"method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}`)
}

// A replacement child answers replay: initialize, then re-creating the turn-less session.
func (h *holdingHarness) answerReplay(child int) {
	h.t.Helper()
	for range 2 {
		frame := parse(readLine(h.t, h.childIn[child]))
		switch frame.Method {
		case "initialize":
			writeLine(h.t, h.children[child].outW, `{"jsonrpc":"2.0","id":`+string(frame.ID)+`,"result":{}}`)
		case "session/new":
			writeLine(h.t, h.children[child].outW, `{"jsonrpc":"2.0","id":`+string(frame.ID)+`,"result":{"sessionId":"S1"}}`)
		default:
			h.t.Fatalf("replay frame = %q, want initialize or session/new", frame.Method)
		}
	}
}

// The promise of the holding child: the editor connects and opens threads through it, and no turn
// ever runs on it. The prompt goes to the box that replaces it, exactly once.
func TestHoldingChildAnswersTheHandshakeAndRunsNoTurn(t *testing.T) {
	h := newHoldingHarness(t, nil, nil)
	if h.r.MoveOffHolding() {
		t.Fatal("moved before the child had answered initialize")
	}
	h.initialize(0)
	h.newSession(0, "S1")
	if !h.r.Holding() {
		t.Fatal("the child does not report that it holds prompts")
	}

	// Anything else still reaches it.
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":9,"method":"session/set_mode","params":{"sessionId":"S1","modeId":"x"}}`)
	if frame := parse(readLine(t, h.childIn[0])); frame.Method != "session/set_mode" {
		t.Fatalf("the holding child got %q, want the settings request", frame.Method)
	}
	if h.r.MoveOffHolding() {
		t.Fatal("moved while the holding child still owed the editor an answer")
	}
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":9,"result":{}}`)
	readLine(t, h.clientOut)

	h.readEditor()
	h.prompt("3")
	h.expectEditor("waiting for the box")
	if h.r.Idle() {
		t.Fatal("a held prompt counts as idle")
	}
	h.r.TellHeld()
	h.expectEditor("waiting for the box")

	if !h.r.MoveOffHolding() {
		t.Fatal("a held prompt stopped the move it is waiting for")
	}
	h.answerReplay(1)
	if frame := parse(readLine(t, h.childIn[1])); frame.Method != "session/prompt" || string(frame.ID) != "3" {
		t.Fatalf("the real box got %q (id %s), want the held prompt", frame.Method, frame.ID)
	}
	if h.r.Holding() || h.r.MoveOffHolding() {
		t.Fatal("the real box reports that it holds prompts")
	}
	writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`)
	h.expectEditor(`"stopReason":"end_turn"`)

	// A prompt now goes straight through.
	h.prompt("4")
	if frame := parse(readLine(t, h.childIn[1])); frame.Method != "session/prompt" || string(frame.ID) != "4" {
		t.Fatalf("the real box got %q (id %s) after the move", frame.Method, frame.ID)
	}
	writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":4,"result":{"stopReason":"end_turn"}}`)
	h.expectEditor(`"stopReason":"end_turn"`)
	_ = h.shutdown()
}

func TestHeldPromptCanBeCancelledAndSentAgain(t *testing.T) {
	h := newHoldingHarness(t, nil, nil)
	h.initialize(0)
	h.newSession(0, "S1")
	h.readEditor()
	h.prompt("3")
	h.expectEditor("waiting for the box")
	h.prompt("4")
	h.expectEditor("already has a prompt waiting")
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"S1"}}`)
	if line := h.expectEditor(`"stopReason":"cancelled"`); !strings.Contains(line, `"id":3`) {
		t.Fatalf("cancelled the wrong prompt: %s", line)
	}
	h.prompt("5")
	h.expectEditor("waiting for the box")
	if !h.r.MoveOffHolding() {
		t.Fatal("the move was refused")
	}
	h.answerReplay(1)
	if frame := parse(readLine(t, h.childIn[1])); string(frame.ID) != "5" {
		t.Fatalf("the real box got prompt %s, want only the one sent after the cancel", frame.ID)
	}
	writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":5,"result":{"stopReason":"end_turn"}}`)
	h.expectEditor(`"stopReason":"end_turn"`)
	_ = h.shutdown()
}

// A thread that closes takes its waiting message with it: answered now, never run on the next box.
func TestClosingAThreadEndsItsHeldPrompt(t *testing.T) {
	for _, method := range []string{"session/close", "session/delete"} {
		t.Run(method, func(t *testing.T) {
			h := newHoldingHarness(t, nil, nil)
			h.initialize(0)
			h.newSession(0, "S1")
			h.readEditor()
			h.prompt("3")
			h.expectEditor("waiting for the box")
			writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":8,"method":"`+method+`","params":{"sessionId":"S1"}}`)
			if frame := parse(readLine(t, h.childIn[0])); frame.Method != method {
				t.Fatalf("the holding child got %q", frame.Method)
			}
			writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":8,"result":{}}`)
			h.expectEditor(`"id":3`)
			if !h.r.Idle() {
				t.Fatal("the closed thread's message still holds the session")
			}
			if !h.r.MoveOffHolding() {
				t.Fatal("the move was refused")
			}
			frame := parse(readLine(t, h.childIn[1]))
			if frame.Method != "initialize" {
				t.Fatalf("the replacement got %q first", frame.Method)
			}
			writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":`+string(frame.ID)+`,"result":{}}`)
			deadline := time.Now().Add(3 * time.Second)
			for !h.r.Idle() {
				if time.Now().After(deadline) {
					t.Fatal("the replacement never finished its replay")
				}
				time.Sleep(5 * time.Millisecond)
			}
			// Whatever the replay sent, this is the next frame: a held prompt would have gone first.
			writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":9,"method":"session/new","params":{"cwd":"/w"}}`)
			if frame := parse(readLine(t, h.childIn[1])); frame.Method != "session/new" {
				t.Fatalf("the replacement got %q: a closed thread's message ran on the next box", frame.Method)
			}
			writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":9,"result":{"sessionId":"S2"}}`)
			h.expectEditor(`"sessionId":"S2"`)
			_ = h.shutdown()
		})
	}
}

// The promise is that a waiting message goes out when the box is ready. A replacement that dies during
// replay was not ready: the message waits for the next one, and the editor hears no error.
func TestHeldPromptSurvivesAReplacementThatDies(t *testing.T) {
	third := newFakeChild()
	third.provider = "codex"
	second := newFakeChild()
	second.provider = "codex"
	first := newFakeChild()
	first.provider, first.holdPrompts = "codex", true
	r := &Restarter{}
	hooks := &Hooks{PromptHeld: func(string) []byte { return []byte(heldStatus) }}
	h := &holdingHarness{proxyHarness: newProxyHarnessChildren(t, []*fakeChild{first, second, third}, hooks, RunOpts{Restarter: r}), r: r, editor: make(chan string, 32)}
	h.initialize(0)
	h.newSession(0, "S1")
	h.readEditor()
	h.prompt("3")
	h.expectEditor("waiting for the box")
	if !r.MoveOffHolding() {
		t.Fatal("the move was refused")
	}
	if frame := parse(readLine(t, h.childIn[1])); frame.Method != "initialize" {
		t.Fatalf("the replacement got %q", frame.Method)
	}
	second.outW.Close() // it dies before it answers
	h.answerReplay(2)
	if frame := parse(readLine(t, h.childIn[2])); frame.Method != "session/prompt" || string(frame.ID) != "3" {
		t.Fatalf("the third child got %q (id %s), want the held prompt", frame.Method, frame.ID)
	}
	writeLine(t, h.children[2].outW, `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`)
	if line := h.expectEditor(`"id":3`); !strings.Contains(line, "end_turn") {
		t.Fatalf("the held prompt was answered with %s", line)
	}
	_ = h.shutdown()
}

// A prompt with no id to answer, or a batch that could hide one, has nothing to hold it by.
func TestFramesThatCouldStartATurnNeverReachAHoldingChild(t *testing.T) {
	for name, frame := range map[string]string{
		"a prompt with no id": `{"jsonrpc":"2.0","method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}`,
		"a batch":             `[{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			in := &recordingWriteCloser{}
			var editor bytes.Buffer
			p := &proxy{out: &editor, child: &Child{In: in, HoldPrompts: true},
				sessions: map[string]*sess{"S1": {adapterID: "native"}}, restartHeld: map[string]clientLine{},
				pending: map[string]bool{}, sessionReqs: map[string]sessionRequest{}}
			p.fromClient([]byte(frame + "\n"))
			if in.Len() != 0 {
				t.Fatalf("the holding child got %s", in.String())
			}
		})
	}
}

// The last line of the promise: whatever path a prompt takes, it does not reach a holding child.
func TestSyntheticPromptNeverReachesAHoldingChild(t *testing.T) {
	in := &recordingWriteCloser{}
	var editor bytes.Buffer
	p := &proxy{
		out: &editor, child: &Child{In: in, HoldPrompts: true},
		sessions:    map[string]*sess{"S1": {adapterID: "native"}},
		restartHeld: map[string]clientLine{},
		pending:     map[string]bool{}, sessionReqs: map[string]sessionRequest{},
	}
	p.forwardClient([]byte(`{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}`+"\n"), originSynthetic)
	if in.Len() != 0 {
		t.Fatalf("the holding child got %s", in.String())
	}
	if held, ok := p.restartHeld["S1"]; !ok || held.origin != originSynthetic {
		t.Fatalf("the prompt was not held as it came: %+v (held: %v)", held, ok)
	}
}

// A prompt for a thread this session does not know has nothing to be held for. It must not fall
// through to the child: that would run a turn on it.
func TestHoldingChildNeverGetsAPromptForAnUnknownThread(t *testing.T) {
	h := newHoldingHarness(t, nil, nil)
	h.initialize(0)
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"never-opened","prompt":[]}}`)
	if line := readLine(t, h.clientOut); !strings.Contains(string(line), "not open yet") {
		t.Fatalf("the editor got %s", line)
	}
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w"}}`)
	if frame := parse(readLine(t, h.childIn[0])); frame.Method != "session/new" {
		t.Fatalf("the child got %q: an earlier prompt reached it", frame.Method)
	}
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"S1"}}`)
	readLine(t, h.clientOut)
	_ = h.shutdown()
}

// While the project's box still cannot start, a provider switch lands on another holding child. The
// prompt stays held across it, and still no turn runs.
func TestPromptStaysHeldAcrossAnotherHoldingChild(t *testing.T) {
	second := newFakeChild()
	second.provider, second.holdPrompts = "codex", true
	h := newHoldingHarness(t, nil, second)
	h.initialize(0)
	h.newSession(0, "S1")
	h.readEditor()
	h.prompt("3")
	h.expectEditor("waiting for the box")
	if !h.r.MoveOffHolding() {
		t.Fatal("the move was refused")
	}
	h.answerReplay(1)
	h.expectEditor("waiting for the box") // the prompt came back through the hold, and said so
	if !h.r.Holding() {
		t.Fatal("the replacement does not hold prompts")
	}
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":9,"method":"session/set_mode","params":{"sessionId":"S1","modeId":"x"}}`)
	if frame := parse(readLine(t, h.childIn[1])); frame.Method != "session/set_mode" {
		t.Fatalf("the holding replacement got %q before the settings request: a prompt reached it", frame.Method)
	}
	writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":9,"result":{}}`)
	_ = h.shutdown()
}

// A resend (a turn a restart interrupted) waits for a child that can run it, and goes out once.
func TestResendWaitsForAChildThatRunsTurns(t *testing.T) {
	resend := []byte(`{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}` + "\n")
	var armed func() bool
	third := newFakeChild()
	third.provider = "codex"
	first := newFakeChild()
	first.provider = "codex"
	second := newFakeChild()
	second.provider, second.holdPrompts = "codex", true
	r := &Restarter{}
	forwarded := 0
	hooks := &Hooks{
		ResumePrompt: func(string) []byte {
			if armed != nil && armed() {
				return resend
			}
			return nil
		},
		PromptForwarded: func(_ []byte, synthetic bool) {
			if synthetic {
				forwarded++
				armed = nil
			}
		},
		PromptHeld: func(string) []byte { return []byte(heldStatus) },
	}
	h := &holdingHarness{proxyHarness: newProxyHarnessChildren(t, []*fakeChild{first, second, third}, hooks, RunOpts{Restarter: r}), r: r, editor: make(chan string, 32)}
	h.initialize(0)
	h.newSession(0, "S1")
	h.readEditor()
	armed = func() bool { return true }
	if !r.RestartIfIdle() {
		t.Fatal("the idle restart was refused")
	}
	h.answerReplay(1)
	wait := time.Now().Add(3 * time.Second)
	for !r.Holding() {
		if time.Now().After(wait) {
			t.Fatal("the holding child never went live")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Traffic after the replay's tail: if a resend had gone out, this child would read it first.
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":9,"method":"session/set_mode","params":{"sessionId":"S1","modeId":"x"}}`)
	if frame := parse(readLine(t, h.childIn[1])); frame.Method != "session/set_mode" {
		t.Fatalf("the holding child got %q before the settings request: a resend reached it", frame.Method)
	}
	writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":9,"result":{}}`)
	h.expectEditor(`"id":9`)
	for !r.MoveOffHolding() {
		if time.Now().After(wait) {
			t.Fatal("a reserved resend stopped the move it waits for")
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.answerReplay(2)
	if frame := parse(readLine(t, h.childIn[2])); frame.Method != "session/prompt" || string(frame.ID) != "7" {
		t.Fatalf("the real box got %q (id %s), want the resend", frame.Method, frame.ID)
	}
	writeLine(t, h.children[2].outW, `{"jsonrpc":"2.0","id":7,"result":{"stopReason":"end_turn"}}`)
	h.expectEditor(`"stopReason":"end_turn"`)
	_ = h.shutdown()
}
