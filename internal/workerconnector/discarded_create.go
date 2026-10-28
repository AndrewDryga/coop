package workerconnector

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// A discarded session is terminal evidence, not permission to rebind the old
// generation. An absent session is not equivalent: missing custody stays held.
func (e *Executor) proveDiscardedCreate(ctx context.Context, entry journalEntry, before createOrigin, operation createOperation) error {
	if operation.ResourceType != "session" || !reference(operation.ResourceID, 1024) {
		return errors.New("acknowledged create has no exact session resource")
	}
	job, err := discardedCreateJob(entry)
	if err != nil {
		return err
	}
	query := url.Values{"key": {before.OperationKey}}
	resource, err := e.api.Do(ctx, Request{Method: "GET", Path: "/v1/operations?" + query.Encode()})
	if err != nil {
		return err
	}
	var original createOperation
	if len(resource) > maxPrivateResponseBytes || json.Unmarshal(resource, &original) != nil ||
		original.ID != before.OperationID || original.Method != "CreateRemoteSession" ||
		original.State != "failed" || original.ResourceType != "" || original.ResourceID != "" {
		return errors.New("original create has no exact resource-free failure proof")
	}
	resource, err = e.api.Do(ctx, Request{Method: "GET", Path: "/v1/sessions/" + url.PathEscape(operation.ResourceID)})
	if err != nil {
		return err
	}
	var remote struct {
		ID                string `json:"id"`
		ExternalRef       string `json:"external_ref"`
		JobRef            string `json:"job_ref"`
		JobDigest         string `json:"job_digest"`
		State             string `json:"state"`
		Activity          string `json:"activity"`
		ActiveTurnID      string `json:"active_turn_id"`
		QueuedTurnCount   *int   `json:"queued_turn_count"`
		QueuedPromptBytes *int64 `json:"queued_prompt_bytes"`
	}
	digest, err := job.Digest()
	if err != nil {
		return err
	}
	if len(resource) > maxPrivateResponseBytes || json.Unmarshal(resource, &remote) != nil ||
		remote.ID != operation.ResourceID || remote.ExternalRef != job.JobRef ||
		remote.JobRef != job.JobRef || remote.JobDigest != digest || remote.State != "discarded" ||
		remote.Activity != "parked" || remote.ActiveTurnID != "" ||
		remote.QueuedTurnCount == nil || *remote.QueuedTurnCount != 0 ||
		remote.QueuedPromptBytes == nil || *remote.QueuedPromptBytes != 0 {
		return errors.New("acknowledged create has no exact discarded session proof")
	}
	return nil
}

func discardedCreateJob(entry journalEntry) (workerproto.JobSpec, error) {
	request, err := workerproto.DecodeAPIRequest(entry.Command.Payload)
	if err != nil {
		return workerproto.JobSpec{}, err
	}
	var body struct {
		Task              string          `json:"task"`
		Job               json.RawMessage `json:"job"`
		ExpectedJobDigest string          `json:"expected_job_digest"`
	}
	if err := json.Unmarshal(request.Body, &body); err != nil {
		return workerproto.JobSpec{}, err
	}
	job, err := workerproto.DecodeJobSpec(body.Job)
	if err != nil {
		return workerproto.JobSpec{}, err
	}
	digest, err := job.Digest()
	if err != nil || body.Task != job.JobRef || body.ExpectedJobDigest != digest {
		return workerproto.JobSpec{}, errors.New("acknowledged create job identity is not exact")
	}
	return job, nil
}
