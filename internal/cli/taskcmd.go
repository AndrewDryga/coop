package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

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
	case "rm", "remove":
		return tasksFolderRemove(root, args)
	case "split":
		return tasksFolderSplit(repo, root, args)
	case "decisions":
		return tasksFolderDecisions(root)
	default:
		return 2, unknownErr("tasks command", sub,
			[]string{"list", "lint", "add", "claim", "block", "unblock", "done", "rm", "split", "decisions"})
	}
}

// isTasksSubcommand reports whether s names a `coop tasks` subcommand (or alias). cmdTasks
// uses it to catch `coop tasks --tasks <sub>`, where --tasks swallows the subcommand as a
// queue path. Keep in sync with the dispatch switch above.
func isTasksSubcommand(s string) bool {
	switch s {
	case "list", "ls", "lint", "add", "claim", "start", "block", "unblock", "done", "remove", "rm", "split", "decisions":
		return true
	}
	return false
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

// slugify turns a title into a lowercase, hyphenated id fragment: runs of non-letter/digit
// become a single "-", trimmed, capped to keep folder names sane. Letters and digits are
// taken Unicode-wide (unicode.IsLetter/IsDigit), so a Cyrillic or CJK title yields a real
// slug instead of being dropped to "" — git and every modern filesystem store UTF-8 paths.
func slugify(s string) string {
	var b strings.Builder
	lastDash := true // suppress a leading dash
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
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
		"task.md": "<!-- TASK SPEC — a fresh agent must work this from this file ALONE.\n" +
			"     FIRST, BEFORE ANY CODE: replace every <…> placeholder below — the real problem and\n" +
			"     where it lives (Context), what proves it's done incl. a green gate (Acceptance), and\n" +
			"     the boring plan (Approach). This thinking IS step one, not a formality. Can't fill it\n" +
			"     honestly? It isn't ready — run: coop tasks block " + id + "\n" +
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
		return 2, errors.New(`that title has no letters or digits to build a task id from — use a title with at least one word, e.g. coop tasks add "fix login retry"`)
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
	ui.OK("added %s — fill in its task.md (Context · Acceptance · Approach · Subtasks); log.md + state.md seeded", id)
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
		ui.Note("%s is already %s", t.ID, stateLabel(newState))
		return 0, nil
	}
	if err := moveTaskDir(root, t, newState); err != nil {
		return -1, err
	}
	ui.OK("%s %s", pastVerb, t.ID)
	return 0, nil
}

// tasksFolderUnblock moves a task out of 50_blocked/ back to 10_in_progress/ — but only if it's
// actually blocked, so a fat-fingered id can't silently reopen a done (or todo) task.
func tasksFolderUnblock(root string, args []string) (int, error) {
	if len(args) < 1 {
		return 2, errors.New(`usage: coop tasks unblock <id> [answer]`)
	}
	t, err := findTask(root, args[0])
	if err != nil {
		return 1, err
	}
	if t.State != stateBlocked {
		return 1, fmt.Errorf("%s is not blocked (it's %s) — nothing to unblock", t.ID, stateLabel(t.State))
	}
	// An optional inline answer is recorded into decision.md's Resolution before the move, so
	// deciding is one command — no open-file/edit/save round-trip — and the answer rides along in
	// the task's audit trail to 99_done/.
	answer := strings.TrimSpace(strings.Join(args[1:], " "))
	if answer != "" {
		if err := recordResolution(filepath.Join(t.Dir, "decision.md"), answer); err != nil {
			return -1, err
		}
	}
	if err := moveTaskDir(root, t, stateInProgress); err != nil {
		return -1, err
	}
	if answer != "" {
		ui.OK("unblocked %s — recorded your answer in decision.md, now in progress", t.ID)
	} else {
		ui.OK("unblocked %s — now in progress", t.ID)
	}
	return 0, nil
}

// recordResolution writes a human's answer into decision.md's "**Resolution:**" line so that
// `coop tasks unblock <id> <answer>` resolves the decision in one step. It replaces an existing
// Resolution line in place (dropping the placeholder), or appends one if the file has none.
func recordResolution(decPath, answer string) error {
	line := "**Resolution:** " + answer
	body := readFileString(decPath)
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "**Resolution:**") {
			lines[i] = line
			out := strings.Join(lines, "\n")
			if !strings.HasSuffix(out, "\n") {
				out += "\n"
			}
			return os.WriteFile(decPath, []byte(out), 0o644)
		}
	}
	out := line + "\n"
	if strings.TrimSpace(body) != "" {
		out = strings.TrimRight(body, "\n") + "\n\n" + line + "\n"
	}
	return os.WriteFile(decPath, []byte(out), 0o644)
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
	if !pathExists(t.Dir) {
		// The source vanished between findTask and now — a concurrent move to a DIFFERENT state
		// won the race. Report it as the actionable message this guard promises, not a raw ENOENT.
		return fmt.Errorf("can't move %s: it changed state under us (a concurrent move won) — re-run 'coop tasks'", t.ID)
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
			"     Options, and Recommendation; a HUMAN decides — either write Resolution below and\n" +
			"     run 'coop tasks unblock " + t.ID + "', or do both in one step:\n" +
			"       coop tasks unblock " + t.ID + " \"A — go with Postgres\" -->\n\n" +
			"# Decision: " + t.Title + "?\n\n" +
			"**Blocks:** this task (`" + t.ID + "`).\n\n" +
			"**The decision:** <what must be chosen, and why it can't be undone cheaply>\n\n" +
			"**Options:**\n" +
			"- **A — <name>:** <consequence>\n" +
			"- **B — <name>:** <consequence>\n\n" +
			"**Recommendation:** <the agent's pick + one line why>\n\n" +
			"---\n\n" +
			"**Resolution:** <!-- HUMAN: your answer (e.g. \"A — go with Postgres\"); or pass it inline to 'coop tasks unblock " + t.ID + "' -->\n"
		if err := os.WriteFile(dec, []byte(stub), 0o644); err != nil {
			return -1, err
		}
	}
	ui.OK("blocked %s — fill in its decision.md, then a human resolves it and runs 'coop tasks unblock %s'", t.ID, t.ID)
	return 0, nil
}

// tasksFolderRemove deletes task folders — `remove <id>` for one (any state), or
// `remove --all-done` to clear the 99_done/ archive. It is a MANUAL, human action: the
// loop and skills only ever MOVE a finished task to 99_done/, never delete it, so done
// tasks accumulate until someone prunes them with this.
func tasksFolderRemove(root string, args []string) (int, error) {
	const usage = "usage: coop tasks rm <id>  |  coop tasks rm --all-done"
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
			ui.Note("no done tasks to remove")
			return 0, nil
		}
		ui.OK("removed %s", ui.Count(removed, "done task"))
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
	ui.OK("removed %s (was %s — note why in the commit)", t.ID, stateLabel(t.State))
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
		ui.Note("no todo tasks to split")
		return 0, nil
	}
	wrote := 0
	for i, rel := range written {
		if rel == "" {
			continue
		}
		ui.Note("wrote %s (%s)", rel, ui.Count(counts[i], "task"))
		wrote++
	}
	if wrote < n {
		ui.Warn("only %s — wrote %s, not the %d requested", ui.Count(total, "todo task"), ui.Count(wrote, "slice"), n)
	}
	// The slices are COPIES; the source .agent/tasks is untouched. Say which to run so a
	// later loop doesn't process every task twice.
	ui.Note("the slices are copies — .agent/tasks is unchanged; loop one fork per slice")
	ui.Note("e.g. coop fork s1 --loop --tasks .agent/tasks.1 ; don't also loop .agent/tasks, or each task runs twice")
	return 0, nil
}

func tasksFolderList(root string) (int, error) {
	items := readTaskTree(root)
	if len(items) == 0 {
		ui.Note("no tasks yet — add one with 'coop tasks add \"<title>\"'")
		return 0, nil
	}
	p := ui.For(os.Stdout)
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
			fmt.Print("\n\n") // two blank lines between state sections
		}
		first = false
		// The state label is colored by state (the shared key — cyan todo · yellow in progress ·
		// red blocked · green done), so a section is findable by its color; the count rides dim.
		fmt.Printf("%s %s\n", p.Bold(paintState(p, state, stateLabel(state))), p.Dim(fmt.Sprintf("(%d)", len(ts))))
		for i, t := range ts {
			if i > 0 {
				fmt.Println() // one blank line between tasks
			}
			// Title-first (what a human scans), wrapped across as many lines as it needs so the
			// whole title is readable and uncluttered. The id — a long machine handle you only
			// need to `claim`/`done` — drops to a faint line below, led by the at-a-glance markers
			// (subtask count, blocked flag), so the title carries no trailing noise.
			for _, tl := range wrapWords(t.Title, titleWrapWidth()) {
				fmt.Printf("  %s\n", tl)
			}
			if m := listMarkers(p, t); m != "" {
				fmt.Printf("    %s  %s\n", m, p.Faint(t.ID))
			} else {
				fmt.Printf("    %s\n", p.Faint(t.ID))
			}
		}
	}
	c, _ := taskTreeCounts(items)
	summary := strings.Join([]string{
		paintCount(c.Todo, p.Cyan) + " todo",
		paintCount(c.Doing, p.Yellow) + " in progress",
		paintCount(c.Blocked, p.Red) + " blocked",
		paintCount(c.Done, p.Green) + " done",
	}, p.Dim(" · "))
	fmt.Print("\n")
	if p.Enabled() {
		fmt.Printf("  %s\n", p.Dim(strings.Repeat("─", bannerWidth()-2))) // footer rule, right-aligned to the header's
	}
	fmt.Printf("  %s\n", summary)
	return 0, nil
}

// paintState colors s by task state — the one key shared across the list (the state headings
// and the summary counts): cyan todo · yellow in progress · red blocked · green done.
func paintState(p ui.Palette, state, s string) string {
	switch state {
	case stateInProgress:
		return p.Yellow(s)
	case stateBlocked:
		return p.Red(s)
	case stateDone:
		return p.Green(s)
	default: // todo
		return p.Cyan(s)
	}
}

// titleWrapWidth is the column budget for wrapping a task title: the terminal width less the
// 2-space indent, clamped so it reads on a wide terminal and fits a narrow one. When stdout is NOT
// a terminal (a pipe/redirect) it returns a very large width, so the title prints on ONE line and
// the full text stays greppable instead of being split across lines.
func titleWrapWidth() int {
	w := ui.TermWidthRaw(os.Stdout)
	if w <= 0 {
		return 1 << 30 // not a terminal: don't wrap — emit the whole title on one line
	}
	if w > 120 {
		w = 120
	}
	w -= 2 // the 2-space indent
	if w < 12 {
		w = 12 // floor so a genuinely narrow pane still wraps to a usable width
	}
	return w
}

// bannerWidth is the column span for the list's header/footer rules — the terminal width,
// clamped like titleWrapWidth so a rule neither overruns a narrow pane nor stretches across an
// ultra-wide one. Only consulted on a terminal (rules are drawn only when color is on).
func bannerWidth() int {
	w := ui.TermWidthRaw(os.Stdout)
	switch {
	case w <= 0:
		return 80 // width unknown (not a terminal)
	case w > 120:
		return 120
	case w < 20:
		return 20 // floor: keeps the footer rule (width-2) positive and the banner readable
	}
	return w
}

// banner renders a queue's section header for the monorepo roll-up. On a terminal it's a cyan
// marker + bold path + a dim rule filling the width — a clean divider between queues; piped, it
// falls back to a plain "# path" so a redirect stays simple (and the roll-up tests see a stable
// label). The matching footer rule is drawn under each queue's summary in tasksFolderList.
func banner(p ui.Palette, path string) string {
	if !p.Enabled() {
		return "# " + path
	}
	visible := "▸ " + path + " "
	rule := ""
	if pad := bannerWidth() - len([]rune(visible)); pad > 0 {
		rule = p.Dim(strings.Repeat("─", pad))
	}
	return p.Cyan("▸") + " " + p.Bold(path) + " " + rule
}

// listMarkers renders a task's at-a-glance markers — subtask progress (plain while work remains,
// gray once every box is checked so a finished count recedes) and a red ⚠ on a blocked task —
// joined with two spaces, or "" when there are none (a task with no subtasks shows no count). They
// lead the id line, so the wrapped title above stays clean.
func listMarkers(p ui.Palette, t taskItem) string {
	var parts []string
	if n := len(t.Subtasks); n > 0 {
		done := t.doneSubtasks()
		prog := fmt.Sprintf("[%d/%d]", done, n)
		if done == n {
			prog = p.Gray(prog) // fully done — recede the count
		}
		parts = append(parts, prog)
	}
	if t.State == stateBlocked {
		parts = append(parts, p.Red("⚠"))
	}
	return strings.Join(parts, "  ")
}

func tasksFolderDecisions(root string) (int, error) {
	items := readTaskTree(root)
	p := ui.For(os.Stdout)
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
		if n > 1 {
			fmt.Println() // each decision is its own block
		}
		// Question first (what you weigh), the id gray below (the handle you `unblock` with),
		// the recommendation dim under it — same shape as the task list.
		fmt.Printf("%s %s\n", p.Red("⚠"), p.Bold(sanitizeCell(question)))
		fmt.Printf("    %s\n", p.Faint(t.ID))
		if rec != "" {
			fmt.Printf("    %s %s\n", p.Dim("rec:"), sanitizeCell(rec))
		}
	}
	if n == 0 {
		ui.OK("no open decisions — nothing is blocked")
		return 0, nil
	}
	ui.Note(`%s — answer with 'coop tasks unblock <id> "<answer>"' (or edit its decision.md, then unblock)`, ui.Count(n, "open decision"))
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
		if len(items) == 0 {
			ui.Note("no tasks to check")
		} else {
			ui.OK("no issues — %s checked", ui.Count(len(items), "task"))
		}
		return 0, nil
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	ui.Error("%s across %s — fix the above", ui.Count(len(findings), "issue"), ui.Count(len(items), "task"))
	return 1, nil
}
