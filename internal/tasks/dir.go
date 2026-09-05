package tasks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/taskstate"
)

// The folder-based task system. A task is a FOLDER under .agent/tasks/<state>/<id>/
// holding a task.md (a small YAML-ish frontmatter + a markdown body) and, optionally,
// spec.md / log.md / state.md / decision.md / screenshots/ / artifacts/ / tmp/. A task's
// workflow state is the parent directory it sits in — there is no status field, so
// nothing can drift. Moving the folder between state dirs IS the state change.
// The contract is in AGENTS.md; converting a legacy single-file queue is MIGRATING.md.

// tasksRoot is the repo-relative task queue directory every reader works against.
const TasksRoot = ".agent/tasks"

// Task state directories, in lifecycle order. The directory name IS the status. Each carries a
// numeric sort-key prefix so a plain `ls .agent/tasks` lists the states in lifecycle order
// (todo → in_progress → blocked → done) instead of alphabetically; done uses "99_" so it always
// sorts last. The names live in internal/taskstate — the one source the scaffold shares (cli
// imports scaffold, so scaffold can't import back) — and these are local aliases so call sites
// read unchanged. stateLabel strips the prefix for human-facing output; paths use the dir name.
const (
	StateTodo       = taskstate.Todo
	StateInProgress = taskstate.InProgress
	StateBlocked    = taskstate.Blocked
	StateDone       = taskstate.Done
	// stateBacklog is the staging drawer for unscheduled ideas (`coop backlog`). It lives OUTSIDE
	// taskStates on purpose, so readTaskTree/findTask/the loop/counters never see it — only the
	// readBacklog-based `coop backlog` commands do. See taskstate.Backlog.
	StateBacklog = taskstate.Backlog
)

// taskStates is the canonical ordered set of state directories.
var TaskStates = taskstate.All

// taskItem is one parsed task folder.
type Item struct {
	ID          string // = folder name; the stable handle (constant across moves)
	Title       string // frontmatter `title:`, else the body H1, else the ID
	State       string // one of taskStates — the parent directory
	Dir         string // absolute path to the task folder
	Subtasks    []bool // one per body checkbox; true = checked/done
	HasDecision bool   // a decision.md is present (must hold iff State == blocked)
}

// doneSubtasks returns how many of the task's subtask checkboxes are checked.
func (t Item) doneSubtasks() int {
	n := 0
	for _, d := range t.Subtasks {
		if d {
			n++
		}
	}
	return n
}

func taskQueueExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect task queue %s: %w", path, err)
	}
	return true, nil
}

// subtaskRe matches a markdown checkbox list item (a subtask) and captures its marker.
// It allows leading indentation so nested steps still count; the marker is one char.
var subtaskRe = regexp.MustCompile(`^[ \t]*[-*] \[(.)\] `)

// splitFrontmatter separates a leading `---` … `---` YAML-ish header from the body.
// It parses only flat `key: value` lines (enough for id/title/labels/parent/updated);
// comment (`#`) and blank lines are skipped. With no well-formed header, all of content
// is the body and fields is empty. Kept dependency-free on purpose — coop is stdlib-only.
func SplitFrontmatter(content string) (fields map[string]string, body string) {
	fields = map[string]string{}
	lines, body, ok := frontmatterLines(content)
	if !ok {
		return fields, content
	}
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if k, v, ok := strings.Cut(t, ":"); ok {
			fields[strings.TrimSpace(k)] = unquoteScalar(strings.TrimSpace(v))
		}
	}
	return fields, body
}

// FrontmatterList reads a list-valued frontmatter field in the three shapes a task file uses: a
// YAML flow list (`paths: [a, b]`), a block list (`paths:` followed by `- a` lines), and the bare
// space- or comma-separated scalar (`paths: a b`). Items are unquoted; a missing field yields nil.
func FrontmatterList(content, key string) []string {
	lines, _, ok := frontmatterLines(content)
	if !ok {
		return nil
	}
	var items []string
	add := func(item string) {
		if item = unquoteScalar(strings.TrimSpace(item)); item != "" {
			items = append(items, item)
		}
	}
	for i, l := range lines {
		k, v, found := strings.Cut(strings.TrimSpace(l), ":")
		if !found || strings.TrimSpace(k) != key {
			continue
		}
		v = strings.TrimSpace(v)
		switch {
		case v == "":
			for _, next := range lines[i+1:] {
				n := strings.TrimSpace(next)
				if !strings.HasPrefix(n, "-") {
					break
				}
				add(strings.TrimPrefix(n, "-"))
			}
		case strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]"):
			for _, part := range strings.Split(v[1:len(v)-1], ",") {
				add(part)
			}
		default:
			for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
				add(part)
			}
		}
		return items
	}
	return nil
}

// frontmatterLines returns the lines between the leading `---` fences and the body after them.
// The frontmatter is the first fence — but `coop tasks add` seeds task.md with a leading
// `<!-- … -->` header (and a hand-written file may have blank lines) before it, so skip those
// first. Without this a seeded task's title/labels/status field would go unparsed.
func frontmatterLines(content string) (lines []string, body string, ok bool) {
	all := strings.Split(content, "\n")
	start := 0
	for start < len(all) && strings.TrimSpace(all[start]) == "" {
		start++
	}
	if start < len(all) && strings.HasPrefix(strings.TrimSpace(all[start]), "<!--") {
		for start < len(all) {
			closed := strings.Contains(all[start], "-->")
			start++
			if closed {
				break
			}
		}
		for start < len(all) && strings.TrimSpace(all[start]) == "" {
			start++
		}
	}
	if start >= len(all) || strings.TrimSpace(all[start]) != "---" {
		return nil, content, false
	}
	end := -1
	for i := start + 1; i < len(all); i++ {
		if strings.TrimSpace(all[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, content, false // no closing fence — treat the whole thing as body
	}
	return all[start+1 : end], strings.Join(all[end+1:], "\n"), true
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

// parseTaskFolder reads dir/task.md into an Item, with State set by the caller. It returns
// ok=false only when a real task folder has no task.md, so an ordinary stray folder is skipped;
// present but unsafe or unreadable metadata is an error.
func parseTaskFolder(dir, state string) (Item, bool, error) {
	root, err := OpenTaskMetadataRoot(dir)
	if err != nil {
		return Item{}, false, err
	}
	defer root.Close()
	if _, err := root.Lstat("task.md"); errors.Is(err, os.ErrNotExist) {
		return Item{}, false, nil
	} else if err != nil {
		return Item{}, false, fmt.Errorf("inspect task.md: %w", err)
	}
	data, err := ReadTaskMetadataFile(root, "task.md")
	if err != nil {
		return Item{}, false, fmt.Errorf("read task.md: %w", err)
	}
	content := string(data)
	fields, body := SplitFrontmatter(content)
	id := filepath.Base(dir)
	title := fields["title"]
	if title == "" {
		title = firstH1(body)
	}
	if title == "" {
		title = id
	}
	title = sanitizeCell(title) // task.md can be agent-authored — keep control chars/ANSI out of output
	hasDecision := false
	if _, err := root.Lstat("decision.md"); err == nil {
		if _, err := ReadTaskMetadataFile(root, "decision.md"); err != nil {
			return Item{}, false, fmt.Errorf("read decision.md: %w", err)
		}
		hasDecision = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return Item{}, false, fmt.Errorf("inspect decision.md: %w", err)
	}
	return Item{
		ID:          id,
		Title:       title,
		State:       state,
		Dir:         dir,
		Subtasks:    scanSubtasks(body),
		HasDecision: hasDecision,
	}, true, nil
}

// ReadTaskTree enumerates every task folder under root's state directories, sorted by
// state (lifecycle order) then ID, so callers get a stable ordering. A missing state
// dir is simply empty. root is typically <repo>/.agent/tasks.
func ReadTaskTree(root string) ([]Item, error) {
	for attempt := 0; attempt < 3; attempt++ {
		items, retry, err := readTaskTreeOnce(root)
		if err != nil || !retry {
			return items, err
		}
	}
	return nil, fmt.Errorf("task queue %s kept changing while it was read; retry", root)
}

func readTaskTreeOnce(root string) ([]Item, bool, error) {
	var items []Item
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect task queue %s: %w", root, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("task queue %s is not a real directory", root)
	}
	// The four ReadDir calls below aren't one atomic snapshot, so a task being moved between state
	// dirs (an os.Rename) can be read in BOTH — once in the source dir, once in the destination.
	// Dedup by id, keeping the first (lifecycle-earliest) occurrence, so a torn read can't inflate
	// the counts (coop tasks watch) or flash a false "✓ done" as the last task finishes. A
	// PERSISTENT duplicate (a copy mistake, not a rename in flight) is an error: no caller may pick
	// one copy as lifecycle authority. The second look below still tolerates an atomic move that
	// completed while the four directories were scanned.
	seen := map[string]string{}
	duplicates := map[string][]string{}
	for _, state := range TaskStates {
		stateDir := filepath.Join(root, state)
		info, err := os.Lstat(stateDir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, fmt.Errorf("inspect lifecycle state %s: %w", stateDir, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, false, fmt.Errorf("lifecycle state %s is not a real directory", stateDir)
		}
		entries, err := os.ReadDir(stateDir)
		if errors.Is(err, os.ErrNotExist) {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("read lifecycle state %s: %w", stateDir, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				if e.Type().IsRegular() {
					// A stray regular file (a Finder .DS_Store from browsing the queue, an editor swap
					// file) cannot redirect lifecycle authority the way a symlink can, so it must not
					// hide the whole queue; `coop tasks lint` names the non-dotfile ones as misplaced work.
					continue
				}
				return nil, false, fmt.Errorf("task entry %s is not a real directory", filepath.Join(stateDir, e.Name()))
			}
			t, ok, err := parseTaskFolder(filepath.Join(stateDir, e.Name()), state)
			if errors.Is(err, os.ErrNotExist) {
				return nil, true, nil
			}
			if err != nil {
				return nil, false, fmt.Errorf("read task %s/%s: %w", state, e.Name(), err)
			}
			if !ok {
				continue
			}
			if first, exists := seen[t.ID]; exists {
				if len(duplicates[t.ID]) == 0 {
					duplicates[t.ID] = append(duplicates[t.ID], first)
				}
				duplicates[t.ID] = append(duplicates[t.ID], state)
			} else {
				seen[t.ID] = state
				items = append(items, t)
			}
		}
	}
	for id, states := range duplicates {
		var still []string
		for _, state := range states {
			_, err := os.Lstat(filepath.Join(root, state, id, "task.md"))
			if err == nil {
				still = append(still, state)
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, false, fmt.Errorf("recheck duplicate task %s in %s: %w", id, state, err)
			}
		}
		if len(still) > 1 {
			return nil, false, fmt.Errorf("task %s exists in multiple lifecycle states: %s", id, strings.Join(still, ", "))
		}
		if len(still) != len(states) {
			return nil, true, nil
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		si, sj := StateOrder(items[i].State), StateOrder(items[j].State)
		if si != sj {
			return si < sj
		}
		return items[i].ID < items[j].ID
	})
	return items, false, nil
}

// readBacklog enumerates the task folders under root's xx_backlog/, sorted by id. It reads ONE state
// dir — unlike readTaskTree, which walks the four lifecycle states and deliberately skips backlog —
// so it's the only path that surfaces backlog items (the `coop backlog` commands). A missing dir is
// simply empty. No cross-state dedup is needed: a backlog item lives only here until it's promoted
// (an atomic os.Rename out), so it can't be read in two states at once.
func ReadBacklog(root string) ([]Item, error) {
	var items []Item
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect task queue %s: %w", root, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("task queue %s is not a real directory", root)
	}
	backlog := filepath.Join(root, StateBacklog)
	info, err = os.Lstat(backlog)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect backlog %s: %w", backlog, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("backlog %s is not a real directory", backlog)
	}
	entries, err := os.ReadDir(backlog)
	if err != nil {
		return nil, fmt.Errorf("read backlog %s: %w", backlog, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			if e.Type().IsRegular() {
				continue // same as the lifecycle dirs: a stray file cannot redirect authority
			}
			return nil, fmt.Errorf("backlog entry %s is not a real directory", filepath.Join(backlog, e.Name()))
		}
		t, ok, err := parseTaskFolder(filepath.Join(backlog, e.Name()), StateBacklog)
		if errors.Is(err, os.ErrNotExist) {
			continue // promotion is one atomic rename out of the backlog
		}
		if err != nil {
			return nil, fmt.Errorf("read backlog item %s: %w", e.Name(), err)
		}
		if ok {
			items = append(items, t)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

// scaffoldStateDirs creates the four task-state dirs (00_todo/10_in_progress/50_blocked/99_done)
// under root. The move-a-folder-between-states protocol relies on every target dir existing: a bare
// `mv 00_todo/x 10_in_progress/` with no 10_in_progress/ *renames* the task folder to a file called
// 10_in_progress, silently corrupting the queue. Canonical queue and one-task projection producers
// call this before any move (`coop init` scaffolds the same four its own way). Idempotent — MkdirAll
// on an existing dir is a no-op.
func ScaffoldStateDirs(root string) error {
	for _, st := range TaskStates {
		if err := os.MkdirAll(filepath.Join(root, st), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// stateOrder maps a state to its lifecycle index for sorting (unknown states sort last).
func StateOrder(state string) int {
	for i, s := range TaskStates {
		if s == state {
			return i
		}
	}
	return len(TaskStates)
}

// stateLabel is a state's human-readable name with the on-disk sort prefix stripped
// ("00_todo" → "todo", "99_done" → "done"). Output uses it; filesystem paths use the
// dir name verbatim (so a path coop prints is one you can actually cd into).
func StateLabel(state string) string {
	if _, name, ok := strings.Cut(state, "_"); ok {
		return name
	}
	return state
}

// queueCounts reads a task queue directory (.agent/tasks) and returns its counts and active
// task — the one seam the status and loop readers funnel through. A missing/empty dir
// reads as all-zero.
func QueueCounts(dir string) (TaskCounts, string, error) {
	items, err := ReadTaskTree(dir)
	if err != nil {
		return TaskCounts{}, "", err
	}
	counts, active := TaskTreeCounts(items)
	return counts, active, nil
}

// latestTaskLog returns the last n lines of the most-recently-modified per-task log.md under
// ws's .agent/tasks tree (the agent's "why") — surfaced by `coop fork review`; "" if none.
func LatestTaskLog(ws string, n int) string {
	// Only COMPLETED tasks (99_done): a review's "why" is what this queue FINISHED. Scanning every
	// state can select a newer todo template instead; empty means the queue has finished nothing.
	matches, _ := filepath.Glob(filepath.Join(ws, TasksRoot, StateDone, "*", "log.md"))
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

// LatestForkTaskLog reads reviewed execution projections through the generation registry. It
// replaces review's old assumption that a fork owns a copied .agent/tasks tree.
func LatestForkTaskLog(repo, name string, n int) (string, error) {
	identity, ok, err := forkspace.ReadGeneration(repo, name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	assignments, err := ForkAssignments(repo, identity)
	if err != nil {
		return "", err
	}
	newest, newestMod := "", time.Time{}
	for _, assignment := range assignments {
		owner := assignment.Record.Fork
		if owner.Phase != ForkAssignmentReviewing && owner.Phase != ForkAssignmentReady {
			continue
		}
		item, ok, err := CurrentTask(owner.Projection, assignment.Item.ID)
		if err != nil {
			return "", err
		}
		if !ok || item.State != StateDone {
			continue
		}
		path := filepath.Join(item.Dir, "log.md")
		info, err := os.Lstat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.ModTime().After(newestMod) {
			opened, openErr := OpenTaskMetadataRoot(item.Dir)
			if openErr != nil {
				continue
			}
			data, readErr := ReadTaskMetadataFile(opened, "log.md")
			_ = opened.Close()
			if readErr == nil {
				newest, newestMod = string(data), info.ModTime()
			}
		}
	}
	if newest == "" {
		return "", nil
	}
	return lastLines(newest, n), nil
}

// taskTreeCounts tallies a task tree into the shared taskCounts and returns the "active"
// task title — the first in_progress task, or failing that the first todo — so the
// status and loop views agree on what a queue is "working on".
func TaskTreeCounts(items []Item) (TaskCounts, string) {
	var c TaskCounts
	active, firstTodo := "", ""
	for _, t := range items {
		switch t.State {
		case StateTodo:
			c.Todo++
			if firstTodo == "" {
				firstTodo = t.Title
			}
		case StateInProgress:
			c.Doing++
			if active == "" {
				active = t.Title
			}
		case StateBlocked:
			c.Blocked++
		case StateDone:
			c.Done++
		}
	}
	if active == "" {
		active = firstTodo
	}
	return c, active
}
