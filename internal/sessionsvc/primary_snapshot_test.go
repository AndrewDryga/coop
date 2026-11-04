package sessionsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
)

// Repeated read-only investigations still copied the primary's entire history
// after companion sharing shipped, filling the same worker volume again.
func TestReadOnlyPrimariesSharePayloadAcrossFreshnessReceipts(t *testing.T) {
	ctx := context.Background()
	repo := storageTestRepo(t)
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	t.Cleanup(func() { _ = svc.Stop() })
	svc.Job.RepositoryReadOnly = true
	first, err := svc.CreateRemoteSession(ctx, "first", svc.request(t, "first"))
	if err != nil {
		t.Fatal(err)
	}
	svc.Job.Source.Binding.ResolvedAt = svc.Job.Source.Binding.ResolvedAt.Add(time.Second)
	stageTestJobBinding(t, svc.stateRoot, repo, *svc.Job.Source)
	second, err := svc.CreateRemoteSession(ctx, "second", svc.request(t, "second"))
	if err != nil {
		t.Fatal(err)
	}
	a, b := first.Workspace, second.Workspace
	one, err := os.Stat(filepath.Join(a, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	two, err := os.Stat(filepath.Join(b, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(one, two) {
		t.Fatal("same authorized revision stores duplicate read-only primary payloads")
	}
	for _, name := range []string{"index", "config", "HEAD"} {
		one, err := os.Stat(filepath.Join(a, ".git", name))
		if err != nil {
			t.Fatal(err)
		}
		two, err := os.Stat(filepath.Join(b, ".git", name))
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(one, two) {
			t.Fatalf("mutable primary Git metadata %s is shared", name)
		}
	}
	if _, err := svc.GetChanges(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	svc.Job.RepositoryReadOnly = false
	writable, err := svc.CreateRemoteSession(ctx, "writable", svc.request(t, "writable"))
	if err != nil {
		t.Fatal(err)
	}
	private, err := os.Stat(filepath.Join(writable.Workspace, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(one, private) {
		t.Fatal("writable primary borrowed immutable payload")
	}
}

func TestReadOnlyPrimaryOwnsDivergedDefaultHistory(t *testing.T) {
	ctx := context.Background()
	repo := storageTestRepo(t)
	base := gitOut(repo, "rev-parse", "HEAD")
	sessionWorkspaceGit(t, repo, "checkout", "-qb", "selected")
	sessionWorkspaceWrite(t, filepath.Join(repo, "selected.txt"), "selected\n")
	sessionWorkspaceGit(t, repo, "add", ".")
	sessionWorkspaceGit(t, repo, "commit", "-qm", "selected")
	selected := gitOut(repo, "rev-parse", "HEAD")
	sessionWorkspaceGit(t, repo, "checkout", "-q", "main")
	sessionWorkspaceWrite(t, filepath.Join(repo, "default.txt"), "default\n")
	sessionWorkspaceGit(t, repo, "add", ".")
	sessionWorkspaceGit(t, repo, "commit", "-qm", "default")
	current := gitOut(repo, "rev-parse", "HEAD")
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	t.Cleanup(func() { _ = svc.Stop() })
	svc.pullSource(t, repo, 7, selected, current, base)
	svc.Job.RepositoryReadOnly = true
	first, err := svc.CreateRemoteSession(ctx, "diverged", svc.request(t, "diverged"))
	if err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string]string{"HEAD": selected, "origin/main": current, "origin/HEAD": current, base: base} {
		if got := gitOut(first.Workspace, "rev-parse", ref); got != want {
			t.Fatalf("%s = %s, want %s", ref, got, want)
		}
	}
	changes, err := svc.GetChanges(ctx, first.ID)
	if err != nil || changes.AdmittedSourceTree != svc.Job.Source.Binding.AdmittedTree {
		t.Fatalf("admitted selected history lost: %+v, %v", changes, err)
	}
	if err := svc.validateRestoreWorkspaceSession(ctx, first, RestoreWorkspaceCheckpointRequest{ExpectedRevision: first.Revision}, false); err == nil {
		t.Fatal("shared primary accepted restore")
	}
	if err := svc.validateCheckpointSession(ctx, first, first.Revision); err == nil {
		t.Fatal("shared primary accepted checkpoint")
	}
}

func TestReadOnlyPrimarySharesNestedLFSButKeepsPrivateMetadata(t *testing.T) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte("hydrated content\n"), 1000)
	repo := nestedSessionSource(t, payload)
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	t.Cleanup(func() { _ = svc.Stop() })
	svc.Job.RepositoryReadOnly = true
	first, err := svc.CreateRemoteSession(ctx, "nested-first", svc.request(t, "nested-first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CreateRemoteSession(ctx, "nested-second", svc.request(t, "nested-second"))
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range []string{"", "vendor/child", "vendor/child/nested space"} {
		one := filepath.Join(first.Workspace, child)
		two := filepath.Join(second.Workspace, child)
		for _, name := range []string{"asset.bin", ".git/config", ".git/index"} {
			a, err := os.Stat(filepath.Join(one, name))
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.Stat(filepath.Join(two, name))
			if err != nil {
				t.Fatal(err)
			}
			if os.SameFile(a, b) != (name == "asset.bin") {
				t.Fatalf("incorrect sharing for %s/%s", child, name)
			}
		}
		got, err := os.ReadFile(filepath.Join(one, "asset.bin"))
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("LFS not hydrated: %v", err)
		}
	}
	if _, err := svc.CreateRemoteSession(ctx, "nested-first", svc.request(t, "nested-first")); err != nil {
		t.Fatalf("readonly retry: %v", err)
	}
	if _, err := svc.GetChanges(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
}

func TestReadOnlyPrimarySeparatesRepositoryAndControllerAuthority(t *testing.T) {
	ctx := context.Background()
	repo := storageTestRepo(t)
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	t.Cleanup(func() { _ = svc.Stop() })
	svc.Job.RepositoryReadOnly = true
	var prior []os.FileInfo
	for i, identity := range []string{
		`{"controller_url":"https://one.example","worker_id":"worker","workspace_ref":"a"}`,
		`{"controller_url":"https://two.example","worker_id":"worker","workspace_ref":"a"}`,
		`{"controller_url":"https://two.example","worker_id":"worker","workspace_ref":"b"}`,
		`{"controller_url":"https://two.example","worker_id":"worker","workspace_ref":"b"}`,
	} {
		if i == 3 {
			svc.Job.Source.RepositoryRef = "test:other-grant"
			stageTestJobBinding(t, svc.stateRoot, repo, *svc.Job.Source)
		}
		if err := os.WriteFile(filepath.Join(svc.stateRoot, "identity.json"), []byte(identity), 0600); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("scope-%d", i)
		owner, err := svc.CreateRemoteSession(ctx, id, svc.request(t, id))
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(owner.Workspace, "payload.bin"))
		if err != nil {
			t.Fatal(err)
		}
		for _, other := range prior {
			if os.SameFile(other, info) {
				t.Fatal("primary reused another authenticated source authority")
			}
		}
		source, err := os.Stat(filepath.Join(owner.Repository, "payload.bin"))
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(source, info) {
			t.Fatal("primary shares mutable staged source")
		}
		prior = append(prior, info)
	}
}

func TestReadOnlyPrimaryConcurrentCreationKeepsOneImmutablePayload(t *testing.T) {
	ctx := context.Background()
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), storageTestRepo(t), nil)
	t.Cleanup(func() { _ = svc.Stop() })
	svc.Job.RepositoryReadOnly = true
	type result struct {
		owner session.Session
		err   error
	}
	results := make(chan result, 2)
	for _, id := range []string{"concurrent-one", "concurrent-two"} {
		req := svc.request(t, id)
		go func() { owner, err := svc.CreateRemoteSession(ctx, id, req); results <- result{owner, err} }()
	}
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent create: %v %v", first.err, second.err)
	}
	a, err := os.Stat(filepath.Join(first.owner.Workspace, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(filepath.Join(second.owner.Workspace, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Fatal("concurrent sessions duplicated payload")
	}
}

func TestReadOnlyPrimaryRetryPreservesPublishedGenerationAndRejectsChangedReceipt(t *testing.T) {
	ctx := context.Background()
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), storageTestRepo(t), nil)
	t.Cleanup(func() { _ = svc.Stop() })
	p := &primarySnapshotSource{svc.stateRoot, *svc.Job.Source}
	repo, err := stagedJobRepository(svc.stateRoot, *svc.Job.Source)
	if err != nil {
		t.Fatal(err)
	}
	prior := reserveSessionWorkspaceLocked
	failure := errors.New("reservation persistence interrupted")
	reserveSessionWorkspaceLocked = func(string, forkspace.WorkspaceReservation) error { return failure }
	t.Cleanup(func() { reserveSessionWorkspaceLocked = prior })
	_, err = ensureSessionWorkspaceForJobContext(ctx, nil, repo, "shared-retry", p.source.Binding.SelectedCommit, testSessionStoreID, "owner", p)
	if !errors.Is(err, errSessionWorkspacePublicationPending) {
		t.Fatalf("publication did not remain retryable: %v", err)
	}
	workspace := forkspace.Workspace(repo, "shared-retry")
	before, err := os.Stat(filepath.Join(workspace, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	reserveSessionWorkspaceLocked = prior
	recovered, err := ensureSessionWorkspaceForJobContext(ctx, nil, repo, "shared-retry", p.source.Binding.SelectedCommit, testSessionStoreID, "owner", p)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(recovered.Path, "payload.bin"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("recovery replaced payload: %v", err)
	}
	if _, err = ensureSessionWorkspaceForJobContext(ctx, nil, repo, "shared-retry", p.source.Binding.SelectedCommit, testSessionStoreID, "other", p); err == nil {
		t.Fatal("foreign session adopted projection")
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git", primarySnapshotMarker), []byte(`{"version":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ensureSessionWorkspaceForJobContext(ctx, nil, repo, "shared-retry", p.source.Binding.SelectedCommit, testSessionStoreID, "owner", p); err == nil {
		t.Fatal("unknown projection receipt accepted")
	}
	check, err := CheckPrimarySnapshots(svc.stateRoot)
	if err != nil || check.Newer != 1 {
		t.Fatalf("unknown receipt inventory: %+v %v", check, err)
	}
	if !pathExists(workspace) {
		t.Fatal("unknown projection removed")
	}
}

func TestUnreservedImmutablePrimaryCannotBeAdoptedByNativeFork(t *testing.T) {
	ctx := context.Background()
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "custom-worker-state"), storageTestRepo(t), nil)
	t.Cleanup(func() { _ = svc.Stop() })
	p := &primarySnapshotSource{svc.stateRoot, *svc.Job.Source}
	repo, err := stagedJobRepository(svc.stateRoot, *svc.Job.Source)
	if err != nil {
		t.Fatal(err)
	}
	name := "review-crash-gap"
	workspace := forkspace.Workspace(repo, name)
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	// Crash after projection publication but before generation/reservation creation.
	if err = createReadOnlyPrimarySnapshot(ctx, p, repo, name, workspace); err != nil {
		t.Fatal(err)
	}
	payload, err := os.Stat(filepath.Join(workspace, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if payload.Mode().Perm()&0222 != 0 {
		t.Fatal("not an immutable payload")
	}
	// This is the native fork command's bind/require-no-reservation sequence.
	identity, err := forkspace.EnsureGenerationLocked(repo, name)
	if err == nil {
		err = forkspace.RequireNoWorkspaceReservationLocked(repo, identity)
	}
	if err == nil {
		t.Error("native fork adopted unreserved immutable primary after publication crash")
	}
}

func TestUnknownPrimaryReceiptSurvivesDiscard(t *testing.T) {
	ctx := context.Background()
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), storageTestRepo(t), nil)
	t.Cleanup(func() { _ = svc.Stop() })
	svc.Job.RepositoryReadOnly = true
	owner, err := svc.CreateRemoteSession(ctx, "create", svc.request(t, "unknown-discard"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owner.Workspace, ".git", primarySnapshotMarker), []byte(`{"version":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	closed, err := svc.Close(ctx, "close", session.CloseSessionRequest{SessionID: owner.ID, ExpectedRevision: owner.Revision})
	if err != nil {
		return
	} // Preservation through refusal is safe.
	plan, err := svc.PlanDiscard(ctx, "plan", PlanDiscardRequest{SessionID: closed.ID, ExpectedRevision: closed.Revision})
	if err != nil {
		return
	}
	_, err = svc.Discard(ctx, "discard", DiscardRequest{PlanOperationID: plan.OperationID})
	if err == nil {
		t.Fatal("unknown primary receipt was discarded")
	}
	if _, err := os.Stat(owner.Workspace); err != nil {
		t.Fatalf("unknown primary disappeared: %v", err)
	}
}

func TestPrimaryPhysicalAccountingAndDiscardKeepLivePayload(t *testing.T) {
	ctx := context.Background()
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), storageTestRepo(t), nil)
	t.Cleanup(func() { _ = svc.Stop() })
	svc.Job.RepositoryReadOnly = true
	first, err := svc.CreateRemoteSession(ctx, "first", svc.request(t, "first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CreateRemoteSession(ctx, "second", svc.request(t, "second"))
	if err != nil {
		t.Fatal(err)
	}
	usage, err := forkspace.NewUsageScan().Measure(first.Workspace)
	if err != nil || usage.SharedBytes == 0 {
		t.Fatalf("primary payload not shared: %+v %v", usage, err)
	}
	marker, err := os.Stat(filepath.Join(first.Workspace, forkspace.GenerationMarkerName))
	if err != nil {
		t.Fatal(err)
	}
	anchorBytes := int64(marker.Sys().(*syscall.Stat_t).Blocks) * 512
	payloadBytes := usage.SharedBytes - anchorBytes
	if payloadBytes < storageTestPayload {
		t.Fatalf("shared payload accounting is too small: %d", payloadBytes)
	}
	measure := func(payload int64, owners int) {
		// Each fork also has its own generation anchor linked into private
		// lifecycle state. Those independent identities each count once.
		want := payload + int64(owners)*anchorBytes
		t.Helper()
		report, err := svc.measureStorage(ctx)
		if err != nil || report.Totals.Unknown || report.Totals.BaselineSharedBytes != want {
			t.Fatalf("primary accounting = %+v %v, want exactly %d shared bytes", report.Totals, err, want)
		}
		if report.Totals.reclaimable() != 0 {
			t.Fatalf("live shared bytes promised reclaimable: %+v", report.Totals)
		}
	}
	measure(payloadBytes, 2)
	count, problems := pruneCompanionSnapshots(ctx, svc.stateRoot, time.Now().Add(2*time.Hour), 1, 4, true)
	if count != 0 || len(problems) != 0 {
		t.Fatalf("live primary cache evicted: %d %v", count, problems)
	}
	// Simulate external cache eviction: kernel references must retain every byte,
	// and telemetry may not count the unlink as storage recovered.
	if err := os.RemoveAll(filepath.Join(svc.stateRoot, companionSnapshotRoot)); err != nil {
		t.Fatal(err)
	}
	measure(payloadBytes, 2)
	discard := func(owner session.Session, id string) {
		t.Helper()
		closed, err := svc.Close(ctx, "close-"+id, session.CloseSessionRequest{SessionID: owner.ID, ExpectedRevision: owner.Revision})
		if err != nil {
			t.Fatal(err)
		}
		plan, err := svc.PlanDiscard(ctx, "plan-"+id, PlanDiscardRequest{SessionID: owner.ID, ExpectedRevision: closed.Revision})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = svc.Discard(ctx, "discard-"+id, DiscardRequest{PlanOperationID: plan.OperationID}); err != nil {
			t.Fatal(err)
		}
		if _, err = svc.ReclaimStorage(ctx); err != nil {
			t.Fatal(err)
		}
		if pathExists(owner.Workspace) {
			t.Fatal("authorized primary discard retained workspace")
		}
	}
	discard(first, "first")
	measure(0, 1)
	if _, err := svc.GetChanges(ctx, second.ID); err != nil {
		t.Fatalf("sibling discard broke live primary: %v", err)
	}
	discard(second, "second")
}

func TestPrimaryLastDiscardReleasesCacheAndOldIndependentForksStayIndependent(t *testing.T) {
	ctx := context.Background()
	repo := storageTestRepo(t)
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	t.Cleanup(func() { _ = svc.Stop() })
	p := &primarySnapshotSource{svc.stateRoot, *svc.Job.Source}
	staged, err := stagedJobRepository(svc.stateRoot, *svc.Job.Source)
	if err != nil {
		t.Fatal(err)
	}
	old, err := ensureSessionWorkspaceContext(ctx, nil, staged, "historical", p.source.Binding.SelectedCommit, testSessionStoreID, "old")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(old.Path, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ensureSessionWorkspaceForJobContext(ctx, nil, staged, "historical", p.source.Binding.SelectedCommit, testSessionStoreID, "old", p); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(old.Path, "payload.bin"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("old readonly copy was converted")
	}
	if pathExists(filepath.Join(old.Path, ".git", primarySnapshotMarker)) {
		t.Fatal("old copy gained false sharing provenance")
	}
	sessionWorkspaceWrite(t, filepath.Join(old.Path, "keep.txt"), "operator history\n")
	if _, err = ensureSessionWorkspaceForJobContext(ctx, nil, staged, "historical", p.source.Binding.SelectedCommit, testSessionStoreID, "old", p); err == nil {
		t.Fatal("dirty historical fork adopted")
	}
	if data, err := os.ReadFile(filepath.Join(old.Path, "keep.txt")); err != nil || string(data) != "operator history\n" {
		t.Fatal("historical data changed")
	}
	svc.Job.RepositoryReadOnly = true
	owner, err := svc.CreateRemoteSession(ctx, "shared", svc.request(t, "shared"))
	if err != nil {
		t.Fatal(err)
	}
	check, err := CheckPrimarySnapshots(svc.stateRoot)
	if err != nil || check.Current != 1 || len(check.Damaged) != 0 {
		t.Fatalf("primary upgrade inventory: %+v %v", check, err)
	}
	closed, err := svc.Close(ctx, "close", session.CloseSessionRequest{SessionID: owner.ID, ExpectedRevision: owner.Revision})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := svc.PlanDiscard(ctx, "plan", PlanDiscardRequest{SessionID: owner.ID, ExpectedRevision: closed.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Discard(ctx, "discard", DiscardRequest{PlanOperationID: plan.OperationID}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.ReclaimStorage(ctx); err != nil {
		t.Fatal(err)
	}
	count, problems := pruneCompanionSnapshots(ctx, svc.stateRoot, time.Now().Add(2*time.Hour), 1, 4, true)
	if count != 1 || len(problems) != 0 {
		t.Fatalf("last primary reference kept cache: %d %v", count, problems)
	}
	if !pathExists(filepath.Join(old.Path, "keep.txt")) {
		t.Fatal("cache cleanup lost old dirty data")
	}
}

func TestMissingPrimaryReceiptCannotHydrateSharedPayload(t *testing.T) {
	ctx := context.Background()
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), storageTestRepo(t), nil)
	t.Cleanup(func() { _ = svc.Stop() })
	p := &primarySnapshotSource{svc.stateRoot, *svc.Job.Source}
	repo, err := stagedJobRepository(svc.stateRoot, *svc.Job.Source)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := ensureSessionWorkspaceForJobContext(ctx, nil, repo, "missing-marker", p.source.Binding.SelectedCommit, testSessionStoreID, "owner", p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(owner.Path, ".git", primarySnapshotMarker)); err != nil {
		t.Fatal(err)
	}
	if _, err = ensureSessionWorkspaceForJobContext(ctx, nil, repo, "missing-marker", p.source.Binding.SelectedCommit, testSessionStoreID, "owner", p); err == nil || !strings.Contains(err.Error(), "unproven shared primary") {
		t.Fatalf("lost receipt became mutable history: %v", err)
	}
	if !pathExists(filepath.Join(owner.Path, "payload.bin")) {
		t.Fatal("lost receipt deleted payload")
	}
}

func TestPrimaryUpgradeCheckRejectsNonprivateCurrentReceipt(t *testing.T) {
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), storageTestRepo(t), nil)
	t.Cleanup(func() { _ = svc.Stop() })
	svc.Job.RepositoryReadOnly = true
	owner, err := svc.CreateRemoteSession(t.Context(), "check", svc.request(t, "check"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(owner.Workspace, ".git", primarySnapshotMarker), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := forkspace.ReadImmutablePrimarySnapshot(owner.Workspace); err == nil {
		t.Fatal("runtime accepted nonprivate receipt")
	}
	check, err := CheckPrimarySnapshots(svc.stateRoot)
	if err != nil || check.Current != 0 || len(check.Damaged) != 1 {
		t.Fatalf("upgrade check accepted runtime-refused receipt: %+v %v", check, err)
	}
}

func TestPrimaryUpgradeCheckIncludesStagedReceipts(t *testing.T) {
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), storageTestRepo(t), nil)
	t.Cleanup(func() { _ = svc.Stop() })
	svc.Job.RepositoryReadOnly = true
	owner, err := svc.CreateRemoteSession(t.Context(), "check-staged", svc.request(t, "check-staged"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(owner.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(owner.Repository, owner.ForkName)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := forkspace.StageWorkspaceDiscardLocked(owner.Repository, owner.ForkName, info)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, ".git", primarySnapshotMarker), []byte(`{"version":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	check, err := CheckPrimarySnapshots(svc.stateRoot)
	if err != nil || check.Newer != 1 {
		t.Fatalf("upgrade inventory omitted staged unknown receipt: %+v %v", check, err)
	}
}

func TestNativeRemovalPreservesUnreservedPrimary(t *testing.T) {
	ctx := context.Background()
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "custom-worker-state"), storageTestRepo(t), nil)
	t.Cleanup(func() { _ = svc.Stop() })
	p := &primarySnapshotSource{svc.stateRoot, *svc.Job.Source}
	repo, err := stagedJobRepository(svc.stateRoot, *svc.Job.Source)
	if err != nil {
		t.Fatal(err)
	}
	name := "native-remove-gap"
	workspace := forkspace.Workspace(repo, name)
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err = createReadOnlyPrimarySnapshot(ctx, p, repo, name, workspace); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(workspace, ".git", primarySnapshotMarker), []byte(`{"version":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	// The native rm path uses name availability (not RequireNoWorkspaceReservation).
	if err = forkspace.RequireForkNameAvailable(repo, name); err != nil {
		return
	}
	if err = forkspace.Destroy(repo, name); err == nil {
		t.Fatal("native rm deleted an unreserved primary with unknown receipt")
	}
	if _, err = os.Stat(workspace); err != nil {
		t.Fatalf("unknown primary disappeared: %v", err)
	}
}

// A copied read-only projection kept valid generation anchors but an old inode
// receipt. Eight cleanup attempts then stopped at a hidden internal_error.
func TestCopiedReadOnlyPrimaryQuarantinesAndRetainsCompletedHistory(t *testing.T) {
	ctx := t.Context()
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), storageTestRepo(t), nil)
	t.Cleanup(func() { _ = svc.Stop() })
	svc.Job.RepositoryReadOnly = true
	bound, err := svc.CreateRemoteSession(ctx, "copied-primary", svc.request(t, "copied-primary"))
	if err != nil {
		t.Fatal(err)
	}
	turn, err := svc.Store().SubmitTurn(ctx, "completed-read", session.SubmitTurnRequest{SessionID: bound.ID, ExpectedRevision: bound.Revision, Prompt: "Read the frozen source."})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc.Store().LeaseNextTurn(ctx, bound.ID); err != nil || !ok {
		t.Fatalf("lease: %v %v", ok, err)
	}
	if _, err := svc.Store().MarkTurnSendIntent(ctx, bound.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store().MarkTurnSent(ctx, bound.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	completed, err := svc.Store().CompleteTurn(ctx, session.CompleteTurnRequest{SessionID: bound.ID, TurnID: turn.ID, Message: "Saved completed answer."})
	if err != nil {
		t.Fatal(err)
	}
	bound = mustSession(t, svc, bound.ID)
	bound, err = svc.Close(ctx, "close-copied-primary", session.CloseSessionRequest{SessionID: bound.ID, ExpectedRevision: bound.Revision})
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(bound.Workspace, ".git", primarySnapshotMarker)
	receipt, present, err := forkspace.ReadImmutablePrimarySnapshot(bound.Workspace)
	if err != nil || !present {
		t.Fatalf("valid primary receipt: %v %v", present, err)
	}
	// The exact mismatch observed after copying a Docker volume; no generation
	// marker or reservation is forged or adopted by the recovery.
	receipt.Directory.Inode++
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, raw, 0600); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(bound.Workspace, "payload.bin")
	before, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := svc.CreateRemoteSession(ctx, "healthy-readonly", svc.request(t, "healthy-readonly"))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !svc.sessionQuarantined(bound.ID) {
		t.Fatal("copied primary stayed outside quarantine and would strand cleanup")
	}
	if svc.sessionQuarantined(healthy.ID) {
		t.Fatal("fresh read-only primary was quarantined")
	}
	if _, err := svc.GetChanges(ctx, healthy.ID); err != nil {
		t.Fatalf("healthy primary unusable: %v", err)
	}
	checked, err := CheckPrimarySnapshots(svc.stateRoot)
	if err != nil || checked.Current != 1 || checked.Held != 1 || len(checked.Damaged) != 0 {
		t.Fatalf("known copied primary blocks upgrade: %+v %v", checked, err)
	}
	current := mustSession(t, svc, bound.ID)
	if _, err := svc.PlanDiscard(ctx, "copied-primary-plan", PlanDiscardRequest{SessionID: bound.ID, ExpectedRevision: current.Revision}); session.CodeOf(err) != session.CodeInvalidSessionState {
		t.Fatalf("quarantined files became discardable: %v", err)
	}
	req := DiscardRequest{RetireQuarantined: true, SessionID: bound.ID, ExpectedRevision: current.Revision}
	stale := req
	stale.ExpectedRevision++
	if _, err := svc.Discard(ctx, "copied-primary-stale", stale); session.CodeOf(err) != session.CodeRevisionConflict {
		t.Fatalf("stale retirement: %v", err)
	}
	for range 2 {
		retired, err := svc.Discard(ctx, "copied-primary-retire", req)
		if err != nil || retired.State != session.SessionDiscarded {
			t.Fatalf("explicit record retirement: %+v %v", retired, err)
		}
		if got, err := os.ReadFile(marker); err != nil || !bytes.Equal(got, raw) {
			t.Fatal("retirement rewrote receipt")
		}
		if got, err := os.ReadFile(payload); err != nil || !bytes.Equal(got, before) {
			t.Fatal("retirement changed source content")
		}
		got, err := svc.Store().GetTurn(ctx, bound.ID, completed.ID)
		if err != nil || got.State != completed.State || got.AssistantMessage != completed.AssistantMessage {
			t.Fatalf("completed history lost: %+v %v", got, err)
		}
	}
}

func TestPrimaryQuarantineDoesNotInterpretUnsupportedOwnership(t *testing.T) {
	for _, kind := range []string{"newer", "malformed", "nonprivate", "wrong-key"} {
		t.Run(kind, func(t *testing.T) {
			svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), storageTestRepo(t), nil)
			t.Cleanup(func() { _ = svc.Stop() })
			svc.Job.RepositoryReadOnly = true
			bound, err := svc.CreateRemoteSession(t.Context(), "unsupported-primary", svc.request(t, "unsupported-primary"))
			if err != nil {
				t.Fatal(err)
			}
			receipt, present, err := forkspace.ReadImmutablePrimarySnapshot(bound.Workspace)
			if err != nil || !present {
				t.Fatalf("initial receipt: %v %v", present, err)
			}
			if kind == "newer" {
				receipt.Version++
			}
			if kind == "wrong-key" {
				receipt.Key = "invalid"
			}
			raw, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "malformed" {
				raw = []byte("{")
			}
			marker := filepath.Join(bound.Workspace, ".git", primarySnapshotMarker)
			if err := os.WriteFile(marker, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "nonprivate" {
				if err := os.Chmod(marker, 0644); err != nil {
					t.Fatal(err)
				}
			}
			check, err := CheckPrimarySnapshots(svc.stateRoot)
			if err != nil || check.Held != 0 || check.Current != 0 {
				t.Fatalf("unsupported ownership was accepted: %+v %v", check, err)
			}
			if kind == "newer" {
				if check.Newer != 1 {
					t.Fatalf("newer receipt: %+v", check)
				}
			} else if len(check.Damaged) != 1 {
				t.Fatalf("damaged receipt: %+v", check)
			}
			if err := svc.Start(t.Context()); err == nil {
				t.Fatal("unsupported primary silently started")
			}
			if svc.sessionQuarantined(bound.ID) {
				t.Fatal("unsupported format interpreted as a known copied primary")
			}
			if got, err := os.ReadFile(marker); err != nil || !bytes.Equal(got, raw) {
				t.Fatal("unsupported ownership was rewritten")
			}
			if _, err := os.Stat(filepath.Join(bound.Workspace, "payload.bin")); err != nil {
				t.Fatal("unsupported data removed")
			}
		})
	}
}
