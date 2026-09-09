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
	HardCeiling    *egress.Mode
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
	id, err := s.projectID(project)
	if err != nil {
		return AdmissionPreview{}, err
	}
	approval, err := s.approval(id)
	if err != nil {
		return AdmissionPreview{}, err
	}
	return input.preview(approval)
}

func (a Admission) preview(approval *Approval) (AdmissionPreview, error) {
	mode, err := a.resolveMode(approval)
	if err != nil {
		return AdmissionPreview{}, err
	}
	_, pending := checkRequestEnvelope(approval, a.Requests)
	return AdmissionPreview{Mode: mode, Pending: pending}, nil
}

// Admit is the shared new-run authority boundary. Read one approval for both
// posture and envelope checks: rereading between them could combine two different
// operator decisions. A concurrent approval change applies to subsequent captures.
func (s *Store) Admit(project string, input Admission) (egress.Snapshot, error) {
	if err := s.authorityAvailable(); err != nil {
		return egress.Snapshot{}, err
	}
	id, err := s.projectID(project)
	if err != nil {
		return egress.Snapshot{}, err
	}
	approval, err := s.approval(id)
	if err != nil {
		return egress.Snapshot{}, err
	}
	mode, err := input.resolveMode(approval)
	if err != nil {
		return egress.Snapshot{}, err
	}
	if _, err := s.checkRequests(approval, input.Requests, input.Bundles); err != nil {
		return egress.Snapshot{}, err
	}
	operator := append([]egress.Input{}, input.Operator...)
	if mode == egress.Filtered {
		operator = append(operator, input.Automatic...)
	}
	return s.capture(project, id, mode, input.Requests, operator, input.Bundles, input.ExportDestinations)
}

func (a Admission) resolveMode(approval *Approval) (egress.Mode, error) {
	for _, field := range []struct {
		name  string
		value *egress.Mode
	}{{"hard ceiling", a.HardCeiling}, {"invocation", a.InvocationMode}, {"host preference", a.HostPreference}, {"project request", a.ProjectMode}, {"named policy", a.PolicyMode}} {
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
	explicit := a.InvocationMode != nil || a.PolicyMode != nil
	if a.PolicyMode != nil {
		if a.InvocationMode != nil {
			return "", errors.New("network_policy_conflict: named API policies forbid invocation overrides")
		}
		mode = *a.PolicyMode
		if remembered != nil && *remembered != egress.Open && *remembered != mode {
			return "", errors.New("network_policy_conflict: reconcile the named policy and remembered project restriction on the host")
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
	if a.HardCeiling != nil && modeBreadth(mode) > modeBreadth(*a.HardCeiling) {
		if explicit {
			return "", errors.New("network_ceiling_exceeded: explicit mode exceeds the host network ceiling")
		}
		mode = *a.HardCeiling
	}
	if mode != egress.Filtered && a.hasRules() {
		return "", errors.New("network_policy_conflict: egress rules require filtered mode")
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

// Modes form a ceiling order, not a union of independent privileges. Concrete
// destination authority is compiled separately and never inferred from this rank.
func modeBreadth(mode egress.Mode) int {
	switch mode {
	case egress.Open:
		return 2
	case egress.Filtered:
		return 1
	default:
		return 0 // callers have already validated the mode
	}
}
