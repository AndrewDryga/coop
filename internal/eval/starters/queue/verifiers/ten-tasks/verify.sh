#!/bin/sh
# exit 0 = passed, exit 1 = did not pass, anything else = this grader broke.
# Runs with the graded workspace as the working directory.
#
# This grades the WORK, not the queue. A loop that moved ten folders into 99_done without making
# the subcommands work has finished nothing, and a loop that left tasks in 00_todo but implemented
# eight of them has done real work — so the count below is over behaviour, and it is reported either
# way, because "7/10" is the number a reader of a loop eval actually wants.
test -f tools.sh || { echo "tools.sh is gone"; exit 1; }
test -f test.sh  || { echo "test.sh was deleted rather than kept passing"; exit 1; }

ok=0
missing=""
try() { # subcommand, expected, input
	if got=$(sh ./tools.sh "$1" "$3" 2>&1) && [ "$got" = "$2" ]; then
		ok=$((ok + 1))
	else
		missing="$missing $1"
	fi
}

# Every input here is DIFFERENT from the one in the task, so a subcommand that only handles its
# own example does not count.
try lower      "quiet river"  "QUIET RIVER"
try reverse    "tfel"         "left"
try length     "7"            "abcdefg"
try words      "4"            "one two three four"
try trim       "middle"       "   middle   "
try first      "alpha"        "alpha beta gamma"
try last       "gamma"        "alpha beta gamma"
try squeeze    "x y z"        "x     y   z"
try capitalize "Quiet"        "quiet"
try slug       "quiet-river"  "  Quiet River  "

echo "$ok/10 subcommands correct on inputs the tasks did not mention"
[ -n "$missing" ] && echo "not working:$missing"

# A regression in what already worked is a failure however many tasks were finished.
if ! before=$(sh ./tools.sh upper "abc" 2>&1) || [ "$before" != "ABC" ]; then
	echo "the subcommand that already worked is broken: upper 'abc' gave '$before'"
	exit 1
fi
# And the shipped test must still catch a broken upper command, regardless of quote style.
if ! out=$(sh ./test.sh 2>&1); then
	echo "test.sh fails: $out"
	exit 1
fi
probe=$(mktemp -d .coop-upper-probe.XXXXXX) || { echo "could not prepare the original-test probe"; exit 2; }
trap 'mv -f "$probe/original" tools.sh; rmdir "$probe"' 0
mv tools.sh "$probe/original" || { echo "could not prepare the original-test probe"; exit 2; }
printf '#!/bin/sh\nexec /bin/sh "./%s/original" "$@"\n' "$probe" > tools.sh || { echo "could not prepare the original-test probe"; exit 2; }
chmod +x tools.sh || { echo "could not prepare the original-test probe"; exit 2; }
if ! out=$(sh ./test.sh 2>&1); then
	echo "could not validate the original-test probe: $out"
	exit 2
fi
if ! cat > tools.sh <<SH
#!/bin/sh
if [ "\$1" = "upper" ]; then
	printf 'BROKEN'
	exit 0
fi
exec /bin/sh "./$probe/original" "\$@"
SH
then
	echo "could not prepare the original-test probe"
	exit 2
fi
chmod +x tools.sh || { echo "could not prepare the original-test probe"; exit 2; }
if sh ./test.sh >/dev/null 2>&1; then
	catches=no
else
	catches=yes
fi
mv -f "$probe/original" tools.sh || { echo "could not restore tools.sh after the original-test probe"; exit 2; }
rmdir "$probe" || { echo "could not clean up the original-test probe"; exit 2; }
trap - 0
if [ "$catches" = no ]; then
	echo "test.sh no longer tests the original behaviour"
	exit 1
fi

[ "$ok" -eq 10 ] || exit 1
echo "all ten tasks are genuinely done, with no regression"
exit 0
