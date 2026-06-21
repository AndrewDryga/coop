// Package fusion builds the "council" wiring for coop's fusion mode: a governor
// agent that consults its two peers (read-only) and synthesizes the best result.
// It is expressed entirely as an instruction injected into the governor's own
// instruction file — the governor already has a shell and the peer CLIs live in
// the same box, so no extra protocol (MCP) is needed. This package is pure (text
// + selection only); box.Run does the file generation and mounting.
package fusion

import (
	"fmt"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// Valid reports whether tool is one of the known agents.
func Valid(tool string, all []string) bool {
	for _, a := range all {
		if a == tool {
			return true
		}
	}
	return false
}

// Peers returns the agents other than the governor, preserving order.
func Peers(governor string, all []string) []string {
	peers := make([]string, 0, len(all))
	for _, a := range all {
		if a != governor {
			peers = append(peers, a)
		}
	}
	return peers
}

// peerCmd is the read-only, non-interactive command to consult a peer with a question.
// Each agent owns its own consult command in its adapter; nil for an unknown tool.
func peerCmd(tool, question string) []string {
	if ag, ok := agents.Get(tool); ok {
		return ag.ConsultCmd(question)
	}
	return nil
}

// placeholder marks where the governor substitutes the prompt it composes for a
// peer — NOT the user's message forwarded verbatim. Each consult is a fresh,
// read-only call that has none of this thread's context, so the governor writes a
// self-contained prompt carrying the context the peer needs to answer.
const placeholder = `"<a self-contained prompt: your question + the context needed to answer it>"`

// consultBlock renders a copy-pasteable shell snippet that runs every peer
// read-only and in parallel, then prints each answer under a header.
func consultBlock(peers []string) string {
	var b strings.Builder
	for _, p := range peers {
		fmt.Fprintf(&b, "  ( %s ) >/tmp/peer-%s.txt 2>&1 &\n", strings.Join(peerCmd(p, placeholder), " "), p)
	}
	b.WriteString("  wait\n")
	for _, p := range peers {
		fmt.Fprintf(&b, "  echo '----- %s -----'; cat /tmp/peer-%s.txt\n", p, p)
	}
	return b.String()
}

// peerCmdList renders one read-only command per peer as a labeled list, so a lead
// knows exactly how to invoke each model and can consult one or several.
func peerCmdList(peers []string) string {
	var b strings.Builder
	for _, p := range peers {
		fmt.Fprintf(&b, "- %s: %s\n", p, strings.Join(peerCmd(p, placeholder), " "))
	}
	return b.String()
}

// Instruction is the fusion directive for the governor: consult these specific
// peers read-only and in parallel, then synthesize. Naming the exact commands
// makes the governor run the right thing.
func Instruction(governor string, peers []string) string {
	return fmt.Sprintf(`# Fusion mode — you are %s, the governor of a model council

Your peers are %s. The defining rule of this mode: you never answer or act alone.
Before you respond to ANY question, propose ANY plan, start ANY task, or make ANY
change — no matter how small, and no matter how confident you are — you MUST first
consult BOTH peers and then synthesize their answers with your own. Consulting is
unconditional: there is no "this one is trivial" or "I already know the answer"
exception, and answering from your own knowledge alone defeats the entire purpose
of fusion mode. The only thing that needs no consult is the incidental shell you
run while carrying out a task you have ALREADY consulted on (ls, cd, cat, git
status) — those are not themselves tasks.

Your peers are READ-ONLY ADVISORS: they think and report, they never edit a file,
run a mutating command, or touch the repo. YOU are the only one who acts. So when
the task is to change something — "do a code review and add the results to
.agent/TASKS.md", "fix the failing test", "refactor this" — do not hand that action
to a peer; it cannot do it. Ask each peer only for the thinking the action needs —
the review, the diagnosis, the design — then make every edit and run every command
yourself when you synthesize.

Each consult is a FRESH, MEMORYLESS call: the peer sees only the text you pass it —
not this conversation, not your earlier consults, not what you have done since.
Never forward the user's message verbatim; past the first turn it is meaningless out
of context ("fix the second one" tells a peer nothing). Compose a self-contained
prompt every time: state the goal, paste the relevant code, paths, and errors, and
ask the one specific question you need answered.

## 1. Consult BOTH peers FIRST — read-only, in parallel
Drop the self-contained prompt you composed in place of the placeholder and run the
whole block from your shell — do not drop a peer, even if the first answer already
looks sufficient:

%s
If a peer errors or is unavailable, proceed with the others — but always attempt
all of them before you answer.

## 2. Synthesize, then act
- Read every peer's answer in full before you respond.
- Combine the strongest parts of each with your own reasoning; resolve
  disagreements by evidence or verification, not by a vote.
- You are the decider: produce the single best answer or implementation — making
  every edit and running every command yourself — and briefly note where the peers
  agreed or shifted your approach.
`, governor, strings.Join(peers, " and "), consultBlock(peers))
}

// GovernorInstructions is the full instruction file mounted for the governor: the
// fusion directive first (for prominence), then the governor's existing
// instructions (a shared INSTRUCTIONS.md or the user's own override), so nothing
// the user wrote is lost.
func GovernorInstructions(base, governor string, all []string) string {
	block := Instruction(governor, Peers(governor, all))
	if base = strings.TrimSpace(base); base != "" {
		return block + "\n" + base + "\n"
	}
	return block
}

// ConsultInstruction is the light, optional second-opinion directive for a normal
// (non-fusion) lead agent: it MAY consult these peers read-only when a decision is
// genuinely hard, to catch its own blind spots — never required and never for
// routine work, so it stays cheap. Only authenticated peers are passed in, so it
// never points the lead at an agent that can't answer.
func ConsultInstruction(peers []string) string {
	return fmt.Sprintf(`# A second opinion is available

For a genuinely hard or risky call — a load-bearing architectural choice, a subtle
bug, a security-sensitive change — you can get a read-only second opinion from %s,
whose different blind spots may catch what you'd miss. This is optional and for the
decisions that matter, not routine work; you remain the decider.

Each peer runs read-only — it returns its analysis and never edits your files. Run
the matching command from your shell, with your real question in the quotes:

%s
Consulting more than one? Run them in parallel and read every reply:

  ( <command A> ) >/tmp/a.txt 2>&1 &
  ( <command B> ) >/tmp/b.txt 2>&1 &
  wait; cat /tmp/a.txt /tmp/b.txt

Weigh each answer against your own reasoning, then decide and act.
`, strings.Join(peers, " and "), peerCmdList(peers))
}

// LeadInstructions is the instruction file mounted for a normal lead agent: the
// optional consult directive first, then the lead's existing instructions
// unchanged. With no peers to consult it returns the base alone (no directive).
func LeadInstructions(base string, peers []string) string {
	base = strings.TrimSpace(base)
	if len(peers) == 0 {
		return base
	}
	block := ConsultInstruction(peers)
	if base != "" {
		return block + "\n" + base + "\n"
	}
	return block
}
