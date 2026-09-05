package workerconnector

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/workertls"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestProductionWorkerRequestsRefuseRedirects(t *testing.T) {
	f := newWorkerRedirectFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	poll := workerproto.Poll{Version: workerproto.Version, PollRef: "poll:worker-a:1", Worker: hello(now)}
	configuration := f.configuration(t)
	if _, err := f.transport(t, configuration).Poll(ctx, poll); err != nil {
		t.Fatalf("direct TLS enrollment/poll control: %v", err)
	}
	identity := readRedirectFixtureFile(t, configuration.IdentityFile)
	checkpoint, bundle := testWorkspaceCheckpoint(t, now)
	output := []byte{137, 80, 78, 71, 13, 10, 26, 10, 'o'}
	patch := []byte("diff --git a/a b/a\n+reviewed\n")
	for _, operation := range []struct {
		name, method, path string
		call               func(*HTTPTransport) error
	}{
		{"enroll", "POST", "/v1/coop-workers/enroll", func(transport *HTTPTransport) error { _, err := transport.Poll(ctx, poll); return err }},
		{"renew", "POST", "/v1/coop-workers/renew", func(transport *HTTPTransport) error {
			if _, err := transport.identity.clientFor(ctx); err != nil {
				return err
			}
			transport.identity.mu.Lock()
			transport.identity.expiresAt = time.Now()
			transport.identity.mu.Unlock()
			_, err := transport.Poll(ctx, poll)
			return err
		}},
		{"poll", "POST", "/v1/coop-workers/poll", func(transport *HTTPTransport) error { _, err := transport.Poll(ctx, poll); return err }},
		{"input", "GET", "/v1/coop-workers/commands/command-1/input-artifacts/input-1", func(transport *HTTPTransport) error {
			_, err := transport.FetchInputArtifact(ctx, "command-1", "input-1")
			return err
		}},
		{"checkpoint_download", "GET", "/v1/coop-workers/commands/command-1/workspace-checkpoints/transfer-1", func(transport *HTTPTransport) error {
			_, _, err := transport.FetchWorkspaceCheckpoint(ctx, "command-1", "transfer-1")
			return err
		}},
		{"output", "PUT", "/v1/coop-workers/commands/command-1/output-artifacts/output-1", func(transport *HTTPTransport) error {
			_, err := transport.UploadOutputArtifact(ctx, "command-1", Artifact{ID: "output-1", Name: "chart.png", MediaType: "image/png", SHA256: sha256sum(output), Data: output})
			return err
		}},
		{"review", "PUT", "/v1/coop-workers/commands/command-1/review-patches/review-1", func(transport *HTTPTransport) error {
			_, err := transport.UploadReviewPatch(ctx, "command-1", "review-1", sha256sum(patch), patch)
			return err
		}},
		{"checkpoint_upload", "PUT", "/v1/coop-workers/commands/command-1/workspace-checkpoints/" + checkpoint.CheckpointRef, func(transport *HTTPTransport) error {
			_, err := transport.UploadWorkspaceCheckpoint(ctx, "command-1", checkpoint, bundle)
			return err
		}},
	} {
		t.Run(operation.name, func(t *testing.T) {
			for _, status := range []int{301, 302, 303, 307, 308} {
				for _, destination := range []struct{ name, url string }{
					{"plaintext", f.plaintext}, {"other_tls", f.otherTLS},
					{"same_origin", f.base.BaseURL + "/redirected"}, {"relative", "/redirected"},
					{"malformed_escape", "/%zz"}, {"malformed_port", "https://redirect.invalid:bad/"},
					{"missing", ""},
				} {
					t.Run(fmt.Sprintf("%d/%s", status, destination.name), func(t *testing.T) {
						current := configuration
						if operation.name == "enroll" {
							current = f.configuration(t)
						}
						location := destination.url
						if location != "" {
							location += "?private-location-canary"
						}
						f.rule.Store(&workerRedirect{method: operation.method, path: operation.path, status: status, destination: location})
						hits, requests, handshakes := f.redirects.Load(), f.destinations.Load(), f.handshakes.Load()
						err := operation.call(f.transport(t, current))
						if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) || strings.Contains(err.Error(), "private-location-canary") {
							t.Errorf("redirect must return only the original bounded status error: %v", err)
						}
						if f.redirects.Load() != hits+1 || f.destinations.Load() != requests || f.handshakes.Load() != handshakes {
							t.Errorf("redirect boundary: source=%d destination_requests=%d destination_handshakes=%d", f.redirects.Load()-hits, f.destinations.Load()-requests, f.handshakes.Load()-handshakes)
						}
						if operation.name == "enroll" {
							if _, err := os.Stat(current.IdentityFile); !errors.Is(err, os.ErrNotExist) {
								t.Errorf("redirected enrollment published identity: %v", err)
							}
							if string(readRedirectFixtureFile(t, current.EnrollmentTokenFile)) != f.token {
								t.Error("redirected enrollment consumed or changed token")
							}
						} else if !bytes.Equal(identity, readRedirectFixtureFile(t, current.IdentityFile)) {
							t.Error("redirect changed the saved identity")
						}
						f.rule.Store(nil)
					})
				}
			}
		})
	}
}

func TestRedirectedWorkerPollRetainsReceiptsUntilDirectAcknowledgement(t *testing.T) {
	f := newWorkerRedirectFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	configuration := f.configuration(t)
	dir := t.TempDir()
	api := &fakeAPI{response: json.RawMessage(`{"id":"session-1","revision":2}`)}
	open := func() *Executor {
		t.Helper()
		executor, err := NewExecutor(ExecutorConfig{API: api, JournalDir: dir, Now: func() time.Time { return now }, WorkerID: "worker-a"})
		if err != nil {
			t.Fatal(err)
		}
		return executor
	}
	executor := open()
	command := createCommand(now.Add(time.Minute))
	command.Kind, command.Payload = "get_session", json.RawMessage(`{"coop_session_id":"session-1"}`)
	result, err := executor.Execute(ctx, command)
	if err != nil || result.State != "succeeded" {
		t.Fatalf("seed real command receipt: %+v, %v", result, err)
	}
	if err := executor.journal.advanceReceiptScan("previous-command"); err != nil {
		t.Fatal(err)
	}
	receipt := readRedirectFixtureFile(t, executor.journal.path(command.CommandID))
	cursor := readRedirectFixtureFile(t, executor.journal.receiptScanPath())
	poll := func() error {
		t.Helper()
		connector, err := NewConnector(ConnectorConfig{
			Executor: open(), Transport: f.transport(t, configuration), Now: func() time.Time { return now },
			Hello: func(_ context.Context, now time.Time) workerproto.WorkerHello { return hello(now) },
		})
		if err != nil {
			t.Fatal(err)
		}
		return connector.PollOnce(ctx)
	}
	f.rule.Store(&workerRedirect{method: "POST", path: "/v1/coop-workers/poll", status: 307, destination: f.plaintext})
	if err := poll(); err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("redirected poll did not fail at original status: %v", err)
	}
	before := f.lastPoll()
	if len(before.CommandResults) != 1 || !sameResult(before.CommandResults[0], result) {
		t.Fatalf("redirect fixture did not receive pending result: %+v", before.CommandResults)
	}
	if f.destinations.Load() != 0 || !bytes.Equal(receipt, readRedirectFixtureFile(t, executor.journal.path(command.CommandID))) ||
		!bytes.Equal(cursor, readRedirectFixtureFile(t, executor.journal.receiptScanPath())) {
		t.Fatal("redirect followed or changed receipt/cursor custody")
	}
	identity := readRedirectFixtureFile(t, configuration.IdentityFile)
	f.rule.Store(nil)
	f.acknowledge.Store(true)
	if err := poll(); err != nil {
		t.Fatalf("direct replay/ACK: %v", err)
	}
	if !reflect.DeepEqual(before.CommandResults, f.lastPoll().CommandResults) || len(api.requests) != 1 {
		t.Fatal("restart changed the receipt or repeated the local operation")
	}
	if _, err := os.Stat(executor.journal.path(command.CommandID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("direct ACK did not release receipt: %v", err)
	}
	if string(readRedirectFixtureFile(t, executor.journal.receiptScanPath())) != command.CommandID {
		t.Fatal("direct ACK did not advance the receipt scan")
	}
	if err := poll(); err != nil || len(f.lastPoll().CommandResults) != 0 {
		t.Fatalf("acknowledged receipt replayed after restart: %v", err)
	}
	if !bytes.Equal(identity, readRedirectFixtureFile(t, configuration.IdentityFile)) {
		t.Fatal("recovery replaced saved identity")
	}
}

type workerRedirect struct {
	method, path, destination string
	status                    int
}

type workerRedirectFixture struct {
	base                                HTTPTransportConfig
	token, plaintext, otherTLS          string
	rule                                atomic.Pointer[workerRedirect]
	redirects, destinations, handshakes atomic.Int32
	acknowledge                         atomic.Bool
	mu                                  sync.Mutex
	received                            workerproto.Poll
}

func newWorkerRedirectFixture(t *testing.T) *workerRedirectFixture {
	t.Helper()
	f := &workerRedirectFixture{token: strings.Repeat("synthetic-only-", 3)}
	ca := workertls.NewCA(t)
	roots := x509.NewCertPool()
	roots.AddCert(ca.Certificate)
	// A followed poll gets a protocol-valid malicious ACK, not just an HTTP
	// failure. Custody must never be released on that redirected response.
	sink := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.destinations.Add(1)
		var poll workerproto.Poll
		if err := json.NewDecoder(r.Body).Decode(&poll); err != nil || poll.PollRef == "" {
			http.Error(w, "fixture destination", http.StatusTeapot)
			return
		}
		writeRedirectFixturePoll(w, poll, true)
	})
	plaintext := httptest.NewServer(sink)
	t.Cleanup(plaintext.Close)
	f.plaintext = plaintext.URL
	other := httptest.NewUnstartedServer(sink)
	other.TLS = &tls.Config{
		Certificates: []tls.Certificate{ca.ServerCertificate(t)}, ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs: roots, MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { f.handshakes.Add(1); return nil, nil },
	}
	other.StartTLS()
	t.Cleanup(other.Close)
	f.otherTLS = other.URL
	controller := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			sink.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != "/v1/coop-workers/enroll" && (r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || r.TLS.PeerCertificates[0].Subject.CommonName != "worker-a") {
			http.Error(w, "verified worker identity required", http.StatusUnauthorized)
			return
		}
		var poll workerproto.Poll
		if r.URL.Path == "/v1/coop-workers/poll" {
			body, err := io.ReadAll(io.LimitReader(r.Body, workerproto.MaxDocumentBytes+1))
			if err == nil {
				poll, err = workerproto.DecodePoll(body)
			}
			if err != nil {
				t.Error(err)
				http.Error(w, "invalid poll", http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.received = poll
			f.mu.Unlock()
		}
		if rule := f.rule.Load(); rule != nil && r.Method == rule.method && r.URL.Path == rule.path {
			f.redirects.Add(1)
			if rule.destination != "" {
				w.Header().Set("Location", rule.destination)
			}
			w.WriteHeader(rule.status)
			return
		}
		switch r.URL.Path {
		case "/v1/coop-workers/enroll":
			var document map[string]string
			if err := json.NewDecoder(r.Body).Decode(&document); err != nil || document["token"] != f.token || document["worker_id"] != "worker-a" || document["workspace_ref"] != "workspace-main" {
				http.Error(w, "invalid bootstrap authority", http.StatusForbidden)
				return
			}
			identity, err := ca.Identity(document["public_key_pem"], "worker-a", "workspace-main")
			if err != nil {
				t.Error(err)
				http.Error(w, "invalid worker key", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(identity)
		case "/v1/coop-workers/poll":
			writeRedirectFixturePoll(w, poll, f.acknowledge.Load())
		default:
			http.NotFound(w, r)
		}
	}))
	controller.TLS = &tls.Config{Certificates: []tls.Certificate{ca.ServerCertificate(t)}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: roots, MinVersion: tls.VersionTLS13}
	controller.StartTLS()
	t.Cleanup(controller.Close)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, ca.PEM, 0o600); err != nil {
		t.Fatal(err)
	}
	f.base = HTTPTransportConfig{BaseURL: controller.URL, CAFile: caPath, WorkerID: "worker-a", WorkspaceRef: "workspace-main", Timeout: 5 * time.Second, RenewBefore: time.Minute}
	return f
}

func (f *workerRedirectFixture) configuration(t *testing.T) HTTPTransportConfig {
	t.Helper()
	dir := t.TempDir()
	configuration := f.base
	configuration.IdentityFile, configuration.EnrollmentTokenFile = filepath.Join(dir, "identity.pem"), filepath.Join(dir, "token")
	if err := os.WriteFile(configuration.EnrollmentTokenFile, []byte(f.token), 0o600); err != nil {
		t.Fatal(err)
	}
	return configuration
}

func (f *workerRedirectFixture) transport(t *testing.T, configuration HTTPTransportConfig) *HTTPTransport {
	t.Helper()
	transport, err := NewHTTPTransport(configuration)
	if err != nil {
		t.Fatal(err)
	}
	return transport
}

func (f *workerRedirectFixture) lastPoll() workerproto.Poll {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.received
}

func writeRedirectFixturePoll(w http.ResponseWriter, poll workerproto.Poll, acknowledge bool) {
	response := workerproto.Response{Version: workerproto.Version, PollRef: poll.PollRef, ServerTime: time.Now().UTC()}
	if acknowledge {
		for _, result := range poll.CommandResults {
			response.AcknowledgedResultCommandIDs = append(response.AcknowledgedResultCommandIDs, result.CommandID)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func readRedirectFixtureFile(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}
