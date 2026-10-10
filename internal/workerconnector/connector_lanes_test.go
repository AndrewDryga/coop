package workerconnector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

type gatedCommandAPI struct {
	gates    map[string]chan struct{}
	mu       sync.Mutex
	requests []Request
}

func (a *gatedCommandAPI) Do(context.Context, Request) (json.RawMessage, error) {
	return nil, errors.New("unexpected non-streaming request")
}

func (a *gatedCommandAPI) Forward(ctx context.Context, request Request, _ io.Reader) (*http.Response, error) {
	a.mu.Lock()
	a.requests = append(a.requests, request)
	a.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-a.gates[request.Path]:
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}
}

func (a *gatedCommandAPI) paths() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	paths := make([]string, len(a.requests))
	for i, request := range a.requests {
		paths[i] = request.Path
	}
	return paths
}

func laneCommand(index int, method string) workerproto.Command {
	command := createCommand(time.Now().Add(time.Hour))
	command.CommandID = fmt.Sprintf("018f04f4-1111-7000-8000-%012d", index)
	command.IdempotencyKey = fmt.Sprintf("lane:%d", index)
	command.Payload, _ = json.Marshal(workerproto.APIRequest{Method: method, Path: fmt.Sprintf("/v1/sessions/s/turns/%d", index)})
	return command
}

func commandPath(command workerproto.Command) string {
	request, _ := workerproto.DecodeAPIRequest(command.Payload)
	return request.Path
}

func runCommandLanes(t *testing.T, commands []workerproto.Command, api *gatedCommandAPI, observe func(workerproto.Poll, *workerproto.Response), failures ...chan error) (*Executor, func()) {
	t.Helper()
	executor, err := NewExecutor(ExecutorConfig{API: api, JournalDir: t.TempDir(), Now: time.Now, WorkerID: "worker-a"})
	if err != nil {
		t.Fatal(err)
	}
	polls := 0
	transport := pollTransportFunc(func(_ context.Context, poll workerproto.Poll) (workerproto.Response, error) {
		response := workerproto.Response{Version: workerproto.Version, PollRef: poll.PollRef, ServerTime: time.Now()}
		polls++
		if polls == 1 {
			response.Commands = commands
		}
		if observe != nil {
			observe(poll, &response)
		}
		return response, nil
	})
	connector, err := NewConnector(ConnectorConfig{Executor: executor, Hello: func(_ context.Context, clock time.Time) workerproto.WorkerHello { return hello(clock) }, Now: time.Now, Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- connector.Run(ctx, time.Minute, func(err error) {
			if len(failures) > 0 {
				failures[0] <- err
			} else {
				t.Errorf("connector: %v", err)
			}
		})
	}()
	var stopped sync.Once
	stop := func() {
		stopped.Do(func() {
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Errorf("Run: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return executor, stop
}

func TestConnectorReadBypassesSlowCommandAndPreservesMutationOrder(t *testing.T) {
	for _, slowMethod := range []string{http.MethodPost, http.MethodGet} {
		t.Run(slowMethod, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				first, second, read := laneCommand(1, slowMethod), laneCommand(2, http.MethodPost), laneCommand(3, http.MethodGet)
				read.SessionRef = "018f04f4-2222-7000-8000-000000000003"
				read.Payload = apiPayload(http.MethodGet, "/v1/sessions/other/turns/3", nil)
				api := &gatedCommandAPI{gates: map[string]chan struct{}{}}
				for _, command := range []workerproto.Command{first, second, read} {
					api.gates[commandPath(command)] = make(chan struct{})
				}
				close(api.gates[commandPath(read)])
				reported := false
				executor, stop := runCommandLanes(t, []workerproto.Command{first, second, read}, api, func(poll workerproto.Poll, _ *workerproto.Response) {
					for _, result := range poll.CommandResults {
						if result.CommandID == read.CommandID {
							reported = true
						}
					}
				})
				synctest.Wait()
				paths := api.paths()
				wantCalls := 2
				if slowMethod == http.MethodGet {
					wantCalls = 3
				}
				if len(paths) != wantCalls || !reported {
					t.Fatalf("blocked %s: paths=%v, read reported=%t", slowMethod, paths, reported)
				}
				if slowMethod == http.MethodPost {
					for _, path := range paths {
						if path == commandPath(second) {
							t.Fatal("second mutation overtook the first")
						}
					}
				}
				close(api.gates[commandPath(first)])
				synctest.Wait()
				if len(api.paths()) != 3 {
					t.Fatalf("queued mutation did not start: %v", api.paths())
				}
				close(api.gates[commandPath(second)])
				synctest.Wait()
				stop()
				// Restarted executors replay each saved receipt without forwarding again.
				reopened, err := NewExecutor(ExecutorConfig{API: api, JournalDir: filepath.Dir(executor.journal.dir), Now: time.Now, WorkerID: "worker-a"})
				if err != nil {
					t.Fatal(err)
				}
				ids := []string{}
				for _, command := range []workerproto.Command{first, second, read} {
					result, err := reopened.Execute(context.Background(), command)
					if err != nil || result.State != "succeeded" {
						t.Fatalf("replay: %+v, %v", result, err)
					}
					ids = append(ids, command.CommandID)
				}
				if len(api.paths()) != 3 {
					t.Fatal("replay forwarded a command twice")
				}
				if err := reopened.journal.acknowledgeResults(ids); err != nil {
					t.Fatal(err)
				}
				if entries, err := reopened.journal.pending(); err != nil || len(entries) != 0 {
					t.Fatalf("ACK custody: %v, %v", entries, err)
				}
			})
		})
	}
}

func TestReadOnlyCommandRequiresAValidatedReadRequest(t *testing.T) {
	for _, fixture := range []struct {
		payload  string
		readOnly bool
	}{
		{`{"method":"GET","path":"/v1/sessions/s"}`, true},
		{`{"method":"HEAD","path":"/v1/operations/k"}`, true},
		{`{"method":"POST","path":"/v1/sessions/s/close"}`, false},
		{`{"method":"DELETE","path":"/v1/sessions/s"}`, false},
		{`{"method":"PATCH","path":"/v1/sessions/s"}`, false},
		{`{"method":"GET","path":"/v1/sessions/s","body":{}}`, false},
		{`{"method":"GET","path":"/v1/sessions/s","unknown":true}`, false},
		{`{"method":"GET","path":"https://example.com/v1/sessions"}`, false},
		{`{"method":"get","path":"/v1/sessions/s"}`, false},
	} {
		command := laneCommand(1, http.MethodGet)
		command.Payload = json.RawMessage(fixture.payload)
		if got := readOnlyCommand(command); got != fixture.readOnly {
			t.Errorf("readOnlyCommand(%s)=%t, want %t", fixture.payload, got, fixture.readOnly)
		}
	}
}

func TestConnectorQueuedReadExpiresBeforeForwarding(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &gatedCommandAPI{gates: map[string]chan struct{}{}}
		commands := []workerproto.Command{}
		for i := 1; i <= 5; i++ {
			command := laneCommand(i, http.MethodGet)
			api.gates[commandPath(command)] = make(chan struct{})
			commands = append(commands, command)
		}
		commands[4].LeaseExpiresAt = time.Now().Add(time.Second)
		failures := make(chan error, 4)
		executor, _ := runCommandLanes(t, commands, api, nil, failures)
		synctest.Wait()
		time.Sleep(2 * time.Second)
		close(api.gates[commandPath(commands[0])])
		synctest.Wait()
		if paths := api.paths(); len(paths) != 4 {
			t.Fatalf("expired queued read forwarded: %v", paths)
		}
		select {
		case err := <-failures:
			// The lease watcher can cancel before Execute checks the deadline.
			if !errors.Is(err, ErrLeaseExpired) && !errors.Is(err, context.Canceled) {
				t.Fatalf("queued expiry: %v", err)
			}
		default:
			t.Fatal("queued expiry was not reported")
		}
		if _, err := executor.journal.read(executor.journal.path(commands[4].CommandID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expired queued read published a receipt: %v", err)
		}
	})
}

func TestConnectorActiveReadRedeliveryRenewsWithoutDuplicatingOrChangingItsLane(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		command := laneCommand(1, http.MethodGet)
		command.LeaseExpiresAt = time.Now().Add(90 * time.Second)
		api := &gatedCommandAPI{gates: map[string]chan struct{}{commandPath(command): make(chan struct{})}}
		failures := make(chan error, 4)
		polls := 0
		runCommandLanes(t, []workerproto.Command{command}, api, func(_ workerproto.Poll, response *workerproto.Response) {
			polls++
			if polls == 2 {
				renewed := command
				renewed.LeaseExpiresAt = time.Now().Add(time.Hour)
				response.Commands = []workerproto.Command{renewed}
			}
			if polls == 3 {
				changed := command
				changed.Payload = laneCommand(2, http.MethodPost).Payload
				response.Commands = []workerproto.Command{changed}
			}
		}, failures)
		synctest.Wait()
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if paths := api.paths(); len(paths) != 1 {
			t.Fatalf("active redelivery forwarded twice: %v", paths)
		}
		select {
		case err := <-failures:
			if !errors.Is(err, ErrCommandConflict) {
				t.Fatalf("changed read identity: %v", err)
			}
		default:
			t.Fatal("changed payload was not rejected")
		}
		if len(failures) != 0 {
			t.Fatal("renewed read expired at its old deadline")
		}
		close(api.gates[commandPath(command)])
		synctest.Wait()
		if len(api.paths()) != 1 {
			t.Fatal("conflicting mutation executed after read settlement")
		}
	})
}

func TestConnectorBoundsReadLaneWithoutBlockingMutationOrShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &gatedCommandAPI{gates: map[string]chan struct{}{}}
		commands := []workerproto.Command{}
		for i := 1; i <= 6; i++ {
			command := laneCommand(i, http.MethodGet)
			commands = append(commands, command)
			api.gates[commandPath(command)] = make(chan struct{})
		}
		mutation := laneCommand(7, http.MethodPost)
		commands = append(commands, mutation, laneCommand(8, http.MethodPost))
		api.gates[commandPath(mutation)] = make(chan struct{})
		_, stop := runCommandLanes(t, commands, api, nil)
		synctest.Wait()
		paths := api.paths()
		if len(paths) != 5 {
			t.Fatalf("active lanes=%v; want four reads and one mutation", paths)
		}
		foundMutation := false
		for _, path := range paths {
			if path == commandPath(mutation) {
				foundMutation = true
			}
		}
		if !foundMutation {
			t.Fatal("full read lane blocked a mutation behind queued reads")
		}
		close(api.gates[commandPath(commands[0])])
		synctest.Wait()
		if len(api.paths()) != 6 {
			t.Fatalf("free read slot did not admit exactly one read: %v", api.paths())
		}
		// Cancel all five active lanes with two jobs still queued. Run must
		// finish executor completion sends rather than deadlock or start them.
		stop()
		paths = api.paths()
		if len(paths) != 6 {
			t.Fatalf("shutdown admitted queued jobs: %v", paths)
		}
		for _, path := range paths {
			if path == commandPath(commands[5]) || path == commandPath(commands[7]) {
				t.Fatalf("queued job reached the API during shutdown: %s", path)
			}
		}
	})
}
