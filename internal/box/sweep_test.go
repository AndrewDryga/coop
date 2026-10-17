package box

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/processidentity"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/starttoken"
)

// fakeRuntimeScript is a container runtime backed by one file per container: $COOP_TEST_BOXES/<id>
// holds that container's labels JSON, exactly as `inspect --format {{json .Config.Labels}}` prints
// it. Every call is appended to $COOP_TEST_EVENTS, so a test can prove what the sweep touched — and
// what it never asked about. COOP_TEST_FAILURE=<command> fails that command, and =garbled makes a
// listing print a line no runtime would. No container daemon is involved at any point.
const fakeRuntimeScript = `#!/bin/sh
printf '%s\n' "$*" >> "$COOP_TEST_EVENTS"
if [ -n "$COOP_TEST_FAILURE" ] && [ "$COOP_TEST_FAILURE" = "$1" ]; then
	echo "fake runtime: $1 failed" >&2
	exit 41
fi
case "$1" in
ps)
	if [ "$COOP_TEST_FAILURE" = garbled ]; then
		echo 'not a listing'
		exit 0
	fi
	shift
	format=
	previous=
	for arg do
		[ "$previous" = --format ] && format=$arg
		previous=$arg
	done
	for f in "$COOP_TEST_BOXES"/*; do
		[ -e "$f" ] || continue
		match=1
		for filter in "$@"; do
			case "$filter" in
			label=*=*)
				pair=${filter#label=}
				grep -qF "\"${pair%%=*}\":\"${pair#*=}\"" "$f" || match=0
				;;
			label=*)
				grep -qF "\"${filter#label=}\":" "$f" || match=0
				;;
			network=*)
				grep -qF "\"network\":\"${filter#network=}\"" "$f" || match=0
				;;
			esac
		done
		[ "$match" = 1 ] || continue
		if [ -z "$format" ]; then
			printf '%s\n' "$(basename "$f")"
			continue
		fi
		# A one-call listing: the id, then each label the format names, as one JSON array.
		line="[\"$(basename "$f")\""
		for name in $(printf '%s' "$format" | grep -o '.Label "[^"]*"' | sed 's/^.Label "//; s/"$//'); do
			line="$line,\"$(sed -n "s/.*\"$name\":\"\([^\"]*\)\".*/\1/p" "$f")\""
		done
		printf '%s]\n' "$line"
	done
	;;
inspect)
	cat "$COOP_TEST_BOXES/$4"
	;;
rm)
	shift 2
	for id in "$@"; do rm -f "$COOP_TEST_BOXES/$id"; done
	;;
network)
	# One file per network under $COOP_TEST_NETWORKS/<id>: its labels JSON. A container is attached
	# to a network when its labels carry "network":"<id>" (see the ps filter above).
	case "$2" in
	ls)
		shift 2
		for f in "$COOP_TEST_NETWORKS"/*; do
			[ -e "$f" ] || continue
			match=1
			for filter in "$@"; do
				case "$filter" in
				label=*=*)
					pair=${filter#label=}
					grep -qF "\"${pair%%=*}\":\"${pair#*=}\"" "$f" || match=0
					;;
				label=*)
					grep -qF "\"${filter#label=}\":" "$f" || match=0
					;;
				esac
			done
			[ "$match" = 1 ] && printf '%s\n' "$(basename "$f")"
		done
		;;
	inspect)
		key=$(printf '%s' "$4" | sed 's/.*"\(.*\)".*/\1/')
		sed -n "s/.*\"$key\":\"\([^\"]*\)\".*/\1/p" "$COOP_TEST_NETWORKS/$5"
		;;
	rm)
		rm -f "$COOP_TEST_NETWORKS/$3"
		;;
	esac
	;;
volume)
	# One file per volume under $COOP_TEST_VOLUMES/<name>: its labels JSON.
	case "$2" in
	ls)
		shift 2
		for f in "$COOP_TEST_VOLUMES"/*; do
			[ -e "$f" ] || continue
			match=1
			for filter in "$@"; do
				case "$filter" in
				label=*=*)
					pair=${filter#label=}
					grep -qF "\"${pair%%=*}\":\"${pair#*=}\"" "$f" || match=0
					;;
				esac
			done
			[ "$match" = 1 ] && printf '%s\n' "$(basename "$f")"
		done
		;;
	rm)
		# A volume labeled "in-use" stands for one a container still holds, which the runtime refuses.
		if grep -qF '"in-use"' "$COOP_TEST_VOLUMES/$3"; then
			echo "fake runtime: volume $3 is in use" >&2
			exit 1
		fi
		rm -f "$COOP_TEST_VOLUMES/$3"
		;;
	esac
	;;
esac
exit 0
`

// fakeRuntime installs the script above and returns the runtime plus the directory whose files are
// the containers it can see.
func fakeRuntime(t *testing.T) (runtime.Runtime, string) {
	t.Helper()
	dir := t.TempDir()
	cli := filepath.Join(dir, "runtime")
	boxes := filepath.Join(dir, "boxes")
	if err := os.MkdirAll(boxes, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cli, []byte(fakeRuntimeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_BOXES", boxes)
	t.Setenv("COOP_TEST_EVENTS", filepath.Join(dir, "events"))
	networks := filepath.Join(dir, "networks")
	if err := os.MkdirAll(networks, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_NETWORKS", networks)
	volumes := filepath.Join(dir, "volumes")
	if err := os.MkdirAll(volumes, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_VOLUMES", volumes)
	return runtime.Runtime{Name: cli}, boxes
}

// addFakeNetwork registers a network with the labels compose gives it; project "" means a network
// compose did not create.
func addFakeNetwork(t *testing.T, id, name, project string) {
	t.Helper()
	labels := map[string]string{"name": name}
	if project != "" {
		labels[composeProjectLabel] = project
	}
	data, err := json.Marshal(labels)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("COOP_TEST_NETWORKS"), id), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func remainingFakeNetworks(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(os.Getenv("COOP_TEST_NETWORKS"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

func addFakeBox(t *testing.T, boxes, id string, labels map[string]string) {
	t.Helper()
	addFakeContainer(t, boxes, id, LabelBox, labels)
}

// addFakeContainer registers a coop container of kind: a box, a broker helper, a project service.
func addFakeContainer(t *testing.T, boxes, id, kind string, labels map[string]string) {
	t.Helper()
	labels[LabelKey] = kind
	data, err := json.Marshal(labels)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(boxes, id), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func fakeRuntimeEvents(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("COOP_TEST_EVENTS"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func liveSupervisorLabel(t *testing.T, workspace string) string {
	t.Helper()
	value := supervisorLabelValue(workspaceScope(workspace), os.Getpid())
	if value == "" {
		t.Skip("this host has no stable process identity, so no box can be labeled")
	}
	return value
}

// deadSupervisorLabel names a process that is provably not the one that launched the box: the pid is
// live but the recorded start token is another process's, which is PID reuse — the case that must
// read as dead without waiting for any pid to actually be recycled.
func deadSupervisorLabel(workspace string) string {
	return supervisorLabel(workspace, os.Getpid())
}

// goneSupervisorLabel names a process that has exited.
func goneSupervisorLabel(t *testing.T, workspace string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return supervisorLabel(workspace, cmd.Process.Pid)
}

// supervisorLabel builds a well-formed label for pid carrying a start token that is NOT pid's, so
// the identity test can only ever answer "gone" or "reused" — never a live match.
func supervisorLabel(workspace string, pid int) string {
	return supervisorLabelVersion + ":" + workspaceScope(workspace) + ":" + strconv.Itoa(pid) + ":" + starttoken.Stale()
}

// The whole decision table, in one place: only a box of THIS workspace whose recorded supervisor is
// provably dead may be removed. Everything else — a live supervisor, another workspace's box, a box
// with no supervisor label, and a label this version cannot read — is left alone, and the ones coop
// cannot attribute at all are reported so an operator can see them.
func TestSurveyOrphanBoxesDecisionTable(t *testing.T) {
	rt, boxes := fakeRuntime(t)
	workspace, foreign := t.TempDir(), t.TempDir()
	dead := deadSupervisorLabel(workspace)
	gone := goneSupervisorLabel(t, workspace)
	addFakeBox(t, boxes, "dead-in-scope", map[string]string{LabelHost: dead})
	addFakeBox(t, boxes, "gone-in-scope", map[string]string{LabelHost: gone})
	addFakeBox(t, boxes, "live-in-scope", map[string]string{LabelHost: liveSupervisorLabel(t, workspace)})
	addFakeBox(t, boxes, "dead-other-repo", map[string]string{LabelHost: deadSupervisorLabel(foreign)})
	addFakeBox(t, boxes, "dead-other-fork", map[string]string{
		LabelHost: deadSupervisorLabel(filepath.Join(foreign, "-forks", "perf")), LabelForkOwner: "v1-someone-else",
	})
	addFakeBox(t, boxes, "unlabeled", map[string]string{})
	addFakeBox(t, boxes, "malformed-version", map[string]string{LabelHost: "v0:" + workspaceScope(workspace) + ":2:linux-proc-v1:1"})
	addFakeBox(t, boxes, "malformed-pid", map[string]string{LabelHost: supervisorLabelVersion + ":" + workspaceScope(workspace) + ":nope:linux-proc-v1:1"})
	addFakeBox(t, boxes, "malformed-token", map[string]string{LabelHost: supervisorLabelVersion + ":" + workspaceScope(workspace) + ":2:homemade-v9:1"})
	// A development service whose Compose file planted a box's labels is no box at all.
	addFakeBox(t, boxes, "planted-dev-service", map[string]string{LabelHost: gone, composeProjectLabel: ComposeProject(workspace)})
	// Broker helpers are judged by the same proof, on their own label, and are never reported: they
	// are not boxes anyone started.
	addFakeContainer(t, boxes, "broker-dead-in-scope", LabelBroker, map[string]string{LabelHost: gone})
	addFakeContainer(t, boxes, "broker-live-in-scope", LabelBroker, map[string]string{LabelHost: liveSupervisorLabel(t, workspace)})
	addFakeContainer(t, boxes, "broker-dead-other-repo", LabelBroker, map[string]string{LabelHost: deadSupervisorLabel(foreign)})
	addFakeContainer(t, boxes, "broker-unlabeled", LabelBroker, map[string]string{})
	addFakeContainer(t, boxes, "broker-planted", LabelBroker, map[string]string{LabelHost: gone, composeProjectLabel: ComposeProject(workspace)})
	// Every other coop container is another sweep's, however dead its supervisor.
	addFakeContainer(t, boxes, "dead-service", LabelService, map[string]string{LabelHost: gone})
	addFakeContainer(t, boxes, "dead-task-channel", LabelTaskChannel, map[string]string{LabelHost: gone})

	survey, err := SurveyOrphanBoxes(context.Background(), rt, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if survey.Checked != 10 {
		t.Errorf("checked %d boxes, want 10", survey.Checked)
	}
	var brokers []string
	for _, broker := range survey.Brokers {
		brokers = append(brokers, broker.ID)
		if broker.Evidence != gone || broker.PID <= 1 {
			t.Errorf("orphaned broker %#v must carry the label evidence and the pid it rests on", broker)
		}
	}
	if want := []string{"broker-dead-in-scope"}; !slices.Equal(brokers, want) {
		t.Errorf("orphaned brokers = %v, want %v", brokers, want)
	}
	var orphans []string
	for _, orphan := range survey.Orphans {
		orphans = append(orphans, orphan.ID)
		if orphan.Evidence == "" || orphan.PID <= 1 {
			t.Errorf("orphan %#v must carry the label evidence and the pid it rests on", orphan)
		}
	}
	slices.Sort(orphans)
	if want := []string{"dead-in-scope", "gone-in-scope"}; !slices.Equal(orphans, want) {
		t.Errorf("orphans = %v, want %v", orphans, want)
	}
	unattributed := append([]string(nil), survey.Unattributed...)
	slices.Sort(unattributed)
	want := []string{"malformed-pid", "malformed-token", "malformed-version", "planted-dev-service", "unlabeled"}
	if !slices.Equal(unattributed, want) {
		t.Errorf("unattributed = %v, want %v", unattributed, want)
	}
	if strings.Contains(fakeRuntimeEvents(t), "rm ") {
		t.Errorf("a survey must never remove anything:\n%s", fakeRuntimeEvents(t))
	}
}

// The sweep removes exactly the orphans of its own workspace, by the dead supervisor's own label —
// and leaves the live-supervisor, foreign-workspace, and unlabeled boxes in place.
func TestReapOrphanBoxesRemovesOnlyProvablyDeadOwnScope(t *testing.T) {
	rt, boxes := fakeRuntime(t)
	workspace, foreign := t.TempDir(), t.TempDir()
	dead := deadSupervisorLabel(workspace)
	addFakeBox(t, boxes, "orphan-a", map[string]string{LabelHost: dead})
	addFakeBox(t, boxes, "orphan-b", map[string]string{LabelHost: dead}) // same dead coop, two boxes
	addFakeBox(t, boxes, "live", map[string]string{LabelHost: liveSupervisorLabel(t, workspace)})
	addFakeBox(t, boxes, "foreign", map[string]string{LabelHost: deadSupervisorLabel(foreign)})
	addFakeBox(t, boxes, "legacy", map[string]string{})
	gone := goneSupervisorLabel(t, workspace)
	addFakeBox(t, boxes, "planted-dev-service", map[string]string{LabelHost: gone, composeProjectLabel: ComposeProject(workspace)})
	// A killed coop's box left with --rm, but its broker stayed behind; a live run's broker stays put.
	lone := goneSupervisorLabel(t, workspace)
	addFakeContainer(t, boxes, "lone-broker", LabelBroker, map[string]string{LabelHost: lone})
	addFakeContainer(t, boxes, "orphan-broker", LabelBroker, map[string]string{LabelHost: dead})
	addFakeContainer(t, boxes, "live-broker", LabelBroker, map[string]string{LabelHost: liveSupervisorLabel(t, workspace)})

	n, err := ReapOrphanBoxes(context.Background(), rt, workspace)
	if err != nil || n != 2 {
		t.Fatalf("ReapOrphanBoxes = (%d, %v), want (2, nil): the brokers are removed but not counted as boxes", n, err)
	}
	left, err := os.ReadDir(boxes)
	if err != nil {
		t.Fatal(err)
	}
	var remaining []string
	for _, entry := range left {
		remaining = append(remaining, entry.Name())
	}
	slices.Sort(remaining)
	if want := []string{"foreign", "legacy", "live", "live-broker", "planted-dev-service"}; !slices.Equal(remaining, want) {
		t.Fatalf("remaining containers = %v, want %v", remaining, want)
	}
	events := fakeRuntimeEvents(t)
	// One removal pass per dead supervisor and kind, filtered by its exact label — never by id list,
	// age, or name.
	for _, filter := range []string{
		"label=coop=box --filter label=" + LabelHost + "=" + dead,
		"label=coop=broker --filter label=" + LabelHost + "=" + dead,
		"label=coop=broker --filter label=" + LabelHost + "=" + lone,
	} {
		if !strings.Contains(events, "ps -q -a --filter "+filter+"\n") {
			t.Errorf("reap did not use the exact-label filter %s:\n%s", filter, events)
		}
	}
	for _, id := range []string{"live", "foreign", "legacy", "live-broker"} {
		if strings.Contains(events, "rm -f "+id) {
			t.Errorf("reap removed %s, which it must never touch:\n%s", id, events)
		}
	}
}

// A sweep that finds nothing to remove asks the runtime exactly once, however many live boxes,
// brokers and other coop containers there are: every start of a loop, fork or build pays for it,
// and the scripted process E2Es pin that count.
func TestReapOrphanBoxesWithNothingToRemoveIsOneListing(t *testing.T) {
	rt, boxes := fakeRuntime(t)
	workspace := t.TempDir()
	live := liveSupervisorLabel(t, workspace)
	addFakeBox(t, boxes, "live", map[string]string{LabelHost: live})
	addFakeContainer(t, boxes, "live-broker", LabelBroker, map[string]string{LabelHost: live})
	addFakeContainer(t, boxes, "task-channel", LabelTaskChannel, map[string]string{})

	if n, err := ReapOrphanBoxes(context.Background(), rt, workspace); err != nil || n != 0 {
		t.Fatalf("ReapOrphanBoxes = (%d, %v), want (0, nil)", n, err)
	}
	events := strings.Split(strings.TrimSpace(fakeRuntimeEvents(t)), "\n")
	want := `ps -a --no-trunc --filter label=coop --format [{{json .ID}},{{json (.Label "coop")}},{{json (.Label "coop.host")}},{{json (.Label "com.docker.compose.project")}}]`
	if len(events) != 1 || events[0] != want {
		t.Fatalf("runtime calls = %q, want the one listing %q", events, want)
	}
}

// A runtime that cannot answer, or answers with a listing it cannot read, is not evidence that
// nothing is running: the sweep reports the failure and removes nothing.
func TestReapOrphanBoxesFailsClosed(t *testing.T) {
	for _, failure := range []string{"ps", "garbled"} {
		t.Run(failure, func(t *testing.T) {
			rt, boxes := fakeRuntime(t)
			workspace := t.TempDir()
			addFakeBox(t, boxes, "orphan", map[string]string{LabelHost: deadSupervisorLabel(workspace)})
			t.Setenv("COOP_TEST_FAILURE", failure)

			n, err := ReapOrphanBoxes(context.Background(), rt, workspace)
			if n != 0 || err == nil {
				t.Fatalf("ReapOrphanBoxes = (%d, %v), want (0, error)", n, err)
			}
			if _, statErr := os.Stat(filepath.Join(boxes, "orphan")); statErr != nil {
				t.Fatalf("a failed query removed a box: %v", statErr)
			}
		})
	}
}

// Every box records the host process supervising it, in a versioned form that reads back — and a
// run with a policy repo is scoped to THAT durable repo, not to the disposable tree it mounts.
func TestAssembleArgsSupervisorLabel(t *testing.T) {
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: t.TempDir()}
	mounts := []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}}
	args := func(spec RunSpec) []string {
		return assembleArgs(cfg, true, spec, mounts, "/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, "", "")
	}
	value := ""
	for _, arg := range args(RunSpec{Image: "i", Repo: "/r"}) {
		if rest, ok := strings.CutPrefix(arg, LabelHost+"="); ok {
			value = rest
		}
	}
	if value == "" {
		t.Fatalf("no %s label on an ordinary box", LabelHost)
	}
	ref, ok := parseSupervisorLabel(value)
	if !ok || ref.pid != os.Getpid() || ref.scope != workspaceScope("/r") ||
		ref.token != processidentity.StartToken(os.Getpid()) {
		t.Fatalf("%s=%q parsed to %#v, want this process in /r's scope", LabelHost, value, ref)
	}
	// A review/gate box mounts a disposable candidate; scope it to the repo that will still exist.
	review := args(RunSpec{Image: "i", Repo: "/tmp/candidate", PolicyRepo: "/r"})
	if !containsSeq(review, []string{"--label", LabelHost + "=" + value}) {
		t.Errorf("a policy-repo run must be scoped to the policy repo: %v", review)
	}
}

// The network sweep removes only networks of coop's own compose projects that nothing is attached
// to: a network still used by a stopped service stays, and a human's compose project is never even
// asked about.
func TestReapOrphanNetworksRemovesOnlyUnusedCoopNetworks(t *testing.T) {
	rt, boxes := fakeRuntime(t)
	addFakeNetwork(t, "unused", "coop-emisar-a922f3c5_default", "coop-emisar-a922f3c5")
	addFakeNetwork(t, "unused-run", "coop-emisar-a922f3c5-run-c4d5e6f7_default", "coop-emisar-a922f3c5-run-c4d5e6f7")
	addFakeNetwork(t, "used", "coop-emisar-b1c2d3e4_default", "coop-emisar-b1c2d3e4")
	addFakeNetwork(t, "theirs", "responder-kernel_default", "responder-kernel")
	addFakeNetwork(t, "plain", "bridge", "")
	addFakeBox(t, boxes, "stopped-db", map[string]string{"network": "used"})

	n, err := ReapOrphanNetworks(context.Background(), rt)
	if err != nil || n != 2 {
		t.Fatalf("ReapOrphanNetworks = (%d, %v), want (2, nil)", n, err)
	}
	if got, want := remainingFakeNetworks(t), []string{"plain", "theirs", "used"}; !slices.Equal(got, want) {
		t.Fatalf("remaining networks = %v, want %v", got, want)
	}
	events := fakeRuntimeEvents(t)
	if !strings.Contains(events, "network ls -q --filter label="+composeProjectLabel+"\n") {
		t.Errorf("sweep did not list compose networks by label:\n%s", events)
	}
	if strings.Contains(events, "network=theirs") || strings.Contains(events, "network rm theirs") {
		t.Errorf("sweep touched a human's compose project:\n%s", events)
	}
	if strings.Contains(events, "network rm used") {
		t.Errorf("sweep removed a network a stopped container still uses:\n%s", events)
	}
}

// A runtime that cannot list networks is not evidence there are none.
func TestReapOrphanNetworksFailsClosed(t *testing.T) {
	rt, _ := fakeRuntime(t)
	addFakeNetwork(t, "unused", "coop-emisar-a922f3c5_default", "coop-emisar-a922f3c5")
	t.Setenv("COOP_TEST_FAILURE", "network")
	if n, err := ReapOrphanNetworks(context.Background(), rt); err == nil || n != 0 {
		t.Fatalf("ReapOrphanNetworks = (%d, %v), want a failure and nothing removed", n, err)
	}
	if got := remainingFakeNetworks(t); !slices.Equal(got, []string{"unused"}) {
		t.Fatalf("remaining networks = %v, want the network untouched", got)
	}
}
