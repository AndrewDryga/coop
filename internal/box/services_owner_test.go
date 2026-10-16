package box

import (
	"context"
	"encoding/json"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
// containers first and then its volumes. A live run, another workspace's run, the development
// project `coop up` runs and a service with no supervisor label are all left alone.
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

	removed, err := ReapOrphanServices(context.Background(), rt, workspace)
	if err != nil || removed != 2 {
		t.Fatalf("ReapOrphanServices = %d, %v; want the dead run's two containers", removed, err)
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
