package fusion

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var allAgents = []string{"claude", "codex", "gemini"}

// TestConsultWrapperShellcheck keeps the embedded coop-consult script clean. It's a Go
// string constant, so the normal shellcheck pass can't see it; run it here when
// shellcheck is available (skipped otherwise, so CI without it still passes).
func TestConsultWrapperShellcheck(t *testing.T) {
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck not installed")
	}
	f := filepath.Join(t.TempDir(), "coop-consult")
	if err := os.WriteFile(f, []byte(ConsultWrapper), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(sc, f).CombinedOutput(); err != nil {
		t.Errorf("shellcheck flagged the consult wrapper:\n%s", out)
	}
}

func TestValid(t *testing.T) {
	for _, ok := range allAgents {
		if !Valid(ok, allAgents) {
			t.Errorf("Valid(%q) = false, want true", ok)
		}
	}
	if Valid("gpt", allAgents) {
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
		if got := Peers(governor, allAgents); !slices.Equal(got, want) {
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
	ins := Instruction("codex", Peers("codex", allAgents))
	// Names both peers via the coop-consult wrapper.
	for _, want := range []string{
		"coop-consult claude --fresh",
		"coop-consult gemini --fresh",
	} {
		if !strings.Contains(ins, want) {
			t.Errorf("instruction missing peer consult %q", want)
		}
	}
	// Must NOT tell codex (the governor) to consult itself.
	if strings.Contains(ins, "coop-consult codex") {
		t.Error("instruction tells the governor to consult itself")
	}
	// Has the synthesis guidance and names the governor.
	for _, want := range []string{"Synthesize", "governor", "codex"} {
		if !strings.Contains(ins, want) {
			t.Errorf("instruction missing %q", want)
		}
	}
}

func TestInstructionGovernorActsPeersAdvise(t *testing.T) {
	ins := Instruction("codex", Peers("codex", allAgents))
	// Peers are read-only advisors; the governor does the writing itself (concern 1:
	// a write-task is not handed to a peer, it's split into consult + the governor's act).
	for _, want := range []string{"READ-ONLY ADVISORS", ".agent/TASKS.md", "yourself"} {
		if !strings.Contains(ins, want) {
			t.Errorf("instruction missing read-only-advisor guidance %q", want)
		}
	}
	// The fresh/continue session-mode contract: a full self-contained prompt on --fresh,
	// only the delta on --continue, never forward the user's message verbatim, and trust
	// the status line when a --continue falls back to fresh (concern 2: follow-up turns).
	for _, want := range []string{"--fresh", "--continue", "self-contained", "delta", "verbatim", "status line"} {
		if !strings.Contains(ins, want) {
			t.Errorf("instruction missing session-mode guidance %q", want)
		}
	}
}

// TestConsultWrapperMatchesAdapters guards against the wrapper's hardcoded read-only
// flags drifting from each adapter's ConsultCmd.
func TestConsultWrapperMatchesAdapters(t *testing.T) {
	for _, peer := range allAgents {
		cmd := peerCmd(peer, "Q") // e.g. [claude -p --permission-mode plan Q]
		for _, tok := range cmd[:len(cmd)-1] {
			if !strings.Contains(ConsultWrapper, tok) {
				t.Errorf("coop-consult wrapper missing %s consult flag %q (adapter ConsultCmd drifted?)", peer, tok)
			}
		}
	}
}

func TestGovernorInstructionsPreservesBase(t *testing.T) {
	base := "# Project rules\nAlways run the gate."
	out := GovernorInstructions(base, "codex", allAgents)
	if !strings.Contains(out, base) {
		t.Error("base instructions were dropped")
	}
	// Fusion block comes first (prominence), base after it.
	if i, j := strings.Index(out, "Fusion mode"), strings.Index(out, "Project rules"); i < 0 || j < 0 || i > j {
		t.Errorf("expected fusion block before base: fusion@%d base@%d", i, j)
	}
	// Empty base → just the block, no trailing junk.
	if out := GovernorInstructions("  \n ", "claude", allAgents); !strings.HasPrefix(out, "# Fusion mode") {
		t.Errorf("empty base should yield just the block, got prefix %q", out[:min(20, len(out))])
	}
}

func TestLeadInstructions(t *testing.T) {
	// No peers → the base is returned unchanged (nothing to consult).
	if got := LeadInstructions("BASE", nil); got != "BASE" {
		t.Errorf("no peers: got %q, want %q", got, "BASE")
	}
	// With peers → an optional directive that spells out each peer's coop-consult
	// invocation (default --fresh), naming only those peers, with the base kept.
	out := LeadInstructions("BASE", []string{"codex", "gemini"})
	for _, want := range []string{"second opinion", "coop-consult codex --fresh", "coop-consult gemini --fresh", "Default to --fresh", "BASE"} {
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
