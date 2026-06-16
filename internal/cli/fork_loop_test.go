package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestAgentLoopCmd(t *testing.T) {
	a := &app{cfg: &config.Config{
		ClaudeCmd: []string{"claude", "--dangerously-skip-permissions"},
		CodexCmd:  []string{"codex", "--dangerously-bypass-approvals-and-sandbox"},
		GeminiCmd: []string{"gemini", "--yolo"},
	}}
	tests := []struct {
		agent string
		want  []string
	}{
		{"claude", []string{"claude", "--dangerously-skip-permissions", "-p", "DO"}},
		{"gemini", []string{"gemini", "--yolo", "-p", "DO"}},
		{"codex", []string{"codex", "exec", "--dangerously-bypass-approvals-and-sandbox", "DO"}},
	}
	for _, tc := range tests {
		if got := a.agentLoopCmd(tc.agent, "DO"); !slices.Equal(got, tc.want) {
			t.Errorf("agentLoopCmd(%q) = %v, want %v", tc.agent, got, tc.want)
		}
	}
}

func TestParseForkCreateLoopFlags(t *testing.T) {
	tests := []struct {
		args                 []string
		loop, detach, worker bool
		agent                string
	}{
		{[]string{"perf", "codex", "--loop"}, true, false, false, "codex"},
		{[]string{"perf", "-d"}, true, true, false, "claude"},
		{[]string{"perf", "gemini", "--loop", "-d"}, true, true, false, "gemini"},
		{[]string{"perf", "--_detached"}, true, false, true, "claude"},
	}
	for _, tc := range tests {
		fa, err := parseForkCreate(tc.args)
		if err != nil {
			t.Errorf("parseForkCreate(%v) err = %v", tc.args, err)
			continue
		}
		if fa.loop != tc.loop || fa.detach != tc.detach || fa.worker != tc.worker || fa.agent != tc.agent {
			t.Errorf("parseForkCreate(%v) = {loop:%v detach:%v worker:%v agent:%q}, want {loop:%v detach:%v worker:%v agent:%q}",
				tc.args, fa.loop, fa.detach, fa.worker, fa.agent, tc.loop, tc.detach, tc.worker, tc.agent)
		}
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
