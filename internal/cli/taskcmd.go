package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/ui"
)

// Folder-mode `coop tasks` subcommands. A task's state is its parent directory, so every
// transition is a folder move (atomic os.Rename); git records the rename at commit time.
// Dispatched from cmdTasks when the resolved source is a .agent/tasks directory.

// cmdTasksFolder routes `coop tasks <sub>` against a folder-mode tree rooted at root
// (absolute path to .agent/tasks). No sub-command lists the tree.
func cmdTasksFolder(root string, rest []string) (int, error) {
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	args := rest[1:]
	switch sub {
	case "", "list", "ls":
		return tasksFolderList(root)
	case "lint":
		return tasksFolderLint(root)
	case "add":
		return tasksFolderAdd(root, args)
	case "claim", "start":
		return tasksFolderMove(root, args, stateInProgress, "claimed")
	case "block":
		return tasksFolderBlock(root, args)
	case "unblock":
		return tasksFolderMove(root, args, stateInProgress, "unblocked")
	case "done":
		return tasksFolderMove(root, args, stateDone, "done")
	case "drop":
		return tasksFolderDrop(root, args)
	case "decisions":
		return tasksFolderDecisions(root)
	default:
		return 2, unknownErr("tasks command", sub,
			[]string{"list", "lint", "add", "claim", "block", "unblock", "done", "drop", "decisions"})
	}
}

// findTask locates a task by ID across all state dirs: an exact ID match, else a unique
// substring match (so a slug fragment works). Ambiguous or absent is an error.
func findTask(root, id string) (taskItem, error) {
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
	return truncate(slug, 48)
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
	dir := filepath.Join(root, stateTodo, id)
	if pathExists(dir) {
		return 1, fmt.Errorf("task %q already exists", id)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return -1, err
	}
	body := fmt.Sprintf("---\nid: %s\ntitle: %s\nlabels: []\nupdated: %s\n---\n\n# %s\n\n"+
		"**Context:** \n\n"+
		"**Acceptance criteria:** \n\n"+
		"**Approach:** \n\n"+
		"## Subtasks\n- [ ] \n",
		id, title, time.Now().Format(time.RFC3339), title)
	if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte(body), 0o644); err != nil {
		return -1, err
	}
	ui.Info("added todo/%s — fill in its Context / Acceptance criteria / Approach / Subtasks", id)
	return 0, nil
}

// tasksFolderMove relocates a task's folder to newState (claim/unblock/done). Moving to the
// state it's already in is a no-op note, not an error.
func tasksFolderMove(root string, args []string, newState, verb string) (int, error) {
	if len(args) < 1 {
		return 2, fmt.Errorf("usage: coop tasks %s <id>", verb)
	}
	t, err := findTask(root, args[0])
	if err != nil {
		return 1, err
	}
	if t.State == newState {
		ui.Info("%s is already %s", t.ID, newState)
		return 0, nil
	}
	if err := moveTaskDir(root, t, newState); err != nil {
		return -1, err
	}
	ui.Info("%s %s — now %s/%s", verb, t.ID, newState, t.ID)
	return 0, nil
}

// moveTaskDir renames a task's folder into root/newState, creating the state dir if needed.
func moveTaskDir(root string, t taskItem, newState string) error {
	dest := filepath.Join(root, newState, t.ID)
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
		stub := fmt.Sprintf("# Decision: %s?\n\n**Blocks:** this task (`%s`).\n\n"+
			"**The decision:** \n\n**Options:**\n- **A — :** \n- **B — :** \n\n"+
			"**Recommendation:** \n\n---\n\n**Resolution:** \n", t.Title, t.ID)
		if err := os.WriteFile(dec, []byte(stub), 0o644); err != nil {
			return -1, err
		}
	}
	ui.Info("blocked %s — fill in blocked/%s/decision.md (the question, options, your recommendation)", t.ID, t.ID)
	return 0, nil
}

func tasksFolderDrop(root string, args []string) (int, error) {
	if len(args) < 1 {
		return 2, errors.New("usage: coop tasks drop <id>")
	}
	t, err := findTask(root, args[0])
	if err != nil {
		return 1, err
	}
	if err := os.RemoveAll(t.Dir); err != nil {
		return -1, err
	}
	ui.Info("dropped %s/%s (the record is in git history; note why in LOG.md)", t.State, t.ID)
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
	for _, state := range taskStates {
		ts := byState[state]
		if len(ts) == 0 {
			continue
		}
		fmt.Printf("%s (%d)\n", ui.Bold(state), len(ts))
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
		ui.Info("no open decisions — nothing in blocked/")
		return 0, nil
	}
	ui.Info("%d open decision(s) — resolve in each blocked/<id>/decision.md, then 'coop tasks unblock <id>'", n)
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
			add(t.ID, "in blocked/ but has no decision.md — add one or move it out of blocked/")
		}
		if t.State == stateTodo && t.HasDecision {
			add(t.ID, "has a decision.md but is in todo/ — an open decision means it should be in blocked/")
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
