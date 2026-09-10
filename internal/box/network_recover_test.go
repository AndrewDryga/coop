package box

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/networkstate"
)

// departSupervisor rewrites one retained run's supervisor identity so the
// kernel reports the launching process as departed — what a host crash, a
// SIGKILL or a reboot leaves behind.
func departSupervisor(t *testing.T, store *networkstate.Store, id string) {
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
	// Same pid, a different start time in the same token format: the kernel
	// proves the process that launched this run is not the one holding that pid
	// now, which is exactly what a reused pid after a crash looks like.
	supervisor["start_token"] = token + "9"
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
		if err != nil || len(results) != 1 || !slices.Contains(results[0].Removed, "agent") || !results[0].Sealed {
			t.Fatalf("an absent container was not settled: %+v %v", results, err)
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

// The launch fixture's daemon is exactly the bounded surface recovery needs.
var _ recoverDocker = (*filteredDaemonFixture)(nil)
