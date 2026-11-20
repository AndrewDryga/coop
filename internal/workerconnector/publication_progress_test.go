package workerconnector

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// The journal keeps an entry until it is older than the retention and the service has settled its
// operation. Old settled entries go; recent ones, unsettled ones, unknown ones and files that do not
// prove their own intent stay, and a failed check stops the pass without removing more.
func TestPrunePublicationProgressRemovesOnlyOldSettledEntries(t *testing.T) {
	host := publicationHost{stateRoot: t.TempDir()}
	now := time.Now()
	old := now.Add(-publicationProgressRetention - time.Hour)
	intent := func(key string) workerproto.PublishIntent {
		return workerproto.PublishIntent{SessionID: "session", ReviewOperationID: "review", JobRef: "job:publish", CommandKey: key,
			Repository: workerproto.RepositoryIdentity{RepositoryRef: "repo:example", GitHubRepository: "example/repo", GitHubRepositoryID: 42},
			Request:    workerproto.PublishRequest{Branch: "coop/" + key, BaseBranch: "main"}}
	}
	write := func(key string, at time.Time) string {
		t.Helper()
		if err := host.savePublicationProgress(intent(key), publicationProgress{Attempted: true}); err != nil {
			t.Fatal(err)
		}
		path, _, err := host.publicationProgressPath(intent(key))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	settledKeys := map[string]bool{"old-1": true, "old-2": true, "old-3": true, "recent": true, "forged": true}
	var asked []string
	settled := func(key string) (bool, error) {
		asked = append(asked, key)
		return settledKeys[key], nil
	}
	gone := []string{write("old-1", old), write("old-2", old), write("old-3", old)}
	kept := []string{write("recent", now.Add(-time.Hour)), write("running", old), write("unknown", old)}
	// A file whose content names a settled key but whose name is another intent's digest proves nothing.
	forged := write("forged", old)
	impostor := filepath.Join(filepath.Dir(forged), "0000000000000000000000000000000000000000000000000000000000000000.json")
	if err := os.Rename(forged, impostor); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(filepath.Dir(forged), ".old-1.json-123")
	if err := os.WriteFile(stray, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(impostor, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stray, old, old); err != nil {
		t.Fatal(err)
	}
	kept = append(kept, impostor, stray)
	// An interrupted atomic write leaves its temporary file; one 30 days old is no live write.
	temporary := func(name string, at time.Time) string {
		t.Helper()
		path := filepath.Join(filepath.Dir(forged), name)
		if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	digest := strings.TrimSuffix(filepath.Base(gone[0]), ".json")
	gone = append(gone, temporary("."+digest+".json-111", old))
	kept = append(kept, temporary("."+digest+".json-222", now.Add(-time.Hour)))

	removed, err := PrunePublicationProgress(host.stateRoot, now, settled)
	if err != nil || removed != len(gone) {
		t.Fatalf("PrunePublicationProgress = %d, %v; want %d removed", removed, err, len(gone))
	}
	for _, path := range gone {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old settled entry %s stayed: %v", path, err)
		}
	}
	for _, path := range kept {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("entry %s was removed: %v", path, err)
		}
	}
	if slices.Contains(asked, "recent") || slices.Contains(asked, "forged") {
		t.Fatalf("a recent or unproven entry reached the settled check: %v", asked)
	}
	// Survivors still replay as before.
	if saved, err := host.readPublicationProgress(intent("running")); err != nil || !saved.Attempted {
		t.Fatalf("the unsettled marker no longer reads: %+v, %v", saved, err)
	}

	// A check that fails stops the pass and keeps the entry it was asking about.
	write("old-4", old)
	failure := errors.New("store unavailable")
	removed, err = PrunePublicationProgress(host.stateRoot, now, func(string) (bool, error) { return true, failure })
	if !errors.Is(err, failure) || removed != 0 {
		t.Fatalf("PrunePublicationProgress with a failing check = %d, %v", removed, err)
	}
	if saved, err := host.readPublicationProgress(intent("old-4")); err != nil || !saved.Attempted {
		t.Fatalf("a failed check removed an entry: %+v, %v", saved, err)
	}

	// No journal folder is nothing to do.
	if removed, err := PrunePublicationProgress(t.TempDir(), now, settled); err != nil || removed != 0 {
		t.Fatalf("PrunePublicationProgress without a journal = %d, %v", removed, err)
	}
}
