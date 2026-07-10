package fusion

import (
	"sort"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// ConsultWrapperPath is where coop mounts the coop-consult script inside the box — on
// PATH, so the governor/lead invokes it as a bare `coop-consult`.
const ConsultWrapperPath = "/usr/local/bin/coop-consult"

// ConsultWrapper renders the `coop-consult` script coop mounts into every fusion and
// --consult box. It gives the lead a uniform `coop-consult <peer> --fresh|--continue`
// interface over each registered peer's read-only consult command, hiding the per-agent
// session-id mechanics — the per-agent shell comes from each adapter (ConsultFresh /
// ConsultResume / ShellPrelude), so adding a provider needs no edit here. claude/gemini
// start a session under a generated --session-id and resume it with --resume; codex
// captures its thread_id from `codex exec --json` and resumes with `codex exec resume`.
// Continuity lets a follow-up turn send only the delta; the first line printed reports
// whether the session continued or started fresh, so the lead can tell when a --continue
// fell back to fresh. The peer stays READ-ONLY throughout.
//
// The per-agent read-only flags come from each adapter's ConsultCmd family —
// TestConsultWrapperMatchesAdapters asserts each rendered arm carries them.
func ConsultWrapper() string { return renderConsult(registeredConsults()) }

// consultInput is the narrow slice of an Agent the consult generator needs — so a drift
// test can pass a fake 4th agent without implementing the whole Agent interface. Every
// registered agent satisfies it.
type consultInput interface {
	Name() string
	ConsultFresh() string
	ConsultResume() string
	ShellPrelude() string
}

// registeredConsults returns every registered agent as a consultInput, sorted by name for
// a deterministic script.
func registeredConsults() []consultInput {
	names := agents.Names() // already sorted
	out := make([]consultInput, 0, len(names))
	for _, n := range names {
		if a, ok := agents.Get(n); ok {
			out = append(out, a)
		}
	}
	return out
}

// caseArm renders one `<name>)\n<body indented one tab>\n\t;;` arm; body lines are
// indented uniformly (each fragment is flush-left).
func caseArm(name, body string) string {
	var b strings.Builder
	b.WriteString(name + ")\n")
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString("\t" + line + "\n")
	}
	b.WriteString("\t;;\n")
	return b.String()
}

// preludes collects every agent's ShellPrelude (deduped, in order) — helper functions the
// case arms rely on, emitted once before the dispatch (codex's codex_text filter).
func preludes(as []consultInput) string {
	seen := map[string]bool{}
	var out []string
	for _, a := range as {
		if p := a.ShellPrelude(); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
}

func renderConsult(as []consultInput) string {
	names := make([]string, len(as))
	var fresh, resume strings.Builder
	for i, a := range as {
		names[i] = a.Name()
		fresh.WriteString(caseArm(a.Name(), a.ConsultFresh()))
		resume.WriteString(caseArm(a.Name(), a.ConsultResume()))
	}
	sort.Strings(names)
	peerAlt := strings.Join(names, " | ")
	peerList := strings.Join(names, "|")

	var b strings.Builder
	b.WriteString(`#!/bin/sh
# coop-consult — ask a peer read-only, with optional cross-turn continuity.
# Generated and mounted by coop; do not edit.
#   coop-consult <peer|role> <--fresh|--continue> [prompt]
# <peer> is ` + peerList + ` (fusion / --consult ad-hoc). A preset CONSULT ROLE — or a
# native role degraded under a non-Claude lead — is addressed by its ROLE name: coop exports
# COOP_CONSULT_<ROLE>_{AGENT,MODEL,CONTRACT}, so it runs on the role's own agent + model with
# its persona (CONTRACT) prepended to the prompt. The target is READ-ONLY: it analyses and
# reports, it never edits your files. --fresh starts a new session; --continue resumes the
# last one for that target (send only the delta). Prompt is the trailing arg or piped on
# stdin. The first line printed is the session status to read. Each consult is time-bounded
# (default 30m; set COOP_CONSULT_TIMEOUT in seconds).
# The model is resolved into $model below (a role's COOP_CONSULT_<ROLE>_MODEL wins over the
# per-peer COOP_PEER_MODEL_<PEER>), expanded into the --model flag in each arm.
set -u

die() { echo "coop-consult: $1" >&2; exit 2; }
[ "$#" -ge 2 ] || die "usage: coop-consult <peer|role> <--fresh|--continue> [prompt]"
name=$1
mode=$2
shift 2
# Validate the target up front, before it's used in the idfile path below — a bogus name
# would otherwise build a stray/traversed /tmp path and make --continue's line lie.
case "$name" in
'' | *[!a-z0-9-]*) die "invalid consult target: $name (letters, digits, dashes)" ;;
esac
key=$(printf '%s' "$name" | tr 'a-z-' 'A-Z_')
# A preset consult role (or a native role degraded under a non-Claude lead) carries its own
# agent/model/persona via COOP_CONSULT_<ROLE>_*; otherwise the name IS the peer agent.
eval "peer=\${COOP_CONSULT_${key}_AGENT:-}"
eval "rolemodel=\${COOP_CONSULT_${key}_MODEL:-}"
eval "persona=\${COOP_CONSULT_${key}_CONTRACT:-}"
if [ -n "$peer" ]; then
	role=1
else
	peer=$name
	persona=
	role=
fi
# The peer must be BOTH a registered adapter AND in this run's council (COOP_PEERS — the
# agents whose credentials coop actually mounted). The second check is the security gate:
# it refuses a peer the run never named, so a compromised lead can't consult (and thereby
# drive) an unlisted agent even though its adapter arm exists below.
case "$peer" in
` + peerAlt + `) ;;
*) die "unknown peer: $name (expected ` + peerList + `, or a preset consult role)" ;;
esac
case " ${COOP_PEERS:-} " in
*" $peer "*) ;;
*) die "peer not in this run's council: $name (COOP_PEERS='${COOP_PEERS:-}')" ;;
esac
# Resolve the model into one $model: a role's own model wins; else the per-peer default
# coop exported (COOP_PEER_MODEL_<PEER>). One var, so every arm expands it the same way.
peerkey=$(printf '%s' "$peer" | tr 'a-z-' 'A-Z_')
eval "model=\${COOP_PEER_MODEL_${peerkey}:-}"
if [ -n "$role" ] && [ -n "$rolemodel" ]; then model=$rolemodel; fi
prompt=${*:-}
[ -n "$prompt" ] || prompt=$(cat)
# A role's persona (its contract) is prepended so the peer answers AS that role.
if [ -n "$persona" ] && [ -r "$persona" ]; then
	prompt=$(cat "$persona"; printf '\n\n---\n\nYour question:\n\n%s' "$prompt")
fi
# The prompt is captured above, so no peer needs stdin. Detach it: claude -p reads
# piped stdin on top of its arg and blocks forever on an inherited open pipe (the
# governor backgrounds these consults). One redirect covers every peer.
exec </dev/null
idfile="/tmp/coop-consult-${key}.id"

new_id() {
	if [ -r /proc/sys/kernel/random/uuid ]; then
		cat /proc/sys/kernel/random/uuid
	else
		od -An -tx1 -N16 /dev/urandom | tr -d ' \n' |
			sed 's/\(........\)\(....\)\(....\)\(....\)\(............\)/\1-\2-\3-\4-\5/'
	fi
}
`)
	if p := preludes(as); p != "" {
		b.WriteString(p + "\n")
	}
	b.WriteString(`
# Bound every consult so a slow or wedged peer can't stall the lead's wait. Default 30m;
# on timeout the peer is skipped with a notice (on stderr, so it survives codex's $()
# capture and the codex_text pipe) and the lead synthesizes from whoever answered. -k
# gives a peer that ignores SIGTERM a short grace before SIGKILL; GNU timeout exits 124
# (TERM) or 137 (KILL) on expiry.
consult_timeout=${COOP_CONSULT_TIMEOUT:-1800}
run() {
	timeout -k 30 "$consult_timeout" "$@"
	st=$?
	if [ "$st" -eq 124 ] || [ "$st" -eq 137 ]; then
		echo "[$peer: no reply within ${consult_timeout}s — skipped; synthesize without it]" >&2
	fi
	return "$st"
}

case "$mode" in
--continue)
	if [ -f "$idfile" ]; then
		id=$(cat "$idfile")
		echo "[$peer: continued — recalls your earlier consult; send only the delta]"
		case "$peer" in
` + indentArms(resume.String(), "\t\t") + `		*) die "unknown peer: $peer" ;;
		esac
		exit
	fi
	echo "[$peer: --continue had no live session — started FRESH, resend full context]"
	;;
--fresh) echo "[$peer: fresh session]" ;;
*) die "mode must be --fresh or --continue" ;;
esac

# Fresh session (also the fallback when --continue found no stored id).
id=$(new_id)
case "$peer" in
` + fresh.String() + `*) die "unknown peer: $peer" ;;
esac
`)
	return b.String()
}

// indentArms prefixes every non-empty line of a rendered case-arm block with pad — used to
// nest the --continue arms under the outer `if [ -f "$idfile" ]` block.
func indentArms(arms, pad string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(arms, "\n"), "\n") {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(pad + line + "\n")
	}
	return b.String()
}
