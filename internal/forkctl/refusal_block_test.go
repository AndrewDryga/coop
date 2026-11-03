package forkctl

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ui"
)

// `coop fork rm` refuses work it would discard in the shared block: what is there, and the ways
// forward. It is a command that could not do its job (exit 1), not rejected input.
func TestForkRmGuardNamesWhatDeletingWouldDiscard(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{RepoOverride: repo}}
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refusal := func(stage string) *ui.UsageError {
		t.Helper()
		code, err := c.ForkRm([]string{"perf", "--yes"})
		var usage *ui.UsageError
		if code != 1 || !errors.As(err, &usage) {
			t.Fatalf("fork rm with %s = (%d, %v), want exit 1 in the shared block", stage, code, err)
		}
		return usage
	}
	dirty := refusal("uncommitted changes")
	if dirty.Headline != `Fork "perf" has uncommitted changes` || dirty.Cause != "Deleting it would discard them." ||
		!slices.Equal(dirty.Rows, [][2]string{{"Review it:", "coop fork review perf"}, {"Delete it anyway:", "coop fork rm perf --force"}}) {
		t.Errorf("uncommitted refusal = %#v", dirty)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "work")
	unmerged := refusal("unmerged commits")
	if unmerged.Headline != `Fork "perf" has unmerged commits` ||
		!slices.Equal(unmerged.Rows, [][2]string{{"Merge it:", "coop fork merge perf"}, {"Delete it anyway:", "coop fork rm perf --force"}}) {
		t.Errorf("unmerged refusal = %#v", unmerged)
	}
	if !pathExists(ws) {
		t.Fatal("a refused fork rm deleted the fork")
	}
}
