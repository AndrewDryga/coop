package workerconnector

import (
	"encoding/json"
	"strings"
	"testing"
)

// The record of a sent prompt reaches the controller, which rebuilds the
// exact text from its own prompt and schema (sessionsvc/prompt_record.go).
// Coop's text crosses exactly, or as its digest marked withheld: a correction
// can quote the answer it rejects, and that answer can hold a token.
func TestPromptRecordCrossesExactlyOrWithheldWithItsDigest(t *testing.T) {
	if !operatorActivityEvent("prompt.sent") {
		t.Fatal("prompt.sent does not reach the controller")
	}
	note := "\n\n<coop-output>Save only final generated images in .coop-output/turn-1.</coop-output>"
	token := "ghp_" + strings.Repeat("aB7C", 9)
	correction := "Your previous final response was rejected: \"" + token + "\" is not valid."
	record := map[string]any{
		"version": 1, "state": "sent", "text_sha256": sha256Text("x"), "text_bytes": 1,
		"parts": []any{
			map[string]any{"kind": "caller_prompt", "sha256": sha256Text("prompt"), "bytes": 6},
			map[string]any{"kind": "text", "text": correction, "sha256": sha256Text(correction), "bytes": len(correction)},
			map[string]any{"kind": "output_schema", "sha256": sha256Text("{}"), "bytes": 2},
			map[string]any{"kind": "text", "text": note, "sha256": sha256Text(note), "bytes": len(note)},
		},
		"blocks": []any{
			map[string]any{"type": "image", "name": "chart.png", "media_type": "image/png", "sha256": sha256Text("png"), "bytes": 3, "data": "cG5n"},
		},
	}
	encoded, _ := json.Marshal(record)
	value := decodeNarration(t, "prompt.sent", string(encoded))
	parts, _ := value["parts"].([]any)
	if len(parts) != 4 {
		t.Fatalf("parts = %v", value["parts"])
	}
	withheld := parts[1].(map[string]any)
	if withheld["kind"] != "withheld" || withheld["reason"] != "likely GitHub token" ||
		withheld["sha256"] != sha256Text(correction) || withheld["text"] != nil {
		t.Fatalf("a correction quoting a token crossed: %v", withheld)
	}
	if parts[3].(map[string]any)["text"] != note {
		t.Fatalf("Coop's note did not cross exactly: %v", parts[3])
	}
	if strings.Contains(publicJSON(t, value), token) {
		t.Fatal("the token crossed")
	}
	block := value["blocks"].([]any)[0].(map[string]any)
	if block["data"] != nil || block["name"] != "chart.png" || block["bytes"] != float64(3) {
		t.Fatalf("an attachment crossed as more than its identity: %v", block)
	}

	// A text part whose digest does not match its text is not a record of what was sent.
	record["parts"] = []any{map[string]any{"kind": "text", "text": note, "sha256": sha256Text("other"), "bytes": len(note)}}
	encoded, _ = json.Marshal(record)
	if _, ok := publicActivityPayload("prompt.sent", encoded); ok {
		t.Fatal("a record whose text does not match its digest crossed")
	}
}

func publicJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
