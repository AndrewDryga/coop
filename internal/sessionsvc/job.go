package sessionsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// Network receipts predate jobs. Preserve their historical authority when inspecting old rows.
func sessionNetworkAuthorityDigest(bound session.Session) string {
	if bound.JobDigest != "" {
		return bound.JobDigest
	}
	return bound.AuthorityDigest
}

func (s *Service) sessionExecution(ctx context.Context, bound session.Session) (executionConfig, error) {
	if pending, err := s.store.HasPendingWorkspaceRestore(ctx, bound.ID, ""); err != nil || pending {
		return executionConfig{}, errors.Join(err, errors.New("workspace restore must finish before execution"))
	}
	if bound.JobDigest == "" || len(bound.JobDocument) == 0 {
		return executionConfig{}, errors.New("session has no durable job authority")
	}
	job, err := workerproto.DecodeJobSpec(bound.JobDocument)
	if err != nil {
		return executionConfig{}, fmt.Errorf("decode session job authority: %w", err)
	}
	digest, err := job.Digest()
	if err != nil || digest != bound.JobDigest {
		return executionConfig{}, errors.New("session job authority digest is invalid")
	}
	policy, err := s.resolveJobExecution(ctx, job)
	if err != nil {
		return executionConfig{}, err
	}
	if job.JobRef != bound.JobRef || policy.Repository != bound.Repository ||
		string(policy.Mode) != bound.Mode || policy.RepositoryReadOnly != bound.RepositoryReadOnly ||
		policy.OmitEnv == bound.ProjectEnv || policy.OmitMCP == bound.ProjectMCP ||
		policy.MaxQueuedTurns != bound.MaxQueuedTurns || policy.MaxQueuedBytes != bound.MaxQueuedBytes ||
		policy.TurnTimeout != bound.TurnTimeout || policy.MaxPatchBytes != bound.MaxPatchBytes ||
		string(policy.Egress.Mode) != bound.NetworkMode {
		return executionConfig{}, errors.New("session no longer matches its frozen job authority")
	}
	var source *session.SourceBinding
	base := ""
	if job.Source != nil {
		source, base = &job.Source.Binding, job.Source.Binding.BaseCommit
	} else if job.Mode != "bare" {
		base = emptyJobCommit
	}
	if !reflect.DeepEqual(source, bound.Source) || base != bound.BaseCommit || len(bound.Companions) != len(job.Companions) {
		return executionConfig{}, errors.New("session sources no longer match its frozen job authority")
	}
	for i, companion := range bound.Companions {
		expected := job.Companions[i]
		workspace, err := sessionCompanionWorkspace(s.store.Root(), bound.ID, expected.Name)
		if err != nil || companion.Name != expected.Name || companion.Repository != policy.Companions[i].Repository ||
			companion.Workspace != workspace || companion.BaseCommit != expected.Source.Binding.SelectedCommit {
			return executionConfig{}, errors.New("session companion no longer matches its frozen job authority")
		}
	}
	found := false
	for _, target := range policy.Targets {
		found = found || target.String() == bound.Target
	}
	if !found {
		return executionConfig{}, errors.New("session target is outside its frozen job authority")
	}
	return policy, nil
}

// resolveJobExecution converts authenticated controller authority to the service's execution snapshot.
// Source paths are derived from the private state root, never taken from the job document.
func (s *Service) resolveJobExecution(ctx context.Context, job workerproto.JobSpec) (executionConfig, error) {
	if err := job.Validate(); err != nil {
		return executionConfig{}, err
	}
	policy := executionConfig{
		Mode: agents.ExecutionMode(job.Mode), OmitEnv: true, OmitMCP: true,
		RepositoryReadOnly: job.RepositoryReadOnly,
		Egress:             executionNetwork{Mode: egress.Mode(job.Egress.Mode), ExportDestinations: job.Egress.ExportDestinations},
		MaxTurns:           job.Limits.MaxTurns, MaxQueuedTurns: job.Limits.MaxQueuedTurns,
		MaxQueuedBytes: job.Limits.MaxQueuedBytes, TurnTimeout: time.Duration(job.Limits.TurnTimeoutMS) * time.Millisecond,
		WarmIdleTimeout: time.Duration(job.Limits.WarmIdleTimeoutMS) * time.Millisecond,
		MaxPatchBytes:   job.Limits.MaxPatchBytes,
	}
	targets, err := jobTargets(job)
	if err != nil {
		return executionConfig{}, err
	}
	policy.Targets = targets
	rules, err := jobEgressRules(job)
	if err != nil {
		return executionConfig{}, err
	}
	policy.Egress.Rules = rules
	if job.Source != nil {
		policy.Repository, err = stagedJobRepository(ctx, s.store.Root(), *job.Source)
	} else if job.Mode != "bare" {
		policy.Repository, err = emptyJobRepository(ctx, s.store.Root())
	}
	if err != nil {
		return executionConfig{}, err
	}
	for _, companion := range job.Companions {
		repository, err := stagedJobRepository(ctx, s.store.Root(), companion.Source)
		if err != nil {
			return executionConfig{}, fmt.Errorf("job companion %s: %w", companion.Name, err)
		}
		policy.Companions = append(policy.Companions, executionCompanion{Name: companion.Name, Repository: repository})
	}
	return policy, nil
}

func jobTargets(job workerproto.JobSpec) ([]agents.Target, error) {
	targets := make([]agents.Target, 0, len(job.Targets))
	for _, target := range job.Targets {
		parsed, err := agents.ParseTarget(target)
		if err != nil || len(parsed.Accounts) > 1 || parsed.String() != target {
			return nil, fmt.Errorf("invalid job target %q", target)
		}
		targets = append(targets, parsed)
	}
	return targets, nil
}

func jobEgressRules(job workerproto.JobSpec) ([]egress.Rule, error) {
	rules := make([]egress.Rule, 0, len(job.Egress.Rules))
	for _, rule := range job.Egress.Rules {
		if rule.To.Service != "" {
			return nil, errors.New("controller job service grants need explicit service authority")
		}
		rules = append(rules, egress.Rule{
			To: egress.Destination{
				Domain: rule.To.Domain, IP: rule.To.IP, CIDR: rule.To.CIDR,
				Service: rule.To.Service, Provider: rule.To.Provider, Features: rule.To.Features,
			},
			Protocol: rule.Protocol, Ports: rule.Ports, Types: rule.Types, Codes: rule.Codes,
		})
	}
	normalized, err := egress.NormalizeRules(rules)
	if err != nil || len(rules) != 0 && !reflect.DeepEqual(normalized, rules) {
		return nil, errors.New("job egress rules are invalid or noncanonical")
	}
	for _, rule := range normalized {
		if rule.To.Provider == "" {
			if err := egress.SupportedRule(rule); err != nil {
				return nil, fmt.Errorf("unsupported job egress rule: %w", err)
			}
		}
	}
	if len(normalized) == 0 {
		return nil, nil
	}
	return normalized, nil
}

// The connector publishes this receipt only after fetching the frozen source.
func stagedJobRepository(ctx context.Context, stateRoot string, source workerproto.JobSource) (string, error) {
	key, err := source.StagingKey()
	if err != nil {
		return "", err
	}
	directory := filepath.Join(stateRoot, "job-sources", key)
	for _, path := range []string{directory, filepath.Join(directory, "repository")} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("job source is not staged in private worker state")
		}
	}
	receipt := filepath.Join(directory, "source.json")
	info, err := os.Lstat(receipt)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 256<<10 {
		return "", errors.New("job source receipt is missing or changed")
	}
	document, err := os.ReadFile(receipt)
	var recorded workerproto.JobSource
	if err != nil || len(document) > 256<<10 || json.Unmarshal(document, &recorded) != nil || !reflect.DeepEqual(recorded, source) {
		return "", errors.New("job source receipt does not match the requested source")
	}
	repository := filepath.Join(directory, "repository")
	commit, err := sessionWorkspaceCommitContext(ctx, repository, "HEAD")
	if err != nil || commit != source.Binding.SelectedCommit {
		return "", errors.New("staged job repository HEAD does not match its exact commit")
	}
	tree, err := sessionWorkspaceTree(repository, commit)
	if err != nil || tree != source.Binding.AdmittedTree {
		return "", errors.New("staged job repository tree does not match")
	}
	if err := verifyJobSubmoduleManifest(ctx, repository, commit, source.Submodules); err != nil {
		return "", err
	}
	return repository, nil
}

func jobRepositoryFreshness(job workerproto.JobSpec, createdAt time.Time) []session.RepositoryFreshnessReceipt {
	if job.Mode == "bare" {
		return nil
	}
	receipt := func(name string, source workerproto.JobSource, selected bool) session.RepositoryFreshnessReceipt {
		binding := source.Binding
		requested, resolved := binding.DefaultRef, binding.DefaultCommit
		if selected {
			requested, resolved = binding.SelectedCommit, binding.SelectedCommit
			if binding.SelectedRef != nil {
				requested = *binding.SelectedRef
			}
		}
		return session.RepositoryFreshnessReceipt{
			Version: 2, Name: name, RequestedRevision: requested, ResolvedRevision: resolved,
			FetchedAt: binding.ResolvedAt, RemoteIdentity: binding.RemoteIdentity,
			StaleBaseStatus: "not_applicable",
		}
	}
	primary := session.RepositoryFreshnessReceipt{
		Version: 2, Name: "primary", RequestedRevision: "HEAD", ResolvedRevision: emptyJobCommit,
		WorkspaceBaseRevision: emptyJobCommit, FetchedAt: createdAt, RemoteIdentity: "local",
		StaleBaseStatus: "not_applicable",
	}
	if job.Source != nil {
		primary = receipt("primary", *job.Source, false)
		primary.WorkspaceBaseRevision = job.Source.Binding.BaseCommit
	}
	result := []session.RepositoryFreshnessReceipt{primary}
	if job.Source != nil && job.Source.Binding.Kind != session.SourceDefault {
		result = append(result, receipt(sessionSourceLabel, *job.Source, true))
	}
	for _, companion := range job.Companions {
		result = append(result, receipt(companion.Name, companion.Source, true))
	}
	return result
}
