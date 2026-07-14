package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/ui"
)

// The Coop-Task trailer binds a commit to the task it completes. The agent writes it (loopWorkPrompt
// instructs it); the HOST controller reads it to verify attempts, resume informed after a crash, and
// reconcile the parent queue after a fork merge — the LLM still moves folders, the controller only
// supplies evidence and repairs drift. Before this, nothing linked a commit to a task
// (git log --grep <id> was 0 repo-wide), so "one task = one commit" was unobservable and a crash
// between commit and folder-move was ambiguous.
const coopTaskTrailer = "Coop-Task"

// commitsForTask returns the short shas whose Coop-Task trailer equals id. rangeExpr limits the
// search (e.g. "base..HEAD"); empty scans all of HEAD's reachable history. The trailer value is a
// single-line id, so a tab-split off %h is robust.
func commitsForTask(repo, rangeExpr, id string) []string {
	args := []string{"log", "--format=%h%x09%(trailers:key=" + coopTaskTrailer + ",valueonly)"}
	if rangeExpr != "" {
		args = append(args, rangeExpr)
	}
	var shas []string
	for _, line := range strings.Split(gitOut(repo, args...), "\n") {
		sha, val, ok := strings.Cut(line, "\t")
		if ok && strings.TrimSpace(val) == id {
			shas = append(shas, sha)
		}
	}
	return shas
}

// gateGuardGlobs name the files that DEFINE what "green" means — the candidate's own verifier: the
// Makefile/gate, the loop + project config, the hooks, CI. A task that edits these could weaken the
// gate to pass itself (cross-vendor review is no defense when every reviewer trusts the same mutable
// oracle). A trailing "/" matches a directory prefix; else an exact base name.
var gateGuardGlobs = []string{
	"Makefile", "makefile", "GNUmakefile",
	".agent/project.yaml", ".agent/loop.yaml",
	".agent/skills/sweep/",
	".claude/hooks/", ".claude/settings.json", ".claude/settings.local.json",
	".github/workflows/",
}

// isGateGuardPath reports whether a repo-relative path is gate-defining (in gateGuardGlobs).
func isGateGuardPath(f string) bool {
	for _, g := range gateGuardGlobs {
		if strings.HasSuffix(g, "/") {
			if strings.HasPrefix(f, g) {
				return true
			}
		} else if f == g || strings.HasSuffix(f, "/"+g) {
			return true
		}
	}
	return false
}

// protectedGateChanges returns the gate-defining files a commit range (base..head) touched — the
// boring first step of the verifier trust boundary: detect (host-side, deterministic) when a task
// edited its own checker, so the review can be told to scrutinize it rather than trust it blind.
// Empty when the range is empty or touched none.
func protectedGateChanges(repo, base, head string) []string {
	if base == "" || head == "" || base == head {
		return nil
	}
	var hits []string
	for _, f := range strings.Split(gitOut(repo, "diff", "--name-only", base+".."+head), "\n") {
		if f = strings.TrimSpace(f); f != "" && isGateGuardPath(f) {
			hits = append(hits, f)
		}
	}
	return hits
}

// queueSnapshot maps task id → state across the hosts, for diffing what an iteration moved.
func queueSnapshot(hosts []string) map[string]string {
	m := map[string]string{}
	for _, h := range hosts {
		for _, t := range readTaskTree(h) {
			m[t.ID] = t.State
		}
	}
	return m
}

// finishedTasks returns the ids that moved INTO done/ between two snapshots — what an iteration
// completed. Pure, sorted for stable output.
func finishedTasks(before, after map[string]string) []string {
	var ids []string
	for id, st := range after {
		if st == stateDone && before[id] != stateDone {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// blockedTaskIDs returns the ids currently parked in 50_blocked/ across the hosts — what needs a
// human decision, for the closing digest. Sorted.
func blockedTaskIDs(hosts []string) []string {
	var ids []string
	for id, st := range queueSnapshot(hosts) {
		if st == stateBlocked {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// reopenedBySignoff returns the ids the signoff bounced OUT of done/ (back to todo or in_progress)
// between two snapshots — what the review reopened this round, for the health digest. Sorted.
func reopenedBySignoff(before, after map[string]string) []string {
	var ids []string
	for id, st := range after {
		if before[id] == stateDone && st != stateDone {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// untrailered returns the finished ids with NO Coop-Task commit in the iteration's range — a
// completion the agent didn't tag, so the host can't bind it to a commit. An empty range (no HEAD
// movement, e.g. a folder move with no commit) leaves every finished id untrailered.
func untrailered(repo, base, head string, finished []string) []string {
	rng := ""
	if base != "" && head != "" && base != head {
		rng = base + ".." + head
	}
	var missing []string
	for _, id := range finished {
		if rng == "" || len(commitsForTask(repo, rng, id)) == 0 {
			missing = append(missing, id)
		}
	}
	return missing
}

// resumeLine is the informed-resume hint for an in_progress task that ALREADY has a commit carrying
// its Coop-Task trailer in history. Empty when there's none (a genuinely mid-work task — the
// blind-resume path stays byte-identical). It names the fact but doesn't assume the case, because a
// landed trailer means EITHER a crash after commit before the folder-move OR a review reopen for
// rework — so it tells the agent to disambiguate from the task's own log.md/state.md.
func resumeLine(id string, commits []string) string {
	if len(commits) == 0 {
		return ""
	}
	return "Task " + id + " has commit(s) " + strings.Join(commits, ", ") + " already in history carrying " +
		"its Coop-Task trailer. Read its log.md/state.md and determine which case applies: (a) a prior " +
		"attempt COMMITTED then was interrupted before moving the folder to 99_done/ — verify that work " +
		"against the acceptance criteria and finish the move, do NOT redo it; or (b) the review REOPENED it " +
		"for rework (its log.md will say what's wrong) — do that rework. Disambiguate before acting."
}

// resumePrefixFor builds the informed-resume preamble for the assigned task when its Coop-Task
// trailer is already in history. Empty when none, so a fresh claim keeps the ordinary prompt.
func (a *app) resumePrefixFor(repo, id string) string {
	return resumeLine(id, commitsForTask(repo, "", id))
}

// assignLoopTask selects the authoritative next task and claims todo work before a box starts.
// Already in-progress work is a resume and needs no move.
func assignLoopTask(hosts []string) (taskCounts, queuedTask, bool, error) {
	c, selected, ok := queueState(hosts)
	if !ok {
		return c, queuedTask{}, false, nil
	}
	if selected.Item.State != stateTodo {
		return c, selected, true, nil
	}
	if err := moveTaskDir(selected.Root, selected.Item, stateInProgress); err != nil {
		return c, queuedTask{}, false, fmt.Errorf("claim task %s: %w", selected.Item.ID, err)
	}
	selected.Item.State = stateInProgress
	selected.Item.Dir = filepath.Join(selected.Root, stateInProgress, selected.Item.ID)
	c.Todo--
	c.Doing++
	return c, selected, true, nil
}

// reconcileAction is what post-merge reconciliation should do with one parent-queue task after a
// fork landed: move a trailer-landed todo/in_progress task to done, or FLAG (never auto-move) a
// blocked one.
type reconcileAction struct {
	ID   string
	Move bool // true → move to done/; false → flag for a human (blocked/ tasks)
}

// reconcileMerged decides, for each parent-queue task whose Coop-Task trailer now appears in
// parent history (landed by the merge), what to do: a todo/ or in_progress/ task is reconciled to
// done/ (redoing landed work is the worse failure — it already passed the fork's own review and the
// merge gate); a blocked/ task is only flagged, never moved, since a human parked it. Pure: it maps
// (task states, the set of landed ids) to actions.
func reconcileMerged(states map[string]string, landed map[string]bool) []reconcileAction {
	var acts []reconcileAction
	for id, st := range states {
		if !landed[id] {
			continue
		}
		switch st {
		case stateTodo, stateInProgress:
			acts = append(acts, reconcileAction{ID: id, Move: true})
		case stateBlocked:
			acts = append(acts, reconcileAction{ID: id, Move: false})
		}
	}
	slices.SortFunc(acts, func(a, b reconcileAction) int { return strings.Compare(a.ID, b.ID) })
	return acts
}

// landedTasks is the set of task ids whose Coop-Task trailer appears anywhere in repo's history.
func landedTasks(repo string) map[string]bool {
	set := map[string]bool{}
	for _, line := range strings.Split(gitOut(repo, "log", "--format=%(trailers:key="+coopTaskTrailer+",valueonly)"), "\n") {
		if v := strings.TrimSpace(line); v != "" {
			set[v] = true
		}
	}
	return set
}

// reconcileQueueAfterMerge moves any parent-queue task whose Coop-Task trailer now sits in parent
// history (landed by the just-merged fork) from todo/ or in_progress/ to done/, with a reconcile
// note; a blocked task with a landed trailer is flagged for a human, never moved. Best-effort — the
// merge already succeeded, so a reconcile hiccup must not fail it. Prevents the parent loop from
// redoing work a fork already landed.
func (a *app) reconcileQueueAfterMerge(repo, forkName string) {
	queues, err := taskQueues(a.cfg, repo, nil)
	if err != nil {
		return
	}
	landed := landedTasks(repo)
	for _, q := range queues {
		host := filepath.Join(repo, q)
		states := map[string]string{}
		items := map[string]taskItem{}
		for _, t := range readTaskTree(host) {
			states[t.ID] = t.State
			items[t.ID] = t
		}
		for _, act := range reconcileMerged(states, landed) {
			if !act.Move {
				ui.Warn("task %s is blocked but its work landed via fork %s — a human should reconcile it", act.ID, forkName)
				continue
			}
			if err := moveTaskDir(host, items[act.ID], stateDone); err != nil {
				ui.Warn("reconcile: could not move %s to done: %v", act.ID, err)
				continue
			}
			appendTaskLog(filepath.Join(host, stateDone, act.ID), "reconciled: landed by fork "+forkName)
		}
	}
}

// unblockResolved is the loop's built-in preflight, run host-side (no box, no model): every
// blocked task whose decision.md now carries a filled-in Resolution — the same bar
// `coop tasks unblock` applies (decisionResolved) — moves back to 00_todo/ with a log note.
// A task with no decision.md, or one whose format decisionResolved can't read, stays parked:
// never act on a file we can't parse confidently. Best-effort; a move failure warns and skips.
// Returns the unblocked ids in readTaskTree order.
func unblockResolved(hosts []string) []string {
	var ids []string
	for _, host := range hosts {
		for _, t := range readTaskTree(host) {
			if t.State != stateBlocked || !decisionResolved(filepath.Join(t.Dir, "decision.md")) {
				continue
			}
			if err := moveTaskDir(host, t, stateTodo); err != nil {
				ui.Warn("pre-flight: could not unblock %s: %v", t.ID, err)
				continue
			}
			appendTaskLog(filepath.Join(host, stateTodo, t.ID), "preflight: resolution filled in — unblocked")
			ids = append(ids, t.ID)
		}
	}
	return ids
}

// appendTaskLog appends a one-line note to a task folder's log.md, best-effort.
func appendTaskLog(taskDir, note string) {
	f, err := os.OpenFile(filepath.Join(taskDir, "log.md"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("\n- " + note + "\n")
}
