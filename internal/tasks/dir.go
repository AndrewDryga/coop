package tasks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/taskstate"
)

// The folder-based task system. A task is a FOLDER under .agent/tasks/<state>/<id>/
// holding a task.md (a small YAML-ish frontmatter + a markdown body) and, optionally,
// spec.md / log.md / state.md / decision.md / screenshots/ / artifacts/ / tmp/. A task's
// workflow state is the parent directory it sits in — there is no status field, so
// nothing can drift. Moving the folder between state dirs IS the state change.
// The contract is in AGENTS.md.

// TasksRoot is the repo-relative task queue directory every reader works against.
const TasksRoot = ".agent/tasks"

// Task state directories, in lifecycle order. The directory name IS the status. Each carries a
// numeric sort-key prefix so a plain `ls .agent/tasks` lists the states in lifecycle order
// (todo → in_progress → blocked → done) instead of alphabetically; done uses "99_" so it always
// sorts last. The names live in internal/taskstate — the one source the scaffold shares (cli
// imports scaffold, so scaffold can't import back) — and these are local aliases so call sites
// read unchanged. StateLabel strips the prefix for human-facing output; paths use the dir name.
const (
	StateTodo       = taskstate.Todo
	StateInProgress = taskstate.InProgress
	StateBlocked    = taskstate.Blocked
	StateDone       = taskstate.Done
	// StateBacklog is the staging drawer for unscheduled ideas (`coop backlog`). It lives OUTSIDE
	// TaskStates on purpose, so ReadTaskTree/FindTask/the loop/counters never see it — only the
	// ReadBacklog-based `coop backlog` commands do. See taskstate.Backlog.
	StateBacklog = taskstate.Backlog
)

// TaskStates is the canonical ordered set of state directories.
var TaskStates = taskstate.All

// Item is one parsed task folder.
type Item struct {
	ID            string   // = folder name; the stable handle (constant across moves)
	Title         string   // frontmatter `title:`, else the body H1, else the ID
	State         string   // one of TaskStates — the parent directory
	Dir           string   // absolute path to the task folder
	Subtasks      []bool   // one per body checkbox; true = checked/done
	SubtaskLabels []string // SubtaskLabels[i] is the text of Subtasks[i]; empty for an Item built in memory
	HasDecision   bool     // a decision.md is present (must hold iff State == blocked)
	Priority      string   // frontmatter `priority:` as written, "" without one; see priorityRank
	WaitsOn       string   // frontmatter `waits_on:`, the task id this one must not start before (wait.go)
}

// taskPriorities are the values of the optional `priority:` frontmatter field, in the order the
// loop claims them. A task without the field is normal.
var taskPriorities = []string{"high", "normal", "low"}

// priorityRank orders todo tasks for the loop's next claim: high, then normal, then low, each in
// folder order. A missing or unknown value counts as normal, so a typo never moves a task; coop
// tasks lint names it.
func priorityRank(t Item) int {
	switch t.Priority {
	case "high":
		return 0
	case "low":
		return 2
	}
	return 1
}

// byPriority is the stable sort key for todo tasks already in folder order.
func byPriority(a, b Item) int { return priorityRank(a) - priorityRank(b) }

// claimOrder sorts todo tasks, given in the order the loop reads them (its queues in order, each in
// folder order), into the order it claims them: a person's priority first, that read order within
// each. The tasks board lists Up next with it too, so the board cannot name another next task.
func claimOrder(todo []QueuedTask) {
	slices.SortStableFunc(todo, func(a, b QueuedTask) int { return byPriority(a.Item, b.Item) })
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

// openSubtaskLabels returns the text of each unchecked item in task order, or nil when the item
// carries no labels to match its checkboxes.
func (t Item) openSubtaskLabels() []string {
	if len(t.SubtaskLabels) != len(t.Subtasks) {
		return nil
	}
	var open []string
	for i, done := range t.Subtasks {
		if !done {
			open = append(open, t.SubtaskLabels[i])
		}
	}
	return open
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

// SplitFrontmatter separates a leading `---` … `---` YAML-ish header from the body.
// It parses only flat `key: value` lines (enough for id/title/labels/parent/updated);
// comment (`#`) and blank lines are skipped. With no well-formed header, all of content
// is the body and fields is empty.
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
	start, end, ok := frontmatterFences(all)
	if !ok {
		return nil, content, false
	}
	return all[start+1 : end], strings.Join(all[end+1:], "\n"), true
}

// frontmatterFences finds the indexes of the opening and closing `---` lines frontmatterLines reads
// between, so a writer edits exactly the block a reader parses.
func frontmatterFences(all []string) (start, end int, ok bool) {
	start = 0
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
		return 0, 0, false
	}
	for i := start + 1; i < len(all); i++ {
		if strings.TrimSpace(all[i]) == "---" {
			return start, i, true
		}
	}
	return 0, 0, false // no closing fence — treat the whole thing as body
}

// unquoteScalar strips a YAML scalar's surrounding quotes. A title that opens with a flow
// indicator — `title: "[DEAD] …"` — MUST be quoted in the frontmatter, so the raw value arrives
// with the quotes; return the text the author meant.
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

// scanSubtasks returns the body's subtask checkboxes (outside code fences) as done-flags and
// their labels, index for index. A marker of x/X counts as done; anything else (space, w, /, …)
// is not done.
func scanSubtasks(body string) (done []bool, labels []string) {
	items := ScanChecklist(body)
	done = make([]bool, 0, len(items))
	labels = make([]string, 0, len(items))
	for _, item := range items {
		done = append(done, item.Checked)
		labels = append(labels, item.Label)
	}
	return done, labels
}

// ChecklistItem is one subtask checkbox with the label that follows its marker.
type ChecklistItem struct {
	Label   string
	Checked bool
}

// ScanChecklist is scanSubtasks with the labels kept: the same lines, the same fence rule and
// the same done marker, so the labels line up one-to-one with Item.Subtasks and a checkpoint's
// subtask booleans. Labels are sanitized like titles, because task.md is agent-authored.
func ScanChecklist(body string) []ChecklistItem {
	var items []ChecklistItem
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
			items = append(items, ChecklistItem{
				Label:   sanitizeCell(strings.TrimSpace(line[len(m[0]):])),
				Checked: strings.EqualFold(m[1], "x"),
			})
		}
	}
	return items
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
	return parseTaskFolderIn(root, dir, state)
}

// parseTaskFolderIn is parseTaskFolder for a task folder the caller already opened as root.
func parseTaskFolderIn(root *os.Root, dir, state string) (Item, bool, error) {
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
	subtasks, labels := scanSubtasks(body)
	return Item{
		ID:            id,
		Title:         title,
		State:         state,
		Dir:           dir,
		Subtasks:      subtasks,
		SubtaskLabels: labels,
		HasDecision:   hasDecision,
		Priority:      fields["priority"],
		WaitsOn:       sanitizeCell(strings.TrimSpace(fields[waitsOnField])),
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

// ReadTaskTreeBelow is ReadTaskTree for a queue inside top, a folder a running box mounts and can
// change anything in: the in-box task tool reads its queues this way, and the loop checks them with
// it at startup. It reads the queue at root only when every folder from top down to root is a real
// folder (requireRealFolders): a box that swapped one for a link, a fork's assignment folder or
// .agent, would otherwise have the tool list another project's tasks, and a subproject folder that
// is a link leads there too. ReadTaskTree itself refuses a link only at the queue root and below.
func ReadTaskTreeBelow(top, root string) ([]Item, error) {
	if err := requireRealFolders(top, root); err != nil {
		return nil, fmt.Errorf("refusing task queue %s: %w", root, err)
	}
	return ReadTaskTree(root)
}

// readStateEntries lists folder, a state folder opened as a real folder.
func readStateEntries(folder *os.Root) ([]os.DirEntry, error) {
	dir, err := folder.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, err
}

// parseTaskEntry reads the task folder name inside the open state folder. Opening it from that
// handle, not by path, also finds the task in the same state folder the listing came from, and
// skips re-opening the queue and the state folder for each archived task, which made a read of
// 1,300 tasks take about twice as long.
func parseTaskEntry(folder *os.Root, stateDir, name, state string) (Item, bool, error) {
	root, err := openRealSubroot(folder, name)
	if err != nil {
		return Item{}, false, err
	}
	defer root.Close()
	return parseTaskFolderIn(root, filepath.Join(stateDir, name), state)
}

// taskTreeReads counts whole-tree reads. A queue keeps every archived task, so a read costs a few
// file calls per task; the tests hold a project snapshot to one read per queue with it.
var taskTreeReads atomic.Int64

func readTaskTreeOnce(root string) ([]Item, bool, error) {
	taskTreeReads.Add(1)
	var items []Item
	info, err := lstatTaskPath(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect task queue %s: %w", root, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("task queue %s is not a real directory", root)
	}
	// One handle for the whole read: a state folder is found in it, never by name again.
	queue, err := openQueueRoot(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open task queue %s: %w", root, err)
	}
	defer queue.Close()
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
		info, err := queue.Lstat(state)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, fmt.Errorf("inspect lifecycle state %s: %w", stateDir, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, false, fmt.Errorf("lifecycle state %s is not a real directory", stateDir)
		}
		folder, err := openRealSubroot(queue, state)
		if errors.Is(err, os.ErrNotExist) {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("read lifecycle state %s: %w", stateDir, err)
		}
		stateItems, retry, err := readStateFolder(folder, stateDir, state, seen, duplicates)
		folder.Close()
		if err != nil || retry {
			return nil, retry, err
		}
		items = append(items, stateItems...)
	}
	for id, states := range duplicates {
		var still []string
		for _, state := range states {
			_, err := queue.Lstat(filepath.Join(state, id, "task.md"))
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

// readStateFolder parses every task folder in one open state folder, recording ids in seen and
// a second sighting in duplicates. retry reports a folder that moved away mid-read.
func readStateFolder(folder *os.Root, stateDir, state string, seen map[string]string, duplicates map[string][]string) ([]Item, bool, error) {
	entries, err := readStateEntries(folder)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read lifecycle state %s: %w", stateDir, err)
	}
	var items []Item
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
		t, ok, err := parseTaskEntry(folder, stateDir, e.Name(), state)
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
	return items, false, nil
}

// ReadBacklog enumerates the task folders under root's xx_backlog/, sorted by id. It reads ONE state
// dir — unlike ReadTaskTree, which walks the four lifecycle states and deliberately skips backlog —
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

// ScaffoldStateDirs creates the four task-state dirs (00_todo/10_in_progress/50_blocked/99_done)
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

// StateOrder maps a state to its lifecycle index for sorting (unknown states sort last).
func StateOrder(state string) int {
	for i, s := range TaskStates {
		if s == state {
			return i
		}
	}
	return len(TaskStates)
}

// StateLabel is a state's human-readable name with the on-disk sort prefix stripped
// ("00_todo" → "todo", "99_done" → "done"). Output uses it; filesystem paths use the
// dir name verbatim (so a path coop prints is one you can actually cd into).
func StateLabel(state string) string {
	if _, name, ok := strings.Cut(state, "_"); ok {
		return name
	}
	return state
}

// QueueCounts reads a task queue directory (.agent/tasks) and returns its counts and active
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

// LatestForkTaskLog reads reviewed execution projections through the generation registry, because
// a fork owns no copy of the .agent/tasks tree.
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

// TaskTreeCounts tallies a task tree into the shared TaskCounts and returns the "active"
// task title — the first in_progress task, or failing that the todo task the loop claims next —
// so the status and loop views agree on what a queue is "working on".
func TaskTreeCounts(items []Item) (TaskCounts, string) {
	var c TaskCounts
	active, firstTodo := "", ""
	var next Item
	for _, t := range items {
		switch t.State {
		case StateTodo:
			c.Todo++
			if firstTodo == "" || byPriority(t, next) < 0 {
				firstTodo, next = t.Title, t
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
