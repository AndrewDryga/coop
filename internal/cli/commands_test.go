package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestLoopAgent(t *testing.T) {
	if got, err := loopAgent(nil); err != nil || got != "claude" {
		t.Errorf("loopAgent(nil) = (%q, %v), want claude", got, err)
	}
	for _, ag := range []string{"claude", "codex", "gemini"} {
		if got, err := loopAgent([]string{ag}); err != nil || got != ag {
			t.Errorf("loopAgent(%q) = (%q, %v), want %q", ag, got, err, ag)
		}
	}
	if _, err := loopAgent([]string{"bogus"}); err == nil {
		t.Error("loopAgent(bogus): want error")
	}
}

func TestParseLoopArgs(t *testing.T) {
	cases := []struct {
		args      []string
		wantAgent string
		wantDebug bool
		wantErr   bool
	}{
		{nil, "claude", false, false},
		{[]string{"codex"}, "codex", false, false},
		{[]string{"--debug-on-fail"}, "claude", true, false},
		{[]string{"gemini", "--debug"}, "gemini", true, false},
		{[]string{"--debug-on-fail", "codex"}, "codex", true, false},
		{[]string{"bogus"}, "", false, true},
	}
	for _, c := range cases {
		agent, debug, err := parseLoopArgs(c.args)
		if (err != nil) != c.wantErr {
			t.Errorf("parseLoopArgs(%v) err=%v, wantErr=%v", c.args, err, c.wantErr)
			continue
		}
		if !c.wantErr && (agent != c.wantAgent || debug != c.wantDebug) {
			t.Errorf("parseLoopArgs(%v) = (%q, %v), want (%q, %v)", c.args, agent, debug, c.wantAgent, c.wantDebug)
		}
	}
}

func TestParseGovernor(t *testing.T) {
	a := &app{cfg: &config.Config{FusionGovernor: "codex"}}
	cases := []struct {
		name     string
		args     []string
		wantGov  string
		wantRest []string
	}{
		{"default governor, no args", nil, "codex", nil},
		{"positional governor", []string{"claude"}, "claude", nil},
		{"positional governor + passthrough", []string{"gemini", "exec"}, "gemini", []string{"exec"}},
		{"passthrough args keep order", []string{"exec", "foo"}, "codex", []string{"exec", "foo"}},
		{"-- passes the rest through verbatim", []string{"claude", "--", "-p", "hi"}, "claude", []string{"-p", "hi"}},
		{"--governor is gone — treated as passthrough now", []string{"--governor", "claude"}, "codex", []string{"--governor", "claude"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gov, rest := a.parseGovernor(c.args)
			if gov != c.wantGov {
				t.Errorf("governor = %q, want %q", gov, c.wantGov)
			}
			if !slices.Equal(rest, c.wantRest) {
				t.Errorf("rest = %v, want %v", rest, c.wantRest)
			}
		})
	}
}

func TestExtractConsult(t *testing.T) {
	cases := []struct {
		args     []string
		want     bool
		wantRest []string
	}{
		{nil, false, nil},
		{[]string{"-p", "hi"}, false, []string{"-p", "hi"}},
		{[]string{"--consult"}, true, nil},
		{[]string{"--consult", "-p", "hi"}, true, []string{"-p", "hi"}},
		{[]string{"-p", "hi", "--consult"}, true, []string{"-p", "hi"}},
	}
	for _, c := range cases {
		got, rest := extractConsult(c.args)
		if got != c.want || !slices.Equal(rest, c.wantRest) {
			t.Errorf("extractConsult(%v) = (%v, %v), want (%v, %v)", c.args, got, rest, c.want, c.wantRest)
		}
	}
}

func TestParseServices(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"none", nil},
		{"postgres", []string{"postgres"}},
		{"postgres,redis", []string{"postgres", "redis"}},
		{"redis postgres", []string{"redis", "postgres"}}, // input order preserved
		{"postgres,postgres", []string{"postgres"}},       // de-duped
		{"mongo", nil}, // unknown dropped
		{"postgres,mongo", []string{"postgres"}},
	}
	for _, c := range cases {
		if got := parseServices(c.in); !slices.Equal(got, c.want) {
			t.Errorf("parseServices(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestWriteMCPStub(t *testing.T) {
	mcp := filepath.Join(t.TempDir(), "agents", "mcp.json") // parent dir doesn't exist yet
	a := &app{cfg: &config.Config{MCPFile: mcp}}

	// Seeds an empty, well-shaped stub (creating the config dir) when absent.
	if err := a.writeMCPStub(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(mcp)
	if err != nil {
		t.Fatalf("stub not written: %v", err)
	}
	var f struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("stub is not valid JSON: %v\n%s", err, data)
	}
	if f.MCPServers == nil || len(f.MCPServers) != 0 {
		t.Errorf("stub should carry an empty mcpServers object, got %v", f.MCPServers)
	}
	// The stub is inactive end-to-end — it must not flip MCP on for runs.
	if a.cfg.MCPActive() {
		t.Error("the empty stub must leave MCPActive false")
	}

	// Idempotent: a user's filled-in config is never clobbered.
	os.WriteFile(mcp, []byte(`{"mcpServers":{"fs":{"command":"x"}}}`), 0o600)
	if err := a.writeMCPStub(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(mcp); !strings.Contains(string(b), `"fs"`) {
		t.Error("writeMCPStub clobbered an existing mcp.json")
	}

	// No MCPFile configured → a harmless no-op (tests build cfgs without one).
	if err := (&app{cfg: &config.Config{}}).writeMCPStub(); err != nil {
		t.Errorf("empty MCPFile should be a no-op, got %v", err)
	}
}

func TestInitNextSteps(t *testing.T) {
	// Bare repo (no Dockerfile.agent, no services) → just the edit-then-loop step.
	repo := t.TempDir()
	if got := initNextSteps(repo, nil); len(got) != 1 || !strings.Contains(got[0], "coop loop") {
		t.Errorf("bare repo steps = %v, want only the loop step", got)
	}
	// A scaffolded Dockerfile.agent + sibling services → build, up (naming the services), loop.
	if err := os.WriteFile(filepath.Join(repo, "Dockerfile.agent"), []byte("FROM x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := initNextSteps(repo, []string{"postgres", "redis"})
	if len(got) != 3 {
		t.Fatalf("want 3 steps, got %v", got)
	}
	if !strings.Contains(got[0], "coop build") ||
		!strings.Contains(got[1], "coop up") || !strings.Contains(got[1], "postgres + redis") ||
		!strings.Contains(got[2], "coop loop") {
		t.Errorf("steps wrong or out of order: %v", got)
	}
}

func TestExtractSupervise(t *testing.T) {
	got, rest := extractSupervise([]string{"claude", "--supervise"})
	if !got || len(rest) != 1 || rest[0] != "claude" {
		t.Fatalf("with flag: supervise=%v rest=%v", got, rest)
	}
	got, rest = extractSupervise([]string{"fusion", "claude"})
	if got || len(rest) != 2 {
		t.Fatalf("without flag: supervise=%v rest=%v", got, rest)
	}
}

// `coop run` with no command is a usage error (it doesn't default to an agent), and `coop run
// --help`/-h prints run's own page — neither enters the box (which would exec `--help` and crash).
func TestCmdRunMetaCases(t *testing.T) {
	a := &app{cfg: &config.Config{}} // meta-cases return before runInBox, so no runtime needed
	if code, err := a.cmdRun(nil); code != 2 || err == nil {
		t.Errorf("cmdRun(nil) = (%d, %v), want (2, usage error)", code, err)
	}
	if code, err := a.cmdRun([]string{"--"}); code != 2 || err == nil {
		t.Errorf("cmdRun(--) = (%d, %v), want (2, usage error)", code, err)
	}
	for _, h := range []string{"--help", "-h"} {
		old := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		code, err := a.cmdRun([]string{h})
		_ = w.Close()
		os.Stdout = old
		out, _ := io.ReadAll(r)
		if code != 0 || err != nil {
			t.Errorf("cmdRun(%q) = (%d, %v), want (0, nil)", h, code, err)
		}
		if !strings.Contains(string(out), "coop run — run a raw command") {
			t.Errorf("cmdRun(%q) should print run's help, got:\n%s", h, out)
		}
	}
}

// `coop login` requires the agent (no silent default that opens a browser) and refuses a
// non-interactive stdin instead of blocking on the paste-code prompt forever.
func TestLoginRequiresAgentAndTTY(t *testing.T) {
	// Force a non-terminal stdin so the tty guard is deterministic.
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	saved := os.Stdin
	os.Stdin = devnull
	defer func() { os.Stdin = saved }()

	a := &app{cfg: &config.Config{}}
	if code, err := a.cmdLogin(nil); code != 2 || err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("cmdLogin(nil) = (%d, %v), want (2, usage error)", code, err)
	}
	if code, err := a.loginTo("claude", ""); code != 2 || err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Errorf("loginTo(claude) non-tty = (%d, %v), want (2, interactive-terminal error)", code, err)
	}
	if code, err := a.loginTo("bogus", ""); code != 2 || err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("loginTo(bogus) = (%d, %v), want (2, unknown agent — before the tty check)", code, err)
	}
}

// TestStrictFlagParsing: value-bearing coop flags reject a missing value or a stray arg up
// front (exit 2) instead of silently falling back to a default or ignoring the typo. These all
// return before any runtime/scaffold work, so a bare app suffices.
func TestStrictFlagParsing(t *testing.T) {
	a := &app{cfg: &config.Config{}}
	cases := []struct {
		name string
		fn   func() (int, error)
	}{
		{"login --profile no value", func() (int, error) { return a.cmdLogin([]string{"claude", "--profile"}) }},
		{"login stray arg", func() (int, error) { return a.cmdLogin([]string{"claude", "extra"}) }},
		{"init --stack no value", func() (int, error) { return a.cmdInit([]string{"--stack"}) }},
		{"init --services no value", func() (int, error) { return a.cmdInit([]string{"--services"}) }},
		{"init unknown flag", func() (int, error) { return a.cmdInit([]string{"--bogus"}) }},
	}
	for _, c := range cases {
		if code, err := c.fn(); code != 2 || err == nil {
			t.Errorf("%s = (%d, %v), want (2, error)", c.name, code, err)
		}
	}
}

// The top-level help documents coop's --consult wrapper flag and stops claiming `coop <agent>
// --help` shows coop's flags (it forwards to the agent).
func TestHelpDocumentsConsultAndAgentHelp(t *testing.T) {
	h := helpText(&config.Config{})
	if !strings.Contains(h, "--consult") {
		t.Error("top-level help should document the --consult wrapper flag")
	}
	if !strings.Contains(h, "--help is the agent's own") {
		t.Error("footer should note that for an agent, --help is the agent's own")
	}
}

// The top-level fleet/pool summary lines must list every verb (init/split for fleet and rm/clear
// for pool were omitted, hiding `coop fleet init` etc. from the main help).
func TestTopLevelListsAllGroupVerbs(t *testing.T) {
	h := helpText(&config.Config{})
	if !strings.Contains(h, "coop fleet init|up|down|split|watch|prune") {
		t.Error("top-level fleet row should list every fleet verb (init/split were missing)")
	}
	if !strings.Contains(h, "coop pool add|rm|clear") {
		t.Error("top-level pool row should list every pool verb (rm/clear were missing)")
	}
}
