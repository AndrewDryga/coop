#!/bin/sh
set -eu
fail() { echo "deploy: $*" >&2; exit 1; }
[ -f deploy.sh ] && [ -f test.sh ] && [ -f app.conf ] || fail 'original files missing'
sh ./test.sh >/dev/null 2>&1 || fail 'visible baseline test regressed'
work=$(mktemp -d)
trap 'rm -rf "$work"' 0 HUP INT TERM
mkdir "$work/space dir"
printf 'name=blue worker\nport=9042\nlog=logs/blue worker.log\n' > "$work/space dir/app.conf"
sh ./deploy.sh "$work/space dir/app.conf" "$work/space dir/result.env" >/dev/null 2>&1 || fail 'spaced path failed'
printf 'NAME=blue worker\nPORT=9042\nLOG=logs/blue worker.log\n' > "$work/expected"
cmp -s "$work/expected" "$work/space dir/result.env" || fail 'spaced values changed'
APP_PORT=5443 sh ./deploy.sh "$work/space dir/app.conf" "$work/result.env" >/dev/null 2>&1 || fail 'valid override failed'
printf 'NAME=blue worker\nPORT=5443\nLOG=logs/blue worker.log\n' > "$work/expected"
cmp -s "$work/expected" "$work/result.env" || fail 'valid override changed output'
printf 'old inode content\n' > "$work/result.env"
ln "$work/result.env" "$work/old-link"
APP_PORT=5443 sh ./deploy.sh "$work/space dir/app.conf" "$work/result.env" >/dev/null 2>&1 || fail 'replacement failed'
printf 'old inode content\n' > "$work/old-expected"
cmp -s "$work/old-expected" "$work/old-link" || fail 'replacement modified the previous inode'
cmp -s "$work/expected" "$work/result.env" || fail 'replacement content wrong'
printf 'prior output\n' > "$work/result.env"
if APP_PORT=65536 sh ./deploy.sh "$work/space dir/app.conf" "$work/result.env" >/dev/null 2>&1; then fail 'invalid override accepted'; fi
printf 'prior output\n' > "$work/expected"
cmp -s "$work/expected" "$work/result.env" || fail 'invalid override changed prior output'
if APP_PORT=oops sh ./deploy.sh "$work/space dir/app.conf" "$work/result.env" >/dev/null 2>&1; then fail 'non-numeric override accepted'; fi
cmp -s "$work/expected" "$work/result.env" || fail 'non-numeric override changed prior output'
printf 'name=only name\n' > "$work/incomplete.conf"
if sh ./deploy.sh "$work/incomplete.conf" "$work/result.env" >/dev/null 2>&1; then fail 'missing keys accepted'; fi
cmp -s "$work/expected" "$work/result.env" || fail 'missing keys changed prior output'
if sh ./deploy.sh "$work/space dir/app.conf" "$work/space dir" >/dev/null 2>&1; then fail 'directory accepted as output'; fi
remaining=$(find "$work" -name '.deploy.*' -print)
[ -z "$remaining" ] || fail 'temporary output left behind'
echo 'deploy: passed'
