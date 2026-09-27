package box

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// SessionNetworkCaptureEnv carries a remote session's frozen network authority
// from the host daemon to the ACP child it starts. It is a HOST-parent variable
// only: the daemon scrubs every COOP_* from the environment it builds, and no
// box ever sees this name — a process inside one could otherwise hand itself a
// policy the owner never admitted.
const SessionNetworkCaptureEnv = "COOP_NETWORK_CAPTURE"

// ControllerJobEnv is a host-parent marker for a session whose repository is code, not
// a source of Coop box settings. It is never passed inside the model box.
const ControllerJobEnv = "COOP_CONTROLLER_JOB"

func ControllerJobFromEnvironment() (bool, error) {
	digest := os.Getenv(ControllerJobEnv)
	if digest == "" {
		return false, nil
	}
	if len(digest) != 64 {
		return false, errors.New("controller job identity is invalid")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != digest {
		return false, errors.New("controller job identity is invalid")
	}
	return true, nil
}

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
	JobDigest     string `json:"job_digest,omitempty"`
}

// Encode renders the environment value for one child launch.
func (c SessionNetworkCapture) Encode() (string, error) {
	if c.Project == "" || c.Fingerprint == "" || c.Qualification == "" || c.SessionID == "" || c.AttemptID == "" {
		return "", errors.New("this session's network details are incomplete")
	}
	if c.JobDigest != "" {
		if err := (networkstate.JobSnapshotRef{JobDigest: c.JobDigest, SessionID: c.SessionID}).Validate(); err != nil {
			return "", err
		}
	}
	data, err := json.Marshal(c)
	if err != nil || len(data) > sessionNetworkCaptureLimit {
		return "", errors.New("this session's network details cannot be encoded")
	}
	return string(data), nil
}

// ControllerJobNetwork is authenticated, immutable authority received from the controller. It never
// consults repository project policy or this worker's local network approvals.
type ControllerJobNetwork struct {
	JobDigest          string
	SessionID          string
	Mode               egress.Mode
	Rules              []egress.Rule
	ExportDestinations bool
}

func AdmitControllerJobNetwork(cfg *config.Config, rt runtime.Runtime, spec RunSpec, job ControllerJobNetwork) (egress.Mode, *CapturedEgress, error) {
	mode, err := egress.ParseMode(string(job.Mode))
	if err != nil {
		return "", nil, err
	}
	ref := networkstate.JobSnapshotRef{JobDigest: job.JobDigest, SessionID: job.SessionID}
	if err := ref.Validate(); err != nil {
		return "", nil, err
	}
	if mode != egress.Filtered {
		if len(job.Rules) != 0 || job.ExportDestinations {
			return "", nil, errors.New("open and offline controller jobs cannot carry filtered network authority")
		}
		return mode, nil, nil
	}
	if cfg == nil {
		return "", nil, errors.New("filtered controller job needs host configuration")
	}
	if err := checkFilteredRuntime(rt); err != nil {
		return "", nil, err
	}
	if err := checkFilteredSupport(cfg); err != nil {
		return "", nil, err
	}
	rules, err := egress.NormalizeRules(job.Rules)
	if err != nil {
		return "", nil, err
	}
	for _, rule := range rules {
		if rule.To.Service != "" {
			return "", nil, errors.New("controller job service grants need explicit service authority")
		}
	}
	input, err := filteredNetworkSources(cfg, spec, networkstate.Admission{
		Operator: []egress.Input{{Rules: rules, Origin: egress.Origin{Kind: "controller", Name: job.JobDigest}}},
	})
	if err != nil {
		return "", nil, fmt.Errorf("controller job network rules: %w", err)
	}
	root, err := NetworkStatePath()
	if err != nil {
		return "", nil, err
	}
	exposed, err := networkExposureRoots(cfg, spec)
	if err != nil {
		return "", nil, err
	}
	project, err := canonicalProjectDir(projectPolicyRepo(spec))
	if err != nil {
		return "", nil, err
	}
	store, err := networkstate.Open(root, exposed)
	if err != nil {
		return "", nil, err
	}
	policy, err := store.CaptureJob(ref, rules, input.Bundles, job.ExportDestinations)
	if err != nil {
		_ = store.Close()
		return "", nil, err
	}
	ctx := networkAdmissionContext(spec)
	docker, err := runtime.InspectDocker(ctx, rt)
	if err != nil {
		_ = store.Close()
		return "", nil, err
	}
	defer docker.Close()
	qualification, err := ensureNetworkQualification(ctx, func() (*networkstate.Qualification, error) {
		return currentQualification(ctx, docker, store, policy, cfg.ImageOverride)
	}, nil)
	if err != nil {
		_ = store.Close()
		return "", nil, err
	}
	return mode, &CapturedEgress{Store: store, Project: project, Fingerprint: policy.Fingerprint,
		QualificationID: qualification.ID, JobDigest: job.JobDigest, SessionID: job.SessionID}, nil
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
	if (reference.JobDigest != "") != spec.ControllerJob ||
		reference.JobDigest != "" && os.Getenv(ControllerJobEnv) != reference.JobDigest {
		return nil, errors.New("controller job network reference does not match this child")
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
	var policy egress.Snapshot
	if reference.JobDigest != "" {
		policy, err = store.LoadJobSnapshot(networkstate.JobSnapshotRef{JobDigest: reference.JobDigest, SessionID: reference.SessionID}, reference.Fingerprint)
	} else {
		policy, err = store.LoadSnapshot(reference.Project, reference.Fingerprint)
	}
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
		SessionID:       reference.SessionID, AttemptID: reference.AttemptID, JobDigest: reference.JobDigest,
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
