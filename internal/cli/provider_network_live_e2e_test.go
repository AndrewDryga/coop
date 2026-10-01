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
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func TestProviderNetworkLiveCompatibility(t *testing.T) {
	testProviderLiveCompatibility(t, liveWorkflowNetwork)
}

// executeProviderNetworkLiveChild runs a fresh prompt, its native resume, and a controlled shared
// MCP tool call behind one frozen policy, then reads the sealed receipts back and
// requires every destination the boxes actually reached to be one this policy granted.
//
// A host that cannot be set up for filtered runs, has no runtime or no credential SKIPS: an
// unqualified host cannot fail a compatibility claim it never made.
func executeProviderNetworkLiveChild(target agents.Target, marker, attemptFile, preflightReason string) liveprovider.ProviderResult {
	result := liveprovider.ProviderResult{Provider: target.Provider}
	fail := func(reason, phase, class string, code int) liveprovider.ProviderResult {
		return providerNetworkLiveFailure(result, reason, phase, class, code)
	}
	harnessFail := func(detail string) liveprovider.ProviderResult {
		failed := fail(liveprovider.ReasonHarnessFailed, "harness", "harness", 0)
		failed.DetailCode = detail
		return failed
	}
	skip := func(reason string) liveprovider.ProviderResult {
		result.Status, result.ReasonCode = liveprovider.StatusSkipped, reason
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
		return skip(liveprovider.ReasonMissingRuntime)
	}
	if preflightReason != "" {
		return skip(preflightReason)
	}
	// One admission for all launches, exactly as a loop or an ACP session does: a policy that
	// changed between them would be a different experiment.
	filtered := egress.Filtered
	spec := box.RunSpec{
		Repo: cfg.RepoOverride, Agent: target.Provider, AgentCommand: true, Homes: true,
	}
	capture, err := box.AdmitNetwork(cfg, rt, spec, box.NetworkAdmission{InvocationMode: &filtered})
	if err != nil {
		// A host that cannot be set up for filtered runs — admission tries that
		// itself now — is a skip, not a compatibility failure.
		if errors.Is(err, box.ErrNetworkSetupFailed) {
			return skip(liveprovider.ReasonMissingImage)
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
	version, code, err := runProviderNetworkLiveBox(cfg, rt, capture, target.Provider, []string{interactive[0], "--version"}, liveVersionDeadline)
	if err != nil || code != 0 {
		if code == 127 {
			return skip(liveprovider.ReasonMissingCLI)
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
	stdout, code, runErr := runProviderNetworkLiveBox(cfg, rt, capture, target.Provider, command, livePromptDeadline)
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
		providerResumeRecallPrompt(target.Provider))
	if !ok {
		return harnessFail("session_lookup")
	}
	stdout, code, runErr = runProviderNetworkLiveBox(cfg, rt, capture, target.Provider, command, livePromptDeadline)
	if runErr != nil || code != 0 {
		if errors.Is(runErr, context.DeadlineExceeded) {
			return fail(liveprovider.ReasonPromptTimeout, "resume", "timeout", code)
		}
		return fail(liveprovider.ReasonPromptExit, "resume", "provider", code)
	}
	if _, reply, err = providerResumeLiveOutput(ag, resolvedID, stdout); err != nil || reply != marker {
		return fail(liveprovider.ReasonMarkerMismatch, "resume", "marker", code)
	}
	// Stage three is mandatory, independent of the operator's personal MCP configuration. Only
	// this launch's witness counts; the earlier prompts may have connected without calling a tool.
	witness := filepath.Join(cfg.AgentProfileDir(target.Provider, account), ".coop-network-mcp.log")
	command = ag.Headless(cfg, "Call exactly the coop_probe_tool MCP tool once; wait for its result, then respond with exactly "+
		marker+" and no other text. Do not answer without calling the tool.")
	command = providerNetworkLiveMCPCommand(strings.TrimRight(cfg.HomeInBox, "/")+"/."+target.Provider+"/.coop-network-mcp.log", command)
	if _, code, runErr = runProviderNetworkLiveBox(cfg, rt, capture, target.Provider, command, livePromptDeadline); runErr != nil || code != 0 {
		if errors.Is(runErr, context.DeadlineExceeded) {
			return fail(liveprovider.ReasonPromptTimeout, "mcp", "timeout", code)
		}
		return fail(liveprovider.ReasonPromptExit, "mcp", "provider", code)
	}
	if err := verifyProviderNetworkLiveMCP(cfg.ConfigDir, witness); err != nil {
		failed := fail(liveprovider.ReasonPromptExit, "mcp", "provider", 0)
		failed.DetailCode = "mcp_tool_witness"
		return failed
	}
	// The receipts are the evidence: what the boxes reached has to be what the policy granted.
	if detail, err := verifyProviderNetworkLiveReceipts(capture, ag.CredentialBroker().Upstream); err != nil {
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

// Keep attempt truth from the independently written marker boundary. A failed version probe is
// not paid work; an MCP/setup error after a prompt must not erase the calls already made.
func providerNetworkLiveFailure(result liveprovider.ProviderResult, reason, phase, class string, code int) liveprovider.ProviderResult {
	result.Passed = false
	result.Status, result.ReasonCode = liveprovider.StatusFailed, reason
	result.Phase, result.ExitCode, result.ErrorClass = phase, code, class
	result.TimedOut = reason == liveprovider.ReasonPromptTimeout
	return result
}

// providerChatterHosts are the hosts a managed client reaches on its own — never for the
// operator's work — and that no core bundle grants: release feeds, installers, telemetry.
var providerChatterHosts = []string{
	"raw.githubusercontent.com", "api.github.com", "registry.npmjs.org", "formulae.brew.sh",
	"downloads.claude.ai", "datadoghq.com", "datadoghq.eu", "sentry.io", "ab.chatgpt.com", "statsig.com",
	"mixpanel.com",
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
	executions, err := providerNetworkLiveExecutions(evidence)
	if err != nil {
		return "executions", err
	}
	for _, summary := range executions {
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
	provider string, command []string, deadline time.Duration) (string, int, error) {
	stdout := liveprovider.NewBoundedBuffer(liveOutputLimit)
	stderr := liveprovider.NewBoundedBuffer(liveOutputLimit)
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
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
func verifyProviderNetworkLiveReceipts(capture *box.CapturedEgress, brokerUpstream string) (string, error) {
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
		// A broker upstream is intentionally absent from the agent policy: only the
		// gateway's session-bound route may reach it. Every other reached name must
		// still be an ordinary frozen-policy grant.
		if !policy.Domain(name, 443).Allowed && name != brokerUpstream {
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
	executions, err := providerNetworkLiveExecutions(evidence)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, summary := range executions {
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

func providerNetworkLiveExecutions(evidence *networkstate.Evidence) ([]networkstate.ExecutionSummary, error) {
	var executions []networkstate.ExecutionSummary
	for cursor := ""; ; {
		page, err := evidence.Executions(cursor)
		if err != nil {
			return nil, err
		}
		executions = append(executions, page.Executions...)
		if page.Next == "" {
			return executions, nil
		}
		cursor = page.Next
	}
}

func prepareProviderNetworkLiveMCP(layout procharness.Layout, selection liveprovider.Selection, home, platform string) (string, error) {
	system, arch, ok := strings.Cut(platform, "/")
	if !ok || system != "linux" || (arch != "amd64" && arch != "arm64") || !filepath.IsAbs(home) {
		return "", errors.New("unsupported MCP fixture platform or home")
	}
	profile := filepath.Join(layout.Config, selection.Provider, "profiles", selection.Account)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", filepath.Join(profile, "mcpprobe"), "../box/testdata/mcpprobe")
	build.Env = append(os.Environ(), "GOOS="+system, "GOARCH="+arch, "CGO_ENABLED=0")
	if err := build.Run(); err != nil {
		return "", errors.New("build controlled MCP fixture")
	}
	boxProfile := home + "/." + selection.Provider
	body, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"coop-probe": map[string]any{
		"type": "stdio", "command": boxProfile + "/mcpprobe", "args": []string{},
		"env": map[string]string{"COOP_PROBE_LOG": boxProfile + "/.coop-network-mcp.log"},
	}}})
	if err != nil {
		return "", err
	}
	path := filepath.Join(layout.State, "provider-network-mcp.json")
	return path, os.WriteFile(path, body, 0o600)
}

func providerNetworkLiveMCPCommand(path string, command []string) []string {
	// Reset in the same container that writes the witness. Host unlink/truncate can leave
	// Docker Desktop's next bind-mounted write missing or prefixed by stale-offset holes.
	return append([]string{"/bin/sh", "-c", `/bin/rm -f -- "$1" && shift && exec "$@"`, "coop-mcp-witness", path}, command...)
}

// The profile witness proves cooperative native-client compatibility, not adversarial attestation:
// the model can write its own profile. Source config stays outside every agent mount.
func verifyProviderNetworkLiveMCP(root, path string) error {
	f, err := procharness.OpenRegularFile(root, path, os.O_RDONLY)
	if err != nil {
		return errors.New("missing regular MCP witness")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(data) > 65536 || !providerNetworkLiveMCPWitness(string(data)) {
		return errors.New("missing ordered MCP handshake and tool-call witness")
	}
	return nil
}

func providerNetworkLiveMCPWitness(log string) bool {
	steps := []string{"launched", "initialize", "answered initialize", "notifications/initialized", "tools/list", "tools/call coop_probe_tool", "answered tools/call coop_probe_tool"}
	next, calls := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if strings.HasPrefix(line, "tools/call ") {
			calls++
		}
		if line == "launched" {
			next = 0
		}
		index := slices.Index(steps, line)
		if index < 0 {
			if strings.HasPrefix(line, "tools/call") {
				return false
			}
			continue
		}
		if index == 4 && next == 5 {
			continue // clients may refresh the tool catalog before invoking it
		}
		if index != next {
			return false
		}
		next++
	}
	return next == len(steps) && calls == 1
}
