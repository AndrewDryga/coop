#!/bin/sh
# Run unpaid baseline/oracle controls in the trusted image, offline and credential-free.
set -eu
[ "$#" -eq 1 ] || { echo 'usage: sh runtime-qualification.sh <immutable-image-id>' >&2; exit 2; }
image=$1
suite=$(CDPATH='' cd -- "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' 0 HUP INT TERM
chmod 755 "$work"

for case_id in deploy ledger static; do
	for variant in baseline oracle; do
		workspace="$work/$case_id-$variant"
		mkdir "$workspace"
		cp -R "$suite/fixtures/$case_id/." "$workspace/"
		if [ "$variant" = oracle ]; then
			cp -R "$suite/verifiers/$case_id/oracle/." "$workspace/"
		fi
		chmod -R a+rX "$workspace"
		if docker run --rm --network none --read-only --tmpfs /tmp:rw,nosuid,nodev,size=64m \
			--pids-limit 128 --memory 1g --cpus 2 --entrypoint /bin/sh \
			-e PYTHONDONTWRITEBYTECODE=1 \
			-v "$workspace:/workspace:rw" -v "$suite/verifiers/$case_id:/coop-verifier:ro" \
			-w /workspace "$image" /coop-verifier/verify.sh > "$work/result.log" 2>&1; then
			[ "$variant" = oracle ] || { cat "$work/result.log" >&2; echo "$case_id baseline unexpectedly passed" >&2; exit 1; }
		else
			[ "$variant" = baseline ] || { cat "$work/result.log" >&2; echo "$case_id oracle failed" >&2; exit 1; }
			case "$case_id" in
				deploy) reason='deploy: spaced values changed' ;;
				ledger) reason='ledger: quoted CSV import failed' ;;
				static) reason='static: query broke ordinary GET' ;;
			esac
			grep -q "$reason" "$work/result.log" || { cat "$work/result.log" >&2; echo "$case_id baseline failed for the wrong reason" >&2; exit 1; }
		fi
		printf '%s %s: expected verdict\n' "$case_id" "$variant"
	done
done
