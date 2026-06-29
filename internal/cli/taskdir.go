package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/taskstate"
)

// The folder-based task system. A task is a FOLDER under .agent/tasks/<state>/<id>/
// holding a task.md (a small YAML-ish frontmatter + a markdown body) and, optionally,
// spec.md / log.md / state.md / decision.md / screenshots/ / artifacts/. A task's
// workflow state is the parent directory it sits in — there is no status field, so
// nothing can drift. Moving the folder between state dirs IS the state change.
// The contract is in AGENTS.md; converting a legacy single-file queue is MIGRATING.md.

// tasksRoot is the repo-relative task queue directory every reader works against.
const tasksRoot = ".agent/tasks"

// Task state directories, in lifecycle order. The directory name IS the status. Each carries a
// numeric sort-key prefix so a plain `ls .agent/tasks` lists the states in lifecycle order
// (todo → in_progress → blocked → done) instead of alphabetically; done uses "99_" so it always
// sorts last. The names live in internal/taskstate — the one source the scaffold shares (cli
// imports scaffold, so scaffold can't import back) — and these are local aliases so call sites
// read unchanged. stateLabel strips the prefix for human-facing output; paths use the dir name.
const (
	stateTodo       = taskstate.Todo
	stateInProgress = taskstate.InProgress
	stateBlocked    = taskstate.Blocked
	stateDone       = taskstate.Done
)

// taskStates is the canonical ordered set of state directories.
var taskStates = taskstate.All

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

// legacyTasksFile returns the path to a coop-v2 `.agent/TASKS.md` sitting beside the (v3) folder
// queue dir, or "" if none. An upgrading customer is pointed at MIGRATING.md instead of being told
// to `coop init` — which would scaffold an EMPTY queue next to their populated legacy file and read
// as "v3 ate my tasks". queueRoot is the queue dir (…/.agent/tasks); the legacy file is its sibling.
func legacyTasksFile(queueRoot string) string {
	p := filepath.Join(filepath.Dir(queueRoot), "TASKS.md")
	if fileExists(p) {
		return p
	}
	return ""
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
	// The frontmatter is the first `---` fence — but `coop tasks add` seeds task.md with a leading
	// `<!-- … -->` header (and a hand-written file may have blank lines) before it, so skip those
	// first. Without this a seeded task's title/labels/status field would go unparsed.
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	if start < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[start]), "<!--") {
		for start < len(lines) {
			closed := strings.Contains(lines[start], "-->")
			start++
			if closed {
				break
			}
		}
		for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
			start++
		}
	}
	if start >= len(lines) || strings.TrimSpace(lines[start]) != "---" {
		return fields, content
	}
	end := -1
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return fields, content // no closing fence — treat the whole thing as body
	}
	for _, l := range lines[start+1 : end] {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if k, v, ok := strings.Cut(t, ":"); ok {
			fields[strings.TrimSpace(k)] = unquoteScalar(strings.TrimSpace(v))
		}
	}
	return fields, strings.Join(lines[end+1:], "\n")
}

// unquoteScalar strips a YAML scalar's surrounding quotes. A title that opens with a flow
// indicator — `title: "[DEAD] …"` — MUST be quoted in the frontmatter, so the raw value arrives
// with the quotes; return the text the author meant. Stdlib-only (no YAML dependency).
func unquoteScalar(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		if s, err := strconv.Unquote(v); err == nil { // handles \" \\ \n … for the common case
			return s
		}
		return v[1 : len(v)-1] // a malformed escape: at least drop the outer quotes
	}
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'") // YAML single-quote: '' is an escaped '
	}
	return v
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
	title = sanitizeCell(title) // task.md can be agent-authored — keep control chars/ANSI out of output
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
	// The four ReadDir calls below aren't one atomic snapshot, so a task being moved between state
	// dirs (an os.Rename) can be read in BOTH — once in the source dir, once in the destination.
	// Dedup by id, keeping the first (lifecycle-earliest) occurrence, so a torn read can't inflate
	// the counts (coop tasks watch) or flash a false "✓ done" as the last task finishes.
	seen := map[string]bool{}
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
			if t, ok := parseTaskFolder(filepath.Join(stateDir, e.Name()), state); ok && !seen[t.ID] {
				seen[t.ID] = true
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
	// Pre-clean each slice so a RE-split regenerates from the current source instead of merging into
	// a stale one: without this, a task already worked in a slice (moved to its 99_done/) plus a fresh
	// todo copy of the same id would leave the id in two states in one slice — and a loop on it would
	// re-run completed work. Slices are disposable copies of the (untouched) source, safe to rebuild.
	for _, name := range names {
		if e := os.RemoveAll(filepath.Join(parent, "tasks."+name)); e != nil {
			return nil, nil, 0, e
		}
	}
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

// stateLabel is a state's human-readable name with the on-disk sort prefix stripped
// ("00_todo" → "todo", "99_done" → "done"). Output uses it; filesystem paths use the
// dir name verbatim (so a path coop prints is one you can actually cd into).
func stateLabel(state string) string {
	if _, name, ok := strings.Cut(state, "_"); ok {
		return name
	}
	return state
}

// queueCounts reads a task queue directory (.agent/tasks) and returns its counts and active
// task — the one seam the status, loop, and fleet readers funnel through. A missing/empty dir
// reads as all-zero.
func queueCounts(dir string) (taskCounts, string) {
	return taskTreeCounts(readTaskTree(dir))
}

// wsTaskSource returns a workspace's task queue directory (.agent/tasks). Used by the status
// and fleet views, which read a fork's queue directly rather than through taskQueues.
func wsTaskSource(ws string) string {
	return filepath.Join(ws, tasksRoot)
}

// latestTaskLog returns the last n lines of the most-recently-modified per-task log.md under
// ws's .agent/tasks tree (the agent's "why") — surfaced by `coop fork review`; "" if none.
func latestTaskLog(ws string, n int) string {
	matches, _ := filepath.Glob(filepath.Join(ws, tasksRoot, "*", "*", "log.md"))
	newest, newestMod := "", time.Time{}
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.ModTime().After(newestMod) {
			newest, newestMod = m, fi.ModTime()
		}
	}
	if newest == "" {
		return ""
	}
	return lastLines(readFileString(newest), n)
}

// taskTreeCounts tallies a task tree into the shared taskCounts and returns the "active"
// task title — the first in_progress task, or failing that the first todo — so the
// status, loop, and fleet views all agree on what a queue is "working on".
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
