package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/AndrewDryga/coop/internal/eval"
	"github.com/AndrewDryga/coop/internal/ui"
)

func (a *app) evalInspect(args []string) (int, error) {
	if len(args) > 1 {
		return 2, rejectArgs("eval inspect", args[1:])
	}
	if len(args) == 1 && strings.HasPrefix(args[0], "-") {
		return 2, unknownOptionErr(args[0], "eval inspect", nil)
	}
	root, err := evalStateRoot()
	if err != nil {
		return 1, err
	}
	id := ""
	if len(args) == 1 {
		id = args[0]
	} else {
		ids, err := eval.ListRuns(root)
		if err != nil {
			return 1, err
		}
		if len(ids) == 0 {
			return 1, fmt.Errorf("no eval runs recorded yet — preview one with 'coop eval run core codex --timeout 35m --dry-run'")
		}
		id = ids[0]
	}
	run, err := eval.LoadRun(root, id)
	if errors.Is(err, os.ErrNotExist) {
		return 1, fmt.Errorf("eval run %q was not found — use 'coop eval runs' to find recorded runs", id)
	}
	if err != nil {
		return 1, fmt.Errorf("read eval run %q: %w", id, err)
	}
	sum, sealed, err := eval.LoadSummary(root, id)
	if err != nil {
		return 1, fmt.Errorf("read summary for eval run %q: %w", id, err)
	}
	trials, err := eval.LoadTrials(root, id)
	if err != nil {
		return 1, fmt.Errorf("read trials for eval run %q: %w", id, err)
	}
	if !sealed {
		// A run can be inspected while it is executing, or after interruption. Missing
		// records remain pending in the full requested matrix; never shrink the denominator.
		sum = &eval.RunSummary{Requested: len(run.Cases) * len(run.Configs) * run.Repeat, Counts: map[eval.TrialStatus]int{}}
		for _, tr := range trials {
			sum.Counts[tr.Status]++
		}
		if missing := sum.Requested - len(trials); missing > 0 {
			sum.Counts[eval.TrialPending] += missing
		}
	}
	fmt.Printf("Eval %s\n", evalDisplayText(id))
	printEvalText("Suite: ", run.Suite)
	fmt.Println("Configurations:")
	for _, c := range run.Configs {
		printEvalText("  - ", c.Description())
	}
	renderEvalRuntimes("", run.Runtimes)
	fmt.Printf("Started: %s\n", run.CreatedAt.Local().Format("2006-01-02 15:04 MST"))
	printEvalText("Result: ", evalResultLine(*sum))
	printEvalText("Change size: ", evalChangeSizeLine(evalSizeSummary(trials), sum.Counts[eval.TrialPassed]+sum.Counts[eval.TrialFailed]))
	if !sealed {
		fmt.Println("\n⚠ No final summary — running or interrupted; not comparable yet.")
	}
	if sum.Counts[eval.TrialError] > 0 {
		fmt.Println("\nAn error is not a model-quality failure: no graded verdict was obtained.")
	}
	if sum.Counts[eval.TrialTimedOut]+sum.Counts[eval.TrialPending]+sum.Counts[eval.TrialRunning] > 0 {
		fmt.Println("Timeouts and pending trials also leave grading incomplete.")
	}
	printed := false
	for _, tr := range trials {
		if tr.Status == eval.TrialPassed {
			continue
		}
		if !printed {
			fmt.Println("\nTrials needing attention:")
			printed = true
		}
		printEvalText("  ", fmt.Sprintf("%s — %s, repeat %d: %s", tr.Case, tr.ConfigLabel, tr.Repetition+1, tr.Status))
		detail := tr.Detail
		if detail == "" {
			detail = "No diagnostic was recorded."
			if tr.Status == eval.TrialPending {
				detail = "This trial did not start."
			} else if tr.Status == eval.TrialRunning {
				detail = "Started, but no final result has been recorded."
			}
		}
		// Provider/verifier output is evidence, never terminal control or a next action.
		fmt.Println("    Recorded detail:")
		const maxDetail = 2400
		if len(detail) > maxDetail {
			detail = detail[:maxDetail] + "… (truncated; see the trial record)"
		}
		for _, line := range strings.Split(detail, "\n") {
			printEvalText("      ", line)
		}
	}
	if printed || !sealed {
		fmt.Println("\nCheck the recorded cause before starting another paid run.")
	}
	if sum.Counts[eval.TrialError] > 0 {
		fmt.Println("For sign-in or quota errors: coop credentials <agent>")
	}
	if sum.Counts[eval.TrialTimedOut]+sum.Counts[eval.TrialPending] > 0 {
		fmt.Println("For timeouts: check both the case timeout and the run's --timeout.")
	}
	fmt.Printf("\nRecords: %s\n", evalDisplayText(filepath.Join(root, id)))
	fmt.Println("  Trial diagnostics: trials/*.json; retained work, when available: work/")
	return 0, nil
}

func renderEvalRuntimes(indent string, runtimes []eval.RunRuntime) {
	for _, r := range runtimes {
		printEvalText(indent+"Runtime: ", fmt.Sprintf("%s — %s, %s, %s, %s; %d CPU / %d GiB / %d PIDs", r.Case, r.Protocol, r.ImageID, r.Platform, r.Workdir, r.CPUs, r.MemoryBytes>>30, r.PIDs))
		printEvalText(indent+"  Coop-adapted: ", fmt.Sprintf("declared storage %d GiB; storage limit unenforced and usage unmeasured", r.DeclaredStorageBytes>>30))
	}
}

func evalSizeSummary(trials []eval.TrialRecord) eval.SizeSummary {
	var size eval.SizeSummary
	for _, trial := range trials {
		if trial.Size == nil || trial.Status != eval.TrialPassed && trial.Status != eval.TrialFailed {
			continue
		}
		size.Measured++
		size.CodeBefore += trial.Size.CodeBefore
		size.CodeAfter += trial.Size.CodeAfter
		size.NetGrowth += trial.Size.NetGrowth()
	}
	return size
}

func evalChangeSizeLine(size eval.SizeSummary, graded int) string {
	if size.Measured == 0 {
		if graded == 0 {
			return "not measured (0/0 graded trials; no trial reached grading)"
		}
		return fmt.Sprintf("not measured (0/%d graded trials; optional cloc unavailable or failed; verdict unaffected)", graded)
	}
	return fmt.Sprintf("net code %+d (%d→%d lines); measured %d/%d graded trials",
		size.NetGrowth, size.CodeBefore, size.CodeAfter, size.Measured, graded)
}

func evalConfigLabels(run *eval.RunRecord) []string {
	labels := make([]string, 0, len(run.Configs))
	for _, c := range run.Configs {
		labels = append(labels, c.Label)
	}
	return labels
}

func evalResultLine(s eval.RunSummary) string {
	passed, failed := s.Counts[eval.TrialPassed], s.Counts[eval.TrialFailed]
	parts := []string{fmt.Sprintf("%d/%d passed", passed, s.Requested), fmt.Sprintf("%d/%d graded", passed+failed, s.Requested)}
	for _, st := range []struct {
		status eval.TrialStatus
		label  string
	}{{eval.TrialFailed, "failed"}, {eval.TrialError, "errors"}, {eval.TrialTimedOut, "timed out"}, {eval.TrialPending, "pending"}, {eval.TrialRunning, "running"}} {
		if n := s.Counts[st.status]; n > 0 {
			label := st.label
			if st.status == eval.TrialError && n == 1 {
				label = "error"
			}
			parts = append(parts, fmt.Sprintf("%d %s", n, label))
		}
	}
	return strings.Join(parts, "; ")
}

func evalCaseResult(c eval.CaseCounts) string {
	parts := []string{fmt.Sprintf("%d/%d passed", c.Passed, c.Requested), fmt.Sprintf("%d/%d graded", c.Covered(), c.Requested)}
	for _, s := range []struct {
		n     int
		label string
	}{{c.Failed, "failed"}, {c.Errored, "error"}, {c.TimedOut, "timed out"}, {c.Pending, "pending"}} {
		if s.n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", s.label, s.n))
		}
	}
	return strings.Join(parts, ", ")
}

// Quote only nonprinting runes: persisted model output must not send cursor, clipboard,
// bidi or other terminal controls. Visible text remains readable, including Unicode.
func evalDisplayText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		} else {
			q := strconv.QuoteRuneToGraphic(r)
			b.WriteString(q[1 : len(q)-1])
		}
	}
	return b.String()
}

func printEvalText(prefix, text string) {
	for _, line := range ui.PrefixedLines(prefix, evalDisplayText(text), ui.TermWidth(os.Stdout)) {
		fmt.Println(line)
	}
}
