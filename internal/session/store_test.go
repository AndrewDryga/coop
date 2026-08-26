package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenProtectsRootDatabaseAndRejectsUnsafeRoots(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store := openTestStore(t, root)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	assertMode(t, root, 0o700)
	assertMode(t, filepath.Join(root, databaseName), 0o600)
	store = openTestStore(t, root)
	defer store.Close()
	var foreignKeys int
	if err := store.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, err=%v", foreignKeys, err)
	}
	var journalMode string
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil || journalMode != "wal" {
		t.Fatalf("journal_mode = %q, err=%v", journalMode, err)
	}
	var timeout int
	if err := store.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != busyTimeout {
		t.Fatalf("busy_timeout = %d, err=%v", timeout, err)
	}

	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); err == nil {
		t.Fatal("Open accepted a symlink state root")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file); err == nil {
		t.Fatal("Open accepted a non-directory state root")
	}
}

func TestOpenExclusivelyOwnsStateRootLockAndReopensAfterClose(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store := openTestStore(t, root)
	assertMode(t, filepath.Join(root, lockName), 0o600)
	if _, err := Open(root); err == nil || !strings.Contains(err.Error(), "another session daemon owns this state root") {
		t.Fatalf("second state-root open error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, root)
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	unsafeRoot := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(unsafeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(unsafeRoot, lockName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(unsafeRoot); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory lock error = %v", err)
	}

	linkRoot := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(linkRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(linkRoot, lockName)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(linkRoot); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink lock error = %v", err)
	}
}

func TestPersistenceAndOperationReplayBeforeRevisionValidation(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	store := openTestStore(t, root)
	sess, err := store.CreateSession(ctx, "create-1", CreateSessionRequest{Target: "codex:model"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.SubmitTurn(ctx, "turn-1", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "durable prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestStore(t, root)
	defer store.Close()
	gotSession, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotSession.Target != sess.Target || gotSession.LastEventSequence != 2 {
		t.Fatalf("reopened session = %+v", gotSession)
	}
	gotTurn, err := store.GetTurn(ctx, sess.ID, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTurn.Prompt != "durable prompt" || gotTurn.Ordinal != 1 {
		t.Fatalf("reopened turn = %+v", gotTurn)
	}

	// Cancellation advances the control revision. The exact retry must replay before
	// checking that stale revision, and must not create a second turn.
	if _, err := store.CancelTurn(ctx, "cancel-1", CancelTurnRequest{SessionID: sess.ID, TurnID: turn.ID, ExpectedRevision: sess.Revision}); err != nil {
		t.Fatal(err)
	}
	replayed, err := store.SubmitTurn(ctx, "turn-1", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "durable prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != turn.ID {
		t.Fatalf("replayed turn ID = %q, want %q", replayed.ID, turn.ID)
	}
	if op, err := store.GetOperation(ctx, "turn-1"); err != nil || op.State != OperationSucceeded || op.ResourceID != turn.ID {
		t.Fatalf("turn operation = %+v, %v", op, err)
	}
}

func TestTurnArtifactsAreDurableBoundAndRemovedAfterCompletion(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	store := openTestStore(t, root)
	sess, err := store.CreateSession(ctx, "artifact-session", CreateSessionRequest{Target: "codex:model"})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("\x89PNG\r\n\x1a\nartifact")
	digest := sha256.Sum256(data)
	artifact := InputArtifact{
		Name: "bug.png", MediaType: "image/png",
		SHA256: hex.EncodeToString(digest[:]), Data: data,
	}
	turn, err := store.SubmitTurn(ctx, "artifact-turn", SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "inspect",
		Artifacts: []InputArtifact{artifact},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Artifacts) != 0 {
		t.Fatal("admission response exposed artifact bytes")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, root)
	defer store.Close()
	leased, ok, err := store.LeaseNextTurn(ctx, sess.ID)
	if err != nil || !ok || len(leased.Artifacts) != 1 {
		t.Fatalf("durable artifact lease = %+v, ok=%v, err=%v", leased.Artifacts, ok, err)
	}
	if string(leased.Artifacts[0].Data) != string(data) {
		t.Fatalf("leased artifact data = %q", leased.Artifacts[0].Data)
	}
	if _, err := store.MarkTurnSendIntent(ctx, sess.ID, leased.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkTurnSent(ctx, sess.ID, leased.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteTurn(ctx, CompleteTurnRequest{
		SessionID: sess.ID, TurnID: leased.ID, Message: "done",
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM turn_artifacts WHERE turn_id = ?`, leased.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("terminal artifact rows = %d, want 0", count)
	}
}

func TestTurnOutputContractIsDurableAndRejectsTheWrongSchemaDigest(t *testing.T) {
	// A malformed Responder result previously reached the caller because its schema
	// was only an input artifact. The completion boundary needs the contract after a
	// daemon restart, not merely while admitting the HTTP request.
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	store := openTestStore(t, root)
	sess, err := store.CreateSession(ctx, "contract-session", CreateSessionRequest{Target: "codex:model"})
	if err != nil {
		t.Fatal(err)
	}
	schema := json.RawMessage(`{"type":"object","properties":{"reply":{"type":"string"}},"required":["reply"],"additionalProperties":false}`)
	digest := sha256.Sum256(schema)
	contract := &OutputContract{JSONSchema: schema, SHA256: hex.EncodeToString(digest[:])}
	turn, err := store.SubmitTurn(ctx, "contract-turn", SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "answer",
		OutputContract: contract,
	})
	if err != nil {
		t.Fatal(err)
	}
	if turn.OutputContract == nil || turn.OutputContract.SHA256 != contract.SHA256 {
		t.Fatalf("admitted output contract = %+v", turn.OutputContract)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, root)
	defer store.Close()
	leased, ok, err := store.LeaseNextTurn(ctx, sess.ID)
	if err != nil || !ok || leased.OutputContract == nil {
		t.Fatalf("durable output contract lease = %+v, ok=%v, err=%v", leased.OutputContract, ok, err)
	}
	if got := string(leased.OutputContract.JSONSchema); got != string(schema) || leased.OutputContract.SHA256 != contract.SHA256 {
		t.Fatalf("leased output contract = %+v", leased.OutputContract)
	}

	bad := *contract
	bad.SHA256 = strings.Repeat("0", 64)
	if _, err := store.SubmitTurn(ctx, "bad-contract-turn", SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "answer",
		OutputContract: &bad,
	}); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("wrong output-contract digest error = %v", err)
	}
}

func TestSemanticCandidateMustBeAcceptedByDigestBeforeTheTurnCompletes(t *testing.T) {
	// A schema-valid Responder result could still contradict the frozen episode.
	// Once Coop completed it, the caller could only bolt on another correction
	// turn. The candidate must stay unpublished and retryable until the caller
	// accepts these exact bytes.
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	store := openTestStore(t, root)
	sess, err := store.CreateSession(ctx, "semantic-session", CreateSessionRequest{
		Target: "codex:model", MaxTurns: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	schema := json.RawMessage(`{"type":"object","properties":{"reply":{"type":"string"}},"required":["reply"],"additionalProperties":false}`)
	digest := sha256.Sum256(schema)
	turn, err := store.SubmitTurn(ctx, "semantic-turn", SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "answer",
		OutputContract: &OutputContract{
			JSONSchema: schema, SHA256: hex.EncodeToString(digest[:]),
			RequireSemanticValidation: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, ok, err := store.LeaseNextTurn(ctx, sess.ID)
	if err != nil || !ok {
		t.Fatalf("lease = %+v, ok=%v, err=%v", turn, ok, err)
	}
	if _, err := store.MarkTurnSendIntent(ctx, sess.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkTurnSent(ctx, sess.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteTurn(ctx, CompleteTurnRequest{
		SessionID: sess.ID, TurnID: turn.ID, Message: `{"reply":"unsafe"}`,
	}); CodeOf(err) != CodeTurnNotRunnable {
		t.Fatalf("unaccepted semantic completion error = %v", err)
	}

	candidateBytes := `{"reply":"safe"}`
	candidateDigest := sha256.Sum256([]byte(candidateBytes))
	staged, err := store.StageTurnCandidate(ctx, StageTurnCandidateRequest{
		SessionID: sess.ID, TurnID: turn.ID, Message: candidateBytes,
		SHA256: hex.EncodeToString(candidateDigest[:]), Attempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if staged.State != TurnAwaitingValidation || staged.AssistantMessage != "" ||
		staged.Candidate == nil || staged.Candidate.Message != candidateBytes {
		t.Fatalf("staged semantic candidate = %+v", staged)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, root)
	defer store.Close()
	staged, err = store.GetTurn(ctx, sess.ID, turn.ID)
	if err != nil || staged.State != TurnAwaitingValidation || staged.Candidate == nil ||
		staged.Candidate.Message != candidateBytes {
		t.Fatalf("recovered semantic candidate = %+v, err=%v", staged, err)
	}
	cleanupTurns, err := store.ListRuntimeCleanupTurns(ctx)
	if err != nil || len(cleanupTurns) != 1 || cleanupTurns[0].ID != turn.ID ||
		cleanupTurns[0].State != TurnAwaitingValidation {
		t.Fatalf("runtime cleanup turns = %+v, err=%v", cleanupTurns, err)
	}
	if _, err := store.CompleteTurn(ctx, CompleteTurnRequest{
		SessionID: sess.ID, TurnID: turn.ID,
		CandidateSHA256: strings.Repeat("0", 64),
	}); CodeOf(err) != CodeRevisionConflict {
		t.Fatalf("stale candidate acceptance error = %v", err)
	}
	completed, err := store.CompleteTurn(ctx, CompleteTurnRequest{
		SessionID: sess.ID, TurnID: turn.ID,
		CandidateSHA256: staged.Candidate.SHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != TurnCompleted || completed.AssistantMessage != candidateBytes ||
		completed.ValidationReceipt == "" {
		t.Fatalf("accepted semantic turn = %+v", completed)
	}
	if cleanupTurns, err := store.ListRuntimeCleanupTurns(ctx); err != nil || len(cleanupTurns) != 0 {
		t.Fatalf("runtime cleanup turns after acceptance = %+v, err=%v", cleanupTurns, err)
	}
}

func TestRejectedSemanticCandidateRequeuesTheSameLogicalTurn(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "semantic-repair-session", CreateSessionRequest{Target: "codex:model"})
	if err != nil {
		t.Fatal(err)
	}
	schema := json.RawMessage(`{"type":"object"}`)
	digest := sha256.Sum256(schema)
	admitted, err := store.SubmitTurn(ctx, "semantic-repair-turn", SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "answer",
		OutputContract: &OutputContract{
			JSONSchema: schema, SHA256: hex.EncodeToString(digest[:]),
			RequireSemanticValidation: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	leased, ok, err := store.LeaseNextTurn(ctx, sess.ID)
	if err != nil || !ok {
		t.Fatalf("lease = %+v, ok=%v, err=%v", leased, ok, err)
	}
	if _, err := store.MarkTurnSendIntent(ctx, sess.ID, leased.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkTurnSent(ctx, sess.ID, leased.ID); err != nil {
		t.Fatal(err)
	}
	message := `{"reply":"healthy"}`
	messageDigest := sha256.Sum256([]byte(message))
	staged, err := store.StageTurnCandidate(ctx, StageTurnCandidateRequest{
		SessionID: sess.ID, TurnID: leased.ID, Message: message,
		SHA256: hex.EncodeToString(messageDigest[:]), Attempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	requeued, err := store.RejectTurnCandidate(ctx, RejectTurnCandidateRequest{
		SessionID: sess.ID, TurnID: leased.ID,
		CandidateSHA256: staged.Candidate.SHA256,
		Violations:      []string{"completion.status: contradicts current OOM evidence"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if requeued.ID != admitted.ID || requeued.State != TurnQueued || requeued.Candidate != nil ||
		requeued.ValidationAttempt != 1 || !strings.Contains(requeued.ValidationError, "contradicts current OOM evidence") {
		t.Fatalf("requeued semantic turn = %+v", requeued)
	}
	leasedAgain, ok, err := store.LeaseNextTurn(ctx, sess.ID)
	if err != nil || !ok || leasedAgain.ID != admitted.ID || leasedAgain.ValidationAttempt != 1 {
		t.Fatalf("re-leased semantic turn = %+v, ok=%v, err=%v", leasedAgain, ok, err)
	}
}

func TestThirdRejectedSemanticCandidateFailsWithoutPublishingItsMessage(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "semantic-exhaustion-session", CreateSessionRequest{Target: "codex:model"})
	if err != nil {
		t.Fatal(err)
	}
	schema := json.RawMessage(`{"type":"object"}`)
	digest := sha256.Sum256(schema)
	_, err = store.SubmitTurn(ctx, "semantic-exhaustion-turn", SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "answer",
		OutputContract: &OutputContract{
			JSONSchema: schema, SHA256: hex.EncodeToString(digest[:]),
			RequireSemanticValidation: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, ok, err := store.LeaseNextTurn(ctx, sess.ID)
	if err != nil || !ok {
		t.Fatalf("lease = %+v, ok=%v, err=%v", turn, ok, err)
	}
	if _, err := store.MarkTurnSendIntent(ctx, sess.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkTurnSent(ctx, sess.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	message := `{"reply":"still unsafe"}`
	messageDigest := sha256.Sum256([]byte(message))
	staged, err := store.StageTurnCandidate(ctx, StageTurnCandidateRequest{
		SessionID: sess.ID, TurnID: turn.ID, Message: message,
		SHA256: hex.EncodeToString(messageDigest[:]), Attempt: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := store.RejectTurnCandidate(ctx, RejectTurnCandidateRequest{
		SessionID: sess.ID, TurnID: turn.ID, CandidateSHA256: staged.Candidate.SHA256,
		Violations: []string{"unsupported healthy completion"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != TurnFailed || failed.ErrorCode != CodeOutputContractFailed ||
		failed.AssistantMessage != "" || failed.Candidate != nil {
		t.Fatalf("exhausted semantic turn = %+v", failed)
	}
}

func TestAResponderTurnKeepsFourCustomerArtifactsAndItsContract(t *testing.T) {
	// Responder already accepts four customer files. Its result schema is a
	// required fifth input, so the transport must keep all five and still
	// reject an unbounded sixth.
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "five-artifact-session", CreateSessionRequest{Target: "codex:model"})
	if err != nil {
		t.Fatal(err)
	}
	artifacts := make([]InputArtifact, 0, 6)
	for index := 0; index < 6; index++ {
		data := []byte(fmt.Sprintf("artifact-%d", index))
		digest := sha256.Sum256(data)
		artifacts = append(artifacts, InputArtifact{
			Name: fmt.Sprintf("artifact-%d.txt", index), MediaType: "text/plain",
			SHA256: hex.EncodeToString(digest[:]), Data: data,
		})
	}
	if _, err := store.SubmitTurn(ctx, "five-artifacts", SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "inspect",
		Artifacts: artifacts[:5],
	}); err != nil {
		t.Fatalf("five bounded input artifacts were rejected: %v", err)
	}
	if _, err := store.SubmitTurn(ctx, "six-artifacts", SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "inspect",
		Artifacts: artifacts,
	}); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("six input artifacts error = %v", err)
	}
}

func TestCompletedTurnOutputArtifactExposesMetadataAndExactBytes(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "output-session", CreateSessionRequest{Target: "codex:model"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.SubmitTurn(ctx, "output-turn", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "draw"})
	if err != nil {
		t.Fatal(err)
	}
	leased, ok, err := store.LeaseNextTurn(ctx, sess.ID)
	if err != nil || !ok || leased.ID != turn.ID {
		t.Fatalf("lease = %+v ok=%v err=%v", leased, ok, err)
	}
	if _, err := store.MarkTurnSendIntent(ctx, sess.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkTurnSent(ctx, sess.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	data := []byte("\x89PNG\r\n\x1a\nvisual")
	digest := sha256.Sum256(data)
	artifact := OutputArtifact{
		ID: "artifact_123", Name: "load.png", MediaType: "image/png",
		SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(data)), Data: data,
	}
	completed, err := store.CompleteTurn(ctx, CompleteTurnRequest{SessionID: sess.ID, TurnID: turn.ID, Message: "done", Artifacts: []OutputArtifact{artifact}})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed.OutputArtifacts) != 1 || len(completed.OutputArtifacts[0].Data) != 0 {
		t.Fatalf("completion exposed artifact data: %+v", completed.OutputArtifacts)
	}
	loaded, err := store.GetTurn(ctx, sess.ID, turn.ID)
	if err != nil || len(loaded.OutputArtifacts) != 1 || len(loaded.OutputArtifacts[0].Data) != 0 {
		t.Fatalf("loaded metadata = %+v err=%v", loaded.OutputArtifacts, err)
	}
	got, err := store.GetOutputArtifact(ctx, sess.ID, turn.ID, artifact.ID)
	if err != nil || string(got.Data) != string(data) || got.SHA256 != artifact.SHA256 {
		t.Fatalf("artifact = %+v err=%v", got, err)
	}
}

func TestTurnArtifactValidationAndIdempotencyBinding(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "artifact-validation", CreateSessionRequest{Target: "target"})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("hello")
	digest := sha256.Sum256(data)
	valid := InputArtifact{
		Name: "notes.txt", MediaType: "text/plain",
		SHA256: hex.EncodeToString(digest[:]), Data: data,
	}
	req := SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "inspect",
		Artifacts: []InputArtifact{valid},
	}
	first, err := store.SubmitTurn(ctx, "artifact-idempotency", req)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.SubmitTurn(ctx, "artifact-idempotency", req)
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("artifact replay = %+v, err=%v", replayed, err)
	}
	changed := req
	changed.Artifacts = append([]InputArtifact(nil), req.Artifacts...)
	changed.Artifacts[0].Data = []byte("other")
	if _, err := store.SubmitTurn(ctx, "artifact-idempotency", changed); CodeOf(err) != CodeIdempotencyConflict {
		t.Fatalf("changed artifact replay error = %v", err)
	}
	bad := req
	bad.Artifacts = []InputArtifact{{
		Name: "../escape.png", MediaType: "image/png",
		SHA256: valid.SHA256, Data: data,
	}}
	if _, err := store.SubmitTurn(ctx, "bad-artifact", bad); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("invalid artifact error = %v", err)
	}
}

// The escalation floor is read by the runner when the turn is LEASED, which can
// be after a controller restart, so it has to come back off disk with the turn
// rather than living in the admitting process. A floor that survives only in
// memory would silently re-deliver a corrected turn on the rung the caller
// escalated past.
func TestAnEscalationFloorSurvivesTheStoreItWasAdmittedThrough(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	store := openTestStore(t, root)
	sess, err := store.CreateSession(ctx, "floor-session", CreateSessionRequest{Target: "codex:model"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.SubmitTurn(ctx, "floor-turn", SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "re-deliver higher", MinTargetIndex: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if turn.MinTargetIndex != 2 {
		t.Fatalf("admitted turn floor = %d, want 2", turn.MinTargetIndex)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestStore(t, root)
	defer store.Close()
	leased, ok, err := store.LeaseNextTurn(ctx, sess.ID)
	if err != nil || !ok {
		t.Fatalf("lease turn = %+v, ok=%v, err=%v", leased, ok, err)
	}
	if leased.MinTargetIndex != 2 {
		t.Fatalf("leased turn floor = %d, want the admitted 2", leased.MinTargetIndex)
	}
	if leased.ID != turn.ID {
		t.Fatalf("leased turn = %q, want %q", leased.ID, turn.ID)
	}
}

// Responder cannot recover a floor-one turn from a spent Claude quota by
// submitting an ordinary floor-zero turn: a floor is not a seat assignment,
// and the session remains durably parked on Claude. The explicit rewind must
// survive the asynchronous store boundary so the runner can move that one
// retry back to healthy Codex after a controller restart.
func TestATargetRewindSurvivesTheStoreItWasAdmittedThrough(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	store := openTestStore(t, root)
	sess, err := store.CreateSession(ctx, "rewind-session", CreateSessionRequest{Target: "claude:model"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.SubmitTurn(ctx, "rewind-turn", SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "use the healthy fallback",
		RewindTarget: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !turn.RewindTarget {
		t.Fatal("admitted turn forgot its target rewind")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestStore(t, root)
	defer store.Close()
	leased, ok, err := store.LeaseNextTurn(ctx, sess.ID)
	if err != nil || !ok {
		t.Fatalf("lease turn = %+v, ok=%v, err=%v", leased, ok, err)
	}
	if !leased.RewindTarget {
		t.Fatal("leased turn forgot the persisted target rewind")
	}
}

func TestATargetRewindCannotAlsoDemandAHigherFloor(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "contradictory-target", CreateSessionRequest{Target: "claude:model"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SubmitTurn(ctx, "contradictory-target-turn", SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "go down and up",
		MinTargetIndex: 1, RewindTarget: true,
	})
	if CodeOf(err) != CodeInvalidRequest || !strings.Contains(err.Error(), "rewind_target") {
		t.Fatalf("contradictory target error = %v, want invalid_request naming rewind_target", err)
	}
}

// A negative rung is not a rung. The store owns the shape of the index because
// it is the layer that writes it; the ladder's size is the service's question.
func TestANegativeEscalationFloorIsRefused(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "negative-floor", CreateSessionRequest{Target: "codex:model"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SubmitTurn(ctx, "negative-floor-turn", SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "escalate", MinTargetIndex: -1,
	})
	if CodeOf(err) != CodeInvalidRequest || !strings.Contains(err.Error(), "min_target_index") {
		t.Fatalf("negative floor error = %v, want an invalid_request naming the field", err)
	}
}

// Responder holds idempotency keys across a Coop upgrade: a turn admitted before
// the floor existed, retried after it, must still REPLAY rather than conflict.
// CanonicalRequestHash marshals the request struct, so a new field without
// omitempty would have changed every hash in flight and turned each of those
// retries into a duplicate turn.
func TestATurnWithoutAFloorHashesAsThePreFloorRequestDid(t *testing.T) {
	preFloor := struct {
		SessionID        string
		ExpectedRevision int64
		Prompt           string
		Artifacts        []InputArtifact
	}{SessionID: "sess-1", ExpectedRevision: 3, Prompt: "investigate"}
	want, err := CanonicalRequestHash(preFloor)
	if err != nil {
		t.Fatal(err)
	}
	got, err := CanonicalRequestHash(SubmitTurnRequest{
		SessionID: "sess-1", ExpectedRevision: 3, Prompt: "investigate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("unfloored request hash = %s, want the pre-floor %s", got, want)
	}
	floored, err := CanonicalRequestHash(SubmitTurnRequest{
		SessionID: "sess-1", ExpectedRevision: 3, Prompt: "investigate", MinTargetIndex: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if floored == want {
		t.Fatal("a floored request hashes as an unfloored one; the same key would replay the wrong rung")
	}
	rewound, err := CanonicalRequestHash(SubmitTurnRequest{
		SessionID: "sess-1", ExpectedRevision: 3, Prompt: "investigate", RewindTarget: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rewound == want {
		t.Fatal("a rewound request hashes as an ordinary one; the same key would replay the wrong rung")
	}
}

func TestMarkOperationRunningIsBoundedIdempotentAndRaceSafe(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	op, replay, err := store.ReserveOperation(ctx, "create", "intent-key", map[string]string{"task": "one"})
	if err != nil || replay {
		t.Fatalf("reserve intent operation = %+v, replay=%v, err=%v", op, replay, err)
	}
	intent := []byte(`{"base":"abc","fork":"fork-1"}`)
	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- store.MarkOperationRunning(ctx, op.ID, intent)
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("identical concurrent intent = %v", err)
		}
	}
	got, err := store.GetOperationByID(ctx, op.ID)
	if err != nil || got.State != OperationRunning || string(got.Result) != string(intent) {
		t.Fatalf("running intent = %+v, err=%v", got, err)
	}
	if err := store.MarkOperationRunning(ctx, op.ID, []byte(`{"base":"different"}`)); CodeOf(err) != CodeOperationIntentConflict {
		t.Fatalf("different intent error = %v", err)
	}
	if err := store.CompleteOperation(ctx, op.ID, "session", "s1", []byte("result")); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOperationRunning(ctx, op.ID, intent); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("terminal intent error = %v", err)
	}
	if err := store.MarkOperationRunning(ctx, "missing", intent); CodeOf(err) != CodeOperationNotFound {
		t.Fatalf("unknown intent error = %v", err)
	}
}

func TestReplaceOperationIntentAndListIncompleteOperations(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	first, _, err := store.ReserveOperation(ctx, "CreateRemoteSession", "replace-first", map[string]string{"task": "one"})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := store.ReserveOperation(ctx, "RunReview", "replace-second", map[string]string{"task": "two"})
	if err != nil {
		t.Fatal(err)
	}
	firstIntent := []byte(`{"phase":"admitted"}`)
	secondIntent := []byte(`{"phase":"reviewing"}`)
	if err := store.MarkOperationRunning(ctx, first.ID, firstIntent); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOperationRunning(ctx, second.ID, secondIntent); err != nil {
		t.Fatal(err)
	}
	reserved, _, err := store.ReserveOperation(ctx, "CreateRemoteSession", "reserved-operation", map[string]string{"task": "not started"})
	if err != nil {
		t.Fatal(err)
	}
	pinned := []byte(`{"phase":"pinned","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	if err := store.ReplaceOperationIntent(ctx, first.ID, firstIntent, pinned); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceOperationIntent(ctx, first.ID, firstIntent, []byte(`{"phase":"stale"}`)); CodeOf(err) != CodeOperationIntentConflict {
		t.Fatalf("stale intent replacement error = %v", err)
	}
	running, err := store.ListIncompleteOperations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 3 {
		t.Fatalf("incomplete operations = %+v", running)
	}
	byID := make(map[string]Operation, len(running))
	for _, operation := range running {
		byID[operation.ID] = operation
	}
	if string(byID[first.ID].Result) != string(pinned) ||
		string(byID[second.ID].Result) != string(secondIntent) ||
		byID[reserved.ID].State != OperationReserved {
		t.Fatalf("incomplete operation intents = %+v", byID)
	}
	if err := store.CompleteOperation(ctx, first.ID, "session", "s1", []byte(`{"id":"s1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceOperationIntent(ctx, first.ID, pinned, []byte(`{"phase":"late"}`)); CodeOf(err) != CodeOperationIntentConflict {
		t.Fatalf("terminal intent replacement error = %v", err)
	}
	running, err = store.ListIncompleteOperations(ctx)
	if err != nil || len(running) != 2 {
		t.Fatalf("incomplete operations after completion = %+v, err=%v", running, err)
	}
}

func TestReconcileOperationRejectsAStaleSnapshotWithTheSameClock(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	op, _, err := store.ReserveOperation(ctx, "CreateRemoteSession", "reconcile-cas", map[string]string{"task": "one"})
	if err != nil {
		t.Fatal(err)
	}
	admitted := []byte(`{"phase":"admitted"}`)
	if err := store.MarkOperationRunning(ctx, op.ID, admitted); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.GetOperationByID(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	pinned := []byte(`{"phase":"pinned","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	if err := store.ReplaceOperationIntent(ctx, op.ID, admitted, pinned); err != nil {
		t.Fatal(err)
	}
	changed, err := store.ReconcileOperation(ctx, snapshot, OperationUncertain, CodeOperationUncertain, "stale watchdog")
	if err != nil || changed {
		t.Fatalf("stale reconcile = changed %t, %v", changed, err)
	}
	current, err := store.GetOperationByID(ctx, op.ID)
	if err != nil || current.State != OperationRunning || string(current.Result) != string(pinned) {
		t.Fatalf("current operation = %+v, %v", current, err)
	}
	if err := store.CompleteOperation(ctx, op.ID, "session", "ses_1", []byte(`{"id":"ses_1"}`)); err != nil {
		t.Fatal(err)
	}
	changed, err = store.ReconcileOperation(ctx, current, OperationUncertain, CodeOperationUncertain, "late watchdog")
	if err != nil || changed {
		t.Fatalf("completed reconcile = changed %t, %v", changed, err)
	}
}

func TestMarkOperationUncertainPreservesRunningIntent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	op, replay, err := store.ReserveOperation(ctx, "RunReview", "uncertain-key", map[string]string{"session": "s1"})
	if err != nil || replay {
		t.Fatalf("reserve review operation = %+v, replay=%v, err=%v", op, replay, err)
	}
	intent := []byte(`{"session_id":"s1","parent_head":"abc"}`)
	if err := store.MarkOperationRunning(ctx, op.ID, intent); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOperationUncertain(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetOperationByID(ctx, op.ID)
	if err != nil || got.State != OperationUncertain || string(got.Result) != string(intent) || got.ErrorCode != CodeOperationUncertain {
		t.Fatalf("uncertain operation = %+v, err=%v", got, err)
	}
	if err := store.MarkOperationUncertain(ctx, op.ID); err != nil {
		t.Fatalf("idempotent uncertain transition = %v", err)
	}
	if err := store.CompleteOperation(ctx, op.ID, "review", "s1", []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOperationUncertain(ctx, op.ID); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("terminal uncertain transition = %v", err)
	}
}

func TestFailOperationPreservesRunningIntentAndResourceIdentity(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	req := CreateSessionRequest{Target: "target", PolicyDigest: strings.Repeat("a", 64), TurnTimeout: time.Hour, MaxPatchBytes: DefaultMaxPatchBytes}
	op, replay, err := store.ReserveOperation(ctx, "CreateSession", "failure-preserve", req)
	if err != nil || replay {
		t.Fatalf("reserve operation = %+v, replay=%v, err=%v", op, replay, err)
	}
	intent := []byte(`{"session_id":"session-intent","fork_name":"fork-intent"}`)
	if err := store.MarkOperationRunning(ctx, op.ID, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE operations SET resource_type = ?, resource_id = ? WHERE id = ?`, "fork", "fork-intent", op.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.FailOperation(ctx, op.ID, CodeInternal, "workspace creation failed"); err != nil {
		t.Fatal(err)
	}
	failed, err := store.GetOperationByID(ctx, op.ID)
	if err != nil || failed.State != OperationFailed || string(failed.Result) != string(intent) || failed.ResourceType != "fork" || failed.ResourceID != "fork-intent" {
		t.Fatalf("failed operation lost intent = %+v, err=%v", failed, err)
	}
	if _, err := store.CreateSession(ctx, "failure-preserve", req); CodeOf(err) != CodeInternal {
		t.Fatalf("replayed failure = %v", err)
	}
}

func TestSessionPersistsEffectivePolicyFieldsAndRejectsMalformedReplay(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	store := openTestStore(t, root)
	sess, err := store.CreateSession(ctx, "policy-fields", CreateSessionRequest{
		Target: "target", PolicyDigest: strings.Repeat("a", 64), OmitEnv: true, OmitMCP: true,
		RepositoryReadOnly: true,
		TurnTimeout:        3 * time.Minute, MaxPatchBytes: 1234,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sess.PolicyDigest != strings.Repeat("a", 64) || sess.ProjectEnv || sess.ProjectMCP ||
		!sess.RepositoryReadOnly ||
		sess.TurnTimeout != 3*time.Minute || sess.MaxPatchBytes != 1234 {
		t.Fatalf("created policy fields = %+v", sess)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, root)
	defer store.Close()
	reopened, err := store.GetSession(ctx, sess.ID)
	if err != nil || reopened.PolicyDigest != sess.PolicyDigest || reopened.ProjectEnv || reopened.ProjectMCP ||
		!reopened.RepositoryReadOnly ||
		reopened.TurnTimeout != sess.TurnTimeout || reopened.MaxPatchBytes != sess.MaxPatchBytes {
		t.Fatalf("reopened policy fields = %+v, err=%v", reopened, err)
	}

	badSessionReq := CreateSessionRequest{Target: "target"}
	badSessionOp, _, err := store.ReserveOperation(ctx, "CreateSession", "malformed-session-result", badSessionReq)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteOperation(ctx, badSessionOp.ID, "session", "bad", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSession(ctx, "malformed-session-result", badSessionReq); err == nil {
		t.Fatal("empty session replay unexpectedly succeeded")
	}
	turnReq := SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "prompt"}
	badTurnOp, _, err := store.ReserveOperation(ctx, "SubmitTurn", "malformed-turn-result", turnReq)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteOperation(ctx, badTurnOp.ID, "turn", "bad", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubmitTurn(ctx, "malformed-turn-result", turnReq); err == nil {
		t.Fatal("empty turn replay unexpectedly succeeded")
	}
}

func TestDiscardedSessionIsTerminal(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	sess, err := store.CreateSession(ctx, "create-terminal", CreateSessionRequest{Target: "target"})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := store.CloseSession(ctx, "close-terminal", CloseSessionRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	discarded, err := store.MarkSessionDiscarded(ctx, closed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExtendBudget(ctx, "extend-discarded", ExtendBudgetRequest{
		SessionID: discarded.ID, ExpectedRevision: discarded.Revision, AdditionalTurns: 1,
	}); CodeOf(err) != CodeInvalidSessionState {
		t.Fatalf("extend discarded session = %v", err)
	}
	if _, err := store.ExhaustBudget(ctx, "exhaust-discarded", ExhaustBudgetRequest{
		SessionID: discarded.ID, ExpectedRevision: discarded.Revision,
	}); CodeOf(err) != CodeInvalidSessionState {
		t.Fatalf("exhaust discarded session = %v", err)
	}
	if _, err := store.CloseSession(ctx, "close-discarded", CloseSessionRequest{
		SessionID: discarded.ID, ExpectedRevision: discarded.Revision,
	}); CodeOf(err) != CodeInvalidSessionState {
		t.Fatalf("close discarded session = %v", err)
	}
	if _, ok, err := store.LeaseNextTurn(ctx, discarded.ID); err != nil || ok {
		t.Fatalf("lease discarded session = ok=%v err=%v", ok, err)
	}
	got, err := store.GetSession(ctx, discarded.ID)
	if err != nil || got.State != SessionDiscarded || got.Revision != discarded.Revision {
		t.Fatalf("discard tombstone changed = %+v, err=%v", got, err)
	}
}

func TestOperationKeyConflictsAndTerminalErrorsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	store := openTestStore(t, root)
	op, replay, err := store.ReserveOperation(ctx, "TestMethod", "same-key", map[string]any{"a": 1, "b": "x"})
	if err != nil || replay {
		t.Fatalf("reserve operation = %+v, replay=%v, err=%v", op, replay, err)
	}
	if err := store.CompleteOperation(ctx, op.ID, "thing", "thing-1", []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReserveOperation(ctx, "OtherMethod", "same-key", map[string]any{"a": 1, "b": "x"}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("method conflict = %v", err)
	}
	if _, _, err := store.ReserveOperation(ctx, "TestMethod", "same-key", map[string]any{"a": 2, "b": "x"}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("hash conflict = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, root)
	defer store.Close()
	replayed, replay, err := store.ReserveOperation(ctx, "TestMethod", "same-key", map[string]any{"b": "x", "a": 1})
	if err != nil || !replay || replayed.State != OperationSucceeded || string(replayed.Result) != `{"ok":true}` {
		t.Fatalf("replayed operation = %+v, replay=%v, err=%v", replayed, replay, err)
	}

	if _, err := store.CreateSession(ctx, "bad-create", CreateSessionRequest{}); err == nil {
		t.Fatal("invalid create unexpectedly succeeded")
	}
	if _, err := store.CreateSession(ctx, "bad-create", CreateSessionRequest{}); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("invalid create replay = %v", err)
	}
	failed, err := store.GetOperation(ctx, "bad-create")
	if err != nil || failed.State != OperationFailed || failed.ErrorCode != CodeInvalidRequest {
		t.Fatalf("failed operation = %+v, %v", failed, err)
	}
}

func TestConcurrentAdmissionsGetFIFOOrdinalsAndOneRunningTurn(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "create", CreateSessionRequest{Target: "target", MaxQueuedTurns: 4, MaxQueuedBytes: 100})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	turns := make(chan Turn, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			turn, err := store.SubmitTurn(ctx, fmt.Sprintf("turn-%d", i), SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: fmt.Sprintf("prompt-%d", i)})
			if err != nil {
				errs <- err
				return
			}
			turns <- turn
		}(i)
	}
	wg.Wait()
	close(turns)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var admitted []Turn
	for turn := range turns {
		admitted = append(admitted, turn)
	}
	sort.Slice(admitted, func(i, j int) bool { return admitted[i].Ordinal < admitted[j].Ordinal })
	if len(admitted) != 2 || admitted[0].Ordinal != 1 || admitted[1].Ordinal != 2 {
		t.Fatalf("FIFO ordinals = %+v", admitted)
	}

	leased, ok, err := store.LeaseNextTurn(ctx, sess.ID)
	if err != nil || !ok {
		t.Fatalf("first lease = %+v, ok=%v, err=%v", leased, ok, err)
	}
	if _, ok, err := store.LeaseNextTurn(ctx, sess.ID); err != nil || ok {
		t.Fatalf("second lease while active = ok=%v, err=%v", ok, err)
	}
	if leased.Ordinal != 1 {
		t.Fatalf("leased ordinal = %d", leased.Ordinal)
	}
	if _, err := store.MarkTurnSendIntent(ctx, sess.ID, leased.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkTurnSent(ctx, sess.ID, leased.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteTurn(ctx, CompleteTurnRequest{SessionID: sess.ID, TurnID: leased.ID, Message: "done"}); err != nil {
		t.Fatal(err)
	}
	next, ok, err := store.LeaseNextTurn(ctx, sess.ID)
	if err != nil || !ok || next.Ordinal != 2 {
		t.Fatalf("second FIFO lease = %+v, ok=%v, err=%v", next, ok, err)
	}
}

func TestQueueBoundsQueuedCancellationAndBudgetExhaustion(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "create", CreateSessionRequest{Target: "target", MaxQueuedTurns: 2, MaxQueuedBytes: 5})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.SubmitTurn(ctx, "first", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: 1, Prompt: "12345"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubmitTurn(ctx, "too-large", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: 1, Prompt: "x"}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("queue bytes error = %v", err)
	}
	if _, err := store.CancelTurn(ctx, "cancel", CancelTurnRequest{SessionID: sess.ID, TurnID: first.ID, ExpectedRevision: 1}); err != nil {
		t.Fatal(err)
	}
	second, err := store.SubmitTurn(ctx, "second", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: 2, Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	third, err := store.SubmitTurn(ctx, "third", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: 2, Prompt: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExhaustBudget(ctx, "budget", ExhaustBudgetRequest{SessionID: sess.ID, ExpectedRevision: 2}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{second.ID, third.ID} {
		turn, err := store.GetTurn(ctx, sess.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if turn.State != TurnBudgetExhausted || turn.ErrorCode != CodeBudgetExhausted {
			t.Fatalf("budget turn = %+v", turn)
		}
	}
	got, err := store.GetSession(ctx, sess.ID)
	if err != nil || got.State != SessionExhausted || got.QueuedTurnCount != 0 || got.QueuedPromptBytes != 0 {
		t.Fatalf("exhausted session = %+v, err=%v", got, err)
	}
}

func TestEventOrderingAndCursorReplay(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "create", CreateSessionRequest{Target: "target"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.SubmitTurn(ctx, "turn", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: 1, Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	leased, ok, err := store.LeaseNextTurn(ctx, sess.ID)
	if err != nil || !ok || leased.ID == "" {
		t.Fatalf("lease event turn = %+v, ok=%v, err=%v", leased, ok, err)
	}
	if _, err := store.MarkTurnSendIntent(ctx, sess.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkTurnSent(ctx, sess.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteTurn(ctx, CompleteTurnRequest{SessionID: sess.ID, TurnID: turn.ID, Message: "answer"}); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListEvents(ctx, sess.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []EventType{EventSessionCreated, EventTurnQueued, EventTurnStarted, EventAssistantMessage, EventTurnCompleted}
	if len(events) != len(want) {
		t.Fatalf("events = %+v", events)
	}
	for i, event := range events {
		if event.Sequence != int64(i+1) || event.Type != want[i] {
			t.Fatalf("event %d = %+v, want sequence/type %d/%s", i, event, i+1, want[i])
		}
	}
	partial, err := store.ListEvents(ctx, sess.ID, 2, 2)
	if err != nil || len(partial) != 2 || partial[0].Sequence != 3 || partial[1].Sequence != 4 {
		t.Fatalf("cursor replay = %+v, err=%v", partial, err)
	}
}

func TestSessionBindingPersistenceAndFixedIDValidation(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	store := openTestStore(t, root)
	sess, err := store.CreateSession(ctx, "create-fixed", CreateSessionRequest{
		ID: "session-fixed", Target: "target", Policy: "policy", Repository: "/repo",
		Workspace: "/workspace", ForkName: "fork-fixed", BaseCommit: "0123456789abcdef",
		PullRequest: &PullRequestBinding{
			Number: 514, Ref: "refs/pull/514/head",
			HeadCommit: "0123456789abcdef0123456789abcdef01234567",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != "session-fixed" || sess.Policy != "policy" || sess.Repository != "/repo" || sess.Workspace != "/workspace" || sess.ForkName != "fork-fixed" || sess.BaseCommit != "0123456789abcdef" {
		t.Fatalf("created session binding = %+v", sess)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, root)
	defer store.Close()
	reopened, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Policy != sess.Policy || reopened.Repository != sess.Repository || reopened.Workspace != sess.Workspace || reopened.ForkName != sess.ForkName || reopened.BaseCommit != sess.BaseCommit ||
		reopened.PullRequest == nil || reopened.PullRequest.Number != 514 ||
		reopened.PullRequest.Ref != "refs/pull/514/head" ||
		reopened.PullRequest.HeadCommit != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("reopened session binding = %+v", reopened)
	}
	for name, req := range map[string]CreateSessionRequest{
		"partial binding": {ID: "partial", Target: "target", Policy: "policy"},
		"invalid id":      {ID: "bad\x00id", Target: "target"},
		"long id":         {ID: strings.Repeat("x", MaxIDBytes+1), Target: "target"},
		"invalid pull request ref": {
			ID: "bad-pull-ref", Target: "target", Policy: "policy", Repository: "/repo",
			Workspace: "/workspace-bad", ForkName: "fork-bad", BaseCommit: "base",
			PullRequest: &PullRequestBinding{Number: 514, Ref: "refs/heads/main", HeadCommit: strings.Repeat("a", 40)},
		},
		"invalid pull request head": {
			ID: "bad-pull-head", Target: "target", Policy: "policy", Repository: "/repo",
			Workspace: "/workspace-bad", ForkName: "fork-bad", BaseCommit: "base",
			PullRequest: &PullRequestBinding{Number: 514, Ref: "refs/pull/514/head", HeadCommit: "not-a-commit"},
		},
	} {
		if _, err := store.CreateSession(ctx, "invalid-"+name, req); CodeOf(err) != CodeInvalidRequest {
			t.Fatalf("%s error = %v", name, err)
		}
	}
	if _, err := store.CreateSession(ctx, "duplicate-fixed", CreateSessionRequest{ID: sess.ID, Target: "target"}); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("duplicate fixed ID error = %v", err)
	}
}

func TestLeaseSendCheckpointsAndCompletion(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "create", CreateSessionRequest{Target: "target"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.SubmitTurn(ctx, "turn", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	turn, ok, err := store.LeaseNextTurn(ctx, sess.ID)
	if err != nil || !ok || turn.State != TurnStarting || turn.SendState != SendStateNone {
		t.Fatalf("leased turn = %+v, ok=%v, err=%v", turn, ok, err)
	}
	if got := mustGetSession(t, store, ctx, sess.ID); got.Activity != ActivityStarting {
		t.Fatalf("leased activity = %s", got.Activity)
	}
	if _, err := store.CompleteTurn(ctx, CompleteTurnRequest{SessionID: sess.ID, TurnID: turn.ID}); CodeOf(err) != CodeTurnNotRunnable {
		t.Fatalf("completion before send error = %v", err)
	}
	if _, err := store.MarkTurnSent(ctx, sess.ID, turn.ID); CodeOf(err) != CodeTurnNotRunnable {
		t.Fatalf("sent before intent error = %v", err)
	}
	turn, err = store.MarkTurnSendIntent(ctx, sess.ID, turn.ID)
	if err != nil || turn.SendState != SendStateIntent || turn.State != TurnStarting {
		t.Fatalf("intent checkpoint = %+v, err=%v", turn, err)
	}
	if _, err := store.CompleteTurn(ctx, CompleteTurnRequest{SessionID: sess.ID, TurnID: turn.ID}); CodeOf(err) != CodeTurnNotRunnable {
		t.Fatalf("completion before sent error = %v", err)
	}
	turn, err = store.MarkTurnSent(ctx, sess.ID, turn.ID)
	if err != nil || turn.SendState != SendStateSent || turn.State != TurnRunning {
		t.Fatalf("sent checkpoint = %+v, err=%v", turn, err)
	}
	completed, err := store.CompleteTurn(ctx, CompleteTurnRequest{SessionID: sess.ID, TurnID: turn.ID, Message: "answer"})
	if err != nil || completed.State != TurnCompleted || completed.SendState != SendStateSent {
		t.Fatalf("completed turn = %+v, err=%v", completed, err)
	}
}

func TestCompletedTurnsPersistProviderCostAsCumulativeDeltas(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "cost-session", CreateSessionRequest{Target: "claude:model"})
	if err != nil {
		t.Fatal(err)
	}
	complete := func(key string, cumulative float64) Turn {
		t.Helper()
		turn, err := store.SubmitTurn(ctx, key, SubmitTurnRequest{
			SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		turn, found, err := store.LeaseNextTurn(ctx, sess.ID)
		if err != nil || !found {
			t.Fatalf("lease %s = %+v, found=%t, err=%v", key, turn, found, err)
		}
		if _, err := store.MarkTurnSendIntent(ctx, sess.ID, turn.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.MarkTurnSent(ctx, sess.ID, turn.ID); err != nil {
			t.Fatal(err)
		}
		turn, err = store.CompleteTurn(ctx, CompleteTurnRequest{
			SessionID: sess.ID, TurnID: turn.ID, Message: "done",
			CumulativeCostUSD: cumulative, CostRecorded: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return turn
	}

	first := complete("cost-turn-1", 0.25)
	second := complete("cost-turn-2", 0.40)
	reset := complete("cost-turn-3", 0.10)
	if !first.Usage.CostRecorded || first.Usage.CostUSD != 0.25 {
		t.Fatalf("first cost = %+v, want reported 0.25", first.Usage)
	}
	if !second.Usage.CostRecorded || second.Usage.CostUSD < 0.149999 || second.Usage.CostUSD > 0.150001 {
		t.Fatalf("second cost = %+v, want reported cumulative delta 0.15", second.Usage)
	}
	if !reset.Usage.CostRecorded || reset.Usage.CostUSD != 0.10 {
		t.Fatalf("reset cost = %+v, want new-segment cost 0.10", reset.Usage)
	}
	got, err := store.GetTurn(ctx, sess.ID, second.ID)
	if err != nil || !got.Usage.CostRecorded || got.Usage.CostUSD != second.Usage.CostUSD {
		t.Fatalf("reopened second cost = %+v, err=%v", got.Usage, err)
	}
}

func TestNativeSessionBindingIsImmutableAfterFirstValidID(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "create", CreateSessionRequest{Target: "target"})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.BindNativeSession(ctx, sess.ID, "native-1")
	if err != nil || bound.NativeSessionID != "native-1" || bound.Revision != sess.Revision {
		t.Fatalf("first native binding = %+v, err=%v", bound, err)
	}
	repeat, err := store.BindNativeSession(ctx, sess.ID, "native-1")
	if err != nil || repeat.NativeSessionID != "native-1" || repeat.Revision != sess.Revision {
		t.Fatalf("repeat native binding = %+v, err=%v", repeat, err)
	}
	if _, err := store.BindNativeSession(ctx, sess.ID, "native-2"); CodeOf(err) != CodeNativeSessionConflict {
		t.Fatalf("native conflict error = %v", err)
	}
	if _, err := store.BindNativeSession(ctx, sess.ID, ""); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("empty native ID error = %v", err)
	}
}

func TestReconcileInterruptedTurnsRetainsSendEvidenceAndFIFO(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	makeSession := func(prefix string) (Session, Turn, Turn) {
		t.Helper()
		sess, err := store.CreateSession(ctx, prefix+"-create", CreateSessionRequest{Target: "target"})
		if err != nil {
			t.Fatal(err)
		}
		first, err := store.SubmitTurn(ctx, prefix+"-first", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "first"})
		if err != nil {
			t.Fatal(err)
		}
		second, err := store.SubmitTurn(ctx, prefix+"-second", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "second"})
		if err != nil {
			t.Fatal(err)
		}
		return sess, first, second
	}
	beforeSess, before, _ := makeSession("before")
	if _, ok, err := store.LeaseNextTurn(ctx, beforeSess.ID); err != nil || !ok {
		t.Fatalf("lease before intent = %v, ok=%v", err, ok)
	}
	afterSess, after, afterQueued := makeSession("after")
	if _, ok, err := store.LeaseNextTurn(ctx, afterSess.ID); err != nil || !ok {
		t.Fatalf("lease after intent = %v, ok=%v", err, ok)
	}
	if _, err := store.MarkTurnSendIntent(ctx, afterSess.ID, after.ID); err != nil {
		t.Fatal(err)
	}
	affected, err := store.ReconcileInterruptedTurns(ctx)
	if err != nil || len(affected) != 2 {
		t.Fatalf("interrupted turns = %+v, err=%v", affected, err)
	}
	gotBefore, err := store.GetTurn(ctx, beforeSess.ID, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotBefore.State != TurnQueued || gotBefore.SendState != SendStateNone || gotBefore.ErrorCode != "" || !gotBefore.StartedAt.IsZero() {
		t.Fatalf("before-intent turn = %+v", gotBefore)
	}
	gotAfter, err := store.GetTurn(ctx, afterSess.ID, after.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotAfter.State != TurnInterrupted || gotAfter.SendState != SendStateIntent {
		t.Fatalf("after-intent turn = %+v", gotAfter)
	}
	for _, tc := range []struct {
		sess  Session
		queue int
	}{
		{sess: beforeSess, queue: 2},
		{sess: afterSess, queue: 1},
	} {
		got := mustGetSession(t, store, ctx, tc.sess.ID)
		if got.Activity != ActivityParked || got.ActiveTurnID != "" || got.QueuedTurnCount != tc.queue {
			t.Fatalf("reconciled session = %+v", got)
		}
	}
	nextBefore, ok, err := store.LeaseNextTurn(ctx, beforeSess.ID)
	if err != nil || !ok || nextBefore.ID != before.ID || nextBefore.Ordinal != 1 {
		t.Fatalf("before FIFO after reconcile = %+v, ok=%v, err=%v", nextBefore, ok, err)
	}
	nextAfter, ok, err := store.LeaseNextTurn(ctx, afterSess.ID)
	if err != nil || !ok || nextAfter.ID != afterQueued.ID || nextAfter.Ordinal != 2 {
		t.Fatalf("after FIFO after reconcile = %+v, ok=%v, err=%v", nextAfter, ok, err)
	}
	events, err := store.ListEvents(ctx, afterSess.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if !containsEvent(events, EventTurnInterrupted) || !containsEvent(events, EventSessionParked) {
		t.Fatalf("reconciliation events = %+v", events)
	}
}

func TestFailTurnExhaustsQueuedTurnsAtBudget(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "create", CreateSessionRequest{Target: "target", MaxTurns: 1, MaxQueuedTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.SubmitTurn(ctx, "first", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SubmitTurn(ctx, "second", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "second"})
	if err != nil {
		t.Fatal(err)
	}
	third, err := store.SubmitTurn(ctx, "third", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "third"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.LeaseNextTurn(ctx, sess.ID); err != nil || !ok {
		t.Fatalf("lease failure turn = %v, ok=%v", err, ok)
	}
	failed, err := store.FailTurn(ctx, FailTurnRequest{SessionID: sess.ID, TurnID: first.ID, ErrorCode: CodeInternal, ErrorDetail: "provider stopped"})
	if err != nil || failed.State != TurnFailed || failed.ErrorCode != CodeInternal {
		t.Fatalf("failed turn = %+v, err=%v", failed, err)
	}
	for _, id := range []string{second.ID, third.ID} {
		turn, err := store.GetTurn(ctx, sess.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if turn.State != TurnBudgetExhausted || turn.ErrorCode != CodeBudgetExhausted {
			t.Fatalf("queued failed turn = %+v", turn)
		}
	}
	got := mustGetSession(t, store, ctx, sess.ID)
	if got.State != SessionExhausted || got.Activity != ActivityParked || got.ActiveTurnID != "" || got.TurnsUsed != 1 || got.QueuedTurnCount != 0 {
		t.Fatalf("failed budget session = %+v", got)
	}
}

func TestListTurnsCursorIsBounded(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "create", CreateSessionRequest{Target: "target"})
	if err != nil {
		t.Fatal(err)
	}
	for i, prompt := range []string{"one", "two", "three"} {
		if _, err := store.SubmitTurn(ctx, fmt.Sprintf("turn-%d", i), SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: prompt}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.ListTurns(ctx, sess.ID, 0, 2)
	if err != nil || len(first) != 2 || first[0].Ordinal != 1 || first[1].Ordinal != 2 {
		t.Fatalf("first turn page = %+v, err=%v", first, err)
	}
	second, err := store.ListTurns(ctx, sess.ID, first[len(first)-1].Ordinal, 2)
	if err != nil || len(second) != 1 || second[0].Ordinal != 3 {
		t.Fatalf("second turn page = %+v, err=%v", second, err)
	}
	if turns, err := store.ListTurns(ctx, sess.ID, 3, 2); err != nil || len(turns) != 0 {
		t.Fatalf("empty turn page = %+v, err=%v", turns, err)
	}
	if _, err := store.ListTurns(ctx, sess.ID, -1, 1); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("negative turn cursor error = %v", err)
	}
	if _, err := store.ListTurns(ctx, sess.ID, 0, 1001); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("large turn limit error = %v", err)
	}
}

func TestCloseRequiresEmptyQueueAndReplaysBeforeRevision(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "create", CreateSessionRequest{ID: "close-fixed", Target: "target", Policy: "policy", Repository: "/repo", Workspace: "/workspace", ForkName: "fork", BaseCommit: "base"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.SubmitTurn(ctx, "turn", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	closeReq := CloseSessionRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision}
	if _, err := store.CloseSession(ctx, "close-blocked", closeReq); CodeOf(err) != CodeInvalidSessionState {
		t.Fatalf("close with queued turn error = %v", err)
	}
	if _, err := store.CancelTurn(ctx, "cancel", CancelTurnRequest{SessionID: sess.ID, TurnID: turn.ID, ExpectedRevision: sess.Revision}); err != nil {
		t.Fatal(err)
	}
	closeReq.ExpectedRevision = 2
	closed, err := store.CloseSession(ctx, "close", closeReq)
	if err != nil || closed.State != SessionClosed || closed.Activity != ActivityParked || closed.Revision != 3 {
		t.Fatalf("closed session = %+v, err=%v", closed, err)
	}
	replayed, err := store.CloseSession(ctx, "close", closeReq)
	if err != nil || replayed.Revision != closed.Revision || replayed.Workspace != "/workspace" || replayed.ForkName != "fork" {
		t.Fatalf("replayed close = %+v, err=%v", replayed, err)
	}
	if _, err := store.CloseSession(ctx, "close-again", CloseSessionRequest{SessionID: sess.ID, ExpectedRevision: closed.Revision}); CodeOf(err) != CodeInvalidSessionState {
		t.Fatalf("new close on closed session error = %v", err)
	}
}

func TestExtendBudgetReopensAndReplays(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "create", CreateSessionRequest{Target: "target", MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.SubmitTurn(ctx, "turn", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.LeaseNextTurn(ctx, sess.ID); err != nil || !ok {
		t.Fatalf("lease budget turn = %v, ok=%v", err, ok)
	}
	if _, err := store.MarkTurnSendIntent(ctx, sess.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkTurnSent(ctx, sess.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteTurn(ctx, CompleteTurnRequest{SessionID: sess.ID, TurnID: turn.ID}); err != nil {
		t.Fatal(err)
	}
	got := mustGetSession(t, store, ctx, sess.ID)
	if got.State != SessionExhausted || got.Revision != 2 {
		t.Fatalf("exhausted session before extension = %+v", got)
	}
	req := ExtendBudgetRequest{SessionID: sess.ID, ExpectedRevision: got.Revision, AdditionalTurns: 2}
	extended, err := store.ExtendBudget(ctx, "extend", req)
	if err != nil || extended.State != SessionOpen || extended.MaxTurns != 3 || extended.Revision != 3 {
		t.Fatalf("extended session = %+v, err=%v", extended, err)
	}
	replayed, err := store.ExtendBudget(ctx, "extend", req)
	if err != nil || replayed.Revision != extended.Revision || replayed.MaxTurns != extended.MaxTurns {
		t.Fatalf("replayed extension = %+v, err=%v", replayed, err)
	}
	if next, err := store.SubmitTurn(ctx, "next", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: extended.Revision, Prompt: "after reopen"}); err != nil || next.Ordinal != 2 {
		t.Fatalf("turn after budget extension = %+v, err=%v", next, err)
	}
	if _, err := store.ExtendBudget(ctx, "bad-addition", ExtendBudgetRequest{SessionID: sess.ID, ExpectedRevision: extended.Revision, AdditionalTurns: 0}); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("zero budget extension error = %v", err)
	}
}

func TestReplayedFailureDetailDoesNotDuplicateStableCode(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	sess, err := store.CreateSession(ctx, "create", CreateSessionRequest{Target: "target"})
	if err != nil {
		t.Fatal(err)
	}
	req := SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: 99, Prompt: "bad revision"}
	if _, err := store.SubmitTurn(ctx, "bad-revision", req); CodeOf(err) != CodeRevisionConflict {
		t.Fatalf("first revision error = %v", err)
	}
	replayed, err := store.SubmitTurn(ctx, "bad-revision", req)
	if CodeOf(err) != CodeRevisionConflict || replayed.ID != "" || strings.Count(err.Error(), string(CodeRevisionConflict)) != 1 {
		t.Fatalf("replayed revision error = %v, turn=%+v", err, replayed)
	}
}

func mustGetSession(t *testing.T, store *Store, ctx context.Context, id string) Session {
	t.Helper()
	sess, err := store.GetSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func containsEvent(events []Event, want EventType) bool {
	for _, event := range events {
		if event.Type == want {
			return true
		}
	}
	return false
}

func openTestStore(t *testing.T, root string) *Store {
	t.Helper()
	clock := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	store, err := Open(root,
		WithClock(func() time.Time { return clock }),
		WithIDGenerator(func(prefix string) string {
			return fmt.Sprintf("%s-%03d", prefix, testIDCounter.Add(1))
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

var testIDCounter atomic.Uint64

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func TestRotateTurnTargetSwapsTheRungAndDropsAForeignTranscript(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, t.TempDir())
	defer store.Close()
	sess, err := store.CreateSession(ctx, "rotate-1", CreateSessionRequest{Target: "codex:sol@oncall"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubmitTurn(ctx, "rotate-turn-1", SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "investigate",
	}); err != nil {
		t.Fatal(err)
	}
	leased, ok, err := store.LeaseNextTurn(ctx, sess.ID)
	if err != nil || !ok {
		t.Fatalf("lease turn = %v, %v", ok, err)
	}
	if _, err := store.BindNativeSession(ctx, sess.ID, "native-1"); err != nil {
		t.Fatal(err)
	}
	// Drive the delivery ledger to where a rate limit actually lands: the prompt was handed to
	// the provider, which then refused it.
	if _, err := store.MarkTurnSendIntent(ctx, sess.ID, leased.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkTurnSent(ctx, sess.ID, leased.ID); err != nil {
		t.Fatal(err)
	}

	// Another credential on the same provider still reads the same transcript store.
	same, rewound, err := store.RotateTurnTarget(ctx, sess.ID, leased.ID, "codex:sol@oncall", "codex:sol@backup", false)
	if err != nil {
		t.Fatal(err)
	}
	if same.Target != "codex:sol@backup" || same.NativeSessionID != "native-1" ||
		same.Revision <= sess.Revision || same.Activity != ActivityStarting {
		t.Fatalf("same-provider rotation = %+v", same)
	}
	// The turn is deliverable again, in exactly the state a fresh lease leaves behind.
	if rewound.State != TurnStarting || rewound.SendState != SendStateNone {
		t.Fatalf("rewound turn = %+v", rewound)
	}
	stored, err := store.GetTurn(ctx, sess.ID, leased.ID)
	if err != nil || stored.State != TurnStarting || stored.SendState != SendStateNone {
		t.Fatalf("stored rewound turn = %+v, err=%v", stored, err)
	}
	// Proof the rewind is real and not just cosmetic: the next delivery is accepted.
	if _, err := store.MarkTurnSendIntent(ctx, sess.ID, leased.ID); err != nil {
		t.Fatalf("rotated turn could not be delivered again: %v", err)
	}

	// A cross-provider hop cannot: the id would outlive the store that can resolve it.
	hop, _, err := store.RotateTurnTarget(ctx, sess.ID, leased.ID, "codex:sol@backup", "claude@oncall", true)
	if err != nil {
		t.Fatal(err)
	}
	if hop.Target != "claude@oncall" || hop.NativeSessionID != "" || hop.Revision <= same.Revision {
		t.Fatalf("cross-provider rotation = %+v", hop)
	}

	events, err := store.ListEvents(ctx, sess.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	rotations := make([]string, 0, 2)
	for _, event := range events {
		if event.Type == EventSessionTargetRotated {
			rotations = append(rotations, string(event.Payload))
		}
	}
	// A client re-seeds context only when the transcript actually went away, so the event has
	// to carry that and not leave the client to re-derive it from the target grammar.
	want := []string{
		`{"from":"codex:sol@oncall","native_session_reset":false,"to":"codex:sol@backup"}`,
		`{"from":"codex:sol@backup","native_session_reset":true,"to":"claude@oncall"}`,
	}
	if len(rotations) != len(want) || rotations[0] != want[0] || rotations[1] != want[1] {
		t.Fatalf("rotation events = %q, want %q", rotations, want)
	}

	// Replaying a rotation someone else already applied must not advance the rung again.
	if _, _, err := store.RotateTurnTarget(
		ctx, sess.ID, leased.ID, "codex:sol@backup", "claude@oncall", true,
	); CodeOf(err) != CodeRevisionConflict {
		t.Fatalf("stale rotation error = %v", err)
	}
	if _, _, err := store.RotateTurnTarget(
		ctx, sess.ID, leased.ID, "claude@oncall", "claude@oncall", false,
	); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("self rotation error = %v", err)
	}
	if _, _, err := store.RotateTurnTarget(
		ctx, "missing-session", leased.ID, "codex@oncall", "claude@oncall", false,
	); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("unknown session error = %v", err)
	}
	// Only the in-flight turn may be rewound; a settled one is not a rotation candidate.
	if _, err := store.MarkTurnSendIntent(ctx, sess.ID, leased.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkTurnSent(ctx, sess.ID, leased.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteTurn(ctx, CompleteTurnRequest{
		SessionID: sess.ID, TurnID: leased.ID, Message: "done",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RotateTurnTarget(
		ctx, sess.ID, leased.ID, "claude@oncall", "codex:sol@oncall", true,
	); CodeOf(err) != CodeTurnNotRunnable {
		t.Fatalf("settled turn rotation error = %v", err)
	}
}
