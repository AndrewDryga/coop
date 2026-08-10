package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/loopcfg"
	"github.com/AndrewDryga/coop/internal/tasks"
)

func shouldRunBetweenAudit(iterationSucceeded, auditAvailable, protected bool) bool {
	return protected || (iterationSucceeded && auditAvailable)
}

// doneTaskDirs maps every done task's id → its folder across the queue(s). The between audit
// diffs a before/after snapshot of it to name exactly which task(s) an iteration finished.
func doneTaskDirs(hosts []string) map[string]string {
	out := map[string]string{}
	for _, h := range hosts {
		for _, t := range tasks.ReadTaskTree(h) {
			if t.State == tasks.StateDone {
				out[t.ID] = t.Dir
			}
		}
	}
	return out
}

// completedReviewSubjects returns only tasks this controller accepted during the current run and
// that are still archived. Commit trailers describe their changes but never grant review authority.
func completedReviewSubjects(hosts []string, completed map[string]bool) []string {
	states := tasks.QueueSnapshot(hosts)
	var ids []string
	for id := range completed {
		if states[id] == tasks.StateDone {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// newlyFinished returns "id — dir" lines (sorted by id) for tasks done now but not before —
// what the last iteration completed, and so what the between audit is about.
func newlyFinished(before, now map[string]string) []string {
	var out []string
	for id, dir := range now {
		if _, ok := before[id]; !ok {
			out = append(out, id+" — "+dir)
		}
	}
	slices.Sort(out)
	return out
}

// reviewBaselineAfterVerdict advances the signoff baseline past a receipt-consistent round without
// rescanning done/. A completion landing during the audit-to-re-anchor handoff must remain outside
// the baseline so the next subject diff reviews it instead of silently absorbing it.
func reviewBaselineAfterVerdict(prior map[string]string, subjects, reopened, concurrent []string) map[string]string {
	baseline := make(map[string]string, len(prior)+len(subjects))
	for id, dir := range prior {
		baseline[id] = dir
	}
	for _, subject := range subjects {
		id, dir, _ := strings.Cut(subject, " — ")
		baseline[id] = dir
	}
	for _, id := range reopened {
		delete(baseline, id)
	}
	for _, id := range concurrent {
		delete(baseline, id)
	}
	return baseline
}

// taskIDsOf strips the " — dir" suffix off newlyFinished lines — the bare ids, for the banner.
func taskIDsOf(finished []string) []string {
	out := make([]string, len(finished))
	for i, f := range finished {
		out[i], _, _ = strings.Cut(f, " — ")
	}
	return out
}

// defaultSignoffRounds is the built-in work→signoff round ceiling when .agent/loop.yaml
// signoff.rounds is unset.
const defaultSignoffRounds = 5

// signoffRounds is the work→signoff round ceiling: .agent/loop.yaml signoff.rounds when set (>0),
// else the built-in default of 5. signoffRoundCap scales it by the batch.
func signoffRounds(lc *loopcfg.Config) int {
	if lc.Signoff.Rounds > 0 {
		return lc.Signoff.Rounds
	}
	return defaultSignoffRounds
}

// blockReopenedTasks parks the exact tasks reopened by the capped signoff round into 50_blocked/
// with a decision.md; unrelated actionable work is left untouched, and the capped loop exits 3
// (blocked on a human) instead of spinning or claiming a false "done".
// The loop runs on the host, where coop's own task helpers are available, so it moves the folders
// directly. Best-effort: a move/write failure is surfaced and skipped, never fatal — the closing
// banner still reports the honest count.
func blockReopenedTasks(hosts, reopened []string, rounds int) error {
	moves := make([]tasks.TrustedTaskMove, 0, len(reopened))
	for _, id := range reopened {
		task, err := lifecycleTaskSubject(hosts, id)
		if err != nil {
			return fmt.Errorf("capped signoff task %s %w", id, err)
		}
		title := task.Item.Title
		moves = append(moves, tasks.TrustedTaskMove{
			Root: task.Root, Task: tasks.Item{ID: id}, NewState: tasks.StateBlocked,
			SourceStates:  []string{tasks.StateTodo, tasks.StateInProgress, tasks.StateDone, tasks.StateBlocked},
			MetadataNames: []string{"decision.md"},
			AfterMove: func(dir string) error {
				return writeReviewBlockDecision(filepath.Join(dir, "decision.md"), id, title, rounds)
			},
		})
	}
	if err := tasks.MoveTrustedTasksFromDoneWith(moves); err != nil {
		return fmt.Errorf("authoritatively block capped signoff tasks: %w", err)
	}
	return nil
}

// writeReviewBlockDecision drops a decision.md explaining that the review kept reopening this task
// past the round cap, so a human knows why it's parked — unless one already exists (don't clobber a
// prior note). Best-effort; mirrors the `coop tasks block` stub shape.
func writeReviewBlockDecision(path, id, title string, rounds int) error {
	if fileExists(path) {
		return nil
	}
	body := fmt.Sprintf("# Decision: the review keeps reopening %q after %d rounds\n\n"+
		"**Blocks:** this task (`%s`).\n\n"+
		"**The decision:** The unattended loop drained the queue and the signoff pass reopened this "+
		"task %d times without it converging — the work loop can't get it to a state the review "+
		"accepts. A human needs to look at why (a gate it can't make green, a spec gap, a flaky test) "+
		"before it goes back in the queue.\n\n"+
		"**Recommendation:** Read the review's reopen notes in this task's log.md, fix the underlying "+
		"issue (or split/redefine the task), then `coop tasks unblock %s`.\n\n"+
		"---\n\n"+
		"**Resolution:** <!-- HUMAN: your answer here, then: coop tasks unblock %s -->\n",
		title, rounds, id, rounds, id, id)
	return os.WriteFile(path, []byte(body), 0o644)
}
