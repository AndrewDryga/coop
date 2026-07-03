package preset

import "strings"

// DelegateWrapperPath is where coop mounts the coop-delegate script inside the box —
// on PATH, beside coop-consult, so the lead invokes it as a bare `coop-delegate`.
const DelegateWrapperPath = "/usr/local/bin/coop-delegate"

// EnvKey renders a role name as the env-var infix the wrapper resolves:
// role "fast" → COOP_DELEGATE_FAST_AGENT / _MODEL / _CONTRACT.
func EnvKey(role string) string {
	return strings.ToUpper(strings.ReplaceAll(role, "-", "_"))
}

// DelegateWrapper is the `coop-delegate` script coop mounts when a preset declares a
// mode: delegate role. It runs the role's configured agent WRITE-CAPABLE against the
// shared worktree, with the role's contract prepended to the prompt, and enforces the
// two v1 guarantees the host can't watch from outside the box:
//   - commit: never — HEAD is recorded before and after; if the delegate committed,
//     the wrapper exits non-zero and says so (no auto-reset: fail loud, leave evidence).
//   - concurrent: never — a global mkdir lock serializes delegate runs; a second call
//     WAITS for the lock (less surprising for an agent caller than failing fast).
//
// The role's agent/model/contract come from COOP_DELEGATE_<ROLE>_* env vars exported
// by host coop (see box.Run), so the wrapper needs no YAML parser in the box. These
// are coordination guarantees for leads that route through the wrapper — not an OS
// permission layer.
const DelegateWrapper = `#!/bin/sh
# coop-delegate — hand a write-capable delegate role one implementation task.
# Generated and mounted by coop from the active preset; do not edit.
#   coop-delegate <role> [prompt]
# The prompt is the trailing argument, or piped on stdin (use a quoted heredoc).
# The delegate MAY edit the worktree; it must NOT commit — the lead reviews the
# diff, runs the gate, and owns the commit. Runs are serialized via a global lock.
set -u

die() { echo "coop-delegate: $1" >&2; exit 2; }
[ "$#" -ge 1 ] || die "usage: coop-delegate <role> [prompt]"
role=$1
shift
case "$role" in
'' | *[!a-z0-9-]*) die "invalid role name: $role" ;;
esac
key=$(printf '%s' "$role" | tr 'a-z-' 'A-Z_')
eval "agent=\${COOP_DELEGATE_${key}_AGENT:-}"
eval "model=\${COOP_DELEGATE_${key}_MODEL:-}"
eval "contract=\${COOP_DELEGATE_${key}_CONTRACT:-}"
[ -n "$agent" ] || die "unknown delegate role: $role (delegate roles come from the preset's mode: delegate entries)"

prompt=${*:-}
[ -n "$prompt" ] || prompt=$(cat)
[ -n "$prompt" ] || die "empty prompt — pass it as an argument or on stdin"
if [ -n "$contract" ] && [ -r "$contract" ]; then
	prompt=$(cat "$contract"; printf '\n\n---\n\nYour task:\n\n%s' "$prompt")
fi
# The prompt is captured; detach stdin so a backgrounded delegate can't block on it.
exec </dev/null

# concurrent: never — one delegate at a time. Wait for the lock rather than fail:
# an agent caller that fans out learns serialization, not a transient error.
lock=${COOP_DELEGATE_LOCK:-/tmp/coop-delegate.lock}
while ! mkdir "$lock" 2>/dev/null; do sleep 1; done
trap 'rmdir "$lock" 2>/dev/null' EXIT INT TERM

before=$(git rev-parse HEAD 2>/dev/null || echo none)
case "$agent" in
claude) claude -p --dangerously-skip-permissions ${model:+--model "$model"} "$prompt" ;;
gemini) gemini --yolo ${model:+--model "$model"} -p "$prompt" ;;
codex) codex exec --dangerously-bypass-approvals-and-sandbox ${model:+--model "$model"} "$prompt" ;;
*) die "role $role names unknown agent: $agent" ;;
esac
st=$?
after=$(git rev-parse HEAD 2>/dev/null || echo none)

if [ "$before" != "$after" ]; then
	echo "coop-delegate: $role COMMITTED ($before -> $after) despite commit: never — inspect 'git log', decide whether to keep or reset it, and own the history" >&2
	exit 3
fi
if [ "$st" -eq 0 ]; then
	echo "[coop-delegate $role: done — review 'git diff', run the gate, then commit yourself]"
else
	echo "[coop-delegate $role: FAILED (exit $st) — read its output above before retrying]" >&2
fi
exit "$st"
`
