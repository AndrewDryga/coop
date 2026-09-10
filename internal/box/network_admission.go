package box

import (
	"context"
	"errors"
	"fmt"
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
		return "", errors.New("restricted networking needs XDG_STATE_HOME to be an absolute path")
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
		return nil, errors.New("restricted networking needs host configuration")
	}
	policyRepo := projectPolicyRepo(spec)
	canonical, err := canonicalProjectDir(policyRepo)
	if err != nil {
		return nil, err
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
	// A direct launch mounts the trusted shared MCP configuration whenever it
	// mounts homes at all, so its automatic dependencies are exactly that file's.
	if input.Automatic, err = NetworkMCPDependencies(cfg, spec); err != nil {
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
	if err := checkFilteredSupport(cfg); err != nil {
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

// canonicalProjectDir is the one project identity every network read and write
// binds to: an absolute, symlink-resolved directory. Two aliases of the same
// tree must never look like two projects, and a path that does not exist is not
// a project at all.
func canonicalProjectDir(repo string) (string, error) {
	canonical, err := filepath.Abs(repo)
	if err == nil {
		canonical, err = filepath.EvalSymlinks(canonical)
	}
	if err != nil {
		return "", errors.New("restricted networking needs a project directory that exists on this host")
	}
	return canonical, nil
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
//
// A project's own .agent/Dockerfile is NOT one of these: it is built on the
// locked client image at launch and proven derived from it (derived_image.go).
// An arbitrary COOP_IMAGE cannot be — nothing qualified it, and no proof can
// turn an unrelated image into the one this host set up.
func checkFilteredSupport(cfg *config.Config) error {
	if cfg.ImageOverride != "" {
		return errors.New("a filtered box runs coop's own image — unset COOP_IMAGE to start one")
	}
	return nil
}

// checkSupportedRequests applies the runtime capability gate to every concrete
// rule this launch would authorize. Provider selectors are skipped: they expand
// from the trusted release bundle into concrete rules the same gate re-checks
// on the compiled snapshot.
func checkSupportedRequests(input networkstate.Admission) error {
	sources := [][]egress.Rule{input.Requests}
	for _, operator := range input.Operator {
		sources = append(sources, operator.Rules)
	}
	for _, automatic := range input.Automatic {
		sources = append(sources, automatic.Rules)
	}
	for _, rules := range sources {
		normalized, err := egress.NormalizeRules(rules)
		if err != nil {
			return err
		}
		for _, rule := range normalized {
			if rule.To.Provider != "" {
				continue
			}
			if err := egress.SupportedRule(rule); err != nil {
				return fmt.Errorf("%s: %w", NetworkRuleText(rule), err)
			}
		}
	}
	return nil
}

// admitFilteredNetwork completes the capture: operator and project rules and
// the provider core bundles the selected targets need, authorized in ONE
// store.Admit so posture and envelope come from one decision. The caller has
// already put this launch's automatic dependencies on input, because only it
// knows which configuration the box will actually mount.
func admitFilteredNetwork(cfg *config.Config, rt runtime.Runtime, spec RunSpec, store *networkstate.Store, canonicalProject string, input networkstate.Admission) (*CapturedEgress, error) {
	bundles, err := NetworkProviderBundles(cfg, spec)
	if err != nil {
		return nil, err
	}
	input.Bundles = bundles
	// Refuse an unsupported transport BEFORE any approval is read or written:
	// a rule this runtime cannot enforce must fail the launch by name, not be
	// remembered as authority and then silently dropped by the gateway.
	if err := checkSupportedRequests(input); err != nil {
		return nil, err
	}
	policy, err := store.Admit(canonicalProject, input)
	if err != nil {
		return nil, err
	}
	if policy.Mode != egress.Filtered {
		return nil, errors.New("this project's egress changed while the box was starting — run it again")
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
	unqualified := errors.New("this host is not set up for filtered runs with this Docker and these agents — run 'coop net setup'")
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
