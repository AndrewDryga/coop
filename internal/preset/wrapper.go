package preset

import (
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// DelegateWrapperPath is where coop mounts the coop-delegate script inside the box —
// on PATH, beside coop-consult, so the lead invokes it as a bare `coop-delegate`.
const DelegateWrapperPath = "/usr/local/bin/coop-delegate"

// DelegateDepthEnv marks a provider process launched by coop-delegate. The wrapper rejects another
// delegate call while it is set, bounding write-capable delegation to one level.
const DelegateDepthEnv = "COOP_DELEGATE_DEPTH"

// EnvKey renders a role name as the env-var infix the wrapper resolves:
// role "fast" → COOP_DELEGATE_FAST_TARGETS / _CONTRACT.
func EnvKey(role string) string {
	return strings.ToUpper(strings.ReplaceAll(role, "-", "_"))
}

// delegateArm renders one `<name>) <body> ;;` write-capable dispatch arm.
func delegateArm(name, body string) string {
	return name + ") " + body + " ;;\n"
}

// DelegateWrapper is the `coop-delegate` script coop mounts when a preset declares a
// mode: delegate role. It runs the role's configured agent WRITE-CAPABLE against the
// shared worktree, with the role's contract prepended to the prompt, and enforces the
// guarantees the host can't watch from outside the box:
//   - commit: never — HEAD is recorded before and after; if the delegate committed,
//     the wrapper exits non-zero and says so (no auto-reset: fail loud, leave evidence).
//   - concurrent: never — a global mkdir lock serializes delegate runs; a second call
//     WAITS for the lock (less surprising for an agent caller than failing fast).
//   - one level — a delegate child receives COOP_DELEGATE_DEPTH=1, and another wrapper
//     invocation fails before reading input or acquiring the serialization lock.
//
// The role's target ladder/contract come from COOP_DELEGATE_<ROLE>_* env vars exported
// by host coop (see box.Run), so the wrapper needs no YAML parser in the box. These
// are coordination guarantees for leads that route through the wrapper — not an OS
// permission layer.
//
// The per-agent write-capable dispatch comes from each adapter's DelegateExec, so adding
// a provider needs no edit here.
func DelegateWrapper() string { return renderDelegate(registeredDelegates()) }

// delegateInput is the narrow slice of an Agent the delegate generator needs — so a drift
// test can pass a fake 4th agent without the whole Agent interface.
type delegateInput interface {
	Name() string
	DelegateExec() string
}

func registeredDelegates() []delegateInput {
	names := agents.Names() // sorted → deterministic script
	out := make([]delegateInput, 0, len(names))
	for _, n := range names {
		if a, ok := agents.Get(n); ok {
			out = append(out, a)
		}
	}
	return out
}

func renderDelegate(as []delegateInput) string {
	var arms strings.Builder
	names := make([]string, 0, len(as))
	for _, a := range as {
		names = append(names, a.Name())
		arms.WriteString(delegateArm(a.Name(), a.DelegateExec()))
	}
	wrapper := strings.Replace(delegateWrapperTmpl, "@@RATE_LIMIT@@\n", agents.ShellRateLimitDetector(), 1)
	wrapper = strings.Replace(wrapper, "@@AGENTS@@\n", strings.Join(names, "|")+") ;;\n", 1)
	return strings.Replace(wrapper, "@@ARMS@@\n", arms.String(), 1)
}

const delegateWrapperTmpl = `#!/bin/sh
# coop-delegate — hand a write-capable delegate role one implementation task.
# Generated and mounted by coop from the active preset; do not edit.
#   coop-delegate <role> [prompt]
# The prompt is the trailing argument, or piped on stdin (use a quoted heredoc).
# The delegate MAY edit the worktree; it must NOT commit — the lead reviews the
# diff, runs the gate, and owns the commit. Runs are serialized via a global lock.
# A role target ladder advances only after a proven non-zero rate-limit failure,
# and only while the Git worktree remains clean and HEAD is unchanged.
set -u
umask 077

die() { echo "coop-delegate: $1" >&2; exit 2; }
case "${COOP_DELEGATE_DEPTH:-0}" in
'' | 0) ;;
*) die "recursive delegation is not allowed — return the work to the lead" ;;
esac
COOP_DELEGATE_DEPTH=1
export COOP_DELEGATE_DEPTH
[ "$#" -ge 1 ] || die "usage: coop-delegate <role> [prompt]"
role=$1
shift
case "$role" in
'' | *[!a-z0-9-]*) die "invalid role name: $role" ;;
esac
key=$(printf '%s' "$role" | tr 'a-z-' 'A-Z_')
eval "targets=\${COOP_DELEGATE_${key}_TARGETS:-}"
eval "contract=\${COOP_DELEGATE_${key}_CONTRACT:-}"
[ -n "$targets" ] || die "unknown delegate role: $role (delegate roles come from the preset's mode: delegate entries)"

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
attempt_dir=${TMPDIR:-/tmp}/coop-delegate-${key}-$$
if ! mkdir "$attempt_dir"; then
	rmdir "$lock" 2>/dev/null || true
	die "cannot create attempt directory: $attempt_dir"
fi
trap 'rm -rf "$attempt_dir"; rmdir "$lock" 2>/dev/null || true' EXIT INT TERM

@@RATE_LIMIT@@

load_target() {
	target=$1
	effort=
	case "$target" in
	*/*) effort=${target##*/} ;;
	esac
	head=${target%%/*}
	model=
	case "$head" in
	*:*) agent=${head%%:*}; model=${head#*:} ;;
	*) agent=$head ;;
	esac
}

resolve_defaults() {
	peerkey=$(printf '%s' "$agent" | tr 'a-z-' 'A-Z_')
	if [ -z "$model" ]; then eval "model=\${COOP_PEER_MODEL_${peerkey}:-}"; fi
	if [ -z "$effort" ]; then eval "effort=\${COOP_PEER_EFFORT_${peerkey}:-}"; fi
}

snapshot_tree() {
	tar --exclude='./.git' --exclude='./.git/*' -cf "$1" . 2>/dev/null
}

snapshot_reflog() {
	git reflog --all --format='%H %gD %gs' >"$1" 2>/dev/null
}

# Split only host-validated target tokens. Their grammar forbids whitespace and globs.
set -f
# shellcheck disable=SC2086
set -- $targets
total=$#
[ "$total" -gt 0 ] || die "delegate role $role has an empty target ladder"
for target do
	load_target "$target"
	case "$agent" in
@@AGENTS@@
	*) die "role $role names unknown agent: $agent" ;;
	esac
	case " ${COOP_PRIMARY:-} ${COOP_PEERS:-} " in
*" $agent "*) available_targets="${available_targets:-} $target" ;;
	*) echo "[coop-delegate $role: skipping $target — provider credentials are not mounted]" >&2 ;;
	esac
done
targets=${available_targets# }
# shellcheck disable=SC2086
set -- $targets
total=$#
[ "$total" -gt 0 ] || die "delegate role $role has no target with mounted credentials"

before=$(git rev-parse HEAD 2>/dev/null || echo none)
baseline_state=not-needed
if [ "$total" -gt 1 ]; then
	baseline_state=unknown
	if baseline_status=$(git status --porcelain --untracked-files=all 2>/dev/null); then
		if [ -n "$baseline_status" ]; then
			baseline_state=dirty
		elif snapshot_tree "$attempt_dir/tree-before.tar" && snapshot_reflog "$attempt_dir/reflog-before"; then
			baseline_state=clean
		fi
	fi
fi

index=0
for target do
	index=$((index + 1))
	load_target "$target"
	resolve_defaults
	out=$attempt_dir/output-$index
	status=$attempt_dir/status-$index
	# The provider arm runs in a child so an adapter exit cannot skip recording
	# its status. tee keeps the delegate's output live while preserving it for proof.
	{
		(
			case "$agent" in
@@ARMS@@
			*) exit 2 ;;
			esac
		)
		printf '%s\n' "$?" >"$status"
	} 2>&1 | tee "$out"
	st=$(cat "$status" 2>/dev/null || printf 1)
	case "$st" in '' | *[!0-9]*) st=1 ;; esac
	after=$(git rev-parse HEAD 2>/dev/null || echo none)

	if [ "$before" != "$after" ]; then
		echo "coop-delegate: $role COMMITTED ($before -> $after) despite commit: never — inspect 'git log', decide whether to keep or reset it, and own the history" >&2
		exit 3
	fi
	if [ "$st" -eq 0 ]; then
		echo "[coop-delegate $role: done on $target — review 'git diff', run the gate, then commit yourself]"
		exit 0
	fi
	if ! coop_rate_limited "$out" || [ "$index" -ge "$total" ]; then
		echo "[coop-delegate $role: FAILED on $target (exit $st) — read its output above before retrying]" >&2
		exit "$st"
	fi
	if [ "$baseline_state" = dirty ]; then
		echo "[coop-delegate $role: $target was rate limited, but fallback is unsafe because the worktree was already dirty; inspect it before retrying]" >&2
		exit "$st"
	fi
	if [ "$baseline_state" != clean ]; then
		echo "[coop-delegate $role: $target was rate limited, but fallback stopped because the initial Git/filesystem state could not be verified]" >&2
		exit "$st"
	fi
	if ! post_status=$(git status --porcelain --untracked-files=all 2>/dev/null); then
		echo "[coop-delegate $role: $target was rate limited, but fallback stopped because Git status failed]" >&2
		exit "$st"
	fi
	if [ -n "$post_status" ]; then
		echo "[coop-delegate $role: $target was rate limited after changing the worktree; fallback stopped — inspect 'git diff' before retrying]" >&2
		exit "$st"
	fi
	if ! snapshot_tree "$attempt_dir/tree-after.tar" ||
		! snapshot_reflog "$attempt_dir/reflog-after"; then
		echo "[coop-delegate $role: $target was rate limited, but fallback stopped because the final Git/filesystem state could not be verified]" >&2
		exit "$st"
	fi
	if ! cmp -s "$attempt_dir/tree-before.tar" "$attempt_dir/tree-after.tar" ||
		! cmp -s "$attempt_dir/reflog-before" "$attempt_dir/reflog-after"; then
		echo "[coop-delegate $role: $target was rate limited after changing ignored files or Git history; fallback stopped — inspect the worktree before retrying]" >&2
		exit "$st"
	fi
	echo "[coop-delegate $role: $target rate limited — trying fallback $((index + 1))/$total]" >&2
done

exit 2
`
