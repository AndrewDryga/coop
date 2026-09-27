package workerconnector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

type HTTPTransportConfig struct {
	BaseURL             string
	CAFile              string
	EnrollmentTokenFile string
	IdentityFile        string
	RenewBefore         time.Duration
	Timeout             time.Duration
}

type HTTPTransport struct {
	stateRoot string
	baseURL   *url.URL
	client    *http.Client
	endpoint  string
	identity  *identityManager
}

func NewHTTPTransport(config HTTPTransportConfig) (*HTTPTransport, error) {
	parsed, err := parseControlPlaneURL(config.BaseURL, true)
	if err != nil {
		return nil, err
	}
	if config.Timeout == 0 {
		config.Timeout = time.Minute
	}
	if config.RenewBefore == 0 {
		config.RenewBefore = time.Hour
	}
	if config.Timeout < 0 || config.Timeout > 5*time.Minute || !filepath.IsAbs(config.IdentityFile) {
		return nil, errors.New("worker identity path or request timeout is invalid")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system TLS roots: %w", err)
	}
	if config.CAFile != "" {
		if !filepath.IsAbs(config.CAFile) {
			return nil, errors.New("worker CA path must be absolute")
		}
		caPEM, err := os.ReadFile(config.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read worker CA: %w", err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("worker CA file contains no certificates")
		}
	}
	identity, err := newIdentityManager(identityConfig{
		baseURL: parsed, identityFile: config.IdentityFile, enrollmentTokenFile: config.EnrollmentTokenFile,
		renewBefore: config.RenewBefore, timeout: config.Timeout,
	}, roots)
	if err != nil {
		return nil, err
	}
	return &HTTPTransport{
		stateRoot: filepath.Dir(config.IdentityFile),
		baseURL:   parsed, identity: identity,
		endpoint: parsed.ResolveReference(&url.URL{Path: "/v1/coop-workers/poll"}).String(),
	}, nil
}

type WorkerIdentity struct {
	ID           string
	WorkspaceRef string
}

func (t *HTTPTransport) Identity(ctx context.Context) (WorkerIdentity, error) {
	if t.identity == nil {
		return WorkerIdentity{}, errors.New("worker identity is not configured")
	}
	if _, err := t.identity.clientFor(ctx); err != nil {
		return WorkerIdentity{}, err
	}
	t.identity.mu.Lock()
	defer t.identity.mu.Unlock()
	return WorkerIdentity{ID: t.identity.workerID, WorkspaceRef: t.identity.workspaceRef}, nil
}

func (t *HTTPTransport) Poll(ctx context.Context, poll workerproto.Poll) (workerproto.Response, error) {
	if err := poll.Validate(); err != nil {
		return workerproto.Response{}, err
	}
	document, err := encodeWireJSON(poll)
	if err != nil {
		return workerproto.Response{}, fmt.Errorf("encode worker poll: %w", err)
	}
	if len(document) > workerproto.MaxDocumentBytes {
		return workerproto.Response{}, errors.New("worker poll exceeds transport bound")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(document))
	if err != nil {
		return workerproto.Response{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	client, err := t.clientFor(ctx)
	if err != nil {
		return workerproto.Response{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return workerproto.Response{}, fmt.Errorf("poll controller worker control plane: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, workerproto.MaxDocumentBytes+1))
	if err != nil {
		return workerproto.Response{}, fmt.Errorf("read controller worker response: %w", err)
	}
	if len(body) > workerproto.MaxDocumentBytes {
		return workerproto.Response{}, errors.New("controller worker response exceeds transport bound")
	}
	mediaType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	if response.StatusCode != http.StatusOK {
		return workerproto.Response{}, fmt.Errorf("controller worker control plane returned HTTP %d", response.StatusCode)
	}
	if !strings.EqualFold(mediaType, "application/json") {
		return workerproto.Response{}, errors.New("controller worker response is not JSON")
	}
	return workerproto.DecodeResponse(body)
}

// FetchJobSourceGrant obtains an uncached, job-bound Contents:read grant. The response
// never enters the durable command, job, local Git config, or model sandbox.
func (t *HTTPTransport) FetchJobSourceGrant(
	ctx context.Context, jobRef string, source workerproto.RepositoryIdentity,
) (JobSourceGrant, error) {
	if !reference(jobRef, 256) {
		return JobSourceGrant{}, errors.New("job source grant identity is invalid")
	}
	if err := source.Validate(); err != nil {
		return JobSourceGrant{}, err
	}
	document, err := json.Marshal(source)
	if err != nil {
		return JobSourceGrant{}, err
	}
	endpoint := t.baseURL.ResolveReference(&url.URL{
		Path:    "/v1/coop-workers/jobs/" + jobRef + "/source-grants",
		RawPath: "/v1/coop-workers/jobs/" + url.PathEscape(jobRef) + "/source-grants",
	}).String()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(document))
	if err != nil {
		return JobSourceGrant{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	client, err := t.clientFor(ctx)
	if err != nil {
		return JobSourceGrant{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return JobSourceGrant{}, fmt.Errorf("fetch controller job source grant: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return JobSourceGrant{}, &BodyStatusError{Status: response.StatusCode}
	}
	mediaType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	if mediaType != "application/json" {
		return JobSourceGrant{}, ErrJobSourceIntegrity
	}
	document, err = io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(document) > 8192 {
		return JobSourceGrant{}, ErrJobSourceIntegrity
	}
	var grant JobSourceGrant
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&grant) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		grant.RepositoryRef != source.RepositoryRef || grant.GitHubRepository != source.GitHubRepository ||
		grant.GitHubRepositoryID != source.GitHubRepositoryID || len(grant.Token) < 1 || len(grant.Token) > 4096 ||
		!grant.ExpiresAt.After(time.Now().Add(30*time.Second)) {
		return JobSourceGrant{}, ErrJobSourceIntegrity
	}
	return grant, nil
}

func (t *HTTPTransport) clientFor(ctx context.Context) (*http.Client, error) {
	if t.identity != nil {
		return t.identity.clientFor(ctx)
	}
	if t.client == nil {
		return nil, errors.New("worker HTTP client is unavailable")
	}
	return t.client, nil
}

func sha256sum(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func parseControlPlaneURL(value string, requireTLS bool) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("worker control plane URL must be an origin")
	}
	if requireTLS && parsed.Scheme != "https" {
		return nil, errors.New("worker control plane URL must use HTTPS")
	}
	if !requireTLS && parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("worker control plane URL scheme is invalid")
	}
	return parsed, nil
}
