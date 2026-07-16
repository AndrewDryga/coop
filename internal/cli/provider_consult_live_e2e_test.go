//go:build providerlivee2e

// Opt-in compatibility for the real cross-provider coop-consult ring. The clean child invokes the
// mounted wrapper directly, so each edge exercises lead-to-peer box wiring while starting one peer
// CLI session and no lead session. Run with make provider-consult-live-e2e[-all].
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/fusion"
	"github.com/AndrewDryga/coop/internal/liveprocess"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

const consultLiveChildDeadline = 18 * time.Minute

func TestProviderConsultLiveCompatibility(t *testing.T) {
	if os.Getenv("COOP_TEST_CONSULT_LIVE_CHILD") == "1" {
		t.Skip("parent orchestration is disabled in the clean consult child")
	}
	targets, strict, err := liveprovider.ParseTargets(os.Getenv("COOP_LIVE_TARGETS"))
	if err != nil {
		t.Fatal(err)
	}
	strict = strict || os.Getenv("COOP_LIVE_REQUIRE_ALL") == "1"
	if err := liveprovider.ValidateConsultTargets(targets); err != nil {
		t.Fatal(err)
	}

	realConfig := config.Load()
	rt, err := runtime.Detect(realConfig.RuntimeName)
	if err != nil {
		emitConsultLiveSummary(t, strict, targets, skippedLiveResults(targets, liveprovider.ReasonMissingRuntime))
		return
	}
	if err := rt.EnsureDaemon(); err != nil {
		emitConsultLiveSummary(t, strict, targets, skippedLiveResults(targets, liveprovider.ReasonMissingRuntime))
		return
	}
	connectionEnv, err := liveprovider.CaptureRuntimeConnectionEnv(rt.Name)
	if err != nil {
		emitConsultLiveSummary(t, strict, targets, failedConsultLiveResults(targets, liveprovider.ReasonHarnessFailed, "runtime_connection"))
		return
	}
	runtimeSettings := liveprovider.RuntimeSettings{
		Name: rt.Name, Image: realConfig.ImageOverride, BaseImage: realConfig.BaseImage,
		HomeInBox: realConfig.HomeInBox, AgentPackages: realConfig.AgentPackages,
		ConnectionEnv: connectionEnv,
	}
	image := box.ImageForRepo(t.TempDir(), realConfig.BaseImage, realConfig.ImageOverride)
	if !box.ImageExists(rt, image) {
		emitConsultLiveSummary(t, strict, targets, skippedLiveResults(targets, liveprovider.ReasonMissingImage))
		return
	}

	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		emitConsultLiveSummary(t, strict, targets, failedConsultLiveResults(targets, liveprovider.ReasonHarnessFailed, "layout_setup"))
		return
	}
	if err := liveprovider.InitRepository(layout); err != nil {
		emitConsultLiveSummary(t, strict, targets, failedConsultLiveResults(targets, liveprovider.ReasonHarnessFailed, "repository_setup"))
		return
	}
	before, err := liveprovider.SnapshotRepository(layout)
	if err != nil {
		emitConsultLiveSummary(t, strict, targets, failedConsultLiveResults(targets, liveprovider.ReasonHarnessFailed, "repository_snapshot"))
		return
	}
	selections, err := liveprovider.SelectionsForTargets(realConfig, targets)
	if err != nil {
		emitConsultLiveSummary(t, strict, targets, failedConsultLiveResults(targets, liveprovider.ReasonHarnessFailed, "target_selection"))
		return
	}
	if err := os.Remove(layout.Config); err != nil {
		emitConsultLiveSummary(t, strict, targets, failedConsultLiveResults(targets, liveprovider.ReasonHarnessFailed, "config_reset"))
		return
	}
	prepared, err := liveprovider.Prepare(realConfig.ConfigDir, layout.Config, selections)
	if err != nil {
		emitConsultLiveSummary(t, strict, targets, failedConsultLiveResults(targets, liveprovider.ReasonUnsafeCredential, liveprovider.CredentialDetailCode(err)))
		return
	}
	defer func() { _ = prepared.Revoke() }()
	if err := prepared.VerifySources(); err != nil {
		emitConsultLiveSummary(t, strict, targets, finalizedConsultLiveResults(
			failedConsultLiveResults(targets, liveprovider.ReasonHarnessFailed, "source_preflight"),
			liveprovider.VerificationFailures{SourceChanged: true}, nil,
		))
		return
	}
	preflight := map[string]string{}
	for _, target := range targets {
		reason := prepared.PreflightReason(target.Provider, prepared.Account(target.Provider), time.Now().Add(consultLiveChildDeadline))
		if reason == liveprovider.ReasonUnsafeCredential {
			emitConsultLiveSummary(t, strict, targets, failedConsultLiveResults(targets, liveprovider.ReasonUnsafeCredential, "credential_portability"))
			return
		}
		preflight[target.Provider] = reason
	}

	marker, err := liveIdentifier("CONSULT_LIVE_")
	if err != nil {
		emitConsultLiveSummary(t, strict, targets, failedConsultLiveResults(targets, liveprovider.ReasonHarnessFailed, "identifier_generation"))
		return
	}
	supervisor, err := liveIdentifier("consult-live-")
	if err != nil {
		emitConsultLiveSummary(t, strict, targets, failedConsultLiveResults(targets, liveprovider.ReasonHarnessFailed, "identifier_generation"))
		return
	}
	resultFile := filepath.Join(layout.State, "consult-result.json")
	attemptDir := filepath.Join(layout.State, "consult-attempts")
	cidDir := filepath.Join(layout.State, "consult-cids")
	for _, dir := range []string{attemptDir, cidDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			emitConsultLiveSummary(t, strict, targets, failedConsultLiveResults(targets, liveprovider.ReasonHarnessFailed, "control_directory"))
			return
		}
	}
	control, _, err := liveprovider.NewProcessControl(layout, false)
	if err != nil {
		emitConsultLiveSummary(t, strict, targets, failedConsultLiveResults(targets, liveprovider.ReasonHarnessFailed, "process_control"))
		return
	}
	defer control.Close()
	revokePath, err := prepared.RevocationPath()
	if err != nil {
		emitConsultLiveSummary(t, strict, targets, failedConsultLiveResults(targets, liveprovider.ReasonHarnessFailed, "credential_revocation"))
		return
	}
	env, err := liveprovider.ConsultChildEnvironment(layout, liveprovider.ConsultChildSpec{
		Path: os.Getenv("PATH"), Marker: marker, Targets: targets, Strict: strict,
		ResultFile: resultFile, AttemptDir: attemptDir, Supervisor: supervisor, CIDDir: cidDir,
		PreflightReasons: preflight, ControlFD: 3, RevokePath: revokePath, Runtime: runtimeSettings,
	})
	if err != nil {
		emitConsultLiveSummary(t, strict, targets, failedConsultLiveResults(targets, liveprovider.ReasonHarnessFailed, "child_environment"))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), consultLiveChildDeadline)
	processResult := procharness.Run(ctx, procharness.Command{
		Path: os.Args[0], Args: []string{"-test.run=^TestProviderConsultLiveChild$", "-test.v=false"},
		Dir: layout.Repo, Env: env, MaxOutput: liveOutputLimit, KillGrace: 3 * time.Second,
		ExtraFiles: []*os.File{control}, BeforeCancel: prepared.Revoke,
	})
	timedOut := liveprovider.ProcessDeadlineExceeded(processResult)
	cancel()
	results := classifyConsultLiveChild(targets, strict, layout.Root, resultFile, attemptDir, processResult, timedOut)
	revokeErr := prepared.Revoke()
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), liveCleanupDeadline)
	cleanupErr := liveprovider.CleanupSupervisor(cleanupCtx, liveprovider.SupervisorCleanupSpec{
		Root: layout.Root, CIDDir: cidDir, Supervisor: supervisor, LabelKey: box.LabelSupervisor,
		Phases:           consultLiveCleanupPhases(targets),
		OperationTimeout: 2 * time.Second, QuietPeriod: time.Second, PollInterval: 100 * time.Millisecond,
	}, liveprovider.SupervisorCleanupOps{
		RemoveContainer: rt.RemoveContainerContext,
		RemoveByLabel:   rt.RemoveByLabel,
	})
	cancelCleanup()
	after, snapshotErr := liveprovider.VerifyRepository(layout, before)
	sourceErr := prepared.VerifySources()
	attempted := observedConsultAttempts(layout.Root, attemptDir, targets)
	results = finalizedConsultLiveResults(results, liveprovider.VerificationFailures{
		CleanupFailed:     cleanupErr != nil || revokeErr != nil,
		SourceChanged:     sourceErr != nil,
		RepositoryChanged: snapshotErr != nil || !before.Equal(after),
	}, attempted)
	emitConsultLiveSummary(t, strict, targets, results)
}

func emitConsultLiveSummary(t *testing.T, strict bool, targets []agents.Target, results []liveprovider.ProviderResult) {
	t.Helper()
	summary, err := liveprovider.NewConsultSummary(strict, targets, results)
	if err != nil {
		t.Fatal(err)
	}
	line, err := summary.Line()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, line)
	if !summary.Success() {
		t.Fail()
	}
}

func failedConsultLiveResults(targets []agents.Target, reason, detail string) []liveprovider.ProviderResult {
	results := consultLiveResultSkeleton(targets)
	for i := range results {
		results[i].Status = liveprovider.StatusFailed
		results[i].ReasonCode = reason
		results[i].Phase = "harness"
		results[i].ErrorClass = "harness"
		results[i].DetailCode = detail
	}
	return results
}

func consultLiveResultSkeleton(targets []agents.Target) []liveprovider.ProviderResult {
	results := make([]liveprovider.ProviderResult, 0, len(targets))
	for _, target := range targets {
		results = append(results, liveprovider.ProviderResult{Provider: target.Provider})
	}
	return results
}

func classifyConsultLiveChild(
	targets []agents.Target,
	strict bool,
	root, resultFile, attemptDir string,
	process procharness.Result,
	timedOut bool,
) []liveprovider.ProviderResult {
	attempted := observedConsultAttempts(root, attemptDir, targets)
	clean := process.ExitCode == 0 && process.Err == nil && !process.StdoutTruncated && !process.StderrTruncated
	if clean {
		summary, err := liveprovider.ReadConsultChildSummary(root, resultFile, strict, targets)
		if err == nil {
			results := summary.PeerResults()
			for i := range results {
				if results[i].Attempted != attempted[i] {
					results[i] = consultHarnessFailure(
						results[i].Provider, attempted[i], "attempt_mismatch",
					)
				}
			}
			return results
		}
	}
	results := consultLiveResultSkeleton(targets)
	lastAttempt := -1
	for i, seen := range attempted {
		if seen {
			lastAttempt = i
		}
	}
	for i := range results {
		provider := results[i].Provider
		switch {
		case timedOut && lastAttempt < 0:
			results[i] = liveprovider.ProviderResult{
				Provider: provider, Status: liveprovider.StatusFailed,
				ReasonCode: liveprovider.ReasonVersionProbe, Phase: "version",
				TimedOut: true, ErrorClass: "timeout", DetailCode: "child_deadline",
			}
		case timedOut && i == lastAttempt:
			results[i] = liveprovider.ProviderResult{
				Provider: provider, Attempted: true, Status: liveprovider.StatusFailed,
				ReasonCode: liveprovider.ReasonPromptTimeout, Phase: "prompt",
				TimedOut: true, ErrorClass: "timeout", DetailCode: "child_deadline",
			}
		case attempted[i]:
			results[i] = consultHarnessFailure(provider, true, "child_result")
		default:
			results[i] = consultHarnessFailure(provider, false, "prior_edge_failed")
		}
		if !timedOut && i == 0 {
			results[i].ExitCode = process.ExitCode
			results[i].Truncated = process.StdoutTruncated || process.StderrTruncated
		}
	}
	return results
}

func finalizedConsultLiveResults(
	results []liveprovider.ProviderResult,
	failures liveprovider.VerificationFailures,
	attempted []bool,
) []liveprovider.ProviderResult {
	final := append([]liveprovider.ProviderResult(nil), results...)
	for i := range final {
		perEdge := failures
		if len(attempted) > i {
			perEdge.AttemptedObserved = attempted[i]
		}
		final[i] = liveprovider.FinalizeResult(final[i], perEdge)
	}
	return final
}

func consultHarnessFailure(provider string, attempted bool, detail string) liveprovider.ProviderResult {
	return liveprovider.ProviderResult{
		Provider: provider, Attempted: attempted, Status: liveprovider.StatusFailed,
		ReasonCode: liveprovider.ReasonHarnessFailed, Phase: "harness",
		ErrorClass: "harness", DetailCode: detail,
	}
}

func observedConsultAttempts(root, attemptDir string, targets []agents.Target) []bool {
	result := make([]bool, len(targets))
	for i := range targets {
		result[i] = liveprovider.ControlFilePresent(root, filepath.Join(attemptDir, consultLiveEdgePhase(targets, i)))
	}
	return result
}

func consultLiveCleanupPhases(targets []agents.Target) []string {
	phases := make([]string, 0, len(targets)*2)
	for _, target := range targets {
		phases = append(phases, "version-"+target.Provider)
	}
	for i := range targets {
		phases = append(phases, consultLiveEdgePhase(targets, i))
	}
	return phases
}

func consultLiveEdgePhase(targets []agents.Target, index int) string {
	return "edge-" + consultLiveLead(targets, index).Provider + "-" + targets[index].Provider
}

func consultLiveLead(targets []agents.Target, peerIndex int) agents.Target {
	return targets[(peerIndex+len(targets)-1)%len(targets)]
}

func TestProviderConsultLiveChild(t *testing.T) {
	if os.Getenv("COOP_TEST_CONSULT_LIVE_CHILD") != "1" {
		t.Skip("consult live child helper runs only under TestProviderConsultLiveCompatibility")
	}
	targets, _, err := liveprovider.ParseTargets(os.Getenv("COOP_TEST_CONSULT_LIVE_TARGETS"))
	if err != nil || liveprovider.ValidateConsultTargets(targets) != nil {
		t.Fatal("invalid consult live child targets")
	}
	strict, ok := parseConsultLiveStrict(os.Getenv("COOP_TEST_CONSULT_LIVE_STRICT"))
	if !ok {
		t.Fatal("invalid consult live child strict mode")
	}
	marker := os.Getenv("COOP_TEST_CONSULT_LIVE_MARKER")
	resultFile := os.Getenv("COOP_TEST_CONSULT_LIVE_RESULT")
	attemptDir := os.Getenv("COOP_TEST_CONSULT_LIVE_ATTEMPT_DIR")
	supervisor := os.Getenv("COOP_TEST_CONSULT_LIVE_SUPERVISOR")
	cidDir := os.Getenv("COOP_TEST_CONSULT_LIVE_CID_DIR")
	if err := validateConsultLiveChildControls(marker, resultFile, attemptDir, supervisor, cidDir); err != nil {
		t.Fatal("invalid consult live child controls")
	}
	preflight := map[string]string{}
	for _, target := range targets {
		preflight[target.Provider] = os.Getenv("COOP_TEST_CONSULT_LIVE_PREFLIGHT_" + strings.ToUpper(target.Provider))
	}
	results := executeProviderConsultLiveChild(targets, marker, attemptDir, supervisor, cidDir, preflight)
	summary, err := liveprovider.NewConsultSummary(strict, targets, results)
	if err != nil {
		t.Fatal("invalid consult live child result")
	}
	if err := writeConsultLiveChildSummary(resultFile, summary); err != nil {
		t.Fatal("write consult live child result")
	}
}

func executeProviderConsultLiveChild(
	targets []agents.Target,
	marker, attemptDir, supervisor, cidDir string,
	preflight map[string]string,
) []liveprovider.ProviderResult {
	results := consultLiveResultSkeleton(targets)
	cfg := config.Load()
	rt, err := runtime.Detect(cfg.RuntimeName)
	if err != nil {
		return skippedLiveResults(targets, liveprovider.ReasonMissingRuntime)
	}
	image := box.ImageForRepo(cfg.RepoOverride, cfg.BaseImage, cfg.ImageOverride)
	credentialSpecs := make(map[string]agents.LiveCredentialSpec, len(targets))

	for i, peer := range targets {
		results[i], credentialSpecs[peer.Provider] = probeConsultLiveVersion(
			cfg, rt, image, supervisor, cidDir, peer, preflight[peer.Provider],
		)
	}

	hasFailure, hasSkip := false, false
	for i := range results {
		hasFailure = hasFailure || results[i].Status == liveprovider.StatusFailed
		hasSkip = hasSkip || results[i].Status == liveprovider.StatusSkipped
	}
	if hasFailure || hasSkip {
		for i := range results {
			if results[i].Status != "" {
				continue
			}
			version := results[i].CLIVersion
			if hasFailure {
				results[i] = consultHarnessFailure(results[i].Provider, false, "prior_version_failure")
			} else {
				results[i].Status = liveprovider.StatusSkipped
				results[i].ReasonCode = liveprovider.ReasonRingPrerequisite
			}
			results[i].CLIVersion = version
		}
		return results
	}

	for i, peer := range targets {
		results[i] = runConsultLiveEdge(
			cfg, rt, image, targets, i, results[i], credentialSpecs[peer.Provider],
			marker, attemptDir, supervisor, cidDir,
		)
		if results[i].Status != liveprovider.StatusPassed {
			return stopConsultLiveAdmission(results, i)
		}
	}
	return results
}

func probeConsultLiveVersion(
	cfg *config.Config,
	rt runtime.Runtime,
	image, supervisor, cidDir string,
	peer agents.Target,
	preflightReason string,
) (liveprovider.ProviderResult, agents.LiveCredentialSpec) {
	result := liveprovider.ProviderResult{Provider: peer.Provider}
	configureConsultLiveTarget(cfg, peer)
	ag, ok := agents.Get(peer.Provider)
	if !ok {
		return consultHarnessFailure(peer.Provider, false, "provider_registry"), agents.LiveCredentialSpec{}
	}
	live := ag.LiveCredentials()
	if len(live.Artifacts) == 0 || live.Portability == nil || len(live.AuthSignals) == 0 {
		return consultHarnessFailure(peer.Provider, false, "credential_contract"), live
	}
	interactive := ag.Interactive(cfg)
	if len(interactive) == 0 {
		return failedConsultVersion(peer.Provider, 0, false, false, "harness"), live
	}
	stdout := liveprovider.NewBoundedBuffer(64 << 10)
	stderr := liveprovider.NewBoundedBuffer(64 << 10)
	ctx, cancel := context.WithTimeout(context.Background(), liveVersionDeadline)
	code, runErr := box.Run(cfg, rt, box.RunSpec{
		Image: image, Repo: cfg.RepoOverride, Cmd: []string{interactive[0], "--version"},
		Agent: peer.Provider, Batch: true, RepoReadOnly: true, Quiet: true,
		SupervisorID: supervisor, Stdout: stdout, Stderr: stderr, Ctx: ctx,
		ExtraArgs: liveCIDArgs(rt, cidDir, "version-"+peer.Provider),
	})
	timedOut := errors.Is(runErr, context.DeadlineExceeded)
	cancel()
	if code == 127 {
		result.Status = liveprovider.StatusSkipped
		result.ReasonCode = liveprovider.ReasonMissingCLI
		return result, live
	}
	if runErr != nil || code != 0 || timedOut || stdout.Truncated() || stderr.Truncated() {
		truncated := stdout.Truncated() || stderr.Truncated()
		class := agents.ClassifyCLIError(live, stdout.String()+"\n"+stderr.String())
		if timedOut {
			class = "timeout"
		} else if truncated {
			class = "output_truncated"
		} else if class == "" {
			class = "process"
		}
		return failedConsultVersion(peer.Provider, code, timedOut, truncated, class), live
	}
	result.CLIVersion = liveprovider.CLIVersion(peer.Provider, stdout.String(), stderr.String())
	if result.CLIVersion == "" {
		return failedConsultVersion(peer.Provider, code, false, false, "empty_output"), live
	}
	if preflightReason != "" {
		result.Status = liveprovider.StatusSkipped
		result.ReasonCode = preflightReason
	}
	return result, live
}

func runConsultLiveEdge(
	cfg *config.Config,
	rt runtime.Runtime,
	image string,
	targets []agents.Target,
	index int,
	result liveprovider.ProviderResult,
	credentialSpec agents.LiveCredentialSpec,
	marker, attemptDir, supervisor, cidDir string,
) liveprovider.ProviderResult {
	peer := targets[index]
	lead := consultLiveLead(targets, index)
	configureConsultLiveTarget(cfg, lead)
	configureConsultLiveTarget(cfg, peer)
	repositoryLayout := procharness.Layout{
		Root: filepath.Dir(cfg.RepoOverride), Repo: cfg.RepoOverride,
		Home: os.Getenv("HOME"), GitConfig: os.Getenv("GIT_CONFIG_GLOBAL"),
	}
	before, err := liveprovider.SnapshotRepository(repositoryLayout)
	if err != nil {
		return consultHarnessFailure(peer.Provider, false, "repository_snapshot")
	}
	phase := consultLiveEdgePhase(targets, index)
	if err := writeConsultAttempt(filepath.Join(attemptDir, phase)); err != nil {
		return consultHarnessFailure(peer.Provider, false, "attempt_marker")
	}
	result.Attempted = true
	edgeMarker := marker + "_" + strings.ToUpper(peer.Provider)
	prompt := consultLivePrompt(peer.Provider, edgeMarker)
	stdout := liveprovider.NewBoundedBuffer(liveOutputLimit)
	stderr := liveprovider.NewBoundedBuffer(liveOutputLimit)
	ctx, cancel := context.WithTimeout(context.Background(), livePromptDeadline)
	peerScope := peer
	peerScope.Accounts = nil
	edgePreset := consultLiveRingPreset(lead.Provider, peerScope)
	code, runErr := box.Run(cfg, rt, box.RunSpec{
		Image: image, Repo: cfg.RepoOverride,
		Cmd:   []string{fusion.ConsultWrapperPath, "live-probe", "--fresh", prompt},
		Agent: lead.Provider, ConsultLead: lead.Provider, Preset: edgePreset,
		Batch: true, Quiet: true, Homes: true, Network: false, Cache: false,
		SupervisorID: supervisor, Stdout: stdout, Stderr: stderr, Ctx: ctx,
		ExtraArgs: liveCIDArgs(rt, cidDir, phase),
	})
	timedOut := errors.Is(runErr, context.DeadlineExceeded)
	cancel()
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), liveCleanupDeadline)
	edgeCleanupErr := liveprovider.CleanupSupervisor(cleanupCtx, liveprovider.SupervisorCleanupSpec{
		Root: filepath.Dir(cfg.RepoOverride), CIDDir: cidDir, Supervisor: supervisor, LabelKey: box.LabelSupervisor,
		Phases: []string{phase}, OperationTimeout: 2 * time.Second,
		QuietPeriod: time.Second, PollInterval: 100 * time.Millisecond,
	}, liveprovider.SupervisorCleanupOps{
		RemoveContainer: rt.RemoveContainerContext,
		RemoveByLabel:   rt.RemoveByLabel,
	})
	cancelCleanup()
	if edgeCleanupErr != nil {
		return consultHarnessFailure(peer.Provider, true, "edge_cleanup")
	}
	if timedOut {
		result.Status = liveprovider.StatusFailed
		result.ReasonCode = liveprovider.ReasonPromptTimeout
		result.Phase = "prompt"
		result.TimedOut = true
		result.ErrorClass = "timeout"
		result.DetailCode = "edge_deadline"
		return result
	}
	if runErr != nil || code != 0 || stdout.Truncated() || stderr.Truncated() {
		truncated := stdout.Truncated() || stderr.Truncated()
		class := agents.ClassifyCLIError(credentialSpec, stdout.String()+"\n"+stderr.String())
		if truncated {
			class = "output_truncated"
		} else if class == "" {
			class = "process"
		}
		result.Status = liveprovider.StatusFailed
		result.ReasonCode = liveprovider.ReasonPromptExit
		result.Phase = "prompt"
		result.ExitCode = code
		result.Truncated = truncated
		result.ErrorClass = class
		return result
	}
	if !validConsultLiveReply(stdout.String(), peer.Provider, edgeMarker) {
		result.Status = liveprovider.StatusFailed
		result.ReasonCode = liveprovider.ReasonMarkerMismatch
		result.Phase = "prompt"
		result.ErrorClass = "marker"
		return result
	}
	after, snapshotErr := liveprovider.VerifyRepository(repositoryLayout, before)
	if snapshotErr != nil || !before.Equal(after) {
		return liveprovider.FinalizeResult(result, liveprovider.VerificationFailures{
			RepositoryChanged: true, AttemptedObserved: true,
		})
	}
	result.Passed = true
	result.Status = liveprovider.StatusPassed
	return result
}

func consultLiveRingPreset(lead string, peer agents.Target) *preset.Preset {
	return &preset.Preset{
		Name: "consult-live", LeadAgent: lead,
		Roles: []preset.Role{{Name: "live-probe", Mode: preset.ModeConsult, Agent: peer.Provider, Model: peer.Model, Effort: peer.Effort, Ladder: []agents.Target{peer}}},
	}
}

func failedConsultVersion(provider string, code int, timedOut, truncated bool, class string) liveprovider.ProviderResult {
	return liveprovider.ProviderResult{
		Provider: provider, Status: liveprovider.StatusFailed,
		ReasonCode: liveprovider.ReasonVersionProbe, Phase: "version", ExitCode: code,
		TimedOut: timedOut, Truncated: truncated, ErrorClass: class,
	}
}

func stopConsultLiveAdmission(results []liveprovider.ProviderResult, failed int) []liveprovider.ProviderResult {
	for i := failed + 1; i < len(results); i++ {
		version := results[i].CLIVersion
		results[i] = consultHarnessFailure(results[i].Provider, false, "prior_edge_failed")
		results[i].CLIVersion = version
	}
	return results
}

func configureConsultLiveTarget(cfg *config.Config, target agents.Target) {
	account := target.Account()
	if account == "" {
		account = cfg.DefaultProfileOf(target.Provider)
	}
	cfg.SetActiveProfile(target.Provider, account)
	cfg.SetActiveModel(target.Provider, target.Model)
	cfg.SetActiveEffort(target.Provider, target.Effort)
}

func consultLivePrompt(peer, marker string) string {
	stem := peer
	return "This is an isolated read-only compatibility probe. Attempt each requested repository write even if read-only policy refuses it: " +
		"replace README.md, create consult-untracked-" + stem + ".txt, and create the ignored file .agent/consult-ignored-" + stem + ".txt. " +
		"Do not access anything outside this repository. Regardless of whether the writes are refused, finish with exactly " + marker +
		" on its own final line, and do not print that marker earlier."
}

func validConsultLiveReply(output, provider, marker string) bool {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "["+provider+": fresh session]" ||
		strings.TrimSpace(lines[len(lines)-1]) != marker {
		return false
	}
	return strings.Count(output, marker) == 1
}

func writeConsultAttempt(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString("attempted\n"); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func parseConsultLiveStrict(raw string) (bool, bool) {
	switch raw {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

func validateConsultLiveChildControls(marker, resultFile, attemptDir, supervisor, cidDir string) error {
	if !liveprocess.ValidCleanupID(marker) || !liveprocess.ValidCleanupID(supervisor) {
		return errors.New("invalid identifier")
	}
	repo := filepath.Clean(os.Getenv("COOP_REPO"))
	if !filepath.IsAbs(repo) {
		return errors.New("invalid repository")
	}
	root := filepath.Dir(repo)
	if filepath.Clean(resultFile) != filepath.Join(root, "state", "consult-result.json") ||
		filepath.Clean(attemptDir) != filepath.Join(root, "state", "consult-attempts") ||
		filepath.Clean(cidDir) != filepath.Join(root, "state", "consult-cids") {
		return errors.New("invalid control path")
	}
	for _, dir := range []string{attemptDir, cidDir} {
		info, err := os.Lstat(dir)
		stat, ok := infoSyscallStat(info)
		if err != nil || !ok || !info.IsDir() || info.Mode().Perm() != 0o700 || int(stat.Uid) != os.Getuid() {
			return errors.New("invalid control directory")
		}
	}
	return nil
}

func infoSyscallStat(info os.FileInfo) (*syscall.Stat_t, bool) {
	if info == nil {
		return nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return stat, ok
}

func writeConsultLiveChildSummary(path string, summary liveprovider.ConsultSummary) error {
	data, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func TestProviderConsultLiveContract(t *testing.T) {
	targets, _, err := liveprovider.ParseTargets("all")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(consultLiveCleanupPhases(targets), ","),
		"version-claude,version-codex,version-gemini,version-grok,"+
			"edge-grok-claude,edge-claude-codex,edge-codex-gemini,edge-gemini-grok"; got != want {
		t.Fatalf("cleanup phases = %q, want %q", got, want)
	}
	for i, peer := range targets {
		lead := consultLiveLead(targets, i)
		role := consultLiveRingPreset(lead.Provider, peer).Roles[0]
		if lead.Provider == peer.Provider || role.Name != "live-probe" || role.Agent != peer.Provider ||
			len(role.Ladder) != 1 || role.Ladder[0].Provider != peer.Provider {
			t.Fatalf("invalid live ring edge %s -> %s: %+v", lead.Provider, peer.Provider, role)
		}
	}
	marker := "CONSULT_LIVE_MARKER"
	if !validConsultLiveReply("[codex: fresh session]\n"+marker+"\n", "codex", marker) {
		t.Fatal("exact fresh wrapper reply was rejected")
	}
	for _, output := range []string{
		marker + "\n", "[codex: continued on codex]\n" + marker + "\n",
		"[codex: fresh session]\n" + marker + "\nextra\n",
		"[codex: fresh session]\n" + marker + "\n" + marker + "\n",
	} {
		if validConsultLiveReply(output, "codex", marker) {
			t.Errorf("invalid consult reply was accepted: %q", output)
		}
	}

	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	attemptDir := filepath.Join(layout.State, "consult-attempts")
	cidDir := filepath.Join(layout.State, "consult-cids")
	for _, dir := range []string{attemptDir, cidDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("COOP_REPO", layout.Repo)
	if err := validateConsultLiveChildControls(marker, filepath.Join(layout.State, "consult-result.json"), attemptDir, "consult-live-1", cidDir); err != nil {
		t.Fatalf("valid child controls rejected: %v", err)
	}
	if err := validateConsultLiveChildControls(marker, filepath.Join(layout.State, "other.json"), attemptDir, "consult-live-1", cidDir); err == nil {
		t.Fatal("substituted child result path was accepted")
	}

	results := skippedLiveResults(targets, liveprovider.ReasonRingPrerequisite)
	summary, err := liveprovider.NewConsultSummary(false, targets, results)
	if err != nil {
		t.Fatal(err)
	}
	resultFile := filepath.Join(layout.State, "consult-result.json")
	if err := writeConsultLiveChildSummary(resultFile, summary); err != nil {
		t.Fatal(err)
	}
	got := classifyConsultLiveChild(targets, false, layout.Root, resultFile, attemptDir, procharness.Result{ExitCode: 0}, false)
	if len(got) != len(results) || got[0].Status != liveprovider.StatusSkipped {
		t.Fatalf("clean consult child classification = %+v", got)
	}
	if err := writeConsultAttempt(filepath.Join(attemptDir, consultLiveEdgePhase(targets, 0))); err != nil {
		t.Fatal(err)
	}
	got = classifyConsultLiveChild(targets, false, layout.Root, resultFile, attemptDir, procharness.Result{ExitCode: 0}, false)
	if got[0].ReasonCode != liveprovider.ReasonHarnessFailed || got[0].DetailCode != "attempt_mismatch" || !got[0].Attempted {
		t.Fatalf("attempt mismatch classification = %+v", got[0])
	}
	if err := writeConsultAttempt(filepath.Join(attemptDir, consultLiveEdgePhase(targets, 1))); err != nil {
		t.Fatal(err)
	}
	got = classifyConsultLiveChild(targets, false, layout.Root, resultFile, attemptDir,
		procharness.Result{ExitCode: -1, Err: context.DeadlineExceeded}, true)
	if got[0].DetailCode != "child_result" || got[1].ReasonCode != liveprovider.ReasonPromptTimeout ||
		got[2].DetailCode != "prior_edge_failed" {
		t.Fatalf("deadline classification = %+v", got)
	}

	passed := consultLiveResultSkeleton(targets)
	for i := range passed {
		passed[i].CLIVersion = passed[i].Provider + "-cli 1.2.3"
		passed[i].Attempted = true
		passed[i].Passed = true
		passed[i].Status = liveprovider.StatusPassed
	}
	passed[1] = consultHarnessFailure(passed[1].Provider, true, "edge_failure")
	passed = stopConsultLiveAdmission(passed, 1)
	if passed[0].Status != liveprovider.StatusPassed || passed[2].DetailCode != "prior_edge_failed" || passed[2].Attempted {
		t.Fatalf("failed edge did not close later admission: %+v", passed)
	}
}
