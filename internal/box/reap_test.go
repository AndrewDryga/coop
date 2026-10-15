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

func TestReapPreservesRunDirectoryWhileAChildArtifactIsMounted(t *testing.T) {
	dir := t.TempDir()
	run := filepath.Join(dir, "coop-run-live")
	if err := os.Mkdir(run, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(run, "env")
	if err := os.WriteFile(artifact, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(run, when, when); err != nil {
		t.Fatal(err)
	}
	removed, err := ReapOrphanTempEntries(context.Background(), fakeMounts{sources: map[string]bool{artifact: true}}, dir, time.Now())
	if err != nil || removed != 0 {
		t.Fatalf("mounted run directory reap = %d, %v", removed, err)
	}
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("mounted artifact was reaped with its parent: %v", err)
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
	if !errors.Is(err, ErrMountsUnknown) {
		t.Fatalf("an unavailable runtime = %v, want ErrMountsUnknown, never an empty mount list", err)
	}
	if removed != 0 {
		t.Fatalf("removed %d entries while unable to check what is in use", removed)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Error("deleted a file without being able to confirm it was unused")
	}
}

func writeDirEntry(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

// Coop's Docker and Compose client directories are coop entries too. A killed run leaves one
// behind every time (3,731 Docker client directories on one Mac), so the sweep must know both
// names, under the same old-and-unmounted rule as everything else.
//
// A live run keeps handing Docker its client directory without mounting it, so in a temp
// directory other users can write, removing one would let any of them recreate it with a Docker
// config of their own. There the Docker client directories stay; everything else is swept as before.
func TestReapCoversCoopClientConfigs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mode         os.FileMode
		dockerReaped bool
	}{
		{name: "private temp directory", mode: 0o700, dockerReaped: true},
		{name: "shared temp directory", mode: 0o1777},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, tc.mode); err != nil {
				t.Fatal(err)
			}
			docker := writeDirEntry(t, dir, "coop-docker-client-123", 3*time.Hour)
			compose := writeDirEntry(t, dir, "coop-compose-client-456", 3*time.Hour)
			starting := writeDirEntry(t, dir, "coop-docker-client-789", time.Minute)

			removed, err := ReapOrphanTempEntries(context.Background(), fakeMounts{sources: map[string]bool{}}, dir, time.Now())
			want := 1
			if tc.dockerReaped {
				want = 2
			}
			if err != nil || removed != want {
				t.Fatalf("reap = %d, %v; want %d", removed, err, want)
			}
			if _, err := os.Stat(compose); !os.IsNotExist(err) {
				t.Errorf("the old Compose client directory survived: %v", err)
			}
			if _, err := os.Stat(docker); os.IsNotExist(err) != tc.dockerReaped {
				t.Errorf("old Docker client directory reaped = %v, want %v", os.IsNotExist(err), tc.dockerReaped)
			}
			if _, err := os.Stat(starting); err != nil {
				t.Errorf("a client directory younger than an hour was removed: %v", err)
			}
		})
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
