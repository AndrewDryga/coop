package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/liveprocess"
	"github.com/AndrewDryga/coop/internal/loopcfg"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// The loop's closing banner must not claim "verified done" when the signoff reopened work — which it
// does by moving done tasks back into 10_in_progress/, not 00_todo/. Regression: the check looked at
// 00_todo/ only, so a reopened task in in_progress fell through to the green "verified done".
func TestLoopClosingBanner(t *testing.T) {
	// Reopened INTO in_progress (the bug): not done, and names the count.
	if b := loopClosingBanner(taskCounts{Done: 2, Doing: 3}, 5); !strings.Contains(b, "review left") ||
		!strings.Contains(b, "3 tasks") || strings.Contains(b, "verified done") {
		t.Errorf("reopened-into-in_progress banner = %q", b)
	}
	// Reopened into todo: same outcome, singular count.
	if b := loopClosingBanner(taskCounts{Done: 4, Todo: 1}, 4); !strings.Contains(b, "review left") ||
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

// The prune nudge fires only once done/ has piled up past the threshold, names the exact command,
// and stays quiet below it — pruning destroys state, so the loop only ever SUGGESTS it.
func TestPruneNudge(t *testing.T) {
	if n := pruneNudge(doneNudgeThreshold - 1); n != "" {
		t.Errorf("below the threshold there should be no nudge, got %q", n)
	}
	n := pruneNudge(23)
	if !strings.Contains(n, "23 done task folders") || !strings.Contains(n, "coop tasks rm --all-done") {
		t.Errorf("nudge should name the count and the exact command, got %q", n)
	}
}

// The loop's exit code lets cron/fleet/CI branch without parsing stderr: a review-reopened queue is
// a failure, 3 means only human-blocked work remains, and 0 means verified done.
func TestLoopExitCode(t *testing.T) {
	cases := []struct {
		cf   taskCounts
		want int
	}{
		{taskCounts{Done: 3, Blocked: 2}, 3}, // blocked, nothing actionable → 3
		{taskCounts{Done: 5}, 0},             // verified done → 0
		{taskCounts{Done: 3, Doing: 1}, 1},   // audit reopened into in_progress → unverified
		{taskCounts{Todo: 2, Blocked: 1}, 1}, // actionable work takes precedence over blocked
	}
	for _, c := range cases {
		if got := loopExitCode(c.cf); got != c.want {
			t.Errorf("loopExitCode(%+v) = %d, want %d", c.cf, got, c.want)
		}
	}
}

func TestLoopIntentionalAndInterruptedStopsAreDistinct(t *testing.T) {
	if loopInterruptedExitCode != 130 {
		t.Fatalf("loop interrupt exit = %d, want conventional SIGINT status 130", loopInterruptedExitCode)
	}
	cf := taskCounts{Done: 3, Todo: 2}
	if got := loopInterruptedBanner(cf); !strings.Contains(got, "interrupted before queue verification") || !strings.Contains(got, "3/5 done") {
		t.Errorf("interrupt banner = %q", got)
	}
	limit := loopTaskLimit{max: 1, settled: 1, lastID: "task-a", lastState: stateDone}
	if got := loopTaskLimitBanner(cf, limit); !strings.Contains(got, "task limit reached") ||
		!strings.Contains(got, "last: task-a done") || !strings.Contains(got, "paused before another task or final signoff") || strings.Contains(got, "verified done") {
		t.Errorf("task-limit banner = %q", got)
	}
	if got := loopTaskLimitBanner(taskCounts{Blocked: 2}, loopTaskLimit{max: 3}); !strings.Contains(got, "no actionable task") ||
		!strings.Contains(got, "no box started") || !strings.Contains(got, "2 blocked") {
		t.Errorf("task-limit idle banner = %q", got)
	}
	partial := loopTaskLimit{max: 3, settled: 1, lastID: "task-a", lastState: stateDone}
	if got := loopTaskLimitBanner(taskCounts{Done: 1}, partial); !strings.Contains(got, "1/3 tasks settled") ||
		!strings.Contains(got, "no actionable task remains") || !strings.Contains(got, "final signoff not run") {
		t.Errorf("partial task-limit banner = %q", got)
	}
	blocked := loopTaskLimit{max: 2, settled: 2, lastID: "task-b", lastState: stateBlocked}
	if got := loopTaskLimitBanner(taskCounts{Done: 1, Blocked: 1}, blocked); !strings.Contains(got, "task limit reached") ||
		!strings.Contains(got, "last: task-b blocked") || !strings.Contains(got, "■") {
		t.Errorf("blocked task-limit banner = %q", got)
	}
}

func TestLoopTaskLimitCountsSettledTasks(t *testing.T) {
	unlimited := loopTaskLimit{}
	unlimited.assign("first")
	if got := unlimited.scope(); got != "" {
		t.Fatalf("unlimited loop scope = %q, want empty", got)
	}

	limit := loopTaskLimit{max: 2}
	limit.assign("first")
	if reached, err := limit.observe(map[string]string{"first": stateInProgress}); reached || err != nil || limit.settled != 0 {
		t.Fatalf("active first task = (reached=%v, err=%v, settled=%d), want not counted", reached, err, limit.settled)
	}
	if reached, err := limit.observe(map[string]string{"first": stateDone}); reached || err != nil || limit.settled != 1 || limit.scope() != "" {
		t.Fatalf("done first task = (reached=%v, err=%v, settled=%d, scope=%q), want 1 and unpinned", reached, err, limit.settled, limit.scope())
	}
	limit.assign("second")
	if reached, err := limit.observe(map[string]string{"second": stateInProgress}); reached || err != nil {
		t.Fatalf("review-reopened second task should remain selected: reached=%v err=%v", reached, err)
	}
	if reached, err := limit.observe(map[string]string{"second": stateBlocked}); !reached || err != nil || limit.settled != 2 || limit.scope() != "second" {
		t.Fatalf("blocked second task = (reached=%v, err=%v, settled=%d, scope=%q), want limit reached and retained", reached, err, limit.settled, limit.scope())
	}
}

func TestLoopTaskLimitRejectsLostSelection(t *testing.T) {
	limit := loopTaskLimit{max: 1}
	limit.assign("missing")
	if _, err := limit.observe(map[string]string{}); err == nil || !strings.Contains(err.Error(), "lost task missing") {
		t.Fatalf("lost selected task error = %v", err)
	}
}

// The loop prompts must name the queue AND AGENTS.md as absolute in-box paths: gemini's
// read_file rejects a relative path, so a relative ".agent/tasks" left gemini/codex fleet forks
// unable to read their own queue (claude resolved it against cwd and was fine).
func TestLoopPromptsUseAbsolutePaths(t *testing.T) {
	repo := "/home/node/proj"
	work := loopWorkPrompt(repo, []string{".agent/tasks"}, "task-42", "claude", nil, nil)
	for _, want := range []string{"/home/node/proj/.agent/tasks", "/home/node/proj/AGENTS.md"} {
		if !strings.Contains(work, want) {
			t.Errorf("work prompt missing absolute %q:\n%s", want, work)
		}
	}
	// Several queues (a monorepo's per-component trees) are all listed, each absolute.
	multi := loopWorkPrompt(repo, []string{"portal/.agent/tasks", "runner/.agent/tasks"}, "task-42", "claude", nil, nil)
	for _, want := range []string{"/home/node/proj/portal/.agent/tasks", "/home/node/proj/runner/.agent/tasks"} {
		if !strings.Contains(multi, want) {
			t.Errorf("multi-queue work prompt missing %q:\n%s", want, multi)
		}
	}
	if review := loopSignoffPrompt(repo, []string{".agent/tasks"}, "", []string{"t1 — /home/node/proj/.agent/tasks/99_done/t1"}); !strings.Contains(review, "/home/node/proj/.agent/tasks") {
		t.Errorf("review prompt should name the absolute queue:\n%s", review)
	}
}

func TestReviewMountPolicy(t *testing.T) {
	queues := []string{"/repo/.agent/tasks", "/repo/service/.agent/tasks"}
	for _, tc := range []struct {
		name     string
		writes   loopcfg.ReviewWrites
		readOnly bool
		writable []string
	}{
		{name: "default", readOnly: true, writable: queues},
		{name: "explicit tasks", writes: loopcfg.ReviewWritesTasks, readOnly: true, writable: queues},
		{name: "explicit repository", writes: loopcfg.ReviewWritesRepo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readOnly, writable := reviewMountPolicy(tc.writes, queues)
			if readOnly != tc.readOnly || !slices.Equal(writable, tc.writable) {
				t.Errorf("reviewMountPolicy(%q) = (%v, %v), want (%v, %v)", tc.writes, readOnly, writable, tc.readOnly, tc.writable)
			}
		})
	}
}

func TestLoopWorkPromptPeerCapabilities(t *testing.T) {
	withoutPeers := loopWorkPrompt("/repo", []string{".agent/tasks"}, "task-42", "claude", nil, nil)
	for _, want := range []string{"no peer wrappers are mounted", "`coop-consult` and `coop-delegate` are unavailable", "do not invoke or probe them"} {
		if !strings.Contains(withoutPeers, want) {
			t.Errorf("no-peer work prompt missing %q:\n%s", want, withoutPeers)
		}
	}

	peers := []agents.Target{
		{Provider: "codex", Model: "gpt-5.5"},
		{Provider: "gemini"},
	}
	withPeers := loopWorkPrompt("/repo", []string{".agent/tasks"}, "task-42", "claude", peers, nil)
	for _, want := range []string{
		"`coop-consult` is available", "configured read-only targets only", "codex:gpt-5.5, gemini",
		"`coop-delegate` is unavailable", "do not invoke it", "Do not assume any other peers or preset roles are mounted",
	} {
		if !strings.Contains(withPeers, want) {
			t.Errorf("configured-peer work prompt missing %q:\n%s", want, withPeers)
		}
	}
	for _, role := range []string{"thinker", "critic", "fast"} {
		if strings.Contains(withPeers, role) {
			t.Errorf("configured-peer work prompt claims arbitrary role %q is available:\n%s", role, withPeers)
		}
	}

	rolePreset := &preset.Preset{Roles: []preset.Role{
		{Name: "critic", Mode: preset.ModeConsult},
		{Name: "fast", Mode: preset.ModeDelegate},
	}}
	withRoles := loopWorkPrompt("/repo", []string{".agent/tasks"}, "task-42", "claude", peers, rolePreset)
	for _, want := range []string{"read-only targets only: critic", "write-capable roles only: fast"} {
		if !strings.Contains(withRoles, want) {
			t.Errorf("preset work prompt missing actual role capability %q:\n%s", want, withRoles)
		}
	}
	if strings.Contains(withRoles, "codex:gpt-5.5") || strings.Contains(withRoles, "gemini") {
		t.Errorf("preset work prompt should report preset routing, not ignored generic peers:\n%s", withRoles)
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
	work := loopWorkPrompt("/repo", []string{".agent/tasks"}, "task-42", "claude", nil, nil)
	for _, want := range []string{
		"is NOT installed", "Work task task-42, already claimed in 10_in_progress/", "into 99_done/", "into 50_blocked/",
		"10_in_progress/", "00_todo/", "git status", "git diff",
		"state.md", "resume note", "AFTER the commit", "final filesystem action", "Status to complete", "Next action to none",
		"assigned task's tmp/ directory", "survives interruption and blocked transitions", "durable artifacts/ directory",
		"Work exactly ONE task per run", "the loop's job, not yours",
		// Reference the commit by its stable trailer, not its volatile SHA (coop re-signs on the host).
		"Coop-Task: <task-id>` trailer", "NOT its SHA", "re-signs your commit",
		// Discovered separate work: simple → 00_todo/, big → xx_backlog/ (never in this commit).
		"SPOT a SEPARATE task", "create its folder in 00_todo/", "xx_backlog/",
		// The contract is auto-loaded as the agent's instruction file — the prompt must not force a
		// re-read of ~2K tokens already in context, only offer the path as a fallback.
		"already loaded in your context", "only if its content is not",
	} {
		if !strings.Contains(work, want) {
			t.Errorf("folder work prompt missing %q:\n%s", want, work)
		}
	}
	for _, forbidden := range []string{"pick the next task", "claim it by moving", "take the task you claimed"} {
		if strings.Contains(work, forbidden) {
			t.Errorf("folder work prompt still delegates host-side selection/claim with %q:\n%s", forbidden, work)
		}
	}
}

// TestLoopPreflightAndReviewFolder: the preflight prompt frames only the CUSTOM cleanup — the
// built-in unblock runs host-side (unblockResolved), never in a box — bounded by the guardrails
// (no task work, no code, no commits); the default review does bookkeeping + ONE whole-repo gate
// and reopens by moving the folder (coop isn't in the box), and the fixed context footer carries
// the queue paths + reopen mechanics.
func TestLoopPreflightAndReviewFolder(t *testing.T) {
	pre := loopPreflightPrompt("/repo", []string{".agent/tasks"}, "Drop stale screenshots.\n")
	for _, want := range []string{
		"do NOT work any task", "write code", "run the gate", "no commits",
		"/repo/AGENTS.md", "`coop` is NOT installed in this box",
		"move task folders between the queue's state dirs ONLY as the cleanup instructions below direct",
		"never start working a task's content", "The cleanup to do: Drop stale screenshots.",
		"/repo/.agent/tasks",
	} {
		if !strings.Contains(pre, want) {
			t.Errorf("preflight prompt missing %q:\n%s", want, pre)
		}
	}
	if strings.Contains(pre, "Leave every 00_todo/ and 10_in_progress/ task untouched") {
		t.Errorf("preflight prompt still forbids the queue moves its cleanup may require:\n%s", pre)
	}
	rev := loopSignoffPrompt("/repo", []string{".agent/tasks"}, "", []string{"t1 — /repo/.agent/tasks/99_done/t1"})
	// The demanding default prompt: a header scoping the review to what THIS RUN completed (never
	// all of 99_done/, which holds prior runs' history), then a senior reviewer's bar — every
	// acceptance criterion met, the repo's rules obeyed, the FAILURE path tested, the change
	// polished (docs updated), a SINGLE whole-repo gate, reopen-by-moving, and no self-fix/commits.
	for _, want := range []string{
		"the ONLY tasks to review this pass", "t1 — /repo/.agent/tasks/99_done/t1", // scoped subjects lead
		"For EVERY task listed above", // the directive binds to the header, not the done/ dir
		"SENIOR REVIEWER", "99_done/",
		"acceptance criterion",                      // 1. meets its goal
		".agent/rules",                              // 2. follows the standards
		"FAILURE/edge path",                         // 3. tested for real
		"docs/README/CHANGELOG",                     // 4. polished
		"ONCE across the WHOLE repo (not per task)", // single whole-repo gate
		"tmp/ was disposable", "evidence that needed to survive completion belongs in artifacts/",
		"never edit a task in place under 99_done/", "reopen the task as a completion-integrity defect",
		"MOVING its folder back to 10_in_progress/", // reopen by moving
		"refreshing state.md to a reopened Status plus one concrete Next action",
		"THE MOMENT you decide", // execute reopens immediately, never batched
		"make no commits",
	} {
		if !strings.Contains(rev, want) {
			t.Errorf("default review prompt missing %q:\n%s", want, rev)
		}
	}
	// The fixed context footer: the absolute queue path, AGENTS.md, and the reopen mechanic —
	// including execute-immediately, so it binds even under a custom review.md override.
	for _, want := range []string{"/repo/.agent/tasks", "/repo/AGENTS.md", "its folder back to 10_in_progress/", "`coop` is NOT installed", "finalizes done-task lifecycle metadata", "refresh state.md", "Execute every reopen IMMEDIATELY"} {
		if !strings.Contains(rev, want) {
			t.Errorf("review prompt footer missing %q:\n%s", want, rev)
		}
	}
}

// The subject header leads, then the built-in senior review ALWAYS runs; .agent/loop.yaml
// signoff.prompt only APPENDS to it (never replaces it). Either way the fixed context footer trails.
func TestLoopReviewPromptAppend(t *testing.T) {
	repo := t.TempDir()
	subjects := []string{"t1 — /repo/.agent/tasks/99_done/t1"}
	// No append → the subject header + the built-in default, no appendix.
	if rev := loopSignoffPrompt(repo, []string{".agent/tasks"}, "", subjects); !strings.HasPrefix(rev, "The task(s) this run completed") || strings.Contains(rev, "project-specific checks") {
		t.Errorf("empty append → header + built-in only, no appendix:\n%s", rev)
	}
	// With an append → the header + built-in lead, then the appended text, then the footer.
	rev := loopSignoffPrompt(repo, []string{".agent/tasks"}, "- Verify CHANGELOG.md gained an entry.", subjects)
	if !strings.HasPrefix(rev, "The task(s) this run completed") || !strings.Contains(rev, "SENIOR REVIEWER") {
		t.Errorf("the built-in review must always follow the header (append never replaces):\n%s", rev)
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
	if !strings.Contains(p, "AUDIT EVIDENCE — <id> — gate:") {
		t.Errorf("the between prompt must request structured audit evidence:\n%s", p)
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

func TestBetweenAuditSetPrompt(t *testing.T) {
	if prompt, run := betweenAuditSetPrompt(false, "", nil); run || prompt != "" {
		t.Errorf("unconfigured ordinary change = %q/%v, want no audit", prompt, run)
	}
	if prompt, run := betweenAuditSetPrompt(false, "", []string{"Makefile"}); !run || prompt != defaultProtectedBetweenPrompt {
		t.Errorf("unconfigured protected change = %q/%v, want built-in audit", prompt, run)
	}
	if prompt, run := betweenAuditSetPrompt(true, "  custom audit  ", nil); !run || prompt != "custom audit" {
		t.Errorf("configured ordinary change = %q/%v, want custom audit", prompt, run)
	}
	if prompt, run := betweenAuditSetPrompt(true, "custom audit", []string{"Makefile"}); !run || prompt != "custom audit" {
		t.Errorf("configured protected change = %q/%v, want custom audit", prompt, run)
	}
	if shouldRunBetweenAudit(false, true, false) {
		t.Error("a failed ordinary iteration must not run the optional audit")
	}
	if !shouldRunBetweenAudit(true, true, false) {
		t.Error("a successful ordinary iteration must run its configured audit")
	}
	if !shouldRunBetweenAudit(false, false, true) {
		t.Error("a failed protected completion must still run the mandatory audit")
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

func TestReviewReopenReceipt(t *testing.T) {
	cases := []struct {
		name, out, verdict string
		ids                []string
		ok                 bool
	}{
		{"pass", "done\nREVIEW COMPLETE — PASS — reopened: none", "PASS", nil, true},
		{"one reopen", "REVIEW COMPLETE — FAIL — reopened: task-a", "FAIL", []string{"task-a"}, true},
		{"multiple sorted", "REVIEW COMPLETE — FAIL — reopened: task-a,task-b", "FAIL", []string{"task-a", "task-b"}, true},
		{"old ambiguous receipt", "REVIEW COMPLETE — reopened 2", "", nil, false},
		{"missing", "I reopened task-a", "", nil, false},
		{"malformed verdict", "REVIEW COMPLETE — MAYBE — reopened: none", "", nil, false},
		{"pass with ids", "REVIEW COMPLETE — PASS — reopened: task-a", "", nil, false},
		{"fail without ids", "REVIEW COMPLETE — FAIL — reopened: none", "", nil, false},
		{"unsorted", "REVIEW COMPLETE — FAIL — reopened: task-b,task-a", "", nil, false},
		{"duplicates", "REVIEW COMPLETE — FAIL — reopened: task-a,task-a", "", nil, false},
		{"spaces in ids", "REVIEW COMPLETE — FAIL — reopened: task-a, task-b", "", nil, false},
		{"not terminal", "REVIEW COMPLETE — PASS — reopened: none\nmore prose", "", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, ok := reviewReopenReceipt(c.out)
			if r.verdict != c.verdict || !slices.Equal(r.reopened, c.ids) || ok != c.ok {
				t.Errorf("reviewReopenReceipt = %+v/%v, want %s,%v/%v", r, ok, c.verdict, c.ids, c.ok)
			}
		})
	}
}

func TestReviewPromptRequiresExactReceipt(t *testing.T) {
	prompt := reviewContextFooter("/repo", []string{".agent/tasks"})
	for _, want := range []string{
		"REVIEW COMPLETE — PASS — reopened: none",
		"REVIEW COMPLETE — FAIL — reopened: <id1>,<id2>",
		"sorted by task ID", "exact IDs", "named review subjects",
		"authoritative review", "do NOT invoke the review-board skill or spawn another review board",
		"focused read-only investigation",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("review prompt missing %q:\n%s", want, prompt)
		}
	}
}

// TestReopenVerdictLost: the guard fires on the 2026-07-10 incident (claimed reopens, none moved)
// and on a missing receipt, but NOT on a consistent PASS or a consistent reopen — so a genuine
// review is never falsely re-run.
func TestReopenVerdictLost(t *testing.T) {
	cases := []struct {
		name     string
		receipt  reviewReceipt
		haveRcpt bool
		actual   []string
		subjects []string
		wantLost bool
	}{
		{"claimed reopen moved none", reviewReceipt{"FAIL", []string{"a"}}, true, nil, []string{"a"}, true},
		{"missing receipt", reviewReceipt{}, false, nil, []string{"a"}, true},
		{"consistent pass with unrelated actionable", reviewReceipt{"PASS", nil}, true, nil, []string{"a"}, false},
		{"consistent exact reopen", reviewReceipt{"FAIL", []string{"a", "b"}}, true, []string{"a", "b"}, []string{"a", "b"}, false},
		{"equal count wrong ids", reviewReceipt{"FAIL", []string{"a"}}, true, []string{"b"}, []string{"a", "b"}, true},
		{"unexpected id", reviewReceipt{"FAIL", []string{"other"}}, true, []string{"other"}, []string{"a"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reopenVerdictLost(c.receipt, c.haveRcpt, c.actual, c.subjects); got != c.wantLost {
				t.Errorf("reopenVerdictLost(%+v,%v,%v,%v) = %v, want %v", c.receipt, c.haveRcpt, c.actual, c.subjects, got, c.wantLost)
			}
		})
	}
}

func TestProtectedAuditVerdict(t *testing.T) {
	runErr := errors.New("review unavailable")
	cases := []struct {
		name                   string
		protected, interrupted bool
		reviewErr              error
		output                 string
		actual, subjects       []string
		wantErr                bool
	}{
		{name: "ordinary audit keeps existing behavior", reviewErr: runErr},
		{name: "protected run failure", protected: true, reviewErr: runErr, wantErr: true},
		{name: "protected missing receipt", protected: true, wantErr: true},
		{name: "protected mismatch", protected: true, output: "REVIEW COMPLETE — FAIL — reopened: a", subjects: []string{"a"}, wantErr: true},
		{name: "protected pass", protected: true, output: "REVIEW COMPLETE — PASS — reopened: none", subjects: []string{"a"}},
		{name: "protected reopen", protected: true, output: "REVIEW COMPLETE — FAIL — reopened: a,b", actual: []string{"a", "b"}, subjects: []string{"a", "b"}},
		{name: "protected unexpected id", protected: true, output: "REVIEW COMPLETE — FAIL — reopened: other", actual: []string{"other"}, subjects: []string{"a"}, wantErr: true},
		{name: "user interruption", protected: true, interrupted: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := protectedAuditVerdict(c.protected, c.interrupted, c.reviewErr, c.output, c.actual, c.subjects)
			if (err != nil) != c.wantErr {
				t.Errorf("protectedAuditVerdict error = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}

func TestBlockReopenedTasksLeavesUnrelatedActionableWork(t *testing.T) {
	q := filepath.Join(t.TempDir(), ".agent", "tasks")
	writeTaskFile(t, filepath.Join(q, stateInProgress, "review-reopen", "task.md"), "# Reopen\n")
	writeTaskFile(t, filepath.Join(q, stateInProgress, "unrelated", "task.md"), "# Unrelated\n")

	blockReopenedTasks([]string{q}, []string{"review-reopen"}, 3)

	if !pathExists(filepath.Join(q, stateBlocked, "review-reopen")) {
		t.Fatal("exact review reopen was not blocked")
	}
	if !pathExists(filepath.Join(q, stateInProgress, "unrelated")) {
		t.Fatal("unrelated actionable task was moved by the signoff cap")
	}
}

// The loop's leading positional is a target (provider[:model][/effort][@account]); no positional →
// no target (hasTarget=false) and the provider is required (caller errors or a preset lead
// supplies it). A malformed/unknown token errors; --model/--credential are unexpected args now.
func TestLoopTargetResolution(t *testing.T) {
	if _, has, ps, _, _, _, _, err := parseLoopArgs(nil, false); err != nil || has || ps != "" {
		t.Errorf("parseLoopArgs(nil) = (has=%v, preset=%q, %v), want (false, \"\", nil) — no implicit default", has, ps, err)
	}
	for _, ag := range []string{"claude", "codex", "gemini"} {
		tg, has, ps, _, _, _, _, err := parseLoopArgs([]string{ag}, false)
		if err != nil || !has || ps != "" || tg.Provider != ag {
			t.Errorf("parseLoopArgs(%q) = (%+v, has=%v, preset=%q, %v), want provider=%q", ag, tg, has, ps, err, ag)
		}
	}
	if tg, has, ps, _, _, _, _, err := parseLoopArgs([]string{"claude:opus-4.8@work"}, false); err != nil || !has || ps != "" ||
		tg.Provider != "claude" || tg.Model != "opus-4.8" || len(tg.Accounts) != 1 || tg.Accounts[0] != "work" {
		t.Errorf("parseLoopArgs(claude:opus-4.8@work) = (%+v, has=%v, preset=%q, %v)", tg, has, ps, err)
	}
	// Keep the documented preset invocation tied to the parser: cmdLoop passes the words after
	// "coop loop" to parseLoopArgs, so its positional preset must remain accepted.
	const documentedPresetLoop = "coop loop frontier"
	words := strings.Fields(documentedPresetLoop)
	if tg, has, ps, _, _, _, _, err := parseLoopArgs(words[2:], false); err != nil || has || ps != "frontier" || tg.Provider != "" {
		t.Errorf("%q = (%+v, has=%v, preset=%q, %v), want positional preset frontier", documentedPresetLoop, tg, has, ps, err)
	}
	// A bare non-target word is a PRESET NAME now (its existence is validated later by
	// loadRunPreset), not an unknown-token error.
	if tg, has, ps, _, _, _, _, err := parseLoopArgs([]string{"frontier"}, false); err != nil || has || ps != "frontier" || tg.Provider != "" {
		t.Errorf("parseLoopArgs(frontier) = (%+v, has=%v, preset=%q, %v), want a preset name and no target", tg, has, ps, err)
	}
	// The model/account ride the target and a preset is the positional, so --model/--credential/
	// --preset are all unexpected args now.
	for _, bad := range [][]string{{"claude", "--model", "opus"}, {"claude", "--credential", "work"}, {"claude", "--preset", "frontier"}} {
		if _, _, _, _, _, _, _, err := parseLoopArgs(bad, false); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
			t.Errorf("parseLoopArgs(%v) should be an unexpected argument, got %v", bad, err)
		}
	}
}

func TestParseLoopArgs(t *testing.T) {
	// --peer is pre-extracted by cmdLoop (see TestExtractPeer), so parseLoopArgs never sees it — it
	// resolves the who-runs positional (a target OR a preset name) + the boolean flags only.
	cases := []struct {
		args          []string
		def           bool // the loop.yaml preflight.enabled default
		wantAgent     string
		wantModel     string
		wantPreset    string
		wantDebug     bool
		wantPreflight bool
		wantNoMCP     bool
		wantMaxTasks  int
		wantErr       bool
	}{
		{args: nil},
		{args: []string{"codex"}, wantAgent: "codex"},
		{args: []string{"--debug-on-fail"}, wantDebug: true},
		{args: []string{"--max-tasks", "1"}, wantMaxTasks: 1},
		{args: []string{"codex", "--max-tasks", "3"}, wantAgent: "codex", wantMaxTasks: 3},
		{args: []string{"--max-tasks"}, wantErr: true},
		{args: []string{"--max-tasks", "0"}, wantErr: true},
		{args: []string{"--max-tasks", "-1"}, wantErr: true},
		{args: []string{"--max-tasks", "many"}, wantErr: true},
		{args: []string{"--max-tasks", "1", "--max-tasks", "2"}, wantErr: true},
		{args: []string{"--once"}, wantErr: true},
		{args: []string{"gemini", "--debug"}, wantErr: true},        // --debug is not a known flag → error
		{args: []string{"--debug-on-fail", "codex"}, wantErr: true}, // a who must LEAD; a trailing positional errors
		// A bare non-target word is a PRESET NAME now (not an unknown-token error).
		{args: []string{"frontier"}, wantPreset: "frontier"},
		{args: []string{"frontier", "--preflight"}, wantPreset: "frontier", wantPreflight: true},
		// preflight: default off, --preflight turns it on, --no-preflight overrides a default-on.
		{args: []string{"--preflight"}, wantPreflight: true},
		{args: []string{"codex", "--preflight"}, wantAgent: "codex", wantPreflight: true},
		{def: true, wantPreflight: true},                                    // preflight.enabled default
		{args: []string{"--no-preflight"}, def: true, wantPreflight: false}, // flag overrides default-on
		// --no-mcp: this run's boxes mount no MCP (the committed form is loop.yaml mcp: false).
		{args: []string{"--no-mcp"}, wantNoMCP: true},
		{args: []string{"claude", "--no-mcp", "--preflight"}, wantAgent: "claude", wantPreflight: true, wantNoMCP: true},
		// The model/account ride the target now; --model/--credential are unexpected args (error).
		{args: []string{"codex:gpt-5"}, wantAgent: "codex", wantModel: "gpt-5"},
		{args: []string{"claude:opus@work"}, wantAgent: "claude", wantModel: "opus"},
		{args: []string{"--model", "haiku"}, wantErr: true},               // unexpected arg
		{args: []string{"claude", "--credential", "work"}, wantErr: true}, // unexpected arg
		{args: []string{"claude", "--preset", "frontier"}, wantErr: true}, // --preset retired → unexpected arg
	}
	for _, c := range cases {
		tg, _, ps, debug, preflight, noMCP, maxTasks, err := parseLoopArgs(c.args, c.def)
		if (err != nil) != c.wantErr {
			t.Errorf("parseLoopArgs(%v) err=%v, wantErr=%v", c.args, err, c.wantErr)
			continue
		}
		if !c.wantErr && (tg.Provider != c.wantAgent || tg.Model != c.wantModel || ps != c.wantPreset || debug != c.wantDebug || preflight != c.wantPreflight || noMCP != c.wantNoMCP || maxTasks != c.wantMaxTasks) {
			t.Errorf("parseLoopArgs(%v, def=%v) = (provider=%q model=%q preset=%q debug=%v preflight=%v noMCP=%v maxTasks=%d), want (%q, %q, %q, %v, %v, %v, %d)",
				c.args, c.def, tg.Provider, tg.Model, ps, debug, preflight, noMCP, maxTasks, c.wantAgent, c.wantModel, c.wantPreset, c.wantDebug, c.wantPreflight, c.wantNoMCP, c.wantMaxTasks)
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
		wantPreset  string
		wantRest    []string
	}{
		{"no governor named — empty, the caller requires one", nil, "", "", "", "", nil},
		{"positional governor", []string{"claude"}, "claude", "", "", "", nil},
		// The governor is a target: its model + account fold out for the one-off selection.
		{"governor target model+account", []string{"claude:opus-4.8@work"}, "claude", "opus-4.8", "work", "", nil},
		{"positional governor + passthrough", []string{"gemini", "exec"}, "gemini", "", "", "", []string{"exec"}},
		// A leading non-target bare word is the PRESET NAME (the who slot); the rest passes through.
		{"leading preset governs, rest passes through", []string{"frontier", "foo"}, "", "", "", "frontier", []string{"foo"}},
		{"bare preset name", []string{"frontier"}, "", "", "", "frontier", nil},
		{"-- passes the rest through verbatim", []string{"claude", "--", "-p", "hi"}, "claude", "", "", "", []string{"-p", "hi"}},
		{"--governor is gone — treated as passthrough now", []string{"--governor", "claude"}, "", "", "", "", []string{"--governor", "claude"}},
		// A SECOND agent token is NOT swallowed as the governor — only the first is; the rest passes through.
		{"second agent token passes through", []string{"codex", "gemini"}, "codex", "", "", "", []string{"gemini"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gov, model, profile, _, ps, rest, _, err := a.parseGovernor(c.args)
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
			if ps != c.wantPreset {
				t.Errorf("preset = %q, want %q", ps, c.wantPreset)
			}
			if !slices.Equal(rest, c.wantRest) {
				t.Errorf("rest = %v, want %v", rest, c.wantRest)
			}
		})
	}
}

func TestCmdFusionCrossProviderPresetPinsFirstRungAndWiresRoleCouncil(t *testing.T) {
	repo := t.TempDir()
	presetDir := filepath.Join(repo, ".agent", "presets", "duo")
	if err := os.MkdirAll(presetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presetDir, "preset.yaml"), []byte(`lead:
  agent: [claude:one/high, codex:two/xhigh]
roles:
  critic:
    mode: consult
    agent: gemini
`), 0o644); err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	cfg := &config.Config{
		ConfigDir: configDir, RepoOverride: repo, HomeInBox: "/home/node", BoxHome: t.TempDir(),
		BaseImage: "test-base", ImageOverride: "test-image", Homes: true, Egress: "none",
	}
	if err := os.WriteFile(cfg.EnvFile(), []byte("GEMINI_API_KEY=test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := &app{cfg: cfg, rt: fusionRecordingRuntime(t, recorder), rtSet: true}
	var code int
	var runErr error
	out := captureStderr(t, func() { code, runErr = a.cmdFusion([]string{"duo"}) })
	if runErr != nil || code != 0 {
		t.Fatalf("cmdFusion(duo) = (%d, %v), want success; stderr:\n%s", code, runErr, out)
	}
	for _, want := range []string{"pins", "claude:one/high", "no fallback rotation"} {
		if !strings.Contains(out, want) {
			t.Errorf("terminal Fusion pin notice missing %q:\n%s", want, out)
		}
	}
	args, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"COOP_CONSULT_CRITIC_TARGETS=gemini", ":/usr/local/bin/coop-consult:ro"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("runtime assembly missing %q:\n%s", want, args)
		}
	}
}

func fusionRecordingRuntime(t *testing.T, recorder string) runtime.Runtime {
	t.Helper()
	shim := filepath.Join(t.TempDir(), "runtime")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + strconv.Quote(recorder) + "\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: shim}
}

func TestACPInnerEmptyPresetSelectionClearsPositionalPreset(t *testing.T) {
	t.Setenv("COOP_ACP_INNER", "1")
	t.Setenv("COOP_ACP_PRESET", "")
	t.Setenv("COOP_ACP_TARGET", "claude")

	configDir := t.TempDir()
	cfg := &config.Config{
		ConfigDir: configDir, RepoOverride: t.TempDir(), HomeInBox: "/home/node", BoxHome: t.TempDir(),
		BaseImage: "test-base", ImageOverride: "test-image", Homes: true, Egress: "none",
	}
	if err := os.WriteFile(cfg.EnvFile(), []byte("GEMINI_API_KEY=test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := &app{cfg: cfg, rt: fusionRecordingRuntime(t, recorder), rtSet: true}
	code, err := a.cmdACP([]string{"fusion", "missing-positional-preset", "--peer", "gemini"})
	if err != nil || code != 0 {
		t.Fatalf("inner ACP clear = (%d, %v), want success without loading the positional preset", code, err)
	}
	if a.preset != nil {
		t.Errorf("empty COOP_ACP_PRESET must clear positional preset, retained %+v", a.preset)
	}
}

func TestACPInnerSelectedPresetAndTargetReplaceLaunchState(t *testing.T) {
	t.Setenv("COOP_ACP_INNER", "1")
	t.Setenv("COOP_ACP_PRESET", "selected")
	t.Setenv("COOP_ACP_TARGET", "codex:acp-selected/high")

	repo := t.TempDir()
	presetDir := filepath.Join(repo, ".agent", "presets", "selected")
	if err := os.MkdirAll(presetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presetDir, "preset.yaml"), []byte(`lead:
  agent: [claude:stale-first/high, codex:acp-selected/high]
roles:
  critic:
    mode: consult
    agent: gemini:role-selected
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ConfigDir: t.TempDir(), RepoOverride: repo, HomeInBox: "/home/node", BoxHome: t.TempDir(),
		BaseImage: "test-base", ImageOverride: "test-image", Homes: true, Egress: "none",
	}
	for _, provider := range []string{"claude", "codex", "gemini"} {
		signInCred(t, cfg, provider, "default")
	}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := &app{cfg: cfg, rt: fusionRecordingRuntime(t, recorder), rtSet: true}
	code, err := a.cmdACP([]string{"fusion", "missing-positional-preset"})
	if err != nil || code != 0 {
		t.Fatalf("inner ACP selected migration = (%d, %v), want success", code, err)
	}
	if a.preset == nil || a.preset.Name != "selected" {
		t.Fatalf("inner ACP loaded preset = %#v, want selected", a.preset)
	}
	if model, effort := cfg.ModelFor("codex"), cfg.EffortFor("codex"); model != "acp-selected" || effort != "high" {
		t.Fatalf("inner ACP effective target = codex:%s/%s, want codex:acp-selected/high", model, effort)
	}
	args, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"codex-acp", "COOP_CONSULT_CRITIC_TARGETS=gemini:role-selected", ":/usr/local/bin/coop-consult:ro"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("inner ACP selected assembly missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(string(args), "stale-first") {
		t.Errorf("inner ACP assembly retained the old launch rung:\n%s", args)
	}
}

func TestACPPlainInnerTargetDoesNotBecomeFusion(t *testing.T) {
	t.Setenv("COOP_ACP_INNER", "1")
	t.Setenv("COOP_ACP_TARGET", "claude")

	cfg := &config.Config{
		ConfigDir: t.TempDir(), RepoOverride: t.TempDir(), HomeInBox: "/home/node", BoxHome: t.TempDir(),
		BaseImage: "test-base", ImageOverride: "test-image", Homes: false, Egress: "none",
	}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := &app{cfg: cfg, rt: fusionRecordingRuntime(t, recorder), rtSet: true}
	code, err := a.cmdACP([]string{"claude"})
	if err != nil || code != 0 {
		t.Fatalf("plain inner ACP target = (%d, %v), want a non-Fusion run", code, err)
	}
}

func TestACPFusionSupervisorDoesNotPinSkippedFirstAccount(t *testing.T) {
	repo := t.TempDir()
	presetDir := filepath.Join(repo, ".agent", "presets", "rotate")
	if err := os.MkdirAll(presetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presetDir, "preset.yaml"), []byte(`lead:
  agent: [claude@ghost, codex@work]
roles:
  critic:
    mode: consult
    agent: claude
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: repo}
	signInCred(t, cfg, "claude", "default")
	signInCred(t, cfg, "codex", "work")

	called := false
	a := &app{cfg: cfg, acpSupervise: func(_ []string, ctrl *acpControl) (int, error) {
		called = true
		target, presetName, ok := ctrl.spawnTarget()
		if !ok || presetName != "rotate" || target.String() != "codex@work" {
			t.Errorf("supervisor target = (%s, %q, %v), want codex@work + rotate", target.String(), presetName, ok)
		}
		return 0, nil
	}}
	code, err := a.cmdACP([]string{"fusion", "rotate"})
	if err != nil || code != 0 || !called {
		t.Fatalf("ACP Fusion skipped-first launch = (%d, %v, supervise=%v), want success", code, err, called)
	}
	if got := cfg.ActiveProfile("claude"); got != "default" {
		t.Fatalf("outer ACP pinned skipped claude@ghost, active profile = %q", got)
	}
}

func TestSpawnBoxExportsEmptyPresetSelection(t *testing.T) {
	recorder := filepath.Join(t.TempDir(), "preset-env")
	shim := filepath.Join(t.TempDir(), "inner")
	script := "#!/bin/sh\nprintf 'set:%s' \"${COOP_ACP_PRESET-UNSET}\" > " + strconv.Quote(recorder) + "\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir()}
	ctrl := newACPControl(cfg, "claude", "", "", t.TempDir(), acpSelection{}, nil, nil, true)
	a := &app{cfg: cfg}
	child, err := a.spawnBox(context.Background(), shim, nil, "test-supervisor", ctrl,
		agents.Target{Provider: "claude"}, "", true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if data, readErr := os.ReadFile(recorder); readErr == nil {
			if got := string(data); got != "set:" {
				t.Fatalf("COOP_ACP_PRESET handoff = %q, want present-but-empty", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("inner process did not record COOP_ACP_PRESET")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestExtractPeer: --peer is REPEATABLE, one peer target per flag. A valueless occurrence errors
// (points at the repeatable form); each value is collected in order; after `--` an agent's own
// --peer passes through verbatim. The retired --consult spelling is now an ordinary passthrough arg.
func TestExtractPeer(t *testing.T) {
	cases := []struct {
		args     []string
		want     []string
		wantRest []string
		wantErr  bool
	}{
		{nil, nil, nil, false},
		{[]string{"-p", "hi"}, nil, []string{"-p", "hi"}, false},
		{[]string{"--peer", "codex"}, []string{"codex"}, nil, false},
		{[]string{"--peer", "codex:gpt-5.5", "--peer", "gemini"}, []string{"codex:gpt-5.5", "gemini"}, nil, false},
		{[]string{"--peer=codex", "-p", "hi"}, []string{"codex"}, []string{"-p", "hi"}, false},
		{[]string{"-p", "hi", "--peer", "gemini"}, []string{"gemini"}, []string{"-p", "hi"}, false},
		// A valueless --peer errors (points at the repeatable form).
		{[]string{"--peer"}, nil, nil, true},
		{[]string{"--peer", "--other"}, nil, nil, true},
		// After --, a --peer is the agent's own arg, not coop's — passed through verbatim.
		{[]string{"--", "--peer", "x"}, nil, []string{"--", "--peer", "x"}, false},
		// The retired --consult is now just an unknown/passthrough token, not a peer flag.
		{[]string{"--consult", "codex"}, nil, []string{"--consult", "codex"}, false},
	}
	for _, c := range cases {
		got, rest, err := extractPeer(c.args)
		if (err != nil) != c.wantErr {
			t.Errorf("extractPeer(%v) err=%v, wantErr=%v", c.args, err, c.wantErr)
			continue
		}
		if c.wantErr {
			continue
		}
		if !slices.Equal(got, c.want) || !slices.Equal(rest, c.wantRest) {
			t.Errorf("extractPeer(%v) = (%v, %v), want (%v, %v)", c.args, got, rest, c.want, c.wantRest)
		}
	}
}

// TestResolvePeers: a --peer value is one peer target — a known, authed provider with an optional
// :model and NO account. An @account, an unauthed provider, and an unknown provider each error
// (naming the peer); an empty list is no peers, no error.
func TestResolvePeers(t *testing.T) {
	dir := t.TempDir()
	// claude authed (a credential file); codex/gemini not signed in.
	os.MkdirAll(filepath.Join(dir, "claude", "profiles", "default"), 0o755)
	os.WriteFile(filepath.Join(dir, "claude", "profiles", "default", ".credentials.json"), []byte("{}"), 0o644)
	a := &app{cfg: &config.Config{ConfigDir: dir}}

	peers, err := a.resolvePeers("--peer", []string{"claude:opus-4.8"})
	if err != nil || len(peers) != 1 || peers[0].Provider != "claude" || peers[0].Model != "opus-4.8" {
		t.Fatalf("resolvePeers(claude:opus-4.8) = (%+v, %v)", peers, err)
	}
	if _, err := a.resolvePeers("--peer", []string{"claude@work"}); err == nil {
		t.Error("a peer with an @account must be rejected (a peer runs on its default account)")
	}
	if _, err := a.resolvePeers("--peer", []string{"codex"}); err == nil {
		t.Error("an unauthed peer must be rejected")
	}
	if _, err := a.resolvePeers("--peer", []string{"borg"}); err == nil {
		t.Error("an unknown provider must be rejected")
	}
	if peers, err := a.resolvePeers("--peer", nil); err != nil || peers != nil {
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
		{"claude", "--supervise"},
		{"fusion", "claude", "junk"},
	} {
		if code, err := a.cmdACP(args); code != 2 || err == nil {
			t.Errorf("cmdACP(%v) = (%d, %v), want (2, usage error)", args, code, err)
		}
	}
}

func TestCleanACPChildEnv(t *testing.T) {
	got := cleanACPChildEnv([]string{
		"PATH=/bin",
		"COOP_ACP_TARGET=gemini",
		"COOP_ACP_PRESET=frontier",
		"COOP_ACP_INNER=1",
		"COOP_ACP_SUPERVISOR=stale",
		"COOP_ACP_CIDFILE=/tmp/stale",
		"COOP_ACP_RESUME_STATE=/tmp/stale",
		liveprocess.ControlFDEnv + "=3",
		liveprocess.ProcessDirEnv + "=/tmp/stale-processes",
		liveprocess.CleanupIDEnv + "=stale-cleanup",
		liveprocess.RevokePathEnv + "=/tmp/.coop-live-revoked-00000000000000000000000000000000",
		"COOP_ACP_TRACE=1",
		"COOP_ACP_CARRY_TOKENS=123",
	})
	joined := strings.Join(got, "\n")
	for _, want := range []string{"PATH=/bin", "COOP_ACP_TRACE=1", "COOP_ACP_CARRY_TOKENS=123"} {
		if !strings.Contains(joined, want) {
			t.Errorf("clean env dropped public setting %q: %v", want, got)
		}
	}
	for _, removed := range []string{
		"COOP_ACP_TARGET", "COOP_ACP_PRESET", "COOP_ACP_INNER", "COOP_ACP_SUPERVISOR",
		"COOP_ACP_CIDFILE", "COOP_ACP_RESUME_STATE", liveprocess.ControlFDEnv, liveprocess.ProcessDirEnv,
		liveprocess.CleanupIDEnv, liveprocess.RevokePathEnv,
	} {
		if strings.Contains(joined, removed+"=") {
			t.Errorf("clean env retained internal setting %q: %v", removed, got)
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

// The top-level help documents coop's --peer wrapper flag and stops claiming `coop <agent>
// --help` shows coop's flags (it forwards to the agent).
func TestHelpDocumentsPeerAndAgentHelp(t *testing.T) {
	h := helpText(&config.Config{})
	if !strings.Contains(h, "--peer") {
		t.Error("top-level help should document the --peer wrapper flag")
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
