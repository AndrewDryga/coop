package workerconnector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestUnixAPIKeepsTheSessionControllerPrivateAndBounded(t *testing.T) {
	socket := unixSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/sessions" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Idempotency-Key") != "create-1" {
			t.Errorf("idempotency key = %q", request.Header.Get("Idempotency-Key"))
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"operation":{"id":"operation-1","state":"running"}}`))
	})}
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close() })

	api, err := NewUnixAPI(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := api.Do(context.Background(), Request{
		Method: "POST", Path: "/v1/sessions", IdempotencyKey: "create-1", Body: []byte(`{"policy":"work","task":"episode"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(result, &decoded); err != nil || decoded["operation"] == nil {
		t.Fatalf("result = %s, %v", result, err)
	}
}

func TestUnixAPIFetchesOneVerifiedRawOutputArtifact(t *testing.T) {
	data := []byte{137, 80, 78, 71, 13, 10, 26, 10, 'x'}
	digest := sha256.Sum256(data)
	socket := unixSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/sessions/session-1/turns/turn-1/artifacts/artifact_chart" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		response.Header().Set("Content-Type", "image/png")
		response.Header().Set("Content-Length", "9")
		response.Header().Set("ETag", `"`+hex.EncodeToString(digest[:])+`"`)
		response.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "chart.png"}))
		_, _ = response.Write(data)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close() })
	api, err := NewUnixAPI(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := api.FetchOutputArtifact(context.Background(), "session-1", "turn-1", "artifact_chart")
	if err != nil || string(artifact.Data) != string(data) || artifact.Name != "chart.png" {
		t.Fatalf("artifact = %+v, %v", artifact, err)
	}
}

func TestUnixAPIFetchesOneVerifiedReviewPatch(t *testing.T) {
	patch := []byte("diff --git a/a b/a\n+verified\n")
	digest := sha256.Sum256(patch)
	socket := unixSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/operations/review-artifact-1/review-patch" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		response.Header().Set("Content-Type", "text/x-diff")
		_, _ = response.Write(patch)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close() })
	api, err := NewUnixAPI(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	fetched, err := api.FetchReviewPatch(
		context.Background(), "review-artifact-1", hex.EncodeToString(digest[:]), int64(len(patch)),
	)
	if err != nil || string(fetched) != string(patch) {
		t.Fatalf("patch = %q, %v", fetched, err)
	}
}

func TestUnixAPIFetchesOneVerifiedWorkspaceCheckpointBundle(t *testing.T) {
	checkpoint, bundle := testWorkspaceCheckpoint(t, time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))
	socket := unixSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/operations/operation-checkpoint-1/checkpoint-bundle" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		response.Header().Set("Content-Type", workerproto.WorkspaceCheckpointBundleMediaType)
		response.Header().Set("Content-Length", strconv.Itoa(len(bundle)))
		response.Header().Set("ETag", `"`+checkpoint.Bundle.SHA256+`"`)
		_, _ = response.Write(bundle)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close() })
	api, err := NewUnixAPI(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	fetched, err := api.FetchWorkspaceCheckpointBundle(context.Background(), "operation-checkpoint-1", checkpoint)
	if err != nil || string(fetched) != string(bundle) {
		t.Fatalf("bundle bytes = %d, %v", len(fetched), err)
	}
}

func TestUnixAPIRestoresOneVerifiedWorkspaceCheckpointBundle(t *testing.T) {
	checkpoint, bundle := testWorkspaceCheckpoint(t, time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))
	socket := unixSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/sessions/session-replacement/workspace/restore" ||
			request.Header.Get("Idempotency-Key") != "restore-operation-1" ||
			request.Header.Get("X-Coop-Expected-Revision") != "1" {
			t.Errorf("request = %s %s headers=%v", request.Method, request.URL.Path, request.Header)
		}
		descriptor, decodeErr := base64.StdEncoding.DecodeString(request.Header.Get("X-Coop-Workspace-Checkpoint"))
		decoded, descriptorErr := workerproto.DecodeWorkspaceCheckpoint(descriptor)
		body, readErr := io.ReadAll(request.Body)
		if decodeErr != nil || descriptorErr != nil || readErr != nil ||
			decoded.CheckpointRef != checkpoint.CheckpointRef || !bytes.Equal(body, bundle) {
			t.Errorf("restore body=%d decode=%v descriptor=%v read=%v", len(body), decodeErr, descriptorErr, readErr)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"operation":{"id":"restore-operation-1","state":"succeeded"},"session":{"id":"session-replacement","revision":2}}`))
	})}
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close() })
	api, err := NewUnixAPI(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := api.RestoreWorkspaceCheckpoint(
		context.Background(), "session-replacement", "restore-operation-1", 1, checkpoint, bundle,
	)
	if err != nil || !bytes.Contains(result, []byte("session-replacement")) {
		t.Fatalf("restore result = %s, %v", result, err)
	}
}

func TestUnixAPIProducesTypedConfirmedErrors(t *testing.T) {
	socket := unixSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusConflict)
		_, _ = response.Write([]byte(`{"code":"revision_conflict","detail":"expected revision 1"}`))
	})}
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close() })

	api, err := NewUnixAPI(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.Do(context.Background(), Request{Method: "GET", Path: "/v1/operations?key=x"})
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Status != 409 || apiErr.Code != "revision_conflict" {
		t.Fatalf("error = %#v", err)
	}
}

func unixSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "coop-worker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "api.sock")
}
