package workerconnector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

const (
	// The daemon accepts a 12 MiB turn body and a fence body of that plus its ordinary 128 KiB
	// request cap (sessionsvc's sessionHTTPFenceMaxBody); the connector admits the same, so an
	// 8 MiB input artifact set — which base64 and the frozen prompt grow past 1 MiB — can still be
	// submitted instead of being refused here before it was ever sent.
	maxPrivateRequestBytes  = 12<<20 + 128<<10
	maxPrivateResponseBytes = 3 << 20
)

type UnixAPI struct {
	client *http.Client
}

func (a *UnixAPI) ListEvents(ctx context.Context, sessionID string, after int64, limit int) ([]workerproto.SessionEvent, error) {
	if !reference(sessionID, 1024) || after < 0 || limit <= 0 || limit > maximumEventPage {
		return nil, errors.New("private Coop session event cursor is invalid")
	}
	query := url.Values{
		"after": {strconv.FormatInt(after, 10)},
		"limit": {strconv.Itoa(limit)},
	}
	path := "/v1/sessions/" + url.PathEscape(sessionID) + "/events?" + query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := a.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxPrivateResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read private Coop session events: %w", err)
	}
	if len(body) > maxPrivateResponseBytes {
		return nil, errors.New("private Coop session events are oversized")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, decodeAPIError(response.StatusCode, body)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		return nil, errors.New("private Coop session events are not JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var events []workerproto.SessionEvent
	if err := decoder.Decode(&events); err != nil || events == nil || len(events) > limit {
		return nil, errors.New("private Coop session event page is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("private Coop session event page has trailing data")
	}
	for index, event := range events {
		if event.SessionID != sessionID || event.Sequence != after+int64(index)+1 || event.Validate() != nil {
			return nil, errors.New("private Coop session event identity is invalid")
		}
	}
	return events, nil
}

func (a *UnixAPI) FetchOutputArtifact(ctx context.Context, sessionID, turnID, artifactID string) (Artifact, error) {
	if !reference(sessionID, 1024) || !reference(turnID, 1024) || !reference(artifactID, 256) {
		return Artifact{}, errors.New("private Coop output artifact identity is invalid")
	}
	path := "/v1/sessions/" + url.PathEscape(sessionID) + "/turns/" + url.PathEscape(turnID) + "/artifacts/" + url.PathEscape(artifactID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+path, nil)
	if err != nil {
		return Artifact{}, err
	}
	response, err := a.client.Do(request)
	if err != nil {
		return Artifact{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, maxPrivateResponseBytes+1))
		return Artifact{}, decodeAPIError(response.StatusCode, body)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		return Artifact{}, errors.New("private Coop output artifact media type is invalid")
	}
	_, disposition, err := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
	if err != nil {
		return Artifact{}, errors.New("private Coop output artifact disposition is invalid")
	}
	expectedBytes, err := strconv.Atoi(response.Header.Get("Content-Length"))
	if err != nil || expectedBytes <= 0 || expectedBytes > maxArtifactBytes {
		return Artifact{}, errors.New("private Coop output artifact length is invalid")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxArtifactBytes+1))
	if err != nil {
		return Artifact{}, fmt.Errorf("read private Coop output artifact: %w", err)
	}
	if len(body) != expectedBytes {
		return Artifact{}, errors.New("private Coop output artifact length does not match")
	}
	sha := strings.Trim(response.Header.Get("ETag"), `"`)
	artifact := Artifact{
		ID: artifactID, Name: disposition["filename"], MediaType: mediaType, SHA256: sha, Data: body,
	}
	if err := validateArtifact(artifact, true); err != nil || !outputArtifactMediaType(artifact.MediaType) {
		return Artifact{}, errors.New("private Coop output artifact identity is invalid")
	}
	return artifact, nil
}

func (a *UnixAPI) FetchReviewPatch(ctx context.Context, artifactID, expectedSHA256 string, expectedBytes int64) ([]byte, error) {
	if !reference(artifactID, 256) || !digest(expectedSHA256) || expectedBytes <= 0 || expectedBytes > maxReviewPatchBytes {
		return nil, errors.New("private Coop review patch identity is invalid")
	}
	path := "/v1/operations/" + url.PathEscape(artifactID) + "/review-patch"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := a.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, maxPrivateResponseBytes+1))
		return nil, decodeAPIError(response.StatusCode, body)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/x-diff" {
		return nil, errors.New("private Coop review patch media type is invalid")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxReviewPatchBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read private Coop review patch: %w", err)
	}
	if int64(len(body)) != expectedBytes || sha256sum(body) != expectedSHA256 {
		return nil, errors.New("private Coop review patch identity does not match")
	}
	return body, nil
}

func (a *UnixAPI) FetchWorkspaceCheckpointBundle(
	ctx context.Context,
	operationID string,
	checkpoint workerproto.WorkspaceCheckpoint,
) ([]byte, error) {
	if !reference(operationID, 256) || checkpoint.Validate() != nil {
		return nil, errors.New("private Coop workspace checkpoint identity is invalid")
	}
	path := "/v1/operations/" + url.PathEscape(operationID) + "/checkpoint-bundle"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := a.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, maxPrivateResponseBytes+1))
		return nil, decodeAPIError(response.StatusCode, body)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != workerproto.WorkspaceCheckpointBundleMediaType {
		return nil, errors.New("private Coop workspace checkpoint media type is invalid")
	}
	expectedBytes, err := strconv.ParseInt(response.Header.Get("Content-Length"), 10, 64)
	if err != nil || expectedBytes != checkpoint.Bundle.ByteSize || expectedBytes > workerproto.MaxWorkspaceCheckpointBundleBytes {
		return nil, errors.New("private Coop workspace checkpoint length is invalid")
	}
	if strings.Trim(response.Header.Get("ETag"), `"`) != checkpoint.Bundle.SHA256 {
		return nil, errors.New("private Coop workspace checkpoint digest is invalid")
	}
	bundle, err := io.ReadAll(io.LimitReader(response.Body, workerproto.MaxWorkspaceCheckpointBundleBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read private Coop workspace checkpoint: %w", err)
	}
	if _, err := workerproto.ValidateWorkspaceCheckpointBundle(checkpoint, bundle); err != nil {
		return nil, fmt.Errorf("validate private Coop workspace checkpoint: %w", err)
	}
	return bundle, nil
}

func (a *UnixAPI) RestoreWorkspaceCheckpoint(
	ctx context.Context,
	sessionID, key string,
	expectedRevision int,
	checkpoint workerproto.WorkspaceCheckpoint,
	bundle []byte,
) (json.RawMessage, error) {
	if !reference(sessionID, 1024) || !reference(key, 512) || expectedRevision <= 0 {
		return nil, errors.New("private Coop workspace restore identity is invalid")
	}
	if _, err := workerproto.ValidateWorkspaceCheckpointBundle(checkpoint, bundle); err != nil {
		return nil, fmt.Errorf("validate private Coop workspace restore: %w", err)
	}
	descriptor, err := json.Marshal(checkpoint)
	if err != nil || len(descriptor) == 0 || len(descriptor) > maxPrivateRequestBytes {
		return nil, errors.New("private Coop workspace restore descriptor is invalid")
	}
	path := "/v1/sessions/" + url.PathEscape(sessionID) + "/workspace/restore"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix"+path, bytes.NewReader(bundle))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", workerproto.WorkspaceCheckpointBundleMediaType)
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("X-Coop-Expected-Revision", strconv.Itoa(expectedRevision))
	request.Header.Set("X-Coop-Workspace-Checkpoint", base64.StdEncoding.EncodeToString(descriptor))
	response, err := a.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxPrivateResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read private Coop workspace restore response: %w", err)
	}
	if len(body) > maxPrivateResponseBytes {
		return nil, errors.New("private Coop workspace restore response is oversized")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, decodeAPIError(response.StatusCode, body)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" || !jsonObject(body) {
		return nil, errors.New("private Coop workspace restore response is not JSON")
	}
	return json.RawMessage(body), nil
}

func NewUnixAPI(socket string, timeout time.Duration) (*UnixAPI, error) {
	if socket == "" || !filepath.IsAbs(socket) || strings.ContainsRune(socket, 0) {
		return nil, errors.New("private Coop API socket must be an absolute path")
	}
	if timeout <= 0 || timeout > 5*time.Minute {
		return nil, errors.New("private Coop API timeout must be positive and bounded")
	}
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
		DisableCompression: true,
	}
	return &UnixAPI{client: &http.Client{Transport: transport, Timeout: timeout}}, nil
}

// ErrRequestRejected marks a private request the connector refused before sending anything: the
// daemon never saw it, so the command's result is a definite failure, never an uncertain one a
// controller would have to reconcile.
var ErrRequestRejected = errors.New("private Coop API request was rejected before it was sent")

func (a *UnixAPI) Do(ctx context.Context, request Request) (json.RawMessage, error) {
	if request.Method != http.MethodGet && request.Method != http.MethodPost {
		return nil, fmt.Errorf("%w: method is not allowed", ErrRequestRejected)
	}
	if len(request.Body) > maxPrivateRequestBytes {
		return nil, fmt.Errorf("%w: body exceeds %d bytes", ErrRequestRejected, maxPrivateRequestBytes)
	}
	parsed, err := url.ParseRequestURI(request.Path)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/v1/") {
		return nil, fmt.Errorf("%w: path is invalid", ErrRequestRejected)
	}
	if request.Method == http.MethodGet && len(request.Body) != 0 {
		return nil, fmt.Errorf("%w: GET cannot carry a body", ErrRequestRejected)
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		request.Method,
		"http://unix"+request.Path,
		bytes.NewReader(request.Body),
	)
	if err != nil {
		return nil, fmt.Errorf("build private Coop API request: %w", err)
	}
	if request.Method == http.MethodPost {
		if request.IdempotencyKey == "" {
			return nil, fmt.Errorf("%w: mutation needs an idempotency key", ErrRequestRejected)
		}
		httpRequest.Header.Set("Content-Type", "application/json")
		httpRequest.Header.Set("Idempotency-Key", request.IdempotencyKey)
		httpRequest.Header.Set("Prefer", "respond-async")
	}

	response, err := a.client.Do(httpRequest)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxPrivateResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read private Coop API response: %w", err)
	}
	if len(body) > maxPrivateResponseBytes {
		return nil, errors.New("private Coop API response is oversized")
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		return nil, errors.New("private Coop API response is not JSON")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, decodeAPIError(response.StatusCode, body)
	}
	if !jsonObject(body) {
		return nil, errors.New("private Coop API response is not an object")
	}
	return json.RawMessage(body), nil
}

func decodeAPIError(status int, body []byte) error {
	var document struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &document); err != nil || document.Code == "" {
		return &APIError{Status: status, Code: "http_error", Detail: fmt.Sprintf("HTTP %d", status)}
	}
	return &APIError{Status: status, Code: bounded(document.Code), Detail: bounded(document.Detail)}
}
