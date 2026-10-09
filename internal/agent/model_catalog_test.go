package agent

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestModelCatalogDescriptors(t *testing.T) {
	want := map[string][]string{"claude": nil, "gemini": nil, "codex": {"codex", "debug", "models"}, "grok": nil}
	for _, name := range Names() {
		ag, _ := Get(name)
		spec := ag.ModelCatalog()
		command, covered := want[name]
		if !covered || !slices.Equal(spec.HostCommand, command) || (spec.ParseHost != nil) != (len(command) > 0) || (spec.ParseACP != nil) != (len(command) == 0) {
			t.Errorf("%s catalog descriptor = %+v, covered=%v", name, spec, covered)
		}
	}
}

func TestModelCatalogMalformedACPParity(t *testing.T) {
	const mixed = `{"options":[{"value":"opus","name":"Opus"},{"value":12},{"value":"sonnet","name":"Sonnet"}]}`
	if got := ParseACPModelOption(json.RawMessage(mixed)); len(got) != 2 || got[0].ID != "opus" || got[1].ID != "sonnet" {
		t.Fatalf("opportunistic partial decode lost valid entries: %v", got)
	}
	for _, provider := range []string{"claude", "gemini", "grok"} {
		ag, _ := Get(provider)
		for _, malformed := range []string{
			`{"id":"other","options":[{"value":12}]}`,
			`{"id":"other","options":[{"value":"ok","description":12}]}`,
			`{"id":12,"options":[]}`,
		} {
			raw := `{"models":{"availableModels":[{"modelId":"gemini-live"}]},"configOptions":[{"id":"model","options":[{"value":"opus"}]},` + malformed + `]}`
			if got := ag.ModelCatalog().ParseACP(json.RawMessage(raw)); got != nil {
				t.Errorf("%s accepted malformed full catalog %s: %v", provider, malformed, got)
			}
		}
	}
}

// Grok 1.0.44's native session/new capture advertises the account catalog through models.
func TestGrokACPModelCatalog(t *testing.T) {
	ag, _ := Get("grok")
	raw := json.RawMessage(`{"sessionId":"native","models":{"currentModelId":"grok-4.5","availableModels":[{"modelId":"grok-4.6","name":"Grok 4.6"},{"modelId":"grok-4.5","name":"Grok 4.5"}]}}`)
	got := ag.ModelCatalog().ParseACP(raw)
	if len(got) != 2 || got[0].ID != "grok-4.6" || got[0].Name != "Grok 4.6" || got[1].ID != "grok-4.5" {
		t.Fatalf("native catalog = %v", got)
	}
	for _, invalid := range []string{`{}`, `{"models":{"availableModels":[{"modelId":5}]}}`, `not json`} {
		if got := ag.ModelCatalog().ParseACP(json.RawMessage(invalid)); len(got) != 0 {
			t.Fatalf("invalid result accepted: %v", got)
		}
	}
}

// TestParseCodexModels: only visibility=="list" models survive, slug + display_name captured,
// a missing display_name falls back to the slug.
func TestParseCodexModels(t *testing.T) {
	out := `{"models":[
	  {"slug":"gpt-5.5","display_name":"GPT-5.5","visibility":"list"},
	  {"slug":"gpt-5.4","display_name":"","visibility":"list"},
	  {"slug":"internal-only","display_name":"Hidden","visibility":"hidden"},
	  {"slug":"","display_name":"Blank","visibility":"list"}
	]}`
	got, err := parseCodexModels([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("parseCodexModels = %v, want only the two list-visible, non-blank models", got)
	}
	if got[0].ID != "gpt-5.5" || got[0].Name != "GPT-5.5" {
		t.Errorf("model 0 = %+v", got[0])
	}
	if got[1].ID != "gpt-5.4" || got[1].Name != "gpt-5.4" {
		t.Errorf("model 1 = %+v, want display_name to fall back to the slug", got[1])
	}
	if _, err := parseCodexModels([]byte("not json")); err == nil {
		t.Error("malformed JSON must error (→ caller keeps static)")
	}
}

// TestParseACPAvailableModels: the ACP session/new `models` field → cache entries, blank ids skipped.
func TestParseACPAvailableModels(t *testing.T) {
	models := json.RawMessage(`{"currentModelId":"gemini-3.5-flash","availableModels":[
	  {"modelId":"gemini-3.5-flash","name":"Flash"},
	  {"modelId":"gemini-2.5-pro","name":"Pro"},
	  {"modelId":"","name":"Blank"}
	]}`)
	got := ParseACPAvailableModels(models)
	if len(got) != 2 || got[0].ID != "gemini-3.5-flash" || got[0].Name != "Flash" || got[1].ID != "gemini-2.5-pro" {
		t.Fatalf("ParseACPAvailableModels = %v", got)
	}
	if ParseACPAvailableModels(nil) != nil {
		t.Error("absent models field → nil")
	}
}

// TestParseACPModelOption: a configOptions model select becomes cache entries, skipping blank ids.
func TestParseACPModelOption(t *testing.T) {
	opts := json.RawMessage(`{"options":[{"value":"opus[1m]","name":"Opus"},{"value":"sonnet","name":"Sonnet"},{"value":"","name":"Blank"}]}`)
	got := ParseACPModelOption(opts)
	if len(got) != 2 || got[0].ID != "opus[1m]" || got[0].Name != "Opus" || got[1].ID != "sonnet" {
		t.Fatalf("ParseACPModelOption = %v", got)
	}
}
