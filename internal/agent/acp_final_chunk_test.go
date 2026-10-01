package agent

import (
	"encoding/json"
	"testing"
)

// Only codex-acp streams progress commentary through the assistant chunk event; its phase marker
// decides, and every other adapter (or a chunk with no marker) is answer text.
func TestACPFinalChunk(t *testing.T) {
	codex, _ := Get("codex")
	for meta, want := range map[string]bool{
		``:   true,
		`{}`: true,
		`{"jetbrains":{"air":{"version":1,"phase":"final_answer"}}}`: true,
		`{"jetbrains":{"air":{"version":1,"phase":"commentary"}}}`:   false,
		`{"jetbrains":{"air":{"version":1,"phase":"reasoning"}}}`:    false,
		`not json`:                         true,
		`{"other":{"phase":"commentary"}}`: true,
	} {
		if got := codex.ACPFinalChunk(json.RawMessage(meta)); got != want {
			t.Errorf("codex ACPFinalChunk(%s) = %v, want %v", meta, got, want)
		}
		if got := codex.ACPProgressChunk(json.RawMessage(meta)); got != (meta == `{"jetbrains":{"air":{"version":1,"phase":"commentary"}}}`) {
			t.Errorf("progress classification must exclude final/private/unknown phases: %s", meta)
		}
	}
	for _, name := range Names() {
		if name == "codex" {
			continue
		}
		a, _ := Get(name)
		if !a.ACPFinalChunk(json.RawMessage(`{"jetbrains":{"air":{"version":1,"phase":"commentary"}}}`)) {
			t.Errorf("%s: ACPFinalChunk rejected a chunk — only codex has a commentary phase", name)
		}
	}
}
