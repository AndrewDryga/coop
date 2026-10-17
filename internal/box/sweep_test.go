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
	return supervisorLabel(workspace, gonePID(t))
}

// gonePID is the pid of a process that has exited.
func gonePID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// supervisorLabel builds a well-formed label of this host for pid carrying a start token that is NOT
// pid's, so the identity test can only ever answer "gone" or "reused" — never a live match.
func supervisorLabel(workspace string, pid int) string {
	return supervisorLabelOn(localHost(), workspace, pid)
}

// localHost is this test binary's host field; TestHostFieldNamesTheHostAndTheNamespace proves the
// binary has one, so an empty one fails there and not as a puzzle in every sweep test.
func localHost() string {
	host, _ := thisHost()
	return host
}

// supervisorLabelOn is supervisorLabel as a coop on host wrote it.
func supervisorLabelOn(host, workspace string, pid int) string {
	return supervisorLabelVersion + ":" + workspaceScope(workspace) + ":" + host + ":" + strconv.Itoa(pid) + ":" + starttoken.Stale()
}

// judgeLegacyLabels makes the test run as a coop on a host (true) or inside a container (false),
// whatever PID namespace the test binary itself runs in: a loop box or a dev container is not the
// host's.
func judgeLegacyLabels(t *testing.T, onHost bool) {
	t.Helper()
	saved := legacyLabelsAreLocal
	legacyLabelsAreLocal = func() bool { return onHost }
	t.Cleanup(func() { legacyLabelsAreLocal = saved })
}

// legacySupervisorLabel is a v1 label, as coop wrote it before labels named their host.
func legacySupervisorLabel(workspace string, pid int, token string) string {
	return legacySupervisorLabelVersion + ":" + workspaceScope(workspace) + ":" + strconv.Itoa(pid) + ":" + token
}

// otherHost is the host field of a coop on another host, or in another PID namespace.
const otherHost = "0123456789abcdef01234567"

// The whole decision table, in one place: only a box of THIS workspace whose recorded supervisor is
// provably dead may be removed. Everything else — a live supervisor, another workspace's box, another
// host's box, a box with no supervisor label, and a label this version cannot read — is left alone,
// and the ones coop cannot attribute at all are reported so an operator can see them. So are this
// workspace's containers that another host supervises, services included, which nobody else would
// ever report.
func TestSurveyOrphanBoxesDecisionTable(t *testing.T) {
	judgeLegacyLabels(t, true)
	rt, boxes := fakeRuntime(t)
	workspace, foreign := t.TempDir(), t.TempDir()
	dead := deadSupervisorLabel(workspace)
	gone := goneSupervisorLabel(t, workspace)
	scope, host := workspaceScope(workspace), localHost()
	addFakeBox(t, boxes, "dead-in-scope", map[string]string{LabelHost: dead})
	addFakeBox(t, boxes, "gone-in-scope", map[string]string{LabelHost: gone})
	addFakeBox(t, boxes, "live-in-scope", map[string]string{LabelHost: liveSupervisorLabel(t, workspace)})
	addFakeBox(t, boxes, "dead-other-repo", map[string]string{LabelHost: deadSupervisorLabel(foreign)})
	addFakeBox(t, boxes, "dead-other-fork", map[string]string{
		LabelHost: deadSupervisorLabel(filepath.Join(foreign, "-forks", "perf")), LabelForkOwner: "v1-someone-else",
	})
	// Another host's pids, or another PID namespace's, mean nothing here: someone else's box, not an
	// unreadable one. A v1 label has no host field and is judged as it always was, unless its token is
	// another platform's.
	addFakeBox(t, boxes, "gone-other-host", map[string]string{LabelHost: supervisorLabelOn(otherHost, workspace, gonePID(t))})
	addFakeBox(t, boxes, "gone-other-host-other-repo", map[string]string{LabelHost: supervisorLabelOn(otherHost, foreign, gonePID(t))})
	addFakeBox(t, boxes, "gone-in-scope-v1", map[string]string{LabelHost: legacySupervisorLabel(workspace, gonePID(t), starttoken.Stale())})
	addFakeBox(t, boxes, "gone-other-platform-v1", map[string]string{LabelHost: legacySupervisorLabel(workspace, gonePID(t), starttoken.Foreign())})
	addFakeBox(t, boxes, "unlabeled", map[string]string{})
	addFakeBox(t, boxes, "malformed-version", map[string]string{LabelHost: "v0:" + scope + ":2:" + starttoken.Stale()})
	addFakeBox(t, boxes, "malformed-host", map[string]string{LabelHost: supervisorLabelVersion + ":" + scope + ":not-a-host:2:" + starttoken.Stale()})
	addFakeBox(t, boxes, "malformed-pid", map[string]string{LabelHost: supervisorLabelVersion + ":" + scope + ":" + host + ":nope:" + starttoken.Stale()})
	addFakeBox(t, boxes, "malformed-token", map[string]string{LabelHost: supervisorLabelVersion + ":" + scope + ":" + host + ":2:homemade-v9:1"})
	// A development service whose Compose file planted a box's labels is no box at all.
	addFakeBox(t, boxes, "planted-dev-service", map[string]string{LabelHost: gone, composeProjectLabel: ComposeProject(workspace)})
	// Broker helpers are judged by the same proof, on their own label, and are never reported: they
	// are not boxes anyone started.
	addFakeContainer(t, boxes, "broker-dead-in-scope", LabelBroker, map[string]string{LabelHost: gone})
	addFakeContainer(t, boxes, "broker-live-in-scope", LabelBroker, map[string]string{LabelHost: liveSupervisorLabel(t, workspace)})
	addFakeContainer(t, boxes, "broker-dead-other-repo", LabelBroker, map[string]string{LabelHost: deadSupervisorLabel(foreign)})
	addFakeContainer(t, boxes, "broker-gone-other-host", LabelBroker, map[string]string{LabelHost: supervisorLabelOn(otherHost, workspace, gonePID(t))})
	addFakeContainer(t, boxes, "broker-unlabeled", LabelBroker, map[string]string{})
	addFakeContainer(t, boxes, "broker-planted", LabelBroker, map[string]string{LabelHost: gone, composeProjectLabel: ComposeProject(workspace)})
	// Every other coop container is another sweep's, however dead its supervisor. Only a run service
	// of this workspace that another host supervises is reported, beside that host's boxes: no sweep
	// would ever remove it.
	addFakeContainer(t, boxes, "dead-service", LabelService, map[string]string{LabelHost: gone})
	addFakeContainer(t, boxes, "dead-task-channel", LabelTaskChannel, map[string]string{LabelHost: gone})
	theirRun := ComposeProjectFor(workspace, "their-loop")
	addFakeService(t, boxes, "service-other-host", theirRun, supervisorLabelOn(otherHost, workspace, gonePID(t)))
	addFakeService(t, boxes, "service-other-host-dev-project", ComposeProject(workspace), supervisorLabelOn(otherHost, workspace, gonePID(t)))
	addFakeService(t, boxes, "service-other-host-other-repo", ComposeProjectFor(foreign, "their-loop"), supervisorLabelOn(otherHost, foreign, gonePID(t)))

	survey, err := SurveyOrphanBoxes(context.Background(), rt, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if survey.Checked != 15 {
		t.Errorf("checked %d boxes, want 15", survey.Checked)
	}
	if survey.HostError != nil {
		t.Errorf("survey host error = %v, want none: every test binary can name its host", survey.HostError)
	}
	otherHosts := append([]string(nil), survey.OtherHost...)
	slices.Sort(otherHosts)
	if want := []string{"broker-gone-other-host", "gone-other-host", "gone-other-platform-v1", "service-other-host"}; !slices.Equal(otherHosts, want) {
		t.Errorf("another host's containers = %v, want %v", otherHosts, want)
	}
	if want := []string{theirRun}; !slices.Equal(survey.OtherHostProjects, want) {
		t.Errorf("another host's run projects = %v, want %v", survey.OtherHostProjects, want)
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
	if want := []string{"dead-in-scope", "gone-in-scope", "gone-in-scope-v1"}; !slices.Equal(orphans, want) {
		t.Errorf("orphans = %v, want %v", orphans, want)
	}
	unattributed := append([]string(nil), survey.Unattributed...)
	slices.Sort(unattributed)
	want := []string{"malformed-host", "malformed-pid", "malformed-token", "malformed-version", "planted-dev-service", "unlabeled"}
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

// A session-service run works in a remote fork's workspace, often gone before any sweep runs there,
// so its boxes and brokers are judged from any workspace: removed when this host's supervisor of the
// run is provably dead, kept when it lives or when another host's coop supervises the run. Another
// workspace's ordinary run is still left to its own workspace.
func TestReapOrphanBoxesRemovesADeadSessionRunsContainersFromAnyWorkspace(t *testing.T) {
	rt, boxes := fakeRuntime(t)
	workspace, gone := t.TempDir(), filepath.Join(t.TempDir(), "remote-fork")
	session := map[string]string{LabelRun: "session-" + strings.Repeat("a", 24)}
	with := func(label string, extra map[string]string) map[string]string {
		labels := map[string]string{LabelHost: label}
		for key, value := range extra {
			labels[key] = value
		}
		return labels
	}
	dead := goneSupervisorLabel(t, gone)
	addFakeContainer(t, boxes, "session-broker-dead", LabelBroker, with(dead, session))
	addFakeBox(t, boxes, "session-box-dead", with(dead, session))
	addFakeContainer(t, boxes, "session-broker-live", LabelBroker, with(liveSupervisorLabel(t, gone), session))
	addFakeContainer(t, boxes, "session-broker-other-host", LabelBroker, with(supervisorLabelOn(otherHost, gone, gonePID(t)), session))
	addFakeContainer(t, boxes, "plain-broker-other-workspace", LabelBroker, with(goneSupervisorLabel(t, gone), nil))

	n, err := ReapOrphanBoxes(context.Background(), rt, workspace)
	if err != nil || n != 1 {
		t.Fatalf("ReapOrphanBoxes = (%d, %v), want the dead session run's one box", n, err)
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
	if want := []string{"plain-broker-other-workspace", "session-broker-live", "session-broker-other-host"}; !slices.Equal(remaining, want) {
		t.Fatalf("remaining containers = %v, want %v", remaining, want)
	}
}

// One Docker daemon can serve two hosts, or a host and a container, at the same repository path, and
// a pid recorded on one of them is gone or another process on the other. So both sweeps remove only
// what this host's own label names, or a v1 label, which has no host field and keeps its old meaning;
// another host's boxes, brokers and services stay, and so does a v1 label carrying another platform's
// start token.
func TestSweepsJudgeOnlyThisHostsSupervisors(t *testing.T) {
	judgeLegacyLabels(t, true)
	workspace := t.TempDir()
	labels := map[string]string{
		"this-host":             supervisorLabel(workspace, gonePID(t)),
		"other-host":            supervisorLabelOn(otherHost, workspace, gonePID(t)),
		"old-v1":                legacySupervisorLabel(workspace, gonePID(t), starttoken.Stale()),
		"old-v1-other-platform": legacySupervisorLabel(workspace, gonePID(t), starttoken.Foreign()),
	}
	t.Run("boxes", func(t *testing.T) {
		rt, boxes := fakeRuntime(t)
		for name, label := range labels {
			addFakeBox(t, boxes, name, map[string]string{LabelHost: label})
			addFakeContainer(t, boxes, name+"-broker", LabelBroker, map[string]string{LabelHost: label})
		}
		survey, err := SurveyOrphanBoxes(context.Background(), rt, workspace)
		if err != nil || len(survey.Unattributed) != 0 {
			t.Fatalf("survey = %+v, %v: another host's box is someone else's, not an unreadable one", survey, err)
		}
		want := []string{"old-v1-other-platform", "old-v1-other-platform-broker", "other-host", "other-host-broker"}
		reported := append([]string(nil), survey.OtherHost...)
		slices.Sort(reported)
		if !slices.Equal(reported, want) {
			t.Fatalf("survey reports %v as another host's, want %v", reported, want)
		}
		if n, err := ReapOrphanBoxes(context.Background(), rt, workspace); err != nil || n != 2 {
			t.Fatalf("ReapOrphanBoxes = (%d, %v), want this host's and the v1 box", n, err)
		}
		if got := remainingFakeEntries(t, boxes); !slices.Equal(got, want) {
			t.Fatalf("containers left = %v, want %v", got, want)
		}
	})
	t.Run("services", func(t *testing.T) {
		rt, boxes := fakeRuntime(t)
		for name, label := range labels {
			project := ComposeProjectFor(workspace, name)
			addFakeService(t, boxes, name+"-db", project, label)
			addFakeVolume(t, name+"_pgdata", project)
		}
		survey, err := SurveyOrphanBoxes(context.Background(), rt, workspace)
		reported := append([]string(nil), survey.OtherHost...)
		slices.Sort(reported)
		if err != nil || !slices.Equal(reported, []string{"old-v1-other-platform-db", "other-host-db"}) || len(survey.OtherHostProjects) != 2 {
			t.Fatalf("survey = %+v, %v; want another host's two run services and their projects", survey, err)
		}
		if n, err := ReapOrphanServices(context.Background(), rt, workspace); err != nil || n != 2 {
			t.Fatalf("ReapOrphanServices = (%d, %v), want this host's and the v1 run", n, err)
		}
		if got, want := remainingFakeEntries(t, boxes), []string{"old-v1-other-platform-db", "other-host-db"}; !slices.Equal(got, want) {
			t.Fatalf("services left = %v, want %v", got, want)
		}
		if got, want := remainingFakeEntries(t, os.Getenv("COOP_TEST_VOLUMES")), []string{"old-v1-other-platform_pgdata", "other-host_pgdata"}; !slices.Equal(got, want) {
			t.Fatalf("volumes left = %v, want %v", got, want)
		}
	})
}

// A coop that cannot name its host (thisHost failed) writes an empty label, and cannot tell its own
// v2 labels from another host's: it removes none of them and reports their boxes as unattributed.
// A v1 label has no host field and is judged as before, and one with another platform's start
// token is still provably another host's.
func TestSweepsWithoutAHostJudgeOnlyV1Labels(t *testing.T) {
	judgeLegacyLabels(t, true)
	workspace := t.TempDir()
	if label := supervisorLabelFor(workspaceScope(workspace), "", os.Getpid()); label != "" {
		t.Fatalf("supervisorLabelFor with no host = %q, want no label", label)
	}
	labels := map[string]string{
		"v2-this-host":          supervisorLabel(workspace, gonePID(t)), // written while the id was readable
		"v2-other-host":         supervisorLabelOn(otherHost, workspace, gonePID(t)),
		"old-v1":                legacySupervisorLabel(workspace, gonePID(t), starttoken.Stale()),
		"old-v1-other-platform": legacySupervisorLabel(workspace, gonePID(t), starttoken.Foreign()),
	}
	ids := func(orphans []OrphanBox) []string {
		var ids []string
		for _, orphan := range orphans {
			ids = append(ids, orphan.ID)
		}
		return ids
	}
	t.Run("boxes", func(t *testing.T) {
		rt, boxes := fakeRuntime(t)
		for name, label := range labels {
			addFakeBox(t, boxes, name, map[string]string{LabelHost: label})
			addFakeContainer(t, boxes, name+"-broker", LabelBroker, map[string]string{LabelHost: label})
		}
		survey, err := surveyOrphanBoxes(context.Background(), rt, workspace, "")
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(survey.Unattributed)
		slices.Sort(survey.OtherHost)
		for _, check := range []struct {
			group     string
			got, want []string
		}{
			{"orphans", ids(survey.Orphans), []string{"old-v1"}},
			{"brokers", ids(survey.Brokers), []string{"old-v1-broker"}},
			{"unattributed", survey.Unattributed, []string{"v2-other-host", "v2-this-host"}},
			{"another host's", survey.OtherHost, []string{"old-v1-other-platform", "old-v1-other-platform-broker"}},
		} {
			if !slices.Equal(check.got, check.want) {
				t.Errorf("%s = %v, want %v", check.group, check.got, check.want)
			}
		}
	})
	t.Run("services", func(t *testing.T) {
		rt, boxes := fakeRuntime(t)
		for name, label := range labels {
			project := ComposeProjectFor(workspace, name)
			addFakeService(t, boxes, name+"-db", project, label)
			addFakeVolume(t, name+"_pgdata", project)
		}
		if n, err := reapOrphanServices(context.Background(), rt, workspace, ""); err != nil || n != 1 {
			t.Fatalf("reapOrphanServices with no host = (%d, %v), want only the v1 run", n, err)
		}
		want := []string{"old-v1-other-platform-db", "v2-other-host-db", "v2-this-host-db"}
		if got := remainingFakeEntries(t, boxes); !slices.Equal(got, want) {
			t.Fatalf("services left = %v, want %v", got, want)
		}
		want = []string{"old-v1-other-platform_pgdata", "v2-other-host_pgdata", "v2-this-host_pgdata"}
		if got := remainingFakeEntries(t, os.Getenv("COOP_TEST_VOLUMES")); !slices.Equal(got, want) {
			t.Fatalf("volumes left = %v, want %v", got, want)
		}
	})
}

// Inside a container, a v1 label could be another container's: it names no PID namespace, and a
// live supervisor there looks gone from here. So a containerized coop removes no v1 box, broker or
// run service, and lists them as another host's; a v2 label of its own is still judged.
func TestAContainerJudgesNoV1Label(t *testing.T) {
	judgeLegacyLabels(t, false)
	workspace := t.TempDir()
	labels := map[string]string{
		"own-v2": supervisorLabel(workspace, gonePID(t)),
		"old-v1": legacySupervisorLabel(workspace, gonePID(t), starttoken.Stale()),
	}
	t.Run("boxes", func(t *testing.T) {
		rt, boxes := fakeRuntime(t)
		for name, label := range labels {
			addFakeBox(t, boxes, name, map[string]string{LabelHost: label})
			addFakeContainer(t, boxes, name+"-broker", LabelBroker, map[string]string{LabelHost: label})
		}
		survey, err := surveyOrphanBoxes(context.Background(), rt, workspace, localHost())
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(survey.OtherHost)
		var orphans, brokers []string
		for _, orphan := range survey.Orphans {
			orphans = append(orphans, orphan.ID)
		}
		for _, broker := range survey.Brokers {
			brokers = append(brokers, broker.ID)
		}
		if !slices.Equal(orphans, []string{"own-v2"}) || !slices.Equal(brokers, []string{"own-v2-broker"}) ||
			!slices.Equal(survey.OtherHost, []string{"old-v1", "old-v1-broker"}) {
			t.Fatalf("orphans %v, brokers %v, another host's %v; want only own-v2 judged and old-v1 listed", orphans, brokers, survey.OtherHost)
		}
	})
	t.Run("services", func(t *testing.T) {
		rt, boxes := fakeRuntime(t)
		for name, label := range labels {
			project := ComposeProjectFor(workspace, name)
			addFakeService(t, boxes, name+"-db", project, label)
			addFakeVolume(t, name+"_pgdata", project)
		}
		if n, err := reapOrphanServices(context.Background(), rt, workspace, localHost()); err != nil || n != 1 {
			t.Fatalf("reapOrphanServices in a container = (%d, %v), want only its own v2 run", n, err)
		}
		if got, want := remainingFakeEntries(t, boxes), []string{"old-v1-db"}; !slices.Equal(got, want) {
			t.Fatalf("services left = %v, want %v", got, want)
		}
		if got, want := remainingFakeEntries(t, os.Getenv("COOP_TEST_VOLUMES")), []string{"old-v1_pgdata"}; !slices.Equal(got, want) {
			t.Fatalf("volumes left = %v, want %v", got, want)
		}
	})
}

// Every box sets coop.host, empty when coop cannot name its supervisor: an agent can write
// .agent/Dockerfile, and Docker keeps a label the image carries unless the run sets it. The native
// broker runs coop's own image, so it takes only the owner labels that have a value; an exact
// Docker reference refuses an empty one.
func TestOwnerLabelsAlwaysSetTheSupervisorLabel(t *testing.T) {
	owner := ownerLabels(RunSpec{Image: "i"})
	if !containsSeq(owner, []string{"--label", LabelHost + "="}) {
		t.Fatalf("ownerLabels with no workspace = %q, want an empty %s label", owner, LabelHost)
	}
	labels := nativeBrokerLabels("0123456789abcdef0123456789abcdef", owner)
	if _, ok := labels[LabelHost]; ok || labels[LabelKey] != LabelBroker {
		t.Fatalf("native broker labels = %v, want the broker's own and no empty %s", labels, LabelHost)
	}
	if labels := nativeBrokerLabels("0123456789abcdef0123456789abcdef", ownerLabels(RunSpec{Image: "i", Repo: t.TempDir()})); labels[LabelHost] == "" {
		t.Fatalf("native broker labels = %v, want its box's supervisor", labels)
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
	want := `ps -a --no-trunc --filter label=coop --format [{{json .ID}},{{json (.Label "coop")}},{{json (.Label "coop.host")}},{{json (.Label "com.docker.compose.project")}},{{json (.Label "coop.run")}}]`
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
	if !ok || ref.pid != os.Getpid() || ref.scope != workspaceScope("/r") || ref.host != localHost() ||
		ref.token != processidentity.StartToken(os.Getpid()) || ref.origin(localHost()) != originHere {
		t.Fatalf("%s=%q parsed to %#v, want this process on this host in /r's scope", LabelHost, value, ref)
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
