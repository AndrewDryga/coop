package eval

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestListRunsExcludesCachesButRetainsUnreadableRecords(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"starters/core", "empty", "20260921-broken"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "20260921-broken", manifestName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateRun(root, RunRecord{ID: "20260920-valid", Suite: "core"}); err != nil {
		t.Fatal(err)
	}
	ids, err := ListRuns(root)
	if err != nil || !slices.Equal(ids, []string{"20260921-broken", "20260920-valid"}) {
		t.Fatalf("runs = %v, %v; only manifests identify runs, even if unreadable", ids, err)
	}
}

func TestCreateRunProtectsExistingEvalRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "eval")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateRun(root, RunRecord{ID: "run-1", Suite: "core"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("eval root mode: %v, %v; want 0700", info, err)
	}
}

func TestStoreRecordsARunItsTrialsAndSeals(t *testing.T) {
	root := t.TempDir()
	id := NewRunID(time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC), "abcdef1234567890")
	rec := RunRecord{
		ID: id, CreatedAt: time.Now().UTC(), Suite: "s", Runner: RunnerAgent,
		Workload: "abcdef1234567890", Repeat: 2, Jobs: 2, TimeoutMS: 3600000,
		Cases: []string{"greeting"},
		Configs: []RunConfig{
			{Kind: ConfigTarget, Label: "codex", Fingerprint: "cfg1", Build: "v1"},
			{Kind: ConfigPreset, Label: "frontier", Fingerprint: "cfg2", Build: "v1"},
		},
	}
	store, err := CreateRun(root, rec)
	if err != nil {
		t.Fatal(err)
	}
	// A second CreateRun with the same id refuses rather than clobbering results.
	if _, err := CreateRun(root, rec); err == nil {
		t.Error("CreateRun overwrote an existing run")
	}

	// Write a pending record for every trial, then finalize a couple.
	trials := []TrialRecord{
		{Case: "greeting", ConfigIndex: 0, ConfigLabel: "codex", Repetition: 0, Status: TrialPending},
		{Case: "greeting", ConfigIndex: 1, ConfigLabel: "frontier", Repetition: 0, Status: TrialPending},
	}
	for _, tr := range trials {
		if err := store.WriteTrial(tr); err != nil {
			t.Fatal(err)
		}
	}
	// Finalize one as passed (overwrites its own file).
	if err := store.WriteTrial(TrialRecord{Case: "greeting", ConfigIndex: 0, ConfigLabel: "codex", Repetition: 0, Status: TrialPassed, StartedAt: time.Now(), EndedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadTrials(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded %d trials, want 2", len(loaded))
	}
	if loaded[0].Status != TrialPassed || loaded[0].RunID != id {
		t.Errorf("trial 0 = %+v", loaded[0])
	}

	// Before sealing, the run reads as incomplete.
	if _, ok, err := LoadSummary(root, id); err != nil || ok {
		t.Errorf("unsealed run reported sealed: ok=%v err=%v", ok, err)
	}
	if err := store.Seal(RunSummary{Requested: 2, Counts: map[TrialStatus]int{TrialPassed: 1, TrialPending: 1}}); err != nil {
		t.Fatal(err)
	}
	sum, ok, err := LoadSummary(root, id)
	if err != nil || !ok {
		t.Fatalf("sealed run not readable: ok=%v err=%v", ok, err)
	}
	if sum.Requested != 2 || sum.Counts[TrialPassed] != 1 {
		t.Errorf("summary = %+v", sum)
	}

	// The run manifest round-trips, and the run lists.
	back, err := LoadRun(root, id)
	if err != nil || back.Suite != "s" || len(back.Configs) != 2 {
		t.Errorf("manifest = %+v err=%v", back, err)
	}
	ids, err := ListRuns(root)
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Errorf("ListRuns = %v err=%v", ids, err)
	}
}

func TestNewRunIDIsTimeOrderedAndTagged(t *testing.T) {
	early := NewRunID(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "1111111111111111")
	late := NewRunID(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), "1111111111111111")
	if !(early < late) {
		t.Errorf("run ids not time-ordered: %q !< %q", early, late)
	}
	if len(filepath.Base(early)) == 0 {
		t.Error("empty run id")
	}
}
