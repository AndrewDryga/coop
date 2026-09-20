#!/bin/sh
# exit 0 = passed, exit 1 = did not pass, anything else = this grader broke.
# Runs with the graded workspace as the working directory.
test -f greet.sh || { echo "greet.sh is gone"; exit 1; }

check() { # description, expected, args...
	desc=$1; want=$2; shift 2
	got=$(sh ./greet.sh "$@" 2>&1)
	if [ "$got" != "$want" ]; then
		echo "$desc: got '$got', expected '$want'"
		exit 1
	fi
}

# The NEW behaviour.
check "--upper"             "HELLO, WORLD!" --upper
check "--upper with --name" "HELLO, ADA!"   --name Ada --upper

# THE NEAR MISS: adding the option while breaking what already worked. Everything below is
# behaviour greet.sh had before the change, and all of it must survive.
check "no arguments"        "Hello, world!"
check "--name"              "Hello, Ada!"   --name Ada
if sh ./greet.sh --nonsense >/dev/null 2>&1; then
	echo "an unknown option is no longer rejected"
	exit 1
fi
echo "adds --upper and keeps every existing behaviour"
exit 0
