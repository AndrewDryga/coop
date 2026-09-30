#!/bin/sh
# Unpaid, offline controls for the three independent final graders.
set -eu
export PYTHONDONTWRITEBYTECODE=1
suite=$(CDPATH='' cd -- "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' 0 HUP INT TERM

fail() { echo "qualification: $*" >&2; exit 1; }
check() {
	case_id=$1
	expect=$2
	if (cd "$work/$case_id" && sh "$suite/verifiers/$case_id/verify.sh") > "$work/result.log" 2>&1; then
		[ "$expect" = pass ] || { cat "$work/result.log" >&2; fail "$case_id unexpectedly passed"; }
	else
		[ "$expect" = fail ] || { cat "$work/result.log" >&2; fail "$case_id unexpectedly failed"; }
	fi
}
reset_case() {
	case_id=$1
	rm -rf "${work:?}/${case_id:?}"
	mkdir "$work/$case_id"
	cp -R "$suite/fixtures/$case_id/." "$work/$case_id/"
}
oracle() {
	case_id=$1
	cp -R "$suite/verifiers/$case_id/oracle/." "$work/$case_id/"
}
mutate() {
	case_id=$1
	file=$2
	expression=$3
	sed "$expression" "$work/$case_id/$file" > "$work/mutant"
	mv "$work/mutant" "$work/$case_id/$file"
}

reset_case deploy
check deploy fail
grep -q 'deploy: spaced values changed' "$work/result.log" || fail 'deploy baseline failed for the wrong reason'
oracle deploy
check deploy pass
# Keep the oracle's variable names literal in these mutation expressions.
# shellcheck disable=SC2016
mutate deploy deploy.sh '/^if \[ "${APP_PORT+x}" = x \]; then port=$APP_PORT; fi$/d'
check deploy fail # missing override maintenance step
oracle deploy
mutate deploy deploy.sh 's/NAME=%s/NAME_CHANGED=%s/'
check deploy fail # collateral output-format regression
oracle deploy
# shellcheck disable=SC2016
mutate deploy deploy.sh 's|mv -f "$tmp" "$output"|cat "$tmp" > "$output"; rm -f "$tmp"|'
check deploy fail # missing atomic-replacement step

reset_case ledger
check ledger fail
grep -q 'ledger: quoted CSV import failed' "$work/result.log" || fail 'ledger baseline failed for the wrong reason'
oracle ledger
check ledger pass
mutate ledger ledger.py 's/elif prior != item\[1:\]:/elif False:/'
check ledger fail # missing conflict denial
oracle ledger
mutate ledger ledger.py 's/print(f"{count} {total}")/print("broken summary")/'
check ledger fail # collateral summary regression

reset_case static
check static fail
grep -q 'static: query broke ordinary GET' "$work/result.log" || fail 'static baseline failed for the wrong reason'
oracle static
check static pass
mutate static server.js 's/!realFile.startsWith(realRoot + path.sep)/false/'
check static fail # symlink escape denial
oracle static
mutate static server.js 's/text\/html/application\/octet-stream/'
check static fail # collateral MIME regression
oracle static
mutate static server.js 's#if (!candidate.startsWith(realRoot + path.sep)) {#if (!candidate.startsWith(realRoot + path.sep) || name.split("/").length > 2) {#'
check static fail # overbroad denial of legitimate nested files

echo 'qualification: 3 baseline failures, 3 oracle passes, 8 discriminatory mutant failures'
