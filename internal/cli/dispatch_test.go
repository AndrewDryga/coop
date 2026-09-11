package cli

import (
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

func TestNearestCommand(t *testing.T) {
	cmds := append(append([]string{}, topLevelCommands...), agents.Names()...)
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
	// A typo names the FULL rejected command (including coop, so it doesn't read as a rejected
	// shell command) and corrects it to a whole command, not a bare token. There is no
	// run-in-the-box fallback: a mistyped coop command is not a request to run a program.
	s := unknownCommandErr([]string{"check-secret"}, false).Error()
	for _, want := range []string{`Unknown command "coop check-secret"`, "Did you mean: coop check-secrets"} {
		if !strings.Contains(s, want) {
			t.Errorf("unknownCommandErr missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "coop run -- ") {
		t.Errorf("a rejected command must not be re-offered as a box command:\n%s", s)
	}
	// A genuine external command gets no noisy suggestion — just the pointer to the command list.
	s = unknownCommandErr([]string{"python"}, false).Error()
	if strings.Contains(s, "Did you mean") || !strings.Contains(s, "See available commands: coop help") {
		t.Errorf("python should get the command-list pointer, no suggestion:\n%s", s)
	}
	// A help request keeps its intent: the correction is itself a help command.
	if s := unknownCommandErr([]string{"doctro"}, true).Error(); !strings.Contains(s, "Did you mean: coop help doctor") {
		t.Errorf("a help request should be corrected to a help command:\n%s", s)
	}
}
