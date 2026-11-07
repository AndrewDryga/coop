package workerconnector

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
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

// A controller's own answer reaches the caller with the route it was given for. A refused enrollment
// token (401 or 403, and the token file stays for a new one) is told apart from the same status on a
// renewal, which sends no token.
func TestIdentityRequestsReturnTheStatusTheControllerAnswered(t *testing.T) {
	ca := workertls.NewCA(t)
	var enrolling atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/coop-workers/enroll" && enrolling.Load() {
			writeWorkerIdentityResponse(t, response, request, ca, http.StatusCreated)
			return
		}
		response.WriteHeader(http.StatusForbidden) // a refused token, and later a refused renewal
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{ca.ServerCertificate(t)}, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	defer server.Close()

	directory := t.TempDir()
	caPath := filepath.Join(directory, "ca.pem")
	tokenPath := filepath.Join(directory, "enrollment-token")
	identityPath := filepath.Join(directory, "identity.json")
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

	_, err = transport.Identity(context.Background())
	var status *IdentityStatusError
	if !errors.As(err, &status) || status.Path != "/v1/coop-workers/enroll" || status.Status != http.StatusForbidden || !status.TokenRefused() {
		t.Fatalf("a refused enrollment = %#v, want the enrollment route's 403 as a refused token", err)
	}
	if err.Error() != "worker identity endpoint returned HTTP 403" {
		t.Errorf("the status line = %q", err)
	}
	if _, err := os.Stat(tokenPath); err != nil {
		t.Errorf("a refused token file was removed: %v", err)
	}
	if _, err := os.Stat(identityPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused enrollment saved an identity: %v", err)
	}

	enrolling.Store(true)
	if _, err := transport.Identity(context.Background()); err != nil {
		t.Fatal(err)
	}
	transport.identity.mu.Lock()
	transport.identity.expiresAt = time.Now()
	transport.identity.mu.Unlock()
	_, err = transport.Identity(context.Background())
	if !errors.As(err, &status) || status.Path != "/v1/coop-workers/renew" || status.Status != http.StatusForbidden || status.TokenRefused() {
		t.Fatalf("a refused renewal = %#v, want the renewal route's 403, which is not a refused token", err)
	}
}

// Only the enrollment route's 401 and 403 are a refused token.
func TestIdentityStatusErrorTokenRefused(t *testing.T) {
	for _, tc := range []struct {
		path   string
		status int
		want   bool
	}{
		{"/v1/coop-workers/enroll", http.StatusUnauthorized, true},
		{"/v1/coop-workers/enroll", http.StatusForbidden, true},
		{"/v1/coop-workers/enroll", http.StatusNotFound, false},
		{"/v1/coop-workers/enroll", http.StatusInternalServerError, false},
		{"/v1/coop-workers/renew", http.StatusUnauthorized, false},
		{"/v1/coop-workers/renew", http.StatusForbidden, false},
	} {
		if got := (&IdentityStatusError{Path: tc.path, Status: tc.status}).TokenRefused(); got != tc.want {
			t.Errorf("TokenRefused() for %d on %s = %v, want %v", tc.status, tc.path, got, tc.want)
		}
	}
}

// Only a certificate from an authority the worker does not know is the --ca-file problem. One for
// another name, or an expired one, is not fixed by it and keeps its own error.
func TestControllerCertificateUntrustedOnlyForAnUnknownAuthority(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the worker sent a request over a connection it could not verify")
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshake is the point
	server.StartTLS()
	defer server.Close()
	directory := t.TempDir()
	tokenPath := filepath.Join(directory, "enrollment-token")
	if err := os.WriteFile(tokenPath, []byte(strings.Repeat("t", 43)), 0o600); err != nil {
		t.Fatal(err)
	}

	// The server's own certificate is in no trust store this machine has.
	transport, err := NewHTTPTransport(HTTPTransportConfig{
		BaseURL: server.URL, EnrollmentTokenFile: tokenPath, IdentityFile: filepath.Join(directory, "identity.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = transport.Identity(context.Background())
	if !ControllerCertificateUntrusted(err) || ControllerUnreachable(err) {
		t.Errorf("a certificate from an unknown authority = %v, want it untrusted and not unreachable", err)
	}

	verification := func(cause error) error {
		return &url.Error{Op: "Post", URL: server.URL, Err: &tls.CertificateVerificationError{Err: cause}}
	}
	for name, err := range map[string]error{
		"another name": verification(x509.HostnameError{Certificate: server.Certificate(), Host: "controller.example"}),
		"expired":      verification(x509.CertificateInvalidError{Cert: server.Certificate(), Reason: x509.Expired}),
		"status line":  errors.New("worker identity endpoint returned HTTP 403"),
	} {
		if ControllerCertificateUntrusted(err) {
			t.Errorf("%s was called untrusted", name)
		}
	}
}

// savedIdentity is a state folder holding a valid saved identity for a controller, with what a test
// needs to change it: the fields of the file and when its certificate ends.
type savedIdentity struct {
	t                       *testing.T
	document                []byte
	fields                  map[string]any
	notAfter                time.Time
	identityPath, tokenPath string
}

func newSavedIdentity(t *testing.T, controller string) *savedIdentity {
	t.Helper()
	_, publicKey, privateKey, err := newWorkerKey()
	if err != nil {
		t.Fatal(err)
	}
	issued, err := workertls.NewCA(t).Identity(publicKey, "worker-a", "workspace-main")
	if err != nil {
		t.Fatal(err)
	}
	notAfter, err := time.Parse(time.RFC3339, issued["certificate_expires_at"])
	if err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal(persistedIdentity{
		ControllerURL: controller, WorkerID: "worker-a", WorkspaceRef: "workspace-main",
		CertificatePEM: issued["certificate_pem"], PrivateKeyPEM: string(privateKey), CACertificatePEM: issued["ca_certificate_pem"],
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(document, &fields); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	saved := &savedIdentity{
		t: t, document: document, fields: fields, notAfter: notAfter,
		identityPath: filepath.Join(directory, "identity.json"), tokenPath: filepath.Join(directory, "enrollment-token"),
	}
	if err := os.WriteFile(saved.tokenPath, []byte(strings.Repeat("t", 43)), 0o600); err != nil {
		t.Fatal(err)
	}
	return saved
}

// with is the valid file with one field changed.
func (s *savedIdentity) with(key string, value any) []byte {
	s.t.Helper()
	changed := maps.Clone(s.fields)
	changed[key] = value
	document, err := json.Marshal(changed)
	if err != nil {
		s.t.Fatal(err)
	}
	return document
}

// load saves file with mode as the identity of a worker asked for controller and loads it at now. A
// load never changes the file or its mode, and never uses the token waiting beside it.
func (s *savedIdentity) load(file []byte, mode os.FileMode, controller string, now time.Time) error {
	t := s.t
	t.Helper()
	if err := os.Remove(s.identityPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.identityPath, file, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.identityPath, mode); err != nil {
		t.Fatal(err)
	}
	transport, err := NewHTTPTransport(HTTPTransportConfig{BaseURL: controller, EnrollmentTokenFile: s.tokenPath, IdentityFile: s.identityPath})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = transport.identity.loadIdentity(now)
	if after, readErr := os.ReadFile(s.identityPath); readErr != nil || !bytes.Equal(after, file) {
		t.Errorf("the saved identity was changed: %v", readErr)
	}
	if info, statErr := os.Lstat(s.identityPath); statErr != nil || info.Mode().Perm() != mode {
		t.Errorf("the saved identity's mode was changed: %v, %v", info, statErr)
	}
	if _, statErr := os.Stat(s.tokenPath); statErr != nil {
		t.Errorf("a token was used over a saved identity: %v", statErr)
	}
	return err
}

// A saved identity this worker cannot use says why. One saved for another controller carries that
// controller's address, and a damaged file, or one with an unusable address, key pair or issuer, says
// it is damaged. The file is never touched, and a token never enrolls over it.
func TestLoadIdentityNamesAnotherControllerAndADamagedFile(t *testing.T) {
	const controller = "https://controller.example"
	saved := newSavedIdentity(t, controller)
	now := time.Now().UTC()
	if err := saved.load(saved.document, 0o600, controller, now); err != nil {
		t.Fatalf("a saved identity for this controller = %v", err)
	}
	var unusable *SavedIdentityError
	if err := saved.load(saved.document, 0o600, "https://other.example", now); !errors.As(err, &unusable) || unusable.Controller != controller || unusable.Path != saved.identityPath {
		t.Errorf("an identity saved for another controller = %#v, want %s and %s", err, controller, saved.identityPath)
	}
	for _, tc := range []struct {
		name string
		file []byte
	}{
		{"not JSON", []byte("{not json")},
		{"an unknown field", saved.with("extra", true)},
		{"no worker", saved.with("worker_id", "")},
		{"an address no worker enrolls with", saved.with("controller_url", "http://insecure.example")},
		{"a key pair that does not match", saved.with("private_key_pem", "nope")},
		{"an issuer that is no certificate", saved.with("ca_certificate_pem", "nope")},
	} {
		if err := saved.load(tc.file, 0o600, controller, now); !errors.As(err, &unusable) || unusable.Controller != "" || unusable.Problem == "" || unusable.Path != saved.identityPath {
			t.Errorf("a saved identity with %s = %#v, want a damaged file at %s", tc.name, err, saved.identityPath)
		}
	}
}

// The controller a saved identity is bound to is the same however its address is spelled: with or
// without a trailing slash or the default port, in any case. Another host or port is another
// controller whatever the spelling.
func TestSavedIdentityIsForTheSameControllerInAnySpelling(t *testing.T) {
	const controller = "https://controller.example"
	saved := newSavedIdentity(t, controller)
	now := time.Now().UTC()
	for _, asked := range []string{controller + "/", "https://CONTROLLER.Example", controller + ":443", "https://Controller.Example:443/"} {
		if err := saved.load(saved.document, 0o600, asked, now); err != nil {
			t.Errorf("an identity saved for %s, asked for %s = %v", controller, asked, err)
		}
	}
	spelled := "https://Controller.Example:443/"
	if err := saved.load(saved.with("controller_url", spelled), 0o600, controller, now); err != nil {
		t.Errorf("an identity saved for %s, asked for %s = %v", spelled, controller, err)
	}
	for _, asked := range []string{"https://other.example", controller + ":8443", "https://sub.controller.example", "https://controller.example.other"} {
		var unusable *SavedIdentityError
		if err := saved.load(saved.document, 0o600, asked, now); !errors.As(err, &unusable) || unusable.Controller != controller {
			t.Errorf("an identity saved for %s, asked for %s = %#v, want another controller", controller, asked, err)
		}
	}
}

// An identity is saved with the plain spelling of its controller, whichever way it was typed, so the
// next load compares like with like.
func TestEnrollmentSavesTheControllerInOneSpelling(t *testing.T) {
	privateKey, publicKey, privateKeyPEM, err := newWorkerKey()
	if err != nil {
		t.Fatal(err)
	}
	issued, err := workertls.NewCA(t).Identity(publicKey, "worker-a", "workspace-main")
	if err != nil {
		t.Fatal(err)
	}
	transport, err := NewHTTPTransport(HTTPTransportConfig{
		BaseURL: "https://Controller.Example:443/", IdentityFile: filepath.Join(t.TempDir(), "identity.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, bundle, err := transport.identity.prepareIdentity(identityResponse{
		CACertificatePEM: issued["ca_certificate_pem"], CertificateExpires: issued["certificate_expires_at"],
		CertificatePEM: issued["certificate_pem"], CertificateSHA256: issued["certificate_sha256"],
		WorkerID: issued["worker_id"], WorkspaceRef: issued["workspace_ref"],
	}, privateKey, privateKeyPEM, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var saved persistedIdentity
	if err := json.Unmarshal(bundle, &saved); err != nil || saved.ControllerURL != "https://controller.example" {
		t.Errorf("the saved controller = %q, %v, want https://controller.example", saved.ControllerURL, err)
	}
}

// An identity whose certificate has ended says when, and one other users can open says its mode.
// A certificate that is not valid yet means a clock that is behind, which keeps its own error. Neither
// file is changed: coop never tightens a mode itself.
func TestLoadIdentityNamesAnExpiredIdentityAndAnExposedFile(t *testing.T) {
	const controller = "https://controller.example"
	saved := newSavedIdentity(t, controller)
	now := time.Now().UTC()
	var unusable *SavedIdentityError
	if err := saved.load(saved.document, 0o600, controller, now.Add(time.Hour)); !errors.As(err, &unusable) || !unusable.Expired.Equal(saved.notAfter) || unusable.Path != saved.identityPath {
		t.Errorf("an identity an hour past its certificate = %#v, want it expired at %s", err, saved.notAfter)
	}
	if err := saved.load(saved.document, 0o600, controller, now.Add(-time.Hour)); err == nil || errors.As(err, &unusable) {
		t.Errorf("an identity before its certificate is valid = %#v, want the x509 error", err)
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660} {
		if err := saved.load(saved.document, mode, controller, now); !errors.As(err, &unusable) || unusable.Mode != mode || unusable.Path != saved.identityPath {
			t.Errorf("an identity file with mode %04o = %#v, want it open to other users", mode, err)
		}
	}
	for _, mode := range []os.FileMode{0o600, 0o400} {
		if err := saved.load(saved.document, mode, controller, now); err != nil {
			t.Errorf("an identity file with mode %04o = %v, want it private", mode, err)
		}
	}
}

// canonicalController keeps the host and the port that is not the default, and nothing else.
func TestCanonicalControllerIsOneSpellingOfAnOrigin(t *testing.T) {
	for asked, want := range map[string]string{
		"https://controller.example":           "https://controller.example",
		"https://controller.example/":          "https://controller.example",
		"https://Controller.Example":           "https://controller.example",
		"https://controller.example:443/":      "https://controller.example",
		"https://controller.example:8443":      "https://controller.example:8443",
		"https://127.0.0.1:59442/":             "https://127.0.0.1:59442",
		"https://[::1]:8443/":                  "https://[::1]:8443",
		"https://[::1]:443":                    "https://[::1]",
		"https://sub.controller.example":       "https://sub.controller.example",
		"https://controller.example.other:443": "https://controller.example.other",
	} {
		parsed, err := parseControlPlaneURL(asked, true)
		if err != nil {
			t.Fatal(err)
		}
		if got := canonicalController(parsed); got != want {
			t.Errorf("canonicalController(%s) = %s, want %s", asked, got, want)
		}
	}
}
