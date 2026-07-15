package procharness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEnvironmentStartsEmptyAndOwnsState(t *testing.T) {
	t.Setenv("COOP_AMBIENT_SENTINEL", "must-not-leak")
	t.Setenv("OPENAI_API_KEY", "must-not-leak")

	layout, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env, err := Environment(layout, map[string]string{
		"PATH":         layout.Bin,
		"COOP_RUNTIME": filepath.Join(layout.Bin, "runtime"),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := envMap(env)
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "TMPDIR", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM"} {
		if got[key] == "" {
			t.Errorf("%s is not test-owned", key)
		}
	}
	for _, key := range []string{"COOP_AMBIENT_SENTINEL", "OPENAI_API_KEY", "SSH_AUTH_SOCK", "HTTPS_PROXY"} {
		if _, ok := got[key]; ok {
			t.Errorf("ambient %s leaked into isolated environment", key)
		}
	}
	if got["HOME"] != layout.Home || got["TMPDIR"] != layout.Tmp {
		t.Fatalf("state roots = HOME %q TMPDIR %q, want %q and %q", got["HOME"], got["TMPDIR"], layout.Home, layout.Tmp)
	}
}

func TestEnvironmentRejectsUnlistedExtensions(t *testing.T) {
	layout, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"OPENAI_API_KEY", "HTTPS_PROXY", "GIT_CONFIG_COUNT", "COOP_UNSCOPED_TEST_KNOB"} {
		if _, err := Environment(layout, map[string]string{key: "must-not-enter"}); err == nil {
			t.Errorf("Environment accepted unlisted key %s", key)
		}
	}
}

func TestCanonicalUnderRootRejectsEscapesAndSymlinks(t *testing.T) {
	layout, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(layout.State, "owned.json")
	if err := os.WriteFile(owned, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := CanonicalUnderRoot(layout.Root, owned); err != nil || got != owned {
		t.Fatalf("owned path = (%q, %v), want (%q, nil)", got, err, owned)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("no\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalUnderRoot(layout.Root, outside); err == nil {
		t.Fatal("outside path was accepted")
	}
	link := filepath.Join(layout.State, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalUnderRoot(layout.Root, link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v, want explicit rejection", err)
	}
}

func TestRunDeadlineReapsTheProcessGroup(t *testing.T) {
	layout, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pids := filepath.Join(layout.State, "pids")
	script := fmt.Sprintf("trap '' TERM; sleep 30 & child=$!; printf '%%s %%s\\n' $$ $child > %s; wait", shellQuote(pids))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	result := Run(ctx, Command{
		Path:      "/bin/sh",
		Args:      []string{"-c", script},
		Dir:       layout.Root,
		Env:       []string{"PATH=/usr/bin:/bin"},
		KillGrace: 50 * time.Millisecond,
		MaxOutput: 1024,
	})
	if result.Err == nil || ctx.Err() == nil {
		t.Fatalf("Run deadline = %v (context %v), want cancellation", result.Err, ctx.Err())
	}
	data, err := os.ReadFile(pids)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatal(err)
		}
		awaitGone(t, pid)
	}
}

func TestStartSignalGroupAndWait(t *testing.T) {
	layout, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(layout.State, "ready")
	script := fmt.Sprintf("trap 'exit 130' INT; : > %s; while :; do sleep 1; done", shellQuote(ready))
	process, err := Start(Command{
		Path: "/bin/sh", Args: []string{"-c", script}, Dir: layout.Root,
		Env: []string{"PATH=/usr/bin:/bin"}, KillGrace: 50 * time.Millisecond, MaxOutput: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer process.Cleanup()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("managed process did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := process.SignalGroup(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := process.Wait(ctx)
	if result.Err != nil || result.ExitCode != 130 {
		t.Fatalf("signaled managed process = exit %d, err %v", result.ExitCode, result.Err)
	}
	if again := process.Cleanup(); again.ExitCode != result.ExitCode || again.Err != result.Err {
		t.Fatalf("idempotent cleanup = %#v, want %#v", again, result)
	}
	if ProcessAlive(process.PID()) {
		t.Fatalf("managed process %d survived Wait", process.PID())
	}
}

func TestRunDeadlineKillsDescendantAfterLeaderExits(t *testing.T) {
	layout, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pids := filepath.Join(layout.State, "leader-first-pids")
	childScript := "trap '' TERM; while :; do sleep 1; done"
	script := fmt.Sprintf("trap 'exit 0' TERM; /bin/sh -c %s & child=$!; printf '%%s %%s\\n' $$ $child > %s; while :; do sleep 1; done", shellQuote(childScript), shellQuote(pids))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	result := Run(ctx, Command{
		Path: "/bin/sh", Args: []string{"-c", script}, Dir: layout.Root,
		Env: []string{"PATH=/usr/bin:/bin"}, KillGrace: 50 * time.Millisecond, MaxOutput: 1024,
	})
	if result.Err == nil || ctx.Err() == nil {
		t.Fatalf("Run deadline = %v (context %v), want cancellation", result.Err, ctx.Err())
	}
	data, err := os.ReadFile(pids)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatal(err)
		}
		awaitGone(t, pid)
	}
}

func TestRunWithEmptyEnvironmentDoesNotInherit(t *testing.T) {
	t.Setenv("COOP_AMBIENT_SENTINEL", "leaked")
	layout, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result := Run(context.Background(), Command{
		Path: "/bin/sh", Args: []string{"-c", `printf '%s' "${COOP_AMBIENT_SENTINEL-unset}"`},
		Dir: layout.Root, Env: nil, MaxOutput: 1024,
	})
	if result.Err != nil || result.ExitCode != 0 || result.Stdout != "unset" {
		t.Fatalf("empty environment run = exit %d err %v stdout %q", result.ExitCode, result.Err, result.Stdout)
	}
}

func TestRunRejectsSuccessfulLeaderThatLeavesAChild(t *testing.T) {
	layout, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(layout.State, "orphan-pid")
	childScript := fmt.Sprintf("trap '' HUP TERM; echo $$ > %s; while :; do sleep 1; done", shellQuote(pidFile))
	parentScript := fmt.Sprintf("/bin/sh -c %s & while [ ! -s %s ]; do sleep 0.01; done", shellQuote(childScript), shellQuote(pidFile))
	result := Run(context.Background(), Command{
		Path: "/bin/sh", Args: []string{"-c", parentScript}, Dir: layout.Root,
		Env: []string{"PATH=/usr/bin:/bin"}, MaxOutput: 1024,
	})
	if result.Err == nil || !strings.Contains(result.Err.Error(), "survived leader exit") {
		t.Fatalf("Run orphan result = exit %d, err %v", result.ExitCode, result.Err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	awaitGone(t, pid)
}

func TestOpenRegularFileRejectsSymlinksAndHardlinks(t *testing.T) {
	layout, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(layout.State, "owned")
	if err := os.WriteFile(owned, []byte("owned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenRegularFile(layout.Root, owned, os.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	link := filepath.Join(layout.State, "symlink")
	if err := os.Symlink(owned, link); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenRegularFile(layout.Root, link, os.O_RDONLY); err == nil {
		f.Close()
		t.Fatal("symlinked control file was accepted")
	}
	hardlink := filepath.Join(layout.State, "hardlink")
	if err := os.Link(owned, hardlink); err != nil {
		t.Skipf("hardlink unavailable: %v", err)
	}
	if f, err := OpenRegularFile(layout.Root, hardlink, os.O_RDONLY); err == nil {
		f.Close()
		t.Fatal("multiply-linked control file was accepted")
	}
}

func TestRunBoundsCapturedOutput(t *testing.T) {
	layout, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result := Run(context.Background(), Command{
		Path: "/bin/sh", Args: []string{"-c", "printf 1234567890; printf abcdefghij >&2"},
		Dir: layout.Root, Env: []string{"PATH=/usr/bin:/bin"}, MaxOutput: 8,
	})
	if result.Err != nil || result.ExitCode != 0 {
		t.Fatalf("Run = exit %d, err %v", result.ExitCode, result.Err)
	}
	if len(result.Stdout) != 8 || len(result.Stderr) != 8 || !result.StdoutTruncated || !result.StderrTruncated {
		t.Fatalf("bounded output = stdout %q/%v stderr %q/%v", result.Stdout, result.StdoutTruncated, result.Stderr, result.StderrTruncated)
	}
}

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func awaitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for ProcessAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ProcessAlive(pid) {
		t.Errorf("process %d survived group cleanup", pid)
	}
}
