package box

import (
	"errors"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/gatewayimage"
	"github.com/AndrewDryga/coop/internal/networkstate"
)

// networkSmokeLaunch is the explicit host preflight permit. It never appears on
// RunSpec, a serialized capture, CLI flags or worker requests: only the setup
// workflow constructs one. The smoke uses the ordinary composition, enforcement
// and exact cleanup engine, so what it proves is what a workload gets.
type networkSmokeLaunch struct {
	authority  *networkstate.QualificationSmoke
	registered func(networkstate.Execution)
}

// capturedNetworkCandidate resolves the exact image pair a launch may use. A
// smoke carries its own unproven candidate; a workload gets one only from a
// completed host record that covers this policy.
func capturedNetworkCandidate(capture *CapturedEgress, policy egress.Snapshot, smoke *networkSmokeLaunch) (networkstate.CandidateSpec, error) {
	if smoke != nil {
		if smoke.authority == nil || capture.QualificationID != "" {
			return networkstate.CandidateSpec{}, errors.New("invalid private network preflight launch")
		}
		return smoke.authority.Candidate(), nil
	}
	qualification, err := capture.Store.Qualification(capture.QualificationID)
	if err != nil {
		return networkstate.CandidateSpec{}, errors.New("restricted networking requires a completed host setup; run `coop net setup`")
	}
	if err := qualification.RequireLaunch(policy); err != nil {
		return networkstate.CandidateSpec{}, err
	}
	return qualification.Candidate, nil
}

// verifyNetworkCandidate refuses a record that belongs to another daemon,
// client definition or gateway source. Requalification is explicit, never
// inferred from a version that merely looks compatible.
func verifyNetworkCandidate(candidate networkstate.CandidateSpec, binding networkstate.RuntimeBinding, imageOverride string) error {
	if !networkstate.EqualRuntimeBinding(candidate.Runtime, binding) {
		return errors.New("runtime differs from the exact network setup; explicit setup is required")
	}
	if imageOverride != "" && imageOverride != candidate.ClientImage {
		return errors.New("restricted networking cannot use an image override outside its qualified image pair")
	}
	definition, _, closure, err := lockedImageDefinition(agents.ClientPlatform{OS: binding.OS, Architecture: binding.Architecture, Libc: candidate.Libc})
	if err != nil {
		return err
	}
	if candidate.ClientDefinition != definition.Labels["coop.clients.definition"] || candidate.ClientClosure != closure.Digest ||
		candidate.GatewaySource != gatewayimage.Fingerprint() || candidate.NodeBase != pinnedNodeImage || candidate.GoBase != pinnedGoImage {
		return errors.New("network setup belongs to another client or gateway definition; explicit setup is required")
	}
	return nil
}
