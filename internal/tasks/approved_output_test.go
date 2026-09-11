package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The approved output states of the `coop tasks` family, byte for byte. These run with color off
// (the captured streams are not terminals), which is the form a fixture pins.
func TestApprovedTaskOutput(t *testing.T) {
	t.Run("36-normal-listing", func(t *testing.T) {
		root := t.TempDir()
		writeTaskFile(t, filepath.Join(root, StateInProgress, "2026-09-11-fix-login-retries", "task.md"),
			"# Fix login retries\n\n## Subtasks\n- [x] one\n- [x] two\n- [ ] three\n- [ ] four\n")
		writeTaskFile(t, filepath.Join(root, StateTodo, "2026-09-11-add-password-reset", "task.md"), "# Add a password reset link\n")
		writeTaskFile(t, filepath.Join(root, StateBlocked, "2026-09-11-choose-session-duration", "task.md"), "# Choose how long sessions last\n")
		writeTaskFile(t, filepath.Join(root, StateBlocked, "2026-09-11-choose-session-duration", "decision.md"), "# Decision: How long should a session last?\n")
		writeTaskFile(t, filepath.Join(root, StateDone, "2026-09-10-show-sign-in-errors", "task.md"), "# Show sign-in errors\n")
		assertApprovedOutput(t, "36-normal-listing", captureStdout(t, func() { _, _ = tasksFolderList(root, false) }))
	})

	t.Run("36-empty", func(t *testing.T) {
		root := t.TempDir()
		if err := ScaffoldStateDirs(root); err != nil {
			t.Fatal(err)
		}
		assertApprovedOutput(t, "36-empty", captureStdout(t, func() { _, _ = tasksFolderList(root, false) }))
	})

	t.Run("36-empty-filter", func(t *testing.T) {
		root := t.TempDir()
		writeTaskFile(t, filepath.Join(root, StateTodo, "2026-09-11-add-password-reset", "task.md"), "# Add a password reset link\n")
		assertApprovedOutput(t, "36-empty-filter", captureStdout(t, func() { _, _ = tasksFolderList(root, false, StateBlocked) }))
	})

	t.Run("38-release-result", func(t *testing.T) {
		root := t.TempDir()
		writeTaskFile(t, filepath.Join(root, StateTodo, "2026-09-11-fix-login-retries", "task.md"), "# Fix login retries\n")
		if code, err := tasksFolderMove(root, []string{"fix-login-retries"}, StateInProgress, "claim", "claimed"); code != 0 || err != nil {
			t.Fatalf("claim: %d, %v", code, err)
		}
		out := captureStderr(t, func() {
			if code, err := tasksFolderRelease(root, []string{"fix-login-retries"}); code != 0 || err != nil {
				t.Fatalf("release: %d, %v", code, err)
			}
		})
		assertApprovedOutput(t, "38-release-result", out)
	})

	t.Run("38-already-returned", func(t *testing.T) {
		root := t.TempDir()
		writeTaskFile(t, filepath.Join(root, StateTodo, "2026-09-11-fix-login-retries", "task.md"), "# Fix login retries\n")
		out := captureStderr(t, func() {
			if code, err := tasksFolderRelease(root, []string{"fix-login-retries"}); code != 0 || err != nil {
				t.Fatalf("release: %d, %v", code, err)
			}
		})
		assertApprovedOutput(t, "38-already-returned", out)
	})

	t.Run("41-decisions-listing", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, StateBlocked, "2026-09-11-choose-session-duration")
		writeTaskFile(t, filepath.Join(dir, "task.md"), "# Choose how long sessions last\n")
		writeTaskFile(t, filepath.Join(dir, "decision.md"),
			"# Decision: How long should a session last?\n\n**Recommendation:** 30 days, with renewal while active.\n")
		assertApprovedOutput(t, "41-decisions-listing", captureStdout(t, func() { _, _ = tasksFolderDecisions(root, nil) }))
	})

	t.Run("41-decisions-empty", func(t *testing.T) {
		root := t.TempDir()
		writeTaskFile(t, filepath.Join(root, StateTodo, "2026-09-11-add-password-reset", "task.md"), "# Add a password reset link\n")
		assertApprovedOutput(t, "41-decisions-empty", captureStderr(t, func() { _, _ = tasksFolderDecisions(root, nil) }))
	})
}

// assertApprovedOutput compares got to the approved fixture byte for byte, reporting the first
// differing line so a drift is readable instead of a wall of text.
func assertApprovedOutput(t *testing.T, fixture, got string) {
	t.Helper()
	path := filepath.Join("testdata", "approved", fixture+".txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if string(data) == got {
		return
	}
	wantLines, gotLines := strings.Split(string(data), "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		w, g := "", ""
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			t.Fatalf("%s line %d:\n want %q\n  got %q", path, i+1, w, g)
		}
	}
	t.Fatalf("%s differs from the approved fixture", path)
}
