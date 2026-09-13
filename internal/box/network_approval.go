package box

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
)

// Posture sources, in the order admission resolves them. They are display
// labels for one decision, never a second precedence ladder.
const (
	AccessFromApproval = "remembered approval"
	AccessFromHost     = "COOP_EGRESS"
	AccessFromProject  = "project request"
	AccessFromDefault  = "built-in default"
)

// NetworkAccess is what a project may reach and what it is currently asking
// for. Reading it creates NOTHING — no authority root, no owner key, no
// approval — so a host that never ran a filtered box reports that fact instead
// of growing the state it would then describe.
type NetworkAccess struct {
	Project string
	Mode    egress.Mode
	Source  string
	// Approval is the remembered host-owned decision, nil when this project was
	// never approved. Requested and RequestedMode are the repo's current ask —
	// exactly what `coop approve` would write; Add and Remove are the rule
	// difference a human would review.
	Approval      *networkstate.Approval
	Requested     []egress.Rule
	RequestedMode egress.Mode
	Add           []egress.Rule
	Remove        []egress.Rule
	// Pending is why a launch would refuse right now — a request that is not
	// what was approved. Describing that is the point of this view, so it is
	// reported here rather than raised as an error nobody can act on. It is the
	// same check every launch and `coop init` make.
	Pending *networkstate.PendingApproval
}

// ProjectNetworkAccess answers `coop net` for one project. It reads the same
// inputs admission does and resolves the mode through the same code, so the
// access shown is the access a launch would get. Host setup is not part of
// it: a launch performs that itself, so its state is machinery, not a decision.
func ProjectNetworkAccess(ctx context.Context, cfg *config.Config, repo string) (NetworkAccess, error) {
	if ctx == nil || cfg == nil {
		return NetworkAccess{}, errors.New("network access requires host configuration and a cancelable context")
	}
	canonical, p, root, exposed, input, err := networkProjectInputs(cfg, repo)
	if err != nil {
		return NetworkAccess{}, err
	}
	out := NetworkAccess{Project: canonical, Requested: p.Box.EgressRules, Source: AccessFromDefault}
	preview, err := networkstate.PreviewAdmission(root, canonical, exposed, input)
	if err != nil {
		return NetworkAccess{}, err
	}
	out.Mode, out.Pending = preview.Mode, preview.Pending
	store, err := networkstate.OpenExisting(root, nil)
	if errors.Is(err, fs.ErrNotExist) {
		out.Source, out.Add, out.RequestedMode = postureSource(input, nil), out.Requested, approvalMode(input, nil)
		return out, nil
	}
	if err != nil {
		return NetworkAccess{}, err
	}
	defer store.Close()
	if out.Approval, err = store.Approval(canonical); err != nil {
		return NetworkAccess{}, err
	}
	out.Source, out.RequestedMode = postureSource(input, out.Approval), approvalMode(input, out.Approval)
	out.Add, out.Remove = NetworkRuleDiff(approvedEnvelope(out.Approval), out.Requested)
	return out, nil
}

// postureSource names the input that decided the mode. It mirrors
// Admission.resolveMode's order exactly; a remembered approval outranks the
// repo precisely so deleting YAML cannot restore the open default.
func postureSource(input networkstate.Admission, approval *networkstate.Approval) string {
	switch {
	case approval != nil:
		return AccessFromApproval
	case input.HostPreference != nil:
		return AccessFromHost
	case input.ProjectMode != nil, len(input.Requests) != 0:
		return AccessFromProject
	default:
		return AccessFromDefault
	}
}

// ProjectNetworkApproval is a host-only review capability. It holds the exact
// before/after an operator is shown plus the digest that binds that view, so a
// decision can never be applied to a request that changed underneath it.
type ProjectNetworkApproval struct {
	store     *networkstate.Store
	project   string
	mode      egress.Mode
	requests  []egress.Rule
	services  map[string]string
	review    networkstate.ApprovalReview
	unchanged bool
	used      bool
}

// ReviewProjectNetwork prepares the approval a human confirms. The mode and
// the rules come from .agent/project.yaml and nowhere else: to change access,
// edit the file and review it again. It reads the repository ONCE: Commit posts
// back the exact rules that were displayed, never a fresh read of a file an
// agent could have rewritten during the prompt.
//
// A request that is already exactly what was approved — the same check every
// launch makes — returns a review with nothing to decide, before any authority
// state exists: a no-op must not create an owner key or refresh a record.
func ReviewProjectNetwork(cfg *config.Config, repo string) (_ *ProjectNetworkApproval, err error) {
	if cfg == nil {
		return nil, errors.New("network approval requires host configuration")
	}
	canonical, p, root, exposed, input, err := networkProjectInputs(cfg, repo)
	if err != nil {
		return nil, err
	}
	for _, rule := range p.Box.EgressRules {
		if rule.To.Provider != "" {
			return nil, errors.New("this release has no optional provider features to approve — an agent's core endpoints are allowed automatically, so drop the provider rule")
		}
	}
	// The same capability gate a launch applies, applied BEFORE the rule is
	// remembered: an approval every launch would refuse by name is not a
	// decision worth storing, and the operator finds out now instead of at the
	// next unattended run.
	if err := checkSupportedRequests(input); err != nil {
		return nil, err
	}
	// Launch posture deliberately prefers a remembered restriction over a project edit. Approval
	// review is the one place that must compare the replacement requested posture instead, or an
	// offline approval can never be changed to filtered without first deleting it.
	var before *networkstate.Approval
	if existing, openErr := networkstate.OpenExisting(root, exposed); openErr == nil {
		before, err = existing.Approval(canonical)
		err = errors.Join(err, existing.Close())
		if err != nil {
			return nil, err
		}
	} else if !errors.Is(openErr, fs.ErrNotExist) {
		return nil, openErr
	}
	modeChanged := before != nil && approvalMode(input, before) != before.Posture
	if !modeChanged {
		preview, previewErr := networkstate.PreviewAdmission(root, canonical, exposed, input)
		if previewErr != nil {
			return nil, previewErr
		}
		if preview.Pending == nil {
			return &ProjectNetworkApproval{project: canonical, unchanged: true}, nil
		}
	}
	// Approve is the explicit host operation that may create the authority
	// root: a launch never does, so this is where an owner key is born.
	store, err := networkstate.Open(root, exposed)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, store.Close())
		}
	}()
	before, err = store.Approval(canonical)
	if err != nil {
		return nil, err
	}
	mode := approvalMode(input, before)
	review, err := store.ReviewApproval(canonical, mode, p.Box.EgressRules, nil, input.Services)
	if err != nil {
		return nil, err
	}
	return &ProjectNetworkApproval{store: store, project: canonical, mode: mode, requests: p.Box.EgressRules, services: input.Services, review: review}, nil
}

func (a *ProjectNetworkApproval) Project() string                { return a.project }
func (a *ProjectNetworkApproval) Mode() egress.Mode              { return a.mode }
func (a *ProjectNetworkApproval) Before() *networkstate.Approval { return a.review.Before }
func (a *ProjectNetworkApproval) After() *networkstate.Approval  { return a.review.After }

// Unchanged reports a request that is exactly what was approved already: there
// is no security question to ask, and nothing was written to find that out.
func (a *ProjectNetworkApproval) Unchanged() bool { return a != nil && a.unchanged }

// Commit writes the reviewed approval. The store rechecks the digest, so a
// request or a stored approval that moved during the prompt fails instead of
// remembering something nobody saw.
func (a *ProjectNetworkApproval) Commit(ctx context.Context) error {
	if a == nil || a.store == nil || a.used {
		return errors.New("this approval was already answered")
	}
	a.used = true
	return a.store.Approve(ctx, a.project, a.mode, a.requests, nil, a.services, a.review.Digest)
}

func (a *ProjectNetworkApproval) Close() error {
	if a == nil || a.store == nil {
		return nil
	}
	store := a.store
	a.store = nil
	return store.Close()
}

// approvalMode is the access the repo's request asks about: the mode it names,
// else filtered when it names rules, and a project with no opinion keeps the
// access already remembered. It mirrors the pending check's own derivation.
func approvalMode(input networkstate.Admission, before *networkstate.Approval) egress.Mode {
	switch {
	case input.ProjectMode != nil:
		return *input.ProjectMode
	case len(input.Requests) != 0:
		return egress.Filtered
	case before != nil:
		return before.Posture
	default:
		return egress.Filtered
	}
}

// networkProjectInputs resolves the one canonical project identity, its
// configuration and the admission inputs both the access view and the approval
// review are built from, so neither can read a different project than a launch.
func networkProjectInputs(cfg *config.Config, repo string) (string, *project.Project, string, []string, networkstate.Admission, error) {
	fail := func(err error) (string, *project.Project, string, []string, networkstate.Admission, error) {
		return "", nil, "", nil, networkstate.Admission{}, err
	}
	canonical, err := canonicalProjectDir(repo)
	if err != nil {
		return fail(err)
	}
	p, err := project.Load(repo)
	if err != nil {
		return fail(err)
	}
	root, err := NetworkStatePath()
	if err != nil {
		return fail(err)
	}
	exposed, err := networkExposureRoots(cfg, RunSpec{Repo: repo})
	if err != nil {
		return fail(err)
	}
	input, err := networkAdmissionInput(cfg, p, NetworkAdmission{}, exposed)
	if err != nil {
		return fail(err)
	}
	if input.Services, err = requestedServiceDigests(repo, p, false); err != nil {
		return fail(err)
	}
	return canonical, p, root, exposed, input, nil
}

// requestedServiceDigests is the reviewed identity of every Compose service the
// project's rules name and its required dependencies. Dependencies are pinned
// for startup but do not become direct network grants.
func requestedServiceDigests(repo string, p *project.Project, repoReadOnly bool) (map[string]string, error) {
	return composeServiceDigests(ComposeFileAt(repo, p.ComposeRel()), repo, repoReadOnly, requestedServices(p.Box.EgressRules))
}

// requestedServices is every Compose service this request names, sorted, once.
func requestedServices(rules []egress.Rule) []string {
	var out []string
	for _, rule := range rules {
		if name := rule.To.Service; name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// checkApprovedServices recomputes the digest of every approved service from the
// Compose file a launch (or this project's current tree) would actually use.
func checkApprovedServices(approval *networkstate.Approval, composeFile, repoRoot string, repoReadOnly bool) error {
	if approval == nil || len(approval.Services) == 0 {
		return nil
	}
	names := make([]string, 0, len(approval.Services))
	for name := range approval.Services {
		names = append(names, name)
	}
	slices.Sort(names)
	digests, err := composeServiceDigests(composeFile, repoRoot, repoReadOnly, names)
	if err != nil {
		return err
	}
	for _, name := range names {
		if digests[name] != approval.Services[name] {
			return fmt.Errorf("the Compose service %q changed since it was approved — review it with 'coop approve'", name)
		}
	}
	return nil
}

func approvedEnvelope(approval *networkstate.Approval) []egress.Rule {
	if approval == nil {
		return nil
	}
	return approval.Envelope
}

// NetworkRuleDiff is a plain set difference over canonical rules: what the repo
// now asks for that is not approved, and what is approved that it no longer
// asks for. There is no ordering, precedence or merge here.
func NetworkRuleDiff(approved, requested []egress.Rule) (add, remove []egress.Rule) {
	key := func(rules []egress.Rule) []string {
		out := make([]string, 0, len(rules))
		for _, rule := range rules {
			data, _ := json.Marshal(rule)
			out = append(out, string(data))
		}
		return out
	}
	approvedKeys, requestedKeys := key(approved), key(requested)
	for i, k := range requestedKeys {
		if !slices.Contains(approvedKeys, k) {
			add = append(add, requested[i])
		}
	}
	for i, k := range approvedKeys {
		if !slices.Contains(requestedKeys, k) {
			remove = append(remove, approved[i])
		}
	}
	return add, remove
}
