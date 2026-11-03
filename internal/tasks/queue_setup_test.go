package tasks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// gitFolder is a fresh folder Coop finds as a Git repository (a .git folder is all FindRepo needs),
// made the working folder the way a person runs coop inside their project.
func gitFolder(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	return repo
}

// Outside a Git repository nothing owns a new queue, so add refuses with the next step and leaves
// the folder as it was. Listing there says the same thing, bare or not, and so does the backlog.
func TestTasksOutsideAProjectRefuseToStartAQueue(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cfg := &config.Config{}
	for _, args := range [][]string{{"add", "Fix login retries"}, {"ls"}, nil} {
		code, err := CmdTasks(Host{}, cfg, args)
		if code != 1 || err == nil || !strings.Contains(err.Error(), "not inside a Git repository") || !strings.Contains(err.Error(), "coop init") {
			t.Fatalf("coop tasks %q outside a project = (%d, %v), want a refusal that names coop init", args, code, err)
		}
	}
	if code, err := CmdBacklog(cfg, []string{"add", "Redesign permissions"}); code != 1 || err == nil || !strings.Contains(err.Error(), "coop init") {
		t.Fatalf("coop backlog add outside a project = (%d, %v), want the same refusal", code, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".agent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused add left a queue behind: %v", err)
	}
}

// In a Git repository nobody ran coop init in, the first add sets the queue up the way init does:
// README.md, which task.md's header points to, and the .gitignore lines that keep task folders out
// of Git. It says what it wrote, once.
func TestTasksFirstAddSetsUpTheQueue(t *testing.T) {
	repo := gitFolder(t)
	cfg := &config.Config{}
	out := captureStderr(t, func() {
		if code, err := CmdTasks(Host{}, cfg, []string{"add", "Fix login retries"}); code != 0 || err != nil {
			t.Fatalf("first add = (%d, %v)", code, err)
		}
	})
	queue := filepath.Join(repo, ".agent", "tasks")
	if !fileExists(filepath.Join(queue, "README.md")) {
		t.Fatal("the first add did not write the queue's README.md")
	}
	if gi := readFileString(filepath.Join(repo, ".gitignore")); !strings.Contains(gi, "**/.agent/tasks/*\n") {
		t.Fatalf(".gitignore does not keep task folders out of Git:\n%s", gi)
	}
	for _, want := range []string{"Set up a task queue in .agent/tasks", "README.md", ".gitignore", "✓ Created task: Fix login retries"} {
		if !strings.Contains(out, want) {
			t.Errorf("first add output lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "Set up a task queue") > strings.Index(out, "Created task") {
		t.Errorf("the setup is reported after the task it made room for:\n%s", out)
	}
	out = captureStderr(t, func() {
		if code, err := CmdTasks(Host{}, cfg, []string{"add", "Show sign-in errors"}); code != 0 || err != nil {
			t.Fatalf("second add = (%d, %v)", code, err)
		}
	})
	if strings.Contains(out, "Set up") {
		t.Errorf("a second add reported setting the queue up again:\n%s", out)
	}
	if code, err := CmdBacklog(cfg, []string{"add", "Redesign permissions"}); code != 0 || err != nil {
		t.Fatalf("backlog add into the set-up queue = (%d, %v)", code, err)
	}
}

// Bare `coop tasks` is `coop tasks ls`: before the first task they print the same empty listing.
// A queue picked explicitly that does not exist is refused, bare or listed, with the add that
// starts it; another command on the project's missing queue names plain add, which sets it up.
func TestBareTasksAndListAgreeBeforeTheFirstTask(t *testing.T) {
	gitFolder(t)
	cfg := &config.Config{}
	var listings []string
	for _, args := range [][]string{nil, {"ls"}} {
		listings = append(listings, captureStdout(t, func() {
			if code, err := CmdTasks(Host{}, cfg, args); code != 0 || err != nil {
				t.Fatalf("coop tasks %q before the first task = (%d, %v)", args, code, err)
			}
		}))
	}
	if listings[0] != listings[1] || !strings.Contains(listings[0], "No tasks yet.") || !strings.Contains(listings[0], `coop tasks add "Describe the work"`) {
		t.Fatalf("bare and ls disagree or miss the empty state:\nbare: %q\nls:   %q", listings[0], listings[1])
	}
	for _, args := range [][]string{{"--tasks", "web/.agent/tasks"}, {"--tasks", "web/.agent/tasks", "ls"}} {
		code, err := CmdTasks(Host{}, cfg, args)
		if code != 1 || err == nil || !strings.Contains(err.Error(), `coop tasks --tasks web/.agent/tasks add "<title>"`) {
			t.Fatalf("coop tasks %q on a missing queue = (%d, %v)", args, code, err)
		}
	}
	code, err := CmdTasks(Host{}, cfg, []string{"lint"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), `coop tasks add "<title>"`) || strings.Contains(err.Error(), "--tasks") {
		t.Fatalf("lint before the first task = (%d, %v), want plain add as the next step", code, err)
	}
}
