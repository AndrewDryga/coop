package agent

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestModelCatalogDescriptors(t *testing.T) {
	// Every catalog comes from the pinned client in a box: a host CLI may be missing or another version.
	for _, name := range Names() {
		ag, _ := Get(name)
		spec := ag.ModelCatalog()
		if spec.ParseACP == nil {
			t.Errorf("%s has no catalog source in the box", name)
		}
		if (spec.ACPAuthMethod != nil) != (name == "grok") {
			t.Errorf("%s unexpected broker bootstrap declaration", name)
		}
	}
}

// Claude's adapter lists most families by alias but Fable by its full id; the menu lists every
// family by the alias people type. A family the catalog already names by alias keeps its other ids,
// as does a variant such as opus[1m], and a provider without aliases lists its catalog unchanged.
func TestModelCatalogListsFamiliesByAlias(t *testing.T) {
	claude, _ := Get("claude")
	for _, tc := range []struct{ ids, want []string }{
		{[]string{"sonnet", "claude-fable-5-1", "opus", "haiku"}, []string{"sonnet", "fable", "opus", "haiku"}},
		{[]string{"opus[1m]", "claude-fable-5-1", "claude-fable-5", "opus", "claude-opus-4-8"},
			[]string{"opus[1m]", "fable", "claude-fable-5", "opus", "claude-opus-4-8"}},
	} {
		if got := claude.ModelCatalog().Listed(tc.ids); !slices.Equal(got, tc.want) {
			t.Errorf("Listed(%v) = %v, want %v", tc.ids, got, tc.want)
		}
	}
	if !slices.Equal(claude.Models()[:4], claude.ModelCatalog().Aliases) {
		t.Errorf("the bundled examples do not lead with the aliases: %v", claude.Models())
	}
	grok, _ := Get("grok")
	if ids := []string{"grok-4.7", "grok-4.5"}; !slices.Equal(grok.ModelCatalog().Listed(ids), ids) {
		t.Error("a provider without aliases had its catalog rewritten")
	}
}

func TestGrokCatalogRequiresAdvertisedExternalHelper(t *testing.T) {
	for _, test := range []struct {
		name, raw string
		allowed   bool
	}{
		{"native broker", `{"authMethods":[{"id":"cached_token"},{"id":"grok.com","_meta":{"external_provider":true}}]}`, true},
		{"no methods", `{}`, false},
		{"device sign-in", `{"authMethods":[{"id":"grok.com","_meta":{"external_provider":false}}]}`, false},
		{"missing marker", `{"authMethods":[{"id":"grok.com"}]}`, false},
		{"wrong method", `{"authMethods":[{"id":"cached_token","_meta":{"external_provider":true}}]}`, false},
		{"string marker", `{"authMethods":[{"id":"grok.com","_meta":{"external_provider":"true"}}]}`, false},
		{"duplicate method", `{"authMethods":[{"id":"grok.com","_meta":{"external_provider":true}},{"id":"grok.com","_meta":{"external_provider":true}}]}`, false},
		{"malformed metadata", `{"authMethods":[{"id":"grok.com","_meta":12}]}`, false},
		{"malformed JSON", `{"authMethods":`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			method, err := grokCatalogAuthMethod(json.RawMessage(test.raw))
			if (err == nil) != test.allowed || test.allowed && method != "grok.com" || !test.allowed && method != "" {
				t.Fatalf("method=%q error=%v allowed=%v", method, err, test.allowed)
			}
		})
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

// Codex's catalog is codex-acp's "model" config option, as an editor session shows it.
func TestCodexCatalogReadsTheModelOption(t *testing.T) {
	codex, _ := Get("codex")
	raw := json.RawMessage(`{"sessionId":"s","configOptions":[
	  {"id":"reasoning_effort","options":[{"value":"low","name":"Low"}]},
	  {"id":"model","currentValue":"gpt-6-luna","options":[{"value":"gpt-6-astra","name":"GPT-6-Astra"},{"value":""},{"value":"gpt-6-luna","name":"GPT-6-Luna"}]}]}`)
	got := codex.ModelCatalog().ParseACP(raw)
	if len(got) != 2 || got[0] != (Model{ID: "gpt-6-astra", Name: "GPT-6-Astra"}) || got[1].ID != "gpt-6-luna" {
		t.Fatalf("codex catalog = %v", got)
	}
	if got := codex.ModelCatalog().ParseACP(json.RawMessage(`{"configOptions":[{"id":"mode","options":[{"value":"x"}]}]}`)); len(got) != 0 {
		t.Fatalf("a result without a model option gave %v", got)
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
