package sessionsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

const storageTestPayload = 512 << 10

func writeStorageBytes(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func storageRetainedNames(result StorageReclaim) []string {
	names := make([]string, 0, len(result.Retained))
	for _, retained := range result.Retained {
		names = append(names, retained.Name)
	}
	return names
}

// storageTestRepo is a checkout every fork clones, carrying one committed payload so a fork's
// working tree holds real bytes while its status stays clean.
func storageTestRepo(t *testing.T) string {
	t.Helper()
	repo, git := gitrepo.New(t)
	body := make([]byte, storageTestPayload)
	for i := range body {
		body[i] = byte(i % 251)
	}
	if err := os.WriteFile(filepath.Join(repo, "payload.bin"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "payload.bin")
	git("commit", "-qm", "payload")
	return repo
}

func storageTestService(t *testing.T, repo string) *Service {
	t.Helper()
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), testSessionPolicies(repo), nil)
	t.Cleanup(func() { _ = service.Stop() })
	return service
}

// storageTestFork creates one real fork workspace and, when sessionID is set, the session row that
// owns it — the reconciliation the accounting has to perform for real.
func storageTestFork(t *testing.T, service *Service, repo, name, sessionID string) forkspace.Identity {
	t.Helper()
	workspace, err := forkspace.Setup(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureGenerationLocked(repo, name)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if sessionID == "" {
		return identity
	}
	if _, err := service.Store().CreateSession(context.Background(), sessionID+"-create", session.CreateSessionRequest{
		ID: sessionID, Target: "codex@work", Policy: "responder",
		Repository: repo, Workspace: workspace, ForkName: name, ForkGeneration: string(identity.Generation),
		BaseCommit: gitOut(repo, "rev-parse", "HEAD"),
		MaxTurns:   3, MaxQueuedTurns: 3, MaxQueuedBytes: 4096,
	}); err != nil {
		t.Fatal(err)
	}
	return identity
}

func storageCloseSession(t *testing.T, service *Service, sessionID string) {
	t.Helper()
	current, err := service.Store().GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store().CloseSession(context.Background(), sessionID+"-close", session.CloseSessionRequest{
		SessionID: sessionID, ExpectedRevision: current.Revision,
	}); err != nil {
		t.Fatal(err)
	}
}

func storageForkEntry(t *testing.T, report StorageReport, name string) StorageFork {
	t.Helper()
	for _, fork := range report.Forks {
		if fork.Name == name {
			return fork
		}
	}
	t.Fatalf("fork %q is missing from the accounting: %+v", name, report.Forks)
	return StorageFork{}
}

func mustSetStorageLimits(t *testing.T, service *Service, limits StorageLimits) {
	t.Helper()
	if err := service.setStorageLimits(limits); err != nil {
		t.Fatal(err)
	}
}

// storageTestLimits places the watermarks around whatever the test machine's volume is actually
// holding, so the fixtures exercise the classification rather than the developer's disk usage.
func storageTestLimits(t *testing.T) StorageLimits {
	t.Helper()
	volume, err := forkspace.MeasureFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	used := volume.CapacityBytes - volume.FreeBytes
	limits := StorageLimits{
		ReserveBytes:          1 << 20,
		LowWatermarkBytes:     used + volume.FreeBytes/4,
		HighWatermarkBytes:    used + volume.FreeBytes/2,
		DisposableBudgetBytes: DefaultStorageDisposableBudget,
		GraceWindow:           0,
		MeasureInterval:       time.Nanosecond,
		MaxReclaimPerPass:     DefaultStorageMaxReclaimPerPass,
	}
	limits.ProtectedBudgetBytes = limits.LowWatermarkBytes
	return limits
}

// The whole point of the accounting: every retained byte has a reason, and the reason decides
// whether the control plane may ask for it back.
func TestStorageReportSeparatesActiveGraceDisposableAndUnattributedForks(t *testing.T) {
	repo := storageTestRepo(t)
	service := storageTestService(t, repo)
	limits := storageTestLimits(t)
	limits.GraceWindow = time.Hour
	mustSetStorageLimits(t, service, limits)

	storageTestFork(t, service, repo, "fork-active", "session-active")
	storageTestFork(t, service, repo, "fork-grace", "session-grace")
	storageCloseSession(t, service, "session-grace")
	// A directory nobody can prove coop created. It is storage, it is reported, and it is never
	// anybody's to delete.
	foreign := filepath.Join(forkspace.Home(repo), "operator-copy")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStorageBytes(t, filepath.Join(foreign, "notes.bin"), 128<<10)

	report, err := service.StorageReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := storageForkEntry(t, report, "fork-active").Category; got != StorageCategoryActive {
		t.Fatalf("open session fork category = %q, want %q", got, StorageCategoryActive)
	}
	if got := storageForkEntry(t, report, "fork-grace").Category; got != StorageCategoryGrace {
		t.Fatalf("just-closed session fork category = %q, want %q", got, StorageCategoryGrace)
	}
	unattributed := storageForkEntry(t, report, "operator-copy")
	if unattributed.Category != StorageCategoryUnattributed || unattributed.SessionID != "" {
		t.Fatalf("foreign directory = %+v, want unattributed with no owner", unattributed)
	}
	if len(report.Problems) != 0 {
		t.Fatalf("measurement reported problems: %v", report.Problems)
	}
	if report.Totals.UnattributedBytes < 128<<10 {
		t.Fatalf("unattributed bytes = %d, want at least the foreign payload", report.Totals.UnattributedBytes)
	}
	if report.Storage.DisposableBytes != 0 {
		t.Fatalf("disposable bytes = %d while every fork is active or in grace", report.Storage.DisposableBytes)
	}
	if report.Storage.UnattributedBytes == nil || *report.Storage.UnattributedBytes != report.Totals.UnattributedBytes {
		t.Fatalf("published unattributed bytes = %v, want %d", report.Storage.UnattributedBytes, report.Totals.UnattributedBytes)
	}
	if err := report.Storage.Validate(); err != nil {
		t.Fatalf("published storage failed its own contract: %v", err)
	}
	t.Logf("totals: %+v", report.Totals)
	t.Logf("published: capacity=%d free=%d disposable=%d protected=%d unattributed=%d allocation=%s",
		report.Storage.CapacityBytes, report.Storage.FreeBytes, report.Storage.DisposableBytes,
		report.Storage.ProtectedBytes, *report.Storage.UnattributedBytes, report.Storage.Allocation)

	// Once the grace window is behind it, the same fork is the control plane's to reclaim.
	limits.GraceWindow = 0
	mustSetStorageLimits(t, service, limits)
	report, err = service.StorageReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := storageForkEntry(t, report, "fork-grace").Category; got != StorageCategoryDisposable {
		t.Fatalf("post-grace fork category = %q, want %q", got, StorageCategoryDisposable)
	}
	if report.Storage.DisposableBytes < storageTestPayload {
		t.Fatalf("disposable bytes = %d, want at least one fork's working tree", report.Storage.DisposableBytes)
	}
}

// A fork holding uncommitted work is not disposable at any age, and the accounting has to say so
// before anything decides the disk is full of garbage.
func TestStorageReportHoldsDirtyAndQuarantinedForksOutOfTheDisposableTotal(t *testing.T) {
	repo := storageTestRepo(t)
	service := storageTestService(t, repo)
	mustSetStorageLimits(t, service, storageTestLimits(t))

	storageTestFork(t, service, repo, "fork-dirty", "session-dirty")
	storageCloseSession(t, service, "session-dirty")
	writeStorageBytes(t, filepath.Join(forkspace.Workspace(repo, "fork-dirty"), "unsaved.bin"), 256<<10)
	storageTestFork(t, service, repo, "fork-quarantined", "session-quarantined")
	storageCloseSession(t, service, "session-quarantined")
	service.mu.Lock()
	service.quarantined["session-quarantined"] = struct{}{}
	service.mu.Unlock()

	report, err := service.StorageReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dirty := storageForkEntry(t, report, "fork-dirty")
	if dirty.Category != StorageCategoryProtected || !strings.Contains(dirty.Reason, "uncommitted") {
		t.Fatalf("dirty fork = %+v, want protected with an uncommitted-work reason", dirty)
	}
	quarantined := storageForkEntry(t, report, "fork-quarantined")
	if quarantined.Category != StorageCategoryProtected || !strings.Contains(quarantined.Reason, "quarantined") {
		t.Fatalf("quarantined fork = %+v, want protected with a quarantine reason", quarantined)
	}
	if report.Storage.DisposableBytes != 0 {
		t.Fatalf("disposable bytes = %d, want nothing while both forks are protected", report.Storage.DisposableBytes)
	}
	if report.Storage.ProtectedBytes < storageTestPayload {
		t.Fatalf("protected bytes = %d, want at least the retained working trees", report.Storage.ProtectedBytes)
	}
}

// Reclamation has to be measurable: the promised bytes must actually leave the volume.
func TestStorageReportDisposableBytesFallAfterTheForkIsDiscarded(t *testing.T) {
	repo := storageTestRepo(t)
	service := storageTestService(t, repo)
	mustSetStorageLimits(t, service, storageTestLimits(t))
	storageTestFork(t, service, repo, "fork-done", "session-done")
	storageCloseSession(t, service, "session-done")

	before, err := service.StorageReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before.Storage.DisposableBytes < storageTestPayload {
		t.Fatalf("disposable bytes = %d before discard, want the fork's working tree", before.Storage.DisposableBytes)
	}
	plan, err := planSessionWorkspaceDiscard(repo, forkspace.Workspace(repo, "fork-done"), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := discardSessionWorkspace(plan); err != nil {
		t.Fatal(err)
	}

	after, err := service.StorageReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.Storage.DisposableBytes != 0 {
		t.Fatalf("disposable bytes = %d after discard, want 0", after.Storage.DisposableBytes)
	}
	freed := before.Storage.DisposableBytes - after.Storage.DisposableBytes
	t.Logf("disposable bytes before=%d after=%d freed=%d (working tree payload %d, shared baseline before=%d)",
		before.Storage.DisposableBytes, after.Storage.DisposableBytes, freed, storageTestPayload, before.Totals.BaselineSharedBytes)
	if freed < storageTestPayload {
		t.Fatalf("accounted reclamation = %d bytes, want at least the %d-byte working tree", freed, storageTestPayload)
	}
	if pathExists(forkspace.Workspace(repo, "fork-done")) {
		t.Fatal("the discarded workspace is still on disk")
	}
	if staged, err := forkspace.StagedDiscards(repo, ""); err != nil || len(staged) != 0 {
		t.Fatalf("discard left staged garbage: %v, %v", staged, err)
	}
}

// Oscillation is its own outage: a worker that reopens the instant one fork is reclaimed and
// closes again on the next allocation never makes progress. Closing at the high watermark and
// reopening only under the low one is the whole mechanism.
func TestStoragePressureClosesAtTheHighWatermarkAndReopensOnlyUnderTheLow(t *testing.T) {
	capacity := int64(100 << 30)
	limits := StorageLimits{
		ReserveBytes: 5 << 30, HighWatermarkBytes: 85 << 30, LowWatermarkBytes: 75 << 30,
		DisposableBudgetBytes: 10 << 30, ProtectedBudgetBytes: 75 << 30,
		GraceWindow: time.Minute, MeasureInterval: time.Minute, MaxReclaimPerPass: 4,
	}
	pressure := &storagePressure{}

	if allocation, reason := pressure.evaluate(limits, capacity, 40<<30, 0); allocation != workerproto.StorageAllocationOpen || reason != nil {
		t.Fatalf("a healthy volume = %q/%v, want open", allocation, reason)
	}
	if allocation, reason := pressure.evaluate(limits, capacity, 14<<30, 0); allocation != workerproto.StorageAllocationRefused ||
		reason == nil || *reason != workerproto.StorageRefusalReserveExhausted {
		t.Fatalf("crossing the high watermark = %q/%v, want a refusal", allocation, reason)
	}
	// Between the watermarks the refusal is sticky: one reclaimed fork must not reopen the gate.
	if allocation, _ := pressure.evaluate(limits, capacity, 20<<30, 0); allocation != workerproto.StorageAllocationRefused {
		t.Fatalf("between the watermarks = %q, want the refusal to hold", allocation)
	}
	if allocation, reason := pressure.evaluate(limits, capacity, 25<<30, 0); allocation != workerproto.StorageAllocationOpen || reason != nil {
		t.Fatalf("recovering under the low watermark = %q/%v, want open", allocation, reason)
	}

	if allocation, reason := pressure.evaluate(limits, capacity, 1<<30, 0); allocation != workerproto.StorageAllocationRefused ||
		reason == nil || *reason != workerproto.StorageRefusalReserveExhausted {
		t.Fatalf("below the reserve = %q/%v, want reserve_exhausted", allocation, reason)
	}
	fresh := &storagePressure{}
	if allocation, reason := fresh.evaluate(limits, capacity, 40<<30, 80<<30); allocation != workerproto.StorageAllocationRefused ||
		reason == nil || *reason != workerproto.StorageRefusalProtectedBudget {
		t.Fatalf("protected storage over budget = %q/%v, want protected_storage_exceeds_budget", allocation, reason)
	}
	// Deleting nothing is the correct response to a budget filled by protected data; recovery
	// happens when the protected bytes themselves go, not by force.
	if allocation, _ := fresh.evaluate(limits, capacity, 40<<30, 10<<30); allocation != workerproto.StorageAllocationOpen {
		t.Fatalf("protected storage back under budget = %q, want open", allocation)
	}
}

// Under pressure the worker stops taking NEW work; it must not stop recovering the work it
// already has, or a full disk becomes a permanently stuck fleet.
func TestForkAllocationRefusesNewForksUnderPressureButStillAdoptsExistingWork(t *testing.T) {
	repo := storageTestRepo(t)
	service := storageTestService(t, repo)
	base := gitOut(repo, "rev-parse", "HEAD")
	existing, err := ensureSessionWorkspaceContext(context.Background(), service, repo, "fork-existing", base)
	if err != nil {
		t.Fatal(err)
	}

	filesystem, err := forkspace.MeasureFilesystem(repo)
	if err != nil {
		t.Fatal(err)
	}
	limits := storageTestLimits(t)
	limits.ReserveBytes = filesystem.FreeBytes + 1 // every allocation now falls below the floor
	mustSetStorageLimits(t, service, limits)

	_, err = ensureSessionWorkspaceContext(context.Background(), service, repo, "fork-new", base)
	if session.CodeOf(err) != session.CodeStorageUnavailable {
		t.Fatalf("a new fork under reserve pressure = %v, want storage_unavailable", err)
	}
	var pressure *StoragePressureError
	if !errors.As(err, &pressure) || pressure.Reason != workerproto.StorageRefusalReserveExhausted {
		t.Fatalf("refusal did not name its cause: %v", err)
	}
	if pathExists(forkspace.Workspace(repo, "fork-new")) {
		t.Fatal("a refused allocation still created a workspace")
	}
	adopted, err := ensureSessionWorkspaceContext(context.Background(), service, repo, "fork-existing", base)
	if err != nil {
		t.Fatalf("recovery of existing work was refused under pressure: %v", err)
	}
	if adopted.Fork != existing.Fork {
		t.Fatalf("recovery adopted %+v, want the original %+v", adopted.Fork, existing.Fork)
	}

	limits.ReserveBytes = 1 << 20
	mustSetStorageLimits(t, service, limits)
	if _, err := ensureSessionWorkspaceContext(context.Background(), service, repo, "fork-new", base); err != nil {
		t.Fatalf("allocation stayed closed after the pressure cleared: %v", err)
	}
}

// A controller that sees an internal error stops retrying; one that sees storage_unavailable with
// the limit that refused it can place elsewhere and come back. The type has to survive the wrapping
// every create failure goes through.
func TestStoragePressureErrorReachesTheAPIAsARetryableTypedRefusal(t *testing.T) {
	wrapped := fmt.Errorf("ensure session workspace: %w", &StoragePressureError{
		Reason: workerproto.StorageRefusalReserveExhausted, FreeBytes: 1 << 20, ReserveBytes: 5 << 30,
	})
	code, status, detail := sessionHTTPError(wrapped)
	if code != string(session.CodeStorageUnavailable) || status != http.StatusServiceUnavailable {
		t.Fatalf("pressure refusal surfaced as %s/%d", code, status)
	}
	if !strings.Contains(detail, "reserve") {
		t.Fatalf("pressure refusal lost its cause: %q", detail)
	}
	watermark := &StoragePressureError{
		Reason: workerproto.StorageRefusalReserveExhausted, FreeBytes: 30 << 30, UsedBytes: 470 << 30,
		ReserveBytes: 25 << 30, HighWatermarkBytes: 475 << 30, LowWatermarkBytes: 450 << 30,
	}
	if !strings.Contains(watermark.Error(), "high watermark") {
		t.Fatalf("a watermark close blamed the reserve: %q", watermark.Error())
	}
}

// Ownership is proven, never assumed. Anything coop cannot prove it created is reported for a
// human to look at and left exactly where it is.
func TestReclaimOwnedOrphansRemovesProvenGarbageAndReportsEverythingElse(t *testing.T) {
	repo := storageTestRepo(t)
	service := storageTestService(t, repo)
	mustSetStorageLimits(t, service, storageTestLimits(t))

	orphan := storageTestFork(t, service, repo, "fork-orphan", "")
	storageTestFork(t, service, repo, "fork-dirty-orphan", "")
	writeStorageBytes(t, filepath.Join(forkspace.Workspace(repo, "fork-dirty-orphan"), "unsaved.bin"), 64<<10)
	storageTestFork(t, service, repo, "fork-owned", "session-owned")
	reserved := storageTestFork(t, service, repo, "fork-reserved", "")
	unlock, err := forkspace.LockState(repo, "fork-reserved")
	if err != nil {
		t.Fatal(err)
	}
	err = forkspace.ReserveWorkspaceLocked(repo, forkspace.WorkspaceReservation{
		Version: forkspace.WorkspaceReservationVersion, Fork: reserved,
		Kind: forkspace.WorkspaceReservationRemoteSession, OwnerID: "session-pending", CreatedAt: time.Now().UTC(),
	})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(forkspace.Home(repo), "operator-copy")
	writeStorageBytes(t, filepath.Join(foreign, "notes.bin"), 32<<10)

	result, err := service.ReclaimOwnedOrphans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Reclaimed) != 1 || !strings.Contains(result.Reclaimed[0], "fork-orphan") {
		t.Fatalf("reclaimed = %v, want only the proven orphan", result.Reclaimed)
	}
	if result.ReclaimedBytes < storageTestPayload {
		t.Fatalf("reclaimed bytes = %d, want at least the orphan's working tree", result.ReclaimedBytes)
	}
	t.Logf("reclaimed %v (%d bytes); retained %+v", result.Reclaimed, result.ReclaimedBytes, result.Retained)
	if pathExists(forkspace.Workspace(repo, "fork-orphan")) {
		t.Fatal("the reclaimed orphan is still on disk")
	}
	if _, ok, _ := forkspace.ReadGeneration(repo, "fork-orphan"); ok {
		t.Fatal("the reclaimed orphan kept its generation record")
	}
	for _, kept := range []string{"fork-dirty-orphan", "fork-owned", "fork-reserved", "operator-copy"} {
		if !pathExists(filepath.Join(forkspace.Home(repo), kept)) {
			t.Fatalf("%s was deleted without proof", kept)
		}
	}
	retained := storageRetainedNames(result)
	if !slices.Contains(retained, "fork-dirty-orphan") || !slices.Contains(retained, "fork-reserved") {
		t.Fatalf("retained candidates were not reported for inspection: %+v", result.Retained)
	}
	if orphan.Generation == "" {
		t.Fatal("fixture produced no generation")
	}
}

// An unreferenced generation that is only seconds old is far more likely to be a create still in
// flight than garbage. Age is read off coop's own durable generation record, never a mtime.
func TestReclaimOwnedOrphansWaitsOutTheGraceWindowBeforeTouchingAnything(t *testing.T) {
	repo := storageTestRepo(t)
	service := storageTestService(t, repo)
	limits := storageTestLimits(t)
	limits.GraceWindow = time.Hour
	mustSetStorageLimits(t, service, limits)
	storageTestFork(t, service, repo, "fork-young", "")

	result, err := service.ReclaimOwnedOrphans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Reclaimed) != 0 {
		t.Fatalf("reclaimed %v inside the grace window", result.Reclaimed)
	}
	if !pathExists(forkspace.Workspace(repo, "fork-young")) {
		t.Fatal("a young unreferenced generation was deleted")
	}
}

// A removal that cannot finish must not report that it did, and what it leaves behind must stay
// finishable. Deleting in place fails both: the fork keeps its name while losing the .git every
// later plan needs, so its bytes are stranded and the receipt is a lie.
func TestDiscardStagesItsWorkspaceAndRefusesAReceiptUntilTheBytesAreGone(t *testing.T) {
	repo, git := gitrepo.New(t)
	writeStorageBytes(t, filepath.Join(repo, "locked", "payload.bin"), storageTestPayload)
	git("add", "locked/payload.bin")
	git("commit", "-qm", "payload")
	service := storageTestService(t, repo)
	mustSetStorageLimits(t, service, storageTestLimits(t))
	storageTestFork(t, service, repo, "fork-stuck", "")
	workspace := forkspace.Workspace(repo, "fork-stuck")
	// Removal will fail partway: the directory holding a tracked file is not writable. The
	// workspace itself stays clean and fully merged, so the plan admits it.
	locked := filepath.Join(workspace, "locked")
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	plan, err := planSessionWorkspaceDiscard(repo, workspace, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := discardSessionWorkspace(plan); err == nil {
		t.Fatal("a discard that could not remove its bytes reported success")
	}
	if pathExists(workspace) {
		t.Fatal("the fork path survived the discard, so no later pass can tell it from live work")
	}
	staged, err := forkspace.StagedDiscards(repo, "fork-stuck")
	if err != nil || len(staged) != 1 {
		t.Fatalf("staged discards = %v, %v, want the interrupted tree retained", staged, err)
	}
	report, err := service.StorageReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Totals.StagedDiscardBytes < storageTestPayload {
		t.Fatalf("staged discard bytes = %d, want the stuck tree still accounted", report.Totals.StagedDiscardBytes)
	}

	// The obstruction clears; the next pass finishes the removal it started.
	if err := os.Chmod(filepath.Join(staged[0], "locked"), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := service.ReclaimOwnedOrphans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.StagedPurged != 1 || pathExists(staged[0]) {
		t.Fatalf("resumed purge = %d, staged still present = %v", result.StagedPurged, pathExists(staged[0]))
	}
}

// The crash case: the rename that begins a removal survived, the removal itself did not. The next
// pass has to finish it without re-deriving anything the crash destroyed.
func TestReclaimOwnedOrphansFinishesAnInterruptedDelete(t *testing.T) {
	repo := storageTestRepo(t)
	service := storageTestService(t, repo)
	mustSetStorageLimits(t, service, storageTestLimits(t))
	storageTestFork(t, service, repo, "fork-interrupted", "")

	handle, info, err := forkspace.Pin(forkspace.Workspace(repo, "fork-interrupted"))
	if err != nil {
		t.Fatal(err)
	}
	staged, err := forkspace.StageWorkspaceDiscardLocked(repo, "fork-interrupted", info)
	_ = handle.Close()
	if err != nil {
		t.Fatal(err)
	}
	// The crash: half the tree removed, the generation record still naming a workspace that is
	// no longer where it was.
	if err := os.RemoveAll(filepath.Join(staged, ".git")); err != nil {
		t.Fatal(err)
	}

	report, err := service.StorageReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Totals.StagedDiscardBytes < storageTestPayload {
		t.Fatalf("staged discard bytes = %d, want the interrupted tree counted", report.Totals.StagedDiscardBytes)
	}

	result, err := service.ReclaimOwnedOrphans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.StagedPurged != 1 {
		t.Fatalf("staged purged = %d, want the interrupted delete finished", result.StagedPurged)
	}
	if pathExists(staged) {
		t.Fatal("the interrupted delete was reported finished while its bytes remain")
	}
	if _, ok, _ := forkspace.ReadGeneration(repo, "fork-interrupted"); ok {
		t.Fatal("the finished delete left its generation record behind")
	}
}

// Operators inspect a worker through its owner-private socket, not a new port.
func TestSessionHTTPStorageReportsTheWorkerAccounting(t *testing.T) {
	repo := storageTestRepo(t)
	service := storageTestService(t, repo)
	mustSetStorageLimits(t, service, storageTestLimits(t))
	storageTestFork(t, service, repo, "fork-active", "session-active")

	response := sessionHTTPTestRequest(t, NewHTTPHandler(service), http.MethodGet, "/v1/storage", "", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /v1/storage = %d: %s", response.Code, response.Body.String())
	}
	var body struct {
		Storage workerproto.Storage `json:"storage"`
		Totals  StorageTotals       `json:"totals"`
		Forks   []StorageFork       `json:"forks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode storage report: %v\n%s", err, response.Body.String())
	}
	if err := body.Storage.Validate(); err != nil {
		t.Fatalf("the published storage object is invalid: %v\n%s", err, response.Body.String())
	}
	if body.Storage.Allocation != workerproto.StorageAllocationOpen {
		t.Fatalf("allocation = %q on a healthy worker", body.Storage.Allocation)
	}
	active := storageForkEntry(t, StorageReport{Forks: body.Forks}, "fork-active")
	if active.SessionID != "session-active" || active.Category != StorageCategoryActive {
		t.Fatalf("per-fork inventory = %+v", body.Forks)
	}
	if control := storageForkEntry(t, StorageReport{Forks: body.Forks}, ".coop"); control.Category != StorageCategoryControl {
		t.Fatalf("coop's own control directory = %+v, want it inventoried as control", control)
	}
	if body.Totals.ActiveBytes < storageTestPayload {
		t.Fatalf("active bytes = %d, want the live fork's working tree", body.Totals.ActiveBytes)
	}
}

// The derived defaults are policy, not law: an operator whose volume is shared with something else
// has to be able to say so, in the file they already own, and be told at load if what they wrote
// cannot hold.
func TestLoadStorageLimitsReadsTheOperatorBlockAndRefusesAnIncoherentOne(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	policies := "version: 1\npolicies:\n  responder:\n    repository: " + repo +
		"\n    target: codex@work\n    max_turns: 1\n    max_queued_turns: 1\n    max_queued_bytes: 1\n" +
		"    max_patch_bytes: 1\n    turn_timeout: 1s\n"
	write := func(t *testing.T, body string) string {
		t.Helper()
		// The loader refuses a path through a symlinked ancestor, and macOS temp roots are one.
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "session-policies.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	bare := write(t, policies)
	if limits, configured, err := LoadStorageLimits(bare); err != nil || configured {
		t.Fatalf("a file with no storage block = %+v, %v, %v, want the derived defaults", limits, configured, err)
	}

	block := "storage:\n  reserve_bytes: 5368709120\n  high_watermark_bytes: 95000000000\n" +
		"  low_watermark_bytes: 90000000000\n  disposable_budget_bytes: 10737418240\n" +
		"  protected_budget_bytes: 90000000000\n  grace_window: 15m\n  measure_interval: 5m\n" +
		"  max_reclaim_per_pass: 4\n"
	path := write(t, policies+block)
	limits, configured, err := LoadStorageLimits(path)
	if err != nil || !configured {
		t.Fatalf("LoadStorageLimits = %+v, %v, %v", limits, configured, err)
	}
	if limits.ReserveBytes != 5368709120 || limits.LowWatermarkBytes != 90000000000 ||
		limits.GraceWindow != 15*time.Minute || limits.MeasureInterval != 5*time.Minute ||
		limits.MaxReclaimPerPass != 4 {
		t.Fatalf("configured limits = %+v", limits)
	}
	// The same file still parses as policies: one document, two readers.
	if loaded, err := LoadPolicies(path, nil); err != nil || len(loaded) != 1 {
		t.Fatalf("policies alongside a storage block = %d, %v", len(loaded), err)
	}

	for name, body := range map[string]string{
		"inverted watermarks": "storage:\n  reserve_bytes: 1\n  high_watermark_bytes: 10\n  low_watermark_bytes: 20\n" +
			"  disposable_budget_bytes: 1\n  protected_budget_bytes: 1\n  grace_window: 1m\n  measure_interval: 1m\n  max_reclaim_per_pass: 1\n",
		"unbounded reclamation": "storage:\n  reserve_bytes: 1\n  high_watermark_bytes: 20\n  low_watermark_bytes: 10\n" +
			"  disposable_budget_bytes: 1\n  protected_budget_bytes: 1\n  grace_window: 1m\n  measure_interval: 1m\n  max_reclaim_per_pass: 0\n",
		"unreadable duration": "storage:\n  reserve_bytes: 1\n  high_watermark_bytes: 20\n  low_watermark_bytes: 10\n" +
			"  disposable_budget_bytes: 1\n  protected_budget_bytes: 1\n  grace_window: soon\n  measure_interval: 1m\n  max_reclaim_per_pass: 1\n",
		"partial block": "storage:\n  reserve_bytes: 1\n",
		"unknown field": "storage:\n  reserve_percent: 5\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := LoadStorageLimits(write(t, policies+body)); err == nil {
				t.Fatal("an incoherent storage block was accepted")
			}
		})
	}
}

func TestStorageLimitsValidateRejectsIncoherentBudgets(t *testing.T) {
	capacity := int64(100 << 30)
	if err := DefaultStorageLimits(capacity).Validate(capacity); err != nil {
		t.Fatalf("the documented defaults are invalid: %v", err)
	}
	for name, mutate := range map[string]func(*StorageLimits){
		"reserve above capacity":    func(l *StorageLimits) { l.ReserveBytes = capacity + 1 },
		"watermarks inverted":       func(l *StorageLimits) { l.LowWatermarkBytes = l.HighWatermarkBytes },
		"high watermark off volume": func(l *StorageLimits) { l.HighWatermarkBytes = capacity + 1 },
		"no disposable budget":      func(l *StorageLimits) { l.DisposableBudgetBytes = 0 },
		"no protected budget":       func(l *StorageLimits) { l.ProtectedBudgetBytes = -1 },
		"negative grace":            func(l *StorageLimits) { l.GraceWindow = -time.Second },
		"no measure interval":       func(l *StorageLimits) { l.MeasureInterval = 0 },
		"unbounded reclaim":         func(l *StorageLimits) { l.MaxReclaimPerPass = 0 },
	} {
		limits := DefaultStorageLimits(capacity)
		mutate(&limits)
		if err := limits.Validate(capacity); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
