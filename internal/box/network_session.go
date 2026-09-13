package box

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// SessionNetworkCaptureEnv carries a remote session's frozen network authority
// from the host daemon to the ACP child it starts. It is a HOST-parent variable
// only: the daemon scrubs every COOP_* from the environment it builds, and no
// box ever sees this name — a process inside one could otherwise hand itself a
// policy the owner never admitted.
const SessionNetworkCaptureEnv = "COOP_NETWORK_CAPTURE"

// sessionNetworkCaptureLimit bounds the encoded reference. Five short opaque
// identities never approach it; anything larger is not this contract.
const sessionNetworkCaptureLimit = 4 << 10

// SessionNetworkCapture is a REFERENCE, not authority. It names the project and
// the owner-keyed snapshot fingerprint the daemon admitted, plus the session and
// attempt the run belongs to. The child proves it by reopening the owner-private
// store and loading that exact snapshot; a forged reference cannot produce one.
type SessionNetworkCapture struct {
	Project       string `json:"project"`
	Fingerprint   string `json:"fingerprint"`
	Qualification string `json:"qualification"`
	SessionID     string `json:"session_id"`
	AttemptID     string `json:"attempt_id"`
}

// Encode renders the environment value for one child launch.
func (c SessionNetworkCapture) Encode() (string, error) {
	if c.Project == "" || c.Fingerprint == "" || c.Qualification == "" || c.SessionID == "" || c.AttemptID == "" {
		return "", errors.New("this session's network details are incomplete")
	}
	data, err := json.Marshal(c)
	if err != nil || len(data) > sessionNetworkCaptureLimit {
		return "", errors.New("this session's network details cannot be encoded")
	}
	return string(data), nil
}

// SessionNetworkAdmission is the named operator policy's network authority, and
// nothing else. A create or turn request cannot reach these fields: the daemon
// reads them from the trusted policy file it loaded at startup.
type SessionNetworkAdmission struct {
	// Mode is the policy's explicit `egress.mode`. It is operator authority, so
	// a remembered project restriction it disagrees with REFUSES the session
	// rather than quietly narrowing or widening it.
	Mode egress.Mode
	// Rules are the policy's own `egress.rules`, already normalized.
	Rules []egress.Rule
	// ExportDestinations opts this session's outbound projections into concrete
	// destination names. False is the default and withholds them.
	ExportDestinations bool
	// OmitMCP says this policy's sessions mount no shared MCP configuration, so
	// admission derives no destinations from it. Granting an MCP host a session
	// can never call would be authority nothing asked for.
	OmitMCP bool
}

// AdmitSessionNetwork resolves and freezes ONE remote session's network posture,
// once, when the session is created. Every later run — cold turn, warm child,
// resume, replay — reuses the captured snapshot instead of admitting again, so
// an approval or config edit landing mid-session can only produce a visible
// denial, never a wider policy.
//
// It returns the resolved mode plus a capture for filtered sessions (nil for
// open and offline ones). Unlike the direct-launch path it does NOT mutate cfg:
// the daemon's configuration is shared by every session it serves.
func AdmitSessionNetwork(cfg *config.Config, rt runtime.Runtime, spec RunSpec, options SessionNetworkAdmission) (egress.Mode, *CapturedEgress, error) {
	plan, err := planSessionNetwork(cfg, spec, options)
	if err != nil {
		return "", nil, err
	}
	mode, err := networkstate.PreviewAdmissionMode(plan.root, plan.project, plan.exposed, plan.input)
	if err != nil {
		return "", nil, err
	}
	if mode != egress.Filtered {
		return mode, nil, nil
	}
	if err := plan.prepareFiltered(cfg, spec, options); err != nil {
		return "", nil, err
	}
	store, err := networkstate.Open(plan.root, plan.exposed)
	if err != nil {
		return "", nil, err
	}
	// The daemon answers an API request; it never builds images or runs a
	// smoke on one, so a host nobody set up ahead of time is refused.
	capture, err := admitFilteredNetwork(cfg, rt, spec, store, plan.project, plan.input, nil)
	if err != nil {
		_ = store.Close()
		return "", nil, err
	}
	return mode, capture, nil
}

// ResolveSessionNetwork answers what one session policy reaches on THIS host right now — the
// posture, plus for a filtered policy the fingerprint AdmitSessionNetwork would freeze — and
// writes nothing at all: no approval, no published snapshot, not even an owner key. It is how a
// daemon publishes a fence before anyone asks for a session, and how a create refuses a stale one.
//
// A policy it cannot resolve — no approval for the project, no host setup record, a rule this
// runtime cannot enforce — is an error, never a quiet open answer.
func ResolveSessionNetwork(cfg *config.Config, spec RunSpec, options SessionNetworkAdmission) (egress.Mode, string, error) {
	mode, snapshot, err := ResolveSessionNetworkSnapshot(cfg, spec, options)
	if err != nil {
		return "", "", err
	}
	return mode, snapshot.Fingerprint, nil
}

// ResolveSessionNetworkSnapshot is ResolveSessionNetwork with its EVIDENCE: the compiled snapshot
// a filtered policy resolves to, so a reader can be shown the grants themselves — the provider
// bundles and the project's approved rules — instead of the policy YAML they could already read.
// The fingerprint on it is the one ResolveSessionNetwork returns; an open or offline policy
// resolves to no snapshot, because nothing was compiled to show.
func ResolveSessionNetworkSnapshot(cfg *config.Config, spec RunSpec, options SessionNetworkAdmission) (egress.Mode, egress.Snapshot, error) {
	plan, err := planSessionNetwork(cfg, spec, options)
	if err != nil {
		return "", egress.Snapshot{}, err
	}
	mode, err := networkstate.PreviewAdmissionMode(plan.root, plan.project, plan.exposed, plan.input)
	if err != nil {
		return "", egress.Snapshot{}, err
	}
	if mode != egress.Filtered {
		return mode, egress.Snapshot{}, nil
	}
	if err := plan.prepareFiltered(cfg, spec, options); err != nil {
		return mode, egress.Snapshot{}, err
	}
	// OpenExisting, never Open: asking what a policy resolves to must not be the act that creates
	// this host's owner key. A host with no network authority yet has no fingerprint to report.
	store, err := networkstate.OpenExisting(plan.root, plan.exposed)
	if errors.Is(err, os.ErrNotExist) {
		return mode, egress.Snapshot{}, errors.New("this host has no network records to resolve against — run 'coop net setup'")
	}
	if err != nil {
		return mode, egress.Snapshot{}, err
	}
	defer store.Close()
	policy, err := resolveFilteredNetwork(cfg, spec, store, plan.project, plan.input)
	if err != nil {
		return mode, egress.Snapshot{}, err
	}
	return mode, policy, nil
}

// sessionNetworkPlan is what a session policy resolves to before any store is opened: the
// owner-private authority root, the project the approval belongs to, everything this launch
// exposes wholesale, and the admission inputs the policy and the repository contribute.
type sessionNetworkPlan struct {
	root    string
	project string
	exposed []string
	input   networkstate.Admission
}

// planSessionNetwork assembles that once for BOTH the publishing and the non-publishing path, so
// a fingerprint the daemon published and the one a create freezes cannot come from different
// inputs. Nothing here reads or writes the owner store.
func planSessionNetwork(cfg *config.Config, spec RunSpec, options SessionNetworkAdmission) (sessionNetworkPlan, error) {
	if cfg == nil {
		return sessionNetworkPlan{}, errors.New("restricted networking needs host configuration")
	}
	// An absent policy mode is not "open": it means the operator wrote no posture at all, so
	// the project's remembered one still decides. Only a written mode is explicit authority.
	var policyMode *egress.Mode
	if options.Mode != "" {
		parsed, err := egress.ParseMode(string(options.Mode))
		if err != nil {
			return sessionNetworkPlan{}, err
		}
		policyMode = &parsed
	}
	policyRepo := projectPolicyRepo(spec)
	canonical, err := canonicalProjectDir(policyRepo)
	if err != nil {
		return sessionNetworkPlan{}, err
	}
	p, err := project.Load(policyRepo)
	if err != nil {
		return sessionNetworkPlan{}, err
	}
	root, err := NetworkStatePath()
	if err != nil {
		return sessionNetworkPlan{}, err
	}
	exposed, err := networkExposureRoots(cfg, spec)
	if err != nil {
		return sessionNetworkPlan{}, err
	}
	input := networkstate.Admission{
		PolicyMode: policyMode, Requests: p.Box.EgressRules,
		ExportDestinations: options.ExportDestinations,
	}
	if input.Services, err = requestedServiceDigests(policyRepo, p, spec.RepoReadOnly); err != nil {
		return sessionNetworkPlan{}, err
	}
	// Without a policy mode the project's own request still speaks, exactly as it
	// does for a direct launch in this repository. With one it is outranked: a
	// named policy is operator authority and a repository cannot argue with it.
	if p.Box.Egress != "" {
		requested, err := egress.ParseMode(p.Box.Egress)
		if err != nil {
			return sessionNetworkPlan{}, err
		}
		input.ProjectMode = &requested
	}
	if len(options.Rules) != 0 {
		rules, err := egress.NormalizeRules(options.Rules)
		if err != nil {
			return sessionNetworkPlan{}, err
		}
		input.Operator = append(input.Operator, egress.Input{
			Rules: rules, Origin: egress.Origin{Kind: "operator", Name: "session-policy"},
		})
	}
	return sessionNetworkPlan{root: root, project: canonical, exposed: exposed, input: input}, nil
}

// prepareFiltered adds what only a filtered launch derives: the support gate this release can
// enforce, and the shared MCP hosts the box will actually be able to call.
func (plan *sessionNetworkPlan) prepareFiltered(cfg *config.Config, spec RunSpec, options SessionNetworkAdmission) error {
	if err := checkFilteredSupport(cfg); err != nil {
		return err
	}
	automatic, err := sessionAutomaticDependencies(cfg, spec, options)
	if err != nil {
		return err
	}
	plan.input.Automatic = automatic
	return nil
}

// sessionAutomaticDependencies is what this session's box will actually be able
// to call. A policy that withholds the shared MCP configuration from its
// sessions withholds its hosts too: the box never receives the file, so granting
// its destinations would be authority nothing asked for and nothing can use.
func sessionAutomaticDependencies(cfg *config.Config, spec RunSpec, options SessionNetworkAdmission) ([]egress.Input, error) {
	if options.OmitMCP {
		return nil, nil
	}
	return NetworkMCPDependencies(cfg, spec)
}

// decodeSessionNetworkCapture reads the environment value strictly: an unknown field is a
// different contract, not a newer one, and a value this large is not this contract at all.
func decodeSessionNetworkCapture(raw string) (SessionNetworkCapture, error) {
	if len(raw) > sessionNetworkCaptureLimit {
		return SessionNetworkCapture{}, errors.New("this session's network details are too large")
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.DisallowUnknownFields()
	var reference SessionNetworkCapture
	if err := decoder.Decode(&reference); err != nil {
		return SessionNetworkCapture{}, errors.New("this session's network details are malformed")
	}
	if _, err := reference.Encode(); err != nil {
		return SessionNetworkCapture{}, err
	}
	return reference, nil
}

// CapturedEgressFromEnvironment re-authenticates the capture a session ACP child
// was handed by its host parent. The environment names a snapshot; only the
// owner-private store can produce it, so a child that cannot load the exact
// project+fingerprint pair under the owner key never launches filtered.
//
// It returns nil for every ordinary launch, which is every launch with no
// SessionNetworkCaptureEnv in the environment.
func CapturedEgressFromEnvironment(cfg *config.Config, spec RunSpec) (*CapturedEgress, error) {
	raw := os.Getenv(SessionNetworkCaptureEnv)
	if raw == "" {
		return nil, nil
	}
	reference, err := decodeSessionNetworkCapture(raw)
	if err != nil {
		return nil, err
	}
	root, err := NetworkStatePath()
	if err != nil {
		return nil, err
	}
	exposed, err := networkExposureRoots(cfg, spec)
	if err != nil {
		return nil, err
	}
	// OpenExisting, never Open: a child must not be able to create an owner key
	// or any other authority state as a side effect of starting.
	store, err := networkstate.OpenExisting(root, exposed)
	if err != nil {
		return nil, err
	}
	policy, err := store.LoadSnapshot(reference.Project, reference.Fingerprint)
	if err != nil {
		_ = store.Close()
		return nil, errors.Join(errors.New("this session's network rules are not on this host"), err)
	}
	if policy.Mode != egress.Filtered {
		_ = store.Close()
		return nil, errors.New("this session's network rules are not filtered ones")
	}
	if _, err := store.Qualification(reference.Qualification); err != nil {
		_ = store.Close()
		return nil, errors.Join(errors.New("this host is no longer set up the way this session was started — run 'coop net setup'"), err)
	}
	return &CapturedEgress{
		Store: store, Project: reference.Project, Fingerprint: reference.Fingerprint,
		QualificationID: reference.Qualification,
		SessionID:       reference.SessionID, AttemptID: reference.AttemptID,
	}, nil
}

// NetworkRunReport folds one run's retained evidence into the bounded summary a
// caller that does not own the terminal — a remote session's event stream — puts
// in front of a human. It is the same grouping a direct run prints on stderr.
func NetworkRunReport(runID string, snapshot networkview.Snapshot) NetworkReport {
	if runID == "" {
		return NetworkReport{}
	}
	return networkRunReport(runID, snapshot)
}
