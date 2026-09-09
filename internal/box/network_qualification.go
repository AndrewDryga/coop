package box

import (
	"errors"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/gatewayimage"
	"github.com/AndrewDryga/coop/internal/networkstate"
)

// networkTrialLaunch is the explicit host qualification permit. It never appears
// on RunSpec, a serialized capture, CLI flags or worker requests: only the setup
// workflow constructs it. Trials use the ordinary composition, enforcement and
// exact cleanup engine, so what they prove is what a workload gets.
type networkTrialLaunch struct {
	authority  *networkstate.QualificationTrial
	caseName   string
	client     *networkstate.QualifiedClient
	registered func(networkstate.Execution)
}

// capturedNetworkCandidate resolves the exact image pair a launch may use. A
// trial carries its own unproven candidate; a workload gets one only from a
// completed qualification that covers this policy and MCP projection.
func capturedNetworkCandidate(capture *CapturedEgress, policy egress.Snapshot, projection string, trial *networkTrialLaunch) (networkstate.CandidateSpec, error) {
	if trial != nil {
		if trial.authority == nil || capture.QualificationID != "" {
			return networkstate.CandidateSpec{}, errors.New("invalid private network qualification launch")
		}
		return trial.authority.Candidate(), nil
	}
	qualification, err := capture.Store.Qualification(capture.QualificationID)
	if err != nil {
		return networkstate.CandidateSpec{}, errors.New("restricted networking requires a completed host qualification; run explicit network setup")
	}
	if err := qualification.RequireLaunch(policy, projection); err != nil {
		return networkstate.CandidateSpec{}, err
	}
	return qualification.Candidate, nil
}

// verifyNetworkCandidate refuses a qualification that belongs to another daemon,
// client definition or gateway source. Requalification is explicit, never
// inferred from a version that merely looks compatible.
func verifyNetworkCandidate(candidate networkstate.CandidateSpec, binding networkstate.RuntimeBinding, imageOverride string) error {
	if !networkstate.EqualRuntimeBinding(candidate.Runtime, binding) {
		return errors.New("runtime differs from the exact network qualification; explicit requalification is required")
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
		return errors.New("network qualification belongs to another client or gateway definition; explicit requalification is required")
	}
	return nil
}
