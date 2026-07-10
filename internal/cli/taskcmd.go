package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/AndrewDryga/coop/internal/ui"
)

// Folder-mode `coop tasks` subcommands. A task's state is its parent directory, so every
// transition is a folder move (atomic os.Rename) in gitignored local working state.
// Dispatched from cmdTasks when the resolved source is a .agent/tasks directory.

// cmdTasksFolder routes `coop tasks <sub>` against a folder-mode tree rooted at root
// (absolute path to .agent/tasks). No sub-command lists the tree.
// taskArgSpec declares a structured subcommand's allowed flags and max positional count.
type taskArgSpec struct {
	flags  []string
	maxPos int
}

// taskArgSpecs validates the structured `coop tasks` subcommands so an unsupported flag or a stray
// argument fails loudly instead of being silently ignored or mistaken for an id. add/unblock take
// free-form text (a title / a human's answer) that may start with "-", and decisions self-validates,
// so they're intentionally absent.
var taskArgSpecs = map[string]taskArgSpec{
	"ls":    {[]string{"--all"}, 0},
	"lint":  {nil, 0},
	"claim": {nil, 1}, "path": {nil, 1},
	"block": {nil, 1}, "done": {nil, 1}, "split": {nil, 1},
	"rm": {[]string{"--all-done", "--yes", "-y"}, 1},
}

// validateArgs enforces a subcommand's flags + positional count: any token starting with "-" must be
// in allowedFlags, and at most maxPos positionals are allowed. So `coop tasks ls --done` or `coop
// tasks claim a b` fails with a clear message rather than quietly doing the wrong thing.
func validateArgs(cmd string, args, allowedFlags []string, maxPos int) error {
	pos := 0
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			if !slices.Contains(allowedFlags, a) {
				hint := ""
				if len(allowedFlags) > 0 {
					hint = " (supported: " + strings.Join(allowedFlags, ", ") + ")"
				}
				return fmt.Errorf("coop %s: unknown flag %q%s", cmd, a, hint)
			}
			continue
		}
		pos++
	}
	if pos > maxPos {
		return fmt.Errorf("coop %s: too many arguments (got %d, expected at most %d)", cmd, pos, maxPos)
	}
	return nil
}

func cmdTasksFolder(repo, root string, rest []string) (int, error) {
	sub := ""
	var args []string
	if len(rest) > 0 {
		sub = rest[0]
		args = rest[1:]
	}
	// Reject unsupported flags / stray arguments up front for the structured subcommands (see
	// taskArgSpecs); add/unblock/decisions handle their own free-form args.
	if spec, ok := taskArgSpecs[sub]; ok {
		if err := validateArgs("tasks "+sub, args, spec.flags, spec.maxPos); err != nil {
			return 2, err
		}
	}
	switch sub {
	case "":
		return tasksFolderList(root, false) // bare `coop tasks` lists the queue (a useful default view; see rule)
	case "ls":
		return tasksFolderList(root, slices.Contains(args, "--all"))
	case "lint":
		return tasksFolderLint(root)
	case "add":
		return tasksFolderAdd(root, args, stateTodo, "tasks add")
	case "claim":
		return tasksFolderMove(root, args, stateInProgress, "claim", "claimed")
	case "start": // v3: renamed to claim
		note, _ := removedCommandNote("tasks start")
		return 2, errors.New(note)
	case "block":
		return tasksFolderBlock(root, args)
	case "unblock":
		return tasksFolderUnblock(root, args)
	case "done":
		return tasksFolderMove(root, args, stateDone, "done", "done")
	case "path":
		return tasksFolderPath(root, args)
	case "rm":
		return tasksFolderRemove(root, args)
	case "clear": // bulk-delete idiom: clear the done archive (like `rm --all-done`)
		return tasksFolderRemove(root, append([]string{"--all-done"}, args...))
	case "split":
		return tasksFolderSplit(repo, root, args)
	case "decisions":
		return tasksFolderDecisions(root, args)
	default:
		return 2, unknownErr("tasks command", sub, tasksVerbs)
	}
}

// tasksVerbs are the canonical `coop tasks` subcommands (primary spellings, no aliases): the single
// source for the unknown-subcommand suggester and isTasksSubcommand, so the two can't drift. `watch`
// belongs here even though cmdTasks (not cmdTasksFolder) handles it — a mistype of it should suggest it.
var tasksVerbs = []string{"ls", "lint", "add", "claim", "block", "unblock", "done", "watch", "queues", "path", "rm", "clear", "split", "decisions"}

// isTasksSubcommand reports whether s names a `coop tasks` subcommand. cmdTasks uses it to catch
// `coop tasks --tasks <sub>`, where --tasks swallows the subcommand as a queue path. v3 keeps no
// compat aliases (ls/rm are the only spellings), so this is plain tasksVerbs membership.
func isTasksSubcommand(s string) bool {
	return slices.Contains(tasksVerbs, s)
}

// matchTask resolves id against a set of task items: an exact ID match wins, else a unique substring
// match (so a slug fragment works). Ambiguous or absent is an error. listCmd names the command that
// lists this set ("coop tasks" / "coop backlog"), so the "run '…' to list" hint points at the right
// place. Shared by findTask (the lifecycle tree) and findBacklogTask (xx_backlog).
func matchTask(items []taskItem, id, listCmd string) (taskItem, error) {
	if id == "" {
		// An empty fragment would substring-match every task ("" is in everything); make it a
		// clear error instead of silently acting on the first/only one.
		return taskItem{}, fmt.Errorf("need a task id (run '%s' to list)", listCmd)
	}
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
		return taskItem{}, fmt.Errorf("no task matching %q (run '%s' to list)", id, listCmd)
	default:
		var ids []string
		for _, h := range hits {
			ids = append(ids, h.ID)
		}
		return taskItem{}, fmt.Errorf("%q matches %d tasks: %s — be more specific", id, len(hits), strings.Join(ids, ", "))
	}
}

// findTask locates a task by ID across the lifecycle state dirs — an exact ID match, else a unique
// substring match. Backlog (xx_backlog) is deliberately NOT searched: it's off to the side, so the
// active id-commands (claim/done/…) can't accidentally act on an un-promoted idea. See findBacklogTask.
func findTask(root, id string) (taskItem, error) {
	return matchTask(readTaskTree(root), id, "coop tasks")
}

// findBacklogTask locates a backlog item by ID under root's xx_backlog/ — the backlog analog of
// findTask, so `coop backlog rm/promote` accept a slug fragment and error clearly on absent/ambiguous.
func findBacklogTask(root, id string) (taskItem, error) {
	return matchTask(readBacklog(root), id, "coop backlog")
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

// taskSection is one **Heading:** block of a task.md body. taskSections is the SINGLE source of the
// body's shape: the scaffold (newTaskFiles), the structured `coop tasks add` flags, and lint
// (taskShapeIssues) all derive from it, so they can't drift. Subtasks are the trailing `## Subtasks`
// checklist — a list, not a section — so they're handled separately.
type taskSection struct{ heading, flag, placeholder string }

var taskSections = []taskSection{
	{"Context", "context", "<the problem, why it matters, and where in the code it lives>"},
	{"Acceptance criteria", "acceptance", "<the gate green + the behaviour/test that proves it's done>"},
	{"Approach", "approach", "<the boring plan; when it outgrows ~a screen, move it into spec.md>"},
}

const defaultSubtask = "<first small, end-to-end, testable step — check off once the gate is green>"

// taskBody renders the task.md body after the `# title` line: each section as `**Heading:** value`
// (a blank/absent value falls back to the section's `<…>` placeholder — that's the scaffold), then
// the `## Subtasks` checklist (the default placeholder when none are given).
func taskBody(values map[string]string, subtasks []string) string {
	var b strings.Builder
	for _, s := range taskSections {
		v := strings.TrimSpace(values[s.heading])
		if v == "" {
			v = s.placeholder
		}
		fmt.Fprintf(&b, "**%s:** %s\n\n", s.heading, v)
	}
	b.WriteString("## Subtasks\n")
	if len(subtasks) == 0 {
		subtasks = []string{defaultSubtask}
	}
	for _, st := range subtasks {
		fmt.Fprintf(&b, "- [ ] %s\n", st)
	}
	return b.String()
}

// newTaskFiles is the set of starter files `coop tasks add` writes into a new task folder: the
// required task.md plus a seeded log.md and state.md. Each opens with an HTML-comment header
// that explains the file and shows its format, so the file is self-documenting yet renders clean
// once filled. The full reference with worked examples is .agent/tasks/README.md. decision.md is
// NOT seeded here — `block` writes it, since a pending decision is what moves a task to
// 50_blocked/ (and a decision.md on a todo task is a lint error). values/subtasks fill the body from
// structured `add` flags; pass nil/empty for the placeholder scaffold.
func newTaskFiles(id, title, now string, values map[string]string, subtasks []string) map[string]string {
	return map[string]string{
		"task.md": "<!-- TASK SPEC — a fresh agent must work this from this file ALONE.\n" +
			"     FIRST, BEFORE ANY CODE: replace every <…> placeholder below — the real problem and\n" +
			"     where it lives (Context), what proves it's done incl. a green gate (Acceptance), and\n" +
			"     the boring plan (Approach). This thinking IS step one, not a formality. Can't fill it\n" +
			"     honestly? It isn't ready — run: coop tasks block " + id + "\n" +
			"     Full format + examples: .agent/tasks/README.md -->\n" +
			"---\nid: " + id + "\ntitle: " + title + "\nlabels: []\nupdated: " + now + "\n---\n\n" +
			"# " + title + "\n\n" + taskBody(values, subtasks),
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

// tasksFolderAdd creates a task folder under root/state (stateTodo for `coop tasks add`, stateBacklog
// for `coop backlog add` — the two share every bit of parsing/validation). cmdLabel is the command as
// typed ("tasks add" / "backlog add"), so error and usage lines name the right command.
func tasksFolderAdd(root string, args []string, state, cmdLabel string) (int, error) {
	return tasksFolderAddWithProject(root, args, state, cmdLabel, "")
}

func tasksFolderAddWithProject(root string, args []string, state, cmdLabel, projectName string) (int, error) {
	// Optional structured flags carve the title from the body: with any of
	// --context/--acceptance/--approach/--subtask the task is created FILLED and validated up front;
	// with none, it's the placeholder scaffold you edit. The flag names ARE the shape (taskSections).
	sectionByFlag := map[string]string{}
	for _, s := range taskSections {
		sectionByFlag[s.flag] = s.heading
	}
	var titleWords, subtasks []string
	values := map[string]string{}
	structured := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			titleWords = append(titleWords, a)
			continue
		}
		flag, val, hasEq := strings.TrimPrefix(a, "--"), "", false
		if eq := strings.IndexByte(flag, '='); eq >= 0 {
			flag, val, hasEq = flag[:eq], flag[eq+1:], true
		}
		heading, isSection := sectionByFlag[flag]
		if !isSection && flag != "subtask" {
			return 2, fmt.Errorf("coop %s: unknown flag %q (known: --context, --acceptance, --approach, --subtask)", cmdLabel, a)
		}
		if !hasEq {
			if i+1 >= len(args) {
				return 2, fmt.Errorf("coop %s --%s needs a value", cmdLabel, flag)
			}
			i++
			val = args[i]
		}
		structured = true
		if flag == "subtask" {
			subtasks = append(subtasks, val)
		} else {
			values[heading] = val
		}
	}
	title := strings.TrimSpace(strings.Join(titleWords, " "))
	if title == "" {
		return 2, fmt.Errorf(`usage: coop %s "<title>" [--context <c> --acceptance <a> --approach <p> --subtask <s>...]`, cmdLabel)
	}
	slug := slugify(title)
	if slug == "" {
		return 2, fmt.Errorf(`that title has no letters or digits to build a task id from — use a title with at least one word, e.g. coop %s "fix login retry"`, cmdLabel)
	}
	// Structured mode is all-or-nothing: every section flag must be given, so we never create a task
	// that's half-filled and half-<…>-placeholder. Omit all the flags to get the placeholder scaffold.
	if structured {
		var missing []string
		for _, s := range taskSections {
			if strings.TrimSpace(values[s.heading]) == "" {
				missing = append(missing, "--"+s.flag)
			}
		}
		if len(missing) > 0 {
			return 2, fmt.Errorf("coop %s: structured flags need every section — missing %s (or omit all flags to scaffold)", cmdLabel, strings.Join(missing, ", "))
		}
	}
	id := time.Now().Format("2006-01-02") + "-" + slug
	// An id is a stable, unique handle, so reject a collision in ANY state — the four lifecycle dirs
	// AND xx_backlog — else a re-add (or a promote) would make two folders share an id, and
	// findTask/findBacklogTask would silently shadow one.
	for _, st := range taskStates {
		if pathExists(filepath.Join(root, st, id)) {
			return 1, fmt.Errorf("task %q already exists in %s/", id, st)
		}
	}
	if pathExists(filepath.Join(root, stateBacklog, id)) {
		return 1, fmt.Errorf("task %q already exists in %s/ — promote it (coop backlog promote %s) instead of re-adding", id, stateBacklog, id)
	}
	// Ensure all four state dirs exist (the queue may be fresh, or predate the four-state scaffold), so
	// the move-a-folder-between-states protocol always has a real dir to move into — same guarantee as
	// `coop init` and the split producers. Then the task's own todo dir.
	if err := scaffoldStateDirs(root); err != nil {
		return -1, err
	}
	// The target dir: stateTodo lives under scaffoldStateDirs above; xx_backlog is created on demand
	// here (like a fresh secondary queue), so `coop init` never has to scaffold an empty backlog drawer.
	dir := filepath.Join(root, state, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return -1, err
	}
	for name, content := range newTaskFiles(id, title, time.Now().Format(time.RFC3339), values, subtasks) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return -1, err
		}
	}
	where := ""
	if projectName != "" {
		where = " in " + projectName
	}
	switch {
	case state == stateBacklog:
		ui.OK("backlogged %s — promote it when it's ready: coop backlog promote %s", id, id)
	case structured:
		ui.OK("added %s%s (filled from flags); log.md + state.md seeded", id, where)
	default:
		ui.OK("added %s%s — fill in its task.md (Context · Acceptance · Approach · Subtasks); log.md + state.md seeded", id, where)
	}
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

// tasksFolderPath prints a task's resolved folder — the id-command companion to `coop fork path`,
// so `cat "$(coop tasks path <id>)/task.md"` works for humans and hooks. Reuses findTask, so a slug
// fragment resolves and an absent/ambiguous id errors exactly like claim/done.
func tasksFolderPath(root string, args []string) (int, error) {
	if len(args) < 1 {
		return 2, errors.New("usage: coop tasks path <id>")
	}
	t, err := findTask(root, args[0])
	if err != nil {
		return 1, err
	}
	fmt.Println(t.Dir)
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
	// The optional inline answer makes deciding one command — no open-file/edit/save round-trip.
	answer := strings.TrimSpace(strings.Join(args[1:], " "))
	// Don't unblock into a state lint rejects: a todo task with an UNRESOLVED decision.md is the
	// inconsistency lint flags. Require a resolution — inline, or pre-written in decision.md — or
	// the task stays blocked.
	if answer == "" && t.HasDecision && !decisionResolved(filepath.Join(t.Dir, "decision.md")) {
		return 2, fmt.Errorf("%s has no resolution yet — write the **Resolution:** in its decision.md, or pass it inline: coop tasks unblock %s \"<answer>\"", t.ID, args[0])
	}
	if err := resolveAndUnblock(root, t, answer); err != nil {
		return -1, err
	}
	if answer != "" {
		ui.OK("unblocked %s — recorded your answer in decision.md, back in todo (claim it to start)", t.ID)
	} else {
		ui.OK("unblocked %s — back in todo (claim it to start)", t.ID)
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

// decisionResolved reports whether a decision.md has a filled-in Resolution (a human's answer),
// as opposed to the empty/placeholder line `coop tasks block` seeds. lint uses it: a resolved
// decision rides along on an unblocked (todo) task as its audit trail, but an unresolved one on a
// non-blocked task is the inconsistency to flag.
func decisionResolved(decPath string) bool {
	for _, line := range strings.Split(readFileString(decPath), "\n") {
		if r, ok := strings.CutPrefix(line, "**Resolution:**"); ok {
			r = strings.TrimSpace(r)
			return r != "" && !strings.HasPrefix(r, "<!--")
		}
	}
	return false
}

// resolveAndUnblock records answer (if non-empty) into the task's decision.md, then returns the
// task to 00_todo/ — NOT 10_in_progress/: in_progress is the "an agent is on this" lock taken by
// `claim`, so a just-unblocked task with nobody on it belongs in the queue as available work; the
// resolved decision.md rides along as the audit trail. Shared by `unblock` and the -i browser.
func resolveAndUnblock(root string, t taskItem, answer string) error {
	if answer != "" {
		if err := recordResolution(filepath.Join(t.Dir, "decision.md"), answer); err != nil {
			return err
		}
	}
	return moveTaskDir(root, t, stateTodo)
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

// tasksFolderRemove deletes task folders — `rm <id>` for one (any state), or
// `remove --all-done` to clear the 99_done/ archive. It is a MANUAL, human action: the
// loop and skills only ever MOVE a finished task to 99_done/, never delete it, so done
// tasks accumulate until someone prunes them with this.
func tasksFolderRemove(root string, args []string) (int, error) {
	const usage = "usage: coop tasks rm <id> [--yes]  |  coop tasks rm --all-done [--yes]"
	yes := hasYes(args)
	var pos []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
		}
	}
	if slices.Contains(args, "--all-done") {
		if len(pos) != 0 {
			return 2, errors.New(usage) // an id and --all-done together is ambiguous
		}
		n := countDone(root)
		if n == 0 {
			ui.Note("no done tasks to remove")
			return 0, nil
		}
		if err := destroyGate("remove "+ui.Count(n, "done task")+" from the archive", yes); err != nil {
			return 2, err
		}
		removed, err := removeAllDone(root)
		if err != nil {
			return -1, err
		}
		ui.OK("removed %s", ui.Count(removed, "done task"))
		return 0, nil
	}
	if len(pos) != 1 {
		return 2, errors.New(usage)
	}
	t, err := findTask(root, pos[0]) // resolve the (possibly substring) match first, so the gate names it
	if err != nil {
		return 1, err
	}
	if err := destroyGate(fmt.Sprintf("delete task %s (%s)", t.ID, stateLabel(t.State)), yes); err != nil {
		return 2, err
	}
	if err := os.RemoveAll(t.Dir); err != nil {
		return -1, err
	}
	ui.OK("removed %s (was %s — note why in the commit)", t.ID, stateLabel(t.State))
	return 0, nil
}

// removeAllDone deletes every task folder in root's 99_done/ archive, returning how many went.
// Shared by the single-queue `rm --all-done` and the multi-queue roll-up.
func removeAllDone(root string) (int, error) {
	removed := 0
	for _, t := range readTaskTree(root) {
		if t.State != stateDone {
			continue
		}
		if err := os.RemoveAll(t.Dir); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// countDone reports how many done tasks removeAllDone would delete — for the pre-delete blast-radius
// prompt, so `rm --all-done` can say the count before the (unrecoverable) removal, not after.
func countDone(root string) int {
	n := 0
	for _, t := range readTaskTree(root) {
		if t.State == stateDone {
			n++
		}
	}
	return n
}

// tasksFolderSplit round-robins the todo tasks into n per-slice trees (.agent/tasks.slice1 …
// .agent/tasks.slicen), as COPIES — the source is untouched. Loop one fork per slice
// (`coop fork slice1 --loop --tasks .agent/tasks.slice1`).
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
		names[i] = "slice" + strconv.Itoa(i+1)
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
	ui.Note("e.g. coop fork slice1 --loop --tasks .agent/tasks.slice1 ; don't also loop .agent/tasks, or each task runs twice")
	return 0, nil
}

// doneListCap caps how many of the (oldest-first sorted) done tasks `coop tasks ls` shows — the
// done archive only grows, and the live todo/in-progress/blocked work shouldn't scroll off below it.
// The full count stays in the section header + summary; `--all` shows everything.
const doneListCap = 5

func tasksFolderList(root string, all bool) (int, error) {
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
		if state == stateDone && !all && len(ts) > doneListCap {
			// Show only the most recent (the tail — folders sort oldest-first); elide the rest.
			fmt.Printf("  %s\n", p.Faint(fmt.Sprintf("… +%d earlier — coop tasks ls --all", len(ts)-doneListCap)))
			ts = ts[len(ts)-doneListCap:]
		}
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
	// Explain the per-task [n/m] marker for a first-time reader — but only when a task actually has
	// subtasks, so the common (subtask-free) listing stays uncluttered.
	for _, t := range items {
		if len(t.Subtasks) > 0 {
			fmt.Printf("  %s\n", p.Dim("[checked/total] = subtasks"))
			break
		}
	}
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

// decisionDivider is the header BETWEEN decisions in the interactive browser (`coop tasks
// decisions -i`). It's a stronger sibling of banner — a cyan ▸ + bold "decision N of M" + the
// task location, then a cyan rule filling the width — so each decision is clearly bordered off
// from the previous one as you scroll. Piped/no-color falls back to a plain, stable label the
// tests match ("decision N of M"). where is the task id, optionally "queue · id" in a monorepo.
func decisionDivider(p ui.Palette, n, total int, where string) string {
	label := fmt.Sprintf("decision %d of %d", n, total)
	if !p.Enabled() {
		return "── " + label + " · " + where + " ──"
	}
	head := "▸ " + label + " · " + where + " "
	rule := ""
	if pad := bannerWidth() - len([]rune(head)); pad > 0 {
		// A HEAVY rule (━, vs the queue banner's light ─) so the interactive divider reads as a
		// strong border between decisions as you scroll through them.
		rule = p.Cyan(strings.Repeat("━", pad))
	}
	return p.Cyan("▸ ") + p.Bold(label) + p.Dim(" · ") + p.Cyan(where) + " " + rule
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

func tasksFolderDecisions(root string, args []string) (int, error) {
	interactive := false
	for _, a := range args {
		switch a {
		case "-i", "--interactive":
			interactive = true
		default:
			return 2, fmt.Errorf("coop tasks decisions: unknown flag %q (only -i / --interactive)", a)
		}
	}
	var decisions []taskItem
	for _, t := range readTaskTree(root) {
		if t.State == stateBlocked {
			decisions = append(decisions, t)
		}
	}
	if len(decisions) == 0 {
		ui.OK("no open decisions — nothing is blocked")
		return 0, nil
	}
	if interactive {
		return decisionsInteractive(root, decisions)
	}
	p := ui.For(os.Stdout)
	for n, t := range decisions {
		question := t.Title
		rec := ""
		for _, line := range strings.Split(readFileString(filepath.Join(t.Dir, "decision.md")), "\n") {
			if q, ok := strings.CutPrefix(line, "# Decision:"); ok {
				question = strings.TrimSpace(q)
			}
			if r, ok := strings.CutPrefix(line, "**Recommendation:**"); ok {
				rec = strings.TrimSpace(r)
			}
		}
		if n > 0 {
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
	fmt.Println() // a blank line sets the footer apart from the last decision
	ui.Note(`%s — answer with 'coop tasks unblock <id> "<answer>"', or 'coop tasks decisions -i' to answer interactively`, ui.Count(len(decisions), "open decision"))
	return 0, nil
}

// decisionRef locates one open decision for the browser: the queue root that owns it, the task
// id (re-resolved on each visit, so a concurrent move is caught), and a display label naming the
// queue in a multi-queue session ("" when there is only one queue — no label needed).
type decisionRef struct{ root, label, id string }

// decisionRefs turns one queue's blocked tasks into browser refs, labeled for the roll-up.
func decisionRefs(root, label string, decisions []taskItem) []decisionRef {
	refs := make([]decisionRef, len(decisions))
	for i, t := range decisions {
		refs[i] = decisionRef{root: root, label: label, id: t.ID}
	}
	return refs
}

// decisionsInteractive walks the open decisions one at a time on a tty (`coop tasks decisions -i`):
// each is shown in full, an answer is read and recorded (unblocking the task), and :n / :p move
// between them, :q stops. It needs a real terminal — in a pipe or the unattended loop there's
// nobody to answer, so it errors instead of hanging.
func decisionsInteractive(root string, decisions []taskItem) (int, error) {
	if !ui.IsTerminal(os.Stdin) {
		return 2, errors.New("coop tasks decisions -i needs an interactive terminal")
	}
	return runDecisionBrowser(decisionRefs(root, "", decisions), os.Stdin, os.Stdout)
}

// runDecisionBrowser is the interactive loop with its I/O injected, so it can be driven in a
// test. Each ref carries its own queue root, so one session can span several queues.
func runDecisionBrowser(refs []decisionRef, in io.Reader, out io.Writer) (int, error) {
	p := ui.For(os.Stdout)
	sc := bufio.NewScanner(in)
	answered, doneCount := 0, 0
	for i := 0; i >= 0; {
		ref := refs[i]
		t, err := findTask(ref.root, ref.id)
		if err != nil {
			return -1, err
		}
		decPath := filepath.Join(t.Dir, "decision.md")
		where := t.ID
		if ref.label != "" {
			where = ref.label + " · " + t.ID // say which queue this decision lives in
		}
		fmt.Fprintf(out, "\n%s\n", decisionDivider(p, i+1, len(refs), where))
		fprintDecisionBody(out, p, readFileString(decPath))
		if decisionResolved(decPath) {
			fmt.Fprintln(out, p.Green("✓ answered")+p.Dim(" — type a new answer to change it"))
		}
		key := func(k string) string { return p.Cyan(k) }
		fmt.Fprintf(out, "%s%s%s%s%s%s%s%s%s%s%s",
			p.Dim("answer ("), key("Enter"), p.Dim("=skip · "), key(":d"), p.Dim(" done · "),
			key(":n"), p.Dim(" next · "), key(":p"), p.Dim(" prev · "), key(":q"), p.Dim(" quit): "))
		if !sc.Scan() {
			break // EOF / ^D ends the session
		}
		line := strings.TrimSpace(sc.Text())
		// :d [reason] marks the current task done — done is terminal, so a reason is optional. Record
		// the reason into decision.md first (if given), then move the folder to 99_done/.
		if line == ":d" || strings.HasPrefix(line, ":d ") {
			if reason := strings.TrimSpace(strings.TrimPrefix(line, ":d")); reason != "" {
				if err := recordResolution(decPath, reason); err != nil {
					return -1, err
				}
			}
			if err := moveTaskDir(ref.root, t, stateDone); err != nil {
				return -1, err
			}
			doneCount++
			if i++; i >= len(refs) {
				i = -1
			}
			continue
		}
		switch line {
		case ":q", ":quit":
			i = -1
		case ":p":
			if i > 0 {
				i--
			}
		case ":n", "":
			i++
		default:
			if t.State == stateBlocked {
				if err := resolveAndUnblock(ref.root, t, line); err != nil {
					return -1, err
				}
				answered++
			} else if err := recordResolution(decPath, line); err != nil {
				return -1, err
			}
			// No per-answer confirmation line: auto-advancing to the next decision (its "── decision
			// N ──" header, drawn below) is the acknowledgement, and a "✓ recorded" line would just
			// scroll onto that header. Re-viewing with :p shows the "✓ answered" marker, and the
			// closing summary counts what was answered.
			i++
		}
		if i >= len(refs) {
			i = -1 // past the last decision → done
		}
	}
	switch {
	case answered > 0 && doneCount > 0:
		ui.OK("answered %s (back in todo) · marked %s done", ui.Count(answered, "decision"), ui.Count(doneCount, "task"))
	case answered > 0:
		ui.OK("answered %s — back in todo (claim to start)", ui.Count(answered, "decision"))
	case doneCount > 0:
		ui.OK("marked %s done", ui.Count(doneCount, "task"))
	default:
		ui.Note("no decisions answered — all still blocked")
	}
	return 0, nil
}

// fprintDecisionBody renders a decision.md for the browser: HTML comments stripped, the
// `# Decision:` question bold, the Blocks / Resolution / `---` lines dropped (the id is in the
// header and the answer is what we're collecting), and the rest indented.
func fprintDecisionBody(out io.Writer, p ui.Palette, content string) {
	prevBlank := false
	for _, raw := range strings.Split(stripHTMLComments(content), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			prevBlank = true
			continue
		}
		if strings.HasPrefix(line, "**Resolution:**") || strings.HasPrefix(line, "**Blocks:**") || line == "---" {
			continue
		}
		if prevBlank {
			fmt.Fprintln(out)
		}
		prevBlank = false
		if q, ok := strings.CutPrefix(line, "# Decision:"); ok {
			fmt.Fprintln(out, p.Bold(strings.TrimSpace(q)))
		} else {
			fmt.Fprintln(out, "  "+line)
		}
	}
}

// stripHTMLComments removes <!-- … --> spans (the per-file header coop seeds and the inline
// placeholder on the Resolution line), so the browser shows only human-meaningful content.
func stripHTMLComments(s string) string {
	for {
		i := strings.Index(s, "<!--")
		if i < 0 {
			return s
		}
		end := strings.Index(s[i:], "-->")
		if end < 0 {
			return s[:i] // unterminated — drop the rest
		}
		s = s[:i] + s[i+end+len("-->"):]
	}
}

// taskShapeIssues reports a required section (taskSections) whose **Heading:** is absent from a
// task.md body — the structural half of "self-contained." It does NOT flag an unfilled `<…>`
// placeholder: a fresh scaffold is lint-clean by design (you add, then fill), and the loop doesn't
// gate on lint. Derived from taskSections so lint and the scaffold share one shape source.
func taskShapeIssues(body string) []string {
	var issues []string
	for _, s := range taskSections {
		if !strings.Contains(body, "**"+s.heading+":**") {
			issues = append(issues, "missing the **"+s.heading+"** section")
		}
	}
	return issues
}

func tasksFolderLint(root string) (int, error) {
	items := readTaskTree(root)
	var findings []string
	add := func(id, msg string) { findings = append(findings, fmt.Sprintf("  %s: %s", id, msg)) }
	// Every queue needs all four state dirs, or the move-a-folder-between-states protocol renames a
	// task into a missing dir and silently corrupts the queue (see scaffoldStateDirs). Split slices and
	// seeded fork queues now scaffold them up front; flag any older tree that predates the fix.
	if fi, err := os.Stat(root); err == nil && fi.IsDir() {
		for _, st := range taskStates {
			if s, e := os.Stat(filepath.Join(root, st)); e != nil || !s.IsDir() {
				add(st, "state dir is missing — the move protocol will corrupt the queue; run 'coop init' (or re-run split)")
			}
		}
	}
	for _, t := range items {
		body := readFileString(filepath.Join(t.Dir, "task.md"))
		// blocked ⇒ a decision.md is present. A RESOLVED decision.md rides along as the audit trail
		// once unblocked (todo→in_progress→done); only an UNRESOLVED one on a non-blocked task is the
		// inconsistency — an open one-way door waiting in the queue instead of parked in 50_blocked/.
		if t.State == stateBlocked && !t.HasDecision {
			add(t.ID, "blocked but has no decision.md — add one, or unblock it")
		}
		if t.State == stateTodo && t.HasDecision && !decisionResolved(filepath.Join(t.Dir, "decision.md")) {
			add(t.ID, "has an unresolved decision.md but is todo — block it (or resolve it and unblock)")
		}
		// a status field is forbidden — the directory IS the status
		if fields, _ := splitFrontmatter(body); fields["status"] != "" {
			add(t.ID, "has a `status:` field — remove it; the parent directory is the status")
		}
		// self-contained: every shape section present and filled — not still a `<…>` placeholder (not
		// for done, which is the shipped record). Supersedes the old acceptance-substring-only check.
		if t.State != stateDone {
			for _, issue := range taskShapeIssues(body) {
				add(t.ID, "not self-contained: "+issue)
			}
		}
	}
	// An id sitting in TWO state dirs is a copy mistake (cp instead of a coop move). readTaskTree
	// deliberately masks it — its dedup exists for torn mid-rename reads — so lint is where the
	// persistent case surfaces (duplicateTaskIDs re-checks, so a task mid-move never flags).
	dups := duplicateTaskIDs(root)
	for _, id := range slices.Sorted(maps.Keys(dups)) {
		add(id, fmt.Sprintf("exists in %s — a task lives in ONE state dir; only the %s copy is listed, so keep the real one and delete the rest",
			strings.Join(dups[id], " AND "), dups[id][0]))
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
