package preset

import (
	"fmt"
	"strings"
)

// LeadContract renders the generated routing block mounted ahead of the lead's own
// instructions: who leads, which roles exist, when to use each, and the EXACT
// invocation for each mode. The generated text is always present; a preset's
// Markdown files append to it (lead.md after the block, each roles/<name>.md after
// its role's contract) — they refine, never replace, the routing/safety text.
func LeadContract(p *Preset) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Orchestration preset %q — you are the lead (%s)\n\n", p.Name, p.LeadAgent)
	b.WriteString("You lead this session: you make the calls, do the final review, run the gate,\n")
	b.WriteString("and make every commit. Route work to your roles by their \"use for\" hints —\n")
	b.WriteString("spend yourself on judgment, not on work a role covers.\n")
	for i := range p.Roles {
		b.WriteString("\n")
		b.WriteString(roleContract(&p.Roles[i]))
	}
	if p.LeadPromptText != "" {
		b.WriteString("\n" + p.LeadPromptText + "\n")
	}
	return b.String()
}

// RoleContract renders one role's generated contract plus its appended Markdown —
// the same text the lead sees for that role, reused as the delegate wrapper's
// prepended contract so the delegate knows its own ground rules.
func RoleContract(r *Role) string {
	return roleContract(r)
}

func roleContract(r *Role) string {
	var b strings.Builder
	model := r.Model
	if model == "" {
		model = "its default model"
	}
	switch r.Mode {
	case ModeNative:
		fmt.Fprintf(&b, "## %s — native %s subagent (%s)\n", r.Name, r.Agent, model)
		writeWhen(&b, r.When)
		fmt.Fprintf(&b, "Invoke it as the @%s subagent in your own session — it thinks inside your\n", SubagentName(r))
		b.WriteString("context; you weigh its conclusion and act on it yourself.\n")
	case ModeConsult:
		fmt.Fprintf(&b, "## %s — read-only consult peer (%s, %s)\n", r.Name, r.Agent, model)
		writeWhen(&b, r.When)
		b.WriteString("It analyses and reports; it NEVER edits a file or runs a mutating command. Ask it:\n\n")
		fmt.Fprintf(&b, "  coop-consult %s --fresh \"<a self-contained prompt: your question + the context needed to answer it>\"\n", r.Agent)
	case ModeDelegate:
		fmt.Fprintf(&b, "## %s — write-capable delegate (%s, %s)\n", r.Name, r.Agent, model)
		writeWhen(&b, r.When)
		b.WriteString("It MAY edit files in this worktree but must NEVER commit; delegate runs are\nserialized (one at a time). Hand it a task with:\n\n")
		fmt.Fprintf(&b, "  coop-delegate %s <<'EOF'\n  <a self-contained implementation prompt>\n  EOF\n\n", r.Name)
		b.WriteString("When it returns, YOU review its `git diff`, run the gate, fix or revert what\nfalls short, and make the commit yourself — the delegate's work ships under\nyour review or not at all.\n")
	}
	// A generated native role's prompt IS its subagent's system prompt (GeneratedSubagent),
	// so don't also dump it into the lead contract; a referenced/other role appends here.
	if r.PromptText != "" && !(r.Mode == ModeNative && r.Subagent == "") {
		b.WriteString("\n" + r.PromptText + "\n")
	}
	return b.String()
}

func writeWhen(b *strings.Builder, when []string) {
	if len(when) > 0 {
		fmt.Fprintf(b, "Use for: %s.\n", strings.Join(when, ", "))
	}
}
