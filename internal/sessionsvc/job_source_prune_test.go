package sessionsvc

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// On a Ryker tenant install (2026-10-13) every Work session staged its environment's 17
// repositories again, and no copy was ever removed: about 3 GB an hour, until the volume was hours
// from the line where the worker stops taking work. Removing a copy a session may still need is
// the opposite failure: a task's next session stages its frozen source again only while the
// branch has not moved.
func TestPruningRemovesOnlyStagedSourcesNoSessionCanStillUse(t *testing.T) {
	ctx := context.Background()
	upstream := storageTestRepo(t)
	service := storageTestService(t, upstream)
	root := filepath.Join(service.stateRoot, "job-sources")
	base := stageTestJobSource(t, service.stateRoot, upstream)
	now := time.Now()
	unused := now.Add(-2 * time.Hour)

	staged := map[string]string{}
	other := base
	other.RepositoryRef, other.GitHubRepository, other.GitHubRepositoryID = "test:other", "example/other", 18
	stageFrom := func(from workerproto.JobSource, name string, resolved int64, changed time.Time) string {
		t.Helper()
		source := from
		source.Binding.ResolvedAt = time.Unix(resolved, 0).UTC()
		stageTestJobBinding(t, service.stateRoot, upstream, source)
		key, err := source.StagingKey()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(root, key), changed, changed); err != nil {
			t.Fatal(err)
		}
		staged[name] = key
		return key
	}
	stage := func(name string, resolved int64, changed time.Time) string {
		t.Helper()
		return stageFrom(base, name, resolved, changed)
	}
	repository := func(key string) string { return filepath.Join(root, key, "repository") }

	baseKey, err := base.StagingKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, baseKey), unused, unused); err != nil {
		t.Fatal(err)
	}
	staged["unused"] = baseKey
	// A copy's fork records live in Coop's private state, keyed by the copy's own path, and go
	// with the copy.
	if err := forkspace.EnsureStateDir(repository(baseKey)); err != nil {
		t.Fatal(err)
	}
	unusedRecords := forkspace.StateDir(repository(baseKey))
	// The repository's newest copy is the one staged ten minutes ago, so this one is not.
	stage("older", 2, now.Add(-90*time.Minute))
	stage("open", 3, unused)
	stage("companion", 4, unused)
	stage("discarded", 5, unused)
	stage("task", 6, unused)
	stage("old-task", 7, unused)
	stage("recent", 8, now.Add(-10*time.Minute))
	// Staged within the hour but not the newest: a create may be about to fork it.
	stage("staging", 13, now.Add(-30*time.Minute))
	// Another repository's newest copy, unused for two hours, is still its next staging's start.
	stageFrom(other, "other-newest", 14, unused)
	stageFrom(other, "other-older", 15, now.Add(-3*time.Hour))
	forked := stage("forked", 9, unused)
	if err := os.MkdirAll(filepath.Join(forkspace.Home(repository(forked)), "remote-0123456789abcdef"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{forkspace.Home(repository(forked)), filepath.Join(root, forked)} {
		if err := os.Chtimes(path, unused, unused); err != nil {
			t.Fatal(err)
		}
	}
	// A copy whose forks folder holds only the records an older build kept there holds no fork.
	legacy := stage("legacy-records", 12, unused)
	legacyRecords := filepath.Join(forkspace.Home(repository(legacy)), ".coop", "executions")
	if err := os.MkdirAll(legacyRecords, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyRecords, "run.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{forkspace.Home(repository(legacy)), filepath.Join(root, legacy)} {
		if err := os.Chtimes(path, unused, unused); err != nil {
			t.Fatal(err)
		}
	}
	// An old .coop that stayed where it was, as one whose anchor could not cross disks does, holds
	// no fork either.
	stayed := stage("stayed-records", 16, unused)
	if err := forkspace.EnsureStateDir(repository(stayed)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(forkspace.Home(repository(stayed)), ".coop"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{forkspace.Home(repository(stayed)), filepath.Join(root, stayed)} {
		if err := os.Chtimes(path, unused, unused); err != nil {
			t.Fatal(err)
		}
	}
	firstDownload := base.RepositoryIdentity().FirstDownloadKey()
	if err := os.Rename(filepath.Join(root, stage("first-download", 10, unused)), filepath.Join(root, firstDownload)); err != nil {
		t.Fatal(err)
	}
	staged["first-download"] = firstDownload
	unreadable := stage("unreadable", 11, unused)
	if err := os.WriteFile(filepath.Join(root, unreadable, "source.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, unreadable), unused, unused); err != nil {
		t.Fatal(err)
	}

	task := &session.WorkspaceTaskBinding{QueueID: "queue", TaskID: "task", ID: "task", OfferRef: "offer"}
	sessions := []session.Session{
		{ID: "open", State: session.SessionOpen, Repository: repository(staged["open"]), UpdatedAt: unused,
			Companions: []session.CompanionRepository{{Name: "library", Repository: repository(staged["companion"])}}},
		{ID: "discarded", State: session.SessionDiscarded, Repository: repository(staged["discarded"]), UpdatedAt: unused},
		{ID: "task", State: session.SessionDiscarded, Repository: repository(staged["task"]), WorkspaceTask: task,
			UpdatedAt: now.Add(-24 * time.Hour)},
		{ID: "old-task", State: session.SessionDiscarded, Repository: repository(staged["old-task"]), WorkspaceTask: task,
			UpdatedAt: now.Add(-31 * 24 * time.Hour)},
	}

	removed, problems := service.pruneJobSources(ctx, sessions, now)
	if len(problems) != 0 {
		t.Fatalf("pruning problems: %v", problems)
	}
	var gone []string
	for name, key := range staged {
		if _, err := os.Lstat(filepath.Join(root, key)); os.IsNotExist(err) {
			gone = append(gone, name)
		}
	}
	slices.Sort(gone)
	if want := []string{"discarded", "legacy-records", "old-task", "older", "other-older", "stayed-records", "unused"}; !slices.Equal(gone, want) || removed != len(want) {
		t.Fatalf("removed %d: %v, want %v", removed, gone, want)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), jobSourcePrunePrefix) {
			t.Fatalf("a removed copy left %s behind", entry.Name())
		}
	}
	for _, records := range []string{unusedRecords, forkspace.StateDir(repository(legacy))} {
		if entries, err := os.ReadDir(records); len(entries) != 0 {
			t.Fatalf("a removed copy left its fork records behind in %s: %v %v", records, entries, err)
		}
	}
}

// The worker's maintenance pass runs the cleanup on the sessions its store holds, and the
// worker's storage accounting still reads what remains.
func TestStorageMaintenanceRemovesStagedSourcesNoSessionUses(t *testing.T) {
	ctx := context.Background()
	upstream := storageTestRepo(t)
	service := storageTestService(t, upstream)
	root := filepath.Join(service.stateRoot, "job-sources")
	unusedSource := stageTestJobSource(t, service.stateRoot, upstream)
	newestSource := unusedSource
	newestSource.Binding.ResolvedAt = time.Unix(2, 0).UTC()
	stageTestJobBinding(t, service.stateRoot, upstream, newestSource)
	unusedKey, _ := unusedSource.StagingKey()
	newestKey, _ := newestSource.StagingKey()
	for key, age := range map[string]time.Duration{unusedKey: 3 * time.Hour, newestKey: 2 * time.Hour} {
		at := time.Now().Add(-age)
		if err := os.Chtimes(filepath.Join(root, key), at, at); err != nil {
			t.Fatal(err)
		}
	}

	result, err := service.ReclaimStorage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.SourcesPruned != 1 || len(result.Problems) != 0 {
		t.Fatalf("maintenance removed %d staged sources (%v), want 1", result.SourcesPruned, result.Problems)
	}
	if _, err := os.Lstat(filepath.Join(root, unusedKey)); !os.IsNotExist(err) {
		t.Fatalf("maintenance kept an unused copy: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, newestKey)); err != nil {
		t.Fatalf("maintenance removed the newest copy, the next staging's head start: %v", err)
	}
	if _, err := service.StorageReport(ctx); err != nil {
		t.Fatalf("storage report after the cleanup: %v", err)
	}
}
