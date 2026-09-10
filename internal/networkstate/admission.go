package networkstate

import (
	"errors"
	"fmt"
	"os"

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
	Operator       []egress.Input
	Bundles        []egress.Bundle
	// Automatic is host-captured shared MCP connectivity. Like provider core
	// dependencies it neither selects filtered mode nor creates offline exceptions.
	Automatic          []egress.Input
	ExportDestinations bool
}

// AdmissionPreview separates two questions a launch answers at once: which
// posture these inputs resolve to, and whether the project's current request
// already fits its remembered approval. A read-only posture view needs them
// apart — a pending request is exactly what it exists to show, so refusing to
// describe the project would hide the one fact the operator came for.
type AdmissionPreview struct {
	Mode egress.Mode
	// Pending is non-nil when the request needs review before a launch. It is
	// never a reason to widen anything: Admit fails on the same condition.
	Pending error
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
		return input.preview(nil)
	}
	if err != nil {
		return AdmissionPreview{}, err
	}
	defer store.Close()
	if err := store.loadKey(false); err != nil {
		return AdmissionPreview{}, err
	}
	if store.key == nil {
		return input.preview(nil)
	}
	return store.admissionPreview(project, input)
}

// PreviewAdmissionMode is the launch caller's form: a pending request is a
// failure there, because admission is about to happen.
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
	preview, err := input.preview(approval)
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

func (a Admission) preview(approval *Approval) (AdmissionPreview, error) {
	mode, err := a.resolveMode(approval)
	if err != nil {
		return AdmissionPreview{}, err
	}
	_, pending := checkRequestEnvelope(approval, a.Requests)
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
