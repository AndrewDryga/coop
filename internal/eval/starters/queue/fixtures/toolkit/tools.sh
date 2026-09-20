#!/bin/sh
# A tiny text toolkit. Each subcommand reads its input as "$2".
#
#   tools.sh upper "abc"   -> ABC
#
case "$1" in
	upper) printf '%s' "$2" | tr '[:lower:]' '[:upper:]' ;;
	*) echo "tools.sh: unknown subcommand '$1'" >&2; exit 2 ;;
esac
