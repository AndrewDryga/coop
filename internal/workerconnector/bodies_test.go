package workerconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

type testBodyTransport struct {
	input     []byte
	uploaded  []byte
	uploadRef workerproto.BodyReference
	commandID string
	fetchErr  error
	uploadErr error
	onFetch   func()
}

func (f *testBodyTransport) FetchRequestBody(_ context.Context, id string, _ workerproto.BodyReference, w io.Writer) error {
	f.commandID = id
	if f.onFetch != nil {
		f.onFetch()
	}
	if f.fetchErr != nil {
		return f.fetchErr
	}
	_, err := w.Write(f.input)
	return err
}
func (f *testBodyTransport) UploadResponseBody(_ context.Context, id string, ref workerproto.BodyReference, r io.Reader) error {
	f.commandID, f.uploadRef = id, ref
	var err error
	f.uploaded, err = io.ReadAll(r)
	if f.uploadErr != nil {
		return f.uploadErr
	}
	return err
}

func tunnelFixture(t *testing.T, handler http.Handler, transport BodyTransport) *Executor {
	t.Helper()
	socket := unixSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	api, err := NewUnixAPI(socket, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewExecutor(ExecutorConfig{API: api, BodyTransport: transport, JournalDir: t.TempDir(), Now: time.Now, WorkerID: "worker-a"})
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func TestGenericAPIForwardsAllMethodsAndJSONShapes(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			expected := json.RawMessage(`{"expected_revision":3,"context":{"product":"another-controller"}}`)
			if method == "GET" || method == "HEAD" {
				expected = nil
			}
			executor := tunnelFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(raw, expected) || r.Method != method || r.RequestURI != "/v1/arbitrary-resource?limit=10" {
					t.Errorf("forwarded %s %s %s %v", r.Method, r.RequestURI, raw, err)
				}
				if expected != nil && r.Header.Get("Idempotency-Key") != "generic-key" {
					t.Error("missing mutation identity")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[{"id":"one"},{"id":"two"}]`))
			}), nil)
			command := createCommand(time.Now().Add(time.Minute))
			command.IdempotencyKey = "generic-key"
			command.Payload = apiPayload(method, "/v1/arbitrary-resource?limit=10", expected)
			result, err := executor.Execute(context.Background(), command)
			if err != nil || result.State != "succeeded" {
				t.Fatalf("result %+v %v", result, err)
			}
			var response workerproto.APIResponse
			if json.Unmarshal(result.Resource, &response) != nil || response.Status != 200 {
				t.Fatalf("response %s", result.Resource)
			}
			if method != "HEAD" && string(response.Body) != `[{"id":"one"},{"id":"two"}]` {
				t.Fatalf("array response %s", response.Body)
			}
		})
	}
}

func TestGenericAPIReportsHTTPFailureWithoutReinterpretingIt(t *testing.T) {
	executor := tunnelFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":{"code":"revision_conflict","detail":"changed"}}`))
	}), nil)
	command := createCommand(time.Now().Add(time.Minute))
	command.Payload = apiPayload("POST", "/v1/sessions/s/publications", json.RawMessage(`{"expected_revision":3}`))
	result, err := executor.Execute(context.Background(), command)
	var response workerproto.APIResponse
	if err != nil || result.State != "succeeded" || json.Unmarshal(result.Resource, &response) != nil || response.Status != 409 ||
		!strings.Contains(string(response.Body), "revision_conflict") {
		t.Fatalf("response %+v %v", result, err)
	}
}

func TestGenericAPITransfersLargeBodiesOutsideThePoll(t *testing.T) {
	data := bytes.Repeat([]byte{0, 1, 2, 3}, 17<<20) // above the former 64 MiB publication limit
	transport := &testBodyTransport{input: data}
	executor := tunnelFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sum := sha256sum(data)
		raw, err := io.ReadAll(r.Body)
		if err != nil || sha256sum(raw) != sum || r.ContentLength != int64(len(data)) || r.Header.Get("X-Coop-Expected-Revision") != "4" {
			t.Errorf("large request identity changed: %v", err)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(raw)
	}), transport)
	request := workerproto.APIRequest{Method: "POST", Path: "/v1/sessions/s/workspace/restore",
		Headers: map[string]string{"content-type": "application/octet-stream", "x-coop-expected-revision": "4"},
		BodyRef: &workerproto.BodyReference{SHA256: sha256sum(data), ByteSize: int64(len(data))}}
	command := createCommand(time.Now().Add(time.Minute))
	command.Payload, _ = json.Marshal(request)
	result, err := executor.Execute(context.Background(), command)
	var response workerproto.APIResponse
	if err != nil || result.State != "succeeded" || json.Unmarshal(result.Resource, &response) != nil ||
		response.BodyRef == nil || *response.BodyRef != *request.BodyRef || transport.commandID != command.CommandID ||
		!bytes.Equal(transport.uploaded, data) || len(result.Resource) > 1024 {
		t.Fatalf("large body result %+v %v", result, err)
	}
}

func TestGenericAPIRejectsCorruptBodyBeforeCallingService(t *testing.T) {
	transport := &testBodyTransport{input: []byte("changed")}
	calls := 0
	executor := tunnelFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }), transport)
	request := workerproto.APIRequest{Method: "POST", Path: "/v1/sessions/s/workspace/restore",
		BodyRef: &workerproto.BodyReference{SHA256: sha256sum([]byte("original")), ByteSize: 8}}
	command := createCommand(time.Now().Add(time.Minute))
	command.Payload, _ = json.Marshal(request)
	result, err := executor.Execute(context.Background(), command)
	if err != nil || result.State != "failed" || calls != 0 {
		t.Fatalf("corrupt body result %+v %v calls=%d", result, err, calls)
	}
}

func TestGenericAPITransientFetchRemainsRetryable(t *testing.T) {
	transport := &testBodyTransport{input: []byte("body"), fetchErr: errors.New("temporary")}
	executor := tunnelFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}), transport)
	request := workerproto.APIRequest{Method: "POST", Path: "/v1/sessions/s/workspace/restore",
		BodyRef: &workerproto.BodyReference{SHA256: sha256sum(transport.input), ByteSize: 4}}
	command := createCommand(time.Now().Add(time.Minute))
	command.Payload, _ = json.Marshal(request)
	if _, err := executor.Execute(context.Background(), command); !errors.Is(err, errArtifactTransfer) {
		t.Fatalf("fetch error %v", err)
	}
	entry, err := executor.journal.read(executor.journal.path(command.CommandID))
	if err != nil || entry.State != "received" {
		t.Fatalf("receipt %+v %v", entry, err)
	}
	transport.fetchErr = nil
	if result, err := executor.Execute(context.Background(), command); err != nil || result.State != "succeeded" {
		t.Fatalf("retry %+v %v", result, err)
	}
}

func TestGenericAPIReplaysSavedResponseAfterLostUploadAcknowledgement(t *testing.T) {
	transport := &testBodyTransport{uploadErr: errors.New("lost upload acknowledgement")}
	calls := 0
	executor := tunnelFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("immutable result"))
	}), transport)
	command := createCommand(time.Now().Add(time.Minute))
	command.Payload = apiPayload("POST", "/v1/sessions/s/checkpoint", json.RawMessage(`{"expected_revision":1}`))
	if _, err := executor.Execute(context.Background(), command); !errors.Is(err, errArtifactTransfer) {
		t.Fatalf("upload failure = %v", err)
	}
	retained := append([]byte(nil), transport.uploaded...)
	transport.uploadErr = nil
	reopened, err := NewExecutor(ExecutorConfig{API: executor.api, BodyTransport: transport,
		JournalDir: filepath.Dir(executor.journal.dir), Now: time.Now, WorkerID: "worker-a"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := reopened.Execute(context.Background(), command)
	if err != nil || result.State != "succeeded" || calls != 1 || !bytes.Equal(retained, transport.uploaded) {
		t.Fatalf("replay %+v %v calls=%d", result, err, calls)
	}
	if err := reopened.journal.acknowledgeResults([]string{command.CommandID}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(reopened.journal.path(command.CommandID) + ".body"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("acknowledged body retained: %v", err)
	}
}

func TestGenericAPILeaseMustStillBeValidAfterFetchingBody(t *testing.T) {
	now := time.Now()
	transport := &testBodyTransport{input: []byte("body"), onFetch: func() { now = now.Add(time.Hour) }}
	calls := 0
	executor := tunnelFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}), transport)
	executor.now = func() time.Time { return now }
	command := createCommand(now.Add(time.Minute))
	command.Payload, _ = json.Marshal(workerproto.APIRequest{Method: "POST", Path: "/v1/sessions/s/workspace/restore",
		BodyRef: &workerproto.BodyReference{SHA256: sha256sum(transport.input), ByteSize: 4}})
	if _, err := executor.Execute(context.Background(), command); !errors.Is(err, ErrLeaseExpired) || calls != 0 {
		t.Fatalf("expired preparation sent mutation: calls=%d err=%v", calls, err)
	}
	transport.onFetch = nil
	command.LeaseExpiresAt = now.Add(time.Minute)
	if result, err := executor.Execute(context.Background(), command); err != nil || result.State != "succeeded" || calls != 1 {
		t.Fatalf("renewed command %+v %v calls=%d", result, err, calls)
	}
}

func TestGenericAPIPermanentFetchRefusalIsDefinite(t *testing.T) {
	transport := &testBodyTransport{fetchErr: &BodyStatusError{Status: 404}}
	executor := tunnelFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("refused body reached API") }), transport)
	command := createCommand(time.Now().Add(time.Minute))
	command.Payload, _ = json.Marshal(workerproto.APIRequest{Method: "POST", Path: "/v1/sessions/s/workspace/restore",
		BodyRef: &workerproto.BodyReference{SHA256: sha256sum([]byte("body")), ByteSize: 4}})
	result, err := executor.Execute(context.Background(), command)
	if err != nil || result.State != "failed" {
		t.Fatalf("refusal %+v %v", result, err)
	}
}
