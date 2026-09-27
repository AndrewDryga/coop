package workerconnector

import (
	"bytes"
	"context"
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
		DisableCompression:    true,
		ResponseHeaderTimeout: timeout,
	}
	return &UnixAPI{client: &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// ErrRequestRejected marks a private request the connector refused before sending anything: the
// daemon never saw it, so the command's result is a definite failure, never an uncertain one a
// controller would have to reconcile.
var ErrRequestRejected = errors.New("private Coop API request was rejected before it was sent")

func (a *UnixAPI) Forward(ctx context.Context, request Request, body io.Reader) (*http.Response, error) {
	shape := workerproto.APIRequest{Method: request.Method, Path: request.Path, Headers: request.Headers}
	if err := shape.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRequestRejected, err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, request.Method, "http://unix"+request.Path, body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRequestRejected, err)
	}
	if request.BodyBytes > 0 {
		httpRequest.ContentLength = request.BodyBytes
	}
	for name, value := range request.Headers {
		httpRequest.Header.Set(name, value)
	}
	if request.Method != "GET" && request.Method != "HEAD" {
		if request.IdempotencyKey == "" {
			return nil, fmt.Errorf("%w: mutation needs an idempotency key", ErrRequestRejected)
		}
		httpRequest.Header.Set("Idempotency-Key", request.IdempotencyKey)
		if httpRequest.Header.Get("Content-Type") == "" {
			httpRequest.Header.Set("Content-Type", "application/json")
		}
		if httpRequest.Header.Get("Prefer") == "" {
			httpRequest.Header.Set("Prefer", "respond-async")
		}
	}
	client := *a.client
	client.Timeout = 0
	return client.Do(httpRequest)
}

func (a *UnixAPI) Do(ctx context.Context, request Request) (json.RawMessage, error) {
	if len(request.Body) > maxPrivateRequestBytes {
		return nil, fmt.Errorf("%w: request body is too large", ErrRequestRejected)
	}
	if (request.Method == "GET" || request.Method == "HEAD") && len(request.Body) > 0 {
		return nil, fmt.Errorf("%w: read cannot carry a body", ErrRequestRejected)
	}
	request.BodyBytes = int64(len(request.Body))
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.client.Timeout)
		defer cancel()
	}
	response, err := a.Forward(ctx, request, bytes.NewReader(request.Body))
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxPrivateResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxPrivateResponseBytes {
		return nil, errors.New("private Coop API response is oversized")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, decodeAPIError(response.StatusCode, body)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || !json.Valid(body) {
		return nil, errors.New("private Coop API response is not JSON")
	}
	return json.RawMessage(body), nil
}

// decodeAPIError reads the daemon's typed error. The session API wraps it as
// {"error":{"code","detail","operation_id"}}; a flat {"code","detail"} is accepted too. Without
// the code the worker could only report "HTTP 409", and a controller could not tell a policy
// digest mismatch from any other conflict.
func decodeAPIError(status int, body []byte) error {
	var document struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
		Error  *struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		} `json:"error"`
	}
	err := json.Unmarshal(body, &document)
	switch {
	case err == nil && document.Error != nil && document.Error.Code != "":
		return &APIError{Status: status, Code: bounded(document.Error.Code), Detail: bounded(document.Error.Detail)}
	case err != nil || document.Code == "":
		return &APIError{Status: status, Code: "http_error", Detail: fmt.Sprintf("HTTP %d", status)}
	}
	return &APIError{Status: status, Code: bounded(document.Code), Detail: bounded(document.Detail)}
}
