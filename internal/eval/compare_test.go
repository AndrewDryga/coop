package eval

import (
	"testing"
	"time"
)

// seedRun writes a sealed run with the given workload and per-(case,status) trials.
func seedRun(t *testing.T, root, id, workload string, configs []string, trials []TrialRecord) {
	t.Helper()
	var rc []RunConfig
	for _, c := range configs {
		rc = append(rc, RunConfig{Kind: ConfigTarget, Label: c})
	}
	store, err := CreateRun(root, RunRecord{ID: id, Suite: "s", Runner: RunnerAgent, Workload: workload, Configs: rc, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[TrialStatus]int{}
	for _, tr := range trials {
		if err := store.WriteTrial(tr); err != nil {
			t.Fatal(err)
		}
		counts[tr.Status]++
	}
	if err := store.Seal(RunSummary{Requested: len(trials), Counts: counts}); err != nil {
		t.Fatal(err)
	}
}

func TestCompareLeadsWithCoverageAndPairsCases(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "before", "WL", []string{"codex"}, []TrialRecord{
		{Case: "a", Status: TrialPassed}, {Case: "b", Status: TrialFailed},
		{Case: "c", Status: TrialError}, {Case: "d", Status: TrialPending},
	})
	seedRun(t, root, "after", "WL", []string{"frontier"}, []TrialRecord{
		{Case: "a", Status: TrialPassed}, {Case: "b", Status: TrialPassed},
	})
	cmp, err := Compare(root, "before", "after")
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Mismatch != "" {
		t.Fatalf("same workload reported a mismatch: %s", cmp.Mismatch)
	}
	// Coverage excludes error/pending, but they stay in Requested — the score's denominator.
	if cmp.Base.Passed != 1 || cmp.Base.Covered() != 2 || cmp.Base.Requested != 4 || cmp.Base.Errored != 1 || cmp.Base.Pending != 1 {
		t.Errorf("base outcome = %+v", cmp.Base)
	}
	if cmp.New.Passed != 2 || cmp.New.Covered() != 2 || cmp.New.Requested != 2 {
		t.Errorf("new outcome = %+v", cmp.New)
	}
	// Case b regressed→improved: base failed, new passed.
	var b CaseComparison
	for _, c := range cmp.Cases {
		if c.Case == "b" {
			b = c
		}
	}
	if b.Base.Failed != 1 || b.New.Passed != 1 {
		t.Errorf("case b pairing = %+v", b)
	}
}

// Two runs of DIFFERENT workloads never merge into one score: the comparison reports the mismatch
// and shows the two sides separately.
func TestCompareRefusesToMergeDifferentWorkloads(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "one", "WL-A", []string{"codex"}, []TrialRecord{{Case: "a", Status: TrialPassed}})
	seedRun(t, root, "two", "WL-B", []string{"codex"}, []TrialRecord{{Case: "a", Status: TrialPassed}})
	cmp, err := Compare(root, "one", "two")
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Mismatch == "" {
		t.Error("different workloads were merged without a mismatch note")
	}
	if len(cmp.Cases) != 0 {
		t.Error("mismatched runs should not pair cases into a single score")
	}
}

// An unsealed (interrupted) run does not compare — coverage would be a lie.
func TestCompareRefusesAnUnsealedRun(t *testing.T) {
	root := t.TempDir()
	if _, err := CreateRun(root, RunRecord{ID: "partial", Suite: "s", Runner: RunnerAgent, Workload: "WL"}); err != nil {
		t.Fatal(err)
	}
	seedRun(t, root, "done", "WL", []string{"codex"}, []TrialRecord{{Case: "a", Status: TrialPassed}})
	if _, err := Compare(root, "partial", "done"); err == nil {
		t.Error("comparing an unsealed run was allowed")
	}
}

// Change size is aggregated over the trials that were actually GRADED, and kept in its own field:
// an errored trial's workspace says nothing about what the configuration would have written, and
// size must never be mixed into the pass counts, because it is a review signal and not a score.
func TestCompareAggregatesSizeOnlyOverGradedTrials(t *testing.T) {
	root := t.TempDir()
	mk := func(id string, trials []TrialRecord) string {
		plan := &Plan{Suite: &Suite{Name: "s", Runner: RunnerAgent, Cases: []Case{{ID: "c"}}}, Repeat: 1, Jobs: 1, Timeout: time.Minute}
		rec := NewRunRecord(plan, []FrozenConfig{{Kind: ConfigTarget, Label: "t"}}, time.Now())
		rec.ID = id
		store, err := CreateRun(root, rec)
		if err != nil {
			t.Fatal(err)
		}
		counts := map[TrialStatus]int{}
		for _, tr := range trials {
			tr.RunID = id
			if err := store.WriteTrial(tr); err != nil {
				t.Fatal(err)
			}
			counts[tr.Status]++
		}
		if err := store.Seal(RunSummary{Requested: len(trials), Counts: counts}); err != nil {
			t.Fatal(err)
		}
		return id
	}

	base := mk("base-run", []TrialRecord{
		{Case: "c", Status: TrialPassed, Size: &TrialSize{CodeBefore: 10, CodeAfter: 30}},
		// Errored: its size must be ignored even though one was recorded.
		{Case: "c", ConfigIndex: 0, Repetition: 1, Status: TrialError, Size: &TrialSize{CodeBefore: 10, CodeAfter: 9000}},
		// Graded but unmeasured: counted as a trial, not as a zero-sized one.
		{Case: "c", ConfigIndex: 0, Repetition: 2, Status: TrialFailed},
	})
	next := mk("next-run", []TrialRecord{
		{Case: "c", Status: TrialPassed, Size: &TrialSize{CodeBefore: 10, CodeAfter: 14}},
		{Case: "c", ConfigIndex: 0, Repetition: 1, Status: TrialError, Size: &TrialSize{CodeBefore: 10, CodeAfter: 9000}},
		{Case: "c", ConfigIndex: 0, Repetition: 2, Status: TrialFailed},
	})

	cmp, err := Compare(root, base, next)
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Base.Size.Measured != 1 || cmp.Base.Size.NetGrowth != 20 {
		t.Errorf("base size = %+v, want 1 measured trial and +20", cmp.Base.Size)
	}
	if cmp.New.Size.Measured != 1 || cmp.New.Size.NetGrowth != 4 {
		t.Errorf("new size = %+v, want 1 measured trial and +4", cmp.New.Size)
	}
	// The errored trial's enormous size must not have leaked into either figure.
	if cmp.Base.Size.CodeAfter > 100 || cmp.New.Size.CodeAfter > 100 {
		t.Errorf("an ungraded trial's size was aggregated: base %+v new %+v", cmp.Base.Size, cmp.New.Size)
	}
	// And size is nowhere near the verdict counts.
	if cmp.Base.Passed != 1 || cmp.New.Passed != 1 {
		t.Errorf("pass counts changed with size: base %d new %d", cmp.Base.Passed, cmp.New.Passed)
	}
}
