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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

const maxEnrollmentTokenBytes = 128

type identityConfig struct {
	baseURL             *url.URL
	caDER               []byte
	identityFile        string
	enrollmentTokenFile string
	workerID            string
	workspaceRef        string
	renewBefore         time.Duration
	timeout             time.Duration
}

type identityManager struct {
	config identityConfig
	roots  *x509.CertPool

	mu        sync.Mutex
	client    *http.Client
	expiresAt time.Time
}

type identityResponse struct {
	CACertificatePEM   string `json:"ca_certificate_pem"`
	CertificateExpires string `json:"certificate_expires_at"`
	CertificatePEM     string `json:"certificate_pem"`
	CertificateSHA256  string `json:"certificate_sha256"`
	WorkerID           string `json:"worker_id"`
	WorkspaceRef       string `json:"workspace_ref"`
}

func newIdentityManager(config identityConfig, roots *x509.CertPool) (*identityManager, error) {
	if config.baseURL == nil || roots == nil || !filepath.IsAbs(config.identityFile) ||
		!reference(config.workerID, 256) || !reference(config.workspaceRef, 256) ||
		config.renewBefore < time.Minute || config.renewBefore > 24*time.Hour ||
		config.timeout <= 0 || len(config.caDER) == 0 {
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
		"worker_id":      m.config.workerID,
		"workspace_ref":  m.config.workspaceRef,
	}
	bootstrap := m.httpClient(nil)
	response, err := m.postIdentity(ctx, bootstrap, "/v1/coop-workers/enroll", document, http.StatusCreated)
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
	info, err := os.Stat(m.config.identityFile)
	if err != nil {
		return nil, time.Time{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, time.Time{}, errors.New("worker identity file must be a private regular file")
	}
	certificate, err := tls.LoadX509KeyPair(m.config.identityFile, m.config.identityFile)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("load worker identity: %w", err)
	}
	leaf, err := validateIdentityCertificate(certificate, m.roots, m.config.workerID, now)
	if err != nil {
		return nil, time.Time{}, err
	}
	return m.httpClient(&certificate), leaf.NotAfter, nil
}

func (m *identityManager) prepareIdentity(response identityResponse, privateKey *rsa.PrivateKey, privateKeyPEM []byte, now time.Time) (*http.Client, time.Time, []byte, error) {
	if response.WorkerID != m.config.workerID || response.WorkspaceRef != m.config.workspaceRef ||
		!digest(response.CertificateSHA256) {
		return nil, time.Time{}, nil, errors.New("worker enrollment response identity does not match")
	}
	caBlock, rest := pem.Decode([]byte(response.CACertificatePEM))
	if caBlock == nil || caBlock.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 || !bytes.Equal(caBlock.Bytes, m.config.caDER) {
		return nil, time.Time{}, nil, errors.New("worker enrollment response CA does not match configured trust")
	}
	certBlock, rest := pem.Decode([]byte(response.CertificatePEM))
	if certBlock == nil || certBlock.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 || sha256sum(certBlock.Bytes) != response.CertificateSHA256 {
		return nil, time.Time{}, nil, errors.New("worker enrollment certificate is invalid")
	}
	bundle := append(append([]byte{}, []byte(response.CertificatePEM)...), privateKeyPEM...)
	certificate, err := tls.X509KeyPair(bundle, bundle)
	if err != nil {
		return nil, time.Time{}, nil, fmt.Errorf("bind worker certificate to generated key: %w", err)
	}
	leaf, err := validateIdentityCertificate(certificate, m.roots, m.config.workerID, now)
	if err != nil {
		return nil, time.Time{}, nil, err
	}
	if parsed, err := time.Parse(time.RFC3339Nano, response.CertificateExpires); err != nil || !parsed.Equal(leaf.NotAfter) {
		return nil, time.Time{}, nil, errors.New("worker enrollment expiry does not match certificate")
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
		return identityResponse{}, fmt.Errorf("worker identity endpoint returned HTTP %d", response.StatusCode)
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
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: m.roots}
	if certificate != nil {
		tlsConfig.Certificates = []tls.Certificate{*certificate}
	}
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Timeout: m.config.timeout, Transport: transport}
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

func readEnrollmentToken(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat worker enrollment token: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() <= 0 || info.Size() > maxEnrollmentTokenBytes {
		return "", errors.New("worker enrollment token must be a private bounded regular file")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read worker enrollment token: %w", err)
	}
	token := strings.TrimSpace(string(value))
	if len(token) < 32 || len(token) > maxEnrollmentTokenBytes || strings.ContainsAny(token, "\r\n\t ") {
		return "", errors.New("worker enrollment token is invalid")
	}
	return token, nil
}

func atomicWriteIdentity(path string, contents []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create worker identity directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".worker-identity-*")
	if err != nil {
		return fmt.Errorf("create worker identity file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install worker identity: %w", err)
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer directoryHandle.Close()
	return directoryHandle.Sync()
}
