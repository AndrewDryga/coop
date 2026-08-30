package workerconnector

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
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
	caCertificate, caKey, caPEM := testWorkerCA(t)
	serverCertificate := testSignedCertificate(t, caCertificate, caKey, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	})
	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(caCertificate)
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
			writeWorkerIdentityResponse(t, response, request, caCertificate, caKey, caPEM, http.StatusCreated)
		case "/v1/coop-workers/renew":
			if len(request.TLS.PeerCertificates) != 1 {
				t.Errorf("renewal client certificates = %d", len(request.TLS.PeerCertificates))
			}
			renewCount.Add(1)
			writeWorkerIdentityResponse(t, response, request, caCertificate, caKey, caPEM, http.StatusOK)
		case "/v1/coop-workers/poll":
			if len(request.TLS.PeerCertificates) != 1 || request.TLS.PeerCertificates[0].Subject.CommonName != "worker-a" {
				t.Errorf("poll client identity = %+v", request.TLS.PeerCertificates)
			}
			var poll workerproto.Poll
			if err := json.NewDecoder(request.Body).Decode(&poll); err != nil {
				t.Fatal(err)
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
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(strings.Repeat("t", 43)), 0o600); err != nil {
		t.Fatal(err)
	}

	transport, err := NewHTTPTransport(HTTPTransportConfig{
		BaseURL: server.URL, CAFile: caPath, EnrollmentTokenFile: tokenPath,
		IdentityFile: identityPath, RenewBefore: time.Minute, Timeout: time.Minute,
		WorkerID: "worker-a", WorkspaceRef: "workspace-main",
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

func TestHTTPTransportMovesArtifactBytesOnlyThroughTheCommandScopedRoutes(t *testing.T) {
	input := []byte("exact authenticated input")
	inputSHA := sha256.Sum256(input)
	output := []byte{137, 80, 78, 71, 13, 10, 26, 10, 'o'}
	outputSHA := sha256.Sum256(output)
	reviewPatch := []byte("diff --git a/a b/a\n+reviewed\n")
	reviewSHA := sha256.Sum256(reviewPatch)
	checkpoint, checkpointBundle := testWorkspaceCheckpoint(t, time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/input-artifacts/artifact:input:1"):
			response.Header().Set("Content-Type", "text/plain")
			response.Header().Set("X-Responder-Artifact-Name", base64.RawURLEncoding.EncodeToString([]byte("input.txt")))
			response.Header().Set("X-Responder-Artifact-SHA256", hex.EncodeToString(inputSHA[:]))
			_, _ = response.Write(input)

		case request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/output-artifacts/artifact_chart"):
			body, _ := io.ReadAll(request.Body)
			if string(body) != string(output) || request.Header.Get("Content-Type") != "image/png" ||
				request.Header.Get("X-Responder-Artifact-SHA256") != hex.EncodeToString(outputSHA[:]) {
				t.Errorf("output upload headers=%v body=%q", request.Header, body)
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"transfer_id":"018f04f4-3333-7000-8000-000000000001"}`))

		case request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/review-patches/review-artifact-1"):
			body, _ := io.ReadAll(request.Body)
			if string(body) != string(reviewPatch) || request.Header.Get("Content-Type") != "text/x-diff" ||
				request.Header.Get("X-Responder-Artifact-SHA256") != hex.EncodeToString(reviewSHA[:]) {
				t.Errorf("review upload headers=%v body=%q", request.Header, body)
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"transfer_id":"018f04f4-4444-7000-8000-000000000001"}`))

		case request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/workspace-checkpoints/"+checkpoint.CheckpointRef):
			body, _ := io.ReadAll(request.Body)
			descriptor, decodeErr := base64.RawURLEncoding.DecodeString(request.Header.Get("X-Responder-Checkpoint-Descriptor"))
			decoded, descriptorErr := workerproto.DecodeWorkspaceCheckpoint(descriptor)
			if !bytes.Equal(body, checkpointBundle) || request.Header.Get("Content-Type") != workerproto.WorkspaceCheckpointBundleMediaType ||
				request.Header.Get("X-Responder-Checkpoint-SHA256") != checkpoint.Bundle.SHA256 || decodeErr != nil || descriptorErr != nil ||
				decoded.CheckpointRef != checkpoint.CheckpointRef {
				t.Errorf("checkpoint upload headers=%v body_bytes=%d decode=%v descriptor=%v", request.Header, len(body), decodeErr, descriptorErr)
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"checkpoint_ref":"` + checkpoint.CheckpointRef + `","state":"stored"}`))

		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/workspace-checkpoints/018f04f4-5555-7000-8000-000000000001"):
			descriptor, _ := json.Marshal(checkpoint)
			response.Header().Set("Content-Type", workerproto.WorkspaceCheckpointBundleMediaType)
			response.Header().Set("X-Responder-Checkpoint-Descriptor", base64.RawURLEncoding.EncodeToString(descriptor))
			response.Header().Set("X-Responder-Checkpoint-SHA256", checkpoint.Bundle.SHA256)
			_, _ = response.Write(checkpointBundle)

		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	transport, err := newHTTPTransport(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	inputArtifact, err := transport.FetchInputArtifact(context.Background(), "018f04f4-1111-7000-8000-000000000001", "artifact:input:1")
	if err != nil || string(inputArtifact.Data) != string(input) || inputArtifact.Name != "input.txt" {
		t.Fatalf("input artifact = %+v, %v", inputArtifact, err)
	}
	resource, err := transport.UploadOutputArtifact(context.Background(), "018f04f4-1111-7000-8000-000000000001", Artifact{
		ID: "artifact_chart", Name: "chart.png", MediaType: "image/png", SHA256: hex.EncodeToString(outputSHA[:]), Data: output,
	})
	if err != nil || !strings.Contains(string(resource), "transfer_id") {
		t.Fatalf("upload resource = %s, %v", resource, err)
	}
	reviewResource, err := transport.UploadReviewPatch(
		context.Background(), "018f04f4-1111-7000-8000-000000000001",
		"review-artifact-1", hex.EncodeToString(reviewSHA[:]), reviewPatch,
	)
	if err != nil || !strings.Contains(string(reviewResource), "transfer_id") {
		t.Fatalf("review resource = %s, %v", reviewResource, err)
	}
	checkpointResource, err := transport.UploadWorkspaceCheckpoint(
		context.Background(), "018f04f4-1111-7000-8000-000000000001", checkpoint, checkpointBundle,
	)
	if err != nil || !strings.Contains(string(checkpointResource), checkpoint.CheckpointRef) {
		t.Fatalf("checkpoint resource = %s, %v", checkpointResource, err)
	}
	fetchedCheckpoint, fetchedBundle, err := transport.FetchWorkspaceCheckpoint(
		context.Background(), "018f04f4-1111-7000-8000-000000000001",
		"018f04f4-5555-7000-8000-000000000001",
	)
	if err != nil || fetchedCheckpoint.CheckpointRef != checkpoint.CheckpointRef ||
		!bytes.Equal(fetchedBundle, checkpointBundle) {
		t.Fatalf("fetched checkpoint = %+v bytes=%d, %v", fetchedCheckpoint, len(fetchedBundle), err)
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
		RenewBefore: time.Hour, Timeout: time.Minute, WorkerID: "worker-a", WorkspaceRef: "workspace-main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if transport.identity == nil || transport.client != nil {
		t.Fatalf("transport identity manager = %+v", transport.identity)
	}
}

func testWorkerCA(t *testing.T) (*x509.Certificate, *rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Responder Test Worker CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func testSignedCertificate(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey, template *x509.Certificate) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func writeWorkerIdentityResponse(t *testing.T, response http.ResponseWriter, request *http.Request, ca *x509.Certificate, caKey *rsa.PrivateKey, caPEM []byte, status int) {
	t.Helper()
	var document map[string]string
	if err := json.NewDecoder(request.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(document["public_key_pem"]))
	if block == nil {
		t.Fatal("missing public key")
	}
	publicKey, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: "worker-a"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(10 * time.Minute).Truncate(time.Second),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, publicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(identityResponse{
		CACertificatePEM: string(caPEM), CertificateExpires: template.NotAfter.Format(time.RFC3339),
		CertificatePEM:    string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		CertificateSHA256: sha256sum(der), WorkerID: "worker-a", WorkspaceRef: "workspace-main",
	})
}
