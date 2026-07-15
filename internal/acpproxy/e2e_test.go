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

var coopE2EBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "coop-acp-e2e-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create ACP E2E temp dir: %v\n", err)
		os.Exit(1)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve repository root: %v\n", err)
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
	cmd.Env = append(os.Environ(), "COOP_ACP_WARM=0")
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

// newSession opens a session with a cwd that exists *inside the box*. coop mounts the
// repo at its real host path, so the test's own dir works; a host temp dir would not.
func newSession(ctx context.Context, c *acpClient, cwd string) error {
	_, err := c.req(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	return err
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

func TestCodexTargetRolloutTruth(t *testing.T) {
	const (
		wantModel  = "gpt-5.6-sol"
		wantEffort = "xhigh"
	)
	live := startLiveACP(t, "codex:"+wantModel+"/"+wantEffort)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
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
		var model, effort string
		walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() || !strings.Contains(info.Name(), sessionID) {
				return err
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()
			decoder := json.NewDecoder(file)
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
					return nil
				}
			}
			return nil
		})
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

// TestPresetOwnsSelectorState drives coop's real outer ACP control layer. A direct preset owns the
// toolbar and ignores persisted plain selector writes; leaving it exposes the normal plain controls.
func TestPresetOwnsSelectorState(t *testing.T) {
	live := startLiveACP(t, "frontier")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
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

func TestSuperviseResume(t *testing.T) {
	live := startLiveACP(t, "claude")
	c := live.client

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cwd, _ := os.Getwd() // inside the mounted repo, so it exists in the box

	// initialize + session/new prove the box is up and authenticated.
	if _, err := c.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": true, "writeTextFile": true}}}); err != nil {
		t.Skipf("initialize failed (box not built / not signed in?): %v", err)
	}
	if err := newSession(ctx, c, cwd); err != nil {
		t.Fatalf("session/new before reload (auth?): %v", err)
	}

	// Reload exactly this supervisor. It re-execs in place and restores the proxy
	// snapshot while keeping the editor's stdio connection open.
	if err := live.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("SIGHUP ACP supervisor: %v", err)
	}

	// The supervisor should respawn + re-auth; session/new must succeed again while the
	// new box spins up.
	resumed := false
	var lastErr error
	for i := 0; i < 20 && ctx.Err() == nil; i++ {
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, 10*time.Second)
		lastErr = newSession(attemptCtx, c, cwd)
		cancelAttempt()
		if lastErr == nil {
			resumed = true
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !resumed {
		t.Fatalf("session/new never succeeded after supervisor reload (last error: %v)\nstderr:\n%s\nwire:\n%s", lastErr, live.stderr.String(), wireDump(c.transcript()))
	}

	// Cleanup closes stdin; the supervisor owns label-scoped box teardown.
}

func TestForeignSessionLoadRejectsUnknownID(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	const loadTimeout = 10 * time.Second
	cases := []struct {
		provider string
		code     int
		markers  []string
	}{
		{provider: "claude", code: -32002, markers: []string{"Resource not found"}},
		{provider: "codex", code: -32603, markers: []string{"no rollout found for thread id"}},
		{provider: "gemini", code: -32603, markers: []string{"No previous sessions found for this project"}},
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
				for _, marker := range append(tc.markers, foreignMarker(tc.provider, foreignID)...) {
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
