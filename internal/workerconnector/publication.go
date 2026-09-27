package workerconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

type publicationGrant struct {
	JobSourceGrant
	ActorID int64 `json:"actor_id"`
}

// PublishReview is a host-only operation. The generic controller authenticates
// the exact command again before releasing a short-lived write credential.
func (t *HTTPTransport) PublishReview(ctx context.Context, repository string, intent workerproto.PublishIntent) (workerproto.PublishResult, error) {
	host := publicationHost{
		stateRoot: t.stateRoot,
		grant:     t.publicationGrant, api: "https://api.github.com",
		remote: "https://github.com/" + intent.Repository.GitHubRepository + ".git", protocol: "https",
		client: &http.Client{Timeout: time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	return host.publish(ctx, repository, intent)
}

func (t *HTTPTransport) publicationGrant(ctx context.Context, intent workerproto.PublishIntent) (publicationGrant, error) {
	var grant publicationGrant
	if !reference(intent.JobRef, 256) || intent.Repository.Validate() != nil || intent.Request.Validate() != nil {
		return grant, errors.New("invalid publication grant request")
	}
	endpoint := t.baseURL.ResolveReference(&url.URL{
		Path:    "/v1/coop-workers/jobs/" + intent.JobRef + "/publication-grants",
		RawPath: "/v1/coop-workers/jobs/" + url.PathEscape(intent.JobRef) + "/publication-grants",
	}).String()
	document, err := json.Marshal(intent)
	if err != nil {
		return grant, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(document))
	if err != nil {
		return grant, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	client, err := t.clientFor(ctx)
	if err != nil {
		return grant, err
	}
	response, err := client.Do(request)
	if err != nil {
		return grant, errors.New("publication grant unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusForbidden {
		return grant, publicationRefusal("publication_authorization_revoked")
	}
	if response.StatusCode != http.StatusOK {
		return grant, &BodyStatusError{Status: response.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(data) > 8192 {
		return grant, errors.New("invalid publication grant")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&grant) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		strings.Split(response.Header.Get("Content-Type"), ";")[0] != "application/json" ||
		grant.RepositoryRef != intent.Repository.RepositoryRef || grant.GitHubRepository != intent.Repository.GitHubRepository ||
		grant.GitHubRepositoryID != intent.Repository.GitHubRepositoryID || grant.ActorID <= 0 ||
		len(grant.Token) < 1 || len(grant.Token) > 4096 || !grant.ExpiresAt.After(time.Now().Add(30*time.Second)) {
		return publicationGrant{}, errors.New("invalid publication grant")
	}
	return grant, nil
}

type publicationHost struct {
	stateRoot             string
	grant                 func(context.Context, workerproto.PublishIntent) (publicationGrant, error)
	client                *http.Client
	api, remote, protocol string
	lfsEndpointForTest    string
}

type githubPublicationPull struct {
	Number   int     `json:"number"`
	URL      string  `json:"html_url"`
	State    string  `json:"state"`
	Draft    bool    `json:"draft"`
	Merged   bool    `json:"merged"`
	MergedAt *string `json:"merged_at"`
	User     struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"user"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			ID int64 `json:"id"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			ID int64 `json:"id"`
		} `json:"repo"`
	} `json:"base"`
}

func (p publicationHost) publish(ctx context.Context, repository string, intent workerproto.PublishIntent) (workerproto.PublishResult, error) {
	ctx, err := withSourceCredentials(ctx, p.stateRoot)
	if err != nil {
		return workerproto.PublishResult{}, err
	}
	result, err := p.publishCandidate(ctx, repository, intent)
	var refusal publicationRefusal
	if errors.As(err, &refusal) {
		return workerproto.PublishResult{Status: "refused", ErrorCode: string(refusal)}, nil
	}
	return result, err
}

type publicationRefusal string

func (r publicationRefusal) Error() string { return string(r) }

func (p publicationHost) publishCandidate(ctx context.Context, repository string, intent workerproto.PublishIntent) (workerproto.PublishResult, error) {
	var empty workerproto.PublishResult
	if intent.Request.Validate() != nil || intent.Repository.Validate() != nil {
		return empty, errors.New("invalid publication request")
	}
	grant, err := p.grant(ctx, intent)
	if err != nil {
		return empty, err
	}
	req := intent.Request
	head, err := sourceGitValue(ctx, repository, "", "", "rev-parse", "--verify", req.CandidateHead+"^{commit}")
	if err != nil || head != req.CandidateHead {
		return empty, errors.New("reviewed commit unavailable")
	}
	tree, err := sourceGitValue(ctx, repository, "", "", "rev-parse", "--verify", req.CandidateHead+"^{tree}")
	if err != nil || tree != req.CandidateTree {
		return empty, errors.New("reviewed tree unavailable")
	}
	if err := forkspace.CopyLFSObjects(ctx, repository, head); err != nil {
		return empty, errors.New("reviewed LFS object unavailable")
	}
	base := "/repos/" + intent.Repository.GitHubRepository
	var remoteRepository struct {
		ID int64 `json:"id"`
	}
	if err := p.github(ctx, grant.Token, "GET", base, nil, &remoteRepository); err != nil {
		return empty, err
	}
	if remoteRepository.ID != intent.Repository.GitHubRepositoryID {
		return empty, publicationRefusal("publication_pull_request_mismatch")
	}
	pull, err := p.findPull(ctx, grant, intent)
	if err != nil {
		return empty, err
	}
	observed, err := p.branchHead(ctx, repository, grant.Token, req.Branch)
	if err != nil {
		return empty, err
	}
	if observed != req.ExpectedHead && observed != head {
		return publicationConflict(intent, observed, pull), nil
	}
	if observed != head {
		if err := p.pushLFS(ctx, repository, grant.Token, intent); err != nil {
			return empty, err
		}
		ref := "refs/heads/" + req.Branch
		if err := sourceGitIO(ctx, repository, grant.Token, p.protocol, nil, io.Discard,
			"-c", "http.followRedirects=false", "push", "--quiet", "--no-verify", "--force-with-lease="+ref+":"+req.ExpectedHead,
			p.remote, head+":"+ref); err != nil {
			observed, lookupErr := p.branchHead(ctx, repository, grant.Token, req.Branch)
			if lookupErr != nil {
				return empty, lookupErr
			}
			if observed != head {
				if observed != req.ExpectedHead {
					return publicationConflict(intent, observed, pull), nil
				}
				return empty, err
			}
		}
	}
	if pull == nil {
		// Reconcile again after the push: another retry may have created the PR.
		pull, err = p.findPull(ctx, grant, intent)
		if err != nil {
			return empty, err
		}
	}
	if pull == nil {
		pull = &githubPublicationPull{}
		body := map[string]any{"title": req.Title, "body": req.Body, "head": req.Branch, "base": req.BaseBranch, "draft": true}
		if err := p.github(ctx, grant.Token, "POST", base+"/pulls", body, pull); err != nil {
			return empty, err
		}
	} else {
		// Updating the exact PR preserves a human's ready-for-review choice.
		path := fmt.Sprintf("%s/pulls/%d", base, pull.Number)
		if err := p.github(ctx, grant.Token, "PATCH", path, map[string]string{"title": req.Title, "body": req.Body}, pull); err != nil {
			return empty, err
		}
	}
	if !exactPublicationPull(*pull, grant, intent) {
		return empty, publicationRefusal("publication_pull_request_mismatch")
	}
	if pull.Head.SHA != head {
		// GitHub's PR projection may lag the successful Git push. Reconcile on
		// retry; a stale response must not permanently refuse the exact candidate.
		return empty, errors.New("published pull request head not yet confirmed")
	}
	return workerproto.PublishResult{Status: "published", Receipt: &workerproto.PublicationReceipt{
		Repository: intent.Repository.RepositoryRef, BranchRef: "refs/heads/" + req.Branch, CandidateTree: req.CandidateTree,
		CommitSHA: head, PullRequestNumber: pull.Number, PullRequestURL: pull.URL,
	}}, nil
}

func (p publicationHost) branchHead(ctx context.Context, repo, token, branch string) (string, error) {
	ref := "refs/heads/" + branch
	lookup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := sourceGitValue(lookup, repo, token, p.protocol, "-c", "http.followRedirects=false", "ls-remote", "--refs", p.remote, ref)
	if err != nil || output == "" {
		return "", err
	}
	fields := strings.Fields(output)
	if len(fields) != 2 || fields[1] != ref || (len(fields[0]) != 40 && len(fields[0]) != 64) {
		return "", errors.New("invalid publication branch identity")
	}
	return fields[0], nil
}

func (p publicationHost) findPull(ctx context.Context, grant publicationGrant, intent workerproto.PublishIntent) (*githubPublicationPull, error) {
	base := "/repos/" + intent.Repository.GitHubRepository + "/pulls"
	var pulls []githubPublicationPull
	if intent.Request.PullRequestNumber > 0 {
		var pull githubPublicationPull
		if err := p.github(ctx, grant.Token, "GET", fmt.Sprintf("%s/%d", base, intent.Request.PullRequestNumber), nil, &pull); err != nil {
			return nil, err
		}
		pulls = []githubPublicationPull{pull}
	} else {
		owner := strings.Split(intent.Repository.GitHubRepository, "/")[0]
		query := url.Values{"state": {"all"}, "head": {owner + ":" + intent.Request.Branch}, "base": {intent.Request.BaseBranch}, "per_page": {"100"}}
		if err := p.github(ctx, grant.Token, "GET", base+"?"+query.Encode(), nil, &pulls); err != nil {
			return nil, err
		}
	}
	if len(pulls) == 0 {
		return nil, nil
	}
	if len(pulls) != 1 || !exactPublicationPull(pulls[0], grant, intent) {
		return nil, publicationRefusal("publication_existing_pull_request_changed")
	}
	return &pulls[0], nil
}

func exactPublicationPull(pull githubPublicationPull, grant publicationGrant, intent workerproto.PublishIntent) bool {
	return pull.Number > 0 && (intent.Request.PullRequestNumber == 0 || pull.Number == intent.Request.PullRequestNumber) &&
		pull.State == "open" && !pull.Merged && pull.MergedAt == nil &&
		pull.Head.Ref == intent.Request.Branch && pull.Base.Ref == intent.Request.BaseBranch &&
		pull.Head.Repo.ID == intent.Repository.GitHubRepositoryID && pull.Base.Repo.ID == intent.Repository.GitHubRepositoryID &&
		pull.User.ID == grant.ActorID && pull.User.Type == "Bot" &&
		pull.URL == fmt.Sprintf("https://github.com/%s/pull/%d", intent.Repository.GitHubRepository, pull.Number) &&
		(intent.Request.PullRequestNumber > 0 || pull.Draft)
}

func publicationConflict(intent workerproto.PublishIntent, observed string, pull *githubPublicationPull) workerproto.PublishResult {
	code := "publication_branch_changed"
	if intent.Request.ExpectedHead == "" {
		code = "publication_branch_already_exists"
	}
	if pull == nil || observed == "" {
		return workerproto.PublishResult{Status: "refused", ErrorCode: code}
	}
	if pull.Head.SHA != observed {
		return workerproto.PublishResult{Status: "refused", ErrorCode: "publication_existing_pull_request_changed"}
	}
	conflict := &workerproto.PublicationConflict{Repository: intent.Repository.RepositoryRef, GitHubRepository: intent.Repository.GitHubRepository,
		BranchRef: "refs/heads/" + intent.Request.Branch, CandidateCommitSHA: intent.Request.CandidateHead, ObservedHeadSHA: observed}
	if pull != nil {
		conflict.PullRequestNumber, conflict.PullRequestURL = pull.Number, pull.URL
	}
	return workerproto.PublishResult{Status: "conflict", ErrorCode: code, Conflict: conflict}
}

func (p publicationHost) pushLFS(ctx context.Context, repo, token string, intent workerproto.PublishIntent) error {
	objects, err := os.CreateTemp("", "coop-lfs-publication-")
	if err != nil {
		return err
	}
	defer os.Remove(objects.Name())
	defer objects.Close()
	count := 0
	err = forkspace.VisitLFSPointers(ctx, repo, intent.Request.CandidateHead, func(pointer forkspace.LFSPointer) error {
		count++
		_, err := fmt.Fprintln(objects, pointer.OID)
		return err
	})
	if err != nil || count == 0 {
		return err
	}
	if _, err := objects.Seek(0, io.SeekStart); err != nil {
		return err
	}
	endpoint := "https://github.com/" + intent.Repository.GitHubRepository + ".git/info/lfs"
	if p.lfsEndpointForTest != "" {
		endpoint = p.lfsEndpointForTest
	}
	return sourceGitIO(ctx, repo, token, p.protocol, objects, io.Discard,
		"-c", "lfs.url="+endpoint, "-c", "lfs.pushurl="+endpoint,
		"-c", "lfs.basictransfersonly=true", "-c", "lfs.standalonetransferagent=",
		"-c", "lfs.remote.autodetect=false", "-c", "lfs.remote.searchall=false",
		"-c", "lfs.transfer.enablehrefrewrite=false", "lfs", "push", "--object-id", p.remote, "--stdin")
}

func (p publicationHost) github(ctx context.Context, token, method, path string, body, result any) error {
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, p.api+path, input)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return errors.New("publication GitHub request unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("publication GitHub request failed (%d)", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(data) > 2<<20 || json.Unmarshal(data, result) != nil {
		return errors.New("invalid publication GitHub response")
	}
	return nil
}
