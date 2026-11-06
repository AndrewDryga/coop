package tasks

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/ui"
)

// A completed task's state.md and log.md are what the next agent, the final review and a person
// read, so they must hold facts, not Coop's scaffold. One night's final review reopened 7 of 12
// completed tasks only because log.md held nothing but the template header, and completed state.md
// files still read the template's <…> Traps text.

// StateTemplateFields names the state.md fields a completed task keeps that still hold one of the
// template's <…> placeholders. Completion rewrites Status and Next action itself, so only Done so far
// and Traps can carry one into the archive. "—" says there is nothing to report; it is not one.
func StateTemplateFields(fields TaskStateFields) []string {
	var labels []string
	if isPlaceholder(fields.DoneSoFar) {
		labels = append(labels, "Done so far")
	}
	if isPlaceholder(fields.Traps) {
		labels = append(labels, "Traps")
	}
	return labels
}

// StateTemplateText names the fields StateTemplateFields found, in the words every check that
// refuses or flags them uses: coop tasks done, a box's tasks_complete and coop tasks lint.
func StateTemplateText(labels []string) string {
	verb := "holds"
	if len(labels) > 1 {
		verb = "hold"
	}
	return "state.md's " + joinWithAnd(labels) + " still " + verb + " the template's <…> text"
}

// stateTemplateFix is what a person writes in such a field instead. The dash is spelled out: a
// bare "—" in a hint reads as punctuation, not as the thing to type.
const stateTemplateFix = "what the next reader needs there, or a dash (—) when there is nothing"

// stateTemplateRefusal is why `coop tasks done` will not complete t: its state.md still holds
// template text in a field completion keeps, which a box's tasks_complete refuses too. retry is the
// command to run once it is written. Completion rewrites a state.md it cannot read, so only a
// readable one is checked.
func stateTemplateRefusal(t Item, retry string) error {
	fields, err := ReadTaskState(t.Dir)
	if err != nil {
		return nil
	}
	labels := StateTemplateFields(fields)
	if len(labels) == 0 {
		return nil
	}
	return ui.CommandFailed("Could not complete "+ui.SafeInline(t.ID), StateTemplateText(labels)+".\nWrite "+stateTemplateFix+".",
		[2]string{"Edit:", ui.SafeInline(displayPath(filepath.Join(t.Dir, "state.md")))},
		[2]string{"Then run:", retry})
}

// coopLogNotes open the notes Coop appends to log.md itself (AppendTaskLogStrict callers), and
// coopLogNoteLines are the whole ones. They record what happened to a task — a claim, a block, a
// review verdict, a refused completion — not what its worker changed or why, so none is an entry.
var (
	coopLogNotes = []string{
		"Claimed by ", "Unblocked: ", "Blocked: ", parkedBy, "review: fail",
		"completion rejected: ", "implementation completion rejected: ",
		"provider exited while an agent-owned background job", "the host watchdog killed this provider attempt",
		"preflight: ", "host parked this task", "host preflight parked this task",
		"Previous decision before completion repair was exhausted:", "Closed with coop tasks done --force",
		"Completed with coop tasks done --from-fork",
		"a commit in task ", "loop stopped ",
	}
	coopLogNoteLines = []string{"Unblocked", "Blocked for a decision in decision.md"}
	// coopLogHeadings head the entries Coop writes under a heading: a decision a new question
	// replaced, quoted below it.
	coopLogHeadings = []string{"## Unanswered question, replaced by a new one", "## Answered decision, replaced by a new question"}
)

func coopLogNote(text string) bool {
	if slices.Contains(coopLogNoteLines, text) {
		return true
	}
	return slices.ContainsFunc(coopLogNotes, func(prefix string) bool { return strings.HasPrefix(text, prefix) })
}

// LogHasEntry reports whether a log.md holds an entry past Coop's template (its HTML comment and
// title) and the notes and replaced decisions Coop writes itself. A no-change closure's
// "## Closure" entry counts: it carries the worker's own reason and evidence.
func LogHasEntry(log string) bool {
	quoting, note := false, false
	for _, line := range strings.Split(withoutHTMLComments(log), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			note = false // a note ends at a blank line; a quoted decision can hold some
			continue
		}
		if quoting && strings.HasPrefix(line, ">") {
			continue
		}
		quoting = false
		switch {
		case strings.HasPrefix(line, "# "): // the title
		case slices.Contains(coopLogHeadings, line):
			quoting = true
		case strings.HasPrefix(line, "- ") && coopLogNote(line[2:]):
			note = true
		case note && !strings.HasPrefix(line, "- ") && !strings.HasPrefix(line, "#"):
			// the rest of a note Coop wrote over several lines
		default:
			return true
		}
	}
	return false
}

// TaskLogHasEntry reads taskDir's log.md under the bounds every task file gets and reports whether
// it holds an entry. A missing log.md holds none.
func TaskLogHasEntry(taskDir string) (bool, error) {
	body, _, err := readOptionalTaskMetadataPath(filepath.Join(taskDir, "log.md"))
	if err != nil {
		return false, err
	}
	return LogHasEntry(string(body)), nil
}

// withoutHTMLComments drops each complete <!-- … --> span. An unterminated one is kept, so stray
// markup can never hide the entries written after it.
func withoutHTMLComments(s string) string {
	var b strings.Builder
	for {
		start := strings.Index(s, "<!--")
		if start < 0 {
			break
		}
		end := strings.Index(s[start+len("<!--"):], "-->")
		if end < 0 {
			break
		}
		b.WriteString(s[:start])
		s = s[start+len("<!--")+end+len("-->"):]
	}
	b.WriteString(s)
	return b.String()
}
