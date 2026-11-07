package workerconnector

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

const (
	maxEnrollmentTokenBytes = 128
	enrollPath              = "/v1/coop-workers/enroll"
)

type identityConfig struct {
	baseURL             *url.URL
	identityFile        string
	enrollmentTokenFile string
	renewBefore         time.Duration
	timeout             time.Duration
}

type identityManager struct {
	config identityConfig
	roots  *x509.CertPool

	mu           sync.Mutex
	client       *http.Client
	expiresAt    time.Time
	workerID     string
	workspaceRef string
}

type identityResponse struct {
	CACertificatePEM   string `json:"ca_certificate_pem"`
	CertificateExpires string `json:"certificate_expires_at"`
	CertificatePEM     string `json:"certificate_pem"`
	CertificateSHA256  string `json:"certificate_sha256"`
	WorkerID           string `json:"worker_id"`
	WorkspaceRef       string `json:"workspace_ref"`
}

type persistedIdentity struct {
	ControllerURL    string `json:"controller_url"`
	WorkerID         string `json:"worker_id"`
	WorkspaceRef     string `json:"workspace_ref"`
	CertificatePEM   string `json:"certificate_pem"`
	PrivateKeyPEM    string `json:"private_key_pem"`
	CACertificatePEM string `json:"ca_certificate_pem"`
}

func newIdentityManager(config identityConfig, roots *x509.CertPool) (*identityManager, error) {
	if config.baseURL == nil || roots == nil || !filepath.IsAbs(config.identityFile) ||
		config.renewBefore < time.Minute || config.renewBefore > 24*time.Hour || config.timeout <= 0 {
		return nil, errors.New("worker identity configuration is invalid")
	}
	if config.enrollmentTokenFile != "" && !filepath.IsAbs(config.enrollmentTokenFile) {
		return nil, errors.New("worker enrollment token file must be absolute")
	}
	return &identityManager{config: config, roots: roots}, nil
}

func (m *identityManager) clientFor(ctx context.Context) (*http.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now().UTC()
	if m.client == nil {
		if err := m.loadOrEnroll(ctx, now); err != nil {
			return nil, err
		}
	}
	if !now.Add(m.config.renewBefore).Before(m.expiresAt) {
		if err := m.renew(ctx, now); err != nil {
			return nil, err
		}
	}
	return m.client, nil
}

func (m *identityManager) loadOrEnroll(ctx context.Context, now time.Time) error {
	client, expiresAt, err := m.loadIdentity(now)
	if err == nil {
		m.client, m.expiresAt = client, expiresAt
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return m.enroll(ctx, now)
}

func (m *identityManager) enroll(ctx context.Context, now time.Time) error {
	if m.config.enrollmentTokenFile == "" {
		return errors.New("worker identity is absent and no enrollment token file is configured")
	}
	token, err := readEnrollmentToken(m.config.enrollmentTokenFile)
	if err != nil {
		return err
	}
	privateKey, publicKeyPEM, privateKeyPEM, err := newWorkerKey()
	if err != nil {
		return err
	}
	document := map[string]string{
		"public_key_pem": publicKeyPEM,
		"token":          token,
	}
	bootstrap := m.httpClient(nil)
	response, err := m.postIdentity(ctx, bootstrap, enrollPath, document, http.StatusCreated)
	if err != nil {
		return err
	}
	client, expiresAt, bundle, err := m.prepareIdentity(response, privateKey, privateKeyPEM, now)
	if err != nil {
		return err
	}
	if err := atomicWriteIdentity(m.config.identityFile, bundle); err != nil {
		return err
	}
	m.client, m.expiresAt = client, expiresAt
	m.workerID, m.workspaceRef = response.WorkerID, response.WorkspaceRef
	if err := os.Remove(m.config.enrollmentTokenFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove consumed worker enrollment token: %w", err)
	}
	return nil
}

func (m *identityManager) renew(ctx context.Context, now time.Time) error {
	privateKey, publicKeyPEM, privateKeyPEM, err := newWorkerKey()
	if err != nil {
		return err
	}
	response, err := m.postIdentity(
		ctx,
		m.client,
		"/v1/coop-workers/renew",
		map[string]string{"public_key_pem": publicKeyPEM},
		http.StatusOK,
	)
	if err != nil {
		return err
	}
	client, expiresAt, bundle, err := m.prepareIdentity(response, privateKey, privateKeyPEM, now)
	if err != nil {
		return err
	}
	if err := atomicWriteIdentity(m.config.identityFile, bundle); err != nil {
		return err
	}
	m.client, m.expiresAt = client, expiresAt
	return nil
}

func (m *identityManager) loadIdentity(now time.Time) (*http.Client, time.Time, error) {
	info, err := os.Lstat(m.config.identityFile)
	if err != nil {
		return nil, time.Time{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 64<<10 {
		return nil, time.Time{}, errors.New("worker identity file must be a private regular file")
	}
	document, err := os.ReadFile(m.config.identityFile)
	if err != nil {
		return nil, time.Time{}, err
	}
	var identity persistedIdentity
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&identity) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		identity.ControllerURL != m.config.baseURL.String() ||
		!reference(identity.WorkerID, 256) || !reference(identity.WorkspaceRef, 256) {
		return nil, time.Time{}, errors.New("load worker identity: invalid identity or another controller")
	}
	certificate, err := tls.X509KeyPair([]byte(identity.CertificatePEM), []byte(identity.PrivateKeyPEM))
	if err != nil {
		return nil, time.Time{}, errors.New("worker identity key pair is invalid")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(identity.CACertificatePEM)) {
		return nil, time.Time{}, errors.New("worker identity issuer is invalid")
	}
	leaf, err := validateIdentityCertificate(certificate, roots, identity.WorkerID, now)
	if err != nil {
		return nil, time.Time{}, err
	}
	m.workerID, m.workspaceRef = identity.WorkerID, identity.WorkspaceRef
	return m.httpClient(&certificate), leaf.NotAfter, nil
}

func (m *identityManager) prepareIdentity(response identityResponse, privateKey *rsa.PrivateKey, privateKeyPEM []byte, now time.Time) (*http.Client, time.Time, []byte, error) {
	if !reference(response.WorkerID, 256) || !reference(response.WorkspaceRef, 256) ||
		(m.workerID != "" && (response.WorkerID != m.workerID || response.WorkspaceRef != m.workspaceRef)) ||
		!digest(response.CertificateSHA256) {
		return nil, time.Time{}, nil, errors.New("worker enrollment response identity does not match")
	}
	// HTTPS authenticates the controller before its private client-certificate issuer is accepted.
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(response.CACertificatePEM)) {
		return nil, time.Time{}, nil, errors.New("worker enrollment issuer is invalid")
	}
	certBlock, rest := pem.Decode([]byte(response.CertificatePEM))
	if certBlock == nil || certBlock.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 || sha256sum(certBlock.Bytes) != response.CertificateSHA256 {
		return nil, time.Time{}, nil, errors.New("worker enrollment certificate is invalid")
	}
	certificate, err := tls.X509KeyPair([]byte(response.CertificatePEM), privateKeyPEM)
	if err != nil {
		return nil, time.Time{}, nil, errors.New("worker certificate does not match generated key")
	}
	leaf, err := validateIdentityCertificate(certificate, roots, response.WorkerID, now)
	if err != nil {
		return nil, time.Time{}, nil, err
	}
	if parsed, err := time.Parse(time.RFC3339Nano, response.CertificateExpires); err != nil || !parsed.Equal(leaf.NotAfter) {
		return nil, time.Time{}, nil, errors.New("worker enrollment expiry does not match certificate")
	}
	bundle, err := json.Marshal(persistedIdentity{
		ControllerURL: m.config.baseURL.String(), WorkerID: response.WorkerID, WorkspaceRef: response.WorkspaceRef,
		CertificatePEM: response.CertificatePEM, PrivateKeyPEM: string(privateKeyPEM), CACertificatePEM: response.CACertificatePEM,
	})
	if err != nil {
		return nil, time.Time{}, nil, err
	}
	return m.httpClient(&certificate), leaf.NotAfter, bundle, nil
}

func (m *identityManager) postIdentity(ctx context.Context, client *http.Client, path string, document any, expectedStatus int) (identityResponse, error) {
	body, err := json.Marshal(document)
	if err != nil {
		return identityResponse{}, err
	}
	endpoint := m.config.baseURL.ResolveReference(&url.URL{Path: path}).String()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return identityResponse{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return identityResponse{}, fmt.Errorf("request worker identity: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, workerproto.MaxDocumentBytes+1))
	if err != nil {
		return identityResponse{}, fmt.Errorf("read worker identity response: %w", err)
	}
	if len(responseBody) > workerproto.MaxDocumentBytes || response.StatusCode != expectedStatus {
		return identityResponse{}, &IdentityStatusError{Path: path, Status: response.StatusCode}
	}
	var result identityResponse
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return identityResponse{}, fmt.Errorf("decode worker identity response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return identityResponse{}, errors.New("worker identity response has trailing data")
	}
	return result, nil
}

func (m *identityManager) httpClient(certificate *tls.Certificate) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ForceAttemptHTTP2 = true
	transport.MaxResponseHeaderBytes = 64 << 10
	transport.ResponseHeaderTimeout = m.config.timeout
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: m.roots}
	if certificate != nil {
		tlsConfig.Certificates = []tls.Certificate{*certificate}
	}
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Timeout: m.config.timeout, Transport: workerRoundTripper{transport}}
}

type workerRoundTripper struct{ *http.Transport }

func (t workerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.Transport.RoundTrip(request)
	if response != nil {
		// Fixed API responses never authorize navigation. Client parses Location
		// before CheckRedirect, leaking malformed values into errors; remove it
		// first so callers receive only the original bounded status and body.
		response.Header.Del("Location")
	}
	return response, err
}

func validateIdentityCertificate(certificate tls.Certificate, roots *x509.CertPool, workerID string, now time.Time) (*x509.Certificate, error) {
	if len(certificate.Certificate) == 0 {
		return nil, errors.New("worker identity contains no certificate")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse worker certificate: %w", err)
	}
	if leaf.Subject.CommonName != workerID {
		return nil, errors.New("worker certificate common name does not match worker identity")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return nil, fmt.Errorf("verify worker certificate: %w", err)
	}
	return leaf, nil
}

func newWorkerKey() (*rsa.PrivateKey, string, []byte, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, "", nil, fmt.Errorf("generate worker identity key: %w", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, "", nil, fmt.Errorf("encode worker public key: %w", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, "", nil, fmt.Errorf("encode worker private key: %w", err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	return privateKey, string(publicPEM), privatePEM, nil
}

// ControllerUnreachable reports a request lost on the network: refused, reset, timed out or not
// resolved. A controller that answers in a way the worker cannot use, such as a TLS alert, plain HTTP
// or a certificate it does not trust, stays that way however long the worker waits.
func ControllerUnreachable(err error) bool {
	var request *url.Error
	if !errors.As(err, &request) {
		return false
	}
	var verification *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &verification) || errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		return false
	}
	var operation *net.OpError
	if errors.As(err, &operation) {
		return operation.Op != "remote error" // the peer's TLS alert
	}
	var lookup *net.DNSError
	return errors.As(err, &lookup) || request.Timeout() || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// ControllerCertificateUntrusted reports a controller whose certificate was signed by an authority
// this worker does not trust, which --ca-file can add. A certificate for another name, or one that
// has expired, is a different problem that --ca-file does not fix.
func ControllerCertificateUntrusted(err error) bool {
	var unknownAuthority x509.UnknownAuthorityError
	return errors.As(err, &unknownAuthority)
}

// IdentityStatusError is a controller's answer to a worker identity request that was not the success
// status. Its text is a bare status line, and TokenRefused names the one answer a person can fix.
type IdentityStatusError struct {
	Path   string // the route asked, such as /v1/coop-workers/enroll
	Status int
}

func (e *IdentityStatusError) Error() string {
	return fmt.Sprintf("worker identity endpoint returned HTTP %d", e.Status)
}

// TokenRefused reports a 401 or 403 to the enrollment request: the controller does not accept the
// token, and sending it again cannot change that. A renewal sends no token, so its 401 or 403 is not this.
func (e *IdentityStatusError) TokenRefused() bool {
	return e.Path == enrollPath && (e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden)
}

// CheckEnrollmentTokenFile refuses an unusable token file before a worker starts anything, in words
// a person can act on. The first connection reads it again and consumes it.
func CheckEnrollmentTokenFile(path string) error {
	_, err := readEnrollmentToken(path)
	var unusable *TokenFileError
	switch {
	case errors.Is(err, os.ErrNotExist):
		return &TokenFileError{Path: path, Problem: "no file at " + path}
	case errors.As(err, &unusable):
		return err
	case err != nil:
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// TokenFileError is an enrollment token file a worker cannot use: Problem names the check it
// failed and Advice what to do about it. Shared marks a file other users can open, which
// `chmod 600 <Path>` makes private; the caller shows that command.
type TokenFileError struct {
	Path, Problem, Advice string
	Shared                bool
}

func (e *TokenFileError) Error() string {
	switch {
	case e.Shared:
		return e.Problem + "; make it private with chmod 600 " + e.Path
	case e.Advice != "":
		return e.Problem + ". " + e.Advice
	}
	return e.Problem
}

// readEnrollmentToken reads the one-time token a first connection exchanges for worker credentials:
// a private, token-sized regular file. A link to one is followed, as a secret mount such as
// /run/secrets often is one; each check it fails is named, with what fixes it.
func readEnrollmentToken(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat worker enrollment token: %w", err)
	}
	switch mode := info.Mode(); {
	case mode.IsDir():
		return "", &TokenFileError{Path: path, Problem: path + " is a folder", Advice: "Give --token-file the token file itself."}
	case !mode.IsRegular():
		return "", &TokenFileError{Path: path, Problem: path + " is not a regular file", Advice: "Give --token-file the token file itself."}
	case mode.Perm()&0o077 != 0:
		return "", &TokenFileError{Path: path, Problem: fmt.Sprintf("%s is open to other users (mode %04o)", path, mode.Perm()), Shared: true}
	case info.Size() == 0:
		return "", &TokenFileError{Path: path, Problem: path + " is empty", Advice: "Save the enrollment token in it."}
	case info.Size() > maxEnrollmentTokenBytes:
		return "", &TokenFileError{Path: path, Problem: fmt.Sprintf("%s is %d bytes, larger than an enrollment token (at most %d)", path, info.Size(), maxEnrollmentTokenBytes),
			Advice: "Save only the enrollment token in it."}
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read worker enrollment token: %w", err)
	}
	token := strings.TrimSpace(string(value))
	if len(token) < 32 || len(token) > maxEnrollmentTokenBytes || strings.ContainsAny(token, "\r\n\t ") {
		return "", &TokenFileError{Path: path, Problem: path + " does not hold an enrollment token",
			Advice: "Save the token the controller gave you in it, on one line."}
	}
	return token, nil
}

func atomicWriteIdentity(path string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create worker identity directory: %w", err)
	}
	if err := config.WriteFileAtomic(path, contents); err != nil {
		return fmt.Errorf("install worker identity: %w", err)
	}
	return nil
}
