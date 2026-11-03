package sessionsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
)

// Each investigation copied every read-only repository and its history. Repeated
// sessions exhausted the worker disk even though they read exactly the same bytes.
func TestCompanionsSharePinnedBytesAcrossFreshnessReceipts(t *testing.T) {
	ctx := context.Background()
	repo, companion := storageTestRepo(t), storageTestRepo(t)
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	t.Cleanup(func() { _ = svc.Stop() })
	svc.companion(t, "context", companion)
	first, err := svc.CreateRemoteSession(ctx, "first", svc.request(t, "first"))
	if err != nil {
		t.Fatal(err)
	}
	svc.Job.Companions[0].Source.Binding.ResolvedAt = svc.Job.Companions[0].Source.Binding.ResolvedAt.Add(time.Second)
	stageTestJobBinding(t, svc.stateRoot, companion, svc.Job.Companions[0].Source)
	second, err := svc.CreateRemoteSession(ctx, "second", svc.request(t, "second"))
	if err != nil {
		t.Fatal(err)
	}
	a, b := first.Companions[0].Workspace, second.Companions[0].Workspace
	one, err := os.Stat(filepath.Join(a, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	two, err := os.Stat(filepath.Join(b, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(one, two) {
		t.Fatal("same authorized revision stores duplicate read-only payloads")
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
			t.Fatalf("mutable Git metadata %s is shared", name)
		}
	}
	_, err = svc.Close(ctx, "close-first", session.CloseSessionRequest{SessionID: first.ID, ExpectedRevision: first.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.ReclaimStorage(ctx); err != nil {
		t.Fatal(err)
	}
	if pathExists(a) {
		t.Fatal("closed companion was retained")
	}
	if err = verifySessionCompanionContext(ctx, second.Companions[0]); err != nil {
		t.Fatalf("closing sibling broke active companion: %v", err)
	}
}

func TestSnapshotEvictionKeepsLiveProjectionsAndReclaimsAfterLastClose(t *testing.T) {
	ctx := context.Background()
	service, owner := companionGCFixture(t, 1, false)
	snapshots := filepath.Join(service.stateRoot, companionSnapshotRoot)
	entries, err := os.ReadDir(snapshots)
	if err != nil || len(entries) != 1 {
		t.Fatalf("snapshot inventory: %v %v", entries, err)
	}
	count, problems := pruneCompanionSnapshots(ctx, service.stateRoot, time.Now(), 1, 4, true)
	if count != 0 || len(problems) != 0 {
		t.Fatalf("evicted active snapshot: %d %v", count, problems)
	}
	// Session-local Git configuration must never alter a sibling or cached tree.
	config := filepath.Join(owner.Companions[0].Workspace, ".git", "config")
	old, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(config, append(old, []byte("\n[coop]\n test = local\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	cachedConfig := filepath.Join(snapshots, entries[0].Name(), "tree", ".git", "config")
	data, err := os.ReadFile(cachedConfig)
	if err != nil || string(data) != string(old) {
		t.Fatalf("cache Git config changed: %v", err)
	}
	if err = os.WriteFile(config, old, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = service.Close(ctx, "snapshot-close", session.CloseSessionRequest{SessionID: owner.ID, ExpectedRevision: owner.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.ReclaimStorage(ctx); err != nil {
		t.Fatal(err)
	}
	count, problems = pruneCompanionSnapshots(ctx, service.stateRoot, time.Now().Add(2*time.Hour), 1, 4, true)
	if count != 1 || len(problems) != 0 {
		t.Fatalf("unreferenced snapshot retained: %d %v", count, problems)
	}
}

func TestSnapshotRefusesReplacedReceiptAndRetainsUnknownData(t *testing.T) {
	ctx := context.Background()
	service, owner := companionGCFixture(t, 1, false)
	parent := filepath.Join(service.stateRoot, companionSnapshotRoot)
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 {
		t.Fatal(err)
	}
	entry := filepath.Join(parent, entries[0].Name())
	if err = os.WriteFile(filepath.Join(entry, "receipt.json"), []byte(`{"version":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	count, problems := pruneCompanionSnapshots(ctx, service.stateRoot, time.Now().Add(2*time.Hour), 1, 4, true)
	if count != 0 || len(problems) == 0 {
		t.Fatalf("unproven cache reclaimed: %d %v", count, problems)
	}
	if _, err = os.Stat(filepath.Join(owner.Companions[0].Workspace, "payload.bin")); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotNeverLinksMutableSourceOrSharesAcrossRepositoryIdentities(t *testing.T) {
	ctx := context.Background()
	upstream := storageTestRepo(t)
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), storageTestRepo(t), nil)
	t.Cleanup(func() { _ = svc.Stop() })
	svc.companion(t, "one", upstream)
	svc.companion(t, "two", upstream)
	// The same upstream and commit, granted through another tenant's binding.
	svc.Job.Companions[1].Source.GitHubRepository = svc.Job.Companions[0].Source.GitHubRepository
	svc.Job.Companions[1].Source.GitHubRepositoryID = svc.Job.Companions[0].Source.GitHubRepositoryID
	stageTestJobBinding(t, svc.stateRoot, upstream, svc.Job.Companions[1].Source)
	owner, err := svc.CreateRemoteSession(ctx, "separate-identities", svc.request(t, "separate-identities"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := os.Stat(filepath.Join(owner.Companions[0].Workspace, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(filepath.Join(owner.Companions[1].Workspace, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Stat(filepath.Join(owner.Companions[0].Repository, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(a, b) || os.SameFile(a, source) {
		t.Fatal("snapshot shared outside its immutable authorized content identity")
	}
	if a.Mode().Perm()&0222 != 0 {
		t.Fatal("shared payload remains writable")
	}
}

func TestSnapshotIdentityChangesWithControllerOrWorkspace(t *testing.T) {
	ctx := context.Background()
	repo := storageTestRepo(t)
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), storageTestRepo(t), nil)
	t.Cleanup(func() { _ = svc.Stop() })
	svc.companion(t, "context", repo)
	var earlier []os.FileInfo
	for index, identity := range []string{
		`{"controller_url":"https://one.example","worker_id":"worker","workspace_ref":"a"}`,
		`{"controller_url":"https://two.example","worker_id":"worker","workspace_ref":"a"}`,
		`{"controller_url":"https://two.example","worker_id":"worker","workspace_ref":"b"}`,
	} {
		if err := os.WriteFile(filepath.Join(svc.stateRoot, "identity.json"), []byte(identity), 0600); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("scope-%d", index)
		owner, err := svc.CreateRemoteSession(ctx, id, svc.request(t, id))
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(owner.Companions[0].Workspace, "payload.bin"))
		if err != nil {
			t.Fatal(err)
		}
		for _, previous := range earlier {
			if os.SameFile(previous, info) {
				t.Fatal("re-enrolled worker reused another controller/workspace snapshot")
			}
		}
		earlier = append(earlier, info)
	}
}

// Interrupted eviction must not leave a canonical key blocking every following
// investigation. Unknown nonempty directories still retain their custody.
func TestSnapshotRetirementRecoversAfterReceiptWasRemoved(t *testing.T) {
	ctx := context.Background()
	svc, _ := companionGCFixture(t, 1, false)
	parent := filepath.Join(svc.stateRoot, companionSnapshotRoot)
	empty := filepath.Join(parent, ".snapshot-retired-"+fmt.Sprintf("%064x", 1)+"-12345")
	unknown := filepath.Join(parent, ".snapshot-retired-"+fmt.Sprintf("%064x", 2)+"-12345")
	for _, path := range []string{empty, unknown} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(unknown, "unknown"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _ = pruneCompanionSnapshots(ctx, svc.stateRoot, time.Now().Add(2*time.Hour), 1, 4, true)
	if pathExists(empty) {
		t.Fatal("receipt-removal crash left an empty retired directory permanently")
	}
	if !pathExists(filepath.Join(unknown, "unknown")) {
		t.Fatal("recovery removed unproven data")
	}
}

func TestSnapshotStorageCountsSharedPayloadOnceAndNeverPromisesHeldBytes(t *testing.T) {
	// Controlled trees isolate the payload's actual allocated blocks from Git
	// metadata. Exercise the exact private-state report used by worker telemetry.
	root := t.TempDir()
	cache := filepath.Join(root, companionSnapshotRoot, "cache")
	first := filepath.Join(root, "repositories", "first")
	second := filepath.Join(root, "repositories", "second")
	for _, directory := range []string{cache, first, second} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	payload := filepath.Join(cache, "payload")
	writeStorageBytes(t, payload, storageTestPayload)
	for _, directory := range []string{first, second} {
		if err := os.Link(payload, filepath.Join(directory, "payload")); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(payload)
	if err != nil {
		t.Fatal(err)
	}
	allocated := int64(info.Sys().(*syscall.Stat_t).Blocks) * 512
	if allocated <= 0 {
		t.Fatal("fixture must consume physical blocks")
	}
	service := &Service{stateRoot: root}
	measure := func(wantShared int64) StorageReport {
		t.Helper()
		var report StorageReport
		service.measurePrivateState(forkspace.NewUsageScan(), &report)
		if report.Totals.Unknown || report.Totals.BaselineSharedBytes != wantShared {
			t.Fatalf("shared physical storage = %d, want %d; unknown=%v", report.Totals.BaselineSharedBytes, wantShared, report.Totals.Unknown)
		}
		if report.Totals.reclaimable() != 0 {
			t.Fatalf("live/cache storage advertised reclaimable: %+v", report.Totals)
		}
		return report
	}
	measure(allocated)
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	measure(allocated) // Removing cache links recovers none of the held payload.
	if err := os.RemoveAll(first); err != nil {
		t.Fatal(err)
	}
	report := measure(0)
	if report.Totals.CompanionBytes < allocated {
		t.Fatalf("last projection's exclusive payload disappeared from accounting: %+v", report.Totals)
	}
	if err := os.RemoveAll(second); err != nil {
		t.Fatal(err)
	}
	after := measure(0)
	if report.Totals.CompanionBytes-after.Totals.CompanionBytes < allocated {
		t.Fatal("last projection removal did not release its payload")
	}
}

func TestSnapshotRetirementResumesPartialDeletionWithoutBlockingReuse(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			ctx := context.Background()
			service, owner := companionGCFixture(t, 1, true)
			if _, err := service.ReclaimStorage(ctx); err != nil {
				t.Fatal(err)
			}
			canonical, err := filepath.EvalSymlinks(service.stateRoot)
			if err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join(canonical, companionSnapshotRoot)
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 1 {
				t.Fatalf("snapshot inventory %v %v", entries, err)
			}
			entry := filepath.Join(parent, entries[0].Name())
			receipt, err := readCompanionSnapshotReceipt(entry)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(entry, "deleting"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err = syncSnapshotDirectory(entry); err != nil {
				t.Fatal(err)
			}
			retired, err := retireCompanionSnapshot(entry, receipt.Key)
			if err != nil {
				t.Fatal(err)
			}
			if partial {
				if err = os.RemoveAll(filepath.Join(retired, "tree")); err != nil {
					t.Fatal(err)
				}
			}
			binding := owner.Companions[0]
			projectionParent, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			binding.Workspace = filepath.Join(projectionParent, "projection")
			if err = createCachedSessionCompanion(ctx, service.stateRoot, binding); err != nil {
				t.Fatalf("retiring old cache blocked fresh same-key request: %v", err)
			}
			removed, problems := pruneCompanionSnapshots(ctx, service.stateRoot, time.Now().Add(2*time.Hour), 1, 4, true)
			if removed != 1 || len(problems) != 0 || pathExists(retired) {
				t.Fatalf("retirement did not resume: %d %v", removed, problems)
			}
			if err = verifySessionCompanionContext(ctx, binding); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSnapshotConcurrentProjectionKeepsOneCacheAndIndependentPaths(t *testing.T) {
	ctx := context.Background()
	service, owner := companionGCFixture(t, 1, false)
	var group sync.WaitGroup
	failures := make(chan error, 2)
	paths := []string{filepath.Join(t.TempDir(), "one"), filepath.Join(t.TempDir(), "two")}
	for _, path := range paths {
		group.Add(1)
		go func(path string) {
			defer group.Done()
			binding := owner.Companions[0]
			binding.Workspace = path
			failures <- createCachedSessionCompanion(ctx, service.stateRoot, binding)
		}(path)
	}
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	a, err := os.Stat(filepath.Join(paths[0], "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(filepath.Join(paths[1], "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Fatal("concurrent projections duplicated pinned payload")
	}
	checked, err := CheckCompanionSnapshots(service.stateRoot)
	if err != nil || checked.Current != 1 || len(checked.Damaged) != 0 {
		t.Fatalf("concurrent cache inventory: %+v %v", checked, err)
	}
}

func TestSnapshotUpgradeCheckIsReadOnlyAndRejectsNewerOwnership(t *testing.T) {
	service, _ := companionGCFixture(t, 1, false)
	checked, err := CheckCompanionSnapshots(service.stateRoot)
	if err != nil || checked.Current != 1 || checked.Version != companionSnapshotVersion || len(checked.Damaged) != 0 {
		t.Fatalf("current cache check: %+v %v", checked, err)
	}
	root, err := filepath.EvalSymlinks(service.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, companionSnapshotRoot))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, companionSnapshotRoot, entries[0].Name(), "receipt.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt companionSnapshotReceipt
	if err = json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	receipt.Version++
	data, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	checked, err = CheckCompanionSnapshots(service.stateRoot)
	if err != nil || checked.Newer != 1 || checked.Current != 0 || len(checked.Damaged) != 0 {
		t.Fatalf("newer cache check: %+v %v", checked, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatal("read-only upgrade check changed receipt", err)
	}
}

func TestSnapshotSweepReclaimsBeforeLaterInventoryExhaustsDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, owner := companionGCFixture(t, 1, true)
	if _, err := service.ReclaimStorage(ctx); err != nil {
		t.Fatal(err)
	}
	// Two distinct independently owned snapshots; the second inventory visit
	// models a slow filesystem exhausting the pass after one reclaimable entry.
	root, err := filepath.EvalSymlinks(service.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, companionSnapshotRoot))
	if err != nil || len(entries) != 1 {
		t.Fatal(err)
	}
	first := filepath.Join(root, companionSnapshotRoot, entries[0].Name())
	key := fmt.Sprintf("%064x", 4)
	second := filepath.Join(root, companionSnapshotRoot, key)
	if filepath.Base(first) == key {
		t.Fatal("fixture key collision")
	}
	if err = buildCompanionSnapshot(ctx, second, key, owner.Companions[0]); err != nil {
		t.Fatal(err)
	}
	visits := 0
	sweep := &companionSnapshotSweep{beforeCandidate: func(string) {
		visits++
		if visits == 2 {
			cancel()
		}
	}}
	removed, problems := pruneCompanionSnapshotsSweep(ctx, root, time.Now().Add(2*time.Hour), 1, 4, true, sweep)
	if removed != 1 {
		t.Fatalf("later inventory deadline starved earlier eligible reclamation: removed=%d visits=%d problems=%v", removed, visits, problems)
	}
}

func TestSnapshotSweepAdvancesPastHeldOrExpiringCandidates(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(fmt.Sprint(expire), func(t *testing.T) {
			service, owner := companionGCFixture(t, 1, false)
			root, err := filepath.EvalSymlinks(service.stateRoot)
			if err != nil {
				t.Fatal(err)
			}
			second := filepath.Join(root, companionSnapshotRoot, strings.Repeat("f", 64))
			if err = buildCompanionSnapshot(context.Background(), second, strings.Repeat("f", 64), owner.Companions[0]); err != nil {
				t.Fatal(err)
			}
			sweep := &companionSnapshotSweep{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if expire {
				sweep.beforeCandidate = func(string) { cancel() }
			}
			removed, _ := pruneCompanionSnapshotsSweep(ctx, root, time.Now().Add(2*time.Hour), 1, 1, true, sweep)
			if removed != 0 || !pathExists(second) {
				t.Fatal("first pass exceeded its candidate bound")
			}
			sweep.beforeCandidate = nil
			removed, problems := pruneCompanionSnapshotsSweep(context.Background(), root, time.Now().Add(2*time.Hour), 1, 1, true, sweep)
			if removed != 1 || len(problems) != 0 || pathExists(second) {
				t.Fatalf("earlier held/expired entry starved later cleanup: %d %v", removed, problems)
			}
		})
	}
}
