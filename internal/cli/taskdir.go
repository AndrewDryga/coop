package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The folder-based task system. A task is a FOLDER under .agent/tasks/<state>/<id>/
// holding a task.md (a small YAML-ish frontmatter + a markdown body) and, optionally,
// spec.md / log.md / state.md / decision.md / screenshots/ / artifacts/. A task's
// workflow state is the parent directory it sits in — there is no status field, so
// nothing can drift. Moving the folder between state dirs IS the state change.
// The contract is in AGENTS.md; converting a legacy single-file queue is MIGRATING.md.

// tasksRoot is the repo-relative directory whose existence selects folder mode; when it
// is a directory, every reader uses it instead of a legacy single TASKS.md file.
const tasksRoot = ".agent/tasks"

// Task state directories, in lifecycle order. The directory name IS the status.
const (
	stateTodo       = "todo"
	stateInProgress = "in_progress"
	stateBlocked    = "blocked"
	stateDone       = "done"
)

// taskStates is the canonical ordered set of state directories.
var taskStates = []string{stateTodo, stateInProgress, stateBlocked, stateDone}

// taskItem is one parsed task folder.
type taskItem struct {
	ID          string // = folder name; the stable handle (constant across moves)
	Title       string // frontmatter `title:`, else the body H1, else the ID
	State       string // one of taskStates — the parent directory
	Dir         string // absolute path to the task folder
	Subtasks    []bool // one per body checkbox; true = checked/done
	HasDecision bool   // a decision.md is present (must hold iff State == blocked)
}

// doneSubtasks returns how many of the task's subtask checkboxes are checked.
func (t taskItem) doneSubtasks() int {
	n := 0
	for _, d := range t.Subtasks {
		if d {
			n++
		}
	}
	return n
}

// isTaskDir reports whether path exists and is a directory — the folder-mode selector.
func isTaskDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// subtaskRe matches a markdown checkbox list item (a subtask) and captures its marker.
// It allows leading indentation so nested steps still count; the marker is one char.
var subtaskRe = regexp.MustCompile(`^[ \t]*[-*] \[(.)\] `)

// splitFrontmatter separates a leading `---` … `---` YAML-ish header from the body.
// It parses only flat `key: value` lines (enough for id/title/labels/parent/updated);
// comment (`#`) and blank lines are skipped. With no well-formed header, all of content
// is the body and fields is empty. Kept dependency-free on purpose — coop is stdlib-only.
func splitFrontmatter(content string) (fields map[string]string, body string) {
	fields = map[string]string{}
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return fields, content
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return fields, content // no closing fence — treat the whole thing as body
	}
	for _, l := range lines[1:end] {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if k, v, ok := strings.Cut(t, ":"); ok {
			fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return fields, strings.Join(lines[end+1:], "\n")
}

// firstH1 returns the first level-1 markdown heading in body (outside code fences), or "".
func firstH1(body string) string {
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		if fenceMarker(line) {
			inFence = !inFence
			continue
		}
		if !inFence && strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(line[len("# "):])
		}
	}
	return ""
}

// scanSubtasks returns the body's subtask checkboxes (outside code fences) as a slice of
// done-flags. A marker of x/X counts as done; anything else (space, w, /, …) is not done.
func scanSubtasks(body string) []bool {
	var subs []bool
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		if fenceMarker(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if m := subtaskRe.FindStringSubmatch(line); m != nil {
			subs = append(subs, strings.EqualFold(m[1], "x"))
		}
	}
	return subs
}

// parseTaskFolder reads dir/task.md into a taskItem, with State set by the caller. It
// returns ok=false when there is no task.md (so a stray non-task folder is skipped).
func parseTaskFolder(dir, state string) (taskItem, bool) {
	path := filepath.Join(dir, "task.md")
	if !fileExists(path) {
		return taskItem{}, false
	}
	content := readFileString(path)
	fields, body := splitFrontmatter(content)
	id := filepath.Base(dir)
	title := fields["title"]
	if title == "" {
		title = firstH1(body)
	}
	if title == "" {
		title = id
	}
	return taskItem{
		ID:          id,
		Title:       title,
		State:       state,
		Dir:         dir,
		Subtasks:    scanSubtasks(body),
		HasDecision: fileExists(filepath.Join(dir, "decision.md")),
	}, true
}

// readTaskTree enumerates every task folder under root's state directories, sorted by
// state (lifecycle order) then ID, so callers get a stable ordering. A missing state
// dir is simply empty. root is typically <repo>/.agent/tasks.
func readTaskTree(root string) []taskItem {
	var items []taskItem
	for _, state := range taskStates {
		stateDir := filepath.Join(root, state)
		entries, err := os.ReadDir(stateDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if t, ok := parseTaskFolder(filepath.Join(stateDir, e.Name()), state); ok {
				items = append(items, t)
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		si, sj := stateOrder(items[i].State), stateOrder(items[j].State)
		if si != sj {
			return si < sj
		}
		return items[i].ID < items[j].ID
	})
	return items
}

// copyTree recursively copies directory src into dst (creating dst and parents). Used to
// seed a fork worktree with a folder-mode task tree, and to write per-fork task slices.
func copyTree(src, dst string) error {
	return fs.WalkDir(os.DirFS(src), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, p) // p is "." for the root, then forward-slash rel paths
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(filepath.Join(src, p), target)
	})
}

// splitTodoFolders round-robins the todo task folders under root into len(names) per-fork
// task trees — siblings of root named "tasks.<name>" — copying each task folder into that
// slice's todo/. The source tree is left untouched (the slices are COPIES). Returns, per
// name, the repo-relative slice dir written ("" when that bucket was empty) and its task
// count, plus the total number of todo tasks. Shared by `coop tasks split` and `coop fleet
// split` so they can't drift.
func splitTodoFolders(repo, root string, names []string) (written []string, counts []int, total int, err error) {
	n := len(names)
	written = make([]string, n)
	counts = make([]int, n)
	if n == 0 {
		return written, counts, 0, nil
	}
	var todo []taskItem
	for _, it := range readTaskTree(root) {
		if it.State == stateTodo {
			todo = append(todo, it)
		}
	}
	parent := filepath.Dir(root)
	for i, it := range todo {
		b := i % n // deterministic round-robin over the sorted todo list
		if e := copyTree(it.Dir, filepath.Join(parent, "tasks."+names[b], stateTodo, it.ID)); e != nil {
			return nil, nil, 0, e
		}
		counts[b]++
	}
	for i := range names {
		if counts[i] == 0 {
			continue
		}
		sliceDir := filepath.Join(parent, "tasks."+names[i])
		if rel, e := filepath.Rel(repo, sliceDir); e == nil {
			written[i] = rel
		} else {
			written[i] = sliceDir
		}
	}
	return written, counts, len(todo), nil
}

// stateOrder maps a state to its lifecycle index for sorting (unknown states sort last).
func stateOrder(state string) int {
	for i, s := range taskStates {
		if s == state {
			return i
		}
	}
	return len(taskStates)
}

// defaultTasksFile is the legacy single-file queue path config falls back to when
// COOP_TASKS is unset (matching config.Load). Used only to detect "the user didn't
// override the default" so a present .agent/tasks/ folder tree can take precedence.
var defaultTasksFile = filepath.Join(".agent", "TASKS.md")

// queueCounts reads a task source — a folder-mode .agent/tasks directory or a legacy
// single TASKS.md file — and returns its counts and active task. This dir-or-file branch
// is the one seam every reader funnels through, so folder and legacy modes can't drift.
func queueCounts(path string) (taskCounts, string) {
	if isTaskDir(path) {
		return taskTreeCounts(readTaskTree(path))
	}
	return scanTasks(readFileString(path))
}

// wsTaskSource returns a workspace's task source: its .agent/tasks folder when that
// exists, else the legacy .agent/TASKS.md. Used by the status and fleet views, which
// read a fork's queue directly rather than through taskQueues.
func wsTaskSource(ws string) string {
	if dir := filepath.Join(ws, tasksRoot); isTaskDir(dir) {
		return dir
	}
	return filepath.Join(ws, ".agent", "TASKS.md")
}

// taskTreeCounts tallies a task tree into the shared taskCounts and returns the "active"
// task title — the first in_progress task, or failing that the first todo — mirroring the
// legacy scanTasks "first [w], else first [ ]" so the status/loop views read the same.
func taskTreeCounts(items []taskItem) (taskCounts, string) {
	var c taskCounts
	active, firstTodo := "", ""
	for _, t := range items {
		switch t.State {
		case stateTodo:
			c.Todo++
			if firstTodo == "" {
				firstTodo = t.Title
			}
		case stateInProgress:
			c.Doing++
			if active == "" {
				active = t.Title
			}
		case stateBlocked:
			c.Blocked++
		case stateDone:
			c.Done++
		}
	}
	if active == "" {
		active = firstTodo
	}
	return c, active
}
