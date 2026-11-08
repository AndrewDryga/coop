package box

import (
	"regexp"
	"strconv"
	"strings"
)

// GateSlotPath is where coop mounts coop-gate-slot in every box, on PATH, so a project's full gate
// opts into the host-wide gate slots with one word in front of it: `coop-gate-slot make check`.
const GateSlotPath = "/usr/local/bin/coop-gate-slot"

// GateTmpPath is the folder a gate keeps its temp files in. A Docker box that carries coop-gate-slot
// mounts it as memory (gateTmpfs), because a box's /tmp sits on the runtime's disk: on OrbStack an
// fsync there takes several times what it takes on the host, with stalls of over a second, and Go's
// race tests sync many small files. coop-gate-slot gives each gate a folder of its own in it and
// points TMPDIR and GOTMPDIR there; the agent's own work and /tmp stay as they were.
const GateTmpPath = "/tmp/coop-gate"

// gateTmpfsOptions are that folder's mount options. Go runs its test binaries from there, so it
// allows programs; nothing else is granted.
const gateTmpfsOptions = "rw,exec,nosuid,nodev"

// gateTmpfs is the --tmpfs value for GateTmpPath. memory is the box's limit (COOP_MEMORY), empty for
// none. Files in a tmpfs count against that limit, so the folder may take a quarter of it, and never
// less than 512m, which a build needs. A box with no limit gets 2g.
func gateTmpfs(memory string) string {
	size := int64(2 << 30)
	if limit, ok := memoryBytes(memory); ok && limit > 0 {
		size = max(limit/4, 512<<20)
	}
	unit, shift := "m", 20
	if size%(1<<30) == 0 {
		unit, shift = "g", 30
	}
	return GateTmpPath + ":" + gateTmpfsOptions + ",size=" + strconv.FormatInt(size>>shift, 10) + unit
}

// memoryForm is what COOP_MEMORY and box.memory accept: Docker's --memory forms.
var memoryForm = regexp.MustCompile(`(?i)^(\d+(?:\.\d*)?|\.\d+) ?(?:([kmgtp])(?:i?b)?|b)?$`)

// memoryBytes reads a --memory value as Docker does: k, m, g, t and p are powers of 1024, with or
// without the b or ib after them. It reports false for a value it cannot read or hold.
func memoryBytes(memory string) (int64, bool) {
	m := memoryForm.FindStringSubmatch(strings.TrimSpace(memory))
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	if m[2] != "" {
		n *= float64(int64(1) << (10 * (1 + strings.Index("kmgtp", strings.ToLower(m[2])))))
	}
	if n >= 1<<62 {
		return 0, false
	}
	return int64(n), true
}

// IsGateTmpfs reports whether value is the --tmpfs value a box with that memory limit gets, size
// included. A filtered box may ask for no other tmpfs.
func IsGateTmpfs(value, memory string) bool { return value == gateTmpfs(memory) }

// GateSlotScript is the coop-gate-slot helper. Every box of a project mounts the project's cache
// volume as ~/.cache, so a flock on a file there is seen by all of them: the project's own boxes and
// its forks share the slots (isolated forks share their own set). Different projects no longer
// share a cache, so each has its own two slots. Each of the two slots is
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
//
// A gate with no TMPDIR gets a temp folder of its own, made inside GateTmpPath when the box has that
// folder and the helper can write to it, and removed when the gate ends. TMPDIR is set to it, and so
// is GOTMPDIR unless the gate set one. A gate that already has a TMPDIR keeps it, and Go's temp
// folder, which follows it, so a project that points its temp files at a bigger disk is left alone.
// Where the box has no such folder, such as a runtime that mounts no memory folder, the gate keeps
// the temp folder it had. A gate killed before it can clean up leaves its folder, so each gate starts
// by removing the folders more than four hours old.
func GateSlotScript() string { return gateSlotScript }

const gateSlotScript = `#!/bin/sh
# coop-gate-slot — run a full gate once it holds one of this host's two gate slots.
#
#   coop-gate-slot make check
#
# Every box of a project shares ~/.cache, so the slots there hold for the project and its forks. A third gate waits for one of
# the first two to finish, and its command starts only then. A gate inside a gate runs at once, in
# the outer gate's slot. A gate with no TMPDIR keeps its temp files in a folder of its own in
# /tmp/coop-gate, which is memory where the runtime allows it: a box's /tmp is on a disk where every
# fsync is slow.
[ "${1-}" != "--" ] || shift
[ "$#" -gt 0 ] || { echo "usage: coop-gate-slot [--] <command> [args...]" >&2; exit 2; }
tmp=/tmp/coop-gate
run=
if [ -z "${TMPDIR-}" ] && [ -d "$tmp" ] && [ -w "$tmp" ]; then
	# A gate killed before it could clean up leaves its folder, so clear the ones that are hours old.
	find "$tmp" -maxdepth 1 -type d -name 'run.*' -mmin +240 -exec rm -rf {} + 2>/dev/null
	if run=$(mktemp -d "$tmp/run.XXXXXX" 2>/dev/null); then
		# Only an exit runs this: a trap on TERM or INT would hold the signal until the gate ends.
		trap 'rm -rf "$run"' EXIT
		export TMPDIR="$run" GOTMPDIR="${GOTMPDIR:-$run}"
	else
		run=
	fi
fi
# With a folder to remove, the gate runs under this script, which removes it after. Without one the
# gate takes this script's place.
launch() { [ -n "$run" ] || exec "$@"; "$@"; exit $?; }
[ -z "${COOP_GATE_SLOT_HELD-}" ] || launch "$@"
dir="$HOME/.cache/coop-gate/slots"
if ! command -v flock >/dev/null 2>&1 || ! mkdir -p "$dir" 2>/dev/null; then
	echo "coop-gate-slot: no gate slot is available here (flock or $dir is missing); running the gate without one" >&2
	launch "$@"
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
