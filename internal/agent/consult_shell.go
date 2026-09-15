package agent

import "strings"

// The provider owns the usage filter; the private append boundary is identical for every
// adapter. Filters produce {input, output, cost?} from a slurped native JSON capture.
func consultPeerRowShell(provider, usageFilter string) string {
	return strings.NewReplacer("__PROVIDER__", provider, "__USAGE_FILTER__", usageFilter).Replace(`__PROVIDER___peer_row() {
	[ -n "${COOP_RUN_ID:-}" ] || return 0
	case "$COOP_RUN_ID" in *[!a-zA-Z0-9._-]*) return 0 ;; esac
	peer_repo=$(git rev-parse --show-toplevel 2>/dev/null || printf '.')
	peer_dir=$peer_repo/.agent/runs
	peer_file=$peer_dir/$COOP_RUN_ID.peers.jsonl
	[ ! -L "$peer_dir" ] && [ -d "$peer_dir" ] || return 0
	[ ! -L "$peer_file" ] && [ -f "$peer_file" ] || return 0
	peer_links=$(stat -c %h "$peer_file" 2>/dev/null || stat -f %l "$peer_file" 2>/dev/null || :)
	peer_permissions=$(stat -c %a "$peer_file" 2>/dev/null || stat -f %Lp "$peer_file" 2>/dev/null || :)
	[ "$peer_links" = 1 ] && [ "$peer_permissions" = 600 ] || return 0
	# Open once, then prove the descriptor still names the private regular file just checked.
	# All later size checks and the append use this descriptor, never a reopened pathname.
	eval 'exec 9>>"$peer_file"' 2>/dev/null || return 0
	[ ! -L "$peer_file" ] && [ -f "$peer_file" ] || { exec 9>&-; return 0; }
	peer_links=$(stat -c %h "$peer_file" 2>/dev/null || stat -f %l "$peer_file" 2>/dev/null || :)
	peer_permissions=$(stat -c %a "$peer_file" 2>/dev/null || stat -f %Lp "$peer_file" 2>/dev/null || :)
	[ "$peer_links" = 1 ] && [ "$peer_permissions" = 600 ] || { exec 9>&-; return 0; }
	peer_fd=/dev/fd/9
	[ -e "$peer_fd" ] || peer_fd=/proc/self/fd/9
	peer_path_identity=$(stat -c 'gnu:%d:%i' "$peer_file" 2>/dev/null || stat -f 'bsd:%i' "$peer_file" 2>/dev/null || :)
	peer_fd_identity=$(stat -Lc 'gnu:%d:%i' "$peer_fd" 2>/dev/null || stat -Lf 'bsd:%i' "$peer_fd" 2>/dev/null || :)
	[ -n "$peer_path_identity" ] && [ "$peer_path_identity" = "$peer_fd_identity" ] && [ ! -L "$peer_file" ] || { exec 9>&-; return 0; }
	peer_bytes=$(stat -Lc %s "$peer_fd" 2>/dev/null || stat -Lf %z "$peer_fd" 2>/dev/null || :)
	case "$peer_bytes" in '' | *[!0-9]*) exec 9>&-; return 0 ;; esac
	peer_usage_limit=1048576
	[ "$peer_bytes" -le $((peer_usage_limit - 4096)) ] || { exec 9>&-; return 0; }
	row=$(jq -sc --arg run "$COOP_RUN_ID" --arg role "$1" --arg model "$2" --arg mode "${3:-}" --arg target "${4:-}" '
		def token: type=="number" and isfinite and floor==. and .>=0 and .<=1000000000;
		__USAGE_FILTER__
		| {run:$run,role:$role,provider:"__PROVIDER__",model:$model,in:.input,out:.output,reported_out:.output}
		  + (if $mode!="" then {mode:$mode} else {} end)
		  + (if $target!="" then {target:$target} else {} end)
		  + (if has("fresh") then {fresh_in:.fresh} else {} end)
		  + (if has("write") then {cache_write:.write} else {} end)
		  + (if has("read") then {cache_read:.read} else {} end)
		  + (if has("duration") then {provider_ms:.duration} else {} end)
		  + (if has("cost") then {cost:.cost,reported_cost:.cost} else {} end)' 2>/dev/null) || { exec 9>&-; return 0; }
	[ -n "$row" ] || { exec 9>&-; return 0; }
	[ "$(printf '%s' "$row" | wc -c | tr -d '[:space:]')" -le 4096 ] || { exec 9>&-; return 0; }
	printf '%s\n' "$row" >&9 2>/dev/null || true
	exec 9>&-
}
`)
}

// Capture and finish are transport mechanics. Each adapter supplies its own *_text parser
// and CLI flags, while the wrapper still decides whether the whole attempt is accepted.
func consultCaptureShell(provider, display string) string {
	return strings.NewReplacer("__PROVIDER__", provider, "__DISPLAY__", display).Replace(`__PROVIDER___finish() {
	provider_status=$1
	raw=$2
	if [ "$provider_status" -ne 0 ]; then
		cat "$raw" >&2
		return "$provider_status"
	fi
	reply=$(__PROVIDER___text <"$raw")
	reply_status=$?
	if [ "$reply_status" -ne 0 ]; then
		echo "[$peer: __DISPLAY__ returned malformed output or no usable reply — retry with: $fresh_retry; if it repeats, check or upgrade __DISPLAY__]" >&2
		return 1
	fi
	candidate_telemetry_raw=$raw
	printf '%s\n' "$reply"
}
__PROVIDER___run() {
	__PROVIDER___raw=$attempt_dir/__PROVIDER__-raw-$index
	__PROVIDER___overflow=$attempt_dir/__PROVIDER__-overflow-$index
	__PROVIDER___capture_status_file=$attempt_dir/__PROVIDER__-capture-status-$index
	__PROVIDER___pipe=$attempt_dir/__PROVIDER__-pipe-$index
	rm -f "$__PROVIDER___overflow" "$__PROVIDER___capture_status_file"
	mkfifo "$__PROVIDER___pipe" || return 1
	start_capture "$__PROVIDER___raw" "$__PROVIDER___overflow" "$attempt_dir/__PROVIDER__-chunk-$index" "$__PROVIDER___pipe" "$__PROVIDER___capture_status_file"
	provider_capture_pid=$capture_pid
	run "$@" >"$__PROVIDER___pipe"
	__PROVIDER___status=$?
	await_capture "$provider_capture_pid" "$__PROVIDER___capture_status_file"
	__PROVIDER___capture_status=$capture_status
	provider_capture_pid=
	rm -f "$__PROVIDER___pipe"
	if [ "$__PROVIDER___capture_status" -ne 0 ]; then
		# The diagnostic reader may have failed too. Report only after the wrapper
		# leaves the attempt pipes, or this echo could kill it with SIGPIPE.
		provider_capture_failed=1
		return 1
	fi
	if [ -f "$__PROVIDER___overflow" ]; then
		echo "[$peer: __DISPLAY__ output exceeded ${consult_stream_limit} bytes — narrow the question and retry with: $fresh_retry]" >&2
		return 1
	fi
	__PROVIDER___finish "$__PROVIDER___status" "$__PROVIDER___raw"
}
`)
}
