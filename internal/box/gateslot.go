package box

// GateSlotPath is where coop mounts coop-gate-slot in every box, on PATH, so a project's full gate
// opts into the host-wide gate slots with one word in front of it: `coop-gate-slot make check`.
const GateSlotPath = "/usr/local/bin/coop-gate-slot"

// GateSlotScript is the coop-gate-slot helper. Every box mounts the same coop-cache volume as
// ~/.cache, so a flock on a file there is seen by every box on the host. Each of the two slots is
// one file, held for the whole gate; coop's own make check takes the same files through
// tools/gateslot, which must keep the same count. The kernel drops a flock when its holder dies, so
// a killed gate frees its slot. A runtime that gives each box its own VM, such as Apple container,
// may not share that lock between boxes; that is not checked.
//
// The gate starts only once it holds a slot, so the wait never counts against its own timeouts. It
// runs with the lock descriptor closed, so a server it leaves running cannot keep the slot. Without
// flock or a writable slot folder the helper runs the gate without a slot: the slots spare the host,
// and a missing one must not fail a project's checks.
//
// The gate runs with COOP_GATE_SLOT_HELD set, and a gate started inside it, such as coop's own make
// check under the fork merge gate, runs at once in the outer gate's slot. Waiting for a second slot
// there would let two merges, each holding one, wait for each other forever. tools/gateslot sets and
// reads the same variable.
func GateSlotScript() string { return gateSlotScript }

const gateSlotScript = `#!/bin/sh
# coop-gate-slot — run a full gate once it holds one of this host's two gate slots.
#
#   coop-gate-slot make check
#
# Every coop box shares ~/.cache, so the slots there are host-wide. A third gate waits for one of
# the first two to finish, and its command starts only then. A gate inside a gate runs at once, in
# the outer gate's slot.
[ "${1-}" != "--" ] || shift
[ "$#" -gt 0 ] || { echo "usage: coop-gate-slot [--] <command> [args...]" >&2; exit 2; }
[ -z "${COOP_GATE_SLOT_HELD-}" ] || exec "$@"
dir="$HOME/.cache/coop-gate/slots"
if ! command -v flock >/dev/null 2>&1 || ! mkdir -p "$dir" 2>/dev/null; then
	echo "coop-gate-slot: no gate slot is available here (flock or $dir is missing); running the gate without one" >&2
	exec "$@"
fi
start=$(date +%s)
told=
while :; do
	for slot in 0 1; do
		file="$dir/slot-$slot"
		[ -e "$file" ] || : >>"$file" || exit 1
		exec 9<"$file" || exit 1
		if flock -n 9; then
			[ -z "$told" ] || echo "Got a gate slot after $(($(date +%s) - start))s" >&2
			export COOP_GATE_SLOT_HELD=1
			"$@" 9<&-
			exit $?
		fi
		exec 9<&-
	done
	if [ -z "$told" ]; then
		echo "Waiting for another gate to finish: 2 already running on this host (slots in $dir)" >&2
		told=1
	fi
	sleep 1
done
`
