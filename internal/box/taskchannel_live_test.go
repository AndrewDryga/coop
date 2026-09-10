//go:build boxruntimee2e

package box_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/taskmcp"
	"github.com/AndrewDryga/coop/internal/tasks"
)

// TestTaskChannelLiveInARealBox is the real proof: a coop-box container reaches its task queue
// through the channel — lists the tools, completes its own task, and is refused on a held one —
// under --network none, with no coop binary and no host socket. It needs a built coop-box image
// and a container runtime, so it rides the boxruntimee2e tag like the other live box tests.
func TestTaskChannelLiveInARealBox(t *testing.T) {
	t.Setenv(tasks.TestLeaseAuthorityRootEnv, t.TempDir())
	rt, err := runtime.Detect(os.Getenv("COOP_RUNTIME"))
	if err != nil {
		t.Skipf("no runtime: %v", err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none", BaseImage: "coop-box"}
	if !box.ImageExists(rt, cfg.BaseImage) {
		t.Skipf("coop-box image %q not built (run: make build && ./coop build)", cfg.BaseImage)
	}
	image := cfg.BaseImage

	repo := t.TempDir()
	queue := filepath.Join(repo, ".agent", "tasks")
	if err := tasks.ScaffoldStateDirs(queue); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"mine", "theirs"} {
		dir := filepath.Join(queue, tasks.StateInProgress, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nid: " + id + "\ntitle: " + id + "\n---\n\n# " + id + "\n\n**Context:** c\n\n**Acceptance criteria:** a\n\n**Approach:** p\n\n## Subtasks\n- [ ] one\n"
		if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	theirs, _, err := tasks.CurrentTask(queue, "theirs")
	if err != nil {
		t.Fatal(err)
	}
	lease, _, err := tasks.TryTaskLease(queue, theirs, tasks.TaskLeaseOwner{RunID: "other", PID: os.Getpid(), Provider: "codex", Target: "codex"})
	if err != nil || lease == nil {
		t.Fatalf("fixture lease: %v", err)
	}
	defer lease.Release()

	server, err := taskmcp.New(taskmcp.Authority{QueueRoots: []string{queue}, Assigned: "mine"})
	if err != nil {
		t.Fatal(err)
	}
	// The box connects with the exact client command the bound MCP snapshot carries, one request
	// per line, and prints the replies. This is what an agent's MCP client does.
	probe := `set -e
[ -S /coop/tasks/mcp.sock ] || { echo "NOSOCKET"; ls -la /coop/tasks; exit 1; }
command -v coop >/dev/null 2>&1 && echo "COOPPRESENT" || echo "NOCOOP"
{
  echo '{"jsonrpc":"2.0","id":"list","method":"tools/list"}'
  echo '{"jsonrpc":"2.0","id":"held","method":"tools/call","params":{"name":"tasks_append_log","arguments":{"id":"theirs","entry":"must not land"}}}'
  echo '{"jsonrpc":"2.0","id":"done","method":"tools/call","params":{"name":"tasks_complete","arguments":{"id":"mine"}}}'
} | socat -t 5 STDIO UNIX-CONNECT:/coop/tasks/mcp.sock`
	var out, errOut strings.Builder
	code, runErr := box.Run(cfg, rt, box.RunSpec{
		Image: image, Repo: repo, Workdir: "/workspace", Cmd: []string{"sh", "-c", probe},
		Batch: true, Quiet: true, Stdout: &out, Stderr: &errOut, TaskTools: server,
	})
	t.Logf("box exit=%d err=%v\nstdout:\n%s\nstderr:\n%s", code, runErr, out.String(), errOut.String())
	got := out.String()
	if strings.Contains(got, "NOSOCKET") || !strings.Contains(got, "NOCOOP") {
		t.Fatalf("box did not see the socket, or coop leaked in:\n%s", got)
	}
	for _, want := range []string{`"tasks_complete"`, "held by another live process", "moved to 99_done/"} {
		if !strings.Contains(got, want) {
			t.Fatalf("channel reply missing %q:\n%s", want, got)
		}
	}
	if _, err := os.Stat(filepath.Join(queue, tasks.StateDone, "mine", "task.md")); err != nil {
		t.Fatalf("the box's task did not complete through the channel: %v", err)
	}
	if log, _ := os.ReadFile(filepath.Join(queue, tasks.StateInProgress, "theirs", "log.md")); strings.Contains(string(log), "must not land") {
		t.Fatal("the refused mutation wrote to the held task")
	}
}
