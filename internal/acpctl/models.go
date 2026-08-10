package acpctl

import "encoding/json"

// Model is one model in the cache: the id the agent's CLI accepts (what --model takes), plus an
// optional friendly display name. Moved from internal/cli/modelscache.go — a pure DTO the control
// caches opportunistically (cacheModels) and cli's `coop models` reads back (internal/cli's
// modelscache.go, which imports this type for its own remaining parsers).
type Model struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// ParseGeminiModels reads a gemini ACP session/new `models` field
// ({availableModels:[{modelId,name}], currentModelId}) into cache entries — the same shape
// synthModelOption renders as the toolbar dropdown. Empty/absent → nil.
func ParseGeminiModels(models json.RawMessage) []Model {
	if len(models) == 0 {
		return nil
	}
	var m struct {
		AvailableModels []struct {
			ModelID string `json:"modelId"`
			Name    string `json:"name"`
		} `json:"availableModels"`
	}
	if json.Unmarshal(models, &m) != nil {
		return nil
	}
	out := make([]Model, 0, len(m.AvailableModels))
	for _, am := range m.AvailableModels {
		if am.ModelID == "" {
			continue
		}
		out = append(out, Model{ID: am.ModelID, Name: am.Name})
	}
	return out
}

// ParseClaudeModelOption reads a claude configOptions `model` select (options:[{value,name}])
// into cache entries — the ids coop already offers in the ACP toolbar.
func ParseClaudeModelOption(options []Option) []Model {
	out := make([]Model, 0, len(options))
	for _, o := range options {
		if o.Value == "" {
			continue
		}
		out = append(out, Model{ID: o.Value, Name: o.Name})
	}
	return out
}
