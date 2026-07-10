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

// The loop's closing banner must not claim "verified done" when the review reopened work — which it
// does by moving done tasks back into 10_in_progress/, not 00_todo/. Regression: the check looked at
// 00_todo/ only, so a reopened task in in_progress fell through to the green "verified done".
func TestLoopClosingBanner(t *testing.T) {
	// Reopened INTO in_progress (the bug): not done, and names the count.
	if b := loopClosingBanner(taskCounts{Done: 2, Doing: 3}, 5); !strings.Contains(b, "review reopened") ||
		!strings.Contains(b, "3 tasks") || strings.Contains(b, "verified done") {
		t.Errorf("reopened-into-in_progress banner = %q", b)
	}
	// Reopened into todo: same outcome, singular count.
	if b := loopClosingBanner(taskCounts{Done: 4, Todo: 1}, 4); !strings.Contains(b, "review reopened") ||
		!strings.Contains(b, "1 task") || strings.Contains(b, "verified done") {
		t.Errorf("reopened-into-todo banner = %q", b)
	}
	// Nothing reopened, some blocked on a decision: not done.
	if b := loopClosingBanner(taskCounts{Done: 3, Blocked: 2}, 3); !strings.Contains(b, "blocked on a decision") ||
		strings.Contains(b, "verified done") {
		t.Errorf("blocked banner = %q", b)
	}
	// Clean audit: verified done, unchanged.
	if b := loopClosingBanner(taskCounts{Done: 5}, 5); !strings.Contains(b, "queue verified done") ||
		!strings.Contains(b, "5/5") {
		t.Errorf("clean banner = %q", b)
	}
}

// The loop's exit code lets cron/fleet/CI branch without parsing stderr: 3 iff it stopped with work
// blocked on a human decision and nothing else actionable; 0 for verified-done and review-reopened.
func TestLoopExitCode(t *testing.T) {
	cases := []struct {
		cf   taskCounts
		want int
	}{
		{taskCounts{Done: 3, Blocked: 2}, 3}, // blocked, nothing actionable → 3
		{taskCounts{Done: 5}, 0},             // verified done → 0
		{taskCounts{Done: 3, Doing: 1}, 0},   // audit reopened into in_progress → 0 by design
		{taskCounts{Todo: 2, Blocked: 1}, 0}, // still actionable → 0, not 3
	}
	for _, c := range cases {
		if got := loopExitCode(c.cf); got != c.want {
			t.Errorf("loopExitCode(%+v) = %d, want %d", c.cf, got, c.want)
		}
	}
}

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
	if review := loopReviewPrompt(repo, []string{".agent/tasks"}); !strings.Contains(review, "/home/node/proj/.agent/tasks") {
		t.Errorf("review prompt should name the absolute queue:\n%s", review)
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

// TestLoopPreflightAndReviewFolder: preflight only unblocks blocked/ tasks with an answered
// decision (no code, no commits); the default review does bookkeeping + ONE whole-repo gate and
// reopens by moving the folder (coop isn't in the box), and the fixed context footer carries the
// queue paths + reopen mechanics.
func TestLoopPreflightAndReviewFolder(t *testing.T) {
	pre := loopPreflightPrompt("/repo", []string{".agent/tasks"})
	for _, want := range []string{"do NOT work any task", "no commits", "moving its folder to 00_todo/", "50_blocked/"} {
		if !strings.Contains(pre, want) {
			t.Errorf("preflight prompt missing %q:\n%s", want, pre)
		}
	}
	rev := loopReviewPrompt("/repo", []string{".agent/tasks"})
	// The default prompt: bookkeeping, a SINGLE whole-repo gate (not per task), reopen, no self-fix.
	for _, want := range []string{"99_done/", "a SINGLE time across the WHOLE repo", "NOT once per task", "make no commits"} {
		if !strings.Contains(rev, want) {
			t.Errorf("default review prompt missing %q:\n%s", want, rev)
		}
	}
	// The fixed context footer: the absolute queue path, AGENTS.md, and the reopen mechanic.
	for _, want := range []string{"/repo/.agent/tasks", "/repo/AGENTS.md", "its folder back to 10_in_progress/", "`coop` is NOT installed"} {
		if !strings.Contains(rev, want) {
			t.Errorf("review prompt footer missing %q:\n%s", want, rev)
		}
	}
}

// The review base is a FULL override when .agent/loop/review.md is present (else the built-in
// default); either way the fixed context footer is appended.
func TestLoopReviewPromptOverride(t *testing.T) {
	repo := t.TempDir()
	// Absent → the built-in default leads.
	if rev := loopReviewPrompt(repo, []string{".agent/tasks"}); !strings.HasPrefix(rev, "Review pass") {
		t.Errorf("without review.md the built-in default should lead:\n%s", rev)
	}
	// Present → its trimmed text IS the base and the default is gone; the footer still trails.
	if err := os.MkdirAll(filepath.Join(repo, ".agent", "loop"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "loop", "review.md"), []byte("\nMy custom review: only check the docs.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rev := loopReviewPrompt(repo, []string{".agent/tasks"})
	if !strings.HasPrefix(rev, "My custom review: only check the docs.") {
		t.Errorf("review.md should be the base:\n%s", rev)
	}
	if strings.Contains(rev, "Review pass — verify") {
		t.Errorf("an override should REPLACE the default, not append to it:\n%s", rev)
	}
	if !strings.Contains(rev, "its folder back to 10_in_progress/") {
		t.Errorf("the fixed context footer must trail an override too:\n%s", rev)
	}
}

// .agent/audit.md, when present, is appended to the review prompt so the pass also runs the
// project's own checks; absent, the generated prompt carries no appendix. Kept for backward
// compatibility beside the review.md override.
func TestLoopReviewInstructionsAppended(t *testing.T) {
	repo := t.TempDir()
	// No file → no appendix.
	if rev := loopReviewPrompt(repo, []string{".agent/tasks"}); strings.Contains(rev, "project-specific checks") {
		t.Errorf("review prompt should carry no appendix without .agent/audit.md:\n%s", rev)
	}
	// With the file → its (trimmed) text is appended after the base, before the footer.
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "audit.md"), []byte("\nVerify CHANGELOG.md gained an entry.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rev := loopReviewPrompt(repo, []string{".agent/tasks"})
	if !strings.HasPrefix(rev, "Review pass") {
		t.Errorf("the default review body should still lead:\n%s", rev)
	}
	if !strings.Contains(rev, "project-specific checks (from .agent/audit.md)") ||
		!strings.Contains(rev, "Verify CHANGELOG.md gained an entry.") {
		t.Errorf("review prompt should append .agent/audit.md's text:\n%s", rev)
	}
}

func TestLoopAgent(t *testing.T) {
	if got, explicit, err := loopAgent(nil); err != nil || got != "claude" || explicit {
		t.Errorf("loopAgent(nil) = (%q, explicit=%v, %v), want claude (defaulted)", got, explicit, err)
	}
	for _, ag := range []string{"claude", "codex", "gemini"} {
		if got, explicit, err := loopAgent([]string{ag}); err != nil || got != ag || !explicit {
			t.Errorf("loopAgent(%q) = (%q, explicit=%v, %v), want %q explicit", ag, got, explicit, err, ag)
		}
	}
	if _, _, err := loopAgent([]string{"bogus"}); err == nil {
		t.Error("loopAgent(bogus): want error")
	}
	// More than one agent is a usage error, not silently last-wins.
	if _, _, err := loopAgent([]string{"claude", "codex"}); err == nil {
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
		{[]string{"gemini", "--debug"}, false, "", "", false, false, false, true}, // v3: --debug retired → error
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
		{[]string{"--model", "haiku", "--debug-on-fail"}, false, "claude", "haiku", false, true, false, false},
		{[]string{"--model"}, false, "", "", false, false, false, true},
		// --consult opts iterations into peer consultation, composing with the other flags.
		{[]string{"--consult"}, false, "claude", "", true, false, false, false},
		{[]string{"claude", "--model", "claude-fable-5", "--consult"}, false, "claude", "claude-fable-5", true, false, false, false},
	}
	for _, c := range cases {
		agent, model, _, consult, debug, preflight, err := parseLoopArgs(c.args, c.def)
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
			gov, rest, _ := a.parseGovernor(c.args)
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

// TestExtractProfileAbsentIsEmpty: with no --credential, extractProfile returns "" — the
// caller (loginTo) then resolves the agent's MARKED default. Returning the literal
// "default" here made a bare `coop login claude` re-auth (and keep re-creating) a husk
// profile named "default" while runs used the marked profile's expired token.
func TestExtractProfileAbsentIsEmpty(t *testing.T) {
	profile, rest, err := extractProfile([]string{"claude"})
	if err != nil || profile != "" || !slices.Equal(rest, []string{"claude"}) {
		t.Errorf("extractProfile(claude) = (%q, %v, %v), want (\"\", [claude], nil)", profile, rest, err)
	}
	if profile, _, _ := extractProfile([]string{"claude", "--credential", "work"}); profile != "work" {
		t.Errorf("explicit --credential = %q, want work", profile)
	}
	// --profile is retired — no longer a coop flag, so it's left untouched (a plain arg), not intercepted.
	if _, rest, err := extractProfile([]string{"claude", "--profile", "work"}); err != nil || !slices.Equal(rest, []string{"claude", "--profile", "work"}) {
		t.Errorf("extractProfile should leave the retired --profile untouched, got rest=%v err=%v", rest, err)
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
		{"space form", []string{"--credential", "work", "-p", "hi"}, "work", []string{"-p", "hi"}, false},
		{"equals form", []string{"--credential=work"}, "work", nil, false},
		{"missing value", []string{"--credential"}, "", nil, true},
		// --profile is retired: no longer a coop flag, so it's left in rest (a plain token), not errored.
		{"retired --profile is a plain token", []string{"--profile", "work"}, "", []string{"--profile", "work"}, false},
		// coop reads its flags only before --; the agent's own --profile passes through verbatim.
		{"passthrough after --", []string{"--", "--profile", "codexprof"}, "", []string{"--", "--profile", "codexprof"}, false},
		{"coop credential then passthrough", []string{"--credential", "work", "--", "--profile", "codexprof"},
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
	// A nonexistent credential must error before any box work, so a typo never silently creates a husk.
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	code, err := a.launchAgent("claude", []string{"--credential", "ghost", "-p", "hi"})
	if code != 2 || err == nil {
		t.Fatalf("launchAgent --credential ghost = (%d, %v), want 2 + error", code, err)
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should name the bad credential: %v", err)
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

// --credential is wired into every agent-launch path; a nonexistent credential must fail fast
// (before any box/Docker work) on fusion and acp too, not just a plain agent run.
func TestRunProfileWiringRejectsUnknown(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	if code, err := a.cmdFusion([]string{"claude", "--credential", "ghost"}); code != 2 || err == nil {
		t.Errorf("cmdFusion --credential ghost = (%d, %v), want 2 + error", code, err)
	}
	if code, err := a.cmdACP([]string{"claude", "--credential", "ghost"}); code != 2 || err == nil {
		t.Errorf("cmdACP --credential ghost = (%d, %v), want 2 + error", code, err)
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
	if code, err := a.loginTo("claude", "../../escape"); code != 2 || err == nil || !strings.Contains(err.Error(), "invalid credential name") {
		t.Errorf("loginTo bad credential = (%d, %v), want (2, invalid credential name)", code, err)
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

// The top-level help lists every fleet verb on its own row (like the fork rows), so none
// is hidden from the main help.
func TestTopLevelListsAllGroupVerbs(t *testing.T) {
	h := helpText(&config.Config{})
	for _, verb := range []string{"init", "up", "down", "watch", "prune"} {
		if !strings.Contains(h, "coop fleet "+verb) {
			t.Errorf("top-level help should list `coop fleet %s` as its own row", verb)
		}
	}
}

// migrateFlatVaults retires a legacy flat login into profiles/default, leaves an already-migrated
// agent alone, skips agents never used (no empty dir left behind), and is idempotent.
func TestMigrateFlatVaults(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}

	// claude: a flat vault — login sits directly in claude/, no profiles/ yet.
	claudeFlat := filepath.Join(dir, "claude")
	if err := os.MkdirAll(claudeFlat, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeFlat, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// gemini: already on the named-profile layout — must be left exactly as-is.
	geminiWork := filepath.Join(dir, "gemini", "profiles", "work")
	if err := os.MkdirAll(geminiWork, 0o700); err != nil {
		t.Fatal(err)
	}
	// codex: never used (no dir at all).

	migrateFlatVaults(cfg)

	// claude's flat login moved into profiles/default; the flat path no longer holds it.
	if !pathExists(filepath.Join(dir, "claude", "profiles", "default", ".credentials.json")) {
		t.Error("flat claude login was not migrated into profiles/default")
	}
	if pathExists(filepath.Join(claudeFlat, ".credentials.json")) {
		t.Error("flat claude login still present at the old path after migration")
	}
	// gemini's existing profile is untouched and no stray default was invented for it.
	if !pathExists(geminiWork) {
		t.Error("existing gemini profile was disturbed by the migration")
	}
	if pathExists(filepath.Join(dir, "gemini", "profiles", "default")) {
		t.Error("migration wrongly created a default profile for an already-migrated agent")
	}
	// codex was never used → no empty dir litters its (nonexistent) home.
	if pathExists(filepath.Join(dir, "codex")) {
		t.Error("migration created a dir for an agent that was never used")
	}

	// Idempotent: a second run leaves the migrated login exactly where it is.
	migrateFlatVaults(cfg)
	if !pathExists(filepath.Join(dir, "claude", "profiles", "default", ".credentials.json")) {
		t.Error("second migrateFlatVaults disturbed the migrated claude login")
	}
}

// TestPromptLine: coop prompt's line shows non-zero segments only, "·"-separated, in a fixed
// order (todo, doing, blocked, looping, forks); "" when idle so an embedding prompt stays clean.
func TestPromptLine(t *testing.T) {
	if got := promptLine(taskCounts{}, 0, 0); got != "" {
		t.Errorf("idle should be empty, got %q", got)
	}
	if got := promptLine(taskCounts{Done: 9}, 0, 0); got != "" {
		t.Errorf("done-only isn't actionable state — should be empty, got %q", got)
	}
	if got := promptLine(taskCounts{Todo: 3, Blocked: 1}, 2, 1); got != "3 todo · 1 blocked · 1 looping · 2 forks" {
		t.Errorf("got %q", got)
	}
	if got := promptLine(taskCounts{Doing: 2}, 1, 0); got != "2 doing · 1 fork" { // singular fork
		t.Errorf("got %q", got)
	}
}
