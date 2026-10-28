package sessionsvc

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/tasks"
)

func TestSessionHTTPRestoreChecksBinaryMutationPreflight(t *testing.T) {
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	ctx := t.Context()
	const offer = "record:task_offer:http-restore"
	source, err := service.CreateRemoteSession(ctx, "http-restore-source", service.request(t, offer))
	if err != nil {
		t.Fatal(err)
	}
	source, err = service.EnsureWorkspaceTask(ctx, "http-restore-task", EnsureWorkspaceTaskRequest{SessionID: source.ID, ExpectedRevision: source.Revision, Task: tasks.ControllerTaskDraft{OfferRef: offer, Title: "Restore HTTP checkpoint", Prompt: "Keep the saved work.", SuccessChecks: []string{"saved content restored"}}})
	if err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(source.Workspace, "saved.txt"), "saved through binary HTTP\n")
	sessionWorkspaceGit(t, source.Workspace, "add", "saved.txt")
	sessionWorkspaceGit(t, source.Workspace, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "saved work")
	captured, err := service.CheckpointWorkspace(ctx, "http-restore-capture", CheckpointWorkspaceRequest{SessionID: source.ID, SessionRef: "http-source", ExpectedRevision: source.Revision, PlacementGeneration: 1, RepositoryRef: "repository"})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := readCheckpointBundle(t, service.Service, captured.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := json.Marshal(captured.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	target, err := service.CreateRemoteSession(ctx, "http-restore-target", service.request(t, offer))
	if err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(target.Workspace, "untouched.txt")
	sessionWorkspaceWrite(t, canary, "preflight must not mutate\n")
	handler := NewHTTPHandler(service.Service)
	path := "/v1/sessions/" + target.ID + "/workspace/restore"
	request := func(key string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(bundle))
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("Content-Type", captured.Checkpoint.Bundle.MediaType)
		r.Header.Set("X-Coop-Expected-Revision", strconv.FormatInt(target.Revision, 10))
		r.Header.Set("X-Coop-Workspace-Checkpoint", base64.StdEncoding.EncodeToString(descriptor))
		return r
	}
	type refusal struct {
		name   string
		change func(*http.Request)
		status int
	}
	cases := []refusal{{"query", func(r *http.Request) { r.URL.RawQuery = "unexpected=1" }, 400}, {"media", func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }, 415}, {"length", func(r *http.Request) { r.ContentLength++ }, 413}}
	for _, header := range []string{"Idempotency-Key", "Content-Type", "X-Coop-Expected-Revision", "X-Coop-Workspace-Checkpoint"} {
		cases = append(cases, refusal{"duplicate-" + header, func(r *http.Request) { r.Header.Add(header, r.Header.Get(header)) }, 400})
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			key := "http-restore-reject-" + test.name
			r := request(key)
			test.change(r)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			if response.Code != test.status || !strings.Contains(response.Body.String(), `"code":"invalid_request"`) {
				t.Fatalf("preflight=%d %s want %d", response.Code, response.Body.String(), test.status)
			}
			if _, err := service.GetOperation(ctx, key); session.CodeOf(err) != session.CodeOperationNotFound {
				t.Fatalf("preflight reserved operation: %v", err)
			}
			current := mustSession(t, service, target.ID)
			if current.Revision != target.Revision || current.WorkspaceTask != nil {
				t.Fatalf("preflight changed session: %+v", current)
			}
			if raw, err := os.ReadFile(canary); err != nil || string(raw) != "preflight must not mutate\n" {
				t.Fatalf("preflight changed workspace: %q %v", raw, err)
			}
			if head := sessionWorkspaceGit(t, target.Workspace, "rev-parse", "HEAD"); head != target.BaseCommit {
				t.Fatalf("preflight moved HEAD: %s", head)
			}
		})
	}
	var first sessionMutationSessionResponse
	for attempt := range 2 {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request("http-restore-valid"))
		if response.Code != 200 {
			t.Fatalf("restore=%d %s", response.Code, response.Body.String())
		}
		var got sessionMutationSessionResponse
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Operation.State != session.OperationSucceeded || got.Session.ID != target.ID {
			t.Fatalf("restore receipt: %+v", got)
		}
		if attempt == 0 {
			first = got
		} else if got.Operation.ID != first.Operation.ID || got.Session.Revision != first.Session.Revision {
			t.Fatal("restore replay changed receipt")
		}
	}
	restored := mustSession(t, service, target.ID)
	if restored.WorkspaceTask == nil || *restored.WorkspaceTask != *source.WorkspaceTask {
		t.Fatalf("restored task=%+v want %+v", restored.WorkspaceTask, source.WorkspaceTask)
	}
	if raw, err := os.ReadFile(filepath.Join(restored.Workspace, "saved.txt")); err != nil || string(raw) != "saved through binary HTTP\n" {
		t.Fatalf("restored content: %q %v", raw, err)
	}
	if head := sessionWorkspaceGit(t, restored.Workspace, "rev-parse", "HEAD"); head != captured.Checkpoint.CommittedRevision {
		t.Fatalf("restored HEAD=%s want %s", head, captured.Checkpoint.CommittedRevision)
	}
}
