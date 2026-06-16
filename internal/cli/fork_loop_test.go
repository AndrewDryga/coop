package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
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

func TestClaudeProjectKey(t *testing.T) {
	if got := claudeProjectKey("/Users/me/proj-forks/demo"); got != "-Users-me-proj-forks-demo" {
		t.Errorf("claudeProjectKey = %q, want -Users-me-proj-forks-demo", got)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestForkResume(t *testing.T) {
	cfgDir := t.TempDir()
	ws := "/work/myrepo-forks/demo"
	a := &app{cfg: &config.Config{
		ConfigDir: cfgDir,
		ClaudeCmd: []string{"claude", "--dangerously-skip-permissions"},
		CodexCmd:  []string{"codex", "--dangerously-bypass-approvals-and-sandbox"},
		GeminiCmd: []string{"gemini", "--yolo"},
	}}

	// No session for this fork yet → fresh command, resumed=false.
	for _, agent := range []string{"claude", "codex", "gemini"} {
		if cmd, resumed := a.forkResume(ws, agent); resumed {
			t.Errorf("forkResume(%s) resumed with no session: %v", agent, cmd)
		}
	}

	// claude resumes via --continue once a session dir exists under projects/<dash-cwd>.
	mustWrite(t, filepath.Join(cfgDir, "claude", "projects", claudeProjectKey(ws), "s.jsonl"), "{}")
	if cmd, ok := a.forkResume(ws, "claude"); !ok ||
		!slices.Equal(cmd, []string{"claude", "--dangerously-skip-permissions", "--continue"}) {
		t.Errorf("forkResume(claude) = (%v, %v)", cmd, ok)
	}

	// gemini resumes --resume latest once a chats session exists under tmp/<basename>.
	mustWrite(t, filepath.Join(cfgDir, "gemini", "tmp", "demo", "chats", "session.jsonl"), "{}")
	if cmd, ok := a.forkResume(ws, "gemini"); !ok ||
		!slices.Equal(cmd, []string{"gemini", "--yolo", "--resume", "latest"}) {
		t.Errorf("forkResume(gemini) = (%v, %v)", cmd, ok)
	}

	// codex resumes the session whose recorded cwd is THIS fork, by its id.
	mustWrite(t, filepath.Join(cfgDir, "codex", "sessions", "2026", "06", "16", "rollout-x.jsonl"),
		`{"type":"session_meta","payload":{"id":"abc-123","cwd":"`+ws+`"}}`+"\n")
	if cmd, ok := a.forkResume(ws, "codex"); !ok ||
		!slices.Equal(cmd, []string{"codex", "resume", "abc-123", "--dangerously-bypass-approvals-and-sandbox"}) {
		t.Errorf("forkResume(codex) = (%v, %v)", cmd, ok)
	}
	// …but a session recorded for a DIFFERENT cwd must not match.
	if cmd, ok := a.forkResume("/work/myrepo-forks/other", "codex"); ok {
		t.Errorf("forkResume(codex, other fork) wrongly matched: %v", cmd)
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
