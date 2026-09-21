package acpproxy

import (
	"sync/atomic"
	"testing"
	"time"
)

// The editor blocks on initialize and on nothing else at startup, so work that only a LATER request
// needs must not run beside it. Coop uses this hook to defer warming other providers: filling the
// pool alongside the lead's own box start measurably slowed the one answer an editor waits for
// (~0.97 s with the fan-out versus ~0.81 s without, on an otherwise identical path).
//
// The contract this test pins: the hook fires AFTER the editor has its answer, and exactly once.
func TestInitializedFiresAfterTheEditorIsAnswered(t *testing.T) {
	var fired atomic.Int64
	h := newProxyHarness(t, 1, &Hooks{Initialized: func() { fired.Add(1) }})

	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	readLine(t, h.childIn[0])
	// The child answers, but the editor is NOT read yet. The proxy's write to the editor blocks
	// until it is, which is what makes this deterministic rather than a sleep race: while the answer
	// is undelivered, the hook must not have fired.
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	time.Sleep(100 * time.Millisecond)
	if got := fired.Load(); got != 0 {
		t.Fatalf("Initialized fired %d times while the editor's answer was still undelivered", got)
	}

	readLine(t, h.clientOut) // now the editor has it
	deadline := time.After(5 * time.Second)
	for fired.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("Initialized never fired; deferred work would wait for the fallback timer instead")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	_ = h.shutdown()
	if got := fired.Load(); got != 1 {
		t.Errorf("Initialized fired %d times, want exactly 1", got)
	}
}

// A failed initialize is not an answer, so the deferred work must NOT be released by it — the
// caller's own fallback decides what to do about a lead that cannot answer.
func TestInitializedDoesNotFireOnAFailedInitialize(t *testing.T) {
	var fired atomic.Int64
	h := newProxyHarness(t, 1, &Hooks{Initialized: func() { fired.Add(1) }})
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	readLine(t, h.childIn[0])
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"no"}}`)
	readLine(t, h.clientOut)
	time.Sleep(150 * time.Millisecond)
	_ = h.shutdown()
	if got := fired.Load(); got != 0 {
		t.Errorf("Initialized fired %d times on a failed initialize, want 0", got)
	}
}

// A nil hook is pure pass-through, as every other hook is.
func TestInitializedIsOptional(t *testing.T) {
	h := newProxyHarness(t, 1, &Hooks{})
	h.initialize(0)
	_ = h.shutdown()
}
