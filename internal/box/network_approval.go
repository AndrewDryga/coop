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
	PostureFromApproval = "remembered approval"
	PostureFromHost     = "COOP_EGRESS"
	PostureFromProject  = "project request"
	PostureFromDefault  = "built-in default"
)

// NetworkPosture is what a project may reach and what it is currently asking
// for. Reading it creates NOTHING — no authority root, no owner key, no
// approval — so a host that never ran a filtered box reports that fact instead
// of growing the state it would then describe.
type NetworkPosture struct {
	Project string
	Mode    egress.Mode
	Source  string
	// Approval is the remembered host-owned decision, nil when this project was
	// never approved. Requested is the repo's current ask; Add and Remove are
	// the difference a human would review.
	Approval  *networkstate.Approval
	Requested []egress.Rule
	Add       []egress.Rule
	Remove    []egress.Rule
	// Setup is this host's newest preflight record, nil when `coop net setup`
	// has never completed here.
	Setup *networkstate.Qualification
	// Pending is why a launch would refuse right now — a request outside the
	// remembered approval. Describing that is the point of this view, so it is
	// reported here rather than raised as an error nobody can act on.
	Pending error
}

// SetupCurrent reports whether the newest host record was made by a coop that
// accepts what this one does. A record from another contract is a record, not
// a launch capability.
func (p NetworkPosture) SetupCurrent() bool {
	return p.Setup != nil && p.Setup.Contract == networkstate.QualificationContract
}

// ProjectNetworkPosture answers `coop net` for one project. It reads the same
// inputs admission does and resolves the mode through the same code, so the
// posture shown is the posture a launch would get.
func ProjectNetworkPosture(ctx context.Context, cfg *config.Config, repo string) (NetworkPosture, error) {
	if ctx == nil || cfg == nil {
		return NetworkPosture{}, errors.New("network posture requires host configuration and a cancelable context")
	}
	canonical, p, root, exposed, input, err := networkProjectInputs(cfg, repo)
	if err != nil {
		return NetworkPosture{}, err
	}
	out := NetworkPosture{Project: canonical, Requested: p.Box.EgressRules, Source: PostureFromDefault}
	preview, err := networkstate.PreviewAdmission(root, canonical, exposed, input)
	if err != nil {
		return NetworkPosture{}, err
	}
	out.Mode, out.Pending = preview.Mode, preview.Pending
	store, err := networkstate.OpenExisting(root, nil)
	if errors.Is(err, fs.ErrNotExist) {
		out.Source, out.Add = postureSource(input, nil), out.Requested
		return out, nil
	}
	if err != nil {
		return NetworkPosture{}, err
	}
	defer store.Close()
	if out.Approval, err = store.Approval(canonical); err != nil {
		return NetworkPosture{}, err
	}
	out.Source = postureSource(input, out.Approval)
	out.Add, out.Remove = NetworkRuleDiff(approvedEnvelope(out.Approval), out.Requested)
	// A drifted service is a pending change like any other: the rule still reads
	// the same, but what it reaches does not.
	if out.Pending == nil {
		out.Pending = checkApprovedServices(out.Approval, ComposeFileAt(repo, p.ComposeRel()), repo, false)
	}
	records, err := store.Qualifications(ctx)
	if err != nil {
		return NetworkPosture{}, err
	}
	if len(records) > 0 {
		out.Setup = &records[0] // Qualifications sorts newest first.
	}
	return out, nil
}

// postureSource names the input that decided the mode. It mirrors
// Admission.resolveMode's order exactly; a remembered approval outranks the
// repo precisely so deleting YAML cannot restore the open default.
func postureSource(input networkstate.Admission, approval *networkstate.Approval) string {
	switch {
	case approval != nil:
		return PostureFromApproval
	case input.HostPreference != nil:
		return PostureFromHost
	case input.ProjectMode != nil, len(input.Requests) != 0:
		return PostureFromProject
	default:
		return PostureFromDefault
	}
}

// ProjectNetworkApproval is a host-only review capability. It holds the exact
// before/after an operator is shown plus the digest that binds that view, so a
// decision can never be applied to a request that changed underneath it.
type ProjectNetworkApproval struct {
	store    *networkstate.Store
	project  string
	mode     egress.Mode
	requests []egress.Rule
	services map[string]string
	review   networkstate.ApprovalReview
	used     bool
}

// ReviewProjectNetwork prepares the approval a human confirms. It reads the
// repository ONCE: Commit posts back the exact rules that were displayed, never
// a fresh read of a file an agent could have rewritten during the prompt.
func ReviewProjectNetwork(cfg *config.Config, repo string, explicit *egress.Mode) (_ *ProjectNetworkApproval, err error) {
	if cfg == nil {
		return nil, errors.New("network approval requires host configuration")
	}
	if explicit != nil {
		if _, err := egress.ParseMode(string(*explicit)); err != nil {
			return nil, err
		}
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
	before, err := store.Approval(canonical)
	if err != nil {
		return nil, err
	}
	mode := approvalMode(input, before, explicit)
	// The reviewed Compose definition is part of the decision: a `service:` rule
	// approved by name alone would follow whatever that name later points at.
	services, err := composeServiceDigests(ComposeFileAt(repo, p.ComposeRel()), repo, false, requestedServices(p.Box.EgressRules))
	if err != nil {
		return nil, err
	}
	review, err := store.ReviewApproval(canonical, mode, p.Box.EgressRules, nil, services)
	if err != nil {
		return nil, err
	}
	return &ProjectNetworkApproval{store: store, project: canonical, mode: mode, requests: p.Box.EgressRules, services: services, review: review}, nil
}

func (a *ProjectNetworkApproval) Project() string                { return a.project }
func (a *ProjectNetworkApproval) Mode() egress.Mode              { return a.mode }
func (a *ProjectNetworkApproval) Before() *networkstate.Approval { return a.review.Before }
func (a *ProjectNetworkApproval) After() *networkstate.Approval  { return a.review.After }

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

// approvalMode picks the posture the review proposes. An explicit --mode wins;
// otherwise the repo's own request is what the operator is being asked about,
// and a project with no opinion keeps the posture already remembered.
func approvalMode(input networkstate.Admission, before *networkstate.Approval, explicit *egress.Mode) egress.Mode {
	switch {
	case explicit != nil:
		return *explicit
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
// configuration and the admission inputs both the posture view and the approval
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
	return canonical, p, root, exposed, input, nil
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
			return fmt.Errorf("the Compose service %q changed since it was approved — review it with 'coop net approve'", name)
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
