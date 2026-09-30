package sessionsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestRuntimeCapacityReservationIsAtomicAndCleanupFailureStaysOccupied(t *testing.T) {
	s := &Service{}
	var admitted atomic.Int32
	var workers sync.WaitGroup
	for i := range 50 {
		workers.Go(func() {
			if ok, _ := s.reserveRuntimeSlot(fmt.Sprint(i)); ok {
				admitted.Add(1)
			}
		})
	}
	workers.Wait()
	if got := admitted.Load(); got != sessionRuntimeSlots {
		t.Fatalf("admitted %d runtimes, want %d", got, sessionRuntimeSlots)
	}
	if got := s.RuntimeCapacity(); got.TurnSlotsFree != 0 || got.State != "busy" {
		t.Fatalf("full capacity=%+v", got)
	}

	s = &Service{}
	if ok, _ := s.reserveRuntimeSlot("warm"); !ok {
		t.Fatal("first reservation refused")
	}
	if ok, _ := s.reserveRuntimeSlot("warm"); !ok || s.RuntimeCapacity().TurnSlotsFree != 3 {
		t.Fatal("same-session warm reuse spent a second permit")
	}
	s.runtimeCleanupFailed("warm")
	s.finishRuntimeSlot(session.Session{ID: "warm"}, nil)
	if ok, _ := s.reserveRuntimeSlot("warm"); ok || s.RuntimeCapacity().TurnSlotsFree != 3 {
		t.Fatal("uncertain cleanup allowed a second child or advertised its slot free")
	}
	s.runtimeCleaned("warm")
	if got := s.RuntimeCapacity().TurnSlotsFree; got != 4 {
		t.Fatalf("verified cleanup left %d free slots", got)
	}
	s.historicalPending = map[string]struct{}{"old": {}}
	if ok, _ := s.reserveRuntimeSlot("new"); ok || s.RuntimeCapacity().TurnSlotsFree != 0 {
		t.Fatal("unknown historical runtime admitted new work")
	}
	s.markHistoricalRuntimeClean("old")
	if ok, _ := s.reserveRuntimeSlot("new"); !ok {
		t.Fatal("historical cleanup did not wake admission")
	}
}

func TestRuntimeCapacityWaitDoesNotLeaseOrSpendTurnTimeout(t *testing.T) {
	started := make(chan time.Duration, 1)
	var store *session.Store
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), "", func(saved *session.Store) Runner {
		store = saved
		return RunnerFunc(func(ctx context.Context, bound session.Session, turn session.Turn) (session.Turn, error) {
			deadline, _ := ctx.Deadline()
			started <- time.Until(deadline)
			if _, err := store.MarkTurnSendIntent(ctx, bound.ID, turn.ID); err != nil {
				return turn, err
			}
			if _, err := store.MarkTurnSent(ctx, bound.ID, turn.ID); err != nil {
				return turn, err
			}
			return store.CompleteTurn(ctx, session.CompleteTurnRequest{SessionID: bound.ID, TurnID: turn.ID, Message: "done"})
		})
	})
	defer service.Stop()
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := range sessionRuntimeSlots {
		if ok, _ := service.reserveRuntimeSlot(fmt.Sprint(i)); !ok {
			t.Fatal("could not fill runtime pool")
		}
	}
	service.Job.Limits.TurnTimeoutMS = 500
	bound, err := service.CreateRemoteSession(context.Background(), "capacity-create", service.request(t, "capacity"))
	if err != nil {
		t.Fatal(err)
	}
	turn, err := service.SubmitTurn(context.Background(), "capacity-turn", session.SubmitTurnRequest{
		SessionID: bound.ID, ExpectedRevision: bound.Revision, Prompt: "wait for capacity",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Waiting longer than the whole model budget must still leave a queued turn.
	select {
	case <-started:
		t.Fatal("started a fifth runtime")
	case <-time.After(600 * time.Millisecond):
	}
	current, err := service.Store().GetTurn(context.Background(), bound.ID, turn.ID)
	if err != nil || current.State != session.TurnQueued {
		t.Fatalf("capacity wait leased/failed turn: %+v, %v", current, err)
	}
	service.runtimeCleaned("0")
	select {
	case remaining := <-started:
		if remaining < 300*time.Millisecond {
			t.Fatalf("capacity wait consumed model budget: %s", remaining)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("freeing capacity did not wake the queued turn")
	}
	waitForSessionTest(t, func() bool {
		current, _ := service.Store().GetTurn(context.Background(), bound.ID, turn.ID)
		return current.State == session.TurnCompleted
	})
}

func TestRuntimeCapacityWaitIsCancellableAndDoesNotHoldSessionLock(t *testing.T) {
	s := &Service{runtimeLocks: make(map[string]*sessionOperationLock)}
	for i := range sessionRuntimeSlots {
		s.reserveRuntimeSlot(fmt.Sprint(i))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		unlock, err := s.lockRuntimeCapacity(ctx, "waiting", nil)
		if unlock != nil {
			unlock()
		}
		done <- err
	}()
	unlocked := s.lockSessionRuntime("waiting")
	unlocked()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("capacity cancellation=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("capacity wait ignored cancellation")
	}
}

func TestRuntimeCapacityHTTPUsesAdmissionState(t *testing.T) {
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	service.reserveRuntimeSlot("busy")
	response := sessionHTTPTestRequest(t, NewHTTPHandler(service.Service), http.MethodGet, "/v1/capacity", "", "", "")
	var capacity workerproto.Capacity
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &capacity) != nil || capacity.TurnSlotsFree != 3 {
		t.Fatalf("capacity response=%d %s", response.Code, response.Body.String())
	}
}

func warmCapacityFixture(t *testing.T) (*sessionACPFixture, *Service, *sessionWarmExecution) {
	t.Helper()
	fixture := newSessionACPFixture(t, "normal")
	service := &Service{store: fixture.store, runner: fixture.runner,
		runtimeLocks:       make(map[string]*sessionOperationLock),
		runtimeCleanupDone: make(map[string]runtimeCleanupStamp)}
	fixture.runner.service = service
	service.reserveRuntimeSlot(fixture.session.ID)
	if err := fixture.runner.PrepareSession(contextWithTurnDeadline(t), fixture.session, time.Minute); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.runner.CloseWarmSessions() })
	service.finishRuntimeSlot(fixture.session, nil)
	if service.RuntimeCapacity().TurnSlotsFree != sessionRuntimeSlots-1 {
		t.Fatal("warm preparation released its runtime permit")
	}
	fixture.runner.warmMu.Lock()
	execution := fixture.runner.warm[fixture.session.ID]
	fixture.runner.warmMu.Unlock()
	return fixture, service, execution
}

func TestWarmExpiryKeepsRuntimeOwnedUntilSerializedTeardown(t *testing.T) {
	fixture, service, execution := warmCapacityFixture(t)
	unlock := service.lockSessionRuntime(fixture.session.ID)
	done := make(chan error, 1)
	go func() { done <- fixture.runner.closeWarmExecution(fixture.session.ID, execution) }()
	// Observe the expirer waiting on the actual runtime lock, not a timing guess.
	waitForSessionTest(t, func() bool {
		service.runtimeMu.Lock()
		defer service.runtimeMu.Unlock()
		return service.runtimeLocks[fixture.session.ID].refs == 2
	})
	fixture.runner.warmMu.Lock()
	retained := fixture.runner.warm[fixture.session.ID] == execution
	fixture.runner.warmMu.Unlock()
	if !retained || service.RuntimeCapacity().TurnSlotsFree != 3 {
		unlock()
		t.Fatal("expiry exposed a free slot or hid the child before owning runtime teardown")
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if service.RuntimeCapacity().TurnSlotsFree != 4 {
		t.Fatal("proven warm teardown did not release capacity")
	}
}

func TestFailedWarmBoxRemovalRetainsCapacityUntilJanitorProvesExactLabelGone(t *testing.T) {
	fixture, service, execution := warmCapacityFixture(t)
	t.Setenv("COOP_TEST_SESSION_BOX_CLEANUP_FAIL", "1")
	if err := fixture.runner.closeWarmExecution(fixture.session.ID, execution); err == nil {
		t.Fatal("failed Docker removal was reported as clean")
	}
	service.finishRuntimeSlot(fixture.session, nil)
	service.cleanupIdleSessionRuntimes(context.Background())
	if ok, _ := service.reserveRuntimeSlot(fixture.session.ID); ok || service.RuntimeCapacity().TurnSlotsFree != 3 {
		t.Fatal("missing warm map entry hid an orphan box from cleanup/admission")
	}
	t.Setenv("COOP_TEST_SESSION_BOX_CLEANUP_FAIL", "")
	service.cleanupIdleSessionRuntimes(context.Background())
	if service.RuntimeCapacity().TurnSlotsFree != 4 {
		t.Fatal("successful exact-label janitor retry did not release capacity")
	}
}

func TestFailedHostProcessStopCannotBeHiddenBySuccessfulDockerCleanup(t *testing.T) {
	fixture, service, execution := warmCapacityFixture(t)
	// Model stopProcess failing while its actual helper process is still alive.
	execution.child.stopOnce.Do(func() {
		execution.child.stopErr = acpFailure(sessionACPCleanupError, "process group survived cleanup")
	})
	defer func() { _ = execution.child.stopProcess() }()
	if err := fixture.runner.closeWarmExecution(fixture.session.ID, execution); err == nil {
		t.Fatal("failed host process stop was reported as clean")
	}
	service.cleanupIdleSessionRuntimes(context.Background())
	if service.RuntimeCapacity().TurnSlotsFree != 3 {
		t.Fatal("Docker cleanup hid the still-live host process")
	}
}

func TestFailedPrepareInvalidatesEarlierRuntimeCleanupProof(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	runner := &periodicCleanupRunner{prepareErr: acpFailure(sessionACPCleanupError, "failed prepare cleanup")}
	service := newSessionFixture(t, Config{StateRoot: filepath.Join(t.TempDir(), "state"), Runner: runner}, repo)
	defer service.Stop()
	service.Job.Limits.WarmIdleTimeoutMS = 60_000
	ctx := context.Background()
	bound, err := service.CreateRemoteSession(ctx, "failed-prepare", service.request(t, "failed-prepare"))
	if err != nil {
		t.Fatal(err)
	}
	service.cleanupIdleSessionRuntimes(ctx)
	if runner.calls.Load() != 1 {
		t.Fatal("initial cleanup proof missing")
	}
	if _, err := service.PrepareSession(ctx, bound.ID, bound.Revision); err == nil {
		t.Fatal("failed Prepare succeeded")
	}
	if service.RuntimeCapacity().TurnSlotsFree != 3 {
		t.Fatal("failed Prepare did not hold capacity")
	}
	service.cleanupIdleSessionRuntimes(ctx)
	if runner.calls.Load() != 2 || service.RuntimeCapacity().TurnSlotsFree != 4 {
		t.Fatal("old cleanup stamp hid failed Prepare from the janitor")
	}
}

func TestInvalidPrepareDoesNotWaitBehindOccupiedRuntimeSlots(t *testing.T) {
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), "", nil)
	defer service.Stop()
	for i := range sessionRuntimeSlots {
		service.reserveRuntimeSlot(fmt.Sprint(i))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := service.PrepareSession(ctx, "missing-session", 1)
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("missing session waited for capacity: %v", err)
	}
}

// Every session that outlives a daemon starts with an unproven runtime, and capacity stays at zero
// until each one is proven. Ryker's worker restarted with 17 parked sessions and sat unplaceable for
// minutes while the janitor proved two a minute; a restart now proves its backlog straight away. A
// runtime that cannot be proven still keeps capacity closed, and the drain does not spin on it.
func TestRestartProvesItsSessionBacklogWithoutWaitingForTheTicker(t *testing.T) {
	for _, failing := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanup fails=%v", failing), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			runner := &periodicCleanupRunner{fail: failing}
			open := func() *Service {
				t.Helper()
				service, err := newSessionServiceWithTestStorage(t, Config{
					StateRoot: root, SourceConfig: &config.Config{ConfigDir: t.TempDir()},
					Runner: runner, CleanupInterval: time.Hour, // the ticker never fires in this test
				})
				if err != nil {
					t.Fatal(err)
				}
				return service
			}
			job := bareWorkerJob()
			document, err := json.Marshal(job)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := job.Digest()
			if err != nil {
				t.Fatal(err)
			}
			const parked = 5
			service := open()
			for i := range parked {
				req := CreateRemoteSessionRequest{Task: fmt.Sprintf("workspace:ready-%d", i), Job: document, ExpectedJobDigest: digest}
				if _, err := service.CreateRemoteSession(context.Background(), fmt.Sprintf("create-%d", i), req); err != nil {
					t.Fatal(err)
				}
			}
			if err := service.Stop(); err != nil {
				t.Fatal(err)
			}
			runner.calls.Store(0)
			service = open()
			defer service.Stop()
			drained := make(chan struct{})
			service.testAfterStartupDrain = func() { close(drained) }
			if err := service.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-drained:
			case <-time.After(wait.Deadline):
				t.Fatal("the restart never finished proving its backlog")
			}
			free, proofs := service.RuntimeCapacity().SessionSlotsFree, runner.calls.Load()
			if !failing && (free != sessionRuntimeSlots || proofs != parked) {
				t.Fatalf("after a restart: %d free slots from %d proofs; want %d from %d", free, proofs, sessionRuntimeSlots, parked)
			}
			if failing && (free != 0 || proofs != runtimeCleanupBatchSize) {
				t.Fatalf("unprovable runtimes: %d free slots from %d proofs; want 0 from one bounded batch of %d", free, proofs, runtimeCleanupBatchSize)
			}
		})
	}
}

type quarantinedRuntimeProofRunner struct {
	proofs  atomic.Int32
	runs    atomic.Int32
	release <-chan struct{}
	fail    atomic.Bool
}

func (r *quarantinedRuntimeProofRunner) Run(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
	r.runs.Add(1)
	return turn, errors.New("quarantined session must not run")
}

func (r *quarantinedRuntimeProofRunner) CleanupUnprovenSessionRuntime(ctx context.Context, _ session.Session) error {
	r.proofs.Add(1)
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if r.fail.Load() {
		return errors.New("runtime inventory unavailable")
	}
	return nil
}

func TestQuarantinedRuntimeBacklogReleasesCapacityAfterExactProof(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	release := make(chan struct{})
	runner := &quarantinedRuntimeProofRunner{release: release}
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, func(*session.Store) Runner { return runner })
	defer service.Stop()
	const legacyCount = 15
	sessions := make([]session.Session, 0, legacyCount)
	for i := range legacyCount {
		name := fmt.Sprintf("legacy-capacity-%02d", i)
		bound, _ := createLegacyBoundSession(t, service, repo, name, name, "")
		sessions = append(sessions, bound)
	}
	drained := make(chan struct{})
	service.testAfterStartupDrain = func() { close(drained) }
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := service.RuntimeCapacity().TurnSlotsFree; got != 0 {
		t.Fatalf("unproven legacy runtimes advertised %d free slots", got)
	}
	close(release)
	select {
	case <-drained:
	case <-time.After(wait.Deadline):
		t.Fatal("startup did not finish the quarantined runtime proof backlog")
	}
	if got := runner.proofs.Load(); got != legacyCount {
		t.Fatalf("proved %d legacy runtimes, want %d", got, legacyCount)
	}
	if got := service.RuntimeCapacity().TurnSlotsFree; got != sessionRuntimeSlots {
		t.Fatalf("fully proven backlog left %d free slots, want %d", got, sessionRuntimeSlots)
	}
	for _, bound := range sessions {
		if !service.sessionQuarantined(bound.ID) {
			t.Fatalf("runtime proof made legacy session %s runnable", bound.ID)
		}
		if _, err := os.Stat(bound.Workspace); err != nil {
			t.Fatalf("runtime proof touched legacy workspace %s: %v", bound.ID, err)
		}
	}
	if got := runner.runs.Load(); got != 0 {
		t.Fatalf("proof ran %d quarantined turns", got)
	}
	current := mustSession(t, service, sessions[0].ID)
	if _, err := service.Discard(context.Background(), "retire-after-proof", DiscardRequest{
		RetireQuarantined: true, SessionID: current.ID, ExpectedRevision: current.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	if got := service.RuntimeCapacity().TurnSlotsFree; got != sessionRuntimeSlots {
		t.Fatalf("retiring a proven session reintroduced capacity uncertainty: %d free slots", got)
	}
}

func TestQuarantinedRuntimeProofFailureKeepsCapacityBusyUntilRetry(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	runner := &quarantinedRuntimeProofRunner{}
	runner.fail.Store(true)
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, func(*session.Store) Runner { return runner })
	defer service.Stop()
	bound, _ := createLegacyBoundSession(t, service, repo, "legacy-failed-proof", "legacy-failed-proof", "")
	drained := make(chan struct{})
	service.testAfterStartupDrain = func() { close(drained) }
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-drained:
	case <-time.After(wait.Deadline):
		t.Fatal("startup proof attempt did not finish")
	}
	if got := service.RuntimeCapacity().TurnSlotsFree; got != 0 {
		t.Fatalf("failed runtime query advertised %d free slots", got)
	}
	runner.fail.Store(false)
	service.cleanupIdleSessionRuntimes(context.Background())
	if got := service.RuntimeCapacity().TurnSlotsFree; got != sessionRuntimeSlots {
		t.Fatalf("successful retry left %d free slots", got)
	}
	if !service.sessionQuarantined(bound.ID) {
		t.Fatal("successful runtime proof lifted workspace quarantine")
	}
}

func TestQuarantinedOwnedBoxKeepsCapacityBusyUntilExactRemoval(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	root := t.TempDir()
	state := filepath.Join(root, "state")
	runtimePath := filepath.Join(root, "fake-runtime")
	logPath := filepath.Join(root, "runtime.log")
	boxPath := filepath.Join(root, "owned-box")
	runtimeScript := `#!/bin/sh
printf '%s\n' "$*" >> "$COOP_TEST_QUARANTINE_RUNTIME_LOG"
case "$1" in
ps)
	case "$*" in
	*"label=coop.run=$COOP_TEST_QUARANTINE_RUN_ID"*)
		[ -f "$COOP_TEST_QUARANTINE_BOX" ] && echo ownedbox
		;;
	esac
	;;
rm)
	[ "$COOP_TEST_QUARANTINE_REMOVE_FAIL" = 1 ] && exit 42
	rm -f "$COOP_TEST_QUARANTINE_BOX"
	;;
esac
`
	if err := os.WriteFile(runtimePath, []byte(runtimeScript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(boxPath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_QUARANTINE_RUNTIME_LOG", logPath)
	t.Setenv("COOP_TEST_QUARANTINE_BOX", boxPath)
	t.Setenv("COOP_TEST_QUARANTINE_REMOVE_FAIL", "1")
	source := &config.Config{ConfigDir: filepath.Join(root, "source")}
	service, err := newSessionServiceWithTestStorage(t, Config{
		StateRoot: state, SourceConfig: source, CleanupInterval: time.Hour,
		RunnerFactory: func(store *session.Store) Runner {
			return newSessionTurnRunner(source, state, store, runtime.Runtime{Name: runtimePath}, "")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	bound, _ := createLegacyBoundSession(t, service, repo, "legacy-owned-box", "legacy-owned-box", "")
	t.Setenv("COOP_TEST_QUARANTINE_RUN_ID", sessionWarmRunID(bound.ID))
	drained := make(chan struct{})
	service.testAfterStartupDrain = func() { close(drained) }
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-drained:
	case <-time.After(wait.Deadline):
		t.Fatal("first owned-box removal attempt did not finish")
	}
	if got := service.RuntimeCapacity().TurnSlotsFree; got != 0 {
		t.Fatalf("live owned box advertised %d free slots", got)
	}
	if _, err := os.Stat(boxPath); err != nil {
		t.Fatalf("failed removal lost the owned box: %v", err)
	}
	t.Setenv("COOP_TEST_QUARANTINE_REMOVE_FAIL", "")
	service.cleanupIdleSessionRuntimes(context.Background())
	if got := service.RuntimeCapacity().TurnSlotsFree; got != sessionRuntimeSlots {
		t.Fatalf("successful exact removal left %d free slots", got)
	}
	if _, err := os.Stat(boxPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned box remains after proof: %v", err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "label="+box.LabelRun+"="+sessionWarmRunID(bound.ID)) ||
		strings.Contains(string(log), "com.docker.compose") || strings.Contains(string(log), box.LabelExecution+"=") {
		t.Fatalf("runtime proof used non-session authority: %s", log)
	}
	if !service.sessionQuarantined(bound.ID) {
		t.Fatal("removing the owned box lifted workspace quarantine")
	}
}

func TestRetiredQuarantinedActiveTurnRestartsWithoutWorkspaceCleanup(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	root := filepath.Join(t.TempDir(), "state")
	runner := &quarantinedRuntimeProofRunner{}
	open := func() *sessionFixture {
		return newTestSessionService(t, root, repo, func(*session.Store) Runner { return runner })
	}
	service := open()
	bound, _ := createLegacyBoundSession(t, service, repo, "legacy-active-retired", "legacy-active-retired", "")
	queued, err := service.Store().SubmitTurn(context.Background(), "legacy-active", session.SubmitTurnRequest{
		SessionID: bound.ID, ExpectedRevision: bound.Revision, Prompt: "interrupted before upgrade",
	})
	if err != nil {
		t.Fatal(err)
	}
	active, ok, err := service.Store().LeaseNextTurn(context.Background(), bound.ID)
	if err != nil || !ok || active.ID != queued.ID {
		t.Fatalf("lease legacy turn = %+v, %t, %v", active, ok, err)
	}
	runID := sessionTurnRunID(bound.ID, active.ID)
	if err := service.Store().BindTurnRuntime(context.Background(), bound.ID, active.ID, "", runID); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	current := mustSession(t, service, bound.ID)
	if _, err := service.Discard(context.Background(), "retire-active", DiscardRequest{
		RetireQuarantined: true, SessionID: current.ID, ExpectedRevision: current.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.Stop(); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	runner = &quarantinedRuntimeProofRunner{release: release}
	service = open()
	defer service.Stop()
	drained := make(chan struct{})
	service.testAfterStartupDrain = func() { close(drained) }
	if err := service.Start(context.Background()); err != nil {
		t.Fatalf("retired active turn broke restart: %v", err)
	}
	if got := service.RuntimeCapacity().TurnSlotsFree; got != 0 {
		t.Fatalf("unproven retired runtime advertised %d free slots", got)
	}
	close(release)
	select {
	case <-drained:
	case <-time.After(wait.Deadline):
		t.Fatal("retired runtime proof did not finish")
	}
	if got := service.RuntimeCapacity().TurnSlotsFree; got != sessionRuntimeSlots {
		t.Fatalf("retired runtime proof left %d free slots", got)
	}
	retained, err := service.Store().GetTurn(context.Background(), bound.ID, active.ID)
	if err != nil || retained.RuntimeRunID != runID || retained.State != active.State {
		t.Fatalf("retired turn history changed: %+v, %v", retained, err)
	}
	if _, err := os.Stat(bound.Workspace); err != nil {
		t.Fatalf("retired workspace changed: %v", err)
	}
	if runner.runs.Load() != 0 {
		t.Fatal("retired turn ran after restart")
	}
}
