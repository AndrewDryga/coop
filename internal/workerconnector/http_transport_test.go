package workerconnector

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/workertls"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestHTTPTransportPostsOneBoundedStrictPoll(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/coop-workers/poll" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type = %q", request.Header.Get("Content-Type"))
		}
		var poll workerproto.Poll
		if err := json.NewDecoder(request.Body).Decode(&poll); err != nil {
			t.Errorf("decode poll: %v", err)
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(workerproto.Response{
			Version: workerproto.Version, PollRef: poll.PollRef, ServerTime: now,
			AcknowledgedResultCommandIDs: []string{}, Commands: []workerproto.Command{},
			EventAcknowledgements: []workerproto.EventAcknowledgement{},
		})
	}))
	defer server.Close()

	transport, err := newHTTPTransport(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	poll := workerproto.Poll{
		Version: workerproto.Version, PollRef: "poll:worker-a:1", Worker: hello(now),
		AcknowledgedCommandIDs: []string{}, CommandResults: []workerproto.CommandResult{}, EventBatches: []workerproto.EventBatch{},
	}
	result, err := transport.Poll(context.Background(), poll)
	if err != nil {
		t.Fatal(err)
	}
	if result.PollRef != poll.PollRef {
		t.Fatalf("response = %+v", result)
	}
}

func TestProductionHTTPTransportEnrollsAndRotatesAWorkerOwnedIdentity(t *testing.T) {
	ca := workertls.NewCA(t)
	serverCertificate := ca.ServerCertificate(t)
	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(ca.Certificate)
	var enrollCount atomic.Int32
	var renewCount atomic.Int32
	var pollCount atomic.Int32

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/coop-workers/enroll":
			if len(request.TLS.PeerCertificates) != 0 {
				t.Errorf("enrollment unexpectedly had a client certificate")
			}
			enrollCount.Add(1)
			writeWorkerIdentityResponse(t, response, request, ca, http.StatusCreated)
		case "/v1/coop-workers/renew":
			if len(request.TLS.VerifiedChains) == 0 || len(request.TLS.PeerCertificates) != 1 {
				t.Errorf("renewal client certificates = %d", len(request.TLS.PeerCertificates))
			}
			renewCount.Add(1)
			writeWorkerIdentityResponse(t, response, request, ca, http.StatusOK)
		case "/v1/coop-workers/poll":
			if len(request.TLS.VerifiedChains) == 0 || len(request.TLS.PeerCertificates) != 1 || request.TLS.PeerCertificates[0].Subject.CommonName != "worker-a" {
				t.Errorf("poll client identity = %+v", request.TLS.PeerCertificates)
			}
			var poll workerproto.Poll
			if err := json.NewDecoder(request.Body).Decode(&poll); err != nil {
				t.Error(err)
				http.Error(response, "invalid poll", http.StatusBadRequest)
				return
			}
			pollCount.Add(1)
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(workerproto.Response{
				Version: workerproto.Version, PollRef: poll.PollRef, ServerTime: time.Now().UTC(),
				AcknowledgedResultCommandIDs: []string{}, Commands: []workerproto.Command{},
				EventAcknowledgements: []workerproto.EventAcknowledgement{},
			})
		default:
			http.NotFound(response, request)
		}
	}))
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCertificate}, ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs: clientRoots, MinVersion: tls.VersionTLS13,
	}
	server.StartTLS()
	defer server.Close()

	directory := t.TempDir()
	caPath := filepath.Join(directory, "ca.pem")
	tokenPath := filepath.Join(directory, "enrollment-token")
	identityPath := filepath.Join(directory, "identity.pem")
	if err := os.WriteFile(caPath, ca.PEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(strings.Repeat("t", 43)), 0o600); err != nil {
		t.Fatal(err)
	}

	transport, err := NewHTTPTransport(HTTPTransportConfig{
		BaseURL: server.URL, CAFile: caPath, EnrollmentTokenFile: tokenPath,
		IdentityFile: identityPath, RenewBefore: time.Minute, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	poll := workerproto.Poll{
		Version: workerproto.Version, PollRef: "poll:worker-a:enroll", Worker: hello(now),
		AcknowledgedCommandIDs: []string{}, CommandResults: []workerproto.CommandResult{}, EventBatches: []workerproto.EventBatch{},
	}
	if _, err := transport.Poll(context.Background(), poll); err != nil {
		t.Fatal(err)
	}
	if enrollCount.Load() != 1 || pollCount.Load() != 1 || renewCount.Load() != 0 {
		t.Fatalf("requests enroll=%d renew=%d poll=%d", enrollCount.Load(), renewCount.Load(), pollCount.Load())
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("consumed token still exists: %v", err)
	}
	if info, err := os.Stat(identityPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("identity file mode = %v, %v", info, err)
	}

	transport.identity.mu.Lock()
	transport.identity.expiresAt = time.Now()
	transport.identity.mu.Unlock()
	poll.PollRef = "poll:worker-a:renew"
	if _, err := transport.Poll(context.Background(), poll); err != nil {
		t.Fatal(err)
	}
	if enrollCount.Load() != 1 || renewCount.Load() != 1 || pollCount.Load() != 2 {
		t.Fatalf("requests enroll=%d renew=%d poll=%d", enrollCount.Load(), renewCount.Load(), pollCount.Load())
	}
}

func TestProductionHTTPTransportRequiresHTTPSAndAWorkerOwnedIdentityFile(t *testing.T) {
	if _, err := NewHTTPTransport(HTTPTransportConfig{BaseURL: "http://responder.example"}); err == nil {
		t.Fatal("plaintext worker control plane was accepted")
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	directory := t.TempDir()
	certificate := server.TLS.Certificates[0]
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})
	privateKey, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey})
	caPath := filepath.Join(directory, "ca.pem")
	identityPath := filepath.Join(directory, "worker-identity.pem")
	for path, body := range map[string][]byte{caPath: certificatePEM, identityPath: append(certificatePEM, keyPEM...)} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	transport, err := NewHTTPTransport(HTTPTransportConfig{
		BaseURL: server.URL, CAFile: caPath, IdentityFile: identityPath,
		RenewBefore: time.Hour, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if transport.identity == nil || transport.client != nil {
		t.Fatalf("transport identity manager = %+v", transport.identity)
	}
}

func writeWorkerIdentityResponse(t *testing.T, response http.ResponseWriter, request *http.Request, ca *workertls.CA, status int) {
	t.Helper()
	var document map[string]string
	if err := json.NewDecoder(request.Body).Decode(&document); err != nil {
		t.Error(err)
		http.Error(response, "invalid identity request", http.StatusBadRequest)
		return
	}
	identity, err := ca.Identity(document["public_key_pem"], "worker-a", "workspace-main")
	if err != nil {
		t.Error(err)
		http.Error(response, "invalid worker key", http.StatusBadRequest)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(identity)
}
