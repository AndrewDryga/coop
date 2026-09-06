package sessionsvc

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestActivityPathCompletionDoesNotRewriteTheStartedEvent(t *testing.T) {
	store, sess := newActivityTestStore(t)
	activity := newSessionActivity(store, sess, "turn-path")
	activity.observe(json.RawMessage(`{"update":{"sessionUpdate":"tool_call","toolCallId":"read-1","title":"Read file 'misleading/path'","kind":"read","status":"in_progress"}}`))
	frame, _ := json.Marshal(map[string]any{"update": map[string]any{
		"sessionUpdate": "tool_call_update", "toolCallId": "read-1", "status": "completed",
		"locations": []map[string]any{{"path": filepath.Join(sess.Workspace, "lib", "a.go"), "line": 4}},
	}})
	activity.observe(frame)
	activity.close(context.Background())
	events := activityEvents(t, store, sess.ID)
	if len(events) != 2 {
		t.Fatalf("events = %d", len(events))
	}
	if _, exists := activityPayload(t, events[0])["path_context"]; exists {
		t.Fatal("invented a path from the starting title")
	}
	encoded, _ := json.Marshal(activityPayload(t, events[1])["path_context"])
	var paths activityPaths
	if err := json.Unmarshal(encoded, &paths); err != nil {
		t.Fatal(err)
	}
	if paths.Basis != "lexical" || len(paths.Paths) != 1 || paths.Paths[0].Path != "lib/a.go" {
		t.Fatalf("completion lost late path evidence: %s", encoded)
	}
	response := sessionHTTPTestRequest(t, NewHTTPHandler(&Service{store: store}), http.MethodGet,
		"/v1/sessions/"+sess.ID+"/events?after=0&limit=100", "", "", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"path_context":{"basis":"lexical","paths":[{"source":"/locations/0/path","path":"lib/a.go","scope":"project"}]}`) {
		t.Fatalf("events endpoint lost project-relative path metadata: %d %s", response.Code, response.Body.String())
	}
}

// Codex ACP's edit content carries full oldText/newText. Preview truncation
// used to erase the short path too, hiding even an explicitly outside edit.
func TestActivityEditPathsSurviveLargeDiffPreviews(t *testing.T) {
	store, sess := newActivityTestStore(t)
	activity := newSessionActivity(store, sess, "turn-large-edit")
	frame, _ := json.Marshal(map[string]any{"update": map[string]any{
		"sessionUpdate": "tool_call", "toolCallId": "edit-1", "kind": "edit", "status": "in_progress",
		"content": []map[string]any{
			{"type": "diff", "path": filepath.Join(sess.Workspace, "lib", "a.go"), "oldText": strings.Repeat("a", 9<<10), "newText": strings.Repeat("b", 9<<10)},
			{"type": "diff", "path": filepath.Join(sess.Workspace, "..", "outside.go"), "oldText": "", "newText": "x"},
		},
	}})
	activity.observe(frame)
	activity.observe(json.RawMessage(`{"update":{"sessionUpdate":"tool_call_update","toolCallId":"edit-1","status":"completed"}}`))
	activity.close(context.Background())
	for _, event := range activityEvents(t, store, sess.ID) {
		encoded, _ := json.Marshal(activityPayload(t, event)["path_context"])
		var paths activityPaths
		if err := json.Unmarshal(encoded, &paths); err != nil || len(paths.Paths) != 2 || paths.Paths[0].Path != "lib/a.go" || paths.Paths[1].Scope != "outside" || paths.Partial {
			t.Fatalf("large edit lost its path facts: %s", encoded)
		}
	}
	response := sessionHTTPTestRequest(t, NewHTTPHandler(&Service{store: store}), http.MethodGet,
		"/v1/sessions/"+sess.ID+"/events?after=0&limit=100", "", "", "")
	if response.Code != http.StatusOK || strings.Count(response.Body.String(), `"path":"lib/a.go"`) != 2 || !strings.Contains(response.Body.String(), `"truncated":true`) {
		t.Fatalf("HTTP evidence lost paths or silently truncated the diff: %d", response.Code)
	}
}

// Responder could only show a worker's entire checkout path or guess its root.
// These are lexical presentation facts, never filesystem containment authority.
func TestActivityPathsAreRelativeToTheBoundWorkspace(t *testing.T) {
	for _, tc := range []struct{ path, scope, display string }{
		{"/workspace/project/lib/a.go", "project", "lib/a.go"},
		{"lib/a.go", "project", "lib/a.go"},
		{"lib/../a.go", "project", "a.go"},
		{"/workspace/project-other/a.go", "outside", ""},
		{"../other/a.go", "outside", ""},
		{`C:\\project\\a.go`, "unknown", ""},
		{`\\server\share\a.go`, "unknown", ""},
		{"~/a.go", "unknown", ""},
		{"bad\x00path", "unknown", ""},
		{"file:///host/checkout/a.go", "unknown", ""},
		{"file:/host/checkout/a.go", "unknown", ""},
		{"./file:///host/checkout/a.go", "unknown", ""},
		{"vscode-remote://host/checkout/a.go", "unknown", ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			input, _ := json.Marshal(map[string]any{"path": tc.path})
			got := activityPathContext("/workspace/project", "read", input, nil, nil)
			if got.Basis != "lexical" || len(got.Paths) != 1 || got.Paths[0].Scope != tc.scope || got.Paths[0].Path != tc.display {
				t.Fatalf("path context = %+v; want %s %q", got, tc.scope, tc.display)
			}
			encoded, _ := json.Marshal(got)
			if strings.Contains(string(encoded), "/workspace") {
				t.Fatal("path metadata exported the host workspace")
			}
		})
	}
}

func TestActivityPathsUseStructuredEvidenceNotCommandsOrMCPArguments(t *testing.T) {
	input := json.RawMessage(`{"path":"lib/input.go","command":"cat /workspace/project/secret"}`)
	locations := json.RawMessage(`[{"path":"/workspace/project/lib/location.go","line":12}]`)
	content := json.RawMessage(`[{"type":"diff","path":"lib/diff.go","oldText":"","newText":"x"}]`)
	got := activityPathContext("/workspace/project", "edit", input, locations, content)
	if len(got.Paths) != 3 {
		t.Fatalf("missing structured paths: %+v", got)
	}
	for i, want := range []string{"/input/path", "/locations/0/path", "/content/0/path"} {
		if got.Paths[i].Source != want || got.Paths[i].Scope != "project" {
			t.Fatalf("path %d = %+v", i, got.Paths[i])
		}
	}
	for _, raw := range []string{`{"server":"remote","arguments":{"path":"/workspace/project/no"}}`, `{"command":"cat /workspace/project/no"}`, `{"truncated":true,"preview":"/workspace/project/no"}`} {
		if got := activityPathContext("/workspace/project", "execute", json.RawMessage(raw), nil, nil); len(got.Paths) != 0 {
			t.Fatalf("invented path from non-filesystem input: %+v", got)
		}
	}
}

func TestActivityPathMetadataIsBoundedAndMissingRootsStayUnknown(t *testing.T) {
	paths := make([]map[string]string, 100)
	for i := range paths {
		paths[i] = map[string]string{"path": "lib/a.go"}
	}
	locations, _ := json.Marshal(paths)
	got := activityPathContext("/workspace/project", "read", nil, locations, nil)
	if len(got.Paths) != 16 || !got.Partial {
		t.Fatalf("unbounded or silent truncation: %+v", got)
	}
	got = activityPathContext("", "read", json.RawMessage(`{"file_path":"lib/a.go"}`), nil, nil)
	if len(got.Paths) != 1 || got.Paths[0].Scope != "unknown" {
		t.Fatalf("guessed missing root: %+v", got)
	}
}
