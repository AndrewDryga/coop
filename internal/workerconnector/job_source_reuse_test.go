package workerconnector

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// A fresh resolution of the same revision used to materialize the whole source
// tree again. Its Git packs shared bytes, but every checkout and LFS payload grew.
func TestFreshSourceReceiptsReuseImmutableWorkingTreeBytes(t *testing.T) {
	ctx := context.Background()
	remote, source := backgroundStagingFixture(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	stager := &privateJobSourceStager{transport: &jobSourceGrantFixture{}, stateRoot: root, remoteForTest: remote}
	if err := stager.Stage(ctx, "first", source); err != nil {
		t.Fatal(err)
	}
	first, _ := source.StagingKey()
	next := source
	next.Binding.ResolvedAt = source.Binding.ResolvedAt.Add(time.Minute)
	if err := stager.Stage(ctx, "second", next); err != nil {
		t.Fatal(err)
	}
	second, _ := next.StagingKey()
	a, b := filepath.Join(root, "job-sources", first, "repository"), filepath.Join(root, "job-sources", second, "repository")
	entries, err := os.ReadDir(a)
	if err != nil {
		t.Fatal(err)
	}
	checked := false
	for _, entry := range entries {
		if entry.Name() == ".git" || !entry.Type().IsRegular() {
			continue
		}
		one, err := os.Stat(filepath.Join(a, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		two, err := os.Stat(filepath.Join(b, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(one, two) {
			t.Fatalf("identical pinned source payload %q occupies duplicate storage", entry.Name())
		}
		checked = true
	}
	if !checked {
		t.Fatal("fixture did not exercise a source payload")
	}
	if err := verifyStagedJobSource(filepath.Dir(b), next); err != nil {
		t.Fatal(err)
	}
	if err := verifyStagedJobSource(filepath.Dir(a), source); err != nil {
		t.Fatal(err)
	}
	one, _ := os.Stat(filepath.Join(a, ".git", "config"))
	two, _ := os.Stat(filepath.Join(b, ".git", "config"))
	if os.SameFile(one, two) {
		t.Fatal("mutable source Git configuration is shared")
	}
}

func TestSourceStoragePressureWaitsBeforeFetchingAndKeepsVerifiedHits(t *testing.T) {
	ctx := context.Background()
	remote, source := backgroundStagingFixture(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	grants := &jobSourceGrantFixture{}
	stager := &privateJobSourceStager{transport: grants, stateRoot: root, remoteForTest: remote}
	if err := stager.Stage(ctx, "existing", source); err != nil {
		t.Fatal(err)
	}
	calls := grants.calls
	stager.storageForTest = func(string) (forkspace.Filesystem, error) {
		return forkspace.Filesystem{CapacityBytes: 100 << 30, FreeBytes: 1 << 30}, nil
	}
	if err := stager.Stage(ctx, "existing-hit", source); err != nil {
		t.Fatalf("verified hit refused under pressure: %v", err)
	}
	next := source
	next.Binding.ResolvedAt = source.Binding.ResolvedAt.Add(time.Minute)
	if err := stager.Stage(ctx, "new", next); !errors.Is(err, errJobSourceStorage) {
		t.Fatalf("new source did not wait for storage: %v", err)
	}
	if grants.calls != calls {
		t.Fatal("storage refusal acquired a source grant or started downloading")
	}
	key, _ := next.StagingKey()
	if _, err := os.Stat(filepath.Join(root, "job-sources", key)); !os.IsNotExist(err) {
		t.Fatal("low-space source published staged bytes")
	}
	executor := &Executor{jobSourceStager: stager}
	_, _, err := executor.stageJobSources(ctx, "new", []workerproto.JobSource{next})
	if !errors.Is(err, errArtifactTransfer) || !errors.Is(err, errJobSourceStorage) || errors.Is(err, ErrRequestRejected) {
		t.Fatalf("storage wait lost temporary command custody: %v", err)
	}
	var waits firstDownloadWaits
	now := time.Now()
	waits.settled("command", err, now)
	if !waits.held("command", now) {
		t.Fatal("storage wait retries every poll")
	}
	stager.storageForTest = nil
	if err := stager.Stage(ctx, "new", next); err != nil {
		t.Fatalf("source did not recover after capacity returned: %v", err)
	}
}

// Capacity waits must survive worker restart without spending the create's
// idempotency key or reporting a terminal result to the controller.
func TestStorageWaitingCommandResumesAfterRestartUnderSameOperationKey(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	journal := t.TempDir()
	command := createWithJobSource(t)
	command.LeaseExpiresAt = now.Add(time.Minute)
	api := &fakeAPI{response: json.RawMessage(`{"operation":{"id":"operation-after-storage","method":"CreateRemoteSession","state":"running"}}`)}
	waiting := &storageWaitingStager{}
	config := ExecutorConfig{API: api, JobSourceStager: waiting, JournalDir: journal, Now: func() time.Time { return now }, WorkerID: "worker-a"}
	first, err := NewExecutor(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = first.Execute(ctx, command); !errors.Is(err, errJobSourceStorage) {
		t.Fatalf("capacity wait became a result: %v", err)
	}
	pending, err := first.journal.pending()
	if err != nil || len(pending) != 1 || pending[0].Result != nil || len(api.requests) != 0 {
		t.Fatalf("waiting command custody: %+v %v requests=%d", pending, err, len(api.requests))
	}
	config.JobSourceStager = &recordingJobSourceStager{}
	resumed, err := NewExecutor(config)
	if err != nil {
		t.Fatal(err)
	}
	result, err := resumed.Execute(ctx, command)
	if err != nil || result.State != "succeeded" || len(api.requests) != 1 || api.requests[0].IdempotencyKey != command.IdempotencyKey {
		t.Fatalf("resumed command: %+v %v requests=%+v", result, err, api.requests)
	}
	if _, err = resumed.Execute(ctx, command); err != nil || len(api.requests) != 1 {
		t.Fatal("redelivery duplicated resumed create", err)
	}
}

type storageWaitingStager struct{}

func (*storageWaitingStager) Stage(context.Context, string, workerproto.JobSource) error {
	return errJobSourceStorage
}

func TestOptionalSourceReuseSurvivesSeedGCBeforeLink(t *testing.T) {
	ctx := context.Background()
	remote, source := backgroundStagingFixture(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	stager := &privateJobSourceStager{transport: &jobSourceGrantFixture{}, stateRoot: root, remoteForTest: remote}
	if err = stager.Stage(ctx, "one", source); err != nil {
		t.Fatal(err)
	}
	first, _ := source.StagingKey()
	seed := filepath.Join(root, "job-sources", first, "repository")
	// A fresh independent verified source exists before optional deduplication.
	next := source
	next.Binding.ResolvedAt = source.Binding.ResolvedAt.Add(time.Minute)
	temporary, err := os.MkdirTemp(filepath.Join(root, "job-sources"), ".source-race-")
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(temporary, "repository")
	command := exec.Command("git", "clone", "--quiet", "--no-local", remote, destination)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fixture clone: %v %s", err, output)
	}
	if err = os.Chmod(destination, 0700); err != nil {
		t.Fatal(err)
	}
	if err = verifySourceRepository(ctx, destination, next); err != nil {
		t.Fatal(err)
	}
	if err = reuseStagedSourcePayloadsWithLink(ctx, root, seed, destination, next, func(string, string) error { return os.ErrPermission }); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("optional reuse swallowed a non-disappearance I/O failure: %v", err)
	}
	removed := false
	err = reuseStagedSourcePayloadsWithLink(ctx, root, seed, destination, next, func(from, to string) error {
		removed = true
		if err := os.RemoveAll(filepath.Dir(seed)); err != nil {
			t.Fatal(err)
		}
		return os.Link(from, to)
	})
	if !removed {
		t.Fatal("fixture did not reach the deduplication race")
	}
	if err != nil {
		t.Fatalf("optional seed GC rejected already verified source: %v", err)
	}
	if err = verifySourceRepository(ctx, destination, next); err != nil {
		t.Fatalf("seed loss damaged independent destination: %v", err)
	}
}
