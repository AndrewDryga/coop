package tasks

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	osuser "os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/AndrewDryga/coop/internal/processidentity"
	"github.com/AndrewDryga/coop/internal/scaffold"
	"github.com/AndrewDryga/coop/internal/ui"
)

// Folder-mode `coop tasks` subcommands. A task's state is its parent directory, so every
// transition is a folder move (atomic os.Rename) in gitignored local working state.
// Dispatched from CmdTasks when the resolved source is a .agent/tasks directory.

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
	"done": {[]string{"--force", "--from-fork"}, 1, "coop tasks done <task-id>"},
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
// It reads the states as a person writes them before a noun — "in-progress", not "in_progress".
func filterLabel(only []string) string {
	labels := make([]string, len(only))
	for i, s := range only {
		labels[i] = strings.ReplaceAll(StateLabel(s), "_", "-")
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

// CmdTasksFolder routes `coop tasks <sub>` against a folder-mode tree rooted at root
// (absolute path to .agent/tasks). No sub-command lists the tree.
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
		return tasksFolderAddWithProject(root, args, StateTodo, "tasks add", repo)
	case "claim":
		return tasksFolderClaim(root, args)
	case "release":
		return tasksFolderRelease(root, args)
	case "lease":
		return tasksFolderLease(root, args)
	case "block":
		return tasksFolderBlock(root, args)
	case "unblock":
		return tasksFolderUnblock(repo, root, args)
	case "park":
		return tasksFolderPark(root, args)
	case "done":
		id := slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--force" || a == "--from-fork" })
		forceClose, fromFork, interactive := slices.Contains(args, "--force"), slices.Contains(args, "--from-fork"), stdinIsTerminal()
		if forceClose && !interactive {
			return 2, errForceCloseNeedsTerminal
		}
		if fromFork && !interactive {
			return 2, errFromForkNeedsTerminal
		}
		return tasksFolderMoveWith(root, id, StateDone, "done", "done", claimOptions{
			actor:      captureClaimActor(realClaimActorProbe, os.Getppid(), interactive, ClaimActor{}),
			forceClose: forceClose,
			fromFork:   fromFork,
			project:    repo,
		})
	case "path":
		return tasksFolderPath(root, args)
	case "rm":
		return tasksFolderRemove(root, args)
	case "decisions":
		return tasksFolderDecisions(repo, root, args)
	default:
		return 2, unknownSubcommandErr("tasks", sub, TasksVerbs)
	}
}

// TasksVerbs are the canonical `coop tasks` subcommands (primary spellings, no aliases): the single
// source for the unknown-subcommand suggester and isTasksSubcommand, so the two can't drift. `watch`
// belongs here even though CmdTasks (not CmdTasksFolder) handles it — a mistype of it should suggest it.
var TasksVerbs = []string{"ls", "lint", "add", "claim", "release", "lease", "block", "unblock", "park", "done", "watch", "queues", "path", "rm", "decisions"}

// isTasksSubcommand reports whether s names a `coop tasks` subcommand. CmdTasks uses it to catch
// `coop tasks --tasks <sub>`, where --tasks swallows the subcommand as a queue path.
func isTasksSubcommand(s string) bool {
	return slices.Contains(TasksVerbs, s)
}

// MatchTask resolves id against a set of task items: an exact ID match wins, else a unique substring
// match (so a slug fragment works). Ambiguous or absent is refused as rejected input (see
// lookupExit). listCmd names the command that lists this set ("coop tasks" / "coop backlog"), so the
// refusal points at the right listing. Shared by FindTask (the lifecycle tree) and findBacklogTask
// (xx_backlog).
func MatchTask(items []Item, id, listCmd string) (Item, error) {
	if id == "" {
		// An empty fragment would substring-match every task ("" is in everything); refuse it
		// instead of silently acting on the first/only one.
		return Item{}, taskLookupRefusal("Missing task ID", listCmd)
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
		return Item{}, taskLookupRefusal(fmt.Sprintf("No task matching %q", id), listCmd)
	default:
		refusal := &ui.UsageError{
			Headline: fmt.Sprintf("%d tasks match %q", len(hits), id),
			Rows:     [][2]string{{"", "Use one of these IDs."}},
		}
		for _, h := range hits {
			refusal.Choices = append(refusal.Choices, ui.SafeInline(h.ID)) // a folder name can come from a box
		}
		return Item{}, refusal
	}
}

// taskLookupRefusal refuses an ID that names no task, pointing at the listing that shows the IDs.
func taskLookupRefusal(headline, listCmd string) *ui.UsageError {
	label := "Tasks:"
	if listCmd == "coop backlog" {
		label = "Ideas:"
	}
	return &ui.UsageError{Headline: headline, Rows: [][2]string{{label, listCmd}}}
}

// lookupExit is the exit code for a task lookup that failed: 2 when the ID itself is the problem (it
// matches no task, or several), 1 when a queue could not be read.
func lookupExit(err error) int {
	var refused *ui.UsageError
	if errors.As(err, &refused) {
		return 2
	}
	return 1
}

// FindTask locates a task by ID across the lifecycle state dirs — an exact ID match, else a unique
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
// FindTask, so `coop backlog rm/promote` accept a slug fragment and error clearly on absent/ambiguous.
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

// taskSection names the required sections shared by structured add flags and lint. Their initial
// text and checklist come from the selected queue's README template, not this list.
type taskSection struct{ heading, flag string }

var taskSections = []taskSection{
	{"Context", "context"},
	{"Acceptance criteria", "acceptance"},
	{"Approach", "approach"},
}

// addOptions are the options `coop tasks add` / `coop backlog add` accept, derived from the section
// flags that ARE the task shape — so a correction can never suggest a flag the parser would refuse.
var addOptions = func() []string {
	out := make([]string, 0, len(taskSections)+1)
	for _, s := range taskSections {
		out = append(out, "--"+s.flag)
	}
	return append(out, "--subtask")
}()

// sectionsFilled reports whether every required section came from structured flags.
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
func newTaskFiles(template taskTemplate, id, title, now string, values map[string]string, subtasks []string) (map[string]string, error) {
	taskMD, err := template.render(id, title, now, values, subtasks)
	if err != nil {
		return nil, err
	}
	if !sectionsFilled(values) {
		taskMD = "<!-- Describe the work before changing code.\n" +
			"     Fill in the problem, completion criteria, approach and subtasks.\n" +
			"     If a decision prevents this, use this run's task tools to record the question\n" +
			"     and block the task. On the host: coop tasks block " + id + "\n" +
			"     Format and examples: .agent/tasks/README.md -->\n" + taskMD
	}
	return map[string]string{
		"task.md": taskMD,
		"log.md": "<!-- Append progress, decisions and problems here.\n" +
			"     Keep earlier entries. Put the latest resume summary in state.md. -->\n\n" +
			"# Log — " + title + "\n",
		"state.md": "<!-- Replace this short summary at each checkpoint so another session can continue.\n" +
			"     Keep the detailed history in log.md. -->\n\n" +
			"# State — " + title + "\n\n" +
			"**Status:** not started\n" +
			"**Done so far:** —\n" +
			"**Next action:** <the next concrete step>\n" +
			"**Traps:** <anything the next session needs to watch for, or —>\n",
	}, nil
}

// tasksFolderAddWithProject creates a task folder under root/state (StateTodo for `coop tasks add`,
// StateBacklog for `coop backlog add` — the two share every bit of parsing/validation). cmdLabel is
// the command as typed ("tasks add" / "backlog add"), so error and usage lines name the right
// command. project is the folder that owns the queue: a queue that does not exist yet is set up in
// it the way coop init would (see createTaskFolder).
func tasksFolderAddWithProject(root string, args []string, state, cmdLabel, project string) (int, error) {
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
				return 2, ui.MissingOptionValue("--"+flag, "coop "+cmdLabel,
					fmt.Sprintf(`coop %s "Fix login retries" --%s "<text>"`, cmdLabel, flag))
			}
			i++
			val = args[i]
		}
		structured = true
		if flag == "subtask" {
			subtasks = append(subtasks, val)
		} else if values[heading] != "" {
			// Repeated section flag (e.g. several --acceptance): ACCUMULATE, don't overwrite. A silent
			// last-wins would lose a multi-clause paste; paragraphs keep each clause under its heading.
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
		return 2, &ui.UsageError{
			Headline: fmt.Sprintf("Invalid title for %q", "coop "+cmdLabel),
			Cause:    "Use at least one letter or digit. The task ID is made from the title's words.",
			Rows: [][2]string{
				{"Example:", "coop " + cmdLabel + ` "Fix login retries"`},
				{"Help:", ui.HelpCommand("coop " + cmdLabel)},
			},
		}
	}
	// Structured mode is all-or-nothing: every section flag must be given, so we never create a task
	// that's half-filled and half-<…>-placeholder. Omit all the flags to get the placeholder scaffold.
	if structured {
		var missing, every, usage []string
		for _, s := range taskSections {
			every = append(every, "--"+s.flag)
			usage = append(usage, "--"+s.flag+" <text>")
			if strings.TrimSpace(values[s.heading]) == "" {
				missing = append(missing, "--"+s.flag)
			}
		}
		if len(missing) > 0 {
			return 2, &ui.UsageError{
				Headline: fmt.Sprintf("Missing %s for %q", ui.List(missing, "and"), "coop "+cmdLabel),
				Cause: "If you use any text option, include " + ui.List(every, "and") + ".\n" +
					"Without the text options, Coop creates a template for you to fill in.",
				Rows: [][2]string{
					{"Usage:", "coop " + cmdLabel + ` "<title>" ` + strings.Join(usage, " ") + " [--subtask <text>...]"},
					{"Help:", ui.HelpCommand("coop " + cmdLabel)},
				},
			}
		}
	}
	id, setup, err := createTaskFolder(project, root, state, slug, title, values, subtasks)
	if err != nil {
		if errors.As(err, &taskExistsError{}) {
			return 1, err
		}
		return -1, err
	}
	printQueueSetup(root, setup)
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
	// The scaffold's result is also the lesson: the command that would have filled it in, with this
	// task's own title and placeholders rather than an example about some other task.
	ui.Note("\nDescribe the problem, completion criteria, approach, and subtasks in this file.")
	ui.Note("Next time, you can fill them in when creating the task:\n")
	ui.Note("  coop tasks add %q \\", title)
	ui.Note(`    --context "<context>" \`)
	ui.Note(`    --acceptance "<acceptance>" \`)
	ui.Note(`    --approach "<approach>" \`)
	ui.Note(`    --subtask "<subtask>"`)
	ui.Note("\nFor more details see:")
	ui.Note("  coop help tasks add")
	return 0, nil
}

// taskExistsError is the id-collision refusal `coop tasks add` reports as a user error (exit 1).
type taskExistsError struct{ msg string }

func (e taskExistsError) Error() string { return e.msg }

// createTaskFolder writes a new task folder <date>-<slug> under root/state from the given body values
// and returns its id, for `coop tasks add` and `coop backlog add`. A queue that does not exist yet is
// made first: `add` is how a project starts tasks before coop init, and how a secondary --tasks queue
// starts. In project it gets coop init's queue part, the README.md task.md points to and the
// .gitignore lines that keep task folders out of Git; setup says what that wrote. The text is checked
// before any of it, so a refused add leaves no empty queue behind for `coop loop` to take.
func createTaskFolder(project, root, state, slug, title string, values map[string]string, subtasks []string) (string, scaffold.QueueSetup, error) {
	var setup scaffold.QueueSetup
	if err := checkTaskText(title, subtasks); err != nil {
		return "", setup, err
	}
	if exists, err := taskQueueExists(root); err != nil {
		return "", setup, err
	} else if !exists && project != "" {
		if setup, err = scaffold.InitQueue(project, root); err != nil {
			return "", setup, fmt.Errorf("set up the task queue %s: %w", displayPath(root), err)
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", setup, err
	}
	queue, err := os.OpenRoot(root)
	if err != nil {
		return "", setup, err
	}
	defer queue.Close()
	id, err := createTaskFolderIn(queue, state, slug, title, values, subtasks)
	return id, setup, err
}

// printQueueSetup says what setting up a new queue wrote, before the task it made room for.
func printQueueSetup(root string, setup scaffold.QueueSetup) {
	if !setup.Readme && !setup.Gitignore {
		return
	}
	ui.Note("Set up a task queue in %s", displayPath(root))
	if setup.Readme {
		ui.Note("  Added README.md, which explains how tasks work")
	}
	if setup.Gitignore {
		ui.Note("  Updated .gitignore to keep task folders out of Git")
	}
	ui.Note("")
}

// createTaskFolderIn is createTaskFolder in the queue folder queue, the one the in-box task channel
// also creates through, so every path creates the same files with the same collision rule. Each
// state folder must be a real folder, and the task folder and its files are made through queue,
// never by name: a box can swap a state folder for a link to another project's, or plant a link in
// the new folder before its files are written.
func createTaskFolderIn(queue *os.Root, state, slug, title string, values map[string]string, subtasks []string) (string, error) {
	id := time.Now().Format("2006-01-02") + "-" + slug
	template, err := loadTaskTemplate(queue)
	if err != nil {
		return "", err
	}
	files, err := newTaskFiles(template, id, title, time.Now().Format(time.RFC3339), values, subtasks)
	if err != nil {
		return "", err
	}
	// An id is a stable, unique handle, so reject a collision in ANY state — the four lifecycle dirs
	// AND xx_backlog — else a re-add (or a promote) would make two folders share an id, and
	// FindTask/findBacklogTask would silently shadow one.
	for _, st := range append(slices.Clone(TaskStates), StateBacklog) {
		dir, err := openRealSubroot(queue, st)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = dir.Lstat(id)
		dir.Close()
		switch {
		case err == nil && st == StateBacklog:
			return "", taskExistsError{fmt.Sprintf("task %q already exists in %s/ — promote it (coop backlog promote %s) instead of re-adding", id, StateBacklog, id)}
		case err == nil:
			return "", taskExistsError{fmt.Sprintf("task %q already exists in %s/", id, st)}
		case !errors.Is(err, os.ErrNotExist):
			return "", err
		}
	}
	// Ensure all four state dirs exist (the queue may be fresh, or missing one), so the
	// move-a-folder-between-states protocol always has a real dir to move into — same guarantee as
	// `coop init`. Then the task's own state dir: xx_backlog is created on demand here (like a fresh
	// secondary queue), so `coop init` never has to scaffold an empty backlog drawer.
	for _, st := range TaskStates {
		dir, err := ensureRealSubroot(queue, st, 0o755)
		if err != nil {
			return "", err
		}
		dir.Close()
	}
	stateDir, err := ensureRealSubroot(queue, state, 0o755)
	if err != nil {
		return "", err
	}
	defer stateDir.Close()
	if err := stateDir.Mkdir(id, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", taskExistsError{fmt.Sprintf("task %q already exists in %s/", id, state)}
		}
		return "", err
	}
	if err := writeTaskFiles(stateDir, id, files); err != nil {
		return "", err
	}
	return id, nil
}

// writeTaskFiles writes files, in name order, into id, the task folder just made in stateDir. Each
// file is created exclusively, so a file planted in the folder first is refused, never followed or
// overwritten. If a file cannot be written, the files written so far and then the empty folder are
// removed through stateDir, so retrying the same title is not refused as a duplicate. A folder that
// still holds anything this call did not write is left in place.
func writeTaskFiles(stateDir *os.Root, id string, files map[string]string) error {
	dir, err := openRealSubroot(stateDir, id)
	if err != nil {
		return errors.Join(err, stateDir.Remove(id))
	}
	defer dir.Close()
	var written []string
	for _, name := range slices.Sorted(maps.Keys(files)) {
		f, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			written = append(written, name)
			_, err = f.Write([]byte(files[name]))
			err = errors.Join(err, f.Close())
		}
		if err != nil {
			for _, made := range written {
				err = errors.Join(err, dir.Remove(made))
			}
			return errors.Join(err, stateDir.Remove(id))
		}
	}
	return nil
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
// may be taken over. Done takes the same actor, plus the project folder whose history the audit
// of a reopened task reads.
type claimOptions struct {
	actor      ClaimActor
	force      bool // claim --force: take over a live competing claim
	forceClose bool // done --force: a person closes a task whose checklist is unfinished
	fromFork   bool // done --from-fork: a person takes a fork's task whose commit already landed
	project    string
}

// stdinIsTerminal is the one terminal check done --force and --from-fork make; tests replace it.
var stdinIsTerminal = func() bool { return ui.IsTerminal(os.Stdin) }

// errForceCloseNeedsTerminal keeps `coop tasks done --force` a person's tool. An agent's shell
// has no terminal, so an unattended run gets this instead of a way past the checklist.
var errForceCloseNeedsTerminal = &ui.UsageError{
	Headline: "coop tasks done --force needs a person at a terminal",
	Cause:    "It closes a task whose checklist is unfinished.\nAn agent leaves unfinished items open, or blocks the task.",
}

var errTaskClaimedByOther = errors.New("task is claimed by another live process")

// competingClaim reports whether an existing claim blocks a new one: only a claim bound to a
// process that is still alive and is not the claimer's own does. A person's claim (no process) or
// a claim whose process is gone is taken over silently — the point of binding is to tell "someone
// is on this" from "someone was".
func competingClaim(existing TaskOwnerRecord, actor ClaimActor) bool {
	if existing.Kind != TaskOwnerHuman || existing.ActorPID == 0 || !ownerProcessLive(existing) {
		return false
	}
	return existing.ActorPID != actor.PID || !processidentity.Same(existing.ActorStart, actor.StartToken)
}

// claimTaskOwnerRecord writes durable evidence that a HUMAN — not a loop-adopted process — owns
// this task, so AssignLoopTaskOnly refuses to adopt it even long after the `coop tasks claim`
// process that called this has exited. Called ONLY from the interactive claim path below: the
// loop's own todo->in_progress adoption (AssignLoopTaskOnly -> MoveTaskDir) must never call this, or
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
	record, err := humanClaimRecord(root, item, opts.actor)
	if err != nil {
		return "", err
	}
	return replaced, lock.Write(record)
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
					return "", opts, ui.MissingOptionValue(key, "coop tasks claim", actorOptionExample("coop tasks claim", key))
				}
				i++
				value = args[i]
			}
			if key == "--as" {
				if label := claimActorLabel(value); label == "" || label != value {
					return "", opts, invalidActorLabel(value, "coop tasks claim")
				}
				opts.actor.Label = value
				continue
			}
			n, err := strconv.Atoi(value)
			if err != nil || n <= 1 {
				return "", opts, invalidActorPID(value, "coop tasks claim", "claiming")
			}
			opts.actor.PID = n
		case "--force":
			opts.force = true
		default:
			if strings.HasPrefix(args[i], "-") && args[i] != "-" {
				return "", opts, unknownOptionErr(key, "coop tasks claim", []string{"--as", "--pid", "--force"})
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

// actorOptionExample is a spelling of --as or --pid that works, for the commands that take them.
func actorOptionExample(command, option string) string {
	if option == "--pid" {
		return command + " login-retries --pid 4242"
	}
	return command + " login-retries --as reviewer"
}

func invalidActorLabel(value, command string) error {
	return ui.InvalidOptionValue(value, "--as", command, "Use a short label made of letters, digits and the characters -_@.:", actorOptionExample(command, "--as"))
}

func invalidActorPID(value, command, role string) error {
	return ui.InvalidOptionValue(value, "--pid", command, "Use the process ID of the "+role+" agent.", actorOptionExample(command, "--pid"))
}

func noLiveActorProcess(pid int) error {
	return ui.CommandFailed(fmt.Sprintf("No live process with a readable identity at PID %d", pid), "It is not running, or its identity cannot be read.")
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
		return 1, noLiveActorProcess(requested)
	}
	return tasksFolderMoveWith(root, []string{id}, StateInProgress, "claim", "claimed", opts)
}

// tasksFolderMoveWith is tasksFolderMove with the claim's actor and takeover choice; done and every
// non-CLI claim pass an empty claimOptions, which binds the claim to nothing.
func tasksFolderMoveWith(root string, args []string, newState, verb, pastVerb string, opts claimOptions) (int, error) {
	if len(args) < 1 {
		return 2, ui.MissingArgument("task ID", "coop tasks "+verb, "coop tasks "+verb+" <task-id>")
	}
	t, err := FindTask(root, args[0])
	if err != nil {
		return lookupExit(err), err
	}
	if newState == StateInProgress {
		if err := claimRefusal(t); err != nil {
			return 1, err
		}
	}
	completion := completionOptions{force: opts.forceClose, actor: opts.actor}
	var forced *IncompleteChecklistError // what a forced close left open, from the close itself
	var landed *LandedForkTask           // the fork a --from-fork completion took the task from
	complete := func() (*IncompleteChecklistError, error) {
		if !opts.fromFork {
			return completeTrustedTask(opts.project, root, t, completion)
		}
		closed, from, err := completeLandedForkTask(opts.project, root, t, completion)
		for _, id := range from.Discovered {
			ui.OK("Added discovered task: %s", id)
		}
		if err == nil {
			landed = &from
		}
		return closed, err
	}
	if t.State == newState {
		switch newState {
		case StateDone:
			if forced, err = complete(); err != nil {
				return -1, trustedCompletionError(err, t, opts)
			}
			ui.Note("%s is already done.", t.Title)
			noteForcedClose(forced)
			noteLandedFork(landed)
			WarnTaskBulk(root, t.Dir)
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
			logClaim(t.Dir, opts.actor)
			printClaimResult(t, opts.actor, note)
		default:
			ui.Note("%s is already %s.", t.Title, StateLabel(newState))
		}
		return 0, nil
	}
	if newState == StateDone {
		// Refuse known unfinished work before stopping this caller's lease
		// holder; trusted completion rechecks current metadata under authority.
		if err := RequireCompletedChecklist(t); err != nil && !opts.forceClose {
			return -1, trustedCompletionError(err, t, opts)
		}
		if _, err := stopOwnLeaseHolder(root, t, opts.actor); err != nil {
			return -1, err
		}
		if opts.forceClose {
			// A loop killed during its final review leaves its window behind, and a close would be
			// enrolled in that dead review. Settle it the way a loop's start does; a live one is skipped.
			if _, err := ReconcileCompletionWindowsWithActivity([]string{root}); err != nil {
				return -1, err
			}
		}
		if forced, err = complete(); err != nil {
			return -1, trustedCompletionError(err, t, opts)
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
		if err := MoveTaskDir(root, t, newState); err != nil {
			if newState == StateInProgress {
				_ = removeTaskOwnerRecord(root, t.ID) // best-effort: the claim never took effect
			}
			return -1, err
		}
	}
	switch newState {
	case StateInProgress:
		logClaim(filepath.Join(root, StateInProgress, t.ID), opts.actor)
		printClaimResult(t, opts.actor, "")
	case StateDone:
		dir := filepath.Join(root, StateDone, t.ID)
		ui.OK("Completed task: %s", t.Title)
		ui.Note("\n  %s", displayPath(dir))
		noteForcedClose(forced)
		noteLandedFork(landed)
		WarnTaskBulk(root, dir)
	default:
		ui.OK("%s %s", pastVerb, t.ID)
	}
	return 0, nil
}

// claimRefusal is why a claim cannot take t, or nil for todo and in-progress work. A blocked task
// returns to todo only through unblock, the one path that records the human's answer and spends any
// audit authority, so a claim never moves it, answered or not. A done task stays in the archive.
func claimRefusal(t Item) error {
	switch t.State {
	case StateBlocked:
		then := [2]string{"Then claim it:", "coop tasks claim " + t.ID}
		if t.HasDecision && !answeredDecision(t) {
			return ui.CommandFailed(t.Title+" is blocked", "Its decision is waiting for your answer.",
				[2]string{"Answer it:", "coop tasks unblock " + t.ID + ` "<answer>"`}, then)
		}
		cause := "Its decision is answered. Unblock returns it to todo."
		if !t.HasDecision {
			cause = "It has no decision to answer. Unblock returns it to todo."
		}
		return ui.CommandFailed(t.Title+" is blocked", cause, [2]string{"Return it to todo:", "coop tasks unblock " + t.ID}, then)
	case StateDone:
		return ui.CommandFailed(t.Title+" is done", "Completed work stays in the archive.",
			[2]string{"For more work, add a task:", `coop tasks add "<title>"`})
	}
	return nil
}

// logClaim journals who took the task in its log.md, as park, block and unblock journal theirs.
// Best-effort: the claim already holds, and a journal line is no reason to undo it.
func logClaim(dir string, actor ClaimActor) {
	who := claimedBy(actor)
	if who == "" {
		user, host := taskOwnerIdentity()
		who = user + "@" + host
	}
	appendTaskLog(dir, "Claimed by "+who)
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
		return lookupExit(err), err
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
// counterpart of BlockTrustedTask, and deliberately the same shape (releaseUnderAuthority). It
// stands down an own lease helper first. A fork-owned task, a task another controller leases, and a
// claim held by another live process are refused — release gives up YOUR claim; it never overrides
// someone else's, never stops a box, and never clears a fork assignment.
func ReleaseTrustedTask(root string, t Item, actor ClaimActor) error {
	if _, err := stopOwnLeaseHolder(root, t, actor); err != nil {
		return err
	}
	return releaseUnderAuthority(root, t, func(record TaskOwnerRecord) error {
		if !competingClaim(record, actor) {
			return nil
		}
		running := "which is still running"
		if claimantElsewhere(record) {
			running = "which Coop cannot check from here — take it over: coop tasks claim --force " + t.ID
		}
		return fmt.Errorf("%w: %s is %s, %s", errTaskClaimedByOther, t.ID, TaskOwnerLabel(record), running)
	})
}

// trustedCompletionError draws a refused completion. A retry keeps the call's --force and
// --from-fork; the plain refusal never suggests --force.
func trustedCompletionError(err error, t Item, opts claimOptions) error {
	var recovery auditCompletionRecoveryError
	if errors.As(err, &recovery) {
		return err
	}
	retry := "coop tasks done " + ui.SafeInline(t.ID)
	if opts.forceClose {
		retry += " --force"
	}
	if opts.fromFork {
		retry += " --from-fork"
	}
	var held *forkHeldError
	if errors.As(err, &held) {
		return &drawnRefusal{usage: held.completionRefusal(), err: err}
	}
	if errors.Is(err, errFinalReviewRunning) {
		return ui.CommandFailed("Could not complete "+ui.SafeInline(t.ID),
			"A loop's final review is running, and it could reopen this task for its open items.",
			[2]string{"", "Try again when that review ends: " + retry})
	}
	var commits *PendingReviewCommitsError
	if errors.As(err, &commits) {
		return ui.CommandFailed("Could not complete "+ui.SafeInline(t.ID), commits.Cause(),
			[2]string{"", commits.Remedy() + ", then retry: " + retry})
	}
	var extra *ExtraBindingsError
	if errors.As(err, &extra) {
		return ui.CommandFailed("Could not complete "+ui.SafeInline(t.ID), extra.Cause(),
			[2]string{"", extra.Remedy() + ", then retry: " + retry})
	}
	var checklist *IncompleteChecklistError
	if errors.As(err, &checklist) {
		return &drawnRefusal{usage: checklistUsage(checklist, t), err: checklist}
	}
	return fmt.Errorf("%w — fix the obstruction, then retry: %s", err, retry)
}

// noteLandedFork tells the person which fork no longer holds the task, and why.
func noteLandedFork(landed *LandedForkTask) {
	switch {
	case landed == nil:
	case landed.Predates:
		ui.Note("  Commit %s carrying its trailer was on this branch before fork %s took the task, and this branch has every commit the fork made since. Fork %s no longer holds it.",
			shortCommit(landed.Commit), landed.Fork, landed.Fork)
	default:
		ui.Note("  Commit %s carrying its trailer is on this branch, so fork %s no longer holds it.",
			shortCommit(landed.Commit), landed.Fork)
	}
}

// noteForcedClose tells the person what a forced close left in the task's log.
func noteForcedClose(e *IncompleteChecklistError) {
	switch {
	case e == nil:
	case e.Total == 0:
		ui.Note("  Closed with --force. Its log.md notes that it had no checklist.")
	case e.Total-e.Done == 1:
		ui.Note("  Closed with --force. Its log.md names the checklist item still open.")
	default:
		ui.Note("  Closed with --force. Its log.md names the %d checklist items still open.", e.Total-e.Done)
	}
}

// drawnRefusal is a refusal drawn for the person that still matches the error it draws, such as
// ErrIncompleteChecklist or ErrTaskSandboxOwned, so a caller can tell why it was refused.
type drawnRefusal struct {
	usage *ui.UsageError
	err   error
}

func (e *drawnRefusal) Error() string   { return e.usage.Error() }
func (e *drawnRefusal) Unwrap() []error { return []error{e.usage, e.err} }

// checklistUsage is the person's view of an unfinished checklist: what is still open, the task.md
// that holds it, and the command to run once it is ticked.
func checklistUsage(e *IncompleteChecklistError, t Item) *ui.UsageError {
	// A folder name can come from a box, so the id and path are escaped like any other refusal.
	id, file := ui.SafeInline(t.ID), ui.SafeInline(displayPath(filepath.Join(t.Dir, "task.md")))
	headline, again := "Could not complete "+id, [2]string{"Then run:", "coop tasks done " + id}
	if t.State == StateDone {
		headline = id + " is already done, but its checklist is unfinished"
	}
	if e.Total == 0 {
		return ui.CommandFailed(headline,
			"It has no checklist.\nList its work as checklist items and tick only what is finished.",
			[2]string{"Add them to:", file}, again)
	}
	open := e.Total - e.Done
	cause := fmt.Sprintf("%d of %d checklist items are still open", open, e.Total)
	tick := "Finish them, then tick them in:"
	if open == 1 {
		cause = fmt.Sprintf("1 of %d checklist items is still open", e.Total)
		tick = "Finish it, then tick it in:"
	}
	shown, more := e.shownOpen()
	if len(shown) == 0 {
		cause += "."
	} else {
		cause += ":"
		for _, label := range shown {
			cause += "\n[ ] " + label
		}
		if more > 0 {
			cause += fmt.Sprintf("\n…and %d more", more)
		}
	}
	return ui.CommandFailed(headline, cause, [2]string{tick, file}, again)
}

// tasksFolderPath prints a task's resolved folder — the id-command companion to `coop fork path`,
// so `cat "$(coop tasks path <id>)/task.md"` works for humans and hooks. Reuses FindTask, so a slug
// fragment resolves and an absent/ambiguous id errors exactly like claim/done.
func tasksFolderPath(root string, args []string) (int, error) {
	if len(args) < 1 {
		return 2, ui.MissingArgument("task ID", "coop tasks path", "coop tasks path <task-id>")
	}
	t, err := FindTask(root, args[0])
	if err != nil {
		return lookupExit(err), err
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
	answer = strings.TrimSpace(strings.Join(args[1:], " "))
	// Recording a note that the answer is still to come would put the task in todo with its question
	// open, the state lint rejects, under a line saying the answer was saved.
	if answer != "" && !resolutionIsAnswer(answer) {
		return "", "", &ui.UsageError{
			Headline: fmt.Sprintf("%q reads as no answer yet", answer),
			Cause:    waitingAnswerNote,
			Rows:     [][2]string{{"Example:", fmt.Sprintf("coop tasks unblock %s \"<answer>\"", args[0])}, {"Help:", "coop help tasks unblock"}},
		}
	}
	return args[0], answer, nil
}

func parseTaskParkArgs(args []string) (id, reason string, err error) {
	if len(args) < 1 {
		return "", "", ui.MissingArgument("task ID", "coop tasks park", `coop tasks park <task-id> ["<reason>"]`)
	}
	for _, arg := range args[1:] {
		if arg != "-" && strings.HasPrefix(arg, "-") {
			return "", "", unknownOptionErr(arg, "coop tasks park", nil)
		}
	}
	return args[0], strings.TrimSpace(strings.Join(args[1:], " ")), nil
}

// tasksFolderPark sets a todo task aside, or answers a blocked task's decision with "not now": the
// folder moves to the backlog drawer with its log (and a blocked task's decision.md), so neither the
// loop nor a preflight can pick it up (the drawer is never scanned). `coop backlog promote` later
// brings it back to todo, or to blocked while its question is still open.
func tasksFolderPark(root string, args []string) (int, error) {
	id, reason, err := parseTaskParkArgs(args)
	if err != nil {
		return 2, err
	}
	t, err := FindTask(root, id)
	if err != nil {
		return lookupExit(err), err
	}
	if err := parkTask(root, t, reason); err != nil {
		var refusal *parkRefusal
		if errors.As(err, &refusal) {
			return 1, err
		}
		return -1, err
	}
	ui.OK("Parked: %s", t.Title)
	if t.State == StateBlocked {
		ui.Note("\n  It's in the backlog now, and its decision stays open.")
	} else {
		ui.Note("\n  It's in the backlog now.")
	}
	ui.Note("  Bring it back: coop backlog promote %s", t.ID)
	return 0, nil
}

// parkRefusal is a task that can't be parked as it stands; the message says what to do instead.
type parkRefusal struct{ msg string }

func (r *parkRefusal) Error() string { return r.msg }

// parkTask moves a todo or blocked task into the backlog, holding its owner record's lock so a
// concurrent claim can't land mid-move. It refuses a task with audit authority (only an explicit
// unblock may spend that) or a live claim, and records the owner's reason in log.md before the move.
// A todo task needs no decision to be set aside, so none is written.
func parkTask(root string, t Item, reason string) error {
	switch t.State {
	case StateTodo, StateBlocked:
	case StateInProgress:
		return &parkRefusal{fmt.Sprintf("%s is in progress: release it first with coop tasks release %s, then park it", t.ID, t.ID)}
	default:
		return &parkRefusal{fmt.Sprintf("%s is %s: only a todo or blocked task can be parked", t.ID, StateLabel(t.State))}
	}
	if _, audited, err := ReadAuditReopenRecord(root, t.ID); err != nil {
		return fmt.Errorf("inspect audit authority for %s: %w", t.ID, err)
	} else if audited {
		return &parkRefusal{fmt.Sprintf("%s was reopened by an audit and carries its authority: answer it with coop tasks unblock %s instead", t.ID, t.ID)}
	}
	owner, err := lockTaskOwner(root, t.ID)
	if err != nil {
		return err
	}
	defer owner.Close()
	if _, claimed, err := owner.Read(); err != nil {
		return err
	} else if claimed {
		return &parkRefusal{fmt.Sprintf("%s is claimed: release it first with coop tasks release %s", t.ID, t.ID)}
	}
	note := "Parked in the backlog by the owner."
	if t.State == StateBlocked {
		note = "Parked in the backlog by the owner; the decision stays open."
	}
	if reason != "" {
		note = "Parked in the backlog by the owner: " + reason
	}
	if err := AppendTaskLogStrict(t.Dir, note); err != nil {
		return fmt.Errorf("record the park in %s's log: %w", t.ID, err)
	}
	return MoveTaskDir(root, t, StateBacklog)
}

// tasksFolderUnblock moves a task out of 50_blocked/ back to 00_todo/ — but only if it's
// actually blocked, so a fat-fingered id can't silently reopen a done (or todo) task.
func tasksFolderUnblock(project, root string, args []string) (int, error) {
	id, answer, parseErr := parseTaskUnblockArgs(args)
	if parseErr != nil {
		return 2, parseErr
	}
	t, err := FindTask(root, id)
	if err != nil {
		return lookupExit(err), err
	}
	fork := taskForkName(root, t)
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
			committed, err := finishPendingAuditUnblock(project, root, t, record)
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
			noteForkResume(fork)
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
		return 2, &ui.UsageError{
			Headline: fmt.Sprintf("Task %q has no resolution yet", t.ID),
			Cause:    "Write the **Resolution:** in its decision.md, or give the answer here.\n" + waitingAnswerNote,
			Rows:     [][2]string{{"Example:", fmt.Sprintf("coop tasks unblock %s \"<answer>\"", args[0])}, {"Help:", "coop help tasks unblock"}},
		}
	}
	if err := resolveAndUnblock(project, root, t, answer); err != nil {
		return -1, unblockRetryError(t.ID, answer != "", err)
	}
	ui.OK("Unblocked task: %s", t.Title)
	if answer != "" {
		ui.Note("\nYour answer is saved in decision.md. The task is back in todo.")
	} else {
		ui.Note("\n  The task is back in todo.")
	}
	noteForkResume(fork)
	return 0, nil
}

// taskForkName is the fork that holds a task in the project's queue, or "" when none does or its
// owner record cannot be read: the name only labels what a person sees, and the commands that act
// on the task read the record themselves.
func taskForkName(root string, item Item) string {
	record, owned, err := readTaskOwnerRecordOf(root, item)
	if err != nil || !owned || record.Kind != TaskOwnerFork || record.Fork == nil {
		return ""
	}
	return record.Fork.Fork.Name
}

// forkResumeNote tells a person who answered a fork's decision how the fork goes on: its loop picks
// the task up by itself, and a stopped loop resumes with the board's restart command.
func forkResumeNote(fork string) string {
	return "Fork " + fork + " picks it up on its next pass. If its loop has stopped, resume it: coop fork " + fork + " --loop -d"
}

func noteForkResume(fork string) {
	if fork != "" {
		ui.Note("\n  %s", forkResumeNote(fork))
	}
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

// decisionResolved reports whether a decision.md holds a human's answer (decisionResolution). lint
// uses it: a resolved decision rides along on an unblocked (todo) task as its audit trail, but an
// unresolved one on a non-blocked task is the inconsistency to flag.
func decisionResolved(decPath string) (bool, error) {
	answer, err := recordedAnswer(decPath)
	return answer != "", err
}

// decisionResolution returns the human's answer from a decision.md body, or "" when its Resolution
// line holds none. Every reader comes through here, so "is it answered" and "what was the answer"
// can never disagree.
func decisionResolution(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if r, ok := strings.CutPrefix(line, decisionResolutionPrefix); ok {
			if r = strings.TrimSpace(r); resolutionIsAnswer(r) {
				return r
			}
			return ""
		}
	}
	return ""
}

// resolutionIsAnswer reports whether text on the Resolution line is an answer: not empty, not a
// comment or <…> placeholder, not punctuation alone, and not a note that the answer is still to come.
func resolutionIsAnswer(r string) bool {
	if strings.HasPrefix(r, "<!--") || isPlaceholder(r) {
		return false
	}
	words := strings.FieldsFunc(strings.ToLower(r), func(c rune) bool { return !unicode.IsLetter(c) && !unicode.IsNumber(c) })
	return len(words) > 0 && !waitingResolution.MatchString(strings.Join(words, " "))
}

// waitingResolution matches a Resolution, reduced to its lowercase words, that opens by saying the
// answer has not come yet. Agents have filled the line with notes such as "pending owner direction",
// and the owner took them for answers. Only the opening is read, because a note can run on ("Pending
// the user's answer. The work so far is committed.") while an answer leads with the decision
// ("Publish it, the pending review is fine"). That asks "Pending review is fine, publish it" again:
// nothing here can tell it from a note, and asking is the safe way to be wrong.
//
// Words that decide nothing may come first ("owner decision pending"). The last group counts only
// alone, since each of them also starts a real answer ("None of them", "Not yet, after the launch").
// An apostrophe splits a word in two, hence the "s" of "owner's" and "hasn t".
var waitingResolution = regexp.MustCompile(
	`^(((the|your|this) )?(([0-9]+|blocked|still|currently|remains|open|is|has|owner|user|human|s|` +
		`question|answer|decision|resolution|response|input|direction|approval|review) )+)?` +
		`((pending|await(ing|ed|s)|wait(ing|s) (for|on|to)|tbd|tbc|undecided|unanswered|unresolved|open question|` +
		`to be (decided|determined|answered|confirmed|resolved|filled)|` +
		`(not|hasn t) (yet )?(answered|decided|resolved|responded)|` +
		`(no (answer|decision|resolution|response)|none|nothing) yet|` +
		`needs? ((your|an?|the|owner|user|human|s) )*(answer|decision|input|direction|response))( |$)|` +
		`(waiting|todo|to do|open|none|not yet|no (answer|decision|resolution|response))$)`)

// waitingAnswerNote is what a person is told wherever an answer is refused for reading as a note.
const waitingAnswerNote = "Start the answer with what to do: text that opens with a word like pending, awaiting or TBD reads as still to come."

type unblockStageError struct {
	stage, artifact, state string
	answerSaved            bool // the person's answer is already in decision.md
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
	blocked := "task remains blocked"
	if failure != nil && failure.answerSaved {
		blocked = "task remains blocked, and your answer is saved in decision.md"
	} else if hasAnswer {
		command += ` "<answer>"`
	}
	stage := "unblock transition"
	remedy := "fix the task state named by the error"
	if errors.As(err, &failure) {
		stage = failure.stage
		switch failure.stage {
		case "audit authority validation":
			remedy = "undo the change the error names (a commit loses a trailer only through a history rewrite)"
		case "decision write":
			remedy = fmt.Sprintf("make %q a writable regular decision file", failure.artifact)
		case "fork decision copy":
			remedy = "repair the fork's copy of the task named by the error"
		case "task folder move":
			remedy = "remove or repair the source/destination task folder named by the error"
		case "audit authority persistence":
			remedy = "repair the host task-authority registry entry named by the error"
		}
	}
	return fmt.Errorf(
		"unblock %s failed during %s: %w — %s; %s, then retry: %s",
		id, stage, err, blocked, remedy, command,
	)
}

// resolveAndUnblock records answer (if non-empty) into the task's decision.md, then returns the
// task to 00_todo/ — NOT 10_in_progress/: in_progress is the "an agent is on this" lock taken by
// `claim`, so a just-unblocked task with nobody on it belongs in the queue as available work; the
// resolved decision.md rides along as the audit trail. Shared by `unblock` and the -i browser.
//
// The answer is saved before anything can refuse the unblock, so a refusal never loses what the
// person typed. A saved answer alone moves no task with audit authority: the loop's pre-flight
// leaves those to this command (UnblockResolved).
func resolveAndUnblock(project, root string, t Item, answer string) error {
	if answer != "" {
		decision := filepath.Join(t.Dir, "decision.md")
		if err := recordResolution(decision, answer); err != nil {
			return &unblockStageError{stage: "decision write", artifact: decision, state: StateBlocked, err: err}
		}
	}
	err := returnBlockedToTodo(project, root, t)
	var failure *unblockStageError
	if answer != "" && errors.As(err, &failure) {
		failure.answerSaved = true
	}
	return err
}

// returnBlockedToTodo is the unblock itself, once any answer is in decision.md.
func returnBlockedToTodo(project, root string, t Item) error {
	transition, err := prepareBlockedAuditReopenUnblock(project, root, t)
	if err != nil {
		return &unblockStageError{stage: "audit authority validation", state: StateBlocked, err: err}
	}
	// A fork works from its own copy of the task and reads the answer there, so the answer goes
	// into that copy too. The lease the transition holds keeps the fork's worker off it meanwhile.
	// Any other assignment phase is refused below, as before.
	owner, owned, err := ReadTaskOwnerRecord(root, t.ID)
	if err != nil {
		return &unblockStageError{stage: "owner record inspection", state: StateBlocked, err: transition.finish(err)}
	}
	if owned && owner.Kind == TaskOwnerFork && owner.Fork != nil &&
		(owner.Fork.Phase == ForkAssignmentBlocked || owner.Fork.Phase == ForkAssignmentPaused) {
		if err := syncForkDecision(project, root, t.ID, *owner.Fork, false); err != nil {
			err = fmt.Errorf("copy the answer into fork %s's copy of the task under %s: %w", owner.Fork.Fork.Name, owner.Fork.Projection, err)
			return &unblockStageError{stage: "fork decision copy", state: StateBlocked, err: transition.finish(err)}
		}
	}
	if err := moveBlockedAuditUnblock(root, t, transition); err != nil {
		return err
	}
	// Journal the answer the task came back with. Best-effort: the unblock already holds.
	dir := filepath.Join(root, StateTodo, t.ID)
	if recorded, _ := recordedAnswer(filepath.Join(dir, "decision.md")); recorded != "" {
		appendTaskLog(dir, "Unblocked: "+recorded)
	} else {
		appendTaskLog(dir, "Unblocked")
	}
	if note := transition.movedNote(); note != "" {
		appendTaskLog(dir, note)
	}
	return nil
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
			fork := *ownerRecord.Fork
			next.Fork = &fork
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

// MoveTaskDir renames a task's folder into root/newState, creating the state dir if needed. If the
// id already exists in newState (a torn move or a stray duplicate across states), it refuses with an
// actionable message rather than letting os.Rename fail with a raw "file exists" and stranding the
// task. (ReadTaskTree dedups such a duplicate on the READ side; this guards the WRITE side.) The
// queue is opened once and both state folders from it, each a real folder, and the rename goes
// through their handles: a box that swaps a state folder for a link cannot send the task elsewhere.
func MoveTaskDir(root string, t Item, newState string) error {
	fromState := filepath.Base(filepath.Dir(t.Dir))
	if filepath.Clean(t.Dir) != filepath.Join(root, fromState, t.ID) {
		return fmt.Errorf("can't move %s: its folder %q is not in the task queue %q", t.ID, t.Dir, root)
	}
	queue, err := openQueueRoot(root)
	if err != nil {
		return fmt.Errorf("open task queue: %w", err)
	}
	defer queue.Close()
	dest := filepath.Join(root, newState, t.ID)
	if fromState != newState {
		if _, err := queue.Lstat(filepath.Join(newState, t.ID)); err == nil {
			return fmt.Errorf("can't move %s to %s/: a task folder with that ID already exists at %q. Review both task folders and resolve the duplicate before retrying", t.ID, StateLabel(newState), dest)
		}
		if err := queue.Mkdir(newState, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	if _, err := queue.Lstat(filepath.Join(fromState, t.ID)); errors.Is(err, os.ErrNotExist) {
		// The source vanished between FindTask and now — a concurrent move to a DIFFERENT state
		// won the race. Report it as the actionable message this guard promises, not a raw ENOENT.
		return fmt.Errorf("can't move %s: it changed state under us (a concurrent move won) — re-run 'coop tasks'", t.ID)
	} else if err != nil || fromState == newState {
		return err
	}
	return moveProjectedTask(queue, fromState, newState, t.ID)
}

// openRealRoot opens path, which must be a real folder and not a link, as an os.Root.
func openRealRoot(path string) (*os.Root, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a link or not a real directory", path)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	if after, err := root.Stat("."); err != nil || !os.SameFile(before, after) {
		root.Close()
		return nil, errors.Join(err, fmt.Errorf("%s changed while it was opened", path))
	}
	return root, nil
}

// removeTaskFolder deletes t's folder through its state folder's handle, so a state folder a box
// swapped for a link cannot point the deletion at another folder.
func removeTaskFolder(root string, t Item) error {
	fromState := filepath.Base(filepath.Dir(t.Dir))
	if filepath.Clean(t.Dir) != filepath.Join(root, fromState, t.ID) {
		return fmt.Errorf("can't delete %s: its folder %q is not in the task queue %q", t.ID, t.Dir, root)
	}
	queue, err := openQueueRoot(root)
	if err != nil {
		return fmt.Errorf("open task queue: %w", err)
	}
	defer queue.Close()
	state, err := openRealSubroot(queue, fromState)
	if err != nil {
		return err
	}
	defer state.Close()
	if info, err := state.Lstat(t.ID); err != nil {
		return err
	} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("task folder %q is a link or not a real directory", t.Dir)
	}
	return state.RemoveAll(t.ID)
}

// removeTaskTmp deletes only taskDir/tmp, through the task folder's handle (OpenTaskMetadataRoot):
// the task folder and its state folder must be real folders, not links, and a tmp link is unlinked
// without following it, as are links nested below a real tmp. Missing tmp is the normal case.
func removeTaskTmp(taskDir string) error {
	root, err := OpenTaskMetadataRoot(taskDir)
	if err != nil {
		return fmt.Errorf("inspect task folder: %w", err)
	}
	defer root.Close()
	tmpDir := filepath.Join(taskDir, "tmp")
	if _, err := root.Lstat("tmp"); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect %q: %w", tmpDir, err)
	}
	if err := root.RemoveAll("tmp"); err != nil {
		return fmt.Errorf("remove %q: %w", tmpDir, err)
	}
	if _, err := root.Lstat("tmp"); !errors.Is(err, os.ErrNotExist) {
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

// NormalizeTaskState atomically replaces only Coop-owned lifecycle fields in a valid state.md.
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

// OpenTaskMetadataRoot opens the task folder taskDir as an os.Root for its files. It opens the queue
// above it once and the state folder and the task folder from there, each a real folder and not a
// link, so a box that swaps either for a link cannot send a host write into another folder.
func OpenTaskMetadataRoot(taskDir string) (*os.Root, error) {
	if taskDir == "" {
		return nil, errors.New("open task folder: no folder given") // never the working directory
	}
	taskDir = filepath.Clean(taskDir)
	if root, ok, err := openPinned(taskDir); ok {
		if err != nil {
			return nil, fmt.Errorf("open task folder %s: %w", taskDir, err)
		}
		return root, nil
	}
	queue, err := openRealRoot(filepath.Dir(filepath.Dir(taskDir)))
	if err != nil {
		return nil, fmt.Errorf("open task folder %s: %w", taskDir, err)
	}
	defer queue.Close()
	root, err := openRealSubroot(queue, filepath.Join(filepath.Base(filepath.Dir(taskDir)), filepath.Base(taskDir)))
	if err != nil {
		return nil, fmt.Errorf("open task folder %s: %w", taskDir, err)
	}
	return root, nil
}

// ReadTaskMetadataFile stays bounded and no-follow: a box can write task folders, so a task file
// may be a link, a FIFO or huge. Coop's own host-only records use config.ReadRecord instead.
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

// finalizeCompletedTask is the single post-move completion boundary. Check the current checklist
// before normalizing state or cleaning tmp; failed validation retains evidence for repair.
func finalizeCompletedTask(id, taskDir string) error {
	// The loop also reaches this boundary after a direct folder move. Never
	// turn that move into a receipt using an earlier, stale checklist.
	if err := requireCurrentCompletedChecklist(taskDir); err != nil {
		return err
	}
	return finishCompletedTask(id, taskDir)
}

// finishCompletedTask normalizes a done task's state.md and removes its tmp/. Only a person's
// forced close reaches it without the checklist check above.
func finishCompletedTask(id, taskDir string) error {
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
// channel; a task another controller leases is refused with ErrTaskLeased.
func BlockTrustedTask(root string, t Item, actor ClaimActor) error {
	return blockTrustedTask(root, t, actor, nil)
}

// blockTrustedTask is BlockTrustedTask with refuse, when set, judging an existing owner record under
// the owner lock; a box's tools refuse a task someone else holds (BlockBoxTask).
func blockTrustedTask(root string, t Item, actor ClaimActor, refuse func(TaskOwnerRecord) error) error {
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
	if owned && refuse != nil {
		if err := refuse(record); err != nil {
			return errors.Join(err, ownerLock.Close(), unlockLeaseFile(authority))
		}
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
var blockTextFlags = []string{"--question", "--why", "--option", "--recommendation"}

// parseBlockArgs reads `coop tasks block <id> [--question <text>] [--why <text>] [--option <text>]... [--recommendation <text>]`,
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
				return req, ui.UnexpectedArgument(arg, "coop tasks block", "coop tasks block <task-id>")
			}
			req.id = arg
			continue
		}
		flag, value, hasValue := strings.Cut(arg, "=")
		if !slices.Contains(blockTextFlags, flag) {
			return req, unknownOptionErr(flag, "coop tasks block", blockTextFlags)
		}
		if !hasValue {
			if i+1 >= len(args) {
				return req, ui.MissingOptionValue(flag, "coop tasks block", `coop tasks block login-retries --question "<question>" --why "<why>" --option "<option>" --recommendation "<choice>"`)
			}
			i++
			value = args[i]
		}
		if flag != "--option" && seen[flag] {
			return req, ui.RepeatedOption(flag, "coop tasks block")
		}
		seen[flag] = true
		req.filled = true
		value = strings.TrimSpace(value)
		switch flag {
		case "--question":
			req.decision.Question = value
		case "--why":
			req.decision.Why = value
		case "--recommendation":
			req.decision.Recommendation = value
		case "--option":
			if value != "" {
				req.decision.Options = append(req.decision.Options, value)
			}
		}
	}
	if req.id == "" {
		return req, ui.MissingArgument("task ID", "coop tasks block", "coop tasks block <task-id>")
	}
	if req.filled {
		var missing []string
		if req.decision.Question == "" {
			missing = append(missing, "--question")
		}
		if req.decision.Why == "" {
			missing = append(missing, "--why")
		}
		if len(req.decision.Options) == 0 {
			missing = append(missing, "--option")
		}
		if req.decision.Recommendation == "" {
			missing = append(missing, "--recommendation")
		}
		if len(missing) > 0 {
			return req, &ui.UsageError{
				Headline: fmt.Sprintf("Missing %s for \"coop tasks block\"", strings.Join(missing, ", ")),
				Cause:    "A decision request needs a question, why the task is blocked, at least one option and a recommendation.\nGive them all, or none to write decision.md yourself.",
				Rows:     [][2]string{{"Help:", "coop help tasks block"}},
			}
		}
		// The question is decision.md's heading, which is one line.
		if strings.ContainsAny(req.decision.Question, "\r\n") {
			return req, &ui.UsageError{
				Headline: "Option \"--question\" must be one line in \"coop tasks block\"",
				Cause:    "Put the explanation in --why.",
				Rows:     [][2]string{{"Help:", "coop help tasks block"}},
			}
		}
	}
	return req, nil
}

func tasksFolderBlock(root string, args []string) (int, error) {
	req, err := parseBlockArgs(args)
	if err != nil {
		return 2, err
	}
	// Before anything moves: text that would forge the human's answer is refused, not parked.
	if req.filled {
		if err := CheckDecision(req.decision); err != nil {
			return 2, err
		}
	}
	t, err := FindTask(root, req.id)
	if err != nil {
		return lookupExit(err), err
	}
	// A plain block would park the human's answer as a fresh question — it happened: a re-run block,
	// its first success hidden by `| tail`, asked again about something already decided. Refuse
	// before anything moves; a new question (--question) is still asked, keeping the answer.
	if !req.filled && t.HasDecision {
		answer, err := recordedAnswer(filepath.Join(t.Dir, "decision.md"))
		if err != nil {
			return -1, err
		}
		if answer != "" && t.State == StateBlocked {
			return 1, fmt.Errorf("%s is already answered (%q) — finish it instead of blocking it again: coop tasks unblock %s", t.ID, answer, t.ID)
		}
		if answer != "" {
			return 1, fmt.Errorf("%s was already answered (%q) — work it instead of blocking it again; to ask something new, block it with --question", t.ID, answer)
		}
	}
	// A plain block of a blocked task has nothing to do; saying "Blocked" would claim a move.
	if !req.filled && t.State == StateBlocked {
		ui.Note("%s is already blocked.", t.Title)
		if t.HasDecision {
			ui.Note("\n  %s", displayPath(filepath.Join(t.Dir, "decision.md")))
		}
		return 0, nil
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
	earlierAnswer := ""
	if req.filled {
		if earlierAnswer, err = saveRequestedDecision(taskDir, t.ID, t.Title, req.decision); err != nil {
			// The move already happened and is durable; say so, and say the request is not saved.
			// A conflict is the human's to resolve (1); anything else is a real failure (-1).
			code := -1
			if errors.Is(err, errDecisionAlreadyWritten) {
				code = 1
			}
			return code, fmt.Errorf("%s is blocked, but its decision request was not saved: %w", t.ID, err)
		}
	} else if err := writeDecisionScaffold(taskDir, t.ID, t.Title); err != nil {
		return -1, fmt.Errorf("%s is blocked, but its decision.md was not written: %w", t.ID, err)
	}
	// An agent often writes decision.md before it blocks; only the template still needs writing.
	written := req.filled
	if !written {
		body, exists, err := readOptionalTaskMetadataPath(dec)
		written = err == nil && exists && !decisionUnwritten(string(body))
	}
	if req.filled {
		appendTaskLog(taskDir, "Blocked: "+req.decision.Question)
	} else {
		appendTaskLog(taskDir, "Blocked for a decision in decision.md")
	}
	ui.OK("Blocked task: %s", t.Title)
	ui.Note("\n  %s", displayPath(dec))
	if earlierAnswer != "" {
		ui.Note("\nIt had been answered (%q); that decision is kept in its log.md.", earlierAnswer)
	}
	if !written {
		ui.Note("\nWrite the question, why the task is blocked, the options, and your recommendation in this file.")
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
const decisionScaffoldMarker = "<what needs to be chosen and why you cannot safely proceed>"

// decisionUnwritten reports whether a decision.md body is still the editable stub: nobody wrote its
// question, so `coop tasks decisions` has no question to show, only the file to fill in.
func decisionUnwritten(body string) bool { return strings.Contains(body, decisionScaffoldMarker) }

// decisionResolutionLine is the last line of every decision.md, stub or filled: the one place a
// human writes the answer, and the command that saves it for them.
const decisionResolutionLine = decisionResolutionPrefix + " <!-- Human: write your answer here, or use coop tasks unblock. -->\n"

// decisionResolutionPrefix opens that line — the one structural line an agent may never write.
const decisionResolutionPrefix = "**Resolution:**"

// writeDecisionScaffold seeds a blocked task's decision.md for a human to fill in, unless it already
// has one: an agent often writes it before blocking. The file is created new through the task
// folder's handle and never through a link, so a link a box planted there is refused.
func writeDecisionScaffold(taskDir, id, title string) error {
	root, err := OpenTaskMetadataRoot(taskDir)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.OpenFile("decision.md", os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if errors.Is(err, os.ErrExist) {
		_, readErr := ReadTaskMetadataFile(root, "decision.md")
		return readErr
	}
	if err != nil {
		return err
	}
	_, err = f.WriteString(renderDecisionScaffold(id, title))
	return errors.Join(err, f.Close())
}

func renderDecisionScaffold(id, title string) string {
	return "<!-- Explain the choice and your recommendation. A human supplies the answer.\n" +
		"     To save the answer and return this task to todo:\n" +
		"     coop tasks unblock " + id + " \"<answer>\" -->\n\n" +
		"# Decision: " + title + "?\n\n" +
		"**Blocks:** this task (" + id + ").\n\n" +
		"**Why it is blocked:** <what happened, and why the task cannot go on without an answer>\n\n" +
		"**The decision:** " + decisionScaffoldMarker + "\n\n" +
		"**Options:**\n\n" +
		"- **A — <name>:** <consequence>\n" +
		"- **B — <name>:** <consequence>\n\n" +
		"**Recommendation:** <your choice and why>\n\n" +
		"---\n\n" +
		decisionResolutionLine
}

// errDecisionAlreadyWritten is the refusal that protects a decision somebody already wrote — a
// human's answer, or an earlier agent's question. It is the caller's to resolve, not a failure.
var errDecisionAlreadyWritten = errors.New("the task already carries a decision somebody wrote")

// saveRequestedDecision writes a complete decision request into a blocked task's decision.md. It
// never destroys content somebody already wrote: the identical request again is a no-op (so a retry
// after a partial failure completes), an ANSWERED decision is kept in log.md before the new question
// replaces it (its answer comes back), and a different question nobody has answered yet is refused
// with the file to edit.
func saveRequestedDecision(taskDir, id, title string, d Decision) (earlierAnswer string, err error) {
	existing, exists, err := readOptionalTaskMetadataPath(filepath.Join(taskDir, "decision.md"))
	rendered := RenderDecision(id, d)
	switch {
	case err == nil && !exists:
	case err != nil:
		return "", err
	case string(existing) == rendered:
		return "", nil // the same request again — already saved, nothing to do
	case decisionResolution(string(existing)) != "":
		return ReplaceDecision(taskDir, id, title, d)
	case string(existing) != renderDecisionScaffold(id, title):
		return "", fmt.Errorf("%w — edit %s instead", errDecisionAlreadyWritten, displayPath(filepath.Join(taskDir, "decision.md")))
	}
	return "", WriteDecision(taskDir, id, title, d)
}

// TaskResolution returns the saved human answer, if any, using the bounded metadata reader.
func TaskResolution(taskDir string) (string, error) {
	return recordedAnswer(filepath.Join(taskDir, "decision.md"))
}

// recordedAnswer is the human's answer in a decision.md (decisionResolution), or "" when it holds none.
func recordedAnswer(decPath string) (string, error) {
	body, _, err := readOptionalTaskMetadataPath(decPath)
	if err != nil {
		return "", err
	}
	return decisionResolution(string(body)), nil
}

// answeredDecision reports whether a blocked task already carries the human's answer — it waits
// only for `coop tasks unblock`, so it is no question to ask again. An unreadable decision counts
// as unanswered: the listing then asks, which is the safe way to be wrong.
func answeredDecision(t Item) bool {
	if t.State != StateBlocked || !t.HasDecision {
		return false
	}
	answered, err := decisionResolved(filepath.Join(t.Dir, "decision.md"))
	return err == nil && answered
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
				return taskRemoveArgs{}, taskRemoveUsage(fmt.Sprintf("Unexpected argument %q for %q", a, "coop tasks rm"))
			}
			request.id = a
		}
	}
	switch {
	case request.allDone && request.id != "":
		return taskRemoveArgs{}, taskRemoveUsage(`Cannot combine a task ID and "--all-done" in "coop tasks rm"`)
	case !request.allDone && request.id == "":
		return taskRemoveArgs{}, taskRemoveUsage(`Missing task ID for "coop tasks rm"`)
	}
	return request, nil
}

// taskRemoveUsage refuses rm's arguments with both of its spellings: one task, or the whole archive.
func taskRemoveUsage(headline string) *ui.UsageError {
	return &ui.UsageError{
		Headline: headline,
		Rows: [][2]string{
			{"Usage:", "coop tasks rm <id> [--yes]"},
			{ui.Continuation, "coop tasks rm --all-done [--yes]"},
			{"Help:", ui.HelpCommand("coop tasks rm")},
		},
	}
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
			return 2, cancelledDeletion(err, "coop tasks rm", "No tasks deleted.")
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
		return lookupExit(err), err
	}
	ui.Note("Permanently delete %q, including its instructions,\nprogress, and saved evidence.\n", t.Title)
	ui.Note("  %s\n", displayPath(t.Dir))
	if err := ui.DestroyGate("Delete this task", request.yes); err != nil {
		return 2, cancelledDeletion(err, "coop tasks rm", "No tasks deleted.")
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
// NOT happen and returns the already-reported sentinel. A gate nothing could answer (a pipe with no
// --yes) is refused in the shared block, pointing at command's page.
func cancelledDeletion(err error, command, nothing string) error {
	if err != nil && err.Error() == "cancelled" {
		ui.Note("Cancelled. %s", nothing)
		return ui.ErrReported
	}
	if errors.Is(err, ui.ErrNeedsConfirmation) {
		return ui.ConfirmationRequired(command)
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
		removePendingReviewRecord(root, task.ID),
		removeLeaseAuthorityMetadata(root, task.ID),
		removeAuditReopenRecord(root, task.ID),
		removeTrustedDoneDeparture(root, task.ID),
		removeTaskOwnerRecord(root, task.ID),
	); err != nil {
		return false, err
	}
	if err := removeTaskFolder(root, task); err != nil {
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

// tasksFolderList prints the queue, then warns about anything left at the queue root that is not
// part of it (see warnQueueRootStrays).
func tasksFolderList(root string, all bool, only ...string) (int, error) {
	code, err := printTaskTree(root, "Tasks", all, only...)
	if err == nil {
		warnQueueRootStrays(root)
	}
	return code, err
}

// printTaskTree prints the queue grouped by state, under heading ("" when the caller already named
// the queue). only narrows it to the given states (a --blocked/--todo/… filter); empty shows every
// state. Each task is three lines at most — its title, the facts that are exceptional about it, and
// its id as an OSC 8 hyperlink to its folder, so it opens on click in a supporting terminal and
// stays plain text in a pipe. There is no count footer: the section headings already carry the counts.
func printTaskTree(root, heading string, all bool, only ...string) (int, error) {
	items, err := ReadTaskTree(root)
	if err != nil {
		return -1, err
	}
	p := ui.For(os.Stdout)
	if len(items) == 0 {
		// Saved ideas are not tasks, but "no tasks" alone would hide them. The count is best-effort:
		// `coop backlog` is where an unreadable drawer is reported.
		ideas, _ := ReadBacklog(root)
		headline := "No tasks yet."
		if len(ideas) > 0 {
			headline += " The backlog holds " + ui.Count(len(ideas), "idea") + "."
		}
		fmt.Printf("%s\n\n", p.Bold(headline))
		fmt.Printf("  Create one: coop tasks add %q\n", "Describe the work")
		if len(ideas) > 0 {
			fmt.Printf("  See ideas:  coop backlog\n")
		}
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
		if state == StateTodo {
			slices.SortStableFunc(ts, byPriority) // the order the loop claims them in
		}
		if !printed && heading != "" {
			fmt.Printf("%s\n", p.Bold(heading))
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
	// The one action the listing can offer: a blocked task is waiting on a human. An answered one
	// waits only for its unblock, which its own row names.
	waiting := 0
	for _, t := range byState[StateBlocked] {
		if !answeredDecision(t) {
			waiting++
		}
	}
	if waiting > 0 && (len(show) == 0 || show[StateBlocked]) {
		fmt.Printf("\nAnswer blocked tasks: coop tasks decisions -i\n")
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
// marker + bold path + a dim rule filling the width — a clean divider between queues; piped, it is
// the queue's path alone, the same name its folder has.
func banner(p ui.Palette, path string) string {
	if !p.Enabled() {
		return path
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
// id: a high or low priority, subtask progress, a blocked task's waiting answer, and an in-progress
// task's owner or reservation — joined with " · ", or "" when there is nothing to say. An ordinary
// todo task, and an in-progress task nobody has claimed or reserved, carry no marker at all: a row
// that always says something says nothing (see rule tag-exceptions-not-every-row).
func listMarkers(p ui.Palette, t Item) string {
	var parts []string
	if t.State != StateDone {
		switch priorityRank(t) {
		case 0:
			parts = append(parts, "High priority")
		case 2:
			parts = append(parts, p.Gray("Low priority"))
		}
	}
	if n := len(t.Subtasks); n > 0 {
		prog := fmt.Sprintf("%d/%d subtasks", t.doneSubtasks(), n)
		if t.doneSubtasks() == n {
			prog = p.Gray(prog) // fully done — recede the count
		}
		parts = append(parts, prog)
	}
	if answeredDecision(t) {
		parts = append(parts, p.Yellow("Answered · finish it: coop tasks unblock "+t.ID))
	} else if t.State == StateBlocked {
		parts = append(parts, p.Red("Needs your answer"))
	}
	if t.State == StateInProgress {
		if m := inProgressMarker(t); m != "" {
			parts = append(parts, p.Dim(m))
		}
	}
	if t.WaitsOn != "" && t.State != StateDone {
		parts = append(parts, p.Dim("Waits for "+t.WaitsOn))
	}
	return strings.Join(parts, p.Dim(" · "))
}

// inProgressMarker labels an in-progress task with the one fact that explains whether the loop will
// touch it next: a durable human claim beats the lease-derived reservation, because a claimed task
// with nobody actively holding its lease would otherwise read as free work (it never is: see
// AssignLoopTaskOnly). A claim whose process has died is the exception worth flagging on its own —
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

func tasksFolderDecisions(project, root string, args []string) (int, error) {
	interactive := false
	for _, a := range args {
		switch a {
		case "-i", "--interactive":
			interactive = true
		default:
			return 2, decisionsArgumentErr(a)
		}
	}
	return queueDecisions(project, root, interactive, readyMerges(project, []string{root}))
}

// queueDecisions lists or walks one queue's open decisions, then the forks ready to merge. The
// multi-queue roll-up passes no merges and lists them once after every queue.
func queueDecisions(project, root string, interactive bool, merges []readyMerge) (int, error) {
	var decisions, answered []Item
	items, err := ReadTaskTree(root)
	if err != nil {
		return -1, err
	}
	for _, t := range items {
		switch {
		case answeredDecision(t):
			answered = append(answered, t)
		case t.State == StateBlocked:
			decisions = append(decisions, t)
		}
	}
	p := ui.For(os.Stdout)
	if interactive && len(decisions)+len(merges) > 0 {
		code, err := decisionsInteractive(project, root, decisions, merges)
		printAnsweredDecisions(p, answered, true)
		return code, err
	}
	if len(decisions) == 0 {
		ui.Note("No questions waiting for your answer.")
		printAnsweredDecisions(p, answered, false)
		printReadyMerges(p, merges, len(answered) > 0)
		return 0, nil
	}
	fmt.Printf("%s%s\n", p.Bold("Questions waiting for your answer"), p.Dim(fmt.Sprintf(" · %d", len(decisions))))
	for _, t := range decisions {
		decisionID := t.ID // the handle unblock takes, then the fork that asked
		if fork := taskForkName(root, t); fork != "" {
			decisionID += " · in fork " + fork
		}
		question := t.Title
		rec := ""
		decPath := filepath.Join(t.Dir, "decision.md")
		body, exists, err := readOptionalTaskMetadataPath(decPath)
		if err != nil {
			return -1, err
		}
		if !exists || decisionUnwritten(string(body)) {
			// The scaffold's "<title>?" and its placeholders are no question: name the file to write.
			fmt.Printf("\n  %s\n", sanitizeCell(t.Title))
			fmt.Printf("    %s %s\n", p.Dim("Decision not written yet:"), displayPath(decPath))
			fmt.Printf("    %s\n", p.Faint(decisionID))
			continue
		}
		if q := decisionQuestion(string(body)); q != "" {
			question = q
		}
		for _, line := range strings.Split(string(body), "\n") {
			if r, ok := strings.CutPrefix(line, "**Recommendation:**"); ok {
				rec = strings.TrimSpace(r)
			}
		}
		// Question first (what you weigh), the recommendation under it, then the id (the handle you
		// `unblock` with) — the same title/markers/id shape as the task list.
		fmt.Printf("\n  %s\n", sanitizeCell(question))
		if rec != "" && !isPlaceholder(rec) {
			fmt.Printf("    %s %s\n", p.Dim("Recommendation:"), sanitizeCell(rec))
		}
		fmt.Printf("    %s\n", p.Faint(decisionID))
	}
	fmt.Print("\nAnswer one: coop tasks unblock <id> \"<answer>\"\n")
	fmt.Print("Read and answer each: coop tasks decisions -i\n")
	printAnsweredDecisions(p, answered, true)
	printReadyMerges(p, merges, true)
	return 0, nil
}

// decisionQuestion is the question a decision.md asks — its "# Decision:" heading — or "" without one.
func decisionQuestion(body string) string {
	question := ""
	for _, line := range strings.Split(body, "\n") {
		if q, ok := strings.CutPrefix(line, "# Decision:"); ok {
			question = strings.TrimSpace(q)
		}
	}
	return question
}

// printAnsweredDecisions lists blocked tasks the human already answered — never a question to ask
// again — each with the one command that finishes it. gap separates it from a listing above.
func printAnsweredDecisions(p ui.Palette, answered []Item, gap bool) {
	if len(answered) == 0 {
		return
	}
	if gap {
		fmt.Print("\n")
	}
	fmt.Printf("%s%s\n", p.Bold("Answered, still blocked"), p.Dim(fmt.Sprintf(" · %d", len(answered))))
	for _, t := range answered {
		answer, _ := recordedAnswer(filepath.Join(t.Dir, "decision.md"))
		fmt.Printf("\n  %s\n", sanitizeCell(t.Title))
		fmt.Printf("    %s %s\n", p.Dim("Answer:"), sanitizeCell(answer))
		fmt.Printf("    Finish it: coop tasks unblock %s\n", t.ID)
	}
}

// decisionRef locates one open decision for the browser: the queue root that owns it, the task
// id (re-resolved on each visit, so a concurrent move is caught), and a display label naming the
// queue in a multi-queue session ("" when there is only one queue — no label needed). A ref with a
// merge is a fork ready to merge instead.
type decisionRef struct {
	root, label, id string
	merge           *readyMerge
}

// decisionRefs turns one queue's blocked tasks into browser refs, labeled for the roll-up.
func decisionRefs(root, label string, decisions []Item) []decisionRef {
	refs := make([]decisionRef, len(decisions))
	for i, t := range decisions {
		refs[i] = decisionRef{root: root, label: label, id: t.ID}
	}
	return refs
}

// decisionsArgumentErr refuses an argument coop tasks decisions does not take.
func decisionsArgumentErr(arg string) error {
	if strings.HasPrefix(arg, "-") {
		return unknownOptionErr(arg, "coop tasks decisions", []string{"-i", "--interactive"})
	}
	return ui.UnexpectedArgument(arg, "coop tasks decisions", "coop tasks decisions [-i]")
}

func decisionsNeedTerminalErr() error {
	return &ui.UsageError{
		Headline: "\"coop tasks decisions -i\" needs an interactive terminal",
		Cause:    "Run it directly in your terminal, or answer one decision with coop tasks unblock.",
		Rows:     [][2]string{{"Example:", "coop tasks unblock login-retries \"<answer>\""}, {"Help:", "coop help tasks decisions"}},
	}
}

// decisionsInteractive walks the open decisions one at a time on a tty (`coop tasks decisions -i`):
// each is shown in full, an answer is read and recorded (unblocking the task), and :n / :p move
// between them, :q stops. The forks ready to merge follow, each offering its review and merge. It
// needs a real terminal — in a pipe or the unattended loop there's nobody to answer, so it errors
// instead of hanging.
func decisionsInteractive(project, root string, decisions []Item, merges []readyMerge) (int, error) {
	if !ui.IsTerminal(os.Stdin) {
		return 2, decisionsNeedTerminalErr()
	}
	refs := append(decisionRefs(root, "", decisions), mergeRefs(merges)...)
	return runDecisionBrowser(project, refs, os.Stdin, os.Stdout)
}

// runDecisionBrowser is the interactive loop with its I/O injected, so it can be driven in a
// test. Each ref carries its own queue root, so one session can span several queues of the one
// project whose history an answer's unblock audits.
func runDecisionBrowser(project string, refs []decisionRef, in io.Reader, out io.Writer) (int, error) {
	p := ui.For(os.Stdout)
	sc := bufio.NewScanner(in)
	answered, parked, deleted, merged := 0, 0, 0, 0
	questions := 0
	for _, ref := range refs {
		if ref.merge == nil {
			questions++
		}
	}
	for i := 0; i >= 0; {
		ref := refs[i]
		if ref.merge != nil {
			// A fork ready to merge is no question: its page runs the fork's own review and merge
			// commands, then reads back whether the merge happened.
			fmt.Fprintf(out, "\n%s\n", decisionDivider(p, i+1, len(refs), "fork "+ref.merge.fork))
			fprintMergePage(out, p, *ref.merge)
			if !sc.Scan() {
				break // EOF / ^D ends the session
			}
			switch strings.TrimSpace(sc.Text()) {
			case ":r":
				runForkFrom(out, "review", ref.merge.fork)
			case ":m":
				runForkFrom(out, "merge", ref.merge.fork)
				if !forkReadyToMerge(project, ref.merge.fork) {
					merged++
					refs = append(refs[:i], refs[i+1:]...)
				}
			case ":q", ":quit":
				i = -1
			case ":p":
				if i > 0 {
					i--
				}
			case ":n", "":
				i++
			default:
				fmt.Fprintf(out, "  Type :r to review the fork, :m to merge it, or press Enter to skip it.\n")
			}
			if i >= len(refs) {
				i = -1
			}
			continue
		}
		t, err := FindTask(ref.root, ref.id)
		if err != nil {
			return -1, err
		}
		fork := taskForkName(ref.root, t)
		decPath := filepath.Join(t.Dir, "decision.md")
		where := t.ID
		if ref.label != "" {
			where = ref.label + " · " + t.ID // say which queue this decision lives in
		}
		if fork != "" {
			where += " · in fork " + fork
		}
		fmt.Fprintf(out, "\n%s\n", decisionDivider(p, i+1, len(refs), sanitizeCell(where)))
		body, exists, err := readOptionalTaskMetadataPath(decPath)
		if err != nil {
			return -1, err
		}
		if !exists || decisionUnwritten(string(body)) {
			fmt.Fprintf(out, "\n%s\n  %s %s\n", p.Bold(sanitizeCell(t.Title)), p.Dim("Decision not written yet:"), sanitizeCell(displayPath(decPath)))
		} else {
			fprintDecisionBody(out, p, string(body))
		}
		prompt := "Your answer (Enter to skip):"
		if answer := decisionResolution(string(body)); answer != "" {
			// The existing answer is shown, and Enter keeps it — the safe key must never be the
			// one that silently discards a decision somebody already made.
			fmt.Fprintf(out, "  %s %s\n", p.Dim("Answered:"), sanitizeCell(answer))
			prompt = "Your answer (Enter to keep it):"
		}
		key := func(k string) string { return p.Cyan(k) }
		fmt.Fprintf(out, "\n%s\n  %s%s%s%s%s%s%s%s%s\n%s ",
			prompt,
			key(":n"), p.Dim(" next · "), key(":p"), p.Dim(" previous · "),
			key(":b"), p.Dim(" park in backlog · "),
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
		// :b parks the task in the backlog with its decision still open (coop tasks park), so a
		// "not now" answer doesn't re-queue it. A refused park says why and stays on this decision.
		if line == ":b" {
			if err := parkTask(ref.root, t, ""); err != nil {
				var refusal *parkRefusal
				if !errors.As(err, &refusal) {
					return -1, err
				}
				fmt.Fprintf(out, "  %s\n", refusal.msg)
				continue
			}
			parked++
			refs = append(refs[:i], refs[i+1:]...)
			if i >= len(refs) {
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
			if !resolutionIsAnswer(line) {
				fmt.Fprintf(out, "  %s\n", waitingAnswerNote)
				continue // nothing saved: ask this question again
			}
			if t.State == StateBlocked {
				if err := resolveAndUnblock(project, ref.root, t, line); err != nil {
					return -1, unblockRetryError(t.ID, true, err)
				}
				answered++
				if fork != "" {
					fmt.Fprintf(out, "\n  %s\n", forkResumeNote(fork))
				}
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
	if err := sc.Err(); err != nil {
		return -1, fmt.Errorf("read decision answer: %w", err)
	}
	// Answers, parks, deletions and merges are counted separately: they are different outcomes, and
	// a session that did several must not report any one of them as the whole story.
	var outcomes []string
	if answered > 0 {
		outcomes = append(outcomes, fmt.Sprintf("answered %s — %s returned to todo", ui.Count(answered, "question"), pluralTasks(answered)))
	}
	if parked > 0 {
		outcomes = append(outcomes, "parked "+ui.Count(parked, "task")+" in the backlog")
	}
	if deleted > 0 {
		outcomes = append(outcomes, "deleted "+ui.Count(deleted, "task"))
	}
	if merged > 0 {
		outcomes = append(outcomes, "merged "+ui.Count(merged, "fork"))
	}
	if len(outcomes) == 0 && questions == 0 {
		ui.Note("No forks merged.")
		return 0, nil
	}
	if len(outcomes) == 0 {
		ui.Note("No answers saved. Tasks are still blocked.")
		return 0, nil
	}
	summary := strings.Join(outcomes, " · ")
	ui.OK("%s", strings.ToUpper(summary[:1])+summary[1:])
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
	// An agent in a box can write this file, and this prints to the terminal of the person answering.
	for _, raw := range strings.Split(stripHTMLComments(content), "\n") {
		line := strings.TrimSpace(sanitizeCell(raw))
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
// task.md body — the structural half of "self-contained"; unfilledParts is the other half. Derived
// from taskSections so lint and the scaffold share one shape source.
func taskShapeIssues(body string) []string {
	var issues []string
	for _, s := range taskSections {
		if !strings.Contains(body, "**"+s.heading+":**") {
			issues = append(issues, "missing the **"+s.heading+"** section")
		}
	}
	return issues
}

// unfilledParts names what a task.md still leaves to write: each required section that is empty or
// wholly a template placeholder, then "a subtask" when a checklist item is one. A fresh scaffold is
// all placeholders. The loop still takes such a task, because the scaffold's header tells its agent
// to fill them in before any other work, but lint names it so a person sees what is unwritten.
func unfilledParts(content string) []string {
	var parts []string
	for _, s := range taskSections {
		if text, ok := sectionText(content, s.heading); ok && (text == "" || isPlaceholder(text)) {
			parts = append(parts, s.heading)
		}
	}
	for _, item := range ScanChecklist(content) {
		if isPlaceholder(item.Label) {
			return append(parts, "a subtask")
		}
	}
	return parts
}

// sectionText is the text of the **heading:** section in content: the rest of its line and the
// lines under it, up to the next required section or markdown heading outside a code fence. ok is
// false when there is no such section.
func sectionText(content, heading string) (text string, ok bool) {
	var lines []string
	inFence := false
	for _, line := range strings.Split(content, "\n") {
		boundary := !inFence && (strings.HasPrefix(line, "#") || startsTaskSection(line))
		if fenceMarker(line) {
			inFence = !inFence
		}
		switch {
		case ok && boundary:
			return strings.TrimSpace(strings.Join(lines, "\n")), true
		case ok:
			lines = append(lines, line)
		case boundary && strings.HasPrefix(line, "**"+heading+":**"):
			ok = true
			lines = append(lines, strings.TrimPrefix(line, "**"+heading+":**"))
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), ok
}

func startsTaskSection(line string) bool {
	for _, s := range taskSections {
		if strings.HasPrefix(line, "**"+s.heading+":**") {
			return true
		}
	}
	return false
}

// isPlaceholder reports whether s is wholly one <…> placeholder, as the templates write them. Text
// that only uses angle brackets, such as "The <div> wrapper" or "<Button> renders <Icon>", is not.
func isPlaceholder(s string) bool {
	s = strings.TrimSpace(s)
	return len(s) >= 2 && s[0] == '<' && s[len(s)-1] == '>' && !strings.ContainsAny(s[1:len(s)-1], "<>")
}

// joinWithAnd reads a list the way a sentence does: "a, b and c".
func joinWithAnd(parts []string) string {
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

func tasksFolderLint(root string) (int, error) {
	items, err := ReadTaskTree(root)
	if err != nil {
		return -1, err
	}
	var findings []string
	// A box names the folders, so a name is escaped before it reaches the terminal.
	add := func(id, msg string) { findings = append(findings, fmt.Sprintf("  %s: %s", ui.SafeInline(id), msg)) }
	// Every queue needs all four state dirs, or the move-a-folder-between-states protocol renames a
	// task into a missing dir and silently corrupts the queue (see ScaffoldStateDirs). Flag any queue
	// that lacks one.
	if fi, err := os.Stat(root); err == nil && fi.IsDir() {
		for _, st := range TaskStates {
			if s, e := os.Stat(filepath.Join(root, st)); e != nil || !s.IsDir() {
				add(st, "state directory is missing. Run 'coop init' to restore it")
			}
		}
	}
	// ReadTaskTree and ReadBacklog skip a regular file where only task folders belong (a Finder
	// .DS_Store must not hide the queue) and a folder with no task.md, so lint is where either
	// becomes visible instead of staying silently unread: a task someone wrote as a file, or evidence
	// saved into a folder that is no task (a shell writes to an unmatched glob as a literal name).
	// Dot-named entries are tool scratch, Coop's own staging folders included.
	for _, st := range append(slices.Clone(TaskStates), StateBacklog) {
		if info, err := os.Lstat(filepath.Join(root, st)); err != nil || !info.IsDir() {
			continue // a missing state dir is reported above; ReadTaskTree and coop backlog refuse a linked one
		}
		entries, err := os.ReadDir(filepath.Join(root, st))
		if err != nil {
			return -1, err
		}
		for _, e := range entries {
			name := e.Name()
			switch {
			case strings.HasPrefix(name, "."):
			case e.Type().IsRegular():
				add(st+"/"+name, "is a file, not a task folder — every task is a folder holding a task.md; move it into one or remove it")
			case e.IsDir():
				if _, err := os.Lstat(filepath.Join(root, st, name, "task.md")); errors.Is(err, os.ErrNotExist) {
					add(st+"/"+name, "is a folder with no task.md, so Coop does not see it as a task — move its files into the task folder they belong to, or remove it")
				} else if err != nil {
					return -1, err
				}
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
		fields, _ := SplitFrontmatter(body)
		if fields["status"] != "" {
			add(t.ID, "has a `status:` field — remove it; the parent directory is the status")
		}
		// the loop reads an unknown priority as normal, so a typo would silently change nothing
		if v, ok := fields["priority"]; ok && t.State != StateDone && !slices.Contains(taskPriorities, v) {
			add(t.ID, fmt.Sprintf("has priority %q — use high, normal or low", v))
		}
		// self-contained: frontmatter, every shape section present, and none still the template's
		// placeholder (not for done, which is the shipped record).
		if t.State != StateDone {
			if _, _, ok := frontmatterLines(body); !ok {
				add(t.ID, "task.md has no frontmatter — start it with the --- block of id, title, labels and updated")
			}
			for _, issue := range taskShapeIssues(body) {
				add(t.ID, "not self-contained: "+issue)
			}
			if parts := unfilledParts(body); len(parts) > 0 {
				add(t.ID, "not filled in yet: "+joinWithAnd(parts)+" (an agent that takes the task fills them in first)")
			}
		}
		// A done task is the shipped record: it says what was done, not what Coop's scaffold asked
		// for. A state.md without the four fields is skipped here rather than guessed at. An empty
		// log.md is not named: completion refuses one now, and listing the archive's old ones would
		// bury the findings someone can act on.
		if t.State == StateDone {
			if fields, err := ReadTaskState(t.Dir); err == nil {
				if labels := StateTemplateFields(fields); len(labels) > 0 {
					add(t.ID, "state.md still holds template text in "+joinWithAnd(labels)+" — write what the next reader needs, or — when there is nothing")
				}
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
