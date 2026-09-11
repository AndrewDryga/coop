package tasks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The backlog's human views are approved copy, not incidental formatting. These pin the exact
// blocks: a saved idea, the listing, a promotion, an empty drawer, and the deletion preview a
// person answers.
func TestBacklogOutput(t *testing.T) {
	// Resolved, because the printed paths are relative to the working directory and macOS hands
	// back /private/var for a /var temp dir — an unresolved repo would print an absolute path.
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(repo, ".agent", "tasks")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	// Empty drawer: the answer, then the one command that changes it.
	got := captureBacklog(t, func() { _, _ = cmdBacklogFolder(root, nil) })
	want := "No saved ideas yet.\n\n  Save one: coop backlog add \"Describe the idea\"\n"
	if got != want {
		t.Errorf("empty backlog =\n%q\nwant\n%q", got, want)
	}

	// An unfilled idea reports what was saved, where, and that it still needs notes.
	got = captureBacklog(t, func() { _, _ = cmdBacklogFolder(root, []string{"add", "Redesign account permissions"}) })
	id := onlyBacklogID(t, root)
	want = "✓ Saved idea: Redesign account permissions\n\n  " +
		filepath.Join(".agent", "tasks", StateBacklog, id, "task.md") + "\n\nAdd your notes to this file.\n"
	if got != want {
		t.Errorf("saved idea =\n%q\nwant\n%q", got, want)
	}

	// The listing leads with the count and ends with the one next step.
	got = captureBacklog(t, func() { _, _ = cmdBacklogFolder(root, nil) })
	want = "Backlog · 1 idea\n\n  Redesign account permissions\n    " + id + "\n\nReady to start: coop backlog promote <id>\n"
	if got != want {
		t.Errorf("backlog listing =\n%q\nwant\n%q", got, want)
	}

	// Promotion names the task and its new home.
	got = captureBacklog(t, func() { _, _ = cmdBacklogFolder(root, []string{"promote", id}) })
	want = "✓ Moved to todo: Redesign account permissions\n\n  " +
		filepath.Join(".agent", "tasks", StateTodo, id, "task.md") + "\n"
	if got != want {
		t.Errorf("promotion =\n%q\nwant\n%q", got, want)
	}
}

// A filled idea reports only what was saved: there are no placeholders left to fill in.
func TestBacklogStructuredAddOmitsTheNotesPrompt(t *testing.T) {
	repo := t.TempDir()
	root := filepath.Join(repo, ".agent", "tasks")
	got := captureBacklog(t, func() {
		_, _ = cmdBacklogFolder(root, []string{"add", "Support offline exports",
			"--context", "why", "--acceptance", "what", "--approach", "how"})
	})
	if !strings.Contains(got, "✓ Saved idea: Support offline exports") {
		t.Errorf("structured add = %q", got)
	}
	if strings.Contains(got, "Add your notes to this file.") {
		t.Errorf("a filled idea must not ask for notes:\n%s", got)
	}
}

// The deletion preview states the loss before the question, and a declined prompt says the idea
// was kept — before anything is removed.
func TestBacklogRemovePreviewAndCancellation(t *testing.T) {
	repo := t.TempDir()
	root := filepath.Join(repo, ".agent", "tasks")
	if _, err := cmdBacklogFolder(root, []string{"add", "Redesign account permissions"}); err != nil {
		t.Fatal(err)
	}
	id := onlyBacklogID(t, root)
	got := captureBacklog(t, func() { _, _ = cmdBacklogFolder(root, []string{"rm", id}) })
	for _, want := range []string{
		"Delete idea: Redesign account permissions",
		"Its notes and saved files will be permanently deleted.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("removal preview missing %q:\n%s", want, got)
		}
	}
	// No terminal to answer on, so the gate refuses rather than deleting — and the idea stays.
	if !pathExists(filepath.Join(root, StateBacklog, id)) {
		t.Error("an unconfirmed deletion must keep the idea")
	}

	// A declined prompt says so, in words, before anything is removed.
	said := captureBacklog(t, func() { _ = backlogCancelled(errors.New("cancelled")) })
	if said != "\nCancelled. The idea was kept.\n" {
		t.Errorf("decline = %q", said)
	}
	if got := backlogCancelled(errors.New("refusing to Delete this idea without confirmation")); got == nil {
		t.Error("a non-TTY refusal must stay an error")
	}
}

// onlyBacklogID returns the single idea in the drawer, failing when there is not exactly one.
func onlyBacklogID(t *testing.T, root string) string {
	t.Helper()
	items, err := ReadBacklog(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("backlog holds %d items, want 1", len(items))
	}
	return items[0].ID
}

// captureBacklog collects both streams: the listing is a stdout view, the results are ui lines on
// stderr, and the approved block is what a person sees when both land on one terminal.
func captureBacklog(t *testing.T, fn func()) string {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = w, w
	done := make(chan string)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, readErr := r.Read(buf)
			b.Write(buf[:n])
			if readErr != nil {
				break
			}
		}
		done <- b.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	return <-done
}
