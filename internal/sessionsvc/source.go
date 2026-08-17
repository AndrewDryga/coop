package sessionsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
)

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
}

type sessionSourceGitRunner func(context.Context, string, ...string) ([]byte, error)

func pinSessionPolicyRepositories(
	ctx context.Context,
	policy Policy,
	pullRequest *RemotePullRequestBinding,
) (sessionPolicyPins, error) {
	sources := make([]sessionRepositorySource, 1+len(policy.Companions))
	sources[0] = sessionRepositorySource{
		label: "primary", repository: policy.Repository,
		remote: policy.Remote, branch: policy.Branch,
	}
	if pullRequest != nil {
		if policy.Remote == "" {
			return sessionPolicyPins{}, &session.Error{
				Code:   session.CodeInvalidRequest,
				Detail: "pull request sessions require an operator-configured remote",
			}
		}
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
			commit, err := pinSessionRepository(ctx, source)
			if err != nil {
				select {
				case errs <- err:
				default:
				}
				cancel()
				return
			}
			commits[index] = commit
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
		creationBase: commits[0], workspaceHead: commits[0], companions: commits[1:],
	}
	if pullRequest == nil {
		return result, nil
	}
	pullHead, err := pinSessionRepository(ctx, sessionRepositorySource{
		label: "pull request", repository: policy.Repository, remote: policy.Remote,
		branch: policy.Branch, ref: fmt.Sprintf("refs/pull/%d/head", pullRequest.Number),
		expected: pullRequest.HeadCommit,
	})
	if err != nil {
		return sessionPolicyPins{}, err
	}
	mergeBase, truncated, err := runSessionWorkspaceGitWithEnvContext(
		ctx, policy.Repository, 4<<10, nil,
		"merge-base", result.creationBase, pullHead,
	)
	if err != nil {
		return sessionPolicyPins{}, fmt.Errorf("resolve pull request merge base: %w", err)
	}
	if truncated {
		return sessionPolicyPins{}, errors.New("pull request merge base exceeds bounds")
	}
	result.creationBase = strings.TrimSpace(string(mergeBase))
	if !validSessionWorkspaceCommit(result.creationBase) {
		return sessionPolicyPins{}, errors.New("pull request merge base is invalid")
	}
	result.workspaceHead = pullHead
	return result, nil
}

func pinSessionRepository(ctx context.Context, source sessionRepositorySource) (string, error) {
	return pinSessionRepositoryWithTimeouts(
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
	if source.remote == "" {
		commit, err := sessionWorkspaceCommitContext(ctx, source.repository, "HEAD")
		if err != nil {
			return "", fmt.Errorf("pin %s repository HEAD: %w", source.label, err)
		}
		return commit, nil
	}

	lookupCtx, cancelLookup := context.WithTimeout(ctx, lookupTimeout)
	ref := source.ref
	if ref == "" {
		ref = "refs/heads/" + source.branch
	}
	out, err := runGit(lookupCtx, source.repository,
		"ls-remote", "--exit-code", "--refs", "--", source.remote, ref)
	cancelLookup()
	if err != nil {
		var exitErr *exec.ExitError
		if source.ref != "" && errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
			return "", &session.Error{
				Code: session.CodeInvalidRequest,
				Detail: fmt.Sprintf(
					"pull request ref %s does not exist on the operator-configured remote; create a fresh task for an open pull request",
					ref,
				),
			}
		}
		display := source.branch
		if source.ref != "" {
			display = source.ref
		}
		return "", repositoryUnavailable(source, display, "", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 || fields[1] != ref || !validSessionWorkspaceCommit(fields[0]) {
		display := source.branch
		if source.ref != "" {
			display = source.ref
		}
		return "", fmt.Errorf(
			"refresh %s repository from %s/%s: remote returned an invalid branch identity",
			source.label, source.remote, display,
		)
	}
	commit := fields[0]
	if source.expected != "" && commit != source.expected {
		return "", &session.Error{
			Code: session.CodeInvalidRequest,
			Detail: fmt.Sprintf(
				"pull request head changed before session creation; expected %s, found %s; create a fresh task for the current revision",
				source.expected, commit,
			),
		}
	}
	fetchCtx, cancelFetch := context.WithTimeout(ctx, fetchTimeout)
	_, err = runGit(fetchCtx, source.repository,
		"fetch", "--quiet", "--no-write-fetch-head", "--no-tags", "--", source.remote, commit)
	cancelFetch()
	if err != nil {
		display := source.branch
		if source.ref != "" {
			display = source.ref
		}
		return "", repositoryUnavailable(source, display, commit, err)
	}
	resolved, err := sessionWorkspaceCommitContext(ctx, source.repository, commit)
	if err != nil {
		return "", fmt.Errorf("verify refreshed %s repository commit %s: %w", source.label, commit, err)
	}
	return resolved, nil
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
	cmd := exec.CommandContext(ctx, "git", gitArgs(dir, args)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
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
