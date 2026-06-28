package scaffold

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/taskstate"
)

// stateDirRe matches a literal task-state dir name: a two-digit sort prefix + lowercase word(s)
// (00_todo, 10_in_progress, 50_blocked, 99_done).
var stateDirRe = regexp.MustCompile(`\b[0-9]{2}_[a-z]+(?:_[a-z]+)*\b`)

// TestTemplatesUseCurrentStateNames is the drift guard: the scaffold templates (the stop-guard
// hook, the docs) name the task-state dirs as literal strings — they can't import taskstate — so a
// rename there would silently diverge from the cli. scaffold.go's mkdirs already shares taskstate
// directly; this pins the string-y templates to the same source, so every state-dir token a
// template uses must be a current taskstate name, and the hook must still count the todo dir.
func TestTemplatesUseCurrentStateNames(t *testing.T) {
	known := map[string]bool{}
	for _, s := range taskstate.All {
		known[s] = true
	}
	if err := fs.WalkDir(templates, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := templates.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range stateDirRe.FindAllString(string(b), -1) {
			if !known[m] {
				t.Errorf("%s names state dir %q — not in taskstate.All %v; a rename missed this template", path, m, taskstate.All)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	hook, err := templates.ReadFile("templates/claude/hooks/stop-guard.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hook), taskstate.Todo) {
		t.Errorf("stop-guard.sh must count the todo dir %q", taskstate.Todo)
	}
}
