// Package consult builds the read-only peer-consultation instructions mounted into
// a lead's own instruction file. The peer CLIs live in the same box, so no extra
// protocol is needed. This package is pure; box.Run owns file generation and mounts.
package consult

import (
	"fmt"
	"strings"
)

// placeholder marks where the lead substitutes the prompt it composes for a peer —
// NOT the user's message forwarded verbatim. A --fresh consult has none of this
// thread's context, so the lead writes a self-contained prompt carrying the context
// the peer needs; a --continue consult sends only the delta (see the instructions).
const placeholder = `"<a self-contained prompt: your question + the context needed to answer it>"`

// consultCall renders the coop-consult invocation that asks a peer read-only. It shows
// --fresh (a new session); on a follow-up about the same subject the lead swaps in
// --continue and sends only the delta. coop-consult hides each agent's session-id
// mechanics behind one uniform interface (see ConsultWrapper).
func consultCall(peer string) string {
	return fmt.Sprintf("coop-consult %s --fresh %s", peer, placeholder)
}

// consultBlock renders a copy-pasteable shell snippet that runs every peer read-only and in
// parallel, then prints replies and diagnostics on their original streams under distinct headers.
func consultBlock(peers []string) string {
	var b strings.Builder
	b.WriteString("  (\n")
	b.WriteString("    consult_tmp=$(mktemp -d) || exit 1\n")
	b.WriteString("    trap 'rm -rf \"$consult_tmp\"' 0\n")
	for _, p := range peers {
		fmt.Fprintf(&b, "    ( %s ) >\"$consult_tmp/peer-%s.reply\" 2>\"$consult_tmp/peer-%s.diagnostics\" &\n", consultCall(p), p, p)
	}
	b.WriteString("    wait\n")
	for _, p := range peers {
		fmt.Fprintf(&b, "    echo '----- %s reply -----'; cat \"$consult_tmp/peer-%s.reply\"\n", p, p)
		fmt.Fprintf(&b, "    if [ -s \"$consult_tmp/peer-%s.diagnostics\" ]; then echo '----- %s diagnostics -----' >&2; cat \"$consult_tmp/peer-%s.diagnostics\" >&2; fi\n", p, p, p)
	}
	b.WriteString("  )\n")
	return b.String()
}

// peerCmdList renders one consult invocation per peer as a labeled list, so a lead
// knows exactly how to invoke each model and can consult one or several.
func peerCmdList(peers []string) string {
	var b strings.Builder
	for _, p := range peers {
		fmt.Fprintf(&b, "- %s: %s\n", p, consultCall(p))
	}
	return b.String()
}

// ConsultInstruction is the light, optional second-opinion directive for a normal
// lead agent: it MAY consult these peers read-only when a decision is
// genuinely hard, to catch its own blind spots — never required and never for
// routine work, so it stays cheap. Only authenticated peers are passed in, so it
// never points the lead at an agent that can't answer.
func ConsultInstruction(peers []string) string {
	return fmt.Sprintf(`# A second opinion is available

For a genuinely hard or risky call — a load-bearing architectural choice, a subtle
bug, a security-sensitive change — you can get a read-only second opinion from %s,
whose different blind spots may catch what you'd miss. This is optional and for the
decisions that matter, not routine work; you remain the decider.

Consult a peer with coop-consult <peer> --fresh "<prompt>" — it runs the peer
read-only (it returns analysis and never edits your files) and prints a one-line
status first. Compose a self-contained prompt: your question plus the context to
answer it.

%s
The one-line status is session metadata, not the peer reply. If your shell or
execution tool yields a session handle, retain it and
poll that same session to terminal exit, accumulating and reading its complete output.
Default to --fresh — each hard call is best judged independently. Use --continue only
to drill deeper into the SAME call you already asked about, sending just what changed.
If it says "FRESH from the saved transcript", Coop already replayed that context; if it
says plain "started FRESH, resend full context", give a full self-contained follow-up.

Consulting more than one? Run them in parallel and read every reply:

%s

Weigh each answer against your own reasoning, then decide and act.
`, strings.Join(peers, " and "), peerCmdList(peers), consultBlock(peers))
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
