package workerconnector

import (
	"encoding/json"
	"strings"
	"testing"
)

// HTTP traces kept the action while outbound traces dropped every path fact,
// leaving Responder unable to distinguish project edits from outside access.
func TestPublicActivityKeepsBoundedPathFactsWithoutRawHostContext(t *testing.T) {
	for _, kind := range []string{"tool.started", "tool.completed"} {
		raw := json.RawMessage(`{"kind":"read","status":"completed","title":"secret title","input":{"path":"/host/checkout/lib/a.go"},"path_context":{"basis":"lexical","root":"/host/checkout","paths":[{"source":"/locations/0/path","scope":"project","path":"lib/a.go","secret":"do not export"},{"source":"/input/path","scope":"outside","path":"/host/secret"}]}}`)
		payload, ok := publicActivityPayload(kind, raw)
		if !ok {
			t.Fatal("rejected a valid tool event")
		}
		var value map[string]any
		if err := json.Unmarshal(payload, &value); err != nil {
			t.Fatal(err)
		}
		if value["kind"] != "read" || !strings.Contains(string(payload), `"path":"lib/a.go"`) || !strings.Contains(string(payload), `"scope":"outside"`) {
			t.Fatalf("lost path context: %s", payload)
		}
		if strings.Contains(string(payload), "/host") || strings.Contains(string(payload), "secret") {
			t.Fatalf("exported forbidden host context: %s", payload)
		}
	}
}

func TestPublicActivityRejectsUntrustedPathMetadataShapes(t *testing.T) {
	for _, item := range []string{
		`{"source":"/locations/0/path","scope":"project","path":"../secret"}`,
		`{"source":"/locations/0/path","scope":"project","path":"/host/secret"}`,
		`{"source":"/locations/0/path","scope":"project","path":"C:\\secret"}`,
		`{"source":"/locations/0/path","scope":"project","path":"file:/host/secret"}`,
		`{"source":"/locations/0/path","scope":"project","path":"vscode-remote:host/secret"}`,
		`{"source":"/locations/0/path","scope":"project","path":"a/../secret"}`,
		`{"source":"/locations/0/path","scope":"project","path":"bad\npath"}`,
		`{"source":"/locations/0/path","scope":"project","path":"` + strings.Repeat("x", 513) + `"}`,
		`{"source":"/private/secret","scope":"project","path":"lib/a.go"}`,
		`{"source":"/locations/0/path","scope":"symlink_safe","path":"lib/a.go"}`,
	} {
		raw := json.RawMessage(`{"path_context":{"basis":"lexical","paths":[` + item + `]}}`)
		payload, ok := publicActivityPayload("tool.completed", raw)
		if !ok || strings.Contains(string(payload), `"path":`) {
			t.Fatalf("invalid path metadata crossed the boundary: %s", payload)
		}
	}
	paths := strings.Repeat(`{"source":"/input/path","scope":"project","path":"lib/a.go"},`, 17)
	raw := json.RawMessage(`{"path_context":{"basis":"lexical","paths":[` + strings.TrimSuffix(paths, ",") + `]}}`)
	payload, ok := publicActivityPayload("tool.completed", raw)
	if !ok || strings.Count(string(payload), `"path":"lib/a.go"`) != 16 || !strings.Contains(string(payload), `"partial":true`) {
		t.Fatalf("path metadata was not bounded honestly: %s", payload)
	}
}
