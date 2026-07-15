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

func TestScriptedACPCarryAcrossProviderSwitches(t *testing.T) {
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
      {"method":"session/new","result":{"sessionId":"S1","configOptions":[]}},
      {"method":"session/set_config_option","result":{"configOptions":[]}},
      {"method":"session/prompt","events":[{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"claude answer"}}}}],"result":{"stopReason":"end_turn"}}
    ]],
    "codex": [[
      {"method":"initialize","result":{"protocolVersion":1,"agentCapabilities":{},"authMethods":[]}},
      {"method":"session/new","result":{"sessionId":"C2","configOptions":[{"id":"fixture","name":"Fixture","type":"select","currentValue":"ready","options":[]}]}},
      {"method":"session/prompt","events":[{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"C2","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"codex answer"}}}}],"result":{"stopReason":"end_turn"}}
    ]],
    "grok": [[
      {"method":"initialize","result":{"protocolVersion":1,"agentCapabilities":{},"authMethods":[]}},
      {"method":"session/new","result":{"sessionId":"G3","configOptions":[{"id":"fixture","name":"Fixture","type":"select","currentValue":"ready","options":[]}]}},
      {"method":"session/prompt","events":[{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"G3","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"grok answer"}}}}],"result":{"stopReason":"end_turn"}}
    ]]
  }
}`
	if err := os.WriteFile(planPath, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}

	proc := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, planPath, "claude", "claude", "codex", "grok")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	response, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}})
	if err != nil {
		t.Fatalf("session/new: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	sid := responseSessionID(response)
	if sid != "S1" {
		t.Fatalf("session/new id = %q, want S1", sid)
	}
	promptScripted(t, ctx, proc, sid, "first question", "claude answer")
	switchScriptedProvider(t, ctx, proc, sid, "codex")
	promptScripted(t, ctx, proc, sid, "second question", "codex answer")
	switchScriptedProvider(t, ctx, proc, sid, "grok")
	promptScripted(t, ctx, proc, sid, "third question", "grok answer")

	assertCarryWire := func(provider string, wants []string) {
		t.Helper()
		wire, err := os.ReadFile(filepath.Join(tmp, "fixture-state", provider+"-0", "wire.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		text := string(wire)
		if strings.Contains(text, `"method":"session/load"`) {
			t.Errorf("%s received a foreign session/load instead of a direct session/new:\n%s", provider, wire)
		}
		if got := strings.Count(text, "[coop] This thread continues"); got != 1 {
			t.Errorf("%s wire has %d carry wrappers, want one:\n%s", provider, got, wire)
		}
		for _, want := range wants {
			if got := strings.Count(text, want); got != 1 {
				t.Errorf("%s wire contains %q %d times, want once:\n%s", provider, want, got, wire)
			}
		}
	}
	assertCarryWire("codex", []string{"first question", "claude answer", "second question", "[provider] claude"})
	assertCarryWire("grok", []string{"first question", "claude answer", "second question", "codex answer", "third question", "[provider] claude", "[provider] codex"})

	for _, frame := range proc.client.transcript() {
		if frame.Msg["method"] == "session/update" && strings.Contains(string(frame.Raw), "[coop] This thread continues") {
			t.Errorf("synthetic carry leaked to the editor in session/update:\n%s", frame.Raw)
		}
	}
}

func TestScriptedACPSessionIdentityLifecycle(t *testing.T) {
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
    "claude": [
      [
        {"method":"initialize","result":{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"load":{},"close":{},"delete":{}}},"authMethods":[]}},
        {"method":"session/new","result":{"sessionId":"A1","configOptions":[]}},
        {"method":"session/set_config_option","result":{"configOptions":[]}},
        {"method":"session/new","result":{"sessionId":"A2","configOptions":[]}},
        {"method":"session/set_config_option","result":{"configOptions":[]}},
        {"method":"session/new","result":{"sessionId":"A3","configOptions":[]}},
        {"method":"session/set_config_option","result":{"configOptions":[]}},
        {"method":"session/prompt","result":{"stopReason":"end_turn"}},
        {"method":"session/close","result":{}},
        {"method":"session/delete","result":{}}
      ],
      [
        {"method":"initialize","result":{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"load":{},"close":{},"delete":{}}},"authMethods":[]}},
        {"method":"session/load","result":{"configOptions":[{"id":"fixture","name":"Fixture","type":"select","currentValue":"reloaded","options":[]}]}},
        {"method":"session/set_config_option","result":{"configOptions":[]}}
      ]
    ],
    "codex": [[
      {"method":"initialize","result":{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"load":{},"close":{},"delete":{}}},"authMethods":[]}},
      {"method":"session/new","result":{"sessionId":"C3","configOptions":[{"id":"fixture","name":"Fixture","type":"select","currentValue":"recreated","options":[]}]}}
    ]]
  }
}`
	if err := os.WriteFile(planPath, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	proc := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, planPath, "claude", "claude", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	newSession := func(cwd string) string {
		t.Helper()
		response, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
		if err != nil {
			t.Fatalf("session/new %s: %v\nstderr:\n%s", cwd, err, proc.stderr.String())
		}
		return responseSessionID(response)
	}
	closedID := newSession(filepath.Join(repo, "closed"))
	deletedID := newSession(filepath.Join(repo, "deleted"))
	liveID := newSession(filepath.Join(repo, "live"))
	if closedID != "A1" || deletedID != "A2" || liveID != "A3" {
		t.Fatalf("fixture session ids = %q/%q/%q, want A1/A2/A3", closedID, deletedID, liveID)
	}
	if _, err := proc.client.req(ctx, "session/prompt", map[string]any{"sessionId": liveID, "prompt": []any{}}); err != nil {
		t.Fatalf("establish live transcript: %v", err)
	}
	if _, err := proc.client.req(ctx, "session/close", map[string]any{"sessionId": closedID}); err != nil {
		t.Fatalf("session/close: %v", err)
	}
	if _, err := proc.client.req(ctx, "session/delete", map[string]any{"sessionId": deletedID}); err != nil {
		t.Fatalf("session/delete: %v", err)
	}

	mark := proc.client.mark()
	if err := proc.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("SIGHUP scripted supervisor: %v", err)
	}
	for {
		event, next, err := proc.client.event(ctx, mark, "session/update")
		if err != nil {
			t.Fatalf("await same-provider reload: %v\nstderr:\n%s\nwire:\n%s", err, proc.stderr.String(), wireDump(proc.client.transcript()))
		}
		mark = next
		if strings.Contains(string(event.Raw), `"currentValue":"reloaded"`) {
			break
		}
	}
	reloadWire, err := os.ReadFile(filepath.Join(tmp, "fixture-state", "claude-1", "wire.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	reloadText := string(reloadWire)
	if strings.Count(reloadText, `"method":"session/load"`) != 1 || !strings.Contains(reloadText, `"sessionId":"A3"`) ||
		strings.Contains(reloadText, `"sessionId":"A1"`) || strings.Contains(reloadText, `"sessionId":"A2"`) {
		t.Fatalf("same-provider reload did not preserve only live native A3:\n%s", reloadWire)
	}

	switchScriptedProvider(t, ctx, proc, liveID, "codex")
	codexWire, err := os.ReadFile(filepath.Join(tmp, "fixture-state", "codex-0", "wire.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	codexText := string(codexWire)
	if strings.Contains(codexText, `"method":"session/load"`) || strings.Count(codexText, `"method":"session/new"`) != 1 ||
		strings.Contains(codexText, `"sessionId":"A3"`) {
		t.Fatalf("cross-provider replay probed Claude identity instead of creating once:\n%s", codexWire)
	}
}

func promptScripted(t *testing.T, ctx context.Context, proc *scriptedACP, sid, prompt, answer string) {
	t.Helper()
	mark := proc.client.mark()
	if _, err := proc.client.req(ctx, "session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": prompt}},
	}); err != nil {
		t.Fatalf("session/prompt %q: %v\nstderr:\n%s\nwire:\n%s", prompt, err, proc.stderr.String(), wireDump(proc.client.transcript()))
	}
	event, _, err := proc.client.event(ctx, mark, "session/update")
	if err != nil || !strings.Contains(string(event.Raw), answer) {
		t.Fatalf("prompt %q event = %s, %v\nwire:\n%s", prompt, event.Raw, err, wireDump(proc.client.transcript()))
	}
}

func switchScriptedProvider(t *testing.T, ctx context.Context, proc *scriptedACP, sid, provider string) {
	t.Helper()
	mark := proc.client.mark()
	if _, err := proc.client.req(ctx, "session/set_config_option", map[string]any{
		"sessionId": sid,
		"configId":  "coop_provider",
		"value":     provider,
	}); err != nil {
		t.Fatalf("switch to %s: %v\nstderr:\n%s", provider, err, proc.stderr.String())
	}
	for {
		event, next, err := proc.client.event(ctx, mark, "session/update")
		if err != nil {
			t.Fatalf("await %s replay: %v\nstderr:\n%s\nwire:\n%s", provider, err, proc.stderr.String(), wireDump(proc.client.transcript()))
		}
		mark = next
		if strings.Contains(string(event.Raw), `"sessionUpdate":"config_option_update"`) {
			return
		}
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
