package workerproto

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// PublishRequest authorizes one exact reviewed commit and one compare-and-swap
// branch update. Credentials and product-specific approval rules live in the controller.
type PublishRequest struct {
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
	Repository        string `json:"repository"`
	BranchRef         string `json:"branch_ref"`
	CandidateTree     string `json:"candidate_tree"`
	CommitSHA         string `json:"commit_sha"`
	PullRequestNumber int    `json:"pull_request_number"`
	PullRequestURL    string `json:"pull_request_url"`
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
