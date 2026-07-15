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

var allAgents = agents.Names() // from the registry, so a new agent is covered without editing tests

// TestConsultWrapperShellcheck keeps the embedded coop-consult script clean. It's a Go
// string constant, so the normal shellcheck pass can't see it; run it here when
// shellcheck is available (skipped otherwise, so CI without it still passes).
func TestConsultWrapperShellcheck(t *testing.T) {
	sc := shellcheckPath(t)
	f := filepath.Join(t.TempDir(), "coop-consult")
	if err := os.WriteFile(f, []byte(ConsultWrapper()), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(sc, f).CombinedOutput(); err != nil {
		t.Errorf("shellcheck flagged the consult wrapper:\n%s", out)
	}
}

func shellcheckPath(t *testing.T) string {
	t.Helper()
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck not installed")
	}
	if out, err := exec.Command(sc, "--version").CombinedOutput(); err != nil {
		t.Skipf("shellcheck not usable: %v\n%s", err, out)
	}
	return sc
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

// Each adapter owns its read-only consult command; assert the exact flags — they're the
// sandbox that keeps a consulted peer from editing files.
func TestConsultCmds(t *testing.T) {
	cases := map[string][]string{
		"claude": {"claude", "-p", "--permission-mode", "plan", "Q"},
		"gemini": {"gemini", "--approval-mode", "plan", "-p", "Q"},
		"codex":  {"codex", "exec", "-s", "read-only", "Q"},
		// grok locks read-only via a tool allowlist — NOT --permission-mode plan, which is a
		// no-op in headless (only bypassPermissions takes effect via that flag).
		"grok": {"grok", "--tools", "read_file,grep,list_dir", "-p", "Q"},
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
	ins := Instruction("codex", []string{"claude", "gemini", "grok"})
	// Names every council member via the coop-consult wrapper.
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
	ins := Instruction("codex", []string{"claude", "gemini", "grok"})
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
	for _, want := range []string{
		"--fresh", "--continue", "self-contained", "delta", "verbatim", "status line",
		"not the member reply", "session handle", "same session to terminal exit", "complete output",
	} {
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
			if !strings.Contains(ConsultWrapper(), tok) {
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
		if !strings.Contains(ConsultWrapper(), want) {
			t.Errorf("coop-consult resume invocation missing %q — a --continue may have lost its read-only flag", want)
		}
	}
}

// TestConsultWrapperCarriesModelOverrides: each peer invocation expands COOP_PEER_MODEL_<PEER>
// into its --model flag (box.Run exports the var when a model is configured), in the fresh
// AND resume forms — so a profile-marked / COOP_<AGENT>_MODEL model reaches consults too.
func TestConsultWrapperCarriesModelOverrides(t *testing.T) {
	w := ConsultWrapper()
	// The per-peer model (COOP_PEER_MODEL_<PEER>, exported by box.Run) is resolved once into a
	// single $model via the generic eval, so a new agent is covered without a new arm.
	if !strings.Contains(w, `eval "model=\${COOP_PEER_MODEL_${peerkey}:-}"`) {
		t.Error("wrapper must resolve COOP_PEER_MODEL_<PEER> into $model generically")
	}
	// Every arm expands that $model — at least the fresh + resume form for each agent.
	if n := strings.Count(w, `${model:+--model "$model"}`); n < 2*len(agents.Names()) {
		t.Errorf("uniform $model expansion appears %d time(s), want >= %d (fresh+resume per agent)", n, 2*len(agents.Names()))
	}
}

func TestConsultRoleDoesNotInheritAdHocPeerModel(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "coop-consult")
	if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	argsLog := filepath.Join(dir, "args")
	codex := `#!/bin/sh
printf '%s\n' "$@" > "$ARGS_LOG"
printf '%s\n' \
'{"type":"thread.started","thread_id":"role-thread"}' \
'{"type":"item.completed","item":{"type":"agent_message","text":"ROLE_REPLY"}}' \
'{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(codex), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "timeout"), []byte("#!/bin/sh\nshift 3\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	role := "model-isolation"
	key := "MODEL_ISOLATION"
	for _, suffix := range []string{"id", "rung", "context"} {
		path := filepath.Join("/tmp", "coop-consult-"+key+"."+suffix)
		_ = os.Remove(path)
		defer os.Remove(path)
	}
	cmd := exec.Command(wrapper, role, "--fresh", "question")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"), "ARGS_LOG="+argsLog,
		"COOP_PEERS=codex", "COOP_CONSULT_"+key+"_TARGETS=codex",
		"COOP_PEER_MODEL_CODEX=raw-peer-model",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("role consult failed: %v\n%s", err, out)
	}
	args, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "raw-peer-model") {
		t.Fatalf("blank role target inherited an ad-hoc peer model:\n%s", args)
	}
}

// TestConsultWrapperContinuityIsolated locks in the anti-bleed contract: consult continuity is
// keyed to a coop-owned, per-target /tmp idfile — a namespace SEPARATE from the agents' real
// session stores — so a peer consult can never be picked up as a normal `coop <agent>` session's
// "latest". (The primary-side guard lives in each adapter's Resume/Interactive; this is the
// consult side.) --fresh mints and writes the id; --continue reads it.
func TestConsultWrapperContinuityIsolated(t *testing.T) {
	w := ConsultWrapper()
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
	out := GovernorInstructions(base, "codex", []string{"claude", "gemini", "grok"})
	if !strings.Contains(out, base) {
		t.Error("base instructions were dropped")
	}
	// Fusion block comes first (prominence), base after it.
	if i, j := strings.Index(out, "Fusion mode"), strings.Index(out, "Project rules"); i < 0 || j < 0 || i > j {
		t.Errorf("expected fusion block before base: fusion@%d base@%d", i, j)
	}
	// Empty base → just the block, no trailing junk.
	if out := GovernorInstructions("  \n ", "claude", []string{"codex", "gemini", "grok"}); !strings.HasPrefix(out, "# Fusion mode") {
		t.Errorf("empty base should yield just the block, got prefix %q", out[:min(20, len(out))])
	}
}

// box.Run passes [governor] + authenticated peers, so the directive must name only those — never an
// unauthenticated peer the governor can't actually consult.
func TestGovernorInstructionsNamesOnlyGivenPeers(t *testing.T) {
	out := GovernorInstructions("base", "codex", []string{"claude"}) // gemini omitted (unauthed)
	if !strings.Contains(out, "claude") {
		t.Errorf("should name the authed peer claude:\n%s", out)
	}
	if strings.Contains(out, "gemini") {
		t.Errorf("must NOT name the omitted (unauthenticated) peer gemini:\n%s", out)
	}
}

func TestInstructionIsCountNeutralAndKeepsRoleNamedLikeGovernor(t *testing.T) {
	for _, members := range [][]string{{"critic"}, {"claude", "critic", "gemini"}} {
		out := GovernorInstructions("base", "claude", members)
		lower := strings.ToLower(out)
		if strings.Contains(lower, "both peer") || strings.Contains(lower, "both member") ||
			(len(members) == 1 && (strings.Contains(lower, "members are") || strings.Contains(lower, "other peer"))) {
			t.Errorf("instruction for %v assumes exactly two members:\n%s", members, out)
		}
		for _, member := range members {
			if !strings.Contains(out, "coop-consult "+member+" --fresh") {
				t.Errorf("instruction for %v dropped member %q:\n%s", members, member, out)
			}
		}
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
	for _, want := range []string{
		"second opinion", "coop-consult codex --fresh", "coop-consult gemini --fresh",
		"not the peer reply", "session handle", "same session to terminal exit", "complete output",
		"Default to --fresh", "BASE",
	} {
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
		"COOP_CONSULT_${key}_TARGETS",
		"COOP_CONSULT_${key}_CONTRACT",
		`cat "$persona"`, // the role's persona is prepended to the prompt
	} {
		if !strings.Contains(ConsultWrapper(), want) {
			t.Errorf("consult wrapper missing role-resolution %q", want)
		}
	}
}

// TestConsultWrapperRefusesUnlistedPeer: the security gate. A registered adapter that is NOT in
// this run's council (COOP_PEERS) is refused before any work — so a lead can't consult (and
// thereby drive) an agent the run never named, even though the adapter's case arm exists.
func TestConsultWrapperRefusesUnlistedPeer(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	f := filepath.Join(t.TempDir(), "coop-consult")
	if err := os.WriteFile(f, []byte(ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	// codex IS a registered adapter but NOT listed → refused with the council message, exit ≠ 0.
	cmd := exec.Command("sh", f, "codex", "--fresh", "hi")
	cmd.Env = append(os.Environ(), "COOP_PEERS=claude gemini")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("wrapper allowed an unlisted peer:\n%s", out)
	}
	if !strings.Contains(string(out), "not in this run's credential scope") {
		t.Errorf("expected the council-refusal message, got:\n%s", out)
	}
	// A LISTED peer clears the council gate (it then tries to run the agent, which isn't
	// installed here — but that's a different failure, never the council refusal).
	cmd = exec.Command("sh", f, "claude", "--fresh", "hi")
	cmd.Env = append(os.Environ(), "COOP_PEERS=claude gemini")
	if out, _ := cmd.CombinedOutput(); strings.Contains(string(out), "not in this run's council") {
		t.Errorf("a listed peer must clear the council gate, got:\n%s", out)
	}
}

func runConsultWrapperStub(t *testing.T, role, peer, providerBody, timeoutBody, runID string) (string, int, string, bool) {
	t.Helper()
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "coop-consult")
	if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(peer, providerBody)
	write("timeout", timeoutBody)
	if err := os.MkdirAll(filepath.Join(dir, ".agent", "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	key := strings.ToUpper(strings.ReplaceAll(role, "-", "_"))
	idfile := filepath.Join("/tmp", "coop-consult-"+key+".id")
	for _, suffix := range []string{"id", "rung", "context"} {
		path := filepath.Join("/tmp", "coop-consult-"+key+"."+suffix)
		os.Remove(path)
		defer os.Remove(path)
	}
	cmd := exec.Command(wrapper, role, "--fresh", "question")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"COOP_PEERS="+peer,
		"COOP_CONSULT_"+key+"_TARGETS="+peer+":test",
		"COOP_RUN_ID="+runID,
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run wrapper: %v\n%s", err, out)
	}
	telemetry, _ := os.ReadFile(filepath.Join(dir, ".agent", "runs", runID+".peers.jsonl"))
	_, idErr := os.Stat(idfile)
	return string(out), code, strings.TrimSpace(string(telemetry)), idErr == nil
}

func TestConsultWrapperCodexReplyContract(t *testing.T) {
	const passTimeout = `shift 3
exec "$@"`
	cases := []struct {
		name          string
		body          string
		wantCode      int
		wantText      string
		wantTelemetry bool
		wantResumable bool
	}{
		{
			name: "successful reply",
			body: `printf '%s\n' \
'{"type":"thread.started","thread_id":"thread-1"}' \
'{"type":"item.completed","item":{"type":"agent_message","text":"CODEX_ANSWER"}}' \
'{"type":"turn.completed","usage":{"input_tokens":11,"output_tokens":5,"reasoning_output_tokens":2}}'`,
			wantText:      "CODEX_ANSWER",
			wantTelemetry: true,
			wantResumable: true,
		},
		{
			name: "completed without reply",
			body: `printf '%s\n' \
'{"type":"thread.started","thread_id":"thread-2"}' \
'{"type":"turn.completed","usage":{"input_tokens":11,"output_tokens":5,"reasoning_output_tokens":2}}'`,
			wantCode:      1,
			wantText:      "malformed output or no usable reply",
			wantTelemetry: true,
		},
		{
			name: "whitespace reply",
			body: `printf '%s\n' \
'{"type":"thread.started","thread_id":"thread-3"}' \
'{"type":"item.completed","item":{"type":"agent_message","text":"   "}}' \
'{"type":"turn.completed","usage":{"input_tokens":11,"output_tokens":5,"reasoning_output_tokens":2}}'`,
			wantCode:      1,
			wantText:      "malformed output or no usable reply",
			wantTelemetry: true,
		},
		{
			name:     "malformed stream",
			body:     `printf '%s\n' 'not-json'`,
			wantCode: 1,
			wantText: "malformed output or no usable reply",
		},
		{
			name:     "provider failure",
			body:     `printf '%s\n' '{"type":"turn.failed","error":{"message":"provider exploded"}}'; exit 7`,
			wantCode: 7,
			wantText: "provider exploded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			role := "codexreply-" + strings.ReplaceAll(tc.name, " ", "-")
			out, code, telemetry, resumable := runConsultWrapperStub(t, role, "codex", tc.body, passTimeout, "reply-run")
			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d:\n%s", code, tc.wantCode, out)
			}
			if !strings.Contains(out, tc.wantText) {
				t.Errorf("output missing %q:\n%s", tc.wantText, out)
			}
			if tc.wantCode == 1 && !strings.Contains(out, "rerun "+role+" with --fresh") {
				t.Errorf("unusable reply lacks actionable retry guidance:\n%s", out)
			}
			if tc.wantTelemetry {
				for _, want := range []string{`"run":"reply-run"`, `"role":"` + role + `"`, `"in":11`, `"out":7`} {
					if !strings.Contains(telemetry, want) {
						t.Errorf("telemetry missing %q: %q", want, telemetry)
					}
				}
			}
			if resumable != tc.wantResumable {
				t.Errorf("resumable state = %v, want %v", resumable, tc.wantResumable)
			}
		})
	}
}

func TestConsultWrapperCodexUnusableResumeClearsContinuation(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "coop-consult")
	if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("codex", `if [ "$2" = resume ]; then
	printf '%s\n' \
'{"type":"item.completed","item":{"type":"command_execution","command":"true"}}' \
'{"type":"turn.completed","usage":{"input_tokens":3,"output_tokens":1}}'
	exit 0
fi
printf '%s\n' \
'{"type":"thread.started","thread_id":"thread-resume"}' \
'{"type":"item.completed","item":{"type":"agent_message","text":"FIRST_REPLY"}}' \
'{"type":"turn.completed","usage":{"input_tokens":2,"output_tokens":1}}'`)
	write("timeout", `shift 3
exec "$@"`)

	role := "codex-unusable-resume"
	key := strings.ToUpper(strings.ReplaceAll(role, "-", "_"))
	idfile := filepath.Join("/tmp", "coop-consult-"+key+".id")
	rungfile := filepath.Join("/tmp", "coop-consult-"+key+".rung")
	contextfile := filepath.Join("/tmp", "coop-consult-"+key+".context")
	for _, path := range []string{idfile, rungfile, contextfile} {
		os.Remove(path)
		defer os.Remove(path)
	}
	run := func(mode string) (string, int) {
		t.Helper()
		cmd := exec.Command(wrapper, role, mode, "question")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"PATH="+dir+":"+os.Getenv("PATH"),
			"TMPDIR="+dir,
			"COOP_PEERS=codex",
			"COOP_CONSULT_"+key+"_TARGETS=codex:test",
		)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run wrapper: %v\n%s", err, out)
		}
		return string(out), exit.ExitCode()
	}

	if out, code := run("--fresh"); code != 0 || !strings.Contains(out, "FIRST_REPLY") {
		t.Fatalf("fresh consult = exit %d:\n%s", code, out)
	}
	for _, path := range []string{idfile, rungfile} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("fresh consult did not create %s: %v", path, err)
		}
	}
	out, code := run("--continue")
	if code != 1 || !strings.Contains(out, "malformed output or no usable reply") {
		t.Fatalf("unusable resume = exit %d:\n%s", code, out)
	}
	for _, path := range []string{idfile, rungfile} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("unusable resume left continuation state %s: %v", path, err)
		}
	}
}

func TestConsultWrapperEmptyReplyAndTimeout(t *testing.T) {
	cases := []struct {
		name        string
		provider    string
		timeoutBody string
		wantCode    int
		wantText    string
	}{
		{
			name:        "empty provider success",
			provider:    "exit 0",
			timeoutBody: "shift 3\nexec \"$@\"",
			wantCode:    1,
			wantText:    "provider returned no usable reply",
		},
		{
			name:        "wrapper timeout",
			provider:    "echo SHOULD_NOT_RUN",
			timeoutBody: "exit 124",
			wantCode:    124,
			wantText:    "no reply within 1800s",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			role := "delivery-" + strings.ReplaceAll(tc.name, " ", "-")
			out, code, _, resumable := runConsultWrapperStub(t, role, "claude", tc.provider, tc.timeoutBody, "")
			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d:\n%s", code, tc.wantCode, out)
			}
			if !strings.Contains(out, tc.wantText) {
				t.Errorf("output missing %q:\n%s", tc.wantText, out)
			}
			if tc.wantCode == 1 && !strings.Contains(out, "rerun "+role+" with --fresh") {
				t.Errorf("empty reply lacks actionable retry guidance:\n%s", out)
			}
			if tc.wantCode == 1 && resumable {
				t.Error("an empty reply must not leave resumable state")
			}
		})
	}
}

func TestConsultWrapperRoleFallsBackAndContinuesSelectedRung(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "coop-consult")
	if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	writeStub := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeStub("claude", `echo "claude $*" >>"$CALLS"
echo "usage limit reached" >&2
exit 9`)
	writeStub("gemini", `echo "gemini $*" >>"$CALLS"
echo "gemini-answer"`)
	writeStub("timeout", `shift 3
exec "$@"`)

	role := "fallbacktest"
	idfile := filepath.Join("/tmp", "coop-consult-"+strings.ToUpper(role)+".id")
	rungfile := filepath.Join("/tmp", "coop-consult-"+strings.ToUpper(role)+".rung")
	defer os.Remove(idfile)
	defer os.Remove(rungfile)
	os.Remove(idfile)
	os.Remove(rungfile)
	env := append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"CALLS="+calls,
		"COOP_PEERS=claude gemini",
		"COOP_CONSULT_FALLBACKTEST_TARGETS=claude:claude-opus-4-8/xhigh gemini:gemini-3.5-flash",
	)
	run := func(mode, prompt string) (string, int) {
		t.Helper()
		cmd := exec.Command(wrapper, role, mode, prompt)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return string(out), exit.ExitCode()
		}
		t.Fatalf("run wrapper: %v", err)
		return "", -1
	}

	out, code := run("--fresh", "question")
	if code != 0 {
		t.Fatalf("fresh fallback exit = %d, want 0:\n%s", code, out)
	}
	for _, want := range []string{"claude:claude-opus-4-8/xhigh rate limited", "starting fallback 2/2", "gemini-answer"} {
		if !strings.Contains(out, want) {
			t.Errorf("fresh fallback output missing %q:\n%s", want, out)
		}
	}
	firstCalls, _ := os.ReadFile(calls)
	if !strings.Contains(string(firstCalls), "claude ") || !strings.Contains(string(firstCalls), "gemini ") {
		t.Fatalf("fresh calls did not run both rungs:\n%s", firstCalls)
	}

	if err := os.WriteFile(calls, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = run("--continue", "delta")
	if code != 0 || !strings.Contains(out, "continued on gemini:gemini-3.5-flash") {
		t.Fatalf("continue selected rung = (exit %d):\n%s", code, out)
	}
	continuedCalls, _ := os.ReadFile(calls)
	if strings.Contains(string(continuedCalls), "claude ") || !strings.Contains(string(continuedCalls), "gemini --approval-mode plan --resume") {
		t.Fatalf("continue must resume only the successful Gemini rung:\n%s", continuedCalls)
	}
}

func TestConsultWrapperContinueFallbackReplaysTranscript(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "coop-consult")
	if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "claude-state")
	geminiArgs := filepath.Join(dir, "gemini-args")
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("claude", `if [ ! -f "$STATE" ]; then
  : >"$STATE"
  echo FIRST_ANSWER
  exit 0
fi
echo "rate limit exceeded" >&2
exit 9`)
	write("gemini", `printf '%s\n' "$@" >"$GEMINI_ARGS"
echo FALLBACK_ANSWER`)
	write("timeout", "shift 3\nexec \"$@\"")

	for _, suffix := range []string{"id", "rung", "context"} {
		path := filepath.Join("/tmp", "coop-consult-CONTEXTTEST."+suffix)
		os.Remove(path)
		defer os.Remove(path)
	}
	env := append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"STATE="+state,
		"GEMINI_ARGS="+geminiArgs,
		"COOP_PEERS=claude gemini",
		"COOP_CONSULT_CONTEXTTEST_TARGETS=claude:one gemini:two",
	)
	run := func(mode, prompt string) (string, int) {
		cmd := exec.Command(wrapper, "contexttest", mode, prompt)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return string(out), exit.ExitCode()
		}
		t.Fatalf("wrapper: %v", err)
		return "", -1
	}
	if out, code := run("--fresh", "FIRST_QUESTION"); code != 0 || !strings.Contains(out, "FIRST_ANSWER") {
		t.Fatalf("initial consult = (exit %d):\n%s", code, out)
	}
	out, code := run("--continue", "SECOND_DELTA")
	if code != 0 || !strings.Contains(out, "trying fallback 2/2") || !strings.Contains(out, "FALLBACK_ANSWER") {
		t.Fatalf("continued fallback = (exit %d):\n%s", code, out)
	}
	args, _ := os.ReadFile(geminiArgs)
	for _, want := range []string{"saved transcript", "FIRST_QUESTION", "FIRST_ANSWER", "SECOND_DELTA"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("fresh fallback prompt missing %q:\n%s", want, args)
		}
	}
}

func TestConsultWrapperFallbackDecisionMatrix(t *testing.T) {
	cases := []struct {
		name       string
		claudeBody string
		geminiBody string
		wantCode   int
		wantGemini bool
		wantText   string
	}{
		{
			name:       "successful prose mentioning a limit",
			claudeBody: `echo "claude $*" >>"$CALLS"; echo "rate limit handling documented"`,
			geminiBody: `echo "gemini $*" >>"$CALLS"`,
			wantCode:   0,
		},
		{
			name:       "ordinary failure",
			claudeBody: `echo "claude $*" >>"$CALLS"; echo "bad request while task text mentions rate limit handling" >&2; exit 7`,
			geminiBody: `echo "gemini $*" >>"$CALLS"`,
			wantCode:   7,
		},
		{
			name:       "timeout after marker",
			claudeBody: `echo "claude $*" >>"$CALLS"; echo "rate limit exceeded" >&2; exit 124`,
			geminiBody: `echo "gemini $*" >>"$CALLS"`,
			wantCode:   124,
		},
		{
			name:       "all rungs limited once",
			claudeBody: `echo "claude $*" >>"$CALLS"; echo "usage limit reached" >&2; exit 9`,
			geminiBody: `echo "gemini $*" >>"$CALLS"; echo "RESOURCE_EXHAUSTED" >&2; exit 8`,
			wantCode:   8,
			wantGemini: true,
			wantText:   "exhausted all 2 target(s)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			wrapper := filepath.Join(dir, "coop-consult")
			if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
				t.Fatal(err)
			}
			write := func(name, body string) {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write("claude", tc.claudeBody)
			write("gemini", tc.geminiBody)
			write("timeout", "shift 3\nexec \"$@\"")
			calls := filepath.Join(dir, "calls")
			role := "decisiontest"
			idfile := filepath.Join("/tmp", "coop-consult-DECISIONTEST.id")
			rungfile := filepath.Join("/tmp", "coop-consult-DECISIONTEST.rung")
			defer os.Remove(idfile)
			defer os.Remove(rungfile)
			os.Remove(idfile)
			os.Remove(rungfile)
			cmd := exec.Command(wrapper, role, "--fresh", "question")
			cmd.Env = append(os.Environ(),
				"PATH="+dir+":"+os.Getenv("PATH"),
				"TMPDIR="+dir,
				"CALLS="+calls,
				"COOP_PEERS=claude gemini",
				"COOP_CONSULT_DECISIONTEST_TARGETS=claude:one gemini:two",
			)
			out, err := cmd.CombinedOutput()
			code := 0
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else if err != nil {
				t.Fatalf("wrapper: %v\n%s", err, out)
			}
			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d:\n%s", code, tc.wantCode, out)
			}
			gotCalls, _ := os.ReadFile(calls)
			hasGemini := strings.Contains(string(gotCalls), "gemini ")
			if hasGemini != tc.wantGemini {
				t.Errorf("Gemini called = %v, want %v:\n%s", hasGemini, tc.wantGemini, gotCalls)
			}
			if strings.Count(string(gotCalls), "claude ") != 1 || strings.Count(string(gotCalls), "gemini ") > 1 {
				t.Errorf("each rung must run at most once:\n%s", gotCalls)
			}
			if tc.wantText != "" && !strings.Contains(string(out), tc.wantText) {
				t.Errorf("output missing %q:\n%s", tc.wantText, out)
			}
		})
	}
}

func TestConsultWrapperValidatesEveryRungBeforeDispatch(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "coop-consult")
	if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\necho called >\"$CALLS\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(wrapper, "guardtest", "--fresh", "question")
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"CALLS="+calls,
		"COOP_PEERS=claude",
		"COOP_CONSULT_GUARDTEST_TARGETS=claude:one not-a-provider:two",
	)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "unknown peer in guardtest ladder") {
		t.Fatalf("invalid later rung should fail validation:\n%s", out)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("first rung dispatched before the later rung was validated")
	}
}

func TestConsultWrapperSkipsRungWithoutMountedCredentials(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "coop-consult")
	if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"claude":  "#!/bin/sh\necho AVAILABLE_OK\n",
		"timeout": "#!/bin/sh\nshift 3\nexec \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(wrapper, "scopetest", "--fresh", "question")
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"COOP_PEERS=claude",
		"COOP_CONSULT_SCOPETEST_TARGETS=claude:one gemini:two",
	)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "skipping gemini:two") || !strings.Contains(string(out), "AVAILABLE_OK") {
		t.Fatalf("unmounted fallback should be skipped while valid rung runs: %v\n%s", err, out)
	}
}

// fakeConsult is a minimal 4th agent for the drift test — enough of the interface for the
// consult generator, so we can prove a newly-registered agent is dispatched without a
// hand-edited case arm.
type fakeConsult struct{}

func (fakeConsult) Name() string { return "grokfake" }
func (fakeConsult) ConsultFresh() string {
	return `run grokfake --plan --session-id "$id" ${model:+--model "$model"} "$prompt"`
}
func (fakeConsult) ConsultResume() string {
	return `run grokfake --plan --resume "$id" ${model:+--model "$model"} "$prompt"`
}
func (fakeConsult) ShellPrelude() string { return "" }

// TestConsultWrapperDispatchesNewAgent: a 4th agent needs no edit here — passed to the
// generator, its case arms and peer validation appear, and the rendered script still passes
// shellcheck. This is the structural guarantee behind "adding an agent is one file".
func TestConsultWrapperDispatchesNewAgent(t *testing.T) {
	reals := registeredConsults()
	w := renderConsult(append(reals, fakeConsult{}))

	// It's a valid peer (validation case), and both dispatch blocks carry its arm.
	if !strings.Contains(w, "grokfake") {
		t.Fatal("the new agent never appears in the rendered wrapper")
	}
	if strings.Count(w, "grokfake)\n") < 2 {
		t.Errorf("the new agent must have a fresh AND a resume case arm, got:\n%s", w)
	}
	if !strings.Contains(w, `run grokfake --plan --resume "$id"`) {
		t.Error("the new agent's resume invocation is missing from the --continue block")
	}

	if sc := shellcheckPath(t); sc != "" {
		f := filepath.Join(t.TempDir(), "coop-consult")
		if err := os.WriteFile(f, []byte(w), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(sc, f).CombinedOutput(); err != nil {
			t.Errorf("shellcheck flagged the wrapper with a 4th agent:\n%s", out)
		}
	}
}
