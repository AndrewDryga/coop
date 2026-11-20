package workerconnector

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// An existing human PR has an immutable target. Only its branch may change;
// its title, body, ready state and base remain under the author's control.
func (p publicationHost) publishExistingPull(ctx context.Context, repository string, grant publicationGrant, intent workerproto.PublishIntent) (workerproto.PublishResult, error) {
	var empty workerproto.PublishResult
	req := intent.Request
	if err := sourceGitIO(ctx, repository, "", "", nil, io.Discard,
		"merge-base", "--is-ancestor", req.ExpectedHead, req.CandidateHead); err != nil {
		return empty, publicationRefusal("publication_pull_request_mismatch")
	}
	observed, err := p.branchHead(ctx, repository, grant.Token, req.Branch)
	if err != nil {
		return empty, publicationRetry("branch_lookup", err)
	}
	// A lost response must recover the same commit, even if someone subsequently
	// changes the PR metadata. This observation never authorizes another push.
	if observed == req.CandidateHead {
		return p.confirmExistingPull(ctx, grant, intent)
	}
	pull, err := p.readExistingPull(ctx, grant, intent)
	if err != nil {
		return empty, publicationRetry("pull_request_lookup", err)
	}
	if !exactPublicationPull(*pull, grant, intent) {
		return empty, publicationRefusal("publication_existing_pull_request_changed")
	}
	if observed != req.ExpectedHead {
		return publicationConflict(intent, observed, pull), nil
	}
	if pull.Head.SHA != req.ExpectedHead {
		return empty, publicationRefusal("publication_existing_pull_request_changed")
	}
	if err := p.pushLFS(ctx, repository, grant.Token, intent); err != nil {
		return empty, publicationRetry("lfs_upload", err)
	}
	// Preparation may take minutes. Renew the controller's authorization and
	// check the exact PR immediately before the atomic branch comparison.
	grant, err = p.grant(ctx, intent)
	if err != nil {
		return empty, publicationRetry("grant", err)
	}
	pull, err = p.findPull(ctx, grant, intent)
	if err != nil {
		return empty, publicationRetry("pull_request_lookup", err)
	}
	if pull == nil || pull.Head.SHA != req.ExpectedHead {
		return empty, publicationRefusal("publication_existing_pull_request_changed")
	}
	ref := "refs/heads/" + req.Branch
	if err := p.savePublicationProgress(intent, publicationProgress{Attempted: true}); err != nil {
		return empty, publicationRetry("candidate", err)
	}
	if err := sourceGitIO(ctx, repository, grant.Token, p.protocol, nil, io.Discard,
		"-c", "http.followRedirects=false", "push", "--quiet", "--no-verify", "--force-with-lease="+ref+":"+req.ExpectedHead,
		p.remote, req.CandidateHead+":"+ref); err != nil {
		observed, lookupErr := p.branchHead(ctx, repository, grant.Token, req.Branch)
		if lookupErr != nil {
			return empty, publicationRetry("branch_lookup", lookupErr)
		}
		if observed != req.CandidateHead {
			if observed != req.ExpectedHead {
				latest, _ := p.readExistingPull(ctx, grant, intent)
				return publicationConflict(intent, observed, latest), nil
			}
			return empty, publicationRetry("git_push", err)
		}
	}
	// A confirmed write remains a confirmed write when GitHub's PR projection
	// lags, fails, or the author changes metadata during the push.
	return p.confirmExistingPull(ctx, grant, intent)
}

// GitHub's PR view lags a Git push by a few seconds. These fixed waits (15 s in
// total) let it catch up before an unconfirmed head becomes the final answer.
var existingPullConfirmationWaits = []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second, 5 * time.Second}

// confirmExistingPull reads the PR after a confirmed push until it shows the
// pushed head, shows a changed target, or the waits run out. Only a PR read that
// shows the pushed head reports published. It never pushes or asks for a grant.
// A worker stopping meanwhile settles nothing: the attempted marker and the
// pushed branch let the next try confirm the same push without pushing again.
func (p publicationHost) confirmExistingPull(ctx context.Context, grant publicationGrant, intent workerproto.PublishIntent) (workerproto.PublishResult, error) {
	stopped := &workerproto.PublicationRetryError{Stage: "pull_request_confirmation"}
	waits := existingPullConfirmationWaits
	if p.confirmationWaitsForTest != nil {
		waits = p.confirmationWaitsForTest
	}
	for attempt := 0; ; attempt++ {
		pull, err := p.readExistingPull(ctx, grant, intent)
		if err != nil && ctx.Err() != nil {
			return workerproto.PublishResult{}, stopped
		}
		result := existingPullReceipt(intent, grant, pull, err)
		if result.Status != "published_target_unverified" || attempt == len(waits) {
			return result, nil
		}
		timer := time.NewTimer(waits[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return workerproto.PublishResult{}, stopped
		case <-timer.C:
		}
	}
}

func (p publicationHost) readExistingPull(ctx context.Context, grant publicationGrant, intent workerproto.PublishIntent) (*githubPublicationPull, error) {
	var pull githubPublicationPull
	path := fmt.Sprintf("/repos/%s/pulls/%d", intent.Repository.GitHubRepository, intent.Request.PullRequestNumber)
	if err := p.github(ctx, grant.Token, "GET", path, nil, &pull); err != nil {
		return nil, err
	}
	return &pull, nil
}

func existingPullReceipt(intent workerproto.PublishIntent, grant publicationGrant, pull *githubPublicationPull, observationError error) workerproto.PublishResult {
	req := intent.Request
	receipt := &workerproto.PublicationReceipt{
		Repository: intent.Repository.RepositoryRef, BranchRef: "refs/heads/" + req.Branch,
		CandidateTree: req.CandidateTree, CommitSHA: req.CandidateHead, PullRequestNumber: req.PullRequestNumber,
		PullRequestURL: fmt.Sprintf("https://github.com/%s/pull/%d", intent.Repository.GitHubRepository, req.PullRequestNumber),
	}
	result := workerproto.PublishResult{Status: "published", Receipt: receipt}
	if observationError != nil || pull == nil {
		result.Status = "published_target_unverified"
		receipt.TargetObservation = &workerproto.PublicationTargetObservation{State: "unverified_after_push"}
	} else if !exactPublicationPull(*pull, grant, intent) || pull.Head.SHA != req.CandidateHead {
		state := "changed_after_push"
		result.Status = "published_target_changed"
		if exactPublicationPull(*pull, grant, intent) && pull.Head.SHA == req.ExpectedHead {
			state, result.Status = "unverified_after_push", "published_target_unverified"
		}
		observation := &workerproto.PublicationTargetObservation{
			State: state, HeadRef: pull.Head.Ref, BaseRef: pull.Base.Ref,
			HeadRepositoryID: pull.Head.Repo.ID, BaseRepositoryID: pull.Base.Repo.ID,
			HeadCommit: pull.Head.SHA, PullRequestState: pull.State,
		}
		if observation.Validate() != nil {
			observation = &workerproto.PublicationTargetObservation{State: "unverified_after_push"}
			result.Status = "published_target_unverified"
		}
		receipt.TargetObservation = observation
	}
	return result
}
