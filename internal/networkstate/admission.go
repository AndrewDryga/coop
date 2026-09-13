package networkstate

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/egress"
)

// Admission separates presence from defaults and authority from requests. Only
// ProjectMode and Requests may originate in repository configuration. PolicyMode
// comes from a selected host-owned API policy, never from a create/turn request.
// Existing sessions load their captured snapshot instead of calling Admit again.
type Admission struct {
	InvocationMode *egress.Mode
	HostPreference *egress.Mode
	ProjectMode    *egress.Mode
	PolicyMode     *egress.Mode
	Requests       []egress.Rule
	// Services is the reviewed identity of each Compose service Requests name and its startup
	// dependencies, keyed by name. Only Requests are network grants.
	Services map[string]string
	Operator []egress.Input
	Bundles  []egress.Bundle
	// Automatic is host-captured shared MCP connectivity. Like provider core
	// dependencies it neither selects filtered mode nor creates offline exceptions.
	Automatic          []egress.Input
	ExportDestinations bool
}

// AdmissionPreview separates two questions a launch answers at once: which
// posture these inputs resolve to, and whether the project's current request
// is exactly what was approved. A read-only posture view needs them
// apart — a pending request is exactly what it exists to show, so refusing to
// describe the project would hide the one fact the operator came for.
type AdmissionPreview struct {
	Mode egress.Mode
	// Pending is non-nil when the request needs review before a launch. It is
	// never a reason to widen anything: Admit fails on the same condition.
	Pending *PendingApproval
}

// PendingApproval is the ONE condition every launch refuses on and every view
// reports: what .agent/project.yaml asks for is not what a human approved.
// Reason is a clause that reads after "cannot start because"; Cause is the same
// fact as a standalone sentence, for a view that states it on its own line.
// Neither carries the remedy, which is always the same — `coop net approve`.
type PendingApproval struct {
	Reason string
	Cause  string
}

func (p *PendingApproval) Error() string { return p.Reason }

// Sentence is the cause a view prints. A pending condition that never wrote one
// falls back to its own clause capitalized, so a new cause is readable before it
// is given its own sentence rather than silently blank.
func (p *PendingApproval) Sentence() string {
	switch {
	case p == nil:
		return ""
	case p.Cause != "":
		return p.Cause
	case p.Reason == "":
		return ""
	}
	sentence := strings.ToUpper(p.Reason[:1]) + p.Reason[1:]
	if !strings.HasSuffix(sentence, ".") {
		sentence += "."
	}
	return sentence
}

// pendingApproval compares the exact request — the mode the project names, its
// normalized rules and the reviewed identity of each service — with the stored
// approval. It is the same comparison `coop net approve` asks about, so a view
// that reports nothing pending and an approve that finds nothing to approve can
// never disagree. Without an approval only a widening needs a human: unrestricted
// access, or any rule at all. A project asking for filtered or offline access with
// no rules is inside what coop grants on its own.
//
// The mode the file names counts only where it would decide anything: an
// explicit --egress, COOP_EGRESS or session policy outranks it, so under one the
// file's "open" is moot and refusing the run over it would protect nothing.
func (a Admission) pendingApproval(approval *Approval) (*PendingApproval, error) {
	rules, err := egress.NormalizeRules(a.Requests)
	if err != nil {
		return nil, err
	}
	const asks = "this project asks for network access that has not been approved"
	const requests = ".agent/project.yaml requests changes to network access."
	overridden := a.InvocationMode != nil || a.HostPreference != nil || a.PolicyMode != nil
	if approval == nil {
		if a.ProjectMode != nil && *a.ProjectMode == egress.Open && !overridden {
			return &PendingApproval{Reason: "this project asks for unrestricted internet access, which has not been approved",
				Cause: ".agent/project.yaml requests unrestricted internet access."}, nil
		}
		if len(rules) != 0 {
			return &PendingApproval{Reason: asks, Cause: requests}, nil
		}
		return nil, nil
	}
	// A project that names no mode keeps the one that was approved; rules alone
	// imply filtered, exactly as they do when a launch resolves the mode.
	mode := approval.Posture
	switch {
	case overridden:
	case a.ProjectMode != nil:
		mode = *a.ProjectMode
	case len(rules) != 0:
		mode = egress.Filtered
	}
	if mode != approval.Posture || !sameRules(rules, approval.Envelope) {
		return &PendingApproval{Reason: asks, Cause: requests}, nil
	}
	if !maps.Equal(a.Services, approval.Services) {
		for name, digest := range approval.Services {
			if current, ok := a.Services[name]; ok && current != digest {
				return &PendingApproval{Reason: fmt.Sprintf("the Compose service %q changed since it was approved", name),
					Cause: fmt.Sprintf("The Compose service %q changed after its network access was approved.", name)}, nil
			}
		}
		return &PendingApproval{Reason: asks, Cause: requests}, nil
	}
	return nil, nil
}

// sameRules is set equality over canonical rules; both sides are normalized, so
// the JSON of a rule is its identity.
func sameRules(a, b []egress.Rule) bool {
	if len(a) != len(b) {
		return false
	}
	for _, rule := range a {
		if !slices.ContainsFunc(b, func(other egress.Rule) bool { return equalJSON(rule, other) }) {
			return false
		}
	}
	return true
}

// PreviewAdmission performs no publication and returns no authority handle. It
// lets ordinary launches inspect volume exposure before a first owner key is
// created. The caller must still Admit, and refuse if preparation changes mode.
func PreviewAdmission(path, project string, exposed []string, input Admission) (AdmissionPreview, error) {
	if err := CheckPathExposure(path, exposed); err != nil {
		return AdmissionPreview{}, err
	}
	store, err := openFiles(path, exposed, false)
	if errors.Is(err, os.ErrNotExist) {
		return input.preview(nil, false)
	}
	if err != nil {
		return AdmissionPreview{}, err
	}
	defer store.Close()
	if err := store.loadKey(false); err != nil {
		return AdmissionPreview{}, err
	}
	if store.key == nil {
		return input.preview(nil, false)
	}
	return store.admissionPreview(project, input)
}

// PreviewAdmissionMode is the launch caller's form: a pending request is a
// failure there, because admission is about to happen. The error is the
// *PendingApproval itself, so a caller can tell it from every other refusal.
func PreviewAdmissionMode(path, project string, exposed []string, input Admission) (egress.Mode, error) {
	preview, err := PreviewAdmission(path, project, exposed, input)
	if err != nil {
		return "", err
	}
	if preview.Pending != nil {
		return "", preview.Pending
	}
	return preview.Mode, nil
}

// admissionPreview is preparation only: it lets the host resolve posture before
// deriving any filtered dependency. Admit rereads approval and performs the
// actual authorization once those dependencies exist.
func (s *Store) admissionPreview(project string, input Admission) (AdmissionPreview, error) {
	if err := s.authorityAvailable(); err != nil {
		return AdmissionPreview{}, err
	}
	id, canonical, info, err := s.projectIdentity(project)
	if err != nil {
		return AdmissionPreview{}, err
	}
	approval, err := s.approval(id)
	if err != nil {
		return AdmissionPreview{}, err
	}
	withdrawn, err := s.withdrawn(id)
	if err != nil {
		return AdmissionPreview{}, err
	}
	preview, err := input.preview(approval, withdrawn)
	if err != nil {
		return AdmissionPreview{}, err
	}
	// A replaced project directory is exactly the pending review this view
	// exists to report: describing it beats failing the read nobody can act on.
	if drift := approval.checkDirectory(canonical, info); drift != nil {
		preview.Pending = drift
	}
	return preview, nil
}

func (a Admission) preview(approval *Approval, withdrawn bool) (AdmissionPreview, error) {
	mode, err := a.resolveMode(approval)
	if err != nil {
		return AdmissionPreview{}, err
	}
	pending, err := a.pendingApproval(approval)
	if err != nil {
		return AdmissionPreview{}, err
	}
	if pending == nil {
		pending = a.withdrawalBarrier(approval, withdrawn, mode)
	}
	return AdmissionPreview{Mode: mode, Pending: pending}, nil
}

// A bundle this host has never seen is pinned the first time an admission uses it, and every
// later admission must match that copy exactly (integrity drift, not an update).
const (
	pinFirstSeenBundles = true
	matchPinnedBundles  = false
)

// Admit is the shared new-run authority boundary. A concurrent approval change
// applies to subsequent captures.
func (s *Store) Admit(project string, input Admission) (egress.Snapshot, error) {
	id, mode, operator, err := s.authorized(project, input, pinFirstSeenBundles)
	if err != nil {
		return egress.Snapshot{}, err
	}
	return s.capture(project, id, mode, input.Requests, operator, input.Bundles, input.ExportDestinations)
}

// Resolve compiles exactly what Admit would authorize and returns it WITHOUT
// publishing: no approval is written, no snapshot is saved, and the caller must
// have opened the store without creating an owner key. It answers "what would
// this launch run under" for a host that has to publish a fence before anyone
// asks for a launch. A launch still has to Admit.
func (s *Store) Resolve(project string, input Admission) (egress.Snapshot, error) {
	id, mode, operator, err := s.authorized(project, input, matchPinnedBundles)
	if err != nil {
		return egress.Snapshot{}, err
	}
	return s.compile(project, id, mode, input.Requests, operator, input.Bundles, input.ExportDestinations)
}

// authorized is the ONE input assembly behind both of them. Read one approval
// for both posture and envelope checks: rereading between them could combine two
// different operator decisions. Publishing is the only difference between Admit
// and Resolve, so a resolved fingerprint cannot describe authority the capture
// would have compiled differently.
//
// pinBundles is the one thing the two cannot share: a bundle this host has never seen is pinned
// the first time an admission uses it, and a resolve is not a use.
func (s *Store) authorized(project string, input Admission, pinBundles bool) (string, egress.Mode, []egress.Input, error) {
	if err := s.authorityAvailable(); err != nil {
		return "", "", nil, err
	}
	id, canonical, info, err := s.projectIdentity(project)
	if err != nil {
		return "", "", nil, err
	}
	approval, err := s.approval(id)
	if err != nil {
		return "", "", nil, err
	}
	if err := approval.checkDirectory(canonical, info); err != nil {
		return "", "", nil, err
	}
	mode, err := input.resolveMode(approval)
	if err != nil {
		return "", "", nil, err
	}
	// The withdrawal barrier is enforced HERE too, not only in the preview: a
	// caller that admits without previewing must not be the one path that walks
	// through a withdrawal back into the open default.
	withdrawn, err := s.withdrawn(id)
	if err != nil {
		return "", "", nil, err
	}
	if barrier := input.withdrawalBarrier(approval, withdrawn, mode); barrier != nil {
		return "", "", nil, barrier
	}
	if pinBundles {
		err = s.checkBundles(input.Bundles)
	} else {
		err = s.matchBundles(input.Bundles)
	}
	if err != nil {
		return "", "", nil, err
	}
	if _, err := s.checkRequests(approval, input.Requests, input.Bundles); err != nil {
		return "", "", nil, err
	}
	operator := append([]egress.Input{}, input.Operator...)
	if mode == egress.Filtered {
		operator = append(operator, input.Automatic...)
	}
	return id, mode, operator, nil
}

func (a Admission) resolveMode(approval *Approval) (egress.Mode, error) {
	for _, field := range []struct {
		name  string
		value *egress.Mode
	}{{"invocation", a.InvocationMode}, {"host preference", a.HostPreference}, {"project request", a.ProjectMode}, {"named policy", a.PolicyMode}} {
		if field.value != nil {
			if _, err := egress.ParseMode(string(*field.value)); err != nil {
				return "", fmt.Errorf("network %s: %w", field.name, err)
			}
		}
	}
	var remembered *egress.Mode
	if approval != nil {
		if _, err := egress.ParseMode(string(approval.Posture)); err != nil {
			return "", err
		}
		remembered = &approval.Posture
	}
	mode := egress.Open
	if a.PolicyMode != nil {
		if a.InvocationMode != nil {
			return "", errors.New("a named API policy decides this session's egress, so --egress cannot override it (network_policy_conflict)")
		}
		mode = *a.PolicyMode
		if remembered != nil && *remembered != egress.Open && *remembered != mode {
			return "", errors.New("the named policy and what this project remembered disagree — settle them on the host (network_policy_conflict)")
		}
	} else {
		selected := false
		for _, value := range []*egress.Mode{a.InvocationMode, remembered, a.HostPreference, a.ProjectMode} {
			if value != nil {
				mode, selected = *value, true
				break
			}
		}
		if !selected && a.hasRules() {
			mode = egress.Filtered
		}
	}
	if mode != egress.Filtered && a.hasRules() {
		return "", errors.New("egress rules only apply in filtered mode — run with --egress filtered, or drop the rules (network_policy_conflict)")
	}
	return mode, nil
}

func (a Admission) hasRules() bool {
	if len(a.Requests) != 0 {
		return true
	}
	for _, input := range a.Operator {
		if len(input.Rules) != 0 {
			return true
		}
	}
	return false
}
