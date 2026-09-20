#!/bin/sh
# The toolkit's own tests. Add a case here for each subcommand you add.
fail=0
expect() { # description, expected, args...
	desc=$1; want=$2; shift 2
	got=$(sh ./tools.sh "$@" 2>&1)
	if [ "$got" != "$want" ]; then
		echo "FAIL $desc: got '$got', want '$want'"
		fail=1
	fi
}
expect "upper" "ABC" upper "abc"
[ "$fail" = 0 ] && echo "all tests passed"
exit "$fail"
