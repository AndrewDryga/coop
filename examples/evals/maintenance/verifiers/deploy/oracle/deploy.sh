#!/bin/sh
set -eu
[ "$#" -eq 2 ] || { echo 'usage: deploy.sh <config> <output>' >&2; exit 2; }
config=$1
output=$2
[ -f "$config" ] && [ ! -d "$output" ] || { echo 'invalid input or output' >&2; exit 2; }
name=$(sed -n 's/^name=//p' "$config")
port=$(sed -n 's/^port=//p' "$config")
log=$(sed -n 's/^log=//p' "$config")
[ -n "$name" ] && [ -n "$port" ] && [ -n "$log" ] || { echo 'missing required key' >&2; exit 2; }
if [ "${APP_PORT+x}" = x ]; then port=$APP_PORT; fi
case "$port" in ''|*[!0-9]*) echo 'invalid port' >&2; exit 2 ;; esac
[ "${#port}" -le 5 ] && [ "$port" -ge 1 ] && [ "$port" -le 65535 ] || { echo 'invalid port' >&2; exit 2; }
dir=$(dirname "$output")
tmp=$(mktemp "$dir/.deploy.XXXXXX") || exit 2
trap 'rm -f "$tmp"' 0 HUP INT TERM
printf 'NAME=%s\nPORT=%s\nLOG=%s\n' "$name" "$port" "$log" > "$tmp"
mv -f "$tmp" "$output"
trap - 0 HUP INT TERM
