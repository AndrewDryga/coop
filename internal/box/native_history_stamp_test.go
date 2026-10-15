package box

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// idleRuntime reports no containers, so no retained writer blocks an import.
func idleRuntime(t *testing.T) runtime.Runtime {
	t.Helper()
	shim := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\ncase \"$1\" in ps) exit 0;; *) exit 19;; esac\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: shim}
}

// Retained stores are owner-private like the custody beside them.
func writeRetainedRollout(t *testing.T, source, day, id, cwd string) string {
	t.Helper()
	rel := filepath.Join("sessions", "2026", "10", day, "rollout-"+id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(source, rel)), 0o700); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q}}`+"\n", id, cwd)
	if err := os.WriteFile(filepath.Join(source, rel), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return rel
}

func nativeHistoryStamps(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(home), "imports"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var stamps []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "complete-") {
			stamps = append(stamps, filepath.Join(filepath.Dir(home), "imports", entry.Name()))
		}
	}
	return stamps
}

func TestNativeHistoryStampSkipsAnImportedSourceUntilItChanges(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node"}
	repo := t.TempDir()
	source := cfg.AgentProfileDir("codex", "default")
	first := writeRetainedRollout(t, source, "01", "11111111-2222-4333-8444-555555555555", repo)
	rt := idleRuntime(t)
	planned := 0
	prepare := func(scope string) string {
		t.Helper()
		home, err := prepareNativeHomeFromSource(t.Context(), cfg, rt, "codex", "default", repo, false, source, scope, func(cwd string) bool {
			planned++
			return cwd == repo
		})
		if err != nil {
			t.Fatal(err)
		}
		return home
	}
	expectPlanned := func(scope string, want bool) string {
		t.Helper()
		planned = 0
		home := prepare(scope)
		if (planned > 0) != want {
			t.Fatalf("scope %q planned=%d, want planning=%v", scope, planned, want)
		}
		return home
	}

	home := expectPlanned("", true)
	if _, err := os.Stat(filepath.Join(home, first)); err != nil {
		t.Fatal("first import missing", err)
	}
	stamps := nativeHistoryStamps(t, home)
	if len(stamps) != 1 {
		t.Fatalf("stamps=%v, want one beside the receipts", stamps)
	}
	if again := expectPlanned("", false); again != home {
		t.Fatal("stamped launch selected another home")
	}
	// A different ownership rule has to plan for itself.
	expectPlanned("another owner", true)
	expectPlanned("another owner", false)

	// An unreadable stamp is a miss, never a reason to fail or to skip.
	if err := os.WriteFile(stamps[0], []byte("not a stamp"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectPlanned("", true)
	expectPlanned("", false)

	second := writeRetainedRollout(t, source, "02", "22222222-2222-4333-8444-555555555555", repo)
	expectPlanned("", true)
	if _, err := os.Stat(filepath.Join(home, second)); err != nil {
		t.Fatal("new retained history was not imported", err)
	}
	expectPlanned("", false)
	err := filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(entry.Name(), "complete-") {
			return fmt.Errorf("stamp inside the mounted home: %s", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNativeHistoryStampWaitsForACompleteImport(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node"}
	repo := t.TempDir()
	source := cfg.AgentProfileDir("codex", "default")
	first := writeRetainedRollout(t, source, "01", "11111111-2222-4333-8444-555555555555", repo)
	second := writeRetainedRollout(t, source, "02", "22222222-2222-4333-8444-555555555555", repo)
	home, err := prepareNativeHome(t.Context(), cfg, "codex", "default", repo, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The second transcript collides with a native file, so the import stops after the first.
	conflict := filepath.Join(home, second)
	writeRepoFile(t, conflict, "native edit\n")
	rt := idleRuntime(t)
	if _, err := PrepareNativeHome(t.Context(), cfg, rt, "codex", "default", repo, false); err == nil {
		t.Fatal("conflicting import succeeded")
	}
	if _, err := os.Stat(filepath.Join(home, first)); err != nil {
		t.Fatal("expected a partial import", err)
	}
	if stamps := nativeHistoryStamps(t, home); len(stamps) != 0 {
		t.Fatalf("partial import stamped complete: %v", stamps)
	}
	if err := os.Remove(conflict); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareNativeHome(t.Context(), cfg, rt, "codex", "default", repo, false); err != nil {
		t.Fatal("unchanged source did not resume its import", err)
	}
	want := mustReadFile(t, filepath.Join(source, second))
	if got := mustReadFile(t, filepath.Join(home, second)); string(got) != string(want) {
		t.Fatalf("resumed import=%q, want %q", got, want)
	}
	if stamps := nativeHistoryStamps(t, home); len(stamps) != 1 {
		t.Fatalf("completed import stamps=%v", stamps)
	}
}

func TestACPThreadBindingMigrationRunsOnlyWhenRetainedStoresChange(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	repo := t.TempDir()
	threads := filepath.Join(cfg.ConfigDir, "acp-threads")
	writeRepoFile(t, filepath.Join(threads, "retained-one.json"), "{}")
	acpHome, err := NativeHomePath(cfg, "codex", "", repo, true)
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	expect := func(step string, want int, fail error) {
		t.Helper()
		err := MigrateACPThreadBindings(t.Context(), cfg, repo, func() error { runs++; return fail })
		if !errors.Is(err, fail) {
			t.Fatalf("%s: err=%v, want %v", step, err, fail)
		}
		if runs != want {
			t.Fatalf("%s: migrations=%d, want %d", step, runs, want)
		}
	}
	expect("first start", 1, nil)
	expect("unchanged stores", 1, nil)
	writeRepoFile(t, filepath.Join(threads, "other-repository", "bindings", "current.json"), "{}")
	expect("another repository's current binding", 1, nil)
	writeRepoFile(t, filepath.Join(acpHome, "sessions", "new-native-session.jsonl"), "{}")
	expect("a new native session in the ACP home", 1, nil)

	writeRepoFile(t, filepath.Join(threads, "retained-two.json"), "{}")
	expect("a new retained binding", 2, nil)
	writeRepoFile(t, filepath.Join(cfg.ConfigDir, "claude", "acp-sessions", "projects", "x", "late.jsonl"), "{}")
	expect("a retained ACP session store change", 3, nil)
	writeRepoFile(t, filepath.Join(filepath.Dir(acpHome), "imports", "receipt.json"), "{}")
	expect("an import into the ACP home", 4, nil)

	writeRepoFile(t, filepath.Join(threads, "retained-three.json"), "{}")
	failed := errors.New("migration interrupted")
	expect("a failed migration", 5, failed)
	expect("retry after the failure", 6, nil)
	expect("after the retry", 6, nil)
}

// Which retained transcripts belong to a project depends on its fork records, which sit outside
// the stamped source: a fork recorded after the project's import reopens it, and its old history
// comes in.
func TestNativeHistoryStampReopensWhenAForkIsRecorded(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: filepath.Join(base, "config"), HomeInBox: "/home/node"}
	repo := filepath.Join(base, "proj")
	ws := forkspace.Workspace(repo, "feat")
	for _, dir := range []string{repo, ws} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	source := cfg.AgentProfileDir("codex", "default")
	rel := writeRetainedRollout(t, source, "01", "11111111-2222-4333-8444-555555555555", ws)
	rt := idleRuntime(t)
	home, err := PrepareNativeHome(t.Context(), cfg, rt, "codex", "default", repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, rel)); err == nil {
		t.Fatal("a fork with no record had its history imported")
	}
	if len(nativeHistoryStamps(t, home)) == 0 {
		t.Fatal("the project's launch left no stamp to test against")
	}
	unlock, err := forkspace.LockState(repo, "feat")
	if err != nil {
		t.Fatal(err)
	}
	_, err = forkspace.EnsureGenerationLocked(repo, "feat")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareNativeHome(t.Context(), cfg, rt, "codex", "default", ws, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, rel)); err != nil {
		t.Fatal("a fork recorded after the stamp never got its retained history", err)
	}
}

// A row appended to a retained index after a stamped launch changes only its size and times, and
// still comes in: indexes merge row by row.
func TestNativeHistoryStampSeesARowAppendedToARetainedIndex(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node"}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := cfg.AgentProfileDir("codex", "default")
	const id = "11111111-2222-4333-8444-555555555555"
	writeRetainedRollout(t, source, "01", id, repo)
	history := filepath.Join(source, "history.jsonl")
	writeRepoFile(t, history, fmt.Sprintf(`{"session_id":%q,"text":"first"}`+"\n", id))
	rt := idleRuntime(t)
	home, err := PrepareNativeHome(t.Context(), cfg, rt, "codex", "default", repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustReadFile(t, filepath.Join(home, "history.jsonl"))), "first") {
		t.Fatal("the first row was not imported")
	}
	f, err := os.OpenFile(history, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, `{"session_id":%q,"text":"later"}`+"\n", id)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareNativeHome(t.Context(), cfg, rt, "codex", "default", repo, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustReadFile(t, filepath.Join(home, "history.jsonl"))), "later") {
		t.Fatal("a row appended to a retained index after the stamp was not imported")
	}
}
