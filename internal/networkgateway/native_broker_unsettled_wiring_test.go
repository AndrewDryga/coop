package networkgateway

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// The revision watermark survives a slow read: a document that comes back with an older revision
// is still a rollback, and still stops the broker for good.
func TestNativeBrokerSlowReadKeepsTheRollbackWatermark(t *testing.T) {
	f := newNativeTransportFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "native") }))
	f.change(func(s *NativeAccessSnapshot) { s.Revision = 5; s.Credential = "private-access-five" })
	if status := nativeStatus(t, f); status != http.StatusOK {
		t.Fatalf("first request: %d", status)
	}
	f.failReads(&nativeDocumentFailure{reason: "document_canceled", attempts: 1})
	if status := nativeStatus(t, f); status != http.StatusForbidden || f.stopped() {
		t.Fatalf("slow read: status %d, stopped %v", status, f.stopped())
	}
	f.change(func(s *NativeAccessSnapshot) { s.Revision = 3; s.Credential = "private-access-three" })
	f.failReads(nil)
	if response, err := f.client.Do(f.request(t, "/http")); err == nil {
		_ = response.Body.Close()
		if response.StatusCode == http.StatusOK {
			t.Fatal("an older revision was served after a slow read")
		}
	}
	if !f.stopped() {
		t.Fatal("a rollback after a slow read did not stop the broker")
	}
	f.broker.mu.Lock()
	reason := f.broker.stop.reason
	f.broker.mu.Unlock()
	if reason != "revision_rollback" {
		t.Fatalf("stopped for %q, want revision_rollback", reason)
	}
}

// A stream opened inside the lease is cut once the lease lapses unread.
func TestNativeBrokerLeaseLapsedWhileUnreadClosesOpenStream(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	f := newNativeTransportFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	f.change(func(s *NativeAccessSnapshot) { s.Revision++; s.Expires = time.Now().Add(2 * time.Second) })
	response, err := f.client.Do(f.request(t, "/sse"))
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("open stream: %v %v", response, err)
	}
	defer response.Body.Close()
	f.failReads(&nativeDocumentFailure{reason: "document_canceled", attempts: 1})
	ended := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, response.Body); ended <- err }()
	select {
	case <-ended:
		if !f.stopped() {
			t.Fatal("the stream ended before the broker stopped")
		}
	case <-time.After(wait.Deadline):
		t.Fatal("an open stream outlived its unread lease")
	}
}

func nativeTestCertificate(t *testing.T, host string) (certificate, key []byte) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalECPrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded})
}

// The runtime hands the guard's event sink to each broker it builds, so a provider refused there
// reaches the run's denials. The broker tests set the sink themselves and cannot see that wiring.
func TestNativeRuntimeRecordsBrokerRefusalsAsRunEvidence(t *testing.T) {
	certificate, key := nativeTestCertificate(t, "example.com")
	binding := NativeBrokerBinding{RunID: "run-one", Provider: "codex", Account: "selected", Epoch: 7}
	snapshot := NativeAccessSnapshot{Binding: binding, Revision: 1, Expires: time.Now().Add(time.Hour), Credential: "private-access-one"}
	var mu sync.Mutex
	var readErr error
	events := NewGuardEvents(nil)
	runtime := &nativeRunRuntime{events: events,
		config: NativeRunConfig{Accounts: []NativeAccountConfig{{Binding: binding, Origins: []NativeOriginConfig{{
			Host: "example.com", Certificate: certificate, Key: key,
			Requests: []NativeBrokerRequestLine{{Method: http.MethodGet, Path: "/http", Header: "Authorization", HeaderPrefix: "Bearer "}},
		}}}}},
		dial: map[string]func(context.Context, string, string) (net.Conn, error){
			"example.com": func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("no upstream in this test")
			},
		},
		fallback: http.NotFoundHandler(),
		read: func(_ context.Context, _ string, value any) error {
			mu.Lock()
			defer mu.Unlock()
			if readErr != nil {
				return readErr
			}
			data, _ := json.Marshal(snapshot)
			return json.Unmarshal(data, value)
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() { done <- runtime.serve(ctx, func() { close(ready) }) }()
	defer func() { cancel(); <-done }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("native runtime did not start: %v", err)
	case <-time.After(wait.Deadline):
		t.Fatal("native runtime did not start")
	}
	mu.Lock()
	readErr = &nativeDocumentFailure{reason: "document_canceled", attempts: 1}
	mu.Unlock()
	conn, err := net.DialTimeout("tcp4", NativeProxyAddress, wait.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(wait.Deadline))
	if _, err := io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT during a slow read: %d", response.StatusCode)
	}
	refusals, _ := events.Drain(MaxGuardEvents)
	for _, event := range refusals {
		if event.Kind == "admission_failed" && event.Name == "example.com" && event.Reason == "native_account_unavailable" {
			return
		}
	}
	t.Fatalf("the runtime's broker refusal never reached the run's events: %+v", refusals)
}
