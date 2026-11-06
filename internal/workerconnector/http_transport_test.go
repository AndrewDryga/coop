package workerconnector

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/workertls"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func newHTTPTransport(baseURL string, client *http.Client) (*HTTPTransport, error) {
	parsed, err := parseControlPlaneURL(baseURL, false)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("worker HTTP client is required")
	}
	return &HTTPTransport{
		baseURL: parsed, client: client, endpoint: parsed.ResolveReference(&url.URL{Path: "/v1/coop-workers/poll"}).String(),
	}, nil
}

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

// A worker refuses an unusable token file before it starts anything, naming the check it failed and
// what to do about it. Permissions are never loosened: a file other users can open is refused with
// the chmod that makes it private. A link to a private token file is followed, as before.
func TestCheckEnrollmentTokenFile(t *testing.T) {
	dir := t.TempDir()
	token := strings.Repeat("t", 43)
	write := func(name, content string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil { // past the umask
			t.Fatal(err)
		}
		return path
	}
	private := write("private", token, 0o600)
	if err := CheckEnrollmentTokenFile(private); err != nil {
		t.Errorf("private file = %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckEnrollmentTokenFile(link); err != nil {
		t.Errorf("link to a private file = %v", err)
	}
	folder := filepath.Join(dir, "folder")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	open := write("open", "qa", 0o644)
	for _, tc := range []struct {
		path, problem, advice string
		shared                bool
	}{
		{missing, "no file at " + missing, "", false},
		{open, open + " is open to other users (mode 0644)", "", true},
		{write("group", token, 0o640), filepath.Join(dir, "group") + " is open to other users (mode 0640)", "", true},
		{folder, folder + " is a folder", "Give --token-file the token file itself.", false},
		{fifo, fifo + " is not a regular file", "Give --token-file the token file itself.", false},
		{write("empty", "", 0o600), filepath.Join(dir, "empty") + " is empty", "Save the enrollment token in it.", false},
		{write("large", strings.Repeat("t", 129), 0o600), filepath.Join(dir, "large") + " is 129 bytes, larger than an enrollment token (at most 128)",
			"Save only the enrollment token in it.", false},
		{write("short", "qa", 0o600), filepath.Join(dir, "short") + " does not hold an enrollment token",
			"Save the token the controller gave you in it, on one line.", false},
	} {
		err := CheckEnrollmentTokenFile(tc.path)
		var unusable *TokenFileError
		if !errors.As(err, &unusable) || unusable.Problem != tc.problem || unusable.Advice != tc.advice || unusable.Shared != tc.shared || unusable.Path != tc.path {
			t.Errorf("CheckEnrollmentTokenFile(%s) = %#v, want problem %q, advice %q, shared %v", tc.path, err, tc.problem, tc.advice, tc.shared)
		}
	}
	if err := CheckEnrollmentTokenFile(open); err == nil || err.Error() != open+" is open to other users (mode 0644); make it private with chmod 600 "+open {
		t.Errorf("a shared file's error = %v, want the chmod that fixes it", err)
	}
}

func TestControllerUnreachableOnlyForLostRequests(t *testing.T) {
	post := func(cause error) error {
		return fmt.Errorf("request worker identity: %w", &url.Error{Op: "Post", URL: "https://c.example", Err: cause})
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"refused", post(&net.OpError{Op: "dial", Err: errors.New("connection refused")}), true},
		{"no such host", post(&net.DNSError{Err: "no such host", Name: "c.example"}), true},
		{"closed early", post(io.EOF), true},
		{"TLS alert", post(&net.OpError{Op: "remote error", Err: errors.New("tls: protocol version")}), false},
		{"not TLS", post(tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}), false},
		{"plain HTTP", post(http.ErrSchemeMismatch), false},
		{"untrusted", post(x509.UnknownAuthorityError{}), false},
		{"refused token", errors.New("worker enrollment was rejected"), false},
	} {
		if got := ControllerUnreachable(tc.err); got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}
}
