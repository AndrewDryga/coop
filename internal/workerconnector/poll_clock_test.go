package workerconnector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

type advancingEventAPI struct {
	fakeAPI
	advance func()
	err     error
}

func (a *advancingEventAPI) ListEvents(context.Context, string, int64, int) ([]workerproto.SessionEvent, error) {
	a.advance()
	return nil, a.err
}

// A stale clock caused the controller to refuse results and new commands after
// slow local activity reads, even while the worker was otherwise idle.
func TestPollClockIsFreshAfterSlowHelloOrActivityWithoutResamplingCapacity(t *testing.T) {
	for _, fixture := range []struct {
		name        string
		slowHello   bool
		activityErr error
	}{
		{name: "activity"},
		{name: "failed activity", activityErr: context.DeadlineExceeded},
		{name: "capacity snapshot", slowHello: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			base := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
			now := base
			advance := func() { now = now.Add(31*time.Second + 123456789*time.Nanosecond) }
			api := &advancingEventAPI{advance: advance, err: fixture.activityErr}
			executor, err := NewExecutor(ExecutorConfig{API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a"})
			if err != nil {
				t.Fatal(err)
			}
			if !fixture.slowHello {
				if err := executor.journal.bindEventStream(createCommand(now.Add(time.Hour)), "coop-session-1"); err != nil {
					t.Fatal(err)
				}
			}
			measuring := false
			helloCalls, polls := 0, 0
			connector, err := NewConnector(ConnectorConfig{
				Executor: executor, Now: func() time.Time { return now },
				Hello: func(_ context.Context, sampled time.Time) workerproto.WorkerHello {
					worker := hello(sampled)
					if measuring {
						helloCalls++
						if fixture.slowHello {
							advance()
						}
						if sampled.After(base) {
							worker.Capacity.SessionSlotsFree = 0
						}
					}
					return worker
				},
				Transport: pollTransportFunc(func(_ context.Context, poll workerproto.Poll) (workerproto.Response, error) {
					polls++
					if now.Sub(poll.Worker.ClockAt) < 0 || now.Sub(poll.Worker.ClockAt) >= time.Second {
						t.Errorf("sent clock=%s, send time=%s", poll.Worker.ClockAt, now)
					}
					if poll.Worker.Capacity != hello(base).Capacity {
						t.Error("clock refresh resampled capacity")
					}
					return workerproto.Response{Version: workerproto.Version, PollRef: poll.PollRef, ServerTime: now}, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			measuring = true
			if err := connector.PollOnce(context.Background()); !errors.Is(err, fixture.activityErr) {
				t.Fatalf("poll: %v, want %v", err, fixture.activityErr)
			}
			if helloCalls != 1 || polls != 1 || now.Sub(base) <= 30*time.Second {
				t.Fatalf("hello=%d polls=%d elapsed=%v", helloCalls, polls, now.Sub(base))
			}
		})
	}
}

func TestFreshPollClockCannotExpandAMaximumReceiptPage(t *testing.T) {
	base := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	sendAt := base.Add(31*time.Second + 123456789*time.Nanosecond)
	executor, err := NewExecutor(ExecutorConfig{API: &fakeAPI{}, JournalDir: t.TempDir(), Now: func() time.Time { return base }, WorkerID: "worker-a"})
	if err != nil {
		t.Fatal(err)
	}
	poll := workerproto.Poll{Version: workerproto.Version, PollRef: "poll:worker-a:1", Worker: hello(base), AcknowledgedCommandIDs: []string{}, CommandResults: []workerproto.CommandResult{}, EventBatches: []workerproto.EventBatch{}}
	entries := []journalEntry{}
	for index := range 2 {
		command := createCommand(base.Add(time.Hour))
		command.CommandID, command.IdempotencyKey = fmt.Sprintf("clock:%d", index), fmt.Sprintf("clock-operation:%d", index)
		entry, err := executor.journal.begin(command)
		if err != nil {
			t.Fatal(err)
		}
		entry.Result = &workerproto.CommandResult{CommandID: command.CommandID, OperationKey: command.IdempotencyKey, State: "succeeded", Resource: json.RawMessage(`{"padding":""}`), Error: json.RawMessage("null")}
		entries = append(entries, entry)
		poll = appendReceipt(poll, entry)
	}
	encoded, err := encodeWireJSON(poll)
	if err != nil {
		t.Fatal(err)
	}
	remaining := workerproto.MaxDocumentBytes - len(encoded)
	for index, entry := range entries {
		padding := remaining / 2
		if index == 1 {
			padding = remaining - padding
		}
		entry.Result.Resource = json.RawMessage(`{"padding":"` + strings.Repeat("x", padding) + `"}`)
		if _, err := executor.complete(entry, *entry.Result); err != nil {
			t.Fatal(err)
		}
	}
	clockReads := 0
	connector, err := NewConnector(ConnectorConfig{Executor: executor,
		Now: func() time.Time {
			clockReads++
			if clockReads > 1 {
				return sendAt
			}
			return base
		},
		Hello: func(_ context.Context, now time.Time) workerproto.WorkerHello { return hello(now) },
		Transport: pollTransportFunc(func(_ context.Context, poll workerproto.Poll) (workerproto.Response, error) {
			encoded, err := encodeWireJSON(poll)
			if err != nil || len(encoded) != workerproto.MaxDocumentBytes {
				t.Fatalf("maximum page=%d bytes, err=%v", len(encoded), err)
			}
			if _, err := workerproto.DecodePoll(encoded); err != nil {
				t.Fatal(err)
			}
			if len(poll.CommandResults) != 2 || sendAt.Sub(poll.Worker.ClockAt) < 0 || sendAt.Sub(poll.Worker.ClockAt) >= time.Second {
				t.Fatalf("results=%d clock=%s send=%s", len(poll.CommandResults), poll.Worker.ClockAt, sendAt)
			}
			return workerproto.Response{Version: workerproto.Version, PollRef: poll.PollRef, ServerTime: sendAt}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	clockReads = 0
	if err := connector.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}
