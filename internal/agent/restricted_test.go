package agent

import (
	"slices"
	"strings"
	"testing"
)

func TestParseExecutionMode(t *testing.T) {
	for _, mode := range []ExecutionMode{ModeNormal, ModeReadOnly, ModeBare} {
		if got, err := ParseExecutionMode(string(mode)); err != nil || got != mode {
			t.Errorf("ParseExecutionMode(%q) = %q, %v", mode, got, err)
		}
	}
	for _, bad := range []string{"", "read-only", "Bare", "normal "} {
		if _, err := ParseExecutionMode(bad); err == nil {
			t.Errorf("ParseExecutionMode(%q) accepted", bad)
		}
	}
	if ModeNormal.Restricted() || !ModeReadOnly.Restricted() || !ModeBare.Restricted() {
		t.Error("Restricted must be true for exactly readonly and bare")
	}
}

// Claude's restricted command: the seeded user settings only, no MCP, and for bare no built-in
// tool — with the empty --tools value closed by a boolean flag and everything kept before a `--`.
func TestClaudeRestrictedCommand(t *testing.T) {
	base := []string{"claude", "--dangerously-skip-permissions", "--model", "opus"}
	got, err := claudeAgent{}.RestrictedCommand(ModeReadOnly, base)
	want := append(slices.Clone(base), "--strict-mcp-config", "--setting-sources", "user")
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("readonly = %q, %v; want %q", got, err, want)
	}
	got, err = claudeAgent{}.RestrictedCommand(ModeBare, append(slices.Clone(base), "-p", "--", "explain"))
	want = []string{"claude", "--dangerously-skip-permissions", "--model", "opus", "-p", "--tools", "", "--append-system-prompt", claudeBareSystemPrompt, "--strict-mcp-config", "--setting-sources", "user", "--", "explain"}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("bare = %q, %v; want %q", got, err, want)
	}
	if got, err := (claudeAgent{}).RestrictedCommand(ModeNormal, base); err != nil || !slices.Equal(got, base) {
		t.Errorf("normal must pass through: %q, %v", got, err)
	}
	if len(base) != 4 {
		t.Fatal("RestrictedCommand mutated its input")
	}
}

// A caller flag that hands back what the mode took away is refused by name, in either spelling.
func TestClaudeRestrictedCommandRefusesConflicts(t *testing.T) {
	cases := map[ExecutionMode][]string{
		ModeReadOnly: {"--mcp-config", "--settings", "--setting-sources", "--plugin-dir"},
		ModeBare:     {"--mcp-config", "--settings", "--setting-sources", "--plugin-dir", "--tools", "--allowedTools", "--allowed-tools"},
	}
	for mode, flags := range cases {
		for _, flag := range flags {
			for _, cmd := range [][]string{{"claude", flag, "x"}, {"claude", flag + "=x"}} {
				if _, err := (claudeAgent{}).RestrictedCommand(mode, cmd); err == nil || !strings.Contains(err.Error(), flag) {
					t.Errorf("%s: %q accepted or misnamed: %v", mode, cmd, err)
				}
			}
		}
	}
	// A tool set is the readonly run's own business; only bare forbids it.
	if _, err := (claudeAgent{}).RestrictedCommand(ModeReadOnly, []string{"claude", "--tools", "Read"}); err != nil {
		t.Errorf("readonly refused a tool selection: %v", err)
	}
}

// No other adapter is qualified: each refuses both restricted modes and passes normal through.
func TestUnqualifiedAdaptersRefuseRestrictedModes(t *testing.T) {
	for _, name := range Names() {
		if name == "claude" {
			continue
		}
		ag, _ := Get(name)
		cmd := []string{name, "--flag"}
		if got, err := ag.RestrictedCommand(ModeNormal, cmd); err != nil || !slices.Equal(got, cmd) {
			t.Errorf("%s normal: %q, %v", name, got, err)
		}
		for _, mode := range []ExecutionMode{ModeReadOnly, ModeBare} {
			if _, err := ag.RestrictedCommand(mode, cmd); err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), string(mode)) {
				t.Errorf("%s %s: want a refusal naming both, got %v", name, mode, err)
			}
		}
	}
}
