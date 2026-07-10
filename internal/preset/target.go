package preset

// ModelTarget is one rung of a lead's rotation ladder — a provider, a model, optionally pinned
// to one account. It's built only programmatically (from the lead's agent: target-ladder via
// leadLadder, and the loop's one-off targets); the YAML `models:` key that decoded straight into
// it is retired — the model rides agent: as a provider:model@account target. Provider "" means
// "the run's default agent" (a one-off ladder inherits the positional's provider); a cross-
// provider lead ladder sets it per rung. A bare model (no Credential) fans out across all
// signed-in accounts at loop start.
type ModelTarget struct {
	Provider   string // "" = the run's default agent (one-off); set per rung for a cross-provider ladder
	Model      string
	Effort     string // "" = the agent's own default; else a reasoning-effort level (see agents.Target.Effort)
	Credential string // "" = fan out across all signed-in accounts
}

// String renders the rung in the [provider:]model[/effort][@account] wire form (for `coop
// presets` + docs). The provider is shown only when it differs from the ladder's lead (a
// cross-provider rung), so a same-provider ladder stays terse.
func (t ModelTarget) String() string {
	m := t.Model
	if t.Provider != "" {
		if t.Model != "" {
			m = t.Provider + ":" + t.Model
		} else {
			m = t.Provider
		}
	}
	if t.Effort != "" {
		m += "/" + t.Effort
	}
	if t.Credential == "" {
		return m
	}
	return m + "@" + t.Credential
}
