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
)

// fakeRuntimeScript is a container runtime backed by one file per container: $COOP_TEST_BOXES/<id>
// holds that container's labels JSON, exactly as `inspect --format {{json .Config.Labels}}` prints
// it. Every call is appended to $COOP_TEST_EVENTS, so a test can prove what the sweep touched — and
// what it never asked about. No container daemon is involved at any point.
const fakeRuntimeScript = `#!/bin/sh
printf '%s\n' "$*" >> "$COOP_TEST_EVENTS"
if [ -n "$COOP_TEST_FAILURE" ] && [ "$COOP_TEST_FAILURE" = "$1" ]; then
	echo "fake runtime: $1 failed" >&2
	exit 41
fi
case "$1" in
ps)
	shift
	for f in "$COOP_TEST_BOXES"/*; do
		[ -e "$f" ] || continue
		match=1
		for filter in "$@"; do
			case "$filter" in
			label=*)
				pair=${filter#label=}
				key=${pair%%=*}
				value=${pair#*=}
				grep -qF "\"$key\":\"$value\"" "$f" || match=0
				;;
			esac
		done
		[ "$match" = 1 ] && printf '%s\n' "$(basename "$f")"
	done
	;;
inspect)
	cat "$COOP_TEST_BOXES/$4"
	;;
rm)
	shift 2
	for id in "$@"; do rm -f "$COOP_TEST_BOXES/$id"; done
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
	return runtime.Runtime{Name: cli}, boxes
}

func addFakeBox(t *testing.T, boxes, id string, labels map[string]string) {
	t.Helper()
	labels[LabelKey] = LabelBox
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
	return supervisorLabelVersion + ":" + workspaceScope(workspace) + ":" + strconv.Itoa(pid) + ":linux-proc-v1:0"
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

	survey, err := SurveyOrphanBoxes(context.Background(), rt, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if survey.Checked != 9 {
		t.Errorf("checked %d boxes, want 9", survey.Checked)
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
	want := []string{"malformed-pid", "malformed-token", "malformed-version", "unlabeled"}
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

	n, err := ReapOrphanBoxes(context.Background(), rt, workspace)
	if err != nil || n != 2 {
		t.Fatalf("ReapOrphanBoxes = (%d, %v), want (2, nil)", n, err)
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
	if want := []string{"foreign", "legacy", "live"}; !slices.Equal(remaining, want) {
		t.Fatalf("remaining boxes = %v, want %v", remaining, want)
	}
	events := fakeRuntimeEvents(t)
	// One removal pass, filtered by the dead supervisor's exact label — never by id list, age, or name.
	if !strings.Contains(events, "ps -q -a --filter label=coop=box --filter label="+LabelHost+"="+dead+"\n") {
		t.Errorf("reap did not use the exact-label filter:\n%s", events)
	}
	for _, id := range []string{"live", "foreign", "legacy"} {
		if strings.Contains(events, "rm -f "+id) {
			t.Errorf("reap removed %s, which it must never touch:\n%s", id, events)
		}
	}
}

// A runtime that cannot answer is not evidence that nothing is running: the sweep reports the
// failure and removes nothing.
func TestReapOrphanBoxesFailsClosed(t *testing.T) {
	for _, failure := range []string{"ps", "inspect"} {
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
