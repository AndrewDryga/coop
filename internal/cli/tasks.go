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

// cmdTasks treats .agent/TASKS.md as a validated surface: list, lint, add, and split.
func (a *app) cmdTasks(args []string) (int, error) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	path := filepath.Join(repo, ".agent", "TASKS.md")
	switch sub {
	case "list", "ls":
		return tasksList(path)
	case "lint":
		return tasksLint(path)
	case "add":
		return tasksAdd(path, args[1:])
	case "split":
		return tasksSplit(repo, path, args[1:])
	default:
		return 2, errors.New(`usage: coop tasks list | lint | add "<title>" | split <n>`)
	}
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
		fmt.Printf("  %s %s\n", taskGlyph[t.State], t.Title)
	}
	c, _ := scanTasks(content)
	fmt.Printf("\n  %d task(s) · %d done · %d in progress · %d todo · %d blocked\n",
		c.total(), c.Done, c.Doing, c.Todo, c.Blocked)
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
