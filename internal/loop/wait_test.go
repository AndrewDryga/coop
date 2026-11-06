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
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func callWaitTestTaskTool(t *testing.T, s box.RunSpec, name string, arguments any) {
	t.Helper()
	if s.TaskTools == nil {
		t.Fatal("task tools are unavailable")
	}
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
	call := func(method string, params any) {
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
	}
	call("initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})
	call("tools/call", map[string]any{"name": name, "arguments": arguments})
}

// waitTestResume is the resume step the fixture tasks' workers left in state.md.
const waitTestResume = "apply tmp/resume.patch, then complete"

// waitTestResult is the provider stream line of a worker that finished its turn.
const waitTestResult = "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"num_turns\":1,\"session_id\":\"018f6352-6281-7ae1-a1d5-07c3399de43d\",\"result\":\"done\"}\n"

// waitLoopFixture is a repository with two todo tasks and a loop for it whose box the test plays. The
// task that waits, alpha-docs, sorts first, so only its wait sends the loop to zeta-bridge.
func waitLoopFixture(t *testing.T) (repo string, git func(...string), queue string, c *Control) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo, git = gitrepo.New(t)
	writeTaskFile(t, filepath.Join(repo, ".gitignore"), ".agent/\n")
	git("add", ".gitignore")
	git("commit", "-m", "base")
	queue = filepath.Join(repo, tasksRoot)
	for _, id := range []string{"alpha-docs", "zeta-bridge"} {
		writeTaskFile(t, filepath.Join(queue, stateTodo, id, "task.md"), "# "+id+"\n- [x] required acceptance checks passed\n")
		writeTaskFile(t, filepath.Join(queue, stateTodo, id, "state.md"), "# State — "+id+"\n\n**Status:** in progress\n**Done so far:** verified\n**Next action:** "+waitTestResume+"\n**Traps:** none\n")
		writeTaskFile(t, filepath.Join(queue, stateTodo, id, "log.md"), "# Log — "+id+"\n\n- Verified the required acceptance checks.\n")
	}
	cfg := openConfig(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true})
	return repo, git, queue, New(cfg, runtime.Runtime{Name: "true"}, "test", Host{})
}

// One run: the first task's worker says it must wait for the second, the loop works the second, and
// that completion releases the first, which the same run then finishes. Nobody is asked anything.
func TestLoopWorksAWaitingTaskOnceTheTaskItWaitsForIsDone(t *testing.T) {
	repo, git, queue, c := waitLoopFixture(t)
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
		if len(worked) == 1 {
			callWaitTestTaskTool(t, s, "tasks_wait", map[string]any{
				"id": id, "on": "zeta-bridge", "reason": "its example must not land before the bridge fix",
			})
		} else {
			writeTaskFile(t, filepath.Join(repo, id+".txt"), "implemented\n")
			git("add", id+".txt")
			git("commit", "-m", "Implement "+id+"\n\nCoop-Task: "+id)
			callWaitTestTaskTool(t, s, "tasks_complete", map[string]any{"id": id, "unrun_checks": []string{}})
		}
		_, err := io.WriteString(s.Stdout, waitTestResult)
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

// tasks_wait checks history only when it is called. A commit the worker makes afterwards, bound or
// not, must not ride into 00_todo/ behind the wait, however the worker exits, and one that forges a
// binding for an archived task is named as the binding check names it.
func TestLoopRejectsCommitsAfterTaskWait(t *testing.T) {
	for _, tc := range []struct {
		name, trailer, want string
		exit                int
	}{
		{name: "bound commit after success", trailer: "alpha-docs"},
		{name: "unbound commit after success"},
		{name: "bound commit after worker failure", trailer: "alpha-docs", exit: 23},
		{name: "unbound commit after worker failure", exit: 23},
		{name: "bound commit before background handoff", trailer: "alpha-docs", exit: box.DescendantsDrainedExit},
		{name: "unbound commit before background handoff", exit: box.DescendantsDrainedExit},
		{name: "bound commit before background timeout", trailer: "alpha-docs", exit: box.DescendantsTimedOutExit},
		{name: "unbound commit before background timeout", exit: box.DescendantsTimedOutExit},
		{name: "forged binding for an archived task", trailer: "omega-archived", want: "binds task omega-archived, which was already done before this attempt started"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, git, queue, c := waitLoopFixture(t)
			if tc.trailer == "omega-archived" {
				writeTaskFile(t, filepath.Join(queue, stateDone, "omega-archived", "task.md"), "# omega-archived\n- [x] shipped\n")
			}
			var worked []string
			var lateCommit string
			c.boxRun = func(s box.RunSpec) (int, error) {
				id := "zeta-bridge"
				if strings.Contains(strings.Join(s.Cmd, "\n"), "Work task alpha-docs") {
					id = "alpha-docs"
				}
				worked = append(worked, id)
				if len(worked) > 1 {
					return box.DescendantsDrainedExit, nil
				}
				callWaitTestTaskTool(t, s, "tasks_wait", map[string]any{
					"id": id, "on": "zeta-bridge", "reason": "its example must not land before the bridge fix",
				})
				writeTaskFile(t, filepath.Join(repo, "late-work.txt"), "committed after waiting\n")
				git("add", "late-work.txt")
				message := "Commit after tasks_wait"
				if tc.trailer != "" {
					message += "\n\nCoop-Task: " + tc.trailer
				}
				git("commit", "-m", message)
				lateCommit = forkspace.GitOut(repo, "rev-parse", "HEAD")
				_, err := io.WriteString(s.Stdout, waitTestResult)
				return tc.exit, err
			}
			var code int
			var err error
			output := captureStderr(t, func() {
				code, err = c.Run(RunSpec{Repo: repo, Image: "fixture", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard, MaxTasks: 2,
					Rotation: ladder.NewRotation([]agents.Target{target("claude", "test")})})
			})
			if code != 1 || err == nil {
				t.Errorf("loop = %d, %v, want a nonzero stop after rejecting the wait\n%s", code, err, output)
			}
			if strings.Join(worked, " ") != "alpha-docs" {
				t.Errorf("worked %v, want only alpha-docs before stopping\n%s", worked, output)
			}
			alpha, ok, taskErr := tasks.CurrentTask(queue, "alpha-docs")
			if taskErr != nil || !ok || alpha.State != tasks.StateInProgress || alpha.WaitsOn != "" {
				t.Errorf("alpha-docs after rejection = %+v, %v, %v; want in progress with no wait", alpha, ok, taskErr)
			}
			state, stateErr := tasks.ReadTaskState(alpha.Dir)
			if stateErr != nil || !strings.Contains(state.Status, "wait rejected") || state.NextAction != waitTestResume {
				t.Errorf("rejected wait resume state = %+v, %v; want the worker's own next action kept", state, stateErr)
			}
			zeta, ok, taskErr := tasks.CurrentTask(queue, "zeta-bridge")
			if taskErr != nil || !ok || zeta.State != tasks.StateTodo {
				t.Errorf("zeta-bridge after rejection = %+v, %v, %v; want todo", zeta, ok, taskErr)
			}
			log, readErr := os.ReadFile(filepath.Join(queue, stateInProgress, "alpha-docs", "log.md"))
			if readErr != nil {
				t.Errorf("read restored alpha-docs log: %v", readErr)
			}
			diagnostic := output
			if err != nil {
				diagnostic += "\n" + err.Error()
			}
			diagnostic += "\n" + string(log)
			for _, want := range []string{"wait", "reject", "tasks_block", "before committing"} {
				if !strings.Contains(strings.ToLower(diagnostic), want) {
					t.Errorf("wait rejection diagnostic/log lacks %q:\n%s", want, diagnostic)
				}
			}
			if lateCommit == "" {
				t.Fatal("worker did not preserve its late commit")
			}
			if short := forkspace.GitOut(repo, "rev-parse", "--short", lateCommit); short == "" || !strings.Contains(diagnostic, short) {
				t.Errorf("wait rejection did not name the late commit %s: %s", short, diagnostic)
			}
			if tc.want != "" && !strings.Contains(diagnostic, tc.want) {
				t.Errorf("wait rejection lacks %q: %s", tc.want, diagnostic)
			}
			if head := forkspace.GitOut(repo, "rev-parse", "HEAD"); head != lateCommit {
				t.Errorf("HEAD after wait rejection = %s, want preserved late commit %s", head, lateCommit)
			}
			if content := forkspace.GitOut(repo, "show", "HEAD:late-work.txt"); content != "committed after waiting" {
				t.Errorf("late committed file = %q, want preserved file contents", content)
			}
		})
	}
}

// checkTaskWait judges only what was committed after the wait began. A commit binding a task the
// attempt never touched is another session's and passes; anything else after the wait refuses, and
// so does a binding for the waiting task anywhere, a rewrite, or history Coop cannot read.
func TestCheckTaskWaitJudgesWhatWasCommittedAfterTheWait(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	// on commits message on top of parent and returns the commit, so each case builds its own history.
	on := func(parent, message string) string {
		t.Helper()
		if parent != "" {
			git("checkout", "-q", "--detach", parent)
		}
		git("commit", "-q", "--allow-empty", "-m", message)
		return forkspace.GitOut(repo, "rev-parse", "HEAD")
	}
	short := func(sha string) string { return forkspace.GitOut(repo, "rev-parse", "--short", sha) }
	root := on("", "root")
	base := on(root, "the wait begins here")
	foreign := on(base, "Another session's work\n\nCoop-Task: elsewhere-task")
	unbound := on(base, "Late work")
	own := on(base, "Late work\n\nCoop-Task: alpha-docs")
	archived := on(base, "Forged\n\nCoop-Task: omega-archived")
	two := on(base, "Two tasks\n\nCoop-Task: elsewhere-task\nCoop-Task: other-task")
	behind := on(foreign, "Late work after another session's")
	rewritten := on(root, "Rewritten")
	many := base
	for range 7 {
		many = on(many, "Late work")
	}
	touched := map[string]string{"alpha-docs": "is the task this attempt works on", "omega-archived": "was already done before this attempt started"}
	for name, head := range map[string]string{"unchanged": base, "another task's commit after the wait": foreign} {
		if err := checkTaskWait(repo, base, head, "alpha-docs", touched); err != nil {
			t.Errorf("%s: wait refused: %v", name, err)
		}
	}
	for _, tc := range []struct{ name, base, head, want string }{
		{"no base", "", base, "could not read the waiting attempt's Git history"},
		{"no head", base, "", "could not read the waiting attempt's Git history"},
		{"unbound commit after the wait", base, unbound, "commit " + short(unbound) + " was made after the wait began at " + base[:12]},
		{"waiting task's commit after the wait", base, own, "commit " + short(own) + " was made after the wait began"},
		{"archived task's commit after the wait", base, archived, "commit " + short(archived) + " was made after the wait began"},
		{"two trailers after the wait", base, two, "commit " + short(two) + " was made after the wait began"},
		{"late work behind another session's commit", base, behind, "commit " + short(behind) + " was made after the wait began"},
		{"many commits after the wait", base, many, ", 2 more were made after the wait began"},
		{"history rewritten after the wait", base, rewritten, "history was rewritten after the wait began at " + base[:12]},
		{"binding from before the wait", own, own, "already has a Coop-Task binding (" + short(own) + ")"},
		{"unreadable history", strings.Repeat("0", 40), strings.Repeat("0", 40), "could not check the waiting task's commit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkTaskWait(repo, tc.base, tc.head, "alpha-docs", touched); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("checkTaskWait = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// Another session commits in this checkout while the loop runs. Its commit before the wait, bound or
// not, is the ordinary binding check's to judge, and one binding a task this attempt never touched
// passes after the wait too: the wait stands and the run goes on.
func TestLoopKeepsAWaitPastAnotherSessionsCommit(t *testing.T) {
	for _, tc := range []struct {
		name, trailer string
		afterWait     bool
	}{
		{name: "unbound commit before the wait"},
		{name: "another task's commit before the wait", trailer: "elsewhere-task"},
		{name: "another task's commit after the wait", trailer: "elsewhere-task", afterWait: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, git, queue, c := waitLoopFixture(t)
			outside := func() {
				writeTaskFile(t, filepath.Join(repo, "outside.txt"), "another session's work\n")
				git("add", "outside.txt")
				message := "Another session's work"
				if tc.trailer != "" {
					message += "\n\nCoop-Task: " + tc.trailer
				}
				git("commit", "-m", message)
			}
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
				if len(worked) == 1 {
					if !tc.afterWait {
						outside()
					}
					callWaitTestTaskTool(t, s, "tasks_wait", map[string]any{
						"id": id, "on": "zeta-bridge", "reason": "its example must not land before the bridge fix",
					})
					if tc.afterWait {
						outside()
					}
				} else {
					writeTaskFile(t, filepath.Join(repo, id+".txt"), "implemented\n")
					git("add", id+".txt")
					git("commit", "-m", "Implement "+id+"\n\nCoop-Task: "+id)
					callWaitTestTaskTool(t, s, "tasks_complete", map[string]any{"id": id, "unrun_checks": []string{}})
				}
				_, err := io.WriteString(s.Stdout, waitTestResult)
				return 0, err
			}
			var code int
			var err error
			output := captureStderr(t, func() {
				code, err = c.Run(RunSpec{Repo: repo, Image: "fixture", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard, MaxTasks: 2,
					Rotation: ladder.NewRotation([]agents.Target{target("claude", "test")})})
			})
			if code != 0 || err != nil || strings.Join(worked, " ") != "alpha-docs zeta-bridge alpha-docs" {
				t.Fatalf("loop = %d, %v, worked %v; want the wait kept and both tasks finished\n%s", code, err, worked, output)
			}
			for _, id := range []string{"alpha-docs", "zeta-bridge"} {
				if item, ok, err := tasks.CurrentTask(queue, id); err != nil || !ok || item.State != tasks.StateDone {
					t.Errorf("%s after the run = %+v, %v, %v", id, item, ok, err)
				}
			}
		})
	}
}

// The box can make the completion audit fail. A wait it asked for in that attempt does not stand: the
// run stops, and the task is back in progress for the next attempt to finish or block.
func TestLoopTakesBackAWaitItCouldNotAudit(t *testing.T) {
	repo, _, queue, c := waitLoopFixture(t)
	other := filepath.Join(".agent", "tasks-b")
	if err := os.MkdirAll(filepath.Join(repo, other, stateTodo), 0o755); err != nil {
		t.Fatal(err)
	}
	var worked []string
	c.boxRun = func(s box.RunSpec) (int, error) {
		worked = append(worked, "attempt")
		if len(worked) > 1 {
			t.Fatalf("the loop went on after an attempt it could not audit")
		}
		callWaitTestTaskTool(t, s, "tasks_wait", map[string]any{
			"id": "alpha-docs", "on": "zeta-bridge", "reason": "its example must not land before the bridge fix",
		})
		// Planted in the other queue the loop works, so the audit fails while alpha-docs stays readable.
		if err := os.Symlink(t.TempDir(), filepath.Join(repo, other, stateTodo, "planted")); err != nil {
			t.Error(err)
		}
		_, err := io.WriteString(s.Stdout, waitTestResult)
		return 0, err
	}
	var code int
	var err error
	output := captureStderr(t, func() {
		code, err = c.Run(RunSpec{Repo: repo, Image: "fixture", Agent: "claude", Queues: []string{tasksRoot, other}, Sink: io.Discard, MaxTasks: 2,
			Rotation: ladder.NewRotation([]agents.Target{target("claude", "test")})})
	})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "scan task completions") {
		t.Fatalf("loop = %d, %v; want a stop on the failed audit\n%s", code, err, output)
	}
	alpha, ok, taskErr := tasks.CurrentTask(queue, "alpha-docs")
	if taskErr != nil || !ok || alpha.State != tasks.StateInProgress || alpha.WaitsOn != "" {
		t.Fatalf("alpha-docs after the failed audit = %+v, %v, %v; want in progress with no wait", alpha, ok, taskErr)
	}
	if log, err := os.ReadFile(filepath.Join(alpha.Dir, "log.md")); err != nil || !strings.Contains(string(log), "could not audit the attempt that asked to wait") {
		t.Fatalf("alpha-docs log = %q, %v", log, err)
	}
}
