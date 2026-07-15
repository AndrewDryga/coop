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

// GeneratedNativeRoles returns native roles the effective lead can host and Coop must generate.
// A role targeting another provider degrades to consult; a role with `subagent:` is an existing
// native reference, so neither belongs in the generated mount.
func (p *Preset) GeneratedNativeRoles(lead string) []Role {
	var out []Role
	for _, r := range p.Roles {
		if nativeRoleUsable(&r, lead) && r.Subagent == "" {
			out = append(out, r)
		}
	}
	return out
}

// NativeBody is a native role's system prompt: its own prompt text, or a sensible default.
// Shared by the generated subagent (its body) and the degraded consult (its persona) so the
// two deliveries of the same role read identically.
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

// ConsultBody is the persona mounted for a consult-wired role — what the peer reads ahead of
// the lead's question. An explicit consult role's persona is its own prompt (none → empty: no
// persona file, the peer answers as itself); a degraded native always has one (NativeBody), so
// the role reads the same whether it runs as a subagent or as a consult.
func ConsultBody(r *Role) string {
	if r.Mode == ModeNative {
		return NativeBody(r)
	}
	return strings.TrimSpace(r.PromptText)
}
