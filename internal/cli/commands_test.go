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

// The loop prompts must name the queue AND AGENTS.md as absolute in-box paths: gemini's
// read_file rejects a relative path, so a relative ".agent/tasks" left gemini/codex fleet forks
// unable to read their own queue (claude resolved it against cwd and was fine).
func TestLoopPromptsUseAbsolutePaths(t *testing.T) {
	repo := "/home/node/proj"
	work := loopWorkPrompt(repo, []string{".agent/tasks"})
	for _, want := range []string{"/home/node/proj/.agent/tasks", "/home/node/proj/AGENTS.md"} {
		if !strings.Contains(work, want) {
			t.Errorf("work prompt missing absolute %q:\n%s", want, work)
		}
	}
	// Several queues (a monorepo's per-component trees) are all listed, each absolute.
	multi := loopWorkPrompt(repo, []string{"portal/.agent/tasks", "runner/.agent/tasks"})
	for _, want := range []string{"/home/node/proj/portal/.agent/tasks", "/home/node/proj/runner/.agent/tasks"} {
		if !strings.Contains(multi, want) {
			t.Errorf("multi-queue work prompt missing %q:\n%s", want, multi)
		}
	}
	if audit := loopAuditPrompt(repo, []string{".agent/tasks"}); !strings.Contains(audit, "/home/node/proj/.agent/tasks") {
		t.Errorf("audit prompt should name the absolute queue:\n%s", audit)
	}
}

// coop's "--" separator must be consumed, not forwarded to the agent: `coop claude -- -p x` must
// reach the agent as `-p x`, not `-- -p x` (which the agent reads as positional, dropping the flag).
func TestDropDashDash(t *testing.T) {
	for _, c := range []struct{ in, want []string }{
		{[]string{"-p", "x"}, []string{"-p", "x"}},                           // no --: unchanged
		{[]string{"--", "-p", "x"}, []string{"-p", "x"}},                     // leading -- stripped
		{[]string{"a", "--", "b", "--", "c"}, []string{"a", "b", "--", "c"}}, // only the first --
		{[]string{"--"}, []string{}},                                         // lone --
	} {
		if got := dropDashDash(c.in); !slices.Equal(got, c.want) {
			t.Errorf("dropDashDash(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestLoopWorkPromptFolderWorkflow: the work prompt drives the folder queue — claim/done/block by
// moving the folder (coop isn't in the box), resume an interrupted in_progress task from its
// state.md + the git diff, finalize state.md (never blank it), and work ONE task per run then stop
// so the loop re-invokes a fresh agent for the next — not one agent draining the queue itself.
func TestLoopWorkPromptFolderWorkflow(t *testing.T) {
	work := loopWorkPrompt("/repo", []string{".agent/tasks"})
	for _, want := range []string{
		"is NOT installed", "moving its folder into 10_in_progress/", "into 99_done/", "into 50_blocked/",
		"10_in_progress/", "00_todo/", "git status", "git diff",
		"state.md", "resume note", "final step", "finished state",
		"Work exactly ONE task per run", "the loop's job, not yours",
	} {
		if !strings.Contains(work, want) {
			t.Errorf("folder work prompt missing %q:\n%s", want, work)
		}
	}
}

// TestLoopPreflightAndAuditFolder: preflight only unblocks blocked/ tasks with an answered
// decision (no code, no commits); audit re-checks the done/ archive against git and reopens by
// moving the folder (coop isn't in the box).
func TestLoopPreflightAndAuditFolder(t *testing.T) {
	pre := loopPreflightPrompt("/repo", []string{".agent/tasks"})
	for _, want := range []string{"do NOT work any task", "no commits", "moving its folder to 00_todo/", "50_blocked/"} {
		if !strings.Contains(pre, want) {
			t.Errorf("preflight prompt missing %q:\n%s", want, pre)
		}
	}
	aud := loopAuditPrompt("/repo", []string{".agent/tasks"})
	if !strings.Contains(aud, "99_done/") || !strings.Contains(aud, "moving its folder back to 10_in_progress/") {
		t.Errorf("audit prompt should re-check 99_done/ and reopen by moving the folder:\n%s", aud)
	}
}

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
	// More than one agent is a usage error, not silently last-wins.
	if _, err := loopAgent([]string{"claude", "codex"}); err == nil {
		t.Error("loopAgent(claude codex): want error for more than one agent")
	}
}

func TestParseLoopArgs(t *testing.T) {
	cases := []struct {
		args          []string
		def           bool // COOP_PREFLIGHT default
		wantAgent     string
		wantModel     string
		wantConsult   bool
		wantDebug     bool
		wantPreflight bool
		wantErr       bool
	}{
		{nil, false, "claude", "", false, false, false, false},
		{[]string{"codex"}, false, "codex", "", false, false, false, false},
		{[]string{"--debug-on-fail"}, false, "claude", "", false, true, false, false},
		{[]string{"gemini", "--debug"}, false, "gemini", "", false, true, false, false},
		{[]string{"--debug-on-fail", "codex"}, false, "codex", "", false, true, false, false},
		{[]string{"bogus"}, false, "", "", false, false, false, true},
		// preflight: default off, --preflight turns it on, --no-preflight overrides a default-on.
		{[]string{"--preflight"}, false, "claude", "", false, false, true, false},
		{[]string{"codex", "--preflight"}, false, "codex", "", false, false, true, false},
		{nil, true, "claude", "", false, false, true, false},                         // COOP_PREFLIGHT=1 default
		{[]string{"--no-preflight"}, true, "claude", "", false, false, false, false}, // flag overrides default-on
		// --model pins the loop's model, space or equals form; a bare --model is an error.
		{[]string{"--model", "haiku"}, false, "claude", "haiku", false, false, false, false},
		{[]string{"codex", "--model=gpt-5"}, false, "codex", "gpt-5", false, false, false, false},
		{[]string{"--model", "haiku", "--debug"}, false, "claude", "haiku", false, true, false, false},
		{[]string{"--model"}, false, "", "", false, false, false, true},
		// --consult opts iterations into peer consultation, composing with the other flags.
		{[]string{"--consult"}, false, "claude", "", true, false, false, false},
		{[]string{"claude", "--model", "claude-fable-5", "--consult"}, false, "claude", "claude-fable-5", true, false, false, false},
	}
	for _, c := range cases {
		agent, model, consult, debug, preflight, err := parseLoopArgs(c.args, c.def)
		if (err != nil) != c.wantErr {
			t.Errorf("parseLoopArgs(%v) err=%v, wantErr=%v", c.args, err, c.wantErr)
			continue
		}
		if !c.wantErr && (agent != c.wantAgent || model != c.wantModel || consult != c.wantConsult || debug != c.wantDebug || preflight != c.wantPreflight) {
			t.Errorf("parseLoopArgs(%v, def=%v) = (%q, model=%q, consult=%v, debug=%v, preflight=%v), want (%q, %q, %v, %v, %v)",
				c.args, c.def, agent, model, consult, debug, preflight, c.wantAgent, c.wantModel, c.wantConsult, c.wantDebug, c.wantPreflight)
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
		// A SECOND agent token is NOT swallowed as the governor — only the first is; the rest passes through.
		{"second agent token passes through", []string{"codex", "gemini"}, "codex", []string{"gemini"}},
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
		// After --, a --consult is the agent's own arg, not coop's — passed through verbatim.
		{[]string{"--", "--consult"}, false, []string{"--", "--consult"}},
		{[]string{"--consult", "--", "--consult"}, true, []string{"--", "--consult"}},
	}
	for _, c := range cases {
		got, rest := extractConsult(c.args)
		if got != c.want || !slices.Equal(rest, c.wantRest) {
			t.Errorf("extractConsult(%v) = (%v, %v), want (%v, %v)", c.args, got, rest, c.want, c.wantRest)
		}
	}
}

// TestExtractProfileAbsentIsEmpty: with no --profile, extractProfile returns "" — the
// caller (loginTo) then resolves the agent's MARKED default. Returning the literal
// "default" here made a bare `coop login claude` re-auth (and keep re-creating) a husk
// profile named "default" while runs used the marked profile's expired token.
func TestExtractProfileAbsentIsEmpty(t *testing.T) {
	profile, rest, err := extractProfile([]string{"claude"})
	if err != nil || profile != "" || !slices.Equal(rest, []string{"claude"}) {
		t.Errorf("extractProfile(claude) = (%q, %v, %v), want (\"\", [claude], nil)", profile, rest, err)
	}
	if profile, _, _ := extractProfile([]string{"claude", "--profile", "work"}); profile != "work" {
		t.Errorf("explicit --profile = %q, want work", profile)
	}
}

func TestExtractRunProfile(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantProfile string
		wantRest    []string
		wantErr     bool
	}{
		{"none", []string{"-p", "hi"}, "", []string{"-p", "hi"}, false},
		{"space form", []string{"--profile", "work", "-p", "hi"}, "work", []string{"-p", "hi"}, false},
		{"equals form", []string{"--profile=work"}, "work", nil, false},
		{"missing value", []string{"--profile"}, "", nil, true},
		// coop reads --profile only before --; the agent's own --profile passes through verbatim.
		{"passthrough after --", []string{"--", "--profile", "codexprof"}, "", []string{"--", "--profile", "codexprof"}, false},
		{"coop profile then passthrough", []string{"--profile", "work", "--", "--profile", "codexprof"},
			"work", []string{"--", "--profile", "codexprof"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			profile, rest, err := extractRunProfile(c.args)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if profile != c.wantProfile || !slices.Equal(rest, c.wantRest) {
				t.Errorf("extractRunProfile(%v) = (%q, %v), want (%q, %v)", c.args, profile, rest, c.wantProfile, c.wantRest)
			}
		})
	}
}

func TestLaunchAgentRejectsUnknownProfile(t *testing.T) {
	// A nonexistent profile must error before any box work, so a typo never silently creates a husk.
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	code, err := a.launchAgent("claude", []string{"--profile", "ghost", "-p", "hi"})
	if code != 2 || err == nil {
		t.Fatalf("launchAgent --profile ghost = (%d, %v), want 2 + error", code, err)
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should name the bad profile: %v", err)
	}
}

func TestSelectRunProfile(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	work := cfg.AgentProfileDir("claude", "work") // signed in
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".credentials.json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.AgentProfileDir("claude", "bare"), 0o700); err != nil { // exists, no creds
		t.Fatal(err)
	}
	a := &app{cfg: cfg}

	if err := a.selectRunProfile("claude", ""); err != nil {
		t.Errorf("empty profile should be a no-op: %v", err)
	}
	if err := a.selectRunProfile("claude", "ghost"); err == nil {
		t.Error("unknown profile should error")
	}
	if err := a.selectRunProfile("claude", "work"); err != nil {
		t.Fatalf("signed-in profile should select: %v", err)
	}
	if got := cfg.AgentDir("claude"); got != work {
		t.Errorf("active dir = %q, want %q", got, work)
	}
	if err := a.selectRunProfile("claude", "bare"); err != nil {
		t.Errorf("an existing but unsigned profile should select with a note, not error: %v", err)
	}
}

// --profile is wired into every agent-launch path; a nonexistent profile must fail fast (before any
// box/Docker work) on fusion and acp too, not just a plain agent run.
func TestRunProfileWiringRejectsUnknown(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	if code, err := a.cmdFusion([]string{"claude", "--profile", "ghost"}); code != 2 || err == nil {
		t.Errorf("cmdFusion --profile ghost = (%d, %v), want 2 + error", code, err)
	}
	if code, err := a.cmdACP([]string{"claude", "--profile", "ghost"}); code != 2 || err == nil {
		t.Errorf("cmdACP --profile ghost = (%d, %v), want 2 + error", code, err)
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
	// In a git repo (no Dockerfile.agent, no services) → just the edit-then-loop step.
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := initNextSteps(repo, nil); len(got) != 1 || !strings.Contains(got[0], "coop loop") {
		t.Errorf("git repo steps = %v, want only the loop step", got)
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
	// Outside a git repo, the first step is `git init` — forks and the loop need one.
	if steps := initNextSteps(t.TempDir(), nil); len(steps) == 0 || !strings.Contains(steps[0], "git init") {
		t.Errorf("non-git repo should lead with `git init`, got %v", steps)
	}
}

// `coop acp` takes an agent (or fusion [governor]) and coop flags only — a leftover token must be a
// usage error (exit 2), not silently ignored. Returns before any box/Docker work.
func TestCmdACPRejectsExtraArgs(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	for _, args := range [][]string{
		{"claude", "foo"},
		{"claude", "--nope"},
		{"fusion", "claude", "junk"},
	} {
		if code, err := a.cmdACP(args); code != 2 || err == nil {
			t.Errorf("cmdACP(%v) = (%d, %v), want (2, usage error)", args, code, err)
		}
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
	// After --, a --supervise is the inner agent's own arg — not consumed by coop.
	got, rest = extractSupervise([]string{"claude", "--", "--supervise"})
	if got || !slices.Equal(rest, []string{"claude", "--", "--supervise"}) {
		t.Fatalf("after --: supervise=%v rest=%v, want false + verbatim", got, rest)
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

func TestValidProfileName(t *testing.T) {
	for _, ok := range []string{"default", "work", "personal_backup", "p1", "acc.2"} {
		if !validProfileName(ok) {
			t.Errorf("%q should be a valid profile name", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "../../x", "a/b", `a\b`, "-x"} {
		if validProfileName(bad) {
			t.Errorf("%q should be rejected (traversal/collision/flag-like)", bad)
		}
	}
}

func TestLoginRejectsBadProfileName(t *testing.T) {
	// A traversal name must be rejected before any vault/dir work — and before the tty check, so it
	// fails the same way piped or at a terminal.
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	if code, err := a.loginTo("claude", "../../escape"); code != 2 || err == nil || !strings.Contains(err.Error(), "invalid profile name") {
		t.Errorf("loginTo bad profile = (%d, %v), want (2, invalid profile name)", code, err)
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
