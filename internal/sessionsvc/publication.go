package sessionsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// ReviewPublisher runs only on the trusted host, against a private retained
// repository. It must reconcile remote effects before repeating them.
type ReviewPublisher func(context.Context, string, workerproto.PublishIntent) (workerproto.PublishResult, error)

func (s *Service) PublishReview(ctx context.Context, key, sessionID, reviewID string, req workerproto.PublishRequest) (session.Operation, error) {
	if !validSessionPathComponent(sessionID) || !validSessionPathComponent(reviewID) {
		return session.Operation{}, &session.Error{Code: session.CodeInvalidRequest, Detail: "invalid publication identity"}
	}
	if err := req.Validate(); err != nil {
		return session.Operation{}, &session.Error{Code: session.CodeInvalidRequest, Detail: err.Error()}
	}
	unlock := s.lockOperation(key)
	defer unlock()
	request := struct {
		SessionID string                     `json:"session_id"`
		ReviewID  string                     `json:"review_operation_id"`
		Request   workerproto.PublishRequest `json:"request"`
	}{sessionID, reviewID, req}
	op, replay, err := s.store.ReserveOperation(ctx, "PublishReview", key, request)
	if err != nil {
		return op, err
	}
	if replay && op.State == session.OperationFailed {
		// Replay the saved refusal as every other mutation does. Returning the failed operation
		// with no error answered 202, and the worker connector reports any 2xx as success.
		return op, wrapServiceOperationError(op.ID, &session.Error{Code: op.ErrorCode, Detail: op.ErrorDetail})
	}
	if !replay || op.State == session.OperationReserved {
		// This lock and the durable intent fence discard both before and after
		// the asynchronous worker starts, including the restart gap.
		release := s.lockSessionRuntime(sessionID)
		defer release()
		bound, getErr := s.store.GetSession(ctx, sessionID)
		if getErr != nil {
			return op, s.failServiceOperation(ctx, op.ID, getErr)
		}
		job, jobErr := workerproto.DecodeJobSpec(bound.JobDocument)
		digest, digestErr := job.Digest()
		if jobErr != nil || digestErr != nil || digest != bound.JobDigest || job.JobRef != bound.JobRef ||
			job.Source == nil || job.RepositoryReadOnly || job.Mode != "normal" || bound.State == session.SessionDiscarded {
			return op, s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeInvalidSessionState, Detail: "publication requires a retained writable controller job"})
		}
		intent := workerproto.PublishIntent{SessionID: sessionID, ReviewOperationID: reviewID,
			JobRef: job.JobRef, JobDigest: digest, Repository: job.Source.RepositoryIdentity(), CommandKey: key, Request: req}
		if _, err := s.publicationCandidate(ctx, intent); err != nil {
			return op, s.failServiceOperation(ctx, op.ID, err)
		}
		if s.reviewPublisher == nil {
			return op, s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeInvalidRequest, Detail: "host publication is not configured"})
		}
		data, err := json.Marshal(intent)
		if err != nil {
			return op, err
		}
		if err := s.store.MarkOperationRunning(ctx, op.ID, data); err != nil {
			return op, err
		}
		op, err = s.store.GetOperationByID(ctx, op.ID)
		if err != nil {
			return op, err
		}
	}
	if op.State == session.OperationRunning {
		s.scheduleBackgroundOperation(op.ID)
	}
	return op, nil
}

func (s *Service) publicationCandidate(ctx context.Context, intent workerproto.PublishIntent) (string, error) {
	_, review, err := s.GetReview(ctx, intent.SessionID, intent.ReviewOperationID)
	if err != nil {
		return "", err
	}
	if review.JobDigest != intent.JobDigest || review.CandidateHead != intent.Request.CandidateHead ||
		review.CandidateTree != intent.Request.CandidateTree || !review.CandidateRetained ||
		review.CandidateTree == review.ParentTree || review.Rebase != ReviewRebaseClean || len(review.PolicyFindings) != 0 ||
		review.Gate == ReviewGateFailed {
		return "", &session.Error{Code: session.CodeInvalidRequest, Detail: "review is not an exact shareable candidate"}
	}
	if intent.Request.Mode == "existing_pull_request" &&
		(review.ParentHead != intent.Request.ExpectedHead || review.Source == nil || review.Source.SelectedCommit != intent.Request.ExpectedHead || review.Gate != ReviewGatePassed || !review.Publishable) {
		return "", &session.Error{Code: session.CodeInvalidRequest, Detail: "existing PR candidate is not based on its authorized head"}
	}
	directory, retained, err := s.retainedReviewCandidate(ctx, intent.ReviewOperationID)
	if ctxErr := ctx.Err(); err != nil && ctxErr != nil {
		// A stop or a departed caller killed the check's Git: that proves nothing about custody, so
		// report the cancellation and leave the publication for its next attempt to check again.
		return "", errors.Join(ctxErr, err)
	}
	if err != nil || !reflect.DeepEqual(review, retained) {
		return "", &session.Error{Code: session.CodeInvalidRequest, Detail: "retained review custody does not match"}
	}
	if intent.Request.Mode == "existing_pull_request" {
		ancestor, err := sessionReviewIsAncestor(directory, intent.Request.ExpectedHead, intent.Request.CandidateHead)
		if err != nil || !ancestor {
			return "", &session.Error{Code: session.CodeInvalidRequest, Detail: "existing PR publication must preserve every original commit"}
		}
	}
	return directory, nil
}

// errPublishSessionBusy reports a publication left running because its session's runtime was
// busy; it is not a failure.
var errPublishSessionBusy = errors.New("publication waits for a busy session")

func (s *Service) runPublishOperation(ctx context.Context, operationID string) error {
	op, err := s.store.GetOperationByID(ctx, operationID)
	if err != nil {
		return err
	}
	unlock := s.lockOperation(op.IdempotencyKey)
	defer unlock()
	op, err = s.store.GetOperationByID(ctx, operationID)
	if err != nil || op.State != session.OperationRunning {
		return err
	}
	var intent workerproto.PublishIntent
	if json.Unmarshal(op.Result, &intent) != nil || intent.CommandKey != op.IdempotencyKey ||
		intent.Request.Validate() != nil || intent.Repository.Validate() != nil {
		return s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeInvalidRequest, Detail: "invalid publication intent"})
	}
	// A background slot must not wait out a whole turn: leave the intent running and let the
	// ticker try again once the session's runtime is free.
	release, ok := s.tryLockSessionRuntime(intent.SessionID)
	if !ok {
		return errPublishSessionBusy
	}
	defer release()
	bound, err := s.store.GetSession(ctx, intent.SessionID)
	if err != nil && ctx.Err() != nil {
		return err // the service is stopping; the next start resumes this publication
	}
	if err != nil || bound.State == session.SessionDiscarded || bound.JobDigest != intent.JobDigest || bound.JobRef != intent.JobRef {
		return s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeInvalidSessionState, Detail: "publication job authority changed"})
	}
	job, err := workerproto.DecodeJobSpec(bound.JobDocument)
	digest, digestErr := job.Digest()
	if err != nil || digestErr != nil || digest != intent.JobDigest || job.JobRef != intent.JobRef ||
		job.Source == nil || job.Source.RepositoryIdentity() != intent.Repository || job.RepositoryReadOnly || job.Mode != "normal" {
		return s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeInvalidRequest, Detail: "publication job document changed"})
	}
	directory, err := s.publicationCandidate(ctx, intent)
	if err != nil {
		return s.failServiceOperation(ctx, op.ID, err)
	}
	if s.reviewPublisher == nil {
		return errors.New("host publication is not configured")
	}
	result, err := s.reviewPublisher(ctx, directory, intent)
	if err != nil {
		// Git/HTTP may have succeeded before a disconnect. Preserve the intent;
		// a retry obtains a fresh grant and reconciles the same branch and PR.
		code, detail := workerproto.PublicationRetryDiagnostic(err)
		return errors.Join(err, s.store.RecordPublicationRetry(ctx, op, session.ErrorCode(code), detail))
	}
	if !validPublicationResult(intent, result) {
		return errors.New("host publication returned a mismatched receipt")
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return s.store.CompleteOperation(ctx, op.ID, "publication", intent.SessionID, data)
}

func validPublicationResult(intent workerproto.PublishIntent, result workerproto.PublishResult) bool {
	req := intent.Request
	if result.Status == "refused" && result.Receipt == nil && result.Conflict == nil {
		if result.ErrorCode == "publication_push_outcome_unverified" {
			return req.Mode == "existing_pull_request"
		}
		switch result.ErrorCode {
		case "publication_branch_changed", "publication_branch_already_exists", "publication_existing_pull_request_changed", "publication_pull_request_mismatch", "publication_authorization_revoked":
			return true
		}
	}
	published := result.Status == "published" || result.Status == "published_target_changed" || result.Status == "published_target_unverified"
	if r := result.Receipt; published && r != nil && result.Conflict == nil && result.ErrorCode == "" {
		if result.Status == "published" && r.TargetObservation != nil {
			return false
		}
		if result.Status != "published" {
			state := "changed_after_push"
			if result.Status == "published_target_unverified" {
				state = "unverified_after_push"
			}
			if req.Mode != "existing_pull_request" || r.TargetObservation == nil || r.TargetObservation.State != state || r.TargetObservation.Validate() != nil {
				return false
			}
		}
		return r.Repository == intent.Repository.RepositoryRef && r.BranchRef == "refs/heads/"+req.Branch &&
			r.CandidateTree == req.CandidateTree && r.CommitSHA == req.CandidateHead && r.PullRequestNumber > 0 &&
			(req.PullRequestNumber == 0 || r.PullRequestNumber == req.PullRequestNumber) &&
			r.PullRequestURL == fmt.Sprintf("https://github.com/%s/pull/%d", intent.Repository.GitHubRepository, r.PullRequestNumber)
	}
	if r := result.Conflict; result.Status == "conflict" && r != nil && result.Receipt == nil {
		return r.Repository == intent.Repository.RepositoryRef && r.GitHubRepository == intent.Repository.GitHubRepository &&
			r.BranchRef == "refs/heads/"+req.Branch && r.CandidateCommitSHA == req.CandidateHead &&
			forkspace.ValidObjectID(r.ObservedHeadSHA) && r.ObservedHeadSHA != req.CandidateHead && r.PullRequestNumber > 0 &&
			r.PullRequestURL == fmt.Sprintf("https://github.com/%s/pull/%d", intent.Repository.GitHubRepository, r.PullRequestNumber) &&
			(result.ErrorCode == "publication_branch_changed" || result.ErrorCode == "publication_branch_already_exists")
	}
	return false
}

func (s *Service) GetPublication(ctx context.Context, sessionID, operationID string) (session.Operation, workerproto.PublishResult, error) {
	op, err := s.store.GetOperationByID(ctx, operationID)
	if err != nil {
		return op, workerproto.PublishResult{}, err
	}
	if op.Method != "PublishReview" || op.State != session.OperationSucceeded || op.ResourceType != "publication" || op.ResourceID != sessionID {
		return op, workerproto.PublishResult{}, &session.Error{Code: session.CodeInvalidRequest, Detail: "operation is not a completed publication for this session"}
	}
	var result workerproto.PublishResult
	err = json.Unmarshal(op.Result, &result)
	return op, result, err
}

func (s *Service) requireNoPendingPublication(ctx context.Context, sessionID string) error {
	ops, err := s.store.ListIncompleteOperations(ctx)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.Method != "PublishReview" || op.State == session.OperationReserved {
			continue
		}
		var intent workerproto.PublishIntent
		if json.Unmarshal(op.Result, &intent) != nil || intent.SessionID == sessionID {
			return &session.Error{Code: session.CodeInvalidSessionState, Detail: "publication must finish before discarding its retained candidate"}
		}
	}
	return nil
}
