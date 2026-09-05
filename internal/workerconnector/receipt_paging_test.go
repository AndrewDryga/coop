package workerconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestReceiptPagesRespectCountAndWireBytesAcrossRestart(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		count    int
		padding  string
		received bool
	}{
		{"mixed count", 150, "small", true},
		{"large results", 5, strings.Repeat("x", 250<<10), false},
		{"HTML result", 1, strings.Repeat("<>&", 200<<10), false},
		{"Unicode separators", 1, strings.Repeat("\u2028\u2029", 100<<10), false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 9, 5, 18, 0, 0, 0, time.UTC)
			dir := t.TempDir()
			api := &eventAPI{fakeAPI: fakeAPI{response: json.RawMessage(`{"value":"` + fixture.padding + `"}`)}}
			open := func() *Executor {
				t.Helper()
				executor, err := NewExecutor(ExecutorConfig{
					API: api, JournalDir: dir, Now: func() time.Time { return now }, WorkerID: "worker-a",
				})
				if err != nil {
					t.Fatal(err)
				}
				return executor
			}
			executor := open()
			receivedBytes := make(map[string][]byte)
			completed := make(map[string]workerproto.CommandResult)
			for index := range fixture.count {
				command := createCommand(now.Add(time.Minute))
				command.CommandID = fmt.Sprintf("command:%03d", index)
				command.IdempotencyKey = fmt.Sprintf("operation:%03d", index)
				command.Kind, command.Payload = "get_session", json.RawMessage(`{"coop_session_id":"session-1"}`)
				if fixture.received && index < fixture.count-25 {
					if _, err := executor.journal.begin(command); err != nil {
						t.Fatal(err)
					}
					body, err := os.ReadFile(executor.journal.path(command.CommandID))
					if err != nil {
						t.Fatal(err)
					}
					receivedBytes[command.CommandID] = body
					continue
				}
				result, err := executor.Execute(ctx, command)
				if err != nil || result.State != "succeeded" {
					t.Fatalf("execute %s: state=%s, err=%v", command.CommandID, result.State, err)
				}
				completed[command.CommandID] = result
				if replay, err := open().Execute(ctx, command); err != nil || !sameResult(result, replay) {
					t.Fatalf("reopened receipt differs: %s, %v", command.CommandID, err)
				}
			}
			if fixture.received {
				command := createCommand(now.Add(time.Minute))
				if err := executor.journal.bindEventStreamResult(command, workerproto.CommandResult{
					State: "succeeded", Resource: json.RawMessage(`{"session":{"id":"session-1"}}`),
				}); err != nil {
					t.Fatal(err)
				}
			}
			requests := make(chan workerproto.Poll, 32)
			var acknowledge atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(io.LimitReader(r.Body, workerproto.MaxDocumentBytes+1))
				if err != nil {
					t.Error(err)
					http.Error(w, "read", http.StatusBadRequest)
					return
				}
				poll, err := workerproto.DecodePoll(body)
				if err != nil {
					t.Errorf("invalid wire poll (%d bytes): %v", len(body), err)
					http.Error(w, "invalid", http.StatusBadRequest)
					return
				}
				requests <- poll
				response := workerproto.Response{Version: workerproto.Version, PollRef: poll.PollRef, ServerTime: now}
				if acknowledge.Load() && len(poll.CommandResults) > 0 {
					// Deliberately acknowledge only one result from a page.
					response.AcknowledgedResultCommandIDs = []string{poll.CommandResults[0].CommandID}
					if fixture.received {
						for _, result := range poll.CommandResults[1:] {
							response.AcknowledgedResultCommandIDs = append(response.AcknowledgedResultCommandIDs, result.CommandID)
						}
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			transport, err := newHTTPTransport(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			seen := make(map[string]bool)
			poll := func() {
				t.Helper()
				connector, err := NewConnector(ConnectorConfig{
					Executor: open(), Hello: func(_ context.Context, clock time.Time) workerproto.WorkerHello { return hello(clock) },
					Now: func() time.Time { return now }, Transport: transport,
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := connector.PollOnce(ctx); err != nil {
					t.Fatal(err)
				}
				request := <-requests
				for _, id := range request.AcknowledgedCommandIDs {
					seen[id] = true
				}
				for _, result := range request.CommandResults {
					if want, ok := completed[result.CommandID]; !ok || !sameResult(want, result) {
						t.Fatalf("wire result differs for %s", result.CommandID)
					}
				}
			}
			// Even expired received receipts retain custody. Reopening every poll must not
			// reset fairness to an unacknowledged prefix.
			now = now.Add(time.Hour)
			for range 4 {
				poll()
			}
			if len(seen) != fixture.count {
				t.Fatalf("only %d of %d receipts published without ACKs", len(seen), fixture.count)
			}
			if len(api.afters) != 0 {
				t.Fatal("activity delayed a completed result on a deferred page")
			}
			acknowledge.Store(true)
			for range 8 {
				poll()
			}
			for id := range completed {
				if _, err := os.Stat(executor.journal.path(id)); !os.IsNotExist(err) {
					t.Fatalf("acknowledged result %s retained: %v", id, err)
				}
			}
			for id, want := range receivedBytes {
				got, err := os.ReadFile(executor.journal.path(id))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("received custody changed for %s: %v", id, err)
				}
			}
			if len(api.requests) != len(completed) {
				t.Fatalf("API executed %d times for %d completed receipts", len(api.requests), len(completed))
			}
		})
	}
}

func TestLegacyUnsendableReceiptDoesNotBlockSettlementOrCommands(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 18, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	api := &eventAPI{fakeAPI: fakeAPI{response: json.RawMessage(`{"answer":"ok"}`)}}
	open := func() *Executor {
		t.Helper()
		executor, err := NewExecutor(ExecutorConfig{API: api, JournalDir: dir, Now: func() time.Time { return now }, WorkerID: "worker-a"})
		if err != nil {
			t.Fatal(err)
		}
		return executor
	}
	executor := open()
	if err := executor.journal.bindEventStreamResult(createCommand(now.Add(time.Minute)), workerproto.CommandResult{
		State: "succeeded", Resource: json.RawMessage(`{"session":{"id":"coop-session-1"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	legacy := createCommand(now.Add(time.Minute))
	legacy.CommandID, legacy.IdempotencyKey = "command:legacy", "operation:legacy"
	legacy.Kind, legacy.Payload = "get_session", json.RawMessage(`{"coop_session_id":"session-1"}`)
	entry, err := executor.journal.begin(legacy)
	if err != nil {
		t.Fatal(err)
	}
	entry.State = "completed"
	entry.Result = &workerproto.CommandResult{
		CommandID: legacy.CommandID, OperationKey: legacy.IdempotencyKey, State: "succeeded",
		Resource: json.RawMessage(`{"value":"` + strings.Repeat("<", 300<<10) + `"}`), Error: json.RawMessage("null"),
	}
	// Reproduce the old writer expanding a legal result beyond the protocol bound.
	legacyBytes, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := executor.journal.path(legacy.CommandID)
	if err := os.WriteFile(legacyPath, legacyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	legacyInfo, err := os.Stat(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	healthy := legacy
	healthy.CommandID, healthy.IdempotencyKey = "command:healthy", "operation:healthy"
	if _, err := executor.Execute(ctx, healthy); err != nil {
		t.Fatal(err)
	}
	delivered := healthy
	delivered.CommandID, delivered.IdempotencyKey = "command:delivered", "operation:delivered"
	var mode atomic.Int32
	var sentCommand atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, workerproto.MaxDocumentBytes+1))
		if err != nil {
			t.Error(err)
			return
		}
		poll, err := workerproto.DecodePoll(body)
		if err != nil {
			t.Errorf("invalid legacy-isolation poll: %v", err)
			http.Error(w, "invalid", http.StatusBadRequest)
			return
		}
		response := workerproto.Response{Version: workerproto.Version, PollRef: poll.PollRef, ServerTime: now}
		for _, result := range poll.CommandResults {
			if result.CommandID == legacy.CommandID {
				t.Error("legacy unsendable result reached the controller")
			}
			response.AcknowledgedResultCommandIDs = append(response.AcknowledgedResultCommandIDs, result.CommandID)
		}
		w.Header().Set("Content-Type", "application/json")
		switch mode.Load() {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			response.PollRef = "poll:wrong"
			response.Commands = []workerproto.Command{delivered}
		case 3:
			response.Version++
			response.Commands = []workerproto.Command{delivered}
		case 4:
			// Fail only cursor publication, after the poll's cursor read succeeded.
			if err := os.Mkdir(executor.journal.receiptScanPath(), 0o700); err != nil {
				t.Error(err)
			}
		}
		if (mode.Load() == 0 || mode.Load() == 4) && sentCommand.CompareAndSwap(false, true) {
			response.Commands = []workerproto.Command{delivered}
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	transport, err := newHTTPTransport(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	poll := func() error {
		connector, err := NewConnector(ConnectorConfig{
			Executor: open(), Hello: func(_ context.Context, clock time.Time) workerproto.WorkerHello { return hello(clock) },
			Now: func() time.Time { return now }, Transport: transport,
		})
		if err != nil {
			return err
		}
		return connector.PollOnce(ctx)
	}
	for _, failure := range []int32{1, 2, 3} {
		mode.Store(failure)
		if err := poll(); err == nil {
			t.Fatalf("invalid response %d succeeded", failure)
		}
		if _, err := os.Stat(executor.journal.path(healthy.CommandID)); err != nil {
			t.Fatalf("invalid response deleted healthy custody: %v", err)
		}
		if _, err := os.Stat(executor.journal.receiptScanPath()); !os.IsNotExist(err) {
			t.Fatalf("invalid response advanced fairness cursor: %v", err)
		}
		if len(api.requests) != 1 {
			t.Fatal("invalid response executed a command")
		}
	}
	mode.Store(4)
	if err := poll(); err == nil || !strings.Contains(err.Error(), legacy.CommandID) {
		t.Fatalf("legacy/cursor refusal = %v", err)
	}
	if _, err := os.Stat(executor.journal.path(healthy.CommandID)); !os.IsNotExist(err) {
		t.Fatalf("cursor failure prevented acknowledgement: %v", err)
	}
	if len(api.requests) != 2 {
		t.Fatal("cursor failure prevented delivered command execution")
	}
	if err := os.Remove(executor.journal.receiptScanPath()); err != nil {
		t.Fatal(err)
	}
	mode.Store(0)
	for range 2 {
		if err := poll(); err == nil || !strings.Contains(err.Error(), legacy.CommandID) {
			t.Fatalf("legacy delivery issue = %v", err)
		}
	}
	if _, err := os.Stat(executor.journal.path(delivered.CommandID)); !os.IsNotExist(err) {
		t.Fatalf("delivered command result was not acknowledged: %v", err)
	}
	if len(api.afters) == 0 {
		t.Fatal("unsendable custody permanently suppressed activity")
	}
	got, err := os.ReadFile(legacyPath)
	if err != nil || !bytes.Equal(got, legacyBytes) {
		t.Fatalf("legacy custody changed: %v", err)
	}
	currentInfo, err := os.Stat(legacyPath)
	if err != nil || !os.SameFile(legacyInfo, currentInfo) {
		t.Fatalf("legacy receipt replaced: %v", err)
	}
}

func TestReceiptEncodingKeepsLegacyCommandIdentity(t *testing.T) {
	now := time.Date(2026, 9, 5, 18, 0, 0, 0, time.UTC)
	command := createCommand(now.Add(time.Minute))
	command.Payload = json.RawMessage(`{"external_ref":"episode:<>&` + "\u2028\u2029" + `","policy":"work-read-only","policy_digest":"` + repeatedDigest("b") + `"}`)
	result := workerproto.CommandResult{
		CommandID: command.CommandID, OperationKey: command.IdempotencyKey, State: "succeeded",
		Resource: json.RawMessage(`{"session":{"id":"session-1"},"value":"<>&` + "\u2028\u2029" + `"}`), Error: json.RawMessage("null"),
	}
	for _, legacy := range []bool{true, false} {
		t.Run(fmt.Sprintf("legacy-%t", legacy), func(t *testing.T) {
			dir := t.TempDir()
			api := &fakeAPI{response: result.Resource}
			open := func() *Executor {
				t.Helper()
				executor, err := NewExecutor(ExecutorConfig{API: api, JournalDir: dir, Now: func() time.Time { return now }, WorkerID: "worker-a"})
				if err != nil {
					t.Fatal(err)
				}
				return executor
			}
			executor := open()
			entry, err := executor.journal.begin(command)
			if err != nil {
				t.Fatal(err)
			}
			if legacy {
				entry.State, entry.Result = "completed", &result
				body, err := json.Marshal(entry)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(executor.journal.path(command.CommandID), body, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if _, err := executor.Execute(context.Background(), command); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				executor = open()
				got, err := executor.Execute(context.Background(), command)
				if err != nil || !sameResult(got, result) {
					t.Fatalf("receipt replay differs: %v", err)
				}
				stored, err := executor.journal.read(executor.journal.path(command.CommandID))
				if err != nil || stored.CommandDigest != entry.CommandDigest {
					t.Fatalf("command identity changed: %v", err)
				}
			}
			wantCalls := 1
			if legacy {
				wantCalls = 0
			}
			if len(api.requests) != wantCalls {
				t.Fatalf("API calls=%d, want %d", len(api.requests), wantCalls)
			}
			changed := command
			changed.Payload = json.RawMessage(`{"external_ref":"changed","policy":"work-read-only","policy_digest":"` + repeatedDigest("b") + `"}`)
			if _, err := executor.Execute(context.Background(), changed); !errors.Is(err, ErrCommandConflict) {
				t.Fatalf("changed command identity accepted: %v", err)
			}
		})
	}
}

func TestHeavyWorkerHelloAndMaximumResultFitTheActualWireEncoding(t *testing.T) {
	for _, unit := range []string{"<>&", "\u2028\u2029"} {
		t.Run(fmt.Sprintf("unit-%x", []byte(unit)), func(t *testing.T) {
			now := time.Date(2026, 9, 5, 18, 0, 0, 0, time.UTC)
			advertisement := hello(now)
			advertisement.WorkspaceRef = strings.Repeat("w", 256)
			advertisement.ProtocolVersion = strings.Repeat("p", 256)
			advertisement.BuildVersion = strings.Repeat("b", 256)
			advertisement.PolicyDigests = make(map[string]string)
			advertisement.PolicyAuthorityDigests = make(map[string]string)
			for index := range workerproto.MaxBatchItems {
				name := fmt.Sprintf("%03d", index) + strings.Repeat("n", 253)
				advertisement.PolicyDigests[name] = repeatedDigest("a")
				advertisement.PolicyAuthorityDigests[name] = repeatedDigest("b")
				advertisement.Repositories = append(advertisement.Repositories, workerproto.Repository{Ref: name, Revision: strings.Repeat("r", 256)})
				advertisement.Capabilities = append(advertisement.Capabilities, workerproto.Capability{Name: name, Version: strings.Repeat("v", 128)})
			}
			const payloadBytes = 768 << 10
			available := payloadBytes - len(`{"value":""}`)
			resource := json.RawMessage(`{"value":"` + strings.Repeat(unit, available/len(unit)) + strings.Repeat("x", available%len(unit)) + `"}`)
			api := &fakeAPI{response: resource}
			dir := t.TempDir()
			executor, err := NewExecutor(ExecutorConfig{API: api, JournalDir: dir, Now: func() time.Time { return now }, WorkerID: "worker-a"})
			if err != nil {
				t.Fatal(err)
			}
			command := createCommand(now.Add(time.Minute))
			command.CommandID, command.IdempotencyKey = strings.Repeat("c", 256), strings.Repeat("k", 512)
			command.Kind, command.Payload = "get_session", json.RawMessage(`{"coop_session_id":"session-1"}`)
			if _, err := executor.Execute(context.Background(), command); err != nil {
				t.Fatal(err)
			}
			executor, err = NewExecutor(ExecutorConfig{API: api, JournalDir: dir, Now: func() time.Time { return now }, WorkerID: "worker-a"})
			if err != nil {
				t.Fatal(err)
			}
			var received atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(io.LimitReader(r.Body, workerproto.MaxDocumentBytes+1))
				if err != nil {
					t.Error(err)
					return
				}
				poll, err := workerproto.DecodePoll(body)
				if err != nil || len(poll.CommandResults) != 1 || !bytes.Equal(poll.CommandResults[0].Resource, resource) {
					t.Errorf("maximal wire result differs (%d bytes): %v", len(body), err)
				}
				received.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(workerproto.Response{
					Version: workerproto.Version, PollRef: poll.PollRef, ServerTime: now,
					AcknowledgedResultCommandIDs: []string{command.CommandID},
				})
			}))
			defer server.Close()
			transport, err := newHTTPTransport(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			connector, err := NewConnector(ConnectorConfig{
				Executor: executor, Hello: func(context.Context, time.Time) workerproto.WorkerHello { return advertisement },
				Now: func() time.Time { return now }, Transport: transport,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := connector.PollOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if received.Load() != 1 || len(api.requests) != 1 {
				t.Fatal("maximal receipt did not deliver exactly once")
			}
			if _, err := os.Stat(executor.journal.path(command.CommandID)); !os.IsNotExist(err) {
				t.Fatalf("maximal result was not acknowledged: %v", err)
			}
		})
	}
}
