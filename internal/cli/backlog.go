package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// coop backlog — the staging drawer for unscheduled ideas, kept as task folders under
// .agent/tasks/xx_backlog/. It replaces the old .agent/BACKLOG.md file: one system (task folders), so
// an idea that's ready becomes live work with `coop backlog promote <id>` — a folder move to 00_todo/,
// not a hand-rewrite. xx_backlog lives OUTSIDE the lifecycle states (taskstate.All), so the loop, the
// Stop hook, and every counter ignore it; only the commands here read it (readBacklog/findBacklogTask).
// Queue resolution and the monorepo roll-up mirror cmdTasks, so backlog behaves the same across queues.

// backlogVerbs are the canonical `coop backlog` subcommands — the source for the unknown-subcommand
// suggester (a bare `coop backlog` lists the drawer, so it isn't a verb).
var backlogVerbs = []string{"ls", "add", "rm", "promote"}

// backlogArgSpecs validates the structured backlog subcommands (like taskArgSpecs for tasks); add takes
// a free-form title, so it's intentionally absent and validates its own args in tasksFolderAdd.
var backlogArgSpecs = map[string]taskArgSpec{
	"ls":      {nil, 0},
	"rm":      {[]string{"--yes", "-y"}, 1},
	"promote": {nil, 1},
}

func (a *app) cmdBacklog(args []string) (int, error) {
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
		// A monorepo configures several queues (the same set coop tasks/loop drain), each with its own
		// xx_backlog. ls rolls up across them; the id commands (rm/promote) find the item in whichever
		// queue holds it; only add — which CREATES into a queue — needs one unambiguous target.
		switch sub {
		case "", "ls":
			return backlogListAll(repo, rels)
		case "rm", "promote":
			return backlogAcrossQueues(repo, rels, rest)
		case "add":
			return 2, fmt.Errorf("coop backlog add works one queue at a time — pass a single --tasks <path> (ls, rm, and promote span all %d configured queues)", len(rels))
		default:
			return 2, unknownErr("backlog command", sub, backlogVerbs)
		}
	}
	if len(rels) == 0 {
		return 2, errors.New("coop backlog: no task queue configured — set COOP_TASKS or pass --tasks <path>")
	}
	return cmdBacklogFolder(filepath.Join(repo, rels[0]), rest)
}

// cmdBacklogFolder dispatches `coop backlog <sub>` against a single queue root (…/.agent/tasks). A bare
// `coop backlog` lists the drawer (the useful default, like bare `coop tasks`); add/rm/promote are the
// verbs. The queue need not exist yet — `add` creates xx_backlog on demand (tasksFolderAdd), the way a
// secondary --tasks queue is bootstrapped.
func cmdBacklogFolder(root string, rest []string) (int, error) {
	sub := ""
	var args []string
	if len(rest) > 0 {
		sub = rest[0]
		args = rest[1:]
	}
	if spec, ok := backlogArgSpecs[sub]; ok {
		if err := validateArgs("backlog "+sub, args, spec.flags, spec.maxPos); err != nil {
			return 2, err
		}
	}
	switch sub {
	case "", "ls":
		return backlogFolderList(root)
	case "add":
		return tasksFolderAdd(root, args, stateBacklog, "backlog add")
	case "rm":
		return backlogFolderRemove(root, args)
	case "promote":
		return backlogFolderPromote(root, args)
	default:
		return 2, unknownErr("backlog command", sub, backlogVerbs)
	}
}

// backlogFolderList prints one queue's backlog drawer: title-first, id faint below — the task list's
// shape, but without state grouping (everything here is one state) and without the subtask/blocked
// markers (a parked idea's placeholder subtask is noise, and nothing here is blocked). Empty is a note.
func backlogFolderList(root string) (int, error) {
	items := readBacklog(root)
	if len(items) == 0 {
		ui.Note("backlog is empty — capture an idea with 'coop backlog add \"<title>\"'")
		return 0, nil
	}
	p := ui.For(os.Stdout)
	fmt.Printf("%s %s\n", p.Bold("backlog"), p.Dim(fmt.Sprintf("(%d)", len(items))))
	for i, t := range items {
		if i > 0 {
			fmt.Println() // one blank line between items
		}
		for _, tl := range wrapWords(t.Title, titleWrapWidth()) {
			fmt.Printf("  %s\n", tl)
		}
		fmt.Printf("    %s\n", p.Faint(t.ID))
	}
	fmt.Print("\n")
	ui.Note("%s — promote one with 'coop backlog promote <id>' when it's ready to work", ui.Count(len(items), "backlog item"))
	return 0, nil
}

// backlogFolderRemove deletes a backlog item (a discarded idea). Reuses the destroyGate confirmation, so
// a fat-fingered id can't silently drop an idea and a piped run must opt in with --yes.
func backlogFolderRemove(root string, args []string) (int, error) {
	yes := hasYes(args)
	var pos []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
		}
	}
	if len(pos) != 1 {
		return 2, errors.New("usage: coop backlog rm <id> [--yes]")
	}
	t, err := findBacklogTask(root, pos[0]) // resolve the (possibly substring) match first, so the gate names it
	if err != nil {
		return 1, err
	}
	if err := destroyGate(fmt.Sprintf("delete backlog item %s", t.ID), yes); err != nil {
		return 2, err
	}
	if err := os.RemoveAll(t.Dir); err != nil {
		return -1, err
	}
	ui.OK("removed backlog item %s (note why in the commit if it mattered)", t.ID)
	return 0, nil
}

// backlogFolderPromote moves a backlog item into 00_todo/ — the whole point of the drawer: an idea
// that's ready becomes live work by a folder move (its id, log, and notes travel with it), not a
// hand-rewrite. Reuses moveTaskDir, so a torn move / id collision in todo is caught the same way.
func backlogFolderPromote(root string, args []string) (int, error) {
	if len(args) < 1 {
		return 2, errors.New("usage: coop backlog promote <id>")
	}
	t, err := findBacklogTask(root, args[0])
	if err != nil {
		return 1, err
	}
	if err := moveTaskDir(root, t, stateTodo); err != nil {
		return -1, err
	}
	ui.OK("promoted %s → todo — flesh out its task.md, then 'coop tasks claim %s' to start", t.ID, t.ID)
	return 0, nil
}

// backlogListAll rolls up `coop backlog` across the configured queues (a monorepo), each under its
// rel-path banner — the backlog analog of tasksListAll.
func backlogListAll(repo string, rels []string) (int, error) {
	p := ui.For(os.Stdout)
	for i, rel := range rels {
		if i > 0 {
			fmt.Print("\n\n\n") // three blank lines between different queues' output
		}
		fmt.Println(banner(p, rel))
		root := filepath.Join(repo, rel)
		if len(readBacklog(root)) == 0 {
			fmt.Println(p.Gray("  (backlog empty)"))
			continue
		}
		if _, err := backlogFolderList(root); err != nil {
			return -1, err
		}
	}
	return 0, nil
}

// backlogAcrossQueues routes `coop backlog rm|promote <id>` in a monorepo: find which queue's backlog
// holds the id, then run the single-queue handler against it — the backlog analog of tasksAcrossQueues.
func backlogAcrossQueues(repo string, rels []string, rest []string) (int, error) {
	args := rest[1:]
	id := ""
	for _, x := range args {
		if !strings.HasPrefix(x, "-") {
			id = x
			break
		}
	}
	if id == "" {
		// No id: delegate to the first queue so the handler's own usage error is what the user sees.
		return cmdBacklogFolder(filepath.Join(repo, rels[0]), rest)
	}
	rel, err := queueOfBacklogTask(repo, rels, id)
	if err != nil {
		return 1, err
	}
	return cmdBacklogFolder(filepath.Join(repo, rel), rest)
}

// queueOfBacklogTask finds which configured queue's xx_backlog holds the id — the backlog analog of
// queueOfTask, sharing its exact/substring precedence and duplicate-across-queues handling.
func queueOfBacklogTask(repo string, rels []string, id string) (string, error) {
	return queueOfTaskWith(repo, rels, id, readBacklog)
}
