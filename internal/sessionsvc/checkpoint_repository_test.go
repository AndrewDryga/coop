package sessionsvc

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func readCheckpointBundle(t *testing.T, service *Service, operationID string) ([]byte, error) {
	t.Helper()
	file, _, err := service.OpenWorkspaceCheckpointBundle(context.Background(), operationID)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func TestLargeCheckpointRestoresGitHistoryAndLFSPayloadWithoutBufferingBundle(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repository, git := gitrepo.New(t)
	sessionWorkspaceWrite(t, filepath.Join(repository, ".gitignore"), ".agent/tasks/\n")
	sessionWorkspaceWrite(t, filepath.Join(repository, "tracked.txt"), "base\n")
	git("add", ".")
	git("commit", "-qm", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repository, nil)
	defer service.Stop()
	ctx := context.Background()
	source, err := service.CreateRemoteSession(ctx, "large-source", service.request(t, "offer:large"))
	if err != nil {
		t.Fatal(err)
	}
	source, err = service.EnsureWorkspaceTask(ctx, "large-task", EnsureWorkspaceTaskRequest{
		SessionID: source.ID, ExpectedRevision: source.Revision,
		Task: tasks.ControllerTaskDraft{OfferRef: "offer:large", Title: "Preserve large work", Prompt: "Keep Git and LFS content.", SuccessChecks: []string{"exact restore"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Two copies, one in Git and one in LFS, each just over half the 64 MiB a v1 bundle was read
	// into memory under: the smallest bundle that proves a v2 one streams past it.
	unit := []byte("large\x00binary\n")
	payload := bytes.Repeat(unit, workerproto.MaxWorkspaceCheckpointBundleBytes/2/len(unit)+1)
	writeSessionLFSSource(t, source.Workspace, payload)
	if err := os.WriteFile(filepath.Join(source.Workspace, "large.data"), payload, 0644); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, source.Workspace, "add", ".")
	sessionWorkspaceGit(t, source.Workspace, "commit", "-qm", "large Git and LFS work")
	sessionWorkspaceWrite(t, filepath.Join(source.Workspace, "tracked.txt"), "dirty\x00binary\n")
	captured, err := service.CheckpointWorkspace(ctx, "large-capture", CheckpointWorkspaceRequest{
		SessionID: source.ID, SessionRef: "source", ExpectedRevision: source.Revision, PlacementGeneration: 1, RepositoryRef: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured.Checkpoint.Bundle.ByteSize < int64(2*len(payload)) {
		t.Fatal("checkpoint did not retain both large payloads")
	}
	if captured.Checkpoint.Bundle.ByteSize <= workerproto.MaxWorkspaceCheckpointBundleBytes {
		t.Fatal("the bundle is not past the v1 cap it proves a stream outgrows")
	}
	body, _, err := service.OpenWorkspaceCheckpointBundle(ctx, captured.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	target, err := service.CreateRemoteSession(ctx, "large-target", service.request(t, "offer:large"))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := service.RestoreWorkspaceCheckpoint(ctx, "large-restore", RestoreWorkspaceCheckpointRequest{
		SessionID: target.ID, ExpectedRevision: target.Revision, Checkpoint: captured.Checkpoint, Stream: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"large.data", "asset.bin"} {
		file, err := os.Open(filepath.Join(restored.Workspace, path))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.New()
		size, err := io.Copy(hash, file)
		_ = file.Close()
		if err != nil || size != int64(len(payload)) || !bytes.Equal(hash.Sum(nil), mustDigest(payload)) {
			t.Fatalf("incomplete large %s: %d bytes, %v", path, size, err)
		}
	}
	if got := gitOut(restored.Workspace, "rev-parse", "HEAD"); got != captured.Checkpoint.CommittedRevision {
		t.Fatal("large restore lost commit identity")
	}
}

func mustDigest(data []byte) []byte {
	digest := sha256.Sum256(data)
	return digest[:]
}

// Coop's runtime janitor holds a parked session's runtime while it checks it. A restore that arrived
// meanwhile was refused as if a turn were running: on 2026-10-11 a Ryker task follow-up stopped for a
// person on "restore requires a parked runtime" one second after its new session was created, and
// only a person's retry, with a whole new session, got past it. A runtime still held once the short
// wait is over is refused as before (TestSubmitTurnDoesNotWaitForARunningTurn holds that a session
// running a turn is refused at once).
func TestRestoreWaitsOutTheJanitorsCheckOfAParkedSession(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repository, git := gitrepo.New(t)
	sessionWorkspaceWrite(t, filepath.Join(repository, ".gitignore"), ".agent/tasks/\n")
	sessionWorkspaceWrite(t, filepath.Join(repository, "tracked.txt"), "base\n")
	git("add", ".")
	git("commit", "-qm", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repository, nil)
	defer service.Stop()
	ctx := context.Background()
	source, err := service.CreateRemoteSession(ctx, "janitor-source", service.request(t, "offer:janitor"))
	if err != nil {
		t.Fatal(err)
	}
	source, err = service.EnsureWorkspaceTask(ctx, "janitor-task", EnsureWorkspaceTaskRequest{
		SessionID: source.ID, ExpectedRevision: source.Revision,
		Task: tasks.ControllerTaskDraft{OfferRef: "offer:janitor", Title: "Follow up", Prompt: "Keep the earlier work.", SuccessChecks: []string{"restored"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(source.Workspace, "tracked.txt"), "earlier work\n")
	captured, err := service.CheckpointWorkspace(ctx, "janitor-capture", CheckpointWorkspaceRequest{
		SessionID: source.ID, SessionRef: "source", ExpectedRevision: source.Revision, PlacementGeneration: 1, RepositoryRef: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := readCheckpointBundle(t, service.Service, captured.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	target, err := service.CreateRemoteSession(ctx, "janitor-target", service.request(t, "offer:janitor"))
	if err != nil {
		t.Fatal(err)
	}
	restore := func(key string) (session.Session, error) {
		return service.RestoreWorkspaceCheckpoint(ctx, key, RestoreWorkspaceCheckpointRequest{
			SessionID: target.ID, ExpectedRevision: target.Revision, Checkpoint: captured.Checkpoint, Stream: bytes.NewReader(bundle),
		})
	}

	service.parkedRuntimeWait = 100 * time.Millisecond
	turn := service.lockSessionRuntime(target.ID)
	_, err = restore("restore-during-turn")
	turn()
	if err == nil || !strings.Contains(err.Error(), "restore requires a parked runtime") {
		t.Fatalf("a restore under a runtime held past the wait = %v, want the parked-runtime refusal", err)
	}

	service.parkedRuntimeWait = 10 * time.Second
	janitor := service.lockSessionRuntime(target.ID)
	time.AfterFunc(300*time.Millisecond, janitor)
	restored, err := restore("restore-during-janitor")
	if err != nil {
		t.Fatalf("a restore during the janitor's check of a parked session was refused: %v", err)
	}
	if restored.WorkspaceTask == nil || *restored.WorkspaceTask != *source.WorkspaceTask {
		t.Fatalf("restored session = %+v, source task = %+v", restored, source.WorkspaceTask)
	}
	if got, err := os.ReadFile(filepath.Join(restored.Workspace, "tracked.txt")); err != nil || string(got) != "earlier work\n" {
		t.Fatalf("restored work = %q, %v", got, err)
	}
}

func TestCheckpointDiskRefusalCancellationAndOwnedStageCleanup(t *testing.T) {
	ctx := context.Background()
	repository, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repository, nil)
	defer service.Stop()
	if _, _, err := service.checkpointDiskContext(ctx, repository, workerproto.MaxWorkspaceCheckpointStreamBytes); err == nil {
		t.Fatal("accepted a checkpoint exceeding available disk")
	}
	pressure := errors.New("test disk pressure")
	guarded, stop := watchCheckpointDisk(ctx, func() error { return pressure })
	defer stop()
	select {
	case <-guarded.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("disk watcher did not cancel")
	}
	if !errors.Is(context.Cause(guarded), pressure) {
		t.Fatal("lost disk pressure cause")
	}
	op, _, err := service.Store().ReserveOperation(ctx, "RestoreWorkspaceCheckpoint", "abandoned-stage", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Store().MarkOperationRunning(ctx, op.ID, []byte(`{"session_id":"held"}`)); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(service.stateRoot, "workspace-checkpoints", ".staging")
	stage := filepath.Join(root, op.ID+"-12345")
	if err := os.MkdirAll(stage, 0700); err != nil {
		t.Fatal(err)
	}
	result := StorageReclaim{}
	limits := StorageLimits{MaxReclaimPerPass: 4}
	unlock := service.lockOperation(op.IdempotencyKey)
	service.reclaimOperationStages(ctx, "workspace-checkpoints", limits, &result)
	unlock()
	if _, err := os.Stat(stage); err != nil {
		t.Fatal("removed an active checkpoint stage")
	}
	service.reclaimOperationStages(ctx, "workspace-checkpoints", limits, &result)
	if _, err := os.Stat(stage); !os.IsNotExist(err) || result.StagedPurged != 1 {
		t.Fatalf("did not reclaim an abandoned stage: %v, %+v", err, result)
	}
	if op, err := service.Store().GetOperationByID(ctx, op.ID); err != nil || op.State != session.OperationRunning {
		t.Fatal("stage cleanup released the durable recovery fence")
	}
}

func TestCheckpointRepositoryKeepsDeletedHistoryLFSAndDirtyObjects(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repository, git := gitrepo.New(t)
	sessionWorkspaceWrite(t, filepath.Join(repository, "file"), "base\n")
	git("add", ".")
	git("commit", "-qm", "base")
	base := gitOut(repository, "rev-parse", "HEAD")
	payload := []byte("historical\x00LFS\n")
	writeSessionLFSSource(t, repository, payload)
	git("add", ".")
	git("commit", "-qm", "add LFS")
	historical := gitOut(repository, "rev-parse", "HEAD")
	git("rm", "asset.bin")
	git("commit", "-qm", "remove LFS")
	head := gitOut(repository, "rev-parse", "HEAD")
	sessionWorkspaceWrite(t, filepath.Join(repository, "file"), "dirty\x00content\n")
	before, err := os.ReadFile(filepath.Join(repository, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = withSessionTrackedTree(context.Background(), repository, func(tree string, env []string) error {
		return writeCheckpointRepository(context.Background(), repository, base, head, tree, env, &output)
	})
	if err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(filepath.Join(repository, ".git", "index")); err != nil || !bytes.Equal(before, after) {
		t.Fatal("checkpoint modified model index")
	}
	reader := tar.NewReader(&output)
	seen := map[string][]byte{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[header.Name], err = io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
	}
	if seen["objects/commit/"+historical] == nil || seen["objects/commit/"+head] == nil || seen["objects/commit/"+base] != nil {
		t.Fatal("checkpoint lost new history or included authorized baseline history")
	}
	if !bytes.Equal(seen[fmt.Sprintf("lfs/%x", sha256.Sum256(payload))], payload) {
		t.Fatal("checkpoint lost an LFS payload deleted from HEAD")
	}
	found := false
	for name, body := range seen {
		found = found || strings.HasPrefix(name, "objects/blob/") && string(body) == "dirty\x00content\n"
	}
	if !found {
		t.Fatal("checkpoint lost uncommitted binary content")
	}
}
