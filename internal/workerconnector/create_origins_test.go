package workerconnector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

type originAPI struct {
	eventAPI
	operation createOperation
	lookup    func(context.Context, Request) (json.RawMessage, error)
}

func (a *originAPI) Forward(ctx context.Context, request Request, body io.Reader) (*http.Response, error) {
	return forwardTestAPI(ctx, a, request, body)
}

func (a *originAPI) Do(ctx context.Context, request Request) (json.RawMessage, error) {
	a.requests = append(a.requests, request)
	if request.Method == "GET" && strings.HasPrefix(request.Path, "/v1/operations?") {
		if a.lookup != nil {
			return a.lookup(ctx, request)
		}
		return json.Marshal(a.operation)
	}
	return json.Marshal(map[string]any{"operation": a.operation})
}

func newOriginFixture(t *testing.T) (*Executor, *originAPI, workerproto.Command) {
	t.Helper()
	now := time.Date(2026, 9, 5, 18, 0, 0, 0, time.UTC)
	api := &originAPI{operation: createOperation{ID: "create-1", Method: "CreateRemoteSession", State: "running"}}
	api.events = []workerproto.SessionEvent{{ID: "evt-1", SessionID: "coop-session-1", Sequence: 1, Type: "session.created", Version: 1, OccurredAt: now}}
	executor, err := NewExecutor(ExecutorConfig{API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a"})
	if err != nil {
		t.Fatal(err)
	}
	return executor, api, createCommand(now.Add(time.Hour))
}

// replaceCreateOrigin makes origin the only record of its session, in the folder its state
// belongs to, and returns that file.
func replaceCreateOrigin(t *testing.T, j *journal, origin createOrigin) string {
	t.Helper()
	pending, settled := j.createOriginPath(origin.SessionRef), j.settledCreateOriginPath(origin.SessionRef)
	for _, path := range []string{pending, settled} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if err := j.writeCreateOriginLocked(origin); err != nil {
		t.Fatal(err)
	}
	if origin.State == "pending" {
		return pending
	}
	return settled
}

func originReceipt(t *testing.T, executor *Executor, command workerproto.Command, resource string) journalEntry {
	t.Helper()
	entry, err := executor.journal.begin(command)
	if err != nil {
		t.Fatal(err)
	}
	entry, err = executor.journal.complete(entry, workerproto.CommandResult{
		CommandID: command.CommandID, OperationKey: command.IdempotencyKey, State: "succeeded",
		Resource: responsePayload(json.RawMessage(resource)), Error: json.RawMessage("null"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// One missing retained operation produced 1,333 warnings in two hours. Its
// absence is not terminal proof: back off lookup, preserving custody and peers.
func TestMissingCreateOriginWaitsWithoutRewritingCustodyOrStarvingActivity(t *testing.T) {
	executor, api, command := newOriginFixture(t)
	now := executor.now()
	executor.now = func() time.Time { return now }
	if _, err := executor.Execute(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	path := executor.journal.createOriginPath(command.SessionRef)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.journal.bindEventStream(workerproto.Command{SessionRef: "healthy", PlacementGeneration: 1}, "coop-session-1"); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	api.lookup = func(context.Context, Request) (json.RawMessage, error) {
		lookups++
		return nil, &APIError{Status: 404, Code: "operation_not_found", Detail: "not found"}
	}
	if batches, err := executor.collectActivity(context.Background(), maximumPollBytes); err == nil || len(batches) != 1 {
		t.Fatalf("first diagnosis or healthy activity lost: %v, %v", batches, err)
	}
	for range 20 {
		if batches, err := executor.collectActivity(context.Background(), maximumPollBytes); err != nil || len(batches) != 1 {
			t.Fatalf("waiting origin starved activity or repeated error: %v, %v", batches, err)
		}
	}
	if lookups != 1 {
		t.Fatalf("waiting origin repeated %d lookups", lookups)
	}
	for _, delay := range []time.Duration{5, 10, 20, 40, 80, 120, 120} {
		now = now.Add(delay * time.Second)
		previous := lookups
		if _, err := executor.collectActivity(context.Background(), maximumPollBytes); err == nil || lookups != previous+1 {
			t.Fatalf("due retry lost: %d, %v", lookups, err)
		}
		now = now.Add(time.Millisecond)
		if _, err := executor.collectActivity(context.Background(), maximumPollBytes); err != nil || lookups != previous+1 {
			t.Fatalf("retry did not wait: %d, %v", lookups, err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatalf("lookup rewrote custody: %v", err)
	}
	if _, err := os.Stat(executor.journal.eventStreamPath(command.SessionRef)); !os.IsNotExist(err) {
		t.Fatal("missing operation manufactured a stream")
	}
	restarted, err := NewExecutor(ExecutorConfig{API: api, JournalDir: filepath.Dir(executor.journal.origins), Now: executor.now, WorkerID: "worker-a"})
	if err != nil {
		t.Fatal(err)
	}
	previous := lookups
	if _, err := restarted.collectActivity(context.Background(), maximumPollBytes); err == nil || lookups != previous+1 {
		t.Fatalf("restart did not reconcile immediately: %d, %v", lookups, err)
	}
	// A new, exactly recorded generation must not inherit the old lookup wait.
	next := command
	next.CommandID, next.IdempotencyKey, next.PlacementGeneration = "create-next", "key-next", 2
	api.operation = createOperation{ID: "create-2", Method: "CreateRemoteSession", State: "running"}
	if _, err := executor.Execute(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	api.lookup = nil
	api.operation.State, api.operation.ResourceType, api.operation.ResourceID = "succeeded", "session", "coop-session-1"
	if _, err := executor.collectActivity(context.Background(), maximumPollBytes); err != nil {
		t.Fatal(err)
	}
	origin, err := executor.journal.loadCreateOrigin(command.SessionRef)
	if err != nil || origin.PlacementGeneration != 2 || origin.CoopSessionID != "coop-session-1" {
		t.Fatalf("new generation did not bind: %+v, %v", origin, err)
	}
}

func TestCreateOriginDeniesUnprovenOrConflictingOperationIdentity(t *testing.T) {
	for _, corrupt := range []func(*journalEntry){
		func(entry *journalEntry) { entry.Result.CommandID = "crossed-command" },
		func(entry *journalEntry) { entry.Result.OperationKey = "crossed-key" },
		func(entry *journalEntry) { entry.CommandDigest = strings.Repeat("0", 64) },
		func(entry *journalEntry) { entry.CommandID = "crossed-entry" },
	} {
		executor, _, command := newOriginFixture(t)
		entry := originReceipt(t, executor, command, `{"operation":{"id":"create-1","method":"CreateRemoteSession","state":"running"}}`)
		corrupt(&entry)
		if err := executor.journal.preserveCreateOrigin(entry); err == nil {
			t.Fatal("crossed receipt identity established origin authority")
		}
		if _, err := os.Stat(executor.journal.createOriginPath(command.SessionRef)); !os.IsNotExist(err) {
			t.Fatal("crossed receipt published an origin")
		}
	}
	for _, operation := range []createOperation{
		{ID: "other-operation", Method: "CreateRemoteSession", State: "succeeded", ResourceType: "session", ResourceID: "coop-session-1"},
		{ID: "create-1", Method: "SubmitTurn", State: "succeeded", ResourceType: "session", ResourceID: "coop-session-1"},
		{ID: "create-1", Method: "CreateRemoteSession", State: "succeeded", ResourceType: "turn", ResourceID: "coop-session-1"},
		{ID: "create-1", Method: "CreateRemoteSession", State: "succeeded", ResourceType: "session"},
		{ID: "create-1", Method: "CreateRemoteSession", State: "unknown"},
	} {
		t.Run(fmt.Sprintf("%s-%s-%s-%s", operation.ID, operation.Method, operation.State, operation.ResourceType), func(t *testing.T) {
			executor, api, command := newOriginFixture(t)
			if _, err := executor.Execute(context.Background(), command); err != nil {
				t.Fatal(err)
			}
			api.operation = operation
			batches, err := executor.collectActivity(context.Background(), maximumPollBytes)
			if err == nil || len(batches) != 0 {
				t.Fatalf("unproven operation accepted: %v, %v", batches, err)
			}
			if _, err := os.Stat(executor.journal.eventStreamPath(command.SessionRef)); !os.IsNotExist(err) {
				t.Fatalf("unproven stream created: %v", err)
			}
			lookup := api.requests[len(api.requests)-1]
			if lookup.Method != "GET" || lookup.Path != "/v1/operations?"+(url.Values{"key": {command.IdempotencyKey}}).Encode() {
				t.Fatalf("lookup lost originating key: %+v", lookup)
			}
		})
	}
	for _, resource := range []string{
		`{"session":{"id":"coop-session-1"},"operation":{"state":"failed"}}`,
		`{"session":{"id":"coop-session-1"},"operation":{"state":"uncertain"}}`,
		`{"session":{"id":"coop-session-1"},"operation":{"id":"` + strings.Repeat("x", 257) + `","state":"succeeded"}}`,
		`{"session":{"id":"coop-session-1"},"operation":{"id":"\u0000","state":"succeeded"}}`,
		`{"session":{"id":"coop-session-1"},"operation":{"method":"SubmitTurn","state":"succeeded"}}`,
	} {
		executor, _, command := newOriginFixture(t)
		entry := originReceipt(t, executor, command, resource)
		if err := executor.journal.preserveCreateOrigin(entry); err == nil {
			t.Fatal("malformed synchronous operation accepted")
		}
		if _, err := os.Stat(executor.journal.createOriginPath(command.SessionRef)); !os.IsNotExist(err) {
			t.Fatalf("malformed origin persisted: %v", err)
		}
	}
}

// An origin that cannot be preserved keeps its own create receipt and reports the failure, while
// every other command still runs and is acknowledged.
func TestCreateOriginPreservationFailureKeepsOnlyItsReceiptAndReportsAfterCommands(t *testing.T) {
	executor, api, command := newOriginFixture(t)
	if err := os.Chmod(executor.journal.origins, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(executor.journal.origins, 0o700) })
	if _, err := executor.Execute(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	healthy := command
	healthy.CommandID, healthy.IdempotencyKey, healthy.Kind = "healthy", "healthy-key", "api_request"
	healthy.Payload = apiPayload("GET", "/v1/sessions/coop-session-1", nil)
	if _, err := executor.Execute(context.Background(), healthy); err != nil {
		t.Fatal(err)
	}
	delivered := healthy
	delivered.CommandID, delivered.IdempotencyKey = "delivered", "delivered-key"
	transport := &scriptedTransport{responses: []workerproto.Response{{Version: workerproto.Version, PollRef: "poll:worker-a:1", ServerTime: executor.now(),
		AcknowledgedResultCommandIDs: []string{command.CommandID, healthy.CommandID}, Commands: []workerproto.Command{delivered}}}}
	connector, err := NewConnector(ConnectorConfig{Executor: executor, Now: executor.now, Hello: func(_ context.Context, clock time.Time) workerproto.WorkerHello { return hello(clock) }, Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	if err := connector.PollOnce(context.Background()); err == nil {
		t.Fatal("preservation error lost")
	}
	if _, err := os.Stat(executor.journal.path(command.CommandID)); err != nil {
		t.Fatal("lost sole recoverable create receipt")
	}
	if _, err := os.Stat(executor.journal.path(healthy.CommandID)); !os.IsNotExist(err) {
		t.Fatal("healthy receipt was not acknowledged")
	}
	if len(api.requests) != 3 {
		t.Fatalf("independent command did not execute: %d calls", len(api.requests))
	}
	if err := os.Chmod(executor.journal.origins, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := executor.journal.acknowledgeResults([]string{command.CommandID}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(executor.journal.path(command.CommandID)); !os.IsNotExist(err) {
		t.Fatal("repaired origin did not release receipt")
	}
	info, err := os.Stat(executor.journal.createOriginPath(command.SessionRef))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("origin protection: %v, %v", info, err)
	}
}

func TestCreateOriginsFenceGenerationsAndNeverResurrectDiscardedStreams(t *testing.T) {
	executor, api, first := newOriginFixture(t)
	clock := executor.now()
	executor.now = func() time.Time { return clock }
	if _, err := executor.Execute(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	old, _ := executor.journal.loadCreateOrigin(first.SessionRef)
	api.operation.State, api.operation.ResourceType, api.operation.ResourceID = "succeeded", "session", "coop-session-1"
	if err := executor.resolveCreateOrigin(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	second := first
	second.CommandID, second.IdempotencyKey, second.PlacementGeneration = "create-second", "key-second", 2
	api.operation = createOperation{ID: "create-2", Method: "CreateRemoteSession", State: "running"}
	if _, err := executor.Execute(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := executor.journal.bindCreateOrigin(old, "old-session"); err == nil {
		t.Fatal("stale lookup overwrote newer pending origin")
	}
	for _, state := range []string{"uncertain", "failed"} {
		api.operation.State = state
		if batches, err := executor.collectActivity(context.Background(), maximumPollBytes); err != nil || len(batches) != 0 {
			t.Fatalf("higher %s origin leaked lower activity: %v, %v", state, batches, err)
		}
		clock = clock.Add(5 * time.Second) // an uncertain create is looked up again after its wait
	}
	if _, err := executor.Execute(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	current, err := executor.journal.loadCreateOrigin(first.SessionRef)
	if err != nil || current.PlacementGeneration != 2 || current.State != "failed" {
		t.Fatalf("lower replay lost failed watermark: %+v, %v", current, err)
	}
	conflict := second
	conflict.CommandID, conflict.IdempotencyKey = "conflict", "conflict-key"
	entry := originReceipt(t, executor, conflict, `{"operation":{"id":"other","method":"CreateRemoteSession","state":"running"}}`)
	if err := executor.journal.preserveCreateOrigin(entry); err == nil {
		t.Fatal("same-generation conflicting origin accepted")
	}
	third := second
	third.CommandID, third.IdempotencyKey, third.PlacementGeneration = "create-third", "key-third", 3
	api.operation = createOperation{ID: "create-3", Method: "CreateRemoteSession", State: "uncertain"}
	if _, err := executor.Execute(context.Background(), third); err != nil {
		t.Fatal(err)
	}
	_, _ = executor.collectActivity(context.Background(), maximumPollBytes)
	api.operation.State, api.operation.ResourceType, api.operation.ResourceID = "succeeded", "session", "coop-session-1"
	clock = clock.Add(5 * time.Second)
	_, _ = executor.collectActivity(context.Background(), maximumPollBytes)
	api.events[0].Type = "workspace.discarded"
	if batches, err := executor.collectActivity(context.Background(), maximumPollBytes); err != nil || len(batches) != 1 {
		t.Fatalf("uncertain did not recover: %v, %v", batches, err)
	}
	if err := executor.journal.acknowledgeEvents([]workerproto.EventAcknowledgement{{SessionRef: third.SessionRef, PlacementGeneration: 1, Sequence: 1}}); err == nil {
		t.Fatal("old terminal ACK removed new generation")
	}
	if err := executor.journal.acknowledgeEvents([]workerproto.EventAcknowledgement{{SessionRef: third.SessionRef, PlacementGeneration: 3, Sequence: 1}}); err != nil {
		t.Fatal(err)
	}
	for _, replay := range []workerproto.Command{first, third} {
		if _, err := executor.Execute(context.Background(), replay); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(executor.journal.eventStreamPath(third.SessionRef)); !os.IsNotExist(err) {
		t.Fatal("discarded stream resurrected")
	}
}

func TestLegacyTerminalStreamRetainsItsGenerationWithoutInventingAnOrigin(t *testing.T) {
	for _, lowerOrigin := range []bool{false, true} {
		t.Run(fmt.Sprintf("lower-origin-%t", lowerOrigin), func(t *testing.T) {
			executor, _, command := newOriginFixture(t)
			if lowerOrigin {
				if _, err := executor.Execute(context.Background(), command); err != nil {
					t.Fatal(err)
				}
			}
			stream := eventStream{Version: eventStreamVersion, SessionRef: command.SessionRef, PlacementGeneration: 2, CoopSessionID: "coop-session-1", LastPublishedSequence: 1, TerminalSequence: 1}
			if err := executor.journal.writeEventStream(executor.journal.eventStreamPath(command.SessionRef), stream); err != nil {
				t.Fatal(err)
			}
			if err := executor.journal.acknowledgeEvents([]workerproto.EventAcknowledgement{{SessionRef: command.SessionRef, PlacementGeneration: 2, Sequence: 1}}); err != nil {
				t.Fatal(err)
			}
			var entry journalEntry
			if lowerOrigin {
				var err error
				entry, err = executor.journal.read(executor.journal.path(command.CommandID))
				if err != nil {
					t.Fatal(err)
				}
			} else {
				entry = originReceipt(t, executor, command, `{"session":{"id":"older-session"}}`)
			}
			if err := executor.journal.preserveCreateOrigin(entry); err != nil {
				t.Fatal(err)
			}
			kept, err := executor.journal.readEventStream(executor.journal.eventStreamPath(command.SessionRef))
			if err != nil || kept.PlacementGeneration != 2 || kept.AcknowledgedSequence != 1 {
				t.Fatalf("legacy floor lost: %+v, %v", kept, err)
			}
			if batches, err := executor.collectActivity(context.Background(), maximumPollBytes); err != nil || len(batches) != 0 {
				t.Fatalf("dormant legacy floor was polled: %v, %v", batches, err)
			}
		})
	}
}

func TestLegacyCreateReceiptMigratesBeforeAcknowledgementWithoutResettingCursors(t *testing.T) {
	executor, _, command := newOriginFixture(t)
	originReceipt(t, executor, command, `{"session":{"id":"coop-session-1"}}`)
	stream := eventStream{Version: eventStreamVersion, SessionRef: command.SessionRef, PlacementGeneration: 1,
		CoopSessionID: "coop-session-1", AcknowledgedSequence: 2, LastPublishedSequence: 4}
	if err := executor.journal.writeEventStream(executor.journal.eventStreamPath(command.SessionRef), stream); err != nil {
		t.Fatal(err)
	}
	if err := executor.journal.acknowledgeResults([]string{command.CommandID}); err != nil {
		t.Fatal(err)
	}
	origin, err := executor.journal.loadCreateOrigin(command.SessionRef)
	if err != nil || origin.State != "bound" || origin.CoopSessionID != stream.CoopSessionID {
		t.Fatalf("legacy origin was not preserved: %+v, %v", origin, err)
	}
	current, err := executor.journal.readEventStream(executor.journal.eventStreamPath(command.SessionRef))
	if err != nil || current != stream {
		t.Fatalf("legacy cursors changed: %+v, %v", current, err)
	}
	if _, err := os.Stat(executor.journal.path(command.CommandID)); !os.IsNotExist(err) {
		t.Fatal("migrated receipt was not acknowledged")
	}
}

func TestActivityQuarantinesCorruptOriginAndScansPastSlowLookups(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor, api, command := newOriginFixture(t)
		command.SessionRef = "a-slow"
		if _, err := executor.Execute(context.Background(), command); err != nil {
			t.Fatal(err)
		}
		for _, ref := range []string{"b-corrupt", "c-healthy"} {
			if err := executor.journal.bindEventStream(workerproto.Command{SessionRef: ref, PlacementGeneration: 1}, "coop-session-1"); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(executor.journal.createOriginPath("b-corrupt"), []byte("broken"), 0o600); err != nil {
			t.Fatal(err)
		}
		api.lookup = func(ctx context.Context, _ Request) (json.RawMessage, error) { <-ctx.Done(); return nil, ctx.Err() }
		start := time.Now()
		if batches, err := executor.collectActivity(context.Background(), maximumPollBytes); err == nil || len(batches) != 0 || time.Since(start) != maximumEventDelay {
			t.Fatalf("slow lookup escaped budget: %v, %v", batches, err)
		}
		batches, err := executor.collectActivity(context.Background(), maximumPollBytes)
		if err == nil || len(batches) != 1 || batches[0].SessionRef != "c-healthy" {
			t.Fatalf("failed origins starved healthy activity: %v, %v", batches, err)
		}
		if _, err := os.Stat(executor.journal.createOriginPath("b-corrupt")); err != nil {
			t.Fatal("corrupt custody erased")
		}
		api.lookup = nil
		delivered := command
		delivered.SessionRef = "unrelated-later-command"
		delivered.CommandID, delivered.IdempotencyKey, delivered.Kind = "get-new", "get-new-key", "api_request"
		delivered.Payload = apiPayload("GET", "/v1/sessions/coop-session-1", nil)
		// Settle the existing create first, so the next poll actually collects activity.
		if err := executor.journal.acknowledgeResults([]string{command.CommandID}); err != nil {
			t.Fatal(err)
		}
		transport := &scriptedTransport{responses: []workerproto.Response{{Version: workerproto.Version, PollRef: "poll:worker-a:1", ServerTime: executor.now(), Commands: []workerproto.Command{delivered}}}}
		connector, err := NewConnector(ConnectorConfig{Executor: executor, Now: executor.now, Hello: func(_ context.Context, clock time.Time) workerproto.WorkerHello { return hello(clock) }, Transport: transport})
		if err != nil {
			t.Fatal(err)
		}
		if err := connector.PollOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "worker create origin") {
			t.Fatalf("activity failure was not reported: %v", err)
		}
		if entry, err := executor.journal.read(executor.journal.path(delivered.CommandID)); err != nil || entry.State != "completed" {
			t.Fatal("activity failure prevented independent command execution")
		}
		if _, err := os.Stat(executor.journal.createOriginPath(delivered.SessionRef)); !os.IsNotExist(err) {
			t.Fatal("a later command invented create authority")
		}
	})
}

func TestActivityMetadataLockExcludesIndependentJournalWriters(t *testing.T) {
	executor, _, command := newOriginFixture(t)
	other, err := openJournal(filepath.Dir(executor.journal.dir))
	if err != nil {
		t.Fatal(err)
	}
	entry := originReceipt(t, executor, command, `{"operation":{"id":"create-1","method":"CreateRemoteSession","state":"running"}}`)
	if err := executor.journal.withActivityLock(func() error {
		if err := other.preserveCreateOrigin(entry); err == nil {
			t.Fatal("second journal writer crossed authority lock")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := other.preserveCreateOrigin(entry); err != nil {
		t.Fatal(err)
	}
}

// An uncertain create was looked up every poll, once a second, for as long as the daemon took to
// settle it. It now waits like a failed lookup, without a warning, and a running create is still
// looked up every poll.
func TestUncertainCreateLookupsBackOffQuietly(t *testing.T) {
	executor, api, command := newOriginFixture(t)
	clock := executor.now()
	executor.now = func() time.Time { return clock }
	if _, err := executor.Execute(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	api.lookup = func(context.Context, Request) (json.RawMessage, error) {
		lookups++
		return json.Marshal(api.operation)
	}
	api.operation.State = "running"
	for range 3 {
		if _, err := executor.collectActivity(context.Background(), maximumPollBytes); err != nil {
			t.Fatal(err)
		}
	}
	if lookups != 3 {
		t.Fatalf("a running create was looked up %d times in 3 polls, want every poll", lookups)
	}
	lookups, api.operation.State = 0, "uncertain"
	for range 20 {
		if _, err := executor.collectActivity(context.Background(), maximumPollBytes); err != nil {
			t.Fatalf("an uncertain create was reported: %v", err)
		}
	}
	if lookups != 1 {
		t.Fatalf("an uncertain create was looked up %d times in 20 polls, want once", lookups)
	}
	for _, delay := range []time.Duration{5, 10, 20} {
		clock = clock.Add(delay * time.Second)
		previous := lookups
		if _, err := executor.collectActivity(context.Background(), maximumPollBytes); err != nil || lookups != previous+1 {
			t.Fatalf("due lookup after %ds: %d lookups, %v", delay, lookups-previous, err)
		}
	}
	api.operation.State, api.operation.ResourceType, api.operation.ResourceID = "succeeded", "session", "coop-session-1"
	clock = clock.Add(40 * time.Second)
	if _, err := executor.collectActivity(context.Background(), maximumPollBytes); err != nil {
		t.Fatal(err)
	}
	if origin, err := executor.journal.loadCreateOrigin(command.SessionRef); err != nil || origin.State != "bound" {
		t.Fatalf("settled create did not bind: %+v, %v", origin, err)
	}
	// Binding moved it out of the folder every poll lists.
	if _, err := os.Stat(executor.journal.createOriginPath(command.SessionRef)); !os.IsNotExist(err) {
		t.Fatalf("a bound create stayed in create-origins/: %v", err)
	}
}

// Every poll opened and decoded every create origin the worker ever made, one file per session it
// ever created. A settled origin now lives in a folder no poll lists, so after a restart a poll
// opens none of them; the settled origin of a live stream is read once, then only checked by stat.
func TestPollOpensNoSettledCreateOrigin(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	executor, api, command := newOriginFixture(t)
	settledOrigin := func(index int, state string) createOrigin {
		origin := createOrigin{
			Version: createOriginVersion, WorkerID: "worker-a", SessionRef: fmt.Sprintf("settled-%03d", index),
			PlacementGeneration: 1, CommandID: fmt.Sprintf("command-%03d", index), CommandDigest: strings.Repeat("a", 64),
			OperationKey: fmt.Sprintf("key-%03d", index), OperationID: fmt.Sprintf("create-%03d", index), State: state,
		}
		if state == "bound" {
			origin.CoopSessionID = "coop-session-1"
		}
		return origin
	}
	// The layout before settled/: every origin in create-origins/. Three sessions are still live.
	var live []string
	for index := range 203 {
		origin := settledOrigin(index, "failed")
		if index >= 200 {
			origin = settledOrigin(index, "bound")
			live = append(live, origin.SessionRef)
			stream := eventStream{Version: eventStreamVersion, SessionRef: origin.SessionRef, PlacementGeneration: 1,
				CoopSessionID: "coop-session-1", AcknowledgedSequence: 1, LastPublishedSequence: 1}
			if err := executor.journal.writeEventStream(executor.journal.eventStreamPath(origin.SessionRef), stream); err != nil {
				t.Fatal(err)
			}
		}
		if err := executor.journal.writeActivityRecord(executor.journal.createOriginPath(origin.SessionRef), origin); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := executor.Execute(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewExecutor(ExecutorConfig{API: api, JournalDir: filepath.Dir(executor.journal.origins), Now: executor.now, WorkerID: "worker-a"})
	if err != nil {
		t.Fatal(err)
	}
	// An unreadable file fails any open, so a poll that opened one would report it.
	unreadable := func(refs ...string) {
		for _, ref := range refs {
			for _, path := range []string{restarted.journal.createOriginPath(ref), restarted.journal.settledCreateOriginPath(ref)} {
				if err := os.Chmod(path, 0); err == nil {
					t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
				}
			}
		}
	}
	for index := range 200 {
		unreadable(settledOrigin(index, "failed").SessionRef)
	}
	if _, err := restarted.collectActivity(context.Background(), maximumPollBytes); err != nil {
		t.Fatalf("a poll opened settled origins: %v", err)
	}
	unreadable(live...)
	for range 3 {
		targets, err := restarted.activityTargets()
		if err != nil || len(targets) != 4 {
			t.Fatalf("targets = %+v, %v; want the pending create and the three live streams", targets, err)
		}
	}
	listed, err := os.ReadDir(restarted.journal.origins)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range listed {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			names = append(names, entry.Name())
		}
	}
	if pending := filepath.Base(restarted.journal.createOriginPath(command.SessionRef)); len(names) != 1 || names[0] != pending {
		t.Fatalf("create-origins/ lists %v, want only the pending create %s", names, pending)
	}
	// A settled session's newer pending create is scanned, and its settled file stays the watermark.
	replaced := createOrigin{
		Version: createOriginVersion, WorkerID: "worker-a", SessionRef: "settled-000", PlacementGeneration: 2,
		CommandID: "command-new", CommandDigest: strings.Repeat("b", 64), OperationKey: "key-new", OperationID: "create-new", State: "pending",
	}
	if err := restarted.journal.writeCreateOriginLocked(replaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(restarted.journal.settledCreateOriginPath("settled-000"), 0o600); err != nil {
		t.Fatal(err)
	}
	targets, err := restarted.activityTargets()
	if err != nil || len(targets) != 5 {
		t.Fatalf("targets = %+v, %v; want the newer pending create too", targets, err)
	}
	if current, err := restarted.journal.loadCreateOrigin("settled-000"); err != nil || current != replaced {
		t.Fatalf("governing origin = %+v, %v; want the newer pending create", current, err)
	}
}

// The move from the flat layout runs at every start and must survive a crash at any point: a
// record is renamed, never copied, so it is in one folder or the other, and a pending file a crash
// left beside its settled twin goes. A second start changes nothing.
func TestOpeningAJournalMovesSettledOriginsOnceAndSafely(t *testing.T) {
	dir := t.TempDir()
	journal, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	origin := func(ref string, generation int, state string) createOrigin {
		value := createOrigin{
			Version: createOriginVersion, WorkerID: "worker-a", SessionRef: ref, PlacementGeneration: generation,
			CommandID: fmt.Sprintf("%s-%d", ref, generation), CommandDigest: strings.Repeat("a", 64),
			OperationKey: fmt.Sprintf("key-%s-%d", ref, generation), OperationID: fmt.Sprintf("op-%s-%d", ref, generation), State: state,
		}
		if state == "bound" {
			value.CoopSessionID = "session-" + ref
		}
		return value
	}
	at := func(value createOrigin) *createOrigin { return &value }
	type layout struct{ pending, settled *createOrigin }
	cases := map[string]struct {
		before, after layout
		governs       createOrigin
	}{
		"flat-settled": {layout{pending: at(origin("flat-settled", 1, "failed"))},
			layout{settled: at(origin("flat-settled", 1, "failed"))}, origin("flat-settled", 1, "failed")},
		"flat-pending": {layout{pending: at(origin("flat-pending", 1, "pending"))},
			layout{pending: at(origin("flat-pending", 1, "pending"))}, origin("flat-pending", 1, "pending")},
		"crash-while-settling": {layout{pending: at(origin("crash-while-settling", 2, "pending")), settled: at(origin("crash-while-settling", 2, "bound"))},
			layout{settled: at(origin("crash-while-settling", 2, "bound"))}, origin("crash-while-settling", 2, "bound")},
		"in-both": {layout{pending: at(origin("in-both", 1, "bound")), settled: at(origin("in-both", 1, "bound"))},
			layout{settled: at(origin("in-both", 1, "bound"))}, origin("in-both", 1, "bound")},
		"newer-pending": {layout{pending: at(origin("newer-pending", 3, "pending")), settled: at(origin("newer-pending", 2, "failed"))},
			layout{pending: at(origin("newer-pending", 3, "pending")), settled: at(origin("newer-pending", 2, "failed"))}, origin("newer-pending", 3, "pending")},
		"newer-flat-settled": {layout{pending: at(origin("newer-flat-settled", 3, "bound")), settled: at(origin("newer-flat-settled", 2, "failed"))},
			layout{settled: at(origin("newer-flat-settled", 3, "bound"))}, origin("newer-flat-settled", 3, "bound")},
		"older-flat-settled": {layout{pending: at(origin("older-flat-settled", 1, "failed")), settled: at(origin("older-flat-settled", 2, "bound"))},
			layout{settled: at(origin("older-flat-settled", 2, "bound"))}, origin("older-flat-settled", 2, "bound")},
		"same-generation-conflict": {layout{pending: at(origin("same-generation-conflict", 2, "failed")), settled: at(origin("same-generation-conflict", 2, "bound"))},
			layout{pending: at(origin("same-generation-conflict", 2, "failed")), settled: at(origin("same-generation-conflict", 2, "bound"))}, origin("same-generation-conflict", 2, "bound")},
	}
	for _, c := range cases {
		ref := c.governs.SessionRef
		for _, place := range []struct {
			path  string
			value *createOrigin
		}{{journal.createOriginPath(ref), c.before.pending}, {journal.settledCreateOriginPath(ref), c.before.settled}} {
			if place.value == nil {
				continue
			}
			if err := journal.writeActivityRecord(place.path, *place.value); err != nil {
				t.Fatal(err)
			}
		}
	}
	damaged := filepath.Join(journal.origins, strings.Repeat("d", 64)+".json")
	if err := os.WriteFile(damaged, []byte("torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	for start := range 2 {
		reopened, err := openJournal(dir)
		if err != nil {
			t.Fatal(err)
		}
		for name, c := range cases {
			ref := c.governs.SessionRef
			for _, place := range []struct {
				path string
				want *createOrigin
			}{{reopened.createOriginPath(ref), c.after.pending}, {reopened.settledCreateOriginPath(ref), c.after.settled}} {
				got, err := reopened.readCreateOrigin(place.path)
				switch {
				case place.want == nil && !os.IsNotExist(err):
					t.Errorf("start %d, %s: %s = %+v, %v; want no file", start, name, place.path, got, err)
				case place.want != nil && (err != nil || got != *place.want):
					t.Errorf("start %d, %s: %s = %+v, %v; want %+v", start, name, place.path, got, err, *place.want)
				}
			}
			if got, err := reopened.loadCreateOrigin(ref); err != nil || got != c.governs {
				t.Errorf("start %d, %s: governing origin = %+v, %v; want %+v", start, name, got, err, c.governs)
			}
		}
		if _, err := os.Stat(damaged); err != nil {
			t.Fatalf("start %d moved or removed a damaged origin: %v", start, err)
		}
	}
}

// A settled origin that no poll lists still decides what its session's stream may do: a stream
// below a settled generation, or bound to another session, is neither narrated nor published.
func TestSettledOriginsStillGuardTheirStreams(t *testing.T) {
	executor, api, _ := newOriginFixture(t)
	settled := func(ref string, generation int, state, sessionID string) {
		t.Helper()
		replaceCreateOrigin(t, executor.journal, createOrigin{
			Version: createOriginVersion, WorkerID: "worker-a", SessionRef: ref, PlacementGeneration: generation,
			CommandID: "create-" + ref, CommandDigest: strings.Repeat("a", 64), OperationKey: "key-" + ref,
			OperationID: "op-" + ref, State: state, CoopSessionID: sessionID,
		})
	}
	streams := map[string]eventStream{}
	for _, ref := range []string{"live", "replaced", "rebound"} {
		stream := eventStream{Version: eventStreamVersion, SessionRef: ref, PlacementGeneration: 1, CoopSessionID: "coop-session-1"}
		if err := executor.journal.writeEventStream(executor.journal.eventStreamPath(ref), stream); err != nil {
			t.Fatal(err)
		}
		streams[ref] = stream
	}
	settled("live", 1, "bound", "coop-session-1")
	settled("replaced", 2, "failed", "")
	settled("rebound", 1, "bound", "coop-session-2")
	batches, err := executor.collectActivity(context.Background(), maximumPollBytes)
	if err != nil || len(batches) != 1 || batches[0].SessionRef != "live" || len(api.afters) != 1 {
		t.Fatalf("batches = %+v, %v after %d event reads; want only the live session narrated", batches, err, len(api.afters))
	}
	for _, ref := range []string{"replaced", "rebound"} {
		if err := executor.journal.publishEventStream(streams[ref], 1, 0); err == nil {
			t.Fatalf("%s: a stream its settled origin forbids was published", ref)
		}
		if err := executor.journal.acknowledgeEvents([]workerproto.EventAcknowledgement{{SessionRef: ref, PlacementGeneration: 1}}); err == nil {
			t.Fatalf("%s: a stream its settled origin forbids was acknowledged", ref)
		}
	}
}

// A crash between writing a settled origin and removing its pending file leaves both. The settled
// one governs: the pending one is not looked up again, and a late bind or failure cannot rewrite it.
func TestPendingFileLeftBesideItsSettledOriginYieldsToIt(t *testing.T) {
	executor, api, command := newOriginFixture(t)
	if _, err := executor.Execute(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	pending, err := executor.journal.loadCreateOrigin(command.SessionRef)
	if err != nil || pending.State != "pending" {
		t.Fatalf("origin = %+v, %v; want pending", pending, err)
	}
	api.operation.State, api.operation.ResourceType, api.operation.ResourceID = "succeeded", "session", "coop-session-1"
	if err := executor.resolveCreateOrigin(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	// Put the pending file back, as a crash before its removal would have left it.
	if err := executor.journal.writeActivityRecord(executor.journal.createOriginPath(command.SessionRef), pending); err != nil {
		t.Fatal(err)
	}
	bound, err := os.ReadFile(executor.journal.settledCreateOriginPath(command.SessionRef))
	if err != nil {
		t.Fatal(err)
	}
	api.requests = nil
	if batches, err := executor.collectActivity(context.Background(), maximumPollBytes); err != nil || len(batches) != 1 {
		t.Fatalf("batches = %+v, %v; want the bound session narrated", batches, err)
	}
	for _, request := range api.requests {
		if strings.HasPrefix(request.Path, "/v1/operations?") {
			t.Fatalf("a settled create was looked up again: %s", request.Path)
		}
	}
	if err := executor.journal.bindCreateOrigin(pending, "coop-session-9"); err != nil {
		t.Fatalf("a late bind of a settled create = %v; want a quiet no-op", err)
	}
	api.operation = createOperation{ID: pending.OperationID, Method: "CreateRemoteSession", State: "failed"}
	if err := executor.resolveCreateOrigin(context.Background(), pending); err == nil {
		t.Fatal("a late failure of a bound create was accepted")
	}
	if after, err := os.ReadFile(executor.journal.settledCreateOriginPath(command.SessionRef)); err != nil || string(after) != string(bound) {
		t.Fatalf("the settled origin was rewritten: %s, %v", after, err)
	}
}
