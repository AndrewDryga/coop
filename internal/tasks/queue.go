package tasks

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/ui"
)

// LegacyForkQueueWithWork detects the retired pre-projection design, where a fork carried a copied
// task queue. Those folders are duplicate authority and must never be merged or scheduled as if
// they were canonical. Empty scaffolds are harmless; any lifecycle task requires explicit human
// migration before the fork may proceed.
func LegacyForkQueueWithWork(workspace string) (string, error) {
	candidates := []string{filepath.Join(workspace, TasksRoot)}
	if rels, err := project.TaskDirs(workspace); err == nil {
		for _, rel := range rels {
			candidate := filepath.Join(workspace, rel)
			if !slices.Contains(candidates, candidate) {
				candidates = append(candidates, candidate)
			}
		}
	}
	for _, root := range candidates {
		items, err := ReadTaskTree(root)
		if err != nil {
			return "", err
		}
		if len(items) == 0 {
			continue
		}
		if rel, err := filepath.Rel(workspace, root); err == nil {
			return rel, nil
		}
		return root, nil
	}
	return "", nil
}

// ExtractTasksFlags pulls every `--tasks <path>` (or `--tasks=<path>`) out of args,
// returning the collected paths and the remaining args. Shared by `coop tasks` and
// `coop loop` so they accept the same repeatable flag. A trailing `--tasks` with no path
// is an error, not a silently-dropped flag.
func ExtractTasksFlags(command string, args []string) (tasks []string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		if v, n, ok, e := flagValue(args, i, "--tasks"); ok {
			if e != nil {
				return nil, nil, ui.MissingOptionValue("--tasks", command, command+" --tasks .agent/tasks")
			}
			tasks = append(tasks, v)
			i += n - 1
			continue
		}
		rest = append(rest, args[i])
	}
	return tasks, rest, nil
}

// extractProjectFlag pulls the add-only `--project <name>` (or `--project=<name>`) out of args.
// It is intentionally separate from ExtractTasksFlags: --tasks targets a raw queue path for every
// tasks command, while --project is the friendly umbrella-project selector for creation only.
func extractProjectFlag(args []string) (projectName string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		if v, n, ok, e := flagValue(args, i, "--project"); ok {
			if e != nil {
				return "", nil, ui.MissingOptionValue("--project", "coop tasks add", `coop tasks add --project web "Fix login retries"`)
			}
			if projectName != "" {
				return "", nil, ui.RepeatedOption("--project", "coop tasks add")
			}
			projectName = v
			i += n - 1
			continue
		}
		rest = append(rest, args[i])
	}
	return projectName, rest, nil
}

type taskProjectChoice struct {
	Name string
	Rel  string
	Note string
}

// taskProjectChoices returns the add targets for an umbrella project. The root queue is always an
// explicit choice for repo-wide/cross-project work, even if it has not been created yet; add can
// bootstrap it just like any other queue.
func taskProjectChoices(repo string) ([]taskProjectChoice, bool, error) {
	p, err := project.Load(repo)
	if err != nil {
		return nil, false, err
	}
	if len(p.Subprojects) == 0 {
		return nil, false, nil
	}
	choices := []taskProjectChoice{{
		Name: "root",
		Rel:  filepath.Join(".agent", "tasks"),
		Note: "repo-wide or cross-project work",
	}}
	for _, sub := range p.Subprojects {
		choices = append(choices, taskProjectChoice{
			Name: sub,
			Rel:  filepath.Join(sub, ".agent", "tasks"),
		})
	}
	return choices, true, nil
}

// findTaskProjectChoice resolves --project by full path first, then by BASENAME when that is
// unambiguous. Members are repo-relative paths, and a nested one is a mouthful
// (terraform/environments/va1) — typing the last segment is what anyone actually reaches for. The
// shorthand only resolves when exactly one member ends in that segment: with two (apps/web and
// site/web) the name is genuinely ambiguous and picking one silently would file work in the wrong
// queue, so it stays an error listing the candidates.
func findTaskProjectChoice(choices []taskProjectChoice, name string) (taskProjectChoice, bool) {
	for _, ch := range choices {
		if ch.Name == name {
			return ch, true
		}
	}
	var matches []taskProjectChoice
	for _, ch := range choices {
		if ch.Name != "root" && path.Base(ch.Name) == name {
			matches = append(matches, ch)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return taskProjectChoice{}, false
}

// workingSubproject is the umbrella member holding the working folder, the nearest one when members
// nest. The shell may reach the repository through a link, so the folder is matched, not spelled.
func workingSubproject(repo string, choices []taskProjectChoice) (taskProjectChoice, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return taskProjectChoice{}, false
	}
	rel, err := box.RepoRelative(repo, cwd)
	if err != nil || !filepath.IsLocal(rel) {
		return taskProjectChoice{}, false
	}
	var members []string
	for _, ch := range choices {
		if ch.Name != "root" {
			members = append(members, ch.Name)
		}
	}
	member, ok := project.NearestSubproject(members, rel)
	if !ok {
		return taskProjectChoice{}, false
	}
	return findTaskProjectChoice(choices, member)
}

// ambiguousTaskProjects returns the members a shorthand name matches when there is more than one,
// so the error can name them instead of just saying "unknown".
func ambiguousTaskProjects(choices []taskProjectChoice, name string) []string {
	var matches []string
	for _, ch := range choices {
		if ch.Name != "root" && path.Base(ch.Name) == name {
			matches = append(matches, ch.Name)
		}
	}
	if len(matches) < 2 {
		return nil
	}
	return matches
}

// taskProjectAddError refuses `coop tasks add` in an umbrella project when it cannot tell which
// queue the task belongs to: no --project at the repository root, or a --project that names no
// single queue. The queues are the answer, so the block lists them. Names and paths come from
// .agent/project.yaml, which a box can write, so they are escaped.
func taskProjectAddError(name string, choices []taskProjectChoice) error {
	const command = "coop tasks add"
	example := "root"
	names, rels := make([]string, len(choices)), make([]string, len(choices))
	nameWidth, relWidth := 0, 0
	for i, ch := range choices {
		names[i], rels[i] = ui.SafeInline(ch.Name), ui.SafeInline(filepath.ToSlash(ch.Rel))
		nameWidth, relWidth = max(nameWidth, len([]rune(names[i]))), max(relWidth, len([]rune(rels[i])))
		if example == "root" && ch.Name != "root" {
			example = names[i]
		}
	}
	refusal := &ui.UsageError{}
	switch dupes := ambiguousTaskProjects(choices, name); {
	case name == "":
		refusal.Headline = fmt.Sprintf("Choose a project for %q", command)
		refusal.Cause = "This repository has " + ui.Count(len(choices), "task queue") + ". Choose one with --project.\n" +
			"Inside a subproject's folder, add uses that subproject."
	case dupes != nil:
		for i := range dupes {
			dupes[i] = ui.SafeInline(dupes[i])
		}
		refusal.Headline = fmt.Sprintf("Project %q matches %d projects in %q", name, len(dupes), command)
		refusal.Cause = "It could be " + ui.List(dupes, "or") + ". Use the full path."
		example = dupes[0]
	default:
		refusal.Headline = fmt.Sprintf("Unknown project %q for %q", name, command)
		refusal.Cause = "Choose one of this repository's task queues."
	}
	for i, ch := range choices {
		row := padRight(names[i], nameWidth) + "  " + rels[i]
		if ch.Note != "" {
			row = padRight(names[i], nameWidth) + "  " + padRight(rels[i], relWidth) + "  " + ch.Note
		}
		refusal.Choices = append(refusal.Choices, row)
	}
	refusal.Rows = [][2]string{
		{"Usage:", command + ` --project <project> "<title>" [<options>]`},
		{"Example:", command + " --project " + example + ` "Fix login retries"`},
		{"Help:", ui.HelpCommand(command)},
	}
	return refusal
}

// TaskQueues resolves the task-queue path(s) for a command — the repeated --tasks flags if
// any, else COOP_TASKS (cfg.TasksFiles, default .agent/tasks) — into repo-relative paths. A
// relative path is taken relative to the repo root (where the box runs); an absolute one must
// live inside the repo. The repo-relative form is what the loop's prompt names and the host
// joins with the repo to read.
func TaskQueues(cfg *config.Config, repo string, flags []string) ([]string, error) {
	given := flags
	if len(given) == 0 {
		given = cfg.TasksFiles
	}
	if len(given) == 0 {
		// No --tasks and no COOP_TASKS: derive from .agent/project.yaml — a monorepo's subproject
		// queues (so you don't hand-maintain COOP_TASKS), or just .agent/tasks for a single repo.
		derived, err := project.TaskDirs(repo)
		if err != nil {
			return nil, err
		}
		given = derived
	}
	var rels []string
	for _, g := range given {
		rel := g
		if filepath.IsAbs(g) {
			// By folder: a COOP_TASKS written through the link a COOP_REPO names still lies in the
			// folder ResolveRepo followed that link to.
			r, err := box.RepoRelative(repo, g)
			if err != nil {
				return nil, err
			}
			rel = r
		}
		rel = filepath.Clean(rel)
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, queueOutsideRepo(g, len(flags) > 0)
		}
		rels = append(rels, rel)
	}
	return rels, nil
}

// queueOutsideRepo refuses a task queue path that leaves the repository. fromFlag says --tasks
// named it; otherwise COOP_TASKS did, and the person may not remember setting it.
func queueOutsideRepo(path string, fromFlag bool) error {
	headline := fmt.Sprintf("Invalid value %q in COOP_TASKS", path)
	if fromFlag {
		headline = fmt.Sprintf("Invalid value %q for %q", path, "--tasks")
	}
	return &ui.UsageError{
		Headline: headline,
		Cause:    "A task queue must be a folder inside this repository, such as .agent/tasks.",
	}
}

// CmdTasks drives the folder task queue (.agent/tasks): one folder per task, its state the
// parent directory. The subcommands (ls/lint/add/claim/release/block/unblock/done/rm/
// decisions) live in cmd.go; a bare `coop tasks` lists the queue.
func CmdTasks(host Host, cfg *config.Config, args []string) (int, error) {
	flags, rest, err := ExtractTasksFlags("coop tasks", args)
	if err != nil {
		return 2, err
	}
	// A leading flag is the default listing with that flag: `coop tasks --blocked` means
	// `coop tasks ls --blocked`, not a subcommand named "--blocked". --tasks is already pulled out,
	// so anything flag-shaped still at the front is an ls filter (validated as one downstream). See
	// rule bare-flag-routes-to-default-view.
	if len(rest) > 0 && strings.HasPrefix(rest[0], "-") && rest[0] != "-" {
		rest = append([]string{"ls"}, rest...)
	}
	if len(rest) > 0 && rest[0] == "rm" {
		if _, err := parseTaskRemoveArgs(rest[1:]); err != nil {
			return 2, err
		}
	}
	repo, err := box.ResolveRepo(cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	project, err := projectFolder(cfg, repo)
	if err != nil {
		return -1, err
	}
	projectName := ""
	if len(rest) > 0 && rest[0] == "add" {
		addArgs := rest[1:]
		projectName, addArgs, err = extractProjectFlag(addArgs)
		if err != nil {
			return 2, err
		}
		rest = append([]string{"add"}, addArgs...)
	}
	explicitQueues := len(flags) > 0 || len(cfg.TasksFiles) > 0
	if projectName != "" && len(flags) > 0 {
		return 2, ui.ConflictingOptions("--project", "--tasks", "coop tasks add")
	}
	if projectName != "" && explicitQueues {
		return 2, &ui.UsageError{
			Headline: `Cannot use "--project" while COOP_TASKS is set`,
			Cause:    "COOP_TASKS already chooses the task queue. Unset it, or leave out --project.",
			Rows:     [][2]string{{"Help:", ui.HelpCommand("coop tasks add")}},
		}
	}
	if len(rest) > 0 && rest[0] == "add" && !explicitQueues {
		choices, umbrella, err := taskProjectChoices(repo)
		if err != nil {
			return 2, err
		}
		if umbrella {
			if projectName == "" {
				// Inside a subproject's folder, the work belongs to it, as `coop context` scopes it.
				if ch, ok := workingSubproject(repo, choices); ok {
					return tasksAddInQueue(repo, project, ch.Rel, rest[1:])
				}
				return 2, taskProjectAddError("", choices)
			}
			ch, ok := findTaskProjectChoice(choices, projectName)
			if !ok {
				return 2, taskProjectAddError(projectName, choices)
			}
			return tasksAddInQueue(repo, project, ch.Rel, rest[1:])
		}
	}
	if projectName != "" {
		return 2, &ui.UsageError{
			Headline: `No subprojects for "--project" in "coop tasks add"`,
			Cause:    ".agent/project.yaml lists no subprojects, so this repository has one task queue.\nLeave out --project.",
			Rows:     [][2]string{{"Help:", ui.HelpCommand("coop tasks add")}},
		}
	}
	rels, err := TaskQueues(cfg, repo, flags)
	if err != nil {
		return 2, err
	}
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	// Aggregate commands may never reach CmdTasksFolder, and id routing performs a lookup first.
	// Reject malformed structured arguments before either path (and before taking a done lock).
	if spec, ok := taskArgSpecs[sub]; ok {
		if err := validateArgs("tasks "+sub, rest[1:], spec); err != nil {
			return 2, err
		}
	}
	if sub == "watch" {
		// The live board watches the queue(s) themselves draining, task-centric across however many
		// are configured and including active forks.
		if len(rels) == 0 {
			return 2, noQueueConfigured("coop tasks watch")
		}
		jsonOutput := false
		for _, arg := range rest[1:] {
			switch {
			case arg == "--json" && jsonOutput:
				return 2, ui.RepeatedOption(arg, "coop tasks watch")
			case arg == "--json":
				jsonOutput = true
			case strings.HasPrefix(arg, "-"):
				return 2, unknownOptionErr(arg, "coop tasks watch", []string{"--json"})
			default:
				return 2, ui.UnexpectedArgument(arg, "coop tasks watch", "coop tasks watch [--json]")
			}
		}
		return TasksWatch(host, repo, rels, jsonOutput)
	}
	if sub == "queues" {
		if len(rest) > 1 {
			if strings.HasPrefix(rest[1], "-") {
				return 2, ui.UnknownOption(rest[1], "coop tasks queues", "")
			}
			return 2, ui.UnexpectedArgument(rest[1], "coop tasks queues", "coop tasks queues")
		}
		// Print each configured queue's absolute path, one per line — a stable primitive for scripts
		// and the sweep queue guard, which counts actionable tasks across every queue.
		for _, rel := range rels {
			fmt.Println(filepath.Join(repo, rel))
		}
		return 0, nil
	}
	if sub == "done" {
		// completeTrustedTask's audit-reopen branch validates against HEAD and consumes authority
		// operations later — this host-driven completion path takes the same ref-authority lock as
		// the work loop's own validate→consume window, so `coop tasks done` can never land mid-loop
		// (and a running loop can never make this refuse by moving HEAD underneath it).
		release, lockErr := LockRefAuthority(cfg, repo)
		if lockErr != nil {
			return -1, fmt.Errorf("acquire ref authority for %s: %w", repo, lockErr)
		}
		defer release()
	}
	if len(rels) > 1 {
		// A monorepo can configure several queues (COOP_TASKS, or repeated --tasks) — the same set
		// `coop loop` drains. The roll-ups span them all (each under a header) and the
		// id-addressed commands find their task in whichever queue holds it; only the commands that
		// CREATE into a queue (add) needs one unambiguous target.
		switch sub {
		case "ls":
			return tasksListAll(repo, rels, rest[1:])
		case "lint":
			return tasksLintAll(repo, rels)
		case "decisions":
			return tasksDecisionsAll(repo, rels, rest[1:])
		case "claim", "release", "lease", "block", "unblock", "park", "done", "path", "rm":
			return tasksAcrossQueues(repo, rels, sub, rest)
		case "":
			return tasksListAll(repo, rels, nil)
		default:
			return 2, &ui.UsageError{
				Headline: fmt.Sprintf("%q works one queue at a time", "coop tasks "+sub),
				Cause:    fmt.Sprintf("This project has %s.", ui.Count(len(rels), "task queue")),
				Rows: [][2]string{
					{"Usage:", "coop tasks --tasks <path> " + sub},
					{"Help:", ui.HelpCommand("coop tasks")},
				},
			}
		}
	}
	if len(rels) == 0 {
		return 2, noQueueConfigured("coop tasks")
	}
	return tasksInQueue(repo, project, rels[0], rest, flags, explicitQueues)
}

// tasksInQueue runs a command on the one queue at rel. explicit says the queue was named (--tasks or
// COOP_TASKS) rather than found in the project.
func tasksInQueue(repo, project, rel string, rest, flags []string, explicit bool) (int, error) {
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	root := filepath.Join(repo, rel)
	exists, err := taskQueueExists(root)
	if err != nil {
		return -1, err
	}
	if !exists {
		// `coop tasks --tasks done` greedily eats `done` as the queue path, leaving no subcommand.
		// If the swallowed value names a subcommand, the user almost certainly meant `coop tasks <sub>`.
		if sub == "" && len(flags) == 1 && isTasksSubcommand(flags[0]) {
			return 2, ui.UnknownValue("queue folder", flags[0], "coop tasks --tasks", "", "coop tasks "+flags[0])
		}
		if project == "" {
			return 1, noProject(repo)
		}
		switch {
		case sub == "add":
			// add makes the queue and sets it up as coop init would (see createTaskFolder): that is
			// how a project starts tasks before coop init, and how a secondary --tasks queue starts.
		case (sub == "" || sub == "ls") && !explicit:
			// The project's own queue before its first task: the empty listing, bare or not.
		case sub != "" && !isTasksSubcommand(sub):
			// A mistyped command is refused as one, whether or not a queue exists yet.
		case sub == "decisions" && !explicit:
			if len(rest) == 1 {
				fmt.Println("No questions waiting for your answer.")
				return 0, nil
			}
		default:
			start := `coop tasks add "<title>"`
			if len(flags) > 0 {
				start = "coop tasks --tasks " + rel + ` add "<title>"`
			}
			return 1, ui.CommandFailed("No task queue at "+rel, "", [2]string{"Add the first task:", start})
		}
	}
	return CmdTasksFolder(repo, root, rest)
}

func tasksAddInQueue(repo, project, rel string, args []string) (int, error) {
	root := filepath.Join(repo, rel)
	if err := refuseQueueOutsideProject(repo, project, root); err != nil {
		return 1, err
	}
	return tasksFolderAddWithProject(root, args, StateTodo, "tasks add", project)
}

// projectFolder is the project a task command may start a queue in: COOP_REPO when it is set, else
// the Git repository holding the working folder, or "" when there is neither. ResolveRepo falls back
// to the working folder, which still reads a queue someone made there, but a new one there would
// hold tasks no project owns: `coop tasks add` in ~/Downloads must not start a queue.
func projectFolder(cfg *config.Config, repo string) (string, error) {
	if cfg.RepoOverride != "" {
		return repo, nil
	}
	if _, found, err := box.FindRepo(); err != nil || !found {
		return "", err
	}
	return repo, nil
}

// refuseQueueOutsideProject refuses the queue at root when it does not exist yet and no project owns
// the folder (see projectFolder), so no command starts a queue there.
func refuseQueueOutsideProject(repo, project, root string) error {
	if project != "" {
		return nil
	}
	if exists, err := taskQueueExists(root); err != nil || exists {
		return err
	}
	return noProject(repo)
}

// noProject refuses a task command in a folder that is no project and has no queue yet.
func noProject(dir string) error {
	return ui.CommandFailed("No project here",
		dir+" is not inside a Git repository.\nRun Coop in your project folder, or set one up here.",
		[2]string{"Set up a project here:", "coop init"})
}

// tasksListAll rolls up `coop tasks ls` across several configured queues (a monorepo with a
// per-project .agent/tasks each), printing every queue under its rel-path header. A queue that
// doesn't exist yet is noted, not fatal — the mutating commands still require a single target.
func tasksListAll(repo string, rels []string, args []string) (int, error) {
	// Validate ls flags here too — the umbrella roll-up doesn't route through CmdTasksFolder, so
	// without this an unknown flag (or a typo like --blockd) would be silently ignored.
	if err := validateArgs("tasks ls", args, taskArgSpecs["ls"]); err != nil {
		return 2, err
	}
	all := slices.Contains(args, "--all")
	only := taskStateFilter(args)
	p := ui.For(os.Stdout)
	for i, rel := range rels {
		if i > 0 {
			fmt.Println() // one blank line between queues, as between the sections inside one
		}
		fmt.Println(banner(p, rel))
		root := filepath.Join(repo, rel)
		// Empty/absent queues print a plain gray line (not ui.Info) so the whole roll-up stays one
		// clean stdout block — no "coop:" prefix mid-list, and `coop tasks ls > file` keeps it all.
		exists, err := taskQueueExists(root)
		if err != nil {
			return -1, err
		}
		if !exists {
			fmt.Println(p.Gray("  (no task queue here yet)"))
			continue
		}
		items, err := ReadTaskTree(root)
		if err != nil {
			return -1, err
		}
		if len(items) == 0 {
			fmt.Println(p.Gray("  (no tasks)"))
			warnQueueRootStrays(root)
			continue
		}
		// The banner already names the queue, so its listing carries no "Tasks" heading of its own.
		if _, err := printTaskTree(root, "", all, only...); err != nil {
			return -1, err
		}
		warnQueueRootStrays(root)
	}
	return 0, nil
}

// tasksAcrossQueues routes an id-addressed subcommand (claim/release/block/unblock/done/path/rm) when several
// queues are configured: find which queue holds the id, then run the normal single-queue handler
// against that queue — so `coop tasks done <id>` just works in a monorepo. `rm --all-done` is the
// id-less exception: it clears every queue's done archive.
func tasksAcrossQueues(repo string, rels []string, sub string, rest []string) (int, error) {
	args := rest[1:]
	var removal taskRemoveArgs
	if sub == "rm" {
		var err error
		removal, err = parseTaskRemoveArgs(args)
		if err != nil {
			return 2, err
		}
	}
	if sub == "rm" && removal.allDone {
		total := 0
		counts := make([]int, len(rels))
		for i, rel := range rels {
			n, err := countDone(filepath.Join(repo, rel))
			if err != nil {
				return -1, err
			}
			counts[i], total = n, total+n
		}
		if total == 0 {
			ui.Note("No completed tasks to delete.")
			return 0, nil
		}
		// Each queue's exact archive path and count, then ONE confirmation for the lot.
		ui.Note("Delete %s\n", ui.Count(total, "completed task"))
		for i, rel := range rels {
			ui.Note("  %s — %s", filepath.Join(rel, StateDone), ui.Count(counts[i], "completed task"))
		}
		ui.Note("  Their instructions, progress and saved evidence will be permanently deleted.\n")
		if err := ui.DestroyGate("Delete these tasks", removal.yes); err != nil {
			return 2, cancelledDeletion(err, "coop tasks rm", "No tasks deleted.")
		}
		removed := 0
		for _, rel := range rels {
			n, err := removeAllDone(filepath.Join(repo, rel))
			removed += n
			if err != nil {
				return -1, fmt.Errorf(
					"deleted %s across the configured queues, then stopped: %w — re-run 'coop tasks rm --all-done --yes'",
					ui.Count(removed, "completed task"), err,
				)
			}
		}
		ui.OK("Deleted %s across %s", ui.Count(removed, "completed task"), ui.Count(len(rels), "queue"))
		return 0, nil
	}
	// The id is the first positional token; with none, delegate as-is so the subcommand's own
	// usage error (e.g. "usage: coop tasks claim <id>") is what the user sees.
	id := removal.id
	switch sub {
	case "rm":
	case "claim":
		// claim's flags take values (--as codex), so the first non-flag token is not the id.
		parsed, _, err := parseClaimArgs(args)
		if err != nil {
			return 2, err
		}
		id = parsed
	case "lease":
		parsed, err := parseLeaseArgs(args)
		if err != nil {
			return 2, err
		}
		id = parsed.id
	case "block":
		// block's decision flags take values too, and the whole request is validated HERE — before
		// the queue is even chosen — so a malformed one never moves a task in any queue.
		parsed, err := parseBlockArgs(args)
		if err != nil {
			return 2, err
		}
		id = parsed.id
	default:
		for _, x := range args {
			if !strings.HasPrefix(x, "-") {
				id = x
				break
			}
		}
	}
	if id == "" {
		return CmdTasksFolder(repo, filepath.Join(repo, rels[0]), rest)
	}
	rel, err := queueOfTask(repo, rels, id)
	if err != nil {
		return lookupExit(err), err
	}
	return CmdTasksFolder(repo, filepath.Join(repo, rel), rest)
}

// queueOfTask finds which configured queue holds the task id — the multi-queue analog of
// FindTask, with the same precedence (an exact id match beats substring matches). It errors when
// the id is absent everywhere, or matches in more than one queue. Explicit or monorepo queues may
// still contain duplicate ids, so acting on an arbitrary one would silently touch the wrong tree.
func queueOfTask(repo string, rels []string, id string) (string, error) {
	return queueOfTaskWith(repo, rels, id, "coop tasks", ReadTaskTree)
}

// queueOfTaskWith is queueOfTask parametrized by the per-queue reader, so the lifecycle tree
// (ReadTaskTree) and the backlog drawer (ReadBacklog) share one resolver. read maps a queue root to
// its items; listCommand names the matching listing in missing-item guidance.
func queueOfTaskWith(repo string, rels []string, id, listCommand string, read func(string) ([]Item, error)) (string, error) {
	hit, err := findTaskAcrossQueuesWith(repo, rels, id, listCommand, read)
	return hit.rel, err
}

// FindTaskAcrossQueues uses the same identity resolution as the lifecycle commands,
// retaining the selected Item so readers need not flatten queues or resolve it again.
func FindTaskAcrossQueues(repo string, rels []string, id string) (Item, error) {
	if len(rels) == 1 {
		return FindTask(filepath.Join(repo, rels[0]), id)
	}
	if id == "" {
		return Item{}, taskLookupRefusal("Missing task ID", "coop tasks")
	}
	hit, err := findTaskAcrossQueuesWith(repo, rels, id, "coop tasks", ReadTaskTree)
	return hit.task, err
}

type queueTask struct {
	rel  string
	task Item
}

func findTaskAcrossQueuesWith(repo string, rels []string, id, listCommand string, read func(string) ([]Item, error)) (queueTask, error) {
	var exact, subs []queueTask
	for _, rel := range rels {
		items, err := read(filepath.Join(repo, rel))
		if err != nil {
			return queueTask{}, err
		}
		for _, t := range items {
			switch {
			case t.ID == id:
				exact = append(exact, queueTask{rel, t})
			case strings.Contains(t.ID, id):
				subs = append(subs, queueTask{rel, t})
			}
		}
	}
	pick := exact
	if len(pick) == 0 {
		pick = subs
	}
	switch len(pick) {
	case 1:
		return pick[0], nil
	case 0:
		return queueTask{}, taskLookupRefusal(fmt.Sprintf("No task matching %q in %s", id, ui.Count(len(rels), "task queue")), listCommand)
	}
	// Each match names its queue, since the same ID can sit in two of them. Paths and folder names
	// are escaped: a folder name can come from a box.
	width := 0
	for _, h := range pick {
		width = max(width, len([]rune(ui.SafeInline(h.rel))))
	}
	refusal := &ui.UsageError{
		Headline: fmt.Sprintf("%d tasks match %q across the task queues", len(pick), id),
		Rows:     [][2]string{{"", "Pick the queue with --tasks <path>."}},
	}
	for _, h := range pick {
		refusal.Choices = append(refusal.Choices, padRight(ui.SafeInline(h.rel), width)+"  "+ui.SafeInline(h.task.ID))
	}
	return queueTask{}, refusal
}

// tasksLintAll rolls `coop tasks lint` up across the configured queues, each under its banner.
// The exit code is the worst of the per-queue runs, so CI still fails on any issue anywhere.
func tasksLintAll(repo string, rels []string) (int, error) {
	p := ui.For(os.Stdout)
	worst := 0
	first := true
	for _, rel := range rels {
		root := filepath.Join(repo, rel)
		exists, err := taskQueueExists(root)
		if err != nil {
			return -1, err
		}
		if !exists {
			continue
		}
		if !first {
			fmt.Println() // one blank line between queues, as between the sections inside one
		}
		first = false
		fmt.Println(banner(p, rel))
		code, err := tasksFolderLint(root)
		if err != nil {
			return code, err
		}
		if code > worst {
			worst = code
		}
	}
	if first {
		ui.Note("no task queues found across %s", ui.Count(len(rels), "configured path"))
	}
	return worst, nil
}

// tasksDecisionsAll rolls up `coop tasks decisions` across several configured queues, each under
// its header (only queues that exist are shown); -i walks every queue's open decisions in one
// interactive session, each header naming the queue the decision lives in.
func tasksDecisionsAll(repo string, rels []string, args []string) (int, error) {
	interactive := false
	for _, a := range args {
		switch a {
		case "-i", "--interactive":
			interactive = true
		default:
			return 2, decisionsArgumentErr(a)
		}
	}
	if interactive {
		var refs []decisionRef
		var answered []Item
		for _, rel := range rels {
			root := filepath.Join(repo, rel)
			exists, err := taskQueueExists(root)
			if err != nil {
				return -1, err
			}
			if !exists {
				continue
			}
			items, err := ReadTaskTree(root)
			if err != nil {
				return -1, err
			}
			for _, t := range items {
				switch {
				case answeredDecision(t):
					answered = append(answered, t)
				case t.State == StateBlocked:
					refs = append(refs, decisionRef{root: root, label: rel, id: t.ID})
				}
			}
		}
		// A task the human already answered is not a question to browse, but it is still theirs to
		// finish — so it is named here too, with the one command that finishes it.
		p := ui.For(os.Stdout)
		if len(refs) == 0 {
			ui.OK("No questions are waiting for your answer")
			printAnsweredDecisions(p, answered, false)
			return 0, nil
		}
		if !ui.IsTerminal(os.Stdin) {
			return 2, decisionsNeedTerminalErr()
		}
		code, err := runDecisionBrowser(repo, refs, os.Stdin, os.Stdout)
		printAnsweredDecisions(p, answered, true)
		return code, err
	}
	return tasksDecisionsRollup(repo, rels)
}

// tasksDecisionsRollup is the non-interactive multi-queue decisions listing.
func tasksDecisionsRollup(repo string, rels []string) (int, error) {
	p := ui.For(os.Stdout)
	first := true
	for _, rel := range rels {
		root := filepath.Join(repo, rel)
		exists, err := taskQueueExists(root)
		if err != nil {
			return -1, err
		}
		if !exists {
			continue
		}
		// Only surface a queue that actually has an open decision, so the roll-up never prints a
		// bare "# path" header over nothing (the per-queue "none" note goes to stderr).
		hasBlocked := false
		items, err := ReadTaskTree(root)
		if err != nil {
			return -1, err
		}
		for _, t := range items {
			if t.State == StateBlocked {
				hasBlocked = true
				break
			}
		}
		if !hasBlocked {
			continue
		}
		if !first {
			fmt.Println() // one blank line between queues, as between the sections inside one
		}
		first = false
		fmt.Println(banner(p, rel))
		if _, err := tasksFolderDecisions(repo, root, nil); err != nil {
			return -1, err
		}
	}
	if first {
		ui.Note("no open decisions across %s", ui.Count(len(rels), "configured queue"))
	}
	return 0, nil
}

// noQueueConfigured is the refusal when nothing told Coop WHICH queue to work: no COOP_TASKS, no
// --tasks, and no queue in this project. command is the one the person typed, so its page is the
// one they are sent to.
func noQueueConfigured(command string) error {
	return &ui.UsageError{
		Headline: fmt.Sprintf("No task queue configured for %q", command),
		Cause:    "Coop needs to know which queue to work.",
		Rows: [][2]string{
			{"Usage:", command + " --tasks <path>"},
			{"Help:", ui.HelpCommand(command)},
		},
	}
}
