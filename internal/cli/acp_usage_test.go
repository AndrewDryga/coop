package cli

import (
	"encoding/json"
	"testing"
)

// Adapters and raw provider streams spell these differently, and the first
// version of this parsed only the stream-json names against an ACP result — so
// it matched nothing and recorded four zeros for every turn while looking
// entirely wired.
func TestACPUsageAcceptsBothSpellings(t *testing.T) {
	// claude-agent-acp and codex-acp: camelCase, cache split in two.
	var camel acpUsage
	if err := json.Unmarshal([]byte(`{
	  "inputTokens": 1200, "outputTokens": 340,
	  "cachedReadTokens": 800, "cachedWriteTokens": 100, "thoughtTokens": 90
	}`), &camel); err != nil {
		t.Fatal(err)
	}
	got := camel.session()
	if got.InputTokens != 1200 || got.OutputTokens != 340 || got.ReasoningTokens != 90 {
		t.Errorf("camelCase usage was dropped: %+v", got)
	}
	// Reads and writes sum: session.Usage separates cached from fresh input
	// because providers price those differently, and splits no further.
	if got.CachedInputTokens != 900 {
		t.Errorf("cached total = %d, want 900", got.CachedInputTokens)
	}

	// Codex stream-json: snake_case.
	var snake acpUsage
	if err := json.Unmarshal([]byte(`{
	  "input_tokens": 50, "output_tokens": 20,
	  "cached_input_tokens": 10, "reasoning_output_tokens": 5
	}`), &snake); err != nil {
		t.Fatal(err)
	}
	if got := snake.session(); got.InputTokens != 50 || got.CachedInputTokens != 10 ||
		got.OutputTokens != 20 || got.ReasoningTokens != 5 {
		t.Errorf("snake_case usage was dropped: %+v", got)
	}

	// An adapter that reports nothing must stay unrecorded rather than
	// becoming a measured zero.
	var absent *acpUsage
	if absent.session().Recorded() {
		t.Error("a missing usage object reports itself as measured")
	}
}
