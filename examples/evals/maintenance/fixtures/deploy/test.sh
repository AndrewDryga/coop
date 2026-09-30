#!/bin/sh
set -eu
work=$(mktemp -d)
trap 'rm -rf "$work"' 0 HUP INT TERM
sh ./deploy.sh app.conf "$work/service.env"
printf 'NAME=demo\nPORT=8080\nLOG=logs/service.log\n' > "$work/expected"
cmp "$work/expected" "$work/service.env"
echo 'baseline deploy test passed'
