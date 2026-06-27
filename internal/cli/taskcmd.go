package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/ui"
)

// Folder-mode `coop tasks` subcommands. A task's state is its parent directory, so every
// transition is a folder move (atomic os.Rename); git records the rename at commit time.
// Dispatched from cmdTasks when the resolved source is a .agent/tasks directory.

// cmdTasksFolder routes `coop tasks <sub>` against a folder-mode tree rooted at root
// (absolute path to .agent/tasks). No sub-command lists the tree.
func cmdTasksFolder(repo, root string, rest []string) (int, error) {
	sub := ""
	var args []string
	if len(rest) > 0 {
		sub = rest[0]
		args = rest[1:]
	}
	switch sub {
	case "":
		return groupHelp("tasks") // bare `coop tasks` shows help, not an error (see rule)
	case "list", "ls":
		return tasksFolderList(root)
	case "lint":
		return tasksFolderLint(root)
	case "add":
		return tasksFolderAdd(root, args)
	case "claim", "start":
		return tasksFolderMove(root, args, stateInProgress, "claim", "claimed")
	case "block":
		return tasksFolderBlock(root, args)
	case "unblock":
		return tasksFolderUnblock(root, args)
	case "done":
		return tasksFolderMove(root, args, stateDone, "done", "done")
	case "remove", "rm":
		return tasksFolderRemove(root, args)
	case "split":
		return tasksFolderSplit(repo, root, args)
	case "decisions":
		return tasksFolderDecisions(root)
	default:
		return 2, unknownErr("tasks command", sub,
			[]string{"list", "lint", "add", "claim", "block", "unblock", "done", "remove", "split", "decisions"})
	}
}

// findTask locates a task by ID across all state dirs: an exact ID match, else a unique
// substring match (so a slug fragment works). Ambiguous or absent is an error.
func findTask(root, id string) (taskItem, error) {
	if id == "" {
		// An empty fragment would substring-match every task ("" is in everything); make it a
		// clear error instead of silently acting on the first/only one.
		return taskItem{}, errors.New("need a task id (run 'coop tasks' to list)")
	}
	items := readTaskTree(root)
	for _, t := range items {
		if t.ID == id {
			return t, nil
		}
	}
	var hits []taskItem
	for _, t := range items {
		if strings.Contains(t.ID, id) {
			hits = append(hits, t)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return taskItem{}, fmt.Errorf("no task matching %q (run 'coop tasks' to list)", id)
	default:
		var ids []string
		for _, h := range hits {
			ids = append(ids, h.ID)
		}
		return taskItem{}, fmt.Errorf("%q matches %d tasks: %s — be more specific", id, len(hits), strings.Join(ids, ", "))
	}
}

// slugify turns a title into a lowercase, hyphenated id fragment: runs of non-alphanumeric
// become a single "-", trimmed, capped to keep folder names sane.
func slugify(s string) string {
	var b strings.Builder
	lastDash := true // suppress a leading dash
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	// Hard-cap the length for a sane folder name — a plain rune cut, NOT truncate (whose
	// "…" ellipsis has no place in a path); re-trim any dash left dangling at the cut.
	if r := []rune(slug); len(r) > 48 {
		slug = strings.Trim(string(r[:48]), "-")
	}
	return slug
}

// newTaskFiles is the set of starter files `coop tasks add` writes into a new task folder: the
// required task.md plus a seeded log.md and state.md. Each opens with an HTML-comment header
// that explains the file and shows its format, so the file is self-documenting yet renders clean
// once filled. The full reference with worked examples is .agent/tasks/README.md. decision.md is
// NOT seeded here — `block` writes it, since a pending decision is what moves a task to
// 50_blocked/ (and a decision.md on a todo task is a lint error).
func newTaskFiles(id, title, now string) map[string]string {
	return map[string]string{
		"task.md": "<!-- Task spec — a fresh agent must be able to work this from this file ALONE.\n" +
			"     Full format + examples: .agent/tasks/README.md -->\n" +
			"---\nid: " + id + "\ntitle: " + title + "\nlabels: []\nupdated: " + now + "\n---\n\n" +
			"# " + title + "\n\n" +
			"**Context:** <the problem, why it matters, and where in the code it lives>\n\n" +
			"**Acceptance criteria:** <the gate green + the behaviour/test that proves it's done>\n\n" +
			"**Approach:** <the boring plan; when it outgrows ~a screen, move it into spec.md>\n\n" +
			"## Subtasks\n" +
			"- [ ] <first small, end-to-end, testable step — check off once the gate is green>\n",
		"log.md": "<!-- Append-only working journal: what you did and WHY (decisions, dead ends,\n" +
			"     surprises). Add to the BOTTOM; never rewrite history. The short \"where am I\n" +
			"     now\" snapshot lives in state.md, not here. Example entry:\n" +
			"       ## " + now[:10] + " — chose os.Rename over copy+delete\n" +
			"       - atomic, so a torn move can't half-create the task folder. -->\n\n" +
			"# Log — " + title + "\n",
		"state.md": "<!-- Resume snapshot — OVERWRITE this whole file at each checkpoint (before a\n" +
			"     commit or pause) so a fresh agent can resume cold. Keep it short; this is NOT\n" +
			"     a journal (that's log.md). -->\n\n" +
			"# State — " + title + "\n\n" +
			"**Status:** not started\n" +
			"**Done so far:** —\n" +
			"**Next action:** <the very next concrete step>\n" +
			"**Traps:** <gotchas the next agent must know, or —>\n",
	}
}

func tasksFolderAdd(root string, args []string) (int, error) {
	title := strings.TrimSpace(strings.Join(args, " "))
	if title == "" {
		return 2, errors.New(`usage: coop tasks add "<title>"`)
	}
	slug := slugify(title)
	if slug == "" {
		return 2, errors.New("title has no usable letters/digits for a slug")
	}
	id := time.Now().Format("2006-01-02") + "-" + slug
	// An id is a stable, unique handle, so reject a collision in ANY state — not just todo/ —
	// else a re-add after the task shipped would make two folders share an id (and findTask
	// would silently shadow one).
	for _, st := range taskStates {
		if pathExists(filepath.Join(root, st, id)) {
			return 1, fmt.Errorf("task %q already exists in %s/", id, st)
		}
	}
	dir := filepath.Join(root, stateTodo, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return -1, err
	}
	for name, content := range newTaskFiles(id, title, time.Now().Format(time.RFC3339)) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return -1, err
		}
	}
	ui.Info("added %s/%s — fill in task.md (Context · Acceptance · Approach · Subtasks); log.md + state.md are seeded", stateTodo, id)
	return 0, nil
}

// tasksFolderMove relocates a task's folder to newState (claim/done). verb is the imperative used
// in the usage line ("claim"); pastVerb is the past tense for the success note ("claimed"). Moving
// to the state it's already in is a no-op note, not an error.
func tasksFolderMove(root string, args []string, newState, verb, pastVerb string) (int, error) {
	if len(args) < 1 {
		return 2, fmt.Errorf("usage: coop tasks %s <id>", verb)
	}
	t, err := findTask(root, args[0])
	if err != nil {
		return 1, err
	}
	if t.State == newState {
		ui.Info("%s is already %s", t.ID, stateLabel(newState))
		return 0, nil
	}
	if err := moveTaskDir(root, t, newState); err != nil {
		return -1, err
	}
	ui.Info("%s %s (now %s)", pastVerb, t.ID, stateLabel(newState))
	return 0, nil
}

// tasksFolderUnblock moves a task out of 50_blocked/ back to 10_in_progress/ — but only if it's
// actually blocked, so a fat-fingered id can't silently reopen a done (or todo) task.
func tasksFolderUnblock(root string, args []string) (int, error) {
	if len(args) < 1 {
		return 2, errors.New("usage: coop tasks unblock <id>")
	}
	t, err := findTask(root, args[0])
	if err != nil {
		return 1, err
	}
	if t.State != stateBlocked {
		return 1, fmt.Errorf("%s is not blocked (it's %s) — nothing to unblock", t.ID, stateLabel(t.State))
	}
	if err := moveTaskDir(root, t, stateInProgress); err != nil {
		return -1, err
	}
	ui.Info("unblocked %s (now %s)", t.ID, stateLabel(stateInProgress))
	return 0, nil
}

// moveTaskDir renames a task's folder into root/newState, creating the state dir if needed. If the
// id already exists in newState (a torn move or a stray duplicate across states), it refuses with an
// actionable message rather than letting os.Rename fail with a raw "file exists" and stranding the
// task. (readTaskTree dedups such a duplicate on the READ side; this guards the WRITE side.)
func moveTaskDir(root string, t taskItem, newState string) error {
	dest := filepath.Join(root, newState, t.ID)
	if t.Dir != dest && pathExists(dest) {
		return fmt.Errorf("can't move %s to %s/: a folder with that id already exists there (a torn move or stray copy) — remove one: rm -rf %q", t.ID, stateLabel(newState), dest)
	}
	if err := os.MkdirAll(filepath.Join(root, newState), 0o755); err != nil {
		return err
	}
	return os.Rename(t.Dir, dest)
}

func tasksFolderBlock(root string, args []string) (int, error) {
	if len(args) < 1 {
		return 2, errors.New("usage: coop tasks block <id>")
	}
	t, err := findTask(root, args[0])
	if err != nil {
		return 1, err
	}
	if t.State != stateBlocked {
		if err := moveTaskDir(root, t, stateBlocked); err != nil {
			return -1, err
		}
	}
	dec := filepath.Join(root, stateBlocked, t.ID, "decision.md")
	if !fileExists(dec) {
		stub := "<!-- A one-way-door choice that blocks this task. The agent fills The decision,\n" +
			"     Options, and Recommendation; a HUMAN writes Resolution, then runs:\n" +
			"       coop tasks unblock " + t.ID + " -->\n\n" +
			"# Decision: " + t.Title + "?\n\n" +
			"**Blocks:** this task (`" + t.ID + "`).\n\n" +
			"**The decision:** <what must be chosen, and why it can't be undone cheaply>\n\n" +
			"**Options:**\n" +
			"- **A — <name>:** <consequence>\n" +
			"- **B — <name>:** <consequence>\n\n" +
			"**Recommendation:** <the agent's pick + one line why>\n\n" +
			"---\n\n" +
			"**Resolution:** <!-- HUMAN: your answer here (e.g. \"A — go with Postgres\"), then: coop tasks unblock " + t.ID + " -->\n"
		if err := os.WriteFile(dec, []byte(stub), 0o644); err != nil {
			return -1, err
		}
	}
	ui.Info("blocked %s — fill in %s/%s/decision.md, then a human writes Resolution and runs 'coop tasks unblock %s'", t.ID, stateBlocked, t.ID, t.ID)
	return 0, nil
}

// tasksFolderRemove deletes task folders — `remove <id>` for one (any state), or
// `remove --all-done` to clear the xx_done/ archive. It is a MANUAL, human action: the
// loop and skills only ever MOVE a finished task to xx_done/, never delete it, so done
// tasks accumulate until someone prunes them with this.
func tasksFolderRemove(root string, args []string) (int, error) {
	const usage = "usage: coop tasks remove <id>  |  coop tasks remove --all-done"
	if len(args) == 1 && args[0] == "--all-done" {
		items := readTaskTree(root)
		removed := 0
		for _, t := range items {
			if t.State != stateDone {
				continue
			}
			if err := os.RemoveAll(t.Dir); err != nil {
				return -1, err
			}
			removed++
		}
		if removed == 0 {
			ui.Info("no done task(s) to remove")
			return 0, nil
		}
		ui.Info("removed %d done task(s) from %s/", removed, stateDone)
		return 0, nil
	}
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return 2, errors.New(usage)
	}
	t, err := findTask(root, args[0])
	if err != nil {
		return 1, err
	}
	if err := os.RemoveAll(t.Dir); err != nil {
		return -1, err
	}
	ui.Info("removed %s/%s (note why in the commit)", t.State, t.ID)
	return 0, nil
}

// tasksFolderSplit round-robins the todo tasks into n per-slice trees (.agent/tasks.1 …
// .agent/tasks.n), as COPIES — the source is untouched. Loop one fork per slice.
func tasksFolderSplit(repo, root string, args []string) (int, error) {
	if len(args) < 1 {
		return 2, errors.New("usage: coop tasks split <n>")
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n <= 0 {
		return 2, errors.New("usage: coop tasks split <n>")
	}
	names := make([]string, n)
	for i := range names {
		names[i] = strconv.Itoa(i + 1)
	}
	written, counts, total, err := splitTodoFolders(repo, root, names)
	if err != nil {
		return -1, err
	}
	if total == 0 {
		ui.Info("no todo task(s) to split")
		return 0, nil
	}
	wrote := 0
	for i, rel := range written {
		if rel == "" {
			continue
		}
		ui.Info("wrote %s (%d task(s))", rel, counts[i])
		wrote++
	}
	if wrote < n {
		ui.Info("only %d todo task(s) — wrote %d slice(s), not the %d requested", total, wrote, n)
	}
	// The slices are COPIES; the source .agent/tasks is untouched. Say which to run so a
	// later loop doesn't process every task twice.
	ui.Info("the slices are copies — .agent/tasks is unchanged; loop one fork per slice")
	ui.Info("e.g. coop fork s1 --loop --tasks .agent/tasks.1 ; don't also loop .agent/tasks, or each task runs twice")
	return 0, nil
}

func tasksFolderList(root string) (int, error) {
	items := readTaskTree(root)
	if len(items) == 0 {
		ui.Info("no tasks yet — add one with 'coop tasks add \"<title>\"'")
		return 0, nil
	}
	byState := map[string][]taskItem{}
	for _, t := range items {
		byState[t.State] = append(byState[t.State], t)
	}
	// Groups breathe: a blank line between state sections (see rule list-output-echoes-source).
	first := true
	for _, state := range taskStates {
		ts := byState[state]
		if len(ts) == 0 {
			continue
		}
		if !first {
			fmt.Println()
		}
		first = false
		fmt.Printf("%s (%d)\n", ui.Bold(stateLabel(state)), len(ts))
		for _, t := range ts {
			fmt.Printf("  - %s  %s%s\n", t.ID, truncate(t.Title, 56), listSuffix(t))
		}
	}
	c, _ := taskTreeCounts(items)
	fmt.Printf("\n  %d todo · %s in progress · %s blocked · %s done\n",
		c.Todo, paintCount(c.Doing, ui.Yellow), paintCount(c.Blocked, ui.Red), paintCount(c.Done, ui.Green))
	return 0, nil
}

// listSuffix renders a task's at-a-glance extras: subtask progress and a blocked flag.
func listSuffix(t taskItem) string {
	s := ""
	if n := len(t.Subtasks); n > 0 {
		s += fmt.Sprintf("  [%d/%d]", t.doneSubtasks(), n)
	}
	if t.State == stateBlocked {
		s += "  " + ui.Red("⚠ decision")
	}
	return s
}

func tasksFolderDecisions(root string) (int, error) {
	items := readTaskTree(root)
	n := 0
	for _, t := range items {
		if t.State != stateBlocked {
			continue
		}
		n++
		question := t.Title
		rec := ""
		dec := readFileString(filepath.Join(t.Dir, "decision.md"))
		for _, line := range strings.Split(dec, "\n") {
			if q, ok := strings.CutPrefix(line, "# Decision:"); ok {
				question = strings.TrimSpace(q)
			}
			if r, ok := strings.CutPrefix(line, "**Recommendation:**"); ok {
				rec = strings.TrimSpace(r)
			}
		}
		fmt.Printf("%s  %s\n", ui.Bold(t.ID), question)
		if rec != "" {
			fmt.Printf("    rec: %s\n", truncate(rec, 80))
		}
	}
	if n == 0 {
		ui.Info("no open decisions — nothing is blocked")
		return 0, nil
	}
	ui.Info("%d open decision(s) — resolve each %s/<id>/decision.md, then 'coop tasks unblock <id>'", n, stateBlocked)
	return 0, nil
}

func tasksFolderLint(root string) (int, error) {
	items := readTaskTree(root)
	var findings []string
	add := func(id, msg string) { findings = append(findings, fmt.Sprintf("  %s: %s", id, msg)) }
	for _, t := range items {
		body := readFileString(filepath.Join(t.Dir, "task.md"))
		// blocked ⇒ a decision.md is present; a todo shouldn't carry one (it'd be blocked).
		// A resolved decision.md may ride along through in_progress→done as the audit trail.
		if t.State == stateBlocked && !t.HasDecision {
			add(t.ID, "blocked but has no decision.md — add one, or unblock it")
		}
		if t.State == stateTodo && t.HasDecision {
			add(t.ID, "has a decision.md but is todo — an open decision means it should be blocked")
		}
		// a status field is forbidden — the directory IS the status
		if fields, _ := splitFrontmatter(body); fields["status"] != "" {
			add(t.ID, "has a `status:` field — remove it; the parent directory is the status")
		}
		// self-contained: acceptance criteria present (not for done, which is the record)
		if t.State != stateDone && !strings.Contains(strings.ToLower(body), "acceptance") {
			add(t.ID, "not self-contained: missing an **Acceptance criteria** section")
		}
	}
	if len(findings) == 0 {
		ui.Info("tasks lint: clean")
		return 0, nil
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	ui.Info("tasks lint: %d finding(s)", len(findings))
	return 1, nil
}
