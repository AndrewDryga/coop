package acpproxy_test

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

type scriptedACP struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	client *acpClient
	stderr *lockedBuffer
	done   chan error
}

func TestScriptedACPDriver(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	coopBin := filepath.Join(tmp, "coop")
	fixtureBin := filepath.Join(tmp, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")

	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(tmp, "plan.json")
	plan := `{
  "providers": {
    "claude": [[
      {"method":"initialize","result":{"protocolVersion":1,"agentCapabilities":{},"authMethods":[]}},
      {"method":"session/new","result":{"sessionId":"N1","configOptions":[]},"events":[{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"N1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"fixture ready"}}}}]},
      {"method":"session/set_config_option","result":{"configOptions":[]}},
      {"method":"session/prompt","error":{"code":-32603,"message":"fixture failure","data":{"type":"fixture_error"}}}
    ]]
  }
}`
	if err := os.WriteFile(planPath, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}

	proc := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, planPath, "claude", "claude")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := proc.client.req(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
	}); err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	mark := proc.client.mark()
	response, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}})
	if err != nil {
		t.Fatalf("session/new: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	if sid := responseSessionID(response); sid != "N1" {
		t.Fatalf("session/new id = %q, want N1: %#v", sid, response)
	}
	event, _, err := proc.client.event(ctx, mark, "session/update")
	if err != nil {
		t.Fatalf("await session/update: %v\nwire:\n%s", err, wireDump(proc.client.transcript()))
	}
	if !strings.Contains(string(event.Raw), "fixture ready") {
		t.Fatalf("session/update lost content: %s", event.Raw)
	}
	_, err = proc.client.req(ctx, "session/prompt", map[string]any{
		"sessionId": "N1",
		"prompt":    []any{map[string]any{"type": "text", "text": "hello"}},
	})
	var rpcError *rpcErr
	if !errors.As(err, &rpcError) || rpcError.code != -32603 || !strings.Contains(rpcError.raw, "fixture_error") {
		t.Fatalf("prompt error = %v, want structured fixture error\nwire:\n%s", err, wireDump(proc.client.transcript()))
	}

	transcript, err := os.ReadFile(filepath.Join(tmp, "fixture-state", "claude-0", "wire.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"initialize", "session/new", "session/set_config_option", "session/prompt"} {
		if !strings.Contains(string(transcript), `"method":"`+method+`"`) {
			t.Fatalf("fixture transcript is missing %s:\n%s", method, transcript)
		}
	}
}

func TestScriptedACPPresetTarget(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	coopBin := filepath.Join(tmp, "coop")
	fixtureBin := filepath.Join(tmp, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")

	repo := filepath.Join(tmp, "repo")
	presetDir := filepath.Join(repo, ".agent", "presets", "target-test")
	if err := os.MkdirAll(presetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presetDir, "preset.yaml"), []byte("lead:\n  agent: codex:gpt-5.6-sol/xhigh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(tmp, "plan.json")
	plan := `{
  "providers": {
    "codex": [[
      {"method":"initialize","result":{"protocolVersion":1,"agentCapabilities":{},"authMethods":[]}},
      {"method":"session/new","result":{"sessionId":"C1","configOptions":[{"id":"model","type":"select","currentValue":"default","options":[]},{"id":"reasoning_effort","type":"select","currentValue":"low","options":[]}]}},
      {"method":"session/set_config_option","result":{"configOptions":[]}},
      {"method":"session/set_config_option","result":{"configOptions":[]}},
      {"method":"session/prompt","events":[{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"C1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"target applied"}}}}],"result":{"stopReason":"end_turn"}}
    ]]
  }
}`
	if err := os.WriteFile(planPath, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}

	proc := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, planPath, "target-test", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	response, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}})
	if err != nil {
		t.Fatalf("session/new: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	if sid := responseSessionID(response); sid != "C1" {
		t.Fatalf("session/new id = %q, want C1", sid)
	}
	mark := proc.client.mark()
	if _, err := proc.client.req(ctx, "session/prompt", map[string]any{
		"sessionId": "C1",
		"prompt":    []any{map[string]any{"type": "text", "text": "identify target"}},
	}); err != nil {
		t.Fatalf("session/prompt: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	if event, _, err := proc.client.event(ctx, mark, "session/update"); err != nil || !strings.Contains(string(event.Raw), "target applied") {
		t.Fatalf("target confirmation event = %s, %v", event.Raw, err)
	}

	wire, err := os.ReadFile(filepath.Join(tmp, "fixture-state", "codex-0", "wire.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	model := `"configId":"model","sessionId":"C1","value":"gpt-5.6-sol"`
	effort := `"configId":"reasoning_effort","sessionId":"C1","value":"xhigh"`
	modelAt, effortAt, promptAt := strings.Index(string(wire), model), strings.Index(string(wire), effort), strings.LastIndex(string(wire), `"method":"session/prompt"`)
	if modelAt < 0 || effortAt <= modelAt || promptAt <= effortAt {
		t.Fatalf("preset target was not applied model then effort before prompt:\n%s", wire)
	}
}

func buildTestBinary(t *testing.T, root, output, pkg string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-trimpath", "-o", output, pkg)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, out)
	}
}

func startScriptedACP(t *testing.T, coopBin, fixtureBin, repo, tmp, plan, target string, providers ...string) *scriptedACP {
	t.Helper()
	configDir := filepath.Join(tmp, "config")
	for _, provider := range providers {
		profileDir := filepath.Join(configDir, provider, "profiles", "default")
		if err := os.MkdirAll(profileDir, 0o700); err != nil {
			t.Fatal(err)
		}
		ag, ok := agents.Get(provider)
		if !ok {
			t.Fatalf("unknown scripted ACP provider %q", provider)
		}
		marker, _ := ag.AuthMarker()
		if err := os.WriteFile(filepath.Join(profileDir, marker), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(coopBin, "acp", target)
	cmd.Env = testEnv(os.Environ(), map[string]string{
		"HOME":                   filepath.Join(tmp, "home"),
		"XDG_CONFIG_HOME":        filepath.Join(tmp, "xdg"),
		"COOP_CONF":              filepath.Join(tmp, "missing.conf"),
		"COOP_CONFIG_DIR":        configDir,
		"COOP_REPO":              repo,
		"COOP_RUNTIME":           fixtureBin,
		"COOP_IMAGE":             "fixture-image",
		"COOP_HOMES":             "0",
		"COOP_NETWORK":           "0",
		"COOP_AUTO_UP":           "0",
		"COOP_CACHE":             "0",
		"COOP_CAFFEINATE":        "0",
		"COOP_EGRESS":            "none",
		"COOP_ACP_WARM":          "0",
		"COOP_NO_UPDATE_CHECK":   "1",
		"COOP_ACP_FIXTURE_PLAN":  plan,
		"COOP_ACP_FIXTURE_STATE": filepath.Join(tmp, "fixture-state"),
	})
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	client := newACPClient(stdin)
	go client.read(stdout)
	proc := &scriptedACP{cmd: cmd, stdin: stdin, client: client, stderr: stderr, done: done}
	t.Cleanup(func() { proc.stop(t) })
	return proc
}

func (p *scriptedACP) stop(t *testing.T) {
	t.Helper()
	_ = p.stdin.Close()
	select {
	case err := <-p.done:
		if err != nil {
			t.Errorf("scripted ACP exited uncleanly: %v\nstderr:\n%s", err, p.stderr.String())
		}
		return
	case <-time.After(5 * time.Second):
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case err := <-p.done:
		t.Errorf("scripted ACP needed SIGTERM after stdin EOF (exit: %v)\nstderr:\n%s", err, p.stderr.String())
		return
	case <-time.After(2 * time.Second):
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	<-p.done
	t.Errorf("scripted ACP needed SIGKILL after stdin EOF\nstderr:\n%s", p.stderr.String())
}

func responseSessionID(response map[string]any) string {
	result, _ := response["result"].(map[string]any)
	sid, _ := result["sessionId"].(string)
	return sid
}

func wireDump(frames []wireFrame) string {
	var b strings.Builder
	for _, frame := range frames {
		b.Write(frame.Raw)
		if len(frame.Raw) == 0 || frame.Raw[len(frame.Raw)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func testEnv(base []string, values map[string]string) []string {
	out := append([]string(nil), base...)
	for key, value := range values {
		prefix := key + "="
		item := prefix + value
		replaced := false
		for i := range out {
			if strings.HasPrefix(out[i], prefix) {
				out[i] = item
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, item)
		}
	}
	return out
}
