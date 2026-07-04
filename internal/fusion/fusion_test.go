package fusion

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
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

// Each adapter owns its read-only consult command; assert the exact flags — they're the
// sandbox that keeps a consulted peer from editing files.
func TestConsultCmds(t *testing.T) {
	cases := map[string][]string{
		"claude": {"claude", "-p", "--permission-mode", "plan", "Q"},
		"gemini": {"gemini", "--approval-mode", "plan", "-p", "Q"},
		"codex":  {"codex", "exec", "-s", "read-only", "Q"},
	}
	for tool, want := range cases {
		ag, ok := agents.Get(tool)
		if !ok {
			t.Fatalf("agent %q not registered", tool)
		}
		if got := ag.ConsultCmd("Q"); !slices.Equal(got, want) {
			t.Errorf("%s ConsultCmd = %v, want %v", tool, got, want)
		}
	}
	if _, ok := agents.Get("gpt"); ok {
		t.Error("an unknown tool should not resolve to an agent")
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
	for _, want := range []string{"READ-ONLY ADVISORS", ".agent/tasks/", "yourself"} {
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
// flags drifting from each adapter's ConsultCmd — for the fresh form (every flag) and the
// --continue/resume form (its read-only mode, which has no adapter source and so could
// loosen unnoticed).
func TestConsultWrapperMatchesAdapters(t *testing.T) {
	for _, peer := range allAgents {
		ag, _ := agents.Get(peer)
		cmd := ag.ConsultCmd("Q") // e.g. [claude -p --permission-mode plan Q]
		for _, tok := range cmd[:len(cmd)-1] {
			if !strings.Contains(ConsultWrapper, tok) {
				t.Errorf("coop-consult wrapper missing %s consult flag %q (adapter ConsultCmd drifted?)", peer, tok)
			}
		}
	}

	// The --continue branch resumes with hand-written invocations (no adapter returns the resume
	// form), so guard that each keeps its read-only sandbox flag — a drift here would silently
	// un-sandbox a resumed peer.
	for _, want := range []string{
		"--permission-mode plan --resume",        // claude
		"--approval-mode plan --resume",          // gemini
		`resume "$id" -c sandbox_mode=read-only`, // codex
	} {
		if !strings.Contains(ConsultWrapper, want) {
			t.Errorf("coop-consult resume invocation missing %q — a --continue may have lost its read-only flag", want)
		}
	}
}

// TestConsultWrapperCarriesModelOverrides: each peer invocation expands COOP_PEER_MODEL_<PEER>
// into its --model flag (box.Run exports the var when a model is configured), in the fresh
// AND resume forms — so a profile-marked / COOP_<AGENT>_MODEL model reaches consults too.
func TestConsultWrapperCarriesModelOverrides(t *testing.T) {
	for _, v := range []string{"COOP_PEER_MODEL_CLAUDE", "COOP_PEER_MODEL_GEMINI", "COOP_PEER_MODEL_CODEX"} {
		want := "${" + v + `:+--model "$` + v + `"}`
		if n := strings.Count(ConsultWrapper, want); n < 2 {
			t.Errorf("wrapper expands %s %d time(s), want it in both the fresh and resume invocations", v, n)
		}
	}
}

// TestConsultWrapperContinuityIsolated locks in the anti-bleed contract: consult continuity is
// keyed to a coop-owned, per-target /tmp idfile — a namespace SEPARATE from the agents' real
// session stores — so a peer consult can never be picked up as a normal `coop <agent>` session's
// "latest". (The primary-side guard lives in each adapter's Resume/Interactive; this is the
// consult side.) --fresh mints and writes the id; --continue reads it.
func TestConsultWrapperContinuityIsolated(t *testing.T) {
	w := ConsultWrapper
	if !strings.Contains(w, `idfile="/tmp/coop-consult-${key}.id"`) {
		t.Error("consult continuity must key a coop-owned per-target /tmp idfile, not an agent session store")
	}
	// It must never read or write the agents' own session stores — touching those is the bleed.
	for _, store := range []string{".claude/projects", ".gemini/tmp", ".codex/sessions"} {
		if strings.Contains(w, store) {
			t.Errorf("the wrapper must not touch the agent session store %q — that reintroduces the bleed", store)
		}
	}
	if !strings.Contains(w, `>"$idfile"`) {
		t.Error("--fresh must write the minted id to the idfile")
	}
	if !strings.Contains(w, `cat "$idfile"`) || !strings.Contains(w, `[ -f "$idfile" ]`) {
		t.Error("--continue must read the stored id from the idfile")
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

// box.Run passes [governor] + authenticated peers, so the directive must name only those — never an
// unauthenticated peer the governor can't actually consult.
func TestGovernorInstructionsNamesOnlyGivenPeers(t *testing.T) {
	out := GovernorInstructions("base", "codex", []string{"codex", "claude"}) // gemini omitted (unauthed)
	if !strings.Contains(out, "claude") {
		t.Errorf("should name the authed peer claude:\n%s", out)
	}
	if strings.Contains(out, "gemini") {
		t.Errorf("must NOT name the omitted (unauthenticated) peer gemini:\n%s", out)
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

// coop-consult also resolves a preset ROLE (a consult role, or a native role degraded under
// a non-Claude lead): COOP_CONSULT_<KEY>_{AGENT,MODEL,CONTRACT}, with the role's persona
// (contract) prepended so it answers AS that role.
func TestConsultWrapperResolvesRoles(t *testing.T) {
	for _, want := range []string{
		"COOP_CONSULT_${key}_AGENT",
		"COOP_CONSULT_${key}_MODEL",
		"COOP_CONSULT_${key}_CONTRACT",
		`cat "$persona"`, // the role's persona is prepended to the prompt
	} {
		if !strings.Contains(ConsultWrapper, want) {
			t.Errorf("consult wrapper missing role-resolution %q", want)
		}
	}
}
