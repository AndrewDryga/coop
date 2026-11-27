package fleet

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestPlacementReservesMachineAndSharedAccounts(t *testing.T) {
	c, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.SetAccountLimit("claude@demo", 8); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Invite("host", "linux", false); err != nil {
		t.Fatal(err)
	}
	job := workerproto.JobSpec{Version: 2, Targets: []string{"claude@demo"}, Mode: "normal", Companions: []workerproto.JobCompanion{}, Egress: workerproto.JobEgress{Mode: "open", Rules: []workerproto.JobRule{}}, Environment: map[string]string{}, Check: workerproto.JobCheck{Argv: []string{}, Environment: map[string]string{}}, Limits: workerproto.JobLimits{MaxTurns: 2, MaxQueuedTurns: 1, MaxQueuedBytes: 1 << 20, TurnTimeoutMS: 60000, MaxPatchBytes: 1 << 20}}
	for _, item := range []struct {
		id  string
		cpu int
	}{{"large", 4000}, {"fits", 2000}, {"second", 1000}} {
		job.JobRef = item.id
		job.Resources = workerproto.JobResources{CPUMillis: item.cpu, MemoryBytes: 1 << 30, PIDs: 128}
		raw, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.Submit("linux", raw, "hello"); err != nil {
			t.Fatal(err)
		}
	}
	job.ModelCredentials = "controller"
	raw, err := job.CanonicalDocument()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Submit("linux", raw, "hello"); err == nil || !strings.Contains(err.Error(), "Ryker") {
		t.Fatalf("submitted unsupported central model job: %v", err)
	}
	w := workerproto.WorkerHello{ID: "host", WorkspaceRef: "linux", Capabilities: []workerproto.Capability{{Name: "job-setup", Version: "2"}}, Capacity: workerproto.Capacity{TurnSlotsFree: 8, TurnSlotsTotal: 8, Machine: &workerproto.MachineCapacity{Daemon: strings.Repeat("a", 64), CPUMillis: 2500, FreeCPUMillis: 2500, MemoryBytes: 2 << 30, FreeMemoryBytes: 2 << 30}}}
	placeOnce := func() {
		t.Helper()
		tx, err := c.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err = place(tx, w); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	want := func(id, stage string) {
		t.Helper()
		s, err := c.Status(id)
		if err != nil || s.Stage != stage {
			t.Fatalf("%s: %s, %v", id, s.Stage, err)
		}
	}
	placeOnce()
	want("fits", "queued")
	w.AccountUsage = &workerproto.AccountUsage{Accounts: []workerproto.AccountUsageAccount{{Account: "claude@other", SignIn: workerproto.AccountSignedIn}}}
	placeOnce()
	want("fits", "queued")
	w.AccountUsage.Accounts[0].Account = "claude@demo"
	placeOnce()
	placeOnce()
	want("fits", "creating")
	want("large", "queued")
	want("second", "queued")
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	unavailable := false
	later := now.Add(time.Hour)
	if err = observeQuota(tx, &workerproto.AccountUsage{Accounts: []workerproto.AccountUsageAccount{{Account: "claude@demo", SignIn: workerproto.AccountSignedIn, CheckedAt: &now, Limits: []workerproto.AccountUsageLimit{{Available: &unavailable, ResetsAt: &later}}}}}, now); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE jobs SET stage='completed' WHERE id='fits'"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	placeOnce()
	want("second", "queued")
}
