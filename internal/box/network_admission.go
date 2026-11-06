package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// NetworkStatePath is the owner-private authority root. It lives under the
// host's state directory precisely because that is never an agent mount.
func NetworkStatePath() (string, error) {
	base, err := config.StateHome()
	if err != nil {
		return "", err
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
	// RulesFile is --egress-rules: the operator's grants for this run. A file
	// inside an agent mount, or one someone else could change, is refused.
	RulesFile string
	// ExistingQualification refuses, naming `coop net setup`, when no current
	// host proof covers the policy, instead of setting the host up. A running
	// editor session re-admitting between turns must not wait on host setup. It
	// never rebuilds a project image either.
	ExistingQualification bool
	// ProjectBuild is what this launch does when its project image is not ready.
	ProjectBuild ProjectBuildMiss
	// HoldPending admits a project that asks for access nobody approved, or whose file does not load,
	// instead of refusing it, for an editor session that connects first and waits for a person. The
	// capture leaves out all that the project's file asks for and says so (CapturedEgress.Pending or
	// BrokenFile), so only a child that holds prompts may run under it. A withdrawal or a replaced project
	// folder still refuses, as does a mode that resolves offline: filtered would give a holding child a
	// network that launch was denied.
	HoldPending bool
}

// ProjectBuildMiss says what an admission does about a project image that is not ready. Whatever it
// says, files no person approved never build here: they refuse, naming `coop build`.
type ProjectBuildMiss int

const (
	// RefuseProjectBuild refuses every miss. A caller gets a build that can take many minutes only by
	// asking for it, so one that only needs an answer, like a model catalog refresh, cannot start one.
	RefuseProjectBuild ProjectBuildMiss = iota
	// RebuildApproved rebuilds files a person already approved, then admits.
	RebuildApproved
	// DeferProjectBuild admits without looking: an editor session connects first and asks
	// EditorBoxReadiness itself. The launch's own proofs still refuse a project image that is not ready.
	DeferProjectBuild
)

// AdmitNetwork resolves this launch's network posture and, for filtered mode,
// captures the frozen policy the gateway will enforce. Filtered is also the
// built-in default, inside a project or not.
//
// It returns nil for open and offline runs: they proceed with cfg.Egress set
// to the resolved mode and write NO host state at all, so an explicitly open
// or offline run never creates an approval store as a side effect.
func AdmitNetwork(cfg *config.Config, rt runtime.Runtime, spec RunSpec, options NetworkAdmission) (*CapturedEgress, error) {
	if cfg == nil {
		return nil, errors.New("restricted networking needs host configuration")
	}
	// Admission marks the posture it resolved on the configuration it is given, so a second one has to
	// start from the first's starting point to resolve the same way, and to see an edit made meanwhile.
	unmarked, first := cfg.Clone(), true
	capture, err := admitRebuildingApproved(options, func() (*CapturedEgress, error) {
		if first {
			first = false
			return admitNetwork(cfg, rt, spec, options, nil)
		}
		again := unmarked.Clone()
		capture, err := admitNetwork(again, rt, spec, options, nil)
		if err == nil {
			cfg.SetEgress(again.Egress)
		}
		return capture, err
	}, func(dfRel string) error { return RebuildApprovedProject(cfg, rt, spec, dfRel) })
	return capture, withBuildCommand(cfg, spec, err)
}

// admitRebuildingApproved admits, and when the only thing missing is a project image a person already
// approved, rebuilds it and admits again: once, not a rebuild loop. A change after this point is a new
// miss, and the final launch proofs remain the last authority before execution.
func admitRebuildingApproved(options NetworkAdmission, admit func() (*CapturedEgress, error), rebuild func(dfRel string) error) (*CapturedEgress, error) {
	capture, err := admit()
	var required *ProjectBuildRequired
	if !errors.As(err, &required) || !required.Approved || options.ExistingQualification || options.ProjectBuild != RebuildApproved {
		return capture, err
	}
	if err := rebuild(required.Dockerfile); err != nil {
		return nil, err
	}
	return admit()
}

// CheckNetworkApproval checks the same requests as launch without changing the configuration,
// writing authority, qualifying the host or building images. A successful preview grants nothing:
// the process that launches the box must still call AdmitNetwork with its complete provider scope.
func CheckNetworkApproval(cfg *config.Config, spec RunSpec, options NetworkAdmission) error {
	_, err := previewNetworkMode(cfg, spec, options)
	return err
}

// previewNetworkMode is CheckNetworkApproval returning the mode the launch would resolve.
func previewNetworkMode(cfg *config.Config, spec RunSpec, options NetworkAdmission) (egress.Mode, error) {
	if cfg == nil {
		return "", errors.New("restricted networking needs host configuration")
	}
	options.HoldPending = false
	var mode egress.Mode
	_, err := admitNetwork(cfg, runtime.Runtime{}, spec, options, &mode)
	return mode, err
}

// admitNetwork admits, or with previewed only checks the approval and reports the mode there.
func admitNetwork(cfg *config.Config, rt runtime.Runtime, spec RunSpec, options NetworkAdmission, previewed *egress.Mode) (*CapturedEgress, error) {
	policyRepo := projectPolicyRepo(spec)
	canonical, err := canonicalProjectDir(policyRepo)
	if err != nil {
		return nil, err
	}
	// refusal is why this project gets at most a child that holds prompts. A file that does not load says
	// nothing here, however much of it reads as rules, so from now on it is one that asks for nothing.
	var refusal error
	p, err := project.Load(policyRepo)
	broken := err != nil
	if broken {
		refusal = &BrokenProjectFile{Err: err}
		if !options.HoldPending {
			return nil, refusal
		}
		p = &project.Project{}
	}
	spec.projectEnv = p.Box.Env
	spec.deferProjectBuild = options.ProjectBuild == DeferProjectBuild
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
	// Every launch that mounts the shared MCP configuration proves its source first: that it lives
	// outside every mount and parses. That is a property of the file the box will read, so it holds
	// whatever the posture. What does NOT hold for every posture is the filtered gateway's
	// literal-only rule, which is why the destinations are derived from this snapshot later, on
	// the filtered path alone.
	mcpSnapshot, err := networkMCPSnapshot(cfg, spec)
	if err != nil {
		return nil, err
	}
	// The preview publishes nothing and creates no owner key, so it is safe on
	// every launch. It still proves the authority root is outside every mount.
	// A pending request fails closed HERE, before any box or main process, with
	// the one review command that settles it.
	preview, err := networkstate.PreviewAdmission(root, canonical, exposed, input)
	if err != nil {
		return nil, err
	}
	if err := checkRuleFlags(spec, options, preview.Mode); err != nil {
		return nil, err
	}
	mode := preview.Mode
	if notice := ignoredHostEgress(input, mode); notice != "" && previewed == nil {
		ui.Warn("%s", notice)
	}
	if preview.Pending != nil && !broken { // compared with a request of nothing, a broken file's pending says nothing
		refusal = pendingRefusal(cfg, spec, p, input, root, canonical, exposed, preview.Pending)
	}
	if refusal != nil {
		if !options.HoldPending || mode == egress.None {
			return nil, refusal
		}
		// A holding child runs filtered. Where nothing filtered can run, no child can hold the session,
		// and what the person has to settle comes first anyway.
		if checkFilteredRuntime(rt) != nil || checkFilteredSupport(cfg) != nil {
			return nil, refusal
		}
		// Whoever chose open did not ask for this host's filtered setup, which can take minutes, only to
		// hear about a broken file the refusal names just as well.
		options.ExistingQualification = options.ExistingQualification || broken && mode != egress.Filtered
		mode, input = egress.Filtered, holdingAdmission(input)
	}
	if previewed != nil {
		*previewed = mode
		return nil, nil
	}
	// Said only for a launch that goes ahead in this mode: a refused or held one never got it.
	if notice := widenedProjectEgress(input, mode); notice != "" {
		ui.Warn("%s", notice)
	}
	cfg.SetEgress(string(mode))
	if mode != egress.Filtered {
		return nil, nil
	}
	capture, err := captureFilteredNetwork(cfg, rt, spec, options, canonical, root, exposed, input, mcpSnapshot)
	if err != nil {
		var unsettled *networkstate.PendingApproval
		if broken || refusal != nil && errors.As(err, &unsettled) {
			return nil, refusal // a withdrawal or a replaced folder: nothing starts until a person looks
		}
		return nil, err
	}
	capture.Default = preview.Default
	if broken {
		capture.BrokenFile = true
	} else {
		capture.Pending = preview.Pending
	}
	return capture, nil
}

// pendingRefusal refuses a launch whose project asks for access nobody approved, naming the review
// that settles it. When coop approve would refuse the request too, a review settles nothing, so it
// names approve's reason and the edit instead.
func pendingRefusal(cfg *config.Config, spec RunSpec, p *project.Project, input networkstate.Admission, root, canonical string, exposed []string, pending *networkstate.PendingApproval) error {
	review := ApproveCommand(approvingProject(cfg), canonical)
	if before, err := priorApproval(root, canonical, exposed); err == nil {
		if refused := approvalRefusal(p, approvalMode(input, before)); refused != nil {
			return fmt.Errorf("%s cannot start because %w\n\n  Change %s, then review it: %s", networkLaunchName(spec), unapprovable(refused), project.File, review)
		}
	}
	return fmt.Errorf("%s cannot start because %w\n\n  Review it: %s", networkLaunchName(spec), pending, review)
}

// captureFilteredNetwork is the filtered half of an admission, once the mode and the inputs are settled.
func captureFilteredNetwork(cfg *config.Config, rt runtime.Runtime, spec RunSpec, options NetworkAdmission, canonical, root string, exposed []string, input networkstate.Admission, mcpSnapshot []byte) (*CapturedEgress, error) {
	// A runtime without the gateway is refused here even when the default chose filtered: a run
	// nobody chose open for never quietly becomes one.
	if err := checkFilteredRuntime(rt); err != nil {
		return nil, err
	}
	if err := checkFilteredSupport(cfg); err != nil {
		return nil, err
	}
	// The shared file's destinations are derived only now, once the posture is filtered and before
	// the store is opened — the order the store's preview is built for, and what prepareFiltered
	// already does for sessions.
	var err error
	if input.Automatic, err = networkMCPDependenciesOf(mcpSnapshot); err != nil {
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
	var qualify func(context.Context) error
	if !options.ExistingQualification {
		qualify = func(ctx context.Context) error {
			if _, err := SetupNetwork(ctx, cfg, rt, networkLaunchStderr(spec), networkLaunchStderr(spec)); err != nil {
				return fmt.Errorf("%s cannot start — %w", networkLaunchName(spec), err)
			}
			return nil
		}
	}
	capture, err := admitFilteredNetwork(cfg, rt, spec, store, canonical, input, qualify)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	return capture, nil
}

// AdmitHoldingNetwork captures holdingAdmission for an editor session whose real boxes run under
// another capture: a child that holds prompts runs under this one, so nothing of the project's can stop
// it. It reads no project file, which an agent may have broken, and never sets the host up. A
// withdrawal or a replaced folder still refuses.
func AdmitHoldingNetwork(cfg *config.Config, rt runtime.Runtime, spec RunSpec, options NetworkAdmission) (*CapturedEgress, error) {
	if cfg == nil {
		return nil, errors.New("restricted networking needs host configuration")
	}
	canonical, err := canonicalProjectDir(projectPolicyRepo(spec))
	if err != nil {
		return nil, err
	}
	spec.deferProjectBuild, options.ExistingQualification = true, true
	root, err := NetworkStatePath()
	if err != nil {
		return nil, err
	}
	exposed, err := networkExposureRoots(cfg, spec)
	if err != nil {
		return nil, err
	}
	input, err := networkAdmissionInput(cfg, &project.Project{}, options, exposed)
	if err != nil {
		return nil, err
	}
	mcpSnapshot, err := networkMCPSnapshot(cfg, spec)
	if err != nil {
		return nil, err
	}
	cfg.SetEgress(string(egress.Filtered))
	return captureFilteredNetwork(cfg, rt, spec, options, canonical, root, exposed, holdingAdmission(input), mcpSnapshot)
}

// holdingAdmission is what a child that holds prompts runs under: the person's own flags, the host's
// shared connectivity and what the providers need, and nothing a project's file asks for, approved or
// not. No turn runs there to use any of it, and a service grant would start the project's sidecars.
// It decides the mode for this one capture and changes no approval.
func holdingAdmission(input networkstate.Admission) networkstate.Admission {
	filtered := egress.Filtered
	input.InvocationMode, input.ProjectMode = &filtered, nil
	input.Requests, input.Services = nil, nil
	return input
}

// checkRuleFlags refuses the person's own rule flags on a run that is not filtered. They only add
// to filtered access, so dropping them quietly would start a box that cannot reach what was asked
// for. The project's own rules are not checked here: a one-off --egress open or offline sets them
// aside, and any other mode leaves them pending (networkstate.Admission.pendingApproval).
func checkRuleFlags(spec RunSpec, options NetworkAdmission, mode egress.Mode) error {
	if mode == egress.Filtered {
		return nil
	}
	var flags []string
	if len(options.Domains) != 0 {
		flags = append(flags, "--allow-domain")
	}
	if options.RulesFile != "" {
		flags = append(flags, "--egress-rules")
	}
	if len(flags) == 0 {
		return nil
	}
	adds := "adds"
	if len(flags) > 1 {
		adds = "add"
	}
	named := strings.Join(flags, " and ")
	return fmt.Errorf("%s cannot start because %s only %s to filtered access, and this run is %s\n\n  Use --egress filtered, or leave out %s",
		networkLaunchName(spec), named, adds, mode.Word(), named)
}

// ApproveCommand is the command that reviews the network request of the project in folder, for a
// person whose coop approve reviews the project here. A fork's approvals are its own, so a launch
// in another checkout, such as a fork loop started from its parent, names that checkout. COOP_REPO,
// not cd: the next command a person runs, a fork restart say, belongs in the folder they are in.
func ApproveCommand(here, folder string) string { return commandFor(here, folder, "coop approve") }

// BuildCommand is ApproveCommand for `coop build --egress filtered`, which builds the checkout it
// runs in: a fork's build files, like its approvals, are its own.
func BuildCommand(here, folder string) string {
	return commandFor(here, folder, "coop build --egress filtered")
}

// commandFor is command for the project in folder, run by a person whose coop works on the project
// here: as it is when that is the same folder, else with COOP_REPO naming folder.
func commandFor(here, folder, command string) string {
	if here != "" && SameFolder(here, folder) {
		return command
	}
	if word, ok := ui.ShellWord(folder); ok {
		return "COOP_REPO=" + word + " " + command
	}
	return command
}

// withBuildCommand has a project build miss name the command that builds its files for this
// process's person (BuildCommand): a launch in another checkout, such as a fork's started from its
// parent, needs that checkout's.
func withBuildCommand(cfg *config.Config, spec RunSpec, err error) error {
	var required *ProjectBuildRequired
	if errors.As(err, &required) {
		required.Command = projectBuildCommand(cfg, projectPolicyRepo(spec))
	}
	return err
}

// projectBuildCommand is BuildCommand for the project in folder, from the project this process's
// person builds.
func projectBuildCommand(cfg *config.Config, folder string) string {
	if canonical, err := canonicalProjectDir(folder); err == nil {
		folder = canonical
	}
	here := ""
	if cfg != nil {
		here = approvingProject(cfg)
	}
	return BuildCommand(here, folder)
}

// approvingProject is the project coop approve reviews, and coop build builds, when this process's
// person runs it, as cli.netProject finds it: COOP_REPO, else the working folder's checkout. "" when
// there is none.
func approvingProject(cfg *config.Config) string {
	if cfg.RepoOverride != "" {
		here, err := ResolveRepo(cfg.RepoOverride)
		if err != nil {
			return ""
		}
		return here
	}
	here, ok, err := FindRepo()
	if err != nil || !ok {
		return ""
	}
	return here
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

// ignoredHostEgress names a COOP_EGRESS a saved approval outranked. Someone who set offline must not
// believe a run is offline while it reaches its approved sites.
func ignoredHostEgress(input networkstate.Admission, mode egress.Mode) string {
	if input.HostPreference == nil || input.InvocationMode != nil || *input.HostPreference == mode {
		return ""
	}
	return fmt.Sprintf("COOP_EGRESS=%s does not apply: this project's approval keeps it %s. For one run, use --egress %s.",
		input.HostPreference.Word(), mode.Word(), input.HostPreference.Word())
}

// widenedProjectEgress names a COOP_EGRESS that gives a run more network access than box.egress
// asks for. COOP_EGRESS outranks the project file, but someone who set it for every project must not
// believe a project its team keeps offline still is. A one-off --egress is that run's own choice.
func widenedProjectEgress(input networkstate.Admission, mode egress.Mode) string {
	if input.InvocationMode != nil || input.HostPreference == nil || *input.HostPreference != mode ||
		input.ProjectMode == nil || !reachesMore(mode, *input.ProjectMode) {
		return ""
	}
	return fmt.Sprintf("COOP_EGRESS=%s overrides box.egress: %s in %s. For one run, use --egress %s.",
		mode.Word(), input.ProjectMode.Word(), project.File, input.ProjectMode.Word())
}

// reachesMore orders the modes by what a run can reach: offline nothing, filtered what was
// approved, open everything.
func reachesMore(mode, than egress.Mode) bool {
	order := []egress.Mode{egress.None, egress.Filtered, egress.Open}
	return slices.Index(order, mode) > slices.Index(order, than)
}

// networkAdmissionInput separates presence from default and authority from
// request. The built-in default is nobody's choice, so an unset COOP_EGRESS
// contributes no host preference at all.
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

// checkFilteredRuntime refuses a launch whose runtime can never serve a filtered box. Every
// filtered entry point asks it before it opens the approval store or starts the host
// qualification, so the refusal names the feature and Docker and leaves no authority state behind
// — instead of surfacing much later as a gateway bind failure. It is the FIRST filtered gate:
// unsetting COOP_IMAGE gets a person on another runtime nowhere. Only launches ask it; resolving
// what a policy means does not. Filtered is the built-in default, so the refusal names every way
// to choose another mode as well as the runtime that works.
func checkFilteredRuntime(rt runtime.Runtime) error {
	if rt.SupportsFilteredNetwork() {
		return nil
	}
	return fmt.Errorf("restricted networking needs docker; %s cannot run the network gateway — run this with --egress open or --egress offline, set COOP_EGRESS, or set COOP_RUNTIME=docker", filepath.Base(rt.Name))
}

// checkFilteredSupport refuses a combination this release cannot enforce, before
// any host state or runtime resource exists. An unsupported tuple fails; it is
// never downgraded to a warning and a wider policy.
//
// A project's own .agent/Dockerfile is NOT one of these: it is built on the
// locked client image explicitly and proven derived from it at launch (derived_image.go).
// An arbitrary COOP_IMAGE cannot be — nothing qualified it, and no proof can
// turn an unrelated image into the one this host set up.
func checkFilteredSupport(cfg *config.Config) error {
	if cfg.ImageOverride != "" {
		return errors.New("a filtered box runs coop's own image — unset COOP_IMAGE to start one")
	}
	return nil
}

// checkSupportedRequests applies the runtime capability gate to every concrete
// rule this launch would authorize, one by one and then all together, since two
// rules can clash where each alone is fine. coop approve asks it of a project's
// rules (approvalRefusal), so the two cannot disagree. Provider selectors are
// skipped: they expand from the trusted release bundle into concrete rules the
// same gate re-checks on the compiled snapshot.
func checkSupportedRequests(input networkstate.Admission) error {
	sources := [][]egress.Rule{input.Requests}
	for _, operator := range input.Operator {
		sources = append(sources, operator.Rules)
	}
	for _, automatic := range input.Automatic {
		sources = append(sources, automatic.Rules)
	}
	var concrete []egress.Rule
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
			concrete = append(concrete, rule)
		}
	}
	return egress.SupportedTogether(concrete)
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
	// Read before the approval: an approval change published after this read advances the
	// generation past it, so a supervisor that polls from this value cannot miss one. It is a
	// hint, never authority, so an unreadable one only means "check again" (zero).
	generation, _ := store.ApprovalGeneration()
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
	if !spec.deferProjectBuild {
		if err := checkFilteredProjectReadiness(ctx, docker, store, spec, networkRuntimeBinding(docker.Info(), docker.Endpoint())); err != nil {
			return nil, err
		}
	}
	qualification, err := ensureNetworkQualification(ctx, func() (*networkstate.Qualification, error) {
		return currentQualification(ctx, docker, store, policy, cfg.ImageOverride)
	}, qualify)
	if err != nil {
		return nil, err
	}
	return &CapturedEgress{Store: store, Project: canonicalProject, Fingerprint: policy.Fingerprint, QualificationID: qualification.ID, ApprovalGeneration: generation}, nil
}

// ErrNetworkSetupRequired refuses a launch that may not set the host up (ExistingQualification) when
// no current host proof covers it.
var ErrNetworkSetupRequired = errors.New("this host is not set up for filtered runs with this Docker and these agents — run 'coop net setup' first")

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
		return nil, ErrNetworkSetupRequired
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
	// RequireLaunch fails every record alike for a policy this runtime cannot enforce, and no setup
	// changes the policy. Read as "no proof", it would set the host up again on every launch only
	// to fail, so it is refused as itself.
	if err := policy.RequireSupported(); err != nil {
		return nil, err
	}
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
