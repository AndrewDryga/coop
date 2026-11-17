package workerconnector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

type BodyTransport interface {
	FetchRequestBody(context.Context, string, workerproto.BodyReference, io.Writer) error
	UploadResponseBody(context.Context, string, workerproto.BodyReference, io.Reader) error
}

type BodyStatusError struct{ Status int }

func (e *BodyStatusError) Error() string {
	return fmt.Sprintf("controller body request returned HTTP %d", e.Status)
}

func (e *Executor) forwardRequest(ctx context.Context, entry journalEntry, command workerproto.Command, request workerproto.APIRequest, expiresAt func() time.Time) (json.RawMessage, error) {
	if entry.Response != nil {
		return e.uploadSavedResponse(ctx, command.CommandID, *entry.Response)
	}
	api, ok := e.api.(streamingAPI)
	if !ok {
		return nil, fmt.Errorf("%w: private API does not support request forwarding", ErrRequestRejected)
	}
	var body io.Reader = bytes.NewReader(request.Body)
	bodyBytes := int64(len(request.Body))
	if request.BodyRef != nil {
		if e.bodyTransport == nil {
			return nil, fmt.Errorf("%w: request body transport is unavailable", ErrRequestRejected)
		}
		file, cleanup, err := e.temporaryBody()
		if err != nil {
			return nil, err
		}
		defer cleanup()
		if err := e.bodyTransport.FetchRequestBody(ctx, command.CommandID, *request.BodyRef, file); err != nil {
			return nil, classifyArtifactFetch(err, "fetch request body")
		}
		if err := verifyBodyFile(file, *request.BodyRef); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrRequestRejected, err)
		}
		body = file
		bodyBytes = request.BodyRef.ByteSize
	}
	if isCreateRequest(command) {
		document, err := io.ReadAll(io.LimitReader(body, 256<<10+1))
		if err != nil || len(document) > 256<<10 {
			return nil, fmt.Errorf("%w: create body exceeds its limit", ErrRequestRejected)
		}
		if err := e.stageCreateJobSources(ctx, document); err != nil {
			return nil, err
		}
		body = bytes.NewReader(document)
	}
	key := ""
	if request.Method != "GET" && request.Method != "HEAD" {
		key = command.IdempotencyKey
	}
	if !e.now().Before(expiresAt()) {
		return nil, ErrLeaseExpired
	}
	if err := e.checkPlacement(command, request); err != nil {
		return nil, err
	}
	response, err := api.Forward(ctx, Request{Method: request.Method, Path: request.Path, Headers: request.Headers, IdempotencyKey: key, BodyBytes: bodyBytes}, body)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var responseBody io.Reader = response.Body
	if project := tunnelProjection(request); project != nil && response.StatusCode == http.StatusOK {
		raw, err := io.ReadAll(io.LimitReader(response.Body, maxPrivateResponseBytes+1))
		if err != nil {
			return nil, err
		}
		if len(raw) > maxPrivateResponseBytes {
			return nil, errors.New("private Coop response is too large to filter")
		}
		projected, err := project(raw)
		if err != nil {
			return nil, err
		}
		responseBody = bytes.NewReader(projected)
	}
	envelope := workerproto.APIResponse{Status: response.StatusCode, Headers: map[string]string{}}
	for _, name := range []string{"Content-Type", "Content-Disposition", "ETag", "X-Coop-Workspace-Checkpoint"} {
		if value := response.Header.Get(name); value != "" {
			envelope.Headers[http.CanonicalHeaderKey(name)] = value
		}
	}
	const inlineLimit = 256 << 10
	prefix, err := io.ReadAll(io.LimitReader(responseBody, inlineLimit+1))
	if err != nil {
		return nil, err
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if len(prefix) == 0 {
		return encodeWireJSON(envelope)
	}
	if len(prefix) <= inlineLimit && mediaType == "application/json" && json.Valid(prefix) {
		envelope.Body = prefix
		return encodeWireJSON(envelope)
	}
	if e.bodyTransport == nil {
		return nil, errors.New("response body transport is unavailable")
	}
	file, cleanup, err := e.temporaryBody()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	sum := sha256.New()
	count, err := io.Copy(io.MultiWriter(file, sum), io.MultiReader(bytes.NewReader(prefix), responseBody))
	if err != nil {
		return nil, err
	}
	ref := workerproto.BodyReference{SHA256: hex.EncodeToString(sum.Sum(nil)), ByteSize: count}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	if err := os.Rename(file.Name(), e.journal.path(command.CommandID)+".body"); err != nil {
		return nil, err
	}
	envelope.BodyRef = &ref
	entry.Response = &envelope
	if _, err := e.journal.save(entry); err != nil {
		return nil, err
	}
	return e.uploadSavedResponse(ctx, command.CommandID, envelope)
}

// tunnelProjection returns the filter a forwarded read must pass before it leaves the worker, or nil.
// The tunnel is otherwise generic, but two reads would undo what the connector keeps local when it
// narrates: a session's raw events carry the host checkout root and unscanned tool fields, and the
// storage report carries the per-fork inventory, which names local paths. Both cross in the shape
// the connector already narrates.
func tunnelProjection(request workerproto.APIRequest) func([]byte) ([]byte, error) {
	if request.Method != http.MethodGet {
		return nil
	}
	parsed, err := url.ParseRequestURI(request.Path)
	if err != nil {
		return nil
	}
	if parsed.Path == "/v1/storage" {
		return publicStorageReport
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/v1/sessions/"), "/")
	if strings.HasPrefix(parsed.Path, "/v1/sessions/") && len(parts) == 2 && parts[0] != "" && parts[1] == "events" {
		return publicEventPage
	}
	return nil
}

// publicEventPage gives each event the payload the connector would narrate: an activity event's
// filtered payload, and none for a lifecycle event.
func publicEventPage(raw []byte) ([]byte, error) {
	var events []workerproto.SessionEvent
	if err := json.Unmarshal(raw, &events); err != nil || events == nil {
		return nil, errors.New("private Coop session events are malformed")
	}
	for i := range events {
		payload, ok := json.RawMessage(nil), false
		if operatorActivityEvent(events[i].Type) {
			payload, ok = publicActivityPayload(events[i].Type, events[i].Payload)
		}
		if !ok {
			payload = nil
		}
		events[i].Payload = payload
	}
	return json.Marshal(events)
}

// publicStorageReport keeps only the storage object the hello already carries.
func publicStorageReport(raw []byte) ([]byte, error) {
	var report storageReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, errors.New("private Coop storage report is malformed")
	}
	if len(report.Storage) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(report)
}

func (e *Executor) uploadSavedResponse(ctx context.Context, commandID string, response workerproto.APIResponse) (json.RawMessage, error) {
	if e.bodyTransport == nil || response.BodyRef == nil || response.BodyRef.Validate() != nil {
		return nil, fmt.Errorf("%w: saved response transport or identity is unavailable", errArtifactTransfer)
	}
	file, err := os.Open(e.journal.path(commandID) + ".body")
	if err != nil {
		return nil, fmt.Errorf("%w: open saved response: %v", errArtifactTransfer, err)
	}
	defer file.Close()
	if err := verifyBodyFile(file, *response.BodyRef); err != nil {
		return nil, fmt.Errorf("%w: %v", errArtifactTransfer, err)
	}
	if err := e.bodyTransport.UploadResponseBody(ctx, commandID, *response.BodyRef, file); err != nil {
		return nil, fmt.Errorf("%w: upload saved response: %v", errArtifactTransfer, err)
	}
	return encodeWireJSON(response)
}

func (e *Executor) temporaryBody() (*os.File, func(), error) {
	directory := filepath.Join(filepath.Dir(e.journal.dir), "body-tmp")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, nil, err
	}
	if err := requirePrivateDirectory(directory); err != nil {
		return nil, nil, err
	}
	file, err := os.CreateTemp(directory, "transfer-")
	if err != nil {
		return nil, nil, err
	}
	return file, func() { _ = file.Close(); _ = os.Remove(file.Name()) }, nil
}

func verifyBodyFile(file *os.File, expected workerproto.BodyReference) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	sum := sha256.New()
	count, err := io.Copy(sum, file)
	if err != nil {
		return err
	}
	if count != expected.ByteSize || hex.EncodeToString(sum.Sum(nil)) != expected.SHA256 {
		return errors.New("transferred body identity does not match")
	}
	_, err = file.Seek(0, io.SeekStart)
	return err
}

func (t *HTTPTransport) FetchRequestBody(ctx context.Context, id string, ref workerproto.BodyReference, destination io.Writer) error {
	if !reference(id, 256) || ref.Validate() != nil {
		return errors.New("invalid request body reference")
	}
	endpoint := t.baseURL.ResolveReference(&url.URL{Path: "/v1/coop-workers/commands/" + url.PathEscape(id) + "/request-body"})
	request, err := http.NewRequestWithContext(ctx, "GET", endpoint.String(), nil)
	if err != nil {
		return err
	}
	client, err := t.clientFor(ctx)
	if err != nil {
		return err
	}
	streamingClient := *client
	streamingClient.Timeout = 0 // Bound connection setup, not the cumulative size of valid work.
	response, err := streamingClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &BodyStatusError{Status: response.StatusCode}
	}
	written, err := io.Copy(destination, io.LimitReader(response.Body, ref.ByteSize+1))
	if err != nil {
		return err
	}
	if written != ref.ByteSize {
		return errors.New("request body length does not match")
	}
	return nil
}

func (t *HTTPTransport) UploadResponseBody(ctx context.Context, id string, ref workerproto.BodyReference, body io.Reader) error {
	if !reference(id, 256) || ref.Validate() != nil {
		return errors.New("invalid response body reference")
	}
	endpoint := t.baseURL.ResolveReference(&url.URL{Path: "/v1/coop-workers/commands/" + url.PathEscape(id) + "/response-body"})
	request, err := http.NewRequestWithContext(ctx, "PUT", endpoint.String(), body)
	if err != nil {
		return err
	}
	request.ContentLength = ref.ByteSize
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Coop-Body-SHA256", ref.SHA256)
	client, err := t.clientFor(ctx)
	if err != nil {
		return err
	}
	streamingClient := *client
	streamingClient.Timeout = 0
	response, err := streamingClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return &BodyStatusError{Status: response.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(raw) > 4096 {
		return errors.New("invalid response body receipt")
	}
	var receipt workerproto.BodyReference
	if decodePayload(raw, &receipt) != nil || receipt != ref {
		return errors.New("response body receipt does not match")
	}
	return nil
}
