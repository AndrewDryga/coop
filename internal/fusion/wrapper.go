package fusion

// ConsultWrapperPath is where coop mounts the coop-consult script inside the box — on
// PATH, so the governor/lead invokes it as a bare `coop-consult`.
const ConsultWrapperPath = "/usr/local/bin/coop-consult"

// ConsultWrapper is the `coop-consult` script coop mounts into every fusion and
// --consult box. It gives the lead a uniform `coop-consult <peer> --fresh|--continue`
// interface over the three peers' read-only consult commands, hiding the per-agent
// session-id mechanics: claude/gemini start a session under a generated --session-id
// and resume it with --resume; codex (which can't preset an id) has its thread_id
// captured from `codex exec --json` and resumes with `codex exec resume`. Continuity
// lets a follow-up turn send only the delta instead of re-pasting the static context;
// the first line printed reports whether the session continued or started fresh, so the
// lead can tell when a --continue fell back to fresh and must resend full context. The
// peer stays READ-ONLY throughout (plan / read-only sandbox), so it never edits files.
//
// Keep the per-agent read-only flags here in sync with each adapter's ConsultCmd —
// TestConsultWrapperMatchesAdapters asserts it.
const ConsultWrapper = `#!/bin/sh
# coop-consult — ask a fusion / --consult peer read-only, with optional cross-turn
# continuity. Generated and mounted by coop; do not edit.
#   coop-consult <peer> <--fresh|--continue> [prompt]
# The peer is READ-ONLY: it analyses and reports, it never edits your files. --fresh
# starts a new session; --continue resumes the peer's last one (send only the delta).
# The prompt is the trailing argument, or piped on stdin (use a quoted heredoc for
# prompts with awkward quoting). The first line printed is the session status to read.
set -u

die() { echo "coop-consult: $1" >&2; exit 2; }
[ "$#" -ge 2 ] || die "usage: coop-consult <claude|codex|gemini> <--fresh|--continue> [prompt]"
peer=$1
mode=$2
shift 2
prompt=${*:-}
[ -n "$prompt" ] || prompt=$(cat)
idfile="/tmp/coop-consult-${peer}.id"

new_id() {
	if [ -r /proc/sys/kernel/random/uuid ]; then
		cat /proc/sys/kernel/random/uuid
	else
		od -An -tx1 -N16 /dev/urandom | tr -d ' \n' |
			sed 's/\(........\)\(....\)\(....\)\(....\)\(............\)/\1-\2-\3-\4-\5/'
	fi
}
# codex --json prints one JSON object per line; pull the agent's reply text.
codex_text() { jq -r 'select(.type=="item.completed" and .item.type=="agent_message").item.text' 2>/dev/null; }

case "$mode" in
--continue)
	if [ -f "$idfile" ]; then
		id=$(cat "$idfile")
		echo "[$peer: continued — recalls your earlier consult; send only the delta]"
		case "$peer" in
		claude) claude -p --permission-mode plan --resume "$id" "$prompt" ;;
		gemini) gemini --approval-mode plan --resume "$id" -p "$prompt" ;;
		codex) codex exec resume "$id" -c sandbox_mode=read-only --json "$prompt" </dev/null | codex_text ;;
		*) die "unknown peer: $peer" ;;
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
claude)
	printf '%s' "$id" >"$idfile"
	claude -p --permission-mode plan --session-id "$id" "$prompt"
	;;
gemini)
	printf '%s' "$id" >"$idfile"
	gemini --approval-mode plan --session-id "$id" -p "$prompt"
	;;
codex)
	out=$(codex exec -s read-only --json "$prompt" </dev/null) || true
	printf '%s\n' "$out" | jq -r 'select(.type=="thread.started").thread_id' 2>/dev/null | head -n1 >"$idfile"
	printf '%s\n' "$out" | codex_text
	;;
*) die "unknown peer: $peer" ;;
esac
`
