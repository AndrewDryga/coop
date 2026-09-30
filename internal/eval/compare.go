package eval

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
)

// Comparison reads retained evidence only; it never launches a trial or chooses a winner.
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
	// Unpaired explains why only separate counts, not matched effects, are available.
	Unpaired string
	Paired   *PairedComparison
}

// ConfigOutcome is one run's headline: the configurations it evaluated and its pass/coverage counts.
type ConfigOutcome struct {
	Configs []string
	CaseCounts
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

// CaseComparison is one case's outcome in each run.
type CaseComparison struct {
	Case    string
	Base    CaseCounts
	New     CaseCounts
	Matched int
	// Delta is the pass-rate difference (after minus before), only for a fully graded case.
	Delta *float64
}

// CaseCounts is the per-status tally for one case within one run (summed over configurations and
// repetitions of that case).
type CaseCounts struct {
	Requested                                  int
	Passed, Failed, Errored, Pending, TimedOut int
}

// Covered counts graded verdicts, never the pass denominator (which is always Requested).
func (c CaseCounts) Covered() int { return c.Passed + c.Failed }

// PairedComparison keeps task diversity separate from repeat precision. Effect is absent unless
// every requested pair was graded. It describes this fixed suite, not an unseen task population.
type PairedComparison struct {
	Cases, Repeat, Requested, Graded int
	Effect                           *PairedEffect
}

// PairedEffect is a fixed-suite pass-rate change and its conservative 95% repeat-uncertainty bound.
type PairedEffect struct {
	Delta, Low, High float64
}

// Compare reads both runs from root and pairs them. It rejects unreadable, unsealed or damaged
// records; a WORKLOAD mismatch is reported IN the comparison (Mismatch set), not as an error,
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
	if base.run.Workload == "" || next.run.Workload == "" {
		cmp.Mismatch = "a workload identity is missing, so these results cannot be paired"
		return cmp, nil
	}
	if base.run.Workload != next.run.Workload {
		cmp.Mismatch = fmt.Sprintf(
			"these runs evaluate different workloads (%s vs %s), so their scores are shown separately, not merged",
			short(base.run.Workload), short(next.run.Workload))
		return cmp, nil
	}
	cmp.Cases = pairCases(base, next)
	switch {
	case baseID == newID:
		cmp.Unpaired = "choose two distinct runs for a matched comparison"
	case len(base.run.Configs) != 1 || len(next.run.Configs) != 1:
		cmp.Unpaired = "paired effects require one configuration per run; configuration correspondence is not inferred"
	case len(cmp.Cases) != len(base.run.Cases) || len(cmp.Cases) != len(next.run.Cases):
		cmp.Unpaired = "the requested case sets differ"
	case base.run.Repeat != next.run.Repeat:
		cmp.Unpaired = "the requested repetition counts differ"
	default:
		cmp.Paired = pairEffects(cmp.Cases, base, next)
	}
	return cmp, nil
}

type sealedRun struct {
	run    *RunRecord
	trials map[string]TrialRecord
}

func loadSealed(root, id string) (sealedRun, error) {
	run, err := LoadRun(root, id)
	if errors.Is(err, os.ErrNotExist) {
		return sealedRun{}, fmt.Errorf("eval run %q was not found — use 'coop eval runs' to find recorded runs", id)
	}
	if err != nil {
		return sealedRun{}, fmt.Errorf("run %s: %w", id, err)
	}
	_, ok, err := LoadSummary(root, id)
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
	if len(run.Cases) == 0 || len(run.Configs) == 0 || run.Repeat < 1 {
		return sealedRun{}, fmt.Errorf("run %s: damaged requested trial matrix", id)
	}
	cases := make(map[string]bool, len(run.Cases))
	for _, c := range run.Cases {
		if c == "" || cases[c] {
			return sealedRun{}, fmt.Errorf("run %s: empty or duplicate case in requested matrix", id)
		}
		cases[c] = true
	}
	r := sealedRun{run: run, trials: make(map[string]TrialRecord)}
	for _, tr := range trials {
		if tr.RunID != id || !cases[tr.Case] || tr.ConfigIndex < 0 || tr.ConfigIndex >= len(run.Configs) || tr.Repetition < 0 || tr.Repetition >= run.Repeat {
			return sealedRun{}, fmt.Errorf("run %s: trial outside its requested matrix", id)
		}
		key := TrialKey(tr.Case, tr.ConfigIndex, tr.Repetition)
		if _, exists := r.trials[key]; exists {
			return sealedRun{}, fmt.Errorf("run %s: duplicate trial %q", id, key)
		}
		r.trials[key] = tr
	}
	// The manifest, not summary counts or file presence, owns the denominator. A missing record
	// after interruption or damage stays pending; it must never improve the apparent pass rate.
	for _, c := range run.Cases {
		for config := range run.Configs {
			for rep := range run.Repeat {
				key := TrialKey(c, config, rep)
				if _, exists := r.trials[key]; !exists {
					r.trials[key] = TrialRecord{Case: c, ConfigIndex: config, Repetition: rep, Status: TrialPending}
				}
			}
		}
	}
	return r, nil
}

func outcomeOf(r sealedRun) ConfigOutcome {
	var o ConfigOutcome
	for _, c := range r.run.Configs {
		o.Configs = append(o.Configs, c.Description())
	}
	for _, tr := range r.trials {
		tally(&o.CaseCounts, tr.Status)
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
	c.Requested++
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

func pairEffects(cases []CaseComparison, base, next sealedRun) *PairedComparison {
	p := &PairedComparison{Cases: len(cases), Repeat: base.run.Repeat, Requested: len(cases) * base.run.Repeat}
	passedChange := 0
	for i := range cases {
		c := &cases[i]
		passedChange += c.New.Passed - c.Base.Passed
		for rep := range p.Repeat {
			key := TrialKey(c.Case, 0, rep)
			b, n := base.trials[key].Status, next.trials[key].Status
			if (b == TrialPassed || b == TrialFailed) && (n == TrialPassed || n == TrialFailed) {
				c.Matched++
			}
		}
		p.Graded += c.Matched
		if c.Matched == p.Repeat {
			delta := float64(c.New.Passed-c.Base.Passed) / float64(p.Repeat)
			c.Delta = &delta
		}
	}
	if p.Graded == p.Requested {
		delta := float64(passedChange) / float64(p.Requested)
		// Hoeffding for 2*N*R independent signed Bernoulli terms, each with range 1/(N*R):
		// P(|estimate - fixed-suite mean| >= h) <= 2*exp(-N*R*h^2). At 95%, h=sqrt(log(40)/(N*R)).
		// Repeats narrow uncertainty on THESE cases; they do not supply new independent tasks.
		half := math.Sqrt(math.Log(40) / float64(p.Requested))
		p.Effect = &PairedEffect{Delta: delta, Low: math.Max(-1, delta-half), High: math.Min(1, delta+half)}
	}
	return p
}

func short(fp string) string { return Fingerprint(fp).Short() }
