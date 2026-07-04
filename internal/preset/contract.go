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
//
// lead is the EFFECTIVE lead agent (an explicit `coop codex --preset …` overrides the
// preset's lead). A native role is a Claude subagent that runs inside the lead's own
// session; under a non-Claude lead it can't, so it degrades to a read-only consult to its
// agent (same model + persona), invoked as `coop-consult <role>` (see roleContract).
func LeadContract(p *Preset, lead string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Orchestration preset %q — you are the lead (%s)\n\n", p.Name, p.LeadAgent)
	b.WriteString("You lead this session: you make the calls, do the final review, run the gate,\n")
	b.WriteString("and make every commit. Route work to your roles by their \"use for\" hints —\n")
	b.WriteString("spend yourself on judgment, not on work a role covers.\n")
	for i := range p.Roles {
		b.WriteString("\n")
		b.WriteString(roleContract(&p.Roles[i], lead))
	}
	if p.LeadPromptText != "" {
		b.WriteString("\n" + p.LeadPromptText + "\n")
	}
	return b.String()
}

// NativeRolesUsable reports whether the effective lead can host this preset's native roles
// (Claude subagents run only inside a Claude lead's session). Used to gate the in-box
// subagent generation and to warn when native roles are dropped for a non-Claude lead.
func (p *Preset) NativeRolesUsable(lead string) bool { return lead == "claude" }

// RoleContract renders one role's generated contract plus its appended Markdown —
// the same text the lead sees for that role, reused as the delegate wrapper's
// prepended contract so the delegate knows its own ground rules. Delegate-only, so the
// lead is immaterial (native/consult degradation never applies).
func RoleContract(r *Role) string {
	return roleContract(r, "claude")
}

func roleContract(r *Role, lead string) string {
	var b strings.Builder
	model := r.Model
	if model == "" {
		model = "its default model"
	}
	// A native role can't run in-session under a non-Claude lead — it degrades to a read-only
	// consult on its agent, rendered exactly like an explicit consult role (both are
	// role-addressed: `coop-consult <role>` carries the role's agent + model + persona).
	mode := r.Mode
	if mode == ModeNative && lead != "claude" {
		mode = ModeConsult
	}
	switch mode {
	case ModeNative:
		fmt.Fprintf(&b, "## %s — native %s subagent (%s)\n", r.Name, r.Agent, model)
		writeWhen(&b, r.When)
		fmt.Fprintf(&b, "Invoke it as the @%s subagent in your own session — it thinks inside your\n", SubagentName(r))
		b.WriteString("context; you weigh its conclusion and act on it yourself.\n")
	case ModeConsult:
		fmt.Fprintf(&b, "## %s — read-only consult (%s, %s)\n", r.Name, r.Agent, model)
		writeWhen(&b, r.When)
		b.WriteString("It analyses and reports; it NEVER edits a file or runs a mutating command. Ask it:\n\n")
		fmt.Fprintf(&b, "  coop-consult %s --fresh \"<a self-contained prompt: your question + the context needed to answer it>\"\n", r.Name)
	case ModeDelegate:
		fmt.Fprintf(&b, "## %s — write-capable delegate (%s, %s)\n", r.Name, r.Agent, model)
		writeWhen(&b, r.When)
		b.WriteString("It MAY edit files in this worktree but must NEVER commit; delegate runs are\nserialized (one at a time). Hand it a task with:\n\n")
		fmt.Fprintf(&b, "  coop-delegate %s <<'EOF'\n  <a self-contained implementation prompt>\n  EOF\n\n", r.Name)
		b.WriteString("When it returns, YOU review its `git diff`, run the gate, fix or revert what\nfalls short, and make the commit yourself — the delegate's work ships under\nyour review or not at all.\n")
	}
	// A role whose prompt reaches its runner elsewhere doesn't dump it into the lead contract:
	// a generated native's prompt IS its subagent's system prompt (GeneratedSubagent), and a
	// consult-wired role's prompt IS the peer's persona (ConsultBody, mounted in the box) —
	// degraded natives included. A delegate or a referenced native appends here.
	promptReachesRunner := r.Mode == ModeConsult || (r.Mode == ModeNative && (r.Subagent == "" || lead != "claude"))
	if r.PromptText != "" && !promptReachesRunner {
		b.WriteString("\n" + r.PromptText + "\n")
	}
	return b.String()
}

func writeWhen(b *strings.Builder, when []string) {
	if len(when) > 0 {
		fmt.Fprintf(b, "Use for: %s.\n", strings.Join(when, ", "))
	}
}
