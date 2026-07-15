package preset

import (
	"fmt"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// LeadContract renders the generated routing block mounted ahead of the lead's own
// instructions: who leads, which roles exist, when to use each, and the EXACT
// invocation for each mode. The generated text is always present; a preset's
// Markdown files append to it (lead.md after the block, each roles/<name>.md after
// its role's contract) — they refine, never replace, the routing/safety text.
//
// lead is the EFFECTIVE lead agent (a loop work.agent ladder or an ACP cross-provider rung
// may run a preset under a different provider than the preset's own lead). A native role runs
// inside a capable lead's session; otherwise it degrades to a read-only consult on its configured
// agent (same model + persona), invoked as `coop-consult <role>` (see roleContract).
func LeadContract(p *Preset, lead string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Orchestration preset %q — you are the lead (%s)\n\n", p.Name, lead)
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
	// A native role degrades under a lead that cannot host it, rendered exactly like an explicit
	// read-only consult role (both are
	// role-addressed: `coop-consult <role>` carries the role's agent + model + persona).
	mode := r.Mode
	if mode == ModeNative && !nativeRoleUsable(r, lead) {
		mode = ModeConsult
	}
	switch mode {
	case ModeNative:
		fmt.Fprintf(&b, "## %s — native %s subagent (%s)\n", r.Name, r.Agent, model)
		writeWhen(&b, r.When)
		fmt.Fprintf(&b, "Invoke it as the @%s subagent in your own session — it thinks inside your\n", SubagentName(r))
		b.WriteString("context; you weigh its conclusion and act on it yourself.\n")
	case ModeConsult:
		fmt.Fprintf(&b, "## %s — read-only consult (%s)\n", r.Name, roleRunner(r, model))
		writeWhen(&b, r.When)
		b.WriteString("Ask it at the moments that are expensive to get wrong — a plan you're about to\n")
		b.WriteString("execute, a security-sensitive change, a tradeoff you're about to lock in.\n")
		b.WriteString("It analyses and reports; it NEVER edits a file or runs a mutating command. Ask it:\n\n")
		fmt.Fprintf(&b, "  coop-consult %s --fresh \"<a self-contained prompt: your question + the context needed to answer it>\"\n", r.Name)
		b.WriteString("\nThe one-line status is not the reply. If your shell or execution tool yields a\n")
		b.WriteString("session handle, retain it and poll that same session to terminal exit, then read\n")
		b.WriteString("the complete accumulated output.\n")
	case ModeDelegate:
		fmt.Fprintf(&b, "## %s — write-capable delegate (%s)\n", r.Name, roleRunner(r, model))
		writeWhen(&b, r.When)
		b.WriteString("The lead's DEFAULT implementer for mechanical work — anything specifiable\n")
		b.WriteString("exactly in a few sentences: boilerplate, repetitive edits, scaffolding,\n")
		b.WriteString("mechanical refactors. Typing those yourself burns your context and altitude\n")
		b.WriteString("on work a cheaper model does fine; by the time you can picture the diff, the\n")
		b.WriteString("spec is already written — hand it off and keep leading. Write it yourself\n")
		b.WriteString("only when the change is smaller than the prompt it would take to specify.\n")
		b.WriteString("It MAY edit files in this worktree but must NEVER commit; delegate runs are\nserialized (one at a time) and bounded to one level. A delegate must not invoke\n`coop-delegate` again; it may still use a configured read-only `coop-consult`. Hand it a task with:\n\n")
		fmt.Fprintf(&b, "  coop-delegate %s <<'EOF'\n  <a self-contained prompt: the files to touch, the exact change, how to\n   verify — it sees none of your conversation>\n  EOF\n\n", r.Name)
		b.WriteString("When it returns, YOU review its `git diff`, run the gate, fix or revert what\nfalls short, and make the commit yourself — the delegate's work ships under\nyour review or not at all.\n")
	}
	// A role whose prompt reaches its runner elsewhere doesn't dump it into the lead contract:
	// a generated native's prompt IS its subagent's system prompt (GeneratedSubagent), and a
	// consult-wired role's prompt IS the peer's persona (ConsultBody, mounted in the box) —
	// degraded natives included. A delegate or a referenced native appends here.
	promptReachesRunner := r.Mode == ModeConsult || (r.Mode == ModeNative && (r.Subagent == "" || !nativeRoleUsable(r, lead)))
	if r.PromptText != "" && !promptReachesRunner {
		b.WriteString("\n" + r.PromptText + "\n")
	}
	return b.String()
}

// nativeRoleUsable requires both halves of native execution: the effective lead must be the
// role's configured provider, and that provider's adapter must own a complete native descriptor.
func nativeRoleUsable(role *Role, lead string) bool {
	if role.Mode != ModeNative || role.Agent != lead {
		return false
	}
	ag, ok := agents.Get(lead)
	if !ok {
		return false
	}
	support := ag.NativeSubagents()
	return support.HomeDir != "" && support.Render != nil
}

func roleRunner(r *Role, primaryModel string) string {
	ladder := r.TargetLadder()
	if len(ladder) <= 1 {
		return r.Agent + ", " + primaryModel
	}
	targets := make([]string, len(ladder))
	for i, target := range ladder {
		targets[i] = target.String()
	}
	return "fallback " + strings.Join(targets, " -> ")
}

func writeWhen(b *strings.Builder, when []string) {
	if len(when) > 0 {
		fmt.Fprintf(b, "Use for: %s.\n", strings.Join(when, ", "))
	}
}
