package sessionsvc

import (
	"context"
	"errors"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

const sessionSourceLabel = "source"

func (s *Service) pinReviewParent(ctx context.Context, sess session.Session, requested string) (string, error) {
	if requested == "" {
		return s.pinCurrentSessionParent(ctx, sess)
	}
	execution, err := s.sessionExecution(ctx, sess)
	if err != nil {
		return "", &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
	}
	job, err := workerproto.DecodeJobSpec(sess.JobDocument)
	if err != nil || job.Source == nil || sess.Source == nil ||
		!forkspace.ValidObjectID(requested) || job.Source.Binding.SelectedCommit != requested || sess.Source.SelectedCommit != requested {
		return "", &session.Error{Code: session.CodeInvalidRequest, Detail: "review parent does not match the job's admitted source"}
	}
	actual, err := sessionWorkspaceCommitContext(ctx, execution.Repository, requested)
	if err != nil || actual != requested {
		return "", &session.Error{Code: session.CodeRepositoryUnavailable, Detail: "admitted review parent is unavailable"}
	}
	return requested, nil
}

func (s *Service) pinCurrentSessionParent(ctx context.Context, sess session.Session) (string, error) {
	execution, err := s.sessionExecution(ctx, sess)
	if err != nil {
		return "", &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
	}
	job, err := workerproto.DecodeJobSpec(sess.JobDocument)
	if err == nil && job.Source == nil {
		return emptyJobCommit, nil // sessionExecution verified the private empty baseline.
	}
	if err != nil {
		return "", &session.Error{Code: session.CodeRepositoryUnavailable,
			Detail: "current default revision requires a controller-authorized source refresh"}
	}
	return s.refreshSessionSource(ctx, job, execution.Repository)
}

// refreshSessionSource asks the job's controller-granted source for its current default commit
// and proves that commit is in the worker's own staged copy. Nothing the workspace holds, no ref
// or config an agent can write, takes part.
func (s *Service) refreshSessionSource(ctx context.Context, job workerproto.JobSpec, repository string) (string, error) {
	if job.Source == nil || s.sourceRefresher == nil {
		return "", &session.Error{Code: session.CodeRepositoryUnavailable,
			Detail: "current default revision requires a controller-authorized source refresh"}
	}
	head, err := s.sourceRefresher(ctx, job.JobRef, *job.Source, repository)
	if errors.Is(err, workerproto.ErrSourceGrantRefused) {
		return "", &session.Error{Code: session.CodeInvalidSessionState,
			Detail: "the controller refuses read access to the job's repository; asking again will not change that"}
	}
	if err != nil {
		return "", &session.Error{Code: session.CodeRepositoryUnavailable, Detail: "could not refresh the job's default revision"}
	}
	actual, err := sessionWorkspaceCommitContext(ctx, repository, head)
	if err != nil || !forkspace.ValidObjectID(head) || actual != head {
		return "", &session.Error{Code: session.CodeRepositoryUnavailable, Detail: "refreshed default revision is unproven"}
	}
	return head, nil
}

// Cleanup needs no execution authority: a job that can no longer run can still be discarded.
func (s *Service) pinDiscardSessionParent(ctx context.Context, sess session.Session) (string, error) {
	if head, err := s.pinCurrentSessionParent(ctx, sess); err == nil {
		return head, nil
	}
	// Missing/expired read authority must not orphan a workspace. The original
	// local parent preserves the unmerged-work guard when a refresh is unavailable.
	return sessionWorkspaceCommitContext(ctx, sess.Repository, "HEAD")
}
