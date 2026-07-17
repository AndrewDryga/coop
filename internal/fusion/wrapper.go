package fusion

import (
	"sort"
	"strconv"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// ConsultWrapperPath is where coop mounts the coop-consult script inside the box — on
// PATH, so the governor/lead invokes it as a bare `coop-consult`.
const ConsultWrapperPath = "/usr/local/bin/coop-consult"

const (
	consultStreamLimitBytes  = 1 << 20
	consultPromptLimitBytes  = 512 << 10
	consultContextLimitBytes = 512 << 10
)

// ConsultWrapper renders the `coop-consult` script coop mounts into every fusion and
// --consult box. It gives the lead a uniform `coop-consult <peer> --fresh|--continue`
// interface over each registered peer's read-only consult command, hiding the per-agent
// session-id mechanics. Each adapter owns its fresh/resume commands and any stream prelude, so
// provider-specific continuity stays out of this registry-derived wrapper.
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
		a, _ := agents.Get(n) // Names is derived from the same registry.
		out = append(out, a)
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
# <peer> is ` + peerList + ` (fusion / --peer ad-hoc). A preset CONSULT ROLE — or a
# native role degraded under a non-Claude lead — is addressed by its ROLE name. Its
# COOP_CONSULT_<ROLE>_TARGETS value is an ordered fallback ladder; each target remains
# READ-ONLY. --fresh starts at rung one. --continue resumes the successful rung and,
# if that provider is now rate limited, starts the next provider fresh. Each rung is
# attempted once and each attempt is time-bounded (default 30m; COOP_CONSULT_TIMEOUT).
set -u
umask 077

consult_stream_limit=` + strconv.Itoa(consultStreamLimitBytes) + `
consult_prompt_limit=` + strconv.Itoa(consultPromptLimitBytes) + `
consult_context_limit=` + strconv.Itoa(consultContextLimitBytes) + `

die() { echo "coop-consult: $1" >&2; exit 2; }
[ "$#" -ge 2 ] || die "usage: coop-consult <peer|role> <--fresh|--continue> [prompt]"
name=$1
mode=$2
shift 2
# Validate the target up front, before it is used in the state path below — a bogus name
# would otherwise build a stray/traversed /tmp path and make --continue's line lie.
case "$name" in
'' | *[!a-z0-9-]*) die "invalid consult target: $name (letters, digits, dashes)" ;;
esac
case "$mode" in
--fresh | --continue) ;;
*) die "mode must be --fresh or --continue" ;;
esac
fresh_retry="coop-consult $name --fresh \"<full prompt>\""
continue_retry="coop-consult $name --continue \"<delta>\""
key=$(printf '%s' "$name" | tr 'a-z-' 'A-Z_')
consult_timeout=${COOP_CONSULT_TIMEOUT:-1800}
case "$consult_timeout" in '' | *[!0-9]*) die "COOP_CONSULT_TIMEOUT must be whole seconds" ;; esac
[ "$consult_timeout" -ge 1 ] && [ "$consult_timeout" -le 86400 ] || die "COOP_CONSULT_TIMEOUT must be within 1..86400 seconds"

state_dir=${TMPDIR:-/tmp}/coop-consult-state
[ ! -L "$state_dir" ] || die "state path is a symlink: $state_dir"
if [ -e "$state_dir" ]; then
	[ -d "$state_dir" ] || die "state path is not a directory: $state_dir"
else
	mkdir "$state_dir" || die "cannot create state directory: $state_dir"
fi
chmod 700 "$state_dir" || die "cannot secure state directory: $state_dir"
statefile="$state_dir/${key}.state"
lockfile="$state_dir/${key}.lock"
attempt_dir=${TMPDIR:-/tmp}/coop-consult-${key}-$$
mkdir "$attempt_dir" || die "cannot create attempt directory: $attempt_dir"
contextfile=$attempt_dir/context
: >"$contextfile" || die "cannot create attempt context"
chmod 600 "$contextfile" || die "cannot secure attempt context"
active_run_pid=
active_run_group=0
output_pid=
diagnostics_pid=
codex_capture_pid=
	lock_owned=0
	lockdir=
	candidate_telemetry_raw=

new_id() {
	if [ -r /proc/sys/kernel/random/uuid ]; then
		cat /proc/sys/kernel/random/uuid
	else
		od -An -tx1 -N16 /dev/urandom | tr -d ' \n' |
			sed 's/\(........\)\(....\)\(....\)\(....\)\(............\)/\1-\2-\3-\4-\5/'
	fi
}

state_file_safe() {
	f=$1
	[ ! -e "$f" ] && [ ! -L "$f" ] && return 1
	[ ! -L "$f" ] && [ -f "$f" ] || die "unsafe continuation state file: $f"
	links=$(stat -c %h "$f" 2>/dev/null || stat -f %l "$f" 2>/dev/null || :)
	case "$links" in 1) ;; *) die "continuation state file must have one hardlink: $f" ;; esac
	permissions=$(stat -c %a "$f" 2>/dev/null || stat -f %Lp "$f" 2>/dev/null || :)
	case "$permissions" in 600) ;; *) die "continuation state file must be mode 0600: $f" ;; esac
	return 0
}

lock_file_safe() {
	f=$1
	[ ! -L "$f" ] && [ -f "$f" ] || die "unsafe continuation lock: $f"
	links=$(stat -c %h "$f" 2>/dev/null || stat -f %l "$f" 2>/dev/null || :)
	case "$links" in 1) ;; *) die "continuation lock must have one hardlink: $f" ;; esac
	permissions=$(stat -c %a "$f" 2>/dev/null || stat -f %Lp "$f" 2>/dev/null || :)
	case "$permissions" in 600) ;; *) die "continuation lock must be mode 0600: $f" ;; esac
}

terminate_pid() {
	pid=$1
	[ -n "$pid" ] || return 0
	kill "$pid" 2>/dev/null || :
	wait "$pid" 2>/dev/null || :
}

# Reached from the EXIT/signal cleanup trap.
# shellcheck disable=SC2329
terminate_active_run() {
	[ -n "$active_run_pid" ] || return 0
	if [ "$active_run_group" -eq 1 ]; then
		kill -TERM "-$active_run_pid" 2>/dev/null || :
		sleep 1
		kill -KILL "-$active_run_pid" 2>/dev/null || :
	else
		kill "$active_run_pid" 2>/dev/null || :
	fi
	wait "$active_run_pid" 2>/dev/null || :
	active_run_pid=
	active_run_group=0
}

# Trap entrypoint, not a direct shell call.
# shellcheck disable=SC2329
cleanup() {
	trap - EXIT INT TERM
	terminate_pid "$output_pid"
	terminate_pid "$diagnostics_pid"
	terminate_pid "$codex_capture_pid"
	terminate_active_run
	if [ "$lock_owned" -eq 1 ] && [ -n "$lockdir" ]; then rmdir "$lockdir" 2>/dev/null || :; fi
	rm -rf "$attempt_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

publish_state() {
	published_id=$1
	next_context=$2
	next_state=$attempt_dir/pending-state
	{
		printf 'COOP-CONSULT-STATE 1\n'
		printf 'target=%s\n' "$target"
		printf 'rung=%s\n' "$index"
		printf 'id=%s\n\n' "$published_id"
		cat "$next_context"
	} >"$next_state" || return 1
	chmod 600 "$next_state" || return 1
	mv "$next_state" "$statefile"
}

clear_failed_resume() {
	[ "$dispatch" = resume ] || return 0
	publish_state "" "$contextfile" || die "cannot clear uncertain continuation session"
}

# A preset consult role (or a native role degraded under a non-Claude lead) carries its own
# target ladder/persona via COOP_CONSULT_<ROLE>_*; otherwise the name IS the peer agent.
eval "targets=\${COOP_CONSULT_${key}_TARGETS:-}"
eval "persona=\${COOP_CONSULT_${key}_CONTRACT:-}"
if [ -n "$targets" ]; then
	role=$name
else
	targets=$name
	persona=
	role=
fi
if [ "$#" -gt 0 ]; then
	question=$*
else
	question_file=$attempt_dir/question
	head -c $((consult_prompt_limit + 1)) >"$question_file" || die "cannot read consult question"
	question_file_bytes=$(wc -c <"$question_file" | tr -d '[:space:]')
	[ "$question_file_bytes" -le "$consult_prompt_limit" ] || die "question exceeds ${consult_prompt_limit} bytes; send a smaller self-contained prompt"
	question=$(cat "$question_file")
fi
question_bytes=$(printf '%s' "$question" | wc -c | tr -d '[:space:]')
[ "$question_bytes" -le "$consult_prompt_limit" ] || die "question exceeds ${consult_prompt_limit} bytes; send a smaller self-contained prompt"
# The prompt is captured above, so no peer needs stdin. Detach it: claude -p reads
# piped stdin on top of its arg and blocks forever on an inherited open pipe (the
# governor backgrounds these consults). One redirect covers every peer.
exec </dev/null

load_target() {
	target=$1
	effort=
	case "$target" in
	*/*) effort=${target##*/} ;;
	esac
	head=${target%%/*}
	model=
	case "$head" in
	*:*) peer=${head%%:*}; model=${head#*:} ;;
	*) peer=$head ;;
	esac
}

resolve_defaults() {
	peerkey=$(printf '%s' "$peer" | tr 'a-z-' 'A-Z_')
	# Role targets already carry their provider default when Coop configured one. A remaining
	# blank means that provider's own CLI default, never an unrelated ad-hoc peer override.
	if [ -z "$role" ] && [ -z "$model" ]; then eval "model=\${COOP_PEER_MODEL_${peerkey}:-}"; fi
	if [ -z "$role" ] && [ -z "$effort" ]; then eval "effort=\${COOP_PEER_EFFORT_${peerkey}:-}"; fi
}

load_rung() {
	wanted=$1
	n=0
	set -f
	# shellcheck disable=SC2086
	set -- $targets
	for candidate do
		n=$((n + 1))
		if [ "$n" -eq "$wanted" ]; then load_target "$candidate"; return 0; fi
	done
	return 1
}

# Split only host-validated target tokens. Validate every fallback before the first
# provider runs so a malformed or out-of-scope later rung cannot surprise the lead.
set -f
# shellcheck disable=SC2086
set -- $targets
total=$#
[ "$total" -gt 0 ] || die "consult role $name has an empty target ladder"
available_targets=
for target do
	case "$target" in '' | *[!a-zA-Z0-9._:/-]*) die "invalid target token in $name ladder" ;; esac
	load_target "$target"
	case "$peer" in
` + peerAlt + `) ;;
	*) die "unknown peer in $name ladder: $peer (expected ` + peerList + `)" ;;
	esac
	if [ -n "$role" ]; then scope="${COOP_PRIMARY:-} ${COOP_PEERS:-}"; else scope=${COOP_PEERS:-}; fi
	case " $scope " in
*" $peer "*) available_targets="$available_targets $target" ;;
		*)
			if [ -n "$role" ]; then
				echo "[coop-consult $name: skipping $target — credential is outside this run; ask the operator to run 'coop login $peer' and relaunch, or remove this preset rung]" >&2
			else
				die "peer not in this run's credential scope: $peer; ask the operator to relaunch Coop with '--peer $peer'"
			fi
		;;
	esac
done
targets=${available_targets# }
# shellcheck disable=SC2086
set -- $targets
total=$#
[ "$total" -gt 0 ] || die "consult role $name has no target with mounted credentials"

# Serialize one target's complete continuation transaction. Each admitted fallback can consume one
# provider timeout plus its termination grace. Coop's image provides flock, whose kernel-held lock
# is released even after SIGKILL/OOM. The mkdir fallback keeps custom images usable, but an unclean
# kill there fails closed until its private lock directory is removed.
lock_limit=$(((consult_timeout + 35) * total + 5))
if command -v flock >/dev/null 2>&1; then
	[ ! -L "$lockfile" ] || die "unsafe continuation lock: $lockfile"
	if [ ! -e "$lockfile" ]; then (set -C; : >"$lockfile") 2>/dev/null || :; fi
	lock_file_safe "$lockfile"
	exec 8<>"$lockfile" || die "cannot open continuation lock: $lockfile"
	lock_file_safe "$lockfile"
	lock_fd=/dev/fd/8
	[ -e "$lock_fd" ] || lock_fd=/proc/self/fd/8
	lock_path_identity=$(stat -c 'gnu:%d:%i' "$lockfile" 2>/dev/null || stat -f 'bsd:%i' "$lockfile" 2>/dev/null || :)
	lock_fd_identity=$(stat -Lc 'gnu:%d:%i' "$lock_fd" 2>/dev/null || stat -Lf 'bsd:%i' "$lock_fd" 2>/dev/null || :)
	[ -n "$lock_path_identity" ] && [ "$lock_path_identity" = "$lock_fd_identity" ] || die "continuation lock changed while opening: $lockfile"
	flock -w "$lock_limit" 8 || die "timed out waiting for another $name consult to finish"
else
	lockdir=$lockfile.d
	lock_wait=0
	while ! mkdir "$lockdir" 2>/dev/null; do
		[ ! -L "$lockdir" ] && [ -d "$lockdir" ] || die "unsafe continuation lock: $lockdir"
		lock_wait=$((lock_wait + 1))
		[ "$lock_wait" -le "$lock_limit" ] || die "timed out waiting for another $name consult to finish; remove $lockdir after confirming no consult is running"
		sleep 1
	done
	lock_owned=1
fi

resume_id=
saved_rung=
saved_target=
if [ "$mode" = --fresh ]; then
	rm -f "$statefile" || die "cannot clear prior continuation state"
else
	if state_file_safe "$statefile"; then
		state_bytes=$(wc -c <"$statefile" | tr -d '[:space:]')
		[ "$state_bytes" -le $((consult_context_limit + 2048)) ] || die "saved continuation record is too large; retry with: $fresh_retry"
		header_lines=$(head -n 5 "$statefile" | wc -l | tr -d '[:space:]')
		[ "$header_lines" = 5 ] || die "saved continuation record is incomplete; retry with: $fresh_retry"
		[ "$(sed -n '1p' "$statefile")" = "COOP-CONSULT-STATE 1" ] || die "saved continuation record has an unknown version; retry with: $fresh_retry"
		target_line=$(sed -n '2p' "$statefile")
		rung_line=$(sed -n '3p' "$statefile")
		id_line=$(sed -n '4p' "$statefile")
		[ -z "$(sed -n '5p' "$statefile")" ] || die "saved continuation record has an invalid header; retry with: $fresh_retry"
		case "$target_line" in target=*) saved_target=${target_line#target=} ;; *) die "saved continuation target is invalid; retry with: $fresh_retry" ;; esac
		case "$rung_line" in rung=*) saved_rung=${rung_line#rung=} ;; *) die "saved continuation rung is invalid; retry with: $fresh_retry" ;; esac
		case "$id_line" in id=*) resume_id=${id_line#id=} ;; *) die "saved continuation session is invalid; retry with: $fresh_retry" ;; esac
		case "$saved_target" in '' | *[!a-zA-Z0-9._:/-]*) die "saved continuation target is invalid; retry with: $fresh_retry" ;; esac
		case "$saved_rung" in '' | *[!0-9]*) die "saved continuation rung is invalid; retry with: $fresh_retry" ;; esac
		case "$resume_id" in *[!a-zA-Z0-9._:-]*) die "saved continuation session is invalid; retry with: $fresh_retry" ;; esac
		[ "$(printf '%s' "$resume_id" | wc -c | tr -d '[:space:]')" -le 512 ] || die "saved continuation session is invalid; retry with: $fresh_retry"
		tail -n +6 "$statefile" >"$contextfile" || die "cannot load saved transcript"
		chmod 600 "$contextfile" || die "cannot secure saved transcript"
		context_bytes=$(wc -c <"$contextfile" | tr -d '[:space:]')
		[ "$context_bytes" -le "$consult_context_limit" ] || die "saved transcript exceeds ${consult_context_limit} bytes; retry with: $fresh_retry"
	fi
fi
`)
	b.WriteString(agents.ShellRateLimitDetector())
	if p := preludes(as); p != "" {
		b.WriteString(p + "\n")
	}
	b.WriteString(`
publish_candidate_telemetry() {
	[ -n "$candidate_telemetry_raw" ] && [ -f "$candidate_telemetry_raw" ] || return 0
	if command -v codex_peer_row >/dev/null 2>&1; then
		codex_peer_row "${role:-$name}" "$model" <"$candidate_telemetry_raw"
	fi
	candidate_telemetry_raw=
}

# Bound every consult so a slow or wedged peer cannot stall the lead's wait. The default is 30m.
# The -k grace terminates a peer that ignores SIGTERM; GNU timeout exits 124 (TERM) or 137 (KILL).
run() {
	active_run_group=0
	if command -v setsid >/dev/null 2>&1; then
		setsid timeout -k 30 "$consult_timeout" "$@" &
		active_run_group=1
	else
		timeout -k 30 "$consult_timeout" "$@" &
	fi
	active_run_pid=$!
	run_pid=$active_run_pid
	wait "$run_pid"
	st=$?
	# A CLI that exits while leaving a writer behind would otherwise hold the capture FIFOs open
	# forever. In the Linux box, setsid gives the whole native command a group we can reap.
	if [ "$active_run_group" -eq 1 ] && kill -0 "-$run_pid" 2>/dev/null; then
		kill -TERM "-$run_pid" 2>/dev/null || :
		sleep 1
		kill -KILL "-$run_pid" 2>/dev/null || :
		st=125
		echo "[$peer: provider left a background process behind — terminated; retry with: $fresh_retry]" >&2
	fi
	active_run_pid=
	active_run_group=0
	return "$st"
}

# Drain one producer into a fixed-size spool without ever letting a provider fill the box disk.
# The one-byte read after the cap distinguishes exact-size EOF from overflow; overflow is marked,
# the remaining producer stream is drained to /dev/null, and no partial reply is accepted later.
bounded_capture() {
	destination=$1
	overflow=$2
	chunk=$3
	: >"$destination"
	rm -f "$overflow" "$chunk"
	total=0
	while [ "$total" -lt "$consult_stream_limit" ]; do
		remaining=$((consult_stream_limit - total))
		block=65536
		if [ "$remaining" -lt "$block" ]; then block=$remaining; fi
		dd bs="$block" count=1 of="$chunk" 2>/dev/null || return 1
		got=$(wc -c <"$chunk" | tr -d '[:space:]')
		case "$got" in '' | *[!0-9]*) got=0 ;; esac
		[ "$got" -gt 0 ] || break
		cat "$chunk" >>"$destination" || return 1
		total=$((total + got))
	done
	if [ "$total" -ge "$consult_stream_limit" ]; then
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

start_capture() {
	destination=$1
	overflow=$2
	chunk=$3
	pipe=$4
	status_file=$5
	{
		bounded_capture "$destination" "$overflow" "$chunk"
		printf '%s\n' "$?" >"$status_file"
	} <"$pipe" 8>&- &
	capture_pid=$!
}

await_capture() {
	pid=$1
	status_file=$2
	waits=0
	while [ ! -f "$status_file" ]; do
		waits=$((waits + 1))
		if [ "$waits" -gt 50 ]; then
			terminate_pid "$pid"
			capture_status=1
			return 0
		fi
		sleep 0.1
	done
	wait "$pid" 2>/dev/null || :
	capture_status=$(cat "$status_file" 2>/dev/null || printf 1)
	case "$capture_status" in '' | *[!0-9]*) capture_status=1 ;; esac
}

dispatch_fresh() {
	id=$(new_id)
	case "$peer" in
` + indentArms(fresh.String(), "\t") + `	*) return 2 ;;
	esac
}

dispatch_resume() {
	id=$resume_id
	case "$peer" in
` + indentArms(resume.String(), "\t") + `	*) return 2 ;;
	esac
}

run_attempt() {
	dispatch=$1
	out=$attempt_dir/output-$index
	diagnostics=$attempt_dir/diagnostics-$index
	reply_overflow=$attempt_dir/reply-overflow-$index
	diagnostics_overflow=$attempt_dir/diagnostics-overflow-$index
	output_capture_status_file=$attempt_dir/output-capture-status-$index
	diagnostics_capture_status_file=$attempt_dir/diagnostics-capture-status-$index
	output_pipe=$attempt_dir/output-pipe-$index
	diagnostics_pipe=$attempt_dir/diagnostics-pipe-$index
	candidate_idfile=$attempt_dir/candidate-id-$index
	rm -f "$candidate_idfile" "$reply_overflow" "$diagnostics_overflow" "$output_capture_status_file" "$diagnostics_capture_status_file"
	build_prompt "$dispatch"
	mkfifo "$output_pipe" || die "cannot create bounded output pipe"
	mkfifo "$diagnostics_pipe" || die "cannot create bounded diagnostics pipe"
	start_capture "$out" "$reply_overflow" "$attempt_dir/reply-chunk-$index" "$output_pipe" "$output_capture_status_file"
	output_pid=$capture_pid
	start_capture "$diagnostics" "$diagnostics_overflow" "$attempt_dir/diagnostics-chunk-$index" "$diagnostics_pipe" "$diagnostics_capture_status_file"
	diagnostics_pid=$capture_pid
	# Preserve the provider's real status outside its adapter subshell while both output streams
	# are independently drained into bounded spools. FD 8 is the wrapper's flock and must not keep
	# the lock alive in a provider child if the wrapper is killed.
	if [ "$dispatch" = resume ]; then
		dispatch_resume 8>&- >"$output_pipe" 2>"$diagnostics_pipe"
	else
		dispatch_fresh 8>&- >"$output_pipe" 2>"$diagnostics_pipe"
	fi
	attempt_status=$?
	await_capture "$output_pid" "$output_capture_status_file"
	output_capture_status=$capture_status
	output_pid=
	await_capture "$diagnostics_pid" "$diagnostics_capture_status_file"
	diagnostics_capture_status=$capture_status
	diagnostics_pid=
	rm -f "$output_pipe" "$diagnostics_pipe"
	case "$attempt_status" in '' | *[!0-9]*) attempt_status=1 ;; esac
	attempt_capture_status=0
	if [ "$output_capture_status" -ne 0 ] || [ "$diagnostics_capture_status" -ne 0 ]; then attempt_capture_status=1; fi
}

build_prompt() {
	delivery=$1
	prompt=$question
	if [ "$delivery" = fresh ] && [ "$mode" = --continue ] && state_file_safe "$contextfile" && [ -s "$contextfile" ]; then
		context_bytes=$(wc -c <"$contextfile" | tr -d '[:space:]')
		[ "$context_bytes" -le "$consult_context_limit" ] || die "saved transcript exceeds ${consult_context_limit} bytes; retry with: $fresh_retry"
		prompt=$(printf '%s\n\n' 'Continue this consult from the saved transcript:'; cat "$contextfile"; printf '\n\nCurrent follow-up:\n%s' "$question")
	fi
	# A role's persona is prepended on every provider, including a fresh fallback.
	if [ -n "$persona" ] && [ -r "$persona" ]; then
		persona_bytes=$(wc -c <"$persona" | tr -d '[:space:]')
		[ "$persona_bytes" -le "$consult_prompt_limit" ] || die "persona exceeds ${consult_prompt_limit} bytes: $persona"
		prompt=$(cat "$persona"; printf '\n\n---\n\nYour question:\n\n%s' "$prompt")
	fi
	prompt_bytes=$(printf '%s' "$prompt" | wc -c | tr -d '[:space:]')
	[ "$prompt_bytes" -le "$consult_prompt_limit" ] || die "constructed prompt exceeds ${consult_prompt_limit} bytes; retry with a smaller prompt: $fresh_retry"
}

record_turn() {
	next_context=$attempt_dir/context-$index
	if [ "$mode" = --fresh ] || ! state_file_safe "$contextfile"; then
		: >"$next_context"
	else
		cat "$contextfile" >"$next_context" || return 1
		printf '\n\n---\n\n' >>"$next_context"
	fi
	{
		printf 'Question:\n%s\n\nReply:\n' "$question"
		cat "$out"
	} >>"$next_context"
	next_context_bytes=$(wc -c <"$next_context" | tr -d '[:space:]')
	[ "$next_context_bytes" -le "$consult_context_limit" ] || return 1
	chmod 600 "$next_context" || return 1
	mv "$next_context" "$contextfile"
}

start=1
dispatch=fresh
if [ "$mode" = --continue ] && [ -n "$saved_rung" ]; then start=$saved_rung; fi
if [ "$start" -lt 1 ] || [ "$start" -gt "$total" ] || [ -z "$saved_target" ]; then
	start=1
	resume_id=
	saved_target=
fi
if [ "$mode" = --continue ] && [ -n "$resume_id" ]; then
	load_rung "$start" || die "cannot resolve saved rung $start for $name"
	if [ "$target" = "$saved_target" ]; then
		dispatch=resume
	else
		start=1
		resume_id=
		saved_target=
	fi
fi

index=$start
while [ "$index" -le "$total" ]; do
	load_rung "$index" || die "cannot resolve rung $index for $name"
	resolve_defaults
	if [ "$dispatch" = resume ]; then
		echo "[$peer: resuming on $target — the native session should recall the earlier consult]"
	elif [ "$mode" = --continue ] && [ "$index" -eq "$start" ]; then
		if [ -s "$contextfile" ]; then
			echo "[$peer: --continue had no live session — started FRESH from the saved transcript]"
		else
			echo "[$peer: --continue had no live session — started FRESH, resend full context]"
		fi
	elif [ "$index" -eq 1 ]; then
		echo "[$peer: fresh session]"
	else
		echo "[$peer: starting fallback $index/$total fresh on $target]"
	fi

	run_attempt "$dispatch"
	st=$attempt_status
	if [ "$attempt_capture_status" -ne 0 ]; then
		rm -f "$candidate_idfile"
		clear_failed_resume
		echo "[$peer: failed to capture provider output safely — retry with: $fresh_retry]" >&2
		exit 1
	fi
	if [ "$st" -eq 124 ] || [ "$st" -eq 137 ]; then
		rm -f "$candidate_idfile"
		clear_failed_resume
		echo "[$peer: no reply within ${consult_timeout}s — skipped; synthesize without it]" >&2
		exit "$st"
	fi
	if [ -f "$reply_overflow" ] || [ -f "$diagnostics_overflow" ]; then
		rm -f "$candidate_idfile"
		clear_failed_resume
		if [ -f "$reply_overflow" ] && [ -f "$diagnostics_overflow" ]; then
			echo "[$peer: reply and diagnostic streams each exceeded ${consult_stream_limit} bytes — no partial output was accepted; retry with: $fresh_retry]" >&2
		elif [ -f "$reply_overflow" ]; then
			echo "[$peer: reply exceeded ${consult_stream_limit} bytes — narrow the question and retry with: $fresh_retry]" >&2
		else
			echo "[$peer: provider diagnostics exceeded ${consult_stream_limit} bytes — no partial diagnostics were printed; check or upgrade the provider CLI, then retry with: $fresh_retry]" >&2
		fi
		exit 1
	fi
	# Diagnostics are never advisor output. A failed command's stdout may still carry native
	# error events, so show that on stderr too; successful reply stdout is released below.
	cat "$diagnostics" >&2
	if [ "$st" -ne 0 ] && [ -s "$out" ]; then cat "$out" >&2; fi
	if [ "$st" -eq 0 ]; then
		if ! grep -q '[^[:space:]]' "$out"; then
			rm -f "$candidate_idfile"
			clear_failed_resume
			echo "[$peer: provider returned no usable reply — retry with: $fresh_retry; if it repeats, check the provider diagnostics]" >&2
			exit 1
		fi
			if ! record_turn; then
				publish_candidate_telemetry
				cat "$out"
			rm -f "$candidate_idfile" "$statefile"
			echo "[$peer: reply delivered, but continuity exceeded ${consult_context_limit} bytes — next request must use: $fresh_retry]" >&2
			exit 0
		fi
		published_id=$resume_id
		missing_session=0
		if [ "$dispatch" = fresh ]; then
			if state_file_safe "$candidate_idfile"; then
				candidate_bytes=$(wc -c <"$candidate_idfile" | tr -d '[:space:]')
				[ "$candidate_bytes" -ge 1 ] && [ "$candidate_bytes" -le 512 ] || die "provider returned an invalid session id"
				published_id=$(cat "$candidate_idfile")
				case "$published_id" in *[!a-zA-Z0-9._:-]*) die "provider returned an invalid session id" ;; esac
			else
				published_id=
				missing_session=1
			fi
		fi
			publish_state "$published_id" "$contextfile" || die "cannot atomically publish successful consult continuation"
			rm -f "$candidate_idfile"
			publish_candidate_telemetry
			cat "$out"
		if [ "$missing_session" -eq 1 ]; then
			echo "[$peer: reply delivered without a resumable session id — $continue_retry will restart fresh from the saved transcript]" >&2
		fi
		exit 0
	fi
	# A failed resume leaves the native session uncertain. Preserve only the last valid
	# transcript; the next --continue will start fresh from it instead of retrying forever.
	rm -f "$candidate_idfile"
	clear_failed_resume
	if [ "$dispatch" = resume ]; then
		echo "[$peer: resume failed; uncertain native session cleared and saved transcript retained]" >&2
	fi
	if ! coop_rate_limited "$out" && ! coop_rate_limited "$diagnostics"; then
		exit "$st"
	fi
	if [ "$dispatch" = resume ] && [ ! -s "$contextfile" ]; then
		echo "[$peer: rate limited, but no saved transcript can seed a fresh fallback; retry with: $fresh_retry]" >&2
		exit "$st"
	fi
	if [ "$index" -ge "$total" ]; then
		if [ -n "$role" ]; then
			if [ "$total" -eq 1 ]; then
				echo "[$peer: rate limited; role $name exhausted its only target]" >&2
			else
				echo "[$peer: rate limited; role $name exhausted all $total targets]" >&2
			fi
		else
			echo "[$peer: rate limited; direct peer has no fallback target]" >&2
		fi
		exit "$st"
	fi
	echo "[$peer: $target rate limited — trying fallback $((index + 1))/$total]" >&2
	index=$((index + 1))
	dispatch=fresh
done

exit 2
`)
	return b.String()
}

// indentArms prefixes every non-empty line of a rendered case-arm block with pad.
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
