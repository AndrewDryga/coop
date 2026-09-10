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
		return "", errors.New("session network capture is incomplete")
	}
	data, err := json.Marshal(c)
	if err != nil || len(data) > sessionNetworkCaptureLimit {
		return "", errors.New("session network capture is unrepresentable")
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
	if cfg == nil {
		return "", nil, errors.New("network admission requires host configuration")
	}
	// An absent policy mode is not "open": it means the operator wrote no posture at all, so
	// the project's remembered one still decides. Only a written mode is explicit authority.
	var policyMode *egress.Mode
	if options.Mode != "" {
		parsed, err := egress.ParseMode(string(options.Mode))
		if err != nil {
			return "", nil, err
		}
		policyMode = &parsed
	}
	policyRepo := projectPolicyRepo(spec)
	canonical, err := canonicalProjectDir(policyRepo)
	if err != nil {
		return "", nil, err
	}
	p, err := project.Load(policyRepo)
	if err != nil {
		return "", nil, err
	}
	root, err := NetworkStatePath()
	if err != nil {
		return "", nil, err
	}
	exposed, err := networkExposureRoots(cfg, spec)
	if err != nil {
		return "", nil, err
	}
	input := networkstate.Admission{
		PolicyMode: policyMode, Requests: p.Box.EgressRules,
		ExportDestinations: options.ExportDestinations,
	}
	// Without a policy mode the project's own request still speaks, exactly as it
	// does for a direct launch in this repository. With one it is outranked: a
	// named policy is operator authority and a repository cannot argue with it.
	if p.Box.Egress != "" {
		requested, err := egress.ParseMode(p.Box.Egress)
		if err != nil {
			return "", nil, err
		}
		input.ProjectMode = &requested
	}
	if len(options.Rules) != 0 {
		rules, err := egress.NormalizeRules(options.Rules)
		if err != nil {
			return "", nil, err
		}
		input.Operator = append(input.Operator, egress.Input{
			Rules: rules, Origin: egress.Origin{Kind: "operator", Name: "session-policy"},
		})
	}
	mode, err := networkstate.PreviewAdmissionMode(root, canonical, exposed, input)
	if err != nil {
		return "", nil, err
	}
	if mode != egress.Filtered {
		return mode, nil, nil
	}
	if err := checkFilteredSupport(cfg, spec, p); err != nil {
		return "", nil, err
	}
	if input.Automatic, err = sessionAutomaticDependencies(cfg, spec, options); err != nil {
		return "", nil, err
	}
	store, err := networkstate.Open(root, exposed)
	if err != nil {
		return "", nil, err
	}
	capture, err := admitFilteredNetwork(cfg, rt, spec, store, canonical, input)
	if err != nil {
		_ = store.Close()
		return "", nil, err
	}
	return mode, capture, nil
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
		return SessionNetworkCapture{}, errors.New("session network capture is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.DisallowUnknownFields()
	var reference SessionNetworkCapture
	if err := decoder.Decode(&reference); err != nil {
		return SessionNetworkCapture{}, errors.New("session network capture is malformed")
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
		return nil, errors.Join(errors.New("this session's network snapshot is not in the owner's store"), err)
	}
	if policy.Mode != egress.Filtered {
		_ = store.Close()
		return nil, errors.New("this session's network snapshot is not a filtered policy")
	}
	if _, err := store.Qualification(reference.Qualification); err != nil {
		_ = store.Close()
		return nil, errors.Join(errors.New("this session's network qualification is unavailable; run `coop net setup`"), err)
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
