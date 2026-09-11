package cli

import (
	"fmt"
	"strings"

	"github.com/AndrewDryga/coop/internal/ui"
)

// The doctor's report. `coop doctor` is the one command whose whole point IS the ledger: the
// person asked coop to perform checks, so every check it performed is printed, passed or not.
// (A passive inspection elsewhere shows facts, not a list of successful checks.)
//
// The counting rules exist because the easy version lies. A check coop never ran is not a pass;
// a probe that died is one failure plus however many checks it was carrying, not a clean sheet;
// and a limit this runtime does not apply reads differently from one somebody switched off. Each
// of those is a separate outcome below, and the verdict names each one it actually saw.

type doctorOutcome int

const (
	doctorPass       doctorOutcome = iota // the check ran and held
	doctorFail                            // the check ran and did not hold
	doctorSkip                            // deliberately not checked here (fallback image, unreadable value)
	doctorNotApplied                      // this runtime does not apply the limit at all
	doctorDisabled                        // switched off by configuration
	doctorProbeFail                       // the probe itself failed; the checks it carried never ran
	doctorUnrun                           // checks a failed probe never reached
	doctorNote                            // host hygiene, outside the isolation tally entirely
)

// doctorRow is one line of the report. covers is how many ordinary checks the line accounts for:
// one for a real check, and for a probe failure the number of checks that probe was carrying — so
// the totals add up to the same 35 whatever went wrong.
type doctorRow struct {
	outcome doctorOutcome
	label   string
	reason  string
	action  string
	details []string // evidence listed under the label, e.g. the ids of the boxes found
	footer  string   // one closing sentence for the row, at section indent
	covers  int
}

type doctorSection struct {
	title string
	rows  []doctorRow
}

// doctorReport collects the sections in print order and answers the verdict at the end.
type doctorReport struct {
	sections []*doctorSection
}

func (r *doctorReport) section(title string) *doctorSection {
	s := &doctorSection{title: title}
	r.sections = append(r.sections, s)
	return s
}

func (s *doctorSection) add(row doctorRow) {
	if row.covers == 0 {
		row.covers = 1
	}
	s.rows = append(s.rows, row)
}

func (s *doctorSection) pass(label string) { s.add(doctorRow{outcome: doctorPass, label: label}) }
func (s *doctorSection) fail(label, reason, action string) {
	s.add(doctorRow{outcome: doctorFail, label: label, reason: reason, action: action})
}
func (s *doctorSection) skip(label, reason string, covers int) {
	s.add(doctorRow{outcome: doctorSkip, label: label, reason: reason, covers: covers})
}

// probeFailed records a probe that never produced its checks: one failure, plus the checks it was
// carrying counted as uncompleted rather than quietly dropped from the total.
func (s *doctorSection) probeFailed(label, reason string, covers int) {
	s.add(doctorRow{outcome: doctorProbeFail, label: label, reason: reason, covers: covers})
}

// unrun records checks that a failure elsewhere prevented — they were neither performed nor
// deliberately skipped.
func (s *doctorSection) unrun(label string, covers int) {
	s.add(doctorRow{outcome: doctorUnrun, label: label, covers: covers})
}

// doctorSections opens the six sections in print order. cmdDoctor and the approved-report
// fixtures both go through it, so a check can never land in one section for a real run and a
// different one for the transcript that is supposed to prove the run.
func (r *doctorReport) doctorSections() (secrets, host, offline, tasks, credentials, clone *doctorSection) {
	return r.section(sectionSecrets), r.section(sectionHost), r.section(sectionOffline),
		r.section(sectionTasks), r.section(sectionCredentials), r.section(sectionClone)
}

// tally is the report's arithmetic, in the categories the verdict distinguishes.
type doctorTally struct {
	passed, failed, probeFailed, uncompleted, notChecked, notApplied, disabled int
}

func (r *doctorReport) tally() doctorTally {
	var t doctorTally
	for _, s := range r.sections {
		for _, row := range s.rows {
			switch row.outcome {
			case doctorPass:
				t.passed++
			case doctorFail:
				t.failed++
			case doctorSkip:
				t.notChecked += row.covers
			case doctorNotApplied:
				t.notApplied += row.covers
			case doctorDisabled:
				t.disabled += row.covers
			case doctorProbeFail:
				t.probeFailed++
				t.uncompleted += row.covers
			case doctorUnrun:
				t.uncompleted += row.covers
			case doctorNote:
				// Deliberately uncounted: an abandoned container is host hygiene, not a hole in
				// the isolation doctor attacks, and must not move the verdict either way.
			}
		}
	}
	return t
}

// print renders every section, then the verdict, and returns the exit code. A run whose checks
// all passed is 0; a fallback run that could not perform some of them is also 0 (it proved what
// it could, as today) but never claims full isolation; anything that actually failed is 1.
func (r *doctorReport) print() int {
	for _, s := range r.sections {
		if len(s.rows) == 0 {
			continue
		}
		ui.Section(s.title)
		for _, row := range s.rows {
			switch row.outcome {
			case doctorPass:
				ui.Pass("%s", row.label)
			case doctorFail:
				ui.Fail(row.label, row.reason, row.action)
			default:
				if row.outcome == doctorProbeFail {
					ui.Fail(row.label, row.reason, row.action)
					continue
				}
				ui.Caution("%s", row.label)
				for _, line := range row.details {
					ui.Note("    %s", line)
				}
				if row.reason != "" {
					ui.Note("")
					for _, line := range strings.Split(row.reason, "\n") {
						ui.Note("        %s", line)
					}
				}
				if row.footer != "" {
					ui.Note("")
					ui.Note("  %s", row.footer)
				}
			}
		}
	}
	t := r.tally()
	ui.Note("")
	if t.failed == 0 && t.probeFailed == 0 && t.notChecked == 0 && t.notApplied == 0 && t.disabled == 0 && t.uncompleted == 0 {
		ui.OK("All %d checks passed", t.passed)
		return 0
	}
	clauses := []string{fmt.Sprintf("%d checks passed", t.passed)}
	if t.failed > 0 {
		clauses = append(clauses, fmt.Sprintf("%d failed", t.failed))
	}
	if t.probeFailed > 0 {
		clauses = append(clauses, ui.Count(t.probeFailed, "probe")+" failed")
	}
	if t.uncompleted > 0 {
		clauses = append(clauses, fmt.Sprintf("%s could not be completed", ui.Count(t.uncompleted, "check")))
	}
	if t.notChecked > 0 {
		clauses = append(clauses, fmt.Sprintf("%d could not be checked", t.notChecked))
	}
	if t.notApplied > 0 {
		clauses = append(clauses, ui.Count(t.notApplied, "limit is", "limits are")+" not applied by this runtime")
	}
	if t.disabled > 0 {
		clauses = append(clauses, fmt.Sprintf("%s disabled", ui.Count(t.disabled, "limit is", "limits are")))
	}
	verdict := strings.Join(clauses, "; ")
	if t.failed > 0 || t.probeFailed > 0 {
		ui.Note("%s %s", ui.Red("✗"), verdict)
		return 1
	}
	ui.Note("%s %s", ui.Yellow("⚠"), verdict)
	return 0
}
