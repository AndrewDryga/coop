package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/AndrewDryga/coop/internal/session"
)

type sessionRepositorySource struct {
	label      string
	repository string
	remote     string
	branch     string
}

func pinSessionPolicyRepositories(ctx context.Context, policy SessionPolicy) (string, []string, error) {
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
		return "", nil, err
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	return commits[0], commits[1:], nil
}

func pinSessionRepository(ctx context.Context, source sessionRepositorySource) (string, error) {
	if source.remote == "" {
		commit, err := sessionWorkspaceCommit(source.repository, "HEAD")
		if err != nil {
			return "", fmt.Errorf("pin %s repository HEAD: %w", source.label, err)
		}
		return commit, nil
	}

	remoteCtx, cancel := context.WithTimeout(ctx, sessionPolicyRemoteTimeout)
	defer cancel()
	ref := "refs/heads/" + source.branch
	out, err := runSessionSourceGit(remoteCtx, source.repository,
		"ls-remote", "--exit-code", "--refs", "--", source.remote, ref)
	if err != nil {
		return "", fmt.Errorf(
			"refresh %s repository from %s/%s: %w; check the remote, branch, network, and Git credentials",
			source.label, source.remote, source.branch, err,
		)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 || fields[1] != ref || !validSessionWorkspaceCommit(fields[0]) {
		return "", fmt.Errorf(
			"refresh %s repository from %s/%s: remote returned an invalid branch identity",
			source.label, source.remote, source.branch,
		)
	}
	commit := fields[0]
	if _, err := runSessionSourceGit(remoteCtx, source.repository,
		"fetch", "--quiet", "--no-write-fetch-head", "--no-tags", "--", source.remote, commit); err != nil {
		return "", fmt.Errorf(
			"refresh %s repository from %s/%s at %s: %w; check the network and Git credentials",
			source.label, source.remote, source.branch, commit, err,
		)
	}
	resolved, err := sessionWorkspaceCommit(source.repository, commit)
	if err != nil {
		return "", fmt.Errorf("verify refreshed %s repository commit %s: %w", source.label, commit, err)
	}
	return resolved, nil
}

func (s *SessionService) pinCurrentSessionParent(
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

func runSessionSourceGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	stdout := &sessionWorkspaceLimitedWriter{limit: sessionWorkspaceGitOutputLimit}
	stderr := &sessionWorkspaceLimitedWriter{limit: sessionWorkspaceErrorLimit}
	cmd := exec.CommandContext(ctx, "git", gitArgs(dir, args)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("git %s timed out after %s", strings.Join(args, " "), sessionPolicyRemoteTimeout)
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
