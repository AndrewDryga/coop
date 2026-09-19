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
// inside that lead's session: every lead that may run the preset was checked to host it
// (CheckLeads), so a role renders as exactly what its preset says it is.
func LeadContract(p *Preset, lead string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Orchestration preset %q — you are the lead (%s)\n\n", p.Name, lead)
	b.WriteString("You lead this session: you make the calls, do the final review, run the gate,\n")
	b.WriteString("and make every commit. Route work to your roles by their \"use for\" hints —\n")
	b.WriteString("spend yourself on judgment, not on work a role covers.\n")
	b.WriteString("\nAsk for concise conclusions. Read the complete reply and material diagnostics in\n")
	b.WriteString("bounded chunks when necessary, not only its tail. Preserve remaining access limits,\n")
	b.WriteString("assumptions and unrun checks in final/state/log. Exit 0 or \"no confirmed blocker\"\n")
	b.WriteString("is not source-verified approval. Resolve named source gaps yourself before claiming\n")
	b.WriteString("a verified review; otherwise keep the partial-review qualification alongside the\n")
	b.WriteString("useful advice. A completed, evidenced review remains usable.\n")
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
	primary := r.Primary()
	model := primary.Model
	if model == "" {
		model = "its default model"
	}
	switch r.Mode {
	case ModeNative:
		fmt.Fprintf(&b, "## %s — native %s subagent (%s)\n", r.Name, primary.Provider, model)
		writeWhen(&b, r.When)
		fmt.Fprintf(&b, "Delegate to it as the %s subagent in your own session — it works with your tools\n", SubagentName(r))
		b.WriteString("and reports back; you weigh its conclusion and act on it yourself.\n")
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
		b.WriteString("Finish by summarizing what you changed, what you checked, and what is still\nunfinished — the lead reads that summary, not your transcript.\n")
		b.WriteString("When it returns, YOU review its `git diff`, run the gate, fix or revert what\nfalls short, and make the commit yourself — the delegate's work ships under\nyour review or not at all.\n")
	}
	// A role whose prompt reaches its runner elsewhere doesn't dump it into the lead contract:
	// a generated native's prompt IS its subagent's system prompt (GeneratedNativeRoles), and a
	// consult role's prompt IS the peer's persona (ConsultBody, mounted in the box). A delegate or
	// a referenced native appends here.
	promptReachesRunner := r.Mode == ModeConsult || (r.Mode == ModeNative && r.Subagent == "")
	if r.PromptText != "" && !promptReachesRunner {
		b.WriteString("\n" + r.PromptText + "\n")
	}
	return b.String()
}

// nativeRoleUsable requires both halves of native execution: the lead must be the role's configured
// provider, and that provider's adapter must own a complete native descriptor. CheckLeads refuses a
// preset any of its leads fails this for.
func nativeRoleUsable(role *Role, lead string) bool {
	if role.Mode != ModeNative || role.Primary().Provider != lead {
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
	if len(r.Targets) <= 1 {
		return r.Primary().Provider + ", " + primaryModel
	}
	targets := make([]string, len(r.Targets))
	for i, target := range r.Targets {
		targets[i] = target.String()
	}
	return "fallback " + strings.Join(targets, " -> ")
}

func writeWhen(b *strings.Builder, when []string) {
	if len(when) > 0 {
		fmt.Fprintf(b, "Use for: %s.\n", strings.Join(when, ", "))
	}
}
