//go:build providerlivee2e

// The credentialed providers, through the RESTRICTED gateway. `coop net setup` proves a curl can
// reach an allowed name; nothing proved that a real provider CLI — its auth refresh, its telemetry,
// its streaming and its session resume — works when every packet crosses the boundary. This is that
// proof, and it is the one that would have caught a missing bundle host before a user did.
//
// It runs the same disposable-vault harness as the other live probes (see provider_live_e2e_test.go)
// with workflow=network: the parent copies only the selected credential into a temporary config, the
// child admits ONE filtered policy on the host and launches every box behind it.
package cli

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
)

func TestProviderNetworkLiveCompatibility(t *testing.T) {
	testProviderLiveCompatibility(t, liveWorkflowNetwork)
}

// executeProviderNetworkLiveChild runs one provider twice behind one frozen policy — a fresh
// prompt and a native resume of that same conversation — then reads the sealed receipts back and
// requires every destination the boxes actually reached to be one this policy granted.
//
// A host that cannot be set up for filtered runs, has no runtime or no credential SKIPS: an
// unqualified host cannot fail a compatibility claim it never made.
func executeProviderNetworkLiveChild(target agents.Target, marker, attemptFile, preflightReason string) liveprovider.ProviderResult {
	result := liveprovider.ProviderResult{Provider: target.Provider}
	fail := func(reason, phase, class string, code int) liveprovider.ProviderResult {
		result.Attempted, result.Passed = true, false
		result.Status, result.ReasonCode = liveprovider.StatusFailed, reason
		result.Phase, result.ExitCode, result.ErrorClass = phase, code, class
		return result
	}
	harnessFail := func(detail string) liveprovider.ProviderResult {
		failed := fail(liveprovider.ReasonHarnessFailed, "harness", "harness", 0)
		failed.Attempted, failed.DetailCode = false, detail
		return failed
	}
	skip := func(reason, detail string) liveprovider.ProviderResult {
		result.Status, result.ReasonCode, result.DetailCode = liveprovider.StatusSkipped, reason, detail
		return result
	}
	cfg, err := config.Load()
	if err != nil {
		return harnessFail("config")
	}
	account := target.Account()
	if account == "" {
		account = cfg.DefaultProfileOf(target.Provider)
	}
	cfg.SetActiveProfile(target.Provider, account)
	cfg.SetActiveModel(target.Provider, target.Model)
	cfg.SetActiveEffort(target.Provider, target.Effort)
	ag, ok := agents.Get(target.Provider)
	if !ok {
		return harnessFail("provider_lookup")
	}
	rt, err := runtime.Detect(cfg.RuntimeName)
	if err != nil {
		return skip(liveprovider.ReasonMissingRuntime, "")
	}
	if preflightReason != "" {
		return skip(preflightReason, "")
	}
	// One admission for both launches, exactly as a loop or an ACP session does: a policy that
	// changed between them would be a different experiment.
	filtered := egress.Filtered
	spec := box.RunSpec{
		Repo: cfg.RepoOverride, Agent: target.Provider, Homes: true,
	}
	capture, err := box.AdmitNetwork(cfg, rt, spec, box.NetworkAdmission{InvocationMode: &filtered})
	if err != nil {
		// A host that cannot be set up for filtered runs — admission tries that
		// itself now — is a skip, not a compatibility failure.
		if errors.Is(err, box.ErrNetworkSetupFailed) {
			return skip(liveprovider.ReasonMissingImage, "network_setup")
		}
		return harnessFail("network_admission")
	}
	defer capture.Close()
	if capture == nil {
		return harnessFail("network_admission")
	}
	// The version probe runs through the gateway too: it is the cheapest proof
	// that the locked client image's CLI starts at all behind the boundary, and
	// the summary contract requires the version of what was exercised.
	interactive := ag.Interactive(cfg)
	if len(interactive) == 0 {
		return harnessFail("version_probe")
	}
	version, code, err := runProviderNetworkLiveBox(cfg, rt, capture, target.Provider, []string{interactive[0], "--version"})
	if err != nil || code != 0 {
		if code == 127 {
			return skip(liveprovider.ReasonMissingCLI, "")
		}
		return fail(liveprovider.ReasonVersionProbe, "version", "harness", code)
	}
	if result.CLIVersion = liveprovider.CLIVersion(target.Provider, version, ""); result.CLIVersion == "" {
		return fail(liveprovider.ReasonVersionProbe, "version", "empty_output", code)
	}
	attempt, err := os.OpenFile(attemptFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return harnessFail("harness")
	}
	if _, err := attempt.WriteString("attempted\n"); err != nil || attempt.Close() != nil {
		return harnessFail("harness")
	}
	result.Attempted = true

	sessionID := ""
	if ag.PresetSessionID() {
		if sessionID, err = newSessionID(); err != nil {
			return harnessFail("identifier_generation")
		}
	}
	// Stage one: an ordinary headless prompt. Its answer proves the provider's whole stack —
	// auth refresh, model call, streaming — worked with no proxy variables and no open egress.
	prompt := "Respond with exactly " + marker + " and no other text."
	command, ok := providerResumeLiveCommand(ag, cfg, liveResumeFresh, sessionID, prompt)
	if !ok {
		return harnessFail("session_lookup")
	}
	stdout, code, runErr := runProviderNetworkLiveBox(cfg, rt, capture, target.Provider, command)
	if runErr != nil || code != 0 {
		if errors.Is(runErr, context.DeadlineExceeded) {
			return fail(liveprovider.ReasonPromptTimeout, "prompt", "timeout", code)
		}
		return fail(liveprovider.ReasonPromptExit, "prompt", "provider", code)
	}
	resolvedID, reply, err := providerResumeLiveOutput(ag, sessionID, stdout)
	if err != nil {
		failed := fail(liveprovider.ReasonPromptExit, "prompt", "provider_stream", code)
		failed.DetailCode = "session_stream"
		return failed
	}
	if reply != marker {
		return fail(liveprovider.ReasonMarkerMismatch, "prompt", "marker", code)
	}
	// Stage two: continue THAT conversation. A resume is where a provider reaches for a second
	// host — a session store, a different API edge — so it is the case a bundle most often misses.
	command, ok = providerResumeLiveCommand(ag, cfg, liveResumeContinue, resolvedID,
		"Respond with exactly the text of your immediately preceding assistant response and no other text.")
	if !ok {
		return harnessFail("session_lookup")
	}
	stdout, code, runErr = runProviderNetworkLiveBox(cfg, rt, capture, target.Provider, command)
	if runErr != nil || code != 0 {
		if errors.Is(runErr, context.DeadlineExceeded) {
			return fail(liveprovider.ReasonPromptTimeout, "resume", "timeout", code)
		}
		return fail(liveprovider.ReasonPromptExit, "resume", "provider", code)
	}
	if _, reply, err = providerResumeLiveOutput(ag, resolvedID, stdout); err != nil || reply != marker {
		return fail(liveprovider.ReasonMarkerMismatch, "resume", "marker", code)
	}
	// Stage three, only when this host actually configures a shared MCP server over HTTP: those
	// hosts are granted automatically by admission, so a tool call is the one way to prove the
	// grant is reachable and not just present. A model that ignores an explicit instruction to
	// call a named tool fails here, which is a finding worth a red run.
	if hosts := providerNetworkLiveMCPHosts(cfg, spec); len(hosts) != 0 {
		command = ag.Headless(cfg, "Call exactly one read-only tool from the MCP server you have, then respond with exactly "+
			marker+" and no other text. Do not answer without calling the tool.")
		if _, code, runErr = runProviderNetworkLiveBox(cfg, rt, capture, target.Provider, command); runErr != nil || code != 0 {
			return fail(liveprovider.ReasonPromptExit, "mcp", "provider", code)
		}
		if detail, err := verifyProviderNetworkLiveReached(capture, hosts); err != nil {
			failed := fail(liveprovider.ReasonPromptExit, "mcp", "network", 0)
			failed.DetailCode = detail
			return failed
		}
	}
	// The receipts are the evidence: what the boxes reached has to be what the policy granted.
	if detail, err := verifyProviderNetworkLiveReceipts(capture); err != nil {
		failed := fail(liveprovider.ReasonPromptExit, "receipt", "network", 0)
		failed.DetailCode = detail
		return failed
	}
	// And a session that only answered a prompt must not have reached for its client's own
	// release feed, package registry or telemetry intake: the box switches that chatter off
	// (BoxEnv, the generated overlays) instead of granting or hiding it, so a refusal here is
	// the managed-client control failing on this exact locked version.
	if detail, err := verifyProviderNetworkLiveSilence(capture); err != nil {
		failed := fail(liveprovider.ReasonPromptExit, "chatter", "network", 0)
		failed.DetailCode = detail
		return failed
	}
	result.Passed, result.Status, result.ReasonCode = true, liveprovider.StatusPassed, ""
	return result
}

// providerChatterHosts are the hosts a managed client reaches on its own — never for the
// operator's work — and that no core bundle grants: release feeds, installers, telemetry.
var providerChatterHosts = []string{
	"raw.githubusercontent.com", "api.github.com", "registry.npmjs.org", "formulae.brew.sh",
	"downloads.claude.ai", "datadoghq.com", "datadoghq.eu", "sentry.io", "ab.chatgpt.com", "statsig.com",
}

// verifyProviderNetworkLiveSilence requires that no retained refusal of this capture's runs names
// a chatter host. The refusals are read whole and by name (the local operator projection), so a
// DNS-only refusal counts as much as a TLS one: either means the client asked.
func verifyProviderNetworkLiveSilence(capture *box.CapturedEgress) (string, error) {
	root, err := box.NetworkStatePath()
	if err != nil {
		return "state", err
	}
	evidence, err := networkstate.OpenEvidence(root, nil)
	if err != nil {
		return "evidence", err
	}
	defer evidence.Close()
	page, err := evidence.Executions("")
	if err != nil {
		return "executions", err
	}
	for _, summary := range page.Executions {
		record, err := evidence.Execution(summary.ID)
		if err != nil || record.Snapshot.PolicyFingerprint != capture.Fingerprint {
			continue
		}
		inspection, err := evidence.Inspect(summary.ID, time.Now(), true)
		if err != nil {
			return "inspect", err
		}
		for _, denial := range inspection.Observed.Denials {
			for _, host := range providerChatterHosts {
				if denial.Name == host || strings.HasSuffix(denial.Name, "."+host) {
					return "chatter_" + strings.ReplaceAll(host, ".", "_"),
						errors.New("a hello-and-exit session asked for " + denial.Name + ": the client's chatter control is not holding")
				}
			}
		}
	}
	return "", nil
}

// runProviderNetworkLiveBox launches ONE filtered box. It passes no extra runtime arguments: a
// filtered launch qualifies bind mounts and KEY=VALUE environment only, and a --cidfile there
// would be refused by name (the gateway engine records the exact container id itself).
func runProviderNetworkLiveBox(cfg *config.Config, rt runtime.Runtime, capture *box.CapturedEgress,
	provider string, command []string) (string, int, error) {
	stdout := liveprovider.NewBoundedBuffer(liveOutputLimit)
	stderr := liveprovider.NewBoundedBuffer(liveOutputLimit)
	ctx, cancel := context.WithTimeout(context.Background(), livePromptDeadline)
	defer cancel()
	code, err := box.Run(cfg, rt, box.RunSpec{
		Repo: cfg.RepoOverride, Cmd: command, Agent: provider, AgentCommand: true,
		Batch: true, Quiet: true, Homes: true, Network: false, Cache: false,
		Stdout: stdout, Stderr: stderr, Ctx: ctx, CapturedEgress: capture,
	})
	if stdout.Truncated() || stderr.Truncated() {
		return "", code, errors.New("live provider output exceeded its bound")
	}
	return stdout.String(), code, err
}

// verifyProviderNetworkLiveReceipts requires every observed destination of this capture's runs to
// be one the frozen policy allows. A provider that quietly needed a host nobody granted would
// otherwise pass on the strength of a retry succeeding somewhere else.
func verifyProviderNetworkLiveReceipts(capture *box.CapturedEgress) (string, error) {
	policy, err := capture.Store.LoadSnapshot(capture.Project, capture.Fingerprint)
	if err != nil {
		return "snapshot", err
	}
	names, err := providerNetworkLiveDestinations(capture)
	if err != nil {
		return "inspect", err
	}
	if len(names) == 0 {
		return "no_receipt", errors.New("no sealed receipt retained a reached destination for this capture")
	}
	for _, name := range names {
		if !policy.Domain(name, 443).Allowed {
			return "ungranted_destination", errors.New("a filtered run reached " + name + ", which this policy never granted")
		}
	}
	return "", nil
}

// providerNetworkLiveDestinations is every host this capture's sealed runs actually reached. It
// reads the LOCAL operator projection: this is the owner's own host, and a receipt that withheld
// its destinations could not answer the question being asked here.
func providerNetworkLiveDestinations(capture *box.CapturedEgress) ([]string, error) {
	root, err := box.NetworkStatePath()
	if err != nil {
		return nil, err
	}
	evidence, err := networkstate.OpenEvidence(root, nil)
	if err != nil {
		return nil, err
	}
	defer evidence.Close()
	page, err := evidence.Executions("")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, summary := range page.Executions {
		record, err := evidence.Execution(summary.ID)
		if err != nil || record.Snapshot.PolicyFingerprint != capture.Fingerprint || record.Receipt == nil {
			continue
		}
		inspection, err := evidence.Inspect(summary.ID, time.Now(), true)
		if err != nil {
			return nil, err
		}
		for _, connection := range inspection.Observed.Connections {
			if connection.Name == "" || connection.State == "denied" || connection.State == "failed" {
				continue
			}
			if !slices.Contains(names, connection.Name) {
				names = append(names, connection.Name)
			}
		}
	}
	return names, nil
}

// verifyProviderNetworkLiveReached requires at least one of hosts to appear as an observed
// destination of this capture's runs.
func verifyProviderNetworkLiveReached(capture *box.CapturedEgress, hosts []string) (string, error) {
	names, err := providerNetworkLiveDestinations(capture)
	if err != nil {
		return "inspect", err
	}
	for _, host := range hosts {
		if slices.Contains(names, host) {
			return "", nil
		}
	}
	return "mcp_unreached", errors.New("no run reached a configured MCP host")
}

// providerNetworkLiveMCPHosts is the shared MCP configuration's HTTP hosts, which admission grants
// automatically. Reaching one is the only MCP claim this probe can make: whether a model chooses to
// call a tool is the model's business, not the boundary's.
func providerNetworkLiveMCPHosts(cfg *config.Config, spec box.RunSpec) []string {
	inputs, err := box.NetworkMCPDependencies(cfg, spec)
	if err != nil {
		return nil
	}
	var hosts []string
	for _, input := range inputs {
		for _, rule := range input.Rules {
			if rule.To.Domain != "" && !slices.Contains(hosts, rule.To.Domain) {
				hosts = append(hosts, rule.To.Domain)
			}
		}
	}
	return hosts
}
