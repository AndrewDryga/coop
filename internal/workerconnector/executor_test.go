package workerconnector

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestDurableCommandReceiptMakesRedeliveryOneIdempotentLocalOperation(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{response: json.RawMessage(`{"operation":{"id":"operation-create-1","state":"running"}}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))

	first, err := executor.Execute(context.Background(), command)
	if err != nil {
		t.Fatalf("first execute: %v", err)
	}
	if first.State != "succeeded" || len(api.requests) != 1 {
		t.Fatalf("first = %+v, requests = %d", first, len(api.requests))
	}
	if got := api.requests[0]; got.Method != "POST" || got.Path != "/v1/sessions" || got.IdempotencyKey != command.IdempotencyKey {
		t.Fatalf("request = %+v", got)
	}

	redelivered := command
	redelivered.LeaseExpiresAt = now.Add(2 * time.Minute)
	second, err := executor.Execute(context.Background(), redelivered)
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if string(second.Resource) != string(first.Resource) || len(api.requests) != 1 {
		t.Fatalf("redelivery = %+v, requests = %d", second, len(api.requests))
	}

	changed := redelivered
	changed.Payload = json.RawMessage(`{"external_ref":"episode-1","policy":"writable","policy_digest":"` + repeatedDigest("b") + `"}`)
	if _, err := executor.Execute(context.Background(), changed); !errors.Is(err, ErrCommandConflict) {
		t.Fatalf("changed payload error = %v, want command conflict", err)
	}
	if len(api.requests) != 1 {
		t.Fatalf("changed payload issued %d requests", len(api.requests))
	}
}

func TestCreateSessionCarriesTheExactPrivateResponderBinding(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{response: json.RawMessage(`{"operation":{"id":"operation-create-1","state":"running"}}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))
	command.Payload = json.RawMessage(`{"external_ref":"episode-1","policy":"work-read-only","policy_digest":"` + repeatedDigest("b") + `","responder_binding":{"endpoint":"https://responder.example/v1/state-tools/mcp","token":"` + strings.Repeat("t", 48) + `"}}`)

	if _, err := executor.Execute(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if len(api.requests) != 1 || string(api.requests[0].Body) !=
		`{"policy":"work-read-only","responder_binding":{"endpoint":"https://responder.example/v1/state-tools/mcp","token":"`+strings.Repeat("t", 48)+`"},"task":"episode-1"}` {
		t.Fatalf("create requests = %+v", api.requests)
	}
}

func TestEnsureWorkspaceCarriesTheExactApprovedTaskToTheBoundSession(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{response: json.RawMessage(`{"operation":{"id":"operation-workspace-1","state":"succeeded"},"session":{"id":"session-1","revision":2}}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{
		"coop_session_id": "session-1", "expected_revision": 1,
		"task": map[string]any{
			"offer_ref": "record:task_offer:0123456789abcdef", "title": "Fix parser retries",
			"prompt":           "Change the parser and preserve idempotency.",
			"success_checks":   []string{"focused tests pass", "retry remains idempotent"},
			"authority_limits": []string{"must not deploy"}, "instruction_ref": "input:trusted:1",
			"source_refs": []string{"artifact:incident:1"},
		},
	})
	command := createCommand(now.Add(time.Minute))
	command.Kind, command.Payload, command.IdempotencyKey = "ensure_workspace", payload, "responder:workspace:1"

	if _, err := executor.Execute(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if len(api.requests) != 1 {
		t.Fatalf("requests = %d", len(api.requests))
	}
	request := api.requests[0]
	if request.Method != "POST" || request.Path != "/v1/sessions/session-1/workspace" ||
		request.IdempotencyKey != command.IdempotencyKey {
		t.Fatalf("workspace request = %+v", request)
	}
	var body map[string]any
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["expected_revision"] != float64(1) || body["task"].(map[string]any)["offer_ref"] != "record:task_offer:0123456789abcdef" {
		t.Fatalf("workspace request body = %+v", body)
	}
}

func TestReplacementWorkerFetchesAndRestoresTheExactAuthorizedCheckpoint(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	checkpoint, bundle := testWorkspaceCheckpoint(t, now)
	api := &fakeAPI{restoreResponse: json.RawMessage(`{"operation":{"id":"operation-restore-1","state":"succeeded"},"session":{"id":"session-replacement","revision":2}}`)}
	artifacts := &fakeArtifactTransport{checkpointFetch: checkpoint, checkpointFetchBundle: bundle}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, ArtifactTransport: artifacts, JournalDir: t.TempDir(),
		Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{
		"coop_session_id": "session-replacement", "expected_revision": 1,
		"task": map[string]any{
			"offer_ref": "record:task_offer:0123456789abcdef", "title": "Restore parser retries",
			"prompt": "Continue the exact writable task.", "success_checks": []string{"focused tests pass"},
			"authority_limits": []string{}, "source_refs": []string{},
		},
		"checkpoint": map[string]any{
			"transfer_id":    "018f04f4-5555-7000-8000-000000000001",
			"checkpoint_ref": checkpoint.CheckpointRef, "sha256": checkpoint.Bundle.SHA256,
			"byte_size": checkpoint.Bundle.ByteSize, "source_session_ref": checkpoint.SessionRef,
			"source_placement_generation": checkpoint.PlacementGeneration,
		},
	})
	command := createCommand(now.Add(time.Minute))
	command.Kind, command.Payload, command.IdempotencyKey = "ensure_workspace", payload, "responder:workspace:restore:1"

	result, err := executor.Execute(context.Background(), command)
	if err != nil || result.State != "succeeded" {
		t.Fatalf("restore result = %+v, err=%v", result, err)
	}
	if len(artifacts.checkpointFetches) != 1 || artifacts.checkpointFetches[0] !=
		command.CommandID+"/018f04f4-5555-7000-8000-000000000001" {
		t.Fatalf("checkpoint fetches = %+v", artifacts.checkpointFetches)
	}
	if len(api.restores) != 1 || api.restores[0].sessionID != "session-replacement" ||
		api.restores[0].key != command.IdempotencyKey || api.restores[0].revision != 1 ||
		api.restores[0].checkpoint.CheckpointRef != checkpoint.CheckpointRef ||
		!bytes.Equal(api.restores[0].bundle, bundle) || len(api.requests) != 0 {
		t.Fatalf("restore calls = %+v, generic requests = %+v", api.restores, api.requests)
	}
}

func TestReceivedBeforeCrashIsSafelyResumedUnderTheSameOperationKey(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	journal, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))
	if _, err := journal.begin(command); err != nil {
		t.Fatal(err)
	}

	api := &fakeAPI{response: json.RawMessage(`{"operation":{"id":"operation-create-after-crash","state":"running"}}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: dir, Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), command)
	if err != nil || result.State != "succeeded" || len(api.requests) != 1 {
		t.Fatalf("resume = %+v, %v, requests = %d", result, err, len(api.requests))
	}
}

func TestLeaseAndWorkerFencesFailBeforeTheUnixAPI(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{response: json.RawMessage(`{}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}

	expired := createCommand(now)
	if _, err := executor.Execute(context.Background(), expired); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expired error = %v", err)
	}

	wrongWorker := createCommand(now.Add(time.Minute))
	wrongWorker.WorkerID = "worker-b"
	if _, err := executor.Execute(context.Background(), wrongWorker); !errors.Is(err, ErrWorkerMismatch) {
		t.Fatalf("wrong-worker error = %v", err)
	}

	if len(api.requests) != 0 {
		t.Fatalf("fenced commands issued %d requests", len(api.requests))
	}
}

func TestConfirmedPrivateAPIErrorKeepsItsHTTPStatusForResponder(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{err: &APIError{Status: 409, Code: "revision_conflict", Detail: "expected revision 1"}}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), createCommand(now.Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(result.Error, &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != float64(409) || body["code"] != "revision_conflict" {
		t.Fatalf("error = %s", result.Error)
	}
}

func TestSubmitTurnCarriesTheExactFrozenPromptAndSchemaToThePrivateAPI(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{response: json.RawMessage(`{"turn":{"id":"turn-remote-1","state":"queued"}}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}

	submission := map[string]any{
		"contract_version":    "work-final-v1",
		"context":             map[string]any{"mode": "full"},
		"input_artifact_refs": []any{},
		"output_schema":       map[string]any{"type": "object"},
		"prompt":              "Inspect this exact episode.",
	}
	payload, _ := json.Marshal(map[string]any{
		"coop_session_id": "session-remote-1", "expected_revision": 2,
		"submission": submission, "submission_sha256": canonicalDigest(t, submission), "turn_ref": "turn-2",
		"responder_binding": map[string]any{
			"endpoint": "https://responder.example/v1/state-tools/mcp",
			"token":    strings.Repeat("t", 48),
		},
	})
	command := createCommand(now.Add(time.Minute))
	command.Kind = "submit_turn"
	command.Payload = payload
	command.IdempotencyKey = "responder:work:turn:turn-2:g1"

	if _, err := executor.Execute(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if len(api.requests) != 1 {
		t.Fatalf("requests = %d", len(api.requests))
	}
	request := api.requests[0]
	if request.Path != "/v1/sessions/session-remote-1/turns" {
		t.Fatalf("path = %q", request.Path)
	}
	var body map[string]any
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["prompt"] != submission["prompt"] {
		t.Fatalf("prompt = %v", body["prompt"])
	}
	if body["expected_revision"] != float64(2) {
		t.Fatalf("revision = %v", body["expected_revision"])
	}
	binding := body["responder_binding"].(map[string]any)
	if binding["endpoint"] != "https://responder.example/v1/state-tools/mcp" ||
		binding["token"] != strings.Repeat("t", 48) {
		t.Fatalf("responder binding = %+v", binding)
	}
	contract := body["output_contract"].(map[string]any)
	if got, _ := json.Marshal(contract["json_schema"]); string(got) != `{"type":"object"}` ||
		contract["require_semantic_validation"] != true || contract["sha256"] != canonicalDigest(t, submission["output_schema"]) {
		t.Fatalf("output contract = %+v", contract)
	}
}

func TestSubmitTurnFetchesOnlyTheFrozenInputArtifactAndVerifiesItsBytes(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	data := []byte("exact authenticated pull request context")
	artifactRef := "artifact:input:review:1"
	artifacts := &fakeArtifactTransport{inputs: map[string]Artifact{
		artifactRef: {ID: artifactRef, Name: "review.txt", MediaType: "text/plain", SHA256: digestBytes(data), Data: data},
	}}
	api := &fakeAPI{response: json.RawMessage(`{"turn":{"id":"turn-remote-1","state":"queued"}}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, ArtifactTransport: artifacts, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	submission := map[string]any{
		"contract_version": "work-final-v1", "context": map[string]any{"mode": "full"},
		"input_artifact_refs": []any{artifactRef}, "output_schema": map[string]any{"type": "object"},
		"prompt": "Inspect the exact attachment.",
	}
	payload, _ := json.Marshal(map[string]any{
		"coop_session_id": "session-remote-1", "expected_revision": 2,
		"submission": submission, "submission_sha256": canonicalDigest(t, submission), "turn_ref": "turn-2",
	})
	command := createCommand(now.Add(time.Minute))
	command.Kind, command.Payload, command.IdempotencyKey = "submit_turn", payload, "responder:work:turn:artifact:g1"

	if _, err := executor.Execute(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if len(artifacts.fetches) != 1 || artifacts.fetches[0] != command.CommandID+"/"+artifactRef {
		t.Fatalf("artifact fetches = %+v", artifacts.fetches)
	}
	var body struct {
		Artifacts []struct {
			Data      []byte `json:"data"`
			MediaType string `json:"media_type"`
			Name      string `json:"name"`
			SHA256    string `json:"sha256"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(api.requests[0].Body, &body); err != nil || len(body.Artifacts) != 1 {
		t.Fatalf("submit body = %s, %v", api.requests[0].Body, err)
	}
	if string(body.Artifacts[0].Data) != string(data) || body.Artifacts[0].SHA256 != digestBytes(data) {
		t.Fatalf("submitted artifact = %+v", body.Artifacts[0])
	}
}

func TestSubmitTurnFenceFetchesOnlyTheAuthenticatedFrozenInputArtifact(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	data := []byte("exact artifact for a stopped turn")
	artifactRef := "artifact:input:stopped-turn:1"
	artifacts := &fakeArtifactTransport{inputs: map[string]Artifact{
		artifactRef: {ID: artifactRef, Name: "request.txt", MediaType: "text/plain", SHA256: digestBytes(data), Data: data},
	}}
	api := &fakeAPI{response: json.RawMessage(`{"operation":{"id":"operation-fence-1","state":"failed","error_code":"operation_fenced"}}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, ArtifactTransport: artifacts, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))
	command.Kind = "fence_operation"
	command.IdempotencyKey = "responder:work:turn:artifact:g1"
	command.Payload = json.RawMessage(`{
		"input_artifact_refs":["` + artifactRef + `"],
		"method":"SubmitTurn",
		"request":{
			"session_id":"session-remote-1",
			"expected_revision":2,
			"prompt":"frozen prompt",
			"responder_binding":{"endpoint":"https://responder.example/v1/state-tools/mcp","token":"` + strings.Repeat("t", 48) + `"},
			"output_contract":{"json_schema":{"type":"object"},"require_semantic_validation":true,"sha256":"` + repeatedDigest("d") + `"}
		}
	}`)

	result, err := executor.Execute(context.Background(), command)
	if err != nil || result.State != "succeeded" {
		t.Fatalf("execute fence = %+v, %v", result, err)
	}
	if len(artifacts.fetches) != 1 || artifacts.fetches[0] != command.CommandID+"/"+artifactRef {
		t.Fatalf("artifact fetches = %+v", artifacts.fetches)
	}
	if len(api.requests) != 1 {
		t.Fatalf("requests = %d", len(api.requests))
	}
	var body struct {
		Method  string `json:"method"`
		Request struct {
			ResponderBinding *responderBinding `json:"responder_binding"`
			Artifacts        []struct {
				Data      []byte `json:"data"`
				MediaType string `json:"media_type"`
				Name      string `json:"name"`
				SHA256    string `json:"sha256"`
			} `json:"artifacts"`
		} `json:"request"`
	}
	if err := json.Unmarshal(api.requests[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Method != "SubmitTurn" || len(body.Request.Artifacts) != 1 ||
		string(body.Request.Artifacts[0].Data) != string(data) ||
		body.Request.Artifacts[0].SHA256 != digestBytes(data) ||
		body.Request.ResponderBinding == nil ||
		body.Request.ResponderBinding.Endpoint != "https://responder.example/v1/state-tools/mcp" ||
		body.Request.ResponderBinding.Token != strings.Repeat("t", 48) {
		t.Fatalf("fence body = %s", api.requests[0].Body)
	}
}

func TestOperationFenceRejectsInlineOrWrongMethodArtifactReferences(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{response: json.RawMessage(`{}`)}
	artifacts := &fakeArtifactTransport{inputs: map[string]Artifact{}}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, ArtifactTransport: artifacts, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}

	commands := []workerproto.Command{
		func() workerproto.Command {
			command := createCommand(now.Add(time.Minute))
			command.Kind = "fence_operation"
			command.Payload = json.RawMessage(`{"input_artifact_refs":[],"method":"SubmitTurn","request":{"session_id":"session-1","expected_revision":1,"prompt":"frozen","artifacts":[]}}`)
			return command
		}(),
		func() workerproto.Command {
			command := createCommand(now.Add(time.Minute))
			command.CommandID = "018f04f4-1111-7000-8000-000000000002"
			command.Kind = "fence_operation"
			command.Payload = json.RawMessage(`{"input_artifact_refs":["artifact:input:1"],"method":"CreateRemoteSession","request":{"policy":"read-only","task":"episode-1"}}`)
			return command
		}(),
	}

	for _, command := range commands {
		result, err := executor.Execute(context.Background(), command)
		if err != nil {
			t.Fatal(err)
		}
		if result.State != "failed" {
			t.Fatalf("result = %+v, want failed", result)
		}
	}
	if len(api.requests) != 0 || len(artifacts.fetches) != 0 {
		t.Fatalf("requests = %d, artifact fetches = %+v", len(api.requests), artifacts.fetches)
	}
}

func TestOutputArtifactIsFetchedPrivatelyThenUploadedUnderTheExactCommand(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	data := []byte{137, 80, 78, 71, 13, 10, 26, 10, 'c'}
	artifact := Artifact{ID: "artifact_chart", Name: "chart.png", MediaType: "image/png", SHA256: digestBytes(data), Data: data}
	api := &fakeAPI{outputArtifact: artifact}
	artifacts := &fakeArtifactTransport{uploadResponse: []byte(`{"transfer_id":"018f04f4-3333-7000-8000-000000000001"}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, ArtifactTransport: artifacts, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))
	command.Kind = "get_output_artifact"
	command.IdempotencyKey = "responder:fleet:read:get_output_artifact:1"
	command.Payload = json.RawMessage(`{"artifact_ref":"artifact_chart","coop_session_id":"session-remote-1","coop_turn_id":"turn-remote-1"}`)

	result, err := executor.Execute(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "succeeded" || len(api.outputFetches) != 1 || len(artifacts.uploads) != 1 {
		t.Fatalf("result = %+v fetches = %+v uploads = %+v", result, api.outputFetches, artifacts.uploads)
	}
	if api.outputFetches[0] != "session-remote-1/turn-remote-1/artifact_chart" || artifacts.uploads[0].ID != artifact.ID {
		t.Fatalf("fetches = %+v uploads = %+v", api.outputFetches, artifacts.uploads)
	}
}

func TestReviewPatchUsesTheSeparateVerifiedBinaryTransfer(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	patch := []byte("diff --git a/a b/a\n+verified\n")
	digest := digestBytes(patch)
	api := &fakeAPI{reviewPatch: patch}
	artifacts := &fakeArtifactTransport{reviewUploadResponse: []byte(`{"transfer_id":"018f04f4-4444-7000-8000-000000000001"}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, ArtifactTransport: artifacts, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))
	command.Kind = "get_review_patch"
	command.IdempotencyKey = "responder:fleet:read:get_review_patch:1"
	payload, _ := json.Marshal(map[string]any{
		"artifact_id": "review-artifact-1", "coop_session_id": "session-remote-1",
		"expected_bytes": len(patch), "expected_sha256": digest,
	})
	command.Payload = payload

	result, err := executor.Execute(context.Background(), command)
	if err != nil || result.State != "succeeded" || len(api.reviewFetches) != 1 || len(artifacts.reviewUploads) != 1 {
		t.Fatalf("result = %+v, %v fetches=%+v uploads=%+v", result, err, api.reviewFetches, artifacts.reviewUploads)
	}
	if api.reviewFetches[0] != "review-artifact-1/"+digest || string(artifacts.reviewUploads[0]) != string(patch) {
		t.Fatalf("fetches=%+v uploads=%q", api.reviewFetches, artifacts.reviewUploads)
	}
}

func TestWorkspaceCheckpointIsCapturedVerifiedAndUploadedUnderTheExactCommand(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	checkpoint, bundle := testWorkspaceCheckpoint(t, now)
	descriptor, _ := json.Marshal(checkpoint)
	api := &fakeAPI{
		response:         json.RawMessage(`{"operation":{"id":"operation-checkpoint-1","method":"CheckpointWorkspace","state":"succeeded","resource_type":"workspace_checkpoint","resource_id":"` + checkpoint.CheckpointRef + `"},"checkpoint":` + string(descriptor) + `}`),
		checkpointBundle: bundle,
	}
	artifacts := &fakeArtifactTransport{
		checkpointUploadResponse: []byte(`{"checkpoint_ref":"` + checkpoint.CheckpointRef + `","state":"stored"}`),
	}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, ArtifactTransport: artifacts, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))
	command.Kind = "checkpoint_workspace"
	command.IdempotencyKey = "responder:fleet:checkpoint:session-1:g1"
	command.Payload = json.RawMessage(`{"coop_session_id":"session-remote-1","session_ref":"` + command.SessionRef + `","expected_revision":4,"repository_ref":"responder"}`)

	result, err := executor.Execute(context.Background(), command)
	if err != nil || result.State != "succeeded" {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if len(api.requests) != 1 {
		t.Fatalf("private requests = %+v", api.requests)
	}
	if got := api.requests[0]; got.Method != "POST" || got.Path != "/v1/sessions/session-remote-1/checkpoint" ||
		got.IdempotencyKey != command.IdempotencyKey || !bytes.Contains(got.Body, []byte(`"placement_generation":1`)) {
		t.Fatalf("checkpoint request = %+v", got)
	}
	if len(api.checkpointFetches) != 1 || api.checkpointFetches[0] != "operation-checkpoint-1/"+checkpoint.CheckpointRef {
		t.Fatalf("checkpoint fetches = %+v", api.checkpointFetches)
	}
	if len(artifacts.checkpointUploads) != 1 || artifacts.checkpointUploads[0].CheckpointRef != checkpoint.CheckpointRef ||
		!bytes.Equal(artifacts.checkpointBundles[0], bundle) {
		t.Fatalf("checkpoint uploads = %+v", artifacts.checkpointUploads)
	}
}

func TestWorkspaceCheckpointSecretsNeverLeaveTheWorker(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	checkpoint, bundle := testWorkspaceCheckpointWithTask(
		t,
		now,
		[]byte("Status: in_progress\napi_key = \"sk-proj-Ab3kP9xR2mQ7vL4Tn8wZ1Cf6\"\n"),
	)
	descriptor, _ := json.Marshal(checkpoint)
	api := &fakeAPI{
		response:         json.RawMessage(`{"operation":{"id":"operation-checkpoint-secret","method":"CheckpointWorkspace","state":"succeeded","resource_type":"workspace_checkpoint","resource_id":"` + checkpoint.CheckpointRef + `"},"checkpoint":` + string(descriptor) + `}`),
		checkpointBundle: bundle,
	}
	artifacts := &fakeArtifactTransport{}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, ArtifactTransport: artifacts, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))
	command.Kind = "checkpoint_workspace"
	command.IdempotencyKey = "responder:fleet:checkpoint:secret:g1"
	command.Payload = json.RawMessage(`{"coop_session_id":"session-remote-1","session_ref":"` + command.SessionRef + `","expected_revision":4,"repository_ref":"responder"}`)

	result, err := executor.Execute(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "failed" || !bytes.Contains(result.Error, []byte(`"code":"checkpoint_secret_detected"`)) {
		t.Fatalf("result = %+v", result)
	}
	if len(artifacts.checkpointUploads) != 0 {
		t.Fatalf("secret checkpoint uploads = %+v", artifacts.checkpointUploads)
	}

	restoreAPI := &fakeAPI{}
	restoreArtifacts := &fakeArtifactTransport{checkpointFetch: checkpoint, checkpointFetchBundle: bundle}
	restoreExecutor, err := NewExecutor(ExecutorConfig{
		API: restoreAPI, ArtifactTransport: restoreArtifacts, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	restore := createCommand(now.Add(time.Minute))
	restore.Kind = "ensure_workspace"
	restore.IdempotencyKey = "responder:fleet:restore:secret:g1"
	restore.Payload, _ = json.Marshal(map[string]any{
		"coop_session_id": "session-replacement", "expected_revision": 1,
		"task": map[string]any{
			"authority_limits": []string{"must not deploy"}, "offer_ref": "record:task_offer:restore-secret",
			"prompt": "Restore the exact workspace.", "source_refs": []string{},
			"success_checks": []string{"focused tests pass"}, "title": "Restore checkpoint",
		},
		"checkpoint": map[string]any{
			"byte_size": checkpoint.Bundle.ByteSize, "checkpoint_ref": checkpoint.CheckpointRef,
			"sha256": checkpoint.Bundle.SHA256, "source_placement_generation": checkpoint.PlacementGeneration,
			"source_session_ref": checkpoint.SessionRef, "transfer_id": "018f04f4-9999-7000-8000-000000000001",
		},
	})

	restored, err := restoreExecutor.Execute(context.Background(), restore)
	if err != nil {
		t.Fatal(err)
	}
	if restored.State != "failed" || !bytes.Contains(restored.Error, []byte(`"code":"checkpoint_secret_detected"`)) {
		t.Fatalf("restore result = %+v", restored)
	}
	if len(restoreAPI.restores) != 0 {
		t.Fatalf("secret checkpoint restores = %+v", restoreAPI.restores)
	}
}

func TestCandidateValidationAndOperationFenceRemainNarrowTypedMutations(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{response: json.RawMessage(`{"operation":{"id":"operation-1","state":"succeeded"}}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}

	validation := createCommand(now.Add(time.Minute))
	validation.Kind = "validate_candidate"
	validation.IdempotencyKey = "responder:work:validation:turn-1:a2:g1"
	validation.Payload = json.RawMessage(`{"coop_session_id":"session-remote-1","coop_turn_id":"turn-remote-1","candidate_attempt":2,"candidate_sha256":"` + repeatedDigest("c") + `","verdict":"reject","violations":["missing required evidence"]}`)
	if _, err := executor.Execute(context.Background(), validation); err != nil {
		t.Fatal(err)
	}

	fence := createCommand(now.Add(time.Minute))
	fence.CommandID = "018f04f4-1111-7000-8000-000000000002"
	fence.Kind = "fence_operation"
	fence.IdempotencyKey = "responder:work:turn:turn-2:g1"
	fence.Payload = json.RawMessage(`{"input_artifact_refs":[],"method":"SubmitTurn","request":{"session_id":"session-remote-1","expected_revision":2,"prompt":"frozen prompt","output_contract":{"json_schema":{"type":"object"},"require_semantic_validation":true,"sha256":"` + repeatedDigest("d") + `"}}}`)
	if _, err := executor.Execute(context.Background(), fence); err != nil {
		t.Fatal(err)
	}

	if len(api.requests) != 2 {
		t.Fatalf("requests = %d", len(api.requests))
	}
	if got := api.requests[0]; got.Path != "/v1/sessions/session-remote-1/turns/turn-remote-1/validation" || got.IdempotencyKey != validation.IdempotencyKey {
		t.Fatalf("validation request = %+v", got)
	}
	if got := api.requests[1]; got.Path != "/v1/operations/fence" || got.IdempotencyKey != fence.IdempotencyKey {
		t.Fatalf("fence request = %+v", got)
	}
}

func TestCandidateValidationRejectsBodiesCoopWouldReject(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{response: json.RawMessage(`{}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))
	command.Kind = "validate_candidate"
	command.IdempotencyKey = "responder:work:validation:turn-1:a1:g1"
	command.Payload = json.RawMessage(`{"coop_session_id":"session-remote-1","coop_turn_id":"turn-remote-1","candidate_attempt":1,"candidate_sha256":"` + repeatedDigest("c") + `","verdict":"reject","violations":[" ` + strings.Repeat("x", 4095) + `"]}`)

	result, err := executor.Execute(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "failed" || len(api.requests) != 0 {
		t.Fatalf("result = %+v, requests = %d", result, len(api.requests))
	}
}

func TestConnectorKeepsAResultUntilResponderAcknowledgesIt(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{response: json.RawMessage(`{"operation":{"id":"operation-create-1","state":"running"}}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))
	transport := &scriptedTransport{responses: []workerproto.Response{
		{Version: 1, PollRef: "poll:worker-a:1", ServerTime: now, Commands: []workerproto.Command{command}},
		{Version: 1, PollRef: "poll:worker-a:2", ServerTime: now, AcknowledgedResultCommandIDs: []string{command.CommandID}},
		{Version: 1, PollRef: "poll:worker-a:3", ServerTime: now},
	}}
	connector, err := NewConnector(ConnectorConfig{
		Executor: executor, Hello: func(clock time.Time) workerproto.WorkerHello { return hello(clock) },
		Now: func() time.Time { return now }, Transport: transport,
	})
	if err != nil {
		t.Fatal(err)
	}

	for index := 0; index < 3; index++ {
		if err := connector.PollOnce(context.Background()); err != nil {
			t.Fatalf("poll %d: %v", index+1, err)
		}
	}
	if len(transport.polls) != 3 {
		t.Fatalf("polls = %d", len(transport.polls))
	}
	if len(transport.polls[1].AcknowledgedCommandIDs) != 1 || len(transport.polls[1].CommandResults) != 1 {
		t.Fatalf("second poll = %+v", transport.polls[1])
	}
	if len(transport.polls[2].AcknowledgedCommandIDs) != 0 || len(transport.polls[2].CommandResults) != 0 {
		t.Fatalf("acknowledged result was retained: %+v", transport.polls[2])
	}
	if len(api.requests) != 1 {
		t.Fatalf("private API requests = %d", len(api.requests))
	}
}

type scriptedTransport struct {
	polls     []workerproto.Poll
	responses []workerproto.Response
}

func (s *scriptedTransport) Poll(_ context.Context, poll workerproto.Poll) (workerproto.Response, error) {
	s.polls = append(s.polls, poll)
	if len(s.responses) == 0 {
		return workerproto.Response{}, errors.New("unexpected poll")
	}
	response := s.responses[0]
	s.responses = s.responses[1:]
	return response, nil
}

func hello(clock time.Time) workerproto.WorkerHello {
	return workerproto.WorkerHello{
		ID: "worker-a", WorkspaceRef: "workspace-main", ProtocolVersion: "1", BuildVersion: "coop-test",
		ClockAt: clock, SandboxDigest: repeatedDigest("a"), PolicyDigests: map[string]string{"work-read-only": repeatedDigest("b")},
		Repositories: []workerproto.Repository{}, Capabilities: []workerproto.Capability{},
		Capacity: workerproto.Capacity{SessionSlotsFree: 1, SessionSlotsTotal: 1, TurnSlotsFree: 1, TurnSlotsTotal: 1, WorkspaceSlotsFree: 1, WorkspaceSlotsTotal: 1, State: "eligible"},
		State:    "eligible",
	}
}

type fakeAPI struct {
	requests          []Request
	response          json.RawMessage
	err               error
	outputArtifact    Artifact
	outputErr         error
	outputFetches     []string
	reviewPatch       []byte
	reviewErr         error
	reviewFetches     []string
	checkpointBundle  []byte
	checkpointErr     error
	checkpointFetches []string
	restoreResponse   json.RawMessage
	restoreErr        error
	restores          []fakeRestoreCall
}

type fakeRestoreCall struct {
	sessionID  string
	key        string
	revision   int
	checkpoint workerproto.WorkspaceCheckpoint
	bundle     []byte
}

func (f *fakeAPI) Do(_ context.Context, request Request) (json.RawMessage, error) {
	f.requests = append(f.requests, request)
	return f.response, f.err
}

func (f *fakeAPI) FetchOutputArtifact(_ context.Context, sessionID, turnID, artifactID string) (Artifact, error) {
	f.outputFetches = append(f.outputFetches, sessionID+"/"+turnID+"/"+artifactID)
	return f.outputArtifact, f.outputErr
}

func (f *fakeAPI) FetchReviewPatch(_ context.Context, artifactID, expectedSHA256 string, _ int64) ([]byte, error) {
	f.reviewFetches = append(f.reviewFetches, artifactID+"/"+expectedSHA256)
	return f.reviewPatch, f.reviewErr
}

func (f *fakeAPI) FetchWorkspaceCheckpointBundle(_ context.Context, operationID string, checkpoint workerproto.WorkspaceCheckpoint) ([]byte, error) {
	f.checkpointFetches = append(f.checkpointFetches, operationID+"/"+checkpoint.CheckpointRef)
	return f.checkpointBundle, f.checkpointErr
}

func (f *fakeAPI) RestoreWorkspaceCheckpoint(
	_ context.Context,
	sessionID, key string,
	revision int,
	checkpoint workerproto.WorkspaceCheckpoint,
	bundle []byte,
) (json.RawMessage, error) {
	f.restores = append(f.restores, fakeRestoreCall{
		sessionID: sessionID, key: key, revision: revision, checkpoint: checkpoint,
		bundle: append([]byte(nil), bundle...),
	})
	return f.restoreResponse, f.restoreErr
}

type fakeArtifactTransport struct {
	inputs                   map[string]Artifact
	fetches                  []string
	uploads                  []Artifact
	uploadResponse           []byte
	reviewUploadResponse     []byte
	reviewUploads            [][]byte
	checkpointUploadResponse []byte
	checkpointUploads        []workerproto.WorkspaceCheckpoint
	checkpointBundles        [][]byte
	err                      error
	checkpointFetch          workerproto.WorkspaceCheckpoint
	checkpointFetchBundle    []byte
	checkpointFetches        []string
}

func (f *fakeArtifactTransport) FetchInputArtifact(_ context.Context, commandID, artifactRef string) (Artifact, error) {
	f.fetches = append(f.fetches, commandID+"/"+artifactRef)
	return f.inputs[artifactRef], f.err
}

func (f *fakeArtifactTransport) UploadOutputArtifact(_ context.Context, _ string, artifact Artifact) ([]byte, error) {
	f.uploads = append(f.uploads, artifact)
	return f.uploadResponse, f.err
}

func (f *fakeArtifactTransport) UploadReviewPatch(_ context.Context, _ string, _ string, _ string, patch []byte) ([]byte, error) {
	f.reviewUploads = append(f.reviewUploads, append([]byte(nil), patch...))
	return f.reviewUploadResponse, f.err
}

func (f *fakeArtifactTransport) UploadWorkspaceCheckpoint(_ context.Context, _ string, checkpoint workerproto.WorkspaceCheckpoint, bundle []byte) ([]byte, error) {
	f.checkpointUploads = append(f.checkpointUploads, checkpoint)
	f.checkpointBundles = append(f.checkpointBundles, append([]byte(nil), bundle...))
	return f.checkpointUploadResponse, f.err
}

func (f *fakeArtifactTransport) FetchWorkspaceCheckpoint(_ context.Context, commandID, transferID string) (workerproto.WorkspaceCheckpoint, []byte, error) {
	f.checkpointFetches = append(f.checkpointFetches, commandID+"/"+transferID)
	return f.checkpointFetch, append([]byte(nil), f.checkpointFetchBundle...), f.err
}

func testWorkspaceCheckpoint(t *testing.T, now time.Time) (workerproto.WorkspaceCheckpoint, []byte) {
	return testWorkspaceCheckpointWithTask(t, now, []byte("Status: in_progress\n"))
}

func testWorkspaceCheckpointWithTask(
	t *testing.T,
	now time.Time,
	task []byte,
) (workerproto.WorkspaceCheckpoint, []byte) {
	t.Helper()
	patch := []byte{}
	base := strings.Repeat("1", 40)
	committed := strings.Repeat("2", 40)
	candidate := repeatedDigest("3")
	checkpointRef := "checkpoint:" + strings.Repeat("4", 32)
	manifest := workerproto.WorkspaceCheckpointBundleManifest{
		Version: workerproto.WorkspaceCheckpointVersion, CheckpointRef: checkpointRef,
		RepositoryRef: "responder", BaseRevision: base, BranchRef: "main",
		CommittedRevision: committed, CandidateTreeSHA256: candidate,
		TrackedPatch: workerproto.WorkspaceCheckpointBundleEntry{Entry: "workspace.patch", SHA256: digestBytes(patch), ByteSize: 0},
		TaskProjection: workerproto.WorkspaceCheckpointTaskProjection{
			QueueID: strings.Repeat("5", 32), TaskID: strings.Repeat("6", 32), ID: "remote-worker-checkpoint",
			State: "in_progress", StateSHA256: digestBytes(task),
			Files: []workerproto.WorkspaceCheckpointFileEntry{{
				PathB64: "c3RhdGUubWQ=", Entry: "task/000000", Mode: 0o644,
				SHA256: digestBytes(task), ByteSize: int64(len(task)),
			}},
		},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for _, member := range []struct {
		name string
		body []byte
	}{{"manifest.json", manifestBytes}, {"workspace.patch", patch}, {"task/000000", task}} {
		header := &tar.Header{Name: member.name, Mode: 0o644, Size: int64(len(member.body)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR, ModTime: time.Unix(0, 0).UTC()}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(member.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	bundle := output.Bytes()
	checkpoint := workerproto.WorkspaceCheckpoint{
		Version: workerproto.WorkspaceCheckpointVersion, CheckpointRef: checkpointRef,
		SessionRef: "018f04f4-2222-7000-8000-000000000001", PlacementGeneration: 1,
		RepositoryRef: "responder", BaseRevision: base, BranchRef: "main",
		CommittedRevision: committed, CandidateTreeSHA256: candidate,
		Task: workerproto.WorkspaceCheckpointTask{
			QueueID: manifest.TaskProjection.QueueID, TaskID: manifest.TaskProjection.TaskID,
			ID: manifest.TaskProjection.ID, State: "in_progress", Subtasks: []bool{false}, StateSHA256: digestBytes(task),
		},
		Gate: workerproto.WorkspaceCheckpointGate{Status: "not_run"},
		Bundle: workerproto.WorkspaceCheckpointBundle{
			MediaType: workerproto.WorkspaceCheckpointBundleMediaType, SHA256: digestBytes(bundle), ByteSize: int64(len(bundle)),
		},
		CreatedAt: now,
	}
	if _, err := workerproto.ValidateWorkspaceCheckpointBundle(checkpoint, bundle); err != nil {
		t.Fatal(err)
	}
	return checkpoint, bundle
}

func createCommand(expires time.Time) workerproto.Command {
	return workerproto.Command{
		CommandID: "018f04f4-1111-7000-8000-000000000001", WorkerID: "worker-a",
		SessionRef: "018f04f4-2222-7000-8000-000000000001", PlacementGeneration: 1,
		LeaseRef: "placement-lease:1", LeaseExpiresAt: expires, Kind: "create_session",
		CommandVersion: 1,
		Payload:        json.RawMessage(`{"external_ref":"episode-1","policy":"work-read-only","policy_digest":"` + repeatedDigest("b") + `"}`),
		IdempotencyKey: "responder:work:create:session-1:g1",
	}
}

func repeatedDigest(character string) string {
	value := ""
	for len(value) < 64 {
		value += character
	}
	return value
}

func canonicalDigest(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalJSON(encoded)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
