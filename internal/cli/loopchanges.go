package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/AndrewDryga/coop/internal/ui"
)

// This file gives the loop's review stages (and the human closing digest) a structured picture of
// what a run changed and how it went — so a signoff/verify prompt like "e2e the affected features"
// resolves against a concrete, per-task list instead of guessing. It leans on the Coop-Task trailer
// (which binds each commit to the task it completed) and the per-iteration health the loop already
// records (reopens, retries, gate-file edits).

// commitInfo is one commit in a loop's range: its short sha and subject line.
type commitInfo struct{ sha, subject string }

// taskChanges is what ONE task (identified by its Coop-Task trailer) changed this loop.
type taskChanges struct {
	id      string
	commits []commitInfo
	files   []string
}

// loopChangeSet is everything a loop changed in a base..head range, grouped by task — the "what
// happened" context. `misc` holds commits with no trailer (a preflight tidy, a manual fixup).
type loopChangeSet struct {
	tasks      []taskChanges // grouped by Coop-Task trailer, in first-seen (chronological) order
	misc       []commitInfo  // untrailered commits
	subsystems []string      // distinct top-level areas of every changed file (internal/box, site, …)
	stat       string        // `git diff --stat base..head` — the aggregate
}

func (cs loopChangeSet) empty() bool { return len(cs.tasks) == 0 && len(cs.misc) == 0 }

// taskHealth is how ONE task's work went during the run — the signals the loop already surfaces
// as warnings, accumulated so the reviewer's attention goes to the shaky work.
type taskHealth struct {
	reopens   int      // times the signoff reopened it before it passed
	gateFiles []string // gate-defining files it edited (a task weakening its own checker)
	retries   int      // work-iteration retries spent on it
	untagged  bool     // it finished with no Coop-Task commit (unbindable)
}

// loopHealth accumulates per-task health across a run; the zero value is a clean run.
type loopHealth struct {
	byTask map[string]*taskHealth
}

func newLoopHealth() *loopHealth { return &loopHealth{byTask: map[string]*taskHealth{}} }

func (h *loopHealth) at(id string) *taskHealth {
	if h.byTask[id] == nil {
		h.byTask[id] = &taskHealth{}
	}
	return h.byTask[id]
}

// noteReopen records that the signoff reopened the given task ids this round.
func (h *loopHealth) noteReopen(ids []string) {
	for _, id := range ids {
		h.at(id).reopens++
	}
}

// noteIteration folds one work iteration's warnings (the gate-file edits and untagged finishes it
// already logged) into the tasks it finished.
func (h *loopHealth) noteIteration(finished, gateFiles, untagged []string) {
	for _, id := range finished {
		th := h.at(id)
		th.gateFiles = append(th.gateFiles, gateFiles...)
		th.retries++
	}
	for _, id := range untagged {
		h.at(id).untagged = true
	}
}

// shaky reports whether a task's run had any risk signal worth the reviewer's extra scrutiny.
func (t taskHealth) shaky() bool {
	return t.reopens > 0 || len(t.gateFiles) > 0 || t.untagged
}

// parseLoopCommits groups `git log --format=<sha>\t<subject>\t<task-id>` output by trailer id
// (first-seen order) plus the untrailered commits. Pure — tested on fixed output.
func parseLoopCommits(logOut string) (order []string, byTask map[string][]commitInfo, misc []commitInfo) {
	byTask = map[string][]commitInfo{}
	for _, line := range strings.Split(strings.TrimSpace(logOut), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 3)
		if len(f) < 2 {
			continue
		}
		ci := commitInfo{sha: f[0], subject: f[1]}
		id := ""
		if len(f) == 3 {
			id = strings.TrimSpace(f[2])
		}
		if id == "" {
			misc = append(misc, ci)
			continue
		}
		if _, seen := byTask[id]; !seen {
			order = append(order, id)
		}
		byTask[id] = append(byTask[id], ci)
	}
	return order, byTask, misc
}

// subsystemsOf reduces changed files to their distinct top-level areas — two segments for a nested
// source layout (internal/box), else the first segment (site, docs), else "(root)" for a top-level
// file. Pure, sorted, deduped.
func subsystemsOf(files []string) []string {
	seen := map[string]bool{}
	for _, f := range files {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		parts := strings.Split(f, "/")
		switch {
		case len(parts) == 1:
			seen["(root)"] = true
		case parts[0] == "internal" || parts[0] == "cmd" || parts[0] == "pkg":
			seen[parts[0]+"/"+parts[1]] = true
		default:
			seen[parts[0]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// loopChanges computes what changed in base..head, grouped by the Coop-Task trailer. Empty set when
// the range is empty. The git calls live here; the parsing (parseLoopCommits/subsystemsOf) is pure.
func loopChanges(repo, base, head string) loopChangeSet {
	if base == "" || head == "" || base == head {
		return loopChangeSet{}
	}
	rng := base + ".." + head
	logOut := gitOut(repo, "log", "--reverse", "--format=%h%x09%s%x09%(trailers:key="+coopTaskTrailer+",valueonly)", rng)
	order, byTask, misc := parseLoopCommits(logOut)
	cs := loopChangeSet{
		misc:       misc,
		subsystems: subsystemsOf(rangeFiles(repo, rng)),
		stat:       strings.TrimSpace(gitOut(repo, "diff", "--stat", rng)),
	}
	for _, id := range order {
		commits := byTask[id]
		cs.tasks = append(cs.tasks, taskChanges{id: id, commits: commits, files: commitFiles(repo, commits)})
	}
	return cs
}

// rangeFiles lists every file changed across a commit range.
func rangeFiles(repo, rng string) []string {
	return splitLines(gitOut(repo, "diff", "--name-only", rng))
}

// commitFiles is the union of files a set of commits touched (sorted, deduped) — each commit's tree
// diff against its parent, so a merge-free loop range attributes files to the task that changed them.
func commitFiles(repo string, commits []commitInfo) []string {
	seen := map[string]bool{}
	for _, c := range commits {
		for _, f := range splitLines(gitOut(repo, "diff-tree", "--no-commit-id", "--name-only", "-r", c.sha)) {
			seen[f] = true
		}
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// abbrev caps a list for a prompt/digest line — the first n, then "+k more".
func abbrev(xs []string, n int) string {
	if len(xs) <= n {
		return strings.Join(xs, ", ")
	}
	return strings.Join(xs[:n], ", ") + fmt.Sprintf(", +%d more", len(xs)-n)
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func (cs loopChangeSet) taskIDs() []string {
	ids := make([]string, len(cs.tasks))
	for i, t := range cs.tasks {
		ids[i] = t.id
	}
	return ids
}

// reviewBlock renders the loop's changes + health as a prompt section for the signoff/verify
// reviewer: one entry per completed task (its commits, the files it touched, and any risk signal the
// run flagged), the affected areas, the aggregate diffstat, and a "look harder at" callout. Empty
// string for an empty range, so an unchanged loop appends nothing.
func (cs loopChangeSet) reviewBlock(h *loopHealth) string {
	if cs.empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## What this loop changed — verify against THIS\n")
	b.WriteString("Each entry is a task this loop completed (bound by its Coop-Task trailer); \"affected features\" means these tasks.\n")
	var shaky []string
	for _, t := range cs.tasks {
		subs := make([]string, len(t.commits))
		for i, c := range t.commits {
			subs[i] = c.subject
		}
		fmt.Fprintf(&b, "- %s — %s\n    files: %s\n", t.id, abbrev(subs, 3), abbrev(t.files, 6))
		if th := h.byTask[t.id]; th != nil && th.shaky() {
			var flags []string
			if th.reopens > 0 {
				flags = append(flags, fmt.Sprintf("signoff reopened it %d×", th.reopens))
			}
			if g := dedupe(th.gateFiles); len(g) > 0 {
				flags = append(flags, "edited gate file(s) "+abbrev(g, 3)+" — confirm the gate wasn't weakened to pass")
			}
			if th.untagged {
				flags = append(flags, "finished with no Coop-Task commit")
			}
			shaky = append(shaky, "  • "+t.id+": "+strings.Join(flags, "; "))
		}
	}
	if len(cs.misc) > 0 {
		misc := make([]string, len(cs.misc))
		for i, c := range cs.misc {
			misc[i] = c.sha + " " + c.subject
		}
		fmt.Fprintf(&b, "- (untrailered commits: %s)\n", abbrev(misc, 4))
	}
	if len(cs.subsystems) > 0 {
		fmt.Fprintf(&b, "Affected areas: %s\n", strings.Join(cs.subsystems, ", "))
	}
	if cs.stat != "" {
		fmt.Fprintf(&b, "\n%s\n", cs.stat)
	}
	if len(shaky) > 0 {
		b.WriteString("\nLook harder at (the run flagged these):\n" + strings.Join(shaky, "\n") + "\n")
	}
	return b.String()
}

// substituteLoopVars replaces the {loop.*} template variables a custom loop.yaml prompt may use, so
// a reviewer/verify prompt can PLACE the context inline (e.g. "e2e {loop.affected}") instead of only
// getting the appended block. A no-op when the prompt uses none.
func substituteLoopVars(prompt string, cs loopChangeSet, h *loopHealth) string {
	if !strings.Contains(prompt, "{loop.") {
		return prompt
	}
	for k, v := range map[string]string{
		"{loop.changes}":  strings.TrimSpace(cs.reviewBlock(h)),
		"{loop.tasks}":    strings.Join(cs.taskIDs(), ", "),
		"{loop.affected}": strings.Join(cs.subsystems, ", "),
	} {
		prompt = strings.ReplaceAll(prompt, k, v)
	}
	return prompt
}

// humanDigest is the human-facing closing block printed above the loop's verdict banner: what shipped
// (per task + its areas), what's blocked on a decision, and any task the run flagged — so you see what
// to review/e2e at a glance instead of a bare counter. Empty when the run changed nothing and blocked
// nothing (the bare banner stands).
func (cs loopChangeSet) humanDigest(h *loopHealth, blocked []string, cost runCost) string {
	if cs.empty() && len(blocked) == 0 && cost.total.usd == 0 {
		return ""
	}
	var b strings.Builder
	if len(cs.tasks) > 0 {
		b.WriteString(ui.Bold("Shipped this run:") + "\n")
		for _, t := range cs.tasks {
			subj := ""
			if len(t.commits) > 0 {
				subj = t.commits[0].subject
			}
			line := fmt.Sprintf("  • %-30s %s  (%s)", t.id, subj, strings.Join(subsystemsOf(t.files), ", "))
			if c := cost.byTask[t.id]; c.usd > 0 {
				line += "  " + ui.Dim(fmt.Sprintf("$%.2f", c.usd))
			}
			b.WriteString(line + "\n")
		}
	}
	if len(cs.subsystems) > 0 {
		fmt.Fprintf(&b, "  Touched: %s\n", strings.Join(cs.subsystems, ", "))
	}
	if cost.total.usd > 0 {
		fmt.Fprintf(&b, "  %s $%.2f · %s in / %s out\n", ui.Bold("Cost:"), cost.total.usd, humanTokens(cost.total.inTok), humanTokens(cost.total.outTok))
	}
	if len(blocked) > 0 {
		fmt.Fprintf(&b, "  %s %s — %s\n", ui.Yellow("Blocked (needs you):"), ui.Count(len(blocked), "task"), abbrev(blocked, 4))
	}
	var shaky []string
	for _, t := range cs.tasks {
		if th := h.byTask[t.id]; th != nil && th.shaky() {
			why := "flagged"
			switch {
			case th.reopens > 0:
				why = fmt.Sprintf("reopened %d×", th.reopens)
			case len(th.gateFiles) > 0:
				why = "edited its gate"
			case th.untagged:
				why = "untagged commit"
			}
			shaky = append(shaky, t.id+" ("+why+")")
		}
	}
	if len(shaky) > 0 {
		fmt.Fprintf(&b, "  %s %s\n", ui.Yellow("Look at:"), abbrev(shaky, 4))
	}
	return strings.TrimRight(b.String(), "\n")
}
