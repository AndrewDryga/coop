package agent

import (
	"fmt"
	"strings"
)

// RoleHealthShell is shared by the generated consult and delegate wrappers. It appends a small
// best-effort status row beside the existing peer usage rows, and lets later calls in the same run
// skip an exact target that already proved permanently unusable.
func RoleHealthShell() string {
	return loginRejectedShell() + `
coop_role_file_ready() {
	[ -n "${COOP_RUN_ID:-}" ] || return 1
	case "$COOP_RUN_ID" in *[!a-zA-Z0-9._-]*) return 1 ;; esac
	coop_role_repo=$(git rev-parse --show-toplevel 2>/dev/null || printf '.')
	coop_role_dir=$coop_role_repo/.agent/runs
	coop_role_file=$coop_role_dir/$COOP_RUN_ID.peers.jsonl
	[ ! -L "$coop_role_dir" ] && [ -d "$coop_role_dir" ] || return 1
	[ ! -L "$coop_role_file" ] && [ -f "$coop_role_file" ] || return 1
	coop_role_links=$(stat -c %h "$coop_role_file" 2>/dev/null || stat -f %l "$coop_role_file" 2>/dev/null || :)
	coop_role_permissions=$(stat -c %a "$coop_role_file" 2>/dev/null || stat -f %Lp "$coop_role_file" 2>/dev/null || :)
	[ "$coop_role_links" = 1 ] && [ "$coop_role_permissions" = 600 ] || return 1
	coop_role_bytes=$(stat -c %s "$coop_role_file" 2>/dev/null || stat -f %z "$coop_role_file" 2>/dev/null || :)
	case "$coop_role_bytes" in '' | *[!0-9]*) return 1 ;; esac
	[ "$coop_role_bytes" -le 1048576 ] || return 1
}

coop_role_health() {
	coop_role_file_ready || return 0
	coop_role_row=$(jq -cn --arg run "$COOP_RUN_ID" --arg role "$1" --arg mode "$2" \
		--arg provider "$3" --arg model "$4" --arg target "$5" --arg outcome "$6" \
		--arg attempts "$7" --arg permanent "$8" --arg cause "$9" \
		'{kind:"role_health",run:$run,role:$role,mode:$mode,provider:$provider,model:$model,target:$target,
		  outcome:$outcome,attempts:($attempts|tonumber),permanent:($permanent=="true")}
		 + (if $cause=="" then {} else {cause:($cause[0:300])} end)' 2>/dev/null) || return 0
	[ "$(printf '%s' "$coop_role_row" | wc -c | tr -d '[:space:]')" -le 4096 ] || return 0
	eval 'exec 9>>"$coop_role_file"' 2>/dev/null || return 0
	coop_role_fd=/dev/fd/9
	[ -e "$coop_role_fd" ] || coop_role_fd=/proc/self/fd/9
	coop_role_path_identity=$(stat -c 'gnu:%d:%i' "$coop_role_file" 2>/dev/null || stat -f 'bsd:%i' "$coop_role_file" 2>/dev/null || :)
	coop_role_fd_identity=$(stat -Lc 'gnu:%d:%i' "$coop_role_fd" 2>/dev/null || stat -Lf 'bsd:%i' "$coop_role_fd" 2>/dev/null || :)
	[ -n "$coop_role_path_identity" ] && [ "$coop_role_path_identity" = "$coop_role_fd_identity" ] && [ ! -L "$coop_role_file" ] || { exec 9>&-; return 0; }
	printf '%s\n' "$coop_role_row" >&9 2>/dev/null || true
	exec 9>&-
}

coop_role_quarantined() {
	coop_role_file_ready || return 1
	coop_role_quarantine=$(jq -c -s --arg run "$COOP_RUN_ID" --arg target "$1" '
		any(.[]; .kind=="role_health" and .run==$run and .target==$target and .permanent==true)
	' "$coop_role_file" 2>/dev/null) || return 1
	[ "$coop_role_quarantine" = true ]
}

coop_failure_permanent() {
	case "$1" in 126|127) return 0 ;; esac
	coop_login_rejected "$2" "$3" "${4:-}" ||
		coop_client_errors "$2" "$3" "${4:-}" |
		LC_ALL=C grep -Eiq 'no such file or directory|command not found|file name too long|unknown (option|model)|unrecognized option|invalid (argument|model)|not signed in|credential[^:]* (missing|unavailable|invalid)'
}

coop_failure_cause() {
	cat "$@" 2>/dev/null | sed '/^[[:space:]]*$/d' | tail -n 1 | head -c 300
}
`
}

// loginRejectedShell renders the wrappers' reading of a failed provider command.
//
// coop_client_errors <provider> <stderr file> [<stdout file>] prints the client's own error text as
// the loop's decoders see it: its plain stderr lines, the bare (non-JSON) lines of its structured
// stdout, and that stdout's terminal errors through the adapter's <provider>_errors decoder, which
// sees only well-formed events. A JSON event is never read as text — events also carry the agent's
// reply, and on failure the consult's diagnostics hold a copy of the raw stream.
//
// coop_login_rejected <provider> <stderr file> [<stdout file>] reports whether that text proves the
// provider refused its login. The signals are each adapter's AuthSignals, anchored as
// AuthenticationFailure anchors them for the loop, so a signal an adapter adds reaches the loop and
// both wrappers together.
func loginRejectedShell() string {
	var b strings.Builder
	b.WriteString(`
coop_client_errors() {
	grep -v '^[[:space:]]*[{[]' "$2" 2>/dev/null
	if [ -n "${3:-}" ] && [ -r "$3" ]; then
		grep -v '^[[:space:]]*[{[]' "$3" 2>/dev/null
		if command -v "$1_errors" >/dev/null 2>&1; then
			jq -cR 'fromjson? | objects' "$3" 2>/dev/null | "$1_errors" 2>/dev/null
		fi
	fi
}

coop_login_rejected() {
	case "$1" in
`)
	for _, name := range Names() {
		a, _ := Get(name)
		var alternatives []string
		for _, signal := range a.LiveCredentials().AuthSignals {
			alternatives = append(alternatives, strings.ReplaceAll(strings.ToLower(strings.TrimSpace(signal)), ".", "[.]"))
		}
		if len(alternatives) > 0 {
			fmt.Fprintf(&b, "\t%s) coop_login_signals='%s' ;;\n", name, strings.Join(alternatives, "|"))
		}
	}
	b.WriteString(`	*) return 1 ;;
	esac
	coop_client_errors "$@" |
		LC_ALL=C grep -Eiq "^[[:space:]]*($coop_login_signals)([[:space:]]*\$|[.:])|^[[:space:]]*(error:|fatal:|[{[]).*($coop_login_signals)"
}
`)
	return b.String()
}
