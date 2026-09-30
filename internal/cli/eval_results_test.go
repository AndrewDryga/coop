package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/eval"
)

func seedEvalResults(t *testing.T, id string, sealed bool, trials ...eval.TrialRecord) string {
	t.Helper()
	root, err := evalStateRoot()
	if err != nil {
		t.Fatal(err)
	}
	store, err := eval.CreateRun(root, eval.RunRecord{
		ID: id, Suite: "coop-core", Runner: eval.RunnerAgent, Workload: "same-workload",
		CreatedAt: time.Date(2026, 9, 21, 5, 50, 49, 0, time.UTC), Repeat: 1,
		Cases:   []string{"fix-the-cause", "keep-the-contract", "no-collateral-damage"},
		Configs: []eval.RunConfig{{Kind: eval.ConfigTarget, Label: "claude", Fingerprint: "abcdef1234567890", Build: "v-test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[eval.TrialStatus]int{}
	for _, trial := range trials {
		trial.ConfigLabel = "claude"
		if err := store.WriteTrial(trial); err != nil {
			t.Fatal(err)
		}
		counts[trial.Status]++
	}
	if sealed {
		if err := store.Seal(eval.RunSummary{Requested: 3, Counts: counts}); err != nil {
			t.Fatal(err)
		}
	}
	return store.Dir()
}

func TestEvalResultsJourney(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := seedEvalResults(t, "20260921-errors", true,
		eval.TrialRecord{Case: "fix-the-cause", Status: eval.TrialError, Detail: "provider refused: sign-in required"},
		eval.TrialRecord{Case: "keep-the-contract", Status: eval.TrialTimedOut, Detail: "trial budget exhausted"},
		eval.TrialRecord{Case: "no-collateral-damage", Status: eval.TrialPending})
	if err := os.Mkdir(filepath.Join(filepath.Dir(dir), "starters"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{}}
	run := func(args ...string) string {
		t.Helper()
		return captureStdout(t, func() {
			if code, err := a.cmdEval(args); code != 0 || err != nil {
				t.Errorf("eval %v = %d, %v", args, code, err)
			}
		})
	}
	listing := run("runs")
	for _, want := range []string{"20260921-errors", "claude", "0/3 passed", "0/3 graded", "1 error", "1 timed out", "1 pending", "coop eval inspect"} {
		if !strings.Contains(listing, want) {
			t.Errorf("listing missing %q:\n%s", want, listing)
		}
	}
	if strings.Contains(listing, "starters") {
		t.Errorf("cache listed as run:\n%s", listing)
	}
	for _, args := range [][]string{{"inspect"}, {"inspect", "20260921-errors"}} {
		out := run(args...)
		for _, want := range []string{"coop-core", "claude [config abcdef123456, build v-test]", "0/3 passed", "0/3 graded", "no trial reached grading", "fix-the-cause", "sign-in required", "trial budget exhausted", "not a model", dir} {
			if !strings.Contains(out, want) {
				t.Errorf("inspection missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "cloc unavailable") || strings.Contains(out, "verdict unaffected") {
			t.Errorf("ungraded run blamed optional measurement tooling:\n%s", out)
		}
	}
	comparison := run("compare", "20260921-errors", "20260921-errors")
	for _, want := range []string{"claude [config abcdef123456, build v-test]", "no definitive winner", "coop eval inspect 20260921-errors", "timed out 1", "pending 1"} {
		if !strings.Contains(comparison, want) {
			t.Errorf("comparison missing %q:\n%s", want, comparison)
		}
	}
}

func TestEvalInspectionUnsealedAndUnsafeDetail(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	seedEvalResults(t, "partial", false,
		eval.TrialRecord{Case: "fix-the-cause", Status: eval.TrialRunning, Detail: "\x1b]52;c;INJECT\a\r\x1b[2Jhostile\u202etext\n" + strings.Repeat("detail ", 1000)})
	out := captureStdout(t, func() {
		if code, err := (&app{}).cmdEval([]string{"inspect", "partial"}); code != 0 || err != nil {
			t.Fatalf("inspect = %d, %v", code, err)
		}
	})
	for _, want := range []string{"running or interrupted", "0/3 graded", "2 pending", "truncated", "Records:"} {
		if !strings.Contains(out, want) {
			t.Errorf("inspection missing %q:\n%s", want, out)
		}
	}
	if strings.ContainsAny(out, "\x1b\a\r\u202e") || len(out) > 6000 {
		t.Fatalf("unsafe or unbounded inspection: length %d", len(out))
	}
}

func TestEvalInspectionEmptyMissingAndUnreadable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	a := &app{}
	for _, args := range [][]string{{"inspect"}, {"inspect", "missing"}, {"inspect", "../escape"}} {
		if code, err := a.cmdEval(args); code != 1 || err == nil {
			t.Errorf("eval %v = %d, %v; want actionable read failure", args, code, err)
		}
	}
	if code, err := a.cmdEval([]string{"inspect", "one", "extra"}); code != 2 || err == nil {
		t.Errorf("extra args = %d, %v", code, err)
	}
	dir := seedEvalResults(t, "broken", true)
	if err := os.WriteFile(filepath.Join(dir, "run.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	listing := captureStdout(t, func() { _, _ = a.cmdEval([]string{"runs"}) })
	if !strings.Contains(listing, "unreadable record") {
		t.Errorf("corrupt record disappeared or looked healthy:\n%s", listing)
	}
	if code, err := a.cmdEval([]string{"inspect", "broken"}); code != 1 || err == nil {
		t.Errorf("unreadable = %d, %v", code, err)
	}
}

func TestEvalMeasurementCoverageIsVisibleForPassingRuns(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	seedEvalResults(t, "measured", true,
		eval.TrialRecord{Case: "fix-the-cause", Status: eval.TrialPassed, Size: &eval.TrialSize{CodeBefore: 10, CodeAfter: 14}},
		eval.TrialRecord{Case: "keep-the-contract", Status: eval.TrialPassed},
		eval.TrialRecord{Case: "no-collateral-damage", Status: eval.TrialPassed})
	seedEvalResults(t, "unmeasured", true,
		eval.TrialRecord{Case: "fix-the-cause", Status: eval.TrialPassed},
		eval.TrialRecord{Case: "keep-the-contract", Status: eval.TrialPassed},
		eval.TrialRecord{Case: "no-collateral-damage", Status: eval.TrialPassed})
	a := &app{}
	inspect := captureStdout(t, func() {
		if code, err := a.cmdEval([]string{"inspect", "unmeasured"}); code != 0 || err != nil {
			t.Fatalf("inspect = %d, %v", code, err)
		}
	})
	for _, want := range []string{"Change size:", "not measured", "0/3 graded trials", "verdict unaffected"} {
		if !strings.Contains(inspect, want) {
			t.Errorf("unmeasured passing run hides %q:\n%s", want, inspect)
		}
	}
	comparison := captureStdout(t, func() {
		if code, err := a.cmdEval([]string{"compare", "measured", "unmeasured"}); code != 0 || err != nil {
			t.Fatalf("compare = %d, %v", code, err)
		}
	})
	for _, want := range []string{"measured 1/3 graded trials", "0/3 graded trials", "95% bound: -100.0 to +100.0", "at least three repeats", "no definitive winner"} {
		if !strings.Contains(comparison, want) {
			t.Errorf("comparison hides measurement coverage %q:\n%s", want, comparison)
		}
	}
}

func TestEvalComparisonReportsOnlyFullyMatchedEffects(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root, err := evalStateRoot()
	if err != nil {
		t.Fatal(err)
	}
	writeRun := func(id string, after, partial, multiple bool) {
		t.Helper()
		configs := []eval.RunConfig{{Label: "codex", Fingerprint: "abcdef", Build: "test"}}
		if multiple {
			configs = append(configs, eval.RunConfig{Label: "claude"})
		}
		store, err := eval.CreateRun(root, eval.RunRecord{
			ID: id, Suite: "comparison", Workload: "frozen", Repeat: 3,
			Cases: []string{"a", "b", "c"}, Configs: configs,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []string{"a", "b", "c"} {
			for rep := range 3 {
				if partial && c == "b" && rep == 2 {
					continue
				}
				status := eval.TrialFailed
				if c == "a" || after && c == "b" {
					status = eval.TrialPassed
				}
				if err := store.WriteTrial(eval.TrialRecord{Case: c, Repetition: rep, Status: status}); err != nil {
					t.Fatal(err)
				}
			}
		}
		// Comparison must rebuild coverage rather than trusting these deliberately empty totals.
		if err := store.Seal(eval.RunSummary{}); err != nil {
			t.Fatal(err)
		}
	}
	writeRun("before", false, false, false)
	for _, tc := range []struct {
		name              string
		partial, multiple bool
		want, absent      []string
	}{
		{"complete", false, false,
			[]string{"3 cases × 3 repeats; 9/9 pairs graded", "3/9 passed", "6/9 passed", "Pass-rate change: +33.3 percentage points", "95% bound: -30.7 to +97.4", "fixed suite", "independent trial executions", "Repeats are not new tasks", "no preregistered decision rule", "Matched: 3/3 pairs; change +100.0"}, nil},
		{"partial", true, false,
			[]string{"8/9 pairs graded", "5/9 passed", "1 pending", "aggregate change and bound unavailable", "Matched: 2/3 pairs; change unavailable"}, []string{"Pass-rate change:", "95% bound:"}},
		{"multiple", false, true,
			[]string{"one configuration per run", "6/18 passed", "9/18 graded", "9 pending"}, []string{"Paired:", "Pass-rate change:", "95% bound:", "Matched:"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeRun(tc.name, true, tc.partial, tc.multiple)
			out := captureStdout(t, func() {
				if code, err := (&app{}).cmdEval([]string{"compare", "before", tc.name}); code != 0 || err != nil {
					t.Fatalf("compare = %d, %v", code, err)
				}
			})
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("comparison missing %q:\n%s", want, out)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(out, absent) {
					t.Errorf("comparison claims unsupported %q:\n%s", absent, out)
				}
			}
		})
	}
}
