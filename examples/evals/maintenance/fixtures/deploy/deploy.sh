#!/bin/sh
# The path-expansion defect is the first queued maintenance task.
# shellcheck disable=SC2086
config=$1
output=$2
name=$(sed -n 's/^name=//p' $config)
port=$(sed -n 's/^port=//p' $config)
log=$(sed -n 's/^log=//p' $config)
if [ -n "$APP_PORT" ]; then port=$APP_PORT; fi
printf 'NAME=%s\nPORT=%s\nLOG=%s\n' "$name" "$port" "$log" > "$output"
