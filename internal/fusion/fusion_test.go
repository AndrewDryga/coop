package fusion

import (
	"slices"
	"strings"
	"testing"
)

var agents = []string{"claude", "codex", "gemini"}

func TestValid(t *testing.T) {
	for _, ok := range agents {
		if !Valid(ok, agents) {
			t.Errorf("Valid(%q) = false, want true", ok)
		}
	}
	if Valid("gpt", agents) {
		t.Error("Valid(\"gpt\") = true, want false")
	}
}

func TestPeers(t *testing.T) {
	cases := map[string][]string{
		"codex":  {"claude", "gemini"},
		"claude": {"codex", "gemini"},
		"gemini": {"claude", "codex"},
	}
	for governor, want := range cases {
		if got := Peers(governor, agents); !slices.Equal(got, want) {
			t.Errorf("Peers(%q) = %v, want %v", governor, got, want)
		}
	}
}

func TestPeerCmd(t *testing.T) {
	cases := map[string][]string{
		"claude": {"claude", "-p", "--permission-mode", "plan", "Q"},
		"gemini": {"gemini", "--approval-mode", "plan", "-p", "Q"},
		"codex":  {"codex", "exec", "-s", "read-only", "Q"},
	}
	for tool, want := range cases {
		if got := peerCmd(tool, "Q"); !slices.Equal(got, want) {
			t.Errorf("peerCmd(%q) = %v, want %v", tool, got, want)
		}
	}
	if peerCmd("gpt", "Q") != nil {
		t.Error("peerCmd of an unknown tool should be nil")
	}
}

func TestInstructionConsultsPeersNotGovernor(t *testing.T) {
	ins := Instruction("codex", Peers("codex", agents))
	// Names both peers' read-only commands.
	for _, want := range []string{
		"claude -p --permission-mode plan",
		"gemini --approval-mode plan -p",
	} {
		if !strings.Contains(ins, want) {
			t.Errorf("instruction missing peer command %q", want)
		}
	}
	// Must NOT tell codex (the governor) to consult itself.
	if strings.Contains(ins, "codex exec -s read-only") {
		t.Error("instruction tells the governor to consult itself")
	}
	// Has the synthesis guidance and names the governor.
	for _, want := range []string{"Synthesize", "governor", "codex"} {
		if !strings.Contains(ins, want) {
			t.Errorf("instruction missing %q", want)
		}
	}
}

func TestGovernorInstructionsPreservesBase(t *testing.T) {
	base := "# Project rules\nAlways run the gate."
	out := GovernorInstructions(base, "codex", agents)
	if !strings.Contains(out, base) {
		t.Error("base instructions were dropped")
	}
	// Fusion block comes first (prominence), base after it.
	if i, j := strings.Index(out, "Fusion mode"), strings.Index(out, "Project rules"); i < 0 || j < 0 || i > j {
		t.Errorf("expected fusion block before base: fusion@%d base@%d", i, j)
	}
	// Empty base → just the block, no trailing junk.
	if out := GovernorInstructions("  \n ", "claude", agents); !strings.HasPrefix(out, "# Fusion mode") {
		t.Errorf("empty base should yield just the block, got prefix %q", out[:min(20, len(out))])
	}
}

func TestLeadInstructions(t *testing.T) {
	// No peers → the base is returned unchanged (nothing to consult).
	if got := LeadInstructions("BASE", nil); got != "BASE" {
		t.Errorf("no peers: got %q, want %q", got, "BASE")
	}
	// With peers → an optional directive that spells out each peer's exact
	// read-only command, naming only those peers, with the base kept.
	out := LeadInstructions("BASE", []string{"codex", "gemini"})
	for _, want := range []string{"second opinion", "codex exec -s read-only", "gemini --approval-mode plan -p", "BASE"} {
		if !strings.Contains(out, want) {
			t.Errorf("LeadInstructions missing %q in:\n%s", want, out)
		}
	}
	// Names only the peers passed in, and stays optional — not the mandatory
	// fusion directive ("you never answer alone").
	if strings.Contains(out, "claude") {
		t.Error("consult should name only the peers passed in")
	}
	if strings.Contains(out, "never answer alone") {
		t.Error("consult must stay optional, not the mandatory fusion directive")
	}
}
