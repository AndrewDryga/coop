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
