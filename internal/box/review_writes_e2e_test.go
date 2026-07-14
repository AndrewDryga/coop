//go:build reviewwritee2e

// Runtime coverage for the task-only review boundary. It deliberately runs only through
// make review-writes-e2e: unit tests remain runtime-free, while this test proves Docker applies
// the nested read-only/read-write mounts as intended.
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
		taskOnly bool
	}{
		{name: "task-only default", taskOnly: true},
		{name: "full repository opt-in", taskOnly: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testReviewWritesDockerRuntime(t, tc.taskOnly)
		})
	}
}

func testReviewWritesDockerRuntime(t *testing.T, taskOnly bool) {
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

	queue := filepath.Join(repo, ".agent", "tasks")
	spec := RunSpec{
		Image: reviewWritesTestImage, Repo: repo, Workdir: "/workspace",
		Cmd:          []string{"sh", "-ec", reviewWritesRuntimeScript(taskOnly)},
		RepoReadOnly: taskOnly, Batch: true, Quiet: true,
		Stdout: io.Discard, Stderr: io.Discard,
	}
	if taskOnly {
		spec.RepoWritablePaths = []string{queue}
	}
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}
	if code, err := Run(cfg, runtime.Runtime{Name: "docker"}, spec); err != nil || code != 0 {
		t.Fatalf("Run = %d, %v; want 0, nil", code, err)
	}

	protected := map[string]string{
		"source.txt":       "source before\n",
		".agent/loop.yaml": "config before\n",
		"Makefile":         "gate before\n",
	}
	for rel, want := range protected {
		if !taskOnly {
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
	log, err := os.ReadFile(filepath.Join(repo, ".agent", "tasks", "99_done", "task", "log.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "runtime log") {
		t.Errorf("task log = %q, want runtime write", log)
	}
	state, err := os.ReadFile(filepath.Join(repo, ".agent", "tasks", "99_done", "task", "state.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(state) != "runtime state\n" {
		t.Errorf("task state = %q, want runtime write", state)
	}
	if _, err := os.Stat(filepath.Join(repo, ".agent", "tasks", "10_in_progress", "task")); !os.IsNotExist(err) {
		t.Fatalf("in-progress task still exists after lifecycle move: %v", err)
	}
}

func reviewWritesRuntimeScript(taskOnly bool) string {
	const protected = "/workspace/source.txt /workspace/.agent/loop.yaml /workspace/Makefile"
	if taskOnly {
		return `for path in ` + protected + `; do
  if (printf 'denied\n' > "$path") 2>/dev/null; then
    echo "unexpected writable path: $path"
    exit 1
  fi
done
printf 'runtime log\n' >> /workspace/.agent/tasks/10_in_progress/task/log.md
printf 'runtime state\n' > /workspace/.agent/tasks/10_in_progress/task/state.md
mv /workspace/.agent/tasks/10_in_progress/task /workspace/.agent/tasks/99_done/task`
	}
	return `for path in ` + protected + `; do
  printf 'allowed\n' > "$path"
done
printf 'runtime log\n' >> /workspace/.agent/tasks/10_in_progress/task/log.md
printf 'runtime state\n' > /workspace/.agent/tasks/10_in_progress/task/state.md
mv /workspace/.agent/tasks/10_in_progress/task /workspace/.agent/tasks/99_done/task`
}
