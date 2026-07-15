//go:build acpe2e

// Real-adapter end-to-end for `coop acp`: build an isolated coop binary, drive it as
// an ACP client over stdio, and verify contracts that unit tests cannot prove. Needs
// a configured runtime, a built box, and signed-in providers. Run with `make acp-e2e`.
//
// Tests signal only the supervisor process they start, then authoritatively reap their unique
// test label, so this suite is safe to run alongside live editor sessions.
package acpproxy_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

var (
	coopE2EBinary          string
	coopE2ERepo            string
	coopE2ELayout          procharness.Layout
	coopE2ERealConfig      *config.Config
	coopE2ERuntime         runtime.Runtime
	coopE2ERuntimeSettings liveprovider.RuntimeSettings
)

const (
	liveACPStderrLimit     = 64 << 10
	liveACPFrameLimit      = 256 << 10
	liveACPTranscriptLimit = 4 << 20
	liveACPFrameCountLimit = 4096
	liveACPPresetLimit     = 4 << 20
)

func TestMain(m *testing.M) {
	os.Exit(runLiveACPTests(m))
}

func runLiveACPTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "coop-acp-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "ACP E2E setup failed: phase=temp_directory error_class=harness")
		return 1
	}
	defer os.RemoveAll(dir)
	coopE2ELayout, err = procharness.NewLayout(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ACP E2E setup failed: phase=layout error_class=harness")
		return 1
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		fmt.Fprintln(os.Stderr, "ACP E2E setup failed: phase=repository_root error_class=harness")
		return 1
	}
	coopE2ERealConfig = config.Load()
	coopE2ERuntime, err = runtime.Detect(coopE2ERealConfig.RuntimeName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ACP E2E setup failed: phase=runtime error_class=prerequisite")
		return 1
	}
	connectionEnv, err := liveprovider.CaptureRuntimeConnectionEnv(coopE2ERuntime.Name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ACP E2E setup failed: phase=runtime_connection error_class=harness")
		return 1
	}
	coopE2ERuntimeSettings = liveprovider.RuntimeSettings{
		Name: coopE2ERuntime.Name, Image: coopE2ERealConfig.ImageOverride,
		BaseImage: coopE2ERealConfig.BaseImage, HomeInBox: coopE2ERealConfig.HomeInBox,
		AgentPackages: coopE2ERealConfig.AgentPackages, ConnectionEnv: connectionEnv,
	}
	coopE2ERepo = coopE2ELayout.Repo
	if err := prepareLiveRepo(root, coopE2ERepo); err != nil {
		fmt.Fprintln(os.Stderr, "ACP E2E setup failed: phase=repository error_class=harness")
		return 1
	}
	coopE2EBinary = filepath.Join(coopE2ELayout.Root, "coop")
	build := exec.Command("go", "build", "-trimpath", "-tags", "cooplivetest", "-o", coopE2EBinary, ".")
	build.Dir = root
	buildOutput := liveprovider.NewBoundedBuffer(liveACPStderrLimit)
	build.Stdout, build.Stderr = buildOutput, buildOutput
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "ACP E2E setup failed: phase=build error_class=process truncated=%t\n", buildOutput.Truncated())
		return 1
	}
	return m.Run()
}

func prepareLiveRepo(sourceRoot, destination string) error {
	source := filepath.Join(sourceRoot, ".agent", "presets", "frontier")
	target := filepath.Join(destination, ".agent", "presets", "frontier")
	if err := liveprovider.CopyRegularTree(sourceRoot, source, target, liveACPPresetLimit); err != nil {
		return err
	}
	init := exec.Command("git", "init", "-q", destination)
	if err := init.Run(); err != nil {
		return errors.New("initialize disposable repository")
	}
	return nil
}

type liveACP struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	client      *acpClient
	stderr      *liveprovider.BoundedBuffer
	done        chan error
	runtime     runtime.Runtime
	supervisor  string
	cleanupRoot string
	cidDir      string
	processDir  string
	credentials *liveprovider.Prepared
}

func startLiveACP(t *testing.T, provider string, requiredProviders ...string) *liveACP {
	t.Helper()
	selections, err := liveACPSelections(provider, requiredProviders...)
	if err != nil {
		failLiveACPSetup(t, "prerequisites")
	}
	processLayout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		failLiveACPSetup(t, "layout")
	}
	processLayout.Repo = coopE2ERepo
	if err := os.Remove(processLayout.Config); err != nil {
		failLiveACPSetup(t, "config_reset")
	}
	credentials, err := liveprovider.Prepare(coopE2ERealConfig.ConfigDir, processLayout.Config, selections)
	if err != nil {
		failLiveACPCredentialSetup(t, err)
	}
	t.Cleanup(func() {
		if err := credentials.VerifySources(); err != nil {
			t.Errorf("ACP source credentials changed during live run")
		}
	})
	for _, selection := range selections {
		reason := credentials.PreflightReason(selection.Provider, selection.Account, time.Now().Add(30*time.Minute))
		if reason == "" {
			continue
		}
		if reason == liveprovider.ReasonUnsafeCredential || os.Getenv("COOP_ACP_LIVE_REQUIRE_ALL") == "1" {
			t.Fatalf("live ACP prerequisite failed: provider=%s reason=%s", selection.Provider, reason)
		}
		t.Skipf("live ACP %s prerequisite for %s", reason, selection.Provider)
	}
	if err := credentials.VerifySources(); err != nil {
		t.Fatalf("%s source credentials changed before ACP start", provider)
	}
	supervisor := "acp-live-" + strings.ReplaceAll(newForeignSessionID(t), "-", "")
	control, processDir, err := liveprovider.NewProcessControl(processLayout, true)
	if err != nil {
		failLiveACPSetup(t, "process_control")
	}
	environment, err := liveprovider.ProcessEnvironment(
		processLayout, os.Getenv("PATH"), coopE2ERuntimeSettings, liveprovider.ProcessSpec{
			Supervisor: supervisor, ProcessDir: processDir, ControlFD: 3,
		},
	)
	if err != nil {
		control.Close()
		failLiveACPSetup(t, "environment")
	}
	t.Setenv("COOP_CONFIG_DIR", processLayout.Config)
	cmd := exec.Command(coopE2EBinary, "acp", provider)
	cmd.Dir = coopE2ERepo
	cmd.Env = append([]string(nil), environment...)
	cmd.ExtraFiles = []*os.File{control}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		failLiveACPSetup(t, "stdin")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		failLiveACPSetup(t, "stdout")
	}
	stderr := liveprovider.NewBoundedBuffer(liveACPStderrLimit)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		control.Close()
		_ = stdin.Close()
		failLiveACPSetup(t, "process_start")
	}
	control.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	c := newACPClientWithLimits(stdin, acpClientLimits{
		MaxFrameBytes: liveACPFrameLimit, MaxTranscriptBytes: liveACPTranscriptLimit,
		MaxFrames: liveACPFrameCountLimit,
	})
	go c.read(stdout)
	live := &liveACP{
		cmd: cmd, stdin: stdin, client: c, stderr: stderr, done: done,
		runtime: coopE2ERuntime, supervisor: supervisor,
		cleanupRoot: processLayout.Root, cidDir: processLayout.State,
		processDir: processDir, credentials: credentials,
	}
	t.Cleanup(func() { live.stop(t) })
	return live
}

func (a *liveACP) diagnostic(phase string, err error) string {
	return liveACPDiagnostic(phase, err, a.stderr.Truncated(), a.client.stats())
}

func (a *liveACP) fail(t *testing.T, phase string, err error) {
	t.Helper()
	t.Fatalf("live ACP failure: %s", a.diagnostic(phase, err))
}

func failLiveACPSetup(t *testing.T, phase string) {
	t.Helper()
	t.Fatalf("live ACP setup failure: %s", liveACPDiagnostic(phase, errors.New("setup"), false, acpClientStats{}))
}

func failLiveACPCredentialSetup(t *testing.T, err error) {
	t.Helper()
	t.Fatalf(
		"live ACP setup failure: %s detail_code=%s action=repair_selected_credential",
		liveACPDiagnostic("credential_isolation", errors.New("setup"), false, acpClientStats{}),
		liveprovider.CredentialDetailCode(err),
	)
}

func liveACPSelections(targetOrPreset string, extraProviders ...string) ([]liveprovider.Selection, error) {
	targets := []agents.Target{}
	if target, err := agents.ParseTarget(targetOrPreset); err == nil {
		targets = append(targets, target)
	} else {
		p, loadErr := preset.Load(coopE2ERepo, "", targetOrPreset)
		if loadErr != nil {
			return nil, loadErr
		}
		if len(p.LeadLadder) == 0 {
			targets = append(targets, agents.Target{Provider: p.LeadAgent})
		} else {
			targets = append(targets, p.LeadLadder...)
		}
	}
	for _, raw := range extraProviders {
		target, err := agents.ParseTarget(raw)
		if err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return nil, errors.New("live ACP target has no providers")
	}
	return liveprovider.SelectionsForTargets(coopE2ERealConfig, targets)
}

func (a *liveACP) stop(t *testing.T) {
	t.Helper()
	var shutdownPhase string
	var shutdownErr error
	revokeErr := a.credentials.Revoke()
	_ = a.stdin.Close()
	select {
	case err := <-a.done:
		if err != nil {
			shutdownPhase, shutdownErr = "shutdown", err
		}
	case <-time.After(20 * time.Second):
		_ = syscall.Kill(-a.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case err := <-a.done:
			shutdownPhase, shutdownErr = "shutdown_sigterm", err
		case <-time.After(20 * time.Second):
			_ = syscall.Kill(-a.cmd.Process.Pid, syscall.SIGKILL)
			select {
			case <-a.done:
				shutdownPhase = "shutdown_sigkill"
				shutdownErr = errors.New("forced termination")
			case <-time.After(5 * time.Second):
				shutdownPhase = "shutdown_reap"
				shutdownErr = errors.New("process did not reap after forced termination")
			}
		}
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cleanupErr := liveprovider.CleanupSupervisor(cleanupCtx, liveprovider.SupervisorCleanupSpec{
		Root: a.cleanupRoot, CIDDir: a.cidDir, ProcessDir: a.processDir, Supervisor: a.supervisor,
		LabelKey: liveprovider.SupervisorLabelKey, OperationTimeout: 2 * time.Second,
		QuietPeriod: time.Second, PollInterval: 100 * time.Millisecond,
	}, liveprovider.SupervisorCleanupOps{
		RemoveContainer: a.runtime.RemoveContainerContext,
		RemoveByLabel:   a.runtime.RemoveByLabel,
	})
	cancel()
	if cleanupErr != nil {
		t.Errorf("live ACP cleanup failure: %s", a.diagnostic("cleanup", cleanupErr))
	}
	if revokeErr != nil {
		t.Errorf("live ACP credential revocation failure: %s", a.diagnostic("credential_revoke", revokeErr))
	}
	if shutdownErr != nil {
		t.Errorf("live ACP shutdown failure: %s", a.diagnostic(shutdownPhase, shutdownErr))
	}
}

type liveConfigOption struct {
	ID           string `json:"id"`
	CurrentValue string `json:"currentValue"`
	Options      []struct {
		Value string `json:"value"`
	} `json:"options"`
}

func liveConfigOptions(t *testing.T, live *liveACP, response map[string]any) map[string]liveConfigOption {
	t.Helper()
	result, ok := response["result"].(map[string]any)
	if !ok {
		live.fail(t, "config_response", nil)
	}
	raw, err := json.Marshal(result["configOptions"])
	if err != nil {
		t.Fatal(err)
	}
	var options []liveConfigOption
	if err := json.Unmarshal(raw, &options); err != nil {
		live.fail(t, "config_decode", err)
	}
	byID := make(map[string]liveConfigOption, len(options))
	for _, option := range options {
		byID[option.ID] = option
	}
	return byID
}

func liveOptionValue(t *testing.T, live *liveACP, option liveConfigOption, reject ...string) string {
	t.Helper()
	for _, candidate := range option.Options {
		if candidate.Value != "" && !slices.Contains(reject, candidate.Value) {
			return candidate.Value
		}
	}
	live.fail(t, "config_option", nil)
	return ""
}

func setLiveConfig(ctx context.Context, t *testing.T, live *liveACP, sessionID, configID, value string) map[string]liveConfigOption {
	t.Helper()
	response, err := live.client.req(ctx, "session/set_config_option", map[string]any{
		"sessionId": sessionID,
		"configId":  configID,
		"value":     value,
	})
	if err != nil {
		live.fail(t, "set_config", err)
	}
	return liveConfigOptions(t, live, response)
}

func awaitLiveReplay(t *testing.T, ctx context.Context, live *liveACP, mark int, sessionID string) map[string]liveConfigOption {
	t.Helper()
	for {
		event, next, err := live.client.event(ctx, mark, "session/update")
		if err != nil {
			live.fail(t, "replay", err)
		}
		mark = next
		raw := string(event.Raw)
		if strings.Contains(raw, sessionID) && strings.Contains(raw, `"sessionUpdate":"config_option_update"`) {
			var update struct {
				Params struct {
					Update struct {
						ConfigOptions []liveConfigOption `json:"configOptions"`
					} `json:"update"`
				} `json:"params"`
			}
			if err := json.Unmarshal(event.Raw, &update); err != nil {
				live.fail(t, "replay_decode", err)
			}
			options := make(map[string]liveConfigOption, len(update.Params.Update.ConfigOptions))
			for _, option := range update.Params.Update.ConfigOptions {
				options[option.ID] = option
			}
			return options
		}
	}
}

func liveAssistantText(frames []wireFrame) string {
	var text strings.Builder
	for _, frame := range frames {
		var event struct {
			Params struct {
				Update struct {
					SessionUpdate string `json:"sessionUpdate"`
					Content       struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"update"`
			} `json:"params"`
		}
		if json.Unmarshal(frame.Raw, &event) == nil && event.Params.Update.SessionUpdate == "agent_message_chunk" {
			text.WriteString(event.Params.Update.Content.Text)
		}
	}
	return text.String()
}

func liveMessageFieldEquals(frames []wireFrame, key, want string) bool {
	var find func(any) bool
	find = func(value any) bool {
		switch value := value.(type) {
		case map[string]any:
			if got, ok := value[key].(string); ok && got == want {
				return true
			}
			for _, nested := range value {
				if find(nested) {
					return true
				}
			}
		case []any:
			for _, nested := range value {
				if find(nested) {
					return true
				}
			}
		}
		return false
	}
	for _, frame := range frames {
		if find(frame.Msg) {
			return true
		}
	}
	return false
}

func liveMessageContains(frames []wireFrame, needle string) bool {
	var find func(any) bool
	find = func(value any) bool {
		switch value := value.(type) {
		case string:
			return strings.Contains(value, needle)
		case map[string]any:
			for _, nested := range value {
				if find(nested) {
					return true
				}
			}
		case []any:
			for _, nested := range value {
				if find(nested) {
					return true
				}
			}
		}
		return false
	}
	for _, frame := range frames {
		if find(frame.Msg) {
			return true
		}
	}
	return false
}

func assertLiveEditorAuthenticationHidden(t *testing.T, live *liveACP, response map[string]any) {
	t.Helper()
	result, _ := response["result"].(map[string]any)
	methods, ok := result["authMethods"].([]any)
	if !ok || len(methods) != 0 {
		live.fail(t, "authentication_capability", nil)
	}
	capabilities, _ := result["agentCapabilities"].(map[string]any)
	if _, ok := capabilities["auth"]; ok {
		live.fail(t, "authentication_capability", nil)
	}
}

func TestCodexTargetRolloutTruth(t *testing.T) {
	const (
		wantModel  = "gpt-5.6-sol"
		wantEffort = "xhigh"
	)
	live := startLiveACP(t, "codex:"+wantModel+"/"+wantEffort)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cwd := coopE2ERepo
	if _, err := live.client.req(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
	}); err != nil {
		live.fail(t, "initialize", err)
	}
	response, err := live.client.req(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		live.fail(t, "session_new", err)
	}
	sessionID := responseSessionID(response)
	if sessionID == "" {
		live.fail(t, "session_new_result", nil)
	}
	if _, err := live.client.req(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": "Reply with only OK."}},
	}); err != nil {
		live.fail(t, "prompt", err)
	}

	rollouts := filepath.Join(config.Load().ConfigDir, "codex", "acp-sessions", "sessions")
	model, effort, err := waitForCodexRolloutTarget(ctx, rollouts, sessionID)
	if err != nil {
		live.fail(t, "rollout_read", err)
	}
	if model != wantModel || effort != wantEffort {
		live.fail(t, "rollout_target", nil)
	}
}

func waitForCodexRolloutTarget(ctx context.Context, root, sessionID string) (string, string, error) {
	var lastErr error
	for {
		model, effort, walkErr := codexStoredTarget(root, sessionID)
		if model != "" || effort != "" {
			return model, effort, nil
		}
		if walkErr != nil {
			lastErr = walkErr
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return "", "", fmt.Errorf("%w (last rollout read: %v)", ctx.Err(), lastErr)
			}
			return "", "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func TestFrontierStoredTargetTruth(t *testing.T) {
	live := startLiveACP(t, "frontier")
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	cwd := coopE2ERepo
	if _, err := live.client.req(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": true, "writeTextFile": true},
		},
	}); err != nil {
		live.fail(t, "initialize", err)
	}
	response, err := live.client.req(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		live.fail(t, "session_new", err)
	}
	sessionID := responseSessionID(response)
	marker := "FRONTIER_TARGET_" + strings.ReplaceAll(newForeignSessionID(t), "-", "")
	if _, err := live.client.req(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": "Reply with only " + marker + "."}},
	}); err != nil {
		live.fail(t, "prompt", err)
	}
	provider, model, effort, err := waitForFrontierStoredTarget(ctx, config.Load().ConfigDir, sessionID, marker)
	if err != nil {
		live.fail(t, "provider_storage", err)
	}
	switch provider {
	case "claude":
		if model != "claude-fable-5" {
			live.fail(t, "provider_target", nil)
		}
	case "codex":
		if model != "gpt-5.6-sol" || effort != "xhigh" {
			live.fail(t, "provider_target", nil)
		}
	default:
		live.fail(t, "provider_target", nil)
	}
}

func waitForFrontierStoredTarget(ctx context.Context, configDir, editorSessionID, marker string) (string, string, string, error) {
	var lastErr error
	for {
		model, claudeErr := claudeStoredModel(filepath.Join(configDir, "claude", "acp-sessions", "projects"), editorSessionID, marker)
		if model != "" {
			return "claude", model, "", nil
		}
		model, effort, codexErr := codexStoredTargetByMarker(filepath.Join(configDir, "codex", "acp-sessions", "sessions"), marker)
		if model != "" {
			return "codex", model, effort, nil
		}
		if claudeErr != nil {
			lastErr = claudeErr
		}
		if codexErr != nil {
			lastErr = codexErr
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return "", "", "", fmt.Errorf("%w (last provider storage read: %v)", ctx.Err(), lastErr)
			}
			return "", "", "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func claudeStoredModel(root, sessionID, marker string) (string, error) {
	want := sessionID + ".jsonl"
	model := ""
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || info.Name() != want {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// The prompt text is present on every attempted rung; the exact reply is present only in the
		// provider that completed the turn. Requiring both occurrences avoids reporting a failed rung.
		if strings.Count(string(data), marker) < 2 {
			return nil
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		for {
			var record struct {
				Message struct {
					Model string `json:"model"`
				} `json:"message"`
			}
			if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return err
			}
			if record.Message.Model != "" && record.Message.Model != "<synthetic>" {
				model = record.Message.Model
				return nil
			}
		}
		return nil
	})
	return model, err
}

func codexStoredTarget(root, sessionID string) (string, string, error) {
	return codexStoredTargetMatching(root, func(info os.FileInfo, _ []byte) bool {
		return strings.Contains(info.Name(), sessionID)
	})
}

func codexStoredTargetByMarker(root, marker string) (string, string, error) {
	return codexStoredTargetMatching(root, func(_ os.FileInfo, data []byte) bool {
		return strings.Count(string(data), marker) >= 2
	})
}

func codexStoredTargetMatching(root string, matches func(os.FileInfo, []byte) bool) (string, string, error) {
	model, effort := "", ""
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".jsonl") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !matches(info, data) {
			return nil
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		for {
			var record struct {
				Type    string `json:"type"`
				Payload struct {
					Model  string `json:"model"`
					Effort string `json:"effort"`
				} `json:"payload"`
			}
			if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return err
			}
			if record.Type == "turn_context" {
				model, effort = record.Payload.Model, record.Payload.Effort
			}
		}
		return nil
	})
	return model, effort, err
}

// TestPresetOwnsSelectorState drives coop's real outer ACP control layer. A direct preset owns the
// toolbar and ignores persisted plain selector writes; leaving it exposes the normal plain controls.
func TestPresetOwnsSelectorState(t *testing.T) {
	live := startLiveACP(t, "frontier")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cwd := coopE2ERepo
	if _, err := live.client.req(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
	}); err != nil {
		live.fail(t, "initialize", err)
	}
	response, err := live.client.req(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		live.fail(t, "session_new", err)
	}
	result, ok := response["result"].(map[string]any)
	if !ok {
		live.fail(t, "session_new_result", nil)
	}
	sessionID, _ := result["sessionId"].(string)
	if sessionID == "" {
		live.fail(t, "session_new_result", nil)
	}
	options := liveConfigOptions(t, live, response)
	if len(options) != 1 {
		live.fail(t, "preset_toolbar", nil)
	}
	if options["coop_preset"].CurrentValue != "frontier" {
		live.fail(t, "preset_toolbar", nil)
	}
	for _, tc := range []struct {
		configID string
		value    string
	}{
		{"coop_provider", "codex"},
		{"coop_account", "work"},
	} {
		options = setLiveConfig(ctx, t, live, sessionID, tc.configID, tc.value)
		if len(options) != 1 || options["coop_preset"].CurrentValue != "frontier" {
			live.fail(t, "preset_replay", nil)
		}
	}

	options = setLiveConfig(ctx, t, live, sessionID, "coop_preset", "none")
	for _, id := range []string{"coop_preset", "coop_provider", "coop_account"} {
		if _, ok := options[id]; !ok {
			live.fail(t, "plain_toolbar", nil)
		}
	}
	if options["coop_preset"].CurrentValue != "none" || options["coop_account"].CurrentValue != "auto" {
		live.fail(t, "plain_toolbar", nil)
	}

	plainProvider := options["coop_provider"].CurrentValue
	for _, option := range options["coop_provider"].Options {
		if option.Value != "" && option.Value != plainProvider {
			plainProvider = option.Value
			break
		}
	}
	if plainProvider != options["coop_provider"].CurrentValue {
		options = setLiveConfig(ctx, t, live, sessionID, "coop_provider", plainProvider)
	}
	plainAccount := "auto"
	for _, option := range options["coop_account"].Options {
		if option.Value != "" && option.Value != "auto" {
			plainAccount = option.Value
			break
		}
	}
	if plainAccount != "auto" {
		options = setLiveConfig(ctx, t, live, sessionID, "coop_account", plainAccount)
	}
	plainProvider = options["coop_provider"].CurrentValue
	plainAccount = options["coop_account"].CurrentValue

	options = setLiveConfig(ctx, t, live, sessionID, "coop_preset", "frontier")
	if len(options) != 1 || options["coop_preset"].CurrentValue != "frontier" {
		live.fail(t, "preset_toolbar", nil)
	}
	if plainProvider == "" || plainAccount == "" {
		live.fail(t, "plain_toolbar", nil)
	}
}

func TestLiveProviderConformance(t *testing.T) {
	cwd := coopE2ERepo
	for _, provider := range agents.Names() {
		t.Run(provider, func(t *testing.T) {
			live := startLiveACP(t, provider)
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
			defer cancel()
			initialize, err := live.client.req(ctx, "initialize", map[string]any{
				"protocolVersion": 1,
				"clientCapabilities": map[string]any{
					"fs": map[string]any{"readTextFile": true, "writeTextFile": true},
				},
			})
			if err != nil {
				live.fail(t, "initialize", err)
			}
			assertLiveEditorAuthenticationHidden(t, live, initialize)
			response, err := live.client.req(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
			if err != nil {
				live.fail(t, "session_new", err)
			}
			sessionID := responseSessionID(response)
			if sessionID == "" {
				live.fail(t, "session_new_result", nil)
			}
			options := liveConfigOptions(t, live, response)
			for _, id := range []string{"coop_preset", "coop_provider", "coop_account"} {
				if _, ok := options[id]; !ok {
					live.fail(t, "toolbar", nil)
				}
			}
			firstMark := live.client.mark()
			if _, err := live.client.req(ctx, "session/prompt", map[string]any{
				"sessionId": sessionID,
				"prompt":    []any{map[string]any{"type": "text", "text": "Reply with only LIVE_ONE_OK."}},
			}); err != nil {
				live.fail(t, "prompt", err)
			}
			if got := liveAssistantText(live.client.transcript()[firstMark:]); !strings.Contains(got, "LIVE_ONE_OK") {
				live.fail(t, "marker", nil)
			}

			modelOption, hasModel := options["model"]
			if !hasModel {
				live.fail(t, "model_capability", nil)
			}
			selectedModel := ""
			initialModel := modelOption.CurrentValue
			if hasModel {
				nextModel := liveOptionValue(t, live, modelOption, modelOption.CurrentValue)
				modelChangeMark := live.client.mark()
				options = setLiveConfig(ctx, t, live, sessionID, "model", nextModel)
				if options["model"].CurrentValue != nextModel {
					live.fail(t, "model_change", nil)
				}
				selectedModel = nextModel
				if provider == "grok" {
					// Grok's alternate model belongs to another agent type. The adapter explicitly asks
					// for a new session; wait for Coop's transparent migration before exercising the
					// separate SIGHUP replay below.
					options = awaitLiveReplay(t, ctx, live, modelChangeMark, sessionID)
					if options["model"].CurrentValue != nextModel {
						live.fail(t, "model_migration", nil)
					}
				}
			}

			replayMark := live.client.mark()
			if err := live.cmd.Process.Signal(syscall.SIGHUP); err != nil {
				live.fail(t, "replay_signal", err)
			}
			replayOptions := awaitLiveReplay(t, ctx, live, replayMark, sessionID)
			if selectedModel != "" && replayOptions["model"].CurrentValue != selectedModel {
				live.fail(t, "model_replay", nil)
			}
			secondMark := live.client.mark()
			if _, err := live.client.req(ctx, "session/prompt", map[string]any{
				"sessionId": sessionID,
				"prompt":    []any{map[string]any{"type": "text", "text": "Reply with only LIVE_TWO_OK."}},
			}); err != nil {
				live.fail(t, "resumed_prompt", err)
			}
			got := liveAssistantText(live.client.transcript()[secondMark:])
			if !strings.Contains(got, "LIVE_TWO_OK") && provider == "codex" && initialModel != "" &&
				strings.Contains(strings.ToLower(got), "selected model is at capacity") {
				t.Log("selected Codex model is at capacity; retrying the resumed thread on the initial model")
				fallback := setLiveConfig(ctx, t, live, sessionID, "model", initialModel)
				if fallback["model"].CurrentValue != initialModel {
					live.fail(t, "model_fallback", nil)
				}
				secondMark = live.client.mark()
				if _, err := live.client.req(ctx, "session/prompt", map[string]any{
					"sessionId": sessionID,
					"prompt":    []any{map[string]any{"type": "text", "text": "Reply with only LIVE_TWO_OK."}},
				}); err != nil {
					live.fail(t, "fallback_prompt", err)
				}
				got = liveAssistantText(live.client.transcript()[secondMark:])
			}
			if !strings.Contains(got, "LIVE_TWO_OK") {
				live.fail(t, "resumed_marker", nil)
			}
			if provider == "grok" && selectedModel != "" {
				if !liveMessageFieldEquals(live.client.transcript()[secondMark:], "modelId", selectedModel) {
					live.fail(t, "model_runtime", nil)
				}
			}
		})
	}
}

func TestLiveCrossProviderCarry(t *testing.T) {
	for _, destination := range []string{"codex", "grok"} {
		t.Run(destination, func(t *testing.T) {
			cwd := coopE2ERepo
			live := startLiveACP(t, "claude", destination)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()
			if _, err := live.client.req(ctx, "initialize", map[string]any{
				"protocolVersion": 1,
				"clientCapabilities": map[string]any{
					"fs": map[string]any{"readTextFile": true, "writeTextFile": true},
				},
			}); err != nil {
				live.fail(t, "initialize", err)
			}
			response, err := live.client.req(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
			if err != nil {
				live.fail(t, "session_new", err)
			}
			sessionID := responseSessionID(response)
			token := "COOP_CARRY_LIVE_" + strings.ToUpper(destination)
			if _, err := live.client.req(ctx, "session/prompt", map[string]any{
				"sessionId": sessionID,
				"prompt":    []any{map[string]any{"type": "text", "text": "Remember the token " + token + " and reply only SOURCE_OK."}},
			}); err != nil {
				live.fail(t, "source_prompt", err)
			}
			options := liveConfigOptions(t, live, response)
			provider := options["coop_provider"]
			found := false
			for _, option := range provider.Options {
				found = found || option.Value == destination
			}
			if !found {
				live.fail(t, "provider_option", nil)
			}
			mark := live.client.mark()
			options = setLiveConfig(ctx, t, live, sessionID, "coop_provider", destination)
			if options["coop_provider"].CurrentValue != destination {
				live.fail(t, "provider_switch", nil)
			}
			destinationMark := live.client.mark()
			if _, err := live.client.req(ctx, "session/prompt", map[string]any{
				"sessionId": sessionID,
				"prompt":    []any{map[string]any{"type": "text", "text": "Reply with only the token you were asked to remember in the carried conversation."}},
			}); err != nil {
				live.fail(t, "destination_prompt", err)
			}
			if got := liveAssistantText(live.client.transcript()[destinationMark:]); !strings.Contains(got, token) {
				live.fail(t, "carry_marker", nil)
			}
			if liveMessageContains(live.client.transcript()[mark:], "[coop] This thread continues") {
				live.fail(t, "carry_visibility", nil)
			}
		})
	}
}

func TestForeignSessionLoadRejectsUnknownID(t *testing.T) {
	cwd := coopE2ERepo
	const loadTimeout = 10 * time.Second
	cases := []struct {
		provider string
		code     int
		markers  []string
	}{
		{provider: "claude", code: -32002, markers: []string{"Resource not found"}},
		{provider: "codex", code: -32603, markers: []string{"no rollout found for thread id"}},
		{provider: "gemini", code: -32603, markers: []string{"Invalid session identifier", "No previous sessions found for this project"}},
		{provider: "grok", code: -32603, markers: []string{"FS_NOT_FOUND", "Path not found"}},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			foreignID := newForeignSessionID(t)
			live := startLiveACP(t, tc.provider)
			initCtx, cancelInit := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancelInit()
			if _, err := live.client.req(initCtx, "initialize", map[string]any{
				"protocolVersion": 1,
				"clientCapabilities": map[string]any{
					"fs": map[string]any{"readTextFile": true, "writeTextFile": true},
				},
			}); err != nil {
				live.fail(t, "initialize", err)
			}
			loadCtx, cancelLoad := context.WithTimeout(context.Background(), loadTimeout)
			started := time.Now()
			_, err := live.client.req(loadCtx, "session/load", map[string]any{
				"cwd":        cwd,
				"mcpServers": []any{},
				"sessionId":  foreignID,
			})
			elapsed := time.Since(started)
			cancelLoad()
			var rpcError *rpcErr
			switch {
			case err == nil:
				live.fail(t, "foreign_session", nil)
			case errors.As(err, &rpcError):
				if elapsed >= loadTimeout {
					live.fail(t, "foreign_session_timeout", context.DeadlineExceeded)
				}
				if rpcError.code != tc.code {
					live.fail(t, "foreign_session_code", rpcError)
				}
				matched := false
				for _, marker := range tc.markers {
					matched = matched || strings.Contains(rpcError.raw, marker)
				}
				if !matched {
					live.fail(t, "foreign_session_message", rpcError)
				}
				for _, marker := range foreignMarker(tc.provider, foreignID) {
					if !strings.Contains(rpcError.raw, marker) {
						live.fail(t, "foreign_session_identity", rpcError)
					}
				}
				t.Logf("foreign session rejected in %s with JSON-RPC code %d", elapsed.Round(time.Millisecond), rpcError.code)
			default:
				live.fail(t, "foreign_session", err)
			}
		})
	}
}

func newForeignSessionID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func foreignMarker(provider, id string) []string {
	if provider == "claude" || provider == "codex" {
		return []string{id}
	}
	return nil
}
