package forkctl

import (
	"errors"
	"strings"
	"testing"
)

// A fork whose loop ended with nothing to take says why in coop fork ls, beside the idle fork. The
// note belongs to the loop that wrote it: the worker's clean exit keeps it, a new loop or an agent
// that runs in the fork clears it, another generation's is not shown, and a failure outranks it.
func TestForkLsShowsWhyAnIdleForksLoopEnded(t *testing.T) {
	repo, ws, identity, control := forkLoopFixture(t)
	const reason = "No tasks ready to work on."
	shows := func(text string) bool { return strings.Contains(text, "    Last loop: "+reason+"\n") }

	NoteForkLoopEnd(ws, identity, reason)
	text, stderr, status := readForkLoopListing(t, control)
	if stderr != "" || status.State != "idle" || !strings.Contains(text, "perf · idle") || !shows(text) {
		t.Fatalf("an idle fork whose loop took no task: status %+v, stderr %q, listing:\n%s", status, stderr, text)
	}

	// The worker records its exit after writing the note; a clean exit is no failure to clear.
	NoteForkLoopExit(repo, identity, 0, nil)
	if text, _, _ = readForkLoopListing(t, control); !shows(text) {
		t.Errorf("the worker's clean exit dropped the note:\n%s", text)
	}

	// A failure is what the fork shows, in place of the note.
	NoteForkLoopExit(repo, identity, 1, errors.New("provider refused"))
	if text, _, status = readForkLoopListing(t, control); status.State != "failed" || shows(text) || !strings.Contains(text, "Loop failed: provider refused") {
		t.Errorf("a failed loop beside the note: status %+v, listing:\n%s", status, text)
	}

	// A new loop, or an agent that ran, starts a new result.
	if err := ClearForkLoopError(ws); err != nil {
		t.Fatal(err)
	}
	if text, _, status = readForkLoopListing(t, control); status.State != "idle" || shows(text) || strings.Contains(text, "Last loop:") {
		t.Errorf("a cleared fork still shows a loop result: status %+v, listing:\n%s", status, text)
	}

	// A note another generation of the fork wrote is not this one's.
	other := identity
	other.Generation = "00000000000000000000000000000000"
	NoteForkLoopEnd(ws, other, reason)
	if text, _, _ = readForkLoopListing(t, control); strings.Contains(text, "Last loop:") {
		t.Errorf("a note of another generation is shown:\n%s", text)
	}

	// What an agent can write there is bounded and escaped like the failure note.
	NoteForkLoopEnd(ws, identity, "\x1b[2J"+strings.Repeat("界", 400))
	if text, _, _ = readForkLoopListing(t, control); strings.Contains(text, "\x1b") || !strings.Contains(text, "Last loop: ") {
		t.Errorf("the note was not bounded and escaped:\n%q", text)
	}
}
