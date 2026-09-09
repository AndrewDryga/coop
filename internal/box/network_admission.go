package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// NetworkStatePath is the owner-private authority root. It lives under the
// host's state directory precisely because that is never an agent mount.
func NetworkStatePath() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		hostHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(hostHome, ".local", "state")
	}
	if !filepath.IsAbs(base) {
		return "", errors.New("network state requires an absolute XDG_STATE_HOME")
	}
	return filepath.Join(base, "coop", "network"), nil
}

// NetworkAdmission is HOST-side launch input only. Nothing in it may originate
// in a repository or an API request: the box decides whether a rules file is
// operator authority or a project request, and a caller cannot relabel it.
type NetworkAdmission struct {
	// InvocationMode is the operator's explicit --egress for this run alone. It
	// overrides a remembered posture without changing it.
	InvocationMode *egress.Mode
	// Domains are --allow-domain grants: exact names, TLS on 443.
	Domains []string
	// RulesFile is --egress-rules. Its authority depends on where the file
	// lives: outside every agent mount it grants, inside one it only requests.
	RulesFile string
}

// AdmitNetwork resolves this launch's network posture and, for filtered mode,
// captures the frozen policy the gateway will enforce.
//
// It returns nil for open and offline runs: they proceed exactly as they do
// today, with cfg.Egress set to the resolved mode. A run that neither requests
// filtered mode nor has any remembered posture writes NO host state at all, so
// an ordinary `coop claude` never creates an approval store as a side effect.
func AdmitNetwork(cfg *config.Config, rt runtime.Runtime, spec RunSpec, options NetworkAdmission) (*CapturedEgress, error) {
	if cfg == nil {
		return nil, errors.New("network admission requires host configuration")
	}
	policyRepo := projectPolicyRepo(spec)
	canonical, err := filepath.Abs(policyRepo)
	if err == nil {
		canonical, err = filepath.EvalSymlinks(canonical)
	}
	if err != nil {
		return nil, errors.New("restricted networking requires an existing canonical project directory")
	}
	p, err := project.Load(policyRepo)
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
	input, err := networkAdmissionInput(cfg, p, options, exposed)
	if err != nil {
		return nil, err
	}
	// The preview publishes nothing and creates no owner key, so it is safe on
	// every launch. It still proves the authority root is outside every mount.
	mode, err := networkstate.PreviewAdmissionMode(root, canonical, exposed, input)
	if err != nil {
		return nil, err
	}
	cfg.SetEgress(string(mode))
	if mode != egress.Filtered {
		return nil, nil
	}
	if err := checkFilteredSupport(cfg, spec, p); err != nil {
		return nil, err
	}
	store, err := networkstate.Open(root, exposed)
	if err != nil {
		return nil, err
	}
	capture, err := admitFilteredNetwork(cfg, rt, spec, store, canonical, input)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	return capture, nil
}

// networkAdmissionInput separates presence from default and authority from
// request. The built-in "open" is not an explicit widening, so an unset
// COOP_EGRESS contributes no host preference at all.
func networkAdmissionInput(cfg *config.Config, p *project.Project, options NetworkAdmission, exposed []string) (networkstate.Admission, error) {
	input := networkstate.Admission{InvocationMode: options.InvocationMode, Requests: p.Box.EgressRules}
	if cfg.Explicit("COOP_EGRESS") {
		preference, err := egress.ParseMode(cfg.Egress)
		if err != nil {
			return networkstate.Admission{}, err
		}
		input.HostPreference = &preference
	}
	if p.Box.Egress != "" {
		requested, err := egress.ParseMode(p.Box.Egress)
		if err != nil {
			return networkstate.Admission{}, err
		}
		input.ProjectMode = &requested
	}
	if len(options.Domains) != 0 {
		var grants []egress.Rule
		for _, domain := range options.Domains {
			grants = append(grants, egress.Rule{To: egress.Destination{Domain: domain}, Protocol: "tls", Ports: []int{443}})
		}
		normalized, err := egress.NormalizeRules(grants)
		if err != nil {
			return networkstate.Admission{}, err
		}
		input.Operator = append(input.Operator, egress.Input{Rules: normalized, Origin: egress.Origin{Kind: "operator", Name: "allow-domain"}})
	}
	if options.RulesFile == "" {
		return input, nil
	}
	// The captured bytes, not the path, become the request or the grant: a file
	// swapped after this read cannot change what was admitted.
	rules, err := captureNetworkRulesFile(options.RulesFile, exposed)
	if err != nil {
		return networkstate.Admission{}, err
	}
	return rules.apply(input)
}

// checkFilteredSupport refuses a combination this release cannot enforce, before
// any host state or runtime resource exists. An unsupported tuple fails; it is
// never downgraded to a warning and a wider policy.
func checkFilteredSupport(cfg *config.Config, spec RunSpec, p *project.Project) error {
	repo := projectPolicyRepo(spec)
	if p.Box.Dockerfile != "" || repo != "" && fileExists(filepath.Join(repo, project.DockerfilePath(repo))) {
		return errors.New("restricted networking runs the qualified client image; this project's Dockerfile is not supported in filtered mode yet")
	}
	if cfg.ImageOverride != "" {
		return errors.New("restricted networking runs the qualified client image; unset COOP_IMAGE to launch it")
	}
	return nil
}

// admitFilteredNetwork completes the capture: operator and project rules and
// the provider core bundles the selected targets need, authorized in ONE
// store.Admit so posture and envelope come from one decision.
func admitFilteredNetwork(cfg *config.Config, rt runtime.Runtime, spec RunSpec, store *networkstate.Store, canonicalProject string, input networkstate.Admission) (*CapturedEgress, error) {
	bundles, err := NetworkProviderBundles(cfg, spec)
	if err != nil {
		return nil, err
	}
	input.Bundles = bundles
	automatic, err := NetworkMCPDependencies(cfg, spec)
	if err != nil {
		return nil, err
	}
	input.Automatic = automatic
	policy, err := store.Admit(canonicalProject, input)
	if err != nil {
		return nil, err
	}
	if policy.Mode != egress.Filtered {
		return nil, errors.New("network posture changed during launch preparation; retry admission")
	}
	ctx := spec.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	// Match the record's own clients before touching the runtime: a selection
	// this host never set up should say so, not fail on a Docker inspection it
	// never needed.
	qualifications, err := store.Qualifications(ctx)
	if err != nil {
		return nil, err
	}
	var covered []networkstate.Qualification
	for _, qualification := range qualifications {
		if qualification.RequireLaunch(policy) == nil {
			covered = append(covered, qualification)
		}
	}
	unqualified := errors.New("no completed network setup matches this runtime and provider selection; run `coop net setup`")
	if len(covered) == 0 {
		return nil, unqualified
	}
	docker, err := runtime.InspectDocker(ctx, rt)
	if err != nil {
		return nil, err
	}
	binding := networkRuntimeBinding(docker.Info(), docker.Endpoint())
	_ = docker.Close()
	for _, qualification := range covered {
		if verifyNetworkCandidate(qualification.Candidate, binding, cfg.ImageOverride) != nil {
			continue
		}
		return &CapturedEgress{Store: store, Project: canonicalProject, Fingerprint: policy.Fingerprint, QualificationID: qualification.ID}, nil
	}
	return nil, unqualified
}
