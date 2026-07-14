package preset

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDelegateWrapperShellcheck keeps the embedded coop-delegate script clean, like the
// coop-consult wrapper's check (skipped when shellcheck isn't installed).
func TestDelegateWrapperShellcheck(t *testing.T) {
	sc := shellcheckPath(t)
	f := filepath.Join(t.TempDir(), "coop-delegate")
	if err := os.WriteFile(f, []byte(DelegateWrapper()), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(sc, f).CombinedOutput(); err != nil {
		t.Errorf("shellcheck flagged the delegate wrapper:\n%s", out)
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

// delegateHarness lays out everything a wrapper run needs: the script, a stub agent
// binary that records its argv (and can run extra shell), a git repo to run in, and a
// contract file. It returns a runner bound to that environment.
type delegateHarness struct {
	t       *testing.T
	dir     string // PATH dir holding the script + stubs
	repo    string // cwd for runs (a real git repo)
	argsLog string // where stubs record their argv
	lock    string
	env     []string
}

func newDelegateHarness(t *testing.T) *delegateHarness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	h := &delegateHarness{t: t, dir: t.TempDir(), repo: t.TempDir()}
	h.argsLog = filepath.Join(h.dir, "argv")
	h.lock = filepath.Join(h.dir, "lock")
	if err := os.WriteFile(filepath.Join(h.dir, "coop-delegate"), []byte(DelegateWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range [][]string{
		{"git", "init", "-q"},
		{"git", "config", "user.email", "t@t"},
		{"git", "config", "user.name", "t"},
		{"git", "commit", "--allow-empty", "-qm", "init"},
	} {
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Dir = h.repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", cmd, err, out)
		}
	}
	contract := filepath.Join(h.dir, "contract.md")
	if err := os.WriteFile(contract, []byte("ROLE CONTRACT TEXT"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.env = append(os.Environ(),
		"PATH="+h.dir+":"+os.Getenv("PATH"),
		"COOP_DELEGATE_FAST_TARGETS=gemini:gemini-3.5-flash",
		"COOP_DELEGATE_FAST_CONTRACT="+contract,
		"COOP_DELEGATE_LOCK="+h.lock,
		"COOP_PRIMARY=claude",
		"COOP_PEERS=codex gemini grok",
	)
	return h
}

func TestDelegateWrapperFallsBackOnRateLimit(t *testing.T) {
	h := newDelegateHarness(t)
	calls := filepath.Join(h.dir, "calls")
	h.env = append(h.env, "COOP_DELEGATE_FAST_TARGETS=codex:gpt-5.6-sol/xhigh gemini:gemini-3.5-flash")
	h.stub("codex", "echo codex >>"+calls+"; echo 'usage limit reached' >&2; exit 9")
	h.stub("gemini", "echo gemini >>"+calls+"; echo fallback-work")
	out, code := h.run("fast", "Implement the thing")
	if code != 0 {
		t.Fatalf("exit = %d, want fallback success:\n%s", code, out)
	}
	got, _ := os.ReadFile(calls)
	if string(got) != "codex\ngemini\n" {
		t.Fatalf("calls = %q, want rung 0 then rung 1", got)
	}
	for _, want := range []string{"codex:gpt-5.6-sol/xhigh rate limited", "done on gemini:gemini-3.5-flash"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDelegateWrapperDoesNotFallbackOnOrdinaryFailure(t *testing.T) {
	h := newDelegateHarness(t)
	calls := filepath.Join(h.dir, "calls")
	h.env = append(h.env, "COOP_DELEGATE_FAST_TARGETS=codex gemini")
	h.stub("codex", "echo codex >>"+calls+"; echo ordinary-failure >&2; exit 7")
	h.stub("gemini", "echo gemini >>"+calls)
	out, code := h.run("fast", "task")
	if code != 7 {
		t.Fatalf("exit = %d, want original failure 7:\n%s", code, out)
	}
	got, _ := os.ReadFile(calls)
	if string(got) != "codex\n" {
		t.Fatalf("calls = %q, want no fallback", got)
	}
}

func TestDelegateWrapperStopsFallbackAfterEdit(t *testing.T) {
	h := newDelegateHarness(t)
	calls := filepath.Join(h.dir, "calls")
	h.env = append(h.env, "COOP_DELEGATE_FAST_TARGETS=codex gemini")
	h.stub("codex", "echo codex >>"+calls+"; echo partial > partial.txt; echo 'rate limit exceeded' >&2; exit 8")
	h.stub("gemini", "echo gemini >>"+calls)
	out, code := h.run("fast", "task")
	if code != 8 || !strings.Contains(out, "after changing the worktree") {
		t.Fatalf("changed-tree limit = (exit %d), want safe refusal:\n%s", code, out)
	}
	got, _ := os.ReadFile(calls)
	if string(got) != "codex\n" {
		t.Fatalf("calls = %q, want no fallback after an edit", got)
	}
}

func TestDelegateWrapperStopsFallbackAfterIgnoredEdit(t *testing.T) {
	h := newDelegateHarness(t)
	calls := filepath.Join(h.dir, "calls")
	if err := os.WriteFile(filepath.Join(h.repo, ".gitignore"), []byte("ignored-state\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", ".gitignore"}, {"commit", "-qm", "ignore fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = h.repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	h.env = append(h.env, "COOP_DELEGATE_FAST_TARGETS=codex gemini")
	h.stub("codex", "echo codex >>"+calls+"; echo changed > ignored-state; echo 'rate limit exceeded' >&2; exit 8")
	h.stub("gemini", "echo gemini >>"+calls)
	out, code := h.run("fast", "task")
	if code != 8 || !strings.Contains(out, "changing ignored files") {
		t.Fatalf("ignored edit = (exit %d), want safe refusal:\n%s", code, out)
	}
	got, _ := os.ReadFile(calls)
	if string(got) != "codex\n" {
		t.Fatalf("calls = %q, want no fallback after ignored edit", got)
	}
}

func TestDelegateWrapperStopsFallbackAfterCommitAndReset(t *testing.T) {
	h := newDelegateHarness(t)
	calls := filepath.Join(h.dir, "calls")
	h.env = append(h.env, "COOP_DELEGATE_FAST_TARGETS=codex gemini")
	h.stub("codex", "echo codex >>"+calls+"; git commit --allow-empty -qm transient; git reset --hard HEAD^ >/dev/null; echo 'rate limit exceeded' >&2; exit 8")
	h.stub("gemini", "echo gemini >>"+calls)
	out, code := h.run("fast", "task")
	if code != 8 || !strings.Contains(out, "Git history") {
		t.Fatalf("commit-reset = (exit %d), want reflog refusal:\n%s", code, out)
	}
	got, _ := os.ReadFile(calls)
	if string(got) != "codex\n" {
		t.Fatalf("calls = %q, want no fallback after commit-reset", got)
	}
}

func TestDelegateWrapperFailsClosedWhenGitStatusFails(t *testing.T) {
	h := newDelegateHarness(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\nif [ \"$1\" = status ]; then exit 70; fi\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(h.dir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(h.dir, "calls")
	h.env = append(h.env, "COOP_DELEGATE_FAST_TARGETS=codex gemini")
	h.stub("codex", "echo codex >>"+calls+"; echo 'rate limit exceeded' >&2; exit 8")
	h.stub("gemini", "echo gemini >>"+calls)
	out, code := h.run("fast", "task")
	if code != 8 || !strings.Contains(out, "could not be verified") {
		t.Fatalf("git status failure = (exit %d), want fail-closed refusal:\n%s", code, out)
	}
	got, _ := os.ReadFile(calls)
	if string(got) != "codex\n" {
		t.Fatalf("calls = %q, want no fallback when Git inspection fails", got)
	}
}

func TestDelegateWrapperReleasesLockWhenAttemptDirFails(t *testing.T) {
	h := newDelegateHarness(t)
	notDir := filepath.Join(h.dir, "not-a-directory")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.env = append(h.env, "TMPDIR="+notDir)
	h.stub("gemini", "")
	out, code := h.run("fast", "task")
	if code != 2 || !strings.Contains(out, "cannot create attempt directory") {
		t.Fatalf("attempt-dir failure = (exit %d):\n%s", code, out)
	}
	if _, err := os.Stat(h.lock); !os.IsNotExist(err) {
		t.Fatalf("delegate lock leaked after attempt-dir failure: %v", err)
	}
}

func TestDelegateWrapperStopsFallbackFromDirtyBaseline(t *testing.T) {
	h := newDelegateHarness(t)
	calls := filepath.Join(h.dir, "calls")
	h.env = append(h.env, "COOP_DELEGATE_FAST_TARGETS=codex gemini")
	if err := os.WriteFile(filepath.Join(h.repo, "existing.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.stub("codex", "echo codex >>"+calls+"; echo 'rate limit exceeded' >&2; exit 6")
	h.stub("gemini", "echo gemini >>"+calls)
	out, code := h.run("fast", "task")
	if code != 6 || !strings.Contains(out, "worktree was already dirty") {
		t.Fatalf("dirty-tree limit = (exit %d), want safe refusal:\n%s", code, out)
	}
	got, _ := os.ReadFile(calls)
	if string(got) != "codex\n" {
		t.Fatalf("calls = %q, want no fallback from dirty baseline", got)
	}
}

func TestDelegateWrapperFallbackDecisionMatrix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		codexBody  string
		geminiBody string
		wantCode   int
		wantGemini bool
		wantText   string
	}{
		{
			name:       "successful prose mentioning a limit",
			codexBody:  `echo codex >>"$CALLS"; echo "rate limit handling documented"`,
			geminiBody: `echo gemini >>"$CALLS"`,
			wantCode:   0,
		},
		{
			name:       "failed prose mentioning a limit",
			codexBody:  `echo codex >>"$CALLS"; echo "failed while task text mentions rate limit handling"; exit 7`,
			geminiBody: `echo gemini >>"$CALLS"`,
			wantCode:   7,
		},
		{
			name:       "all rungs limited once",
			codexBody:  `echo codex >>"$CALLS"; echo "usage limit reached" >&2; exit 9`,
			geminiBody: `echo gemini >>"$CALLS"; echo "RESOURCE_EXHAUSTED" >&2; exit 8`,
			wantCode:   8,
			wantGemini: true,
			wantText:   "FAILED on gemini:two",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDelegateHarness(t)
			calls := filepath.Join(h.dir, "calls")
			h.env = append(h.env,
				"CALLS="+calls,
				"COOP_DELEGATE_FAST_TARGETS=codex:one gemini:two",
			)
			h.stub("codex", tc.codexBody)
			h.stub("gemini", tc.geminiBody)
			out, code := h.run("fast", "task")
			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d:\n%s", code, tc.wantCode, out)
			}
			gotCalls, _ := os.ReadFile(calls)
			hasGemini := strings.Contains(string(gotCalls), "gemini")
			if hasGemini != tc.wantGemini {
				t.Errorf("Gemini called = %v, want %v:\n%s", hasGemini, tc.wantGemini, gotCalls)
			}
			if strings.Count(string(gotCalls), "codex") != 1 || strings.Count(string(gotCalls), "gemini") > 1 {
				t.Errorf("each rung must run at most once:\n%s", gotCalls)
			}
			if tc.wantText != "" && !strings.Contains(out, tc.wantText) {
				t.Errorf("output missing %q:\n%s", tc.wantText, out)
			}
		})
	}
}

func TestDelegateWrapperValidatesEveryRungBeforeDispatch(t *testing.T) {
	h := newDelegateHarness(t)
	calls := filepath.Join(h.dir, "calls")
	h.env = append(h.env, "COOP_DELEGATE_FAST_TARGETS=codex:one not-a-provider:two", "CALLS="+calls)
	h.stub("codex", `echo called >"$CALLS"`)
	out, code := h.run("fast", "task")
	if code != 2 || !strings.Contains(out, "unknown agent") {
		t.Fatalf("invalid later rung = (exit %d), want validation failure:\n%s", code, out)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatal("first rung dispatched before the later rung was validated")
	}
}

func TestDelegateWrapperSkipsRungWithoutMountedCredentials(t *testing.T) {
	h := newDelegateHarness(t)
	for i, entry := range h.env {
		if strings.HasPrefix(entry, "COOP_DELEGATE_FAST_TARGETS=") {
			h.env[i] = "COOP_DELEGATE_FAST_TARGETS=claude:one codex:two"
		}
		if strings.HasPrefix(entry, "COOP_PEERS=") {
			h.env[i] = "COOP_PEERS=gemini"
		}
	}
	h.stub("claude", "echo AVAILABLE_OK")
	out, code := h.run("fast", "task")
	if code != 0 || !strings.Contains(out, "skipping codex:two") || !strings.Contains(out, "AVAILABLE_OK") {
		t.Fatalf("unmounted fallback should be skipped while valid rung runs (exit %d):\n%s", code, out)
	}
}

// stub installs a fake agent binary that records its argv, then runs extra script.
func (h *delegateHarness) stub(agent, extra string) {
	h.t.Helper()
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\n%s\n", h.argsLog, extra)
	if err := os.WriteFile(filepath.Join(h.dir, agent), []byte(script), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

func (h *delegateHarness) run(args ...string) (string, int) {
	h.t.Helper()
	cmd := exec.Command(filepath.Join(h.dir, "coop-delegate"), args...)
	cmd.Dir = h.repo
	cmd.Env = h.env
	out, err := cmd.CombinedOutput()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		h.t.Fatalf("run coop-delegate: %v\n%s", err, out)
	}
	return string(out), code
}

func TestDelegateWrapperRunsRole(t *testing.T) {
	h := newDelegateHarness(t)
	h.stub("gemini", "echo did-the-work")
	out, code := h.run("fast", "Implement the thing")
	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "[coop-delegate fast: done") || !strings.Contains(out, "git diff") {
		t.Errorf("missing the completion summary for the lead:\n%s", out)
	}
	argv, _ := os.ReadFile(h.argsLog)
	got := string(argv)
	for _, want := range []string{"--yolo", "--model", "gemini-3.5-flash", "ROLE CONTRACT TEXT", "Implement the thing"} {
		if !strings.Contains(got, want) {
			t.Errorf("gemini argv missing %q (model/contract/prompt wiring):\n%s", want, got)
		}
	}
}

func TestDelegateWrapperRejects(t *testing.T) {
	h := newDelegateHarness(t)
	h.stub("gemini", "")
	// A role with no COOP_DELEGATE_<ROLE>_TARGETS export isn't a delegate role.
	if out, code := h.run("ghost", "x"); code != 2 || !strings.Contains(out, "unknown delegate role") {
		t.Errorf("unknown role: (code=%d)\n%s", code, out)
	}
	// Role names are validated before env expansion (they become env-var infixes).
	if out, code := h.run("Bad Role", "x"); code != 2 || !strings.Contains(out, "invalid role name") {
		t.Errorf("invalid role name: (code=%d)\n%s", code, out)
	}
	// No prompt anywhere is an error, not a silent empty delegate run.
	cmd := exec.Command(filepath.Join(h.dir, "coop-delegate"), "fast")
	cmd.Dir, cmd.Env, cmd.Stdin = h.repo, h.env, strings.NewReader("")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "empty prompt") {
		t.Errorf("empty prompt should error:\n%s", out)
	}
}

// commit: never — a delegate that commits is caught by the HEAD comparison and fails
// loud (exit 3), leaving the commit in place as evidence for the lead.
func TestDelegateWrapperDetectsCommit(t *testing.T) {
	h := newDelegateHarness(t)
	h.stub("gemini", "git commit --allow-empty -qm sneaky")
	out, code := h.run("fast", "task")
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (commit despite commit: never):\n%s", code, out)
	}
	if !strings.Contains(out, "COMMITTED") || !strings.Contains(out, "commit: never") {
		t.Errorf("missing the loud commit warning:\n%s", out)
	}
}

// concurrent: never — with the lock held, the wrapper WAITS (not fails) until it frees.
func TestDelegateWrapperWaitsForLock(t *testing.T) {
	h := newDelegateHarness(t)
	h.stub("gemini", "")
	if err := os.Mkdir(h.lock, 0o755); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(1200 * time.Millisecond)
		_ = os.Remove(h.lock)
	}()
	start := time.Now()
	out, code := h.run("fast", "task")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 after waiting for the lock:\n%s", code, out)
	}
	if time.Since(start) < time.Second {
		t.Error("wrapper did not wait for the delegate lock (concurrent: never)")
	}
}

// The wrapper's headless invocations must stay write-capable forms of each agent CLI
// (the delegate edits!) — mirror of the consult wrapper's read-only drift guard.
func TestDelegateWrapperInvocations(t *testing.T) {
	for _, want := range []string{
		"claude -p --dangerously-skip-permissions",
		"gemini --yolo",
		"codex exec --dangerously-bypass-approvals-and-sandbox",
	} {
		if !strings.Contains(DelegateWrapper(), want) {
			t.Errorf("delegate wrapper missing the write-capable invocation %q", want)
		}
	}
}

// fakeDelegate is a minimal 4th agent for the drift test — enough for the delegate generator.
type fakeDelegate struct{}

func (fakeDelegate) Name() string { return "grokfake" }
func (fakeDelegate) DelegateExec() string {
	return `grokfake --write ${model:+--model "$model"} "$prompt"`
}

// TestDelegateWrapperDispatchesNewAgent: a 4th agent's write-capable arm is generated from the
// registry with no hand-edit, and the rendered script still shellchecks clean.
func TestDelegateWrapperDispatchesNewAgent(t *testing.T) {
	w := renderDelegate(append(registeredDelegates(), fakeDelegate{}))
	if !strings.Contains(w, `grokfake) grokfake --write`) {
		t.Fatalf("the new agent's delegate arm is missing:\n%s", w)
	}
	if sc := shellcheckPath(t); sc != "" {
		f := filepath.Join(t.TempDir(), "coop-delegate")
		if err := os.WriteFile(f, []byte(w), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(sc, f).CombinedOutput(); err != nil {
			t.Errorf("shellcheck flagged the delegate wrapper with a 4th agent:\n%s", out)
		}
	}
}
