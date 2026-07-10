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
		"COOP_DELEGATE_FAST_AGENT=gemini",
		"COOP_DELEGATE_FAST_MODEL=gemini-3.5-flash",
		"COOP_DELEGATE_FAST_CONTRACT="+contract,
		"COOP_DELEGATE_LOCK="+h.lock,
	)
	return h
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
	// A role with no COOP_DELEGATE_<ROLE>_AGENT export isn't a delegate role.
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
