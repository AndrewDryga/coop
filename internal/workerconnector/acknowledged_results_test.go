package workerconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// One controller reused a create generation twenty times. About 96,000 ACK
// failures then kept healthy event streams off every poll.
func TestAcknowledgedConflictYieldsToActivityWithoutLosingCustody(t *testing.T) {
	executor, api, conflict, original := conflictingReceipt(t)
	now := executor.now()
	var polls []workerproto.Poll
	connect := func() *Connector {
		t.Helper()
		connector, err := NewConnector(ConnectorConfig{Executor: executor, Now: func() time.Time { return now },
			Hello: func(_ context.Context, clock time.Time) workerproto.WorkerHello { return hello(clock) },
			Transport: pollTransportFunc(func(_ context.Context, poll workerproto.Poll) (workerproto.Response, error) {
				polls = append(polls, poll)
				response := workerproto.Response{Version: workerproto.Version, PollRef: poll.PollRef, ServerTime: now}
				for _, result := range poll.CommandResults {
					response.AcknowledgedResultCommandIDs = append(response.AcknowledgedResultCommandIDs, result.CommandID)
				}
				return response, nil
			})})
		if err != nil {
			t.Fatal(err)
		}
		return connector
	}
	healthy := conflict
	healthy.SessionRef = "healthy-session"
	if err := executor.journal.bindEventStream(healthy, "coop-session-1"); err != nil {
		t.Fatal(err)
	}
	connector := connect()
	if err := connector.PollOnce(context.Background()); err == nil {
		t.Fatal("unresolved custody must be reported")
	}
	for range 10 {
		now = now.Add(100 * time.Millisecond)
		if err := connector.PollOnce(context.Background()); err != nil {
			t.Fatalf("unchanged custody flooded the poll: %v", err)
		}
		last := polls[len(polls)-1]
		if len(last.CommandResults) != 0 || len(last.EventBatches) != 1 {
			t.Fatalf("acknowledged receipt starved activity: %+v", last)
		}
	}
	assertReceiptBytes(t, executor, conflict, original)
	lookups := len(api.requests)
	now = now.Add(5 * time.Second)
	if err := connector.PollOnce(context.Background()); err == nil {
		t.Fatal("due repair must report its unresolved result")
	}
	if len(api.requests) != lookups+1 {
		t.Fatalf("repair must make one original-key lookup: %d -> %d", lookups, len(api.requests))
	}
	assertReceiptBytes(t, executor, conflict, original)
	connector = connect()
	if err := connector.PollOnce(context.Background()); err == nil {
		t.Fatal("restart must re-establish ACK custody")
	}
	if len(polls[len(polls)-1].CommandResults) != 1 {
		t.Fatal("restart hid an unacknowledged receipt")
	}
	if err := connector.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(polls[len(polls)-1].EventBatches) != 1 {
		t.Fatal("restart starved activity after fresh ACK")
	}
}

func TestAcknowledgedConflictRetiresOnlyAnExactFailedCreate(t *testing.T) {
	for _, state := range []string{"failed", "running", "uncertain", "succeeded", "wrong-id", "wrong-method", "resource", "unavailable"} {
		t.Run(state, func(t *testing.T) {
			executor, api, conflict, original := conflictingReceipt(t)
			originPath := executor.journal.createOriginPath(conflict.SessionRef)
			origin, err := os.ReadFile(originPath)
			if err != nil {
				t.Fatal(err)
			}
			api.operation.State = state
			switch state {
			case "wrong-id":
				api.operation.ID, api.operation.State = "different", "failed"
			case "wrong-method":
				api.operation.Method, api.operation.State = "SubmitTurn", "failed"
			case "resource":
				api.operation.State, api.operation.ResourceType, api.operation.ResourceID = "failed", "session", "still-owned"
			case "unavailable":
				api.lookup = func(context.Context, Request) (json.RawMessage, error) { return nil, errors.New("offline") }
			}
			connector := acknowledgedConflictConnector(t, executor)
			err = connector.PollOnce(context.Background())
			if state == "failed" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(executor.journal.path(conflict.CommandID)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("proven failed receipt remains: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("unproven retirement succeeded")
				}
				assertReceiptBytes(t, executor, conflict, original)
			}
			after, err := os.ReadFile(originPath)
			if err != nil || !bytes.Equal(origin, after) {
				t.Fatalf("retirement rewrote the origin watermark: %v", err)
			}
			if _, err := os.Stat(executor.journal.eventStreamPath(conflict.SessionRef)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("retirement created a stream")
			}
		})
	}
}

func conflictingReceipt(t *testing.T) (*Executor, *originAPI, workerproto.Command, []byte) {
	t.Helper()
	executor, api, first := newOriginFixture(t)
	if _, err := executor.Execute(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := executor.journal.acknowledgeResults([]string{first.CommandID}); err != nil {
		t.Fatal(err)
	}
	origin, err := executor.journal.readCreateOrigin(executor.journal.createOriginPath(first.SessionRef))
	if err != nil {
		t.Fatal(err)
	}
	api.operation.State = "failed"
	if err := executor.resolveCreateOrigin(context.Background(), origin); err != nil {
		t.Fatal(err)
	}
	conflict := first
	conflict.CommandID, conflict.IdempotencyKey = "create-conflict", "key-conflict"
	api.operation.ID, api.operation.State = "create-conflict", "running"
	originReceipt(t, executor, conflict, `{"operation":{"id":"create-conflict","method":"CreateRemoteSession","state":"running"}}`)
	data, err := os.ReadFile(executor.journal.path(conflict.CommandID))
	if err != nil {
		t.Fatal(err)
	}
	api.requests = nil
	return executor, api, conflict, data
}

func acknowledgedConflictConnector(t *testing.T, executor *Executor) *Connector {
	t.Helper()
	connector, err := NewConnector(ConnectorConfig{Executor: executor, Now: executor.now,
		Hello: func(_ context.Context, clock time.Time) workerproto.WorkerHello { return hello(clock) },
		Transport: pollTransportFunc(func(_ context.Context, poll workerproto.Poll) (workerproto.Response, error) {
			response := workerproto.Response{Version: workerproto.Version, PollRef: poll.PollRef, ServerTime: executor.now()}
			for _, result := range poll.CommandResults {
				response.AcknowledgedResultCommandIDs = append(response.AcknowledgedResultCommandIDs, result.CommandID)
			}
			return response, nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	return connector
}

func assertReceiptBytes(t *testing.T, executor *Executor, command workerproto.Command, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(executor.journal.path(command.CommandID))
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("unresolved receipt custody changed: %v", err)
	}
}

func TestAcknowledgedConflictRejectsOriginChangedDuringLookup(t *testing.T) {
	executor, api, conflict, original := conflictingReceipt(t)
	api.lookup = func(_ context.Context, request Request) (json.RawMessage, error) {
		if !strings.Contains(request.Path, "key=key-conflict") {
			t.Fatalf("wrong create key: %s", request.Path)
		}
		origin, err := executor.journal.readCreateOrigin(executor.journal.createOriginPath(conflict.SessionRef))
		if err != nil {
			t.Fatal(err)
		}
		origin.State, origin.CoopSessionID = "bound", "unexpected-session"
		if err := executor.journal.writeActivityRecord(executor.journal.createOriginPath(conflict.SessionRef), origin); err != nil {
			t.Fatal(err)
		}
		return json.RawMessage(fmt.Sprintf(`{"id":%q,"method":"CreateRemoteSession","state":"failed"}`, api.operation.ID)), nil
	}
	if err := acknowledgedConflictConnector(t, executor).PollOnce(context.Background()); err == nil {
		t.Fatal("stale proof removed receipt")
	}
	assertReceiptBytes(t, executor, conflict, original)
}

func TestAcknowledgedRepairsMakeOneLookupAfterCommandsAndEventACKs(t *testing.T) {
	executor, api, conflict, _ := conflictingReceipt(t)
	for index := range 5 {
		command := conflict
		command.CommandID, command.IdempotencyKey = fmt.Sprintf("extra-%d", index), fmt.Sprintf("extra-key-%d", index)
		originReceipt(t, executor, command, fmt.Sprintf(`{"operation":{"id":"extra-%d","method":"CreateRemoteSession","state":"running"}}`, index))
	}
	healthy := conflict
	healthy.CommandID, healthy.IdempotencyKey, healthy.SessionRef = "healthy-command", "healthy-key", "healthy-session"
	healthy.Payload = apiPayload("GET", "/v1/sessions/coop-session-1", nil)
	stream := eventStream{Version: eventStreamVersion, SessionRef: healthy.SessionRef,
		PlacementGeneration: healthy.PlacementGeneration, CoopSessionID: "coop-session-1", LastPublishedSequence: 1}
	if err := executor.journal.writeEventStream(executor.journal.eventStreamPath(healthy.SessionRef), stream); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	api.lookup = func(ctx context.Context, _ Request) (json.RawMessage, error) {
		lookups++
		entry, err := executor.journal.read(executor.journal.path(healthy.CommandID))
		if err != nil || entry.State != "completed" {
			t.Fatalf("repair ran before healthy command: %v", err)
		}
		current, err := executor.journal.readEventStream(executor.journal.eventStreamPath(healthy.SessionRef))
		if err != nil || current.AcknowledgedSequence != 1 {
			t.Fatalf("repair ran before event ACK: %+v, %v", current, err)
		}
		if _, bounded := ctx.Deadline(); !bounded {
			t.Fatal("repair has no deadline")
		}
		return json.Marshal(api.operation)
	}
	connector := acknowledgedConflictConnector(t, executor)
	original := connector.transport
	connector.transport = pollTransportFunc(func(ctx context.Context, poll workerproto.Poll) (workerproto.Response, error) {
		response, err := original.Poll(ctx, poll)
		response.Commands = []workerproto.Command{healthy}
		response.EventAcknowledgements = []workerproto.EventAcknowledgement{{SessionRef: healthy.SessionRef, PlacementGeneration: healthy.PlacementGeneration, Sequence: 1}}
		return response, err
	})
	if err := connector.PollOnce(context.Background()); err == nil {
		t.Fatal("unresolved repair was hidden")
	}
	if lookups != 1 {
		t.Fatalf("one poll made %d repair lookups", lookups)
	}
	if len(connector.acknowledged) != 6 {
		t.Fatalf("lost acknowledged custody: %d", len(connector.acknowledged))
	}
}

func TestAcknowledgedIdentityChangesRequireFreshACKAndCannotRetireNewBytes(t *testing.T) {
	executor, _, command, _ := conflictingReceipt(t)
	entry, err := executor.journal.read(executor.journal.path(command.CommandID))
	if err != nil {
		t.Fatal(err)
	}
	cache := make(acknowledgedResults)
	cache.deferResult(entry, executor.now())
	changed := entry
	result := *entry.Result
	result.Resource = responsePayload(json.RawMessage(`{"operation":{"id":"different","method":"CreateRemoteSession","state":"running"}}`))
	changed.Result = &result
	if _, err := executor.journal.save(changed); err != nil {
		t.Fatal(err)
	}
	if err := executor.journal.removeAcknowledgedResult(entry); !errors.Is(err, ErrCommandConflict) {
		t.Fatalf("stale ACK retired changed result: %v", err)
	}
	if got := cache.unsent([]journalEntry{changed}); len(got) != 1 || len(cache) != 0 {
		t.Fatal("changed receipt inherited an ACK")
	}
	cache.deferResult(changed, executor.now())
	cache.unsent(nil)
	if len(cache) != 0 {
		t.Fatal("ACK cache retained absent receipts")
	}
}

func TestAcknowledgedRetirementPreservesOriginAndActivityCustody(t *testing.T) {
	for _, state := range []string{"pending", "bound", "failed"} {
		t.Run(state, func(t *testing.T) {
			executor, api, command, receipt := conflictingReceipt(t)
			origin, err := executor.journal.readCreateOrigin(executor.journal.createOriginPath(command.SessionRef))
			if err != nil {
				t.Fatal(err)
			}
			origin.State = state
			if state == "bound" {
				origin.CoopSessionID = "existing-session"
			}
			if err := executor.journal.writeActivityRecord(executor.journal.createOriginPath(command.SessionRef), origin); err != nil {
				t.Fatal(err)
			}
			stream := eventStream{Version: eventStreamVersion, SessionRef: command.SessionRef,
				PlacementGeneration: command.PlacementGeneration, CoopSessionID: "existing-session", LastPublishedSequence: 3, AcknowledgedSequence: 2, TerminalSequence: 3}
			if err := executor.journal.writeEventStream(executor.journal.eventStreamPath(command.SessionRef), stream); err != nil {
				t.Fatal(err)
			}
			if err := executor.journal.advanceEventScan(command.SessionRef); err != nil {
				t.Fatal(err)
			}
			paths := []string{executor.journal.createOriginPath(command.SessionRef), executor.journal.eventStreamPath(command.SessionRef), executor.journal.eventScanPath()}
			before := make(map[string][]byte)
			for _, path := range paths {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[path] = data
			}
			api.operation.State = "failed"
			err = acknowledgedConflictConnector(t, executor).PollOnce(context.Background())
			if state == "failed" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					t.Fatal("unproven old origin permitted retirement")
				}
				assertReceiptBytes(t, executor, command, receipt)
				if len(api.requests) != 0 {
					t.Fatal("unproven origin attempted operation repair")
				}
			}
			for path, expected := range before {
				actual, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(actual, expected) {
					t.Fatalf("activity custody changed: %s: %v", path, err)
				}
			}
		})
	}
}

func TestAcknowledgedConflictRetiresAfterRealHigherGenerationWithoutLookup(t *testing.T) {
	executor, api, command, _ := conflictingReceipt(t)
	connector := acknowledgedConflictConnector(t, executor)
	if err := connector.PollOnce(context.Background()); err == nil {
		t.Fatal("expected initial conflict")
	}
	next := command
	next.CommandID, next.IdempotencyKey, next.PlacementGeneration = "next-command", "next-key", command.PlacementGeneration+1
	originReceipt(t, executor, next, `{"operation":{"id":"next-operation","method":"CreateRemoteSession","state":"running"}}`)
	if err := executor.journal.acknowledgeResults([]string{next.CommandID}); err != nil {
		t.Fatal(err)
	}
	origin, err := executor.journal.readCreateOrigin(executor.journal.createOriginPath(next.SessionRef))
	if err != nil {
		t.Fatal(err)
	}
	api.operation = createOperation{ID: "next-operation", Method: "CreateRemoteSession", State: "failed"}
	if err := executor.resolveCreateOrigin(context.Background(), origin); err != nil {
		t.Fatal(err)
	}
	api.requests = nil
	connector.now = func() time.Time { return executor.now().Add(6 * time.Second) }
	if err := connector.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.requests) != 0 {
		t.Fatal("higher generation needed an unnecessary lookup")
	}
	if _, err := os.Stat(executor.journal.path(command.CommandID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old receipt remained: %v", err)
	}
}

func TestMismatchedPollCannotRememberReceiptAcknowledgement(t *testing.T) {
	executor, _, command, original := conflictingReceipt(t)
	connector := acknowledgedConflictConnector(t, executor)
	authoritative := connector.transport
	connector.transport = pollTransportFunc(func(ctx context.Context, poll workerproto.Poll) (workerproto.Response, error) {
		response, err := authoritative.Poll(ctx, poll)
		response.PollRef = "another-poll"
		return response, err
	})
	if err := connector.PollOnce(context.Background()); err == nil {
		t.Fatal("crossed poll was accepted")
	}
	if len(connector.acknowledged) != 0 {
		t.Fatal("crossed poll suppressed a receipt")
	}
	assertReceiptBytes(t, executor, command, original)
	connector.transport = pollTransportFunc(func(ctx context.Context, poll workerproto.Poll) (workerproto.Response, error) {
		if len(poll.CommandResults) != 1 {
			t.Fatal("crossed ACK suppressed the next result")
		}
		return authoritative.Poll(ctx, poll)
	})
	if err := connector.PollOnce(context.Background()); err == nil {
		t.Fatal("retained conflict not reported")
	}
}

func TestAcknowledgedRepairTimesOutWithoutBlockingTheNextActivityPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor, api, command, original := conflictingReceipt(t)
		healthy := command
		healthy.SessionRef = "healthy-session"
		if err := executor.journal.bindEventStream(healthy, "coop-session-1"); err != nil {
			t.Fatal(err)
		}
		api.lookup = func(ctx context.Context, _ Request) (json.RawMessage, error) { <-ctx.Done(); return nil, ctx.Err() }
		connector := acknowledgedConflictConnector(t, executor)
		connector.now = time.Now
		started := time.Now()
		if err := connector.PollOnce(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unbounded repair: %v", err)
		}
		if elapsed := time.Since(started); elapsed != 2*time.Second {
			t.Fatalf("repair held the poll for %s", elapsed)
		}
		assertReceiptBytes(t, executor, command, original)
		transport := connector.transport
		connector.transport = pollTransportFunc(func(ctx context.Context, poll workerproto.Poll) (workerproto.Response, error) {
			if len(poll.EventBatches) != 1 || len(poll.CommandResults) != 0 {
				t.Fatal("timed-out repair starved the next activity poll")
			}
			return transport.Poll(ctx, poll)
		})
		if err := connector.PollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAcknowledgedRepairKeepsOlderDueReceiptsAheadOfNewConflicts(t *testing.T) {
	executor, _, command, _ := conflictingReceipt(t)
	entry, err := executor.journal.read(executor.journal.path(command.CommandID))
	if err != nil {
		t.Fatal(err)
	}
	cache := make(acknowledgedResults)
	cache.deferResult(entry, executor.now())
	newer := entry
	newCommand, newResult := *entry.Command, *entry.Result
	newer.CommandID, newCommand.CommandID, newResult.CommandID = "new-conflict", "new-conflict", "new-conflict"
	newer.Command, newer.Result = &newCommand, &newResult
	newer.CommandDigest, err = commandDigest(newCommand)
	if err != nil {
		t.Fatal(err)
	}
	identity, valid := acknowledgedIdentity(newer)
	if !valid {
		t.Fatal("invalid test receipt")
	}
	now := executor.now().Add(10 * time.Second)
	identity.nextAttempt = now
	cache[newer.CommandID] = identity
	if next := cache.nextDue([]journalEntry{newer, entry}, now); next == nil || next.CommandID != entry.CommandID {
		t.Fatal("new conflict overtook older due repair")
	}
}
