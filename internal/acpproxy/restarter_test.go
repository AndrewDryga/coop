package acpproxy

import (
	"testing"
	"time"
)

// A restart requested from outside the hooks lands only at a quiet moment: not before the child has
// answered initialize, not while a prompt is in flight, and not twice while a replacement replays.
// A prompt sent during that replay waits for the replacement instead of failing.
func TestRestarterRestartsOnlyAQuietChild(t *testing.T) {
	r := &Restarter{}
	if r.Idle() || r.RestartIfIdle() {
		t.Fatal("an unbound restarter reported a quiet proxy")
	}
	h := newProxyHarnessWith(t, 2, nil, RunOpts{Restarter: r})
	if r.RestartIfIdle() {
		t.Fatal("restarted a child that had not answered initialize")
	}
	h.initialize(0)
	h.newSession(0, "S1")

	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}`)
	readLine(t, h.childIn[0])
	if r.Idle() || r.RestartIfIdle() {
		t.Fatal("restarted the child in the middle of a turn")
	}
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`)
	readLine(t, h.clientOut)

	if !r.Idle() || !r.RestartIfIdle() {
		t.Fatal("a quiet proxy refused the restart")
	}
	if r.Idle() || r.RestartIfIdle() {
		t.Fatal("a second restart was accepted while the replacement replays")
	}
	// The editor's side is read concurrently from here: the replay may update it before the held
	// prompt is released.
	editor := make(chan header, 8)
	go func() {
		for {
			line, err := h.clientOut.ReadBytes('\n')
			if err != nil {
				close(editor)
				return
			}
			editor <- parse(line)
		}
	}()
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}`)
	for range 2 {
		frame := parse(readLine(t, h.childIn[1]))
		switch frame.Method {
		case "initialize", "session/load":
			writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":`+string(frame.ID)+`,"result":{}}`)
		default:
			t.Fatalf("replay frame = %q, want initialize then session/load", frame.Method)
		}
	}
	if prompt := parse(readLine(t, h.childIn[1])); prompt.Method != "session/prompt" || string(prompt.ID) != "4" {
		t.Fatalf("the replacement got %q (id %s), want the prompt sent during the replay", prompt.Method, prompt.ID)
	}
	writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":4,"result":{"stopReason":"end_turn"}}`)
	for answered := false; !answered; {
		select {
		case frame, ok := <-editor:
			if !ok {
				t.Fatal("the editor never got the held prompt's answer")
			}
			if string(frame.ID) == "4" {
				if frame.Error != nil {
					t.Fatalf("the held prompt failed: %s", frame.Error)
				}
				answered = true
			}
		case <-time.After(3 * time.Second):
			t.Fatal("the editor never got the held prompt's answer")
		}
	}
	h.shutdown()
	if r.Idle() || r.RestartIfIdle() {
		t.Fatal("a restarter stayed bound after Run returned")
	}
}

// Idle is every in-flight condition at once; each alone keeps a restart away.
func TestRestarterIdleNeedsEveryConditionQuiet(t *testing.T) {
	quiet := func() *proxy {
		return &proxy{child: &Child{}, initResult: []byte(`{}`), generation: 3, initGeneration: 3,
			pending: map[string]bool{}, restartHeld: map[string]clientLine{}, injected: map[string][]byte{},
			forceByID: map[string]*forceChain{}, forceBySess: map[string]*forceChain{}}
	}
	if !quiet().idleLocked() {
		t.Fatal("a proxy with nothing in flight is not idle")
	}
	for name, busy := range map[string]func(*proxy){
		"no live child":            func(p *proxy) { p.child = nil },
		"no initialize answer":     func(p *proxy) { p.initResult = nil },
		"answer from an old child": func(p *proxy) { p.initGeneration = 2 },
		"candidate replaying":      func(p *proxy) { p.candidate = &Child{} },
		"replay tail":              func(p *proxy) { p.restarting = true },
		"reloading":                func(p *proxy) { p.reloading.Store(true) },
		"shutting down":            func(p *proxy) { p.shuttingDown = true },
		"editor request":           func(p *proxy) { p.pending["7"] = true },
		"held prompt":              func(p *proxy) { p.restartHeld["S"] = clientLine{} },
		"coop request":             func(p *proxy) { p.injected["coop-1"] = nil },
		"setting request":          func(p *proxy) { p.forceByID["coop-2"] = &forceChain{} },
		"setting chain":            func(p *proxy) { p.forceBySess["S"] = &forceChain{} },
	} {
		p := quiet()
		busy(p)
		if p.idleLocked() {
			t.Errorf("%s: the proxy read as idle", name)
		}
	}
}
