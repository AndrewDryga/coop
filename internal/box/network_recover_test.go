package box

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/testutil/starttoken"
)

// departSupervisor rewrites one retained run's supervisor identity so the
// kernel reports the launching process as departed — what a host crash, a
// SIGKILL or a reboot leaves behind.
func departSupervisor(t *testing.T, store *networkstate.Store, id string) {
	t.Helper()
	// Same pid, a different start time in the same token format: the kernel
	// proves the process that launched this run is not the one holding that pid
	// now, which is exactly what a reused pid after a crash looks like.
	rewriteSupervisorToken(t, store, id, func(token string) string { return token + "9" })
}

// rewriteSupervisorToken replaces one retained run's recorded supervisor start
// token with what change makes of it.
func rewriteSupervisorToken(t *testing.T, store *networkstate.Store, id string, change func(token string) string) {
	t.Helper()
	path := filepath.Join(store.Path(), "execution-"+id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	supervisor, _ := record["supervisor"].(map[string]any)
	token, _ := supervisor["start_token"].(string)
	if token == "" {
		t.Fatal("the run recorded no supervisor identity")
	}
	supervisor["start_token"] = change(token)
	updated, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatal(err)
	}
}

// rewriteSupervisorHost replaces one retained run's recorded supervisor host, or removes it when
// host is "", as a record written before the field existed has none.
func rewriteSupervisorHost(t *testing.T, store *networkstate.Store, id, host string) {
	t.Helper()
	path := filepath.Join(store.Path(), "execution-"+id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	supervisor, _ := record["supervisor"].(map[string]any)
	if host == "" {
		delete(supervisor, "host")
	} else {
		supervisor["host"] = host
	}
	updated, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatal(err)
	}
}

func recoveryFixture(t *testing.T) (*filteredExecution, *filteredDaemonFixture, *networkstate.Evidence, recoverConnect) {
	t.Helper()
	f, d := filteredFixture(t)
	if _, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	evidence, err := networkstate.OpenEvidence(f.store.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = evidence.Close() })
	connect := func(context.Context, string) (recoverDocker, string, error) { return d, "fixture-daemon", nil }
	return f, d, evidence, connect
}

// A run whose supervisor died leaves its gateway containers, its two volumes and
// an unsealed receipt. Recovery removes exactly what the run recorded and seals
// an honest partial receipt for the epoch nobody observed the end of.
func TestRecoverNetworkRunsSettlesADepartedSupervisor(t *testing.T) {
	f, d, evidence, connect := recoveryFixture(t)
	departSupervisor(t, f.store, f.record.ID)
	results, err := recoverNetworkRuns(context.Background(), evidence, "", connect)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].RunID != f.record.ID || !results[0].Sealed || len(results[0].Failures) != 0 || len(results[0].Pending) != 0 {
		t.Fatalf("recovery result = %+v", results)
	}
	if !slices.Equal(results[0].Removed, networkRecoveryOrder) {
		t.Fatalf("recovered roles = %v, want every recorded resource", results[0].Removed)
	}
	d.mu.Lock()
	containers, volumes := len(d.containers), len(d.volumes)
	d.mu.Unlock()
	if containers != 0 || volumes != 0 {
		t.Fatalf("recovery left %d container(s) and %d volume(s)", containers, volumes)
	}
	record, err := evidence.Execution(f.record.ID)
	if err != nil || record.Receipt == nil {
		t.Fatal("the interrupted run was not sealed", err)
	}
	if record.Receipt.Workload != "supervisor_lost" || record.Receipt.Finality != "final" || record.Receipt.Completeness != "partial" {
		t.Fatalf("receipt = %+v, want a final partial supervisor_lost receipt", record.Receipt)
	}
	if record.Receipt.Cleanup != "complete" {
		t.Fatalf("cleanup = %q, want complete once every resource is gone", record.Receipt.Cleanup)
	}
	if _, err := os.Stat(f.runfiles); !os.IsNotExist(err) {
		t.Fatal("recovery kept the run's artifact directory", err)
	}
	// Recovery is idempotent: a settled run is not pending, so a second pass has
	// nothing to do and does not reopen a sealed receipt.
	again, err := recoverNetworkRuns(context.Background(), evidence, "", connect)
	if err != nil || len(again) != 0 {
		t.Fatalf("second pass = %+v %v", again, err)
	}
}

// cleanUpWithoutSeal runs a launched run's own teardown and has only its last step, the seal, fail:
// what a supervisor killed between its cleanup and its seal leaves behind. Every resource and the
// artifact are confirmed gone, and there is no receipt.
func cleanUpWithoutSeal(t *testing.T, f *filteredExecution, d *filteredDaemonFixture) {
	t.Helper()
	// "killed" is no workload outcome SealExecution accepts, so teardown stops exactly at the seal.
	if gone, err := f.cleanup("killed"); !gone || err == nil || !strings.Contains(err.Error(), "unknown network workload outcome") {
		t.Fatalf("teardown = %v, %v; want everything removed and only the seal refused", gone, err)
	}
	record, err := f.store.Execution(f.record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Receipt != nil || record.Artifact.State != "gone" || slices.ContainsFunc(record.Resources, func(r networkstate.Resource) bool { return r.State != "gone" }) {
		t.Fatalf("after teardown: receipt %+v, artifact %s, resources %+v; want everything gone and no receipt",
			record.Receipt, record.Artifact.State, record.Resources)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.containers) != 0 || len(d.volumes) != 0 {
		t.Fatalf("teardown left %d container(s) and %d volume(s)", len(d.containers), len(d.volumes))
	}
}

// noDaemon is a runtime that does not answer, as when Docker is stopped.
func noDaemon(context.Context, string) (recoverDocker, string, error) {
	return nil, "", errors.New("Docker is not running")
}

// A supervisor killed after its cleanup but before its seal leaves a run with nothing to remove and
// no receipt, which retention would keep forever. Recovery seals it as supervisor_lost with cleanup
// complete from the record alone, so a daemon that is stopped or replaced cannot keep it open, and
// retention removes it once its window has passed.
func TestRecoverNetworkRunsSealsARunWhoseCleanupFinishedBeforeItsSeal(t *testing.T) {
	f, d, evidence, _ := recoveryFixture(t)
	cleanUpWithoutSeal(t, f, d)
	departSupervisor(t, f.store, f.record.ID)
	now := time.Now()
	results, err := recoverNetworkRunsAt(context.Background(), evidence, "", noDaemon, now)
	if err != nil || len(results) != 1 || results[0].RunID != f.record.ID {
		t.Fatalf("recovery = %+v %v, want the unsealed run", results, err)
	}
	if r := results[0]; !r.Sealed || r.Skipped != "" || len(r.Removed) != 0 || len(r.Pending) != 0 || len(r.Failures) != 0 {
		t.Fatalf("recovery result = %+v, want the run sealed with nothing removed", r)
	}
	record, err := evidence.Execution(f.record.ID)
	if err != nil || record.Receipt == nil {
		t.Fatal("the run was not sealed", err)
	}
	if receipt := record.Receipt; receipt.Workload != "supervisor_lost" || receipt.Finality != "final" ||
		receipt.Completeness != "partial" || receipt.Cleanup != "complete" {
		t.Fatalf("receipt = %+v, want a final partial supervisor_lost receipt with cleanup complete", receipt)
	}

	// Sealed, the run is settled: the next pass has nothing to do, and the first one past the
	// retention window removes the record.
	if again, err := recoverNetworkRunsAt(context.Background(), evidence, "", noDaemon, now); err != nil || len(again) != 0 {
		t.Fatalf("second pass = %+v %v, want nothing left to settle", again, err)
	}
	if _, err := recoverNetworkRunsAt(context.Background(), evidence, "", noDaemon, now.Add(networkstate.ExecutionRetention+time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := evidence.Execution(f.record.ID); !errors.Is(err, networkstate.ErrEvidenceUnavailable) {
		t.Fatalf("the sealed run outlived retention: %v", err)
	}
}

// The same run with its supervisor still running is that process's to seal. Recovery, scoped or
// not, leaves it unsealed, and retention, which removes only a sealed run, keeps it.
func TestRecoverNetworkRunsLeavesAnUnsealedRunToItsLiveSupervisor(t *testing.T) {
	f, d, evidence, connect := recoveryFixture(t)
	cleanUpWithoutSeal(t, f, d)
	later := time.Now().Add(networkstate.ExecutionRetention + time.Hour)
	for _, scope := range []string{"", f.record.ID} {
		results, err := recoverNetworkRunsAt(context.Background(), evidence, scope, connect, later)
		if err != nil || len(results) != 1 || !results[0].Live || results[0].Sealed {
			t.Fatalf("recovery scoped to %q = %+v %v, want the run left to its live supervisor", scope, results, err)
		}
		record, err := evidence.Execution(f.record.ID)
		if err != nil || record.Receipt != nil {
			t.Fatalf("a live supervisor's run was sealed or removed: %+v %v", record.Receipt, err)
		}
	}
}

// A supervisor killed after it removed its containers and volumes but before it removed the run's
// files leaves those files and no receipt. Neither is at the runtime, so recovery finishes both with
// the daemon down: it removes the files and seals the run with cleanup complete.
func TestRecoverNetworkRunsRemovesTheArtifactAndSealsWithTheDaemonDown(t *testing.T) {
	f, d, evidence, _ := recoveryFixture(t)
	// The teardown's own removals, in its order, and nothing after them.
	for _, role := range networkRecoveryOrder {
		if err := f.removeResource(evidence, role); err != nil {
			t.Fatalf("remove %s: %v", role, err)
		}
	}
	d.mu.Lock()
	containers, volumes := len(d.containers), len(d.volumes)
	d.mu.Unlock()
	before, err := evidence.Execution(f.record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if containers != 0 || volumes != 0 || before.Receipt != nil || before.Artifact.State == "gone" ||
		slices.ContainsFunc(before.Resources, func(r networkstate.Resource) bool { return r.State != "gone" }) {
		t.Fatalf("before recovery: %d container(s), %d volume(s), receipt %+v, artifact %s, resources %+v; want only the files and the receipt owed",
			containers, volumes, before.Receipt, before.Artifact.State, before.Resources)
	}
	if _, err := os.Stat(f.runfiles); err != nil {
		t.Fatal("the run's files are already gone:", err)
	}
	departSupervisor(t, f.store, f.record.ID)

	results, err := recoverNetworkRuns(context.Background(), evidence, "", noDaemon)
	if err != nil || len(results) != 1 || results[0].RunID != f.record.ID {
		t.Fatalf("recovery = %+v %v, want the interrupted run", results, err)
	}
	if r := results[0]; !r.Sealed || r.Skipped != "" || len(r.Removed) != 0 || len(r.Pending) != 0 || len(r.Failures) != 0 {
		t.Fatalf("recovery result = %+v, want the files removed and the run sealed without the daemon", r)
	}
	if _, err := os.Stat(f.runfiles); !os.IsNotExist(err) {
		t.Fatal("recovery kept the run's files:", err)
	}
	record, err := evidence.Execution(f.record.ID)
	if err != nil || record.Receipt == nil || record.Artifact.State != "gone" {
		t.Fatalf("after recovery: receipt %+v, artifact %s, %v; want the run sealed and its files gone", record.Receipt, record.Artifact.State, err)
	}
	if receipt := record.Receipt; receipt.Workload != "supervisor_lost" || receipt.Finality != "final" || receipt.Cleanup != "complete" {
		t.Fatalf("receipt = %+v, want a final supervisor_lost receipt with cleanup complete", receipt)
	}
}

// A live supervisor still owns its run. Recovery refuses it by name and pid, and
// removes nothing: reclaiming a running box's gateway would be the worst
// possible outcome of a cleanup command.
func TestRecoverNetworkRunsRefusesALiveOwner(t *testing.T) {
	f, d, evidence, connect := recoveryFixture(t)
	results, err := recoverNetworkRuns(context.Background(), evidence, "", connect)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Skipped == "" || len(results[0].Removed) != 0 {
		t.Fatalf("a live run was recovered: %+v", results)
	}
	if !strings.Contains(results[0].Skipped, strconv.Itoa(os.Getpid())) {
		t.Fatalf("the refusal does not name the owning pid: %q", results[0].Skipped)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.containers) == 0 || len(d.volumes) == 0 {
		t.Fatal("a live run's resources were removed")
	}
	record, err := evidence.Execution(f.record.ID)
	if err != nil || record.Receipt != nil {
		t.Fatal("a live run's receipt was sealed", err)
	}
}

// A run another operating system supervised names a pid this kernel cannot
// check, so recovery leaves it alone as it would a live one: nothing removed,
// nothing sealed.
func TestRecoverNetworkRunsLeavesAnotherSystemsRunAlone(t *testing.T) {
	f, d, evidence, connect := recoveryFixture(t)
	rewriteSupervisorToken(t, f.store, f.record.ID, func(string) string { return starttoken.Foreign() })
	results, err := recoverNetworkRuns(context.Background(), evidence, "", connect)
	if err != nil || len(results) != 1 || !results[0].Live || len(results[0].Removed) != 0 || results[0].Sealed {
		t.Fatalf("recovery of another system's run = %+v %v, want it left alone", results, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.containers) == 0 || len(d.volumes) == 0 {
		t.Fatal("another system's run lost its resources")
	}
	record, err := evidence.Execution(f.record.ID)
	if err != nil || record.Receipt != nil {
		t.Fatal("another system's run was sealed", err)
	}
}

// A run records the host its supervisor ran on, and recovery judges the supervisor only where that
// pid means something: a run from this host whose supervisor is gone is settled; one recorded on
// another host is left alone however its pid looks from here, and said to be another host's; one
// recorded before runs named their host is judged as before by a coop on the host itself.
func TestRecoverNetworkRunsJudgesOnlyThisHostsSupervisors(t *testing.T) {
	host, err := thisHost()
	if err != nil {
		t.Skip("this host cannot be named:", err)
	}
	for _, tc := range []struct {
		name     string
		recorded string // "" removes the field
		settled  bool
	}{
		{"this host", host, true},
		{"another host", strings.Repeat("a", 24), false},
		{"no host recorded", "", legacyLabelsAreLocal()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, d, evidence, connect := recoveryFixture(t)
			if f.record.Supervisor.Host != host {
				t.Fatalf("run recorded host %q, want this host's %q", f.record.Supervisor.Host, host)
			}
			rewriteSupervisorHost(t, f.store, f.record.ID, tc.recorded)
			departSupervisor(t, f.store, f.record.ID)
			results, err := recoverNetworkRuns(context.Background(), evidence, "", connect)
			if err != nil || len(results) != 1 {
				t.Fatalf("recovery = %+v %v", results, err)
			}
			r := results[0]
			if tc.settled != !r.Live || r.Elsewhere != !tc.settled || r.Sealed != tc.settled {
				t.Fatalf("recovery of a run from %s = %+v, want settled %v", tc.name, r, tc.settled)
			}
			if !tc.settled {
				d.mu.Lock()
				kept := len(d.containers) != 0 && len(d.volumes) != 0
				d.mu.Unlock()
				if !kept || !strings.Contains(r.Skipped, "another computer or container") {
					t.Fatalf("another host's run lost its resources or was not named as another host's: %+v", r)
				}
			}
		})
	}
}

// A container the runtime no longer has is gone — but only after an exact
// inspection said so. A removal that fails leaves the run pending, with the
// resource still recorded, rather than a receipt claiming clean custody.
func TestRecoverNetworkRunsProvesAbsenceAndKeepsFailuresPending(t *testing.T) {
	t.Run("absent after an exact inspect", func(t *testing.T) {
		f, d, evidence, connect := recoveryFixture(t)
		departSupervisor(t, f.store, f.record.ID)
		d.mu.Lock()
		for name := range d.containers {
			if strings.Contains(name, "agent") {
				delete(d.containers, name)
			}
		}
		d.mu.Unlock()
		results, err := recoverNetworkRuns(context.Background(), evidence, f.record.ID, connect)
		if err != nil || len(results) != 1 || !results[0].Sealed || slices.Contains(results[0].Pending, "agent") {
			t.Fatalf("an absent container was not settled: %+v %v", results, err)
		}
		// It was already gone, so this pass did not remove it: a report must not
		// claim a container it never touched.
		if slices.Contains(results[0].Removed, "agent") || results[0].RemovedContainers != 2 {
			t.Errorf("an absent container was counted as removed: %+v", results[0])
		}
	})
	t.Run("removal fails", func(t *testing.T) {
		f, d, evidence, connect := recoveryFixture(t)
		departSupervisor(t, f.store, f.record.ID)
		d.refuseRemoval = "controller"
		results, err := recoverNetworkRuns(context.Background(), evidence, f.record.ID, connect)
		if err != nil {
			t.Fatal(err)
		}
		if len(results[0].Failures) == 0 || !slices.Contains(results[0].Pending, "controller") {
			t.Fatalf("a failed removal was reported as settled: %+v", results)
		}
		// The volumes a surviving container could still mount stay put.
		for _, role := range []string{"ipc", "observations"} {
			if !slices.ContainsFunc(results[0].Pending, func(value string) bool { return strings.HasPrefix(value, role) }) {
				t.Fatalf("%s was removed under a container that is not proved gone: %+v", role, results)
			}
		}
		record, err := evidence.Execution(f.record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if record.Receipt == nil || record.Receipt.Cleanup != "pending" {
			t.Fatalf("receipt = %+v, want a sealed receipt recording pending cleanup", record.Receipt)
		}
	})
}

// Recovery removes on the daemon the run recorded, and only there: another
// daemon answering that endpoint owns different containers with the same names.
func TestRecoverNetworkRunsRefusesAnotherDaemon(t *testing.T) {
	f, d, evidence, _ := recoveryFixture(t)
	departSupervisor(t, f.store, f.record.ID)
	other := func(context.Context, string) (recoverDocker, string, error) { return d, "another-daemon", nil }
	results, err := recoverNetworkRuns(context.Background(), evidence, f.record.ID, other)
	if err != nil || len(results) != 1 || !strings.Contains(results[0].Skipped, "different daemon") {
		t.Fatalf("recovery ran against a foreign daemon: %+v %v", results, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.containers) == 0 {
		t.Fatal("a foreign daemon's containers were removed")
	}
}

// A run named by id is the only one that pass may settle: an inspection that
// recovers the run it is about to render must not seal a second interrupted run
// as a side effect. The later unscoped pass then settles what it left.
func TestRecoverNetworkRunsSettlesOnlyTheRunItWasGiven(t *testing.T) {
	f, d, evidence, connect := recoveryFixture(t)
	// A second setup and a second run on the same host: one smoke launches one
	// run, so the run this pass must not touch needs its own.
	smoke, err := f.store.BeginQualification(d.smoke.Candidate(),
		[]networkstate.QualifiedClient{{Provider: "claude", Client: egress.ClientCLI, Version: "2.1.260"}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := smoke.CreateExecution(context.Background(), networkstate.ExecutionSpec{
		Project: f.record.Project, PolicyFingerprint: f.policy.Fingerprint, Runtime: "docker",
		DaemonID: f.record.DaemonID, Endpoint: f.record.Endpoint,
		GatewayImage: f.record.GatewayImage, ClientImage: f.record.ClientImage})
	if err != nil {
		t.Fatal(err)
	}
	departSupervisor(t, f.store, f.record.ID)
	departSupervisor(t, f.store, other.ID)

	results, err := recoverNetworkRuns(context.Background(), evidence, f.record.ID, connect)
	if err != nil || len(results) != 1 || results[0].RunID != f.record.ID || !results[0].Sealed {
		t.Fatalf("an id-scoped recovery = %+v %v, want only the run it was given", results, err)
	}
	spared, err := evidence.Execution(other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if spared.Receipt != nil {
		t.Fatalf("the run nobody asked about was sealed: %+v", spared.Receipt)
	}

	// It was pending all along: the unscoped pass proves the id was the reason
	// it was spared, not a store that had already forgotten it.
	rest, err := recoverNetworkRuns(context.Background(), evidence, "", connect)
	if err != nil || len(rest) != 1 || rest[0].RunID != other.ID || !rest[0].Sealed {
		t.Fatalf("the unscoped pass = %+v %v, want the run the first pass left", rest, err)
	}
}

// Cleanup nobody could finish stays owed, so the next ordinary coop start
// retries it — one bounded pass per start, never a loop of its own. Once it
// settles, later passes have nothing left to do.
func TestRecoverNetworkRunsRetriesAPendingOrphanOnALaterPass(t *testing.T) {
	f, d, evidence, connect := recoveryFixture(t)
	departSupervisor(t, f.store, f.record.ID)
	d.refuseRemoval = "controller"
	first, err := recoverNetworkRuns(context.Background(), evidence, "", connect)
	if err != nil || len(first) != 1 || !slices.Contains(first[0].Pending, "controller") || !first[0].Sealed {
		t.Fatalf("first pass = %+v %v, want the controller left pending and the run sealed", first, err)
	}

	// The next start finds the same run still owed and settles what the last one
	// could not, without a retry loop of its own.
	d.refuseRemoval = ""
	second, err := recoverNetworkRuns(context.Background(), evidence, "", connect)
	if err != nil || len(second) != 1 || second[0].RunID != f.record.ID {
		t.Fatalf("second pass = %+v %v, want the same pending run retried", second, err)
	}
	// The first pass wrote the receipt, so this one must not claim it did.
	if !slices.Contains(second[0].Removed, "controller") || len(second[0].Pending) != 0 || len(second[0].Failures) != 0 || second[0].Sealed {
		t.Fatalf("second pass = %+v, want the retry to finish the cleanup and leave the receipt to the first", second[0])
	}
	d.mu.Lock()
	containers, volumes := len(d.containers), len(d.volumes)
	d.mu.Unlock()
	if containers != 0 || volumes != 0 {
		t.Fatalf("the retry left %d container(s) and %d volume(s)", containers, volumes)
	}

	// Bounded: a settled run is no longer pending, so the start after that does
	// no runtime work at all.
	third, err := recoverNetworkRuns(context.Background(), evidence, "", connect)
	if err != nil || len(third) != 0 {
		t.Fatalf("third pass = %+v %v, want nothing left to retry", third, err)
	}
}

// The launch fixture's daemon is exactly the bounded surface recovery needs.
var _ recoverDocker = (*filteredDaemonFixture)(nil)

// The unscoped pass also lets finished runs go once retention allows: a run it settled is kept
// while recent, removed after networkstate.ExecutionRetention, and a scoped pass or a run still
// owned by a live supervisor is never part of that.
func TestRecoverNetworkRunsRemovesFinishedRunsPastRetention(t *testing.T) {
	f, d, evidence, connect := recoveryFixture(t)
	smoke, err := f.store.BeginQualification(d.smoke.Candidate(),
		[]networkstate.QualifiedClient{{Provider: "claude", Client: egress.ClientCLI, Version: "2.1.260"}})
	if err != nil {
		t.Fatal(err)
	}
	live, err := smoke.CreateExecution(context.Background(), networkstate.ExecutionSpec{
		Project: f.record.Project, PolicyFingerprint: f.policy.Fingerprint, Runtime: "docker",
		DaemonID: f.record.DaemonID, Endpoint: f.record.Endpoint,
		GatewayImage: f.record.GatewayImage, ClientImage: f.record.ClientImage})
	if err != nil {
		t.Fatal(err)
	}
	departSupervisor(t, f.store, f.record.ID)
	exists := func(id string) bool {
		_, err := os.Lstat(filepath.Join(f.store.Path(), "execution-"+id+".json"))
		return err == nil
	}
	now := time.Now()
	if _, err := recoverNetworkRunsAt(context.Background(), evidence, "", connect, now); err != nil {
		t.Fatal(err)
	}
	if !exists(f.record.ID) || !exists(live.ID) {
		t.Fatal("a run inside the retention window was removed")
	}

	later := now.Add(networkstate.ExecutionRetention + time.Hour)
	if _, err := recoverNetworkRunsAt(context.Background(), evidence, live.ID, connect, later); err != nil {
		t.Fatal(err)
	}
	if !exists(f.record.ID) {
		t.Fatal("a scoped pass removed a run it was not given")
	}
	if _, err := recoverNetworkRunsAt(context.Background(), evidence, "", connect, later); err != nil {
		t.Fatal(err)
	}
	if exists(f.record.ID) {
		t.Fatal("the settled run outlived retention")
	}
	if !exists(live.ID) {
		t.Fatal("a run its live supervisor still owns was removed")
	}
}

// A record that cannot be read keeps recovery from claiming nothing is pending, but it does not stop
// retention: removing a finished run is safe whatever else the listing held. Removing a snapshot is
// not: the record it could not read may be the run that names it.
func TestRecoverNetworkRunsStillRemovesExpiredRunsBesideAnUnreadableRecord(t *testing.T) {
	f, _, evidence, connect := recoveryFixture(t)
	unused := unusedSnapshot(t, f.store)
	departSupervisor(t, f.store, f.record.ID)
	now := time.Now()
	if _, err := recoverNetworkRunsAt(context.Background(), evidence, "", connect, now); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(f.store.Path(), "execution-"+strings.Repeat("f", 32)+".json")
	if err := os.WriteFile(broken, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := now.Add(networkstate.ExecutionRetention + time.Hour)
	_, err := recoverNetworkRunsAt(context.Background(), evidence, "", connect, later)
	if err == nil || !strings.Contains(err.Error(), "could not be read whole") {
		t.Fatalf("a partial listing = %v, want the could-not-be-read-whole refusal", err)
	}
	if _, err := os.Lstat(filepath.Join(f.store.Path(), "execution-"+f.record.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("retention stopped at an unreadable record", err)
	}
	if _, err := os.Lstat(broken); err != nil {
		t.Fatal("the unreadable record was removed", err)
	}
	if !snapshotKept(t, f.store, unused) {
		t.Fatal("a listing that could not be read whole removed a snapshot")
	}

	// Read whole again, the listing lets both go: the unused snapshot, and the run's own one
	// now that its record is gone.
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverNetworkRunsAt(context.Background(), evidence, "", connect, later); err != nil {
		t.Fatal(err)
	}
	if snapshotKept(t, f.store, unused) || snapshotKept(t, f.store, f.policy.Fingerprint) {
		t.Fatal("a snapshot nothing uses outlived a listing read whole")
	}
}

// unusedSnapshot admits a filtered policy no run will use and returns its fingerprint.
func unusedSnapshot(t *testing.T, store *networkstate.Store) string {
	t.Helper()
	mode := egress.Filtered
	policy, err := store.Admit(t.TempDir(), networkstate.Admission{InvocationMode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	return policy.Fingerprint
}

func snapshotKept(t *testing.T, store *networkstate.Store, fingerprint string) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(store.Path(), "snapshot-"+fingerprint+".json"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// The unscoped pass also removes the policy snapshots nothing uses: one no retained run names and
// nothing admitted or loaded inside the retention window. A run's own snapshot stays while its
// record does, a session's stays however old, and a pass scoped to one run removes none.
func TestRecoverNetworkRunsRemovesSnapshotsNoRetainedRunUses(t *testing.T) {
	f, _, evidence, connect := recoveryFixture(t)
	unused := unusedSnapshot(t, f.store)
	session, err := f.store.CaptureJob(networkstate.JobSnapshotRef{JobDigest: strings.Repeat("a", 64), SessionID: "remote_one"},
		nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(networkstate.ExecutionRetention + time.Hour)
	if _, err := recoverNetworkRunsAt(context.Background(), evidence, f.record.ID, connect, later); err != nil {
		t.Fatal(err)
	}
	if !snapshotKept(t, f.store, unused) {
		t.Fatal("a pass scoped to one run removed a snapshot")
	}
	if _, err := recoverNetworkRunsAt(context.Background(), evidence, "", connect, later); err != nil {
		t.Fatal(err)
	}
	if snapshotKept(t, f.store, unused) {
		t.Fatal("a snapshot nothing uses outlived retention")
	}
	if !snapshotKept(t, f.store, f.policy.Fingerprint) {
		t.Fatal("the snapshot a retained run names was removed")
	}
	if !snapshotKept(t, f.store, session.Fingerprint) {
		t.Fatal("the snapshot a session was created with was removed")
	}
}

// A setup check's snapshot goes one pass after the record of the check's run, like a launch's. The
// admission marks a current one; the record marks one an earlier coop wrote without a marker, which
// the origin of its one rule proves was setup's. A launch's from before markers stays: a session
// this host cannot see may still use it.
func TestRecoverNetworkRunsRemovesSetupCheckSnapshots(t *testing.T) {
	f, d, evidence, connect := recoveryFixture(t)
	ctx := context.Background()
	marker := func(fingerprint string) string { return filepath.Join(f.store.Path(), "admitted-"+fingerprint+".json") }
	// check admits the setup check's policy for a scratch project and starts the check's run under
	// it, as coop net setup does, then loses the run's supervisor. earlier drops the marker an earlier
	// coop never wrote.
	check := func(earlier bool) string {
		project := t.TempDir()
		policy, err := admitSetupSmoke(f.store, project)
		if err != nil {
			t.Fatal(err)
		}
		if earlier {
			if err := os.Remove(marker(policy.Fingerprint)); err != nil {
				t.Fatal(err)
			}
		}
		smoke, err := f.store.BeginQualification(d.smoke.Candidate(),
			[]networkstate.QualifiedClient{{Provider: "claude", Client: egress.ClientCLI, Version: "2.1.260"}})
		if err != nil {
			t.Fatal(err)
		}
		run, err := smoke.CreateExecution(ctx, networkstate.ExecutionSpec{Project: project, PolicyFingerprint: policy.Fingerprint,
			Runtime: "docker", DaemonID: f.record.DaemonID, Endpoint: f.record.Endpoint,
			GatewayImage: f.record.GatewayImage, ClientImage: f.record.ClientImage})
		if err != nil {
			t.Fatal(err)
		}
		departSupervisor(t, f.store, run.ID)
		return policy.Fingerprint
	}
	current, earlier := check(false), check(true)
	launch := unusedSnapshot(t, f.store)
	if err := os.Remove(marker(launch)); err != nil {
		t.Fatal(err)
	}

	// The first pass settles the interrupted checks. The second, past retention, removes their
	// records and keeps the snapshots those records still named. The third removes the snapshots.
	now := time.Now()
	later := now.Add(networkstate.ExecutionRetention + time.Hour)
	for _, at := range []time.Time{now, later} {
		if _, err := recoverNetworkRunsAt(ctx, evidence, "", connect, at); err != nil {
			t.Fatal(err)
		}
	}
	if !snapshotKept(t, f.store, current) || !snapshotKept(t, f.store, earlier) {
		t.Fatal("a setup check's snapshot went in the pass that removed its run's record")
	}
	if _, err := recoverNetworkRunsAt(ctx, evidence, "", connect, later); err != nil {
		t.Fatal(err)
	}
	if snapshotKept(t, f.store, current) || snapshotKept(t, f.store, earlier) {
		t.Fatalf("a setup check's snapshot outlived its run's record: current %v, earlier %v",
			snapshotKept(t, f.store, current), snapshotKept(t, f.store, earlier))
	}
	if !snapshotKept(t, f.store, launch) {
		t.Fatal("a launch's snapshot from before markers was removed")
	}
}
