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
func (p *Preset) GeneratedNativeRoles() []Role {
	var out []Role
	for _, r := range p.Roles {
		if r.Mode == ModeNative && r.Subagent == "" {
			out = append(out, r)
		}
	}
	return out
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
	body := r.PromptText
	if body == "" {
		body = "You are the " + r.Name + " subagent the lead delegates to. " + desc +
			"\n\nRead whatever code you need and verify claims against the source. Your reply is" +
			" consumed by the lead, not a human: lead with the decision or result, then the" +
			" load-bearing reasoning, then concrete next steps. No preamble."
	}
	b.WriteString(strings.TrimSpace(body) + "\n")
	return name + ".md", b.String()
}
