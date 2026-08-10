package acpctl

import (
	"encoding/json"
	"testing"
)

// TestParseGeminiModels: the ACP session/new `models` field → cache entries, blank ids skipped.
// Moved from internal/cli/modelscache_test.go with ParseGeminiModels (models.go).
func TestParseGeminiModels(t *testing.T) {
	models := json.RawMessage(`{"currentModelId":"gemini-3.5-flash","availableModels":[
	  {"modelId":"gemini-3.5-flash","name":"Flash"},
	  {"modelId":"gemini-2.5-pro","name":"Pro"},
	  {"modelId":"","name":"Blank"}
	]}`)
	got := ParseGeminiModels(models)
	if len(got) != 2 || got[0].ID != "gemini-3.5-flash" || got[0].Name != "Flash" || got[1].ID != "gemini-2.5-pro" {
		t.Fatalf("ParseGeminiModels = %v", got)
	}
	if ParseGeminiModels(nil) != nil {
		t.Error("absent models field → nil")
	}
}

// TestParseClaudeModelOption: the claude configOptions `model` select → cache entries, blank
// values skipped. Moved from internal/cli/modelscache_test.go with ParseClaudeModelOption (models.go).
func TestParseClaudeModelOption(t *testing.T) {
	opts := []Option{
		{Value: "opus[1m]", Name: "Opus"},
		{Value: "sonnet", Name: "Sonnet"},
		{Value: "", Name: "Blank"},
	}
	got := ParseClaudeModelOption(opts)
	if len(got) != 2 || got[0].ID != "opus[1m]" || got[0].Name != "Opus" || got[1].ID != "sonnet" {
		t.Fatalf("ParseClaudeModelOption = %v", got)
	}
}
