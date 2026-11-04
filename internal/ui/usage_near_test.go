package ui

import "testing"

func TestCorrectedCommandKeepsWhatWasTyped(t *testing.T) {
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'a'
	}
	for _, tc := range []struct {
		args, want []string
		typed      bool
	}{
		{args: []string{"tasks", "done", "x", "--forse"}, want: []string{"coop tasks done x --force"}, typed: true},
		{args: []string{"tasks", "done", "--forse=1", "x"}, want: []string{"coop tasks done --force=1 x"}, typed: true},
		{args: []string{"tasks", "add", "two words", "it's", "--forse"}, want: []string{`coop tasks add 'two words' 'it'\''s' --force`}, typed: true},
		{args: []string{"tasks", "add", "", "--forse"}, want: []string{`coop tasks add '' --force`}, typed: true},
		{args: []string{"--forse", "--forse"}, want: []string{"coop --force --forse"}, typed: true},
		// Nothing a person could paste, or nothing worth repeating: the caller points at help.
		{args: []string{"tasks", "add", "two\nlines", "--forse"}, want: []string{""}, typed: true},
		{args: []string{"tasks", "add", string(long), "--forse"}, want: []string{""}, typed: true},
		// Not on the line at all, as when a test calls a parser directly.
		{args: []string{"-test.v"}, want: []string{""}, typed: false},
	} {
		got, typed := correctedCommand(tc.args, "--forse", "--force")
		if got != tc.want[0] || typed != tc.typed {
			t.Errorf("correctedCommand(%q) = %q, %t; want %q, %t", tc.args, got, typed, tc.want[0], tc.typed)
		}
	}
}
