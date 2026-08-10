package tasks

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// defaultSessionStateRootForTest mirrors internal/cli/session_cmd.go's unexported
// defaultSessionStateRoot formula — the two can't share code across the package boundary (an
// internal *_test.go file may not import back into cli; see internal/importdag_test.go invariant
// 1), so this asserts the two independently-computed paths still agree, the same "local-redeclare
// a trivial, stateless leaf" shape gitOut uses (see git.go), sized for a test rather than a shared
// package.
func defaultSessionStateRootForTest() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "coop", "sessions"), nil
}

// git runs a plain (unhardened — this is test setup, not code under test) git command against dir
// with a fixed author/committer identity, failing the test on a non-zero exit. internal/cli's own
// fork_test.go keeps an identical copy for the same reason gitOut does; see git.go.
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

// gitRepo returns a fresh hermetic git repository — internal/cli/sign_test.go keeps its own
// identical one-line wrapper (the actual implementation is internal/testutil/gitrepo, shared by
// both packages) rather than exporting a test helper across the package boundary.
func gitRepo(t *testing.T) (string, func(...string)) { return gitrepo.New(t) }

// initRepo returns a fresh repo with one trailerless commit (a README) — internal/cli/fork_test.go
// keeps its own equivalent (an explicit "main" checkout plus the same one commit) for the same
// reason gitOut does; see git.go. Neither test suite depends on the branch's name, only that HEAD
// resolves to one commit with no Coop-Task trailer.
func initRepo(t *testing.T) string {
	t.Helper()
	repo, run := gitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "init")
	return repo
}

// captureStderr returns whatever fn writes to os.Stderr (ui.Warn/ui.Note/ui.Error go there).
// internal/cli/status_test.go keeps its own identical copy for the same reason gitOut does; see
// git.go.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	return string(out)
}

// captureStdout returns whatever fn writes to os.Stdout (list/decisions print there;
// ui.Info goes to stderr). Colors are off under `go test` (no tty), so output is plain.
// internal/cli/integration_test.go keeps its own copy of this same helper (used well beyond the
// task views) for the same reason gitOut does; see git.go.
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
