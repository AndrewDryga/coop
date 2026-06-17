package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
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

	// --loop without --tasks is rejected (no implicit name→file mapping), as is --tasks
	// without --loop.
	if _, err := parseForkCreate([]string{"perf", "--loop"}); err == nil {
		t.Error("parseForkCreate(--loop, no --tasks): want error")
	}
	if _, err := parseForkCreate([]string{"perf", "-d"}); err == nil {
		t.Error("parseForkCreate(-d, no --tasks): want error")
	}
	if _, err := parseForkCreate([]string{"perf", "--tasks", "q.md"}); err == nil {
		t.Error("parseForkCreate(--tasks without --loop): want error")
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
