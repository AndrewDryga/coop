package sessionsvc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
)

// One retained primary kept sixteen reproducible companion histories alive on
// a worker until its disk filled. Companion GC must preserve unpublished work.
func TestClosedSessionReclaimsCleanCompanionsWithoutDiscardingPrimaryWork(t *testing.T) {
	ctx := context.Background()
	repo := storageTestRepo(t)
	companion := storageTestRepo(t)
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	t.Cleanup(func() { _ = service.Stop() })
	service.companion(t, "context", companion)
	created, err := service.CreateRemoteSession(ctx, "gc-create", service.request(t, "retained-primary"))
	if err != nil {
		t.Fatal(err)
	}
	primaryFile := filepath.Join(created.Workspace, "unpublished.txt")
	if err := os.WriteFile(primaryFile, []byte("keep this work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	closed, err := service.Close(ctx, "gc-close", session.CloseSessionRequest{SessionID: created.ID, ExpectedRevision: created.Revision})
	if err != nil {
		t.Fatal(err)
	}
	reclaimed, err := service.ReclaimStorage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pathExists(created.Companions[0].Workspace) {
		t.Fatalf("closed session still retains its reproducible companion while primary work is unpublished: %+v", reclaimed)
	}
	body, err := os.ReadFile(primaryFile)
	if err != nil || string(body) != "keep this work\n" {
		t.Fatalf("primary work changed: %q, %v", body, err)
	}
	saved, err := service.Store().GetSession(ctx, created.ID)
	if err != nil || saved.State != session.SessionClosed || saved.Revision != closed.Revision || len(saved.Companions) != 1 {
		t.Fatalf("GC changed retained session identity/history: %+v, %v", saved, err)
	}
	if _, err := service.ReclaimStorage(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanDiscard(ctx, "gc-plan", PlanDiscardRequest{SessionID: saved.ID, ExpectedRevision: saved.Revision, AcceptDirty: true, AcceptUnmerged: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Discard(ctx, "gc-discard", DiscardRequest{PlanOperationID: plan.OperationID}); err != nil {
		t.Fatal(err)
	}
}

func companionGCFixture(t *testing.T, count int, closeSession bool) (*Service, session.Session) {
	t.Helper()
	primary, companion := storageTestRepo(t), storageTestRepo(t)
	wrapper := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), primary, nil)
	t.Cleanup(func() { _ = wrapper.Stop() })
	for index := 0; index < count; index++ {
		wrapper.companion(t, fmt.Sprintf("context-%02d", index), companion)
	}
	created, err := wrapper.CreateRemoteSession(context.Background(), "gc-fixture-create", wrapper.request(t, "gc-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if closeSession {
		created, err = wrapper.Close(context.Background(), "gc-fixture-close", session.CloseSessionRequest{SessionID: created.ID, ExpectedRevision: created.Revision})
		if err != nil {
			t.Fatal(err)
		}
	}
	return wrapper.Service, created
}

func TestCompanionGCKeepsActiveUnprovenAndModifiedStorage(t *testing.T) {
	for _, name := range []string{"open", "quarantine", "runtime_cleanup_pending", "dirty", "symlink", "replacement", "linked_metadata"} {
		t.Run(name, func(t *testing.T) {
			service, owner := companionGCFixture(t, 1, name != "open")
			binding := owner.Companions[0]
			switch name {
			case "quarantine":
				service.mu.Lock()
				service.quarantined[owner.ID] = struct{}{}
				service.mu.Unlock()
			case "runtime_cleanup_pending":
				service.runtimeCleanupFailed(owner.ID)
			case "dirty":
				if err := os.WriteFile(filepath.Join(binding.Workspace, "unpublished.txt"), []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink", "replacement":
				old := binding.Workspace + "-original"
				if err := os.Rename(binding.Workspace, old); err != nil {
					t.Fatal(err)
				}
				if name == "symlink" {
					if err := os.Symlink(old, binding.Workspace); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(binding.Workspace, 0o700); err != nil {
					t.Fatal(err)
				}
			case "linked_metadata":
				if err := os.RemoveAll(filepath.Join(binding.Workspace, ".git")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(binding.Workspace, ".git"), []byte("gitdir: /unproven\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := service.ReclaimStorage(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(binding.Workspace); err != nil {
				t.Fatalf("protected %s companion was removed: %v", name, err)
			}
			saved, err := service.Store().GetSession(context.Background(), owner.ID)
			if err != nil || saved.State != owner.State || saved.Revision != owner.Revision {
				t.Fatalf("GC changed session custody: %+v, %v", saved, err)
			}
		})
	}
}

// A protected historical session generated over a thousand reclamation refusals
// in six hours on 2026-11-04. Protection must exclude it from the work budget.
func TestCompanionGCDoesNotRepeatedlySelectProtectedSessions(t *testing.T) {
	for _, protection := range []string{"not_parked", "quarantine", "runtime_cleanup_pending"} {
		t.Run(protection, func(t *testing.T) {
			ctx := context.Background()
			service, owner := companionGCFixture(t, 1, true)
			stored := owner
			binding := owner.Companions[0]
			keep := filepath.Join(owner.Workspace, "unpublished.txt")
			if err := os.WriteFile(keep, []byte("preserve this work"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch protection {
			case "not_parked":
				owner.Activity = session.ActivityRunning
			case "quarantine":
				service.mu.Lock()
				service.quarantined[owner.ID] = struct{}{}
				service.mu.Unlock()
			case "runtime_cleanup_pending":
				service.runtimeCleanupFailed(owner.ID)
			}
			for pass := 0; pass < 2; pass++ {
				var result StorageReclaim
				service.reclaimClosedCompanions(ctx, []session.Session{owner}, &result)
				if len(result.Problems) != 0 || result.CompanionsReclaimed != 0 {
					t.Fatalf("pass %d attempted protected companion reclamation: %+v", pass, result)
				}
				if !pathExists(binding.Workspace) {
					t.Fatal("protected companion was removed")
				}
			}
			if protection != "not_parked" {
				unlock := service.lockSessionRuntime(owner.ID)
				err := service.reclaimClosedCompanion(ctx, owner.ID, binding)
				unlock()
				if err == nil {
					t.Fatal("authoritative cleanup guard admitted a protected session")
				}
			}
			body, err := os.ReadFile(keep)
			if err != nil || string(body) != "preserve this work" {
				t.Fatalf("protected primary work changed: %q, %v", body, err)
			}
			saved, err := service.Store().GetSession(ctx, stored.ID)
			if err != nil || saved.State != stored.State || saved.Revision != stored.Revision {
				t.Fatalf("GC changed retained session history: %+v, %v", saved, err)
			}
		})
	}
}

func TestProtectedCompanionsDoNotSpendTheOnlyReclamationSlot(t *testing.T) {
	ctx := context.Background()
	service, first := companionGCFixture(t, 1, true)
	second, err := service.CreateRemoteSession(ctx, "eligible-create", CreateRemoteSessionRequest{
		Job: first.JobDocument, ExpectedJobDigest: first.JobDigest, Task: "eligible",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err = service.Close(ctx, "eligible-close", session.CloseSessionRequest{SessionID: second.ID, ExpectedRevision: second.Revision})
	if err != nil {
		t.Fatal(err)
	}
	protected, eligible := first, second
	if protected.ID > eligible.ID {
		protected, eligible = eligible, protected
	}
	service.mu.Lock()
	service.quarantined[protected.ID] = struct{}{}
	service.mu.Unlock()
	limits := storageTestLimits(t)
	limits.MaxReclaimPerPass = 1
	mustSetStorageLimits(t, service, limits)
	var result StorageReclaim
	service.reclaimClosedCompanions(ctx, []session.Session{first, second}, &result)
	if result.CompanionsReclaimed != 1 || len(result.Problems) != 0 {
		t.Fatalf("protected session spent the only slot: %+v", result)
	}
	if !pathExists(protected.Companions[0].Workspace) || pathExists(eligible.Companions[0].Workspace) {
		t.Fatal("eligible storage was starved or protected storage was removed")
	}
}

func TestCompanionGCFinishesInterruptedDeletionAndPreservesReplacement(t *testing.T) {
	ctx := context.Background()
	service, owner := companionGCFixture(t, 1, true)
	binding := owner.Companions[0]
	plan, err := planSessionCompanionDiscardContext(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := service.stageCompanionDiscard(ctx, owner, plan)
	if err != nil {
		t.Fatal(err)
	}
	if pathExists(binding.Workspace) {
		t.Fatal("staging did not atomically free the original name")
	}
	if err := os.RemoveAll(filepath.Join(stage, companionDiscardTree, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(binding.Workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(binding.Workspace, "keep.txt")
	if err := os.WriteFile(replacement, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := service.ReclaimStorage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pathExists(stage) || result.CompanionStagesPurged != 1 {
		t.Fatalf("interrupted stage was not reclaimed: %+v", result)
	}
	if body, err := os.ReadFile(replacement); err != nil || string(body) != "replacement" {
		t.Fatalf("GC removed replacement: %q, %v", body, err)
	}
}

func TestCompanionGCResumesOnlyTheExactPreRenameIntent(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacement=%v", replace), func(t *testing.T) {
			ctx := context.Background()
			service, owner := companionGCFixture(t, 1, true)
			binding := owner.Companions[0]
			plan, err := planSessionCompanionDiscardContext(ctx, binding)
			if err != nil {
				t.Fatal(err)
			}
			stage, err := service.stageCompanionDiscard(ctx, owner, plan)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(stage, companionDiscardTree), binding.Workspace); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(stage, companionDiscardStaged)); err != nil {
				t.Fatal(err)
			}
			if replace {
				if err := os.Rename(binding.Workspace, binding.Workspace+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(binding.Workspace, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			result, err := service.ReclaimStorage(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if replace {
				if !pathExists(binding.Workspace) || !pathExists(stage) || len(result.Problems) == 0 {
					t.Fatalf("GC adopted a stale pre-rename intent: %+v", result)
				}
			} else if pathExists(binding.Workspace) || pathExists(stage) {
				t.Fatalf("exact pre-rename intent was not finished: %+v", result)
			}
		})
	}
}

func TestCompanionGCKeepsReceiptFreeAndReplacedStages(t *testing.T) {
	ctx := context.Background()
	service, owner := companionGCFixture(t, 1, true)
	plan, err := planSessionCompanionDiscardContext(ctx, owner.Companions[0])
	if err != nil {
		t.Fatal(err)
	}
	stage, err := service.stageCompanionDiscard(ctx, owner, plan)
	if err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(stage, companionDiscardTree)
	if err := os.Rename(tree, tree+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(filepath.Dir(stage), "companion-unknown", "workspace")
	if err := os.MkdirAll(unknown, 0o700); err != nil {
		t.Fatal(err)
	}
	writeStorageBytes(t, filepath.Join(unknown, "retained.bin"), storageTestPayload)
	result, err := service.ReclaimStorage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	report, err := service.StorageReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Totals.CompanionBytes < storageTestPayload || report.Totals.StagedDiscardBytes != 0 {
		t.Fatalf("unproven companion stages advertised as disposable: %+v", report.Totals)
	}
	if !pathExists(tree) || !pathExists(tree+"-original") || !pathExists(unknown) || len(result.Problems) == 0 {
		t.Fatalf("GC deleted unproven staging: %+v", result)
	}
}

func TestCompanionGCBoundsWorkAndAdvancesPastDirtyCopies(t *testing.T) {
	service, owner := companionGCFixture(t, 2, true)
	limits := storageTestLimits(t)
	limits.MaxReclaimPerPass = 1
	mustSetStorageLimits(t, service, limits)
	first, second := owner.Companions[0].Workspace, owner.Companions[1].Workspace
	if err := os.WriteFile(filepath.Join(first, "keep.txt"), []byte("unpublished"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReclaimStorage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !pathExists(first) || !pathExists(second) {
		t.Fatal("first pass exceeded its companion budget")
	}
	if _, err := service.ReclaimStorage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !pathExists(first) || pathExists(second) {
		t.Fatal("dirty copy starved later reclaimable storage")
	}
}

func TestCompanionGCPreservesTheOriginalPathAfterDeletionCompleted(t *testing.T) {
	ctx := context.Background()
	service, owner := companionGCFixture(t, 1, true)
	binding := owner.Companions[0]
	plan, err := planSessionCompanionDiscardContext(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := service.stageCompanionDiscard(ctx, owner, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(stage, companionDiscardTree)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(binding.Workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(binding.Workspace, "keep")
	if err := os.WriteFile(replacement, []byte("keep replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := service.ReclaimStorage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pathExists(stage) || !pathExists(replacement) || result.CompanionStagesPurged != 1 {
		t.Fatalf("post-delete recovery revisited original path: %+v", result)
	}
}

func TestCompanionGCRejectsReplacementOfThePinnedStage(t *testing.T) {
	ctx := context.Background()
	service, owner := companionGCFixture(t, 1, true)
	plan, err := planSessionCompanionDiscardContext(ctx, owner.Companions[0])
	if err != nil {
		t.Fatal(err)
	}
	stage, err := service.stageCompanionDiscard(ctx, owner, plan)
	if err != nil {
		t.Fatal(err)
	}
	service.testBeforeCompanionPurge = func(path string) {
		service.testBeforeCompanionPurge = nil
		if err := os.Rename(path, path+"-original"); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(path, companionDiscardTree), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, companionDiscardTree, "keep"), []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.purgeCompanionDiscard(ctx, stage); err == nil {
		t.Fatal("GC accepted a different staging root after it pinned the receipt")
	}
	if !pathExists(filepath.Join(stage, companionDiscardTree, "keep")) || !pathExists(filepath.Join(stage+"-original", companionDiscardTree)) {
		t.Fatal("GC deleted a replaced or detached stage")
	}
}

func TestCompanionGCBadStagesDoNotStarveNewClosedCopies(t *testing.T) {
	ctx := context.Background()
	service, owner := companionGCFixture(t, 1, true)
	parent := filepath.Join(service.stateRoot, companionDiscardRoot)
	for i := 0; i < 8; i++ {
		if err := os.MkdirAll(filepath.Join(parent, fmt.Sprintf("companion-unproven-%d", i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	result, err := service.ReclaimStorage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pathExists(owner.Companions[0].Workspace) || result.CompanionsReclaimed != 1 {
		t.Fatalf("unproven old stages starved new reclamation: %+v", result)
	}
}

func TestCompanionGCCancellationAndBusyRuntimePreserveCopies(t *testing.T) {
	service, owner := companionGCFixture(t, 1, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = service.ReclaimStorage(ctx)
	if !pathExists(owner.Companions[0].Workspace) {
		t.Fatal("cancelled GC removed a copy")
	}
	unlock := service.lockSessionRuntime(owner.ID)
	_, err := service.ReclaimStorage(context.Background())
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !pathExists(owner.Companions[0].Workspace) {
		t.Fatal("GC removed a copy while its runtime was held")
	}
}

func TestCreateReclaimsClosedCompanionsBeforeRefusingStoragePressure(t *testing.T) {
	ctx := context.Background()
	service, owner := companionGCFixture(t, 1, true)
	limits := storageTestLimits(t)
	limits.ReserveBytes = 1<<63 - 1
	mustSetStorageLimits(t, service, limits)
	_, err := service.CreateRemoteSession(ctx, "gc-pressured-create", CreateRemoteSessionRequest{Job: owner.JobDocument, ExpectedJobDigest: owner.JobDigest, Task: "pressured"})
	if session.CodeOf(err) != session.CodeStorageUnavailable {
		t.Fatalf("create under pressure = %v, want storage_unavailable", err)
	}
	if pathExists(owner.Companions[0].Workspace) {
		t.Fatal("allocation refused storage before trying to reclaim closed companion copies")
	}
	if !pathExists(owner.Workspace) {
		t.Fatal("pressure cleanup removed the primary workspace")
	}
}

// A later inventory error must not preserve a pre-cleanup pressure refusal.
func TestCompanionGCInvalidatesAccountingEvenWhenLaterInventoryFails(t *testing.T) {
	service, owner := companionGCFixture(t, 1, true)
	service.storage.mu.Lock()
	service.storage.report = &StorageReport{}
	service.storage.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(service.stateRoot, "job-sources", "unproven"), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := service.ReclaimStorage(context.Background())
	if err == nil || result.CompanionsReclaimed != 1 || pathExists(owner.Companions[0].Workspace) {
		t.Fatalf("expected successful companion cleanup followed by an inventory error: %+v, %v", result, err)
	}
	service.storage.mu.Lock()
	cached := service.storage.report
	service.storage.mu.Unlock()
	if cached != nil {
		t.Fatal("successful cleanup left the pre-cleanup storage report cached")
	}
}
