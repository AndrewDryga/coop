package sessionsvc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
)

// sessionSourceLabel names the freshness receipt for a NON-default selection. A default selection
// needs no second receipt: the primary one already proves the exact same ref and object.
const sessionSourceLabel = "source"

// sessionSourceAdvertisedTipLimit bounds how many advertised tips the commit-custody anchor
// compares against. `ls-remote` sorts by refname, so `refs/heads/*` is considered before
// `refs/pull/*/head` and a branch tip is never crowded out by pull-request refs. Evidence beyond
// the bound is not consulted: a commit it would have proven is refused with its reason rather than
// accepted on a partial scan.
const sessionSourceAdvertisedTipLimit = 512

type sessionRepositorySource struct {
	label      string
	repository string
	remote     string
	branch     string
	ref        string
	expected   string
}

type sessionPolicyPins struct {
	creationBase  string
	workspaceHead string
	companions    []string
	receipts      []session.RepositoryFreshnessReceipt
	// binding is the immutable version-1 source identity for a remote-backed policy. An
	// intentionally local policy resolves no remote and therefore publishes no binding; its
	// freshness receipt already says `local`.
	binding *session.SourceBinding
}

type sessionSourceGitRunner func(context.Context, string, ...string) ([]byte, error)

// validateSessionSourceSelector is the request-side half of source authority. The caller supplies
// ONLY a bounded selector value: the repository, the remote and the default branch come from the
// operator policy and can never be named here. Git's own ref rules validate a branch name, and a
// policy with no configured remote refuses every selector whose proof it cannot obtain.
func validateSessionSourceSelector(policy Policy, selector session.SourceSelector) error {
	if err := session.ValidateSourceSelectorShape(selector); err != nil {
		return err
	}
	if selector.Kind == session.SourceBranch {
		if err := exec.Command("git", "check-ref-format", "refs/heads/"+selector.Name).Run(); err != nil {
			return &session.Error{
				Code:   session.CodeInvalidRequest,
				Detail: fmt.Sprintf("branch source %q is not a valid Git branch name", selector.Name),
			}
		}
	}
	if selector.Kind != session.SourceDefault && policy.Remote == "" {
		return &session.Error{
			Code: session.CodeInvalidRequest,
			Detail: fmt.Sprintf(
				"policy %q has no operator-configured remote, so only its default source can be selected",
				policy.Name,
			),
		}
	}
	return nil
}

// pinSessionPolicySources resolves every repository one session needs, in the only order that is
// safe: the configured default head and the companions first, then the selected source, then the
// merge base between them. Every pin is proven against the operator-configured remote before a
// workspace exists, so a failure leaves no partial fork behind.
func pinSessionPolicySources(
	ctx context.Context,
	policy Policy,
	selector session.SourceSelector,
) (sessionPolicyPins, error) {
	if err := validateSessionSourceSelector(policy, selector); err != nil {
		return sessionPolicyPins{}, err
	}
	result, err := pinSessionPolicyDefaults(ctx, policy)
	if err != nil {
		return sessionPolicyPins{}, err
	}
	if policy.Remote == "" {
		// An intentionally local policy has no remote identity to bind, and the selector check
		// above already refused anything but its own default.
		return result, nil
	}
	defaultRef := "refs/heads/" + policy.Branch
	defaultCommit := result.creationBase
	binding := session.SourceBinding{
		Version: session.SourceBindingVersion, Kind: selector.Kind, Requested: selector,
		RemoteIdentity: policy.Remote,
		DefaultRef:     defaultRef, DefaultCommit: defaultCommit,
		SelectedRef: &defaultRef, SelectedCommit: defaultCommit, BaseCommit: defaultCommit,
		ResolvedAt: time.Now().UTC(),
	}
	if selector.Kind != session.SourceDefault {
		receipt, selectedRef, err := pinSessionSelectedSource(ctx, policy, selector)
		if err != nil {
			return sessionPolicyPins{}, err
		}
		result.receipts = append(result.receipts, receipt)
		binding.SelectedCommit = receipt.ResolvedRevision
		binding.SelectedRef = selectedRef
		base, err := sessionSourceMergeBase(ctx, policy.Repository, defaultCommit, binding.SelectedCommit)
		if err != nil {
			return sessionPolicyPins{}, err
		}
		binding.BaseCommit = base
	}
	if selector.Kind == session.SourcePullRequest {
		binding.PullRequestNumber = selector.Number
		binding.PullRequestExpectedHead = selector.ExpectedHeadCommit
	}
	tree, err := sessionWorkspaceTree(policy.Repository, binding.SelectedCommit)
	if err != nil {
		return sessionPolicyPins{}, fmt.Errorf("resolve admitted source tree: %w", err)
	}
	binding.AdmittedTree = tree
	if err := session.ValidateSourceBinding(binding); err != nil {
		return sessionPolicyPins{}, err
	}
	result.creationBase = binding.BaseCommit
	result.workspaceHead = binding.SelectedCommit
	result.receipts[0].WorkspaceBaseRevision = binding.BaseCommit
	result.binding = &binding
	return result, nil
}

// pinSessionPolicyDefaults refreshes the primary repository's configured default branch and every
// companion's, concurrently. Each source is a separate object database, so the fan-out contends
// on nothing; the first failure cancels the rest.
func pinSessionPolicyDefaults(ctx context.Context, policy Policy) (sessionPolicyPins, error) {
	sources := make([]sessionRepositorySource, 1+len(policy.Companions))
	sources[0] = sessionRepositorySource{
		label: "primary", repository: policy.Repository,
		remote: policy.Remote, branch: policy.Branch,
	}
	for index, companion := range policy.Companions {
		sources[index+1] = sessionRepositorySource{
			label: companion.Name, repository: companion.Repository,
			remote: companion.Remote, branch: companion.Branch,
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	commits := make([]string, len(sources))
	receipts := make([]session.RepositoryFreshnessReceipt, len(sources))
	sem := make(chan struct{}, sessionPolicyRemoteConcurrency)
	errs := make(chan error, len(sources))
	var wg sync.WaitGroup
	for index, source := range sources {
		index, source := index, source
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			receipt, err := pinSessionRepositoryReceipt(ctx, source)
			if err != nil {
				select {
				case errs <- err:
				default:
				}
				cancel()
				return
			}
			commits[index] = receipt.ResolvedRevision
			receipts[index] = receipt
		}()
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return sessionPolicyPins{}, err
	}
	if err := ctx.Err(); err != nil {
		return sessionPolicyPins{}, err
	}
	result := sessionPolicyPins{
		creationBase: commits[0], workspaceHead: commits[0], companions: commits[1:], receipts: receipts,
	}
	result.receipts[0].WorkspaceBaseRevision = result.creationBase
	return result, nil
}

// pinSessionSelectedSource resolves one non-default selection and returns its freshness receipt
// plus the derived ref, which is nil for an exact commit because a commit advertises none.
func pinSessionSelectedSource(
	ctx context.Context,
	policy Policy,
	selector session.SourceSelector,
) (session.RepositoryFreshnessReceipt, *string, error) {
	source := sessionRepositorySource{
		label: sessionSourceLabel, repository: policy.Repository,
		remote: policy.Remote, branch: policy.Branch,
	}
	switch selector.Kind {
	case session.SourceBranch:
		source.ref = "refs/heads/" + selector.Name
	case session.SourcePullRequest:
		source.ref = fmt.Sprintf("refs/pull/%d/head", selector.Number)
		source.expected = selector.ExpectedHeadCommit
	case session.SourceCommit:
		receipt, err := pinSessionSourceCommit(ctx, source, selector.SHA)
		return receipt, nil, err
	}
	receipt, err := pinSessionRepositoryReceipt(ctx, source)
	if err != nil {
		return session.RepositoryFreshnessReceipt{}, nil, err
	}
	ref := source.ref
	return receipt, &ref, nil
}

// sessionSourceMergeBase is the review baseline: the merge base of the pinned default head and
// the selected head. Unrelated histories have none, and inventing one would make every later
// comparison lie, so the create fails with a source the operator can act on.
func sessionSourceMergeBase(ctx context.Context, repository, defaultCommit, selected string) (string, error) {
	out, truncated, err := runSessionWorkspaceGitWithEnvContext(
		ctx, repository, 4<<10, nil, "merge-base", defaultCommit, selected,
	)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", &session.Error{
				Code: session.CodeInvalidRequest,
				Detail: fmt.Sprintf(
					"selected source %s has no common history with the configured default branch at %s; choose a source that shares its history",
					selected, defaultCommit,
				),
			}
		}
		return "", fmt.Errorf("resolve selected source merge base: %w", err)
	}
	if truncated {
		return "", errors.New("selected source merge base exceeds bounds")
	}
	base := strings.TrimSpace(string(out))
	if !validSessionWorkspaceCommit(base) {
		return "", errors.New("selected source merge base is invalid")
	}
	return base, nil
}

func pinSessionRepository(ctx context.Context, source sessionRepositorySource) (string, error) {
	receipt, err := pinSessionRepositoryReceipt(ctx, source)
	return receipt.ResolvedRevision, err
}

func pinSessionRepositoryReceipt(ctx context.Context, source sessionRepositorySource) (session.RepositoryFreshnessReceipt, error) {
	return pinSessionRepositoryReceiptWithTimeouts(
		ctx, source, sessionPolicyRemoteLookupTimeout, sessionPolicyRemoteFetchTimeout,
		runSessionSourceGit,
	)
}

func pinSessionRepositoryWithTimeouts(
	ctx context.Context,
	source sessionRepositorySource,
	lookupTimeout time.Duration,
	fetchTimeout time.Duration,
	runGit sessionSourceGitRunner,
) (string, error) {
	receipt, err := pinSessionRepositoryReceiptWithTimeouts(ctx, source, lookupTimeout, fetchTimeout, runGit)
	return receipt.ResolvedRevision, err
}

func pinSessionRepositoryReceiptWithTimeouts(
	ctx context.Context,
	source sessionRepositorySource,
	lookupTimeout time.Duration,
	fetchTimeout time.Duration,
	runGit sessionSourceGitRunner,
) (session.RepositoryFreshnessReceipt, error) {
	requested := "HEAD"
	remoteIdentity := "local"
	if source.remote == "" {
		commit, err := sessionWorkspaceCommitContext(ctx, source.repository, "HEAD")
		if err != nil {
			return session.RepositoryFreshnessReceipt{}, fmt.Errorf("pin %s repository HEAD: %w", source.label, err)
		}
		return repositoryFreshnessReceipt(source.label, requested, commit, remoteIdentity, "not_applicable", ""), nil
	}

	lookupCtx, cancelLookup := context.WithTimeout(ctx, lookupTimeout)
	ref := source.ref
	if ref == "" {
		ref = "refs/heads/" + source.branch
	}
	requested = ref
	remoteIdentity = source.remote
	staleRevision := localSourceRevision(ctx, source)
	out, err := runGit(lookupCtx, source.repository,
		"ls-remote", "--exit-code", "--refs", "--", source.remote, ref)
	cancelLookup()
	if err != nil {
		var exitErr *exec.ExitError
		if source.ref != "" && errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
			return session.RepositoryFreshnessReceipt{}, &session.Error{
				Code: session.CodeInvalidRequest,
				Detail: fmt.Sprintf(
					"selected source ref %s does not exist on the operator-configured remote; choose a source the remote publishes",
					ref,
				),
			}
		}
		return session.RepositoryFreshnessReceipt{}, repositoryUnavailable(source, sourceDisplay(source), "", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 || fields[1] != ref || !validSessionWorkspaceCommit(fields[0]) {
		return session.RepositoryFreshnessReceipt{}, fmt.Errorf(
			"refresh %s repository from %s/%s: remote returned an invalid branch identity",
			source.label, source.remote, sourceDisplay(source),
		)
	}
	commit := fields[0]
	if source.expected != "" && commit != source.expected {
		return session.RepositoryFreshnessReceipt{}, &session.Error{
			Code: session.CodeInvalidRequest,
			Detail: fmt.Sprintf(
				"selected source %s changed before session creation; expected %s, found %s; create a fresh task for the current revision",
				ref, source.expected, commit,
			),
		}
	}
	// ls-remote already proved that this immutable object is the remote branch
	// head. If it is present locally, fetching the same hash cannot make the
	// session more exact; it only makes every new watch session wait on the
	// network again.
	if resolved, resolveErr := sessionWorkspaceCommitContext(ctx, source.repository, commit); resolveErr == nil {
		status, prior := staleBaseEvidence(staleRevision, resolved)
		return repositoryFreshnessReceipt(source.label, requested, resolved, remoteIdentity, status, prior), nil
	} else if ctx.Err() != nil {
		return session.RepositoryFreshnessReceipt{}, resolveErr
	}
	fetchCtx, cancelFetch := context.WithTimeout(ctx, fetchTimeout)
	_, err = runGit(fetchCtx, source.repository,
		"fetch", "--quiet", "--no-write-fetch-head", "--no-tags", "--", source.remote, commit)
	cancelFetch()
	if err != nil {
		return session.RepositoryFreshnessReceipt{}, repositoryUnavailable(source, sourceDisplay(source), commit, err)
	}
	resolved, err := sessionWorkspaceCommitContext(ctx, source.repository, commit)
	if err != nil {
		return session.RepositoryFreshnessReceipt{}, fmt.Errorf("verify refreshed %s repository commit %s: %w", source.label, commit, err)
	}
	status, prior := staleBaseEvidence(staleRevision, resolved)
	return repositoryFreshnessReceipt(source.label, requested, resolved, remoteIdentity, status, prior), nil
}

// pinSessionSourceCommit proves one exact object id against the configured remote. A commit has
// no advertised ref, so the remote is contacted on every create.
//
// The trap this function exists for: `git fetch <remote> <oid>` SHORT-CIRCUITS when the object is
// already in the local object database — it answers success without asking the server at all. A
// cached object is therefore no evidence of anything, and the operator's own unpublished commits
// live in exactly that database. So when the object was already present, custody is anchored a
// second way: the remote's CURRENT advertisement must contain the commit, either as a ref tip or
// as an ancestor of one. Nothing here consults a tracking ref, a previous receipt or local HEAD.
func pinSessionSourceCommit(
	ctx context.Context,
	source sessionRepositorySource,
	sha string,
) (session.RepositoryFreshnessReceipt, error) {
	return pinSessionSourceCommitWithTimeouts(
		ctx, source, sha, sessionPolicyRemoteLookupTimeout, sessionPolicyRemoteFetchTimeout,
		runSessionSourceGit,
	)
}

func pinSessionSourceCommitWithTimeouts(
	ctx context.Context,
	source sessionRepositorySource,
	sha string,
	lookupTimeout time.Duration,
	fetchTimeout time.Duration,
	runGit sessionSourceGitRunner,
) (session.RepositoryFreshnessReceipt, error) {
	cached := sessionSourceObjectPresent(ctx, source.repository, sha)
	fetchCtx, cancelFetch := context.WithTimeout(ctx, fetchTimeout)
	_, err := runGit(fetchCtx, source.repository,
		"fetch", "--quiet", "--no-write-fetch-head", "--no-tags", "--", source.remote, sha)
	cancelFetch()
	if err != nil {
		return session.RepositoryFreshnessReceipt{}, repositoryUnavailable(source, sha, sha, err)
	}
	if cached {
		if err := requireSessionSourceCommitAdvertised(ctx, source, sha, lookupTimeout, runGit); err != nil {
			return session.RepositoryFreshnessReceipt{}, err
		}
	}
	resolved, err := sessionWorkspaceCommitContext(ctx, source.repository, sha)
	if err != nil || resolved != sha {
		return session.RepositoryFreshnessReceipt{}, &session.Error{
			Code: session.CodeInvalidRequest,
			Detail: fmt.Sprintf(
				"object %s served by %s is not a commit; select a commit, a branch, or a pull request",
				sha, source.remote,
			),
		}
	}
	return repositoryFreshnessReceipt(sessionSourceLabel, sha, sha, source.remote, "not_applicable", ""), nil
}

func sessionSourceObjectPresent(ctx context.Context, repository, sha string) bool {
	_, _, err := runSessionWorkspaceGitWithEnvContext(
		ctx, repository, sessionWorkspaceGitOutputLimit, nil,
		"cat-file", "-e", sha,
	)
	return err == nil
}

// requireSessionSourceCommitAdvertised anchors an already-cached object to the remote's current
// advertisement, in the two namespaces a selector may name. Being an advertised tip, or an
// ancestor of one, is genuine custody: the remote serves that history right now.
func requireSessionSourceCommitAdvertised(
	ctx context.Context,
	source sessionRepositorySource,
	sha string,
	lookupTimeout time.Duration,
	runGit sessionSourceGitRunner,
) error {
	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	out, err := runGit(lookupCtx, source.repository,
		"ls-remote", "--refs", "--", source.remote, "refs/heads/*", "refs/pull/*/head")
	cancel()
	if err != nil {
		return repositoryUnavailable(source, sha, "", err)
	}
	tips := make([]string, 0, sessionSourceAdvertisedTipLimit)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !validSessionWorkspaceCommit(fields[0]) {
			continue
		}
		if fields[0] == sha {
			return nil
		}
		if len(tips) < sessionSourceAdvertisedTipLimit {
			tips = append(tips, fields[0])
		}
	}
	if len(tips) > 0 {
		contained, err := sessionSourceContainedInTips(ctx, source.repository, sha, tips)
		if err != nil {
			return fmt.Errorf("check advertised ancestry of %s: %w", sha, err)
		}
		if contained {
			return nil
		}
	}
	return &session.Error{
		Code: session.CodeInvalidRequest,
		Detail: fmt.Sprintf(
			"commit %s is present in the local repository cache but %s publishes no branch or pull request containing it; a cached object is not evidence that the remote serves it",
			sha, source.remote,
		),
	}
}

// sessionSourceContainedInTips answers "is this commit reachable from any of these tips" in ONE
// traversal instead of one subprocess per tip: rev-list excludes everything the tips reach, so
// empty output means at least one of them excluded the commit. --ignore-missing skips a tip this
// cache has never fetched, which is simply not usable as evidence — never a reason to go get it.
func sessionSourceContainedInTips(ctx context.Context, repository, sha string, tips []string) (bool, error) {
	args := append([]string{"rev-list", "--max-count=1", "--ignore-missing", sha, "--not"}, tips...)
	out, truncated, err := runSessionWorkspaceGitWithEnvContext(
		ctx, repository, sessionWorkspaceGitOutputLimit, nil, args...,
	)
	if err != nil {
		return false, err
	}
	if truncated {
		return false, errors.New("advertised ancestry output exceeds bounds")
	}
	return strings.TrimSpace(string(out)) == "", nil
}

func sourceDisplay(source sessionRepositorySource) string {
	if source.ref != "" {
		return source.ref
	}
	return source.branch
}

func repositoryFreshnessReceipt(name, requested, resolved, remote, staleStatus, staleRevision string) session.RepositoryFreshnessReceipt {
	return session.RepositoryFreshnessReceipt{
		Version: 2, Name: strings.ReplaceAll(name, " ", "_"), RequestedRevision: requested, ResolvedRevision: resolved,
		FetchedAt: time.Now().UTC(), RemoteIdentity: remote,
		StaleBaseStatus: staleStatus, StaleBaseRevision: staleRevision,
	}
}

func localSourceRevision(ctx context.Context, source sessionRepositorySource) string {
	if source.expected != "" {
		return source.expected
	}
	if source.branch == "" {
		return ""
	}
	commit, err := sessionWorkspaceCommitContext(ctx, source.repository, "refs/remotes/"+source.remote+"/"+source.branch)
	if err != nil {
		return ""
	}
	return commit
}

func staleBaseEvidence(previous, resolved string) (string, string) {
	if previous == "" {
		return "unknown", ""
	}
	if previous == resolved {
		return "current", previous
	}
	return "stale", previous
}

func repositoryUnavailable(
	source sessionRepositorySource,
	display string,
	commit string,
	cause error,
) error {
	public := fmt.Sprintf(
		"workspace preparation could not refresh the configured %s repository from %s/%s; no model session was created",
		source.label, source.remote, display,
	)
	internal := fmt.Sprintf("refresh %s repository from %s/%s", source.label, source.remote, display)
	guidance := "check the remote, branch, network, and Git credentials"
	if commit != "" {
		internal += " at " + commit
		guidance = "check the network and Git credentials"
	}
	return errors.Join(
		&session.Error{Code: session.CodeRepositoryUnavailable, Detail: public},
		fmt.Errorf("%s: %w; %s", internal, cause, guidance),
	)
}

func (s *Service) pinCurrentSessionParent(
	ctx context.Context,
	sess session.Session,
) (string, error) {
	policy, ok := s.policies[sess.Policy]
	if !ok || resolvedSessionPolicyDigest(policy) != sess.PolicyDigest {
		return "", &session.Error{
			Code:   session.CodeInvalidSessionState,
			Detail: "session policy no longer matches the operator policy; start a new session",
		}
	}
	return pinSessionRepository(ctx, sessionRepositorySource{
		label: "primary", repository: policy.Repository,
		remote: policy.Remote, branch: policy.Branch,
	})
}

// pinDiscardSessionParent pins the parent commit a discard plan is judged against.
//
// Unlike pinCurrentSessionParent it does not require the operator policy to still describe the
// session. The digest guard exists to stop a drifted policy from steering a RUNNING session;
// teardown steers nothing, and refusing it leaves the one outcome worse than either policy: a
// workspace nobody can ever reclaim. An operator editing a policy target orphaned every in-flight
// session exactly that way — cleanup retried into a permanent failure while the forks leaked.
//
// When the policy still matches, the plan keeps full fidelity (remote-pinned parent). When it has
// drifted, the parent falls back to the session's own durable repository binding at its local
// HEAD — the legacy pre-remote behavior. The dirty and unmerged safety checks still run against
// that parent, so committed-but-unpublished work still blocks an unforced discard.
func (s *Service) pinDiscardSessionParent(
	ctx context.Context,
	sess session.Session,
) (string, error) {
	if policy, ok := s.policies[sess.Policy]; ok &&
		resolvedSessionPolicyDigest(policy) == sess.PolicyDigest {
		return pinSessionRepository(ctx, sessionRepositorySource{
			label: "primary", repository: policy.Repository,
			remote: policy.Remote, branch: policy.Branch,
		})
	}
	return pinSessionRepository(ctx, sessionRepositorySource{
		label: "primary", repository: sess.Repository,
	})
}

func runSessionSourceGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	stdout := &sessionWorkspaceLimitedWriter{limit: sessionWorkspaceGitOutputLimit}
	stderr := &sessionWorkspaceLimitedWriter{limit: sessionWorkspaceErrorLimit}
	cmd, err := forkspace.GitCommand(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, errors.Join(ctx.Err(), fmt.Errorf(
					"git %s exceeded its deadline", strings.Join(args, " "),
				))
			}
			return nil, errors.Join(ctx.Err(), err)
		}
		detail := strings.TrimSpace(stderr.buf.String())
		if detail != "" {
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, detail)
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if stdout.truncated {
		return nil, fmt.Errorf("git %s output exceeds %d bytes", strings.Join(args, " "), sessionWorkspaceGitOutputLimit)
	}
	return stdout.buf.Bytes(), nil
}
