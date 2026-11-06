package loop

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// One run: the first task's worker says it must wait for the second, the loop works the second, and
// that completion releases the first, which the same run then finishes. Nobody is asked anything.
func TestLoopWorksAWaitingTaskOnceTheTaskItWaitsForIsDone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo, git := gitrepo.New(t)
	writeTaskFile(t, filepath.Join(repo, ".gitignore"), ".agent/\n")
	git("add", ".gitignore")
	git("commit", "-m", "base")
	queue := filepath.Join(repo, tasksRoot)
	// The waiting task sorts first, so only the wait sends the loop to the other one.
	for _, id := range []string{"alpha-docs", "zeta-bridge"} {
		writeTaskFile(t, filepath.Join(queue, stateTodo, id, "task.md"), "# "+id+"\n- [x] required acceptance checks passed\n")
		writeTaskFile(t, filepath.Join(queue, stateTodo, id, "state.md"), "# State — "+id+"\n\n**Status:** in progress\n**Done so far:** verified\n**Next action:** complete\n**Traps:** none\n")
		writeTaskFile(t, filepath.Join(queue, stateTodo, id, "log.md"), "# Log — "+id+"\n\n- Verified the required acceptance checks.\n")
	}
	cfg := openConfig(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true})
	c := New(cfg, runtime.Runtime{Name: "true"}, "test", Host{})
	var worked []string
	c.boxRun = func(s box.RunSpec) (int, error) {
		if len(worked) == 3 || s.TaskTools == nil {
			t.Fatalf("unexpected attempt after %v", worked)
		}
		id := "zeta-bridge"
		if strings.Contains(strings.Join(s.Cmd, "\n"), "Work task alpha-docs") {
			id = "alpha-docs"
		}
		worked = append(worked, id)
		client, server := net.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.TaskTools.Serve(ctx, server) }()
		defer func() {
			cancel()
			client.Close()
			server.Close()
			<-done
		}()
		reader := bufio.NewReader(client)
		call := func(method string, params any) map[string]any {
			t.Helper()
			if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := json.NewEncoder(client).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}); err != nil {
				t.Fatal(err)
			}
			line, err := reader.ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			var reply map[string]any
			if err := json.Unmarshal(line, &reply); err != nil {
				t.Fatal(err)
			}
			result, ok := reply["result"].(map[string]any)
			if !ok || result["isError"] == true {
				t.Fatalf("%s %v failed: %s", method, params, line)
			}
			return result
		}
		call("initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})
		if len(worked) == 1 {
			call("tools/call", map[string]any{"name": "tasks_wait", "arguments": map[string]any{
				"id": id, "on": "zeta-bridge", "reason": "its example must not land before the bridge fix",
			}})
		} else {
			writeTaskFile(t, filepath.Join(repo, id+".txt"), "implemented\n")
			git("add", id+".txt")
			git("commit", "-m", "Implement "+id+"\n\nCoop-Task: "+id)
			call("tools/call", map[string]any{"name": "tasks_complete", "arguments": map[string]any{"id": id, "unrun_checks": []string{}}})
		}
		_, err := io.WriteString(s.Stdout, "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"num_turns\":1,\"session_id\":\"018f6352-6281-7ae1-a1d5-07c3399de43d\",\"result\":\"done\"}\n")
		return 0, err
	}
	var code int
	var err error
	output := captureStderr(t, func() {
		code, err = c.Run(RunSpec{Repo: repo, Image: "fixture", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard, MaxTasks: 2,
			Rotation: ladder.NewRotation([]agents.Target{target("claude", "test")})})
	})
	if code != 0 || err != nil || strings.Join(worked, " ") != "alpha-docs zeta-bridge alpha-docs" {
		t.Fatalf("loop = %d, %v, worked %v\n%s", code, err, worked, output)
	}
	if !strings.Contains(output, "alpha-docs no longer waits: zeta-bridge is done") {
		t.Fatalf("the released wait was not reported:\n%s", output)
	}
	for _, id := range worked[:2] {
		item, ok, err := tasks.CurrentTask(queue, id)
		if err != nil || !ok || item.State != tasks.StateDone || item.WaitsOn != "" {
			t.Fatalf("%s after the run = %+v, %v, %v", id, item, ok, err)
		}
	}
	log, err := os.ReadFile(filepath.Join(queue, tasks.StateDone, "alpha-docs", "log.md"))
	if err != nil || !strings.Contains(string(log), "waits for zeta-bridge before it can go on") || !strings.Contains(string(log), "zeta-bridge is done, so this task no longer waits for it") {
		t.Fatalf("alpha-docs log = %q, %v", log, err)
	}
}
