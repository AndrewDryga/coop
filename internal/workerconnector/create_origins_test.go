package workerconnector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func originReceipt(t *testing.T, executor *Executor, command workerproto.Command, resource string) journalEntry {
	t.Helper()
	entry, err := executor.journal.begin(command)
	if err != nil {
		t.Fatal(err)
	}
	entry, err = executor.journal.complete(entry, workerproto.CommandResult{
		CommandID: command.CommandID, OperationKey: command.IdempotencyKey, State: "succeeded",
		Resource: json.RawMessage(resource), Error: json.RawMessage("null"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return entry
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

func TestCreateOriginPreservationFailureKeepsOnlyItsReceiptAndReportsAfterCommands(t *testing.T) {
	executor, api, command := newOriginFixture(t)
	failure := errors.New("origin directory sync unavailable")
	executor.journal.testSyncActivityDir = func(path string) error {
		if path == executor.journal.origins {
			return failure
		}
		return syncDir(path)
	}
	if _, err := executor.Execute(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	// Rename succeeded: retry must not mistake visibility for durable authority.
	if _, err := executor.journal.readCreateOrigin(executor.journal.createOriginPath(command.SessionRef)); err != nil {
		t.Fatal(err)
	}
	healthy := command
	healthy.CommandID, healthy.IdempotencyKey, healthy.Kind = "healthy", "healthy-key", "get_session"
	healthy.Payload = json.RawMessage(`{"coop_session_id":"coop-session-1"}`)
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
	if err := connector.PollOnce(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("preservation error lost: %v", err)
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
	executor.journal.testSyncActivityDir = nil
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

func TestCreateOriginBindingRetriesDirectoryDurabilityBeforePublishing(t *testing.T) {
	for _, failureDir := range []string{"stream", "origin"} {
		t.Run(failureDir, func(t *testing.T) {
			executor, api, command := newOriginFixture(t)
			if _, err := executor.Execute(context.Background(), command); err != nil {
				t.Fatal(err)
			}
			origin, err := executor.journal.readCreateOrigin(executor.journal.createOriginPath(command.SessionRef))
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := executor.journal.read(executor.journal.path(command.CommandID))
			if err != nil {
				t.Fatal(err)
			}
			failedPath := executor.journal.streams
			if failureDir == "origin" {
				failedPath = executor.journal.origins
			}
			failure := errors.New("directory sync failed after rename")
			executor.journal.testSyncActivityDir = func(path string) error {
				if path == failedPath {
					return failure
				}
				return syncDir(path)
			}
			if err := executor.journal.bindCreateOrigin(origin, "coop-session-1"); err == nil {
				t.Fatal("post-rename sync failure accepted")
			}
			api.operation.State, api.operation.ResourceType, api.operation.ResourceID = "succeeded", "session", "coop-session-1"
			if batches, err := executor.collectActivity(context.Background(), maximumPollBytes); err == nil || len(batches) != 0 {
				t.Fatalf("undurable binding published: %v, %v", batches, err)
			}
			if err := executor.journal.acknowledgeResults([]string{command.CommandID}); failureDir == "origin" && err == nil {
				t.Fatal("undurable origin released receipt")
			}
			executor.journal.testSyncActivityDir = nil
			for range 2 {
				_, _ = executor.collectActivity(context.Background(), maximumPollBytes)
			}
			stream, err := executor.journal.readEventStream(executor.journal.eventStreamPath(command.SessionRef))
			if err != nil || stream.LastPublishedSequence != 1 {
				t.Fatalf("binding did not recover: %+v, %v", stream, err)
			}
			if err := executor.journal.preserveCreateOrigin(receipt); err != nil {
				t.Fatal(err)
			}
			after, err := executor.journal.readEventStream(executor.journal.eventStreamPath(command.SessionRef))
			if err != nil || after != stream {
				t.Fatal("origin replay reset event cursors")
			}
		})
	}
}

func TestCreateOriginsFenceGenerationsAndNeverResurrectDiscardedStreams(t *testing.T) {
	executor, api, first := newOriginFixture(t)
	if _, err := executor.Execute(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	old, _ := executor.journal.readCreateOrigin(executor.journal.createOriginPath(first.SessionRef))
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
	}
	if _, err := executor.Execute(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	current, err := executor.journal.readCreateOrigin(executor.journal.createOriginPath(first.SessionRef))
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
	origin, err := executor.journal.readCreateOrigin(executor.journal.createOriginPath(command.SessionRef))
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
		delivered.CommandID, delivered.IdempotencyKey, delivered.Kind = "get-new", "get-new-key", "get_session"
		delivered.Payload = json.RawMessage(`{"coop_session_id":"coop-session-1"}`)
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
