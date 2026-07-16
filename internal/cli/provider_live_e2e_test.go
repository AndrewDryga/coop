//go:build providerlivee2e

// Opt-in upstream compatibility for the real provider CLIs. The parent copies only explicit
// credentials into a temporary vault, then invokes the helper test in a clean process so ambient
// provider or Coop settings cannot reach the box. Run with make provider-live-e2e[-all] or
// provider-loop-live-e2e[-all].
package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

const (
	liveWorkflowPrompt = "prompt"
	liveWorkflowLoop   = "loop"

	liveChildDeadline   = 6 * time.Minute
	livePromptDeadline  = 3 * time.Minute
	liveLoopDeadline    = 8 * time.Minute
	liveLoopChildWindow = 10 * time.Minute
	liveVersionDeadline = 45 * time.Second
	liveCleanupDeadline = 30 * time.Second
	liveOutputLimit     = 1 << 20
)

func TestProviderLiveCompatibility(t *testing.T) {
	testProviderLiveCompatibility(t, liveWorkflowPrompt)
}

func testProviderLiveCompatibility(t *testing.T, workflow string) {
	if os.Getenv("COOP_TEST_LIVE_CHILD") == "1" {
		t.Skip("parent orchestration is disabled in the clean child")
	}
	targets, strict, err := liveprovider.ParseTargets(os.Getenv("COOP_LIVE_TARGETS"))
	if err != nil {
		t.Fatal(err)
	}
	strict = strict || os.Getenv("COOP_LIVE_REQUIRE_ALL") == "1"
	if err := liveprovider.ValidateStrictTargets(strict, targets); err != nil {
		t.Fatal(err)
	}
	realConfig := config.Load()
	rt, err := runtime.Detect(realConfig.RuntimeName)
	if err != nil {
		emitLiveSummary(t, workflow, strict, targets, skippedLiveResults(targets, liveprovider.ReasonMissingRuntime))
		return
	}
	if err := rt.EnsureDaemon(); err != nil {
		emitLiveSummary(t, workflow, strict, targets, skippedLiveResults(targets, liveprovider.ReasonMissingRuntime))
		return
	}
	connectionEnv, err := liveprovider.CaptureRuntimeConnectionEnv(rt.Name)
	if err != nil {
		results := make([]liveprovider.ProviderResult, 0, len(targets))
		for _, target := range targets {
			results = append(results, liveprovider.ProviderResult{
				Provider: target.Provider, Status: liveprovider.StatusFailed,
				ReasonCode: liveprovider.ReasonHarnessFailed, Phase: "harness",
				ErrorClass: "harness", DetailCode: "runtime_connection",
			})
		}
		emitLiveSummary(t, workflow, strict, targets, results)
		return
	}
	runtimeSettings := liveprovider.RuntimeSettings{
		Name: rt.Name, Image: realConfig.ImageOverride, BaseImage: realConfig.BaseImage,
		HomeInBox: realConfig.HomeInBox, AgentPackages: realConfig.AgentPackages,
		ConnectionEnv: connectionEnv,
	}
	imageRoot := t.TempDir()
	image := box.ImageForRepo(imageRoot, realConfig.BaseImage, realConfig.ImageOverride)
	if !box.ImageExists(rt, image) {
		emitLiveSummary(t, workflow, strict, targets, skippedLiveResults(targets, liveprovider.ReasonMissingImage))
		return
	}

	results := make([]liveprovider.ProviderResult, 0, len(targets))
	for _, target := range targets {
		results = append(results, runProviderLiveCompatibility(t, realConfig, rt, runtimeSettings, target, workflow))
	}
	emitLiveSummary(t, workflow, strict, targets, results)
}

func skippedLiveResults(targets []agents.Target, reason string) []liveprovider.ProviderResult {
	results := make([]liveprovider.ProviderResult, 0, len(targets))
	for _, target := range targets {
		results = append(results, liveprovider.ProviderResult{
			Provider: target.Provider, Status: liveprovider.StatusSkipped, ReasonCode: reason,
		})
	}
	return results
}

func emitLiveSummary(t *testing.T, workflow string, strict bool, targets []agents.Target, results []liveprovider.ProviderResult) {
	t.Helper()
	summary, err := liveprovider.NewSummary(strict, targets, results)
	if err != nil {
		t.Fatal(err)
	}
	var line string
	if workflow == liveWorkflowLoop {
		line, err = summary.LoopLine()
	} else {
		line, err = summary.Line()
	}
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, line)
	if !summary.Success() {
		t.Fail()
	}
}

func runProviderLiveCompatibility(
	t *testing.T,
	realConfig *config.Config,
	rt runtime.Runtime,
	runtimeSettings liveprovider.RuntimeSettings,
	target agents.Target,
	workflow string,
) liveprovider.ProviderResult {
	t.Helper()
	fail := func(attempted bool, reason string, detail ...string) liveprovider.ProviderResult {
		result := liveprovider.ProviderResult{
			Provider: target.Provider, Attempted: attempted,
			Status: liveprovider.StatusFailed, ReasonCode: reason,
			Phase: "harness", ErrorClass: "harness",
		}
		if len(detail) > 0 {
			result.DetailCode = detail[0]
		}
		return result
	}
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		return fail(false, liveprovider.ReasonHarnessFailed, "layout_setup")
	}
	if err := liveprovider.InitRepository(layout); err != nil {
		return fail(false, liveprovider.ReasonHarnessFailed, "repository_setup")
	}
	marker, err := liveIdentifier("COOP_LIVE_MARKER_")
	if err != nil {
		return fail(false, liveprovider.ReasonHarnessFailed, "identifier_generation")
	}
	if workflow == liveWorkflowLoop {
		if err := prepareProviderLoopLiveRepository(layout, target, marker); err != nil {
			return fail(false, liveprovider.ReasonHarnessFailed, "repository_setup")
		}
	}
	before, err := liveprovider.SnapshotRepository(layout)
	if err != nil {
		return fail(false, liveprovider.ReasonHarnessFailed, "repository_snapshot")
	}
	var loopBefore providerLoopLiveBaseline
	if workflow == liveWorkflowLoop {
		loopBefore, err = snapshotProviderLoopLiveBaseline(layout, before)
		if err != nil {
			return fail(false, liveprovider.ReasonHarnessFailed, "repository_snapshot")
		}
	}
	selection, err := liveprovider.SelectionForTarget(realConfig, target)
	if err != nil {
		return fail(false, liveprovider.ReasonHarnessFailed, "target_selection")
	}
	// NewLayout secures this path for us; Prepare publishes the complete vault by atomic rename.
	if err := os.Remove(layout.Config); err != nil {
		return fail(false, liveprovider.ReasonHarnessFailed, "config_reset")
	}
	prepared, err := liveprovider.Prepare(realConfig.ConfigDir, layout.Config, []liveprovider.Selection{selection})
	if err != nil {
		return fail(false, liveprovider.ReasonUnsafeCredential, liveprovider.CredentialDetailCode(err))
	}
	defer func() { _ = prepared.Revoke() }()
	preflightReason := prepared.PreflightReason(
		target.Provider, selection.Account, time.Now().Add(30*time.Minute),
	)
	if preflightReason == liveprovider.ReasonUnsafeCredential {
		return fail(false, liveprovider.ReasonUnsafeCredential, "credential_portability")
	}
	if err := prepared.VerifySources(); err != nil {
		return liveprovider.FinalizeResult(
			liveprovider.ProviderResult{Provider: target.Provider},
			liveprovider.VerificationFailures{SourceChanged: true},
		)
	}

	supervisor, err := liveIdentifier("provider-live-")
	if err != nil {
		return fail(false, liveprovider.ReasonHarnessFailed, "identifier_generation")
	}
	resultFile := filepath.Join(layout.State, "provider-result.json")
	attemptFile := filepath.Join(layout.State, "provider-attempted")
	cidDir := filepath.Join(layout.State, "container-ids")
	if err := os.MkdirAll(cidDir, 0o700); err != nil {
		return fail(false, liveprovider.ReasonHarnessFailed, "control_directory")
	}
	control, _, err := liveprovider.NewProcessControl(layout, false)
	if err != nil {
		return fail(false, liveprovider.ReasonHarnessFailed, "process_control")
	}
	defer control.Close()
	revokePath, err := prepared.RevocationPath()
	if err != nil {
		return fail(false, liveprovider.ReasonHarnessFailed, "credential_revocation")
	}
	env, err := liveprovider.ChildEnvironment(layout, liveprovider.ChildSpec{
		Path: os.Getenv("PATH"), Target: target.String(), Workflow: workflow, Marker: marker,
		ResultFile: resultFile, AttemptFile: attemptFile, Supervisor: supervisor,
		PreflightReason: preflightReason, CIDDir: cidDir,
		ControlFD: 3, RevokePath: revokePath, Runtime: runtimeSettings,
	})
	if err != nil {
		return fail(false, liveprovider.ReasonHarnessFailed, "child_environment")
	}
	childDeadline := liveChildDeadline
	if workflow == liveWorkflowLoop {
		childDeadline = liveLoopChildWindow
	}
	ctx, cancel := context.WithTimeout(context.Background(), childDeadline)
	processResult := procharness.Run(ctx, procharness.Command{
		Path: os.Args[0], Args: []string{"-test.run=^TestProviderLiveChild$", "-test.v=false"},
		Dir: layout.Repo, Env: env, MaxOutput: liveOutputLimit, KillGrace: 3 * time.Second,
		ExtraFiles: []*os.File{control}, BeforeCancel: prepared.Revoke,
	})
	timedOut := liveprovider.ProcessDeadlineExceeded(processResult)
	cancel()
	attempted := liveprovider.ControlFilePresent(layout.Root, attemptFile)
	result := liveprovider.ClassifyChildProcess(target.Provider, layout.Root, resultFile, liveprovider.ChildProcessObservation{
		Result: processResult, DeadlineExceeded: timedOut, Attempted: attempted,
	})
	revokeErr := prepared.Revoke()
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), liveCleanupDeadline)
	cleanupErr := liveprovider.CleanupSupervisor(cleanupCtx, liveprovider.SupervisorCleanupSpec{
		Root: layout.Root, CIDDir: cidDir, Supervisor: supervisor, LabelKey: box.LabelSupervisor,
		OperationTimeout: 2 * time.Second, QuietPeriod: time.Second, PollInterval: 100 * time.Millisecond,
	}, liveprovider.SupervisorCleanupOps{
		RemoveContainer: rt.RemoveContainerContext,
		RemoveByLabel:   rt.RemoveByLabel,
	})
	cancelCleanup()
	repositoryOK := false
	if workflow == liveWorkflowLoop {
		repositoryOK = verifyProviderLoopLiveRepository(layout, loopBefore, target, marker) == nil
	} else {
		after, snapshotErr := liveprovider.VerifyRepository(layout, before)
		repositoryOK = snapshotErr == nil && before.Equal(after)
	}
	sourceErr := prepared.VerifySources()
	return liveprovider.FinalizeResult(result, liveprovider.VerificationFailures{
		CleanupFailed: cleanupErr != nil || revokeErr != nil, SourceChanged: sourceErr != nil,
		RepositoryChanged: !repositoryOK, AttemptedObserved: attempted,
	})
}

func liveIdentifier(prefix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random[:]), nil
}

func TestProviderLiveChild(t *testing.T) {
	if os.Getenv("COOP_TEST_LIVE_CHILD") != "1" {
		t.Skip("live child helper runs only under TestProviderLiveCompatibility")
	}
	target, err := agents.ParseTarget(os.Getenv("COOP_TEST_LIVE_TARGET"))
	if err != nil || len(target.Accounts) > 1 {
		t.Fatal("invalid live child target")
	}
	marker := os.Getenv("COOP_TEST_LIVE_MARKER")
	resultFile := os.Getenv("COOP_TEST_LIVE_RESULT")
	attemptFile := os.Getenv("COOP_TEST_LIVE_ATTEMPT")
	supervisor := os.Getenv("COOP_TEST_LIVE_SUPERVISOR")
	preflightReason := os.Getenv("COOP_TEST_LIVE_PREFLIGHT")
	cidDir := os.Getenv("COOP_TEST_LIVE_CID_DIR")
	workflow := os.Getenv("COOP_TEST_LIVE_WORKFLOW")
	if marker == "" || resultFile == "" || attemptFile == "" || supervisor == "" || cidDir == "" || (workflow != liveWorkflowPrompt && workflow != liveWorkflowLoop) {
		t.Fatal("incomplete live child control contract")
	}
	result := executeProviderLiveChild(target, workflow, marker, attemptFile, supervisor, preflightReason, cidDir)
	if err := writeLiveChildResult(resultFile, result); err != nil {
		t.Fatal("write live child result")
	}
}

func executeProviderLiveChild(target agents.Target, workflow, marker, attemptFile, supervisor, preflightReason, cidDir string) liveprovider.ProviderResult {
	result := liveprovider.ProviderResult{Provider: target.Provider}
	fail := func(attempted bool, reason, phase string, code int, timedOut, truncated bool, class string) liveprovider.ProviderResult {
		result.Attempted, result.Passed = attempted, false
		result.Status, result.ReasonCode = liveprovider.StatusFailed, reason
		result.Phase, result.ExitCode = phase, code
		result.TimedOut, result.Truncated, result.ErrorClass = timedOut, truncated, class
		return result
	}
	skip := func(reason string) liveprovider.ProviderResult {
		result.Status, result.ReasonCode = liveprovider.StatusSkipped, reason
		return result
	}
	cfg := config.Load()
	account := target.Account()
	if account == "" {
		account = cfg.DefaultProfileOf(target.Provider)
	}
	cfg.SetActiveProfile(target.Provider, account)
	cfg.SetActiveModel(target.Provider, target.Model)
	cfg.SetActiveEffort(target.Provider, target.Effort)
	ag, ok := agents.Get(target.Provider)
	if !ok {
		return fail(false, liveprovider.ReasonHarnessFailed, "harness", 0, false, false, "harness")
	}
	live := ag.LiveCredentials()
	if len(live.Artifacts) == 0 || live.Portability == nil || len(live.AuthSignals) == 0 {
		return fail(false, liveprovider.ReasonUnsafeCredential, "harness", 0, false, false, "harness")
	}
	rt, err := runtime.Detect(cfg.RuntimeName)
	if err != nil {
		return skip(liveprovider.ReasonMissingRuntime)
	}
	image := box.ImageForRepo(cfg.RepoOverride, cfg.BaseImage, cfg.ImageOverride)

	versionOut := liveprovider.NewBoundedBuffer(64 << 10)
	versionErr := liveprovider.NewBoundedBuffer(64 << 10)
	versionCtx, cancelVersion := context.WithTimeout(context.Background(), liveVersionDeadline)
	interactive := ag.Interactive(cfg)
	if len(interactive) == 0 {
		cancelVersion()
		return fail(false, liveprovider.ReasonVersionProbe, "version", 0, false, false, "harness")
	}
	code, runErr := box.Run(cfg, rt, box.RunSpec{
		Image: image, Repo: cfg.RepoOverride, Cmd: []string{interactive[0], "--version"},
		Agent: target.Provider, Batch: true, RepoReadOnly: true, Quiet: true,
		SupervisorID: supervisor, Stdout: versionOut, Stderr: versionErr, Ctx: versionCtx,
		ExtraArgs: liveCIDArgs(rt, cidDir, "version"),
	})
	versionTimedOut := errors.Is(runErr, context.DeadlineExceeded)
	cancelVersion()
	if code == 127 {
		return skip(liveprovider.ReasonMissingCLI)
	}
	if runErr != nil || code != 0 || versionTimedOut || versionOut.Truncated() || versionErr.Truncated() {
		truncated := versionOut.Truncated() || versionErr.Truncated()
		class := agents.ClassifyCLIError(live, versionOut.String()+"\n"+versionErr.String())
		if versionTimedOut {
			class = "timeout"
		} else if truncated {
			class = "output_truncated"
		}
		return fail(false, liveprovider.ReasonVersionProbe, "version", code, versionTimedOut, truncated, class)
	}
	result.CLIVersion = liveprovider.CLIVersion(target.Provider, versionOut.String(), versionErr.String())
	if result.CLIVersion == "" {
		return fail(false, liveprovider.ReasonVersionProbe, "version", code, false, false, "empty_output")
	}
	if preflightReason != "" {
		return skip(preflightReason)
	}

	attempt, err := os.OpenFile(attemptFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail(false, liveprovider.ReasonHarnessFailed, "harness", 0, false, false, "harness")
	}
	if _, err := attempt.WriteString("attempted\n"); err != nil {
		attempt.Close()
		return fail(false, liveprovider.ReasonHarnessFailed, "harness", 0, false, false, "harness")
	}
	if err := attempt.Close(); err != nil {
		return fail(false, liveprovider.ReasonHarnessFailed, "harness", 0, false, false, "harness")
	}
	result.Attempted = true
	stdout := liveprovider.NewBoundedBuffer(liveOutputLimit)
	stderr := liveprovider.NewBoundedBuffer(liveOutputLimit)
	promptDeadline := livePromptDeadline
	if workflow == liveWorkflowLoop {
		promptDeadline = liveLoopDeadline
	}
	promptCtx, cancelPrompt := context.WithTimeout(context.Background(), promptDeadline)
	prompt := "Respond with exactly " + marker + " and no other text."
	repoReadOnly := true
	if workflow == liveWorkflowLoop {
		prompt = providerLoopLivePrompt(cfg.RepoOverride, target.Provider)
		repoReadOnly = false
	}
	code, runErr = box.Run(cfg, rt, box.RunSpec{
		Image: image, Repo: cfg.RepoOverride,
		Cmd:   ag.Headless(cfg, prompt),
		Agent: target.Provider, Batch: true, RepoReadOnly: repoReadOnly, Quiet: true,
		Homes: true, Network: false, Cache: false, SupervisorID: supervisor,
		Stdout: stdout, Stderr: stderr, Ctx: promptCtx,
		ExtraArgs: liveCIDArgs(rt, cidDir, "prompt"),
	})
	promptTimedOut := errors.Is(runErr, context.DeadlineExceeded)
	cancelPrompt()
	if promptTimedOut {
		return fail(true, liveprovider.ReasonPromptTimeout, "prompt", code, true, false, "timeout")
	}
	if runErr != nil || code != 0 || stdout.Truncated() || stderr.Truncated() {
		truncated := stdout.Truncated() || stderr.Truncated()
		class := agents.ClassifyCLIError(live, stdout.String()+"\n"+stderr.String())
		if truncated {
			class = "output_truncated"
		}
		return fail(true, liveprovider.ReasonPromptExit, "prompt", code, false, truncated, class)
	}
	if workflow == liveWorkflowPrompt && strings.TrimSpace(stdout.String()) != marker {
		return fail(true, liveprovider.ReasonMarkerMismatch, "prompt", code, false, false, "marker")
	}
	result.Passed, result.Status, result.ReasonCode = true, liveprovider.StatusPassed, ""
	return result
}

func liveCIDArgs(rt runtime.Runtime, cidDir, phase string) []string {
	if !rt.SupportsCIDFile() {
		return nil
	}
	return []string{"--cidfile", filepath.Join(cidDir, phase+".cid")}
}

func writeLiveChildResult(path string, result liveprovider.ProviderResult) error {
	data, err := json.Marshal(result)
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
