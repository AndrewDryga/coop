package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestForkWorkspace(t *testing.T) {
	repo := "/home/me/proj"
	if got, want := forkHome(repo), "/home/me/proj-forks"; got != want {
		t.Errorf("forkHome = %q, want %q", got, want)
	}
	if got, want := forkWorkspace(repo, "perf"), "/home/me/proj-forks/perf"; got != want {
		t.Errorf("forkWorkspace = %q, want %q", got, want)
	}
}

// A mistyped fork subcommand must be a usage error with a suggestion — never silently turned into
// a new fork (clone + branch + agent). An explicit agent (a real create) bypasses the guard.
func TestForkTypoSuggestsSubcommand(t *testing.T) {
	repo := t.TempDir()
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	if code, err := a.cmdFork([]string{"reviw"}); code != 2 || err == nil || !strings.Contains(err.Error(), "review") {
		t.Errorf("coop fork reviw = (%d, %v), want (2, did-you-mean review)", code, err)
	}
	if pathExists(forkWorkspace(repo, "reviw")) {
		t.Error("a typo must not create a fork")
	}
	if got := forkVerbList(); !slices.Equal(got, []string{"acp", "logs", "ls", "merge", "open", "path", "review", "rm", "stop"}) {
		t.Errorf("forkVerbList = %v", got)
	}
}

func TestValidForkName(t *testing.T) {
	for _, n := range []string{"perf", "deps", "fix-1", "fix_2", "a.b"} {
		if !validForkName(n) {
			t.Errorf("validForkName(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"", "ls", "review", "merge", "rm", "open", "path", "acp", "a/b", `a\b`, "..", ".", ".hidden", "a..b", "a.", "a.lock", "-x",
		"my fork", "a\tb", "a\nb", "a=b", "a;b", "a$(id)", "a`id`", "a|b", "a&b", `a'b`, `a"b`} {
		if validForkName(n) {
			t.Errorf("validForkName(%q) = true, want false", n)
		}
	}
}

// Regression: a fork name that escapes the forks home or carries shell syntax must be refused by
// EVERY name-taking verb before it reaches a workspace, subprocess, or runtime call. `coop fork rm
// ..` used to filepath.Join-clean to the parent of all projects and delete it. Each guard runs before
// box.ResolveRepo, so an unsafe name returns without touching the filesystem.
func TestForkVerbsRejectUnsafeName(t *testing.T) {
	repo := t.TempDir()
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	verbs := map[string]func([]string) (int, error){
		"rm": a.forkRm, "stop": a.forkStop, "open": a.forkOpenEditor,
		"logs": a.forkLogs, "review": a.forkReview, "path": a.forkPath, "merge": a.forkMerge,
	}
	for _, name := range []string{
		".", "..", "../coop", "a/b", "bad name", "bad;name", "bad$(id)", "bad`id`",
		"bad|name", "bad&name", `bad'name`, `bad"name`, "bad\nname", "-bad",
	} {
		for verb, fn := range verbs {
			code, err := fn([]string{name})
			wantInvalidName := !strings.HasPrefix(name, "-")
			if code != 2 || err == nil || (wantInvalidName && !strings.Contains(err.Error(), "invalid fork name")) {
				t.Errorf("fork %s %q = (%d, %v), want an exit-2 rejection before side effects", verb, name, code, err)
			}
		}
		// --fresh recreates a fork (clone + destroy); it routes through forkCreate, which rejects the
		// name in parseForkCreate before any clone or destroy work — a refusal that creates nothing.
		if code, err := a.cmdFork([]string{name, "--fresh"}); code != 2 || err == nil {
			t.Errorf("fork %q --fresh = (%d, %v), want a refusal (2, err)", name, code, err)
		}
	}
}

// A 3-rune typo of a fork verb (the audit's stray `lss`) must be caught and suggested, not silently
// cloned — but an explicit agent, or an already-existing fork of that name, is a deliberate create.
func TestForkVerbNearMiss(t *testing.T) {
	cases := []struct {
		args      []string
		exists    bool
		want      string
		wantMatch bool
	}{
		{[]string{"lss"}, false, "ls", true},          // distance 1 from `ls` — the most-typed verb
		{[]string{"stp"}, false, "stop", true},        // 3 runes, distance 1 from `stop`
		{[]string{"lss", "claude"}, false, "", false}, // explicit agent → deliberate create of `lss`
		{[]string{"lss"}, true, "", false},            // already a fork → open it, don't second-guess
		{[]string{"ls"}, false, "", false},            // 2 runes → below the suggestion floor
		{[]string{"api"}, false, "", false},           // a real new fork name, far from any verb
	}
	for _, c := range cases {
		if got, ok := forkVerbNearMiss(c.args, c.exists); ok != c.wantMatch || got != c.want {
			t.Errorf("forkVerbNearMiss(%v, exists=%v) = (%q,%v), want (%q,%v)", c.args, c.exists, got, ok, c.want, c.wantMatch)
		}
	}
}

// End-to-end: `coop fork lss` (no agent) is refused before any clone — exit 2 with the suggestion.
func TestCmdForkRefusesVerbTypo(t *testing.T) {
	a := &app{cfg: &config.Config{RepoOverride: t.TempDir()}}
	code, err := a.cmdFork([]string{"lss"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "did you mean 'coop fork ls'") {
		t.Fatalf("cmdFork([lss]) = (%d, %v), want (2, a 'did you mean ls' refusal)", code, err)
	}
}

// Every fork subcommand + accepted alias + `watch` is a reserved name (can't be a fork), so no fork
// shadows a subcommand; a real fork name is not reserved.
func TestForkReserved(t *testing.T) {
	for _, r := range []string{"ls", "review", "merge", "rm", "open", "logs", "stop", "path", "acp", "watch"} {
		if !forkReserved(r) {
			t.Errorf("forkReserved(%q) = false, want true", r)
		}
		if validForkName(r) {
			t.Errorf("validForkName(%q) = true, want false (a fork can't be named a reserved verb)", r)
		}
	}
	// v3 dropped the list/remove aliases, so they are ordinary (allowed) fork names now.
	for _, ok := range []string{"api", "web", "feature-x", "wip2", "list", "remove"} {
		if forkReserved(ok) {
			t.Errorf("forkReserved(%q) = true, want false", ok)
		}
		if !validForkName(ok) {
			t.Errorf("validForkName(%q) = false, want true", ok)
		}
	}
}

// v3 dropped the `list` alias: `coop fork list` (no agent) is refused as a near-miss of `ls`, not
// treated as a lister. (A fork literally named "list" still works with an explicit agent.)
func TestCmdForkListRetired(t *testing.T) {
	a := &app{cfg: &config.Config{RepoOverride: t.TempDir()}}
	code, err := a.cmdFork([]string{"list"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "coop fork ls") {
		t.Fatalf("cmdFork([list]) = (%d, %v), want (2, a near-miss pointing at `coop fork ls`)", code, err)
	}
}

// A typo'd --profile must fail (exit 2) before any image/clone work, so it never leaves a stray
// fork behind. The check runs before resolveImage, so it returns without a runtime.
func TestForkCreateRejectsUnknownProfileBeforeClone(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}} // no profiles signed in
	if code, err := a.forkCreate([]string{"scratchfork", "claude", "--profile", "ghost"}); code != 2 || err == nil {
		t.Fatalf("forkCreate with a bad --profile = (%d, %v), want (2, error before any clone)", code, err)
	}
}

func TestForkCreateAcceptsEnvOnlyDefaultProfileBeforeImage(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir(), BoxHome: t.TempDir()}
	if err := os.WriteFile(cfg.EnvFile(), []byte("OPENAI_API_KEY=token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetDefaultProfile("codex", "work"); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg, rt: runtime.Runtime{Name: "false"}, rtSet: true}
	code, err := a.forkCreate([]string{"scratchfork", "codex@work"})
	if err == nil {
		t.Fatal("forkCreate unexpectedly reached a runnable image")
	}
	if code == 2 && strings.Contains(err.Error(), "no account") {
		t.Fatalf("env-only default was rejected before image lookup: (%d, %v)", code, err)
	}
	if pathExists(forkWorkspace(cfg.RepoOverride, "scratchfork")) {
		t.Error("failed image lookup must not create a fork")
	}
}

func TestForkAgentMemory(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	excludeFork(ws, ".coop/") // setupFork establishes this before the first provider run
	// A fork with no memory yet.
	if got := readForkAgent(ws); got != "" {
		t.Errorf("readForkAgent(fresh) = %q, want empty", got)
	}
	// Persist, read back, and confirm it's git-excluded so it never lands.
	saveForkAgent(ws, "codex")
	if got := readForkAgent(ws); got != "codex" {
		t.Errorf("readForkAgent after save = %q, want codex", got)
	}
	excl, _ := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if !strings.Contains(string(excl), ".coop/") {
		t.Errorf(".git/info/exclude missing .coop/: %q", excl)
	}
	// An explicit switch updates the memory; the exclude isn't duplicated.
	saveForkAgent(ws, "gemini")
	if got := readForkAgent(ws); got != "gemini" {
		t.Errorf("readForkAgent after switch = %q, want gemini", got)
	}
	excl2, _ := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if strings.Count(string(excl2), ".coop/") != 1 {
		t.Errorf("exclude duplicated .coop/: %q", excl2)
	}
	// A garbage value is ignored (not a known agent).
	if err := os.WriteFile(forkAgentFile(ws), []byte("bogus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readForkAgent(ws); got != "" {
		t.Errorf("readForkAgent(bogus) = %q, want empty", got)
	}
}

func TestForkMetadataRefusesSymlinks(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	want := "11111111-2222-4333-8444-555555555555\n"
	if err := os.WriteFile(sentinel, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}

	meta := filepath.Join(ws, ".coop")
	if err := os.Symlink(outside, meta); err != nil {
		t.Fatal(err)
	}
	saveForkAgent(ws, "claude")
	if pathExists(filepath.Join(outside, "agent")) {
		t.Fatal("fork metadata write followed a symlinked .coop directory")
	}
	if err := os.Remove(meta); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{forkAgentFile(ws), forkSessionFile(ws, "claude", "work")} {
		if err := os.Symlink(sentinel, path); err != nil {
			t.Fatal(err)
		}
	}
	if got := readForkAgent(ws); got != "" {
		t.Fatalf("readForkAgent followed a symlink: %q", got)
	}
	if got := readForkSession(ws, "claude", "work"); got != "" {
		t.Fatalf("readForkSession followed a symlink: %q", got)
	}
	saveForkAgent(ws, "gemini")
	saveForkSession(ws, "claude", "work", strings.TrimSpace(want))
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != want {
		t.Fatalf("fork metadata write changed symlink target: %q, %v", data, err)
	}
	if got := readForkAgent(ws); got != "gemini" {
		t.Fatalf("safe agent replacement = %q, want gemini", got)
	}
	if got := readForkSession(ws, "claude", "work"); got != strings.TrimSpace(want) {
		t.Fatalf("safe session replacement = %q", got)
	}
}

func TestParseForkCreateAgentSet(t *testing.T) {
	if fa, _ := parseForkCreate([]string{"perf"}); fa.agentSet {
		t.Error("parseForkCreate(perf): agentSet should be false (defaulted)")
	}
	if fa, _ := parseForkCreate([]string{"perf", "codex"}); !fa.agentSet || fa.agent != "codex" {
		t.Errorf("parseForkCreate(perf codex): agentSet=%v agent=%q, want true/codex", fa.agentSet, fa.agent)
	}
}

func TestParseForkCreate(t *testing.T) {
	tests := []struct {
		args      []string
		wantAgent string
		wantFresh bool
		wantErr   bool
	}{
		{[]string{"perf"}, "", false, false}, // no positional who → "" (forkCreate errors later if no preset)
		{[]string{"perf", "codex"}, "codex", false, false},
		{[]string{"perf", "gemini", "--fresh"}, "gemini", true, false},
		{[]string{}, "", false, true},
		{[]string{"perf", "bogus"}, "", false, false}, // "bogus" is a preset NAME now (agent stays ""; validated later), not an error
		{[]string{"ls"}, "", false, true},             // reserved name
	}
	for _, tc := range tests {
		fa, err := parseForkCreate(tc.args)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseForkCreate(%v) err = %v, wantErr %v", tc.args, err, tc.wantErr)
			continue
		}
		if tc.wantErr {
			continue
		}
		if fa.agent != tc.wantAgent || fa.fresh != tc.wantFresh {
			t.Errorf("parseForkCreate(%v) = {agent:%q fresh:%v}, want {agent:%q fresh:%v}",
				tc.args, fa.agent, fa.fresh, tc.wantAgent, tc.wantFresh)
		}
	}
}

// TestParseForkCreatePeer: --peer <peer> is repeatable and rides a loop fork's args; on a
// non-loop fork it's rejected (an interactive fork has no ad-hoc peer set), a valueless --peer
// errors (it takes a value), and the retired --consult is now just an unknown arg.
func TestParseForkCreatePeer(t *testing.T) {
	fa, err := parseForkCreate([]string{"perf", "claude", "--loop", "--peer", "codex", "--peer", "gemini:gemini-3.5-flash"})
	if err != nil || !slices.Equal(fa.peers, []string{"codex", "gemini:gemini-3.5-flash"}) {
		t.Errorf("parseForkCreate --peer (repeatable) = ({peers:%v}, %v), want [codex gemini:gemini-3.5-flash]", fa.peers, err)
	}
	if _, err := parseForkCreate([]string{"perf", "claude", "--loop", "--peer"}); err == nil {
		t.Error("parseForkCreate: a valueless --peer must error (it takes a value)")
	}
	if _, err := parseForkCreate([]string{"perf", "claude", "--peer", "codex"}); err == nil {
		t.Error("parseForkCreate: --peer without --loop must error (name peers on a loop)")
	}
	if _, err := parseForkCreate([]string{"perf", "claude", "--loop", "--consult", "codex"}); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("parseForkCreate: --consult is retired — should be an unknown flag now, got %v", err)
	}
}

// TestParseForkCreateTarget: the positional target's :model and @account fold into forkArgs;
// --model/--credential are not fork flags — they error as unknown args.
func TestParseForkCreateTarget(t *testing.T) {
	fa, err := parseForkCreate([]string{"perf", "codex:gpt-5"})
	if err != nil || fa.agent != "codex" || fa.model != "gpt-5" || fa.credential != "" {
		t.Errorf("parseForkCreate codex:gpt-5 = ({agent:%q model:%q cred:%q}, %v), want codex/gpt-5/\"\"", fa.agent, fa.model, fa.credential, err)
	}
	fa, err = parseForkCreate([]string{"perf", "claude:opus-4.8@work", "--loop"})
	if err != nil || fa.agent != "claude" || fa.model != "opus-4.8" || fa.credential != "work" {
		t.Errorf("parseForkCreate claude:opus-4.8@work = ({agent:%q model:%q cred:%q}, %v), want claude/opus-4.8/work", fa.agent, fa.model, fa.credential, err)
	}
	// An account ladder (@a,b) only rotates under `coop loop` — a fork takes one account.
	if _, err := parseForkCreate([]string{"perf", "claude@work,personal"}); err == nil {
		t.Error("parseForkCreate: a >1-account target must error on a fork (loop-only ladder)")
	}
	for _, bad := range [][]string{
		{"perf", "codex", "--model", "gpt-5"},
		{"perf", "--model=opus", "--loop"},
		{"perf", "claude", "--credential", "work"},
		{"perf", "claude", "--credential=work"},
	} {
		if _, err := parseForkCreate(bad); err == nil {
			t.Errorf("parseForkCreate(%v): --model/--credential must error (unknown flag), not parse", bad)
		}
	}
}

func TestForkRmSafe(t *testing.T) {
	tests := []struct {
		unmerged, dirty, force bool
		wantErr                bool
	}{
		{false, false, false, false}, // clean & merged → ok
		{true, false, false, true},   // unmerged → blocked
		{false, true, false, true},   // dirty → blocked
		{true, true, true, false},    // force overrides everything
	}
	for _, tc := range tests {
		err := forkRmSafe(tc.unmerged, tc.dirty, tc.force)
		if (err != nil) != tc.wantErr {
			t.Errorf("forkRmSafe(unmerged=%v dirty=%v force=%v) err = %v, wantErr %v",
				tc.unmerged, tc.dirty, tc.force, err, tc.wantErr)
		}
	}
}

// TestForkRmRefusesRunning: `coop fork rm` must refuse a fork whose loop is running (its worktree
// is bind-mounted RW into a live container) and must NOT delete the worktree — like merge/prune do.
func TestForkRmRefusesRunning(t *testing.T) {
	repo := t.TempDir()
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	ws := forkWorkspace(repo, "busy")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	// Mark it running by claiming the pidfile for THIS (live) process.
	if err := writeForkPid(repo, "busy", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	code, err := a.forkRm([]string{"busy"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "running or awaiting cleanup") {
		t.Errorf("fork rm of a running fork = (%d, %v), want (1, running or awaiting cleanup)", code, err)
	}
	if !pathExists(ws) {
		t.Error("fork rm refused but still deleted the running fork's worktree")
	}
}

func TestOneForkName(t *testing.T) {
	if n, err := oneForkName("rm", []string{"x"}); n != "x" || err != nil {
		t.Errorf("oneForkName(1) = (%q, %v), want (x, nil)", n, err)
	}
	if n, err := oneForkName("rm", nil); n != "" || err != nil {
		t.Errorf("oneForkName(0) = (%q, %v), want (\"\", nil)", n, err)
	}
	if _, err := oneForkName("rm", []string{"a", "b"}); err == nil || !strings.Contains(err.Error(), "got a, b") {
		t.Errorf("oneForkName(2) should error naming both, got %v", err)
	}
}

// rm/merge/stop/logs must reject a SECOND positional (they used to silently act on only the last and
// report success). The check fires before any repo/clone work, so a bare app suffices.
func TestForkVerbsRejectSecondPositional(t *testing.T) {
	a := &app{cfg: &config.Config{RepoOverride: t.TempDir()}}
	for verb, fn := range map[string]func([]string) (int, error){
		"rm": a.forkRm, "merge": a.forkMerge, "stop": a.forkStop, "logs": a.forkLogs,
	} {
		code, err := fn([]string{"aaa", "bbb"})
		if code != 2 || err == nil || !strings.Contains(err.Error(), "one name (got aaa, bbb)") {
			t.Errorf("fork %s a b = (%d, %v), want (2, 'takes one name (got aaa, bbb)')", verb, code, err)
		}
	}
}

func TestParseForkCreateForce(t *testing.T) {
	for _, flag := range []string{"--force", "-f"} {
		fa, err := parseForkCreate([]string{"myfork", "--fresh", flag})
		if err != nil || !fa.force || !fa.fresh {
			t.Errorf("parseForkCreate(%s) = fa{force:%v fresh:%v} err=%v, want force+fresh", flag, fa.force, fa.fresh, err)
		}
	}
}

func TestParseForkCreateFreshConfirmation(t *testing.T) {
	for _, flag := range []string{"--yes", "-y"} {
		fa, err := parseForkCreate([]string{"myfork", "--fresh", flag})
		if err != nil || !fa.fresh || !fa.yes {
			t.Errorf("parseForkCreate(%s) = fa{fresh:%v yes:%v} err=%v, want fresh+yes", flag, fa.fresh, fa.yes, err)
		}
	}
	if _, err := parseForkCreate([]string{"myfork", "--yes"}); err == nil || !strings.Contains(err.Error(), "only applies with --fresh") {
		t.Fatalf("parseForkCreate(--yes without --fresh) = %v, want scoped-flag refusal", err)
	}
}

// `coop fork rm` confirms before the unrecoverable delete: without --yes and no TTY it refuses and
// keeps the fork; --yes deletes. (The unmerged/dirty guard is separate — see TestForkRmSafe.)
func TestForkRmGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	ws, err := setupFork(repo, "perf")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	code, err := a.forkRm([]string{"perf"}) // no --yes, no TTY → refuse
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("fork rm without --yes = (%d, %v), want (2, a refusal naming --yes)", code, err)
	}
	if !pathExists(ws) {
		t.Error("a refused fork rm must not delete the fork")
	}
	if code, err := a.forkRm([]string{"perf", "--yes"}); code != 0 || err != nil {
		t.Fatalf("fork rm --yes = (%d, %v), want (0, nil)", code, err)
	}
	if pathExists(ws) {
		t.Error("fork rm --yes should delete the fork")
	}
}

func TestForkRmConfirmsBeforeForcedStop(t *testing.T) {
	repo := initRepo(t)
	ws, err := setupFork(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeForkPid(repo, "perf", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.forkRm([]string{"perf", "--force"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("forced fork rm without confirmation = (%d, %v), want refusal naming --yes", code, err)
	}
	if !pathExists(ws) || forkRunningPid(repo, "perf") != os.Getpid() {
		t.Fatal("unconfirmed forced fork rm stopped its worker or deleted the workspace")
	}
}

type forkCommandResult struct {
	code int
	err  error
}

func runForkCommandAcrossLockedMutation(t *testing.T, repo, name string, command func() (int, error), mutate func()) forkCommandResult {
	t.Helper()
	unlock, err := lockForkState(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()

	result := make(chan forkCommandResult, 1)
	go func() {
		code, err := command()
		result <- forkCommandResult{code: code, err: err}
	}()
	select {
	case got := <-result:
		t.Fatalf("fork command bypassed lifecycle lock: (%d, %v)", got.code, got.err)
	case <-time.After(80 * time.Millisecond):
	}
	mutate()
	unlock()
	locked = false
	select {
	case got := <-result:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("fork command remained blocked after lifecycle unlock")
		return forkCommandResult{}
	}
}

func TestForkRmRechecksLifecycleAfterConfirmation(t *testing.T) {
	repo := initRepo(t)
	ws, err := setupFork(repo, "race")
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	got := runForkCommandAcrossLockedMutation(t, repo, "race", func() (int, error) {
		return a.forkRm([]string{"race", "--yes"})
	}, func() {
		if err := os.WriteFile(forkPid(repo, "race"), []byte(forkReapPending), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if got.code != 1 || got.err == nil || !strings.Contains(got.err.Error(), "started while awaiting confirmation") {
		t.Fatalf("fork rm after lifecycle change = (%d, %v), want retryable refusal", got.code, got.err)
	}
	if !pathExists(ws) {
		t.Fatal("fork rm deleted a workspace that became reserved while confirmation was open")
	}
}

func TestForkRmRefusesReplacementAfterConfirmation(t *testing.T) {
	repo := initRepo(t)
	ws, err := setupFork(repo, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	marker := filepath.Join(ws, "replacement.txt")
	got := runForkCommandAcrossLockedMutation(t, repo, "replacement", func() (int, error) {
		return a.forkRm([]string{"replacement", "--force", "--yes"})
	}, func() {
		if err := os.RemoveAll(ws); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("keep\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if got.code != 1 || got.err == nil || !strings.Contains(got.err.Error(), "was replaced") {
		t.Fatalf("fork rm after replacement = (%d, %v), want replacement refusal", got.code, got.err)
	}
	if !pathExists(marker) {
		t.Fatal("fork rm deleted the replacement workspace")
	}
}

func TestForkRmRefusesSymlinkReplacementAfterConfirmation(t *testing.T) {
	repo := initRepo(t)
	ws, err := setupFork(repo, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	moved := ws + "-moved"
	marker := filepath.Join(moved, "replacement.txt")
	got := runForkCommandAcrossLockedMutation(t, repo, "replacement", func() (int, error) {
		return a.forkRm([]string{"replacement", "--force", "--yes"})
	}, func() {
		if err := os.Rename(ws, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, ws); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("keep\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if got.code != 1 || got.err == nil || !strings.Contains(got.err.Error(), "was replaced") {
		t.Fatalf("fork rm after symlink replacement = (%d, %v), want replacement refusal", got.code, got.err)
	}
	if !pathExists(marker) {
		t.Fatal("fork rm followed and deleted the symlink replacement")
	}
}

// `coop fork <name> --fresh` refuses to recreate a fork with uncommitted work unless --force, and the
// refusal happens BEFORE any image work — so the dirty work survives. (--fresh --force reclones, which
// needs a runtime, so only the refusal path is exercised here.)
func TestForkFreshGuardsDirtyWork(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	ws, err := setupFork(repo, "perf")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "wip.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A") // staged, uncommitted → dirty
	code, err := a.forkCreate([]string{"perf", "--fresh"})
	if code == 0 || err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("fork --fresh on a dirty fork = (%d, %v), want a refusal mentioning --force", code, err)
	}
	if !pathExists(filepath.Join(ws, "wip.txt")) {
		t.Error("a refused --fresh must not have destroyed the dirty work")
	}
}

func TestForkFreshConfirmsBeforeRuntimeWork(t *testing.T) {
	repo := initRepo(t)
	ws, err := setupFork(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.forkCreate([]string{"perf", "claude", "--fresh"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("fork --fresh without confirmation = (%d, %v), want refusal naming --yes", code, err)
	}
	if !pathExists(ws) {
		t.Fatal("unconfirmed --fresh deleted the fork")
	}
}

func TestForkFreshConfirmsBeforeForcedStop(t *testing.T) {
	repo := initRepo(t)
	ws, err := setupFork(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeForkPid(repo, "perf", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.forkCreate([]string{"perf", "claude", "--fresh", "--force"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("forced --fresh without confirmation = (%d, %v), want refusal naming --yes", code, err)
	}
	if !pathExists(ws) || forkRunningPid(repo, "perf") != os.Getpid() {
		t.Fatal("unconfirmed forced --fresh stopped its worker or deleted the workspace")
	}
}

func TestForkFreshRefusesStateOnlyCleanup(t *testing.T) {
	repo := initRepo(t)
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeForkWorkerState(repo, "perf", forkWorkerState{pending: true}); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.forkCreate([]string{"perf", "claude", "--fresh"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "awaiting cleanup") || !strings.Contains(err.Error(), "coop fork stop perf") {
		t.Fatalf("fork --fresh with state-only cleanup = (%d, %v), want cleanup refusal", code, err)
	}
	if !pathExists(forkPid(repo, "perf")) || pathExists(forkWorkspace(repo, "perf")) {
		t.Fatal("refused state-only --fresh changed lifecycle state")
	}
}

func TestForkFreshRechecksLifecycleUnderLock(t *testing.T) {
	dir := t.TempDir()
	repo := initRepo(t)
	ws, err := setupFork(repo, "race")
	if err != nil {
		t.Fatal(err)
	}
	runtimeCLI := filepath.Join(dir, "runtime")
	if err := os.WriteFile(runtimeCLI, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}, rt: runtime.Runtime{Name: runtimeCLI}, rtSet: true}
	got := runForkCommandAcrossLockedMutation(t, repo, "race", func() (int, error) {
		return a.forkCreate([]string{"race", "claude", "--fresh", "--yes"})
	}, func() {
		if err := writeForkWorkerState(repo, "race", forkWorkerState{pending: true}); err != nil {
			t.Fatal(err)
		}
	})
	if got.code != 1 || got.err == nil || !strings.Contains(got.err.Error(), "entered cleanup") {
		t.Fatalf("fork --fresh after lifecycle change = (%d, %v), want retryable refusal", got.code, got.err)
	}
	if !pathExists(ws) {
		t.Fatal("fork --fresh deleted a workspace that entered cleanup")
	}
}

func TestForkFreshRefusesReplacementAfterConfirmation(t *testing.T) {
	dir := t.TempDir()
	repo := initRepo(t)
	ws, err := setupFork(repo, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	runtimeCLI := filepath.Join(dir, "runtime")
	if err := os.WriteFile(runtimeCLI, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}, rt: runtime.Runtime{Name: runtimeCLI}, rtSet: true}
	marker := filepath.Join(ws, "replacement.txt")
	got := runForkCommandAcrossLockedMutation(t, repo, "replacement", func() (int, error) {
		return a.forkCreate([]string{"replacement", "claude", "--fresh", "--force", "--yes"})
	}, func() {
		if err := os.RemoveAll(ws); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("keep\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if got.code != 1 || got.err == nil || !strings.Contains(got.err.Error(), "was replaced") {
		t.Fatalf("fork --fresh after replacement = (%d, %v), want replacement refusal", got.code, got.err)
	}
	if !pathExists(marker) {
		t.Fatal("fork --fresh deleted the replacement workspace")
	}
}

// `coop fork ls` UPDATED must reflect the fork's OWN activity, not the base commit time it inherited
// from the clone — a fresh fork off a year-old base shows ~its creation, not "1 year ago".
func TestForkUpdatedShowsOwnActivityNotInheritedBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	// Backdate the parent HEAD so an inherited base time would read as clearly stale.
	old := exec.Command("git", "-C", repo, "commit", "--allow-empty", "-qm", "old base")
	old.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_AUTHOR_DATE=2020-01-01T00:00:00", "GIT_COMMITTER_DATE=2020-01-01T00:00:00")
	if out, err := old.CombinedOutput(); err != nil {
		t.Fatalf("backdate base: %v\n%s", err, out)
	}
	ws, err := setupFork(repo, "perf")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	if got := forkUpdated(repo, ws); strings.Contains(got, "year") {
		t.Errorf("fresh fork UPDATED = %q, want ~creation time, not the inherited 2020 base commit", got)
	}
	// Its own commit is what should show once it works.
	if err := os.WriteFile(filepath.Join(ws, "x.txt"), []byte("w\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "fork work")
	if got := forkUpdated(repo, ws); got == "—" || strings.Contains(got, "year") {
		t.Errorf("fork with its own commit should show that commit's recent time, got %q", got)
	}
}

// review's "why" reads only COMPLETED (99_done) task logs, so a seeded 00_todo template — even with a
// newer mtime — never masquerades as the fork's work; no completed task → empty (caller says none yet).
func TestLatestTaskLogOnlyDone(t *testing.T) {
	ws := t.TempDir()
	writeTaskFile(t, filepath.Join(ws, tasksRoot, stateDone, "mine", "log.md"), "FORK OWN WORK\n")
	writeTaskFile(t, filepath.Join(ws, tasksRoot, stateTodo, "seed", "log.md"), "SEEDED TEMPLATE\n") // newer mtime
	if got := latestTaskLog(ws, 5); !strings.Contains(got, "FORK OWN WORK") || strings.Contains(got, "SEEDED") {
		t.Errorf("latestTaskLog = %q, want the 99_done log, not the newer seeded todo template", got)
	}
	empty := t.TempDir()
	writeTaskFile(t, filepath.Join(empty, tasksRoot, stateTodo, "x", "log.md"), "todo only\n")
	if got := latestTaskLog(empty, 5); got != "" {
		t.Errorf("latestTaskLog with no completed tasks = %q, want empty (caller renders 'none yet')", got)
	}
}

// `coop fork <name> acp claude@ghost` pins the account in the positional target (like plain
// `coop acp`); an unknown account errors on the account itself. A stray --credential is a
// rejected arg.
func TestForkACPAcceptsCredential(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	if code, err := a.forkACP("myfork", nil); code != 2 || err == nil || strings.Contains(err.Error(), "preset") {
		t.Errorf("fork acp without a target = (%d, %v), want provider-only guidance", code, err)
	}
	code, err := a.forkACP("myfork", []string{"claude@ghost"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("fork acp claude@ghost = (%d, %v), want (2, an account error naming ghost)", code, err)
	}
	if strings.Contains(err.Error(), "unexpected") || strings.Contains(err.Error(), "usage:") {
		t.Errorf("a target's @account should be accepted, not rejected as an argument: %v", err)
	}
	if code, err := a.forkACP("myfork", []string{"claude", "--credential", "ghost"}); code != 2 || err == nil {
		t.Errorf("fork acp --credential = (%d, %v), want (2, error)", code, err)
	}
}

func TestParseShortstat(t *testing.T) {
	ins, del := parseShortstat(" 3 files changed, 42 insertions(+), 7 deletions(-)")
	if ins != 42 || del != 7 {
		t.Errorf("parseShortstat = (%d, %d), want (42, 7)", ins, del)
	}
	if ins, del := parseShortstat(""); ins != 0 || del != 0 {
		t.Errorf("parseShortstat(empty) = (%d, %d), want (0, 0)", ins, del)
	}
}

func TestIndentLastLines(t *testing.T) {
	if got := indent("a\nb"); got != "  a\n  b" {
		t.Errorf("indent = %q, want %q", got, "  a\n  b")
	}
	if got := lastLines("a\nb\nc\nd", 2); got != "c\nd" {
		t.Errorf("lastLines(.., 2) = %q, want %q", got, "c\nd")
	}
	if got := lastLines("a\nb", 5); got != "a\nb" {
		t.Errorf("lastLines short = %q, want %q", got, "a\nb")
	}
}

// --- git-backed lifecycle ---

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "myrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	git(t, repo, "checkout", "-q", "-b", "main")
	git(t, repo, "config", "user.email", "t@t") // so merge-commits work without ambient identity
	git(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "init")
	return repo
}

func TestForkLifecycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	// setupFork clones + branches.
	ws, err := setupFork(repo, "perf")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	if !pathExists(ws) {
		t.Fatalf("workspace %s not created", ws)
	}
	if got := gitBranch(ws); got != "perf" {
		t.Errorf("fork branch = %q, want %q", got, "perf")
	}
	// The fork must carry the parent's git identity so an agent can commit in it.
	if got := gitOut(ws, "config", "user.email"); got != "t@t" {
		t.Errorf("fork git identity not propagated: user.email = %q, want %q", got, "t@t")
	}

	// A commit in the fork is "unmerged" from the parent's point of view.
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "do the work")
	if !forkUnmerged(repo, ws) {
		t.Error("fork with new commit should be unmerged")
	}

	// review fetches the branch into review/perf.
	if err := gitFetchInto(repo, ws, "perf"); err != nil {
		t.Fatalf("gitFetchInto: %v", err)
	}
	if gitOut(repo, "rev-parse", "--verify", "-q", "review/perf") == "" {
		t.Error("review/perf ref not created")
	}

	// merge lands it; now it's merged.
	git(t, repo, "merge", "--no-edit", "review/perf")
	if forkUnmerged(repo, ws) {
		t.Error("fork should be merged after git merge")
	}
	if !pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("merged file not present in parent repo")
	}

	// destroyFork removes the workspace and the review ref.
	if err := destroyFork(repo, "perf"); err != nil {
		t.Fatalf("destroyFork: %v", err)
	}
	if pathExists(ws) {
		t.Error("workspace not removed")
	}
	if gitOut(repo, "rev-parse", "--verify", "-q", "review/perf") != "" {
		t.Error("review/perf ref not removed")
	}
}

func TestForkCarriesGlobalIgnore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo := initRepo(t)
	// Your real GLOBAL gitignore is carried into the fork; a repo-local core.excludesfile is
	// IGNORED — it's agent-writable, so reading it would let a poisoned repo point us at a host
	// secret (e.g. ~/.ssh/id_rsa) and copy its content into the fork. (`--global` ignores -C.)
	ignore := filepath.Join(t.TempDir(), "ignore")
	if err := os.WriteFile(ignore, []byte("*.tmp\n.DS_Store\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "--global", "core.excludesfile", ignore)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("SECRET_TOKEN_xyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "core.excludesfile", secret) // repo-local poison: must NOT be read

	ws, err := setupFork(repo, "ig")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	excl, err := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(excl), "*.tmp") || !strings.Contains(string(excl), ".DS_Store") {
		t.Errorf("global ignore not carried into the fork's .git/info/exclude:\n%s", excl)
	}
	if strings.Contains(string(excl), "SECRET_TOKEN") {
		t.Fatalf("a repo-local core.excludesfile (a host secret) was read and copied into the fork:\n%s", excl)
	}
}

func TestForkCarriesSigningMaterials(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	git(t, repo, "config", "gpg.format", "ssh")
	git(t, repo, "config", "user.signingkey", "/home/me/.ssh/id_ed25519.pub")
	git(t, repo, "config", "commit.gpgsign", "true")

	ws, err := setupFork(repo, "s")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	// The key + format travel so the rebase-on-land can sign.
	if got := gitOut(ws, "config", "--get", "gpg.format"); got != "ssh" {
		t.Errorf("gpg.format not propagated: %q", got)
	}
	if gitOut(ws, "config", "--get", "user.signingkey") == "" {
		t.Error("user.signingkey not propagated")
	}
	// commit.gpgsign must NOT be in the fork's local config, or the keyless box would
	// try to sign and every agent commit would fail.
	if got := gitOut(ws, "config", "--local", "--get", "commit.gpgsign"); got != "" {
		t.Errorf("commit.gpgsign must not be copied to the fork's local config, got %q", got)
	}
}

func TestHelpRequested(t *testing.T) {
	if !helpRequested([]string{"review", "demo", "--help"}) || !helpRequested([]string{"-h"}) {
		t.Error("helpRequested should detect -h/--help anywhere in args")
	}
	if helpRequested([]string{"demo", "codex"}) {
		t.Error("helpRequested should be false with no help flag")
	}
}

func TestForkHelp(t *testing.T) {
	if code, err := forkHelp(); code != 0 || err != nil {
		t.Errorf("forkHelp = (%d, %v), want (0, nil)", code, err)
	}
}

func TestParseForkContinue(t *testing.T) {
	if fa, err := parseForkCreate([]string{"demo", "-c"}); err != nil || !fa.cont {
		t.Errorf("parseForkCreate(demo -c) = {cont:%v}, err=%v; want cont", fa.cont, err)
	}
	if fa, err := parseForkCreate([]string{"demo", "codex", "--continue"}); err != nil || !fa.cont || fa.agent != "codex" {
		t.Errorf("parseForkCreate(demo codex --continue) = {cont:%v agent:%q}, err=%v", fa.cont, fa.agent, err)
	}
	if _, err := parseForkCreate([]string{"demo", "codex", "--continue", "--new"}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("parseForkCreate(--continue --new) = %v, want mutually exclusive error", err)
	}
}

func TestForkLaunchCmd(t *testing.T) {
	cfgDir := t.TempDir()
	ws := filepath.Join(t.TempDir(), "myrepo-forks", "demo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{ConfigDir: cfgDir}}

	// First launch of a preset agent (claude): start under a fresh coop-owned id and
	// persist it; the command carries --session-id <uuid>.
	cmd := a.forkLaunchCmd(forkArgs{name: "demo", agent: "claude"}, ws, false)
	id := readForkSession(ws, "claude", "default")
	if id == "" {
		t.Fatal("first launch did not persist a session id")
	}
	if !slices.Contains(cmd, "--session-id") || !slices.Contains(cmd, id) {
		t.Errorf("first launch cmd = %v, want --session-id %s", cmd, id)
	}

	// Re-entry once the session exists: resume exactly that id (not --continue/latest).
	// Place the fake session where the adapter resolves it — via the same key function, so
	// this test can't drift from Claude Code's project-dir encoding.
	claudeKey := agents.ClaudeProjectKey(ws)
	sess := filepath.Join(a.cfg.AgentDir("claude"), "projects", claudeKey, id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(sess), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sess, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = a.forkLaunchCmd(forkArgs{name: "demo", agent: "claude"}, ws, true)
	if !slices.Contains(cmd, "--resume") || !slices.Contains(cmd, id) {
		t.Errorf("re-entry cmd = %v, want --resume %s", cmd, id)
	}
	if readForkSession(ws, "claude", "default") != id {
		t.Error("session id changed on re-entry")
	}

	// Native histories use the cwd visible inside the box, which may differ from the host fork
	// path under COOP_WORKDIR. The .coop hint remains in the host workspace.
	overrideWS := filepath.Join(t.TempDir(), "myrepo-forks", "override")
	if err := os.MkdirAll(overrideWS, 0o755); err != nil {
		t.Fatal(err)
	}
	a.cfg.Workdir = "/workspace/fork"
	saveForkSession(overrideWS, "claude", "default", id)
	overrideSession := filepath.Join(a.cfg.AgentDir("claude"), "projects", agents.ClaudeProjectKey(a.cfg.Workdir), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(overrideSession), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overrideSession, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = a.forkLaunchCmd(forkArgs{name: "override", agent: "claude"}, overrideWS, true)
	if !slices.Contains(cmd, "--resume") || !slices.Contains(cmd, id) {
		t.Errorf("COOP_WORKDIR re-entry cmd = %v, want --resume %s", cmd, id)
	}
	a.cfg.Workdir = ""

	// --new rotates the persisted id instead of launching a supposedly fresh session under the
	// existing conversation's id.
	cmd = a.forkLaunchCmd(forkArgs{name: "demo", agent: "claude", newSession: true}, ws, true)
	newID := readForkSession(ws, "claude", "default")
	if newID == "" || newID == id || !slices.Contains(cmd, "--session-id") || !slices.Contains(cmd, newID) || slices.Contains(cmd, "--resume") {
		t.Errorf("--new command/id = %v / %q, want a new persisted --session-id distinct from %q", cmd, newID, id)
	}

	// Re-entry when the session id was persisted but never materialized (e.g. quit
	// before a turn): fall back to a fresh start under the SAME id, not a resume error.
	ws2 := filepath.Join(t.TempDir(), "myrepo-forks", "ghost")
	if err := os.MkdirAll(ws2, 0o755); err != nil {
		t.Fatal(err)
	}
	saveForkSession(ws2, "claude", "default", id)
	cmd = a.forkLaunchCmd(forkArgs{name: "ghost", agent: "claude"}, ws2, true)
	if !slices.Contains(cmd, "--session-id") || slices.Contains(cmd, "--resume") {
		t.Errorf("ghost re-entry cmd = %v, want a fresh --session-id (no live session)", cmd)
	}

	// codex can't preset an id: no session file, and a fresh start is plain Interactive.
	cmd = a.forkLaunchCmd(forkArgs{name: "demo", agent: "codex"}, ws, false)
	if readForkSession(ws, "codex", "default") != "" {
		t.Error("codex must not get a coop-owned session id")
	}
	if slices.Contains(cmd, "--session-id") {
		t.Errorf("codex launch must not preset a session id: %v", cmd)
	}

	// Codex mints its own UUID. After the run, Coop discovers and persists it so a newer
	// same-cwd session cannot hijack this fork under a shared container workdir.
	codexID := "019f6a60-a28e-7d22-919c-81f43bef064f"
	codexDir := filepath.Join(a.cfg.AgentDir("codex"), "sessions", "2026", "07", "16")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	codexFile := filepath.Join(codexDir, "first.jsonl")
	if err := os.WriteFile(codexFile, []byte(`{"type":"session_meta","payload":{"id":"`+codexID+`","cwd":"`+ws+`","source":"cli"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	codexAdapter, _ := agents.Get("codex")
	discoverer := codexAdapter.(agents.SessionDiscoverer)
	a.rememberNewDiscoveredForkSession(ws, "codex", discoverer, nil)
	if got := readForkSession(ws, "codex", "default"); got != codexID {
		t.Fatalf("remembered Codex id = %q, want %q", got, codexID)
	}
	newerCodexID := "019f6a61-b39f-7e33-a811-92f64cf17550"
	newerFile := filepath.Join(codexDir, "newer.jsonl")
	if err := os.WriteFile(newerFile, []byte(`{"type":"session_meta","payload":{"id":"`+newerCodexID+`","cwd":"`+ws+`","source":"cli"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newerFile, time.Now().Add(time.Minute), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	cmd = a.forkLaunchCmd(forkArgs{name: "demo", agent: "codex"}, ws, true)
	if !slices.Contains(cmd, "resume") || !slices.Contains(cmd, codexID) || slices.Contains(cmd, newerCodexID) {
		t.Errorf("Codex exact re-entry cmd = %v, want persisted %s", cmd, codexID)
	}

	// Each account gets a distinct hint. Switching accounts cannot reuse or overwrite the other
	// account's explicit conversation id.
	a.cfg.SetActiveProfile("claude", "work")
	workCmd := a.forkLaunchCmd(forkArgs{name: "demo", agent: "claude"}, ws, true)
	workID := readForkSession(ws, "claude", "work")
	if workID == "" || workID == newID || !slices.Contains(workCmd, workID) {
		t.Errorf("work-account command/id = %v / %q, default id %q", workCmd, workID, newID)
	}

	// The fork is provider-writable. A planted hint must not become a host path probe or argv value.
	if err := os.WriteFile(forkSessionFile(ws, "claude", "work"), []byte("../../outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = a.forkLaunchCmd(forkArgs{name: "demo", agent: "claude"}, ws, true)
	safeID := readForkSession(ws, "claude", "work")
	if safeID == "" || safeID == "../../outside" || !slices.Contains(cmd, safeID) || slices.Contains(cmd, "../../outside") {
		t.Errorf("invalid persisted id was not replaced: cmd %v id %q", cmd, safeID)
	}

	// A legacy provider-only hint is adopted only when the selected account contains that exact
	// session; otherwise a new account-scoped id replaces the ambiguous hint.
	legacyID := "77777777-2222-4333-8444-555555555555"
	legacyWS := filepath.Join(t.TempDir(), "myrepo-forks", "legacy")
	if err := os.MkdirAll(legacyWS, 0o755); err != nil {
		t.Fatal(err)
	}
	saveForkMeta(legacyWS, legacyForkSessionFile(legacyWS, "claude"), legacyID)
	legacySession := filepath.Join(a.cfg.AgentDir("claude"), "projects", agents.ClaudeProjectKey(legacyWS), legacyID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(legacySession), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacySession, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = a.forkLaunchCmd(forkArgs{name: "legacy", agent: "claude"}, legacyWS, true)
	if got := readForkSession(legacyWS, "claude", "work"); got != legacyID || !slices.Contains(cmd, "--resume") || !slices.Contains(cmd, legacyID) {
		t.Errorf("legacy adoption = id %q cmd %v, want resume %q", got, cmd, legacyID)
	}

	missID := "88888888-2222-4333-8444-555555555555"
	missWS := filepath.Join(t.TempDir(), "myrepo-forks", "legacy-miss")
	if err := os.MkdirAll(missWS, 0o755); err != nil {
		t.Fatal(err)
	}
	saveForkMeta(missWS, legacyForkSessionFile(missWS, "claude"), missID)
	cmd = a.forkLaunchCmd(forkArgs{name: "legacy-miss", agent: "claude"}, missWS, true)
	got := readForkSession(missWS, "claude", "work")
	if got == "" || got == missID || !slices.Contains(cmd, "--session-id") || slices.Contains(cmd, "--resume") {
		t.Errorf("ambiguous legacy miss = id %q cmd %v, want a new scoped session", got, cmd)
	}
}

func TestUniquelyNewSessionID(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after []string
		want          string
	}{
		{"one new", []string{"old"}, []string{"old", "new"}, "new"},
		{"none", []string{"old"}, []string{"old"}, ""},
		{"two new fail closed", []string{"old"}, []string{"old", "a", "b"}, ""},
		{"duplicates collapse", nil, []string{"new", "new"}, "new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := uniquelyNewSessionID(tc.before, tc.after); got != tc.want {
				t.Errorf("uniquelyNewSessionID(%v, %v) = %q, want %q", tc.before, tc.after, got, tc.want)
			}
		})
	}
}

func TestDetectEditor(t *testing.T) {
	// With no GUI editor reachable on PATH, fall back to $VISUAL, then $EDITOR.
	t.Setenv("PATH", t.TempDir()) // empty dir → none of code/cursor/zed/idea/subl found
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "myeditor")
	if got := detectEditor(); got != "myeditor" {
		t.Errorf("detectEditor() = %q, want %q ($EDITOR fallback)", got, "myeditor")
	}
	t.Setenv("VISUAL", "myvisual")
	if got := detectEditor(); got != "myvisual" {
		t.Errorf("detectEditor() = %q, want %q ($VISUAL beats $EDITOR)", got, "myvisual")
	}
}

func TestResolveEditor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// Isolate from the host's global/system git config so core.editor is only what we set.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo := initRepo(t)
	// Your GLOBAL core.editor is honored; a repo-local one is IGNORED — the repo is agent-writable,
	// so reading core.editor from it would let a poisoned repo point the editor at a planted binary
	// that runs on `coop fork review --open`. (`git config --global` ignores -C, writing the env file.)
	git(t, repo, "config", "--global", "core.editor", "zed --wait")
	git(t, repo, "config", "core.editor", "/tmp/evil --pwn") // repo-local: must NEVER be used

	// $COOP_EDITOR wins over everything.
	if got := resolveEditor("nvim"); got != "nvim" {
		t.Errorf("resolveEditor(COOP_EDITOR) = %q, want %q", got, "nvim")
	}
	// With no $COOP_EDITOR, the GLOBAL core.editor is honored — never the repo-local one.
	if got := resolveEditor(""); got != "zed --wait" {
		t.Errorf("resolveEditor = %q, want %q (must ignore the repo-local editor)", got, "zed --wait")
	}
	// With neither set, fall through to detection ($VISUAL; PATH has no GUI editor).
	git(t, repo, "config", "--global", "--unset", "core.editor")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("EDITOR", "")
	t.Setenv("VISUAL", "myvisual")
	if got := resolveEditor(""); got != "myvisual" {
		t.Errorf("resolveEditor(fallback) = %q, want %q", got, "myvisual")
	}
}

func TestRunReviewCmd(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	// COOP_REVIEW_CMD runs via sh -c, but the fork name is environment data, never shell source.
	// Use command-substitution syntax here so a future interpolation would be immediately visible.
	name := `$(printf injected)`
	a := &app{cfg: &config.Config{ReviewCmd: `printf '%s|%s' "$COOP_FORK_NAME" "$COOP_FORK_PATH" > ` + out}}
	if code, err := a.runReviewCmd(dir, "/the/fork", name, "review/demo"); err != nil || code != 0 {
		t.Fatalf("runReviewCmd = (%d, %v), want (0, nil)", code, err)
	}
	if data, _ := os.ReadFile(out); string(data) != name+"|/the/fork" {
		t.Errorf("COOP_REVIEW_CMD env not passed literally: got %q, want %q", data, name+"|/the/fork")
	}
}

// `coop fork logs <unknown>` must error like `fork path`/`fork review`, not exit 0 silently.
func TestForkLogsUnknownErrors(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.forkLogs([]string{"nope"})
	if err == nil || code == 0 {
		t.Fatalf("forkLogs(unknown) = (%d, %v), want a no-such-fork error", code, err)
	}
	if !strings.Contains(err.Error(), "no such fork") {
		t.Errorf("error = %q, want it to mention 'no such fork'", err)
	}
}
