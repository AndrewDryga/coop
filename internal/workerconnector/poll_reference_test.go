package workerconnector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestWorkerPollReferencesStayBoundedAcrossSequenceGrowth(t *testing.T) {
	for _, length := range []int{8, 230, 231, 244, 256} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			id := strings.Repeat("w", length)
			connector, _, requests := pollReferenceConnector(t, id, nil)
			seen := make(map[string]bool)
			for _, sequence := range []uint64{999_999, 1_000_000, ^uint64(0)} {
				// Reach the actual wire boundary without a million journal scans.
				connector.sequence = sequence - 1
				if err := connector.PollOnce(context.Background()); err != nil {
					t.Fatalf("sequence %d: %v", sequence, err)
				}
				poll := <-requests
				if len(poll.PollRef) > 256 || seen[poll.PollRef] || poll.Worker.ID != id {
					t.Fatalf("poll reference or advertised identity changed: %+v", poll)
				}
				seen[poll.PollRef] = true
				want := fmt.Sprintf("poll:%s:%d", id, sequence)
				if length > 230 {
					want = fmt.Sprintf("poll-sha256:%x:%d", sha256.Sum256([]byte(id)), sequence)
				}
				if poll.PollRef != want {
					t.Errorf("reference = %q, want %q", poll.PollRef, want)
				}
			}
		})
	}
}

func TestWorkerPollReferencesSeparateWorkerIdentities(t *testing.T) {
	longID := strings.Repeat("w", 256)
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(longID)))
	seen := make(map[string]bool)
	for _, id := range []string{longID, strings.Repeat("x", 256), digest, "sha256:" + digest} {
		connector, _, requests := pollReferenceConnector(t, id, nil)
		if err := connector.PollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		poll := <-requests
		if seen[poll.PollRef] {
			t.Fatalf("different workers collided at %q", poll.PollRef)
		}
		seen[poll.PollRef] = true
	}
}

func TestWorkerPollReferenceMismatchPreservesCustody(t *testing.T) {
	id := strings.Repeat("w", 256)
	var mode atomic.Int32
	command := createCommand(time.Date(2026, 9, 6, 13, 0, 0, 0, time.UTC))
	command.WorkerID, command.Kind = id, "get_session"
	command.Payload = json.RawMessage(`{"coop_session_id":"session-1"}`)
	next := command
	next.CommandID, next.IdempotencyKey = "next-command", "next-operation"
	connector, api, requests := pollReferenceConnector(t, id, func(response *workerproto.Response) {
		response.AcknowledgedResultCommandIDs = []string{command.CommandID}
		response.Commands = []workerproto.Command{next}
		switch mode.Load() {
		case 0:
			response.PollRef = fmt.Sprintf("poll-sha256:%x:0", sha256.Sum256([]byte(id)))
		case 1:
			response.PollRef = fmt.Sprintf("poll-sha256:%x:2", sha256.Sum256([]byte(strings.Repeat("x", 256))))
		}
	})
	ctx := context.Background()
	result, err := connector.executor.Execute(ctx, command)
	if err != nil || result.State != "succeeded" {
		t.Fatalf("seed receipt: %+v, %v", result, err)
	}
	journal := connector.executor.journal
	if err := journal.advanceReceiptScan("previous-command"); err != nil {
		t.Fatal(err)
	}
	before := make(map[string][]byte)
	for _, path := range []string{journal.path(command.CommandID), journal.receiptScanPath()} {
		before[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, mismatch := range []int32{0, 1} {
		mode.Store(mismatch)
		if err := connector.PollOnce(ctx); err == nil || !strings.Contains(err.Error(), "poll identity does not match") {
			t.Fatalf("wrong echo %d was not refused: %v", mismatch, err)
		}
		poll := <-requests
		if len(poll.CommandResults) != 1 || !sameResult(poll.CommandResults[0], result) {
			t.Fatalf("pending result changed: %+v", poll.CommandResults)
		}
		for path, want := range before {
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("wrong echo changed receipt/cursor custody: %v", err)
			}
		}
		if len(api.requests) != 1 {
			t.Fatal("wrong echo executed a response command")
		}
		if _, err := os.Stat(journal.path(next.CommandID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("wrong echo journaled a response command: %v", err)
		}
	}
	mode.Store(2)
	if err := connector.PollOnce(ctx); err != nil {
		t.Fatalf("exact echo/ACK: %v", err)
	}
	<-requests
	if _, err := os.Stat(journal.path(command.CommandID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact ACK retained receipt: %v", err)
	}
	cursor, err := os.ReadFile(journal.receiptScanPath())
	if err != nil || string(cursor) != command.CommandID || len(api.requests) != 2 {
		t.Fatalf("exact response did not advance scan and execute command: cursor=%q calls=%d err=%v", cursor, len(api.requests), err)
	}
}

func TestWorkerPollHashingDoesNotPermitInvalidIdentity(t *testing.T) {
	now := time.Now().UTC()
	for _, id := range []string{"", strings.Repeat("w", 257), strings.Repeat("w", 230) + "/", strings.Repeat("w", 230) + "\x00"} {
		t.Run(fmt.Sprintf("%d-%x", len(id), sha256.Sum256([]byte(id))), func(t *testing.T) {
			if _, err := LoadConfig(pollReferenceConfigPath(t, id), "coop-test", now); err == nil || !strings.Contains(err.Error(), "worker id") {
				t.Fatalf("invalid worker configuration accepted or rejected for wrong reason: %v", err)
			}
			if _, err := NewConnector(ConnectorConfig{
				Executor: &Executor{}, Transport: &HTTPTransport{}, Now: func() time.Time { return now },
				Hello: func(_ context.Context, clock time.Time) workerproto.WorkerHello {
					worker := hello(clock)
					worker.ID = id
					return worker
				},
			}); err == nil || !strings.Contains(err.Error(), "worker id") {
				t.Fatalf("constructor accepted invalid worker or rejected for wrong reason: %v", err)
			}
		})
	}
}

func pollReferenceConfigPath(t *testing.T, id string) string {
	t.Helper()
	document, err := os.ReadFile("../../docs/examples/worker.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw fileConfig
	if err := json.Unmarshal(document, &raw); err != nil {
		t.Fatal(err)
	}
	raw.WorkerID = id
	document, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "worker.json")
	if err := os.WriteFile(path, document, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func pollReferenceConnector(t *testing.T, id string, respond func(*workerproto.Response)) (*Connector, *fakeAPI, <-chan workerproto.Poll) {
	t.Helper()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	configuration, err := LoadConfig(pollReferenceConfigPath(t, id), "coop-test", now)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan workerproto.Poll, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, workerproto.MaxDocumentBytes+1))
		if err != nil {
			t.Error(err)
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		poll, err := workerproto.DecodePoll(body)
		if err != nil {
			t.Error(err)
			http.Error(w, "invalid poll", http.StatusBadRequest)
			return
		}
		requests <- poll
		response := workerproto.Response{Version: workerproto.Version, PollRef: poll.PollRef, ServerTime: now}
		if respond != nil {
			respond(&response)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	transport, err := newHTTPTransport(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{response: json.RawMessage(`{"id":"session-1"}`)}
	executor, err := NewExecutor(ExecutorConfig{API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: id})
	if err != nil {
		t.Fatal(err)
	}
	connector, err := NewConnector(ConnectorConfig{
		Executor: executor, Transport: transport, Now: func() time.Time { return now },
		Hello: func(_ context.Context, clock time.Time) workerproto.WorkerHello {
			worker := configuration.Hello
			worker.ClockAt = clock
			return worker
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return connector, api, requests
}
