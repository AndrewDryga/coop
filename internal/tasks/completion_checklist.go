package tasks

import (
	"cmp"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrIncompleteChecklist distinguishes unfinished work from a failed metadata
// write or cleanup. A checked item is still not independent verification evidence.
var ErrIncompleteChecklist = errors.New("task checklist is unfinished")

// A refusal names at most maxShownOpen open items, each cut to maxShownLabel runes: task.md is
// agent-authored, so the message stays readable however long the checklist grows.
const (
	maxShownOpen  = 5
	maxShownLabel = 120
)

// IncompleteChecklistError refuses completion of a task whose checklist is empty or still open.
// It matches ErrIncompleteChecklist and names the open items, so every refusal (the host CLI, the
// in-box task tools and the loop's safe stop) says what is left, not only how much.
type IncompleteChecklistError struct {
	ID          string
	Done, Total int
	Open        []string // the open items' labels in task order; empty when they are unknown
}

func (e *IncompleteChecklistError) Is(target error) bool { return target == ErrIncompleteChecklist }

func (e *IncompleteChecklistError) Error() string {
	msg := fmt.Sprintf("%v: %s has %d/%d subtasks done", ErrIncompleteChecklist, e.ID, e.Done, e.Total)
	if e.Total == 0 {
		return msg + "; record a nonempty, truthful checklist before completing"
	}
	if open := e.openSummary(); open != "" {
		msg += "; open: " + open
	}
	return msg + "; finish the remaining work and required checks, and keep a check open if it failed, could not run or has not run"
}

// openSummary lists the open items on one line, quoted and capped: `"Run the gate" and 2 more`.
// It is empty when the labels are unknown.
func (e *IncompleteChecklistError) openSummary() string {
	shown, more := e.shownOpen()
	quoted := make([]string, len(shown))
	for i, label := range shown {
		quoted[i] = strconv.Quote(label)
	}
	summary := strings.Join(quoted, ", ")
	if summary != "" && more > 0 {
		summary += fmt.Sprintf(" and %d more", more)
	}
	return summary
}

// shownOpen returns the open items a refusal names, shortened, and how many more it leaves out.
func (e *IncompleteChecklistError) shownOpen() (shown []string, more int) {
	for i, label := range e.Open {
		if i == maxShownOpen {
			return shown, len(e.Open) - maxShownOpen
		}
		if label == "" {
			label = "(unlabeled)"
		}
		shown = append(shown, truncate(label, maxShownLabel))
	}
	return shown, 0
}

// RequireCompletedChecklist enforces the structural completion contract using
// the same parsed checkboxes as task progress, including its code-fence rules.
func RequireCompletedChecklist(task Item) error {
	done, total := task.doneSubtasks(), len(task.Subtasks)
	if total > 0 && done == total {
		return nil
	}
	return &IncompleteChecklistError{ID: task.ID, Done: done, Total: total, Open: task.openSubtaskLabels()}
}

func requireCurrentCompletedChecklist(taskDir string) error {
	task, ok, err := parseTaskFolder(taskDir, StateDone)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("completion needs a readable task.md in %s", taskDir)
	}
	return RequireCompletedChecklist(task)
}

// forcedCompletionNote is the log line a person's `coop tasks done --force` leaves, so the archive
// never reads as verified: the override, who ran it when Coop knows the process, and what was
// still open. The checklist itself stays as the person left it.
func forcedCompletionNote(e *IncompleteChecklistError, actor ClaimActor) string {
	note := "Closed with coop tasks done --force"
	if actor.PID != 0 {
		note += fmt.Sprintf(" by %s (PID %d)", cmp.Or(actor.Label, "a process"), actor.PID)
	}
	if e.Total == 0 {
		return note + " without a checklist."
	}
	open, verb := e.Total-e.Done, "were"
	if open == 1 {
		verb = "was"
	}
	note += fmt.Sprintf(" while %d of %d checklist items %s open", open, e.Total, verb)
	if summary := e.openSummary(); summary != "" {
		note += ": " + summary
	}
	return note + "."
}
