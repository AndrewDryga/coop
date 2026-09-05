package workerconnector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// Existing event projection tests inspect best-effort batches; origin/error recovery tests
// exercise collectActivity and PollOnce directly, including their reported failures.
func (e *Executor) pendingEventBatches(ctx context.Context, maximumBytes int) []workerproto.EventBatch {
	batches, _ := e.collectActivity(ctx, maximumBytes)
	return batches
}

func TestConnectorResumesSessionEventsFromTheLastResponderAcknowledgement(t *testing.T) {
	now := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	api := &eventAPI{
		fakeAPI: fakeAPI{response: json.RawMessage(`{"operation":{"id":"create-1","state":"succeeded"},"session":{"id":"coop-session-1","revision":2}}`)},
		events: []workerproto.SessionEvent{
			{
				ID: "evt-1", SessionID: "coop-session-1", Sequence: 1, TurnID: "turn-1",
				Type: "session.created", Version: 1, OccurredAt: now,
				Payload: json.RawMessage(`{"message":"raw lifecycle detail must not cross"}`),
			},
			{
				ID: "evt-2", SessionID: "coop-session-1", Sequence: 2, TurnID: "turn-1",
				Type: "tool.started", Version: 1, OccurredAt: now.Add(time.Second),
				Payload: json.RawMessage(`{"tool_call_id":"tool-1","title":"secret title","input":{"server":"emisar","tool":"get_action","arguments":{"action_id":"nomad.job_status_one","reason":"token=secret","args":{"api_key":"secret"}}}}`),
			},
		},
	}
	command := createCommand(now.Add(time.Minute))

	firstExecutor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: dir, Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	firstTransport := &scriptedTransport{responses: []workerproto.Response{{
		Version: workerproto.Version, PollRef: "poll:worker-a:1", ServerTime: now,
		Commands: []workerproto.Command{command},
	}}}
	firstConnector, err := NewConnector(ConnectorConfig{
		Executor: firstExecutor, Hello: func(_ context.Context, clock time.Time) workerproto.WorkerHello { return hello(clock) },
		Now: func() time.Time { return now }, Transport: firstTransport,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := firstConnector.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A process restart must recover both the session binding and its unsent cursor.
	secondExecutor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: dir, Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	secondTransport := &scriptedTransport{responses: []workerproto.Response{
		{
			Version: workerproto.Version, PollRef: "poll:worker-a:1", ServerTime: now,
			AcknowledgedResultCommandIDs: []string{command.CommandID},
		},
		{
			Version: workerproto.Version, PollRef: "poll:worker-a:2", ServerTime: now,
			EventAcknowledgements: []workerproto.EventAcknowledgement{{
				SessionRef: command.SessionRef, PlacementGeneration: command.PlacementGeneration, Sequence: 2,
			}},
		},
	}}
	secondConnector, err := NewConnector(ConnectorConfig{
		Executor: secondExecutor, Hello: func(_ context.Context, clock time.Time) workerproto.WorkerHello { return hello(clock) },
		Now: func() time.Time { return now }, Transport: secondTransport,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := secondConnector.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(secondTransport.polls) != 1 || len(secondTransport.polls[0].EventBatches) != 0 {
		t.Fatalf("activity delayed command settlement: %+v", secondTransport.polls)
	}
	if err := secondConnector.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(secondTransport.polls) != 2 || len(secondTransport.polls[1].EventBatches) != 1 {
		t.Fatalf("event batches = %+v", secondTransport.polls)
	}
	batch := secondTransport.polls[1].EventBatches[0]
	if batch.SessionRef != command.SessionRef || batch.PlacementGeneration != 1 ||
		batch.AfterSequence != 0 || len(batch.Events) != 2 || batch.Events[1].Sequence != 2 {
		t.Fatalf("event batch = %+v", batch)
	}
	var carried workerproto.SessionEvent
	if err := json.Unmarshal(batch.Events[0].Payload, &carried); err != nil ||
		carried.ID != "evt-1" || carried.Type != "session.created" || carried.Sequence != 1 || len(carried.Payload) != 0 {
		t.Fatalf("carried event = %+v, err=%v", carried, err)
	}
	if err := json.Unmarshal(batch.Events[1].Payload, &carried); err != nil ||
		carried.ID != "evt-2" || carried.Type != "tool.started" ||
		string(carried.Payload) != `{"input":{"operation":"nomad.job_status_one","server":"emisar","tool":"get_action"},"tool_call_id":"tool-1"}` {
		t.Fatalf("carried activity = %+v, err=%v", carried, err)
	}

	// The acknowledgement is durable too: another restart resumes strictly after 2.
	thirdExecutor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: dir, Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	thirdTransport := &scriptedTransport{responses: []workerproto.Response{{
		Version: workerproto.Version, PollRef: "poll:worker-a:1", ServerTime: now,
	}}}
	thirdConnector, err := NewConnector(ConnectorConfig{
		Executor: thirdExecutor, Hello: func(_ context.Context, clock time.Time) workerproto.WorkerHello { return hello(clock) },
		Now: func() time.Time { return now }, Transport: thirdTransport,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := thirdConnector.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(thirdTransport.polls[0].EventBatches) != 0 {
		t.Fatalf("acknowledged events replayed: %+v", thirdTransport.polls[0].EventBatches)
	}
	if got := api.afters; len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Fatalf("event cursors = %v", got)
	}
}

func TestOnlyAcknowledgedWorkspaceDiscardReleasesTheDurableStream(t *testing.T) {
	now := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)
	api := &eventAPI{
		fakeAPI: fakeAPI{response: json.RawMessage(`{"operation":{"id":"create-1","state":"succeeded"},"session":{"id":"coop-session-1","revision":2}}`)},
		events: []workerproto.SessionEvent{{
			ID: "evt-closed", SessionID: "coop-session-1", Sequence: 1,
			Type: "session.closed", Version: 1, OccurredAt: now,
		}},
	}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))
	if _, err := executor.Execute(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	batches := executor.pendingEventBatches(context.Background(), maximumPollBytes)
	if len(batches) != 1 || len(batches[0].Events) != 1 {
		t.Fatalf("terminal batch = %+v", batches)
	}
	if err := executor.journal.acknowledgeEvents([]workerproto.EventAcknowledgement{{
		SessionRef: command.SessionRef, PlacementGeneration: 1, Sequence: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	streams, err := executor.journal.eventStreams()
	if err != nil || len(streams) != 1 || streams[0].AcknowledgedSequence != 1 {
		t.Fatalf("closed session stream = %+v, %v", streams, err)
	}

	api.events = append(api.events, workerproto.SessionEvent{
		ID: "evt-discarded", SessionID: "coop-session-1", Sequence: 2,
		Type: "workspace.discarded", Version: 1, OccurredAt: now.Add(time.Second),
	})
	batches = executor.pendingEventBatches(context.Background(), maximumPollBytes)
	if len(batches) != 1 || batches[0].AfterSequence != 1 || len(batches[0].Events) != 1 {
		t.Fatalf("discard batch = %+v", batches)
	}
	if err := executor.journal.acknowledgeEvents([]workerproto.EventAcknowledgement{{
		SessionRef: command.SessionRef, PlacementGeneration: 1, Sequence: 2,
	}}); err != nil {
		t.Fatal(err)
	}
	streams, err = executor.journal.eventStreams()
	if err != nil || len(streams) != 0 {
		t.Fatalf("discarded streams = %+v, %v", streams, err)
	}
}

func TestCommandSettlementKeepsItsWireBudgetAheadOfActivity(t *testing.T) {
	now := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)
	padding := strings.Repeat("x", workerproto.MaxDocumentBytes/2-10_000)
	api := &eventAPI{
		fakeAPI: fakeAPI{response: json.RawMessage(`{"operation":{"id":"create-1","state":"succeeded"},"session":{"id":"coop-session-1","revision":2},"padding":"` + padding + `"}`)},
		events: []workerproto.SessionEvent{{
			ID: "evt-1", SessionID: "coop-session-1", Sequence: 1,
			Type: "model.thought", Version: 1, OccurredAt: now,
			Payload: json.RawMessage(`{"text":"private reasoning"}`),
		}},
	}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))
	if _, err := executor.Execute(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	secondCommand := command
	secondCommand.CommandID = "018f04f4-1111-7000-8000-000000000002"
	secondCommand.SessionRef = "018f04f4-2222-7000-8000-000000000002"
	secondCommand.IdempotencyKey = "responder:work:create:session-2:g1"
	if _, err := executor.Execute(context.Background(), secondCommand); err != nil {
		t.Fatal(err)
	}
	transport := &scriptedTransport{responses: []workerproto.Response{{
		Version: workerproto.Version, PollRef: "poll:worker-a:1", ServerTime: now,
	}}}
	connector, err := NewConnector(ConnectorConfig{
		Executor: executor, Hello: func(_ context.Context, clock time.Time) workerproto.WorkerHello { return hello(clock) },
		Now: func() time.Time { return now }, Transport: transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := connector.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(transport.polls) != 1 || len(transport.polls[0].CommandResults) != 2 {
		t.Fatalf("command settlement missing from poll: %+v", transport.polls)
	}
	if len(transport.polls[0].EventBatches) != 0 {
		t.Fatalf("activity consumed reserved settlement bytes: %+v", transport.polls[0].EventBatches)
	}
	if len(api.afters) != 0 {
		t.Fatalf("activity read delayed command settlement: cursors=%v", api.afters)
	}
	encoded, err := json.Marshal(transport.polls[0])
	if err != nil || len(encoded) > workerproto.MaxDocumentBytes {
		t.Fatalf("poll bytes = %d, err=%v", len(encoded), err)
	}
	streams, err := executor.journal.eventStreams()
	if err != nil || len(streams) != 2 || streams[0].AcknowledgedSequence != 0 {
		t.Fatalf("deferred event stream = %+v, %v", streams, err)
	}
}

func TestEventStreamScanGivesEveryBoundSessionAChanceToPublish(t *testing.T) {
	// Exercise the event cap independently of filesystem throughput. The scan's
	// deadline is tested separately with an API that waits for cancellation.
	synctest.Test(t, func(t *testing.T) {
		now := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)
		api := &multiEventAPI{events: map[string][]workerproto.SessionEvent{}}
		executor, err := NewExecutor(ExecutorConfig{
			API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
		})
		if err != nil {
			t.Fatal(err)
		}
		for index := 0; index < 101; index++ {
			sessionRef := fmt.Sprintf("fair-session-%03d", index)
			coopSessionID := fmt.Sprintf("coop-session-%03d", index)
			if err := executor.journal.writeEventStream(executor.journal.eventStreamPath(sessionRef), eventStream{
				Version: eventStreamVersion, SessionRef: sessionRef, PlacementGeneration: 1, CoopSessionID: coopSessionID,
			}); err != nil {
				t.Fatal(err)
			}
			api.events[coopSessionID] = []workerproto.SessionEvent{{
				ID: "evt-1", SessionID: coopSessionID, Sequence: 1,
				Type: "model.thought", Version: 1, OccurredAt: now,
			}}
		}
		first := executor.pendingEventBatches(context.Background(), maximumPollBytes)
		if len(first) != 100 {
			t.Fatalf("first scan batches = %d", len(first))
		}
		if first[0].SessionRef != "fair-session-000" || first[99].SessionRef != "fair-session-099" {
			t.Fatalf("first scan bounds = %q..%q", first[0].SessionRef, first[99].SessionRef)
		}
		second := executor.pendingEventBatches(context.Background(), maximumPollBytes)
		if len(second) != 100 {
			t.Fatalf("second scan batches = %d", len(second))
		}
		if second[0].SessionRef != "fair-session-100" {
			t.Fatalf("rotated scan did not reach the late session: %+v", second)
		}
	})
}

func TestEventStreamScanDeadlinePreservesRotation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &blockedEventAPI{}
		executor, err := NewExecutor(ExecutorConfig{
			API: api, JournalDir: t.TempDir(), Now: time.Now, WorkerID: "worker-a",
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"session-a", "session-b"} {
			if err := executor.journal.writeEventStream(executor.journal.eventStreamPath(id), eventStream{
				Version: eventStreamVersion, SessionRef: id, PlacementGeneration: 1, CoopSessionID: id,
			}); err != nil {
				t.Fatal(err)
			}
		}
		for index, id := range []string{"session-a", "session-b"} {
			start := time.Now()
			if batches := executor.pendingEventBatches(context.Background(), maximumPollBytes); len(batches) != 0 {
				t.Fatalf("blocked scan returned batches: %+v", batches)
			}
			if elapsed := time.Since(start); elapsed != maximumEventDelay {
				t.Fatalf("scan deadline = %s, want %s", elapsed, maximumEventDelay)
			}
			if len(api.sessions) != index+1 || api.sessions[index] != id {
				t.Fatalf("scan did not resume after timed-out session: %v", api.sessions)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if batches := executor.pendingEventBatches(ctx, maximumPollBytes); len(batches) != 0 || len(api.sessions) != 2 {
			t.Fatalf("cancelled scan read activity: batches=%v calls=%v", batches, api.sessions)
		}
	})
}

func TestConnectorRunRetriesTransportFailureWithoutDroppingCustody(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{}
	executor, err := NewExecutor(ExecutorConfig{API: api, JournalDir: t.TempDir(), Now: time.Now, WorkerID: "worker-a"})
	if err != nil {
		t.Fatal(err)
	}
	transport := &failingThenHealthyTransport{now: now}
	connector, err := NewConnector(ConnectorConfig{
		Executor: executor, Hello: func(_ context.Context, clock time.Time) workerproto.WorkerHello { return hello(clock) },
		Now: time.Now, Transport: transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errorsSeen := make(chan error, 1)
	done := make(chan error, 1)
	go func() { done <- connector.Run(ctx, time.Millisecond, func(err error) { errorsSeen <- err }) }()
	select {
	case err := <-errorsSeen:
		if err == nil {
			t.Fatal("nil transport error")
		}
	case <-time.After(time.Second):
		t.Fatal("transport error was not reported")
	}
	deadline := time.Now().Add(time.Second)
	for transport.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run = %v", err)
	}
	if transport.calls.Load() < 2 {
		t.Fatalf("poll calls = %d", transport.calls.Load())
	}
}

type failingThenHealthyTransport struct {
	calls atomic.Int64
	now   time.Time
}

type eventAPI struct {
	fakeAPI
	events []workerproto.SessionEvent
	afters []int64
}

type multiEventAPI struct {
	fakeAPI
	events map[string][]workerproto.SessionEvent
}

type blockedEventAPI struct {
	fakeAPI
	sessions []string
}

func (f *blockedEventAPI) ListEvents(ctx context.Context, sessionID string, _ int64, _ int) ([]workerproto.SessionEvent, error) {
	f.sessions = append(f.sessions, sessionID)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *multiEventAPI) ListEvents(_ context.Context, sessionID string, after int64, limit int) ([]workerproto.SessionEvent, error) {
	result := make([]workerproto.SessionEvent, 0, limit)
	for _, event := range f.events[sessionID] {
		if event.Sequence > after && len(result) < limit {
			result = append(result, event)
		}
	}
	return result, nil
}

func (f *eventAPI) ListEvents(_ context.Context, sessionID string, after int64, _ int) ([]workerproto.SessionEvent, error) {
	f.afters = append(f.afters, after)
	if sessionID != "coop-session-1" {
		return nil, errors.New("unknown session")
	}
	result := make([]workerproto.SessionEvent, 0, len(f.events))
	for _, event := range f.events {
		if event.Sequence > after {
			result = append(result, event)
		}
	}
	return result, nil
}

func (f *failingThenHealthyTransport) Poll(_ context.Context, poll workerproto.Poll) (workerproto.Response, error) {
	if f.calls.Add(1) == 1 {
		return workerproto.Response{}, errors.New("offline")
	}
	return workerproto.Response{
		Version: workerproto.Version, PollRef: poll.PollRef, ServerTime: f.now,
		AcknowledgedResultCommandIDs: []string{}, Commands: []workerproto.Command{}, EventAcknowledgements: []workerproto.EventAcknowledgement{},
	}, nil
}
