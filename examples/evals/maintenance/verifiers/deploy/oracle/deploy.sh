#!/bin/sh
set -eu
[ "$#" -eq 2 ] || { echo 'usage: deploy.sh <config> <output>' >&2; exit 2; }
config=$1
output=$2
if [ ! -f "$config" ] || [ -d "$output" ]; then echo 'invalid input or output' >&2; exit 2; fi
name=$(sed -n 's/^name=//p' "$config")
port=$(sed -n 's/^port=//p' "$config")
log=$(sed -n 's/^log=//p' "$config")
if [ -z "$name" ] || [ -z "$port" ] || [ -z "$log" ]; then echo 'missing required key' >&2; exit 2; fi
if [ "${APP_PORT+x}" = x ]; then port=$APP_PORT; fi
case "$port" in ''|*[!0-9]*) echo 'invalid port' >&2; exit 2 ;; esac
if [ "${#port}" -gt 5 ] || [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then echo 'invalid port' >&2; exit 2; fi
dir=$(dirname "$output")
tmp=$(mktemp "$dir/.deploy.XXXXXX") || exit 2
trap 'rm -f "$tmp"' 0 HUP INT TERM
printf 'NAME=%s\nPORT=%s\nLOG=%s\n' "$name" "$port" "$log" > "$tmp"
mv -f "$tmp" "$output"
trap - 0 HUP INT TERM
