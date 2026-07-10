package preset

// ModelTarget is one rung of a lead's rotation ladder — a model, optionally pinned to one
// account (model-first, model@credential wire order). It's now built only programmatically
// (from the lead's agent: target-ladder via leadLadder, and the loop's one-off targets); the
// YAML `models:` key that decoded straight into it is retired — the model rides agent: as a
// provider:model@account target. A bare model (no Credential) fans out across all signed-in
// accounts at loop start.
type ModelTarget struct {
	Model      string
	Credential string // "" = fan out across all signed-in accounts
}

// String renders the rung in the model@account wire form (for `coop presets` + docs).
func (t ModelTarget) String() string {
	if t.Credential == "" {
		return t.Model
	}
	return t.Model + "@" + t.Credential
}
