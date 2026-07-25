//go:build reviewwritee2e

// Runtime coverage for the review write boundary. It deliberately runs only through
// make review-writes-e2e: unit tests remain runtime-free, while this test proves Docker keeps the
// task queues remain read-only even when full repository source writes are explicit.
package box

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

const reviewWritesTestImage = "alpine:3.21"

func TestReviewWritesDockerRuntime(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH; run make review-writes-e2e on a Docker host")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("Docker daemon is unavailable; run make review-writes-e2e on a Docker host")
	}
	if err := exec.Command("docker", "image", "inspect", reviewWritesTestImage).Run(); err != nil {
		t.Skipf("%s is unavailable; run make review-writes-e2e", reviewWritesTestImage)
	}

	for _, tc := range []struct {
		name     string
		readOnly bool
	}{
		{name: "report-only default", readOnly: true},
		{name: "full repository opt-in", readOnly: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testReviewWritesDockerRuntime(t, tc.readOnly)
		})
	}
}

func testReviewWritesDockerRuntime(t *testing.T, readOnly bool) {
	t.Helper()
	repo := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("source.txt", "source before\n")
	write(".agent/loop.yaml", "config before\n")
	write("Makefile", "gate before\n")
	write(".agent/tasks/10_in_progress/task/log.md", "log before\n")
	write(".agent/tasks/10_in_progress/task/state.md", "state before\n")
	if err := os.MkdirAll(filepath.Join(repo, ".agent", "tasks", "99_done"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The box drops CAP_DAC_OVERRIDE (boxLimits --cap-drop ALL), so its root can only write files
	// it reaches by ordinary permission, not by owning them. A CI runner owns this fixture under a
	// UID the box does not, so make the tree world-writable. The read-only MOUNT still blocks the
	// protected paths — a writable file on a :ro bind cannot be written — which is what this proves.
	if err := filepath.Walk(repo, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o666)
		if info.IsDir() {
			mode = 0o777
		}
		return os.Chmod(p, mode)
	}); err != nil {
		t.Fatal(err)
	}

	var boxErr strings.Builder
	spec := RunSpec{
		Image: reviewWritesTestImage, Repo: repo, Workdir: "/workspace",
		Cmd:               []string{"sh", "-ec", reviewWritesRuntimeScript(readOnly)},
		RepoReadOnly:      readOnly,
		RepoReadOnlyPaths: []string{filepath.Join(repo, ".agent", "tasks")},
		Batch:             true, Quiet: true,
		Stdout: io.Discard, Stderr: &boxErr,
	}
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}
	if code, err := Run(cfg, runtime.Runtime{Name: "docker"}, spec); err != nil || code != 0 {
		t.Fatalf("Run = %d, %v; want 0, nil\nbox stderr:\n%s", code, err, boxErr.String())
	}

	protected := map[string]string{
		"source.txt":       "source before\n",
		".agent/loop.yaml": "config before\n",
		"Makefile":         "gate before\n",
	}
	for rel, want := range protected {
		if !readOnly {
			want = "allowed\n"
		}
		body, err := os.ReadFile(filepath.Join(repo, rel))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != want {
			t.Errorf("%s = %q, want %q", rel, body, want)
		}
	}
	log, err := os.ReadFile(filepath.Join(repo, ".agent", "tasks", "10_in_progress", "task", "log.md"))
	if err != nil || string(log) != "log before\n" {
		t.Errorf("protected task log = %q, %v", log, err)
	}
	state, err := os.ReadFile(filepath.Join(repo, ".agent", "tasks", "10_in_progress", "task", "state.md"))
	if err != nil || string(state) != "state before\n" {
		t.Errorf("protected task state = %q, %v", state, err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".agent", "tasks", "99_done", "task")); !os.IsNotExist(err) {
		t.Fatalf("review moved the protected task: %v", err)
	}
}

func reviewWritesRuntimeScript(readOnly bool) string {
	const tasks = "/workspace/.agent/tasks/10_in_progress/task/log.md /workspace/.agent/tasks/10_in_progress/task/state.md"
	if readOnly {
		return `for path in /workspace/source.txt /workspace/.agent/loop.yaml /workspace/Makefile ` + tasks + `; do
  if (printf 'denied\n' > "$path") 2>/dev/null; then
    echo "unexpected writable path: $path"
    exit 1
  fi
done
if mv /workspace/.agent/tasks/10_in_progress/task /workspace/.agent/tasks/99_done/task 2>/dev/null; then
  echo "unexpected writable task queue"
  exit 1
fi`
	}
	return `for path in /workspace/source.txt /workspace/.agent/loop.yaml /workspace/Makefile; do
  printf 'allowed\n' > "$path"
done
for path in ` + tasks + `; do
  if (printf 'denied\n' > "$path") 2>/dev/null; then
    echo "unexpected writable task path: $path"
    exit 1
  fi
done
if mv /workspace/.agent/tasks/10_in_progress/task /workspace/.agent/tasks/99_done/task 2>/dev/null; then
  echo "unexpected writable task queue"
  exit 1
fi`
}
