package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
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
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	rels, err := taskQueues(a.cfg, repo, flags)
	if err != nil {
		return 2, err
	}
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	if len(rels) > 1 {
		// A monorepo can configure several queues (COOP_TASKS, or repeated --tasks) — the same set
		// `coop loop`/`coop fleet` drain. The read-only roll-ups span them all (each under a header);
		// the mutating commands need one unambiguous target.
		switch sub {
		case "list", "ls":
			return tasksListAll(repo, rels)
		case "decisions":
			return tasksDecisionsAll(repo, rels)
		case "":
			return groupHelp("tasks")
		default:
			return 2, fmt.Errorf("coop tasks %s works one queue at a time — pass a single --tasks <dir> (list and decisions span all %d configured queues)", sub, len(rels))
		}
	}
	if len(rels) == 0 {
		return 2, errors.New("coop tasks: no task queue configured — set COOP_TASKS or pass --tasks <dir>")
	}
	root := filepath.Join(repo, rels[0])
	// When the queue doesn't exist yet, a bare `coop tasks` still shows help, and `add`
	// bootstraps it on demand (tasksFolderAdd creates the folder) — that's how you start a
	// secondary --tasks queue in a monorepo, since `coop init` only scaffolds the repo root.
	// Every other subcommand needs an existing queue to act on.
	if !isTaskDir(root) {
		legacy := legacyTasksFile(root)
		switch sub {
		case "":
			// `coop tasks --tasks done` greedily eats `done` as the queue path, leaving no
			// subcommand — then help + exit 0 reads as a silent no-op. If the swallowed value
			// names a subcommand, the user almost certainly meant `coop tasks <sub>`.
			if len(flags) == 1 && isTasksSubcommand(flags[0]) {
				return 2, fmt.Errorf("`--tasks` takes a queue dir, not a subcommand — did you mean `coop tasks %s`? (no task queue %q here)", flags[0], rels[0])
			}
			return groupHelp("tasks")
		case "add":
			if legacy != "" {
				return 2, legacyMigrateErr(repo, legacy, rels[0])
			}
			// fall through — tasksFolderAdd creates the queue dir
		default:
			if legacy != "" {
				return 2, legacyMigrateErr(repo, legacy, rels[0])
			}
			return -1, fmt.Errorf("no task queue at %s — run 'coop init' (or 'coop tasks --tasks %s add \"…\"' to start one here)", rels[0], rels[0])
		}
	}
	return cmdTasksFolder(repo, root, rest)
}

// legacyMigrateErr is shown when a coop-v2 `.agent/TASKS.md` exists but the v3 folder queue does
// not — pointing at MIGRATING.md instead of `coop init`, which would scaffold an empty queue beside
// the populated legacy file and read as "v3 ate my tasks". legacyAbs is the legacy file's abs path.
func legacyMigrateErr(repo, legacyAbs, queueRel string) error {
	rel := legacyAbs
	if r, err := filepath.Rel(repo, legacyAbs); err == nil {
		rel = r
	}
	return fmt.Errorf("found a legacy %s from coop v2 — v3 stores one folder per task under %s/. "+
		"convert it once with the prompt in MIGRATING.md, then re-run ('coop init' scaffolds an EMPTY "+
		"queue and does NOT migrate it)", rel, queueRel)
}

// tasksListAll rolls up `coop tasks list` across several configured queues (a monorepo with a
// per-project .agent/tasks each), printing every queue under its rel-path header. A queue that
// doesn't exist yet is noted, not fatal — the mutating commands still require a single target.
func tasksListAll(repo string, rels []string) (int, error) {
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
			if _, err := tasksFolderList(root); err != nil {
				return -1, err
			}
		}
	}
	return 0, nil
}

// tasksDecisionsAll rolls up `coop tasks decisions` across several configured queues, each under
// its header (only queues that exist are shown).
func tasksDecisionsAll(repo string, rels []string) (int, error) {
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
		if _, err := tasksFolderDecisions(root); err != nil {
			return -1, err
		}
	}
	if first {
		ui.Note("no open decisions across %s", ui.Count(len(rels), "configured queue"))
	}
	return 0, nil
}
