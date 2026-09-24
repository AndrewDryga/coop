//go:build providerlivee2e

package loop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

const (
	accountLiveTaskID = "2026-01-01-live-account-recovery"
	accountLiveFile   = "account-recovery.txt"
	accountLiveWindow = 12 * time.Minute
)

func accountLiveFailure(provider, detail string) liveprovider.ProviderResult {
	return liveprovider.ProviderResult{Provider: provider, Status: liveprovider.StatusFailed,
		ReasonCode: liveprovider.ReasonHarnessFailed, Phase: "harness", ErrorClass: "harness", DetailCode: detail}
}

func TestProviderAccountsLiveCompatibility(t *testing.T) {
	if os.Getenv("COOP_LIVE_TARGETS") != "all" || os.Getenv("COOP_TEST_LIVE_CHILD") == "1" {
		t.Skip("operator-only account qualification requires COOP_LIVE_TARGETS=all")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal("load live configuration")
	}
	box.ResolveBaseImage(cfg)
	results := make([]liveprovider.ProviderResult, 0, len(agents.Names()))
	for _, provider := range agents.Names() {
		pair, brokered, err := accountLivePair(cfg, provider)
		switch {
		case err != nil:
			results = append(results, accountLiveFailure(provider, "account_selection"))
		case len(pair) == 0:
			results = append(results, liveprovider.ProviderResult{Provider: provider, Status: liveprovider.StatusNotConfigured})
		default:
			results = append(results, runAccountLivePair(t, cfg, pair, brokered))
		}
	}
	summary, err := liveprovider.NewAccountsSummary(results)
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

func runAccountLivePair(t *testing.T, source *config.Config, pair []liveprovider.Selection, brokered bool) liveprovider.ProviderResult {
	t.Helper()
	provider := pair[0].Provider
	fail := func(detail string) liveprovider.ProviderResult { return accountLiveFailure(provider, detail) }
	rt, err := runtime.Detect(source.RuntimeName)
	if err != nil || rt.EnsureDaemon() != nil {
		return fail("missing_runtime")
	}
	connection, err := liveprovider.CaptureRuntimeConnectionEnv(rt.Name)
	if err != nil {
		return fail("runtime_connection")
	}
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		return fail("layout_setup")
	}
	image := box.ImageForRepo(layout.Repo, source.BaseImage, source.ImageOverride)
	if !box.ImageExists(rt, image) {
		return fail("missing_image")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fail("identifier_generation")
	}
	marker := "COOP_ACCOUNT_" + hex.EncodeToString(nonce[:])
	baseline, err := prepareAccountLiveRepository(layout, marker)
	if err != nil {
		return fail("repository_setup")
	}
	if err := os.Remove(layout.Config); err != nil { // empty, exact NewLayout directory
		return fail("config_setup")
	}
	prepared, err := liveprovider.Prepare(source.ConfigDir, layout.Config, pair)
	if err != nil {
		return fail("credential_copy")
	}
	defer func() { _ = prepared.Revoke() }()
	for _, selected := range pair {
		if reason := prepared.PreflightReason(provider, selected.Account, time.Now().Add(accountLiveWindow+time.Minute)); reason != "" {
			return fail(reason)
		}
	}
	isolated := &config.Config{ConfigDir: layout.Config}
	// A second Prepare supplies the same opaque source-integrity check for the good copy. It is
	// revoked before starting any client; neither its path nor account names enter retained logs.
	witness, err := liveprovider.Prepare(layout.Config, filepath.Join(layout.State, "second-account-witness"), pair[1:])
	if err != nil {
		return fail("second_account_witness")
	}
	faultErr := faultAccountLiveCopy(isolated, pair[0], brokered)
	witnessErr := witness.VerifySources()
	revokeWitnessErr := witness.Revoke()
	if faultErr != nil || witnessErr != nil || revokeWitnessErr != nil || prepared.VerifySources() != nil {
		return fail("credential_fault")
	}
	control, _, err := liveprovider.NewProcessControl(layout, false)
	if err != nil {
		return fail("process_control")
	}
	defer control.Close()
	revokePath, err := prepared.RevocationPath()
	if err != nil {
		return fail("credential_revocation")
	}
	resultFile, attemptFile := filepath.Join(layout.State, "account-result.json"), filepath.Join(layout.State, "account-attempted")
	cidDir := filepath.Join(layout.State, "container-ids")
	if err := os.Mkdir(cidDir, 0o700); err != nil {
		return fail("control_directory")
	}
	supervisor := "account-live-" + hex.EncodeToString(nonce[:])
	target := agents.Target{Provider: provider, Accounts: []string{pair[0].Account, pair[1].Account}}
	spec := liveprovider.ChildSpec{
		Path: os.Getenv("PATH"), Target: target.String(), Workflow: "loop", Marker: marker,
		ResultFile: resultFile, AttemptFile: attemptFile, Supervisor: supervisor, CIDDir: cidDir,
		ControlFD: 3, RevokePath: revokePath,
		Runtime: liveprovider.RuntimeSettings{Name: rt.Name, Image: source.ImageOverride, BaseImage: source.BaseImage,
			HomeInBox: source.HomeInBox, ConnectionEnv: connection},
	}
	if brokered {
		spec.NetworkStateHome = os.Getenv("XDG_STATE_HOME")
		if spec.NetworkStateHome == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return fail("network_state")
			}
			spec.NetworkStateHome = filepath.Join(home, ".local", "state")
		}
	}
	env, err := liveprovider.ChildEnvironment(layout, spec)
	if err != nil {
		return fail("child_environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountLiveWindow)
	process := procharness.Run(ctx, procharness.Command{
		Path: os.Args[0], Args: []string{"-test.run=^TestProviderAccountsLiveChild$", "-test.v=false"},
		Dir: layout.Repo, Env: env, MaxOutput: 1 << 20, KillGrace: 3 * time.Second,
		ExtraFiles: []*os.File{control}, BeforeCancel: prepared.Revoke,
	})
	cancel()
	attempted := liveprovider.ControlFilePresent(layout.Root, attemptFile)
	result := liveprovider.ClassifyChildProcess(provider, layout.Root, resultFile, liveprovider.ChildProcessObservation{
		Result: process, DeadlineExceeded: liveprovider.ProcessDeadlineExceeded(process), Attempted: attempted,
	})
	revokeErr := prepared.Revoke()
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
	cleanupErr := liveprovider.CleanupSupervisor(cleanupCtx, liveprovider.SupervisorCleanupSpec{
		Root: layout.Root, CIDDir: cidDir, Supervisor: supervisor, LabelKey: box.LabelSupervisor,
		OperationTimeout: 2 * time.Second, QuietPeriod: time.Second, PollInterval: 100 * time.Millisecond,
	}, liveprovider.SupervisorCleanupOps{RemoveContainer: rt.RemoveContainerContext, RemoveByLabel: rt.RemoveByLabel})
	cancelCleanup()
	var repositoryErr error
	if result.Passed {
		repositoryErr = verifyAccountLiveRepository(layout, baseline, marker)
	}
	return liveprovider.FinalizeResult(result, liveprovider.VerificationFailures{
		CleanupFailed: revokeErr != nil || cleanupErr != nil, SourceChanged: prepared.VerifySources() != nil,
		RepositoryChanged: repositoryErr != nil, AttemptedObserved: attempted,
	})
}

// The native controller owns admission, decoding, classification, rotation, task MCP and cleanup.
// Its existing private launch seam adds only test ownership, deadlines and a two-work-call cap.
func TestProviderAccountsLiveChild(t *testing.T) {
	if os.Getenv("COOP_TEST_LIVE_CHILD") != "1" {
		t.Skip("clean-process helper only")
	}
	target, err := agents.ParseTarget(os.Getenv("COOP_TEST_LIVE_TARGET"))
	if err != nil || len(target.Accounts) != 2 || target.Accounts[0] == target.Accounts[1] || os.Getenv("COOP_TEST_LIVE_WORKFLOW") != "loop" {
		t.Fatal("invalid account child contract")
	}
	result := accountLiveFailure(target.Provider, "controller")
	defer func() {
		data, err := json.Marshal(result)
		if err != nil || config.WriteFileAtomic(os.Getenv("COOP_TEST_LIVE_RESULT"), append(data, '\n')) != nil {
			t.Error("write account child result")
		}
	}()
	cfg, err := config.Load()
	if err != nil {
		return
	}
	rt, err := runtime.Detect(cfg.RuntimeName)
	if err != nil {
		return
	}
	c := New(cfg, rt, "live-qualification", Host{})
	guard := accountLiveLaunchGuard{provider: target.Provider, accounts: target.Accounts}
	c.boxRun = func(spec box.RunSpec) (int, error) {
		if err := guard.admit(spec, cfg.ActiveProfile(target.Provider)); err != nil {
			// An ordinary launch error asks the controller to retry. Fatal ends this clean helper
			// before a third paid launch; the parent still revokes credentials and reaps ownership.
			t.Fatal("account qualification launch bound or scope violated")
		}
		cid := func(phase string) []string {
			if cfg.Egress == "filtered" || !rt.SupportsCIDFile() {
				return nil
			}
			return []string{"--cidfile", filepath.Join(os.Getenv("COOP_TEST_LIVE_CID_DIR"), phase+".cid")}
		}
		if guard.calls == 1 {
			ag, _ := agents.Get(target.Provider)
			command := ag.Interactive(cfg)
			if len(command) == 0 {
				t.Fatal("provider version command unavailable")
			}
			stdout, stderr := liveprovider.NewBoundedBuffer(64<<10), liveprovider.NewBoundedBuffer(64<<10)
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			code, err := box.Run(cfg, rt, box.RunSpec{
				Image: spec.Image, Repo: spec.Repo, Agent: target.Provider, Cmd: []string{command[0], "--version"},
				Batch: true, Quiet: true, RepoReadOnly: true, SupervisorID: os.Getenv("COOP_TEST_LIVE_SUPERVISOR"),
				CapturedEgress: spec.CapturedEgress, ExtraArgs: cid("version"), Stdout: stdout, Stderr: stderr, Ctx: ctx,
			})
			cancel()
			result.CLIVersion = liveprovider.CLIVersion(target.Provider, stdout.String(), stderr.String())
			if err != nil || code != 0 || result.CLIVersion == "" || stdout.Truncated() || stderr.Truncated() {
				t.Fatal("account qualification version probe failed")
			}
			if err := os.WriteFile(os.Getenv("COOP_TEST_LIVE_ATTEMPT"), []byte("attempted\n"), 0o600); err != nil {
				t.Fatal("write account attempt witness")
			}
			result.Attempted = true
		}
		spec.SupervisorID = os.Getenv("COOP_TEST_LIVE_SUPERVISOR")
		spec.ExtraArgs = append(spec.ExtraArgs, cid(fmt.Sprintf("work-%d", guard.calls))...)
		ctx, cancel := context.WithTimeout(spec.Ctx, 5*time.Minute)
		defer cancel()
		spec.Ctx = ctx
		return box.Run(cfg, rt, spec)
	}
	rungs := []agents.Target{{Provider: target.Provider, Accounts: target.Accounts[:1]}, {Provider: target.Provider, Accounts: target.Accounts[1:]}}
	code, runErr := c.Run(RunSpec{
		Repo: cfg.RepoOverride, Image: box.ImageForRepo(cfg.RepoOverride, cfg.BaseImage, cfg.ImageOverride),
		Agent: target.Provider, Rotation: ladder.NewRotation(rungs), Queues: []string{tasksRoot}, MaxTasks: 1, Sink: io.Discard,
	})
	if runErr != nil || code != 0 || guard.calls != 2 {
		return
	}
	data, err := readRunFile(cfg.RepoOverride, c.runID+".jsonl")
	if err != nil || verifyAccountLiveTelemetry(data, c.runID, target) != nil {
		result.DetailCode = "rotation_evidence"
		return
	}
	result = liveprovider.ProviderResult{Provider: target.Provider, CLIVersion: result.CLIVersion,
		Attempted: true, Passed: true, Status: liveprovider.StatusPassed}
}

type accountLiveLaunchGuard struct {
	provider string
	accounts []string
	calls    int
}

func (g *accountLiveLaunchGuard) admit(spec box.RunSpec, account string) error {
	if g.calls >= 2 || len(g.accounts) != 2 || spec.Agent != g.provider || !spec.AgentCommand ||
		spec.AssignedTask != accountLiveTaskID || spec.TaskTools == nil || spec.Ctx == nil ||
		account != g.accounts[g.calls] {
		return errors.New("unexpected account qualification launch")
	}
	g.calls++
	return nil
}
