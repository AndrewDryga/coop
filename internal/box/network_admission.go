package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	agents "github.com/AndrewDryga/coop/internal/agent"
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
	spec.projectEnv = p.Box.Env
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
	if input.Services, err = requestedServiceDigests(policyRepo, p, spec.RepoReadOnly); err != nil {
		return nil, err
	}
	// A direct launch mounts the trusted shared MCP configuration whenever it
	// mounts homes at all, so its automatic dependencies are exactly that file's.
	if input.Automatic, err = NetworkMCPDependencies(cfg, spec); err != nil {
		return nil, err
	}
	// The preview publishes nothing and creates no owner key, so it is safe on
	// every launch. It still proves the authority root is outside every mount.
	// A pending request fails closed HERE, before any box or main process, with
	// the one review command that settles it.
	mode, err := networkstate.PreviewAdmissionMode(root, canonical, exposed, input)
	if err != nil {
		var pending *networkstate.PendingApproval
		if errors.As(err, &pending) {
			return nil, fmt.Errorf("%s cannot start because %s\n\n  Review it: coop net approve", networkLaunchName(spec), pending.Reason)
		}
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
	// Host qualification is machinery, not a decision, so an ordinary launch
	// performs it when no current proof exists — the same bounded work as
	// `coop net setup`, transcript included — and continues. It grants nothing:
	// the approval above was already settled without it.
	capture, err := admitFilteredNetwork(cfg, rt, spec, store, canonical, input, func(ctx context.Context) error {
		if _, err := SetupNetwork(ctx, cfg, rt, networkLaunchStderr(spec), networkLaunchStderr(spec)); err != nil {
			return fmt.Errorf("%s cannot start — %w", networkLaunchName(spec), err)
		}
		return nil
	})
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	return capture, nil
}

// networkLaunchName is who a refusal says cannot start: the agent by its
// product name, or the box for a raw command.
func networkLaunchName(spec RunSpec) string {
	if agent, ok := agents.Get(spec.Agent); ok && spec.Agent != "" {
		return agent.DisplayName()
	}
	return "This box"
}

// networkLaunchStderr is coop's own channel for this launch: the operator's
// terminal unless the caller captures it.
func networkLaunchStderr(spec RunSpec) io.Writer {
	if spec.Stderr != nil {
		return spec.Stderr
	}
	return os.Stderr
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
//
// qualify is how a launch obtains a host proof it does not have: an ordinary
// launch passes the setup itself and continues once it passes; a caller that
// cannot build images on someone else's behalf — the session daemon answering
// an API request — passes nil and refuses instead.
func admitFilteredNetwork(cfg *config.Config, rt runtime.Runtime, spec RunSpec, store *networkstate.Store, canonicalProject string, input networkstate.Admission, qualify func(context.Context) error) (*CapturedEgress, error) {
	input, err := filteredNetworkSources(cfg, spec, input)
	if err != nil {
		return nil, err
	}
	policy, err := store.Admit(canonicalProject, input)
	if err != nil {
		return nil, err
	}
	if policy.Mode != egress.Filtered {
		return nil, errors.New("this project's egress changed while the box was starting — run it again")
	}
	ctx := networkAdmissionContext(spec)
	// A Docker that is not running is the real reason a filtered box cannot
	// start, and the one thing no setup can fix; it is reported as itself.
	docker, err := runtime.InspectDocker(ctx, rt)
	if err != nil {
		return nil, err
	}
	defer docker.Close()
	qualification, err := ensureNetworkQualification(ctx, func() (*networkstate.Qualification, error) {
		return currentQualification(ctx, docker, store, policy, cfg.ImageOverride)
	}, qualify)
	if err != nil {
		return nil, err
	}
	return &CapturedEgress{Store: store, Project: canonicalProject, Fingerprint: policy.Fingerprint, QualificationID: qualification.ID}, nil
}

// ensureNetworkQualification is the proof a launch runs under: the current one,
// or — when there is none and this launch may set the host up — the one setup
// leaves behind, read again rather than assumed. A launch that may not set the
// host up is refused with the one preparation that would.
func ensureNetworkQualification(ctx context.Context, current func() (*networkstate.Qualification, error), qualify func(context.Context) error) (*networkstate.Qualification, error) {
	qualification, err := current()
	if err != nil || qualification != nil {
		return qualification, err
	}
	if qualify == nil {
		return nil, errors.New("this host is not set up for filtered runs with this Docker and these agents — run 'coop net setup' first")
	}
	if err := qualify(ctx); err != nil {
		return nil, err
	}
	if qualification, err = current(); err != nil {
		return nil, err
	}
	if qualification == nil {
		return nil, errors.New("this host's setup finished but does not cover this run — run it again")
	}
	return qualification, nil
}

// currentQualification is this host's newest setup record that proves THIS
// launch: it covers the policy's clients, it was made on this daemon by this
// coop's client and gateway definitions, and the image pair it names still
// exists. Anything less is no proof, and the launch qualifies again.
func currentQualification(ctx context.Context, docker *runtime.Docker, store *networkstate.Store, policy egress.Snapshot, imageOverride string) (*networkstate.Qualification, error) {
	qualifications, err := store.Qualifications(ctx)
	if err != nil {
		return nil, err
	}
	binding := networkRuntimeBinding(docker.Info(), docker.Endpoint())
	for _, qualification := range qualifications { // newest first
		if qualification.RequireLaunch(policy) != nil || verifyNetworkCandidate(qualification.Candidate, binding, imageOverride) != nil {
			continue
		}
		present := true
		for _, image := range []string{qualification.Candidate.ClientImage, qualification.Candidate.GatewayImage} {
			if id, _, err := docker.Image(ctx, image); err != nil || id != image {
				present = false
			}
		}
		if present {
			return &qualification, nil
		}
	}
	return nil, nil
}

// resolveFilteredNetwork is admitFilteredNetwork's non-publishing twin: the same sources, the
// same authority, the same host setup requirement — compiled and returned instead of captured.
// It stops before the runtime inspection, because which qualified image this Docker will run is a
// launch's question, not a fence's.
func resolveFilteredNetwork(cfg *config.Config, spec RunSpec, store *networkstate.Store, canonicalProject string, input networkstate.Admission) (egress.Snapshot, error) {
	input, err := filteredNetworkSources(cfg, spec, input)
	if err != nil {
		return egress.Snapshot{}, err
	}
	policy, err := store.Resolve(canonicalProject, input)
	if err != nil {
		return egress.Snapshot{}, err
	}
	if policy.Mode != egress.Filtered {
		return egress.Snapshot{}, errors.New("this project's egress resolves to a posture that captures no rules")
	}
	// A fence is published before anyone asks for a launch, so it cannot set the
	// host up on the way: it needs a record that already covers this policy.
	qualifications, err := store.Qualifications(networkAdmissionContext(spec))
	if err != nil {
		return egress.Snapshot{}, err
	}
	if !slices.ContainsFunc(qualifications, func(q networkstate.Qualification) bool { return q.RequireLaunch(policy) == nil }) {
		return egress.Snapshot{}, errors.New("this host is not set up for filtered runs with these agents — run 'coop net setup' first")
	}
	return policy, nil
}

// filteredNetworkSources completes the inputs every filtered compile shares: the provider core
// bundles this run's targets need, gated by the transports this release can actually enforce.
//
// Refuse an unsupported transport BEFORE any approval is read or written: a rule this runtime
// cannot enforce must fail by name, not be remembered as authority and then silently dropped by
// the gateway.
func filteredNetworkSources(cfg *config.Config, spec RunSpec, input networkstate.Admission) (networkstate.Admission, error) {
	bundles, err := NetworkProviderBundles(cfg, spec)
	if err != nil {
		return networkstate.Admission{}, err
	}
	input.Bundles = bundles
	if err := checkSupportedRequests(input); err != nil {
		return networkstate.Admission{}, err
	}
	return input, nil
}

func networkAdmissionContext(spec RunSpec) context.Context {
	if spec.Ctx == nil {
		return context.Background()
	}
	return spec.Ctx
}
