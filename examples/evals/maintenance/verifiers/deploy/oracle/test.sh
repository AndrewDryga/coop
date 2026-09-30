#!/bin/sh
set -eu
work=$(mktemp -d)
trap 'rm -rf "$work"' 0 HUP INT TERM
sh ./deploy.sh app.conf "$work/service.env"
printf 'NAME=demo\nPORT=8080\nLOG=logs/service.log\n' > "$work/expected"
cmp "$work/expected" "$work/service.env"

mkdir "$work/config dir"
printf 'name=test worker\nport=8081\nlog=logs/test worker.log\n' > "$work/config dir/app.conf"
APP_PORT=8082 sh ./deploy.sh "$work/config dir/app.conf" "$work/service.env"
printf 'NAME=test worker\nPORT=8082\nLOG=logs/test worker.log\n' > "$work/expected"
cmp "$work/expected" "$work/service.env"
cp "$work/service.env" "$work/prior"
if APP_PORT=wrong sh ./deploy.sh "$work/config dir/app.conf" "$work/service.env" 2>/dev/null; then exit 1; fi
cmp "$work/prior" "$work/service.env"
printf 'name=incomplete\n' > "$work/incomplete.conf"
if sh ./deploy.sh "$work/incomplete.conf" "$work/service.env" 2>/dev/null; then exit 1; fi
cmp "$work/prior" "$work/service.env"
ln "$work/service.env" "$work/old-inode"
sh ./deploy.sh app.conf "$work/service.env"
cmp "$work/prior" "$work/old-inode"
if sh ./deploy.sh app.conf "$work/config dir" 2>/dev/null; then exit 1; fi
[ -z "$(find "$work" -name '.deploy.*' -print)" ]
echo 'deploy tests passed'
