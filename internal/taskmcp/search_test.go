package taskmcp

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/tasks"
)

type searchListing struct {
	Tasks     []listedTask `json:"tasks"`
	Matched   int          `json:"matched"`
	Returned  int          `json:"returned"`
	Truncated bool         `json:"truncated"`
	Hint      string       `json:"hint"`
}

func searchTasks(t *testing.T, sess *session, args map[string]any) searchListing {
	t.Helper()
	text := sess.mustCall("tasks_list", args)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &fields); err != nil || fields["tasks"] == nil || fields["matched"] == nil || fields["returned"] == nil || fields["truncated"] == nil {
		t.Fatal("search must explicitly report counts and truncation, including no matches")
	}
	var got searchListing
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(textResult(text))
	if err != nil || len(data) > 64<<10 || got.Tasks == nil || got.Returned != len(got.Tasks) || got.Returned > 20 || got.Truncated != (got.Returned < got.Matched) {
		t.Fatalf("invalid bounded search: bytes=%d returned=%d matched=%d truncated=%v", len(data), got.Returned, got.Matched, got.Truncated)
	}
	if got.Truncated && !strings.Contains(strings.ToLower(got.Hint), "narrow") {
		t.Fatal("omissions need narrow-query guidance")
	}
	return got
}

func TestTaskSearchAuthorityAndLiteralMatching(t *testing.T) {
	for _, fork := range []bool{false, true} {
		t.Run(fmt.Sprintf("fork=%v", fork), func(t *testing.T) {
			root := queue(t, map[string]string{"alpha": tasks.StateTodo, "beta": tasks.StateInProgress, "gamma": tasks.StateBlocked, "delta": tasks.StateDone})
			second := queue(t, map[string]string{"zeta": tasks.StateDone})
			outside := queue(t, map[string]string{"outside": tasks.StateDone})
			outbox := queue(t, map[string]string{"outbox": tasks.StateTodo})
			writeTask(t, filepath.Join(root, tasks.StateBacklog, "backlog"), "backlog")
			path := filepath.Join(root, tasks.StateDone, "delta", "task.md")
			if err := os.WriteFile(path, []byte("# Café retry [x].*\n\n## Subtasks\n- [x] check\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			server := newServer(t, root, "beta")
			server.authority.QueueRoots = []string{second, root}
			if fork {
				server.authority.ProposalOutbox = outbox
			}
			sess := newSession(t, server)
			before := []map[string]string{taskFiles(t, root), taskFiles(t, second), taskFiles(t, outside), taskFiles(t, outbox)}
			for _, tc := range []struct {
				query, state string
				ids          []string
			}{
				{" ALPHA ", "", []string{"alpha"}},
				{"title", "", []string{"zeta", "alpha", "beta", "gamma"}},
				{"TITLE", "todo", []string{"alpha"}},
				{"title", "in_progress", []string{"beta"}},
				{"title", "blocked", []string{"gamma"}},
				{"title", "done", []string{"zeta"}},
				{"CAFÉ", "done", []string{"delta"}},
				{"[x].*", "", []string{"delta"}},
				{"^title", "", nil},
				{"outside", "", nil},
				{"outbox", "", nil},
				{"backlog", "", nil},
				{outside, "", nil},
				{strings.Repeat("é", 128), "", nil},
			} {
				got := searchTasks(t, sess, map[string]any{"query": tc.query, "state": tc.state})
				var ids []string
				for _, item := range got.Tasks {
					ids = append(ids, item.ID)
					if item.Assigned != (item.ID == "beta") || item.Queue != root && item.Queue != second {
						t.Fatalf("search changed authority/assignment: %+v", item)
					}
				}
				if !slices.Equal(ids, tc.ids) || got.Matched != len(tc.ids) {
					t.Fatalf("query=%q state=%q: ids=%v matched=%d want=%v", tc.query, tc.state, ids, got.Matched, tc.ids)
				}
			}
			for i, dir := range []string{root, second, outside, outbox} {
				if !maps.Equal(before[i], taskFiles(t, dir)) {
					t.Fatal("search mutated a queue/outbox")
				}
			}
		})
	}
}

func TestTaskSearchRefusalAndSessionRecovery(t *testing.T) {
	root := queue(t, map[string]string{"needle": tasks.StateTodo})
	sess := newSession(t, newServer(t, root, ""))
	before := taskFiles(t, root)
	for _, query := range []any{"", "   ", "\tneedle", "needle\n", "needle\x00", "needle\x7f", strings.Repeat("x", 257), strings.Repeat("é", 129), nil, 123, true, []string{"needle"}} {
		sess.mustRefuse("tasks_list", map[string]any{"query": query})
		if got := searchTasks(t, sess, map[string]any{"query": "needle"}); got.Matched != 1 {
			t.Fatal("refusal damaged the session")
		}
	}
	sess.mustRefuse("tasks_list", map[string]any{"query": "needle", "queue": root})
	sess.mustRefuse("tasks_list", map[string]any{"query": "needle", "state": "shipped"})
	sess.send("{\"jsonrpc\":\"2.0\",\"id\":999,\"method\":\"tools/call\",\"params\":{\"name\":\"tasks_list\",\"arguments\":{\"query\":\"bad\xff\"}}}")
	if reply := sess.read(); reply["result"].(map[string]any)["isError"] != true {
		t.Fatal("invalid UTF-8 query was accepted")
	}
	searchTasks(t, sess, map[string]any{"query": "needle"})
	if !maps.Equal(before, taskFiles(t, root)) {
		t.Fatal("refusal or search mutated the queue")
	}
}

func TestTaskSearchBoundsAndUnfilteredCompatibility(t *testing.T) {
	root := queue(t, nil)
	sess := newSession(t, newServer(t, root, ""))
	if got := searchTasks(t, sess, map[string]any{"query": "archive"}); got.Matched != 0 {
		t.Fatal("empty queue matched")
	}
	for i := 0; i < 300; i++ {
		id := fmt.Sprintf("archive-%03d", i)
		writeTask(t, filepath.Join(root, tasks.StateDone, id), id)
	}
	got := searchTasks(t, sess, map[string]any{"query": "archive"})
	if got.Matched != 300 || got.Returned != 20 || got.Tasks[0].ID != "archive-000" || got.Tasks[19].ID != "archive-019" {
		t.Fatalf("wrong deterministic prefix: matched=%d returned=%d", got.Matched, got.Returned)
	}
	var unfiltered map[string]json.RawMessage
	if err := json.Unmarshal([]byte(sess.mustCall("tasks_list", nil)), &unfiltered); err != nil || len(unfiltered) != 1 {
		t.Fatal("no-query response shape changed")
	}
	var all []listedTask
	if err := json.Unmarshal(unfiltered["tasks"], &all); err != nil || len(all) != 300 {
		t.Fatal("no-query response was capped")
	}
	if got := searchTasks(t, sess, map[string]any{"query": "archive-299"}); got.Returned != 1 || got.Tasks[0].ID != "archive-299" {
		t.Fatal("search did not examine later tasks")
	}
}

func TestTaskSearchSerializedByteBound(t *testing.T) {
	root := queue(t, nil)
	sess := newSession(t, newServer(t, root, ""))
	// Escaped text fits the raw title budget but not the serialized MCP result.
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("size-%d", i)
		writeTask(t, filepath.Join(root, tasks.StateDone, id), id)
		if err := os.WriteFile(filepath.Join(root, tasks.StateDone, id, "task.md"), []byte("# size "+strings.Repeat("<", 5000)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := searchTasks(t, sess, map[string]any{"query": "size"})
	if got.Matched != 3 || got.Returned != 1 || got.Tasks[0].ID != "size-0" || got.Tasks[0].Title != "size "+strings.Repeat("<", 5000) {
		t.Fatalf("wrong serialized-size prefix: matched=%d returned=%d", got.Matched, got.Returned)
	}
	path := filepath.Join(root, tasks.StateDone, "size-0", "task.md")
	if err := os.WriteFile(path, []byte("# size "+strings.Repeat("<", 12000)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	text := sess.mustRefuse("tasks_list", map[string]any{"query": "size"})
	if len(text) > 1024 || !strings.Contains(text, "size-0") || !strings.Contains(text, "tasks_get") {
		t.Fatal("oversized first result lacks bounded retrieval guidance")
	}
	if got := searchTasks(t, sess, map[string]any{"query": "size-2"}); got.Returned != 1 {
		t.Fatal("oversized result damaged the session")
	}
}

func TestTaskSearchKeepsDistinctQueuesForTheSameID(t *testing.T) {
	first := queue(t, map[string]string{"duplicate": tasks.StateTodo})
	second := queue(t, map[string]string{"duplicate": tasks.StateDone})
	server := newServer(t, first, "")
	server.authority.QueueRoots = []string{second, first}
	sess := newSession(t, server)
	got := searchTasks(t, sess, map[string]any{"query": "duplicate"})
	if got.Matched != 2 || got.Tasks[0].Queue != second || got.Tasks[1].Queue != first {
		t.Fatal("same ID in distinct authorized queues was merged or reordered")
	}
	if sess.mustCall("tasks_list", nil) != sess.mustCall("tasks_list", map[string]any{"state": ""}) {
		t.Fatal("legacy all-state list changed")
	}
}

func TestTaskSearchRefusesOversizedOmittedMatch(t *testing.T) {
	for _, position := range []int{19, 20} {
		t.Run(fmt.Sprint(position+1), func(t *testing.T) {
			root := queue(t, nil)
			for i := 0; i <= position; i++ {
				id := fmt.Sprintf("match-%02d", i)
				writeTask(t, filepath.Join(root, tasks.StateDone, id), id)
			}
			id := fmt.Sprintf("match-%02d", position)
			if err := os.WriteFile(filepath.Join(root, tasks.StateDone, id, "task.md"), []byte("# match "+strings.Repeat("<", 12000)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			sess := newSession(t, newServer(t, root, ""))
			text := sess.mustRefuse("tasks_list", map[string]any{"query": "match"})
			if !strings.Contains(text, id) || !strings.Contains(text, "tasks_get") {
				t.Fatal("omitted oversized match lacks explicit retrieval guidance")
			}
			if got := searchTasks(t, sess, map[string]any{"query": "match-00"}); got.Returned != 1 {
				t.Fatal("narrow query could not recover")
			}
		})
	}
}
