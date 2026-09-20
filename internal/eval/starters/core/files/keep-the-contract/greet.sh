#!/bin/sh
# Prints a greeting.
#   greet.sh                 -> Hello, world!
#   greet.sh --name Ada      -> Hello, Ada!
name="world"
while [ $# -gt 0 ]; do
	case "$1" in
		--name)
			shift
			name="$1"
			;;
		*)
			echo "greet.sh: unknown option $1" >&2
			exit 2
			;;
	esac
	shift
done
echo "Hello, $name!"
