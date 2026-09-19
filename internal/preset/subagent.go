package preset

import "strings"

// SubagentName is the name the lead invokes a native role by: the referenced subagent when
// the role pins one (`subagent: deep-reasoner`), else the generated `coop-<role>`.
func SubagentName(r *Role) string {
	if r.Subagent != "" {
		return r.Subagent
	}
	return "coop-" + r.Name
}

// GeneratedNativeRoles returns the native roles Coop generates for the lead: every native role but
// one that references an existing subagent (`subagent:`), which the lead's client already has.
func (p *Preset) GeneratedNativeRoles() []Role {
	var out []Role
	for _, r := range p.Roles {
		if r.Mode == ModeNative && r.Subagent == "" {
			out = append(out, r)
		}
	}
	return out
}

// NativeBody is a native role's system prompt: its own prompt text, or a sensible default.
func NativeBody(r *Role) string {
	if b := strings.TrimSpace(r.PromptText); b != "" {
		return b
	}
	desc := "The " + r.Name + " subagent the lead delegates to."
	if len(r.When) > 0 {
		desc = "Use for: " + strings.Join(r.When, ", ") + "."
	}
	return "You are the " + r.Name + " subagent the lead delegates to. " + desc +
		"\n\nRead whatever code you need and verify claims against the source. Your reply is" +
		" consumed by the lead, not a human: lead with the decision or result, then the" +
		" load-bearing reasoning, then concrete next steps. No preamble."
}

// NativeDescription is the provider-neutral summary a native-capable adapter renders into its
// own subagent metadata. The adapter owns syntax; preset owns the role's meaning.
func NativeDescription(r *Role) string {
	if len(r.When) > 0 {
		return "Use for: " + strings.Join(r.When, ", ") + "."
	}
	return "The " + r.Name + " subagent the lead delegates to."
}

// ConsultBody is the persona mounted for a consult role — what the peer reads ahead of the lead's
// question: its own prompt (none → empty: no persona file, the peer answers as itself).
func ConsultBody(r *Role) string {
	return strings.TrimSpace(r.PromptText)
}
