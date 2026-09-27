package workerconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUnixAPIKeepsTheSessionControllerPrivateAndBounded(t *testing.T) {
	socket := unixSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/sessions" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Idempotency-Key") != "create-1" {
			t.Errorf("idempotency key = %q", request.Header.Get("Idempotency-Key"))
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"operation":{"id":"operation-1","state":"running"}}`))
	})}
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close() })

	api, err := NewUnixAPI(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := api.Do(context.Background(), Request{
		Method: "POST", Path: "/v1/sessions", IdempotencyKey: "create-1", Body: []byte(`{"policy":"work","task":"episode"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(result, &decoded); err != nil || decoded["operation"] == nil {
		t.Fatalf("result = %s, %v", result, err)
	}
}

func TestUnixAPIReadsTheBoundedPublicSessionEventPage(t *testing.T) {
	now := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)
	socket := unixSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/sessions/session-1/events" ||
			request.URL.Query().Get("after") != "4" || request.URL.Query().Get("limit") != "20" {
			t.Errorf("request = %s %s", request.Method, request.URL.String())
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`[{"id":"evt-5","session_id":"session-1","sequence":5,"turn_id":"turn-1","type":"model.thought","version":1,"occurred_at":"` + now.Format(time.RFC3339Nano) + `","payload":{"text":"Inspect the exact runtime."}}]`))
	})}
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close() })

	api, err := NewUnixAPI(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	events, err := api.ListEvents(context.Background(), "session-1", 4, 20)
	if err != nil || len(events) != 1 || events[0].ID != "evt-5" || events[0].Sequence != 5 ||
		!bytes.Contains(events[0].Payload, []byte("exact runtime")) {
		t.Fatalf("events = %+v, %v", events, err)
	}
}

func TestUnixAPIProducesTypedConfirmedErrors(t *testing.T) {
	socket := unixSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusConflict)
		_, _ = response.Write([]byte(`{"code":"revision_conflict","detail":"expected revision 1"}`))
	})}
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close() })

	api, err := NewUnixAPI(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.Do(context.Background(), Request{Method: "GET", Path: "/v1/operations?key=x"})
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Status != 409 || apiErr.Code != "revision_conflict" {
		t.Fatalf("error = %#v", err)
	}
}

func unixSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "coop-worker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "api.sock")
}

// A request the connector refuses before sending is a definite failure — the daemon never saw it —
// and the cap it refuses at admits the daemon's own turn and fence sizes.
func TestUnixAPIPreSendRejectionsAreDefiniteAndAdmitDaemonSizedBodies(t *testing.T) {
	api, err := NewUnixAPI(filepath.Join(t.TempDir(), "absent.sock"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for name, request := range map[string]Request{
		"oversized body":  {Method: "POST", Path: "/v1/sessions", IdempotencyKey: "k", Body: bytes.Repeat([]byte("x"), maxPrivateRequestBytes+1)},
		"GET with a body": {Method: "GET", Path: "/v1/sessions", Body: []byte("{}")},
		"bad path":        {Method: "GET", Path: "/nope"},
		"no key":          {Method: "POST", Path: "/v1/sessions", Body: []byte("{}")},
	} {
		if _, err := api.Do(context.Background(), request); !errors.Is(err, ErrRequestRejected) {
			t.Errorf("%s: err = %v, want a pre-send rejection", name, err)
		}
	}
	// A 12 MiB body passes the connector's own cap and fails only at the absent socket.
	large := Request{Method: "POST", Path: "/v1/sessions", IdempotencyKey: "k", Body: bytes.Repeat([]byte("x"), 12<<20)}
	if _, err := api.Do(context.Background(), large); err == nil || errors.Is(err, ErrRequestRejected) {
		t.Fatalf("a daemon-sized body = %v; want it to reach the socket", err)
	}
	command := createCommand(time.Now().Add(time.Minute))
	result := resultFromCall(command, nil, fmt.Errorf("%w: body exceeds cap", ErrRequestRejected))
	if result.State != "failed" || !strings.Contains(string(result.Error), "invalid_command") {
		t.Fatalf("pre-send rejection result = %+v; want failed/invalid_command, not uncertain", result)
	}
}
