package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestParseForkCreateLoopFlags(t *testing.T) {
	tests := []struct {
		args                 []string
		loop, detach, worker bool
		agent, tasks         string
	}{
		{[]string{"perf", "codex", "--loop", "--tasks", "q.md"}, true, false, false, "codex", "q.md"},
		{[]string{"perf", "-d", "--tasks", "q.md"}, true, true, false, "claude", "q.md"},
		{[]string{"perf", "--loop", "--detach", "--tasks", "q.md"}, true, true, false, "claude", "q.md"}, // long form of -d
		{[]string{"perf", "gemini", "--loop", "-d", "-t", "q.md"}, true, true, false, "gemini", "q.md"},  // short -t
		{[]string{"perf", "--loop", "--tasks=q.md"}, true, false, false, "claude", "q.md"},               // --tasks=VALUE form
		{[]string{"perf", "--_detached", "--tasks", "q.md"}, true, false, true, "claude", "q.md"},
	}
	for _, tc := range tests {
		fa, err := parseForkCreate(tc.args)
		if err != nil {
			t.Errorf("parseForkCreate(%v) err = %v", tc.args, err)
			continue
		}
		if fa.loop != tc.loop || fa.detach != tc.detach || fa.worker != tc.worker || fa.agent != tc.agent || fa.tasks != tc.tasks {
			t.Errorf("parseForkCreate(%v) = {loop:%v detach:%v worker:%v agent:%q tasks:%q}, want {loop:%v detach:%v worker:%v agent:%q tasks:%q}",
				tc.args, fa.loop, fa.detach, fa.worker, fa.agent, fa.tasks, tc.loop, tc.detach, tc.worker, tc.agent, tc.tasks)
		}
	}

	// --loop / -d without --tasks is allowed now — forkCreate defaults it to the repo's
	// .agent/tasks (it knows the repo). --tasks without --loop is still an error.
	if _, err := parseForkCreate([]string{"perf", "--loop"}); err != nil {
		t.Errorf("parseForkCreate(--loop, no --tasks): want no error (defaults later), got %v", err)
	}
	if _, err := parseForkCreate([]string{"perf", "-d"}); err != nil {
		t.Errorf("parseForkCreate(-d, no --tasks): want no error (defaults later), got %v", err)
	}
	if _, err := parseForkCreate([]string{"perf", "--tasks", "q.md"}); err == nil {
		t.Error("parseForkCreate(--tasks without --loop): want error")
	}
}

func TestParseForkCreateCredential(t *testing.T) {
	// --credential pins a single account (space or = form); --model may carry a model@account.
	fa, err := parseForkCreate([]string{"perf", "claude", "--loop", "--tasks", "q.md", "--credential", "work", "--model", "opus@work"})
	if err != nil {
		t.Fatalf("parseForkCreate err = %v", err)
	}
	if fa.credential != "work" || fa.model != "opus@work" {
		t.Errorf("credential=%q model=%q, want work / opus@work", fa.credential, fa.model)
	}
	if fa2, err := parseForkCreate([]string{"perf", "--credential=work"}); err != nil || fa2.credential != "work" {
		t.Errorf("--credential=work → credential=%q err=%v, want work", fa2.credential, err)
	}
	// The retired --profile spelling fails with the rewrite, both forms.
	for _, args := range [][]string{{"perf", "--profile", "work"}, {"perf", "--profile=work"}} {
		if _, err := parseForkCreate(args); err == nil || !strings.Contains(err.Error(), "--credential") {
			t.Errorf("parseForkCreate(%v): the retired --profile must fail with the rewrite, got %v", args, err)
		}
	}
	if _, err := parseForkCreate([]string{"perf", "--credential"}); err == nil {
		t.Error("--credential with no value: want error")
	}
}

func TestForkStopMessages(t *testing.T) {
	repo := t.TempDir()
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	// A fork that doesn't exist → "no such fork" (matching ls/path/rm), not "not running".
	if code, err := a.forkStop([]string{"ghost"}); code != 1 || err == nil || !strings.Contains(err.Error(), "no such fork") {
		t.Errorf("forkStop(ghost) = (%d, %v), want (1, no such fork)", code, err)
	}
	// A fork that exists but has no running loop → "not running".
	if err := os.MkdirAll(forkWorkspace(repo, "idle"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, err := a.forkStop([]string{"idle"}); code != 1 || err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("forkStop(idle) = (%d, %v), want (1, not running)", code, err)
	}
}

func TestForkStatePaths(t *testing.T) {
	repo := "/home/me/proj"
	if got, want := forkStateDir(repo), "/home/me/proj-forks/.coop"; got != want {
		t.Errorf("forkStateDir = %q, want %q", got, want)
	}
	if got, want := forkLog(repo, "perf"), "/home/me/proj-forks/.coop/perf.log"; got != want {
		t.Errorf("forkLog = %q, want %q", got, want)
	}
	if got, want := forkPid(repo, "perf"), "/home/me/proj-forks/.coop/perf.pid"; got != want {
		t.Errorf("forkPid = %q, want %q", got, want)
	}
}

func TestForkRunningPid(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	// No pidfile → 0.
	if got := forkRunningPid(repo, "perf"); got != 0 {
		t.Errorf("forkRunningPid(no file) = %d, want 0", got)
	}
	// A live pid (our own) → that pid.
	if err := os.WriteFile(forkPid(repo, "perf"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := forkRunningPid(repo, "perf"); got != os.Getpid() {
		t.Errorf("forkRunningPid(live) = %d, want %d", got, os.Getpid())
	}
	// A dead/out-of-range pid → 0, and the stale pidfile is cleaned up.
	if err := os.WriteFile(forkPid(repo, "dead"), []byte("2147483646"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := forkRunningPid(repo, "dead"); got != 0 {
		t.Errorf("forkRunningPid(dead) = %d, want 0", got)
	}
	if pathExists(forkPid(repo, "dead")) {
		t.Error("stale pidfile not removed")
	}
}

func TestParsePidfile(t *testing.T) {
	if pid, tok := parsePidfile("123\n"); pid != 123 || tok != "" { // legacy, pid-only
		t.Errorf("parsePidfile(legacy) = %d,%q want 123,\"\"", pid, tok)
	}
	// the start-time token is a date string and may contain spaces (an lstart value)
	if pid, tok := parsePidfile("456\nWed Jun 18 10:00:00 2026\n"); pid != 456 || tok != "Wed Jun 18 10:00:00 2026" {
		t.Errorf("parsePidfile(with token) = %d,%q", pid, tok)
	}
	if pid, _ := parsePidfile("nonsense"); pid != 0 {
		t.Errorf("parsePidfile(junk) pid = %d, want 0", pid)
	}
}

// A pidfile whose start-time token no longer matches the pid's process means the pid was reused by
// an unrelated process after the worker crashed → not running (and cleaned up), not the old false
// "still running". A matching token (a genuinely live worker) is still reported running.
func TestForkRunningPidReusedPid(t *testing.T) {
	if procStartToken(os.Getpid()) == "" {
		t.Skip("ps -o lstart unavailable — can't test start-time corroboration")
	}
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	// Our own (live) pid, but recorded with a start time from a different process → reused → 0.
	if err := os.WriteFile(forkPid(repo, "reused"), []byte(fmt.Sprintf("%d\nNOT THE REAL START\n", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := forkRunningPid(repo, "reused"); got != 0 {
		t.Errorf("forkRunningPid(reused pid) = %d, want 0", got)
	}
	if pathExists(forkPid(repo, "reused")) {
		t.Error("reused-pid pidfile not cleaned up")
	}
	// The same pid recorded with its real start time (writeForkPid round-trip) → genuinely running.
	if err := writeForkPid(repo, "live", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if got := forkRunningPid(repo, "live"); got != os.Getpid() {
		t.Errorf("forkRunningPid(live, matching token) = %d, want %d", got, os.Getpid())
	}
}

// runningForkNames is the guard merge uses to never rebase/delete a fork out from under a live
// loop: only forks with a live pidfile count; a dead pidfile and an absent one don't.
func TestRunningForkNames(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forkPid(repo, "live"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forkPid(repo, "dead"), []byte("2147483646"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runningForkNames(repo, []string{"live", "dead", "absent"})
	if len(got) != 1 || got[0] != "live" {
		t.Errorf("runningForkNames = %v, want [live] (a dead pidfile and an absent one aren't running)", got)
	}
}

func TestDetachForkLoopRefusesDoubleStart(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	// A live pidfile (our own pid) stands in for a worker already looping this fork.
	if err := os.WriteFile(forkPid(repo, "perf"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{}}
	code, err := a.detachForkLoop(repo, "perf", "claude", "", "", "", "", false)
	if err == nil {
		t.Fatal("detachForkLoop started a second worker for an already-running fork")
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	// The original pid must survive — overwriting it is exactly the orphan bug.
	if got := forkRunningPid(repo, "perf"); got != os.Getpid() {
		t.Errorf("pidfile clobbered: forkRunningPid = %d, want %d", got, os.Getpid())
	}
}

// claimForkPid is the atomic reservation that closes the double-start race: a first claim wins, a
// second claim of a LIVE fork is refused, and a STALE pidfile (dead pid) is reclaimed.
func TestClaimForkPid(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := claimForkPid(repo, "x"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !pathExists(forkPid(repo, "x")) {
		t.Fatal("claim should create the pidfile")
	}
	// The placeholder is this (live) process's pid, so a second claim is refused — no double-start.
	if err := claimForkPid(repo, "x"); err == nil {
		t.Error("a second claim of a live fork must be refused")
	}
	// A stale pidfile (a pid that isn't running) is reclaimed.
	if err := os.WriteFile(forkPid(repo, "stale"), []byte("2147483646\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := claimForkPid(repo, "stale"); err != nil {
		t.Errorf("claim should reclaim a stale pidfile, got %v", err)
	}
}

// waitForExit returns immediately-true for a dead pid and false (after the timeout) for a live one
// — forkStop uses it to confirm death before clearing the pidfile.
func TestWaitForExit(t *testing.T) {
	if !waitForExit(2147483646, 2*time.Second) { // a pid that isn't running
		t.Error("waitForExit should report a non-running pid as exited (fast)")
	}
	if waitForExit(os.Getpid(), 80*time.Millisecond) { // we're alive
		t.Error("waitForExit should report a live pid as not-exited after the timeout")
	}
}

// clearForkPidIfMine removes the pidfile only when it names THIS process — never one a different
// live worker owns (the orphan-cascade guard).
func TestClearForkPidIfMine(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeForkPid(repo, "mine", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	clearForkPidIfMine(repo, "mine")
	if pathExists(forkPid(repo, "mine")) {
		t.Error("clearForkPidIfMine should remove a pidfile that names us")
	}
	// A pidfile owned by another pid must be left alone.
	if err := os.WriteFile(forkPid(repo, "other"), []byte("424242\ntoken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clearForkPidIfMine(repo, "other")
	if !pathExists(forkPid(repo, "other")) {
		t.Error("clearForkPidIfMine must NOT remove a pidfile owned by another process")
	}
}

func TestStreamLog(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.log")
	if err := os.WriteFile(p, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var buf bytes.Buffer
	if err := streamLog(p, "", false, &buf, &mu); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "line1\nline2\n" {
		t.Errorf("streamLog = %q, want unprefixed lines", buf.String())
	}
	buf.Reset()
	_ = streamLog(p, "perf", false, &buf, &mu)
	if buf.String() != "perf | line1\nperf | line2\n" {
		t.Errorf("streamLog prefixed = %q", buf.String())
	}
	// A missing log is not an error and produces nothing.
	buf.Reset()
	if err := streamLog(filepath.Join(dir, "missing.log"), "", false, &buf, &mu); err != nil {
		t.Fatalf("missing log should not error: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("missing log produced %q", buf.String())
	}
}
