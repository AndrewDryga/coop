package forkctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/fsidentity"
)

// `coop fork rm --gone` names the repositories whose fork records it would remove before it asks,
// removes nothing when the person says no, and removes exactly those records on yes. It takes no
// fork name and no --force, and needs a person at a terminal even with --yes.
func TestForkRmGoneListsThenRemovesOnConfirmation(t *testing.T) {
	base := t.TempDir()
	// The folder that held the repository still holds other work, so the repository is gone.
	if err := os.WriteFile(filepath.Join(base, "other-work.txt"), []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "deleted-project")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	stored, err := fsidentity.StoredPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := forkspace.EnsureStateDir(repo); err != nil {
		t.Fatal(err)
	}
	records := forkspace.StateDir(repo)
	for name, data := range map[string]string{
		"perf.generation.json":  `{"version":3,"name":"perf"}`,
		"perf.1.candidate.json": `{"canonical_root":"` + stored + `/.agent/tasks"}`,
	} {
		if err := os.WriteFile(filepath.Join(records, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	c := &Control{cfg: &config.Config{ConfigDir: t.TempDir()}}
	for _, args := range [][]string{{"--gone", "--force"}, {"--gone", "perf"}} {
		if code, err := c.ForkRm(args); code != 2 || err == nil {
			t.Errorf("coop fork rm %s = (%d, %v), want a usage refusal", strings.Join(args, " "), code, err)
		}
	}
	terminal := false
	old := goneStdinIsTerminal
	goneStdinIsTerminal = func() bool { return terminal }
	t.Cleanup(func() { goneStdinIsTerminal = old })
	for _, args := range [][]string{{"--gone"}, {"--gone", "--yes"}} {
		if code, err := c.ForkRm(args); code != 2 || err == nil || !strings.Contains(err.Error(), "needs a person at a terminal") {
			t.Fatalf("coop fork rm %s without a terminal = (%d, %v), want the person-only refusal", strings.Join(args, " "), code, err)
		}
	}
	if _, err := os.Lstat(records); err != nil {
		t.Fatalf("a refused removal removed the records: %v", err)
	}
	terminal = true
	asked := ""
	c.askDelete = func(question string) bool { asked = question; return false }
	var code int
	stderr := captureStderr(t, func() { code, err = c.ForkRm([]string{"--gone"}) })
	if code != 1 || !strings.Contains(stderr, "  - "+stored+" (forks perf)") || !strings.Contains(stderr, "Nothing was removed.") || !strings.Contains(stderr, "If you moved one of them") || asked != "Remove them?" {
		t.Fatalf("declined coop fork rm --gone = (%d, %v), asked %q:\n%s", code, err, asked, stderr)
	}
	if _, err := os.Lstat(records); err != nil {
		t.Fatalf("a declined removal removed the records: %v", err)
	}
	stderr = captureStderr(t, func() { code, err = c.ForkRm([]string{"--gone", "--yes"}) })
	if code != 0 || err != nil || !strings.Contains(stderr, "Removed") {
		t.Fatalf("coop fork rm --gone --yes = (%d, %v):\n%s", code, err, stderr)
	}
	if _, err := os.Lstat(records); !os.IsNotExist(err) {
		t.Fatalf("the gone project's records are still there: %v", err)
	}
}
