package workerconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// One completed and discarded session kept retrying its ACK for a day because
// an earlier resource-free failure still owned its placement generation.
func TestDiscardedCreateRetiresOnlyWithExactTerminalProof(t *testing.T) {
	for _, scenario := range []string{"discarded", "active", "closed", "missing", "wrong-session", "wrong-job", "wrong-digest", "wrong-external", "missing-queue", "missing-bytes", "queued", "queued-bytes", "bad-request-digest", "bad-request-task", "active-turn", "wrong-operation", "original-running", "origin-race", "receipt-race"} {
		t.Run(scenario, func(t *testing.T) {
			executor, _, conflict, _ := conflictingReceipt(t)
			originPath := executor.journal.createOriginPath(conflict.SessionRef)
			original, err := os.ReadFile(originPath)
			if err != nil {
				t.Fatal(err)
			}
			command := createWithJobSource(t)
			request, err := workerproto.DecodeAPIRequest(command.Payload)
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Job workerproto.JobSpec `json:"job"`
			}
			if err := json.Unmarshal(request.Body, &body); err != nil {
				t.Fatal(err)
			}
			body.Job.JobRef = "job:discarded"
			digest, err := body.Job.Digest()
			if err != nil {
				t.Fatal(err)
			}
			expectedDigest, task := digest, body.Job.JobRef
			if scenario == "bad-request-digest" {
				expectedDigest = strings.Repeat("0", 64)
			}
			if scenario == "bad-request-task" {
				task = "another-task"
			}
			request.Body, err = json.Marshal(map[string]any{"task": task, "job": body.Job, "expected_job_digest": expectedDigest})
			if err != nil {
				t.Fatal(err)
			}
			conflict.Payload, err = json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			conflict.CommandID, conflict.IdempotencyKey = "discarded-create", "discarded-key"
			entry := originReceipt(t, executor, conflict, `{"operation":{"id":"discarded-op","method":"CreateRemoteSession","state":"running"}}`)
			receipt, err := os.ReadFile(executor.journal.path(conflict.CommandID))
			if err != nil {
				t.Fatal(err)
			}
			session := map[string]any{"id": "discarded-session", "external_ref": body.Job.JobRef, "job_ref": body.Job.JobRef, "job_digest": digest, "state": "discarded", "activity": "parked", "queued_turn_count": 0, "queued_prompt_bytes": 0}
			switch scenario {
			case "active", "closed":
				session["state"] = scenario
			case "wrong-session":
				session["id"] = "another-session"
			case "wrong-job":
				session["job_ref"] = "another-job"
			case "wrong-digest":
				session["job_digest"] = strings.Repeat("0", 64)
			case "wrong-external":
				session["external_ref"] = "another-task"
			case "missing-queue":
				delete(session, "queued_turn_count")
			case "missing-bytes":
				delete(session, "queued_prompt_bytes")
			case "queued-bytes":
				session["queued_prompt_bytes"] = 1
			case "queued":
				session["queued_turn_count"] = 1
			case "active-turn":
				session["active_turn_id"] = "active-turn"
			}
			executor.api = discardedAPI{read: func(request Request) (json.RawMessage, error) {
				switch request.Path {
				case "/v1/operations?key=discarded-key":
					id := "discarded-op"
					if scenario == "wrong-operation" {
						id = "other-operation"
					}
					return json.Marshal(createOperation{ID: id, Method: "CreateRemoteSession", State: "succeeded", ResourceType: "session", ResourceID: "discarded-session"})
				case "/v1/operations?key=responder%3Awork%3Acreate%3Asession-1%3Ag1":
					state := "failed"
					if scenario == "original-running" {
						state = "running"
					}
					return json.Marshal(createOperation{ID: "create-1", Method: "CreateRemoteSession", State: state})
				case "/v1/sessions/discarded-session":
					if scenario == "missing" {
						return nil, errors.New("404 session_not_found")
					}
					if scenario == "origin-race" {
						origin, err := executor.journal.readCreateOrigin(originPath)
						if err != nil {
							t.Fatal(err)
						}
						origin.State = "pending"
						if err := executor.journal.writeActivityRecord(originPath, origin); err != nil {
							t.Fatal(err)
						}
					}
					if scenario == "receipt-race" {
						changed := entry
						result := *entry.Result
						result.Resource = responsePayload(json.RawMessage(`{"changed":true}`))
						changed.Result = &result
						if _, err := executor.journal.save(changed); err != nil {
							t.Fatal(err)
						}
					}
					return json.Marshal(session)
				default:
					t.Fatalf("unexpected proof request: %s", request.Path)
					return nil, nil
				}
			}}
			stream := eventStream{Version: eventStreamVersion, SessionRef: conflict.SessionRef, PlacementGeneration: conflict.PlacementGeneration, CoopSessionID: "discarded-session", LastPublishedSequence: 5, AcknowledgedSequence: 4, TerminalSequence: 5}
			if err := executor.journal.writeEventStream(executor.journal.eventStreamPath(conflict.SessionRef), stream); err != nil {
				t.Fatal(err)
			}
			if err := executor.journal.advanceEventScan(conflict.SessionRef); err != nil {
				t.Fatal(err)
			}
			saved := map[string][]byte{}
			for _, path := range []string{executor.journal.eventStreamPath(conflict.SessionRef), executor.journal.eventScanPath()} {
				value, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				saved[path] = value
			}
			err = executor.acknowledgeResult(context.Background(), entry)
			if scenario == "discarded" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(executor.journal.path(conflict.CommandID)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("discarded receipt remains: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("unproven receipt retired")
				}
				if scenario != "receipt-race" {
					assertReceiptBytes(t, executor, conflict, receipt)
				} else if _, err := os.Stat(executor.journal.path(conflict.CommandID)); err != nil {
					t.Fatal("changed receipt was removed")
				}
			}
			if scenario != "origin-race" {
				after, err := os.ReadFile(originPath)
				if err != nil || !bytes.Equal(original, after) {
					t.Fatalf("origin was rewritten: %v", err)
				}
			}
			for path, before := range saved {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("retirement changed activity custody: %v", err)
				}
			}
		})
	}
}

type discardedAPI struct {
	read func(Request) (json.RawMessage, error)
}

func (a discardedAPI) Do(_ context.Context, r Request) (json.RawMessage, error) { return a.read(r) }
func (a discardedAPI) Forward(ctx context.Context, r Request, b io.Reader) (*http.Response, error) {
	return forwardTestAPI(ctx, a, r, b)
}
