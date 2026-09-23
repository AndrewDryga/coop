package eval

import (
	"errors"
	"fmt"
	"os"
	"sort"
)

// The primary regression workflow: run a suite, change a preset/loop config or the Coop build, run
// the same suite again, then compare the two runs. Comparison is honest by construction — it leads
// with pass counts and COVERAGE (how many requested trials actually produced a graded result), pairs
// each case's outcome across the two runs, and refuses to reduce a mismatched WORKLOAD to a single
// number. A smaller anything never makes a winner by itself; incomplete coverage has no definitive
// winner. This is pure over two sealed runs' records; it launches nothing.

// Comparison is the result of comparing two runs of the same workload.
type Comparison struct {
	BaseID string
	NewID  string
	Suite  string
	// Mismatch is set when the two runs are not comparable (different workload); the rest is then a
	// side-by-side of separate results, never a merged score.
	Mismatch string

	Base ConfigOutcome
	New  ConfigOutcome
	// Cases pairs each case's per-run status counts, in case-id order.
	Cases []CaseComparison
}

// ConfigOutcome is one run's headline: the configurations it evaluated and its pass/coverage counts.
type ConfigOutcome struct {
	Configs   []string
	Requested int
	Passed    int
	Failed    int
	Errored   int
	Pending   int
	TimedOut  int
	// Size is the change-size summary over the trials that were actually GRADED. It is a separate
	// field, never mixed into the counts above, because size is a review signal and not a score: a
	// smaller wrong answer does not beat a larger right one, and two equally correct solutions of
	// different size is a question for a human, not a ranking.
	Size SizeSummary
}

// SizeSummary aggregates change size across a run's graded trials. Measured is how many of them
// carried a measurement, so a partial figure is never read as a complete one.
type SizeSummary struct {
	Measured   int
	NetGrowth  int // summed after-minus-before code across measured trials
	CodeBefore int
	CodeAfter  int
}

// Covered is the trials that reached a graded verdict (passed or failed) — the COVERAGE figure,
// shown beside the pass count, never used as the pass denominator (that is always Requested). A run
// with low coverage has no definitive result, however its passes look.
func (o ConfigOutcome) Covered() int { return o.Passed + o.Failed }

// CaseComparison is one case's outcome in each run.
type CaseComparison struct {
	Case string
	Base CaseCounts
	New  CaseCounts
}

// CaseCounts is the per-status tally for one case within one run (summed over configurations and
// repetitions of that case).
type CaseCounts struct {
	Passed, Failed, Errored, Pending, TimedOut int
}

// Compare reads both runs from root and pairs them. It returns an error only for an unreadable or
// unsealed run; a WORKLOAD mismatch is reported IN the comparison (Mismatch set), not as an error,
// so the caller can still show the two separate results and explain why they cannot be merged.
func Compare(root, baseID, newID string) (*Comparison, error) {
	base, err := loadSealed(root, baseID)
	if err != nil {
		return nil, err
	}
	next, err := loadSealed(root, newID)
	if err != nil {
		return nil, err
	}
	cmp := &Comparison{
		BaseID: baseID, NewID: newID, Suite: base.run.Suite,
		Base: outcomeOf(base), New: outcomeOf(next),
	}
	if base.run.Workload != next.run.Workload {
		cmp.Mismatch = fmt.Sprintf(
			"these runs evaluate different workloads (%s vs %s), so their scores are shown separately, not merged",
			short(base.run.Workload), short(next.run.Workload))
		return cmp, nil
	}
	cmp.Cases = pairCases(base, next)
	return cmp, nil
}

type sealedRun struct {
	run     *RunRecord
	summary *RunSummary
	trials  []TrialRecord
}

func loadSealed(root, id string) (sealedRun, error) {
	run, err := LoadRun(root, id)
	if errors.Is(err, os.ErrNotExist) {
		return sealedRun{}, fmt.Errorf("eval run %q was not found — use 'coop eval runs' to find recorded runs", id)
	}
	if err != nil {
		return sealedRun{}, fmt.Errorf("run %s: %w", id, err)
	}
	summary, ok, err := LoadSummary(root, id)
	if err != nil {
		return sealedRun{}, err
	}
	if !ok {
		return sealedRun{}, fmt.Errorf("run %s has no final summary (running or interrupted); only completed runs compare — inspect it with 'coop eval inspect %s'", id, id)
	}
	trials, err := LoadTrials(root, id)
	if err != nil {
		return sealedRun{}, err
	}
	return sealedRun{run: run, summary: summary, trials: trials}, nil
}

func outcomeOf(r sealedRun) ConfigOutcome {
	o := ConfigOutcome{Requested: r.summary.Requested}
	for _, c := range r.run.Configs {
		o.Configs = append(o.Configs, c.Label)
	}
	for _, tr := range r.trials {
		// Only a graded trial's size means anything: an errored or never-started trial's workspace
		// says nothing about what the configuration would have written.
		if tr.Size == nil || (tr.Status != TrialPassed && tr.Status != TrialFailed) {
			continue
		}
		o.Size.Measured++
		o.Size.CodeBefore += tr.Size.CodeBefore
		o.Size.CodeAfter += tr.Size.CodeAfter
		o.Size.NetGrowth += tr.Size.NetGrowth()
	}
	for status, n := range r.summary.Counts {
		switch status {
		case TrialPassed:
			o.Passed += n
		case TrialFailed:
			o.Failed += n
		case TrialError:
			o.Errored += n
		case TrialTimedOut:
			o.TimedOut += n
		case TrialPending, TrialRunning:
			o.Pending += n
		default:
			o.Errored += n // an unknown status is kept in the matrix, never dropped
		}
	}
	return o
}

// pairCases groups both runs' trials by case id and tallies each side, so a reader sees per-case
// wins and regressions rather than only a run total.
func pairCases(base, next sealedRun) []CaseComparison {
	byCase := map[string]*CaseComparison{}
	get := func(id string) *CaseComparison {
		if c := byCase[id]; c != nil {
			return c
		}
		c := &CaseComparison{Case: id}
		byCase[id] = c
		return c
	}
	for _, tr := range base.trials {
		tally(&get(tr.Case).Base, tr.Status)
	}
	for _, tr := range next.trials {
		tally(&get(tr.Case).New, tr.Status)
	}
	out := make([]CaseComparison, 0, len(byCase))
	for _, c := range byCase {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Case < out[j].Case })
	return out
}

func tally(c *CaseCounts, status TrialStatus) {
	switch status {
	case TrialPassed:
		c.Passed++
	case TrialFailed:
		c.Failed++
	case TrialError:
		c.Errored++
	case TrialTimedOut:
		c.TimedOut++
	case TrialPending, TrialRunning:
		c.Pending++
	default:
		c.Errored++ // an unknown status is kept, never silently dropped
	}
}

func short(fp string) string { return Fingerprint(fp).Short() }
