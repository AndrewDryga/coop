package sessionsvc

import (
	"context"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

const sessionSourceLabel = "source"

func (s *Service) pinCurrentSessionParent(ctx context.Context, sess session.Session) (string, error) {
	execution, err := s.sessionExecution(ctx, sess)
	if err != nil {
		return "", &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
	}
	job, err := workerproto.DecodeJobSpec(sess.JobDocument)
	if err == nil && job.Source == nil && job.Mode != "bare" {
		return emptyJobCommit, nil // sessionExecution verified the private empty baseline.
	}
	if err != nil || job.Source == nil || s.sourceRefresher == nil {
		return "", &session.Error{Code: session.CodeRepositoryUnavailable,
			Detail: "current default revision requires a controller-authorized source refresh"}
	}
	head, err := s.sourceRefresher(ctx, job.JobRef, *job.Source, execution.Repository)
	if err != nil {
		return "", &session.Error{Code: session.CodeRepositoryUnavailable, Detail: "could not refresh the job's default revision"}
	}
	actual, err := sessionWorkspaceCommitContext(ctx, execution.Repository, head)
	if err != nil || !validSessionWorkspaceCommit(head) || actual != head {
		return "", &session.Error{Code: session.CodeRepositoryUnavailable, Detail: "refreshed default revision is unproven"}
	}
	return head, nil
}

// Cleanup remains available for historical sessions without reauthorizing execution.
func (s *Service) pinDiscardSessionParent(ctx context.Context, sess session.Session) (string, error) {
	if sess.JobDigest != "" {
		if head, err := s.pinCurrentSessionParent(ctx, sess); err == nil {
			return head, nil
		}
	}
	// Missing/expired read authority must not orphan a workspace. The original
	// local parent preserves the unmerged-work guard when a refresh is unavailable.
	return sessionWorkspaceCommitContext(ctx, sess.Repository, "HEAD")
}
