package preset

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ModelTarget is one entry in a lead's `models:` fallback ladder (and a fleet fork's) —
// written MODEL-first: a model, optionally pinned to one account. It's the only place a
// model+account pair appears in coop's config; the order is always model@credential.
//
//	claude-fable-5              # bare model → runs on EVERY signed-in account (rotating)
//	claude-opus-4-8@work       # pinned → that model on the "work" account only
//	{model: opus, credential: work}   # the same, spelled with keys
//
// A bare model (no Credential) fans out across all signed-in accounts at loop start — the
// clean-sheet replacement for the old rotation pool's zero-config "all accounts" default.
type ModelTarget struct {
	Model      string
	Credential string // "" = fan out across all signed-in accounts
}

func (t *ModelTarget) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		model, cred, hadAt := strings.Cut(n.Value, "@")
		t.Model, t.Credential = strings.TrimSpace(model), strings.TrimSpace(cred)
		if hadAt && t.Credential == "" {
			return fmt.Errorf("%q has an empty account after @ — use model@account, or drop the @", n.Value)
		}
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("a models entry is model (\"opus\", \"opus@work\") or {model: opus, credential: work}")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch key := n.Content[i].Value; key {
		case "model", "credential":
		default:
			return fmt.Errorf("models entry: unknown key %q (known: model, credential)", key)
		}
	}
	var m struct {
		Model      string `yaml:"model"`
		Credential string `yaml:"credential"`
	}
	if err := n.Decode(&m); err != nil {
		return err
	}
	t.Model, t.Credential = m.Model, m.Credential
	return nil
}

// validate checks one entry: a non-empty model with no whitespace/leading-dash, and — when
// pinned — a single path-safe account segment (it becomes a profile directory name).
func (t ModelTarget) validate() error {
	if t.Model == "" || strings.HasPrefix(t.Model, "-") || strings.ContainsAny(t.Model, " \t\r\n@") {
		return fmt.Errorf("has an invalid model %q — a model id has no spaces, no '@', and no leading '-'", t.Model)
	}
	if c := t.Credential; c != "" && (strings.ContainsAny(c, "/\\@") || c == "." || c == ".." || strings.HasPrefix(c, "-")) {
		return fmt.Errorf("has an invalid account %q — use a single segment (no '/', '..', or leading '-')", c)
	}
	return nil
}

// String renders the entry back in the model@account wire form (for `coop presets` + docs).
func (t ModelTarget) String() string {
	if t.Credential == "" {
		return t.Model
	}
	return t.Model + "@" + t.Credential
}
