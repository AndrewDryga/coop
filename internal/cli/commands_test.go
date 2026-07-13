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

// The loop's closing banner must not claim "verified done" when the signoff reopened work — which it
// does by moving done tasks back into 10_in_progress/, not 00_todo/. Regression: the check looked at
// 00_todo/ only, so a reopened task in in_progress fell through to the green "verified done".
func TestLoopClosingBanner(t *testing.T) {
	// Reopened INTO in_progress (the bug): not done, and names the count.
	if b := loopClosingBanner(taskCounts{Done: 2, Doing: 3}, 5); !strings.Contains(b, "signoff reopened") ||
		!strings.Contains(b, "3 tasks") || strings.Contains(b, "verified done") {
		t.Errorf("reopened-into-in_progress banner = %q", b)
	}
	// Reopened into todo: same outcome, singular count.
	if b := loopClosingBanner(taskCounts{Done: 4, Todo: 1}, 4); !strings.Contains(b, "signoff reopened") ||
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
	if review := loopSignoffPrompt(repo, []string{".agent/tasks"}, ""); !strings.Contains(review, "/home/node/proj/.agent/tasks") {
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
		// Reference the commit by its stable trailer, not its volatile SHA (coop re-signs on the host).
		"Coop-Task: <task-id>` trailer", "NOT its SHA", "re-signs your commit",
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
	pre := loopPreflightPrompt("/repo", []string{".agent/tasks"}, "")
	for _, want := range []string{"do NOT work any task", "no commits", "moving its folder to 00_todo/", "50_blocked/"} {
		if !strings.Contains(pre, want) {
			t.Errorf("preflight prompt missing %q:\n%s", want, pre)
		}
	}
	rev := loopSignoffPrompt("/repo", []string{".agent/tasks"}, "")
	// The demanding default prompt: a senior reviewer's bar — every acceptance criterion met, the
	// repo's rules obeyed, the FAILURE path tested, the change polished (docs updated), a SINGLE
	// whole-repo gate, reopen-by-moving, and no self-fix/commits.
	for _, want := range []string{
		"SENIOR REVIEWER", "99_done/",
		"acceptance criterion",                      // 1. meets its goal
		".agent/rules",                              // 2. follows the standards
		"FAILURE/edge path",                         // 3. tested for real
		"docs/README/CHANGELOG",                     // 4. polished
		"ONCE across the WHOLE repo (not per task)", // single whole-repo gate
		"MOVING its folder back to 10_in_progress/", // reopen by moving
		"THE MOMENT you decide",                     // execute reopens immediately, never batched
		"make no commits",
	} {
		if !strings.Contains(rev, want) {
			t.Errorf("default review prompt missing %q:\n%s", want, rev)
		}
	}
	// The fixed context footer: the absolute queue path, AGENTS.md, and the reopen mechanic —
	// including execute-immediately, so it binds even under a custom review.md override.
	for _, want := range []string{"/repo/.agent/tasks", "/repo/AGENTS.md", "its folder back to 10_in_progress/", "`coop` is NOT installed", "Execute every reopen IMMEDIATELY"} {
		if !strings.Contains(rev, want) {
			t.Errorf("review prompt footer missing %q:\n%s", want, rev)
		}
	}
}

// The built-in senior review ALWAYS leads; .agent/loop.yaml signoff.prompt only APPENDS to it
// (never replaces it). Either way the fixed context footer trails.
func TestLoopReviewPromptAppend(t *testing.T) {
	repo := t.TempDir()
	// No append → the built-in default, no appendix.
	if rev := loopSignoffPrompt(repo, []string{".agent/tasks"}, ""); !strings.HasPrefix(rev, "Review pass") || strings.Contains(rev, "project-specific checks") {
		t.Errorf("empty append → built-in only, no appendix:\n%s", rev)
	}
	// With an append → the built-in leads, then the appended text, then the footer.
	rev := loopSignoffPrompt(repo, []string{".agent/tasks"}, "- Verify CHANGELOG.md gained an entry.")
	if !strings.HasPrefix(rev, "Review pass") || !strings.Contains(rev, "SENIOR REVIEWER") {
		t.Errorf("the built-in review must always lead (append never replaces):\n%s", rev)
	}
	if !strings.Contains(rev, "project-specific checks") || !strings.Contains(rev, "Verify CHANGELOG.md gained an entry.") {
		t.Errorf("signoff.prompt text should be appended:\n%s", rev)
	}
	if !strings.Contains(rev, "its folder back to 10_in_progress/") {
		t.Errorf("the fixed context footer must trail:\n%s", rev)
	}
}

// TestLoopBetweenPrompt: a header names the just-finished task(s) — the audit's subject, so the
// prompt never asks the agent to guess "the most recent" — then between.prompt (SET, not read
// from a file), then the fixed footer.
func TestLoopBetweenPrompt(t *testing.T) {
	finished := []string{"2026-07-11-fix-timer — /repo/.agent/tasks/99_done/2026-07-11-fix-timer"}
	p := loopBetweenPrompt("/repo", []string{".agent/tasks"}, "\nAudit the task named above.\n", finished, nil)
	if !strings.HasPrefix(p, "The task(s) the last iteration just completed") || !strings.Contains(p, "2026-07-11-fix-timer — ") {
		t.Errorf("the header must name the finished task:\n%s", p)
	}
	if !strings.Contains(p, "Audit the task named above.") {
		t.Errorf("between.prompt text should follow the header:\n%s", p)
	}
	if !strings.Contains(p, "its folder back to 10_in_progress/") {
		t.Errorf("the fixed context footer must trail the between prompt:\n%s", p)
	}
	// A gate-defining change adds a PROTECTED CHANGE note naming the file.
	if pg := loopBetweenPrompt("/repo", []string{".agent/tasks"}, "Audit.", finished, []string{"Makefile"}); !strings.Contains(pg, "PROTECTED CHANGE") || !strings.Contains(pg, "Makefile") {
		t.Errorf("a gate-file change should add the protected-change note:\n%s", pg)
	}
	// No identified task (defensive) → no header, prompt leads.
	if p := loopBetweenPrompt("/repo", []string{".agent/tasks"}, "Audit.", nil, nil); !strings.HasPrefix(p, "Audit.") {
		t.Errorf("without finished tasks the prompt should lead:\n%s", p)
	}
}

// TestNewlyFinished: the before/after done-set diff names exactly what an iteration completed,
// sorted; taskIDsOf strips the dirs for the banner.
func TestNewlyFinished(t *testing.T) {
	before := map[string]string{"a": "/q/99_done/a"}
	now := map[string]string{"a": "/q/99_done/a", "c": "/q/99_done/c", "b": "/q/99_done/b"}
	got := newlyFinished(before, now)
	want := []string{"b — /q/99_done/b", "c — /q/99_done/c"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("newlyFinished = %v, want %v", got, want)
	}
	if ids := taskIDsOf(got); ids[0] != "b" || ids[1] != "c" {
		t.Errorf("taskIDsOf = %v, want [b c]", ids)
	}
	if extra := newlyFinished(now, now); len(extra) != 0 {
		t.Errorf("no change should mean no finished tasks, got %v", extra)
	}
}

// TestReviewLadder: a review stage's ladder keeps each rung's PROVIDER, model, effort, and the
// fallback rungs — the fix for stepModel, which kept only (model, effort) off the first rung and
// dropped the provider, so a claude-led run's `codex:…` signoff resolved to `claude --model
// <a-codex-model>` and the cross-vendor reviewer was never actually run.
func TestReviewLadder(t *testing.T) {
	ladder, err := reviewLadder([]string{"codex:gpt-5.6-sol/xhigh", "claude:claude-fable-5/xhigh"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ladder) != 2 {
		t.Fatalf("both rungs must survive (the fallback too), got %d", len(ladder))
	}
	// Rung 0 keeps its provider — NOT discarded onto the work provider.
	if ladder[0].Provider != "codex" || ladder[0].Model != "gpt-5.6-sol" || ladder[0].Effort != "xhigh" {
		t.Errorf("rung 0 = %+v, want codex / gpt-5.6-sol / xhigh", ladder[0])
	}
	// Rung 1 (the fallback) survives with its own provider — stepModel dropped it entirely.
	if ladder[1].Provider != "claude" || ladder[1].Model != "claude-fable-5" {
		t.Errorf("rung 1 = %+v, want claude / claude-fable-5", ladder[1])
	}
	// An empty ladder yields no rungs — the caller falls back to the work rotation.
	if got, _ := reviewLadder(nil); len(got) != 0 {
		t.Errorf("empty ladder → no rungs, got %v", got)
	}
}

// TestReviewReopenReceipt: the "REVIEW COMPLETE — reopened <N>" tally is parsed off a review's
// output, tolerant of the dash and trailing punctuation, and a review that merely MENTIONS
// reopening in prose without the receipt line reads as missing (ok=false) — not as N>0.
func TestReviewReopenReceipt(t *testing.T) {
	cases := []struct {
		name   string
		out    string
		wantN  int
		wantOk bool
	}{
		{"present", "did the review\nREVIEW COMPLETE — reopened 3", 3, true},
		{"zero", "everything passes\nREVIEW COMPLETE — reopened 0", 0, true},
		{"hyphen dash", "REVIEW COMPLETE - reopened 2", 2, true},
		{"trailing period", "REVIEW COMPLETE — reopened 4.", 4, true},
		{"missing entirely", "I reopened two tasks (in prose) but wrote no receipt", 0, false},
		{"mentions reopen, receipt 0", "I considered reopening auth-fix but it holds.\nREVIEW COMPLETE — reopened 0", 0, true},
		{"repeated line, last wins", "REVIEW COMPLETE — reopened 9\nwait, more\nREVIEW COMPLETE — reopened 1", 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, ok := reviewReopenReceipt(c.out)
			if n != c.wantN || ok != c.wantOk {
				t.Errorf("reviewReopenReceipt = %d/%v, want %d/%v", n, ok, c.wantN, c.wantOk)
			}
		})
	}
}

// TestReopenVerdictLost: the guard fires on the 2026-07-10 incident (claimed reopens, none moved)
// and on a missing receipt, but NOT on a consistent PASS or a consistent reopen — so a genuine
// review is never falsely re-run.
func TestReopenVerdictLost(t *testing.T) {
	cases := []struct {
		name     string
		claimed  int
		haveRcpt bool
		actual   int
		wantLost bool
	}{
		{"incident: claimed 6, moved 0", 6, true, 0, true},
		{"missing receipt", 0, false, 0, true},
		{"consistent pass", 0, true, 0, false},
		{"consistent reopen", 2, true, 2, false},
		{"undercount", 1, true, 3, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reopenVerdictLost(c.claimed, c.haveRcpt, c.actual); got != c.wantLost {
				t.Errorf("reopenVerdictLost(%d,%v,%d) = %v, want %v", c.claimed, c.haveRcpt, c.actual, got, c.wantLost)
			}
		})
	}
}

// The loop's leading positional is a target (provider[:model][@account]); no positional →
// no target (hasTarget=false) and the provider is required (caller errors or a preset lead
// supplies it). A malformed/unknown token errors; --model/--credential are unexpected args now.
func TestLoopTargetResolution(t *testing.T) {
	if _, has, _, _, err := parseLoopArgs(nil, false); err != nil || has {
		t.Errorf("parseLoopArgs(nil) = (has=%v, %v), want (false, nil) — no implicit default", has, err)
	}
	for _, ag := range []string{"claude", "codex", "gemini"} {
		tg, has, _, _, err := parseLoopArgs([]string{ag}, false)
		if err != nil || !has || tg.Provider != ag {
			t.Errorf("parseLoopArgs(%q) = (%+v, has=%v, %v), want provider=%q", ag, tg, has, err, ag)
		}
	}
	if tg, has, _, _, err := parseLoopArgs([]string{"claude:opus-4.8@work"}, false); err != nil || !has ||
		tg.Provider != "claude" || tg.Model != "opus-4.8" || len(tg.Accounts) != 1 || tg.Accounts[0] != "work" {
		t.Errorf("parseLoopArgs(claude:opus-4.8@work) = (%+v, %v)", tg, err)
	}
	if _, _, _, _, err := parseLoopArgs([]string{"bogus"}, false); err == nil {
		t.Error("parseLoopArgs(bogus): want error (unknown token)")
	}
	if _, _, _, _, err := parseLoopArgs([]string{"claude", "--model", "opus"}, false); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("--model should be an unexpected argument now, got %v", err)
	}
	if _, _, _, _, err := parseLoopArgs([]string{"claude", "--credential", "work"}, false); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("--credential should be an unexpected argument now, got %v", err)
	}
}

func TestParseLoopArgs(t *testing.T) {
	// --consult is pre-extracted by cmdLoop (see TestExtractConsult), so parseLoopArgs never sees
	// it — it resolves the target + the boolean flags only.
	cases := []struct {
		args          []string
		def           bool // the loop.yaml preflight.enabled default
		wantAgent     string
		wantModel     string
		wantDebug     bool
		wantPreflight bool
		wantErr       bool
	}{
		{nil, false, "", "", false, false, false},
		{[]string{"codex"}, false, "codex", "", false, false, false},
		{[]string{"--debug-on-fail"}, false, "", "", true, false, false},
		{[]string{"gemini", "--debug"}, false, "", "", false, false, true},        // --debug is not a known flag → error
		{[]string{"--debug-on-fail", "codex"}, false, "", "", false, false, true}, // a target must LEAD; a trailing positional errors
		{[]string{"bogus"}, false, "", "", false, false, true},
		// preflight: default off, --preflight turns it on, --no-preflight overrides a default-on.
		{[]string{"--preflight"}, false, "", "", false, true, false},
		{[]string{"codex", "--preflight"}, false, "codex", "", false, true, false},
		{nil, true, "", "", false, true, false},                         // preflight.enabled default
		{[]string{"--no-preflight"}, true, "", "", false, false, false}, // flag overrides default-on
		// The model/account ride the target now; --model/--credential are unexpected args (error).
		{[]string{"codex:gpt-5"}, false, "codex", "gpt-5", false, false, false},
		{[]string{"claude:opus@work"}, false, "claude", "opus", false, false, false},
		{[]string{"--model", "haiku"}, false, "", "", false, false, true},               // unexpected arg
		{[]string{"claude", "--credential", "work"}, false, "", "", false, false, true}, // unexpected arg
	}
	for _, c := range cases {
		tg, _, debug, preflight, err := parseLoopArgs(c.args, c.def)
		if (err != nil) != c.wantErr {
			t.Errorf("parseLoopArgs(%v) err=%v, wantErr=%v", c.args, err, c.wantErr)
			continue
		}
		if !c.wantErr && (tg.Provider != c.wantAgent || tg.Model != c.wantModel || debug != c.wantDebug || preflight != c.wantPreflight) {
			t.Errorf("parseLoopArgs(%v, def=%v) = (provider=%q model=%q debug=%v preflight=%v), want (%q, %q, %v, %v)",
				c.args, c.def, tg.Provider, tg.Model, debug, preflight, c.wantAgent, c.wantModel, c.wantDebug, c.wantPreflight)
		}
	}
}

func TestParseGovernor(t *testing.T) {
	a := &app{cfg: &config.Config{}}
	cases := []struct {
		name        string
		args        []string
		wantGov     string
		wantModel   string
		wantProfile string
		wantRest    []string
	}{
		{"no governor named — empty, the caller requires one", nil, "", "", "", nil},
		{"positional governor", []string{"claude"}, "claude", "", "", nil},
		// The governor is a target: its model + account fold out for the one-off selection.
		{"governor target model+account", []string{"claude:opus-4.8@work"}, "claude", "opus-4.8", "work", nil},
		{"positional governor + passthrough", []string{"gemini", "exec"}, "gemini", "", "", []string{"exec"}},
		{"passthrough args keep order", []string{"exec", "foo"}, "", "", "", []string{"exec", "foo"}},
		{"-- passes the rest through verbatim", []string{"claude", "--", "-p", "hi"}, "claude", "", "", []string{"-p", "hi"}},
		{"--governor is gone — treated as passthrough now", []string{"--governor", "claude"}, "", "", "", []string{"--governor", "claude"}},
		// A SECOND agent token is NOT swallowed as the governor — only the first is; the rest passes through.
		{"second agent token passes through", []string{"codex", "gemini"}, "codex", "", "", []string{"gemini"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gov, model, profile, _, rest, _, err := a.parseGovernor(c.args)
			if err != nil {
				t.Fatalf("parseGovernor(%v) errored: %v", c.args, err)
			}
			if gov != c.wantGov {
				t.Errorf("governor = %q, want %q", gov, c.wantGov)
			}
			if model != c.wantModel {
				t.Errorf("model = %q, want %q", model, c.wantModel)
			}
			if profile != c.wantProfile {
				t.Errorf("profile = %q, want %q", profile, c.wantProfile)
			}
			if !slices.Equal(rest, c.wantRest) {
				t.Errorf("rest = %v, want %v", rest, c.wantRest)
			}
		})
	}
}

// TestExtractConsult: --consult is REPEATABLE, one peer target per flag. The old boolean form
// (no value) errors with the rewrite; each value is collected in order; after `--` an agent's own
// --consult passes through verbatim.
func TestExtractConsult(t *testing.T) {
	cases := []struct {
		args     []string
		want     []string
		wantRest []string
		wantErr  bool
	}{
		{nil, nil, nil, false},
		{[]string{"-p", "hi"}, nil, []string{"-p", "hi"}, false},
		{[]string{"--consult", "codex"}, []string{"codex"}, nil, false},
		{[]string{"--consult", "codex:gpt-5.5", "--consult", "gemini"}, []string{"codex:gpt-5.5", "gemini"}, nil, false},
		{[]string{"--consult=codex", "-p", "hi"}, []string{"codex"}, []string{"-p", "hi"}, false},
		{[]string{"-p", "hi", "--consult", "gemini"}, []string{"gemini"}, []string{"-p", "hi"}, false},
		// The old boolean spelling (no value) errors with the rewrite.
		{[]string{"--consult"}, nil, nil, true},
		{[]string{"--consult", "--other"}, nil, nil, true},
		// After --, a --consult is the agent's own arg, not coop's — passed through verbatim.
		{[]string{"--", "--consult", "x"}, nil, []string{"--", "--consult", "x"}, false},
	}
	for _, c := range cases {
		got, rest, err := extractConsult(c.args)
		if (err != nil) != c.wantErr {
			t.Errorf("extractConsult(%v) err=%v, wantErr=%v", c.args, err, c.wantErr)
			continue
		}
		if c.wantErr {
			continue
		}
		if !slices.Equal(got, c.want) || !slices.Equal(rest, c.wantRest) {
			t.Errorf("extractConsult(%v) = (%v, %v), want (%v, %v)", c.args, got, rest, c.want, c.wantRest)
		}
	}
}

// TestResolvePeers: a --peer/--consult value is one peer target — a known, authed provider with
// an optional :model and NO account. An @account, an unauthed provider, and an unknown provider
// each error (naming the peer); an empty list is no peers, no error.
func TestResolvePeers(t *testing.T) {
	dir := t.TempDir()
	// claude authed (a credential file); codex/gemini not signed in.
	os.MkdirAll(filepath.Join(dir, "claude", "profiles", "default"), 0o755)
	os.WriteFile(filepath.Join(dir, "claude", "profiles", "default", ".credentials.json"), []byte("{}"), 0o644)
	a := &app{cfg: &config.Config{ConfigDir: dir}}

	peers, err := a.resolvePeers("--consult", []string{"claude:opus-4.8"})
	if err != nil || len(peers) != 1 || peers[0].Provider != "claude" || peers[0].Model != "opus-4.8" {
		t.Fatalf("resolvePeers(claude:opus-4.8) = (%+v, %v)", peers, err)
	}
	if _, err := a.resolvePeers("--consult", []string{"claude@work"}); err == nil {
		t.Error("a peer with an @account must be rejected (a peer runs on its default account)")
	}
	if _, err := a.resolvePeers("--peer", []string{"codex"}); err == nil {
		t.Error("an unauthed peer must be rejected")
	}
	if _, err := a.resolvePeers("--peer", []string{"borg"}); err == nil {
		t.Error("an unknown provider must be rejected")
	}
	if peers, err := a.resolvePeers("--consult", nil); err != nil || peers != nil {
		t.Errorf("resolvePeers(nil) = (%v, %v), want (nil, nil)", peers, err)
	}
}

// TestCmdLoginTarget: the account rides the target (coop login claude@work); a stray --credential
// is an unexpected arg; a :model has no meaning for login; an account ladder is loop-only. The happy
// path parses and reaches loginTo (which then needs a TTY) — proof the target flowed through.
func TestCmdLoginTarget(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	// claude@work parses and flows to loginTo — non-TTY there, NOT a parse error.
	if code, err := a.cmdLogin([]string{"claude@work"}); code != 2 || err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Errorf("cmdLogin(claude@work) = (%d, %v), want it to parse and hit the TTY check", code, err)
	}
	if _, err := a.cmdLogin([]string{"claude", "--credential", "work"}); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("cmdLogin --credential must be an unexpected argument, got %v", err)
	}
	if _, err := a.cmdLogin([]string{"claude:opus"}); err == nil || !strings.Contains(err.Error(), "no model") {
		t.Errorf("cmdLogin claude:opus must reject the model, got %v", err)
	}
	if _, err := a.cmdLogin([]string{"claude@work,personal"}); err == nil {
		t.Error("cmdLogin claude@work,personal must reject an account ladder (loop-only)")
	}
}

func TestLaunchAgentRejectsUnknownProfile(t *testing.T) {
	// A nonexistent account in the target must error before any box work (claude@ghost), so a
	// typo never silently creates a husk.
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	code, err := a.launchAgent("claude@ghost", []string{"-p", "hi"})
	if code != 2 || err == nil {
		t.Fatalf("launchAgent claude@ghost = (%d, %v), want 2 + error", code, err)
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should name the bad account: %v", err)
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

// A nonexistent account in the target must fail fast (before any box/Docker work) on fusion and
// acp too, not just a plain agent run; a stray --credential is a rejected arg on each surface.
func TestRunProfileWiringRejectsUnknown(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	if code, err := a.cmdFusion([]string{"claude@ghost"}); code != 2 || err == nil {
		t.Errorf("cmdFusion claude@ghost = (%d, %v), want 2 + error", code, err)
	}
	if code, err := a.cmdFusion([]string{"claude", "--credential", "ghost"}); code != 2 || err == nil {
		t.Errorf("cmdFusion --credential = (%d, %v), want 2 + error", code, err)
	}
	if code, err := a.cmdACP([]string{"claude@ghost"}); code != 2 || err == nil {
		t.Errorf("cmdACP claude@ghost = (%d, %v), want 2 + error", code, err)
	}
	if code, err := a.cmdACP([]string{"claude", "--credential", "ghost"}); code != 2 || err == nil {
		t.Errorf("cmdACP --credential = (%d, %v), want 2 + error", code, err)
	}
}

// A bare `coop acp` (no provider) defaults to the first signed-in provider — the toolbar's provider
// dropdown switches it live — and only errors when nothing is signed in.
func TestDefaultACPProvider(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	if got := defaultACPProvider(cfg); got != "" {
		t.Errorf("no signed-in agent should default to \"\", got %q", got)
	}
	// Sign codex in (its credential file), the way box.AuthedAgents detects it.
	os.MkdirAll(filepath.Join(dir, "codex", "profiles", "default"), 0o755)
	os.WriteFile(filepath.Join(dir, "codex", "profiles", "default", "auth.json"), []byte("{}"), 0o644)
	if got := defaultACPProvider(cfg); got != "codex" {
		t.Errorf("defaultACPProvider = %q, want codex (the only signed-in agent)", got)
	}
}

// `coop acp` with no provider AND nothing signed in fails fast (exit 2) rather than hanging or
// spawning — the fallback when the default has nothing to resolve to.
func TestACPNoProviderNoneSignedIn(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	code, err := a.cmdACP([]string{})
	if code != 2 || err == nil {
		t.Fatalf("bare cmdACP with nothing signed in = (%d, %v), want (2, error)", code, err)
	}
	if !strings.Contains(err.Error(), "coop login") {
		t.Errorf("error should point at signing in, got: %v", err)
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

// TestPromptLine: coop prompt's line shows non-zero segments only, "·"-separated, in a fixed
// order (todo, doing, blocked, looping, forks); "" when idle so an embedding prompt stays clean.
func TestPromptLine(t *testing.T) {
	if got := promptLine(taskCounts{}, 0, 0, false); got != "" {
		t.Errorf("idle should be empty, got %q", got)
	}
	if got := promptLine(taskCounts{Done: 9}, 0, 0, false); got != "" {
		t.Errorf("done-only isn't actionable state — should be empty, got %q", got)
	}
	if got := promptLine(taskCounts{Todo: 3, Blocked: 1}, 2, 1, false); got != "3 todo · 1 blocked · 1 looping · 2 forks" {
		t.Errorf("got %q", got)
	}
	if got := promptLine(taskCounts{Doing: 2}, 1, 0, false); got != "2 doing · 1 fork" { // singular fork
		t.Errorf("got %q", got)
	}
	// The unsigned nudge appends when set; alone (no other state) it's the whole line.
	if got := promptLine(taskCounts{Todo: 1}, 0, 0, true); got != "1 todo · unsigned" {
		t.Errorf("got %q", got)
	}
	if got := promptLine(taskCounts{}, 0, 0, true); got != "unsigned" {
		t.Errorf("unsigned alone should be the whole line, got %q", got)
	}
}

func TestSignOnExitAndPromptWarn(t *testing.T) {
	// shouldSignOnExit: only when you sign, not a fork, clean tree.
	cases := []struct{ fork, signs, dirty, want bool }{
		{false, true, false, true},   // sign a clean interactive session
		{true, true, false, false},   // fork → land-time re-sign owns it
		{false, false, false, false}, // you don't sign by default
		{false, true, true, false},   // dirty tree → never touch it
	}
	for _, c := range cases {
		if got := shouldSignOnExit(c.fork, c.signs, c.dirty); got != c.want {
			t.Errorf("shouldSignOnExit(fork=%v,signs=%v,dirty=%v) = %v, want %v", c.fork, c.signs, c.dirty, got, c.want)
		}
	}
	// promptSignWarn: only when you sign AND HEAD is unsigned.
	if !promptSignWarn(true, true) || promptSignWarn(true, false) || promptSignWarn(false, true) {
		t.Error("promptSignWarn should fire only when signs && headUnsigned")
	}
}

func TestScaffoldAgentSet(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()} // no agents signed in
	if got := scaffoldAgentSet(cfg, "all", true); len(got) != 3 {
		t.Errorf(`--agents all → 3 scaffoldable agents, got %v`, got)
	}
	// A named list is kept to the scaffoldable set — grok has no per-agent dir, so it's dropped.
	if got := scaffoldAgentSet(cfg, "claude,grok,codex", true); len(got) != 2 || got[0] != "claude" || got[1] != "codex" {
		t.Errorf("named list should keep scaffoldable only: %v", got)
	}
	// No flag, no credentials → empty (.agent/ only).
	if got := scaffoldAgentSet(cfg, "", false); len(got) != 0 {
		t.Errorf("no flag + no creds → empty, got %v", got)
	}
}
