package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// agentLoopCmd builds the headless, autonomous command for one loop iteration of
// the given agent, carrying prompt. Each CLI has its own non-interactive form:
//
//	claude --dangerously-skip-permissions -p <prompt>
//	gemini --yolo -p <prompt>
//	codex  exec --dangerously-bypass-approvals-and-sandbox <prompt>
func (a *app) agentLoopCmd(agent, prompt string) []string {
	switch agent {
	case "codex":
		// codex runs headless via an `exec` subcommand; the prompt is positional.
		base := a.cfg.CodexCmd
		if len(base) == 0 {
			base = []string{"codex"}
		}
		out := append([]string{base[0], "exec"}, base[1:]...)
		return append(out, prompt)
	case "gemini":
		return append(append([]string{}, a.cfg.GeminiCmd...), "-p", prompt)
	default: // claude
		return append(append([]string{}, a.cfg.ClaudeCmd...), "-p", prompt)
	}
}

// forkResume returns the command to run agent in a fork being re-entered: if a prior
// session for THIS fork exists, the agent's cwd-scoped resume command; otherwise the
// normal command. The bool reports whether it resumed (for the status line). The
// fork's cwd is stable and ~/.<agent> persists, so the session is right there — each
// CLI just resumes its own way, and we scope to this fork so we never pick up an
// unrelated session.
func (a *app) forkResume(ws, agent string) ([]string, bool) {
	switch agent {
	case "codex":
		// `codex resume --last` is GLOBAL, so find this fork's most recent session by
		// the cwd recorded in its session files and resume that one by id.
		if id := latestCodexSession(a.cfg.AgentDir("codex"), ws); id != "" {
			base := a.cfg.CodexCmd
			if len(base) == 0 {
				base = []string{"codex"}
			}
			return append([]string{base[0], "resume", id}, base[1:]...), true
		}
	case "gemini":
		// gemini keys sessions by project basename under ~/.gemini/tmp/<base>/chats.
		if hasEntries(filepath.Join(a.cfg.AgentDir("gemini"), "tmp", filepath.Base(ws), "chats")) {
			return append(append([]string{}, a.cfg.GeminiCmd...), "--resume", "latest"), true
		}
	default: // claude
		if hasEntries(filepath.Join(a.cfg.AgentDir("claude"), "projects", claudeProjectKey(ws))) {
			return append(append([]string{}, a.cfg.ClaudeCmd...), "--continue"), true
		}
	}
	return a.defaultCmd(agent), false
}

// claudeProjectKey is how Claude Code names a project's session dir: the absolute cwd
// with path separators turned into dashes.
func claudeProjectKey(ws string) string { return strings.ReplaceAll(ws, "/", "-") }

// hasEntries reports whether dir exists and holds at least one entry.
func hasEntries(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// latestCodexSession returns the id of the most recent codex session recorded for
// cwd, or "" if none. Codex stores sessions flat by date as JSONL whose first line is
// a session_meta carrying {id, cwd}.
func latestCodexSession(codexDir, cwd string) string {
	var bestID string
	var bestTime time.Time
	_ = filepath.WalkDir(filepath.Join(codexDir, "sessions"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		f, openErr := os.Open(p)
		if openErr != nil {
			return nil
		}
		defer f.Close()
		line, _ := bufio.NewReader(f).ReadString('\n')
		var m struct {
			Payload struct {
				ID, Cwd string
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &m) != nil || m.Payload.Cwd != cwd || m.Payload.ID == "" {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(bestTime) {
			bestTime, bestID = info.ModTime(), m.Payload.ID
		}
		return nil
	})
	return bestID
}

// Per-fork process state (logs + pidfiles) lives in <repo>-forks/.coop/.
func forkStateDir(repo string) string  { return filepath.Join(forkHome(repo), ".coop") }
func forkLog(repo, name string) string { return filepath.Join(forkStateDir(repo), name+".log") }
func forkPid(repo, name string) string { return filepath.Join(forkStateDir(repo), name+".pid") }

// forkRunningPid returns the live pid of a detached loop for name, or 0. A stale
// pidfile (process gone) is cleaned up.
func forkRunningPid(repo, name string) int {
	data, err := os.ReadFile(forkPid(repo, name))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	if syscall.Kill(pid, 0) != nil {
		_ = os.Remove(forkPid(repo, name))
		return 0
	}
	return pid
}

// runForkLoop seeds the fork's queue from the tasks file given to --tasks (only when
// the fork has none yet, so a resumed loop keeps its own progress), then runs the
// unattended loop with the chosen agent, capturing output to the fork's log.
// detached=true means this process IS the background worker (its stdio is already the
// log, and it owns the pidfile). tasks is an absolute path resolved by the caller.
func (a *app) runForkLoop(repo, ws, name, agent, tasks string, detached bool) (int, error) {
	dst := filepath.Join(ws, ".agent", "TASKS.md")
	if tasks != "" && !fileExists(dst) {
		if err := os.MkdirAll(filepath.Join(ws, ".agent"), 0o755); err != nil {
			return -1, err
		}
		if err := copyFile(tasks, dst); err != nil {
			return -1, err
		}
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	var sink io.Writer
	if detached {
		defer os.Remove(forkPid(repo, name)) // the worker clears its pidfile when done
	} else {
		// Foreground: tee to a log so `coop fork logs` works after the fact too.
		if err := os.MkdirAll(forkStateDir(repo), 0o755); err == nil {
			if f, err := os.Create(forkLog(repo, name)); err == nil {
				defer f.Close()
				sink = f
			}
		}
	}
	code, err := a.loop(ws, img, agent, sink)
	if err == nil && !detached {
		forkNextSteps(name)
	}
	return code, err
}

// detachForkLoop re-execs coop as a session-leader background worker whose stdio is
// the fork's log, records its pid, and returns immediately. tasks is an absolute path
// (resolved by the caller) forwarded so the worker seeds the same queue.
func (a *app) detachForkLoop(repo, name, agent, tasks string) (int, error) {
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		return -1, err
	}
	logf, err := os.Create(forkLog(repo, name))
	if err != nil {
		return -1, err
	}
	defer logf.Close()
	self, err := os.Executable()
	if err != nil {
		return -1, fmt.Errorf("locate coop binary: %w", err)
	}
	cmd := exec.Command(self, "fork", name, agent, "--loop", "--tasks", tasks, "--_detached")
	cmd.Dir = repo // ResolveRepo finds the parent repo, then the worker resumes the fork
	cmd.Stdout, cmd.Stderr, cmd.Stdin = logf, logf, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	_ = os.WriteFile(forkPid(repo, name), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	ui.Info("started fork %s (%s) in the background", name, agent)
	ui.Info("  coop fork logs %s -f   ·   coop fork stop %s", name, name)
	return 0, nil
}

func (a *app) forkLogs(args []string) (int, error) {
	name, follow := "", false
	for _, x := range args {
		switch x {
		case "-f", "--follow":
			follow = true
		default:
			if strings.HasPrefix(x, "-") {
				return 2, fmt.Errorf("coop fork logs: unknown flag %q", x)
			}
			name = x
		}
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	var mu sync.Mutex
	if name != "" {
		return 0, streamLog(forkLog(repo, name), "", follow, os.Stdout, &mu)
	}
	names := forkNames(repo)
	if len(names) == 0 {
		ui.Info("no forks yet")
		return 0, nil
	}
	if !follow {
		for _, n := range names {
			_ = streamLog(forkLog(repo, n), n, false, os.Stdout, &mu)
		}
		return 0, nil
	}
	// Follow every fork at once, prefixed (compose-style). Followers never return,
	// so this blocks until Ctrl-C.
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			_ = streamLog(forkLog(repo, name), name, true, os.Stdout, &mu)
		}(n)
	}
	wg.Wait()
	return 0, nil
}

// streamLog prints a log file (optionally prefixed and followed) to w under mu.
func streamLog(path, prefix string, follow bool, w io.Writer, mu *sync.Mutex) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no output yet
		}
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			mu.Lock()
			if prefix != "" {
				fmt.Fprintf(w, "%s | %s", prefix, line)
			} else {
				fmt.Fprint(w, line)
			}
			mu.Unlock()
		}
		if err == io.EOF {
			if !follow {
				return nil
			}
			time.Sleep(300 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
	}
}

func (a *app) forkStop(args []string) (int, error) {
	if len(args) == 0 || args[0] == "" {
		return 2, errors.New("usage: coop fork stop <name>")
	}
	name := args[0]
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	pid := forkRunningPid(repo, name)
	if pid == 0 {
		return 1, fmt.Errorf("fork %s is not running", name)
	}
	// The worker is a session leader (Setsid); signal its whole group, falling back
	// to the single pid.
	if syscall.Kill(-pid, syscall.SIGTERM) != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	_ = os.Remove(forkPid(repo, name))
	ui.Info("stopped fork %s", name)
	return 0, nil
}
