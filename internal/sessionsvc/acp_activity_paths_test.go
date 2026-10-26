package sessionsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
)

func TestActivityPathsFollowTheACPWorkingDirectory(t *testing.T) {
	// Production agents read /workspace, but recording against the host checkout
	// falsely called those reads outside. Host identity must still stay private.
	for _, mode := range []string{"fresh", "resumed", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newSessionACPFixture(t, "activity-paths")
			wantRoot := StableWorkdir
			if mode == "legacy" {
				bound, err := fixture.store.BindNativeSession(context.Background(), fixture.session.ID, "native-1")
				if err != nil {
					t.Fatal(err)
				}
				fixture.session = bound
				wantRoot = bound.Workspace
			} else if fixture.session.Workspace == wantRoot {
				t.Fatal("fixture must distinguish the host checkout from the in-box cwd")
			}
			turns := 1
			if mode == "resumed" {
				turns = 2
			}
			for index := range turns {
				turn := fixture.submit(t, fmt.Sprintf("inspect the checkout, turn %d", index))
				result, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn)
				if err != nil || result.State != session.TurnCompleted {
					t.Fatalf("run = %+v, %v", result, err)
				}
				fixture.session, err = fixture.store.GetSession(context.Background(), fixture.session.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			method := "session/new"
			if mode != "fresh" {
				method = "session/load"
			}
			if got := sessionNewParamsForMethod(t, fixture.childLog, method)["cwd"]; got != wantRoot {
				t.Fatalf("%s cwd = %v, want %s", method, got, wantRoot)
			}
			events := activityEvents(t, fixture.store, fixture.session.ID)
			if len(events) != turns*10 {
				t.Fatalf("activity events = %d, want %d", len(events), turns*10)
			}
			for _, event := range events {
				payload := activityPayload(t, event)
				var paths activityPaths
				encoded, _ := json.Marshal(payload["path_context"])
				if err := json.Unmarshal(encoded, &paths); err != nil {
					t.Fatal(err)
				}
				id, _ := payload["tool_call_id"].(string)
				wantScope, wantPath := "project", "lib/a.go"
				if id == "outside" {
					wantScope, wantPath = "outside", ""
				}
				if paths.Basis != "lexical" || len(paths.Paths) != 1 || paths.Paths[0].Scope != wantScope || paths.Paths[0].Path != wantPath {
					t.Errorf("%s %s path context = %s, want %s %q", event.Type, id, encoded, wantScope, wantPath)
				}
				// Host identity must still be removed from embedded output. The
				// public /workspace alias is only the classification root.
				if payload["checkout_root"] != fixture.session.Workspace {
					t.Errorf("%s lost its host privacy root: %v", event.Type, payload["checkout_root"])
				}
			}
		})
	}
}

func sendACPActivityPaths(send func(any), nativeID, cwd string) {
	for _, tool := range []struct {
		id, kind string
		fields   map[string]any
	}{
		{"input", "read", map[string]any{"rawInput": map[string]any{"path": filepath.Join(cwd, "lib/a.go")}}},
		{"location", "read", map[string]any{"locations": []map[string]any{{"path": filepath.Join(cwd, "lib/a.go")}}}},
		{"diff", "edit", map[string]any{"content": []map[string]any{{"type": "diff", "path": filepath.Join(cwd, "lib/a.go"), "oldText": "a", "newText": "b"}}}},
		{"relative", "read", map[string]any{"rawInput": map[string]any{"path": "lib/a.go"}}},
		{"outside", "read", map[string]any{"rawInput": map[string]any{"path": cwd + "-other/a.go"}}},
	} {
		for _, status := range []string{"in_progress", "completed"} {
			update := map[string]any{"sessionUpdate": "tool_call", "toolCallId": tool.id, "kind": tool.kind, "title": "Inspect file", "status": status}
			if status == "completed" {
				update["sessionUpdate"] = "tool_call_update"
			} else {
				for key, value := range tool.fields {
					update[key] = value
				}
			}
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": nativeID, "update": update}})
		}
	}
}
