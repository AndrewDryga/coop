package fusion

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

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
		"FRESH from the saved transcript", "resend full context",
	} {
		if !strings.Contains(ins, want) {
			t.Errorf("instruction missing session-mode guidance %q", want)
		}
	}
}

func TestInstructionKeepsPeerRepliesSeparateFromDiagnostics(t *testing.T) {
	ins := Instruction("codex", []string{"claude"})
	for _, want := range []string{"peer-claude.reply", "peer-claude.diagnostics", "claude reply", "claude diagnostics"} {
		if !strings.Contains(ins, want) {
			t.Errorf("parallel consult block missing %q:\n%s", want, ins)
		}
	}
	if strings.Contains(ins, "2>&1") {
		t.Fatalf("parallel consult block merges provider diagnostics into advisor replies:\n%s", ins)
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
	cmd := exec.Command(wrapper, role, "--fresh", "question")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"), "TMPDIR="+dir, "ARGS_LOG="+argsLog,
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
// keyed to a coop-owned, per-target state file below TMPDIR — a namespace SEPARATE from the agents' real
// session stores — so a peer consult can never be picked up as a normal `coop <agent>` session's
// "latest". (The primary-side guard lives in each adapter's Resume/Interactive; this is the
// consult side.) --fresh stages a complete record; --continue validates and reads it.
func TestConsultWrapperContinuityIsolated(t *testing.T) {
	w := ConsultWrapper()
	if !strings.Contains(w, `state_dir=${TMPDIR:-/tmp}/coop-consult-state`) || !strings.Contains(w, `statefile="$state_dir/${key}.state"`) {
		t.Error("consult continuity must key coop-owned per-target state below TMPDIR, not an agent session store")
	}
	// It must never read or write the agents' own session stores — touching those is the bleed.
	for _, store := range []string{".claude/projects", ".gemini/tmp", ".codex/sessions"} {
		if strings.Contains(w, store) {
			t.Errorf("the wrapper must not touch the agent session store %q — that reintroduces the bleed", store)
		}
	}
	if !strings.Contains(w, `>"$candidate_idfile"`) || !strings.Contains(w, `mv "$next_state" "$statefile"`) {
		t.Error("--fresh must stage its id and atomically publish one complete record after a usable reply")
	}
	if !strings.Contains(w, `state_file_safe "$statefile"`) || !strings.Contains(w, `resume_id=${id_line#id=}`) {
		t.Error("--continue must validate and read the stored id from the continuation record")
	}
}

func TestConsultWrapperInterruptedPublicationKeepsPriorRecord(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "coop-consult")
	if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"claude":  "#!/bin/sh\ncase \" $* \" in *\" --resume \"*) echo SECOND_REPLY ;; *) echo FIRST_REPLY ;; esac\n",
		"timeout": "#!/bin/sh\nshift 3\nexec \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	role, key := "atomic-state", "ATOMIC_STATE"
	baseEnv := append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TMPDIR="+dir,
		"COOP_PEERS=claude", "COOP_CONSULT_"+key+"_TARGETS=claude:test")
	run := func(mode, prompt string, env []string) ([]byte, error) {
		cmd := exec.Command(wrapper, role, mode, prompt)
		cmd.Env = env
		return cmd.CombinedOutput()
	}
	if out, err := run("--fresh", "first", baseEnv); err != nil {
		t.Fatalf("initial consult: %v\n%s", err, out)
	}
	statePath := filepath.Join(dir, "coop-consult-state", key+".state")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	realMV, err := exec.LookPath("mv")
	if err != nil {
		t.Fatal(err)
	}
	ready, release := filepath.Join(dir, "publish-ready"), filepath.Join(dir, "publish-release")
	mvShim := `#!/bin/sh
dest=
for arg do dest=$arg; done
case "$dest" in *.state) : >"$READY"; while [ ! -e "$RELEASE" ]; do sleep 0.05; done ;; esac
exec "$REAL_MV" "$@"
`
	mvPath := filepath.Join(dir, "mv")
	if err := os.WriteFile(mvPath, []byte(mvShim), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(wrapper, role, "--continue", "second")
	cmd.Env = append(baseEnv, "READY="+ready, "RELEASE="+release, "REAL_MV="+realMV)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			t.Fatal("continuation never reached its atomic publication")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got, err := os.ReadFile(statePath); err != nil || !bytes.Equal(got, before) {
		t.Fatalf("record changed before atomic publication: %v\n%s", err, got)
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	_ = cmd.Wait()
	if got, err := os.ReadFile(statePath); err != nil || !bytes.Equal(got, before) {
		t.Fatalf("interrupted publication left a partial or mixed record: %v\n%s", err, got)
	}
	if err := os.Remove(mvPath); err != nil {
		t.Fatal(err)
	}
	if out, err := run("--continue", "third", baseEnv); err != nil || !bytes.Contains(out, []byte("SECOND_REPLY")) {
		t.Fatalf("prior record was not resumable after interruption: %v\n%s", err, out)
	}
}

func TestConsultWrapperLockSerializesAndRecoversAfterOwnerExit(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "coop-consult")
	if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	provider := `#!/bin/sh
echo start >>"$CALLS"
case " $* " in *FIRST*) : >"$READY"; while [ ! -e "$RELEASE" ]; do sleep 0.05; done ;; esac
case " $* " in *CRASH*) : >"$CRASH_READY"; while [ ! -e "$CRASH_RELEASE" ]; do sleep 0.05; done ;; esac
echo REPLY
`
	for name, body := range map[string]string{"claude": provider, "timeout": "#!/bin/sh\nshift 3\nexec \"$@\"\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	role, key := "lock-test", "LOCK_TEST"
	calls, ready, release := filepath.Join(dir, "calls"), filepath.Join(dir, "ready"), filepath.Join(dir, "release")
	crashReady, crashRelease := filepath.Join(dir, "crash-ready"), filepath.Join(dir, "crash-release")
	env := append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TMPDIR="+dir, "CALLS="+calls, "READY="+ready, "RELEASE="+release,
		"CRASH_READY="+crashReady, "CRASH_RELEASE="+crashRelease, "COOP_PEERS=claude", "COOP_CONSULT_"+key+"_TARGETS=claude:test")
	first := exec.Command(wrapper, role, "--fresh", "FIRST")
	first.Env = env
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = first.Process.Kill()
			t.Fatal("first consult did not reach provider gate")
		}
		time.Sleep(20 * time.Millisecond)
	}
	second := exec.Command(wrapper, role, "--fresh", "SECOND")
	second.Env = env
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if data, _ := os.ReadFile(calls); strings.Count(string(data), "start") != 1 {
		t.Fatalf("second consult entered provider while the first held the lock: %q", data)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := second.Wait(); err != nil {
		t.Fatal(err)
	}

	if _, err := exec.LookPath("flock"); err == nil {
		crashed := exec.Command(wrapper, role, "--fresh", "CRASH")
		crashed.Env = env
		if err := crashed.Start(); err != nil {
			t.Fatal(err)
		}
		waitForConsultTestFile(t, crashReady, crashed)
		if err := crashed.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = crashed.Wait()
		recovered := exec.Command(wrapper, role, "--fresh", "RECOVERED")
		recovered.Env = env
		if out, err := recovered.CombinedOutput(); err != nil || !bytes.Contains(out, []byte("REPLY")) {
			t.Fatalf("kernel lock survived an unclean owner exit: %v\n%s", err, out)
		}
		_ = os.WriteFile(crashRelease, nil, 0o600)
	} else {
		t.Log("flock unavailable; serialization was exercised through the mkdir compatibility path")
	}
}

func TestConsultWrapperLockBudgetCoversEveryAdmittedRung(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "coop-consult")
	if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	scripts := map[string]string{
		"flock":   "#!/bin/sh\nprintf '%s\\n' \"$*\" >\"$FLOCK_ARGS\"\n",
		"claude":  "#!/bin/sh\necho 'usage limit reached' >&2\nexit 9\n",
		"gemini":  "#!/bin/sh\necho FALLBACK_REPLY\n",
		"timeout": "#!/bin/sh\nshift 3\nexec \"$@\"\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	flockArgs := filepath.Join(dir, "flock-args")
	cmd := exec.Command(wrapper, "budget", "--fresh", "question")
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TMPDIR="+dir,
		"FLOCK_ARGS="+flockArgs, "COOP_CONSULT_TIMEOUT=2", "COOP_PRIMARY=codex",
		"COOP_PEERS=claude gemini", "COOP_CONSULT_BUDGET_TARGETS=claude:one gemini:two")
	if out, err := cmd.CombinedOutput(); err != nil || !bytes.Contains(out, []byte("FALLBACK_REPLY")) {
		t.Fatalf("two-rung consult = %v\n%s", err, out)
	}
	got, err := os.ReadFile(flockArgs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "-w 79 8" {
		t.Fatalf("two-rung lock args = %q, want %q", got, "-w 79 8")
	}
}

func TestConsultWrapperRejectsHardlinkedContinuationLockBeforeDispatch(t *testing.T) {
	dir := t.TempDir()
	role, key := "hardlink-lock", "HARDLINK_LOCK"
	stateDir := filepath.Join(dir, "coop-consult-state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside")
	if err := os.WriteFile(outside, []byte("do not touch"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(stateDir, key+".lock")); err != nil {
		t.Fatal(err)
	}
	called := filepath.Join(dir, "called")
	for name, body := range map[string]string{
		"coop-consult": ConsultWrapper(),
		"claude":       "#!/bin/sh\nprintf called >\"$CALLED\"\necho SHOULD_NOT_RUN\n",
		"flock":        "#!/bin/sh\nexit 0\n",
		"timeout":      "#!/bin/sh\nshift 3\nexec \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(filepath.Join(dir, "coop-consult"), role, "--fresh", "question")
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TMPDIR="+dir,
		"CALLED="+called, "COOP_PEERS=claude", "COOP_CONSULT_"+key+"_TARGETS=claude:test")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "continuation lock must have one hardlink") {
		t.Fatalf("hardlinked lock was not rejected: %v\n%s", err, out)
	}
	if _, err := os.Stat(called); !os.IsNotExist(err) {
		t.Fatalf("provider dispatched through unsafe lock: %v", err)
	}
	if info, err := os.Stat(outside); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o644 {
		t.Fatalf("wrapper changed hardlinked file mode to %o", info.Mode().Perm())
	}
}

func waitForConsultTestFile(t *testing.T, path string, cmd *exec.Cmd) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("consult never reached provider gate %s", filepath.Base(path))
		}
		time.Sleep(20 * time.Millisecond)
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
		"Default to --fresh", "FRESH from the saved transcript", "resend full context", "BASE",
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
	if strings.Contains(out, "2>&1") {
		t.Fatal("optional consult instructions merge provider diagnostics into advisor replies")
	}
}

// coop-consult also resolves a preset ROLE (a consult role, or a native role degraded under
// a non-Claude lead): COOP_CONSULT_<KEY>_{TARGETS,CONTRACT}, with the role's persona
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
	if runID != "" {
		if err := os.WriteFile(filepath.Join(dir, ".agent", "runs", runID+".peers.jsonl"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	key := strings.ToUpper(strings.ReplaceAll(role, "-", "_"))
	statefile := filepath.Join(dir, "coop-consult-state", key+".state")
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
	state, stateErr := os.ReadFile(statefile)
	resumable := stateErr == nil && !bytes.Contains(state, []byte("\nid=\n"))
	return string(out), code, strings.TrimSpace(string(telemetry)), resumable
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
			wantCode: 1,
			wantText: "malformed output or no usable reply",
		},
		{
			name: "whitespace reply",
			body: `printf '%s\n' \
'{"type":"thread.started","thread_id":"thread-3"}' \
'{"type":"item.completed","item":{"type":"agent_message","text":"   "}}' \
'{"type":"turn.completed","usage":{"input_tokens":11,"output_tokens":5,"reasoning_output_tokens":2}}'`,
			wantCode: 1,
			wantText: "malformed output or no usable reply",
		},
		{
			name:     "malformed stream",
			body:     `printf '%s\n' 'not-json'`,
			wantCode: 1,
			wantText: "malformed output or no usable reply",
		},
		{
			name: "provider failure",
			body: `printf '%s\n' \
'{"type":"turn.completed","usage":{"input_tokens":11,"output_tokens":5,"reasoning_output_tokens":2}}' \
'{"type":"turn.failed","error":{"message":"provider exploded"}}'; exit 7`,
			wantCode: 7,
			wantText: "provider exploded",
		},
		{
			name: "invalid thread id is not resumable",
			body: `printf '%s\n' \
'{"type":"thread.started","thread_id":null}' \
'{"type":"item.completed","item":{"type":"agent_message","text":"VALID_REPLY"}}' \
'{"type":"turn.completed","usage":{"input_tokens":11,"output_tokens":5,"reasoning_output_tokens":2}}'`,
			wantText:      "VALID_REPLY",
			wantTelemetry: true,
		},
		{
			name: "invalid usage is not telemetry",
			body: `printf '%s\n' \
'{"type":"thread.started","thread_id":"valid-thread"}' \
'{"type":"item.completed","item":{"type":"agent_message","text":"VALID_REPLY"}}' \
'{"type":"turn.completed","usage":{"input_tokens":-1,"output_tokens":1.5,"reasoning_output_tokens":1000000001}}'`,
			wantText:      "VALID_REPLY",
			wantResumable: true,
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
			if tc.wantCode == 1 && !strings.Contains(out, "coop-consult "+role+` --fresh "<full prompt>"`) {
				t.Errorf("unusable reply lacks actionable retry guidance:\n%s", out)
			}
			if tc.wantTelemetry {
				for _, want := range []string{`"run":"reply-run"`, `"role":"` + role + `"`, `"in":11`, `"out":7`} {
					if !strings.Contains(telemetry, want) {
						t.Errorf("telemetry missing %q: %q", want, telemetry)
					}
				}
			} else if telemetry != "" {
				t.Errorf("invalid or absent usage wrote telemetry: %q", telemetry)
			}
			if resumable != tc.wantResumable {
				t.Errorf("resumable state = %v, want %v", resumable, tc.wantResumable)
			}
		})
	}
}

func TestConsultWrapperCodexTelemetryWaitsForFullAttemptAcceptance(t *testing.T) {
	const passTimeout = `shift 3
exec "$@"`
	body := fmt.Sprintf(`printf '%%s\n' \
'{"type":"thread.started","thread_id":"thread-overflow"}' \
'{"type":"item.completed","item":{"type":"agent_message","text":"VALID_REPLY"}}' \
'{"type":"turn.completed","usage":{"input_tokens":11,"output_tokens":5,"reasoning_output_tokens":2}}'
dd if=/dev/zero bs=%d count=1 2>/dev/null | tr '\000' X >&2`, consultStreamLimitBytes+1)
	out, code, telemetry, resumable := runConsultWrapperStub(t, "codex-diagnostic-overflow", "codex", body, passTimeout, "overflow-run")
	if code != 1 || !strings.Contains(out, "provider diagnostics exceeded") {
		t.Fatalf("diagnostic overflow = exit %d:\n%s", code, out)
	}
	if telemetry != "" {
		t.Fatalf("rejected attempt appended telemetry: %q", telemetry)
	}
	if resumable {
		t.Fatal("rejected attempt retained resumable state")
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
	stateDir := filepath.Join(dir, "coop-consult-state")
	statefile := filepath.Join(stateDir, key+".state")
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
	if _, err := os.Stat(statefile); err != nil {
		t.Fatalf("fresh consult did not create %s: %v", statefile, err)
	}
	out, code := run("--continue")
	if code != 1 || !strings.Contains(out, "malformed output or no usable reply") {
		t.Fatalf("unusable resume = exit %d:\n%s", code, out)
	}
	state, err := os.ReadFile(statefile)
	if err != nil || !bytes.Contains(state, []byte("\nid=\n\n")) || !bytes.Contains(state, []byte("target=codex:test\n")) {
		t.Errorf("unusable resume did not atomically retain transcript/rung with an empty id: %v\n%s", err, state)
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
			if tc.wantCode == 1 && !strings.Contains(out, "coop-consult "+role+` --fresh "<full prompt>"`) {
				t.Errorf("empty reply lacks actionable retry guidance:\n%s", out)
			}
			if resumable {
				t.Error("an unusable or timed-out fresh reply must not leave resumable state")
			}
		})
	}
}

func TestConsultWrapperPlainProviderFailureStateAndReplyStream(t *testing.T) {
	const passTimeout = `shift 3
exec "$@"`
	for _, peer := range []string{"claude", "gemini", "grok"} {
		peer := peer
		t.Run(peer+" ordinary failure", func(t *testing.T) {
			out, code, _, resumable := runConsultWrapperStub(t, "failure-"+peer, peer, `echo PROVIDER_DIAGNOSTIC >&2; exit 7`, passTimeout, "")
			if code != 7 || !strings.Contains(out, "PROVIDER_DIAGNOSTIC") {
				t.Fatalf("ordinary failure = exit %d:\n%s", code, out)
			}
			if resumable {
				t.Error("failed fresh provider call left resumable state")
			}
		})
		t.Run(peer+" stderr only success", func(t *testing.T) {
			role := "stderr-only-" + peer
			out, code, _, resumable := runConsultWrapperStub(t, role, peer, `echo PROVIDER_WARNING >&2`, passTimeout, "")
			if code != 1 || !strings.Contains(out, "PROVIDER_WARNING") || !strings.Contains(out, "provider returned no usable reply") {
				t.Fatalf("stderr-only success = exit %d:\n%s", code, out)
			}
			if resumable {
				t.Error("stderr-only success became a resumable advisor reply")
			}
		})
	}
}

func TestConsultWrapperFailedResumeRestartsFromTranscriptOnNextContinue(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "coop-consult")
	if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "provider-state")
	calls := filepath.Join(dir, "calls")
	provider := `printf '%s\n' "$*" >>"$CALLS"
case " $* " in
*" --resume "*) echo SESSION_GONE >&2; exit 7 ;;
esac
if [ -f "$STATE" ]; then echo RECOVERED_REPLY; else : >"$STATE"; echo FIRST_REPLY; fi`
	for name, body := range map[string]string{
		"claude":  "#!/bin/sh\n" + provider + "\n",
		"timeout": "#!/bin/sh\nshift 3\nexec \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	role := "resume-recovery"
	key := strings.ToUpper(strings.ReplaceAll(role, "-", "_"))
	stateDir := filepath.Join(dir, "coop-consult-state")
	run := func(prompt string) (string, int) {
		t.Helper()
		cmd := exec.Command(wrapper, role, "--continue", prompt)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"PATH="+dir+":"+os.Getenv("PATH"), "TMPDIR="+dir,
			"STATE="+state, "CALLS="+calls, "COOP_PEERS=claude",
			"COOP_CONSULT_"+key+"_TARGETS=claude:test",
		)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return string(out), exit.ExitCode()
		}
		t.Fatalf("wrapper: %v\n%s", err, out)
		return "", -1
	}
	defer os.RemoveAll(stateDir)
	if out, code := run("FIRST_QUESTION"); code != 0 || !strings.Contains(out, "FIRST_REPLY") {
		t.Fatalf("initial continue-without-id = exit %d:\n%s", code, out)
	}
	if out, code := run("SECOND_DELTA"); code != 7 || !strings.Contains(out, "SESSION_GONE") {
		t.Fatalf("failed resume = exit %d:\n%s", code, out)
	}
	stateData, err := os.ReadFile(filepath.Join(stateDir, "RESUME_RECOVERY.state"))
	if err != nil || !bytes.Contains(stateData, []byte("\nid=\n\n")) {
		t.Fatalf("failed resume retained an uncertain id or lost recovery state: %v\n%s", err, stateData)
	}
	out, code := run("THIRD_DELTA")
	if code != 0 || !strings.Contains(out, "RECOVERED_REPLY") {
		t.Fatalf("next continue did not recover fresh = exit %d:\n%s", code, out)
	}
	gotCalls, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"saved transcript", "FIRST_QUESTION", "FIRST_REPLY", "THIRD_DELTA"} {
		if !strings.Contains(string(gotCalls), want) {
			t.Errorf("recovery prompt missing %q:\n%s", want, gotCalls)
		}
	}
}

func TestConsultWrapperDirectCodexTelemetryUsesProviderIdentity(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "coop-consult")
	if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"codex": `#!/bin/sh
printf '%s\n' \
'{"type":"thread.started","thread_id":"thread-direct"}' \
'{"type":"item.completed","item":{"type":"agent_message","text":"DIRECT_REPLY"}}' \
'{"type":"turn.completed","usage":{"input_tokens":3,"output_tokens":2}}'
`,
		"timeout": "#!/bin/sh\nshift 3\nexec \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, ".agent", "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".agent", "runs", "direct-run.peers.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(wrapper, "codex", "--fresh", "question")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TMPDIR="+dir, "COOP_PEERS=codex", "COOP_RUN_ID=direct-run")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("direct Codex consult: %v\n%s", err, out)
	}
	row, err := os.ReadFile(filepath.Join(dir, ".agent", "runs", "direct-run.peers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(row), `"role":"codex"`) {
		t.Fatalf("direct peer telemetry lost invocation identity: %s", row)
	}
}

func TestConsultWrapperBoundsProviderOutputAndContinuation(t *testing.T) {
	const passTimeout = `shift 3
exec "$@"`
	t.Run("reply overflow is terminal", func(t *testing.T) {
		body := `dd if=/dev/zero bs=1048577 count=1 2>/dev/null | tr '\000' X`
		out, code, _, resumable := runConsultWrapperStub(t, "reply-overflow", "claude", body, passTimeout, "")
		if code != 1 || !strings.Contains(out, "reply exceeded 1048576 bytes") {
			t.Fatalf("reply overflow = exit %d, bytes %d:\n%s", code, len(out), out)
		}
		if resumable {
			t.Error("overflowed reply became resumable")
		}
		if len(out) > 64<<10 {
			t.Fatalf("overflowed reply leaked its bounded spool to the caller: %d bytes", len(out))
		}
	})

	t.Run("stdin prompt overflow fails before dispatch", func(t *testing.T) {
		dir := t.TempDir()
		wrapper := filepath.Join(dir, "coop-consult")
		calls := filepath.Join(dir, "calls")
		for name, body := range map[string]string{
			"coop-consult": ConsultWrapper(),
			"claude":       "#!/bin/sh\nprintf called >\"$CALLS\"\necho SHOULD_NOT_RUN\n",
			"timeout":      "#!/bin/sh\nshift 3\nexec \"$@\"\n",
		} {
			mode := os.FileMode(0o755)
			if name == "coop-consult" {
				body = ConsultWrapper()
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command(wrapper, "claude", "--fresh")
		cmd.Stdin = strings.NewReader(strings.Repeat("Q", consultPromptLimitBytes+1))
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TMPDIR="+dir,
			"CALLS="+calls, "COOP_PEERS=claude")
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "question exceeds 524288 bytes") {
			t.Fatalf("oversized stdin prompt was not rejected: %v\n%s", err, out)
		}
		if _, err := os.Stat(calls); !os.IsNotExist(err) {
			t.Fatalf("provider dispatched for oversized stdin prompt: %v", err)
		}
	})

	t.Run("capture helper failure is terminal", func(t *testing.T) {
		dir := t.TempDir()
		wrapper := filepath.Join(dir, "coop-consult")
		for name, body := range map[string]string{
			"claude":  "#!/bin/sh\necho SHOULD_NOT_BE_ACCEPTED\n",
			"dd":      "#!/bin/sh\nexit 70\n",
			"timeout": "#!/bin/sh\nshift 3\nexec \"$@\"\n",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
			t.Fatal(err)
		}
		role := "capture-failure"
		cmd := exec.Command(wrapper, role, "--fresh", "question")
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TMPDIR="+dir,
			"COOP_PEERS=claude", "COOP_CONSULT_CAPTURE_FAILURE_TARGETS=claude:test")
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "failed to capture provider output safely") {
			t.Fatalf("capture failure was not terminal: %v\n%s", err, out)
		}
		stateDir := filepath.Join(dir, "coop-consult-state")
		if _, err := os.Stat(filepath.Join(stateDir, "CAPTURE_FAILURE.state")); !os.IsNotExist(err) {
			t.Errorf("capture failure retained continuation state: %v", err)
		}
	})

	t.Run("transcript overflow delivers reply then clears continuity", func(t *testing.T) {
		dir := t.TempDir()
		wrapper := filepath.Join(dir, "coop-consult")
		if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{
			"claude":  "#!/bin/sh\ndd if=/dev/zero bs=270000 count=1 2>/dev/null | tr '\\000' Y\n",
			"timeout": "#!/bin/sh\nshift 3\nexec \"$@\"\n",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		role := "context-overflow"
		key := "CONTEXT_OVERFLOW"
		run := func(mode, prompt string) (string, int) {
			cmd := exec.Command(wrapper, role, mode, prompt)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TMPDIR="+dir,
				"COOP_PEERS=claude", "COOP_CONSULT_"+key+"_TARGETS=claude:test")
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
		if out, code := run("--fresh", "first"); code != 0 || len(out) < 270000 {
			t.Fatalf("first bounded turn = exit %d, bytes %d", code, len(out))
		}
		out, code := run("--continue", "second")
		if code != 0 || !strings.Contains(out, "reply delivered, but continuity exceeded 524288 bytes") {
			t.Fatalf("context overflow = exit %d, bytes %d, tail %q", code, len(out), out[max(0, len(out)-256):])
		}
		stateDir := filepath.Join(dir, "coop-consult-state")
		if _, err := os.Stat(filepath.Join(stateDir, key+".state")); !os.IsNotExist(err) {
			t.Errorf("context overflow retained continuation state: %v", err)
		}
	})
}

func TestConsultWrapperRejectsUnsafeContinuationRecordBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T, statefile string)
		want string
	}{
		{
			name: "symlink",
			make: func(t *testing.T, statefile string) {
				t.Helper()
				target := filepath.Join(filepath.Dir(statefile), "outside")
				if err := os.WriteFile(target, []byte("session"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, statefile); err != nil {
					t.Fatal(err)
				}
			},
			want: "unsafe continuation state file",
		},
		{
			name: "hardlink",
			make: func(t *testing.T, statefile string) {
				t.Helper()
				if err := os.WriteFile(statefile, []byte("session"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(statefile, statefile+".other"); err != nil {
					t.Fatal(err)
				}
			},
			want: "must have one hardlink",
		},
		{
			name: "permissive mode",
			make: func(t *testing.T, statefile string) {
				t.Helper()
				if err := os.WriteFile(statefile, []byte("session"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "must be mode 0600",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			wrapper := filepath.Join(dir, "coop-consult")
			calls := filepath.Join(dir, "calls")
			for name, body := range map[string]string{
				"claude":  "#!/bin/sh\nprintf called >\"$CALLS\"\n",
				"timeout": "#!/bin/sh\nshift 3\nexec \"$@\"\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
				t.Fatal(err)
			}
			stateDir := filepath.Join(dir, "coop-consult-state")
			if err := os.Mkdir(stateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			tc.make(t, filepath.Join(stateDir, "UNSAFE_STATE.state"))
			cmd := exec.Command(wrapper, "unsafe-state", "--continue", "question")
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TMPDIR="+dir,
				"CALLS="+calls, "COOP_PEERS=claude", "COOP_CONSULT_UNSAFE_STATE_TARGETS=claude:test")
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("unsafe %s state was not rejected: %v\n%s", tc.name, err, out)
			}
			if _, err := os.Stat(calls); !os.IsNotExist(err) {
				t.Fatalf("provider dispatched with unsafe %s state: %v", tc.name, err)
			}
		})
	}
}

func TestConsultWrapperLadderReorderInvalidatesNativeSession(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "coop-consult")
	calls := filepath.Join(dir, "calls")
	for name, body := range map[string]string{
		"claude": `#!/bin/sh
echo "claude $*" >>"$CALLS"
echo "usage limit reached" >&2
exit 9
`,
		"gemini": `#!/bin/sh
echo "gemini $*" >>"$CALLS"
echo GEMINI_REPLY
`,
		"timeout": "#!/bin/sh\nshift 3\nexec \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(ladder, mode string) (string, int) {
		t.Helper()
		cmd := exec.Command(wrapper, "reorder", mode, "question")
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TMPDIR="+dir,
			"CALLS="+calls, "COOP_PRIMARY=codex", "COOP_PEERS=claude gemini", "COOP_CONSULT_REORDER_TARGETS="+ladder)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return string(out), exit.ExitCode()
		}
		t.Fatalf("wrapper: %v\n%s", err, out)
		return "", -1
	}
	if out, code := run("claude:one gemini:two", "--fresh"); code != 0 || !strings.Contains(out, "GEMINI_REPLY") {
		t.Fatalf("initial fallback = exit %d:\n%s", code, out)
	}
	if err := os.WriteFile(calls, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := run("gemini:two claude:one", "--continue")
	if code != 0 || !strings.Contains(out, "started FRESH from the saved transcript") {
		t.Fatalf("reordered ladder did not restart safely = exit %d:\n%s", code, out)
	}
	got, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "--resume") || strings.Contains(string(got), "claude ") ||
		!strings.Contains(string(got), "gemini --approval-mode plan --session-id") || !strings.Contains(string(got), " -p ") {
		t.Fatalf("reordered ladder reused the old rung identity:\n%s", got)
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
	if code != 0 || !strings.Contains(out, "resuming on gemini:gemini-3.5-flash") {
		t.Fatalf("continue selected rung = (exit %d):\n%s", code, out)
	}
	continuedCalls, _ := os.ReadFile(calls)
	if strings.Contains(string(continuedCalls), "claude ") || !strings.Contains(string(continuedCalls), "gemini --approval-mode plan --resume") {
		t.Fatalf("continue must resume only the successful Gemini rung:\n%s", continuedCalls)
	}
}

func TestConsultWrapperFailedFallbackResumeRestartsSameRung(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "coop-consult")
	calls := filepath.Join(dir, "calls")
	for name, body := range map[string]string{
		"claude": `#!/bin/sh
echo "claude $*" >>"$CALLS"
echo "usage limit reached" >&2
exit 9
`,
		"gemini": `#!/bin/sh
echo "gemini $*" >>"$CALLS"
case " $* " in
*" --resume "*) echo SESSION_GONE >&2; exit 7 ;;
esac
echo GEMINI_REPLY
`,
		"timeout": "#!/bin/sh\nshift 3\nexec \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(mode, question string) (string, int) {
		t.Helper()
		cmd := exec.Command(wrapper, "fallback-recovery", mode, question)
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TMPDIR="+dir,
			"CALLS="+calls, "COOP_PRIMARY=codex", "COOP_PEERS=claude gemini",
			"COOP_CONSULT_FALLBACK_RECOVERY_TARGETS=claude:one gemini:two")
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return string(out), exit.ExitCode()
		}
		t.Fatalf("wrapper: %v\n%s", err, out)
		return "", -1
	}
	if out, code := run("--fresh", "first question"); code != 0 || !strings.Contains(out, "GEMINI_REPLY") {
		t.Fatalf("initial fallback = exit %d:\n%s", code, out)
	}
	if err := os.WriteFile(calls, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := run("--continue", "second delta"); code != 7 || !strings.Contains(out, "SESSION_GONE") {
		t.Fatalf("failed fallback resume = exit %d:\n%s", code, out)
	}
	if out, code := run("--continue", "third delta"); code != 0 || !strings.Contains(out, "started FRESH from the saved transcript") || !strings.Contains(out, "GEMINI_REPLY") {
		t.Fatalf("next continue did not restart fallback rung = exit %d:\n%s", code, out)
	}
	got, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "claude ") || strings.Count(string(got), "gemini ") != 2 {
		t.Fatalf("failed fallback resume returned to the wrong rung:\n%s", got)
	}
}

func TestConsultWrapperRateLimitedResumePublishesSuccessfulFallback(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "coop-consult")
	calls := filepath.Join(dir, "calls")
	scripts := map[string]string{
		"claude": `#!/bin/sh
echo "claude $*" >>"$CALLS"
case " $* " in
*" --resume "*) echo "usage limit reached" >&2; exit 9 ;;
esac
echo CLAUDE_REPLY
`,
		"gemini": `#!/bin/sh
echo "gemini $*" >>"$CALLS"
case " $* " in
*" --resume "*) echo GEMINI_CONTINUED ;;
*) echo GEMINI_FALLBACK ;;
esac
`,
		"timeout": "#!/bin/sh\nshift 3\nexec \"$@\"\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(wrapper, []byte(ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(mode, question string) (string, int) {
		t.Helper()
		cmd := exec.Command(wrapper, "resume-fallback", mode, question)
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TMPDIR="+dir,
			"CALLS="+calls, "COOP_PRIMARY=codex", "COOP_PEERS=claude gemini",
			"COOP_CONSULT_RESUME_FALLBACK_TARGETS=claude:one gemini:two")
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return string(out), exit.ExitCode()
		}
		t.Fatalf("wrapper: %v\n%s", err, out)
		return "", -1
	}
	if out, code := run("--fresh", "first"); code != 0 || !strings.Contains(out, "CLAUDE_REPLY") {
		t.Fatalf("initial consult = exit %d:\n%s", code, out)
	}
	if out, code := run("--continue", "second"); code != 0 || !strings.Contains(out, "GEMINI_FALLBACK") ||
		!strings.Contains(out, "uncertain native session cleared and saved transcript retained") || strings.Contains(out, "next coop-consult") {
		t.Fatalf("resume fallback = exit %d:\n%s", code, out)
	}
	if out, code := run("--continue", "third"); code != 0 || !strings.Contains(out, "GEMINI_CONTINUED") {
		t.Fatalf("fallback continuation = exit %d:\n%s", code, out)
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
			wantText:   "exhausted all 2 targets",
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
