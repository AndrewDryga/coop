package agent

import (
	"encoding/json"
	"slices"
	"strings"
)

// Model is a native CLI model id and its optional human-readable name.
type Model struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// ModelCatalogSpec selects exactly one source. Host commands are run by the caller;
// ACP parsers receive the complete session/new result from its scoped probe.
type ModelCatalogSpec struct {
	HostCommand []string
	ParseHost   func([]byte) ([]Model, error)
	ParseACP    func(json.RawMessage) []Model
	// ACPAuthMethod selects only a noninteractive broker bootstrap proven by initialize.
	ACPAuthMethod func(json.RawMessage) (string, error)
	// Aliases are family names the CLI resolves to that family's current model (Claude's opus).
	Aliases []string
}

// Listed is a catalog's ids as a menu offers them. A catalog can name one family by its alias and
// another only by a full id (Claude's adapter lists sonnet but claude-fable-5-1), so an id of a
// family whose alias the catalog lacks reads as that alias too. One id per family takes it; any
// other keeps its full id.
func (s ModelCatalogSpec) Listed(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		for _, alias := range s.Aliases {
			if slices.Contains(strings.Split(id, "-"), alias) && !slices.Contains(ids, alias) && !slices.Contains(out, alias) {
				id = alias
				break
			}
		}
		out = append(out, id)
	}
	return out
}

// Preserve complete session-result decoding for forced discovery, including
// malformed option metadata unrelated to the selected provider's catalog shape.
type acpModelCatalog struct {
	Models        json.RawMessage `json:"models"`
	ConfigOptions []struct {
		ID      string               `json:"id"`
		Options []catalogOptionValue `json:"options"`
	} `json:"configOptions"`
}

type catalogOptionValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ParseACPAvailableModels reads the protocol's models field. This is also used
// opportunistically by ACP control, regardless of a provider's discovery command.
func ParseACPAvailableModels(models json.RawMessage) []Model {
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
		if am.ModelID != "" {
			out = append(out, Model{ID: am.ModelID, Name: am.Name})
		}
	}
	return out
}

// ParseACPModelOption reads one protocol model select, not the entire options array.
func ParseACPModelOption(raw json.RawMessage) []Model {
	var option struct {
		Options []catalogOptionValue `json:"options"`
	}
	// Opportunistic control has always retained valid entries from a partial decode.
	_ = json.Unmarshal(raw, &option)
	return modelsFromOptions(option.Options)
}

func modelsFromOptions(options []catalogOptionValue) []Model {
	out := make([]Model, 0, len(options))
	for _, o := range options {
		if o.Value != "" {
			out = append(out, Model{ID: o.Value, Name: o.Name})
		}
	}
	return out
}
