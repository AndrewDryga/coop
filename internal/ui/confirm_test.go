package ui

import (
	"io"
	"os"
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

// confirmAnswer points Confirm's reader seam at s for one test, restoring the real (nil → tty)
// path afterwards.
func confirmAnswer(t *testing.T, s string) {
	t.Helper()
	old := confirmInput
	confirmInput = strings.NewReader(s)
	t.Cleanup(func() { confirmInput = old })
}

// captureStderr returns whatever fn wrote to os.Stderr — here, Confirm's prompt.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	return string(out)
}

// Confirm's answer handling, driven through the reader seam because the branches below are
// unreachable without a terminal. A bare Enter and an EOF (a closed or empty input) both take the
// caller's default — which for every destructive verb is No — and the prompt shows which default
// is armed, so the user can see it before pressing Enter.
func TestConfirmReadsTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		input  string
		def    bool
		want   bool
		prompt string
	}{
		// The question mark belongs to the caller's text (DestroyGate supplies its own); Confirm
		// adds only the hint that names the armed default.
		{"bare Enter takes the default No", "\n", false, false, "delete X [y/N] "},
		{"bare Enter takes an affirmative default", "\n", true, true, "delete X [Y/n] "},
		{"explicit yes", "y\n", false, true, "delete X [y/N] "},
		{"explicit spelled-out yes", "YES\n", false, true, "delete X [y/N] "},
		{"explicit no overrides a Yes default", "n\n", true, false, "delete X [Y/n] "},
		{"empty input (EOF) takes the default", "", false, false, "delete X [y/N] "},
		{"EOF cannot flip an affirmative default", "", true, true, "delete X [Y/n] "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			confirmAnswer(t, tc.input)
			var got bool
			out := captureStderr(t, func() { got = Confirm("delete X", tc.def) })
			if got != tc.want {
				t.Errorf("Confirm(%q, def=%v) = %v, want %v", tc.input, tc.def, got, tc.want)
			}
			if out != tc.prompt {
				t.Errorf("Confirm prompt = %q, want %q", out, tc.prompt)
			}
		})
	}
}

// A closed pipe is the hang-up case (the terminal went away mid-question): it reads as EOF, so
// Confirm takes the default instead of blocking or treating the silence as consent.
func TestConfirmOnClosedInput(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	defer r.Close()
	old := confirmInput
	confirmInput = r
	t.Cleanup(func() { confirmInput = old })

	var got bool
	captureStderr(t, func() { got = Confirm("delete X", false) })
	if got {
		t.Error("Confirm on closed input = true, want the default No")
	}
}

// The seam is nil in production, and this test env has no tty — so Confirm still short-circuits to
// the default WITHOUT prompting, exactly as it did before the seam existed. That silence is the
// proof the interactive path is untouched: nothing is read, nothing is printed.
func TestConfirmWithoutTTYDoesNotPrompt(t *testing.T) {
	if confirmInput != nil {
		t.Fatal("confirmInput must be nil outside a test that sets it")
	}
	for _, def := range []bool{false, true} {
		var got bool
		out := captureStderr(t, func() { got = Confirm("delete X", def) })
		if got != def {
			t.Errorf("Confirm(no tty, def=%v) = %v, want the default", def, got)
		}
		if out != "" {
			t.Errorf("Confirm(no tty) printed %q, want nothing", out)
		}
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
