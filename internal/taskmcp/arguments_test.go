package taskmcp

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/tasks"
)

func TestTaskArgumentsRejectInvalidUTF8BeforeDecoding(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool string
		args string
	}{
		{"complete", "tasks_append_log", `{"id":"t1","entry":"` + string([]byte{0xff}) + `"}`},
		{"missing fields", "tasks_update_state", `{"id":"t1","status":"in progress","done_so_far":"` + string([]byte{0xff}) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := queue(t, map[string]string{"t1": tasks.StateInProgress})
			sess := newSession(t, newServer(t, root, "t1"))
			before := taskFiles(t, root)
			sess.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tc.tool + `","arguments":` + tc.args + `}}`)
			reply := sess.read()
			result := reply["result"].(map[string]any)
			if result["isError"] != true || !maps.Equal(before, taskFiles(t, root)) {
				t.Fatalf("invalid UTF-8 was accepted or mutated the task: %v", result)
			}
			if !strings.Contains(fmt.Sprint(result["content"]), "send valid UTF-8 JSON") {
				t.Fatalf("malformed UTF-8 did not reach the strict wire guard: %v", result)
			}
		})
	}
}

func TestTaskArgumentSchemasDescribeRuntimeBoundsAndExamples(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	sess := newSession(t, newServer(t, root, "t1"))
	reply := sess.request("tools/list", nil)
	descriptors := reply["result"].(map[string]any)["tools"].([]any)
	for _, raw := range descriptors {
		descriptor := raw.(map[string]any)
		name := descriptor["name"].(string)
		schema := descriptor["inputSchema"].(map[string]any)
		properties := schema["properties"].(map[string]any)
		for _, field := range []string{"options", "subtasks"} {
			if p, ok := properties[field].(map[string]any); ok {
				if p["minItems"] != float64(1) || p["maxItems"] != float64(tasks.TaskListLimit) {
					t.Fatalf("%s.%s list bounds = %v", name, field, p)
				}
			}
		}
		switch name {
		case "tasks_update_state":
			if len(examples(schema)) != 1 {
				t.Fatal("state tool needs one complete example")
			}
			if !strings.Contains(descriptor["description"].(string), "EVERY call") {
				t.Fatal("snapshot replacement semantics absent")
			}
			for _, field := range []string{"status", "done_so_far", "next_action", "traps"} {
				p := properties[field].(map[string]any)
				if p["minLength"] != float64(1) || p["maxLength"] != nil || !strings.Contains(p["description"].(string), "65536 bytes") {
					t.Fatalf("%s byte limit is missing or advertised as a character maximum: %v", field, p)
				}
			}
		case "tasks_propose":
			if len(examples(schema)) != 1 {
				t.Fatal("proposal tool needs one complete example")
			}
			for field, want := range map[string]string{"context": "distinguish suspected cause from proven evidence", "acceptance": "preserving the failing behavior and denial assertions"} {
				if !strings.Contains(properties[field].(map[string]any)["description"].(string), want) {
					t.Fatalf("proposal %s lacks evidence guidance", field)
				}
			}
			proposalExample := examples(schema)[0]
			if !strings.Contains(proposalExample["context"].(string), "cause unknown") || !strings.Contains(proposalExample["acceptance"].(string), "missing or wrong crash reason still fails") {
				t.Fatal("proposal example overstates cause or weakens denial")
			}
			p := properties["title"].(map[string]any)
			if !strings.Contains(p["description"].(string), "256 bytes") {
				t.Fatalf("title constraint = %v", p)
			}
			values := properties["kind"].(map[string]any)["enum"]
			body, _ := json.Marshal(values)
			if string(body) != `["task","backlog"]` {
				t.Fatalf("kind enum = %s", body)
			}
		case "tasks_list":
			body, _ := json.Marshal(properties["state"].(map[string]any)["enum"])
			if string(body) != `["","todo","in_progress","blocked","done"]` {
				t.Fatalf("state enum = %s", body)
			}
			sess.mustCall(name, map[string]any{"state": ""})
		}
		for _, example := range examples(schema) {
			args := maps.Clone(example)
			if _, ok := args["id"]; ok {
				args["id"] = "t1"
			}
			sess.mustCall(name, args)
		}
	}
}

func examples(schema map[string]any) []map[string]any {
	var result []map[string]any
	if values, ok := schema["examples"].([]any); ok {
		for _, value := range values {
			result = append(result, value.(map[string]any))
		}
	}
	return result
}

func TestTaskArgumentsKeepStrictTextAndTypeBoundaries(t *testing.T) {
	for _, fork := range []bool{false, true} {
		t.Run(fmt.Sprintf("fork=%v", fork), func(t *testing.T) {
			root := queue(t, map[string]string{"t1": tasks.StateInProgress})
			outbox := t.TempDir()
			server := newServer(t, root, "t1")
			if fork {
				server.authority.ProposalOutbox = outbox
			}
			sess := newSession(t, server)
			state := map[string]any{"id": "t1", "status": "in progress", "done_so_far": "—", "next_action": "none", "traps": "—"}
			proposal := map[string]any{"kind": "task", "title": "A finding", "context": "c", "acceptance": "a", "approach": "p", "subtasks": []string{"s"}}
			before, outboxBefore := taskFiles(t, root), taskFiles(t, outbox)
			for _, tc := range []struct {
				field string
				value any
			}{
				{"next_action", nil}, {"next_action", ""}, {"next_action", "  "},
				{"next_action", "line\nbreak"}, {"next_action", "tab\tstop"},
				{"next_action", "carriage\rreturn"}, {"next_action", "\x00"}, {"next_action", "\x1b"},
				{"next_action", true}, {"next_action", 1}, {"next_action", []string{"none"}},
				{"next_action", strings.Repeat("é", blockLimit/2+1)}, {"unexpected", "private argument text"},
			} {
				args := maps.Clone(state)
				args[tc.field] = tc.value
				text := sess.mustRefuse("tasks_update_state", args)
				if strings.Contains(text, "missing required fields") || strings.Contains(text, "private argument text") {
					t.Fatalf("present invalid field misclassified or echoed: %s", text)
				}
			}
			for _, tc := range []struct {
				field string
				value any
			}{
				{"title", "—"}, {"title", strings.Repeat("é", tasks.TaskTitleLimit/2+1)},
				{"context", ""}, {"context", "\r"}, {"acceptance", nil}, {"acceptance", "  "},
				{"approach", strings.Repeat("é", blockLimit/2+1)}, {"kind", "epic"},
				{"subtasks", []string{}}, {"subtasks", make([]string, maxItems+1)},
				{"subtasks", []string{"s", "bad\nline"}}, {"subtasks", []string{strings.Repeat("é", lineLimit/2+1)}},
				{"subtasks", []any{1}}, {"unexpected", true},
			} {
				args := maps.Clone(proposal)
				args[tc.field] = tc.value
				text := sess.mustRefuse("tasks_propose", args)
				if strings.Contains(text, "fork task proposal") || strings.Contains(text, "missing required fields") {
					t.Fatalf("proposal lacks field-specific repair: %s", text)
				}
			}
			if !maps.Equal(before, taskFiles(t, root)) || !maps.Equal(outboxBefore, taskFiles(t, outbox)) {
				t.Fatal("invalid fields mutated task files or outbox")
			}
			// Exact Unicode byte limits and multiline control exceptions are accepted unchanged.
			state["status"] = strings.Repeat("é", blockLimit/2)
			state["done_so_far"] = "one\ntwo\tthree"
			sess.mustCall("tasks_update_state", state)
			subtasks := make([]string, maxItems)
			for i := range subtasks {
				subtasks[i] = "s"
			}
			subtasks[0] = strings.Repeat("é", lineLimit/2)
			proposal["title"] = strings.Repeat("é", tasks.TaskTitleLimit/2)
			proposal["context"] = strings.Repeat("é", blockLimit/2)
			proposal["subtasks"] = subtasks
			sess.mustCall("tasks_propose", proposal)
		})
	}
}

func TestTaskArgumentListBoundsAndNestedTypes(t *testing.T) {
	for _, name := range []string{"tasks_set_subtasks", "tasks_block"} {
		t.Run(name, func(t *testing.T) {
			root := queue(t, map[string]string{"t1": tasks.StateInProgress})
			sess := newSession(t, newServer(t, root, "t1"))
			field := "subtasks"
			args := map[string]any{"id": "t1"}
			item := any(map[string]any{"text": strings.Repeat("é", lineLimit/2)})
			if name == "tasks_block" {
				field = "options"
				args["decision"], args["recommendation"] = "A decision", "A"
				item = strings.Repeat("é", lineLimit/2)
			}
			before := taskFiles(t, root)
			for _, count := range []int{0, maxItems + 1} {
				items := make([]any, count)
				for i := range items {
					items[i] = item
				}
				args[field] = items
				sess.mustRefuse(name, args)
			}
			args[field] = nil
			sess.mustRefuse(name, args)
			if name == "tasks_set_subtasks" {
				for _, bad := range []map[string]any{
					{"done": true}, {"text": nil}, {"text": "x", "done": "true"}, {"text": "x", "done": nil},
					{"text": "x", "unknown": true}, {"text": strings.Repeat("é", lineLimit/2+1)},
				} {
					args[field] = []any{bad}
					sess.mustRefuse(name, args)
				}
			} else {
				args[field] = []string{"a\nb"}
				sess.mustRefuse(name, args)
			}
			if !maps.Equal(before, taskFiles(t, root)) {
				t.Fatal("refused list changed the task")
			}
			items := make([]any, maxItems)
			for i := range items {
				items[i] = item
			}
			args[field] = items
			sess.mustCall(name, args)
		})
	}
}

func TestTaskOptionalStringsRejectNullWithoutMutation(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	sess := newSession(t, newServer(t, root, "t1"))
	before := taskFiles(t, root)
	sess.mustRefuse("tasks_list", map[string]any{"state": nil})
	sess.mustRefuse("tasks_propose", map[string]any{
		"kind": "task", "title": "Null queue", "context": "c", "acceptance": "a",
		"approach": "p", "subtasks": []string{"s"}, "queue": nil,
	})
	if !maps.Equal(before, taskFiles(t, root)) {
		t.Fatal("null optional string changed the queue")
	}
}

func TestForkProposalRefusesUnimportableSerializedSize(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	outbox := t.TempDir()
	server := newServer(t, root, "t1")
	server.authority.ProposalOutbox = outbox
	sess := newSession(t, server)
	before := taskFiles(t, outbox)
	// Each block obeys its own raw byte limit; JSON escapes expand the aggregate.
	block := "x" + strings.Repeat("\t", blockLimit-1)
	text := sess.mustRefuse("tasks_propose", map[string]any{
		"kind": "task", "title": "Oversized proposal", "context": block,
		"acceptance": block, "approach": block, "subtasks": []string{"s"},
	})
	if !strings.Contains(text, "262144") || !maps.Equal(before, taskFiles(t, outbox)) {
		t.Fatalf("oversized proposal lacks limit feedback or wrote the outbox: %s", text)
	}
}

func TestTaskArgumentsPreserveShapeAndQueueRefusals(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	sess := newSession(t, newServer(t, root, "t1"))
	before := taskFiles(t, root)
	for _, raw := range []string{"null", "[]", "true", "3", `"text"`} {
		reply := sess.request("tools/call", map[string]any{"name": "tasks_update_state", "arguments": json.RawMessage(raw)})
		result := reply["result"].(map[string]any)
		if result["isError"] != true {
			t.Fatalf("non-object arguments accepted: %s", raw)
		}
	}
	sess.mustRefuse("tasks_propose", map[string]any{
		"kind": "task", "title": "Wrong queue", "context": "c", "acceptance": "a",
		"approach": "p", "subtasks": []string{"s"}, "queue": filepath.Join(t.TempDir(), "outside"),
	})
	if !maps.Equal(before, taskFiles(t, root)) {
		t.Fatal("invalid shape or queue changed the task")
	}
}

func taskFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			files[rel+"/"] = ""
			return nil
		}
		body, err := os.ReadFile(path)
		files[rel] = string(body)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestMissingArgumentsReportEveryFieldWithoutMutation(t *testing.T) {
	for _, fork := range []bool{false, true} {
		mode := "plain"
		if fork {
			mode = "fork"
		}
		t.Run(mode, func(t *testing.T) {
			root := queue(t, map[string]string{"t1": tasks.StateInProgress})
			outbox := t.TempDir()
			server := newServer(t, root, "t1")
			if fork {
				server.authority.ProposalOutbox = outbox
			}
			sess := newSession(t, server)
			before, outboxBefore := taskFiles(t, root), taskFiles(t, outbox)
			cases := []struct {
				name    string
				args    map[string]any
				missing string
			}{
				{"tasks_update_state", map[string]any{"id": "t1", "status": "in progress", "done_so_far": "private argument text"}, "next_action, traps"},
				{"tasks_propose", map[string]any{"kind": "task", "title": "Follow-up", "context": "private argument text"}, "acceptance, approach, subtasks"},
				{"tasks_block", map[string]any{"id": "t1"}, "decision, options, recommendation"},
			}
			for _, tc := range cases {
				first := sess.mustRefuse(tc.name, tc.args)
				if !strings.Contains(first, "missing required fields: "+tc.missing) || !strings.Contains(first, "resend the complete arguments object") {
					t.Fatalf("%s lacks all missing fields and repair: %s", tc.name, first)
				}
				if strings.Contains(first, "private argument text") {
					t.Fatalf("%s echoed submitted prose", tc.name)
				}
				if again := sess.mustRefuse(tc.name, tc.args); again != first {
					t.Fatalf("%s refusal order changed: %q != %q", tc.name, first, again)
				}
				if !maps.Equal(before, taskFiles(t, root)) || !maps.Equal(outboxBefore, taskFiles(t, outbox)) {
					t.Fatalf("%s mutated task files or outbox on refusal", tc.name)
				}
			}
			sess.mustCall("tasks_update_state", map[string]any{"id": "t1", "status": "in progress", "done_so_far": "Review finished", "next_action": "none", "traps": "—"})
			sess.mustCall("tasks_propose", map[string]any{"kind": "task", "title": "Follow-up", "context": "A separately scoped finding", "acceptance": "The focused regression fails before the fix and passes after", "approach": "Reproduce then fix the cause", "subtasks": []string{"Add the regression and run the gate"}})
			entries, err := os.ReadDir(outbox)
			if err != nil {
				t.Fatal(err)
			}
			proposed, err := os.ReadDir(filepath.Join(root, tasks.StateTodo))
			if err != nil {
				t.Fatal(err)
			}
			if fork && (len(entries) != 1 || len(proposed) != 0) || !fork && (len(entries) != 0 || len(proposed) != 1) {
				t.Fatalf("repair destinations: outbox=%d todo=%d fork=%v", len(entries), len(proposed), fork)
			}
		})
	}
}
