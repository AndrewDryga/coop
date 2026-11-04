package tasks

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

// refusal is err as the shared block, failing the test when it is a bare sentence instead.
func refusal(t *testing.T, err error) *ui.UsageError {
	t.Helper()
	var usage *ui.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("refusal = %#v, want the shared block", err)
	}
	return usage
}

// pipedStdin runs the test with no terminal on stdin, as a script or an agent runs Coop.
func pipedStdin(t *testing.T) {
	t.Helper()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdin
	os.Stdin = devnull
	t.Cleanup(func() {
		os.Stdin = saved
		_ = devnull.Close()
	})
}

// An ID that names no single task is rejected input: the block says what matched, points at the
// family's own listing, and the command exits 2. A queue that cannot be read stays exit 1.
func TestTaskLookupRefusalsAreRejectedInput(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"2026-01-01-fix-login", "2026-01-02-fix-logout"} {
		writeTaskFile(t, filepath.Join(root, StateTodo, id, "task.md"), "# "+id+"\n")
	}
	writeTaskFile(t, filepath.Join(root, StateBacklog, "2026-01-03-idea", "task.md"), "# idea\n")

	_, err := FindTask(root, "absent")
	if got, want := refusal(t, err).Render(ui.Palette{}), "\n✗ No task matching \"absent\"\n\n  Tasks: coop tasks\n"; got != want {
		t.Errorf("missing task block:\n%q\nwant\n%q", got, want)
	}
	if lookupExit(err) != 2 {
		t.Errorf("lookupExit(missing task) = %d, want 2", lookupExit(err))
	}
	_, err = findBacklogTask(root, "absent")
	if got, want := refusal(t, err).Rows, [][2]string{{"Ideas:", "coop backlog"}}; !slices.Equal(got, want) {
		t.Errorf("missing idea rows = %q, want %q", got, want)
	}

	_, err = FindTask(root, "fix")
	ambiguous := refusal(t, err)
	if ambiguous.Headline != `2 tasks match "fix"` ||
		!slices.Equal(slices.Sorted(slices.Values(ambiguous.Choices)), []string{"2026-01-01-fix-login", "2026-01-02-fix-logout"}) ||
		!slices.Equal(ambiguous.Rows, [][2]string{{"", "Use one of these IDs."}}) {
		t.Errorf("ambiguous block = %#v, want both IDs listed", ambiguous)
	}

	if code := lookupExit(errors.New("read queue: permission denied")); code != 1 {
		t.Errorf("lookupExit(unreadable queue) = %d, want 1", code)
	}
}

// The add refusals are drawn in the shared block, each naming what to change.
func TestTasksAddRefusesInTheSharedBlock(t *testing.T) {
	root := t.TempDir()
	_, err := tasksFolderAdd(root, []string{"Half filled", "--context", "only this"}, StateTodo, "tasks add")
	want := "\n✗ Missing --acceptance and --approach for \"coop tasks add\"\n\n" +
		"      If you use any text option, include --context, --acceptance, and --approach.\n" +
		"      Without the text options, Coop creates a template for you to fill in.\n\n" +
		"  Usage: coop tasks add \"<title>\" --context <text> --acceptance <text> --approach <text> [--subtask <text>...]\n" +
		"  Help:  coop help tasks add\n"
	if got := refusal(t, err).Render(ui.Palette{}); got != want {
		t.Errorf("partial structured add:\n%s\nwant\n%s", got, want)
	}

	for _, tc := range []struct {
		args     []string
		headline string
	}{
		{[]string{"Fix login", "--context"}, `Missing value for "--context" in "coop tasks add"`},
		{[]string{"!!!"}, `Invalid title for "coop tasks add"`},
	} {
		code, err := tasksFolderAdd(root, tc.args, StateTodo, "tasks add")
		if got := refusal(t, err).Headline; code != 2 || got != tc.headline {
			t.Errorf("add %q = (%d, %q), want (2, %q)", tc.args, code, got, tc.headline)
		}
	}
	if items := mustReadTaskTree(t, root); len(items) != 0 {
		t.Errorf("refused adds created tasks: %+v", items)
	}
}

// A queue outside the repository is refused by name, saying whether --tasks or COOP_TASKS chose it:
// a person may not remember setting the variable.
func TestTaskQueuesOutsideTheRepositoryNameTheirSource(t *testing.T) {
	repo := t.TempDir()
	outside := filepath.Join(t.TempDir(), "tasks")
	_, err := TaskQueues(&config.Config{}, repo, []string{outside})
	if got, want := refusal(t, err).Headline, `Invalid value "`+outside+`" for "--tasks"`; got != want {
		t.Errorf("--tasks outside = %q, want %q", got, want)
	}
	_, err = TaskQueues(&config.Config{TasksFiles: []string{"../tasks"}}, repo, nil)
	block := refusal(t, err)
	if block.Headline != `Invalid value "../tasks" in COOP_TASKS` ||
		block.Cause != "A task queue must be a folder inside this repository, such as .agent/tasks." {
		t.Errorf("COOP_TASKS outside = %#v", block)
	}
}

// --project is refused in the shared block when it cannot choose a queue, before anything is added.
func TestTasksAddProjectSelectorRefusals(t *testing.T) {
	repo := gitFolder(t)
	for _, tc := range []struct {
		cfg      config.Config
		args     []string
		headline string
	}{
		{config.Config{}, []string{"add", "--project", "web", "x"}, `No subprojects for "--project" in "coop tasks add"`},
		{config.Config{}, []string{"add", "--project", "a", "--project", "b", "x"}, `Option "--project" can only be used once in "coop tasks add"`},
		{config.Config{}, []string{"add", "x", "--project"}, `Missing value for "--project" in "coop tasks add"`},
		{config.Config{}, []string{"--tasks", ".agent/tasks", "add", "--project", "web", "x"}, `Cannot combine "--project" and "--tasks" in "coop tasks add"`},
		{config.Config{TasksFiles: []string{".agent/tasks"}}, []string{"add", "--project", "web", "x"}, `Cannot use "--project" while COOP_TASKS is set`},
	} {
		cfg := tc.cfg
		code, err := CmdTasks(Host{}, &cfg, tc.args)
		if got := refusal(t, err).Headline; code != 2 || got != tc.headline {
			t.Errorf("coop tasks %q = (%d, %q), want (2, %q)", tc.args, code, got, tc.headline)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, TasksRoot)); !os.IsNotExist(err) {
		t.Errorf("a refused add set up a queue: %v", err)
	}
}

// watch takes only --json, once; anything else is refused in the shared block before the board opens.
func TestTasksWatchRefusesOtherArguments(t *testing.T) {
	cfg := validationQueues(t, 1)
	for _, tc := range []struct {
		args     []string
		headline string
	}{
		{[]string{"watch", "--bogus"}, `Unknown option "--bogus" for "coop tasks watch"`},
		{[]string{"watch", "--json", "--json"}, `Option "--json" can only be used once in "coop tasks watch"`},
		{[]string{"watch", "extra"}, `Unexpected argument "extra" for "coop tasks watch"`},
	} {
		code, err := CmdTasks(Host{}, cfg, tc.args)
		if got := refusal(t, err).Headline; code != 2 || got != tc.headline {
			t.Errorf("coop tasks %q = (%d, %q), want (2, %q)", tc.args, code, got, tc.headline)
		}
	}
}

// The claim, lease, block and decisions parsers refuse in the shared block, before any move, and a
// process Coop cannot bind to is a failure of the command rather than bad input.
func TestTaskActionParsersRefuseInTheSharedBlock(t *testing.T) {
	pipedStdin(t)
	cfg := validationQueues(t, 1)
	for _, tc := range []struct {
		args     []string
		code     int
		headline string
	}{
		{[]string{"claim", "x", "--as"}, 2, `Missing value for "--as" in "coop tasks claim"`},
		{[]string{"claim", "x", "--as", "not ok"}, 2, `Invalid value "not ok" for "--as" in "coop tasks claim"`},
		{[]string{"claim", "x", "--pid", "zero"}, 2, `Invalid value "zero" for "--pid" in "coop tasks claim"`},
		{[]string{"claim", "x", "--pid", "999999999"}, 1, `No live process with a readable identity at PID 999999999`},
		{[]string{"lease", "x", "--"}, 2, `Missing command after -- for "coop tasks lease"`},
		{[]string{"lease", "x", "--pid"}, 2, `Missing value for "--pid" in "coop tasks lease"`},
		{[]string{"lease", "x", "--as", "not ok"}, 2, `Invalid value "not ok" for "--as" in "coop tasks lease"`},
		{[]string{"lease", "x", "--pid", "zero"}, 2, `Invalid value "zero" for "--pid" in "coop tasks lease"`},
		{[]string{"lease", "x", "--bogus=1"}, 2, `Unknown option "--bogus" for "coop tasks lease"`},
		{[]string{"claim", "x", "--bogus=1"}, 2, `Unknown option "--bogus" for "coop tasks claim"`},
		{[]string{"claim", "x", "--tasks"}, 2, `Missing value for "--tasks" in "coop tasks"`},
		{[]string{"lease", "x", "y"}, 2, `Unexpected argument "y" for "coop tasks lease"`},
		{[]string{"lease", "x", "--pid", "999999999"}, 1, `No live process with a readable identity at PID 999999999`},
		{[]string{"block", "x", "y"}, 2, `Unexpected argument "y" for "coop tasks block"`},
		{[]string{"block", "x", "--answer=1"}, 2, `Unknown option "--answer" for "coop tasks block"`},
		{[]string{"block", "x", "--why", "w"}, 2, `Missing --question, --option, --recommendation for "coop tasks block"`},
		{[]string{"decisions", "--bogus"}, 2, `Unknown option "--bogus" for "coop tasks decisions"`},
		{[]string{"decisions", "extra"}, 2, `Unexpected argument "extra" for "coop tasks decisions"`},
		{[]string{"unblock", "x", "pending", "owner", "direction"}, 2, `"pending owner direction" reads as no answer yet`},
	} {
		code, err := CmdTasks(Host{}, cfg, tc.args)
		if got := refusal(t, err).Headline; code != tc.code || got != tc.headline {
			t.Errorf("coop tasks %q = (%d, %q), want (%d, %q)", tc.args, code, got, tc.code, tc.headline)
		}
	}
}

// A suggestion must work as written, so it keeps the task ID and the value the person typed, quoted.
func TestUnknownOptionSuggestionsKeepTheRestOfTheCommand(t *testing.T) {
	cfg := validationQueues(t, 1)
	typed := os.Args
	t.Cleanup(func() { os.Args = typed })
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"claim", "x", "--ass", "y"}, "coop tasks claim x --as y"},
		{[]string{"lease", "x", "--pdi=42", "--", "make", "test"}, "coop tasks lease x --pid=42 -- make test"},
		{[]string{"block", "x", "--questoin", "why not?"}, "coop tasks block x --question 'why not?'"},
		{[]string{"done", "x", "--forse"}, "coop tasks done x --force"},
	} {
		os.Args = append([]string{"coop", "tasks"}, tc.args...)
		_, err := CmdTasks(Host{}, cfg, tc.args)
		var got string
		for _, row := range refusal(t, err).Rows {
			if row[0] == "Did you mean:" {
				got = row[1]
			}
		}
		if got != tc.want {
			t.Errorf("coop tasks %q suggests %q, want %q", tc.args, got, tc.want)
		}
	}
}

// Without every required option, the example shows them all, so it too would be accepted.
func TestBlockExampleWouldBeAccepted(t *testing.T) {
	code, err := tasksFolderBlock(t.TempDir(), []string{"x", "--why"})
	var example string
	for _, row := range refusal(t, err).Rows {
		if row[0] == "Example:" {
			example = row[1]
		}
	}
	if code != 2 || !strings.Contains(example, "--question") || !strings.Contains(example, "--recommendation") {
		t.Errorf("block --why = %d, example %q; want one that would be accepted", code, example)
	}
}

// Without a terminal and without --yes, a deletion is refused in the shared block that names the
// command's page, and nothing is deleted.
func TestDeletionWithoutATerminalIsRefusedInTheSharedBlock(t *testing.T) {
	pipedStdin(t)
	root := t.TempDir()
	task := filepath.Join(root, StateTodo, "2026-01-01-keep")
	idea := filepath.Join(root, StateBacklog, "2026-01-02-idea")
	done := filepath.Join(root, StateDone, "2026-01-03-done")
	for _, dir := range []string{task, idea, done} {
		writeTaskFile(t, filepath.Join(dir, "task.md"), "# keep\n")
	}
	for _, tc := range []struct {
		run  func() (int, error)
		help string
	}{
		{func() (int, error) { return tasksFolderRemove(root, []string{"keep"}) }, "coop help tasks rm"},
		{func() (int, error) { return tasksFolderRemove(root, []string{"--all-done"}) }, "coop help tasks rm"},
		{func() (int, error) { return backlogFolderRemove(root, []string{"idea"}) }, "coop help backlog rm"},
	} {
		var code int
		var err error
		captureStderr(t, func() { code, err = tc.run() })
		block := refusal(t, err)
		if code != 2 || block.Headline != "Confirmation required" || !slices.Equal(block.Rows, [][2]string{{"Help:", tc.help}}) {
			t.Errorf("piped deletion = (%d, %#v), want Confirmation required pointing at %s", code, block, tc.help)
		}
	}
	for _, dir := range []string{task, idea, done} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("a refused deletion removed %s: %v", dir, err)
		}
	}
}
