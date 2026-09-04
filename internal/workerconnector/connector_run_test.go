package workerconnector

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

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

func (f *failingThenHealthyTransport) Poll(_ context.Context, poll workerproto.Poll) (workerproto.Response, error) {
	if f.calls.Add(1) == 1 {
		return workerproto.Response{}, errors.New("offline")
	}
	return workerproto.Response{
		Version: workerproto.Version, PollRef: poll.PollRef, ServerTime: f.now,
		AcknowledgedResultCommandIDs: []string{}, Commands: []workerproto.Command{}, EventAcknowledgements: []workerproto.EventAcknowledgement{},
	}, nil
}
