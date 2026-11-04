package sessionsvc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/starttoken"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

type unusedWarmRunner struct {
	proofs, runs int
	fail         bool
}

func (r *unusedWarmRunner) Run(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
	r.runs++
	return turn, errors.New("must not run")
}
func (r *unusedWarmRunner) CleanupSession(context.Context, session.Session) error {
	return errors.New("runtime still has live or unverifiable sandbox activity")
}
func (r *unusedWarmRunner) CleanupUnprovenSessionRuntime(context.Context, session.Session) error {
	r.proofs++
	if r.fail {
		return errors.New("runtime inventory unavailable")
	}
	return nil
}

func unusedWarmFixture(t *testing.T, readOnly bool) (*sessionFixture, session.Session, *unusedWarmRunner, string, []byte) {
	t.Helper()
	runner := &unusedWarmRunner{}
	svc := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), storageTestRepo(t), func(*session.Store) Runner { return runner })
	t.Cleanup(func() { _ = svc.Stop() })
	svc.Job.RepositoryReadOnly = readOnly
	bound, err := svc.CreateRemoteSession(t.Context(), "unused-warm", svc.request(t, "unused-warm"))
	if err != nil {
		t.Fatal(err)
	}
	identity := forkspace.Identity{Name: bound.ForkName, Generation: forkspace.Generation(bound.ForkGeneration)}
	record := forkspace.ExecutionRecord{Version: 1, ID: strings.Repeat("8", 32), Kind: forkspace.ExecutionRemoteSession,
		Role: forkspace.ExecutionRoleWarm, Workspace: bound.Workspace, Fork: &identity,
		SourceID: sessionWarmRunID(bound.ID), PID: 1 << 30, Token: starttoken.Foreign(), StartedAt: time.Now().UTC()}
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(forkspace.StateDir(bound.Repository), "executions")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, record.ID+".json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	svc.historicalPending[bound.ID] = struct{}{}
	return svc, bound, runner, path, body
}

// A container restart left one unused admission-ready warm receipt in another
// PID namespace. Its uncertain owner blocked every session slot indefinitely.
func TestUnusedForeignWarmQuarantinesWithoutDeletingEvidenceAndRestoresCapacity(t *testing.T) {
	svc, bound, runner, path, body := unusedWarmFixture(t, true)
	if svc.RuntimeCapacity().TurnSlotsFree != 0 {
		t.Fatal("unknown runtime must hold capacity")
	}
	svc.cleanupIdleSessionRuntimes(t.Context())
	if svc.RuntimeCapacity().TurnSlotsFree != sessionRuntimeSlots || !svc.sessionQuarantined(bound.ID) {
		t.Fatal("unused foreign warm runtime stranded all capacity instead of preserving quarantine")
	}
	if runner.proofs != 1 || runner.runs != 0 {
		t.Fatalf("proofs=%d runs=%d", runner.proofs, runner.runs)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(body) {
		t.Fatalf("foreign evidence changed: %v", err)
	}
	if _, err := os.Stat(bound.Workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitTurn(t.Context(), "quarantined-submit", session.SubmitTurnRequest{SessionID: bound.ID, ExpectedRevision: bound.Revision, Prompt: "must not run"}); session.CodeOf(err) != session.CodeInvalidSessionState {
		t.Fatalf("submit = %v", err)
	}
	if _, err := svc.PrepareSession(t.Context(), bound.ID, bound.Revision); session.CodeOf(err) != session.CodeInvalidSessionState {
		t.Fatalf("prepare = %v", err)
	}
	if _, err := svc.GetChanges(t.Context(), bound.ID); session.CodeOf(err) != session.CodeInvalidSessionState {
		t.Fatalf("workspace use = %v", err)
	}
	closed, err := svc.Close(t.Context(), "quarantined-close", session.CloseSessionRequest{SessionID: bound.ID, ExpectedRevision: bound.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PlanDiscard(t.Context(), "quarantined-plan", PlanDiscardRequest{SessionID: bound.ID, ExpectedRevision: closed.Revision}); session.CodeOf(err) != session.CodeInvalidSessionState {
		t.Fatalf("ordinary discard = %v", err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				svc.sessionQuarantined(bound.ID)
			}
		}
	}()
	_, err = svc.Discard(t.Context(), "quarantined-retire", DiscardRequest{SessionID: bound.ID, ExpectedRevision: closed.Revision, RetireQuarantined: true})
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(body) {
		t.Fatalf("retirement changed evidence: %v", err)
	}
	if _, err := os.Stat(bound.Workspace); err != nil {
		t.Fatal(err)
	}
}

func TestUnusedForeignWarmKeepsCapacityUntilExactRuntimeProof(t *testing.T) {
	svc, bound, runner, path, _ := unusedWarmFixture(t, true)
	runner.fail = true
	svc.cleanupIdleSessionRuntimes(t.Context())
	if svc.RuntimeCapacity().TurnSlotsFree != 0 {
		t.Fatal("failed inventory freed capacity")
	}
	if !svc.sessionQuarantined(bound.ID) {
		t.Fatal("unusable session remains admissible")
	}
	runner.fail = false
	svc.cleanupIdleSessionRuntimes(t.Context())
	if svc.RuntimeCapacity().TurnSlotsFree != sessionRuntimeSlots {
		t.Fatal("proof retry did not restore capacity")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestUnusedWarmQuarantineRejectsBusyOrCrossedOrLiveEvidence(t *testing.T) {
	for _, kind := range []string{"writable", "queued", "active", "completed", "source", "workspace", "generation", "role", "local", "unknown", "mixed", "malformed", "missing"} {
		t.Run(kind, func(t *testing.T) {
			svc, bound, runner, path, body := unusedWarmFixture(t, kind != "writable")
			var record forkspace.ExecutionRecord
			if err := json.Unmarshal(body, &record); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "queued", "active", "completed":
				turn, err := svc.Store().SubmitTurn(t.Context(), "existing-turn", session.SubmitTurnRequest{SessionID: bound.ID, ExpectedRevision: bound.Revision, Prompt: "retained turn"})
				if err != nil {
					t.Fatal(err)
				}
				if kind != "queued" {
					if _, ok, err := svc.Store().LeaseNextTurn(t.Context(), bound.ID); err != nil || !ok {
						t.Fatalf("lease: %v %v", ok, err)
					}
				}
				if kind == "completed" {
					if _, err := svc.Store().MarkTurnSendIntent(t.Context(), bound.ID, turn.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := svc.Store().MarkTurnSent(t.Context(), bound.ID, turn.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := svc.Store().CompleteTurn(t.Context(), session.CompleteTurnRequest{SessionID: bound.ID, TurnID: turn.ID, Message: "saved answer"}); err != nil {
						t.Fatal(err)
					}
				}
			case "source":
				record.SourceID = "session-another-owner"
			case "workspace":
				record.Workspace = filepath.Join(t.TempDir(), "other")
			case "generation":
				record.Fork.Generation = forkspace.Generation(strings.Repeat("9", 32))
			case "role":
				record.Role = forkspace.ExecutionRoleActiveTurn
			case "local":
				record.Token = starttoken.Stale()
			case "unknown":
				record.Token = "unverifiable"
			case "mixed":
				other := record
				other.ID = strings.Repeat("9", 32)
				data, _ := json.Marshal(other)
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), other.ID+".json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "malformed" && kind != "missing" {
				data, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := mustSession(t, svc, bound.ID)
			if svc.quarantineUnusedWarmRuntime(t.Context(), bound) {
				t.Fatal("unsafe evidence authorized quarantine")
			}
			if svc.sessionQuarantined(bound.ID) || runner.proofs != 0 || svc.RuntimeCapacity().TurnSlotsFree != 0 {
				t.Fatal("refused quarantine changed runtime authority")
			}
			if after := mustSession(t, svc, bound.ID); !reflect.DeepEqual(before, after) {
				t.Fatal("refused quarantine changed history")
			}
		})
	}
}

func TestUnusedWarmQuarantineFencesAnAlreadyValidatedSubmit(t *testing.T) {
	svc, bound, _, _, _ := unusedWarmFixture(t, true)
	validated, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseSubmit := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseSubmit)
	svc.testBeforeSubmitAdmission = func() { close(validated); <-release }
	result := make(chan error, 1)
	go func() {
		_, err := svc.SubmitTurn(t.Context(), "racing-submit", session.SubmitTurnRequest{
			SessionID: bound.ID, ExpectedRevision: bound.Revision, Prompt: "must not run"})
		result <- err
	}()
	select {
	case <-validated:
	case <-time.After(wait.Deadline):
		t.Fatal("submit did not finish prevalidation")
	}
	if !svc.quarantineUnusedWarmRuntime(t.Context(), bound) {
		t.Fatal("unused exact warm session was not quarantined")
	}
	releaseSubmit()
	select {
	case err := <-result:
		if session.CodeOf(err) != session.CodeInvalidSessionState {
			t.Fatalf("racing admission = %v", err)
		}
	case <-time.After(wait.Deadline):
		t.Fatal("racing submit did not finish")
	}
	turns, err := svc.Store().ListTurns(t.Context(), bound.ID, 0, 1)
	if err != nil || len(turns) != 0 {
		t.Fatalf("quarantined session admitted work: %v %v", turns, err)
	}
}

func TestUnusedWarmQuarantineRetriesAProofFirstSeenAfterStartup(t *testing.T) {
	svc, bound, runner, _, _ := unusedWarmFixture(t, true)
	delete(svc.historicalPending, bound.ID)
	svc.runtimeCleanupFailed(bound.ID)
	runner.fail = true
	svc.cleanupIdleSessionRuntimes(t.Context())
	runner.fail = false
	svc.cleanupIdleSessionRuntimes(t.Context())
	if svc.RuntimeCapacity().TurnSlotsFree != sessionRuntimeSlots {
		t.Fatal("new quarantine lost its runtime-proof retry and stranded capacity")
	}
}

func TestUnusedWarmQuarantineIsReestablishedBeforeStartupCanAdmitWork(t *testing.T) {
	svc, bound, _, path, body := unusedWarmFixture(t, true)
	if !svc.quarantineUnusedWarmRuntime(t.Context(), bound) {
		t.Fatal("initial quarantine failed")
	}
	if err := svc.Stop(); err != nil {
		t.Fatal(err)
	}
	runner := &unusedWarmRunner{}
	reopened := newTestSessionService(t, svc.stateRoot, bound.Repository, func(*session.Store) Runner { return runner })
	t.Cleanup(func() { _ = reopened.Stop() })
	admitted := false
	reopened.testStartupExecution = func(id string) {
		if id == bound.ID {
			admitted = true
		}
	}
	if err := reopened.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if admitted || !reopened.sessionQuarantined(bound.ID) {
		t.Fatal("restart exposed the preserved foreign warm session before rebuilding quarantine")
	}
	if _, err := reopened.SubmitTurn(t.Context(), "restart-submit", session.SubmitTurnRequest{SessionID: bound.ID, ExpectedRevision: bound.Revision, Prompt: "must not run"}); session.CodeOf(err) != session.CodeInvalidSessionState {
		t.Fatalf("submit after restart = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(body) {
		t.Fatalf("restart changed evidence: %v", err)
	}
}

func TestRuntimeReservationRechecksNewPendingCustodyAtAdmission(t *testing.T) {
	s := &Service{historicalPending: make(map[string]struct{})}
	checked, release := make(chan struct{}), make(chan struct{})
	s.testBeforeRuntimeReservation = func() { close(checked); <-release }
	result := make(chan bool, 1)
	go func() { ok, _ := s.reserveRuntimeSlot("new"); result <- ok }()
	<-checked
	s.runtimeMu.Lock()
	s.historicalMu.Lock()
	s.historicalPending["old"] = struct{}{}
	s.historicalMu.Unlock()
	s.runtimeMu.Unlock()
	close(release)
	if <-result {
		t.Fatal("reservation ignored custody discovered before its admission lock")
	}
}
