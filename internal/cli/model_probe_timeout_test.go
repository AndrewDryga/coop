package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestACPModelHandshakeStartupHasIndependentBudget(t *testing.T) {
	child, methods, replies, adapterDone := controlledModelChild(t)
	finished := make(chan error, 1)
	const catalog = 100 * time.Millisecond
	go func() {
		_, err := acpModelHandshakeWithin(t.Context(), child, "/repo", agents.ModelCatalogSpec{}, 2*time.Second, catalog)
		finished <- err
	}()
	requireModelPhase(t, methods, "initialize")
	// This elapsed budget is the regression itself: startup must not consume the catalog's limit.
	timer := time.NewTimer(2 * catalog)
	defer timer.Stop()
	<-timer.C
	replies <- json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	requireModelPhase(t, methods, "session/new")
	replies <- json.RawMessage(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"catalog"}}`)
	if err := <-finished; err != nil {
		t.Fatal("healthy startup consumed catalog budget", err)
	}
	child.Stop()
	<-adapterDone
}

func TestACPModelHandshakePhaseTimeoutsAndCancellation(t *testing.T) {
	for _, phase := range []string{"initialize", "authenticate", "session/new"} {
		for _, cancelled := range []bool{false, true} {
			name := phase + "/deadline"
			if cancelled {
				name = phase + "/cancellation"
			}
			t.Run(name, func(t *testing.T) {
				child, methods, replies, adapterDone := controlledModelChild(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				startup, catalog := time.Second, time.Second
				if !cancelled {
					if phase != "session/new" {
						startup = 30 * time.Millisecond
					} else {
						catalog = 30 * time.Millisecond
					}
				}
				finished := make(chan error, 1)
				catalogSpec := agents.ModelCatalogSpec{}
				if phase == "authenticate" {
					catalogSpec.ACPAuthMethod = func(json.RawMessage) (string, error) { return "broker", nil }
				}
				go func() {
					_, err := acpModelHandshakeWithin(ctx, child, "/repo", catalogSpec, startup, catalog)
					finished <- err
				}()
				requireModelPhase(t, methods, "initialize")
				if phase != "initialize" {
					replies <- json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`)
					requireModelPhase(t, methods, phase)
				}
				want := context.DeadlineExceeded
				if cancelled {
					cancel()
					want = context.Canceled
				}
				select {
				case err := <-finished:
					if !errors.Is(err, want) || !cancelled && ctx.Err() != nil {
						t.Fatalf("phase error=%v parent=%v, want%v", err, ctx.Err(), want)
					}
				case <-time.After(wait.Deadline):
					t.Fatal("hung model phase did not stop")
				}
				child.Stop()
				select {
				case <-adapterDone:
				case <-time.After(wait.Deadline):
					t.Fatal("model teardown did not release pipe operations")
				}
				if len(methods) != 0 {
					t.Fatal("failed phase continued to another request")
				}
			})
		}
	}
}

type modelProbeRequest struct {
	ID     int
	Method string
	Params map[string]any
}

func requireModelPhase(t *testing.T, methods <-chan modelProbeRequest, want string) modelProbeRequest {
	t.Helper()
	select {
	case request := <-methods:
		if request.Method != want {
			t.Fatalf("model request=%q, want%q", request.Method, want)
		}
		return request
	case <-time.After(wait.Deadline):
		t.Fatal("model request not received")
	}
	return modelProbeRequest{}
}

func controlledModelChild(t *testing.T) (*acpproxy.Child, <-chan modelProbeRequest, chan<- json.RawMessage, <-chan struct{}) {
	t.Helper()
	inputR, inputW := io.Pipe()
	outputR, outputW := io.Pipe()
	methods := make(chan modelProbeRequest, 4)
	replies := make(chan json.RawMessage)
	stopped, done := make(chan struct{}), make(chan struct{})
	var once sync.Once
	child := &acpproxy.Child{In: inputW, Out: outputR, Stop: func() {
		once.Do(func() {
			close(stopped)
			_ = inputR.Close()
			_ = inputW.Close()
			_ = outputR.Close()
			_ = outputW.Close()
		})
	}}
	t.Cleanup(child.Stop)
	go func() {
		defer close(done)
		decoder, encoder := json.NewDecoder(inputR), json.NewEncoder(outputW)
		for {
			var request modelProbeRequest
			if decoder.Decode(&request) != nil {
				return
			}
			methods <- request
			select {
			case reply := <-replies:
				if encoder.Encode(reply) != nil {
					return
				}
			case <-stopped:
				return
			}
		}
	}()
	return child, methods, replies, done
}
