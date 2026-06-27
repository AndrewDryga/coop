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
# Each consult is time-bounded (default 30m; set COOP_CONSULT_TIMEOUT in seconds to change);
# a peer that doesn't answer in time is skipped with a notice so you synthesize from whoever did.
set -u

die() { echo "coop-consult: $1" >&2; exit 2; }
[ "$#" -ge 2 ] || die "usage: coop-consult <claude|codex|gemini> <--fresh|--continue> [prompt]"
peer=$1
mode=$2
shift 2
# Validate the peer up front, before it's used in the idfile path below — a bogus name would
# otherwise build a stray/traversed /tmp path and make --continue's "continued" line lie.
case "$peer" in
claude | codex | gemini) ;;
*) die "unknown peer: $peer (expected claude|codex|gemini)" ;;
esac
prompt=${*:-}
[ -n "$prompt" ] || prompt=$(cat)
# The prompt is captured above, so no peer needs stdin. Detach it: claude -p reads
# piped stdin on top of its arg and blocks forever on an inherited open pipe (the
# governor backgrounds these consults), which hung claude consults at the status
# line until they timed out while gemini returned. One redirect covers every peer.
exec </dev/null
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
		claude) run claude -p --permission-mode plan --resume "$id" "$prompt" ;;
		gemini) run gemini --approval-mode plan --resume "$id" -p "$prompt" ;;
		codex) out=$(run codex exec resume "$id" -c sandbox_mode=read-only --json "$prompt"); st=$?; printf '%s\n' "$out" | codex_text; exit "$st" ;;
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
	run claude -p --permission-mode plan --session-id "$id" "$prompt"
	;;
gemini)
	printf '%s' "$id" >"$idfile"
	run gemini --approval-mode plan --session-id "$id" -p "$prompt"
	;;
codex)
	out=$(run codex exec -s read-only --json "$prompt"); st=$?
	# Only record the thread id when one was actually parsed — on a timeout/failure $out is empty,
	# and writing an empty idfile would make the next --continue run "codex exec resume ''".
	tid=$(printf '%s\n' "$out" | jq -r 'select(.type=="thread.started").thread_id' 2>/dev/null | head -n1)
	if [ -n "$tid" ]; then printf '%s' "$tid" >"$idfile"; fi
	printf '%s\n' "$out" | codex_text
	# Propagate codex's own exit status (timeout/error), not the codex_text pipe's 0, so a
	# consult failure is observable like claude/gemini's instead of always looking successful.
	exit "$st"
	;;
*) die "unknown peer: $peer" ;;
esac
`
