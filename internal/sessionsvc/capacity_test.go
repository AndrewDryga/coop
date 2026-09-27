package sessionsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
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
