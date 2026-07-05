package taskstate

import (
	"slices"
	"testing"
)

// TestBacklogOutsideAll pins the load-bearing invariant: Backlog must NOT be in All. readTaskTree,
// every counter, the loop's done-check, and the Stop hook all walk All — if Backlog leaked in, the
// loop would start working un-promoted ideas and the hook would nag about them. This test fails the
// build the moment someone "helpfully" appends Backlog to All.
func TestBacklogOutsideAll(t *testing.T) {
	if slices.Contains(All, Backlog) {
		t.Fatalf("Backlog (%q) must stay OUT of All %v — it's a staging drawer, not a lifecycle state", Backlog, All)
	}
	// The four lifecycle states, on the other hand, must all be present and in order.
	if want := []string{Todo, InProgress, Blocked, Done}; !slices.Equal(All, want) {
		t.Fatalf("All = %v, want the four lifecycle states in order %v", All, want)
	}
}
