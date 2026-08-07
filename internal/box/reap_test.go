package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeMounts struct {
	sources map[string]bool
	err     error
}

func (f fakeMounts) MountSourcesByLabel(
	context.Context, string, string,
) (map[string]bool, error) {
	return f.sources, f.err
}

func writeEntry(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

// Reaping takes both signals, because either alone is wrong.
//
// Ownership alone races a box that has written its config and not yet started
// its container — for that moment its files are genuinely unmounted, and
// deleting one breaks the launch in a way that looks like a corrupt config.
// Age alone is wrong because a warm box outlives any threshold.
func TestReapNeedsBothUnmountedAndOld(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	orphan := writeEntry(t, dir, "coop-mcp-orphan", 3*time.Hour)
	inUse := writeEntry(t, dir, "coop-mcp-live", 3*time.Hour)
	justWritten := writeEntry(t, dir, "coop-mcp-starting", time.Minute)
	notOurs := writeEntry(t, dir, "someone-elses-file", 3*time.Hour)

	removed, err := ReapOrphanTempEntries(
		context.Background(),
		fakeMounts{sources: map[string]bool{inUse: true}},
		dir, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d entries, want 1", removed)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("the orphan survived")
	}
	for name, path := range map[string]string{
		"a mounted file":            inUse,
		"a box that is starting up": justWritten,
		"another program's file":    notOurs,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was deleted: %v", name, err)
		}
	}
}

// A runtime that cannot be asked what is mounted must stop the reaper, not
// empty the directory.
//
// This is the failure that would turn a cleanup into an outage: docker briefly
// unavailable, every path reads as unmounted, and every running box loses its
// configuration at once.
func TestReapRefusesToGuessWhenTheRuntimeIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	orphan := writeEntry(t, dir, "coop-mcp-orphan", 3*time.Hour)

	removed, err := ReapOrphanTempEntries(
		context.Background(),
		fakeMounts{err: errors.New("cannot connect to the docker daemon")},
		dir, time.Now(),
	)
	if err == nil {
		t.Fatal("an unavailable runtime was treated as an empty mount list")
	}
	if removed != 0 {
		t.Fatalf("removed %d entries while unable to check what is in use", removed)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Error("deleted a file without being able to confirm it was unused")
	}
}

// Only coop's own temp entries are candidates. The temp directory is shared.
func TestReapTouchesOnlyCoopEntries(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"important-user-data", "go-build123", ".hidden", "coopish-but-not-ours",
	} {
		writeEntry(t, dir, name, 72*time.Hour)
	}
	removed, err := ReapOrphanTempEntries(
		context.Background(), fakeMounts{sources: map[string]bool{}}, dir, time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed %d entries that were not coop's", removed)
	}
}
