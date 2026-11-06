package taskmcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/tasks"
)

// newSessionServer serves the fixture queue at root to an interactive session bound to actor.
func newSessionServer(t *testing.T, root string, actor tasks.ClaimActor) *Server {
	t.Helper()
	s, err := New(Authority{Workspace: filepath.Dir(filepath.Dir(root)), QueueRoots: []string{root}, Session: &actor,
		Owner: tasks.TaskLeaseOwner{PID: os.Getpid(), Provider: actor.Label, Target: actor.Label}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sessionActor(t *testing.T, label string, pid int) tasks.ClaimActor {
	t.Helper()
	actor, err := tasks.SessionClaimActor(label, pid)
	if err != nil {
		t.Skipf("no stable process identity on this platform: %v", err)
	}
	return actor
}

// liveProcess starts a child that outlives the test body, so a second session has a live process
// to bind its claims to.
func liveProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a sleeper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

func TestNewRefusesASessionMixedWithLoopAuthority(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateTodo})
	workspace := filepath.Dir(filepath.Dir(root))
	actor := sessionActor(t, "claude", os.Getpid())
	for name, authority := range map[string]Authority{
		"assigned task": {Workspace: workspace, QueueRoots: []string{root}, Session: &actor, Assigned: "t1"},
		"fork outbox":   {Workspace: workspace, QueueRoots: []string{root}, Session: &actor, ProposalOutbox: outboxIn(t, root)},
		"unbound actor": {Workspace: workspace, QueueRoots: []string{root}, Session: &tasks.ClaimActor{Label: "claude"}},
	} {
		if _, err := New(authority); err == nil {
			t.Errorf("%s: New accepted a session authority it must refuse", name)
		}
	}
}

// A session is told it has no coop CLI and is offered the claim/release pair; a loop box is not.
func TestSessionAddsClaimAndReleaseOnlyForAnInteractiveSession(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateTodo})
	sess := newSession(t, newSessionServer(t, root, sessionActor(t, "claude", os.Getpid())))
	result := sess.request("initialize", map[string]any{"protocolVersion": protocolVersion})["result"].(map[string]any)
	instructions, _ := result["instructions"].(string)
	for _, want := range []string{"not installed", "tasks_claim", "tasks_release", "never by moving task folders"} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("session instructions lack %q: %s", want, instructions)
		}
	}
	var listed []string
	for _, tool := range sess.request("tools/list", nil)["result"].(map[string]any)["tools"].([]any) {
		listed = append(listed, tool.(map[string]any)["name"].(string))
	}
	want := []string{"tasks_list", "tasks_get", "tasks_claim", "tasks_update_state", "tasks_append_log", "tasks_complete", "tasks_block", "tasks_release", "tasks_set_subtasks", "tasks_propose"}
	if !slices.Equal(listed, want) {
		t.Fatalf("session tools/list = %v, want %v", listed, want)
	}

	loop := newSession(t, newServer(t, queue(t, map[string]string{"mine": tasks.StateInProgress}), "mine"))
	if instructions := loop.request("initialize", nil)["result"].(map[string]any)["instructions"].(string); strings.Contains(instructions, "tasks_claim") {
		t.Fatalf("a loop box must not be told to claim: %s", instructions)
	}
	for _, name := range []string{"tasks_claim", "tasks_release"} {
		reply := loop.request("tools/call", map[string]any{"name": name, "arguments": map[string]any{"id": "mine"}})
		if code := rpcErrorCode(t, reply); code != codeInvalidParams {
			t.Fatalf("%s on a loop box = %v, want unknown tool", name, reply)
		}
	}
}

// The whole interactive cycle runs through the tools: claim a todo task, work it, complete it.
func TestSessionClaimsWorksAndCompletesATask(t *testing.T) {
	root := queue(t, map[string]string{"work": tasks.StateTodo})
	actor := sessionActor(t, "claude", os.Getpid())
	sess := newSession(t, newSessionServer(t, root, actor))
	if text := sess.mustCall("tasks_claim", map[string]any{"id": "work"}); !strings.Contains(text, "claimed by this session") {
		t.Fatalf("claim = %s", text)
	}
	if _, err := os.Stat(filepath.Join(root, tasks.StateInProgress, "work")); err != nil {
		t.Fatalf("claimed task is not in progress: %v", err)
	}
	record, owned, err := tasks.ReadTaskOwnerRecord(root, "work")
	if err != nil || !owned || record.ActorPID != actor.PID || record.Actor != "claude" {
		t.Fatalf("claim record = %+v, %t, %v", record, owned, err)
	}
	if text := sess.mustCall("tasks_list", map[string]any{"state": "in_progress"}); !strings.Contains(text, `"work"`) {
		t.Fatalf("list after claim = %s", text)
	}
	sess.mustCall("tasks_append_log", map[string]any{"id": "work", "entry": "## today — did it"})
	sess.mustCall("tasks_set_subtasks", map[string]any{"id": "work", "subtasks": []map[string]any{{"text": "first", "done": true}, {"text": "second", "done": true}}})
	if text := sess.mustRefuse("tasks_complete", map[string]any{"id": "work", "unrun_checks": []string{}, "outcome": "already_satisfied", "reason": "r", "evidence": "e"}); !strings.Contains(text, "without outcome") {
		t.Fatalf("a session's no-change completion must explain the plain path: %s", text)
	}
	if text := sess.mustCall("tasks_get", map[string]any{"id": "work"}); !strings.Contains(text, `"claim": "yours"`) {
		t.Fatalf("the session's own claim must read as yours: %s", text)
	}
	sess.mustCall("tasks_complete", map[string]any{"id": "work", "unrun_checks": []string{}})
	if _, err := os.Stat(filepath.Join(root, tasks.StateDone, "work")); err != nil {
		t.Fatalf("completed task is not done: %v", err)
	}
	// A done task is final: a late note would take a lease that clears its completion receipt.
	if text := sess.mustRefuse("tasks_append_log", map[string]any{"id": "work", "entry": "## later — one more note"}); !strings.Contains(text, "final") {
		t.Fatalf("a write to a done task = %s", text)
	}
	if _, owned, _ := tasks.ReadTaskOwnerRecord(root, "work"); owned {
		t.Fatal("completion must clear the session's claim")
	}
}

func TestSessionClaimAndReleaseRefuseWhatTheSessionDoesNotOwn(t *testing.T) {
	root := queue(t, map[string]string{
		"theirs": tasks.StateTodo, "held": tasks.StateInProgress, "blocked": tasks.StateBlocked,
		"done": tasks.StateDone, "mine": tasks.StateTodo, "free": tasks.StateTodo,
	})
	other := newSession(t, newSessionServer(t, root, sessionActor(t, "codex", liveProcess(t))))
	other.mustCall("tasks_claim", map[string]any{"id": "theirs"})
	held, _, err := tasks.CurrentTask(root, "held")
	if err != nil {
		t.Fatal(err)
	}
	lease, _, err := tasks.TryTaskLease(root, held, tasks.TaskLeaseOwner{RunID: "loop", PID: os.Getpid(), Provider: "gemini"})
	if err != nil || lease == nil {
		t.Fatalf("fixture lease: %v", err)
	}
	defer lease.Release()

	sess := newSession(t, newSessionServer(t, root, sessionActor(t, "claude", os.Getpid())))
	if text := sess.mustCall("tasks_get", map[string]any{"id": "theirs"}); !strings.Contains(text, `"claim": "claimed by codex (PID`) {
		t.Fatalf("another session's claim must be named: %s", text)
	}
	// Another live session's claim keeps every write off its task, not just a second claim.
	for name, args := range map[string]map[string]any{
		"tasks_append_log":   {"id": "theirs", "entry": "x"},
		"tasks_update_state": {"id": "theirs", "status": "s"},
		"tasks_set_subtasks": {"id": "theirs", "subtasks": []map[string]any{{"text": "x"}}},
		"tasks_complete":     {"id": "theirs", "unrun_checks": []string{}},
		"tasks_block":        {"id": "theirs", "question": "q", "why": "W", "options": []string{"A"}, "recommendation": "A"},
	} {
		if text := sess.mustRefuse(name, args); !strings.Contains(text, "claimed by codex") {
			t.Fatalf("%s on another session's claimed task = %s", name, text)
		}
	}
	for id, want := range map[string]string{
		"theirs": "claimed by codex", "held": "held by another live process", "blocked": "human", "done": "archive",
	} {
		if text := sess.mustRefuse("tasks_claim", map[string]any{"id": id}); !strings.Contains(text, want) {
			t.Fatalf("claim %s refusal = %s, want it to say %q", id, text, want)
		}
	}
	if text := sess.mustRefuse("tasks_release", map[string]any{"id": "theirs"}); !strings.Contains(text, "claimed by codex") {
		t.Fatalf("releasing another session's claim = %s", text)
	}
	if _, err := os.Stat(filepath.Join(root, tasks.StateInProgress, "theirs")); err != nil {
		t.Fatalf("a refused release moved the other session's task: %v", err)
	}
	if text := sess.mustCall("tasks_release", map[string]any{"id": "free"}); !strings.Contains(text, "already") {
		t.Fatalf("releasing an unclaimed todo task = %s", text)
	}
	sess.mustCall("tasks_claim", map[string]any{"id": "mine"})
	if text := sess.mustCall("tasks_release", map[string]any{"id": "mine"}); !strings.Contains(text, "returned to 00_todo/") {
		t.Fatalf("release of the session's own claim = %s", text)
	}
	if _, owned, _ := tasks.ReadTaskOwnerRecord(root, "mine"); owned {
		t.Fatal("release must drop the session's claim")
	}
	if _, err := os.Stat(filepath.Join(root, tasks.StateTodo, "mine")); err != nil {
		t.Fatalf("released task is not in todo: %v", err)
	}
}

// A loop box keeps to its assignment: a task a live session claimed, and a done task, are refused
// to it as they are to a session, so it cannot close or rewrite either.
func TestLoopBoxStaysOffClaimedAndDoneTasks(t *testing.T) {
	root := queue(t, map[string]string{"mine": tasks.StateInProgress, "theirs": tasks.StateTodo, "shipped": tasks.StateDone})
	session := newSession(t, newSessionServer(t, root, sessionActor(t, "codex", liveProcess(t))))
	session.mustCall("tasks_claim", map[string]any{"id": "theirs"})
	loop := newSession(t, newServer(t, root, "mine"))
	for name, args := range map[string]map[string]any{
		"tasks_append_log":   {"id": "theirs", "entry": "x"},
		"tasks_set_subtasks": {"id": "theirs", "subtasks": []map[string]any{{"text": "x", "done": true}}},
		"tasks_complete":     {"id": "theirs", "unrun_checks": []string{}},
		"tasks_block":        {"id": "theirs", "question": "q", "why": "W", "options": []string{"A"}, "recommendation": "A"},
	} {
		if text := loop.mustRefuse(name, args); !strings.Contains(text, "claimed by codex") {
			t.Fatalf("loop box %s on a session's claimed task = %s", name, text)
		}
	}
	if _, err := os.Stat(filepath.Join(root, tasks.StateInProgress, "theirs")); err != nil {
		t.Fatalf("the session's task moved: %v", err)
	}
	if text := loop.mustRefuse("tasks_append_log", map[string]any{"id": "shipped", "entry": "late"}); !strings.Contains(text, "final") {
		t.Fatalf("loop box write to a done task = %s", text)
	}
	loop.mustCall("tasks_append_log", map[string]any{"id": "mine", "entry": "## now — own task"})
}
