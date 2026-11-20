package workerproto

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// PublishRequest authorizes one exact reviewed commit and one compare-and-swap
// branch update. Credentials and product-specific approval rules live in the controller.
type PublishRequest struct {
	Mode              string `json:"mode,omitempty"`
	AuthorizationRef  string `json:"authorization_ref"`
	CandidateHead     string `json:"candidate_head"`
	CandidateTree     string `json:"candidate_tree"`
	Branch            string `json:"branch"`
	BaseBranch        string `json:"base_branch"`
	ExpectedHead      string `json:"expected_head"`
	PullRequestNumber int    `json:"pull_request_number"`
	Title             string `json:"title"`
	Body              string `json:"body"`
}

func (r PublishRequest) Validate() error {
	if r.Mode != "" && r.Mode != "existing_pull_request" {
		return errors.New("invalid publication mode")
	}
	if r.Mode == "existing_pull_request" && (r.PullRequestNumber <= 0 || r.ExpectedHead == "") {
		return errors.New("existing pull request publication needs an exact target and head")
	}
	if reference(r.AuthorizationRef, 1024, "publication authorization") != nil ||
		!publicationObject(r.CandidateHead) || !publicationObject(r.CandidateTree) ||
		(r.ExpectedHead != "" && !publicationObject(r.ExpectedHead)) || r.PullRequestNumber < 0 ||
		(r.PullRequestNumber > 0 && r.ExpectedHead == "") ||
		!publicationBranch(r.Branch) || !publicationBranch(r.BaseBranch) || r.Branch == r.BaseBranch ||
		!publicationText(r.Title, 256) || !publicationText(r.Body, 32<<10) {
		return errors.New("invalid publication identity or destination")
	}
	return nil
}

// PublishIntent is the durable worker intent and the controller's grant request.
// CommandKey identifies the authenticated API command, not an approval by itself.
type PublishIntent struct {
	SessionID         string             `json:"session_id"`
	ReviewOperationID string             `json:"review_operation_id"`
	JobRef            string             `json:"job_ref"`
	JobDigest         string             `json:"job_digest"`
	Repository        RepositoryIdentity `json:"repository"`
	CommandKey        string             `json:"command_key"`
	Request           PublishRequest     `json:"request"`
}

type PublicationReceipt struct {
	Repository        string                        `json:"repository"`
	BranchRef         string                        `json:"branch_ref"`
	CandidateTree     string                        `json:"candidate_tree"`
	CommitSHA         string                        `json:"commit_sha"`
	PullRequestNumber int                           `json:"pull_request_number"`
	PullRequestURL    string                        `json:"pull_request_url"`
	TargetObservation *PublicationTargetObservation `json:"target_observation,omitempty"`
}

// A successful branch write and its later PR observation are separate facts.
type PublicationTargetObservation struct {
	State            string `json:"state"`
	HeadRef          string `json:"head_ref"`
	BaseRef          string `json:"base_ref"`
	HeadRepositoryID int64  `json:"head_repository_id"`
	BaseRepositoryID int64  `json:"base_repository_id"`
	HeadCommit       string `json:"head_commit"`
	PullRequestState string `json:"pull_request_state"`
}

func (o PublicationTargetObservation) Validate() error {
	if o.State != "changed_after_push" && o.State != "unverified_after_push" {
		return errors.New("invalid publication target observation")
	}
	if len(o.HeadRef) > 256 || len(o.BaseRef) > 256 ||
		!utf8.ValidString(o.HeadRef) || !utf8.ValidString(o.BaseRef) ||
		o.HeadRepositoryID < 0 || o.BaseRepositoryID < 0 ||
		(o.HeadCommit != "" && !publicationObject(o.HeadCommit)) ||
		(o.PullRequestState != "" && o.PullRequestState != "open" && o.PullRequestState != "closed") {
		return errors.New("invalid publication target observation fields")
	}
	return nil
}

type PublicationConflict struct {
	Repository         string `json:"repository"`
	GitHubRepository   string `json:"github_repository"`
	BranchRef          string `json:"branch_ref"`
	CandidateCommitSHA string `json:"candidate_commit_sha"`
	ObservedHeadSHA    string `json:"observed_head_sha"`
	PullRequestNumber  int    `json:"pull_request_number"`
	PullRequestURL     string `json:"pull_request_url"`
}

// A CAS conflict is a completed operation with evidence, not a lost HTTP error body.
type PublishResult struct {
	Status    string               `json:"status"`
	Receipt   *PublicationReceipt  `json:"receipt,omitempty"`
	Conflict  *PublicationConflict `json:"conflict,omitempty"`
	ErrorCode string               `json:"error_code,omitempty"`
	// ClosedPullRequest names the closed pull request that keeps a new-PR publish off its
	// branch, only with ErrorCode PublicationClosedPullRequest.
	ClosedPullRequest *PublicationPullRequest `json:"closed_pull_request,omitempty"`
}

// PublicationClosedPullRequest refuses a new-PR publish to a branch that already has a closed
// pull request the App does not own. Opening a second PR on that branch, or pushing to it, would
// write over someone else's work, so the way out is a new branch.
const PublicationClosedPullRequest = "publication_branch_has_closed_pull_request"

type PublicationPullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

func publicationObject(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func publicationBranch(s string) bool {
	if len(s) == 0 || len(s) > 240 || s == "@" || strings.HasPrefix(s, "-") ||
		strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.HasSuffix(s, ".") {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._/-", r)) {
			return false
		}
	}
	return true
}

func publicationText(s string, limit int) bool {
	return len(s) > 0 && len(s) <= limit && utf8.ValidString(s) && !strings.ContainsRune(s, 0) && strings.TrimSpace(s) != ""
}
