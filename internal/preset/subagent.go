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

// GeneratedNativeRoles are the native roles coop generates a subagent for — those that DON'T
// reference an existing one. A role with `subagent:` set is a reference, so it's excluded.
// Used for the in-box subagent mount under a Claude lead.
func (p *Preset) GeneratedNativeRoles() []Role {
	var out []Role
	for _, r := range p.Roles {
		if r.Mode == ModeNative && r.Subagent == "" {
			out = append(out, r)
		}
	}
	return out
}

// DegradedNativeRoles are the native roles that degrade to a read-only consult because the
// effective lead can't host Claude subagents in-session (any native role when lead isn't
// claude). Each is wired as `coop-consult <role>` carrying the role's agent + model + persona.
func (p *Preset) DegradedNativeRoles(lead string) []Role {
	if lead == "claude" {
		return nil
	}
	var out []Role
	for _, r := range p.Roles {
		if r.Mode == ModeNative {
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

// GeneratedSubagent renders the Claude subagent file coop overlays into the box for a
// generated native role: `coop-<role>.md`, with the role's model in the frontmatter (so a
// native role's model finally takes effect), a `when:`-derived description (Claude uses it
// for auto-delegation), and the role's prompt as the system prompt (a default if absent).
// Only valid for a role GeneratedNativeRoles returned (native, no subagent).
func GeneratedSubagent(r *Role) (filename, content string) {
	name := "coop-" + r.Name
	desc := "The " + r.Name + " subagent the lead delegates to."
	if len(r.When) > 0 {
		desc = "Use for: " + strings.Join(r.When, ", ") + "."
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + name + "\n")
	b.WriteString("description: " + desc + "\n")
	if r.Model != "" {
		b.WriteString("model: " + r.Model + "\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(NativeBody(r) + "\n")
	return name + ".md", b.String()
}
