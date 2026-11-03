package box

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func addFakeLabeledVolume(t *testing.T, name string, labels map[string]string) {
	t.Helper()
	data, err := json.Marshal(labels)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("COOP_TEST_VOLUMES"), name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A killed run cannot close its task channel, so its workspace's sweep removes the volume once the
// run's coop is provably gone. A live run's, another workspace's, an unlabeled one and one a
// container still mounts stay where they are.
func TestReapOrphanTaskChannelsRemovesOnlyDeadRunsVolumes(t *testing.T) {
	rt, _ := fakeRuntime(t)
	workspace, foreign := t.TempDir(), t.TempDir()
	dead := deadSupervisorLabel(workspace)
	for name, labels := range map[string]map[string]string{
		"coop-tasks-dead":      {LabelKey: LabelTaskChannel, LabelHost: dead},
		"coop-tasks-gone":      {LabelKey: LabelTaskChannel, LabelHost: goneSupervisorLabel(t, workspace)},
		"coop-tasks-live":      {LabelKey: LabelTaskChannel, LabelHost: liveSupervisorLabel(t, workspace)},
		"coop-tasks-foreign":   {LabelKey: LabelTaskChannel, LabelHost: deadSupervisorLabel(foreign)},
		"coop-tasks-unlabeled": {LabelKey: LabelTaskChannel},
		"coop-tasks-mounted":   {LabelKey: LabelTaskChannel, LabelHost: dead, "in-use": "1"},
		"someone-elses-data":   {LabelHost: dead},
	} {
		addFakeLabeledVolume(t, name, labels)
	}
	n, err := ReapOrphanTaskChannels(context.Background(), rt, workspace)
	if err != nil || n != 2 {
		t.Fatalf("ReapOrphanTaskChannels = (%d, %v), want (2, nil)", n, err)
	}
	want := []string{"coop-tasks-foreign", "coop-tasks-live", "coop-tasks-mounted", "coop-tasks-unlabeled", "someone-elses-data"}
	if got := remainingFakeEntries(t, os.Getenv("COOP_TEST_VOLUMES")); !slices.Equal(got, want) {
		t.Fatalf("remaining volumes = %v, want %v", got, want)
	}
	if events := fakeRuntimeEvents(t); strings.Contains(events, "volume rm coop-tasks-live") || strings.Contains(events, "volume rm someone-elses-data") {
		t.Fatalf("the sweep asked to remove a volume it must never touch:\n%s", events)
	}
}

// A runtime that cannot list volumes fails the sweep instead of reading as "nothing left behind".
func TestReapOrphanTaskChannelsFailsClosed(t *testing.T) {
	rt, _ := fakeRuntime(t)
	t.Setenv("COOP_TEST_FAILURE", "volume")
	addFakeLabeledVolume(t, "coop-tasks-dead", map[string]string{LabelKey: LabelTaskChannel, LabelHost: deadSupervisorLabel(t.TempDir())})
	if n, err := ReapOrphanTaskChannels(context.Background(), rt, t.TempDir()); err == nil || n != 0 {
		t.Fatalf("ReapOrphanTaskChannels on a failing runtime = (%d, %v), want an error", n, err)
	}
}

// The channel volume carries the owner label the sweep judges, and none when coop could not name
// its owner — such a volume is never swept, like an unattributed box.
func TestTaskChannelVolumeCarriesItsOwner(t *testing.T) {
	rt, _ := fakeRuntime(t)
	owner := liveSupervisorLabel(t, t.TempDir())
	for _, label := range []string{owner, ""} {
		volume := NewTaskChannelVolume()
		_, _ = startTaskChannel(context.Background(), rt, "img", volume, "", label, echoServer{}, filepath.Join(t.TempDir(), "mux.js"))
		create := ""
		for _, line := range strings.Split(fakeRuntimeEvents(t), "\n") {
			if strings.HasPrefix(line, "volume create") && strings.HasSuffix(line, volume) {
				create = line
			}
		}
		hasOwner := strings.Contains(create, "--label "+LabelHost+"=")
		if create == "" || hasOwner != (label != "") || (label != "" && !strings.Contains(create, LabelHost+"="+label)) {
			t.Fatalf("owner %q: volume create = %q", label, create)
		}
	}
}

// A volume name handed in from outside the run selects only coop's own channel volume.
func TestValidTaskChannelVolumeAcceptsOnlyCoopsOwnNames(t *testing.T) {
	if name := NewTaskChannelVolume(); !ValidTaskChannelVolume(name) {
		t.Fatalf("a fresh name %q is refused", name)
	}
	for _, name := range []string{"", "coop-tasks-", "coop-tasks-0123456789ABCDEF", "coop-tasks-0123456789abcde",
		"coop-tasks-0123456789abcdef0", "coop-cache-0123456789abcdef", "coop-tasks-0123456789abcdeg", "coop-tasks-../../etc/passwd"} {
		if ValidTaskChannelVolume(name) {
			t.Errorf("ValidTaskChannelVolume(%q) = true", name)
		}
	}
}
