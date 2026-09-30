package eval

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// seedRun writes a sealed run with the given workload and per-(case,status) trials.
func seedRun(t *testing.T, root, id, workload string, configs []string, trials []TrialRecord) {
	t.Helper()
	cases := map[string]bool{}
	repeat := 1
	for _, tr := range trials {
		cases[tr.Case] = true
		repeat = max(repeat, tr.Repetition+1)
	}
	ids := make([]string, 0, len(cases))
	for c := range cases {
		ids = append(ids, c)
	}
	sort.Strings(ids)
	seedMatrix(t, root, id, workload, configs, ids, repeat, trials)
}

func seedMatrix(t *testing.T, root, id, workload string, configs, cases []string, repeat int, trials []TrialRecord) {
	t.Helper()
	var rc []RunConfig
	for _, c := range configs {
		rc = append(rc, RunConfig{Kind: ConfigTarget, Label: c})
	}
	store, err := CreateRun(root, RunRecord{ID: id, Suite: "s", Runner: RunnerAgent, Workload: workload, Configs: rc, Cases: cases, Repeat: repeat, CreatedAt: time.Now()})
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
	if cmp.Paired != nil || !strings.Contains(cmp.Unpaired, "case sets differ") {
		t.Errorf("different requested case sets paired: %+v", cmp)
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

func TestCompareMissingRunNamesTheIDAndRecovery(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "present", "WL", []string{"codex"}, []TrialRecord{{Case: "a", Status: TrialPassed}})
	for _, tc := range []struct{ before, after, missing string }{
		{"missing-before", "present", "missing-before"},
		{"present", "missing-after", "missing-after"},
	} {
		_, err := Compare(root, tc.before, tc.after)
		if err == nil || !strings.Contains(err.Error(), "eval run \""+tc.missing+"\" was not found") ||
			!strings.Contains(err.Error(), "coop eval runs") || strings.Contains(err.Error(), root) {
			t.Errorf("compare %q to %q = %v; want named run, list command, no internal path", tc.before, tc.after, err)
		}
	}
}

func TestCompareDoesNotCallDamagedExistingRunMissing(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "present", "WL", []string{"codex"}, []TrialRecord{{Case: "a", Status: TrialPassed}})
	seedRun(t, root, "broken", "WL", []string{"codex"}, []TrialRecord{{Case: "a", Status: TrialPassed}})
	if err := os.WriteFile(filepath.Join(root, "broken", "run.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Compare(root, "broken", "present"); err == nil || strings.Contains(err.Error(), "was not found") {
		t.Errorf("corrupt manifest was mislabeled as missing: %v", err)
	}
	if err := os.Rename(filepath.Join(root, "present", "trials"), filepath.Join(root, "present", "trials-moved")); err != nil {
		t.Fatal(err)
	}
	if _, err := Compare(root, "present", "broken"); err == nil || strings.Contains(err.Error(), "was not found") {
		t.Errorf("missing trial directory was mislabeled as missing run: %v", err)
	}
}

// Change size is aggregated over the trials that were actually GRADED, and kept in its own field:
// an errored trial's workspace says nothing about what the configuration would have written, and
// size must never be mixed into the pass counts, because it is a review signal and not a score.
func TestCompareAggregatesSizeOnlyOverGradedTrials(t *testing.T) {
	root := t.TempDir()
	mk := func(id string, trials []TrialRecord) string {
		plan := &Plan{Suite: &Suite{Name: "s", Runner: RunnerAgent, Cases: []Case{{ID: "c"}}}, Repeat: 3, Jobs: 1, Timeout: time.Minute}
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

func TestCompareKeepsMissingRecordsInTheRequestedMatrix(t *testing.T) {
	root := t.TempDir()
	// Deliberately stale summary totals describe only the present records. Both complete case
	// inventories must survive, including a wholly absent case and unknown/running statuses.
	seedMatrix(t, root, "before", "WL", []string{"codex"}, []string{"a", "b", "c"}, 2, []TrialRecord{
		{Case: "a", Status: TrialPassed}, {Case: "a", Repetition: 1, Status: TrialRunning},
		{Case: "b", Status: TrialStatus("unknown")}, {Case: "b", Repetition: 1, Status: TrialTimedOut},
	})
	seedMatrix(t, root, "after", "WL", []string{"codex"}, []string{"c", "b", "a"}, 2, nil)
	cmp, err := Compare(root, "before", "after")
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Base.Requested != 6 || cmp.Base.Pending != 3 || cmp.Base.Errored != 1 || cmp.Base.TimedOut != 1 || cmp.Base.Passed != 1 || cmp.New.Requested != 6 || cmp.New.Pending != 6 {
		t.Fatalf("missing evidence dropped: before %+v after %+v", cmp.Base, cmp.New)
	}
	if len(cmp.Cases) != 3 || cmp.Cases[2].Case != "c" || cmp.Cases[2].Base.Pending != 2 || cmp.Cases[2].New.Pending != 2 {
		t.Fatalf("absent case dropped: %+v", cmp.Cases)
	}
	if cmp.Paired == nil || cmp.Paired.Requested != 6 || cmp.Paired.Graded != 0 || cmp.Paired.Effect != nil {
		t.Fatalf("incomplete evidence produced a score: %+v", cmp.Paired)
	}
}

func TestCompareMatchedEffectsRequireCompleteCoverage(t *testing.T) {
	root := t.TempDir()
	before := []TrialRecord{
		{Case: "a", Status: TrialPassed}, {Case: "a", Repetition: 1, Status: TrialFailed}, {Case: "a", Repetition: 2, Status: TrialFailed},
		{Case: "b", Status: TrialPassed}, {Case: "b", Repetition: 1, Status: TrialPassed}, {Case: "b", Repetition: 2, Status: TrialFailed},
	}
	after := []TrialRecord{
		{Case: "a", Status: TrialPassed}, {Case: "a", Repetition: 1, Status: TrialPassed}, {Case: "a", Repetition: 2, Status: TrialPassed},
		{Case: "b", Status: TrialFailed}, {Case: "b", Repetition: 1, Status: TrialFailed}, {Case: "b", Repetition: 2, Status: TrialPassed},
	}
	seedMatrix(t, root, "before", "WL", []string{"codex"}, []string{"a", "b"}, 3, before)
	seedMatrix(t, root, "after", "WL", []string{"frontier"}, []string{"b", "a"}, 3, after)
	cmp, err := Compare(root, "before", "after")
	if err != nil {
		t.Fatal(err)
	}
	p := cmp.Paired
	if p == nil || p.Cases != 2 || p.Repeat != 3 || p.Requested != 6 || p.Graded != 6 || p.Effect == nil {
		t.Fatalf("complete pairing = %+v", p)
	}
	delta := 1.0 / 6
	half := math.Sqrt(math.Log(40) / 6)
	if math.Abs(p.Effect.Delta-delta) > 1e-12 || math.Abs(p.Effect.Low-(delta-half)) > 1e-12 || math.Abs(p.Effect.High-(delta+half)) > 1e-12 {
		t.Errorf("fixed-suite effect = %+v", p.Effect)
	}
	if *cmp.Cases[0].Delta != 2.0/3 || *cmp.Cases[1].Delta != -1.0/3 {
		t.Errorf("case effects = %+v", cmp.Cases)
	}
	// A failed trial is graded. A missing, pending, timed-out or errored trial is not. Keep the
	// complete case's effect but never manufacture a whole-suite score from the remaining pairs.
	for _, status := range []TrialStatus{TrialPending, TrialRunning, TrialTimedOut, TrialError, "unknown"} {
		t.Run(string(status), func(t *testing.T) {
			after[5].Status = status
			seedMatrix(t, root, "partial-"+string(status), "WL", []string{"frontier"}, []string{"a", "b"}, 3, after)
			cmp, err := Compare(root, "before", "partial-"+string(status))
			if err != nil {
				t.Fatal(err)
			}
			if cmp.Paired.Graded != 5 || cmp.Paired.Effect != nil || cmp.Cases[0].Delta == nil || cmp.Cases[1].Delta != nil || cmp.Cases[1].Matched != 2 {
				t.Fatalf("incomplete pairing = %+v, cases %+v", cmp.Paired, cmp.Cases)
			}
		})
	}
}

func TestCompareDoesNotInferConfigurationOrRepeatPairing(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "one", "WL", []string{"codex"}, []TrialRecord{{Case: "a", Status: TrialPassed}})
	for _, tc := range []struct {
		name     string
		workload string
		configs  []string
		repeat   int
		want     string
	}{
		{"multiple", "WL", []string{"codex", "claude"}, 1, "one configuration"},
		{"repeats", "WL", []string{"codex"}, 3, "repetition counts differ"},
		{"identity", "", []string{"codex"}, 1, "workload identity is missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedMatrix(t, root, tc.name, tc.workload, tc.configs, []string{"a"}, tc.repeat, nil)
			cmp, err := Compare(root, "one", tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if cmp.Paired != nil || !strings.Contains(cmp.Unpaired+cmp.Mismatch, tc.want) {
				t.Fatalf("unmatched runs paired: %+v", cmp)
			}
			if cmp.New.Requested != len(tc.configs)*tc.repeat || cmp.New.Pending != cmp.New.Requested {
				t.Errorf("unpaired summaries lost records: %+v", cmp.New)
			}
		})
	}
	cmp, err := Compare(root, "one", "one")
	if err != nil || cmp.Paired != nil || !strings.Contains(cmp.Unpaired, "distinct runs") {
		t.Fatalf("same evidence treated as independent executions: %+v, %v", cmp, err)
	}
}

func TestCompareRejectsDamagedTrialMatrices(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cases     []string
		repeat    int
		trial     TrialRecord
		duplicate bool
	}{
		{name: "duplicate record", cases: []string{"a"}, repeat: 1, trial: TrialRecord{Case: "a"}, duplicate: true},
		{name: "undeclared case", cases: []string{"a"}, repeat: 1, trial: TrialRecord{Case: "b"}},
		{name: "extra configuration", cases: []string{"a"}, repeat: 1, trial: TrialRecord{Case: "a", ConfigIndex: 1}},
		{name: "extra repetition", cases: []string{"a"}, repeat: 1, trial: TrialRecord{Case: "a", Repetition: 1}},
		{name: "negative repetition", cases: []string{"a"}, repeat: 1, trial: TrialRecord{Case: "a", Repetition: -1}},
		{name: "duplicate case", cases: []string{"a", "a"}, repeat: 1, trial: TrialRecord{Case: "a"}},
		{name: "empty matrix", repeat: 1, trial: TrialRecord{Case: "a"}},
		{name: "zero repeats", cases: []string{"a"}, trial: TrialRecord{Case: "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			seedMatrix(t, root, "broken", "WL", []string{"codex"}, tc.cases, tc.repeat, []TrialRecord{tc.trial})
			if tc.duplicate {
				path := filepath.Join(root, "broken", "trials", TrialKey("a", 0, 0)+".json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "broken", "trials", "duplicate.json"), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Compare(root, "broken", "broken"); err == nil {
				t.Fatal("damaged evidence was accepted")
			}
		})
	}
}
