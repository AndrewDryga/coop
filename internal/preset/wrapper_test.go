package preset

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/fusion"
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
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	flock := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestDelegateFlockHelper$ -- \"$@\"\n", testBinary)
	if err := os.WriteFile(filepath.Join(h.dir, "flock"), []byte(flock), 0o755); err != nil {
		t.Fatal(err)
	}
	setsid := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestDelegateSetsidHelper$ -- \"$@\"\n", testBinary)
	if err := os.WriteFile(filepath.Join(h.dir, "setsid"), []byte(setsid), 0o755); err != nil {
		t.Fatal(err)
	}
	timeout := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestDelegateTimeoutHelper$ -- \"$@\"\n", testBinary)
	if err := os.WriteFile(filepath.Join(h.dir, "timeout"), []byte(timeout), 0o755); err != nil {
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
		"TMPDIR="+h.dir,
		"COOP_DELEGATE_FAST_TARGETS=gemini:gemini-3.5-flash",
		"COOP_DELEGATE_FAST_CONTRACT="+contract,
		"COOP_DELEGATE_LOCK="+h.lock,
		"COOP_PRIMARY=claude",
		"COOP_PEERS=codex gemini grok",
		DelegateDepthEnv+"=0",
		"COOP_DELEGATE_FLOCK_HELPER=1",
		"COOP_DELEGATE_SETSID_HELPER=1",
		"COOP_DELEGATE_TIMEOUT_HELPER=1",
	)
	return h
}

func TestDelegateFlockHelper(t *testing.T) {
	if os.Getenv("COOP_DELEGATE_FLOCK_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 4 || args[0] != "--" || args[1] != "-w" {
		os.Exit(2)
	}
	seconds, err := strconv.Atoi(args[2])
	if err != nil || seconds < 0 {
		os.Exit(2)
	}
	fd, err := strconv.Atoi(args[3])
	if err != nil || fd < 0 {
		os.Exit(2)
	}
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			os.Exit(0)
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			os.Exit(1)
		}
		if !time.Now().Before(deadline) {
			os.Exit(1)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDelegateSetsidHelper(t *testing.T) {
	if os.Getenv("COOP_DELEGATE_SETSID_HELPER") != "1" {
		return
	}
	args := helperArgs()
	if len(args) == 0 {
		os.Exit(2)
	}
	path, err := exec.LookPath(args[0])
	if err != nil {
		os.Exit(127)
	}
	if group, err := syscall.Getpgid(0); err != nil || group == os.Getpid() {
		os.Exit(1)
	}
	if _, err := syscall.Setsid(); err != nil {
		os.Exit(1)
	}
	if err := syscall.Exec(path, args, os.Environ()); err != nil {
		os.Exit(126)
	}
}

func TestDelegateTimeoutHelper(t *testing.T) {
	if os.Getenv("COOP_DELEGATE_TIMEOUT_HELPER") != "1" {
		return
	}
	args := helperArgs()
	if len(args) < 4 || args[0] != "-k" {
		os.Exit(2)
	}
	grace, err := strconv.Atoi(args[1])
	if err != nil || grace < 1 {
		os.Exit(2)
	}
	seconds, err := strconv.Atoi(args[2])
	if err != nil || seconds < 1 {
		os.Exit(2)
	}
	cmd := exec.Command(args[3], args[4:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		os.Exit(126)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		os.Exit(commandExitCode(err))
	case <-time.After(time.Duration(seconds) * time.Second):
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-done:
		os.Exit(124)
	case <-time.After(time.Duration(grace) * time.Second):
		_ = cmd.Process.Kill()
		<-done
		os.Exit(137)
	}
}

func helperArgs() []string {
	for index, arg := range os.Args {
		if arg == "--" {
			return os.Args[index+1:]
		}
	}
	return nil
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 1
}

func TestDelegateWrapperFallsBackOnRateLimit(t *testing.T) {
	h := newDelegateHarness(t)
	calls := filepath.Join(h.dir, "calls")
	h.env = append(h.env, "COOP_DELEGATE_FAST_TARGETS=codex:gpt-5.6-sol/xhigh gemini:gemini-3.5-flash")
	h.stub("codex", "echo codex:$COOP_DELEGATE_DEPTH >>"+calls+"; echo 'usage limit reached' >&2; exit 9")
	h.stub("gemini", "echo gemini:$COOP_DELEGATE_DEPTH >>"+calls+"; echo fallback-work")
	out, code := h.run("fast", "Implement the thing")
	if code != 0 {
		t.Fatalf("exit = %d, want fallback success:\n%s", code, out)
	}
	got, _ := os.ReadFile(calls)
	if string(got) != "codex:1\ngemini:1\n" {
		t.Fatalf("calls = %q, want rung 0 then rung 1", got)
	}
	for _, want := range []string{"codex:gpt-5.6-sol/xhigh rate limited", "done on gemini:gemini-3.5-flash"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDelegateWrapperReportsAllUnmountedTargetsBeforeDispatch(t *testing.T) {
	h := newDelegateHarness(t)
	for index, entry := range h.env {
		if strings.HasPrefix(entry, "COOP_PRIMARY=") || strings.HasPrefix(entry, "COOP_PEERS=") {
			h.env[index] = strings.SplitN(entry, "=", 2)[0] + "="
		}
	}
	launched := filepath.Join(h.dir, "launched")
	h.stub("gemini", "touch "+launched)
	out, code := h.run("fast", "task")
	if code != 2 || !strings.Contains(out, "no target with mounted credentials") {
		t.Fatalf("all-unmounted targets = exit %d, want explicit refusal:\n%s", code, out)
	}
	if _, err := os.Stat(launched); !os.IsNotExist(err) {
		t.Fatalf("provider dispatched without mounted credentials: %v", err)
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
	if code != 3 || !strings.Contains(out, "Git history") {
		t.Fatalf("commit-reset = (exit %d), want reflog refusal:\n%s", code, out)
	}
	got, _ := os.ReadFile(calls)
	if string(got) != "codex\n" {
		t.Fatalf("calls = %q, want no fallback after commit-reset", got)
	}
}

func TestDelegateWrapperDetectsSuccessfulCommitAndReset(t *testing.T) {
	h := newDelegateHarness(t)
	h.stub("gemini", "git commit --allow-empty -qm transient; git reset --hard HEAD^ >/dev/null")
	out, code := h.run("fast", "task")
	if code != 3 || !strings.Contains(out, "Git history") || strings.Contains(out, "done on") {
		t.Fatalf("successful commit-reset = (exit %d), want loud commit:never refusal:\n%s", code, out)
	}
}

func TestDelegateWrapperStopsFallbackAfterRefMutation(t *testing.T) {
	h := newDelegateHarness(t)
	calls := filepath.Join(h.dir, "calls")
	h.env = append(h.env, "COOP_DELEGATE_FAST_TARGETS=codex gemini")
	h.stub("codex", "echo codex >>"+calls+"; git tag hidden-mutation; echo 'rate limit exceeded' >&2; exit 8")
	h.stub("gemini", "echo gemini >>"+calls)
	out, code := h.run("fast", "task")
	if code != 3 || !strings.Contains(out, "Git history") {
		t.Fatalf("ref mutation = (exit %d), want fallback refusal:\n%s", code, out)
	}
	got, _ := os.ReadFile(calls)
	if string(got) != "codex\n" {
		t.Fatalf("calls = %q, want no fallback after ref mutation", got)
	}
}

func TestDelegateWrapperFailsClosedWhenGitHeadIsUnverifiable(t *testing.T) {
	h := newDelegateHarness(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\nif [ \"$1\" = rev-parse ]; then exit 70; fi\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(h.dir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	launched := filepath.Join(h.dir, "launched")
	h.stub("gemini", "touch "+launched)
	out, code := h.run("fast", "task")
	if code != 2 || !strings.Contains(out, "cannot verify Git HEAD") {
		t.Fatalf("unverifiable Git HEAD = (exit %d), want fail-closed refusal:\n%s", code, out)
	}
	if _, err := os.Stat(launched); !os.IsNotExist(err) {
		t.Fatalf("provider dispatched without a verified Git HEAD: %v", err)
	}
}

func TestDelegateWrapperRejectsDisabledReflogsBeforeDispatch(t *testing.T) {
	h := newDelegateHarness(t)
	config := exec.Command("git", "config", "core.logAllRefUpdates", "false")
	config.Dir = h.repo
	if out, err := config.CombinedOutput(); err != nil {
		t.Fatalf("disable reflogs: %v\n%s", err, out)
	}
	launched := filepath.Join(h.dir, "launched")
	h.stub("gemini", "touch "+launched)
	out, code := h.run("fast", "task")
	if code != 2 || !strings.Contains(out, "Git reflogs are disabled") {
		t.Fatalf("disabled reflogs = exit %d, want fail-closed refusal:\n%s", code, out)
	}
	if _, err := os.Stat(launched); !os.IsNotExist(err) {
		t.Fatalf("provider dispatched without reflog proof: %v", err)
	}
}

func TestDelegateWrapperBoundsIgnoredTreeSnapshot(t *testing.T) {
	h := newDelegateHarness(t)
	if err := os.WriteFile(filepath.Join(h.repo, ".gitignore"), []byte("large-ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", ".gitignore"}, {"commit", "-qm", "ignore fixture cache"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = h.repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	large := filepath.Join(h.repo, "large-ignored")
	if err := os.WriteFile(large, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(large, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte{'x'}, 1<<20)
	remaining := int64(delegateSnapshotBlocks+1024) * 1024
	for remaining > 0 {
		write := int64(len(chunk))
		if remaining < write {
			write = remaining
		}
		if _, err := f.Write(chunk[:write]); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		remaining -= write
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(h.dir, "calls")
	h.env = append(h.env, "COOP_DELEGATE_FAST_TARGETS=codex gemini")
	h.stub("codex", "echo codex >>"+calls+"; echo 'usage limit reached' >&2; exit 9")
	h.stub("gemini", "echo gemini >>"+calls)
	out, code := h.run("fast", "task")
	if code != 9 || !strings.Contains(out, "snapshot size/time bounds apply") {
		t.Fatalf("oversized ignored snapshot = exit %d, want bounded fail-closed fallback:\n%s", code, out)
	}
	got, _ := os.ReadFile(calls)
	if string(got) != "codex\n" {
		t.Fatalf("oversized ignored snapshot calls = %q, want no fallback", got)
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
	return h.runContext(context.Background(), args...)
}

func (h *delegateHarness) runContext(ctx context.Context, args ...string) (string, int) {
	h.t.Helper()
	cmd := exec.CommandContext(ctx, filepath.Join(h.dir, "coop-delegate"), args...)
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

func (h *delegateHarness) runGroupDeadline(deadline time.Duration, args ...string) (string, int, bool) {
	h.t.Helper()
	cmd := exec.Command(filepath.Join(h.dir, "coop-delegate"), args...)
	cmd.Dir, cmd.Env = h.repo, h.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return output.String(), commandExitCode(err), false
	case <-time.After(deadline):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return output.String(), -1, true
	}
}

func TestDelegateWrapperRunsRole(t *testing.T) {
	h := newDelegateHarness(t)
	h.stub("gemini", "echo depth=$COOP_DELEGATE_DEPTH; echo did-the-work")
	out, code := h.run("fast", "Implement the thing")
	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "[coop-delegate fast: done") || !strings.Contains(out, "git diff") {
		t.Errorf("missing the completion summary for the lead:\n%s", out)
	}
	if !strings.Contains(out, "depth=1") {
		t.Errorf("delegate child did not receive %s=1:\n%s", DelegateDepthEnv, out)
	}
	argv, _ := os.ReadFile(h.argsLog)
	got := string(argv)
	for _, want := range []string{"--yolo", "--model", "gemini-3.5-flash", "ROLE CONTRACT TEXT", "Implement the thing"} {
		if !strings.Contains(got, want) {
			t.Errorf("gemini argv missing %q (model/contract/prompt wiring):\n%s", want, got)
		}
	}
}

func TestDelegateRoleDoesNotInheritAdHocPeerModel(t *testing.T) {
	h := newDelegateHarness(t)
	h.env = append(h.env,
		"COOP_DELEGATE_FAST_TARGETS=codex",
		"COOP_PEER_MODEL_CODEX=raw-peer-model",
	)
	h.stub("codex", "echo did-the-work")
	if out, code := h.run("fast", "Implement the thing"); code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, out)
	}
	argv, err := os.ReadFile(h.argsLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), "raw-peer-model") {
		t.Fatalf("blank delegate role inherited an ad-hoc peer model:\n%s", argv)
	}
}

func TestDelegateWrapperRejectsRecursiveInvocationBeforeLock(t *testing.T) {
	h := newDelegateHarness(t)
	launched := filepath.Join(h.dir, "launched")
	h.stub("gemini", "touch "+launched)
	if err := os.Mkdir(h.lock, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(h.dir, "coop-delegate"), "fast", "nested work")
	cmd.Dir = h.repo
	cmd.Env = append(h.env, DelegateDepthEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("recursive invocation waited on the delegate lock instead of failing first: %v", ctx.Err())
	}
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 || !strings.Contains(string(out), "recursive delegation is not allowed") {
		t.Fatalf("recursive invocation = (%v):\n%s", err, out)
	}
	if _, err := os.Stat(launched); err == nil || !os.IsNotExist(err) {
		t.Fatal("recursive invocation launched the provider")
	}
}

func TestDelegateDepthDoesNotBlockReadOnlyConsult(t *testing.T) {
	script := filepath.Join(t.TempDir(), "coop-consult")
	if err := os.WriteFile(script, []byte(fusion.ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	cmd.Env = append(os.Environ(), DelegateDepthEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "usage: coop-consult") || strings.Contains(string(out), "recursive") {
		t.Fatalf("read-only consult was affected by delegate depth (%v):\n%s", err, out)
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

func TestDelegateWrapperRejectsOversizedInputBeforeDispatch(t *testing.T) {
	h := newDelegateHarness(t)
	launched := filepath.Join(h.dir, "launched")
	h.stub("gemini", "touch "+launched)
	cmd := exec.Command(filepath.Join(h.dir, "coop-delegate"), "fast")
	cmd.Dir, cmd.Env = h.repo, h.env
	cmd.Stdin = strings.NewReader(strings.Repeat("p", delegatePromptLimitBytes+1))
	out, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 || !strings.Contains(string(out), fmt.Sprintf("prompt exceeds %d bytes", delegatePromptLimitBytes)) {
		t.Fatalf("oversized input = (%v):\n%s", err, out)
	}
	if _, err := os.Stat(launched); !os.IsNotExist(err) {
		t.Fatalf("oversized input dispatched provider: %v", err)
	}
}

func TestDelegateWrapperAcceptsPromptAtExecutableBoundary(t *testing.T) {
	h := newDelegateHarness(t)
	for index, entry := range h.env {
		if strings.HasPrefix(entry, "COOP_DELEGATE_FAST_CONTRACT=") {
			h.env[index] = "COOP_DELEGATE_FAST_CONTRACT="
		}
	}
	h.stub("gemini", "echo reached")
	cmd := exec.Command(filepath.Join(h.dir, "coop-delegate"), "fast")
	cmd.Dir, cmd.Env = h.repo, h.env
	cmd.Stdin = strings.NewReader(strings.Repeat("p", delegatePromptLimitBytes))
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "reached") {
		t.Fatalf("exact-boundary prompt failed: %v\n%s", err, out)
	}
}

func TestDelegateWrapperRejectsOversizedConstructedPromptBeforeDispatch(t *testing.T) {
	h := newDelegateHarness(t)
	launched := filepath.Join(h.dir, "launched")
	h.stub("gemini", "touch "+launched)
	for _, entry := range h.env {
		if strings.HasPrefix(entry, "COOP_DELEGATE_FAST_CONTRACT=") {
			contract := strings.TrimPrefix(entry, "COOP_DELEGATE_FAST_CONTRACT=")
			if err := os.WriteFile(contract, []byte(strings.Repeat("c", delegatePromptLimitBytes-32)), 0o600); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	out, code := h.run("fast", strings.Repeat("q", 64))
	if code != 2 || !strings.Contains(out, "constructed prompt exceeds") {
		t.Fatalf("oversized constructed prompt = exit %d, want refusal:\n%s", code, out)
	}
	if _, err := os.Stat(launched); !os.IsNotExist(err) {
		t.Fatalf("oversized constructed prompt dispatched provider: %v", err)
	}
}

func TestDelegateWrapperRejectsInvalidTimeoutBeforeDispatch(t *testing.T) {
	for _, value := range []string{"0", "not-a-number", "86401"} {
		t.Run(value, func(t *testing.T) {
			h := newDelegateHarness(t)
			launched := filepath.Join(h.dir, "launched")
			h.stub("gemini", "touch "+launched)
			h.env = append(h.env, "COOP_DELEGATE_TIMEOUT="+value)
			out, code := h.run("fast", "task")
			if code != 2 || !strings.Contains(out, "COOP_DELEGATE_TIMEOUT") {
				t.Fatalf("timeout %q = exit %d, want refusal:\n%s", value, code, out)
			}
			if _, err := os.Stat(launched); !os.IsNotExist(err) {
				t.Fatalf("timeout %q dispatched provider: %v", value, err)
			}
		})
	}
}

func TestDelegateWrapperStopsOutputOverflowWithoutFallback(t *testing.T) {
	h := newDelegateHarness(t)
	calls := filepath.Join(h.dir, "calls")
	h.env = append(h.env, "COOP_DELEGATE_FAST_TARGETS=codex gemini")
	h.stub("codex", "echo codex >>"+calls+"; dd if=/dev/zero bs=1048577 count=1 2>/dev/null | tr '\\000' X; echo 'usage limit reached' >&2; exit 9")
	h.stub("gemini", "echo gemini >>"+calls)
	out, code := h.run("fast", "task")
	if code != 1 || !strings.Contains(out, "output exceeded 1048576 bytes") || len(out) > (1<<20)+(64<<10) {
		t.Fatalf("overflow = exit %d bytes %d, want bounded terminal failure:\n%s", code, len(out), out[max(0, len(out)-4096):])
	}
	got, _ := os.ReadFile(calls)
	if string(got) != "codex\n" {
		t.Fatalf("calls = %q, want no fallback after overflow", got)
	}
}

func TestDelegateWrapperTimesOutWithoutFallback(t *testing.T) {
	h := newDelegateHarness(t)
	calls := filepath.Join(h.dir, "calls")
	h.env = append(h.env, "COOP_DELEGATE_FAST_TARGETS=codex gemini", "COOP_DELEGATE_TIMEOUT=1")
	h.stub("codex", "echo codex >>"+calls+"; echo 'usage limit reached' >&2; trap 'exit 143' TERM; while :; do sleep 1; done")
	h.stub("gemini", "echo gemini >>"+calls)
	start := time.Now()
	out, code, exceeded := h.runGroupDeadline(5*time.Second, "fast", "task")
	if exceeded || code != 124 || time.Since(start) > 5*time.Second || !strings.Contains(out, "bounded execution failed") {
		t.Fatalf("timeout = exit %d after %s, want bounded terminal failure:\n%s", code, time.Since(start), out)
	}
	got, _ := os.ReadFile(calls)
	if string(got) != "codex\n" {
		t.Fatalf("calls = %q, want no fallback after timeout", got)
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
	lock, err := os.OpenFile(h.lock, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(1200 * time.Millisecond)
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	}()
	start := time.Now()
	out, code := h.run("fast", "task")
	<-released
	if code != 0 {
		t.Fatalf("exit = %d, want 0 after waiting for the lock:\n%s", code, out)
	}
	if time.Since(start) < time.Second {
		t.Error("wrapper did not wait for the delegate lock (concurrent: never)")
	}
}

func TestDelegateWrapperRejectsUnsafePrecreatedLock(t *testing.T) {
	h := newDelegateHarness(t)
	launched := filepath.Join(h.dir, "launched")
	h.stub("gemini", "touch "+launched)
	if err := os.Mkdir(h.lock, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	out, code := h.runContext(ctx, "fast", "task")
	if ctx.Err() != nil {
		t.Fatalf("unsafe precreated lock hung instead of failing closed: %v", ctx.Err())
	}
	if code != 2 || !strings.Contains(out, "unsafe delegate lock") {
		t.Fatalf("unsafe precreated lock = (exit %d), want explicit refusal:\n%s", code, out)
	}
	if _, err := os.Stat(launched); !os.IsNotExist(err) {
		t.Fatalf("provider dispatched through unsafe lock: %v", err)
	}
}

func TestDelegateWrapperRejectsHardlinkedLockBeforeDispatch(t *testing.T) {
	h := newDelegateHarness(t)
	outside := filepath.Join(h.dir, "outside-lock")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, h.lock); err != nil {
		t.Fatal(err)
	}
	launched := filepath.Join(h.dir, "launched")
	h.stub("gemini", "touch "+launched)
	out, code := h.run("fast", "task")
	if code != 2 || !strings.Contains(out, "delegate lock must have one hardlink") {
		t.Fatalf("hardlinked lock = (exit %d), want explicit refusal:\n%s", code, out)
	}
	if _, err := os.Stat(launched); !os.IsNotExist(err) {
		t.Fatalf("provider dispatched through hardlinked lock: %v", err)
	}
}

func TestDelegateWrapperKernelLockReleasesAfterUncleanGroupExit(t *testing.T) {
	h := newDelegateHarness(t)
	ready := filepath.Join(h.dir, "ready")
	h.stub("gemini", "touch "+ready+"; while :; do sleep 1; done")
	cmd := exec.Command(filepath.Join(h.dir, "coop-delegate"), "fast", "task")
	cmd.Dir, cmd.Env = h.repo, h.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	providerGroup := 0
	wrapperWaited := false
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if providerGroup > 0 {
			_ = syscall.Kill(-providerGroup, syscall.SIGKILL)
		}
		if !wrapperWaited {
			select {
			case <-done:
			case <-time.After(time.Second):
			}
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		matches, err := filepath.Glob(filepath.Join(h.dir, "coop-delegate-FAST-*", "active-pid"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == 1 {
			data, readErr := os.ReadFile(matches[0])
			if readErr == nil {
				providerGroup, readErr = strconv.Atoi(strings.TrimSpace(string(data)))
			}
			if readErr != nil {
				providerGroup = 0
			}
		}
		_, readyErr := os.Stat(ready)
		if readyErr != nil && !os.IsNotExist(readyErr) {
			t.Fatal(readyErr)
		}
		if readyErr == nil && providerGroup > 0 {
			break
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
			t.Fatal("first delegate did not reach the provider")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-done
	wrapperWaited = true

	lock, err := os.OpenFile(h.lock, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		if err == nil {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		}
		t.Fatalf("detached provider did not retain the delegate lock: %v", err)
	}
	if err := syscall.Kill(-providerGroup, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	releaseDeadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			t.Fatal(err)
		}
		if time.Now().After(releaseDeadline) {
			_ = syscall.Kill(-providerGroup, syscall.SIGKILL)
			t.Fatal("delegate lock did not release after its provider group exited")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	h.stub("gemini", "echo recovered")
	if out, code := h.run("fast", "task"); code != 0 || !strings.Contains(out, "recovered") {
		t.Fatalf("next delegate after unclean exit = (exit %d):\n%s", code, out)
	}
}

// fakeDelegate is a minimal future agent for the drift test — enough for the delegate generator.
type fakeDelegate struct{}

func (fakeDelegate) Name() string { return "grokfake" }
func (fakeDelegate) DelegateExec() string {
	return `grokfake --write ${model:+--model "$model"} "$prompt"`
}

// TestDelegateWrapperDispatchesNewAgent: a future agent's bounded write-capable arm is generated from the
// registry with no hand-edit, and the rendered script still shellchecks clean.
func TestDelegateWrapperDispatchesNewAgent(t *testing.T) {
	w := renderDelegate(append(registeredDelegates(), fakeDelegate{}))
	if !strings.Contains(w, `grokfake) run_delegate grokfake --write`) {
		t.Fatalf("the new agent's delegate arm is missing:\n%s", w)
	}
	sc := shellcheckPath(t)
	f := filepath.Join(t.TempDir(), "coop-delegate")
	if err := os.WriteFile(f, []byte(w), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(sc, f).CombinedOutput(); err != nil {
		t.Errorf("shellcheck flagged the delegate wrapper with a future agent:\n%s", out)
	}
}
