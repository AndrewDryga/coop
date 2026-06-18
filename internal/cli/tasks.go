package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

// task is one parsed TASKS.md entry: its state marker, title, the section it sits under,
// and its raw lines (the "- [x] …" line plus the indented body) so a slice stays whole.
type task struct {
	State   string // " ", "w", "x", "B", or "E" (the example marker)
	Title   string
	Section string // the most recent "## …" heading, e.g. "Active" or "Example"
	Lines   []string
	LineNo  int // 1-based line of the title
}

func (t task) block() string { return strings.Join(t.Lines, "\n") }

// parseTasks is the one robust TASKS.md parser, shared so the loop, fleet split, status,
// and `coop tasks` can't drift. A task runs from its anchored "- [x] " line to the next
// task or "#" heading; everything indented under it is the body (trailing blanks trimmed).
func parseTasks(content string) []task {
	var tasks []task
	var cur *task
	section := ""
	flush := func() {
		if cur == nil {
			return
		}
		for len(cur.Lines) > 0 && strings.TrimSpace(cur.Lines[len(cur.Lines)-1]) == "" {
			cur.Lines = cur.Lines[:len(cur.Lines)-1]
		}
		tasks = append(tasks, *cur)
		cur = nil
	}
	for i, line := range strings.Split(content, "\n") {
		if m := taskLineRe.FindStringSubmatch(line); m != nil {
			flush()
			cur = &task{State: m[1], Title: strings.TrimSpace(line[len(m[0]):]), Section: section, Lines: []string{line}, LineNo: i + 1}
			continue
		}
		if strings.HasPrefix(line, "## ") {
			flush()
			section = strings.TrimSpace(line[len("## "):])
			continue
		}
		if strings.HasPrefix(line, "#") { // any other top-level heading/comment ends a body
			flush()
			continue
		}
		if cur != nil {
			cur.Lines = append(cur.Lines, line)
		}
	}
	flush()
	return tasks
}

// splitOpenTaskBlocks round-robins the unchecked ([ ]) task blocks (title + body) of a
// TASKS.md into n buckets — the anchored, body-preserving slice that `coop tasks split`
// and `coop fleet split` share. The legend, prose, and [E] example are excluded.
func splitOpenTaskBlocks(content string, n int) [][]string {
	buckets := make([][]string, n)
	i := 0
	for _, t := range parseTasks(content) {
		if t.State != " " {
			continue
		}
		buckets[i%n] = append(buckets[i%n], t.block())
		i++
	}
	return buckets
}

// extractTasksFlags pulls every `--tasks <path>` (or `--tasks=<path>`) out of args,
// returning the collected paths and the remaining args. Shared by `coop tasks` and
// `coop loop` so they accept the same repeatable flag.
func extractTasksFlags(args []string) (tasks []string, rest []string) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--tasks" && i+1 < len(args):
			tasks = append(tasks, args[i+1])
			i++
		case strings.HasPrefix(args[i], "--tasks="):
			tasks = append(tasks, strings.TrimPrefix(args[i], "--tasks="))
		default:
			rest = append(rest, args[i])
		}
	}
	return tasks, rest
}

// taskQueues resolves the task-file paths for a command — the repeated --tasks flags if
// any, else COOP_TASKS (cfg.TasksFiles) — into repo-relative paths. A relative path is
// taken relative to the repo root (where the box runs); an absolute one must live inside
// the repo. The repo-relative form is what the loop's prompt names and the host joins with
// the repo to read.
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

// cmdTasks treats the task queue(s) as a validated surface: list, lint, add, and split.
// list and lint span every --tasks/COOP_TASKS file; add and split target exactly one.
func (a *app) cmdTasks(args []string) (int, error) {
	flags, rest := extractTasksFlags(args)
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	rels, err := taskQueues(a.cfg, repo, flags)
	if err != nil {
		return 2, err
	}
	switch sub {
	case "list", "ls":
		return runPerFile(repo, rels, tasksList)
	case "lint":
		return runPerFile(repo, rels, tasksLint)
	case "add":
		if len(rels) != 1 {
			return 2, errors.New("coop tasks add targets one file — give a single --tasks")
		}
		return tasksAdd(filepath.Join(repo, rels[0]), rest[1:])
	case "split":
		if len(rels) != 1 {
			return 2, errors.New("coop tasks split targets one file — give a single --tasks")
		}
		return tasksSplit(repo, filepath.Join(repo, rels[0]), rest[1:])
	default:
		return 2, errors.New(`usage: coop tasks [--tasks <path>]... list | lint | add "<title>" | split <n>`)
	}
}

// runPerFile applies a per-file task command across each queue (repo-relative rels read
// under repo), printing the queue's path as a header when there's more than one. It stops
// at the first error and returns the worst (nonzero) exit code, so `lint` across files
// fails if any file has findings.
func runPerFile(repo string, rels []string, fn func(string) (int, error)) (int, error) {
	worst := 0
	for i, rel := range rels {
		if len(rels) > 1 {
			if i > 0 {
				fmt.Println() // a blank line between files
			}
			fmt.Println(ui.Bold(rel))
		}
		code, err := fn(filepath.Join(repo, rel))
		if err != nil {
			return code, err
		}
		if code != 0 {
			worst = code
		}
	}
	return worst, nil
}

var taskGlyph = map[string]string{" ": "[ ]", "w": "[w]", "x": "[x]", "B": "[B]"}

func tasksList(path string) (int, error) {
	content := readFileString(path)
	if content == "" {
		return -1, fmt.Errorf("no tasks at %s — run 'coop init'", path)
	}
	for _, t := range parseTasks(content) {
		if t.State == "E" {
			continue // the example, not work
		}
		// Color the state marker (and recede a done task's title), echoing the source's
		// "- [ ]". The verbs aren't width-padded, so coloring is safe here.
		glyph, title := taskGlyph[t.State], t.Title
		switch t.State {
		case "x":
			glyph, title = ui.Green(glyph), ui.Dim(title)
		case "w":
			glyph = ui.Yellow(glyph)
		case "B":
			glyph = ui.Red(glyph)
		}
		fmt.Printf("  - %s %s\n", glyph, title)
	}
	c, _ := scanTasks(content)
	color := func(v int, paint func(string) string) string {
		if v > 0 {
			return paint(strconv.Itoa(v))
		}
		return strconv.Itoa(v) // a zero stays plain, so "0 blocked" doesn't read as an alarm
	}
	fmt.Printf("\n  %d task(s) · %s done · %s in progress · %d todo · %s blocked\n",
		c.total(), color(c.Done, ui.Green), color(c.Doing, ui.Yellow), c.Todo, color(c.Blocked, ui.Red))
	return 0, nil
}

// requiredSections is the self-contained shape the contract requires of every task.
var requiredSections = []string{"Context", "Likely files", "Implementation direction", "Acceptance"}

// malformedMarkerRe matches a top-level list item that looks like a task but whose marker
// is not a real state — caught when taskLineRe (the valid form) does not also match.
var malformedMarkerRe = regexp.MustCompile(`^- \[[^\]]*\]`)

func tasksLint(path string) (int, error) {
	content := readFileString(path)
	if content == "" {
		return -1, fmt.Errorf("no tasks at %s — run 'coop init'", path)
	}
	var findings []string
	add := func(line int, msg string) { findings = append(findings, fmt.Sprintf("  TASKS.md:%d: %s", line, msg)) }

	for i, line := range strings.Split(content, "\n") {
		if malformedMarkerRe.MatchString(line) && !taskLineRe.MatchString(line) {
			add(i+1, "malformed task marker (use [ ] [w] [x] [B], or [E] for the example)")
		}
	}

	for _, t := range parseTasks(content) {
		if t.State == "E" {
			continue // the example is exempt from the work checks
		}
		if t.State == "w" {
			add(t.LineNo, fmt.Sprintf("stale claim? %q is [w] (claimed) but not done — finish it, reset to [ ], or remove", short(t.Title)))
		}
		if t.State == " " && strings.EqualFold(t.Section, "Example") {
			add(t.LineNo, fmt.Sprintf("unchecked task %q is under ## Example — the loop would pick it up; move it to ## Active or mark it [E]", short(t.Title)))
		}
		if t.State == " " || t.State == "w" || t.State == "B" {
			if missing := missingSections(t); len(missing) > 0 {
				add(t.LineNo, fmt.Sprintf("not self-contained: %q is missing %s", short(t.Title), strings.Join(missing, ", ")))
			}
		}
	}

	if len(findings) == 0 {
		ui.Info("tasks lint: clean")
		return 0, nil
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	ui.Info("tasks lint: %d finding(s)", len(findings))
	return 1, nil
}

// missingSections reports which of the required self-contained sections a task's body
// lacks (case-insensitive substring match on the section labels).
func missingSections(t task) []string {
	body := strings.ToLower(t.block())
	var missing []string
	for _, s := range requiredSections {
		if !strings.Contains(body, strings.ToLower(s)) {
			missing = append(missing, s)
		}
	}
	return missing
}

func short(s string) string { return truncate(s, 50) }

func tasksAdd(path string, args []string) (int, error) {
	title := strings.TrimSpace(strings.Join(args, " "))
	if title == "" {
		return 2, errors.New(`usage: coop tasks add "<title>"`)
	}
	content := readFileString(path)
	if content == "" {
		return -1, fmt.Errorf("no tasks at %s — run 'coop init'", path)
	}
	stub := fmt.Sprintf("\n- [ ] %s\n"+
		"  - **Context:** \n"+
		"  - **Likely files:** \n"+
		"  - **Implementation direction:** \n"+
		"  - **Acceptance checks:** \n", title)
	// ## Active is conventionally the last section, so appending keeps the new task in it.
	updated := strings.TrimRight(content, "\n") + "\n" + stub
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return -1, err
	}
	ui.Info("added task %q — fill in its Context / Likely files / Direction / Acceptance", short(title))
	return 0, nil
}

func tasksSplit(repo, path string, args []string) (int, error) {
	if len(args) < 1 {
		return 2, errors.New("usage: coop tasks split <n>")
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n <= 0 {
		return 2, errors.New("usage: coop tasks split <n>")
	}
	content := readFileString(path)
	if content == "" {
		return -1, fmt.Errorf("no tasks at %s — run 'coop init'", path)
	}
	wrote := 0
	for i, blocks := range splitOpenTaskBlocks(content, n) {
		if len(blocks) == 0 {
			continue
		}
		rel := filepath.Join(".agent", "TASKS."+strconv.Itoa(i+1)+".md")
		body := fmt.Sprintf("# %s — slice %d of %d\n\n%s\n", rel, i+1, n, strings.Join(blocks, "\n\n"))
		if err := os.WriteFile(filepath.Join(repo, rel), []byte(body), 0o644); err != nil {
			return -1, err
		}
		ui.Info("wrote %s (%d task[s])", rel, len(blocks))
		wrote++
	}
	if wrote == 0 {
		ui.Info("no unchecked [ ] tasks to split")
	}
	return 0, nil
}
