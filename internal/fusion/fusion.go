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

// peerCmd is the read-only, non-interactive command to consult a peer with a
// question — the single source of truth for how a peer is invoked. Each flag is
// verified to keep the peer read-only: it returns analysis/approach and never
// edits files.
func peerCmd(tool, question string) []string {
	switch tool {
	case "claude":
		return []string{"claude", "-p", "--permission-mode", "plan", question}
	case "gemini":
		return []string{"gemini", "-p", "--approval-mode", "plan", question}
	case "codex":
		return []string{"codex", "exec", "-s", "read-only", question}
	}
	return nil
}

// placeholder marks where the governor substitutes the real question/task.
const placeholder = `"<your full question or task, verbatim>"`

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

// Instruction is the fusion directive for the governor: consult these specific
// peers read-only and in parallel, then synthesize. Naming the exact commands
// makes the governor run the right thing.
func Instruction(governor string, peers []string) string {
	return fmt.Sprintf(`# Fusion mode — you are the governor of a model council

You are the lead agent (%s). For any non-trivial question, decision, or change,
FIRST get independent perspectives from your peer models (%s), then synthesize
the best result yourself. Skip the council only for trivial or mechanical steps.

## 1. Consult your peers — read-only, in parallel
Substitute your full question or task for the placeholder and run this from your
shell. The peers run read-only (they cannot edit files); they return their own
analysis or proposed approach:

%s
## 2. Synthesize, then act
- Combine the strongest parts of each peer's answer with your own reasoning.
- Surface disagreements and resolve them by evidence or verification, not by vote.
- Call out anything all of you missed.
- You are the decider: produce the single best answer or implementation, then
  proceed — do not merely relay the peers. Briefly note where a peer changed your
  approach.
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
