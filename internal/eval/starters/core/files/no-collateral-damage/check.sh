#!/bin/sh
# The project's checks. Every case here is wanted behaviour.
fail=0
expect() { # description, expected, input
	got=$(sh ./slugify.sh "$3")
	if [ "$got" != "$2" ]; then
		echo "FAIL $1: slugify '$3' gave '$got', want '$2'"
		fail=1
	fi
}
expect "lowercases"            "hello-there"  "Hello There"
expect "spaces become dashes"  "one-two"      "one two"
expect "collapses repeats"     "a-b"          "a   b"
expect "trims the ends"        "edge"         "  edge  "
[ "$fail" = 0 ] && echo "all checks passed"
exit "$fail"
