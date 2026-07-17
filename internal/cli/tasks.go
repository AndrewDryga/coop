package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/ui"
)

// extractTasksFlags pulls every `--tasks <path>` (or `--tasks=<path>`) out of args,
// returning the collected paths and the remaining args. Shared by `coop tasks` and
// `coop loop` so they accept the same repeatable flag. A trailing `--tasks` with no path
// is an error, not a silently-dropped flag.
func extractTasksFlags(args []string) (tasks []string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		if v, n, ok, e := flagValue(args, i, "--tasks"); ok {
			if e != nil {
				return nil, nil, e
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
// It is intentionally separate from extractTasksFlags: --tasks targets a raw queue path for every
// tasks command, while --project is the friendly umbrella-project selector for creation only.
func extractProjectFlag(args []string) (projectName string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		if v, n, ok, e := flagValue(args, i, "--project"); ok {
			if e != nil {
				return "", nil, e
			}
			if projectName != "" {
				return "", nil, errors.New("coop tasks add: pass --project only once")
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

func findTaskProjectChoice(choices []taskProjectChoice, name string) (taskProjectChoice, bool) {
	for _, ch := range choices {
		if ch.Name == name {
			return ch, true
		}
	}
	return taskProjectChoice{}, false
}

func taskProjectAddError(name string, choices []taskProjectChoice) error {
	nameWidth, relWidth := 0, 0
	for _, ch := range choices {
		if len(ch.Name) > nameWidth {
			nameWidth = len(ch.Name)
		}
		if len(filepath.ToSlash(ch.Rel)) > relWidth {
			relWidth = len(filepath.ToSlash(ch.Rel))
		}
	}
	firstNonRoot := ""
	for _, ch := range choices {
		if ch.Name != "root" {
			firstNonRoot = ch.Name
			break
		}
	}
	var b strings.Builder
	if name == "" {
		b.WriteString("coop tasks add: this umbrella project has multiple task queues; choose one with --project <name>")
	} else {
		fmt.Fprintf(&b, "coop tasks add: unknown project %q; choose one with --project <name>", name)
	}
	b.WriteString("\n\nusage:\n")
	b.WriteString("  coop tasks add --project <project> \"<title>\" [--context <text> --acceptance <text> --approach <text> --subtask <text>...]\n\n")
	b.WriteString("projects:\n")
	for _, ch := range choices {
		rel := filepath.ToSlash(ch.Rel)
		if ch.Note != "" {
			fmt.Fprintf(&b, "  %-*s  %-*s  %s", nameWidth, ch.Name, relWidth, rel, ch.Note)
		} else {
			fmt.Fprintf(&b, "  %-*s  %s", nameWidth, ch.Name, rel)
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nexamples:\n")
	b.WriteString("  coop tasks add --project root \"Update shared release checklist\"\n")
	if firstNonRoot != "" {
		fmt.Fprintf(&b, "\n  coop tasks add --project %s \"Fix auth retry\" \\\n", firstNonRoot)
		fmt.Fprintf(&b, "    --context %q \\\n", firstNonRoot+" auth retries can loop forever")
		b.WriteString("    --acceptance \"make check green + retry cap test\" \\\n")
		b.WriteString("    --approach \"cap retries and surface the final error\" \\\n")
		b.WriteString("    --subtask \"add retry cap\" \\\n")
		b.WriteString("    --subtask \"test the failure path\"")
	}
	return errors.New(b.String())
}

// taskQueues resolves the task-queue path(s) for a command — the repeated --tasks flags if
// any, else COOP_TASKS (cfg.TasksFiles, default .agent/tasks) — into repo-relative paths. A
// relative path is taken relative to the repo root (where the box runs); an absolute one must
// live inside the repo. The repo-relative form is what the loop's prompt names and the host
// joins with the repo to read.
func taskQueues(cfg *config.Config, repo string, flags []string) ([]string, error) {
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
			r, err := filepath.Rel(repo, g)
			if err != nil {
				return nil, err
			}
			rel = r
		}
		rel = filepath.Clean(rel)
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("tasks path %q is outside the repo", g)
		}
		rels = append(rels, rel)
	}
	return rels, nil
}

// cmdTasks drives the folder task queue (.agent/tasks): one folder per task, its state the
// parent directory. The subcommands (list/lint/add/claim/block/unblock/done/remove/split/
// decisions) live in taskcmd.go; a bare `coop tasks` shows help.
func (a *app) cmdTasks(args []string) (int, error) {
	flags, rest, err := extractTasksFlags(args)
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
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
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
	explicitQueues := len(flags) > 0 || len(a.cfg.TasksFiles) > 0
	if projectName != "" && explicitQueues {
		return 2, errors.New("coop tasks add: --project cannot be combined with --tasks or COOP_TASKS; choose one queue selector")
	}
	if len(rest) > 0 && rest[0] == "add" && !explicitQueues {
		choices, umbrella, err := taskProjectChoices(repo)
		if err != nil {
			return 2, err
		}
		if umbrella {
			if projectName == "" {
				return 2, taskProjectAddError("", choices)
			}
			ch, ok := findTaskProjectChoice(choices, projectName)
			if !ok {
				return 2, taskProjectAddError(projectName, choices)
			}
			return tasksAddInQueue(repo, ch.Rel, rest[1:], ch.Name)
		}
	}
	if projectName != "" {
		return 2, errors.New("coop tasks add: --project is only for umbrella projects with subprojects in .agent/project.yaml")
	}
	rels, err := taskQueues(a.cfg, repo, flags)
	if err != nil {
		return 2, err
	}
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	if sub == "watch" {
		// The live board watches the queue(s) themselves draining — task-centric, across however
		// many are configured — unlike the per-fork `coop fleet watch`.
		if len(rels) == 0 {
			return 2, errors.New("coop tasks watch: no task queue configured — set COOP_TASKS or pass --tasks <path>")
		}
		return a.tasksWatch(repo, rels)
	}
	if sub == "queues" {
		// Print each configured queue's absolute path, one per line — a stable primitive for scripts
		// and the sweep queue guard, which counts actionable tasks across every queue.
		for _, rel := range rels {
			fmt.Println(filepath.Join(repo, rel))
		}
		return 0, nil
	}
	if len(rels) > 1 {
		// A monorepo can configure several queues (COOP_TASKS, or repeated --tasks) — the same set
		// `coop loop`/`coop fleet` drain. The roll-ups span them all (each under a header) and the
		// id-addressed commands find their task in whichever queue holds it; only the commands that
		// CREATE into a queue (add, split) need one unambiguous target.
		switch sub {
		case "ls":
			return tasksListAll(repo, rels, rest[1:])
		case "lint":
			return tasksLintAll(repo, rels)
		case "decisions":
			return tasksDecisionsAll(repo, rels, rest[1:])
		case "claim", "block", "unblock", "done", "path", "rm", "clear":
			return tasksAcrossQueues(repo, rels, sub, rest)
		case "":
			return tasksListAll(repo, rels, nil)
		default:
			return 2, fmt.Errorf("coop tasks %s works one queue at a time — pass a single --tasks <path> (ls, lint, decisions, and the id commands span all %d configured queues)", sub, len(rels))
		}
	}
	if len(rels) == 0 {
		return 2, errors.New("coop tasks: no task queue configured — set COOP_TASKS or pass --tasks <path>")
	}
	return tasksInQueue(repo, rels[0], rest, flags)
}

func tasksInQueue(repo, rel string, rest, flags []string) (int, error) {
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	root := filepath.Join(repo, rel)
	// When the queue doesn't exist yet, a bare `coop tasks` still shows help, and `add`
	// bootstraps it on demand (tasksFolderAdd creates the folder) — that's how you start a
	// secondary --tasks queue in a monorepo, since `coop init` only scaffolds the repo root.
	// Every other subcommand needs an existing queue to act on.
	if !isTaskDir(root) {
		switch sub {
		case "":
			// `coop tasks --tasks done` greedily eats `done` as the queue path, leaving no
			// subcommand — then help + exit 0 reads as a silent no-op. If the swallowed value
			// names a subcommand, the user almost certainly meant `coop tasks <sub>`.
			if len(flags) == 1 && isTasksSubcommand(flags[0]) {
				return 2, fmt.Errorf("`--tasks` takes a queue dir, not a subcommand — did you mean `coop tasks %s`? (no task queue %q here)", flags[0], rel)
			}
			return groupHelp("tasks")
		case "add":
			// fall through — tasksFolderAdd creates the queue dir
		default:
			return -1, fmt.Errorf("no task queue at %s — run 'coop init' (or 'coop tasks --tasks %s add \"…\"' to start one here)", rel, rel)
		}
	}
	return cmdTasksFolder(repo, root, rest)
}

func tasksAddInQueue(repo, rel string, args []string, projectName string) (int, error) {
	root := filepath.Join(repo, rel)
	return tasksFolderAddWithProject(root, args, stateTodo, "tasks add", projectName)
}

// tasksListAll rolls up `coop tasks ls` across several configured queues (a monorepo with a
// per-project .agent/tasks each), printing every queue under its rel-path header. A queue that
// doesn't exist yet is noted, not fatal — the mutating commands still require a single target.
func tasksListAll(repo string, rels []string, args []string) (int, error) {
	// Validate ls flags here too — the umbrella roll-up doesn't route through cmdTasksFolder, so
	// without this an unknown flag (or a typo like --blockd) would be silently ignored.
	if err := validateArgs("tasks ls", args, lsFlags, 0); err != nil {
		return 2, err
	}
	all := slices.Contains(args, "--all")
	only := taskStateFilter(args)
	p := ui.For(os.Stdout)
	for i, rel := range rels {
		if i > 0 {
			fmt.Print("\n\n\n") // three blank lines between different files' output
		}
		fmt.Println(banner(p, rel))
		root := filepath.Join(repo, rel)
		// Empty/absent queues print a plain gray line (not ui.Info) so the whole roll-up stays one
		// clean stdout block — no "coop:" prefix mid-list, and `coop tasks ls > file` keeps it all.
		switch {
		case !isTaskDir(root):
			fmt.Println(p.Gray("  (no task queue here yet)"))
		case len(readTaskTree(root)) == 0:
			fmt.Println(p.Gray("  (no tasks)"))
		default:
			if _, err := tasksFolderList(root, all, only...); err != nil {
				return -1, err
			}
		}
	}
	return 0, nil
}

// tasksAcrossQueues routes an id-addressed subcommand (claim/block/unblock/done/rm) when several
// queues are configured: find which queue holds the id, then run the normal single-queue handler
// against that queue — so `coop tasks done <id>` just works in a monorepo. `rm --all-done` is the
// id-less exception: it clears every queue's done archive.
func tasksAcrossQueues(repo string, rels []string, sub string, rest []string) (int, error) {
	args := rest[1:]
	if sub == "clear" || (sub == "rm" && slices.Contains(args, "--all-done")) {
		total := 0
		for _, rel := range rels {
			total += countDone(filepath.Join(repo, rel))
		}
		if total == 0 {
			ui.Note("no done tasks to remove in any of the %s", ui.Count(len(rels), "configured queue"))
			return 0, nil
		}
		if err := destroyGate(fmt.Sprintf("remove %s across %s", ui.Count(total, "done task"), ui.Count(len(rels), "queue")), hasYes(args)); err != nil {
			return 2, err
		}
		removed := 0
		for _, rel := range rels {
			n, err := removeAllDone(filepath.Join(repo, rel))
			if err != nil {
				return -1, err
			}
			removed += n
		}
		ui.OK("removed %s across %s", ui.Count(removed, "done task"), ui.Count(len(rels), "queue"))
		return 0, nil
	}
	// The id is the first positional token; with none, delegate as-is so the subcommand's own
	// usage error (e.g. "usage: coop tasks claim <id>") is what the user sees.
	id := ""
	for _, x := range args {
		if !strings.HasPrefix(x, "-") {
			id = x
			break
		}
	}
	if id == "" {
		return cmdTasksFolder(repo, filepath.Join(repo, rels[0]), rest)
	}
	rel, err := queueOfTask(repo, rels, id)
	if err != nil {
		return 1, err
	}
	return cmdTasksFolder(repo, filepath.Join(repo, rel), rest)
}

// queueOfTask finds which configured queue holds the task id — the multi-queue analog of
// findTask, with the same precedence (an exact id match beats substring matches). It errors when
// the id is absent everywhere, or matches in more than one queue: `coop tasks split` copies tasks
// into slices WITH their ids, so duplicates across queues are real — acting on an arbitrary one
// would silently touch the wrong tree.
func queueOfTask(repo string, rels []string, id string) (string, error) {
	return queueOfTaskWith(repo, rels, id, readTaskTree)
}

// queueOfTaskWith is queueOfTask parametrized by the per-queue reader, so the lifecycle tree
// (readTaskTree) and the backlog drawer (readBacklog) share one resolver. read maps a queue root to
// its items; everything else — exact-beats-substring precedence, the absent/ambiguous errors — is common.
func queueOfTaskWith(repo string, rels []string, id string, read func(string) []taskItem) (string, error) {
	type hit struct{ rel, id string }
	var exact, subs []hit
	for _, rel := range rels {
		for _, t := range read(filepath.Join(repo, rel)) {
			switch {
			case t.ID == id:
				exact = append(exact, hit{rel, t.ID})
			case strings.Contains(t.ID, id):
				subs = append(subs, hit{rel, t.ID})
			}
		}
	}
	pick := exact
	if len(pick) == 0 {
		pick = subs
	}
	switch len(pick) {
	case 1:
		return pick[0].rel, nil
	case 0:
		return "", fmt.Errorf("no task matching %q in any of the %d configured queues (run 'coop tasks' to list)", id, len(rels))
	}
	where := make([]string, len(pick))
	for i, h := range pick {
		where[i] = h.rel + ": " + h.id
	}
	return "", fmt.Errorf("%q matches %d tasks across the queues (%s) — pass a single --tasks <path> to pick one", id, len(pick), strings.Join(where, ", "))
}

// tasksLintAll rolls `coop tasks lint` up across the configured queues, each under its banner.
// The exit code is the worst of the per-queue runs, so CI still fails on any issue anywhere.
func tasksLintAll(repo string, rels []string) (int, error) {
	p := ui.For(os.Stdout)
	worst := 0
	first := true
	for _, rel := range rels {
		root := filepath.Join(repo, rel)
		if !isTaskDir(root) {
			continue
		}
		if !first {
			fmt.Print("\n\n\n") // three blank lines between different queues' output
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
			return 2, fmt.Errorf("coop tasks decisions: unknown flag %q (only -i / --interactive)", a)
		}
	}
	if interactive {
		var refs []decisionRef
		for _, rel := range rels {
			root := filepath.Join(repo, rel)
			if !isTaskDir(root) {
				continue
			}
			for _, t := range readTaskTree(root) {
				if t.State == stateBlocked {
					refs = append(refs, decisionRef{root: root, label: rel, id: t.ID})
				}
			}
		}
		if len(refs) == 0 {
			ui.OK("no open decisions — nothing is blocked")
			return 0, nil
		}
		if !ui.IsTerminal(os.Stdin) {
			return 2, errors.New("coop tasks decisions -i needs an interactive terminal")
		}
		return runDecisionBrowser(refs, os.Stdin, os.Stdout)
	}
	return tasksDecisionsRollup(repo, rels)
}

// tasksDecisionsRollup is the non-interactive multi-queue decisions listing.
func tasksDecisionsRollup(repo string, rels []string) (int, error) {
	p := ui.For(os.Stdout)
	first := true
	for _, rel := range rels {
		root := filepath.Join(repo, rel)
		if !isTaskDir(root) {
			continue
		}
		// Only surface a queue that actually has an open decision, so the roll-up never prints a
		// bare "# path" header over nothing (the per-queue "none" note goes to stderr).
		hasBlocked := false
		for _, t := range readTaskTree(root) {
			if t.State == stateBlocked {
				hasBlocked = true
				break
			}
		}
		if !hasBlocked {
			continue
		}
		if !first {
			fmt.Print("\n\n\n") // three blank lines between different files' output
		}
		first = false
		fmt.Println(banner(p, rel))
		if _, err := tasksFolderDecisions(root, nil); err != nil {
			return -1, err
		}
	}
	if first {
		ui.Note("no open decisions across %s", ui.Count(len(rels), "configured queue"))
	}
	return 0, nil
}
