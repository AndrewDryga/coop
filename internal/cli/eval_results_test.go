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
		Configs: []eval.RunConfig{{Kind: eval.ConfigTarget, Label: "claude", Build: "v-test"}},
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
		for _, want := range []string{"coop-core", "claude", "0/3 passed", "0/3 graded", "fix-the-cause", "sign-in required", "trial budget exhausted", "not a model", dir} {
			if !strings.Contains(out, want) {
				t.Errorf("inspection missing %q:\n%s", want, out)
			}
		}
	}
	comparison := run("compare", "20260921-errors", "20260921-errors")
	for _, want := range []string{"no definitive winner", "coop eval inspect 20260921-errors", "timed out 1", "pending 1"} {
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
