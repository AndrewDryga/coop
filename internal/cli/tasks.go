package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
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
// parent directory. The subcommands (list/lint/add/claim/block/unblock/done/drop/decisions/
// split) live in taskcmd.go; a bare `coop tasks` shows help.
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
	if len(rels) != 1 {
		return 2, errors.New("coop tasks works one queue at a time — give a single --tasks <dir>")
	}
	root := filepath.Join(repo, rels[0])
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	// When the queue doesn't exist yet, a bare `coop tasks` still shows help, and `add`
	// bootstraps it on demand (tasksFolderAdd creates the folder) — that's how you start a
	// secondary --tasks queue in a monorepo, since `coop init` only scaffolds the repo root.
	// Every other subcommand needs an existing queue to act on.
	if !isTaskDir(root) {
		switch sub {
		case "":
			return groupHelp("tasks")
		case "add":
			// fall through — tasksFolderAdd creates the queue dir
		default:
			return -1, fmt.Errorf("no task queue at %s — run 'coop init' (or 'coop tasks --tasks %s add \"…\"' to start one here)", rels[0], rels[0])
		}
	}
	return cmdTasksFolder(repo, root, rest)
}
