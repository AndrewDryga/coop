package workerconnector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
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
	WorkerID            string
	WorkspaceRef        string
}

type HTTPTransport struct {
	baseURL  *url.URL
	client   *http.Client
	endpoint string
	identity *identityManager
}

func NewHTTPTransport(config HTTPTransportConfig) (*HTTPTransport, error) {
	parsed, err := parseControlPlaneURL(config.BaseURL, true)
	if err != nil {
		return nil, err
	}
	if config.Timeout <= 0 || config.Timeout > 5*time.Minute {
		return nil, errors.New("worker control plane timeout must be between zero and five minutes")
	}
	for name, path := range map[string]string{"CA": config.CAFile, "identity": config.IdentityFile} {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("worker %s file must be absolute", name)
		}
	}
	if config.EnrollmentTokenFile != "" && !filepath.IsAbs(config.EnrollmentTokenFile) {
		return nil, errors.New("worker enrollment token file must be absolute")
	}
	caPEM, err := os.ReadFile(config.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read worker CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("worker CA file contains no certificates")
	}
	block, rest := pem.Decode(caPEM)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("worker CA file must contain exactly one certificate")
	}
	identity, err := newIdentityManager(identityConfig{
		baseURL: parsed, caDER: block.Bytes, identityFile: config.IdentityFile,
		enrollmentTokenFile: config.EnrollmentTokenFile, workerID: config.WorkerID,
		workspaceRef: config.WorkspaceRef, renewBefore: config.RenewBefore, timeout: config.Timeout,
	}, roots)
	if err != nil {
		return nil, err
	}
	return &HTTPTransport{
		baseURL: parsed, identity: identity,
		endpoint: parsed.ResolveReference(&url.URL{Path: "/v1/coop-workers/poll"}).String(),
	}, nil
}

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
		return workerproto.Response{}, fmt.Errorf("poll responder worker control plane: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, workerproto.MaxDocumentBytes+1))
	if err != nil {
		return workerproto.Response{}, fmt.Errorf("read responder worker response: %w", err)
	}
	if len(body) > workerproto.MaxDocumentBytes {
		return workerproto.Response{}, errors.New("responder worker response exceeds transport bound")
	}
	mediaType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	if response.StatusCode != http.StatusOK {
		return workerproto.Response{}, fmt.Errorf("responder worker control plane returned HTTP %d", response.StatusCode)
	}
	if !strings.EqualFold(mediaType, "application/json") {
		return workerproto.Response{}, errors.New("responder worker response is not JSON")
	}
	return workerproto.DecodeResponse(body)
}

func (t *HTTPTransport) FetchInputArtifact(ctx context.Context, commandID, artifactRef string) (Artifact, error) {
	if !reference(commandID, 256) || !reference(artifactRef, 256) {
		return Artifact{}, errors.New("input artifact request identity is invalid")
	}
	endpoint := t.baseURL.ResolveReference(&url.URL{Path: "/v1/coop-workers/commands/" + url.PathEscape(commandID) + "/input-artifacts/" + url.PathEscape(artifactRef)}).String()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Artifact{}, err
	}
	request.Header.Set("Accept", "application/octet-stream")
	client, err := t.clientFor(ctx)
	if err != nil {
		return Artifact{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return Artifact{}, fmt.Errorf("fetch responder input artifact: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Artifact{}, &ArtifactStatusError{Status: response.StatusCode}
	}
	encodedName := response.Header.Get("X-Responder-Artifact-Name")
	name, err := base64.RawURLEncoding.DecodeString(encodedName)
	if err != nil {
		return Artifact{}, errors.New("responder input artifact name is invalid")
	}
	mediaType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	body, err := io.ReadAll(io.LimitReader(response.Body, maxArtifactBytes+1))
	if err != nil {
		return Artifact{}, fmt.Errorf("read responder input artifact: %w", err)
	}
	artifact := Artifact{
		ID: artifactRef, Name: string(name), MediaType: mediaType,
		SHA256: response.Header.Get("X-Responder-Artifact-SHA256"), Data: body,
	}
	if err := validateArtifact(artifact, true); err != nil {
		return Artifact{}, fmt.Errorf("validate responder input artifact: %w", err)
	}
	return artifact, nil
}

func (t *HTTPTransport) FetchWorkspaceCheckpoint(
	ctx context.Context,
	commandID, transferID string,
) (workerproto.WorkspaceCheckpoint, []byte, error) {
	if !reference(commandID, 256) || !reference(transferID, 256) {
		return workerproto.WorkspaceCheckpoint{}, nil, errors.New("workspace checkpoint fetch identity is invalid")
	}
	endpoint := t.baseURL.ResolveReference(&url.URL{Path: "/v1/coop-workers/commands/" +
		url.PathEscape(commandID) + "/workspace-checkpoints/" + url.PathEscape(transferID)}).String()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, nil, err
	}
	request.Header.Set("Accept", workerproto.WorkspaceCheckpointBundleMediaType)
	client, err := t.clientFor(ctx)
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, nil, fmt.Errorf("fetch responder workspace checkpoint: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return workerproto.WorkspaceCheckpoint{}, nil, &ArtifactStatusError{Status: response.StatusCode}
	}
	mediaType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	if mediaType != workerproto.WorkspaceCheckpointBundleMediaType {
		return workerproto.WorkspaceCheckpoint{}, nil, errors.New("responder workspace checkpoint media type is invalid")
	}
	descriptor, err := base64.RawURLEncoding.DecodeString(response.Header.Get("X-Responder-Checkpoint-Descriptor"))
	if err != nil || len(descriptor) == 0 || len(descriptor) > 32<<10 {
		return workerproto.WorkspaceCheckpoint{}, nil, errors.New("responder workspace checkpoint descriptor is invalid")
	}
	checkpoint, err := workerproto.DecodeWorkspaceCheckpoint(descriptor)
	if err != nil || response.Header.Get("X-Responder-Checkpoint-SHA256") != checkpoint.Bundle.SHA256 {
		return workerproto.WorkspaceCheckpoint{}, nil, errors.New("responder workspace checkpoint identity is invalid")
	}
	bundle, err := io.ReadAll(io.LimitReader(response.Body, workerproto.MaxWorkspaceCheckpointBundleBytes+1))
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, nil, fmt.Errorf("read responder workspace checkpoint: %w", err)
	}
	if _, err := workerproto.ValidateWorkspaceCheckpointBundle(checkpoint, bundle); err != nil {
		return workerproto.WorkspaceCheckpoint{}, nil, fmt.Errorf("validate responder workspace checkpoint: %w", err)
	}
	return checkpoint, bundle, nil
}

func (t *HTTPTransport) UploadOutputArtifact(ctx context.Context, commandID string, artifact Artifact) ([]byte, error) {
	if !reference(commandID, 256) || validateArtifact(artifact, true) != nil || !outputArtifactMediaType(artifact.MediaType) {
		return nil, errors.New("output artifact upload identity is invalid")
	}
	endpoint := t.baseURL.ResolveReference(&url.URL{Path: "/v1/coop-workers/commands/" + url.PathEscape(commandID) + "/output-artifacts/" + url.PathEscape(artifact.ID)}).String()
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(artifact.Data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", artifact.MediaType)
	request.Header.Set("X-Responder-Artifact-Name", base64.RawURLEncoding.EncodeToString([]byte(artifact.Name)))
	request.Header.Set("X-Responder-Artifact-SHA256", artifact.SHA256)
	client, err := t.clientFor(ctx)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("upload responder output artifact: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, workerproto.MaxDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read responder output artifact receipt: %w", err)
	}
	if len(body) > workerproto.MaxDocumentBytes {
		return nil, errors.New("responder output artifact receipt exceeds transport bound")
	}
	if response.StatusCode != http.StatusOK || !jsonObject(body) {
		return nil, fmt.Errorf("responder output artifact returned HTTP %d", response.StatusCode)
	}
	return body, nil
}

func (t *HTTPTransport) UploadReviewPatch(ctx context.Context, commandID, artifactID, expectedSHA256 string, patch []byte) ([]byte, error) {
	if !reference(commandID, 256) || !reference(artifactID, 256) || !digest(expectedSHA256) || len(patch) == 0 || len(patch) > maxReviewPatchBytes {
		return nil, errors.New("review patch upload identity is invalid")
	}
	if sha256sum(patch) != expectedSHA256 {
		return nil, errors.New("review patch digest does not match")
	}
	endpoint := t.baseURL.ResolveReference(&url.URL{Path: "/v1/coop-workers/commands/" + url.PathEscape(commandID) + "/review-patches/" + url.PathEscape(artifactID)}).String()
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(patch))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "text/x-diff")
	request.Header.Set("X-Responder-Artifact-SHA256", expectedSHA256)
	client, err := t.clientFor(ctx)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("upload responder review patch: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, workerproto.MaxDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read responder review patch receipt: %w", err)
	}
	if len(body) > workerproto.MaxDocumentBytes {
		return nil, errors.New("responder review patch receipt exceeds transport bound")
	}
	if response.StatusCode != http.StatusOK || !jsonObject(body) {
		return nil, fmt.Errorf("responder review patch returned HTTP %d", response.StatusCode)
	}
	return body, nil
}

func (t *HTTPTransport) UploadWorkspaceCheckpoint(
	ctx context.Context,
	commandID string,
	checkpoint workerproto.WorkspaceCheckpoint,
	bundle []byte,
) ([]byte, error) {
	if !reference(commandID, 256) {
		return nil, errors.New("workspace checkpoint upload command identity is invalid")
	}
	if _, err := workerproto.ValidateWorkspaceCheckpointBundle(checkpoint, bundle); err != nil {
		return nil, fmt.Errorf("validate workspace checkpoint upload: %w", err)
	}
	descriptor, err := json.Marshal(checkpoint)
	if err != nil || len(descriptor) > 32<<10 {
		return nil, errors.New("workspace checkpoint descriptor exceeds transport bound")
	}
	endpoint := t.baseURL.ResolveReference(&url.URL{Path: "/v1/coop-workers/commands/" + url.PathEscape(commandID) + "/workspace-checkpoints/" + url.PathEscape(checkpoint.CheckpointRef)}).String()
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(bundle))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", workerproto.WorkspaceCheckpointBundleMediaType)
	request.Header.Set("X-Responder-Checkpoint-Descriptor", base64.RawURLEncoding.EncodeToString(descriptor))
	request.Header.Set("X-Responder-Checkpoint-SHA256", checkpoint.Bundle.SHA256)
	client, err := t.clientFor(ctx)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("upload responder workspace checkpoint: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, workerproto.MaxDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read responder workspace checkpoint receipt: %w", err)
	}
	if len(body) > workerproto.MaxDocumentBytes {
		return nil, errors.New("responder workspace checkpoint receipt exceeds transport bound")
	}
	if response.StatusCode != http.StatusOK || !jsonObject(body) {
		return nil, fmt.Errorf("responder workspace checkpoint returned HTTP %d", response.StatusCode)
	}
	return body, nil
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
