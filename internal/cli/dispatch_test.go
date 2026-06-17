package cli

import (
	"strings"
	"testing"
)

func TestNearestCommand(t *testing.T) {
	cmds := append(append([]string{}, topLevelCommands...), "claude", "codex", "gemini")
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"check-secret", "check-secrets", true}, // dropped a trailing char
		{"staus", "status", true},               // a missing char
		{"dctor", "doctor", true},               // a missing char
		{"claud", "claude", true},               // agent name
		{"npm", "", false},                      // too short for a fuzzy match
		{"ls", "", false},                       // too short
		{"make", "", false},                     // a real command, far from any subcommand
		{"python", "", false},                   // ditto
	}
	for _, c := range cases {
		got, ok := nearestCommand(c.in, cmds)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("nearestCommand(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestUnknownCommandErr(t *testing.T) {
	// A typo gets a suggestion plus the box hint carrying the full command line.
	s := unknownCommandErr([]string{"check-secret", "--all"}).Error()
	for _, want := range []string{
		`unknown command "check-secret"`,
		`did you mean "check-secrets"?`,
		"coop run -- check-secret --all",
		"coop help",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("unknownCommandErr missing %q in:\n%s", want, s)
		}
	}
	// A genuine external command gets no noisy suggestion — just the box hint.
	if s := unknownCommandErr([]string{"python"}).Error(); strings.Contains(s, "did you mean") {
		t.Errorf("python should get no suggestion:\n%s", s)
	}
}
