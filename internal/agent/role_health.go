package agent

// RoleHealthShell is shared by the generated consult and delegate wrappers. It appends a small
// best-effort status row beside the existing peer usage rows, and lets later calls in the same run
// skip an exact target that already proved permanently unusable.
func RoleHealthShell() string {
	return `
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

coop_login_rejected() {
	LC_ALL=C grep -Eiq 'not signed in|credential[^:]* (missing|unavailable|invalid)|"http_status": 401' "$@" 2>/dev/null
}

coop_failure_permanent() {
	coop_failure_status=$1
	shift
	case "$coop_failure_status" in 126|127) return 0 ;; esac
	coop_login_rejected "$@" ||
		LC_ALL=C grep -Eiq 'no such file or directory|command not found|file name too long|unknown (option|model)|unrecognized option|invalid (argument|model)' "$@" 2>/dev/null
}

coop_failure_cause() {
	cat "$@" 2>/dev/null | sed '/^[[:space:]]*$/d' | tail -n 1 | head -c 300
}
`
}
