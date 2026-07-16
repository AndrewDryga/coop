package agent

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
)

// cleanCmdEnv unsets the per-agent command and model overrides so the defaults are exercised.
func cleanCmdEnv(t *testing.T) {
	t.Helper()
	for _, e := range []string{
		"COOP_CLAUDE_CMD", "COOP_CODEX_CMD", "COOP_GEMINI_CMD", "COOP_GROK_CMD",
		"COOP_CLAUDE_MODEL", "COOP_CODEX_MODEL", "COOP_GEMINI_MODEL", "COOP_GROK_MODEL",
	} {
		if v, ok := os.LookupEnv(e); ok {
			os.Unsetenv(e)
			t.Cleanup(func() { os.Setenv(e, v) })
		}
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustLiveCredentials(t *testing.T, agent Agent) LiveCredentialSpec {
	t.Helper()
	return agent.LiveCredentials()
}

func TestRegistry(t *testing.T) {
	names := Names()
	if !slices.IsSorted(names) {
		t.Errorf("Names() = %v, want sorted registry order", names)
	}
	for _, name := range []string{"claude", "codex", "gemini", "grok"} {
		if !slices.Contains(names, name) {
			t.Errorf("Names() = %v, missing supported provider %s", names, name)
		}
	}
	if !Valid("codex") || Valid("nope") {
		t.Error("Valid: codex should be valid, nope should not")
	}
	if _, ok := Get("gemini"); !ok {
		t.Error("Get(gemini) missing")
	}
	if _, ok := Get("nope"); ok {
		t.Error("Get(nope) should be absent")
	}
	// Every agent carries a human product name for the UX surfaces (the ACP dropdowns) —
	// distinct from its grammar token, never empty.
	for _, n := range Names() {
		a, _ := Get(n)
		if d := a.DisplayName(); d == "" || d == n {
			t.Errorf("%s: DisplayName() = %q, want a human product name (e.g. Codex)", n, d)
		}
	}
	// Packages is the union across agents (claude 2 + codex 2 + gemini 1; grok is a native
	// binary, not npm, so it adds none).
	if got := Packages(); !slices.Contains(got, claudeCLIPackage) ||
		!slices.Contains(got, claudeACPPackage) ||
		!slices.Contains(got, codexCLIPackage) ||
		!slices.Contains(got, codexACPPackage) ||
		!slices.Contains(got, geminiCLIPackage) {
		t.Errorf("Packages() = %v", got)
	}
}

func TestCommands(t *testing.T) {
	cleanCmdEnv(t)
	cfg := &config.Config{} // no mcp.json → no --mcp-config
	cases := []struct {
		name                              string
		interactive, headless, acp, csult []string
	}{
		{"claude",
			[]string{"claude", "--dangerously-skip-permissions"},
			[]string{"claude", "--dangerously-skip-permissions", "-p", "go"},
			[]string{"claude-agent-acp"},
			[]string{"claude", "-p", "--permission-mode", "plan", "q"}},
		{"codex",
			[]string{"codex", "--dangerously-bypass-approvals-and-sandbox"},
			[]string{"codex", "exec", "--dangerously-bypass-approvals-and-sandbox", "go"},
			[]string{"codex-acp"},
			[]string{"codex", "exec", "-s", "read-only", "q"}},
		{"gemini",
			[]string{"gemini", "--yolo"},
			[]string{"gemini", "--yolo", "-p", "go"},
			[]string{"gemini", "--acp"},
			[]string{"gemini", "--approval-mode", "plan", "-p", "q"}},
		{"grok",
			[]string{"grok", "--permission-mode", "bypassPermissions"},
			// -p takes the prompt as its value, so the prompt is last, after the mode flags.
			[]string{"grok", "--permission-mode", "bypassPermissions", "-p", "go"},
			[]string{"grok", "agent", "stdio"},
			// read-only via the tool allowlist, NOT --permission-mode plan (a no-op in headless).
			[]string{"grok", "--tools", "read_file,grep,list_dir", "-p", "q"}},
	}
	for _, c := range cases {
		a, _ := Get(c.name)
		if got := a.Interactive(cfg); !slices.Equal(got, c.interactive) {
			t.Errorf("%s Interactive = %v", c.name, got)
		}
		if got := a.Headless(cfg, "go"); !slices.Equal(got, c.headless) {
			t.Errorf("%s Headless = %v", c.name, got)
		}
		if got := a.ACP(cfg); !slices.Equal(got, c.acp) {
			t.Errorf("%s ACP = %v", c.name, got)
		}
		if got := a.ConsultCmd("q"); !slices.Equal(got, c.csult) {
			t.Errorf("%s ConsultCmd = %v", c.name, got)
		}
	}
}

func TestStreamSpecs(t *testing.T) {
	cases := []struct {
		name string
		want StreamSpec
	}{
		{"claude", StreamSpec{Format: StreamClaudeJSON, Flags: []string{"--output-format", "stream-json", "--verbose"}}},
		{"codex", StreamSpec{Format: StreamCodexJSON, Flags: []string{"--json"}, TrailingArgs: 1}},
		{"gemini", StreamSpec{Format: StreamGeminiJSON, Flags: []string{"-o", "stream-json"}, TrailingArgs: 2}},
		{"grok", StreamSpec{Format: StreamGrokJSON, Flags: []string{"--output-format", "streaming-json"}, TrailingArgs: 2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, _ := Get(c.name)
			got := a.Stream()
			if got.Format != c.want.Format || got.TrailingArgs != c.want.TrailingArgs || !slices.Equal(got.Flags, c.want.Flags) {
				t.Errorf("Stream() = %#v, want %#v", got, c.want)
			}
		})
	}
}

// TestModelSelection: a resolved model rides every command form as a --model flag, replaces
// a COOP_<AGENT>_CMD baked default without duplicates, and every agent answers Models()
// (non-empty menu for `coop models`).
func TestModelSelection(t *testing.T) {
	cleanCmdEnv(t)
	cfg := &config.Config{}
	cfg.SetActiveModel("claude", "opus")
	cfg.SetActiveModel("codex", "gpt-5")
	cfg.SetActiveModel("gemini", "gemini-2.5-pro")
	cfg.SetActiveModel("grok", "grok-4.5")
	cases := []struct {
		name                 string
		interactive, acp     []string
		headlessHasModelFlag bool
	}{
		{"claude", []string{"claude", "--dangerously-skip-permissions", "--model", "opus"}, []string{"claude-agent-acp"}, true},
		{"codex", []string{"codex", "--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-5"}, []string{"codex-acp"}, true},
		// gemini's ACP is its own binary, so the model rides the ACP command too.
		{"gemini", []string{"gemini", "--yolo", "--model", "gemini-2.5-pro"}, []string{"gemini", "--acp", "--model", "gemini-2.5-pro"}, true},
		// grok's ACP is its own binary; the model flag goes BEFORE the `stdio` mode.
		{"grok", []string{"grok", "--permission-mode", "bypassPermissions", "--model", "grok-4.5"}, []string{"grok", "agent", "--model", "grok-4.5", "stdio"}, true},
	}
	for _, c := range cases {
		a, _ := Get(c.name)
		if got := a.Interactive(cfg); !slices.Equal(got, c.interactive) {
			t.Errorf("%s Interactive with model = %v, want %v", c.name, got, c.interactive)
		}
		if got := a.ACP(cfg); !slices.Equal(got, c.acp) {
			t.Errorf("%s ACP with model = %v, want %v", c.name, got, c.acp)
		}
		if got := a.Headless(cfg, "go"); hasModelFlag(got) != c.headlessHasModelFlag {
			t.Errorf("%s Headless with model = %v, want a --model flag", c.name, got)
		}
		if len(a.Models()) == 0 {
			t.Errorf("%s Models() is empty — `coop models` would show nothing", c.name)
		}
	}
	// An env-var default (COOP_<AGENT>_MODEL) reaches base() the same way.
	cfg2 := &config.Config{}
	t.Setenv("COOP_CLAUDE_MODEL", "haiku")
	a, _ := Get("claude")
	want := []string{"claude", "--dangerously-skip-permissions", "--model", "haiku"}
	if got := a.Interactive(cfg2); !slices.Equal(got, want) {
		t.Errorf("claude Interactive with COOP_CLAUDE_MODEL = %v, want %v", got, want)
	}
	// A resolved model outranks a CMD override's baked default, without adding a duplicate flag.
	t.Setenv("COOP_CLAUDE_CMD", "claude --model sonnet")
	want = []string{"claude", "--model", "haiku"}
	if got := a.Interactive(cfg2); !slices.Equal(got, want) {
		t.Errorf("claude Interactive with resolved + baked model = %v, want %v", got, want)
	}
}

func TestWithModel(t *testing.T) {
	if got := withModel([]string{"claude"}, ""); !slices.Equal(got, []string{"claude"}) {
		t.Errorf("empty model must be a no-op, got %v", got)
	}
	if got := withModel([]string{"claude", "--model", "baked"}, ""); !slices.Equal(got, []string{"claude", "--model", "baked"}) {
		t.Errorf("empty resolved model changed baked command: %v", got)
	}
	for _, tc := range []struct {
		baked, want []string
	}{
		{[]string{"codex", "--model", "x"}, []string{"codex", "--model", "y"}},
		{[]string{"codex", "--model=x"}, []string{"codex", "--model=y"}},
		{[]string{"codex", "-m", "x"}, []string{"codex", "-m", "y"}},
		{[]string{"codex", "-m=x"}, []string{"codex", "-m=y"}},
		{[]string{"codex", "--model", "--json"}, []string{"codex", "--model", "y", "--json"}},
		{[]string{"codex", "--model", "x", "-m=z"}, []string{"codex", "--model", "y"}},
		{[]string{"codex", "--", "--model", "prompt"}, []string{"codex", "--model", "y", "--", "--model", "prompt"}},
	} {
		if got := withModel(tc.baked, "y"); !slices.Equal(got, tc.want) {
			t.Errorf("withModel(%v) = %v, want %v", tc.baked, got, tc.want)
		}
	}
}

func TestEffortSelection(t *testing.T) {
	cleanCmdEnv(t)
	cfg := &config.Config{}
	cfg.SetActiveEffort("claude", "xhigh")
	cfg.SetActiveEffort("codex", "high")
	cfg.SetActiveEffort("gemini", "high") // gemini has no effort control → the flag never appears
	cfg.SetActiveEffort("grok", "high")
	cases := []struct {
		name             string
		interactive, acp []string
	}{
		{"claude", []string{"claude", "--dangerously-skip-permissions", "--effort", "xhigh"}, []string{"claude-agent-acp"}},
		{"codex", []string{"codex", "--dangerously-bypass-approvals-and-sandbox", "-c", "model_reasoning_effort=high"}, []string{"codex-acp"}},
		{"gemini", []string{"gemini", "--yolo"}, []string{"gemini", "--acp"}}, // no effort flag anywhere
		// grok's ACP is its own binary; the effort flag goes BEFORE the `stdio` mode, like the model.
		{"grok", []string{"grok", "--permission-mode", "bypassPermissions", "--reasoning-effort", "high"}, []string{"grok", "agent", "--reasoning-effort", "high", "stdio"}},
	}
	for _, c := range cases {
		a, _ := Get(c.name)
		if got := a.Interactive(cfg); !slices.Equal(got, c.interactive) {
			t.Errorf("%s Interactive with effort = %v, want %v", c.name, got, c.interactive)
		}
		if got := a.ACP(cfg); !slices.Equal(got, c.acp) {
			t.Errorf("%s ACP with effort = %v, want %v", c.name, got, c.acp)
		}
	}
	for name, want := range map[string]bool{"claude": true, "codex": true, "grok": true, "gemini": false} {
		a, _ := Get(name)
		if SupportsEffort(a) != want {
			t.Errorf("SupportsEffort(%s) = %v, want %v", name, SupportsEffort(a), want)
		}
	}
	// claude-agent-acp takes no flags, so claude's effort rides an env var instead.
	if claude, _ := Get("claude"); claude.EffortEnv() != "CLAUDE_CODE_EFFORT_LEVEL" {
		t.Errorf("claude EffortEnv = %q, want CLAUDE_CODE_EFFORT_LEVEL", claude.EffortEnv())
	}
}

func TestNativeSubagentCapabilityIsAdapterOwned(t *testing.T) {
	for name, want := range map[string]bool{"claude": true, "codex": false, "gemini": false, "grok": false} {
		a, ok := Get(name)
		if !ok {
			t.Fatalf("agent %q not registered", name)
		}
		support := a.NativeSubagents()
		if got := support.Render != nil; got != want {
			t.Errorf("%s native renderer present = %v, want %v", name, got, want)
		}
		if want && support.HomeDir == "" {
			t.Errorf("%s native support has no destination", name)
		}
	}
	claude, _ := Get("claude")
	support := claude.NativeSubagents()
	name, content := support.Render(NativeSubagent{
		Name: "coop-thinker", Description: "Use for: architecture.", Model: "opus",
		Effort: "xhigh", Prompt: "Think hard.",
	})
	if name != "coop-thinker.md" || support.HomeDir != ".claude/agents" {
		t.Errorf("Claude native destination = (%q, %q), want coop-thinker.md under .claude/agents", name, support.HomeDir)
	}
	for _, want := range []string{"name: coop-thinker", "description: Use for: architecture.", "model: opus", "effort: xhigh", "Think hard."} {
		if !strings.Contains(content, want) {
			t.Errorf("Claude native rendering missing %q:\n%s", want, content)
		}
	}
}

func TestWithEffort(t *testing.T) {
	claude, _ := Get("claude")
	gemini, _ := Get("gemini")
	codex, _ := Get("codex")
	grok, _ := Get("grok")
	if got := withEffort([]string{"claude"}, claude, ""); !slices.Equal(got, []string{"claude"}) {
		t.Errorf("empty effort must be a no-op, got %v", got)
	}
	if got := withEffort([]string{"claude", "--effort", "baked"}, claude, ""); !slices.Equal(got, []string{"claude", "--effort", "baked"}) {
		t.Errorf("empty resolved effort changed baked command: %v", got)
	}
	if got := withEffort([]string{"gemini"}, gemini, "high"); !slices.Equal(got, []string{"gemini"}) {
		t.Errorf("withEffort for an effortless agent must be a no-op, got %v", got)
	}
	for _, tc := range []struct {
		a           Agent
		baked, want []string
	}{
		{claude, []string{"claude", "--effort", "low"}, []string{"claude", "--effort", "high"}},
		{claude, []string{"claude", "--effort=low"}, []string{"claude", "--effort=high"}},
		{claude, []string{"claude", "--effort", "--verbose"}, []string{"claude", "--effort", "high", "--verbose"}},
		{claude, []string{"claude", "--effort", "low", "--effort=max"}, []string{"claude", "--effort", "high"}},
		{codex, []string{"codex", "-c", "model_reasoning_effort=low"}, []string{"codex", "-c", "model_reasoning_effort=high"}},
		{codex, []string{"codex", "--config", "model_reasoning_effort=low"}, []string{"codex", "--config", "model_reasoning_effort=high"}},
		{codex, []string{"codex", "--config=model_reasoning_effort=low"}, []string{"codex", "--config=model_reasoning_effort=high"}},
		{codex, []string{"codex", "-c", "sandbox_mode=read-only", "-c", "model_reasoning_effort=low", "-c", "model_reasoning_effort=max"}, []string{"codex", "-c", "sandbox_mode=read-only", "-c", "model_reasoning_effort=high"}},
		{grok, []string{"grok", "--effort", "low"}, []string{"grok", "--effort", "high"}},
		{grok, []string{"grok", "--effort=low", "--reasoning-effort", "max"}, []string{"grok", "--effort=high"}},
		{grok, []string{"grok", "--", "--effort", "prompt"}, []string{"grok", "--reasoning-effort", "high", "--", "--effort", "prompt"}},
	} {
		if got := withEffort(tc.baked, tc.a, "high"); !slices.Equal(got, tc.want) {
			t.Errorf("withEffort(%v) = %v, want %v", tc.baked, got, tc.want)
		}
	}
	joined := EffortSpec{Style: EffortFlagJoined, Flag: "--thinking"}
	if got := joined.Args("high"); !slices.Equal(got, []string{"--thinking=high"}) {
		t.Errorf("joined effort args = %v", got)
	}
	if got, ok := normalizeJoinedFlag([]string{"agent", "--thinking=low", "--thinking=max"}, []string{"--thinking"}, "high"); !ok || !slices.Equal(got, []string{"agent", "--thinking=high"}) {
		t.Errorf("joined effort normalization = %v, %v", got, ok)
	}
	if got, ok := normalizeAssignmentFlag([]string{"agent", "--config=reasoning=low", "-c", "reasoning=max"}, []string{"-c", "--config"}, "reasoning=", "reasoning=high"); !ok || !slices.Equal(got, []string{"agent", "--config=reasoning=high"}) {
		t.Errorf("assignment-alias effort normalization = %v, %v", got, ok)
	}
}

func TestEmptyCmdOverrideStillRunnable(t *testing.T) {
	cfg := &config.Config{} // no mcp.json → no --mcp-config trailing claude's base
	// An explicitly-empty override (COOP_<AGENT>_CMD="") must still produce a runnable command:
	// base()[0] is the executable, and the headless/exec forms index it — an empty argv would
	// otherwise try to exec the first flag (or run the image with no command).
	for _, c := range []struct{ name, env, want string }{
		{"claude", "COOP_CLAUDE_CMD", "claude"},
		{"codex", "COOP_CODEX_CMD", "codex"},
		{"gemini", "COOP_GEMINI_CMD", "gemini"},
		{"grok", "COOP_GROK_CMD", "grok"},
	} {
		t.Setenv(c.env, "")
		a, _ := Get(c.name)
		if got := a.Interactive(cfg); len(got) == 0 || got[0] != c.want {
			t.Errorf("%s Interactive with empty override = %v, want it to start with %q", c.name, got, c.want)
		}
		if got := a.Headless(cfg, "go"); len(got) == 0 || got[0] != c.want {
			t.Errorf("%s Headless with empty override = %v, want it to start with %q", c.name, got, c.want)
		}
	}
}

func TestClaudeMCPConfig(t *testing.T) {
	cleanCmdEnv(t)
	dir := t.TempDir()
	mcp := filepath.Join(dir, "mcp.json")
	mustWrite(t, mcp, `{"mcpServers":{"fs":{"command":"npx","args":["-y","server"]}}}`) // a declared server → MCP active
	cfg := &config.Config{MCPFile: mcp, MCPInBox: "/home/node/.mcp.json"}
	a, _ := Get("claude")
	want := []string{"claude", "--dangerously-skip-permissions", "--mcp-config", "/home/node/.mcp.json"}
	if got := a.Interactive(cfg); !slices.Equal(got, want) {
		t.Errorf("claude Interactive with mcp = %v, want %v", got, want)
	}
}

func TestResume(t *testing.T) {
	cleanCmdEnv(t)
	cfgDir := t.TempDir()
	ws := "/work/myrepo-forks/demo"
	id := "11111111-2222-4333-8444-555555555555"
	cfg := &config.Config{ConfigDir: cfgDir}

	// No session yet → fresh command, resumed=false (for every agent).
	for _, name := range Names() {
		a, _ := Get(name)
		if cmd, resumed := a.Resume(cfg, ws, id); resumed {
			t.Errorf("Resume(%s) resumed with no session: %v", name, cmd)
		}
	}

	// claude resumes the exact coop-owned id (projects/<cwd>/<id>.jsonl), not --continue.
	claude, _ := Get("claude")
	mustWrite(t, filepath.Join(cfg.AgentDir("claude"), "projects", ClaudeProjectKey(ws), id+".jsonl"), "{}")
	if cmd, ok := claude.Resume(cfg, ws, id); !ok ||
		!slices.Equal(cmd, []string{"claude", "--dangerously-skip-permissions", "--resume", id}) {
		t.Errorf("claude Resume = (%v, %v)", cmd, ok)
	}
	// A different id (no session file) must not resume.
	if cmd, ok := claude.Resume(cfg, ws, "99999999-2222-4333-8444-555555555555"); ok {
		t.Errorf("claude Resume matched an id with no session file: %v", cmd)
	}
	linkedClaudeID := "22222222-2222-4333-8444-555555555555"
	outsideClaude := filepath.Join(t.TempDir(), "outside.jsonl")
	mustWrite(t, outsideClaude, "{}\n")
	if err := os.Symlink(outsideClaude, filepath.Join(cfg.AgentDir("claude"), "projects", ClaudeProjectKey(ws), linkedClaudeID+".jsonl")); err != nil {
		t.Fatal(err)
	}
	if cmd, ok := claude.Resume(cfg, ws, linkedClaudeID); ok {
		t.Errorf("claude Resume followed a session symlink: %v", cmd)
	}
	directoryClaudeID := "33333333-2222-4333-8444-555555555555"
	if err := os.Mkdir(filepath.Join(cfg.AgentDir("claude"), "projects", ClaudeProjectKey(ws), directoryClaudeID+".jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if cmd, ok := claude.Resume(cfg, ws, directoryClaudeID); ok {
		t.Errorf("claude Resume accepted a session directory: %v", cmd)
	}

	// gemini resumes the exact id, matched by file content (not "latest").
	gemini, _ := Get("gemini")
	mustWrite(t, filepath.Join(cfg.AgentDir("gemini"), "tmp", "demo", "chats", "session-x.jsonl"),
		fmt.Sprintf(`{"sessionId":%q,"projectHash":"%x"}`, id, sha256.Sum256([]byte(ws))))
	if cmd, ok := gemini.Resume(cfg, ws, id); !ok ||
		!slices.Equal(cmd, []string{"gemini", "--yolo", "--resume", id}) {
		t.Errorf("gemini Resume = (%v, %v)", cmd, ok)
	}
	// gemini's tmp bucket is a version-dependent slug/hash of the path, not the raw basename — so
	// resume must find the id regardless of the bucket name (the old raw-basename lookup silently
	// missed a fork named e.g. "My.Repo" whose real bucket is "my-repo").
	slugWs := filepath.Join(t.TempDir(), "My.Cool_Repo")
	mustWrite(t, filepath.Join(cfg.AgentDir("gemini"), "tmp", "my-cool-repo", "chats", "s.jsonl"),
		fmt.Sprintf(`{"sessionId":%q,"projectHash":"%x"}`, id, sha256.Sum256([]byte(slugWs))))
	if _, ok := gemini.Resume(cfg, slugWs, id); !ok {
		t.Error("gemini Resume must match a session in a slug-named bucket, not only the raw basename")
	}
	// gemini 0.46+ also writes 64-char-hash buckets alongside slug ones (seen on real hosts). A
	// DISTINCT id that lives ONLY in a hash bucket must still resolve — proving the content scan
	// spans every bucket scheme, not just slugs. (A raw-basename lookup would silently miss it.)
	hashID := "77777777-2222-4333-8444-555555555555"
	hashBucket := "00019aef076a44ed361af8d31415c187d0650aad947127fd02c5617717734f4f"
	hashWs := filepath.Join(t.TempDir(), "hashed-repo")
	mustWrite(t, filepath.Join(cfg.AgentDir("gemini"), "tmp", hashBucket, "chats", "h.jsonl"),
		fmt.Sprintf(`{"sessionId":%q,"projectHash":"%x"}`, hashID, sha256.Sum256([]byte(hashWs))))
	if _, ok := gemini.Resume(cfg, hashWs, hashID); !ok {
		t.Error("gemini Resume must match a session in a 64-char-hash bucket (the gemini 0.46+ scheme)")
	}
	if cmd, ok := gemini.Resume(cfg, filepath.Join(t.TempDir(), "wrong-cwd"), id); ok {
		t.Errorf("gemini Resume matched the same id under another cwd: %v", cmd)
	}
	// Gemini still loads legacy whole-file JSON records. Pretty printing must not make the
	// persisted fork session disappear after a CLI upgrade.
	legacyGeminiID := "66666666-2222-4333-8444-555555555555"
	legacyGeminiWS := filepath.Join(t.TempDir(), "legacy-gemini")
	mustWrite(t, filepath.Join(cfg.AgentDir("gemini"), "tmp", "legacy", "chats", "session.json"), fmt.Sprintf(`{
  "messages": [],
  "sessionId": %q,
  "projectHash": "%x"
}`, legacyGeminiID, sha256.Sum256([]byte(legacyGeminiWS))))
	if _, ok := gemini.Resume(cfg, legacyGeminiWS, legacyGeminiID); !ok {
		t.Error("gemini Resume must match a pretty-printed legacy JSON session")
	}
	missingHashID := "33333333-2222-4333-8444-555555555555"
	mustWrite(t, filepath.Join(cfg.AgentDir("gemini"), "tmp", "legacy-no-hash", "chats", "session.json"),
		fmt.Sprintf(`{"sessionId":%q}`, missingHashID))
	if cmd, ok := gemini.Resume(cfg, legacyGeminiWS, missingHashID); ok {
		t.Errorf("gemini Resume accepted legacy metadata with no cwd projectHash: %v", cmd)
	}
	hashOnlyID := "11111111-2222-4333-8444-555555555555"
	mustWrite(t, filepath.Join(cfg.AgentDir("gemini"), "tmp", fmt.Sprintf("%x", sha256.Sum256([]byte(legacyGeminiWS))), "chats", "legacy.json"),
		fmt.Sprintf(`{"sessionId":%q}`, hashOnlyID))
	if _, ok := gemini.Resume(cfg, legacyGeminiWS, hashOnlyID); !ok {
		t.Error("gemini Resume must accept a no-projectHash legacy record in the exact cwd hash bucket")
	}
	symlinkID := "22222222-2222-4333-8444-555555555555"
	outside := filepath.Join(t.TempDir(), "outside.json")
	mustWrite(t, outside, fmt.Sprintf(`{"sessionId":%q,"projectHash":"%x"}`, symlinkID, sha256.Sum256([]byte(legacyGeminiWS))))
	symlink := filepath.Join(cfg.AgentDir("gemini"), "tmp", "linked", "chats", "session.json")
	if err := os.MkdirAll(filepath.Dir(symlink), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, symlink); err != nil {
		t.Fatal(err)
	}
	if cmd, ok := gemini.Resume(cfg, legacyGeminiWS, symlinkID); ok {
		t.Errorf("gemini Resume followed a provider-created session symlink: %v", cmd)
	}

	// grok resumes the exact coop-owned id in the matching cwd bucket.
	grok, _ := Get("grok")
	mustWrite(t, filepath.Join(cfg.AgentDir("grok"), "sessions", url.PathEscape(ws), id, "summary.json"), `{}`)
	if cmd, ok := grok.Resume(cfg, ws, id); !ok ||
		!slices.Equal(cmd, []string{"grok", "--permission-mode", "bypassPermissions", "--resume", id}) {
		t.Errorf("grok Resume = (%v, %v)", cmd, ok)
	}
	// A different id (no matching session) must not resume.
	if cmd, ok := grok.Resume(cfg, ws, "88888888-2222-4333-8444-555555555555"); ok {
		t.Errorf("grok Resume matched an id with no session: %v", cmd)
	}
	if cmd, ok := grok.Resume(cfg, filepath.Join(t.TempDir(), "wrong-cwd"), id); ok {
		t.Errorf("grok Resume matched the same id under another cwd: %v", cmd)
	}
	// Overlong encoded cwd names use a slug/hash bucket plus a bounded .cwd marker.
	longGrokID := "55555555-2222-4333-8444-555555555555"
	longGrokWS := "/" + strings.Repeat("long-directory/", 24) + "repo"
	longBucket := filepath.Join(cfg.AgentDir("grok"), "sessions", "repo-deadbeef")
	mustWrite(t, filepath.Join(longBucket, ".cwd"), longGrokWS+"\n")
	mustWrite(t, filepath.Join(longBucket, longGrokID, "summary.json"), `{}`)
	if _, ok := grok.Resume(cfg, longGrokWS, longGrokID); !ok {
		t.Error("grok Resume must match a long-cwd .cwd bucket")
	}
	duplicateGrokWS := "/" + strings.Repeat("duplicate-directory/", 24) + "repo"
	duplicateGrokID := "66666666-2222-4333-8444-555555555555"
	staleGrokBucket := filepath.Join(cfg.AgentDir("grok"), "sessions", "repo-stale")
	validGrokBucket := filepath.Join(cfg.AgentDir("grok"), "sessions", "repo-valid")
	mustWrite(t, filepath.Join(staleGrokBucket, ".cwd"), duplicateGrokWS+"\n")
	mustWrite(t, filepath.Join(validGrokBucket, ".cwd"), duplicateGrokWS+"\n")
	mustWrite(t, filepath.Join(validGrokBucket, duplicateGrokID, "summary.json"), `{}`)
	if _, ok := grok.Resume(cfg, duplicateGrokWS, duplicateGrokID); !ok {
		t.Error("grok Resume let a stale matching cwd bucket mask a later valid bucket")
	}
	emptyGrokID := "44444444-2222-4333-8444-555555555555"
	if err := os.MkdirAll(filepath.Join(longBucket, emptyGrokID), 0o700); err != nil {
		t.Fatal(err)
	}
	if cmd, ok := grok.Resume(cfg, longGrokWS, emptyGrokID); ok {
		t.Errorf("grok Resume accepted an empty session directory: %v", cmd)
	}

	// Codex resumes only an exact persisted CLI session for the cwd. Empty-ID legacy discovery
	// belongs to the fork launcher, and newer exec/editor/unknown records are not interactive CLI.
	codex, _ := Get("codex")
	sess := filepath.Join(cfg.AgentDir("codex"), "sessions", "2026", "06")
	interactiveCodexID := "019f6a60-a28e-7d22-919c-81f43bef064f"
	execCodexID := "019f6a61-b39f-7e33-a811-92f64cf17550"
	mustWrite(t, filepath.Join(sess, "16", "rollout-interactive.jsonl"),
		`{"type":"session_meta","payload":{"id":"`+interactiveCodexID+`","cwd":"`+ws+`","source":"cli"}}`+"\n")
	mustWrite(t, filepath.Join(sess, "17", "rollout-exec.jsonl"),
		`{"type":"session_meta","payload":{"id":"`+execCodexID+`","cwd":"`+ws+`","source":"exec"}}`+"\n")
	for i, source := range []string{"vscode", "unknown"} {
		foreignID := fmt.Sprintf("019f6a6%d-b39f-7e33-a811-92f64cf1755%d", i+2, i+1)
		mustWrite(t, filepath.Join(sess, fmt.Sprintf("%d", 18+i), "rollout-foreign.jsonl"),
			`{"type":"session_meta","payload":{"id":"`+foreignID+`","cwd":"`+ws+`","source":"`+source+`"}}`+"\n")
	}
	mustWrite(t, filepath.Join(sess, "20", "rollout-malformed.jsonl"),
		`{"type":"session_meta","payload":{"id":"--help","cwd":"`+ws+`","source":"cli"}}`+"\n")
	mustWrite(t, filepath.Join(sess, "21", "rollout-wrong-type.jsonl"),
		`{"type":"response_item","payload":{"id":"019f6a64-b39f-7e33-a811-92f64cf17553","cwd":"`+ws+`","source":"cli"}}`+"\n")
	if cmd, ok := codex.Resume(cfg, ws, ""); ok {
		t.Errorf("codex Resume(empty) selected global history: %v", cmd)
	}
	if cmd, ok := codex.Resume(cfg, ws, interactiveCodexID); !ok || !slices.Contains(cmd, interactiveCodexID) {
		t.Errorf("codex Resume(exact id) = (%v, %v)", cmd, ok)
	}
	if cmd, ok := codex.Resume(cfg, ws, "missing-id"); ok {
		t.Errorf("codex Resume accepted another native id: %v", cmd)
	}
	discoverer := codex.(SessionDiscoverer)
	if got := discoverer.LatestSessionID(cfg, ws); got != interactiveCodexID {
		t.Errorf("codex latest CLI session = %q, want %q", got, interactiveCodexID)
	}
	if got := discoverer.SessionIDs(cfg, ws); !slices.Equal(got, []string{interactiveCodexID}) {
		t.Errorf("codex CLI session IDs = %v, want only %q", got, interactiveCodexID)
	}
	unsafeCodexID := "019f6a62-8f6e-7440-a55e-9df3ff5b77dd"
	outsideCodex := filepath.Join(t.TempDir(), "outside.jsonl")
	mustWrite(t, outsideCodex, `{"type":"session_meta","payload":{"id":"`+unsafeCodexID+`","cwd":"`+ws+`","source":"cli"}}`+"\n")
	if err := os.Symlink(outsideCodex, filepath.Join(sess, "symlink.jsonl")); err != nil {
		t.Fatal(err)
	}
	if cmd, ok := codex.Resume(cfg, ws, unsafeCodexID); ok {
		t.Errorf("codex Resume followed a provider-created rollout symlink: %v", cmd)
	}
	oversizeCodexID := "019f6a63-8f6e-7440-a55e-9df3ff5b77dd"
	mustWrite(t, filepath.Join(sess, "oversize.jsonl"), strings.Repeat(" ", codexSessionMetadataLimit+1)+
		`{"type":"session_meta","payload":{"id":"`+oversizeCodexID+`","cwd":"`+ws+`","source":"cli"}}`)
	if cmd, ok := codex.Resume(cfg, ws, oversizeCodexID); ok {
		t.Errorf("codex Resume accepted oversized first-line metadata: %v", cmd)
	}
	if err := os.Mkdir(filepath.Join(sess, "special.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A session recorded for a DIFFERENT cwd must not match.
	if cmd, ok := codex.Resume(cfg, "/work/myrepo-forks/other", id); ok {
		t.Errorf("codex Resume(other fork) wrongly matched: %v", cmd)
	}
	for _, name := range Names() {
		ag, _ := Get(name)
		if cmd, ok := ag.Resume(cfg, ws, "--help"); ok {
			t.Errorf("%s Resume accepted a non-UUID session id: %v", name, cmd)
		}
	}
}

func TestValidSessionID(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"019f6a60-a28e-7d22-919c-81f43bef064f", true},
		{"11111111-2222-4333-8444-555555555555", true},
		{"--help", false},
		{"019F6A60-A28E-7D22-919C-81F43BEF064F", false},
		{"019f6a60a28e7d22919c81f43bef064f", false},
		{"019f6a60-a28e-0d22-919c-81f43bef064f", false},
		{"019f6a60-a28e-7d22-719c-81f43bef064f", false},
	} {
		if got := ValidSessionID(tc.id); got != tc.want {
			t.Errorf("ValidSessionID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestResumeRejectsSymlinkedSessionRoots(t *testing.T) {
	ws := "/work/repo"
	id := "019f6a60-a28e-7d22-919c-81f43bef064f"
	for _, provider := range []string{"claude", "codex", "gemini", "grok"} {
		t.Run(provider, func(t *testing.T) {
			cfg := &config.Config{ConfigDir: t.TempDir()}
			outside := t.TempDir()
			var rootPath string
			switch provider {
			case "claude":
				rootPath = filepath.Join(cfg.AgentDir(provider), "projects")
				mustWrite(t, filepath.Join(outside, ClaudeProjectKey(ws), id+".jsonl"), `{}`)
			case "codex":
				rootPath = filepath.Join(cfg.AgentDir(provider), "sessions")
				mustWrite(t, filepath.Join(outside, "2026", "07", "rollout.jsonl"),
					`{"type":"session_meta","payload":{"id":"`+id+`","cwd":"`+ws+`","source":"cli"}}`+"\n")
			case "gemini":
				rootPath = filepath.Join(cfg.AgentDir(provider), "tmp")
				mustWrite(t, filepath.Join(outside, "bucket", "chats", "session.jsonl"),
					fmt.Sprintf(`{"sessionId":%q,"projectHash":"%x"}`, id, sha256.Sum256([]byte(ws))))
			case "grok":
				rootPath = filepath.Join(cfg.AgentDir(provider), "sessions")
				mustWrite(t, filepath.Join(outside, url.PathEscape(ws), id, "summary.json"), `{}`)
			}
			if err := os.MkdirAll(filepath.Dir(rootPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, rootPath); err != nil {
				t.Fatal(err)
			}
			ag, _ := Get(provider)
			if cmd, ok := ag.Resume(cfg, ws, id); ok {
				t.Errorf("Resume followed symlinked %s history root: %v", provider, cmd)
			}
		})
	}
}

func TestStartSessionAndPreset(t *testing.T) {
	cleanCmdEnv(t)
	cfg := &config.Config{ConfigDir: t.TempDir()}
	id := "11111111-2222-4333-8444-555555555555"

	// claude/gemini/grok preset a caller-chosen id; codex cannot.
	for name, want := range map[string]bool{"claude": true, "gemini": true, "grok": true, "codex": false} {
		a, _ := Get(name)
		if a.PresetSessionID() != want {
			t.Errorf("%s PresetSessionID = %v, want %v", name, a.PresetSessionID(), want)
		}
	}

	claude, _ := Get("claude")
	if cmd := claude.StartSession(cfg, id); !slices.Equal(cmd, []string{"claude", "--dangerously-skip-permissions", "--session-id", id}) {
		t.Errorf("claude StartSession = %v", cmd)
	}
	gemini, _ := Get("gemini")
	if cmd := gemini.StartSession(cfg, id); !slices.Equal(cmd, []string{"gemini", "--yolo", "--session-id", id}) {
		t.Errorf("gemini StartSession = %v", cmd)
	}
	grok, _ := Get("grok")
	if cmd := grok.StartSession(cfg, id); !slices.Equal(cmd, []string{"grok", "--permission-mode", "bypassPermissions", "--session-id", id}) {
		t.Errorf("grok StartSession = %v", cmd)
	}
	// codex ignores the id and just starts interactively.
	codex, _ := Get("codex")
	if cmd := codex.StartSession(cfg, id); !slices.Equal(cmd, codex.Interactive(cfg)) {
		t.Errorf("codex StartSession = %v, want Interactive", cmd)
	}
	// Empty id → Interactive for the preset agents too.
	if cmd := claude.StartSession(cfg, ""); !slices.Equal(cmd, claude.Interactive(cfg)) {
		t.Errorf("claude StartSession(empty) = %v, want Interactive", cmd)
	}
}

func TestMetadata(t *testing.T) {
	owners := map[string]string{}
	for _, name := range Names() {
		a, _ := Get(name)
		instruction := a.InstructionFile()
		if instruction == "" || filepath.Base(instruction) != instruction || strings.ContainsAny(instruction, `/\\`) {
			t.Errorf("%s InstructionFile = %q, want one safe basename", name, instruction)
		}
		authFile, authEnv := a.AuthMarker()
		if authFile == "" || filepath.Base(authFile) != authFile || strings.ContainsAny(authFile, `/\\`) || authEnv == "" {
			t.Errorf("%s AuthMarker = (%q,%q), want a safe basename and env key", name, authFile, authEnv)
		}
		seen := map[string]bool{}
		primaryEnvCount := 0
		for _, key := range a.CredentialEnvKeys() {
			if key == "" || seen[key] {
				t.Errorf("%s CredentialEnvKeys contains an empty or duplicate key %q", name, key)
			}
			seen[key] = true
			if owner, exists := owners[key]; exists {
				t.Errorf("credential key %q is owned by both %s and %s", key, owner, name)
			} else {
				owners[key] = name
			}
			if key == authEnv {
				primaryEnvCount++
			}
		}
		if primaryEnvCount != 1 {
			t.Errorf("%s primary AuthMarker key %q appears %d times in CredentialEnvKeys", name, authEnv, primaryEnvCount)
		}
		live := a.LiveCredentials()
		if live.Portability == nil {
			t.Errorf("%s live credentials have no portability check", name)
		}
		if len(live.AuthSignals) == 0 {
			t.Errorf("%s live credentials have no auth diagnostics", name)
		}
		artifactSeen := map[string]bool{}
		primaryFiles := 0
		for _, artifact := range live.Artifacts {
			if artifact.Name == "" || filepath.Base(artifact.Name) != artifact.Name || strings.ContainsAny(artifact.Name, `/\\`) || artifactSeen[artifact.Name] {
				t.Errorf("%s live credentials contain unsafe or duplicate basename %q", name, artifact.Name)
			}
			artifactSeen[artifact.Name] = true
			if artifact.Project == nil {
				t.Errorf("%s credential artifact %q has no safe projector", name, artifact.Name)
			}
			if artifact.Primary {
				primaryFiles++
				if artifact.Name != authFile {
					t.Errorf("%s primary credential artifact = %q, want AuthMarker %q", name, artifact.Name, authFile)
				}
			}
		}
		if primaryFiles != 1 {
			t.Errorf("%s has %d primary credential artifacts, want 1", name, primaryFiles)
		}
	}
}

func TestGeminiCredentialSelectorProjection(t *testing.T) {
	gemini, _ := Get("gemini")
	project := mustLiveCredentials(t, gemini).Artifacts[2].Project
	got, err := project([]byte(`{"security":{"auth":{"selectedType":"oauth-personal","hidden":"drop"},"other":"drop"},"hooks":{"before":"drop"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"security":{"auth":{"selectedType":"oauth-personal"}}}` + "\n"; string(got) != want {
		t.Errorf("projected settings = %s, want %s", got, want)
	}
	if got, err := project([]byte(`{"security":{}}`)); err != nil || got != nil {
		t.Errorf("missing auth projection = %q, %v; want nil, nil", got, err)
	}
	if _, err := project([]byte(`{`)); err == nil {
		t.Fatal("malformed settings projection succeeded")
	}
}

func TestCredentialArtifactProjectionUsesExactAccessOnlySchemas(t *testing.T) {
	tests := []struct {
		provider string
		input    string
		want     string
	}{
		{
			provider: "claude",
			input:    `{"TOP_UNKNOWN_CANARY":"drop","claudeAiOauth":{"accessToken":"access","expiresAt":4102444800000,"scopes":["user:inference","account:read"],"refreshToken":"REFRESH_CANARY","subscriptionType":"NESTED_CANARY","deep":{"refreshToken":"DEEP_CANARY"}}}`,
			want:     `{"claudeAiOauth":{"accessToken":"access","expiresAt":4102444800000,"scopes":["user:inference"]}}` + "\n",
		},
		{
			provider: "codex",
			input:    `{"auth_mode":"chatgpt","OPENAI_API_KEY":"INACTIVE_CANARY","tokens":{"id_token":"identity","access_token":"access","refresh_token":"REFRESH_CANARY","account_id":"account","deep":{"refresh_token":"DEEP_CANARY"}},"last_refresh":"2026-07-15T00:00:00Z","TOP_UNKNOWN_CANARY":"drop"}`,
			want:     `{"auth_mode":"chatgpt","tokens":{"id_token":"identity","access_token":"access","refresh_token":"","account_id":"account"},"last_refresh":"2026-07-15T00:00:00Z"}` + "\n",
		},
		{
			provider: "grok",
			input:    `{"issuer::one":{"key":"access-one","expires_at":"2999-01-01T00:00:00Z","auth_mode":"oauth","oidc_issuer":"issuer-one","oidc_client_id":"client-one","principal_id":"principal-one","principal_type":"user","user_id":"user-one","team_id":"team-one","create_time":"2026-07-16T01:00:00Z","refresh_token":"REFRESH_CANARY","email":"PRIVATE_CANARY","profile":{"deep":"DEEP_CANARY"}},"issuer::two":{"key":"access-two","expires_at":"2999-02-01T00:00:00Z","auth_mode":"oauth","oidc_issuer":"issuer-two","oidc_client_id":"client-two","principal_id":"principal-two","principal_type":"user","user_id":"user-two","team_id":"team-two","create_time":"2026-07-16T02:00:00Z","coding_data_retention_opt_out":true,"TOP_UNKNOWN_CANARY":"drop"},"incomplete":{"key":"drop"}}`,
			want:     `{"issuer::one":{"key":"access-one","expires_at":"2999-01-01T00:00:00Z","auth_mode":"oauth","oidc_issuer":"issuer-one","oidc_client_id":"client-one","principal_id":"principal-one","principal_type":"user","user_id":"user-one","team_id":"team-one","create_time":"2026-07-16T01:00:00Z"},"issuer::two":{"key":"access-two","expires_at":"2999-02-01T00:00:00Z","auth_mode":"oauth","oidc_issuer":"issuer-two","oidc_client_id":"client-two","principal_id":"principal-two","principal_type":"user","user_id":"user-two","team_id":"team-two","create_time":"2026-07-16T02:00:00Z"}}` + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			ag, _ := Get(tt.provider)
			got, err := mustLiveCredentials(t, ag).Artifacts[0].Project([]byte(tt.input))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("projected credential = %s, want %s", got, tt.want)
			}
			for _, canary := range []string{"REFRESH_CANARY", "TOP_UNKNOWN_CANARY", "NESTED_CANARY", "DEEP_CANARY", "INACTIVE_CANARY", "PRIVATE_CANARY"} {
				if strings.Contains(string(got), canary) {
					t.Fatalf("projected credential retained unknown authority %s: %s", canary, got)
				}
			}
		})
	}

	codex, _ := Get("codex")
	codexProject := mustLiveCredentials(t, codex).Artifacts[0].Project
	got, err := codexProject([]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"api-access","tokens":{"id_token":"INACTIVE_CANARY","access_token":"INACTIVE_CANARY","refresh_token":"REFRESH_CANARY"},"last_refresh":"INACTIVE_CANARY"}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"auth_mode":"apikey","OPENAI_API_KEY":"api-access"}` + "\n"; string(got) != want {
		t.Fatalf("Codex API-key projection = %s, want %s", got, want)
	}
	for name, input := range map[string]string{
		"unsupported mode":     `{"auth_mode":"other","OPENAI_API_KEY":"key"}`,
		"missing id token":     `{"auth_mode":"chatgpt","tokens":{"access_token":"access"},"last_refresh":"now"}`,
		"missing last refresh": `{"auth_mode":"chatgpt","tokens":{"id_token":"id","access_token":"access"}}`,
	} {
		t.Run("codex "+name, func(t *testing.T) {
			if got, err := codexProject([]byte(input)); err == nil || got != nil {
				t.Fatalf("projection = %q, %v; want fail closed", got, err)
			}
		})
	}

	claude, _ := Get("claude")
	if got, err := mustLiveCredentials(t, claude).Artifacts[0].Project([]byte(`{"claudeAiOauth":{"accessToken":"access","expiresAt":7,"scopes":["account:read"]}}`)); err == nil || got != nil {
		t.Fatalf("Claude projection without inference scope = %q, %v; want fail closed", got, err)
	}

	for _, provider := range []string{"claude", "codex", "grok"} {
		ag, _ := Get(provider)
		if got, err := mustLiveCredentials(t, ag).Artifacts[0].Project([]byte(`{}`)); err == nil || got != nil {
			t.Errorf("%s empty credential projection = %q, %v; want a fail-closed error", provider, got, err)
		}
	}
	grok, _ := Get("grok")
	grokProject := mustLiveCredentials(t, grok).Artifacts[0].Project
	if got, err := grokProject([]byte(`{"rootRefresh":"ROOT_CANARY","issuer::id":{"key":"access","expires_at":"2999-01-01T00:00:00Z"}}`)); err == nil || got != nil {
		t.Errorf("Grok scalar root projection = %q, %v; want a fail-closed error", got, err)
	}
	validGrokEntry := map[string]string{
		"key": "access", "expires_at": "2999-01-01T00:00:00Z", "auth_mode": "oauth",
		"oidc_issuer": "issuer", "oidc_client_id": "client", "principal_id": "principal",
		"principal_type": "user", "user_id": "user", "team_id": "team",
		"create_time": "2026-07-16T01:00:00Z",
	}
	for _, missing := range []string{
		"key", "expires_at", "auth_mode", "oidc_issuer", "oidc_client_id", "principal_id",
		"principal_type", "user_id", "team_id", "create_time",
	} {
		t.Run("grok missing "+missing, func(t *testing.T) {
			entry := make(map[string]string, len(validGrokEntry)-1)
			for key, value := range validGrokEntry {
				if key != missing {
					entry[key] = value
				}
			}
			input, err := json.Marshal(map[string]map[string]string{"issuer::id": entry})
			if err != nil {
				t.Fatal(err)
			}
			if got, err := grokProject(input); err == nil || got != nil {
				t.Fatalf("projection = %q, %v; want fail closed", got, err)
			}
		})
	}
	for name, input := range map[string]string{
		"malformed expiry":      `{"issuer::id":{"key":"access","expires_at":"not-a-time","auth_mode":"oauth","oidc_issuer":"issuer","oidc_client_id":"client","principal_id":"principal","principal_type":"user","user_id":"user","team_id":"team","create_time":"2026-07-16T01:00:00Z"}}`,
		"malformed create time": `{"issuer::id":{"key":"access","expires_at":"2999-01-01T00:00:00Z","auth_mode":"oauth","oidc_issuer":"issuer","oidc_client_id":"client","principal_id":"principal","principal_type":"user","user_id":"user","team_id":"team","create_time":"not-a-time"}}`,
	} {
		t.Run("grok "+name, func(t *testing.T) {
			if got, err := grokProject([]byte(input)); err == nil || got != nil {
				t.Fatalf("projection = %q, %v; want fail closed", got, err)
			}
		})
	}

	gemini, _ := Get("gemini")
	for _, artifact := range mustLiveCredentials(t, gemini).Artifacts[:2] {
		if got, err := artifact.Project([]byte(`{"secret":"HOST_BOUND_CANARY"}`)); err != nil || got != nil {
			t.Errorf("host-bound Gemini artifact %s projection = %q, %v; want nil", artifact.Name, got, err)
		}
	}
}

func TestGeminiCredentialEnvPrecedence(t *testing.T) {
	gemini, _ := Get("gemini")
	dir := t.TempDir()
	for authType, wantKeys := range map[string][]string{
		"gemini-api-key": {"GEMINI_API_KEY"},
		"vertex-ai":      {"GOOGLE_API_KEY"},
		"oauth-personal": nil,
		"":               nil,
	} {
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"security":{"auth":{"selectedType":"`+authType+`"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := gemini.ActiveCredentialEnvKeys(dir, true); !slices.Equal(got, wantKeys) {
			t.Errorf("selectedType %q env keys = %v, want %v", authType, got, wantKeys)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := gemini.ActiveCredentialEnvKeys(dir, true); got != nil {
		t.Fatalf("malformed settings selected env keys %v", got)
	}
	if err := os.Remove(filepath.Join(dir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if got := gemini.ActiveCredentialEnvKeys(dir, true); got != nil {
		t.Fatalf("marker without selector selected env keys %v", got)
	}
	if got := gemini.ActiveCredentialEnvKeys(dir, false); !slices.Equal(got, gemini.CredentialEnvKeys()) {
		t.Fatalf("missing marker and selector env keys = %v, want %v", got, gemini.CredentialEnvKeys())
	}
}

func TestPortableCredentialSafetyRequiresValidityBeyondDeadline(t *testing.T) {
	root := t.TempDir()
	deadline := time.Now().Add(time.Hour)
	future := deadline.Add(time.Hour)
	past := deadline.Add(-time.Minute)

	claude, _ := Get("claude")
	claudeLive := mustLiveCredentials(t, claude)
	claudeDir := filepath.Join(root, "claude")
	mustWrite(t, filepath.Join(claudeDir, ".credentials.json"), fmt.Sprintf(
		`{"claudeAiOauth":{"accessToken":"access","expiresAt":%d,"scopes":["user:inference"]}}`, future.UnixMilli()))
	if got := claudeLive.Portability(claudeDir, deadline); got != CredentialPortable {
		t.Fatal("unexpired Claude access token was not portable")
	}
	mustWrite(t, filepath.Join(claudeDir, ".credentials.json"), fmt.Sprintf(
		`{"claudeAiOauth":{"accessToken":"access","refreshToken":"refresh","expiresAt":%d,"scopes":["user:inference"]}}`, past.UnixMilli()))
	if got := claudeLive.Portability(claudeDir, deadline); got != CredentialRefreshRequired {
		t.Fatal("expired Claude refresh credential was treated as safe to copy")
	}

	codex, _ := Get("codex")
	codexLive := mustLiveCredentials(t, codex)
	codexDir := filepath.Join(root, "codex")
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, future.Unix())))
	mustWrite(t, filepath.Join(codexDir, "auth.json"), `{"auth_mode":"chatgpt","tokens":{"id_token":"identity","access_token":"x.`+payload+`.x"},"last_refresh":"2026-07-15T00:00:00Z"}`)
	if got := codexLive.Portability(codexDir, deadline); got != CredentialPortable {
		t.Fatal("unexpired Codex access token was not portable")
	}

	gemini, _ := Get("gemini")
	if got := mustLiveCredentials(t, gemini).Portability(filepath.Join(root, "gemini"), deadline); got != CredentialNotPortable {
		t.Fatal("host-bound Gemini file keychain was treated as portable")
	}

	grok, _ := Get("grok")
	grokLive := mustLiveCredentials(t, grok)
	grokDir := filepath.Join(root, "grok")
	mustWrite(t, filepath.Join(grokDir, "auth.json"), fmt.Sprintf(
		`{"issuer::id":{"key":"access","expires_at":%q,"auth_mode":"oauth","oidc_issuer":"issuer","oidc_client_id":"client","principal_id":"principal","principal_type":"user","user_id":"user","team_id":"team","create_time":"2026-07-16T01:00:00Z"}}`, future.Format(time.RFC3339Nano)))
	if got := grokLive.Portability(grokDir, deadline); got != CredentialPortable {
		t.Fatal("unexpired Grok access token was not portable")
	}
	mustWrite(t, filepath.Join(grokDir, "auth.json"), fmt.Sprintf(
		`{"issuer::id":{"key":"access","refresh_token":"refresh","expires_at":%q,"auth_mode":"oauth","oidc_issuer":"issuer","oidc_client_id":"client","principal_id":"principal","principal_type":"user","user_id":"user","team_id":"team","create_time":"2026-07-16T01:00:00Z"}}`, past.Format(time.RFC3339Nano)))
	if got := grokLive.Portability(grokDir, deadline); got != CredentialRefreshRequired {
		t.Fatal("expired Grok refresh credential was treated as safe to copy")
	}
}

func TestCLIErrorClassificationIsProviderOwnedAndRedacted(t *testing.T) {
	cases := map[string]string{
		"claude": "Not logged in",
		"codex":  "Authentication required",
		"gemini": "Manual authorization is required",
		"grok":   "Not signed in",
	}
	for provider, output := range cases {
		ag, _ := Get(provider)
		live := mustLiveCredentials(t, ag)
		if got := ClassifyCLIError(live, output); got != "authentication" {
			t.Errorf("%s auth classification = %q", provider, got)
		}
		if got := ClassifyCLIError(live, "usage limit reached"); got != "rate_limit" {
			t.Errorf("%s rate classification = %q", provider, got)
		}
		if got := ClassifyCLIError(live, "unknown failure"); got != "process" {
			t.Errorf("%s generic classification = %q", provider, got)
		}
	}
}

func TestMCP(t *testing.T) {
	dir := t.TempDir()
	mcpFile := filepath.Join(dir, "mcp.json")
	mustWrite(t, mcpFile, `{"mcpServers":{"x":{"command":"y"}}}`)
	cfg := &config.Config{MCPFile: mcpFile, ConfigDir: dir, HomeInBox: "/home/node"}

	// claude reads mcp.json raw (--mcp-config) → no generated mounts.
	claude, _ := Get("claude")
	if m, err := claude.MCP(cfg); err != nil || len(m) != 0 {
		t.Errorf("claude MCP = %v, %v; want none (reads mcp.json directly)", m, err)
	}
	// gemini/codex/grok generate a config file at their native path (grok reuses codex's
	// [mcp_servers.*] TOML shape).
	for name, boxPath := range map[string]string{
		"gemini": "/home/node/.gemini/settings.json",
		"codex":  "/home/node/.codex/config.toml",
		"grok":   "/home/node/.grok/config.toml",
	} {
		ag, _ := Get(name)
		m, err := ag.MCP(cfg)
		if err != nil || len(m) != 1 || m[0].BoxPath != boxPath || m[0].Content == "" {
			t.Errorf("%s MCP = %v, %v; want one non-empty mount at %s", name, m, err, boxPath)
		}
	}
}

// TestClaudeProjectKey: the session-dir key dashes every non-alphanumeric char (matching
// Claude Code), so a dotted segment like ".agent" maps to "-agent" and coop resolves the
// right project dir. Ground truth: Claude stores "/x/.config" as "-x--config".
func TestClaudeProjectKey(t *testing.T) {
	cases := map[string]string{
		"/Users/a/Projects/os/coop": "-Users-a-Projects-os-coop",
		"/x/.config/hammerspoon":    "-x--config-hammerspoon",
		"/repo/.agent":              "-repo--agent",
		"/has_underscore/and.dot":   "-has-underscore-and-dot",
	}
	for in, want := range cases {
		if got := ClaudeProjectKey(in); got != want {
			t.Errorf("ClaudeProjectKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLogin(t *testing.T) {
	cfg := &config.Config{}
	for name, want := range map[string][]string{
		"claude": {"claude", "auth", "login"},
		"gemini": {"gemini"},
		"codex":  {"codex", "login", "--device-auth"},
		"grok":   {"grok", "login", "--device-auth"},
	} {
		a, _ := Get(name)
		if got := a.Login(cfg); !slices.Equal(got, want) {
			t.Errorf("%s Login = %v, want %v", name, got, want)
		}
	}
}

// TestACPRateLimitSignalsPinned pins each adapter's structured limit markers — the wire
// format the ACP controller rotates on. A change here must be a conscious adapter edit
// (the controller itself carries no provider constants).
func TestACPRateLimitSignalsPinned(t *testing.T) {
	want := map[string][]ACPSignal{
		"claude": {{Key: "errorKind", Value: "rate_limit"}},
		"codex":  {{Value: "usageLimitExceeded"}},
		"gemini": {{Value: "RESOURCE_EXHAUSTED"}},
		// grok's ACP limit marker isn't captured yet (needs a live limit in a box) — pin the
		// current honest state: no structured signal, so it rotates only on the output-token axis.
		"grok": nil,
	}
	for name, w := range want {
		a, ok := Get(name)
		if !ok {
			t.Fatalf("agent %s not registered", name)
		}
		got := a.ACPRateLimitSignals()
		if len(got) != len(w) {
			t.Fatalf("%s signals = %+v, want %+v", name, got, w)
		}
		for i := range w {
			if got[i] != w[i] {
				t.Errorf("%s signal[%d] = %+v, want %+v", name, i, got[i], w[i])
			}
		}
	}
}

// TestACPSessionSettingsAndBoxEnv pins each adapter's ordered target settings and box env.
func TestACPSessionSettingsAndBoxEnv(t *testing.T) {
	claude, _ := Get("claude")
	target := Target{Model: "model-x", Effort: "xhigh"}
	if got := claude.ACPSessionSettings(target); !slices.Equal(got, []ACPSessionSetting{
		{Method: ACPSetConfigOption, ConfigID: "mode", Value: "bypassPermissions"},
		{Method: ACPSetConfigOption, ConfigID: "model", Value: "model-x"},
		{Method: ACPSetConfigOption, ConfigID: "effort", Value: "xhigh"},
	}) {
		t.Errorf("claude ACPSessionSettings = %v", got)
	}
	wantEnv := []string{"CLAUDE_CONFIG_DIR=/home/node/.claude", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0"}
	if got := claude.BoxEnv("/home/node"); len(got) != 2 || got[0] != wantEnv[0] || got[1] != wantEnv[1] {
		t.Errorf("claude BoxEnv = %v, want %v", got, wantEnv)
	}
	// codex redirects its single-writer sqlite state OFF the shared home to a container-local
	// path, so parallel codex boxes on one account don't collide on the state runtime.
	codex, _ := Get("codex")
	if got := codex.ACPSessionSettings(target); !slices.Equal(got, []ACPSessionSetting{
		{Method: ACPSetConfigOption, ConfigID: "model", Value: "model-x"},
		{Method: ACPSetConfigOption, ConfigID: "reasoning_effort", Value: "xhigh"},
	}) {
		t.Errorf("codex ACPSessionSettings = %v", got)
	}
	if got := codex.BoxEnv("/home/node"); len(got) != 1 || got[0] != "CODEX_SQLITE_HOME=/home/node/.codex-state" {
		t.Errorf("codex BoxEnv = %v, want [CODEX_SQLITE_HOME=/home/node/.codex-state]", got)
	}
	gemini, _ := Get("gemini")
	if got := gemini.ACPSessionSettings(target); !slices.Equal(got, []ACPSessionSetting{{Method: ACPSetModel, Value: "model-x"}}) {
		t.Errorf("gemini ACPSessionSettings = %v", got)
	}
	for _, n := range Names() {
		a, _ := Get(n)
		_ = a.ACPSessionSettings(target)
		_ = a.BoxEnv("/home/node")
		if n == "grok" {
			if settings := a.ACPSessionSettings(target); len(settings) != 0 {
				t.Errorf("grok should force no session settings, got %v", settings)
			}
		}
		if n != "claude" && n != "codex" {
			if env := a.BoxEnv("/home/node"); len(env) != 0 {
				t.Errorf("%s should need no box env, got %v", n, env)
			}
		}
	}
}
