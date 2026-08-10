package forkctl

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
)

// Test-only shorthands for the task-tree constants these tests seed fixtures with; internal/tasks
// keeps the canonical exported ones (internal/cli's own tests alias the same way).
const (
	tasksRoot = tasks.TasksRoot
	stateTodo = tasks.StateTodo
	stateDone = tasks.StateDone
)

var (
	latestTaskLog = tasks.LatestTaskLog
	isTaskDir     = tasks.IsTaskDir
	taskStates    = tasks.TaskStates
)

// lastLines returns the last n lines of s (trailing blank lines trimmed first).
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// git runs a git command in dir with a fixed committer identity, failing the test on a non-zero
// exit. It takes an explicit dir (unlike gitrepo.New's bound runner) because these tests drive the
// parent repo AND its forks' worktrees.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initRepo is a parent repo on `main` with one commit — the thing a fork is cut from. It lives in a
// NAMED subdirectory of the temp dir because forkspace puts a fork's workspace in the sibling
// <repo>-forks/, which must not collide with the temp root itself.
func initRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "myrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	git(t, repo, "checkout", "-q", "-b", "main")
	git(t, repo, "config", "user.email", "t@t") // so merge-commits work without ambient identity
	git(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "init")
	return repo
}

// writeTaskFile creates path (making its parent dirs) with content, to seed a task folder as a
// fixture without going through the real `coop tasks` dispatcher.
func writeTaskFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// captureStdout returns whatever fn writes to os.Stdout (the ls/dossier tables print there; ui.Info
// goes to stderr — see captureStderr). Colors are off under `go test` (no tty), so output is plain.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out)
}

type forkCommandResult struct {
	code int
	err  error
}

// runForkCommandAcrossLockedMutation proves a fork command re-checks lifecycle state under the
// forkspace lock: it starts command while the lock is held (so the command must block), applies
// mutate, then releases and returns what the command decided about the changed world.
func runForkCommandAcrossLockedMutation(t *testing.T, repo, name string, command func() (int, error), mutate func()) forkCommandResult {
	t.Helper()
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()

	result := make(chan forkCommandResult, 1)
	go func() {
		code, err := command()
		result <- forkCommandResult{code: code, err: err}
	}()
	select {
	case got := <-result:
		t.Fatalf("fork command bypassed lifecycle lock: (%d, %v)", got.code, got.err)
	case <-time.After(80 * time.Millisecond):
	}
	mutate()
	unlock()
	locked = false
	select {
	case got := <-result:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("fork command remained blocked after lifecycle unlock")
		return forkCommandResult{}
	}
}
