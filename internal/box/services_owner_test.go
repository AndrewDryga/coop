package box

import (
	"context"
	"encoding/json"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func addFakeService(t *testing.T, boxes, id, project, host string) {
	t.Helper()
	labels := map[string]string{LabelKey: LabelService, composeProjectLabel: project}
	if host != "" {
		labels[LabelHost] = host
	}
	data, err := json.Marshal(labels)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(boxes, id), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func addFakeVolume(t *testing.T, name, project string) {
	t.Helper()
	data, err := json.Marshal(map[string]string{composeProjectLabel: project})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("COOP_TEST_VOLUMES"), name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func remainingFakeEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
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

// Only a per-run project of THIS workspace whose recorded supervisor is provably dead goes, its
// containers first and then its networks and volumes. A live run, another workspace's run, the
// development project `coop up` runs and a service with no supervisor label are all left alone.
func TestReapOrphanServicesRemovesOnlyDeadRunsOfThisWorkspace(t *testing.T) {
	rt, boxes := fakeRuntime(t)
	workspace, other := t.TempDir(), t.TempDir()
	deadRun := ComposeProjectFor(workspace, "dead-loop")
	liveRun := ComposeProjectFor(workspace, "live-loop")
	otherRun := ComposeProjectFor(other, "dead-loop")
	devProject := ComposeProjectFor(workspace, "")
	dead := goneSupervisorLabel(t, workspace)
	addFakeService(t, boxes, "dead-db", deadRun, dead)
	addFakeService(t, boxes, "dead-keycloak", deadRun, dead)
	addFakeService(t, boxes, "live-db", liveRun, liveSupervisorLabel(t, workspace))
	addFakeService(t, boxes, "other-db", otherRun, goneSupervisorLabel(t, other))
	addFakeService(t, boxes, "dev-db", devProject, dead)
	addFakeService(t, boxes, "unlabeled-db", ComposeProjectFor(workspace, "old-loop"), "")
	addFakeVolume(t, deadRun+"_pgdata", deadRun)
	addFakeVolume(t, liveRun+"_pgdata", liveRun)
	addFakeVolume(t, devProject+"_pgdata", devProject)
	addFakeNetwork(t, "dead-net", deadRun+"_default", deadRun)
	addFakeNetwork(t, "live-net", liveRun+"_default", liveRun)
	addFakeNetwork(t, "other-net", otherRun+"_default", otherRun)
	addFakeNetwork(t, "dev-net", devProject+"_default", devProject)

	removed, err := ReapOrphanServices(context.Background(), rt, workspace)
	if err != nil || removed != 2 {
		t.Fatalf("ReapOrphanServices = %d, %v; want the dead run's two containers", removed, err)
	}
	if got, want := remainingFakeNetworks(t), []string{"dev-net", "live-net", "other-net"}; !slices.Equal(got, want) {
		t.Fatalf("networks left = %v, want %v", got, want)
	}
	if got, want := remainingFakeEntries(t, boxes), []string{"dev-db", "live-db", "other-db", "unlabeled-db"}; !slices.Equal(got, want) {
		t.Fatalf("containers left = %v, want %v", got, want)
	}
	want := []string{devProject + "_pgdata", liveRun + "_pgdata"}
	slices.Sort(want)
	if got := remainingFakeEntries(t, os.Getenv("COOP_TEST_VOLUMES")); !slices.Equal(got, want) {
		t.Fatalf("volumes left = %v, want %v", got, want)
	}
}

// A runtime that cannot be asked what is running reports an error and removes nothing.
func TestReapOrphanServicesFailsClosed(t *testing.T) {
	rt, boxes := fakeRuntime(t)
	workspace := t.TempDir()
	addFakeService(t, boxes, "dead-db", ComposeProjectFor(workspace, "dead-loop"), goneSupervisorLabel(t, workspace))
	t.Setenv("COOP_TEST_FAILURE", "ps")
	if removed, err := ReapOrphanServices(context.Background(), rt, workspace); err == nil || removed != 0 {
		t.Fatalf("ReapOrphanServices with a failing runtime = %d, %v; want an error and nothing removed", removed, err)
	}
	if got := remainingFakeEntries(t, boxes); !slices.Equal(got, []string{"dead-db"}) {
		t.Fatalf("containers left = %v", got)
	}
}

// The development project gets no override, so its file may plant any label; a checkout named like a
// run suffix makes its project name look like a per-run one, yet the sweep never takes it.
func TestReapOrphanServicesNeverTakesADevelopmentProjectNamedLikeARun(t *testing.T) {
	rt, boxes := fakeRuntime(t)
	workspace := filepath.Join(t.TempDir(), "api-1234abcd-run")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	dev := ComposeProject(workspace)
	addFakeService(t, boxes, "dev-db", dev, goneSupervisorLabel(t, workspace))
	addFakeVolume(t, dev+"_pgdata", dev)
	if removed, err := ReapOrphanServices(context.Background(), rt, workspace); err != nil || removed != 0 {
		t.Fatalf("ReapOrphanServices = %d, %v; want the development project left alone", removed, err)
	}
	if got := remainingFakeEntries(t, boxes); !slices.Equal(got, []string{"dev-db"}) {
		t.Fatalf("containers left = %v", got)
	}
	if got := remainingFakeEntries(t, os.Getenv("COOP_TEST_VOLUMES")); !slices.Equal(got, []string{dev + "_pgdata"}) {
		t.Fatalf("volumes left = %v", got)
	}
}

// A killed run's in-use volume does not stop the sweep: the other dead run still goes, and the
// refusal is reported.
func TestReapOrphanServicesKeepsGoingPastARefusedVolume(t *testing.T) {
	rt, boxes := fakeRuntime(t)
	workspace := t.TempDir()
	first, second := ComposeProjectFor(workspace, "first-loop"), ComposeProjectFor(workspace, "second-loop")
	dead := goneSupervisorLabel(t, workspace)
	addFakeService(t, boxes, "a-db", first, dead)
	addFakeService(t, boxes, "b-db", second, dead)
	held, err := json.Marshal(map[string]string{composeProjectLabel: first, "in-use": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("COOP_TEST_VOLUMES"), first+"_pgdata"), held, 0o644); err != nil {
		t.Fatal(err)
	}
	addFakeVolume(t, second+"_pgdata", second)

	removed, err := ReapOrphanServices(context.Background(), rt, workspace)
	if err == nil || removed != 2 {
		t.Fatalf("ReapOrphanServices = %d, %v; want both containers removed and the refusal reported", removed, err)
	}
	if got := remainingFakeEntries(t, boxes); len(got) != 0 {
		t.Fatalf("containers left = %v", got)
	}
	if got, want := remainingFakeEntries(t, os.Getenv("COOP_TEST_VOLUMES")), []string{first + "_pgdata"}; !slices.Equal(got, want) {
		t.Fatalf("volumes left = %v, want %v", got, want)
	}
}

// A killed session's box can still be on its way out when the next session sweeps. The network it
// holds stays, for a later pass, and the volumes go anyway: nothing finds the project again.
func TestReapOrphanServicesLeavesANetworkABoxStillHolds(t *testing.T) {
	rt, boxes := fakeRuntime(t)
	workspace := t.TempDir()
	run := ComposeProjectFor(workspace, "dead-session")
	addFakeService(t, boxes, "dead-db", run, goneSupervisorLabel(t, workspace))
	addFakeContainer(t, boxes, "dying-box", LabelBox, map[string]string{"network": "held-net"})
	addFakeNetwork(t, "held-net", run+"_default", run)
	addFakeVolume(t, run+"_pgdata", run)

	if removed, err := ReapOrphanServices(context.Background(), rt, workspace); err != nil || removed != 1 {
		t.Fatalf("ReapOrphanServices = %d, %v; want the service removed and nothing reported", removed, err)
	}
	if got := remainingFakeEntries(t, boxes); !slices.Equal(got, []string{"dying-box"}) {
		t.Fatalf("containers left = %v", got)
	}
	if got := remainingFakeNetworks(t); !slices.Equal(got, []string{"held-net"}) {
		t.Fatalf("networks left = %v", got)
	}
	if got := remainingFakeEntries(t, os.Getenv("COOP_TEST_VOLUMES")); len(got) != 0 {
		t.Fatalf("volumes left = %v", got)
	}
}

// A runtime that cannot list a killed run's networks still loses the run's volumes: once its
// containers are gone no sweep finds the project again, so a volume left now stays for good.
func TestReapOrphanServicesRemovesVolumesPastANetworkFailure(t *testing.T) {
	rt, boxes := fakeRuntime(t)
	workspace := t.TempDir()
	run := ComposeProjectFor(workspace, "dead-session")
	addFakeService(t, boxes, "dead-db", run, goneSupervisorLabel(t, workspace))
	addFakeVolume(t, run+"_pgdata", run)
	t.Setenv("COOP_TEST_FAILURE", "network")

	if removed, err := ReapOrphanServices(context.Background(), rt, workspace); err == nil || removed != 1 {
		t.Fatalf("ReapOrphanServices = %d, %v; want the service removed and the network failure reported", removed, err)
	}
	if got := remainingFakeEntries(t, os.Getenv("COOP_TEST_VOLUMES")); len(got) != 0 {
		t.Fatalf("volumes left = %v", got)
	}
}

// A loop's or session's project names the process whose exit ends it: the loop's own, or an editor
// session's supervisor, the parent of the inner child that starts the services, scoped to the
// workspace that names the project. The development project carries no labels.
func TestRunServiceLabelsNameTheRunsSupervisor(t *testing.T) {
	workspace, policy := t.TempDir(), t.TempDir()
	liveSupervisorLabel(t, workspace)
	if got := runServiceLabels(RunSpec{Repo: workspace}, ""); got != nil {
		t.Fatalf("development project labels = %v, want none", got)
	}
	for _, tc := range []struct {
		name string
		spec RunSpec
		want string
	}{
		{"loop", RunSpec{Repo: workspace}, supervisorLabelValue(workspaceScope(workspace), os.Getpid())},
		{"editor session", RunSpec{Repo: workspace, SupervisorID: "0123456789abcdef"}, supervisorLabelValue(workspaceScope(workspace), os.Getppid())},
		{"policy repository", RunSpec{Repo: workspace, PolicyRepo: policy}, supervisorLabelValue(workspaceScope(workspace), os.Getpid())},
	} {
		got := runServiceLabels(tc.spec, "run-one")
		if got[LabelKey] != LabelService || got[LabelHost] != tc.want || len(got) != 2 {
			t.Errorf("%s: labels = %v, want %s=%s and %s=%q", tc.name, got, LabelKey, LabelService, LabelHost, tc.want)
		}
	}
}

// A child can wait long at the service launch lock. A supervisor killed meanwhile is still the one its
// services name, so the next launch can prove them over: read after the wait, the label would be empty
// and no sweep would ever remove them.
func TestSessionServicesNameTheSupervisorThatDiedDuringTheLaunchWait(t *testing.T) {
	repo, _ := writeCompose(t, "services:\n  db:\n    image: postgres:18\n    expose: [5432]\n")
	want := liveSupervisorLabel(t, repo)
	dir := t.TempDir()
	labels, shim := filepath.Join(dir, "labels"), filepath.Join(dir, "runtime")
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do case \"$arg\" in *coop-compose-override-labels.yml) cp \"$arg\" " + strconv.Quote(labels) + " ;; esac; done\n" +
		"case \"$*\" in\n" +
		"  *'config --format json'*) printf '%s\\n' '{\"services\":{\"db\":{\"expose\":[\"5432\"]}}}' ;;\n" +
		"  *'config --services'*) printf '%s\\n' db ;;\n" +
		"esac\n"
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var supervisorGone atomic.Bool
	read := make(chan struct{})
	var readOnce sync.Once
	parentPID = func() int {
		pid := os.Getpid()
		if supervisorGone.Load() {
			pid = 1 // what a killed parent leaves
		}
		// Signal only once the answer is fixed: the test kills the supervisor the moment it hears the
		// read, and signalling first let that land before this first read, so it saw a dead parent.
		readOnce.Do(func() { close(read) })
		return pid
	}
	t.Cleanup(func() { parentPID = os.Getppid })
	unlock, err := forkspace.LockServiceLaunch(context.Background(), repo, true)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "open", AutoUp: true}
		_, err := Run(cfg, runtime.Runtime{Name: shim}, RunSpec{
			Image: "i", Repo: repo, Cmd: []string{"true"}, Network: true, Batch: true, Quiet: true,
			SupervisorID: "0123456789abcdef", RunID: "run-one",
		})
		done <- err
	}()
	// No service starts under this lock, so the supervisor dies during the wait. A run that reads its
	// parent only after the wait gets here on the timeout, and then fails on the label below.
	select {
	case <-read:
	case <-time.After(5 * time.Second):
	}
	supervisorGone.Store(true)
	unlock()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(labels)
	if err != nil {
		t.Fatalf("the services started without the label override: %v", err)
	}
	if !strings.Contains(string(data), strconv.Quote(want)) {
		t.Fatalf("service labels do not name the supervisor %s:\n%s", want, data)
	}
}

// coop's own override gives every service the labels, quoted, so they replace whatever the project's
// file says; the filtered path's override carries the same block.
func TestServiceLabelOverrideLabelsEveryService(t *testing.T) {
	workspace := t.TempDir()
	host := liveSupervisorLabel(t, workspace)
	labels := map[string]string{LabelKey: LabelService, LabelHost: host}
	path, cleanup, err := writeServiceLabelOverride([]string{"db", "keycloak"}, labels, workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block := "    labels:\n      \"coop\": \"service\"\n      \"coop.host\": \"" + host + "\"\n"
	for _, service := range []string{"db", "keycloak"} {
		if want := `  "` + service + `":` + "\n" + block; !strings.Contains(string(data), want) {
			t.Errorf("override lacks %q:\n%s", want, data)
		}
	}
	filtered, err := filteredServiceOverride("coop-example_filtered", []string{"db"}, netip.Prefix{}, nil, labels)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(filtered), block) {
		t.Errorf("filtered override lacks the labels:\n%s", filtered)
	}
}

// A loop's or session's own project starts with the label override; the development project
// `coop up` runs never does.
func TestRunServicesStartWithTheLabelOverride(t *testing.T) {
	workspace := t.TempDir()
	labels := map[string]string{LabelKey: LabelService, LabelHost: liveSupervisorLabel(t, workspace)}
	for _, tc := range []struct {
		owner  string
		labels map[string]string
	}{{"loop-one", labels}, {"", nil}} {
		repo := testComposeRepoWithServices(t, "db", "keycloak")
		rec := filepath.Join(t.TempDir(), "rec")
		rt := composeRuntimeWithServices(t, rec, []string{"db", "keycloak"}, 0)
		if _, err := startServicesFileContext(context.Background(), rt, repo, filepath.Join(repo, ".agent", "compose.yml"),
			tc.owner, "", tc.labels, io.Discard, io.Discard, false, false, nil, nil); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(rec)
		if err != nil {
			t.Fatal(err)
		}
		var up string
		for _, call := range strings.Split(string(data), "\n") {
			if strings.Contains(call, " up -d --wait") {
				up = call
			}
		}
		if got, want := strings.Contains(up, "coop-compose-override-labels.yml"), tc.labels != nil; got != want {
			t.Fatalf("owner %q: up carries the label override = %v, want %v:\n%s", tc.owner, got, want, up)
		}
	}
}
