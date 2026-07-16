package preset

import (
	"strconv"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// DelegateWrapperPath is where coop mounts the coop-delegate script inside the box —
// on PATH, beside coop-consult, so the lead invokes it as a bare `coop-delegate`.
const DelegateWrapperPath = "/usr/local/bin/coop-delegate"

// DelegateDepthEnv marks a provider process launched by coop-delegate. The wrapper rejects another
// delegate call while it is set, bounding write-capable delegation to one level.
const DelegateDepthEnv = "COOP_DELEGATE_DEPTH"

const (
	delegateStreamLimitBytes = 1 << 20
	delegatePromptLimitBytes = 96 << 10
	delegateSnapshotBlocks   = 64 << 10
)

// EnvKey renders a role name as the env-var infix the wrapper resolves:
// role "fast" → COOP_DELEGATE_FAST_TARGETS / _CONTRACT.
func EnvKey(role string) string {
	return strings.ToUpper(strings.ReplaceAll(role, "-", "_"))
}

// delegateArm renders one `<name>) <body> ;;` write-capable dispatch arm.
func delegateArm(name, body string) string {
	return name + ") run_delegate " + body + " ;;\n"
}

// DelegateWrapper is the `coop-delegate` script coop mounts when a preset declares a
// mode: delegate role. It runs the role's configured agent WRITE-CAPABLE against the
// shared worktree, with the role's contract prepended to the prompt, and enforces the
// guarantees the host can't watch from outside the box:
//   - commit: never — HEAD, refs, and reflogs are recorded before and after; if the
//     delegate mutates history, the wrapper exits non-zero and leaves the evidence.
//   - concurrent: never — a kernel flock serializes delegate runs; a second call waits
//     for the bounded lock deadline.
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
// test can pass a fake future agent without the whole Agent interface.
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
	wrapper := strings.NewReplacer(
		"@@STREAM_LIMIT@@", strconv.Itoa(delegateStreamLimitBytes),
		"@@PROMPT_LIMIT@@", strconv.Itoa(delegatePromptLimitBytes),
		"@@SNAPSHOT_BLOCKS@@", strconv.Itoa(delegateSnapshotBlocks),
	).Replace(delegateWrapperTmpl)
	wrapper = strings.Replace(wrapper, "@@RATE_LIMIT@@\n", agents.ShellRateLimitDetector(), 1)
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
# and only while the Git/filesystem snapshot and Git history remain unchanged.
set -u
umask 077
delegate_stream_limit=@@STREAM_LIMIT@@
delegate_prompt_limit=@@PROMPT_LIMIT@@
delegate_snapshot_blocks=@@SNAPSHOT_BLOCKS@@
delegate_timeout=${COOP_DELEGATE_TIMEOUT:-1800}
case "$delegate_timeout" in '' | *[!0-9]*) echo "coop-delegate: COOP_DELEGATE_TIMEOUT must be whole seconds" >&2; exit 2 ;; esac
[ "$delegate_timeout" -ge 1 ] && [ "$delegate_timeout" -le 86400 ] || {
	echo "coop-delegate: COOP_DELEGATE_TIMEOUT must be within 1..86400 seconds" >&2
	exit 2
}

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

attempt_dir=${TMPDIR:-/tmp}/coop-delegate-${key}-$$
mkdir "$attempt_dir" || die "cannot create attempt directory: $attempt_dir"
active_pid_file=$attempt_dir/active-pid

terminate_active() {
	[ -f "$active_pid_file" ] || return 0
	pid=$(cat "$active_pid_file" 2>/dev/null || :)
	case "$pid" in '' | *[!0-9]*) rm -f "$active_pid_file"; return 0 ;; esac
	kill -TERM "-$pid" 2>/dev/null || :
	sleep 1
	kill -KILL "-$pid" 2>/dev/null || :
	rm -f "$active_pid_file"
}

cleanup() {
	trap - EXIT INT TERM
	terminate_active
	rm -rf "$attempt_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

question_file=$attempt_dir/question
if [ "$#" -gt 0 ]; then
	printf '%s' "$*" >"$question_file" || die "cannot capture delegate prompt"
else
	head -c $((delegate_prompt_limit + 1)) >"$question_file" || die "cannot read delegate prompt"
fi
question_bytes=$(wc -c <"$question_file" | tr -d '[:space:]')
case "$question_bytes" in '' | *[!0-9]*) die "cannot measure delegate prompt" ;; esac
[ "$question_bytes" -le "$delegate_prompt_limit" ] || die "prompt exceeds ${delegate_prompt_limit} bytes; send a smaller self-contained task"
[ "$question_bytes" -gt 0 ] || die "empty prompt — pass it as an argument or on stdin"

prompt_file=$attempt_dir/prompt
if [ -n "$contract" ]; then
	[ -r "$contract" ] || die "delegate contract is unreadable: $contract"
	contract_file=$attempt_dir/contract
	head -c $((delegate_prompt_limit + 1)) "$contract" >"$contract_file" || die "cannot read delegate contract"
	contract_bytes=$(wc -c <"$contract_file" | tr -d '[:space:]')
	case "$contract_bytes" in '' | *[!0-9]*) die "cannot measure delegate contract" ;; esac
	[ "$contract_bytes" -le "$delegate_prompt_limit" ] || die "delegate contract exceeds ${delegate_prompt_limit} bytes"
	{
		cat "$contract_file"
		printf '\n\n---\n\nYour task:\n\n'
		cat "$question_file"
	} >"$prompt_file" || die "cannot construct delegate prompt"
else
	cp "$question_file" "$prompt_file" || die "cannot construct delegate prompt"
fi
prompt_bytes=$(wc -c <"$prompt_file" | tr -d '[:space:]')
case "$prompt_bytes" in '' | *[!0-9]*) die "cannot measure constructed delegate prompt" ;; esac
[ "$prompt_bytes" -le "$delegate_prompt_limit" ] || die "constructed prompt exceeds ${delegate_prompt_limit} bytes; shorten the role contract or task"
prompt=$(cat "$prompt_file")
# The prompt is captured; detach stdin so a backgrounded delegate cannot block on it.
exec </dev/null

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

snapshot_tree() {
	(
		ulimit -f "$delegate_snapshot_blocks" || exit 1
		timeout -k 30 30 tar --exclude='./.git' --exclude='./.git/*' -cf "$1" . 2>/dev/null
	)
}

snapshot_reflog() {
	git reflog --all --format='%H %gD %gs' >"$1" 2>/dev/null
}

snapshot_refs() {
	git for-each-ref --sort=refname --format='%(refname) %(objectname) %(*objectname)' >"$1" 2>/dev/null
}

lock_file_safe() {
	f=$1
	[ ! -L "$f" ] && [ -f "$f" ] || die "unsafe delegate lock: $f"
	links=$(stat -c %h "$f" 2>/dev/null || stat -f %l "$f" 2>/dev/null || :)
	case "$links" in 1) ;; *) die "delegate lock must have one hardlink: $f" ;; esac
	permissions=$(stat -c %a "$f" 2>/dev/null || stat -f %Lp "$f" 2>/dev/null || :)
	case "$permissions" in 600) ;; *) die "delegate lock must be mode 0600: $f" ;; esac
}

run_delegate() {
	setsid timeout -k 30 "$delegate_timeout" "$@" &
	pid=$!
	if ! printf '%s\n' "$pid" >"$active_pid_file"; then
		kill -TERM "-$pid" 2>/dev/null || :
		sleep 1
		kill -KILL "-$pid" 2>/dev/null || :
		wait "$pid" 2>/dev/null || :
		return 125
	fi
	wait "$pid"
	st=$?
	# A provider that exits while leaving a writer behind would otherwise hold output open and let
	# work continue after the serialization lock is released.
	if kill -0 "-$pid" 2>/dev/null; then
		kill -TERM "-$pid" 2>/dev/null || :
		sleep 1
		kill -KILL "-$pid" 2>/dev/null || :
		st=125
		echo "[coop-delegate $role: $target left a background process behind — terminated]" >&2
	fi
	rm -f "$active_pid_file"
	return "$st"
}

bounded_capture() {
	destination=$1
	overflow=$2
	chunk=$3
	: >"$destination"
	rm -f "$overflow" "$chunk"
	total=0
	while [ "$total" -lt "$delegate_stream_limit" ]; do
		remaining=$((delegate_stream_limit - total))
		block=65536
		if [ "$remaining" -lt "$block" ]; then block=$remaining; fi
		dd bs="$block" count=1 of="$chunk" 2>/dev/null || return 1
		got=$(wc -c <"$chunk" | tr -d '[:space:]')
		case "$got" in '' | *[!0-9]*) got=0 ;; esac
		[ "$got" -gt 0 ] || break
		cat "$chunk" >>"$destination" || return 1
		cat "$chunk" || return 1
		total=$((total + got))
	done
	if [ "$total" -ge "$delegate_stream_limit" ]; then
		dd bs=1 count=1 of="$chunk" 2>/dev/null || return 1
		got=$(wc -c <"$chunk" | tr -d '[:space:]')
		case "$got" in '' | *[!0-9]*) got=0 ;; esac
		if [ "$got" -gt 0 ]; then
			: >"$overflow" || return 1
			cat >/dev/null || return 1
		fi
	fi
	rm -f "$chunk" || return 1
}

# Split only host-validated target tokens. Their grammar forbids whitespace and globs.
set -f
# shellcheck disable=SC2086
set -- $targets
total=$#
[ "$total" -gt 0 ] || die "delegate role $role has an empty target ladder"
available_targets=
for target do
	load_target "$target"
	case "$agent" in
@@AGENTS@@
	*) die "role $role names unknown agent: $agent" ;;
	esac
	case " ${COOP_PRIMARY:-} ${COOP_PEERS:-} " in
*" $agent "*) available_targets="$available_targets $target" ;;
	*) echo "[coop-delegate $role: skipping $target — provider credentials are not mounted]" >&2 ;;
	esac
done
targets=${available_targets# }
# shellcheck disable=SC2086
set -- $targets
total=$#
[ "$total" -gt 0 ] || die "delegate role $role has no target with mounted credentials"

# concurrent: never. The Coop image guarantees util-linux flock, whose kernel ownership cannot
# strand later delegates after an unclean process exit. A custom image missing it fails loudly.
command -v flock >/dev/null 2>&1 || die "flock is required for crash-safe delegate serialization"
command -v setsid >/dev/null 2>&1 || die "setsid is required for bounded delegate process cleanup"
command -v timeout >/dev/null 2>&1 || die "timeout is required for bounded delegate execution"
lock=${COOP_DELEGATE_LOCK:-${TMPDIR:-/tmp}/coop-delegate.lock}
[ ! -L "$lock" ] || die "unsafe delegate lock: $lock"
if [ ! -e "$lock" ]; then (set -C; : >"$lock") 2>/dev/null || :; fi
lock_file_safe "$lock"
exec 8<>"$lock" || die "cannot open delegate lock: $lock"
lock_file_safe "$lock"
lock_fd=/dev/fd/8
[ -e "$lock_fd" ] || lock_fd=/proc/self/fd/8
lock_path_identity=$(stat -c 'gnu:%d:%i' "$lock" 2>/dev/null || stat -f 'bsd:%i' "$lock" 2>/dev/null || :)
lock_fd_identity=$(stat -Lc 'gnu:%d:%i' "$lock_fd" 2>/dev/null || stat -Lf 'bsd:%i' "$lock_fd" 2>/dev/null || :)
[ -n "$lock_path_identity" ] && [ "$lock_path_identity" = "$lock_fd_identity" ] || die "delegate lock changed while opening: $lock"
lock_limit=$(((delegate_timeout + 35) * total + 5))
flock -w "$lock_limit" 8 || die "timed out waiting for another delegate to finish"

before=$(git rev-parse --verify 'HEAD^{commit}' 2>/dev/null) || die "cannot verify Git HEAD before delegate dispatch"
log_updates=$(git config --bool core.logAllRefUpdates 2>/dev/null || :)
[ "$log_updates" != false ] || die "Git reflogs are disabled; commit: never cannot be verified"
reflog_head=$(git rev-parse --verify 'HEAD@{0}^{commit}' 2>/dev/null) || die "cannot verify the Git HEAD reflog before delegate dispatch"
[ "$reflog_head" = "$before" ] || die "Git HEAD and its reflog disagree before delegate dispatch"
snapshot_reflog "$attempt_dir/reflog-before" || die "cannot verify Git reflog before delegate dispatch"
snapshot_refs "$attempt_dir/refs-before" || die "cannot verify Git refs before delegate dispatch"
baseline_state=unknown
if [ "$total" -gt 1 ]; then
	if baseline_status=$(git status --porcelain --untracked-files=all 2>/dev/null); then
		if [ -n "$baseline_status" ]; then
			baseline_state=dirty
		elif snapshot_tree "$attempt_dir/tree-before.tar"; then
			baseline_state=clean
		fi
	fi
fi

index=0
for target do
	index=$((index + 1))
	load_target "$target"
	out=$attempt_dir/output-$index
	status=$attempt_dir/status-$index
	overflow=$attempt_dir/output-overflow-$index
	capture_status=$attempt_dir/capture-status-$index
	chunk=$attempt_dir/output-chunk-$index
	# The provider arm runs in a child so an adapter exit cannot skip recording
	# its status. The bounded drain keeps initial output live and preserves it for proof. The
	# provider subtree retains flock FD 8 until all writers exit; the capture child closes it.
	{
		(
			case "$agent" in
@@ARMS@@
			*) exit 2 ;;
			esac
		)
		printf '%s\n' "$?" >"$status"
	} 2>&1 | {
		bounded_capture "$out" "$overflow" "$chunk"
		printf '%s\n' "$?" >"$capture_status"
	} 8>&-
	st=$(cat "$status" 2>/dev/null || printf 1)
	case "$st" in '' | *[!0-9]*) st=1 ;; esac
	captured=$(cat "$capture_status" 2>/dev/null || printf 1)
	case "$captured" in '' | *[!0-9]*) captured=1 ;; esac
	after=$(git rev-parse --verify 'HEAD^{commit}' 2>/dev/null) || {
		echo "coop-delegate: $role changed or removed Git HEAD despite commit: never — inspect the repository and own the history" >&2
		exit 3
	}
	if ! snapshot_reflog "$attempt_dir/reflog-after" ||
		! snapshot_refs "$attempt_dir/refs-after"; then
		echo "coop-delegate: $role left Git history unverifiable despite commit: never — inspect the repository and own the history" >&2
		exit 3
	fi

	if [ "$before" != "$after" ]; then
		echo "coop-delegate: $role COMMITTED ($before -> $after) despite commit: never — inspect 'git log', decide what to keep, and own the history" >&2
		exit 3
	fi
	if ! cmp -s "$attempt_dir/refs-before" "$attempt_dir/refs-after" ||
		! cmp -s "$attempt_dir/reflog-before" "$attempt_dir/reflog-after"; then
		echo "coop-delegate: $role changed Git history or refs despite commit: never — inspect 'git log' and 'git reflog', decide what to keep, and own the history" >&2
		exit 3
	fi
	if [ "$captured" -ne 0 ]; then
		echo "[coop-delegate $role: failed to capture bounded provider output — fallback stopped]" >&2
		exit 1
	fi
	if [ -f "$overflow" ]; then
		echo "[coop-delegate $role: output exceeded ${delegate_stream_limit} bytes — partial output was shown and fallback stopped]" >&2
		exit 1
	fi
	if [ "$st" -eq 0 ]; then
		echo "[coop-delegate $role: done on $target — review 'git status --short', 'git diff', and 'git diff --cached'; run the gate, then commit yourself]"
		exit 0
	fi
	case "$st" in
	124|125|126|127|137)
		echo "[coop-delegate $role: bounded execution failed on $target (exit $st) — fallback stopped]" >&2
		exit "$st"
		;;
	esac
	if ! coop_rate_limited "$out" || [ "$index" -ge "$total" ]; then
		echo "[coop-delegate $role: FAILED on $target (exit $st) — read its output above before retrying]" >&2
		exit "$st"
	fi
	if [ "$baseline_state" = dirty ]; then
		echo "[coop-delegate $role: $target was rate limited, but fallback is unsafe because the worktree was already dirty; inspect it before retrying]" >&2
		exit "$st"
	fi
	if [ "$baseline_state" != clean ]; then
		echo "[coop-delegate $role: $target was rate limited, but fallback stopped because the initial Git status or filesystem snapshot could not be verified (snapshot size/time bounds apply)]" >&2
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
	if ! snapshot_tree "$attempt_dir/tree-after.tar"; then
		echo "[coop-delegate $role: $target was rate limited, but fallback stopped because the final Git/filesystem snapshot exceeded its size/time bound or could not be read]" >&2
		exit "$st"
	fi
	if ! cmp -s "$attempt_dir/tree-before.tar" "$attempt_dir/tree-after.tar"; then
		echo "[coop-delegate $role: $target was rate limited after changing ignored files; fallback stopped — inspect the worktree before retrying]" >&2
		exit "$st"
	fi
	echo "[coop-delegate $role: $target rate limited — trying fallback $((index + 1))/$total]" >&2
done
`
