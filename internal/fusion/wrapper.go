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
# <peer> is ` + peerList + ` (fusion / --peer ad-hoc). A preset CONSULT ROLE — or a
# native role degraded under a non-Claude lead — is addressed by its ROLE name. Its
# COOP_CONSULT_<ROLE>_TARGETS value is an ordered fallback ladder; each target remains
# READ-ONLY. --fresh starts at rung one. --continue resumes the successful rung and,
# if that provider is now rate limited, starts the next provider fresh. Each rung is
# attempted once and each attempt is time-bounded (default 30m; COOP_CONSULT_TIMEOUT).
set -u
umask 077

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
question=${*:-}
[ -n "$question" ] || question=$(cat)
# The prompt is captured above, so no peer needs stdin. Detach it: claude -p reads
# piped stdin on top of its arg and blocks forever on an inherited open pipe (the
# governor backgrounds these consults). One redirect covers every peer.
exec </dev/null
idfile="/tmp/coop-consult-${key}.id"
rungfile="/tmp/coop-consult-${key}.rung"
contextfile="/tmp/coop-consult-${key}.context"
attempt_dir=${TMPDIR:-/tmp}/coop-consult-${key}-$$
mkdir "$attempt_dir" || die "cannot create attempt directory: $attempt_dir"
trap 'rm -rf "$attempt_dir"' EXIT INT TERM

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
	if [ -z "$model" ]; then eval "model=\${COOP_PEER_MODEL_${peerkey}:-}"; fi
	if [ -z "$effort" ]; then eval "effort=\${COOP_PEER_EFFORT_${peerkey}:-}"; fi
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
for target do
	load_target "$target"
	case "$peer" in
` + peerAlt + `) ;;
	*) die "unknown peer in $name ladder: $peer (expected ` + peerList + `)" ;;
	esac
	if [ -n "$role" ]; then scope="${COOP_PRIMARY:-} ${COOP_PEERS:-}"; else scope=${COOP_PEERS:-}; fi
	case " $scope " in
*" $peer "*) available_targets="${available_targets:-} $target" ;;
	*)
		if [ -n "$role" ]; then
			echo "[coop-consult $name: skipping $target — provider credentials are not mounted]" >&2
		else
			die "peer not in this run's credential scope: $peer"
		fi
		;;
	esac
done
targets=${available_targets# }
# shellcheck disable=SC2086
set -- $targets
total=$#
[ "$total" -gt 0 ] || die "consult role $name has no target with mounted credentials"

case "$mode" in
--fresh | --continue) ;;
*) die "mode must be --fresh or --continue" ;;
esac
if [ "$mode" = --fresh ]; then rm -f "$idfile" "$rungfile" "$contextfile"; fi

new_id() {
	if [ -r /proc/sys/kernel/random/uuid ]; then
		cat /proc/sys/kernel/random/uuid
	else
		od -An -tx1 -N16 /dev/urandom | tr -d ' \n' |
			sed 's/\(........\)\(....\)\(....\)\(....\)\(............\)/\1-\2-\3-\4-\5/'
	fi
}
`)
	b.WriteString(agents.ShellRateLimitDetector())
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

dispatch_fresh() {
	id=$(new_id)
	case "$peer" in
` + indentArms(fresh.String(), "\t") + `	*) return 2 ;;
	esac
}

dispatch_resume() {
	id=$(cat "$idfile")
	case "$peer" in
` + indentArms(resume.String(), "\t") + `	*) return 2 ;;
	esac
}

run_attempt() {
	dispatch=$1
	out=$attempt_dir/output-$index
	status=$attempt_dir/status-$index
	build_prompt "$dispatch"
	# Keep output live through tee, while the status file preserves the provider's
	# real exit code even for adapter bodies that exit explicitly.
	{
		(
			if [ "$dispatch" = resume ]; then dispatch_resume; else dispatch_fresh; fi
		)
		printf '%s\n' "$?" >"$status"
	} 2>&1 | tee "$out"
	attempt_status=$(cat "$status" 2>/dev/null || printf 1)
	case "$attempt_status" in '' | *[!0-9]*) attempt_status=1 ;; esac
}

build_prompt() {
	delivery=$1
	prompt=$question
	if [ "$delivery" = fresh ] && [ "$mode" = --continue ] && [ -s "$contextfile" ]; then
		prompt=$(printf '%s\n\n' 'Continue this consult from the saved transcript:'; cat "$contextfile"; printf '\n\nCurrent follow-up:\n%s' "$question")
	fi
	# A role's persona is prepended on every provider, including a fresh fallback.
	if [ -n "$persona" ] && [ -r "$persona" ]; then
		prompt=$(cat "$persona"; printf '\n\n---\n\nYour question:\n\n%s' "$prompt")
	fi
}

record_turn() {
	if [ "$mode" = --fresh ] || [ ! -f "$contextfile" ]; then
		: >"$contextfile"
	else
		printf '\n\n---\n\n' >>"$contextfile"
	fi
	{
		printf 'Question:\n%s\n\nReply:\n' "$question"
		cat "$out"
	} >>"$contextfile"
}

start=1
dispatch=fresh
if [ "$mode" = --continue ] && [ -f "$idfile" ]; then
	if [ -f "$rungfile" ]; then start=$(cat "$rungfile"); fi
	case "$start" in
	'' | *[!0-9]*) start=1 ;;
	esac
	if [ "$start" -lt 1 ] || [ "$start" -gt "$total" ]; then start=1; fi
	dispatch=resume
fi

index=$start
while [ "$index" -le "$total" ]; do
	load_rung "$index" || die "cannot resolve rung $index for $name"
	resolve_defaults
	if [ "$dispatch" = resume ]; then
		echo "[$peer: continued on $target — recalls your earlier consult; send only the delta]"
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
	if [ "$st" -eq 0 ]; then
		printf '%s' "$index" >"$rungfile"
		record_turn
		exit 0
	fi
	if [ "$st" -eq 124 ] || [ "$st" -eq 137 ]; then
		exit "$st"
	fi
	if ! coop_rate_limited "$out"; then
		exit "$st"
	fi
	if [ "$dispatch" = resume ] && [ ! -s "$contextfile" ]; then
		echo "[$peer: rate limited, but no saved transcript can seed a fresh fallback; rerun $name with --fresh]" >&2
		exit "$st"
	fi
	rm -f "$idfile" "$rungfile"
	if [ "$index" -ge "$total" ]; then
		echo "[$peer: rate limited; role $name exhausted all $total target(s)]" >&2
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
