//go:build acpe2e

// Real-adapter end-to-end for `coop acp`: build an isolated coop binary, drive it as
// an ACP client over stdio, and verify contracts that unit tests cannot prove. Needs
// Docker, a built box, and signed-in providers. Run with `make acp-e2e`.
//
// Tests signal only the supervisor process they start; its normal label-scoped cleanup
// owns the boxes, so this suite is safe to run alongside live editor sessions.
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

	"github.com/AndrewDryga/coop/internal/config"
)

var (
	coopE2EBinary    string
	coopE2EConfigDir string
	coopE2ERepo      string
	coopE2ERuntime   []string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "coop-acp-e2e-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create ACP E2E temp dir: %v\n", err)
		os.Exit(1)
	}
	// macOS returns /var/... while child processes and container mounts resolve the same directory as
	// /private/var/.... Keep the editor cwd byte-identical to the path mounted into provider boxes.
	if resolved, rerr := filepath.EvalSymlinks(dir); rerr == nil {
		dir = resolved
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve repository root: %v\n", err)
		os.Exit(1)
	}
	realConfig := config.Load()
	realConfigDir := realConfig.ConfigDir
	for key, value := range map[string]string{
		"COOP_RUNTIME":        realConfig.RuntimeName,
		"COOP_IMAGE":          realConfig.ImageOverride,
		"COOP_BASE_IMAGE":     realConfig.BaseImage,
		"COOP_HOME_IN_BOX":    realConfig.HomeInBox,
		"COOP_AGENT_PACKAGES": realConfig.AgentPackages,
	} {
		if value != "" {
			coopE2ERuntime = append(coopE2ERuntime, key+"="+value)
		}
	}
	coopE2EConfigDir = filepath.Join(dir, "config")
	if err := prepareLiveConfig(realConfigDir, coopE2EConfigDir); err != nil {
		fmt.Fprintf(os.Stderr, "prepare isolated ACP E2E config: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("COOP_CONFIG_DIR", coopE2EConfigDir); err != nil {
		fmt.Fprintf(os.Stderr, "select isolated ACP E2E config: %v\n", err)
		os.Exit(1)
	}
	coopE2ERepo = filepath.Join(dir, "repo")
	if err := prepareLiveRepo(root, coopE2ERepo); err != nil {
		fmt.Fprintf(os.Stderr, "prepare isolated ACP E2E repo: %v\n", err)
		os.Exit(1)
	}
	coopE2EBinary = filepath.Join(dir, "coop")
	build := exec.Command("go", "build", "-trimpath", "-o", coopE2EBinary, ".")
	build.Dir = root
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "build isolated coop for ACP E2E: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func prepareLiveRepo(sourceRoot, destination string) error {
	source := filepath.Join(sourceRoot, ".agent", "presets", "frontier")
	target := filepath.Join(destination, ".agent", "presets", "frontier")
	if err := filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(target, relative)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	init := exec.Command("git", "init", "-q", destination)
	if output, err := init.CombinedOutput(); err != nil {
		return fmt.Errorf("git init disposable repo: %w (%s)", err, output)
	}
	return nil
}

func prepareLiveConfig(source, destination string) error {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	// Copy only non-secret selector metadata. Live ACP gets credential-only profile overlays below,
	// without granting test agents the user's ambient env secrets, MCP authority, or global instructions.
	for _, name := range []string{"defaults", "models", "pools.json"} {
		src := filepath.Join(source, name)
		data, err := os.ReadFile(src)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", src, err)
		}
		info, err := os.Stat(src)
		if err != nil {
			return fmt.Errorf("stat %s: %w", src, err)
		}
		if err := os.WriteFile(filepath.Join(destination, name), data, info.Mode().Perm()); err != nil {
			return fmt.Errorf("copy %s: %w", src, err)
		}
	}
	if err := copyCredentialEnv(filepath.Join(source, "env"), filepath.Join(destination, "env")); err != nil {
		return err
	}
	credentialFiles := map[string][]string{
		"claude": {".credentials.json"},
		"codex":  {"auth.json"},
		"gemini": {"gemini-credentials.json", "google_accounts.json"},
		"grok":   {"auth.json"},
	}
	for _, provider := range []string{"claude", "codex", "gemini", "grok"} {
		providerDir := filepath.Join(destination, provider)
		if err := os.MkdirAll(providerDir, 0o700); err != nil {
			return err
		}
		profiles := filepath.Join(source, provider, "profiles")
		entries, err := os.ReadDir(profiles)
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("read %s: %w", profiles, err)
		}
		targetProfiles := filepath.Join(providerDir, "profiles")
		if err := os.MkdirAll(targetProfiles, 0o700); err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			targetProfile := filepath.Join(targetProfiles, entry.Name())
			if err := os.MkdirAll(targetProfile, 0o700); err != nil {
				return err
			}
			for _, name := range credentialFiles[provider] {
				src := filepath.Join(profiles, entry.Name(), name)
				data, err := os.ReadFile(src)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return fmt.Errorf("read isolated %s credential: %w", provider, err)
				}
				if err := os.WriteFile(filepath.Join(targetProfile, name), data, 0o600); err != nil {
					return fmt.Errorf("copy isolated %s credential: %w", provider, err)
				}
			}
			if provider == "gemini" {
				if err := copyGeminiAuthSelection(
					filepath.Join(profiles, entry.Name(), "settings.json"),
					filepath.Join(targetProfile, "settings.json"),
				); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func copyGeminiAuthSelection(source, destination string) error {
	data, err := os.ReadFile(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Gemini auth selection: %w", err)
	}
	var settings map[string]any
	if json.Unmarshal(data, &settings) != nil {
		return nil
	}
	security, _ := settings["security"].(map[string]any)
	auth, ok := security["auth"]
	if !ok {
		return nil
	}
	isolated, err := json.Marshal(map[string]any{"security": map[string]any{"auth": auth}})
	if err != nil {
		return fmt.Errorf("encode Gemini auth selection: %w", err)
	}
	return os.WriteFile(destination, append(isolated, '\n'), 0o600)
}

func copyCredentialEnv(source, destination string) error {
	data, err := os.ReadFile(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read credential env: %w", err)
	}
	allowed := map[string]bool{
		"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true, "CLAUDE_CODE_OAUTH_TOKEN": true,
		"OPENAI_API_KEY": true, "GEMINI_API_KEY": true, "GOOGLE_API_KEY": true, "XAI_API_KEY": true,
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, _, ok := strings.Cut(trimmed, "=")
		if ok && allowed[strings.TrimSpace(key)] {
			kept = append(kept, line)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return os.WriteFile(destination, []byte(strings.Join(kept, "\n")+"\n"), 0o600)
}

type liveACP struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	client *acpClient
	stderr *lockedBuffer
	done   chan error
}

func startLiveACP(t *testing.T, provider string) *liveACP {
	t.Helper()
	cmd := exec.Command(coopE2EBinary, "acp", provider)
	cmd.Dir = coopE2ERepo
	cmd.Env = liveACPEnvironment()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("%s stdin pipe: %v", provider, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		t.Fatalf("%s stdout pipe: %v", provider, err)
	}
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		t.Fatalf("start %s ACP: %v", provider, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	c := newACPClient(stdin)
	go c.read(stdout)
	live := &liveACP{cmd: cmd, stdin: stdin, client: c, stderr: stderr, done: done}
	t.Cleanup(func() { live.stop(t) })
	return live
}

func liveACPEnvironment() []string {
	env := make([]string, 0, len(os.Environ())+len(coopE2ERuntime)+10)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(key, "COOP_") || key == "XDG_CONFIG_HOME" {
			continue
		}
		env = append(env, value)
	}
	xdg := filepath.Join(filepath.Dir(coopE2EConfigDir), "xdg")
	env = append(env, coopE2ERuntime...)
	return append(env,
		"COOP_ACP_WARM=0",
		"COOP_CONFIG_DIR="+coopE2EConfigDir,
		"COOP_REPO="+coopE2ERepo,
		"COOP_WORKDIR=",
		"COOP_MCP_FILE="+filepath.Join(coopE2EConfigDir, "mcp.disabled.json"),
		"COOP_CONF="+filepath.Join(coopE2EConfigDir, "coop.disabled.conf"),
		"COOP_HOMES=1",
		"COOP_NETWORK=0",
		"COOP_CACHE=0",
		"COOP_EGRESS=open",
		"COOP_NO_UPDATE_CHECK=1",
		"XDG_CONFIG_HOME="+xdg,
	)
}

func (a *liveACP) stop(t *testing.T) {
	t.Helper()
	_ = a.stdin.Close()
	select {
	case err := <-a.done:
		if err != nil {
			t.Errorf("ACP process exited uncleanly after stdin EOF: %v\nstderr:\n%s", err, a.stderr.String())
		}
		return
	case <-time.After(20 * time.Second):
	}
	_ = syscall.Kill(-a.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case err := <-a.done:
		t.Errorf("ACP process needed SIGTERM after stdin EOF (exit: %v)\nstderr:\n%s", err, a.stderr.String())
		return
	case <-time.After(20 * time.Second):
	}
	_ = syscall.Kill(-a.cmd.Process.Pid, syscall.SIGKILL)
	<-a.done
	t.Errorf("ACP process did not stop after stdin EOF and SIGTERM; stderr:\n%s", a.stderr.String())
}

type liveConfigOption struct {
	ID           string `json:"id"`
	CurrentValue string `json:"currentValue"`
	Options      []struct {
		Value string `json:"value"`
	} `json:"options"`
}

func liveConfigOptions(t *testing.T, response map[string]any) map[string]liveConfigOption {
	t.Helper()
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("ACP response has no object result: %#v", response)
	}
	raw, err := json.Marshal(result["configOptions"])
	if err != nil {
		t.Fatal(err)
	}
	var options []liveConfigOption
	if err := json.Unmarshal(raw, &options); err != nil {
		t.Fatalf("decode configOptions: %v (%s)", err, raw)
	}
	byID := make(map[string]liveConfigOption, len(options))
	for _, option := range options {
		byID[option.ID] = option
	}
	return byID
}

func liveOptionValue(t *testing.T, option liveConfigOption, reject ...string) string {
	t.Helper()
	for _, candidate := range option.Options {
		if candidate.Value != "" && !slices.Contains(reject, candidate.Value) {
			return candidate.Value
		}
	}
	t.Fatalf("option %q has no usable value (rejecting %v): %+v", option.ID, reject, option.Options)
	return ""
}

func setLiveConfig(ctx context.Context, t *testing.T, client *acpClient, sessionID, configID, value string) map[string]liveConfigOption {
	t.Helper()
	response, err := client.req(ctx, "session/set_config_option", map[string]any{
		"sessionId": sessionID,
		"configId":  configID,
		"value":     value,
	})
	if err != nil {
		t.Fatalf("set %s=%s: %v", configID, value, err)
	}
	return liveConfigOptions(t, response)
}

func awaitLiveReplay(t *testing.T, ctx context.Context, live *liveACP, mark int, sessionID string) map[string]liveConfigOption {
	t.Helper()
	for {
		event, next, err := live.client.event(ctx, mark, "session/update")
		if err != nil {
			t.Fatalf("await replay for session %s: %v\nstderr:\n%s\nwire:\n%s", sessionID, err, live.stderr.String(), wireDump(live.client.transcript()))
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
				t.Fatalf("decode replay config update: %v (%s)", err, event.Raw)
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
		t.Fatalf("initialize Codex target E2E: %v\nstderr:\n%s", err, live.stderr.String())
	}
	response, err := live.client.req(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		t.Fatalf("session/new Codex target E2E: %v\nstderr:\n%s", err, live.stderr.String())
	}
	sessionID := responseSessionID(response)
	if sessionID == "" {
		t.Fatalf("session/new returned no session id: %#v", response)
	}
	if _, err := live.client.req(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": "Reply with only OK."}},
	}); err != nil {
		t.Fatalf("Codex target prompt: %v\nstderr:\n%s", err, live.stderr.String())
	}

	rollouts := filepath.Join(config.Load().ConfigDir, "codex", "acp-sessions", "sessions")
	model, effort, err := waitForCodexRolloutTarget(ctx, rollouts, sessionID)
	if err != nil {
		t.Fatalf("read Codex rollout target for %s: %v", sessionID, err)
	}
	if model != wantModel || effort != wantEffort {
		t.Fatalf("Codex rollout target = %s/%s, want %s/%s", model, effort, wantModel, wantEffort)
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
		t.Fatalf("initialize Frontier truth E2E: %v\nstderr:\n%s", err, live.stderr.String())
	}
	response, err := live.client.req(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		t.Fatalf("session/new Frontier truth E2E: %v\nstderr:\n%s", err, live.stderr.String())
	}
	sessionID := responseSessionID(response)
	marker := "FRONTIER_TARGET_" + strings.ReplaceAll(newForeignSessionID(t), "-", "")
	if _, err := live.client.req(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": "Reply with only " + marker + "."}},
	}); err != nil {
		t.Fatalf("Frontier target prompt: %v\nstderr:\n%s\nwire:\n%s", err, live.stderr.String(), wireDump(live.client.transcript()))
	}
	provider, model, effort, err := waitForFrontierStoredTarget(ctx, config.Load().ConfigDir, sessionID, marker)
	if err != nil {
		t.Fatalf("read Frontier provider storage: %v", err)
	}
	switch provider {
	case "claude":
		if model != "claude-fable-5" {
			t.Fatalf("Frontier Claude storage model = %q, want claude-fable-5", model)
		}
	case "codex":
		if model != "gpt-5.6-sol" || effort != "xhigh" {
			t.Fatalf("Frontier Codex storage target = %s/%s, want gpt-5.6-sol/xhigh", model, effort)
		}
	default:
		t.Fatalf("Frontier persisted unexpected provider target %s:%s/%s", provider, model, effort)
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
		t.Fatalf("initialize selector E2E: %v\nstderr:\n%s", err, live.stderr.String())
	}
	response, err := live.client.req(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		t.Fatalf("session/new selector E2E: %v\nstderr:\n%s", err, live.stderr.String())
	}
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("session/new returned no object result: %#v", response)
	}
	sessionID, _ := result["sessionId"].(string)
	if sessionID == "" {
		t.Fatalf("session/new returned no sessionId: %#v", response)
	}
	options := liveConfigOptions(t, response)
	if len(options) != 1 {
		t.Fatalf("initial direct-preset toolbar = %+v, want only coop_preset", options)
	}
	if options["coop_preset"].CurrentValue != "frontier" {
		t.Fatalf("initial preset toolbar is not truthful: %+v", options)
	}
	for _, tc := range []struct {
		configID string
		value    string
	}{
		{"coop_provider", "codex"},
		{"coop_account", "work"},
	} {
		options = setLiveConfig(ctx, t, live.client, sessionID, tc.configID, tc.value)
		if len(options) != 1 || options["coop_preset"].CurrentValue != "frontier" {
			t.Fatalf("active preset %s replay changed toolbar: %+v", tc.configID, options)
		}
	}

	options = setLiveConfig(ctx, t, live.client, sessionID, "coop_preset", "none")
	for _, id := range []string{"coop_preset", "coop_provider", "coop_account"} {
		if _, ok := options[id]; !ok {
			t.Fatalf("leaving preset did not expose normal control %s: %+v", id, options)
		}
	}
	if options["coop_preset"].CurrentValue != "none" || options["coop_account"].CurrentValue != "auto" {
		t.Fatalf("leaving preset did not return to plain provider/automatic account: %+v", options)
	}

	plainProvider := options["coop_provider"].CurrentValue
	for _, option := range options["coop_provider"].Options {
		if option.Value != "" && option.Value != plainProvider {
			plainProvider = option.Value
			break
		}
	}
	if plainProvider != options["coop_provider"].CurrentValue {
		options = setLiveConfig(ctx, t, live.client, sessionID, "coop_provider", plainProvider)
	}
	plainAccount := "auto"
	for _, option := range options["coop_account"].Options {
		if option.Value != "" && option.Value != "auto" {
			plainAccount = option.Value
			break
		}
	}
	if plainAccount != "auto" {
		options = setLiveConfig(ctx, t, live.client, sessionID, "coop_account", plainAccount)
	}
	plainProvider = options["coop_provider"].CurrentValue
	plainAccount = options["coop_account"].CurrentValue

	options = setLiveConfig(ctx, t, live.client, sessionID, "coop_preset", "frontier")
	if len(options) != 1 || options["coop_preset"].CurrentValue != "frontier" {
		t.Fatalf("frontier did not reclaim the toolbar: %+v", options)
	}
	if plainProvider == "" || plainAccount == "" {
		t.Fatalf("plain controls did not expose effective values before re-entering preset: provider=%q account=%q", plainProvider, plainAccount)
	}
}

func TestLiveProviderConformance(t *testing.T) {
	cwd := coopE2ERepo
	for _, tc := range []struct {
		provider     string
		hasLiveModel bool
	}{
		{provider: "claude", hasLiveModel: true},
		{provider: "codex", hasLiveModel: true},
		{provider: "gemini", hasLiveModel: true},
		{provider: "grok", hasLiveModel: true},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			live := startLiveACP(t, tc.provider)
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
			defer cancel()
			initialize, err := live.client.req(ctx, "initialize", map[string]any{
				"protocolVersion": 1,
				"clientCapabilities": map[string]any{
					"fs": map[string]any{"readTextFile": true, "writeTextFile": true},
				},
			})
			if err != nil {
				t.Fatalf("initialize %s: %v\nstderr:\n%s", tc.provider, err, live.stderr.String())
			}
			assertEditorAuthenticationHidden(t, initialize)
			response, err := live.client.req(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
			if err != nil {
				t.Fatalf("session/new %s: %v\nstderr:\n%s", tc.provider, err, live.stderr.String())
			}
			sessionID := responseSessionID(response)
			if sessionID == "" {
				t.Fatalf("%s session/new returned no session id: %#v", tc.provider, response)
			}
			options := liveConfigOptions(t, response)
			for _, id := range []string{"coop_preset", "coop_provider", "coop_account"} {
				if _, ok := options[id]; !ok {
					t.Fatalf("%s toolbar is missing %s: %+v", tc.provider, id, options)
				}
			}
			firstMark := live.client.mark()
			if _, err := live.client.req(ctx, "session/prompt", map[string]any{
				"sessionId": sessionID,
				"prompt":    []any{map[string]any{"type": "text", "text": "Reply with only LIVE_ONE_OK."}},
			}); err != nil {
				t.Fatalf("first %s prompt: %v\nstderr:\n%s\nwire:\n%s", tc.provider, err, live.stderr.String(), wireDump(live.client.transcript()))
			}
			if got := liveAssistantText(live.client.transcript()[firstMark:]); !strings.Contains(got, "LIVE_ONE_OK") {
				t.Fatalf("first %s assistant text = %q, want LIVE_ONE_OK", tc.provider, got)
			}

			modelOption, hasModel := options["model"]
			if hasModel != tc.hasLiveModel {
				t.Fatalf("%s live model capability = %v, want %v: %+v", tc.provider, hasModel, tc.hasLiveModel, options)
			}
			selectedModel := ""
			initialModel := modelOption.CurrentValue
			if hasModel {
				nextModel := liveOptionValue(t, modelOption, modelOption.CurrentValue)
				modelChangeMark := live.client.mark()
				options = setLiveConfig(ctx, t, live.client, sessionID, "model", nextModel)
				if options["model"].CurrentValue != nextModel {
					t.Fatalf("%s model after live change = %q, want %q", tc.provider, options["model"].CurrentValue, nextModel)
				}
				selectedModel = nextModel
				if tc.provider == "grok" {
					// Grok's alternate model belongs to another agent type. The adapter explicitly asks
					// for a new session; wait for Coop's transparent migration before exercising the
					// separate SIGHUP replay below.
					options = awaitLiveReplay(t, ctx, live, modelChangeMark, sessionID)
					if options["model"].CurrentValue != nextModel {
						t.Fatalf("%s model after agent migration = %q, want %q", tc.provider, options["model"].CurrentValue, nextModel)
					}
				}
			}

			replayMark := live.client.mark()
			if err := live.cmd.Process.Signal(syscall.SIGHUP); err != nil {
				t.Fatalf("SIGHUP %s ACP supervisor: %v", tc.provider, err)
			}
			replayOptions := awaitLiveReplay(t, ctx, live, replayMark, sessionID)
			if selectedModel != "" && replayOptions["model"].CurrentValue != selectedModel {
				t.Fatalf("%s model after replay = %q, want %q", tc.provider, replayOptions["model"].CurrentValue, selectedModel)
			}
			secondMark := live.client.mark()
			if _, err := live.client.req(ctx, "session/prompt", map[string]any{
				"sessionId": sessionID,
				"prompt":    []any{map[string]any{"type": "text", "text": "Reply with only LIVE_TWO_OK."}},
			}); err != nil {
				t.Fatalf("resumed %s prompt on original session %s: %v\nstderr:\n%s\nwire:\n%s", tc.provider, sessionID, err, live.stderr.String(), wireDump(live.client.transcript()))
			}
			got := liveAssistantText(live.client.transcript()[secondMark:])
			if !strings.Contains(got, "LIVE_TWO_OK") && tc.provider == "codex" && initialModel != "" &&
				strings.Contains(strings.ToLower(got), "selected model is at capacity") {
				t.Logf("selected Codex model %s is at capacity; retrying the resumed thread on proven model %s", selectedModel, initialModel)
				fallback := setLiveConfig(ctx, t, live.client, sessionID, "model", initialModel)
				if fallback["model"].CurrentValue != initialModel {
					t.Fatalf("Codex capacity fallback model = %q, want %q", fallback["model"].CurrentValue, initialModel)
				}
				secondMark = live.client.mark()
				if _, err := live.client.req(ctx, "session/prompt", map[string]any{
					"sessionId": sessionID,
					"prompt":    []any{map[string]any{"type": "text", "text": "Reply with only LIVE_TWO_OK."}},
				}); err != nil {
					t.Fatalf("Codex prompt after capacity fallback: %v\nstderr:\n%s\nwire:\n%s", err, live.stderr.String(), wireDump(live.client.transcript()))
				}
				got = liveAssistantText(live.client.transcript()[secondMark:])
			}
			if !strings.Contains(got, "LIVE_TWO_OK") {
				t.Fatalf("resumed %s assistant text = %q, want LIVE_TWO_OK", tc.provider, got)
			}
			if tc.provider == "grok" && selectedModel != "" {
				raw := wireDump(live.client.transcript()[secondMark:])
				if !strings.Contains(raw, `"modelId":"`+selectedModel+`"`) {
					t.Fatalf("Grok runtime did not report selected model %q after migration + replay:\n%s", selectedModel, raw)
				}
			}
		})
	}
}

func TestLiveCrossProviderCarry(t *testing.T) {
	for _, destination := range []string{"codex", "grok"} {
		t.Run(destination, func(t *testing.T) {
			cwd := coopE2ERepo
			live := startLiveACP(t, "claude")
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()
			if _, err := live.client.req(ctx, "initialize", map[string]any{
				"protocolVersion": 1,
				"clientCapabilities": map[string]any{
					"fs": map[string]any{"readTextFile": true, "writeTextFile": true},
				},
			}); err != nil {
				t.Fatalf("initialize live cross-provider session: %v\nstderr:\n%s", err, live.stderr.String())
			}
			response, err := live.client.req(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
			if err != nil {
				t.Fatalf("session/new live cross-provider session: %v\nstderr:\n%s", err, live.stderr.String())
			}
			sessionID := responseSessionID(response)
			token := "COOP_CARRY_LIVE_" + strings.ToUpper(destination)
			if _, err := live.client.req(ctx, "session/prompt", map[string]any{
				"sessionId": sessionID,
				"prompt":    []any{map[string]any{"type": "text", "text": "Remember the token " + token + " and reply only SOURCE_OK."}},
			}); err != nil {
				t.Fatalf("source live prompt: %v\nstderr:\n%s", err, live.stderr.String())
			}
			options := liveConfigOptions(t, response)
			provider := options["coop_provider"]
			found := false
			for _, option := range provider.Options {
				found = found || option.Value == destination
			}
			if !found {
				t.Fatalf("Claude toolbar cannot switch to signed-in %s: %+v", destination, provider)
			}
			mark := live.client.mark()
			options = setLiveConfig(ctx, t, live.client, sessionID, "coop_provider", destination)
			if options["coop_provider"].CurrentValue != destination {
				t.Fatalf("provider switch ack = %+v, want %s", options["coop_provider"], destination)
			}
			destinationMark := live.client.mark()
			if _, err := live.client.req(ctx, "session/prompt", map[string]any{
				"sessionId": sessionID,
				"prompt":    []any{map[string]any{"type": "text", "text": "Reply with only the token you were asked to remember in the carried conversation."}},
			}); err != nil {
				t.Fatalf("destination live prompt: %v\nstderr:\n%s\nwire:\n%s", err, live.stderr.String(), wireDump(live.client.transcript()))
			}
			if got := liveAssistantText(live.client.transcript()[destinationMark:]); !strings.Contains(got, token) {
				t.Fatalf("%s did not receive carried token, assistant text = %q\nwire:\n%s", destination, got, wireDump(live.client.transcript()))
			}
			for _, frame := range live.client.transcript()[mark:] {
				if frame.Msg["method"] == "session/update" && strings.Contains(string(frame.Raw), "[coop] This thread continues") {
					t.Fatalf("synthetic live carry leaked back from %s: %s", destination, frame.Raw)
				}
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
				t.Fatalf("initialize %s ACP: %v\nstderr:\n%s", tc.provider, err, live.stderr.String())
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
				t.Fatalf("%s loaded foreign session %s successfully; want JSON-RPC error", tc.provider, foreignID)
			case errors.As(err, &rpcError):
				if elapsed >= loadTimeout {
					t.Fatalf("%s rejected the foreign session after %s; want under %s", tc.provider, elapsed, loadTimeout)
				}
				if rpcError.code != tc.code {
					t.Fatalf("%s foreign-session error code = %d, want %d: %s", tc.provider, rpcError.code, tc.code, rpcError)
				}
				matched := false
				for _, marker := range tc.markers {
					matched = matched || strings.Contains(rpcError.raw, marker)
				}
				if !matched {
					t.Fatalf("%s error does not identify an unknown session (want one of %q): %s", tc.provider, tc.markers, rpcError)
				}
				for _, marker := range foreignMarker(tc.provider, foreignID) {
					if !strings.Contains(rpcError.raw, marker) {
						t.Fatalf("%s error does not identify an unknown session (missing %q): %s", tc.provider, marker, rpcError)
					}
				}
				t.Logf("%s rejected foreign session in %s: %s", tc.provider, elapsed.Round(time.Millisecond), rpcError)
			default:
				t.Fatalf("%s did not reject foreign session with a JSON-RPC error after %s: %v\nstderr:\n%s", tc.provider, elapsed.Round(time.Millisecond), err, live.stderr.String())
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
