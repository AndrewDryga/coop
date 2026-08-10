package ui

import (
	"strings"
	"testing"
)

// DestroyGate proceeds with --yes; without it in a non-TTY (the test env) it refuses and names --yes,
// so a pipe/CI can't delete on its own. (The TTY default-No prompt path needs a terminal to exercise.)
func TestDestroyGate(t *testing.T) {
	if err := DestroyGate("delete X", true); err != nil {
		t.Errorf("DestroyGate(yes) = %v, want nil (proceed)", err)
	}
	if err := DestroyGate("delete task Y (todo)", false); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("DestroyGate(no, piped) = %v, want a refusal naming --yes", err)
	}
	var prompt string
	ask := func(got string) bool {
		prompt = got
		return ConfirmationResponse("yes", false)
	}
	if err := DestroyGate("delete task Z", false, ask); err != nil {
		t.Errorf("DestroyGate(injected yes) = %v, want nil (proceed)", err)
	}
	if prompt != "delete task Z? this can't be undone" {
		t.Errorf("DestroyGate prompt = %q", prompt)
	}
	if err := DestroyGate("delete task Z", false, func(string) bool {
		return ConfirmationResponse("", false)
	}); err == nil || err.Error() != "cancelled" {
		t.Errorf("DestroyGate(injected default No) = %v, want cancelled", err)
	}
	// More than one callback is a programming error, not a silent pick-the-first.
	if err := DestroyGate("delete task Z", false, ask, ask); err == nil {
		t.Error("DestroyGate(two callbacks) = nil, want an error")
	}
}

// A bare Enter takes the caller's default — No for a delete, Yes only where the caller says so —
// and anything that isn't y/yes is a No.
func TestConfirmationResponse(t *testing.T) {
	for _, tc := range []struct {
		resp string
		def  bool
		want bool
	}{
		{"", false, false},
		{"", true, true},
		{"y", false, true},
		{"YES", false, true},
		{" yes \n", false, true},
		{"n", true, false},
		{"nope", true, false},
	} {
		if got := ConfirmationResponse(tc.resp, tc.def); got != tc.want {
			t.Errorf("ConfirmationResponse(%q, %v) = %v, want %v", tc.resp, tc.def, got, tc.want)
		}
	}
}
