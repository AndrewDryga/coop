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
		{"task", "tasks", true},                 // a missing char
		{"dctor", "doctor", true},               // a missing char
		{"claud", "claude", true},               // agent name
		{"lop", "loop", true},                   // 3 runes at distance 1 — the floor allows it
		{"npm", "", false},                      // 3 runes, but no command within 1 edit
		{"ls", "", false},                       // 1-2 runes: suggestion-free (routes to the run-in-box hint)
		{"cp", "", false},                       // ditto
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

func TestCommandHelpCoversSubcommands(t *testing.T) {
	// `coop <cmd> --help` routes to focused help. `run` forwards --help to the box command,
	// `fork` has its own forkHelp, and help/version are the global help — every other
	// top-level command must carry a commandHelp entry whose synopsis names it.
	exempt := map[string]bool{"run": true, "fork": true, "help": true, "version": true}
	for _, name := range topLevelCommands {
		if exempt[name] {
			continue
		}
		h, ok := commandHelp[name]
		if !ok {
			t.Errorf("no commandHelp for subcommand %q (add focused help, or exempt it)", name)
			continue
		}
		first := h
		if i := strings.IndexByte(h, '\n'); i >= 0 {
			first = h[:i]
		}
		if !strings.Contains(first, "coop "+name) {
			t.Errorf("commandHelp[%q] synopsis %q should name the command", name, first)
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
