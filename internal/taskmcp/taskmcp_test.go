package taskmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/tasks"
)

// session drives one Serve loop over an in-memory connection pair.
type session struct {
	t      *testing.T
	conn   net.Conn
	reader *bufio.Reader
	done   chan error
	cancel context.CancelFunc
	next   int
}

func newSession(t *testing.T, s *Server) *session {
	t.Helper()
	client, server := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, server) }()
	sess := &session{t: t, conn: client, reader: bufio.NewReaderSize(client, 8<<20), done: done, cancel: cancel}
	t.Cleanup(func() {
		cancel()
		client.Close()
		server.Close()
	})
	return sess
}

func (s *session) send(raw string) {
	s.t.Helper()
	if _, err := io.WriteString(s.conn, raw+"\n"); err != nil {
		s.t.Fatalf("write: %v", err)
	}
}

func (s *session) read() map[string]any {
	s.t.Helper()
	_ = s.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := s.reader.ReadBytes('\n')
	if err != nil {
		s.t.Fatalf("read: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(line, &frame); err != nil {
		s.t.Fatalf("decode %q: %v", line, err)
	}
	return frame
}

// request sends a JSON-RPC request and returns its reply.
func (s *session) request(method string, params any) map[string]any {
	s.t.Helper()
	s.next++
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": s.next, "method": method, "params": params})
	s.send(string(body))
	reply := s.read()
	if got, _ := reply["id"].(float64); int(got) != s.next {
		s.t.Fatalf("reply id = %v, want %d: %v", reply["id"], s.next, reply)
	}
	return reply
}

// call runs a tool and returns its text and whether it was flagged as an error.
func (s *session) call(name string, args map[string]any) (string, bool) {
	s.t.Helper()
	reply := s.request("tools/call", map[string]any{"name": name, "arguments": args})
	result, ok := reply["result"].(map[string]any)
	if !ok {
		s.t.Fatalf("tools/call %s: no result in %v", name, reply)
	}
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	isError, _ := result["isError"].(bool)
	return text, isError
}

func (s *session) mustCall(name string, args map[string]any) string {
	s.t.Helper()
	text, isError := s.call(name, args)
	if isError {
		s.t.Fatalf("%s refused: %s", name, text)
	}
	return text
}

func (s *session) mustRefuse(name string, args map[string]any) string {
	s.t.Helper()
	text, isError := s.call(name, args)
	if !isError {
		s.t.Fatalf("%s should have been refused, got: %s", name, text)
	}
	return text
}

func rpcErrorCode(t *testing.T, reply map[string]any) int {
	t.Helper()
	e, ok := reply["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error reply, got %v", reply)
	}
	return int(e["code"].(float64))
}

// queue builds a fixture queue with one task per requested state and returns its root.
func queue(t *testing.T, ids map[string]string) string {
	t.Helper()
	t.Setenv(tasks.TestLeaseAuthorityRootEnv, t.TempDir())
	root := filepath.Join(t.TempDir(), ".agent", "tasks")
	if err := tasks.ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	for id, state := range ids {
		writeTask(t, filepath.Join(root, state, id), id)
	}
	return root
}

func writeTask(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	title := "Title of " + id
	files := map[string]string{
		"task.md": "---\nid: " + id + "\ntitle: " + title + "\nlabels: []\nupdated: 2026-09-10T00:00:00Z\n---\n\n# " + title + "\n\n" +
			"**Context:** some context\n\n**Acceptance criteria:** gate green\n\n**Approach:** boring\n\n## Subtasks\n- [x] first\n- [ ] second\n",
		"state.md": "# State — " + title + "\n\n**Status:** in progress\n**Done so far:** first\n**Next action:** second\n**Traps:** —\n",
		"log.md":   "# Log — " + title + "\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func newServer(t *testing.T, root, assigned string) *Server {
	t.Helper()
	s, err := New(Authority{QueueRoots: []string{root}, Assigned: assigned,
		Owner: tasks.TaskLeaseOwner{RunID: "test-run", PID: os.Getpid(), Provider: "test", Target: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewNeedsAQueueRoot(t *testing.T) {
	if _, err := New(Authority{}); err == nil {
		t.Fatal("an authority without queue roots must be refused")
	}
}

func TestHandshakeEchoesAKnownProtocolVersionAndListsExactlyTheToolSet(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	sess := newSession(t, newServer(t, root, "t1"))
	reply := sess.request("initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})
	result := reply["result"].(map[string]any)
	if result["protocolVersion"] != "2024-11-05" {
		t.Fatalf("protocolVersion = %v, want the client's own", result["protocolVersion"])
	}
	if result["serverInfo"].(map[string]any)["name"] != ServerName {
		t.Fatalf("serverInfo = %v", result["serverInfo"])
	}
	if _, ok := result["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("capabilities = %v, want tools", result["capabilities"])
	}
	sess.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	// A notification gets no reply; the next reply must be to the list request.
	reply = sess.request("tools/list", nil)
	var names []string
	for _, tool := range reply["result"].(map[string]any)["tools"].([]any) {
		m := tool.(map[string]any)
		names = append(names, m["name"].(string))
		schema := m["inputSchema"].(map[string]any)
		if schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Fatalf("tool %s schema = %v", m["name"], schema)
		}
	}
	want := []string{"tasks_list", "tasks_get", "tasks_update_state", "tasks_append_log", "tasks_complete", "tasks_block", "tasks_set_subtasks", "tasks_propose"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools/list = %v, want exactly %v", names, want)
	}
	if !slices.Equal(ToolNames(), want) {
		t.Fatalf("ToolNames() = %v", ToolNames())
	}
	if reply := sess.request("ping", nil); reply["error"] != nil {
		t.Fatalf("ping = %v", reply)
	}
}

func TestUnknownProtocolVersionGetsTheServersOwn(t *testing.T) {
	root := queue(t, nil)
	sess := newSession(t, newServer(t, root, ""))
	reply := sess.request("initialize", map[string]any{"protocolVersion": "1999-01-01"})
	if got := reply["result"].(map[string]any)["protocolVersion"]; got != protocolVersion {
		t.Fatalf("protocolVersion = %v, want %s", got, protocolVersion)
	}
}

func TestProtocolFaults(t *testing.T) {
	root := queue(t, nil)
	sess := newSession(t, newServer(t, root, ""))
	sess.send(`{not json`)
	if code := rpcErrorCode(t, sess.read()); code != codeParse {
		t.Fatalf("malformed JSON code = %d, want %d", code, codeParse)
	}
	if code := rpcErrorCode(t, sess.request("exec", map[string]any{"command": "id"})); code != codeMethodNotFound {
		t.Fatalf("unknown method code = %d, want %d", code, codeMethodNotFound)
	}
	if code := rpcErrorCode(t, sess.request("tools/call", map[string]any{"name": "shell", "arguments": map[string]any{}})); code != codeInvalidParams {
		t.Fatalf("unknown tool code = %d, want %d", code, codeInvalidParams)
	}
	sess.send(`{"jsonrpc":"2.0","id":{"nested":1},"method":"ping"}`)
	if code := rpcErrorCode(t, sess.read()); code != codeInvalidRequest {
		t.Fatalf("object id code = %d, want %d", code, codeInvalidRequest)
	}
	sess.send(`{"id":7,"method":"ping"}`)
	if code := rpcErrorCode(t, sess.read()); code != codeInvalidRequest {
		t.Fatalf("missing jsonrpc code = %d, want %d", code, codeInvalidRequest)
	}
	// An oversized frame is refused and skipped; the session is still alive afterwards.
	sess.send(`{"jsonrpc":"2.0","id":99,"method":"ping","params":{"pad":"` + strings.Repeat("x", maxFrameBytes) + `"}}`)
	if code := rpcErrorCode(t, sess.read()); code != codeParse {
		t.Fatalf("oversized frame code = %d, want %d", code, codeParse)
	}
	if reply := sess.request("ping", nil); reply["error"] != nil {
		t.Fatalf("session did not survive the oversized frame: %v", reply)
	}
}

func TestSessionEndsCleanlyOnPeerClose(t *testing.T) {
	root := queue(t, nil)
	sess := newSession(t, newServer(t, root, ""))
	sess.request("ping", nil)
	sess.conn.Close()
	select {
	case err := <-sess.done:
		if err != nil {
			t.Fatalf("Serve returned %v on a clean close", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the peer closed")
	}
}

func TestListAndGet(t *testing.T) {
	root := queue(t, map[string]string{"a-todo": tasks.StateTodo, "b-doing": tasks.StateInProgress, "c-done": tasks.StateDone})
	sess := newSession(t, newServer(t, root, "b-doing"))
	var listing struct {
		Tasks []listedTask `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(sess.mustCall("tasks_list", nil)), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Tasks) != 3 || listing.Tasks[0].ID != "a-todo" || listing.Tasks[0].State != "todo" ||
		!listing.Tasks[1].Assigned || listing.Tasks[1].Subtasks.Done != 1 || listing.Tasks[1].Subtasks.Total != 2 ||
		listing.Tasks[2].Queue != root {
		t.Fatalf("tasks_list = %+v", listing.Tasks)
	}
	if err := json.Unmarshal([]byte(sess.mustCall("tasks_list", map[string]any{"state": "done"})), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Tasks) != 1 || listing.Tasks[0].ID != "c-done" {
		t.Fatalf("filtered tasks_list = %+v", listing.Tasks)
	}
	sess.mustRefuse("tasks_list", map[string]any{"state": "shipped"})
	var got struct {
		ID, Dir string
		Files   map[string]string `json:"files"`
	}
	if err := json.Unmarshal([]byte(sess.mustCall("tasks_get", map[string]any{"id": "b-doing"})), &got); err != nil {
		t.Fatal(err)
	}
	if got.Dir != filepath.Join(root, tasks.StateInProgress, "b-doing") || !strings.Contains(got.Files["task.md"], "## Subtasks") ||
		!strings.Contains(got.Files["state.md"], "**Status:**") || got.Files["decision.md"] != "" {
		t.Fatalf("tasks_get = %+v", got)
	}
	if text := sess.mustRefuse("tasks_get", map[string]any{"id": "b"}); !strings.Contains(text, `no task with id "b"`) {
		t.Fatalf("a fragment must not resolve: %s", text)
	}
	sess.mustRefuse("tasks_get", map[string]any{"id": "b-doing", "verbose": true})
}

func TestStateLogAndSubtasksOnTheAssignedTask(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	sess := newSession(t, newServer(t, root, "t1"))
	dir := filepath.Join(root, tasks.StateInProgress, "t1")
	sess.mustCall("tasks_update_state", map[string]any{"id": "t1", "status": "in progress — step 2", "done_so_far": "one\ntwo", "next_action": "three", "traps": "—"})
	sess.mustCall("tasks_update_state", map[string]any{"id": "t1", "next_action": "four"})
	state, _ := os.ReadFile(filepath.Join(dir, "state.md"))
	if want := "# State — Title of t1\n\n**Status:** in progress — step 2\n**Done so far:** one\ntwo\n**Next action:** four\n**Traps:** —\n"; string(state) != want {
		t.Fatalf("state.md = %q", state)
	}
	sess.mustRefuse("tasks_update_state", map[string]any{"id": "t1"})
	sess.mustRefuse("tasks_update_state", map[string]any{"id": "t1", "traps": nil})
	sess.mustRefuse("tasks_update_state", map[string]any{"id": "t1", "status": "a\nb", "done_so_far": "x", "next_action": "y", "traps": "z"})
	sess.mustRefuse("tasks_update_state", map[string]any{"id": "t1", "status": "esc\x1b[31m", "done_so_far": "x", "next_action": "y", "traps": "z"})
	sess.mustCall("tasks_append_log", map[string]any{"id": "t1", "entry": "## 2026-09-10 — did a thing\n- because"})
	log, _ := os.ReadFile(filepath.Join(dir, "log.md"))
	if want := "# Log — Title of t1\n\n## 2026-09-10 — did a thing\n- because\n"; string(log) != want {
		t.Fatalf("log.md = %q", log)
	}
	sess.mustRefuse("tasks_append_log", map[string]any{"id": "t1", "entry": "\x00"})
	text := sess.mustCall("tasks_set_subtasks", map[string]any{"id": "t1", "subtasks": []map[string]any{{"text": "first", "done": true}, {"text": "second half", "done": true}, {"text": "third", "done": false}}})
	if !strings.Contains(text, "2/3") {
		t.Fatalf("set_subtasks = %s", text)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "task.md"))
	if !strings.HasSuffix(string(body), "**Approach:** boring\n\n## Subtasks\n- [x] first\n- [x] second half\n- [ ] third\n") {
		t.Fatalf("task.md = %q", body)
	}
	items, err := tasks.ReadTaskTree(root)
	if err != nil || len(items) != 1 || !slices.Equal(items[0].Subtasks, []bool{true, true, false}) {
		t.Fatalf("host counter reads %+v (%v)", items, err)
	}
	if code, err := tasks.CmdTasksFolder(root, root, []string{"lint"}); code != 0 || err != nil {
		t.Fatalf("coop tasks lint = %d %v", code, err)
	}
	sess.mustRefuse("tasks_set_subtasks", map[string]any{"id": "t1", "subtasks": []map[string]any{}})
	sess.mustRefuse("tasks_set_subtasks", map[string]any{"id": "t1", "subtasks": []map[string]any{{"text": "a\nb"}}})
}

func TestSubtasksSectionIsAppendedWhenAbsentAndKeepsTrailingSections(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	dir := filepath.Join(root, tasks.StateInProgress, "t1")
	base := "---\nid: t1\ntitle: T\n---\n\n# T\n\n**Context:** c\n\n**Acceptance criteria:** a\n\n**Approach:** p\n"
	if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tasks.RewriteSubtasks(dir, []tasks.Subtask{{Text: "one"}}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "task.md"))
	if string(body) != base+"\n## Subtasks\n- [ ] one\n" {
		t.Fatalf("appended = %q", body)
	}
	withNotes := base + "\n## Subtasks\n- [ ] one\n- [ ] two\n\n## Notes\nkeep me\n"
	if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte(withNotes), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tasks.RewriteSubtasks(dir, []tasks.Subtask{{Text: "one", Done: true}}); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(filepath.Join(dir, "task.md"))
	if string(body) != base+"\n## Subtasks\n- [x] one\n\n## Notes\nkeep me\n" {
		t.Fatalf("rewritten = %q", body)
	}
}

func TestCompleteTheAssignedTaskMovesAndNormalizes(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	finishChecklist(t, root, "t1")
	sess := newSession(t, newServer(t, root, "t1"))
	sess.mustCall("tasks_complete", map[string]any{"id": "t1"})
	done := filepath.Join(root, tasks.StateDone, "t1")
	state, err := os.ReadFile(filepath.Join(done, "state.md"))
	if err != nil {
		t.Fatalf("task did not land in done: %v", err)
	}
	if !strings.Contains(string(state), "**Status:** complete\n") || !strings.Contains(string(state), "**Next action:** none\n") ||
		!strings.Contains(string(state), "**Done so far:** first\n") {
		t.Fatalf("state.md = %q", state)
	}
	if text := sess.mustCall("tasks_complete", map[string]any{"id": "t1"}); !strings.Contains(text, "already done") {
		t.Fatalf("second complete = %s", text)
	}
}

func TestAssignedCompletionRefusalCanBeRepairedInTheSameSession(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	finishChecklist(t, root, "t1")
	s := newServer(t, root, "t1")
	var ready atomic.Bool
	var checks atomic.Int32
	s.authority.ValidateAssignedCompletion = func(CompletionClaim) error {
		checks.Add(1)
		if !ready.Load() {
			return fmt.Errorf("missing Coop-Task binding")
		}
		return nil
	}
	sess := newSession(t, s)
	before, err := os.ReadFile(filepath.Join(root, tasks.StateInProgress, "t1", "state.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := sess.mustRefuse("tasks_complete", map[string]any{"id": "t1"})
	if !strings.Contains(text, "missing Coop-Task binding") || !strings.Contains(text, "retry tasks_complete in this turn") {
		t.Fatalf("completion refusal lacks repair guidance: %s", text)
	}
	after, err := os.ReadFile(filepath.Join(root, tasks.StateInProgress, "t1", "state.md"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("refused completion changed state: %q, %v", after, err)
	}
	if _, err := os.Stat(filepath.Join(root, tasks.StateDone, "t1")); !os.IsNotExist(err) {
		t.Fatalf("refused task moved to done: %v", err)
	}
	ready.Store(true)
	sess.mustCall("tasks_complete", map[string]any{"id": "t1"})
	if checks.Load() != 2 {
		t.Fatalf("completion checks = %d, want a fresh check on both calls", checks.Load())
	}
	sess.mustCall("tasks_complete", map[string]any{"id": "t1"})
	if checks.Load() != 2 {
		t.Fatal("idempotent completion revalidated an already-moved task")
	}
}

func TestServerRetainsOnlyTheAssignedTerminalRefusal(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress, "t2": tasks.StateInProgress})
	finishChecklist(t, root, "t1")
	s := newServer(t, root, "t1")
	s.authority.ValidateAssignedCompletion = func(CompletionClaim) error { return errors.New("missing task binding") }
	sess := newSession(t, s)

	completeRefusal := sess.mustRefuse("tasks_complete", map[string]any{"id": "t1"})
	got, ok := s.LatestAssignedTerminalRefusal()
	if !ok || got.Action != "tasks_complete" || got.Detail != completeRefusal {
		t.Fatalf("completion refusal = %+v, %v; want exact %q", got, ok, completeRefusal)
	}

	// An ordinary tool error and another task's terminal error are not worker-exit guidance.
	sess.mustRefuse("tasks_update_state", map[string]any{"id": "t1"})
	sess.mustRefuse("tasks_block", map[string]any{"id": "t2"})
	if after, _ := s.LatestAssignedTerminalRefusal(); after != got {
		t.Fatalf("unrelated refusal replaced assigned terminal refusal: %+v", after)
	}

	blockRefusal := sess.mustRefuse("tasks_block", map[string]any{"id": "t1"})
	got, ok = s.LatestAssignedTerminalRefusal()
	if !ok || got.Action != "tasks_block" || got.Detail != blockRefusal || !strings.Contains(got.Detail, "decision, options, recommendation") {
		t.Fatalf("block refusal = %+v, %v; want exact %q", got, ok, blockRefusal)
	}
}

func TestAssignedTaskCanCloseWithAnExplicitNoChangeOutcome(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	finishChecklist(t, root, "t1")
	s := newServer(t, root, "t1")
	s.authority.ValidateAssignedCompletion = func(claim CompletionClaim) error {
		if claim.Outcome != "already_satisfied" || claim.Reason != "Existing implementation covers the task." || claim.Evidence != "Commit for task-old; focused check passed." {
			t.Fatalf("claim = %+v", claim)
		}
		return nil
	}
	sess := newSession(t, s)
	before := taskFiles(t, root)
	for _, args := range []map[string]any{
		{"id": "t1", "outcome": "already_satisfied"},
		{"id": "t1", "outcome": "already_satisfied", "reason": "why", "evidence": nil},
		{"id": "t1", "reason": "why", "evidence": "proof"},
	} {
		sess.mustRefuse("tasks_complete", args)
	}
	if !maps.Equal(before, taskFiles(t, root)) {
		t.Fatal("invalid no-change completion mutated task files")
	}
	text := sess.mustCall("tasks_complete", map[string]any{
		"id": "t1", "outcome": "already_satisfied",
		"reason": "Existing implementation covers the task.", "evidence": "Commit for task-old; focused check passed.",
	})
	if !strings.Contains(text, "closed as already satisfied") {
		t.Fatalf("completion = %s", text)
	}
	dir := filepath.Join(root, tasks.StateDone, "t1")
	state, _ := os.ReadFile(filepath.Join(dir, "state.md"))
	log, _ := os.ReadFile(filepath.Join(dir, "log.md"))
	for _, want := range []string{"**Status:** complete — already satisfied", "Existing implementation covers the task.", "Commit for task-old; focused check passed."} {
		if !strings.Contains(string(state), want) {
			t.Fatalf("state lacks %q: %s", want, state)
		}
	}
	if !strings.Contains(string(log), "## Closure — already satisfied") {
		t.Fatalf("log = %s", log)
	}
}

func TestBlockTheAssignedTaskWritesItsDecision(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	sess := newSession(t, newServer(t, root, "t1"))
	sess.mustRefuse("tasks_block", map[string]any{"id": "t1", "decision": "q", "options": []string{}, "recommendation": "r"})
	sess.mustCall("tasks_block", map[string]any{"id": "t1", "decision": "Postgres or SQLite?", "options": []string{"A — Postgres: ops cost", "B — SQLite: single node"}, "recommendation": "A — scale matters"})
	blocked := filepath.Join(root, tasks.StateBlocked, "t1")
	decision, err := os.ReadFile(filepath.Join(blocked, "decision.md"))
	if err != nil {
		t.Fatalf("task did not land in blocked with a decision: %v", err)
	}
	for _, want := range []string{"# Decision: Title of t1?", "**The decision:** Postgres or SQLite?", "- A — Postgres: ops cost", "- B — SQLite: single node", "**Recommendation:** A — scale matters", "**Resolution:** <!-- Human: write your answer here"} {
		if !strings.Contains(string(decision), want) {
			t.Fatalf("decision.md lacks %q:\n%s", want, decision)
		}
	}
	if code, err := tasks.CmdTasksFolder(root, root, []string{"lint"}); code != 0 || err != nil {
		t.Fatalf("coop tasks lint = %d %v", code, err)
	}
}

// The one refusal: a mutation on a task another live process holds. The holder here is a lease
// taken through the same host authority a concurrent `coop loop` would hold.
func TestMutationOnATaskAnotherLiveProcessHoldsIsRefused(t *testing.T) {
	root := queue(t, map[string]string{"mine": tasks.StateInProgress, "theirs": tasks.StateInProgress, "free": tasks.StateTodo})
	items, err := tasks.ReadTaskTree(root)
	if err != nil {
		t.Fatal(err)
	}
	var theirs tasks.Item
	for _, item := range items {
		if item.ID == "theirs" {
			theirs = item
		}
	}
	lease, observed, err := tasks.TryTaskLease(root, theirs, tasks.TaskLeaseOwner{RunID: "other-run", PID: os.Getpid(), Provider: "codex", Target: "codex"})
	if err != nil || lease == nil {
		t.Fatalf("fixture lease: %v %v", err, observed)
	}
	defer lease.Release()
	sess := newSession(t, newServer(t, root, "mine"))
	for name, args := range map[string]map[string]any{
		"tasks_append_log":   {"id": "theirs", "entry": "hi"},
		"tasks_update_state": {"id": "theirs", "status": "s", "done_so_far": "d", "next_action": "n", "traps": "t"},
		"tasks_set_subtasks": {"id": "theirs", "subtasks": []map[string]any{{"text": "x"}}},
		"tasks_complete":     {"id": "theirs"},
		"tasks_block":        {"id": "theirs", "decision": "q", "options": []string{"A"}, "recommendation": "A"},
	} {
		text := sess.mustRefuse(name, args)
		if !strings.Contains(text, "held by another live process") || !strings.Contains(text, "theirs") {
			t.Fatalf("%s refusal is not legible: %s", name, text)
		}
	}
	log, _ := os.ReadFile(filepath.Join(root, tasks.StateInProgress, "theirs", "log.md"))
	if strings.Contains(string(log), "hi") {
		t.Fatal("the refused append still wrote")
	}
	// Reads are never refused, and an unheld task is fully reachable: the server leases it for the
	// call and lets go after.
	sess.mustCall("tasks_get", map[string]any{"id": "theirs"})
	sess.mustCall("tasks_append_log", map[string]any{"id": "free", "entry": "note on a free task"})
	log, _ = os.ReadFile(filepath.Join(root, tasks.StateTodo, "free", "log.md"))
	if !strings.Contains(string(log), "note on a free task") {
		t.Fatalf("append on a free task did not land: %q", log)
	}
	free, _, err := tasks.CurrentTask(root, "free")
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := tasks.TryTaskLease(root, free, tasks.TaskLeaseOwner{RunID: "x", PID: os.Getpid()})
	if err != nil || again == nil {
		t.Fatalf("the server did not release its lease on the free task: %v", err)
	}
	again.Release()
}

// A server with no assigned task (nothing leased by its host) still completes an unheld task —
// through the host's trusted completion, which takes and records its own lease — and is refused
// on one that is held.
func TestCompleteWithoutAnAssignedLeaseUsesTheTrustedPath(t *testing.T) {
	root := queue(t, map[string]string{"free": tasks.StateInProgress, "held": tasks.StateInProgress})
	finishChecklist(t, root, "free")
	held, _, err := tasks.CurrentTask(root, "held")
	if err != nil {
		t.Fatal(err)
	}
	lease, _, err := tasks.TryTaskLease(root, held, tasks.TaskLeaseOwner{RunID: "other", PID: os.Getpid(), Provider: "claude"})
	if err != nil || lease == nil {
		t.Fatalf("fixture lease: %v", err)
	}
	defer lease.Release()
	sess := newSession(t, newServer(t, root, ""))
	if text := sess.mustRefuse("tasks_complete", map[string]any{"id": "held"}); !strings.Contains(text, "held by another live process") {
		t.Fatalf("refusal = %s", text)
	}
	sess.mustCall("tasks_complete", map[string]any{"id": "free"})
	state, err := os.ReadFile(filepath.Join(root, tasks.StateDone, "free", "state.md"))
	if err != nil || !strings.Contains(string(state), "**Status:** complete") {
		t.Fatalf("trusted completion did not land: %v %q", err, state)
	}
	if text := sess.mustRefuse("tasks_block", map[string]any{"id": "free", "decision": "q", "options": []string{"A"}, "recommendation": "A"}); !strings.Contains(text, "already done") {
		t.Fatalf("block on a done task = %s", text)
	}
}

func TestProposeCreatesAQueueFolderTheHostWouldHave(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	sess := newSession(t, newServer(t, root, "t1"))
	draft := map[string]any{"kind": "task", "title": "Fix the login retry", "context": "ctx", "acceptance": "gate green", "approach": "boring", "subtasks": []string{"one", "two"}}
	text := sess.mustCall("tasks_propose", draft)
	id := time.Now().Format("2006-01-02") + "-fix-the-login-retry"
	if !strings.Contains(text, id) || !strings.Contains(text, tasks.StateTodo) {
		t.Fatalf("propose = %s", text)
	}
	body, err := os.ReadFile(filepath.Join(root, tasks.StateTodo, id, "task.md"))
	if err != nil {
		t.Fatalf("no todo folder: %v", err)
	}
	for _, want := range []string{"title: Fix the login retry", "**Context:** ctx", "**Acceptance criteria:** gate green", "**Approach:** boring", "- [ ] one\n- [ ] two\n"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("task.md lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(string(body), "TASK SPEC") {
		t.Fatal("a filled task must not carry the fill-me header")
	}
	if _, err := os.Stat(filepath.Join(root, tasks.StateTodo, id, "state.md")); err != nil {
		t.Fatalf("state.md not seeded: %v", err)
	}
	if code, err := tasks.CmdTasksFolder(root, root, []string{"lint"}); code != 0 || err != nil {
		t.Fatalf("coop tasks lint = %d %v", code, err)
	}
	// The same title twice is the CLI's own collision refusal.
	if text := sess.mustRefuse("tasks_propose", draft); !strings.Contains(text, "already exists") {
		t.Fatalf("collision = %s", text)
	}
	backlog := map[string]any{"kind": "backlog", "title": "Rewrite everything", "context": "c", "acceptance": "a", "approach": "p", "subtasks": []string{"scope it"}}
	if text := sess.mustCall("tasks_propose", backlog); !strings.Contains(text, tasks.StateBacklog) {
		t.Fatalf("backlog propose = %s", text)
	}
	if _, err := os.Stat(filepath.Join(root, tasks.StateBacklog, time.Now().Format("2006-01-02")+"-rewrite-everything", "task.md")); err != nil {
		t.Fatalf("no backlog folder: %v", err)
	}
	// Validation is the fork-proposal rule: control characters and over-limit text are refused.
	for _, bad := range []map[string]any{
		{"kind": "task", "title": "esc\x1b", "context": "c", "acceptance": "a", "approach": "p", "subtasks": []string{"s"}},
		{"kind": "task", "title": "ok", "context": strings.Repeat("x", 64<<10+1), "acceptance": "a", "approach": "p", "subtasks": []string{"s"}},
		{"kind": "task", "title": "ok", "context": "c", "acceptance": "a", "approach": "p", "subtasks": []string{}},
		{"kind": "epic", "title": "ok", "context": "c", "acceptance": "a", "approach": "p", "subtasks": []string{"s"}},
		{"kind": "task", "title": "ok", "context": "c", "acceptance": "a", "approach": "p", "subtasks": []string{"s"}, "queue": "/nope"},
	} {
		sess.mustRefuse("tasks_propose", bad)
	}
}

func TestProposeInAForkWritesTheOutboxTheHostImports(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	outbox := t.TempDir()
	s, err := New(Authority{QueueRoots: []string{root}, Assigned: "t1", ProposalOutbox: outbox})
	if err != nil {
		t.Fatal(err)
	}
	sess := newSession(t, s)
	text := sess.mustCall("tasks_propose", map[string]any{"kind": "task", "title": "Fork finding", "context": "c", "acceptance": "a", "approach": "p", "subtasks": []string{"s"}})
	entries, err := os.ReadDir(outbox)
	if err != nil || len(entries) != 1 {
		t.Fatalf("outbox = %v %v", entries, err)
	}
	name := entries[0].Name()
	if !strings.HasSuffix(name, ".json") || len(name) != 37 || !strings.Contains(text, name) {
		t.Fatalf("outbox file = %s (%s)", name, text)
	}
	data, _ := os.ReadFile(filepath.Join(outbox, name))
	var proposal tasks.ForkTaskProposal
	if err := json.Unmarshal(data, &proposal); err != nil {
		t.Fatal(err)
	}
	if proposal.Version != 1 || proposal.ID+".json" != name || proposal.Kind != tasks.ForkProposalTask || proposal.Title != "Fork finding" || !slices.Equal(proposal.Subtasks, []string{"s"}) {
		t.Fatalf("proposal = %+v", proposal)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, tasks.StateTodo)); len(entries) != 0 {
		t.Fatalf("a fork proposal must not create a queue folder: %v", entries)
	}
}

// Every session shares one server; two concurrent sessions never corrupt each other's replies.
func TestConcurrentSessionsShareTheQueue(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	s := newServer(t, root, "t1")
	a, b := newSession(t, s), newSession(t, s)
	for i := 0; i < 20; i++ {
		a.mustCall("tasks_append_log", map[string]any{"id": "t1", "entry": fmt.Sprintf("a%d", i)})
		b.mustCall("tasks_append_log", map[string]any{"id": "t1", "entry": fmt.Sprintf("b%d", i)})
	}
	log, _ := os.ReadFile(filepath.Join(root, tasks.StateInProgress, "t1", "log.md"))
	if strings.Count(string(log), "\na") != 20 || strings.Count(string(log), "\nb") != 20 {
		t.Fatalf("log.md lost entries:\n%s", log)
	}
}
