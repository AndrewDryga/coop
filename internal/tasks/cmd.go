package tasks

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	osuser "os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
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
	flags  []string // the options this subcommand accepts, and the ones a correction may suggest
	maxPos int      // how many positionals it takes
	usage  string   // its syntax line, shown when an argument is missing or extra
}

// taskArgSpecs validates the structured `coop tasks` subcommands so an unsupported flag or a stray
// argument fails loudly instead of being silently ignored or mistaken for an id. add takes a
// free-form title that may start with "-"; block, claim, rm, unblock and decisions validate their
// own grammar (their flags take values), so those commands are intentionally absent.
var taskArgSpecs = map[string]taskArgSpec{
	"ls":      {lsFlags, 0, "coop tasks ls [<options>...]"},
	"lint":    {nil, 0, "coop tasks lint"},
	"release": {nil, 1, "coop tasks release <task-id>"}, "path": {nil, 1, "coop tasks path <task-id>"},
	"done": {nil, 1, "coop tasks done <task-id>"},
}

// lsFlags are the flags `coop tasks ls` accepts: --all (uncap the done archive) plus a per-state
// filter. The filter flags name the lifecycle states (todo/in-progress/blocked/done); pass several
// to union them. Shared by the single-queue validator (taskArgSpecs) and the umbrella roll-up
// (tasksListAll) so both accept the same set.
var lsFlags = []string{"--all", "--todo", "--in-progress", "--blocked", "--done"}

// lsStateFlags maps each ls filter flag to the lifecycle state it selects.
var lsStateFlags = map[string]string{
	"--todo":        StateTodo,
	"--in-progress": StateInProgress,
	"--blocked":     StateBlocked,
	"--done":        StateDone,
}

// taskStateFilter reads the --todo/--in-progress/--blocked/--done flags out of args into the set of
// states to show, in lifecycle order. Empty (no filter flag given) means "show every state".
func taskStateFilter(args []string) []string {
	want := map[string]bool{}
	for flag, state := range lsStateFlags {
		if slices.Contains(args, flag) {
			want[state] = true
		}
	}
	var states []string
	for _, state := range TaskStates { // emit in lifecycle order, deduped
		if want[state] {
			states = append(states, state)
		}
	}
	return states
}

// filterLabel names the filtered states for the "No <…> tasks." note when a filter matches nothing.
// It reads the states as a person says them — "in progress", not the directory's "in_progress".
func filterLabel(only []string) string {
	labels := make([]string, len(only))
	for i, s := range only {
		labels[i] = strings.ReplaceAll(StateLabel(s), "_", " ")
	}
	return strings.Join(labels, "/")
}

// validateArgs enforces a subcommand's grammar from its spec: any token starting with "-" must be
// one of its options, and at most spec.maxPos positionals are allowed. So `coop tasks ls --done` or
// `coop tasks release a b` is refused — naming the FIRST extra argument — rather than quietly doing
// the wrong thing. cmd is the command path without "coop".
func validateArgs(cmd string, args []string, spec taskArgSpec) error {
	pos := 0
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			if !slices.Contains(spec.flags, a) {
				return unknownOptionErr(a, "coop "+cmd, spec.flags)
			}
			continue
		}
		pos++
		if pos > spec.maxPos {
			return ui.UnexpectedArgument(a, "coop "+cmd, spec.usage)
		}
	}
	return nil
}

func CmdTasksFolder(repo, root string, rest []string) (int, error) {
	sub := ""
	var args []string
	if len(rest) > 0 {
		sub = rest[0]
		args = rest[1:]
	}
	// Reject unsupported flags / stray arguments up front for the structured subcommands (see
	// taskArgSpecs); add/rm/unblock/decisions own their argument parsing.
	if spec, ok := taskArgSpecs[sub]; ok {
		if err := validateArgs("tasks "+sub, args, spec); err != nil {
			return 2, err
		}
	}
	switch sub {
	case "":
		return tasksFolderList(root, false) // bare `coop tasks` lists the queue (a useful default view; see rule)
	case "ls":
		return tasksFolderList(root, slices.Contains(args, "--all"), taskStateFilter(args)...)
	case "lint":
		return tasksFolderLint(root)
	case "add":
		return tasksFolderAdd(root, args, StateTodo, "tasks add")
	case "claim":
		return tasksFolderClaim(root, args)
	case "release":
		return tasksFolderRelease(root, args)
	case "lease":
		return tasksFolderLease(root, args)
	case "block":
		return tasksFolderBlock(root, args)
	case "unblock":
		return tasksFolderUnblock(root, args)
	case "done":
		return tasksFolderMoveWith(root, args, StateDone, "done", "done", claimOptions{
			actor: captureClaimActor(realClaimActorProbe, os.Getppid(), ui.IsTerminal(os.Stdin), ClaimActor{}),
		})
	case "path":
		return tasksFolderPath(root, args)
	case "rm":
		return tasksFolderRemove(root, args)
	case "decisions":
		return tasksFolderDecisions(root, args)
	default:
		return 2, unknownSubcommandErr("tasks", sub, TasksVerbs)
	}
}

// tasksVerbs are the canonical `coop tasks` subcommands (primary spellings, no aliases): the single
// source for the unknown-subcommand suggester and isTasksSubcommand, so the two can't drift. `watch`
// belongs here even though cmdTasks (not cmdTasksFolder) handles it — a mistype of it should suggest it.
var TasksVerbs = []string{"ls", "lint", "add", "claim", "release", "lease", "block", "unblock", "done", "watch", "queues", "path", "rm", "decisions"}

// isTasksSubcommand reports whether s names a `coop tasks` subcommand. cmdTasks uses it to catch
// `coop tasks --tasks <sub>`, where --tasks swallows the subcommand as a queue path. v3 keeps no
// compat aliases (ls/rm are the only spellings), so this is plain tasksVerbs membership.
func isTasksSubcommand(s string) bool {
	return slices.Contains(TasksVerbs, s)
}

// matchTask resolves id against a set of task items: an exact ID match wins, else a unique substring
// match (so a slug fragment works). Ambiguous or absent is an error. listCmd names the command that
// lists this set ("coop tasks" / "coop backlog"), so the "run '…' to list" hint points at the right
// place. Shared by findTask (the lifecycle tree) and findBacklogTask (xx_backlog).
func MatchTask(items []Item, id, listCmd string) (Item, error) {
	if id == "" {
		// An empty fragment would substring-match every task ("" is in everything); make it a
		// clear error instead of silently acting on the first/only one.
		return Item{}, fmt.Errorf("need a task id (run '%s' to list)", listCmd)
	}
	for _, t := range items {
		if t.ID == id {
			return t, nil
		}
	}
	var hits []Item
	for _, t := range items {
		if strings.Contains(t.ID, id) {
			hits = append(hits, t)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return Item{}, fmt.Errorf("no task matching %q (run '%s' to list)", id, listCmd)
	default:
		var ids []string
		for _, h := range hits {
			ids = append(ids, h.ID)
		}
		return Item{}, fmt.Errorf("%q matches %d tasks: %s — be more specific", id, len(hits), strings.Join(ids, ", "))
	}
}

// findTask locates a task by ID across the lifecycle state dirs — an exact ID match, else a unique
// substring match. Backlog (xx_backlog) is deliberately NOT searched: it's off to the side, so the
// active id-commands (claim/done/…) can't accidentally act on an un-promoted idea. See findBacklogTask.
func FindTask(root, id string) (Item, error) {
	items, err := ReadTaskTree(root)
	if err != nil {
		return Item{}, err
	}
	return MatchTask(items, id, "coop tasks")
}

// findBacklogTask locates a backlog item by ID under root's xx_backlog/ — the backlog analog of
// findTask, so `coop backlog rm/promote` accept a slug fragment and error clearly on absent/ambiguous.
func findBacklogTask(root, id string) (Item, error) {
	items, err := ReadBacklog(root)
	if err != nil {
		return Item{}, err
	}
	return MatchTask(items, id, "coop backlog")
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

// addOptions are the options `coop tasks add` / `coop backlog add` accept, derived from the section
// flags that ARE the task shape — so a correction can never suggest a flag the parser would refuse.
var addOptions = func() []string {
	out := make([]string, 0, len(taskSections)+1)
	for _, s := range taskSections {
		out = append(out, "--"+s.flag)
	}
	return append(out, "--subtask")
}()

// claimOptionNames are `coop tasks claim`'s options, for the same reason.
var claimOptionNames = []string{"--as", "--pid", "--force"}

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

// sectionsFilled reports whether every taskSection already carries real content — i.e. taskBody will
// render no `<…>` section placeholder, so the fill-me header would be an order with nothing left to do.
func sectionsFilled(values map[string]string) bool {
	for _, s := range taskSections {
		if strings.TrimSpace(values[s.heading]) == "" {
			return false
		}
	}
	return true
}

// newTaskFiles is the set of starter files `coop tasks add` writes into a new task folder: the
// required task.md plus a seeded log.md and state.md. log.md and state.md always open with an
// HTML-comment header that explains the file and shows its format, so the file is self-documenting
// yet renders clean once filled. task.md's header is the fill-me INSTRUCTION, so it's seeded only
// for the placeholder scaffold: a task that arrives already filled (structured `add` flags, an
// imported fork proposal) has nothing to replace, and the header would be the first thing its agent
// reads telling it to redo work that's done. The full reference with worked examples is
// .agent/tasks/README.md. decision.md is NOT seeded here — `block` writes it, since a pending
// decision is what moves a task to 50_blocked/ (and a decision.md on a todo task is a lint error).
// values/subtasks fill the body from structured `add` flags; pass nil/empty for the scaffold.
func newTaskFiles(id, title, now string, values map[string]string, subtasks []string) map[string]string {
	taskMD := "---\nid: " + id + "\ntitle: " + title + "\nlabels: []\nupdated: " + now + "\n---\n\n" +
		"# " + title + "\n\n" + taskBody(values, subtasks)
	if !sectionsFilled(values) {
		taskMD = "<!-- TASK SPEC — a fresh agent must work this from this file ALONE.\n" +
			"     FIRST, BEFORE ANY CODE: replace every <…> placeholder below — the real problem and\n" +
			"     where it lives (Context), what proves it's done incl. a green gate (Acceptance), and\n" +
			"     the boring plan (Approach). This thinking IS step one, not a formality. Can't fill it\n" +
			"     honestly? It isn't ready — run: coop tasks block " + id + "\n" +
			"     Full format + examples: .agent/tasks/README.md -->\n" + taskMD
	}
	return map[string]string{
		"task.md": taskMD,
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

// projectName selected the queue this task is created in; the created path below already shows
// which one it landed in, so the name is not repeated in the result.
func tasksFolderAddWithProject(root string, args []string, state, cmdLabel, _ string) (int, error) {
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
			return 2, unknownOptionErr(a, "coop "+cmdLabel, addOptions)
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
		} else if values[heading] != "" {
			// Repeated section flag (e.g. several --acceptance): ACCUMULATE, don't overwrite.
			// A silent last-wins dropped every earlier value — real data loss on a multi-clause
			// paste. Join as paragraphs so each clause survives under its heading.
			values[heading] += "\n\n" + val
		} else {
			values[heading] = val
		}
	}
	title := strings.TrimSpace(strings.Join(titleWords, " "))
	if title == "" {
		return 2, ui.MissingArgument("title", "coop "+cmdLabel, "coop "+cmdLabel+` "<title>"`)
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
	id, err := createTaskFolder(root, state, slug, title, values, subtasks)
	if err != nil {
		if errors.As(err, &taskExistsError{}) {
			return 1, err
		}
		return -1, err
	}
	if state == StateBacklog {
		// A saved idea reports what was saved and where; only the unfilled scaffold asks for notes,
		// because only it has placeholders left to replace.
		ui.OK("Saved idea: %s", title)
		ui.Note("\n  %s", displayPath(filepath.Join(root, state, id, "task.md")))
		if !structured {
			ui.Note("\nAdd your notes to this file.")
		}
		return 0, nil
	}
	ui.OK("Created task: %s", title)
	ui.Note("\n  %s", displayPath(filepath.Join(root, state, id, "task.md")))
	if structured {
		return 0, nil // it arrived filled; there is nothing left to tell the author to do
	}
	// The scaffold's result is also the lesson: the exact command that would have filled it in.
	ui.Note("\nDescribe the problem, completion criteria, approach, and subtasks in this file.")
	ui.Note("Next time, you can fill them in when creating the task:\n")
	ui.Note("  coop tasks add %q \\", title)
	ui.Note("    --context \"An expired session retries forever.\" \\")
	ui.Note("    --acceptance \"An expired session returns to sign-in.\" \\")
	ui.Note("    --approach \"Stop retrying after an authentication failure.\" \\")
	ui.Note("    --subtask \"Test an expired session.\"")
	ui.Note("\nFor more details see:")
	ui.Note("  coop help tasks add")
	return 0, nil
}

// taskExistsError is the id-collision refusal `coop tasks add` reports as a user error (exit 1).
type taskExistsError struct{ msg string }

func (e taskExistsError) Error() string { return e.msg }

// createTaskFolder writes a new task folder <date>-<slug> under root/state from the given body values
// and returns its id. Shared by `coop tasks add`, `coop backlog add`, and the in-box task channel, so
// every path creates the same files with the same collision rule.
func createTaskFolder(root, state, slug, title string, values map[string]string, subtasks []string) (string, error) {
	id := time.Now().Format("2006-01-02") + "-" + slug
	// An id is a stable, unique handle, so reject a collision in ANY state — the four lifecycle dirs
	// AND xx_backlog — else a re-add (or a promote) would make two folders share an id, and
	// findTask/findBacklogTask would silently shadow one.
	for _, st := range TaskStates {
		if pathExists(filepath.Join(root, st, id)) {
			return "", taskExistsError{fmt.Sprintf("task %q already exists in %s/", id, st)}
		}
	}
	if pathExists(filepath.Join(root, StateBacklog, id)) {
		return "", taskExistsError{fmt.Sprintf("task %q already exists in %s/ — promote it (coop backlog promote %s) instead of re-adding", id, StateBacklog, id)}
	}
	// Ensure all four state dirs exist (the queue may be fresh, or predate the four-state scaffold), so
	// the move-a-folder-between-states protocol always has a real dir to move into — same guarantee as
	// `coop init`. Then the task's own todo dir.
	if err := ScaffoldStateDirs(root); err != nil {
		return "", err
	}
	// The target dir: stateTodo lives under scaffoldStateDirs above; xx_backlog is created on demand
	// here (like a fresh secondary queue), so `coop init` never has to scaffold an empty backlog drawer.
	dir := filepath.Join(root, state, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for name, content := range newTaskFiles(id, title, time.Now().Format(time.RFC3339), values, subtasks) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	return id, nil
}

// taskOwnerIdentity is the best-effort "who is claiming this" pair `coop tasks claim` records: no
// network call, no config lookup, nothing that could block or slow a claim down — a claim that
// can't identify its human still durably reserves the task, it just reserves it for "unknown".
func taskOwnerIdentity() (user, host string) {
	for _, key := range []string{"USER", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			user = v
			break
		}
	}
	if user == "" {
		if u, err := osuser.Current(); err == nil {
			user = strings.TrimSpace(u.Username)
		}
	}
	if user == "" {
		user = "unknown"
	}
	if h, err := os.Hostname(); err == nil {
		host = strings.TrimSpace(h)
	}
	if host == "" {
		host = "unknown"
	}
	return user, host
}

// claimOptions is what `coop tasks claim` learned from its flags and its own process tree: the
// actor the claim binds to (zero for a person at a terminal) and whether a live competing claim
// may be taken over.
type claimOptions struct {
	actor ClaimActor
	force bool
}

var errTaskClaimedByOther = errors.New("task is claimed by another live process")

// competingClaim reports whether an existing claim blocks a new one: only a claim bound to a
// process that is still alive and is not the claimer's own does. A person's claim (no process) or
// a claim whose process is gone is taken over silently, as every re-claim was before claims
// carried an identity — the point of binding is to tell "someone is on this" from "someone was".
func competingClaim(existing TaskOwnerRecord, actor ClaimActor) bool {
	if existing.Kind != TaskOwnerHuman || existing.ActorPID == 0 || !ownerProcessLive(existing) {
		return false
	}
	return existing.ActorPID != actor.PID || existing.ActorStart != actor.StartToken
}

// claimTaskOwnerRecord writes durable evidence that a HUMAN — not a loop-adopted process — owns
// this task, so assignLoopTaskOnly refuses to adopt it even long after the `coop tasks claim`
// process that called this has exited. Called ONLY from the interactive claim path below: the
// loop's own todo->in_progress adoption (assignLoopTaskOnly -> moveTaskDir) must never call this, or
// the loop would lock itself out of its own resumed work. It returns the label of a previous owner
// whose process was gone, so the caller can say the claim was a takeover.
func claimTaskOwnerRecord(root, id string, opts claimOptions) (string, error) {
	lock, err := lockTaskOwner(root, id)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	replaced := ""
	if record, ok, err := lock.Read(); err != nil {
		return "", err
	} else if ok && record.Kind == TaskOwnerFork {
		return "", fmt.Errorf("%w: %s", ErrTaskSandboxOwned, TaskOwnerLabel(record))
	} else if ok && !opts.force && competingClaim(record, opts.actor) {
		return "", fmt.Errorf("%w: %s is %s — release it first (coop tasks release %s) or take it over with --force",
			errTaskClaimedByOther, id, TaskOwnerLabel(record), id)
	} else if ok && record.ActorPID != 0 && !ownerProcessLive(record) {
		replaced = TaskOwnerLabel(record)
	}
	item, ok, err := CurrentTask(root, id)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("task changed before its claim could be recorded")
	}
	instance, err := EnsureTaskInstance(root, item)
	if err != nil {
		return "", err
	}
	user, host := taskOwnerIdentity()
	return replaced, lock.Write(TaskOwnerRecord{
		Version:    taskOwnershipRecordVersion,
		TaskID:     id,
		Kind:       TaskOwnerHuman,
		Task:       &instance,
		Source:     taskOwnerSourceInteractiveClaim,
		User:       user,
		Host:       host,
		ClaimedAt:  time.Now(),
		Actor:      opts.actor.Label,
		ActorPID:   opts.actor.PID,
		ActorStart: opts.actor.StartToken,
	})
}

// claimedBy names the actor a claim bound to, for the result's "Claimed by:" row. A claim at a
// terminal binds to nothing and has no label — the row is omitted rather than filled with a guess.
func claimedBy(actor ClaimActor) string {
	switch {
	case actor.PID != 0:
		return fmt.Sprintf("%s (PID %d)", actor.Label, actor.PID)
	case actor.Label != "":
		return actor.Label
	}
	return ""
}

// claimSuffix names the actor a lease bound to, for `coop tasks lease`'s progress lines.
func claimSuffix(actor ClaimActor) string {
	if who := claimedBy(actor); who != "" {
		return " as " + who
	}
	return ""
}

// parseClaimArgs reads `coop tasks claim <id> [--as <label>] [--pid <n>] [--force]`.
func parseClaimArgs(args []string) (string, claimOptions, error) {
	var id string
	var opts claimOptions
	for i := 0; i < len(args); i++ {
		key, value, hasValue := strings.Cut(args[i], "=")
		switch key {
		case "--as", "--pid":
			if !hasValue {
				if i+1 >= len(args) {
					return "", opts, fmt.Errorf("coop tasks claim: %s needs a value", key)
				}
				i++
				value = args[i]
			}
			if key == "--as" {
				if label := claimActorLabel(value); label == "" || label != value {
					return "", opts, errors.New("coop tasks claim: --as takes a short label made of letters, digits, and the characters -_@.: only")
				}
				opts.actor.Label = value
				continue
			}
			n, err := strconv.Atoi(value)
			if err != nil || n <= 1 {
				return "", opts, errors.New("coop tasks claim: --pid takes the process id of the claiming agent")
			}
			opts.actor.PID = n
		case "--force":
			opts.force = true
		default:
			if strings.HasPrefix(args[i], "-") && args[i] != "-" {
				return "", opts, unknownOptionErr(args[i], "coop tasks claim", claimOptionNames)
			}
			if id != "" {
				return "", opts, ui.UnexpectedArgument(args[i], "coop tasks claim", "coop tasks claim <task-id>")
			}
			id = args[i]
		}
	}
	if id == "" {
		return "", opts, ui.MissingArgument("task ID", "coop tasks claim", "coop tasks claim <task-id>")
	}
	return id, opts, nil
}

// tasksFolderClaim is the `coop tasks claim` entry: it binds the claim to the process that made it
// (see captureClaimActor) before the move so the queue can show who holds the task and whether
// they are still alive.
func tasksFolderClaim(root string, args []string) (int, error) {
	id, opts, err := parseClaimArgs(args)
	if err != nil {
		return 2, err
	}
	requested := opts.actor.PID
	opts.actor = captureClaimActor(realClaimActorProbe, os.Getppid(), ui.IsTerminal(os.Stdin), opts.actor)
	if requested != 0 && opts.actor.PID == 0 {
		return 1, fmt.Errorf("coop tasks claim: no live process with a readable identity at pid %d — it is not running, or its identity cannot be read", requested)
	}
	return tasksFolderMoveWith(root, []string{id}, StateInProgress, "claim", "claimed", opts)
}

// tasksFolderMove relocates a task's folder to newState (claim/done). verb is the imperative used
// in the usage line ("claim"); pastVerb is the past tense for the success note ("claimed"). Moving
// to the state it's already in is a no-op note, not an error.
func tasksFolderMove(root string, args []string, newState, verb, pastVerb string) (int, error) {
	return tasksFolderMoveWith(root, args, newState, verb, pastVerb, claimOptions{})
}

// tasksFolderMoveWith is tasksFolderMove with the claim's actor and takeover choice; done and every
// non-CLI claim pass an empty claimOptions, which binds the claim to nothing.
func tasksFolderMoveWith(root string, args []string, newState, verb, pastVerb string, opts claimOptions) (int, error) {
	if len(args) < 1 {
		return 2, ui.MissingArgument("task ID", "coop tasks "+verb, "coop tasks "+verb+" <task-id>")
	}
	t, err := FindTask(root, args[0])
	if err != nil {
		return 1, err
	}
	if t.State == newState {
		switch newState {
		case StateDone:
			if err := CompleteTrustedTask(root, t); err != nil {
				return -1, trustedCompletionError(err, t.ID)
			}
			ui.Note("%s is already done.", t.Title)
		case StateInProgress:
			// Re-claiming a task already in progress (yours, or one the loop currently holds) is a
			// legitimate take-over, not a no-op: it (re)asserts durable ownership regardless of who
			// put it there.
			replaced, err := claimTaskOwnerRecord(root, t.ID, opts)
			if errors.Is(err, errTaskClaimedByOther) {
				return 1, err
			}
			if err != nil {
				return -1, fmt.Errorf("%s is already in progress, but recording your claim failed: %w", t.ID, err)
			}
			note := "Already in progress; the claim now belongs to you."
			if replaced != "" {
				note = "Previous owner: " + replaced
			}
			printClaimResult(t, opts.actor, note)
		default:
			ui.Note("%s is already %s.", t.Title, StateLabel(newState))
		}
		return 0, nil
	}
	if newState == StateDone {
		if _, err := stopOwnLeaseHolder(root, t, opts.actor); err != nil {
			return -1, err
		}
		if err := CompleteTrustedTask(root, t); err != nil {
			return -1, trustedCompletionError(err, t.ID)
		}
	} else {
		// A human claim must protect the task from the INSTANT it starts, not from whenever the
		// folder move happens to land — so the record is written FIRST, before the move, and rolled
		// back if the move then fails. A claim that didn't take must not leave a phantom owner
		// blocking the loop forever: fail-closed cuts both ways here (no gap while claiming, no
		// orphan record when claiming fails).
		if newState == StateInProgress {
			if _, err := claimTaskOwnerRecord(root, t.ID, opts); err != nil {
				if errors.Is(err, errTaskClaimedByOther) {
					return 1, err
				}
				return -1, fmt.Errorf("record claim ownership for %s: %w", t.ID, err)
			}
		}
		var err error
		if t.State == StateDone {
			err = moveTrustedTaskFromDone(root, t, newState)
		} else {
			err = MoveTaskDir(root, t, newState)
		}
		if err != nil {
			if newState == StateInProgress {
				_ = removeTaskOwnerRecord(root, t.ID) // best-effort: the claim never took effect
			}
			return -1, err
		}
	}
	switch newState {
	case StateInProgress:
		printClaimResult(t, opts.actor, "")
	case StateDone:
		ui.OK("Completed task: %s", t.Title)
		ui.Note("\n  %s", displayPath(filepath.Join(root, StateDone, t.ID)))
	default:
		ui.OK("%s %s", pastVerb, t.ID)
	}
	return 0, nil
}

// printClaimResult is the claim's result: who now holds the task, anything exceptional about how
// the claim was taken, and the id. A claim made at a terminal binds to no process, so it has no
// actor row to print — nothing is invented to fill the space.
func printClaimResult(t Item, actor ClaimActor, note string) {
	ui.OK("Claimed task: %s", t.Title)
	ui.Note("") // the headline's own blank line: what follows is conditional, so it can't carry it
	if who := claimedBy(actor); who != "" {
		ui.Note("  Claimed by: %s", who)
	}
	if note != "" {
		ui.Note("  %s", note)
	}
	ui.Note("  %s", t.ID)
}

// tasksFolderRelease is the explicit hand-back for a claim (the counterpart to claim): it returns
// the folder to 00_todo/ and drops the durable owner record, so the next agent — a loop iteration
// or a human — takes it as ordinary ready work. The task's instructions, state.md, log.md, decision
// history, evidence and tmp travel with the folder; only the claim is given up.
func tasksFolderRelease(root string, args []string) (int, error) {
	if len(args) < 1 {
		return 2, ui.MissingArgument("task ID", "coop tasks release", "coop tasks release <task-id>")
	}
	t, err := FindTask(root, args[0])
	if err != nil {
		return 1, err
	}
	switch t.State {
	case StateTodo:
		// Only an UNCLAIMED todo task is already returned: a claim on a todo task is still a claim
		// to give up, and takes the same guarded path below.
		if _, owned, err := ReadTaskOwnerRecord(root, t.ID); err != nil {
			return -1, fmt.Errorf("read owner record for %s: %w", t.ID, err)
		} else if !owned {
			ui.Note("%s is already in todo.", t.Title)
			return 0, nil
		}
	case StateInProgress:
	default:
		return 1, fmt.Errorf("%s is %s. %s", t.Title, StateLabel(t.State), releaseStateRemedy(t.State))
	}
	if err := ReleaseTrustedTask(root, t, captureClaimActor(realClaimActorProbe, os.Getppid(), ui.IsTerminal(os.Stdin), ClaimActor{})); err != nil {
		if errors.Is(err, ErrTaskLeased) || errors.Is(err, ErrTaskSandboxOwned) ||
			errors.Is(err, errTaskClaimedByOther) || errors.Is(err, errTaskChangedBeforeRelease) {
			return 1, err
		}
		return -1, err
	}
	ui.OK("Returned task to todo: %s", t.Title)
	ui.Note("\nIts progress and handoff notes are kept.")
	return 0, nil
}

// releaseStateRemedy says what to do instead for a task release will not touch.
func releaseStateRemedy(state string) string {
	if state == StateBlocked {
		return "Resolve its decision before returning it to todo."
	}
	return "Completed work stays in the archive."
}

var errTaskChangedBeforeRelease = errors.New("task changed before it could be returned to todo")

// ReleaseTrustedTask returns a live task to 00_todo/ under host task authority — the release
// counterpart of BlockTrustedTask, and deliberately the same shape. It stands down an own lease
// helper, takes the authority flock, and holds it together with the owner lock through re-resolving
// the exact task instance, the move, and clearing the claim, so a fork assignment cannot appear
// after a stale precheck and be stranded in todo. The claim is held ACROSS the move and dropped
// only afterwards: a crash in between leaves the task in todo still claimed, which an identical
// retry finishes. A fork-owned task, a task another controller leases, and a claim held by another
// live process are refused — release gives up YOUR claim; it never overrides someone else's, never
// stops a box, and never clears a fork assignment.
func ReleaseTrustedTask(root string, t Item, actor ClaimActor) error {
	if _, err := stopOwnLeaseHolder(root, t, actor); err != nil {
		return err
	}
	authority, err := lockLeaseAuthority(root, t.ID, true, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("task %s is %w", t.ID, ErrTaskLeased)
		}
		return err
	}
	ownerLock, err := lockTaskOwner(root, t.ID)
	if err != nil {
		return errors.Join(err, unlockLeaseFile(authority))
	}
	current, ok, err := CurrentTask(root, t.ID)
	if err != nil {
		return errors.Join(err, ownerLock.Close(), unlockLeaseFile(authority))
	}
	if !ok || current.State != t.State || current.Dir != t.Dir {
		return errors.Join(errTaskChangedBeforeRelease, ownerLock.Close(), unlockLeaseFile(authority))
	}
	record, owned, err := ownerLock.Read()
	if err != nil {
		return errors.Join(err, ownerLock.Close(), unlockLeaseFile(authority))
	}
	if owned && record.Kind == TaskOwnerFork {
		return errors.Join(
			fmt.Errorf("%w: cannot release %s while it is %s", ErrTaskSandboxOwned, t.ID, TaskOwnerLabel(record)),
			ownerLock.Close(), unlockLeaseFile(authority),
		)
	}
	if owned && competingClaim(record, actor) {
		return errors.Join(
			fmt.Errorf("%w: %s is %s, which is still running", errTaskClaimedByOther, t.ID, TaskOwnerLabel(record)),
			ownerLock.Close(), unlockLeaseFile(authority),
		)
	}
	if current.State != StateTodo {
		if err := MoveTaskDir(root, current, StateTodo); err != nil {
			return errors.Join(err, ownerLock.Close(), unlockLeaseFile(authority))
		}
	}
	if owned {
		if err := removeTaskOwnerRecordFile(root, t.ID); err != nil {
			return errors.Join(fmt.Errorf("task %s is now in todo, but clearing your claim failed: %w", t.ID, err), ownerLock.Close(), unlockLeaseFile(authority))
		}
	}
	return errors.Join(ownerLock.Close(), unlockLeaseFile(authority))
}

func trustedCompletionError(err error, id string) error {
	var recovery auditCompletionRecoveryError
	if errors.As(err, &recovery) {
		return err
	}
	return fmt.Errorf("%w — fix the obstruction, then retry: coop tasks done %s", err, id)
}

// tasksFolderPath prints a task's resolved folder — the id-command companion to `coop fork path`,
// so `cat "$(coop tasks path <id>)/task.md"` works for humans and hooks. Reuses findTask, so a slug
// fragment resolves and an absent/ambiguous id errors exactly like claim/done.
func tasksFolderPath(root string, args []string) (int, error) {
	if len(args) < 1 {
		return 2, ui.MissingArgument("task ID", "coop tasks path", "coop tasks path <task-id>")
	}
	t, err := FindTask(root, args[0])
	if err != nil {
		return 1, err
	}
	fmt.Println(t.Dir)
	return 0, nil
}

func parseTaskUnblockArgs(args []string) (id, answer string, err error) {
	if len(args) < 1 {
		return "", "", ui.MissingArgument("task ID", "coop tasks unblock", `coop tasks unblock <task-id> ["<answer>"]`)
	}
	for _, arg := range args[1:] {
		if arg != "-" && strings.HasPrefix(arg, "-") {
			return "", "", unknownOptionErr(arg, "coop tasks unblock", nil)
		}
	}
	return args[0], strings.TrimSpace(strings.Join(args[1:], " ")), nil
}

// tasksFolderUnblock moves a task out of 50_blocked/ back to 00_todo/ — but only if it's
// actually blocked, so a fat-fingered id can't silently reopen a done (or todo) task.
func tasksFolderUnblock(root string, args []string) (int, error) {
	id, answer, parseErr := parseTaskUnblockArgs(args)
	if parseErr != nil {
		return 2, parseErr
	}
	t, err := FindTask(root, id)
	if err != nil {
		return 1, err
	}
	if t.State == StateTodo {
		record, ok, readErr := ReadAuditReopenRecord(root, t.ID)
		if readErr != nil {
			return -1, fmt.Errorf(
				"inspect interrupted audit unblock for %s failed: %w — task remains todo; repair the host authority registry, then retry: coop tasks unblock %s",
				t.ID, readErr, t.ID,
			)
		}
		if ok && record.UnblockPending {
			if answer != "" {
				decision := filepath.Join(t.Dir, "decision.md")
				if err := recordResolution(decision, answer); err != nil {
					return -1, fmt.Errorf(
						"update interrupted audit unblock decision %q failed: %w — task remains todo with non-authorizing pending authority; repair the decision file, then retry: coop tasks unblock %s \"<answer>\"",
						decision, err, t.ID,
					)
				}
			}
			committed, err := finishPendingAuditUnblock(root, t, record)
			if err != nil && committed {
				return -1, fmt.Errorf(
					"interrupted audit unblock for %s activated and the task remains todo, but releasing its authority lock failed: %w — do not retry; inspect the current task path: coop tasks path %s",
					t.ID, err, t.ID,
				)
			}
			if err != nil {
				return -1, fmt.Errorf(
					"finish interrupted audit unblock for %s failed: %w — task remains todo with non-authorizing pending authority; repair the host authority or Git history named by the error, then retry: coop tasks unblock %s",
					t.ID, err, t.ID,
				)
			}
			ui.OK("Finished unblocking task: %s", t.Title)
			ui.Note("\n  Its pending audit authority is active. The task is in todo.")
			return 0, nil
		}
		if recovered, recoverErr := finishInterruptedForkUnblock(root, t); recoverErr != nil {
			return -1, fmt.Errorf("finish interrupted fork unblock for %s: %w", t.ID, recoverErr)
		} else if recovered {
			ui.OK("Finished unblocking task: %s", t.Title)
			ui.Note("\n  Its fork assignment is paused. The task is in todo.")
			return 0, nil
		}
	}
	if t.State != StateBlocked {
		return 1, fmt.Errorf("%s is not blocked (it's %s) — nothing to unblock", t.ID, StateLabel(t.State))
	}
	// The optional inline answer makes deciding one command — no open-file/edit/save round-trip.
	// Don't unblock into a state lint rejects: a todo task with an UNRESOLVED decision.md is the
	// inconsistency lint flags. Require a resolution — inline, or pre-written in decision.md — or
	// the task stays blocked.
	resolved, err := decisionResolved(filepath.Join(t.Dir, "decision.md"))
	if err != nil {
		return -1, err
	}
	if answer == "" && t.HasDecision && !resolved {
		return 2, fmt.Errorf("%s has no resolution yet — write the **Resolution:** in its decision.md, or pass it inline: coop tasks unblock %s \"<answer>\"", t.ID, args[0])
	}
	if err := resolveAndUnblock(root, t, answer); err != nil {
		return -1, unblockRetryError(t.ID, answer != "", err)
	}
	ui.OK("Unblocked task: %s", t.Title)
	if answer != "" {
		ui.Note("\nYour answer is saved in decision.md. The task is back in todo.")
	} else {
		ui.Note("\n  The task is back in todo.")
	}
	return 0, nil
}

// recordResolution writes a human's answer into decision.md's "**Resolution:**" line so that
// `coop tasks unblock <id> <answer>` resolves the decision in one step. It replaces an existing
// Resolution line in place (dropping the placeholder), or appends one if the file has none.
func recordResolution(decPath, answer string) error {
	line := "**Resolution:** " + answer
	taskRoot, err := OpenTaskMetadataRoot(filepath.Dir(decPath))
	if err != nil {
		return err
	}
	defer taskRoot.Close()
	bodyBytes, _, err := readOptionalTaskMetadataFile(taskRoot, filepath.Base(decPath))
	if err != nil {
		return err
	}
	body := string(bodyBytes)
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "**Resolution:**") {
			lines[i] = line
			out := strings.Join(lines, "\n")
			if !strings.HasSuffix(out, "\n") {
				out += "\n"
			}
			return AtomicWriteTaskFile(taskRoot, filepath.Base(decPath), []byte(out))
		}
	}
	out := line + "\n"
	if strings.TrimSpace(body) != "" {
		out = strings.TrimRight(body, "\n") + "\n\n" + line + "\n"
	}
	return AtomicWriteTaskFile(taskRoot, filepath.Base(decPath), []byte(out))
}

// decisionResolved reports whether a decision.md has a filled-in Resolution (a human's answer),
// as opposed to the empty/placeholder line `coop tasks block` seeds. lint uses it: a resolved
// decision rides along on an unblocked (todo) task as its audit trail, but an unresolved one on a
// non-blocked task is the inconsistency to flag.
func decisionResolved(decPath string) (bool, error) {
	taskRoot, err := OpenTaskMetadataRoot(filepath.Dir(decPath))
	if err != nil {
		return false, err
	}
	defer taskRoot.Close()
	body, exists, err := readOptionalTaskMetadataFile(taskRoot, filepath.Base(decPath))
	if err != nil || !exists {
		return false, err
	}
	for _, line := range strings.Split(string(body), "\n") {
		if r, ok := strings.CutPrefix(line, "**Resolution:**"); ok {
			r = strings.TrimSpace(r)
			return r != "" && !strings.HasPrefix(r, "<!--"), nil
		}
	}
	return false, nil
}

// decisionResolution returns the human's answer from a decision.md body, or "" when it carries only
// the placeholder. Shared with decisionResolved's predicate so "is it answered" and "what was the
// answer" can never disagree.
func decisionResolution(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if r, ok := strings.CutPrefix(line, "**Resolution:**"); ok {
			r = strings.TrimSpace(r)
			if r == "" || strings.HasPrefix(r, "<!--") {
				return ""
			}
			return r
		}
	}
	return ""
}

type unblockStageError struct {
	stage, artifact, state string
	err                    error
}

func (e *unblockStageError) Error() string { return e.err.Error() }
func (e *unblockStageError) Unwrap() error { return e.err }

func unblockRetryError(id string, hasAnswer bool, err error) error {
	var failure *unblockStageError
	if errors.As(err, &failure) && failure.state == StateTodo {
		return fmt.Errorf(
			"unblock %s moved the task to todo, but %s failed: %w — do not retry; inspect the current task path: coop tasks path %s",
			id, failure.stage, err, id,
		)
	}
	if failure != nil && failure.state == "" {
		return fmt.Errorf(
			"unblock %s failed during %s and could not restore a known task state: %w — inspect the current path before retrying: coop tasks path %s",
			id, failure.stage, err, id,
		)
	}
	command := "coop tasks unblock " + id
	if hasAnswer {
		command += ` "<answer>"`
	}
	stage := "unblock transition"
	remedy := "fix the task state named by the error"
	if errors.As(err, &failure) {
		stage = failure.stage
		switch failure.stage {
		case "audit authority validation":
			remedy = "restore the unchanged host audit record and reviewed Git baseline named by the error"
		case "decision write":
			remedy = fmt.Sprintf("make %q a writable regular decision file", failure.artifact)
		case "task folder move":
			remedy = "remove or repair the source/destination task folder named by the error"
		case "audit authority persistence":
			remedy = "repair the host task-authority registry entry named by the error"
		}
	}
	return fmt.Errorf(
		"unblock %s failed during %s: %w — task remains blocked; %s, then retry: %s",
		id, stage, err, remedy, command,
	)
}

// resolveAndUnblock records answer (if non-empty) into the task's decision.md, then returns the
// task to 00_todo/ — NOT 10_in_progress/: in_progress is the "an agent is on this" lock taken by
// `claim`, so a just-unblocked task with nobody on it belongs in the queue as available work; the
// resolved decision.md rides along as the audit trail. Shared by `unblock` and the -i browser.
func resolveAndUnblock(root string, t Item, answer string) error {
	transition, err := prepareBlockedAuditReopenUnblock(root, t)
	if err != nil {
		return &unblockStageError{stage: "audit authority validation", state: StateBlocked, err: err}
	}
	if answer != "" {
		decision := filepath.Join(t.Dir, "decision.md")
		if err := recordResolution(decision, answer); err != nil {
			return &unblockStageError{
				stage: "decision write", artifact: decision, state: StateBlocked,
				err: transition.finish(err),
			}
		}
	}
	return moveBlockedAuditUnblock(root, t, transition)
}

// moveBlockedAuditUnblock writes a non-authorizing pending record, moves the folder, then activates
// the replacement. A crash at either boundary remains fail-closed: blocked+pending is explicitly
// retryable, while todo+pending requires the explicit host unblock recovery under the same lock.
func moveBlockedAuditUnblock(root string, t Item, transition *blockedAuditUnblock) error {
	if err := transition.markPending(); err != nil {
		return &unblockStageError{
			stage: "audit authority persistence", state: StateBlocked,
			err: transition.finish(err),
		}
	}
	ownerLock, err := lockTaskOwner(root, t.ID)
	if err != nil {
		return &unblockStageError{
			stage: "owner record lock", state: StateBlocked,
			err: transition.finish(errors.Join(err, transition.restorePrevious())),
		}
	}
	ownerRecord, owned, err := ownerLock.Read()
	if err != nil {
		return &unblockStageError{
			stage: "owner record inspection", state: StateBlocked,
			err: transition.finish(errors.Join(err, ownerLock.Close(), transition.restorePrevious())),
		}
	}
	ownerChanged := false
	rollbackOwner := func() error {
		if !ownerChanged {
			return nil
		}
		return ownerLock.Write(ownerRecord)
	}
	if owned && ownerRecord.Kind == TaskOwnerFork {
		if ownerRecord.Fork == nil || (ownerRecord.Fork.Phase != ForkAssignmentBlocked && ownerRecord.Fork.Phase != ForkAssignmentPaused) {
			return &unblockStageError{
				stage: "owner record validation", state: StateBlocked,
				err: transition.finish(errors.Join(fmt.Errorf("fork assignment is not blocked or paused"), ownerLock.Close(), transition.restorePrevious())),
			}
		}
		if ownerRecord.Fork.Phase == ForkAssignmentBlocked {
			next := ownerRecord
			next.Fork.Phase = ForkAssignmentPaused
			next.Fork.CandidateID = ""
			next.Fork.ProjectionDigest = ""
			next.Fork.UpdatedAt = time.Now().UTC()
			if err := ownerLock.Write(next); err != nil {
				return &unblockStageError{
					stage: "fork assignment resume", state: StateBlocked,
					err: transition.finish(errors.Join(err, ownerLock.Close(), transition.restorePrevious())),
				}
			}
			ownerChanged = true
		}
	} else if owned {
		if err := removeTaskOwnerRecordFile(root, t.ID); err != nil {
			return &unblockStageError{
				stage: "owner record cleanup", state: StateBlocked,
				err: transition.finish(errors.Join(err, ownerLock.Close(), transition.restorePrevious())),
			}
		}
		ownerChanged = true
	}
	if err := MoveTaskDir(root, t, StateTodo); err != nil {
		return &unblockStageError{
			stage: "task folder move", state: StateBlocked,
			err: transition.finish(errors.Join(err, rollbackOwner(), ownerLock.Close(), transition.restorePrevious())),
		}
	}
	moved := t
	moved.State = StateTodo
	moved.Dir = filepath.Join(root, StateTodo, t.ID)
	if err := transition.persist(); err != nil {
		rollbackErr := MoveTaskDir(root, moved, StateBlocked)
		var recordRollbackErr error
		if rollbackErr == nil {
			recordRollbackErr = errors.Join(rollbackOwner(), transition.restorePrevious())
		}
		state := StateBlocked
		if rollbackErr != nil {
			state = ""
		}
		return &unblockStageError{
			stage: "audit authority persistence", state: state,
			err: transition.finish(errors.Join(err, rollbackErr, recordRollbackErr, ownerLock.Close())),
		}
	}
	if err := ownerLock.Close(); err != nil {
		return &unblockStageError{stage: "owner record lock release", state: StateTodo, err: transition.finish(err)}
	}
	if err := transition.finish(nil); err != nil {
		return &unblockStageError{stage: "host authority lock release", state: StateTodo, err: err}
	}
	return nil
}

// moveTaskDir renames a task's folder into root/newState, creating the state dir if needed. If the
// id already exists in newState (a torn move or a stray duplicate across states), it refuses with an
// actionable message rather than letting os.Rename fail with a raw "file exists" and stranding the
// task. (readTaskTree dedups such a duplicate on the READ side; this guards the WRITE side.)
func MoveTaskDir(root string, t Item, newState string) error {
	dest := filepath.Join(root, newState, t.ID)
	if t.Dir != dest && pathExists(dest) {
		return fmt.Errorf("can't move %s to %s/: a folder with that id already exists there (a torn move or stray copy) — remove one: rm -rf %q", t.ID, StateLabel(newState), dest)
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

// taskLocalPath resolves a child beneath taskDir and rejects traversal or absolute paths. Today
// cleanup passes the constant "tmp"; keeping the containment check at the deletion boundary makes
// that invariant explicit and prevents a future caller from turning task cleanup into an arbitrary
// path remover.
func taskLocalPath(taskDir, child string) (string, error) {
	if child == "" || filepath.IsAbs(child) {
		return "", fmt.Errorf("task-local path %q must be relative", child)
	}
	base, err := filepath.Abs(taskDir)
	if err != nil {
		return "", err
	}
	target, err := filepath.Abs(filepath.Join(base, child))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("task-local path %q escapes task folder %q", child, base)
	}
	return target, nil
}

// removeTaskTmp deletes only taskDir/tmp. The task folder itself must be a real directory, not a
// symlink; a tmp symlink is unlinked without following it, and os.RemoveAll likewise does not follow
// symlinks nested below a real tmp directory. Missing tmp is the normal case for existing tasks.
func removeTaskTmp(taskDir string) error {
	info, err := os.Lstat(taskDir)
	if err != nil {
		return fmt.Errorf("inspect task folder: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("refusing tmp cleanup through non-directory task folder %q", taskDir)
	}
	tmpDir, err := taskLocalPath(taskDir, "tmp")
	if err != nil {
		return err
	}
	tmpInfo, err := os.Lstat(tmpDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %q: %w", tmpDir, err)
	}
	if tmpInfo.Mode()&os.ModeSymlink != 0 {
		err = os.Remove(tmpDir)
	} else {
		err = os.RemoveAll(tmpDir)
	}
	if err != nil {
		return fmt.Errorf("remove %q: %w", tmpDir, err)
	}
	if _, err := os.Lstat(tmpDir); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("remove %q: path still exists", tmpDir)
		}
		return fmt.Errorf("verify removal of %q: %w", tmpDir, err)
	}
	return nil
}

// taskTmpCleaner is a narrow test seam for proving that every completion path propagates a cleanup
// error. Production always uses removeTaskTmp.
var taskTmpCleaner = removeTaskTmp

const (
	taskStateStatus = "**Status:**"
	taskStateDone   = "**Done so far:**"
	taskStateNext   = "**Next action:**"
	taskStateTraps  = "**Traps:**"
)

func normalizeCompletedTaskState(id, taskDir string) error {
	return NormalizeTaskState(id, taskDir, "complete", "none", "—", "—")
}

// normalizeTaskState atomically replaces only Coop-owned lifecycle fields in a valid state.md.
// Agent-authored summaries, traps, headings, and surrounding prose remain byte-for-byte apart
// from those two lines. Missing or ambiguous snapshots retain unique Done/Traps values when safe.
func NormalizeTaskState(id, taskDir, statusValue, nextValue, doneFallback, trapsFallback string) error {
	root, err := OpenTaskMetadataRoot(taskDir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := normalizeTaskStateRoot(id, root, statusValue, nextValue, doneFallback, trapsFallback); err != nil {
		return fmt.Errorf("normalize %q: %w", filepath.Join(taskDir, "state.md"), err)
	}
	return nil
}

func normalizeTaskStateRoot(id string, root *os.Root, statusValue, nextValue, doneFallback, trapsFallback string) error {
	body, err := ReadTaskMetadataFile(root, "state.md")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read state.md: %w", err)
	}
	lines := strings.Split(string(body), "\n")
	status := labeledLineIndexes(lines, taskStateStatus)
	done := labeledLineIndexes(lines, taskStateDone)
	next := labeledLineIndexes(lines, taskStateNext)
	traps := labeledLineIndexes(lines, taskStateTraps)
	var out string
	if err == nil && len(status) == 1 && len(done) == 1 && len(next) == 1 && len(traps) == 1 {
		lines[status[0]] = taskStateStatus + " " + statusValue
		lines[next[0]] = taskStateNext + " " + nextValue
		out = strings.Join(lines, "\n")
	} else {
		doneValue := uniqueLabeledValue(lines, taskStateDone, doneFallback)
		trapsValue := uniqueLabeledValue(lines, taskStateTraps, trapsFallback)
		out = fmt.Sprintf("# State — %s\n\n%s %s\n%s %s\n%s %s\n%s %s\n",
			id, taskStateStatus, statusValue, taskStateDone, doneValue, taskStateNext, nextValue, taskStateTraps, trapsValue)
	}
	if err == nil && string(body) == out {
		return nil
	}
	if err := AtomicWriteTaskFile(root, "state.md", []byte(out)); err != nil {
		return fmt.Errorf("write state.md: %w", err)
	}
	return nil
}

func labeledLineIndexes(lines []string, label string) []int {
	var indexes []int
	for i, line := range lines {
		if strings.HasPrefix(line, label) {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

func uniqueLabeledValue(lines []string, label, fallback string) string {
	indexes := labeledLineIndexes(lines, label)
	if len(indexes) != 1 {
		return fallback
	}
	value := strings.TrimSpace(strings.TrimPrefix(lines[indexes[0]], label))
	if value == "" {
		return fallback
	}
	return value
}

const taskMetadataFileLimit = 1 << 20

func OpenTaskMetadataRoot(taskDir string) (*os.Root, error) {
	before, err := os.Lstat(taskDir)
	if err != nil {
		return nil, fmt.Errorf("inspect task folder: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("refusing task metadata access through non-directory task folder %q", taskDir)
	}
	root, err := os.OpenRoot(taskDir)
	if err != nil {
		return nil, fmt.Errorf("open task folder: %w", err)
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		root.Close()
		if err != nil {
			return nil, fmt.Errorf("reinspect task folder: %w", err)
		}
		return nil, fmt.Errorf("task folder %q changed while opening", taskDir)
	}
	return root, nil
}

func ReadTaskMetadataFile(root *os.Root, name string) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err := validateTaskMetadataFile(name, before); err != nil {
		return nil, err
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("task metadata file %q changed while opening", name)
	}
	if err := validateTaskMetadataFile(name, after); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, taskMetadataFileLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > taskMetadataFileLimit {
		return nil, fmt.Errorf("task metadata file %q exceeds %d bytes", name, taskMetadataFileLimit)
	}
	return data, nil
}

func readOptionalTaskMetadataFile(root *os.Root, name string) ([]byte, bool, error) {
	if _, err := root.Lstat(name); errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	data, err := ReadTaskMetadataFile(root, name)
	return data, err == nil, err
}

func readOptionalTaskMetadataPath(path string) ([]byte, bool, error) {
	root, err := OpenTaskMetadataRoot(filepath.Dir(path))
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	return readOptionalTaskMetadataFile(root, filepath.Base(path))
}

func validateTaskMetadataFile(name string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !ok || stat.Nlink != 1 {
		return fmt.Errorf("task metadata file %q is not a single-link regular file", name)
	}
	if info.Size() < 0 || info.Size() > taskMetadataFileLimit {
		return fmt.Errorf("task metadata file %q exceeds %d bytes", name, taskMetadataFileLimit)
	}
	return nil
}

func AtomicWriteTaskFile(root *os.Root, name string, body []byte) error {
	var f *os.File
	var tmp string
	var err error
	for attempt := 0; attempt < 100; attempt++ {
		tmp = fmt.Sprintf(".%s-%d-%d-%d", name, os.Getpid(), time.Now().UnixNano(), attempt)
		f, err = root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if err == nil || !errors.Is(err, os.ErrExist) {
			break
		}
	}
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}

// finalizeCompletedTask is the single post-move completion boundary. State comes first so a failed
// metadata write retains tmp for diagnosis and retry; both operations are idempotent.
func finalizeCompletedTask(id, taskDir string) error {
	if err := normalizeCompletedTaskState(id, taskDir); err != nil {
		return fmt.Errorf("task %s reached done, but its state finalization failed: %w", id, err)
	}
	if err := taskTmpCleaner(taskDir); err != nil {
		return fmt.Errorf("task %s reached done, but its tmp cleanup failed: %w", id, err)
	}
	return nil
}

// ErrTaskLeased is the refusal every host mutation shares when another live controller holds the
// task's authority flock: "task <id> is leased by another controller".
var ErrTaskLeased = errors.New("leased by another controller")

var errTaskChangedBeforeBlock = errors.New("task changed before it could be blocked")

// BlockTrustedTask moves a live (non-done) task into 50_blocked/ under host task authority: it
// stands down an own lease holder, takes the authority flock, and holds it together with the owner
// lock through check, move, and owner-record removal, so an assignment cannot appear after a stale
// precheck and become stranded in blocked. Shared by `coop tasks block` and the in-box task
// channel; a task another controller leases is refused with errTaskLeasedElsewhere.
func BlockTrustedTask(root string, t Item, actor ClaimActor) error {
	if _, err := stopOwnLeaseHolder(root, t, actor); err != nil {
		return err
	}
	authority, err := lockLeaseAuthority(root, t.ID, true, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("task %s is %w", t.ID, ErrTaskLeased)
		}
		return err
	}
	ownerLock, err := lockTaskOwner(root, t.ID)
	if err != nil {
		return errors.Join(err, unlockLeaseFile(authority))
	}
	current, ok, err := CurrentTask(root, t.ID)
	if err != nil {
		return errors.Join(err, ownerLock.Close(), unlockLeaseFile(authority))
	}
	if !ok || current.State != t.State || current.Dir != t.Dir {
		return errors.Join(errTaskChangedBeforeBlock, ownerLock.Close(), unlockLeaseFile(authority))
	}
	record, owned, err := ownerLock.Read()
	if err != nil {
		return errors.Join(err, ownerLock.Close(), unlockLeaseFile(authority))
	}
	if owned && record.Kind == TaskOwnerFork {
		return errors.Join(
			fmt.Errorf("%w: cannot block %s while it is %s", ErrTaskSandboxOwned, t.ID, TaskOwnerLabel(record)),
			ownerLock.Close(), unlockLeaseFile(authority),
		)
	}
	if current.State != StateBlocked {
		if err := MoveTaskDir(root, current, StateBlocked); err != nil {
			return errors.Join(err, ownerLock.Close(), unlockLeaseFile(authority))
		}
	}
	if owned {
		if err := removeTaskOwnerRecordFile(root, t.ID); err != nil {
			return errors.Join(fmt.Errorf("task %s is now blocked, but clearing its owner record failed: %w", t.ID, err), ownerLock.Close(), unlockLeaseFile(authority))
		}
	}
	return errors.Join(ownerLock.Close(), unlockLeaseFile(authority))
}

// blockRequest is what `coop tasks block` learned from its arguments: the task, and — when the
// structured text flags are used — the complete decision request to save.
type blockRequest struct {
	id       string
	decision Decision
	filled   bool // a text flag was given, so Coop writes the decision instead of an editable stub
}

// blockTextFlags are the flags that carry a decision request. They are optional as a SET: with none
// of them `block` seeds the editable template, and with any of them the request must be complete.
var blockTextFlags = []string{"--question", "--option", "--recommendation"}

// parseBlockArgs reads `coop tasks block <id> [--question <text>] [--option <text>]... [--recommendation <text>]`,
// each also in its `--flag=value` spelling. Every check here runs BEFORE the task moves — including
// in the multi-queue dispatcher, which parses the same way — so a malformed request can never leave
// a task parked on a decision Coop could not write.
func parseBlockArgs(args []string) (blockRequest, error) {
	var req blockRequest
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			if req.id != "" {
				return req, errors.New("coop tasks block: too many arguments (one task id at most)")
			}
			req.id = arg
			continue
		}
		flag, value, hasValue := strings.Cut(arg, "=")
		if !slices.Contains(blockTextFlags, flag) {
			return req, fmt.Errorf("coop tasks block: unknown flag %q (known: %s)", arg, strings.Join(blockTextFlags, ", "))
		}
		if !hasValue {
			if i+1 >= len(args) {
				return req, fmt.Errorf("coop tasks block %s needs a value", flag)
			}
			i++
			value = args[i]
		}
		if flag != "--option" && seen[flag] {
			return req, fmt.Errorf("coop tasks block: %s can only be used once", flag)
		}
		seen[flag] = true
		req.filled = true
		value = strings.TrimSpace(value)
		switch flag {
		case "--question":
			req.decision.Question = value
		case "--recommendation":
			req.decision.Recommendation = value
		case "--option":
			if value != "" {
				req.decision.Options = append(req.decision.Options, value)
			}
		}
	}
	if req.id == "" {
		return req, errors.New("usage: coop tasks block <id>")
	}
	if req.filled {
		var missing []string
		if req.decision.Question == "" {
			missing = append(missing, "--question")
		}
		if len(req.decision.Options) == 0 {
			missing = append(missing, "--option")
		}
		if req.decision.Recommendation == "" {
			missing = append(missing, "--recommendation")
		}
		if len(missing) > 0 {
			return req, fmt.Errorf("coop tasks block: a decision request needs a question, at least one option and a recommendation — missing %s (or omit all three to write decision.md yourself)", strings.Join(missing, ", "))
		}
	}
	return req, nil
}

func tasksFolderBlock(root string, args []string) (int, error) {
	req, err := parseBlockArgs(args)
	if err != nil {
		return 2, err
	}
	t, err := FindTask(root, req.id)
	if err != nil {
		return 1, err
	}
	if t.State == StateDone {
		if err := refuseForkTaskOwner(root, t.ID, "block"); err != nil {
			return 1, err
		}
		if err := moveTrustedTaskFromDone(root, t, StateBlocked); err != nil {
			return -1, err
		}
	} else if err := BlockTrustedTask(root, t, captureClaimActor(realClaimActorProbe, os.Getppid(), ui.IsTerminal(os.Stdin), ClaimActor{})); err != nil {
		if errors.Is(err, ErrTaskLeased) || errors.Is(err, ErrTaskSandboxOwned) || errors.Is(err, errTaskChangedBeforeBlock) {
			return 1, err
		}
		return -1, err
	}
	taskDir := filepath.Join(root, StateBlocked, t.ID)
	dec := filepath.Join(taskDir, "decision.md")
	if req.filled {
		if err := saveRequestedDecision(taskDir, t.ID, t.Title, req.decision); err != nil {
			// The move already happened and is durable; say so, and say the request is not saved.
			// A conflict is the human's to resolve (1); anything else is a real failure (-1).
			code := -1
			if errors.Is(err, errDecisionAlreadyWritten) {
				code = 1
			}
			return code, fmt.Errorf("%s is blocked, but its decision request was not saved: %w", t.ID, err)
		}
	} else if !fileExists(dec) {
		stub := "<!-- A one-way-door choice that blocks this task. The agent fills The decision,\n" +
			"     Options, and Recommendation; a HUMAN decides — either write Resolution below and\n" +
			"     run 'coop tasks unblock " + t.ID + "', or do both in one step:\n" +
			"       coop tasks unblock " + t.ID + " \"A — go with Postgres\" -->\n\n" +
			"# Decision: " + t.Title + "?\n\n" +
			"**Blocks:** this task (`" + t.ID + "`).\n\n" +
			"**The decision:** " + decisionScaffoldMarker + "\n\n" +
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
	ui.OK("Blocked task: %s", t.Title)
	ui.Note("\n  %s", displayPath(dec))
	if !req.filled {
		ui.Note("\nWrite the question, options, and recommendation in this file.")
		ui.Note("To fill them in as you block a task, see:")
		ui.Note("\n  coop help tasks block")
	}
	return 0, nil
}

// taskRemoveSpec is `coop tasks rm`'s grammar: one id OR --all-done, both optionally unattended.
var taskRemoveSpec = taskArgSpec{[]string{"--all-done", "--yes", "-y"}, 1, "coop tasks rm <task-id> [--yes]"}

// decisionScaffoldMarker is the placeholder the editable stub leaves where the question goes. Its
// presence is how `block --question …` tells "nobody has written this decision yet" from "a human
// or an earlier agent wrote one", which it must never overwrite.
const decisionScaffoldMarker = "<what must be chosen, and why it can't be undone cheaply>"

// errDecisionAlreadyWritten is the refusal that protects a decision somebody already wrote — a
// human's answer, or an earlier agent's question. It is the caller's to resolve, not a failure.
var errDecisionAlreadyWritten = errors.New("the task already carries a decision somebody wrote")

// saveRequestedDecision writes a complete decision request into a blocked task's decision.md. It
// never destroys content somebody already wrote: the identical request again is a no-op (so a retry
// after a partial failure completes), and a DIFFERENT one is refused with the file to edit.
func saveRequestedDecision(taskDir, id, title string, d Decision) error {
	existing, err := os.ReadFile(filepath.Join(taskDir, "decision.md"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	case !strings.Contains(string(existing), decisionScaffoldMarker):
		if string(existing) == RenderDecision(id, title, d) {
			return nil // the same request again — already saved, nothing to do
		}
		return fmt.Errorf("%w — edit %s instead", errDecisionAlreadyWritten, displayPath(filepath.Join(taskDir, "decision.md")))
	}
	return WriteDecision(taskDir, id, title, d)
}

// displayPath renders an absolute path the way a person reading the output sees it: relative to the
// working directory when it sits beneath it (the ordinary case — coop runs at the project root),
// else the absolute path. The second attempt is for a symlinked working directory (macOS's
// /tmp -> /private/tmp): the task tree is resolved, the shell's cwd may not be, and printing an
// absolute path because of that alone would be noise.
func displayPath(abs string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return abs
	}
	if rel, ok := relativeTo(cwd, abs); ok {
		return rel
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil && resolved != cwd {
		if rel, ok := relativeTo(resolved, abs); ok {
			return rel
		}
	}
	return abs
}

// relativeTo reports path relative to base, and whether it actually sits beneath it.
func relativeTo(base, path string) (string, bool) {
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

type taskRemoveArgs struct {
	id      string
	allDone bool
	yes     bool
}

// Keep syntax independent of discovery: a typo must not reach an archive scan,
// confirmation or deletion, even when the archive is empty or spans many queues.
func parseTaskRemoveArgs(args []string) (taskRemoveArgs, error) {
	const usage = "usage: coop tasks rm <id> [--yes]  |  coop tasks rm --all-done [--yes]"
	if err := validateArgs("tasks rm", args, taskRemoveSpec); err != nil {
		return taskRemoveArgs{}, err
	}
	var request taskRemoveArgs
	for _, a := range args {
		switch a {
		case "--all-done":
			request.allDone = true
		case "--yes", "-y":
			request.yes = true
		default:
			if a == "" || a == "-" {
				return taskRemoveArgs{}, errors.New(usage)
			}
			request.id = a
		}
	}
	if request.allDone && request.id != "" || !request.allDone && request.id == "" {
		return taskRemoveArgs{}, errors.New(usage)
	}
	return request, nil
}

// tasksFolderRemove deletes task folders — `rm <id>` for one (any state), or
// `rm --all-done` to clear the 99_done/ archive. It is a MANUAL, human action: the
// loop and skills only ever MOVE a finished task to 99_done/, never delete it.
func tasksFolderRemove(root string, args []string) (int, error) {
	request, err := parseTaskRemoveArgs(args)
	if err != nil {
		return 2, err
	}
	if request.allDone {
		n, err := countDone(root)
		if err != nil {
			return -1, err
		}
		if n == 0 {
			ui.Note("No completed tasks to delete.")
			return 0, nil
		}
		// The blast radius before the question: how many, from exactly which archive, and what is
		// lost with them.
		ui.Note("Delete %s\n", ui.Count(n, "completed task"))
		ui.Note("  %s", displayPath(filepath.Join(root, StateDone)))
		ui.Note("  Their instructions, progress and saved evidence will be permanently deleted.\n")
		if err := ui.DestroyGate("Delete these tasks", request.yes); err != nil {
			return 2, cancelledDeletion(err, "No tasks deleted.")
		}
		removed, err := removeAllDone(root)
		if err != nil {
			// Partial deletion is durable: say exactly how many are gone before anything else.
			return -1, fmt.Errorf(
				"deleted %s, then stopped: %w — re-run 'coop tasks rm --all-done --yes'",
				ui.Count(removed, "completed task"), err,
			)
		}
		ui.OK("Deleted %s", ui.Count(removed, "completed task"))
		return 0, nil
	}
	t, err := FindTask(root, request.id) // resolve the (possibly substring) match first, so the gate names it
	if err != nil {
		return 1, err
	}
	ui.Note("Permanently delete %q, including its instructions,\nprogress, and saved evidence.\n", t.Title)
	ui.Note("  %s\n", displayPath(t.Dir))
	if err := ui.DestroyGate("Delete this task", request.yes); err != nil {
		return 2, cancelledDeletion(err, "No tasks deleted.")
	}
	removed, err := removeTaskFolderAndRecords(root, t)
	if err != nil && removed {
		// The folder IS gone; only the lock cleanup is owed. Say which of the two happened.
		ui.Warn("Task deleted; cleanup incomplete")
		return -1, fmt.Errorf("%s was deleted, but its final lock cleanup failed: %w", t.ID, err)
	}
	if err != nil {
		return -1, fmt.Errorf("delete task %s: %w — task not removed; re-run 'coop tasks rm %s'", t.ID, err, t.ID)
	}
	ui.OK("Deleted task: %s", t.Title)
	return 0, nil
}

// cancelledDeletion turns the shared gate's bare "cancelled" into the family's decline result: a
// declined deletion is not a failure to report with a red ✗, it is an answer — so it prints what did
// NOT happen and returns the already-reported sentinel. Any other gate refusal (a pipe with no
// --yes) is its own actionable message and passes through as an error.
func cancelledDeletion(err error, nothing string) error {
	if err != nil && err.Error() == "cancelled" {
		ui.Note("Cancelled. %s", nothing)
		return ui.ErrReported
	}
	return err
}

// removeTaskFolderAndRecords is the single post-confirmation deletion boundary. Completion-window
// snapshots take the index lock too, and every controller shares the persistent task-authority
// inode, so holding both through RemoveAll makes the folder and its host records disappear as one
// serialized operation. A box-side deletion bypasses this helper and still fails closed at replay.
func removeTaskFolderAndRecords(root string, task Item) (removed bool, err error) {
	if err := refuseForkTaskOwner(root, task.ID, "remove"); err != nil {
		return false, err
	}
	indexFile, index, err := lockCompletionWindowIndex(root)
	if err != nil {
		return false, err
	}
	defer func() {
		err = errors.Join(err, unlockLeaseFile(indexFile))
	}()

	authority, err := lockLeaseAuthority(root, task.ID, true, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, errors.New("task is still leased by a live controller; stop it before deleting")
		}
		return false, err
	}
	defer func() {
		err = errors.Join(err, unlockLeaseFile(authority))
	}()

	current, ok, err := CurrentTask(root, task.ID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, errors.New("task disappeared before deletion")
	}
	if current.Dir != task.Dir {
		return false, fmt.Errorf("task changed state from %s to %s before deletion", StateLabel(task.State), StateLabel(current.State))
	}

	changed := false
	for windowID, record := range index.Windows {
		if _, ok := record.Baseline[task.ID]; ok {
			delete(record.Baseline, task.ID)
			changed = true
		}
		if record.AllowedDoneDeparture == task.ID {
			record.AllowedDoneDeparture = ""
			changed = true
		}
		if before := len(record.AllowedDoneDepartures); before > 0 {
			record.AllowedDoneDepartures = slices.DeleteFunc(record.AllowedDoneDepartures, func(candidate string) bool {
				return candidate == task.ID
			})
			changed = changed || len(record.AllowedDoneDepartures) != before
		}
		if before := len(record.BaselineMutations); before > 0 {
			record.BaselineMutations = slices.DeleteFunc(record.BaselineMutations, func(candidate string) bool {
				return candidate == task.ID
			})
			changed = changed || len(record.BaselineMutations) != before
		}
		if before := len(record.RecoveredDepartures); before > 0 {
			record.RecoveredDepartures = slices.DeleteFunc(record.RecoveredDepartures, func(candidate string) bool {
				return candidate == task.ID
			})
			changed = changed || len(record.RecoveredDepartures) != before
		}
		if before := len(record.ReviewSubjects); before > 0 {
			record.ReviewSubjects = slices.DeleteFunc(record.ReviewSubjects, func(candidate string) bool {
				return candidate == task.ID
			})
			if len(record.ReviewSubjects) != before {
				// Keep subject-scoped review semantics after the final subject id is
				// authoritatively deleted; an empty unscoped review remains strict.
				record.ReviewSubjectScoped = true
				changed = true
			}
		}
		if record.WorkSubject == task.ID {
			record.WorkSubject = ""
			changed = true
		}
		index.Windows[windowID] = record
	}
	if changed {
		err = writeCompletionWindowIndex(root, index)
	}
	if err != nil {
		return false, err
	}
	if err := errors.Join(
		clearLeaseCompletionReceipt(authority),
		removeLeaseAuthorityMetadata(root, task.ID),
		removeAuditReopenRecord(root, task.ID),
		removeTrustedDoneDeparture(root, task.ID),
		removeTaskOwnerRecord(root, task.ID),
	); err != nil {
		return false, err
	}
	if err := os.RemoveAll(task.Dir); err != nil {
		return false, err
	}
	if survivor, ok, readErr := CurrentTask(root, task.ID); readErr != nil {
		return false, readErr
	} else if ok {
		return false, fmt.Errorf("task changed state during deletion and survives in %s", StateLabel(survivor.State))
	}
	return true, nil
}

// removeAllDone deletes every task folder in root's 99_done/ archive, returning how many went.
// Shared by the single-queue `rm --all-done` and the multi-queue roll-up.
func removeAllDone(root string) (int, error) {
	removed := 0
	items, err := ReadTaskTree(root)
	if err != nil {
		return 0, err
	}
	for _, t := range items {
		if t.State != StateDone {
			continue
		}
		taskRemoved, err := removeTaskFolderAndRecords(root, t)
		if taskRemoved {
			removed++
		}
		if err != nil && taskRemoved {
			return removed, fmt.Errorf("delete task %s: task was removed, but final lock cleanup failed: %w", t.ID, err)
		}
		if err != nil {
			return removed, fmt.Errorf("delete task %s: %w — task not removed", t.ID, err)
		}
	}
	return removed, nil
}

// countDone reports how many done tasks removeAllDone would delete — for the pre-delete blast-radius
// prompt, so `rm --all-done` can say the count before the (unrecoverable) removal, not after.
func countDone(root string) (int, error) {
	n := 0
	items, err := ReadTaskTree(root)
	if err != nil {
		return 0, err
	}
	for _, t := range items {
		if t.State == StateDone {
			n++
		}
	}
	return n, nil
}

// doneListCap caps how many of the (oldest-first sorted) done tasks `coop tasks ls` shows — the
// done archive only grows, and the live todo/in-progress/blocked work shouldn't scroll off below it.
// The full count stays in the section header + summary; `--all` shows everything.
const doneListCap = 5

// listStateOrder is the order the listing shows its sections: the way work moves through the queue
// as a person reads it — what is being worked on, what is ready next, what is parked on a decision,
// then the archive. (TaskStates is the LIFECYCLE order, which the counters and the loop use.)
var listStateOrder = []string{StateInProgress, StateTodo, StateBlocked, StateDone}

// tasksFolderList prints the queue grouped by state. only narrows it to the given states (a
// --blocked/--todo/… filter); empty shows every state. Each task is three lines at most — its
// title, the facts that are exceptional about it, and its id as an OSC 8 hyperlink to its folder,
// so it opens on click in a supporting terminal and stays plain text in a pipe. There is no count
// footer: the section headings already carry the counts.
func tasksFolderList(root string, all bool, only ...string) (int, error) {
	items, err := ReadTaskTree(root)
	if err != nil {
		return -1, err
	}
	p := ui.For(os.Stdout)
	if len(items) == 0 {
		fmt.Printf("%s\n\n", p.Bold("No tasks yet."))
		fmt.Printf("  Create one: coop tasks add %q\n", "Describe the work")
		return 0, nil
	}
	show := map[string]bool{}
	for _, s := range only {
		show[s] = true
	}
	byState := map[string][]Item{}
	for _, t := range items {
		byState[t.State] = append(byState[t.State], t)
	}
	printed := false
	for _, state := range listStateOrder {
		if len(show) > 0 && !show[state] { // a filter narrows which sections render
			continue
		}
		ts := byState[state]
		if len(ts) == 0 { // an empty lifecycle section is not an empty heading
			continue
		}
		if !printed {
			fmt.Printf("%s\n", p.Bold("Tasks"))
		}
		printed = true
		// The state label is colored by state (the shared key — cyan todo · yellow in progress ·
		// red blocked · green done), so a section is findable by its color; the count rides dim.
		heading := strings.ToUpper(strings.ReplaceAll(StateLabel(state), "_", " "))
		fmt.Printf("\n%s%s\n", p.Bold(paintState(p, state, heading)), p.Dim(fmt.Sprintf(" · %d", len(ts))))
		total := len(ts)
		capped := state == StateDone && !all && total > doneListCap
		if capped {
			ts = ts[total-doneListCap:] // the most recent (folders sort oldest-first)
		}
		for _, t := range ts {
			// Title-first (what a human scans), wrapped across as many lines as it needs so the
			// whole title is readable. Then only what is exceptional about this task, then the id —
			// a long machine handle you need only to claim/done — faint, linking to its folder.
			for _, tl := range wrapWords(t.Title, titleWrapWidth()) {
				fmt.Printf("  %s\n", tl)
			}
			if m := listMarkers(p, t); m != "" {
				fmt.Printf("    %s\n", m)
			}
			fmt.Printf("    %s\n", p.Link(fileURI(t.Dir), p.Faint(t.ID)))
		}
		if capped {
			fmt.Printf("  %s\n", p.Dim(fmt.Sprintf("Showing %d of %d completed tasks. See all: coop tasks ls --done --all", doneListCap, total)))
		}
	}
	if !printed {
		// A filter that matched nothing — say so plainly rather than an empty block.
		fmt.Printf("No %s tasks.\n", filterLabel(only))
		return 0, nil
	}
	// The one action the listing can offer: a blocked task is waiting on a human.
	if len(byState[StateBlocked]) > 0 && (len(show) == 0 || show[StateBlocked]) {
		fmt.Printf("\nAnswer blocked tasks: coop tasks decisions\n")
	}
	return 0, nil
}

// fileURI renders an absolute path as a file:// URL for an OSC 8 hyperlink — percent-encoding
// spaces and other unsafe characters so a task folder like ".../a b" links correctly.
func fileURI(abs string) string {
	return (&url.URL{Scheme: "file", Path: abs}).String()
}

// paintState colors s by task state — the one key shared across the list (the state headings
// and the summary counts): cyan todo · yellow in progress · red blocked · green done.
func paintState(p ui.Palette, state, s string) string {
	switch state {
	case StateInProgress:
		return p.Yellow(s)
	case StateBlocked:
		return p.Red(s)
	case StateDone:
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
// decisions -i`): a heavy rule, "Question N of M", the same rule again, then the task it belongs
// to. The three head lines are bold cyan so one question is clearly bordered off from the previous
// as you scroll — but the rule is drawn in plain text too, so the border survives NO_COLOR and a
// redirect (color never carries the only meaning). where is the task id, optionally "queue · id".
func decisionDivider(p ui.Palette, n, total int, where string) string {
	// A HEAVY rule (━, vs the queue banner's light ─) so the interactive divider reads as a strong
	// border between questions.
	rule := strings.Repeat("━", decisionDividerWidth())
	head := p.Bold(p.Cyan(rule)) + "\n" +
		p.Bold(p.Cyan(fmt.Sprintf("Question %d of %d", n, total))) + "\n" +
		p.Bold(p.Cyan(rule))
	return head + "\n  " + where
}

// decisionDividerWidth is the divider's span: the terminal's width, capped at 72 columns so the
// border frames the question instead of stretching across an ultra-wide pane, and floored so a
// narrow one still shows a border.
func decisionDividerWidth() int {
	w := ui.TermWidthRaw(os.Stdout)
	switch {
	case w <= 0, w > 72:
		return 72 // width unknown (not a terminal), or wider than the cap
	case w < 20:
		return 20
	}
	return w
}

// listMarkers renders what is EXCEPTIONAL about a task, on its own line between the title and the
// id: subtask progress, a blocked task's waiting answer, and an in-progress task's owner or
// reservation — joined with " · ", or "" when there is nothing to say. An ordinary todo task, and
// an in-progress task nobody has claimed or reserved, carry no marker at all: a row that always
// says something says nothing (see rule tag-exceptions-not-every-row).
func listMarkers(p ui.Palette, t Item) string {
	var parts []string
	if n := len(t.Subtasks); n > 0 {
		prog := fmt.Sprintf("%d/%d subtasks", t.doneSubtasks(), n)
		if t.doneSubtasks() == n {
			prog = p.Gray(prog) // fully done — recede the count
		}
		parts = append(parts, prog)
	}
	if t.State == StateBlocked {
		parts = append(parts, p.Red("Needs your answer"))
	}
	if t.State == StateInProgress {
		if m := inProgressMarker(t); m != "" {
			parts = append(parts, p.Dim(m))
		}
	}
	return strings.Join(parts, p.Dim(" · "))
}

// inProgressMarker labels an in-progress task with the one fact that explains whether the loop will
// touch it next: a durable human claim beats the lease-derived reservation, because a claimed task
// with nobody actively holding its lease would otherwise read as free work (it never is: see
// assignLoopTaskOnly). A claim whose process has died is the exception worth flagging on its own —
// nobody is on it, and the loop needs `coop loop --preflight` to take it back. An unclaimed,
// unreserved task says nothing: that IS the ordinary in-progress row. A read error falls back to
// the reservation: ls is a display, not the adoption gate, so it degrades instead of failing.
func inProgressMarker(t Item) string {
	root := filepath.Dir(filepath.Dir(t.Dir))
	lease := observeTaskLease(t, time.Now())
	if rec, owned, err := ReadTaskOwnerRecord(root, t.ID); err == nil && owned {
		if rec.Kind == TaskOwnerHuman && rec.ActorPID != 0 && !ownerProcessLive(rec) {
			return "⚠ Owner process has stopped"
		}
		if lease.State != leaseUnleased {
			return TaskOwnerLabel(rec) + " · " + leaseReservation(lease) // claimed AND actively held
		}
		return TaskOwnerLabel(rec)
	}
	return leaseReservation(lease)
}

// leaseReservation says who is holding the task's work lock right now — a RESERVATION, not a claim
// and not proof of a running agent. Nobody holding it is the ordinary case and says nothing.
func leaseReservation(o TaskLeaseObservation) string {
	switch o.State {
	case leaseStalled:
		return "Reserved by " + o.Provider + " (stalled)"
	case leaseBusy:
		return "Reserved by " + o.Provider
	default:
		return ""
	}
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
	var decisions []Item
	items, err := ReadTaskTree(root)
	if err != nil {
		return -1, err
	}
	for _, t := range items {
		if t.State == StateBlocked {
			decisions = append(decisions, t)
		}
	}
	if len(decisions) == 0 {
		ui.Note("No questions waiting for your answer.")
		return 0, nil
	}
	if interactive {
		return decisionsInteractive(root, decisions)
	}
	p := ui.For(os.Stdout)
	fmt.Printf("%s%s\n", p.Bold("Questions waiting for your answer"), p.Dim(fmt.Sprintf(" · %d", len(decisions))))
	for _, t := range decisions {
		question := t.Title
		rec := ""
		body, _, err := readOptionalTaskMetadataPath(filepath.Join(t.Dir, "decision.md"))
		if err != nil {
			return -1, err
		}
		for _, line := range strings.Split(string(body), "\n") {
			if q, ok := strings.CutPrefix(line, "# Decision:"); ok {
				question = strings.TrimSpace(q)
			}
			if r, ok := strings.CutPrefix(line, "**Recommendation:**"); ok {
				rec = strings.TrimSpace(r)
			}
		}
		// Question first (what you weigh), the recommendation under it, then the id (the handle you
		// `unblock` with) — the same title/markers/id shape as the task list.
		fmt.Printf("\n  %s\n", sanitizeCell(question))
		if rec != "" {
			fmt.Printf("    %s %s\n", p.Dim("Recommendation:"), sanitizeCell(rec))
		}
		fmt.Printf("    %s\n", p.Faint(t.ID))
	}
	fmt.Print("\nAnswer one: coop tasks unblock <id> \"<answer>\"\n")
	fmt.Print("Read and answer each: coop tasks decisions -i\n")
	return 0, nil
}

// decisionRef locates one open decision for the browser: the queue root that owns it, the task
// id (re-resolved on each visit, so a concurrent move is caught), and a display label naming the
// queue in a multi-queue session ("" when there is only one queue — no label needed).
type decisionRef struct{ root, label, id string }

// decisionRefs turns one queue's blocked tasks into browser refs, labeled for the roll-up.
func decisionRefs(root, label string, decisions []Item) []decisionRef {
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
func decisionsInteractive(root string, decisions []Item) (int, error) {
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
	answered, deleted := 0, 0
	for i := 0; i >= 0; {
		ref := refs[i]
		t, err := FindTask(ref.root, ref.id)
		if err != nil {
			return -1, err
		}
		decPath := filepath.Join(t.Dir, "decision.md")
		where := t.ID
		if ref.label != "" {
			where = ref.label + " · " + t.ID // say which queue this decision lives in
		}
		fmt.Fprintf(out, "\n%s\n", decisionDivider(p, i+1, len(refs), where))
		body, _, err := readOptionalTaskMetadataPath(decPath)
		if err != nil {
			return -1, err
		}
		fprintDecisionBody(out, p, string(body))
		resolved, err := decisionResolved(decPath)
		if err != nil {
			return -1, err
		}
		prompt := "Your answer (Enter to skip):"
		if resolved {
			// The existing answer is shown, and Enter keeps it — the safe key must never be the
			// one that silently discards a decision somebody already made.
			if answer := decisionResolution(string(body)); answer != "" {
				fmt.Fprintf(out, "  %s %s\n", p.Dim("Answered:"), answer)
			}
			prompt = "Your answer (Enter to keep it):"
		}
		key := func(k string) string { return p.Cyan(k) }
		fmt.Fprintf(out, "\n%s\n  %s%s%s%s%s%s%s\n%s ",
			prompt,
			key(":n"), p.Dim(" next · "), key(":p"), p.Dim(" previous · "),
			key(":d"), p.Dim(" delete task · "), key(":q")+p.Dim(" quit"), p.Dim(">"))
		if !sc.Scan() {
			break // EOF / ^D ends the session
		}
		line := strings.TrimSpace(sc.Text())
		// :d deletes (drops) the current decision's task — an unrecoverable folder removal, so route
		// the browser's own scanner through the shared destruction gate (default No, so a stray Enter
		// cancels). A declined confirm is a safe no-op that stays on the current decision. Deleting
		// the folder also drops its ref, so :p/:n never revisit a gone task.
		if line == ":d" {
			readConfirmation := false
			gateErr := ui.DestroyGate("Delete task "+t.ID, false, func(prompt string) bool {
				// The permanent loss, in the future tense, BEFORE the question — the same shape
				// every other destructive preview uses.
				fmt.Fprintf(out, "\n  Its instructions, progress and saved evidence will be permanently deleted.\n\n")
				fmt.Fprintf(out, "%s [y/N]: ", p.Red(prompt))
				if !sc.Scan() {
					return false
				}
				readConfirmation = true
				return ui.ConfirmationResponse(sc.Text(), false)
			})
			if !readConfirmation {
				break // EOF at the confirm ends the session — nothing deleted
			}
			if gateErr != nil {
				continue // declined: stay on the current decision
			}
			removedTask, err := removeTaskFolderAndRecords(ref.root, t)
			if err != nil && removedTask {
				return -1, fmt.Errorf("delete task %s: task was removed, but final lock cleanup failed: %w", t.ID, err)
			}
			if err != nil {
				return -1, fmt.Errorf("delete task %s: %w — task not removed", t.ID, err)
			}
			deleted++
			refs = append(refs[:i], refs[i+1:]...) // drop the gone ref; index i now points at the next
			if i >= len(refs) {
				i = -1 // deleted the last decision → done
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
			if t.State == StateBlocked {
				if err := resolveAndUnblock(ref.root, t, line); err != nil {
					return -1, unblockRetryError(t.ID, true, err)
				}
				answered++
			} else if err := recordResolution(decPath, line); err != nil {
				return -1, err
			}
			// No per-answer confirmation line: auto-advancing to the next question (its bordered
			// "Question N of M" header, drawn below) is the acknowledgement, and a "✓ recorded" line
			// would just scroll onto that header. Re-viewing with :p shows the saved answer, and the
			// closing summary counts what was answered.
			i++
		}
		if i >= len(refs) {
			i = -1 // past the last decision → done
		}
	}
	// Answers and deletions are counted separately: they are different outcomes, and a session that
	// did both must not report either as the whole story.
	switch {
	case answered > 0 && deleted > 0:
		ui.OK("Answered %s — %s returned to todo · deleted %s",
			ui.Count(answered, "question"), pluralTasks(answered), ui.Count(deleted, "task"))
	case answered > 0:
		ui.OK("Answered %s — %s returned to todo", ui.Count(answered, "question"), pluralTasks(answered))
	case deleted > 0:
		ui.OK("Deleted %s", ui.Count(deleted, "task"))
	default:
		ui.Note("No answers saved. Tasks are still blocked.")
	}
	return 0, nil
}

// pluralTasks says "task"/"tasks" for the answered count without repeating the number.
func pluralTasks(n int) string {
	if n == 1 {
		return "task"
	}
	return "tasks"
}

// fprintDecisionBody renders a decision.md for the browser: HTML comments stripped, the
// `# Decision:` question bold, the Blocks / Resolution / `---` lines dropped (the id is in the
// header and the answer is what we're collecting), and the rest indented.
func fprintDecisionBody(out io.Writer, p ui.Palette, content string) {
	prevBlank := true // the divider's task id ends the header — the question starts its own block
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
	items, err := ReadTaskTree(root)
	if err != nil {
		return -1, err
	}
	var findings []string
	add := func(id, msg string) { findings = append(findings, fmt.Sprintf("  %s: %s", id, msg)) }
	// Every queue needs all four state dirs, or the move-a-folder-between-states protocol renames a
	// task into a missing dir and silently corrupts the queue (see scaffoldStateDirs). Flag any older
	// tree that predates the fix.
	if fi, err := os.Stat(root); err == nil && fi.IsDir() {
		for _, st := range TaskStates {
			if s, e := os.Stat(filepath.Join(root, st)); e != nil || !s.IsDir() {
				add(st, "state dir is missing — the move protocol will corrupt the queue; run 'coop init'")
			}
		}
	}
	// ReadTaskTree skips a regular file where only task folders belong (a Finder .DS_Store must not
	// hide the queue), so lint is where a non-dotfile one — most likely a task someone wrote as a
	// file — becomes visible instead of staying silently unworked.
	for _, st := range TaskStates {
		entries, err := os.ReadDir(filepath.Join(root, st))
		if err != nil {
			continue // a missing state dir is reported above; an unreadable one already failed ReadTaskTree
		}
		for _, e := range entries {
			if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".") {
				add(st+"/"+e.Name(), "is a file, not a task folder — every task is a folder holding a task.md; move it into one or remove it")
			}
		}
	}
	for _, t := range items {
		bodyBytes, exists, err := readOptionalTaskMetadataPath(filepath.Join(t.Dir, "task.md"))
		if err != nil {
			return -1, err
		}
		if !exists {
			return -1, fmt.Errorf("task %s lost task.md while linting", t.ID)
		}
		body := string(bodyBytes)
		// blocked ⇒ a decision.md is present. A RESOLVED decision.md rides along as the audit trail
		// once unblocked (todo→in_progress→done); only an UNRESOLVED one on a non-blocked task is the
		// inconsistency — an open one-way door waiting in the queue instead of parked in 50_blocked/.
		if t.State == StateBlocked && !t.HasDecision {
			add(t.ID, "blocked but has no decision.md — add one, or unblock it")
		}
		if t.State == StateTodo && t.HasDecision {
			resolved, err := decisionResolved(filepath.Join(t.Dir, "decision.md"))
			if err != nil {
				return -1, err
			}
			if !resolved {
				add(t.ID, "has an unresolved decision.md but is todo — block it (or resolve it and unblock)")
			}
		}
		// a status field is forbidden — the directory IS the status
		if fields, _ := SplitFrontmatter(body); fields["status"] != "" {
			add(t.ID, "has a `status:` field — remove it; the parent directory is the status")
		}
		// self-contained: every shape section present and filled — not still a `<…>` placeholder (not
		// for done, which is the shipped record). Supersedes the old acceptance-substring-only check.
		if t.State != StateDone {
			for _, issue := range taskShapeIssues(body) {
				add(t.ID, "not self-contained: "+issue)
			}
		}
	}
	if len(findings) == 0 {
		if len(items) == 0 {
			ui.Note("No tasks to check.")
		} else {
			ui.OK("Checked %s — no task-file issues", ui.Count(len(items), "task"))
		}
		return 0, nil
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	ui.Error("Found %s", ui.Count(len(findings), "task-file issue"))
	return 1, nil
}
