package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkctl"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// A mistyped fork subcommand must be a usage error with a suggestion — never silently turned into
// a new fork (clone + branch + agent). An explicit agent (a real create) bypasses the guard.
func TestForkTypoSuggestsSubcommand(t *testing.T) {
	repo := t.TempDir()
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	if code, err := a.cmdFork([]string{"reviw"}); code != 2 || err == nil || !strings.Contains(err.Error(), "review") {
		t.Errorf("coop fork reviw = (%d, %v), want (2, did-you-mean review)", code, err)
	}
	if pathExists(forkspace.Workspace(repo, "reviw")) {
		t.Error("a typo must not create a fork")
	}
}

// Regression: a fork name that escapes the forks home or carries shell syntax must be refused by
// EVERY name-taking verb before it reaches a workspace, subprocess, or runtime call. `coop fork rm
// ..` used to filepath.Join-clean to the parent of all projects and delete it. Each guard runs before
// box.ResolveRepo, so an unsafe name returns without touching the filesystem.
func TestForkVerbsRejectUnsafeName(t *testing.T) {
	repo := t.TempDir()
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	fc := a.forkctl()
	verbs := map[string]func([]string) (int, error){
		"rm": fc.ForkRm, "stop": fc.ForkStop, "open": fc.ForkOpenEditor,
		"logs": fc.ForkLogs, "review": fc.ForkReview, "path": fc.ForkPath, "merge": fc.ForkMerge,
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
		{[]string{"lss"}, false, "ls", true},               // distance 1 from `ls` — the most-typed verb
		{[]string{"stp"}, false, "stop", true},             // 3 runes, distance 1 from `stop`
		{[]string{"lss", "claude"}, false, "", false},      // explicit target → deliberate create of `lss`
		{[]string{"lss", "codex:gpt-5"}, false, "", false}, // full target → deliberate create
		{[]string{"lss", "frontier"}, false, "", false},    // preset → deliberate create
		{[]string{"lss"}, true, "", false},                 // already a fork → open it, don't second-guess
		{[]string{"ls"}, false, "", false},                 // 2 runes → below the suggestion floor
		{[]string{"api"}, false, "", false},                // a real new fork name, far from any verb
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
	if pathExists(forkspace.Workspace(cfg.RepoOverride, "scratchfork")) {
		t.Error("failed image lookup must not create a fork")
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

type forkCommandResult struct {
	code int
	err  error
}

func runForkCommandAcrossLockedMutation(t *testing.T, repo, name string, command func() (int, error), mutate func()) forkCommandResult {
	t.Helper()
	unlock, err := forkspace.LockState(repo, name)
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

// `coop fork <name> --fresh` refuses to recreate a fork with uncommitted work unless --force, and the
// refusal happens BEFORE any image work — so the dirty work survives. (--fresh --force reclones, which
// needs a runtime, so only the refusal path is exercised here.)
func TestForkFreshGuardsDirtyWork(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
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

func TestForkStartPreflightsUnsupportedStateBeforeWorkspaceMutation(t *testing.T) {
	repo := initRepo(t)
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte("2147483646\nlinux-proc-v1:1:2\n")
	path := forkspace.PidPath(repo, "blocked")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.forkCreate([]string{"blocked", "claude"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "pre-v8") {
		t.Fatalf("fork start over unsupported state = (%d, %v), want pre-v8 refusal", code, err)
	}
	if pathExists(forkspace.Workspace(repo, "blocked")) {
		t.Fatal("unsupported state allowed fork setup before refusal")
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != string(raw) {
		t.Fatalf("unsupported state changed = %q, %v; want exact %q", got, readErr, raw)
	}
}

func TestForkFreshConfirmsBeforeRuntimeWork(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "perf")
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
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WritePid(repo, "perf", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.forkCreate([]string{"perf", "claude", "--fresh", "--force"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("forced --fresh without confirmation = (%d, %v), want refusal naming --yes", code, err)
	}
	if !pathExists(ws) || forkspace.RunningPid(repo, "perf") != os.Getpid() {
		t.Fatal("unconfirmed forced --fresh stopped its worker or deleted the workspace")
	}
}

func TestForkFreshRefusesStateOnlyCleanup(t *testing.T) {
	repo := initRepo(t)
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WriteWorkerState(repo, "perf", forkspace.WorkerState{Pending: true}); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.forkCreate([]string{"perf", "claude", "--fresh"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "awaiting cleanup") || !strings.Contains(err.Error(), "coop fork stop perf") {
		t.Fatalf("fork --fresh with state-only cleanup = (%d, %v), want cleanup refusal", code, err)
	}
	if !pathExists(forkspace.PidPath(repo, "perf")) || pathExists(forkspace.Workspace(repo, "perf")) {
		t.Fatal("refused state-only --fresh changed lifecycle state")
	}
}

func TestForkFreshRechecksLifecycleUnderLock(t *testing.T) {
	dir := t.TempDir()
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "race")
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
		if err := forkspace.WriteWorkerState(repo, "race", forkspace.WorkerState{Pending: true}); err != nil {
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
	ws, err := forkspace.Setup(repo, "replacement")
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
	code, err = a.forkACP("myfork", []string{"claude", "--credential", "ghost"})
	wantUsage := fmt.Sprintf("usage: coop fork myfork acp <%s>[:model][/effort][@account]", strings.Join(agents.Names(), "|"))
	if code != 2 || err == nil || err.Error() != wantUsage {
		t.Errorf("fork acp --credential = (%d, %v), want (2, %q)", code, err, wantUsage)
	}
}

func TestForkACPMountsSessionCompanionsReadOnly(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	companion := filepath.Join(root, "topology")
	for _, path := range []string{
		repo,
		forkspace.Workspace(repo, "myfork"),
		companion,
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bindings, err := json.Marshal([]map[string]string{{
		"name": "topology", "repository": filepath.Join(root, "source-checkout"),
		"workspace":   companion,
		"base_commit": "0123456789abcdef0123456789abcdef01234567",
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_SESSION_COMPANIONS", string(bindings))
	recorder := filepath.Join(root, "runtime-args")
	a := &app{
		cfg: &config.Config{
			RepoOverride: repo, ConfigDir: filepath.Join(root, "config"),
			BoxHome: filepath.Join(root, "box"), HomeInBox: "/home/node",
			ImageOverride: "test-image", Egress: "none",
		},
		rt: recordingRuntime(t, recorder), rtSet: true,
	}
	if code, runErr := a.forkACP("myfork", []string{"codex"}); runErr != nil || code != 0 {
		t.Fatalf("forkACP = (%d, %v), want mounted companion", code, runErr)
	}
	args, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		companion + ":/coop/repositories/topology:ro",
		`"path":"/coop/repositories/topology"`,
		`"base_commit":"0123456789abcdef0123456789abcdef01234567"`,
	} {
		if !strings.Contains(string(args), want) {
			t.Errorf("fork ACP runtime args lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(string(args), filepath.Join(root, "source-checkout")) {
		t.Fatalf("fork ACP mounted the policy source checkout:\n%s", args)
	}
}

// A live Responder triage turn on 2026-08-18 committed a typo fix before the
// engineering-task confirmation because its supposedly read-only session was
// only constrained by prompt text. The trusted session bit must reach the
// runtime mount: an answer from any provider is input, not an authority check.
func TestForkACPPhysicallyMountsAReadOnlySessionRepositoryReadOnly(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	workspace := forkspace.Workspace(repo, "readonly")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_SESSION_REPOSITORY_READ_ONLY", "1")
	recorder := filepath.Join(root, "runtime-args")
	a := &app{
		cfg: &config.Config{
			RepoOverride: repo, ConfigDir: filepath.Join(root, "config"),
			BoxHome: filepath.Join(root, "box"), HomeInBox: "/home/node",
			ImageOverride: "test-image", Egress: "none",
		},
		rt: recordingRuntime(t, recorder), rtSet: true,
	}
	if code, runErr := a.forkACP("readonly", []string{"codex"}); runErr != nil || code != 0 {
		t.Fatalf("forkACP = (%d, %v), want a read-only mounted session", code, runErr)
	}
	args, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), workspace+":"+workspace+":ro") {
		t.Fatalf("read-only session repository was writable:\n%s", args)
	}
}

func TestForkACPRejectsMalformedSessionCompanionsBeforeRuntime(t *testing.T) {
	t.Setenv("COOP_SESSION_COMPANIONS", "{")
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := &app{
		cfg: &config.Config{RepoOverride: t.TempDir(), ImageOverride: "test-image"},
		rt:  recordingRuntime(t, recorder), rtSet: true,
	}
	code, err := a.forkACP("myfork", []string{"codex"})
	if code != -1 || err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("forkACP with malformed companions = (%d, %v), want fail closed", code, err)
	}
	if _, err := os.Stat(recorder); !os.IsNotExist(err) {
		t.Fatalf("malformed companion binding reached runtime: %v", err)
	}
}

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
	// Both config doors shut in the PROCESS environment, so they reach the fork commands under
	// test too — they shell out to git themselves, inheriting this environment
	// (.agent/kb/rules/hermetic-git-tests.md). Pinning only the fixture commands' own env would
	// leave the code under test reading the developer's config.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
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
	id := forkctl.ReadForkSession(ws, "claude", "default")
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
	if forkctl.ReadForkSession(ws, "claude", "default") != id {
		t.Error("session id changed on re-entry")
	}

	// Native histories use the cwd visible inside the box, which may differ from the host fork
	// path under COOP_WORKDIR. The .coop hint remains in the host workspace.
	overrideWS := filepath.Join(t.TempDir(), "myrepo-forks", "override")
	if err := os.MkdirAll(overrideWS, 0o755); err != nil {
		t.Fatal(err)
	}
	a.cfg.Workdir = "/workspace/fork"
	forkctl.SaveForkSession(overrideWS, "claude", "default", id)
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
	newID := forkctl.ReadForkSession(ws, "claude", "default")
	if newID == "" || newID == id || !slices.Contains(cmd, "--session-id") || !slices.Contains(cmd, newID) || slices.Contains(cmd, "--resume") {
		t.Errorf("--new command/id = %v / %q, want a new persisted --session-id distinct from %q", cmd, newID, id)
	}

	// Re-entry when the session id was persisted but never materialized (e.g. quit
	// before a turn): fall back to a fresh start under the SAME id, not a resume error.
	ws2 := filepath.Join(t.TempDir(), "myrepo-forks", "ghost")
	if err := os.MkdirAll(ws2, 0o755); err != nil {
		t.Fatal(err)
	}
	forkctl.SaveForkSession(ws2, "claude", "default", id)
	cmd = a.forkLaunchCmd(forkArgs{name: "ghost", agent: "claude"}, ws2, true)
	if !slices.Contains(cmd, "--session-id") || slices.Contains(cmd, "--resume") {
		t.Errorf("ghost re-entry cmd = %v, want a fresh --session-id (no live session)", cmd)
	}

	// codex can't preset an id: no session file, and a fresh start is plain Interactive.
	cmd = a.forkLaunchCmd(forkArgs{name: "demo", agent: "codex"}, ws, false)
	if forkctl.ReadForkSession(ws, "codex", "default") != "" {
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
	if got := forkctl.ReadForkSession(ws, "codex", "default"); got != codexID {
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
	workID := forkctl.ReadForkSession(ws, "claude", "work")
	if workID == "" || workID == newID || !slices.Contains(workCmd, workID) {
		t.Errorf("work-account command/id = %v / %q, default id %q", workCmd, workID, newID)
	}

	// The fork is provider-writable. A planted hint must not become a host path probe or argv value.
	if err := os.WriteFile(forkctl.ForkSessionFile(ws, "claude", "work"), []byte("../../outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = a.forkLaunchCmd(forkArgs{name: "demo", agent: "claude"}, ws, true)
	safeID := forkctl.ReadForkSession(ws, "claude", "work")
	if safeID == "" || safeID == "../../outside" || !slices.Contains(cmd, safeID) || slices.Contains(cmd, "../../outside") {
		t.Errorf("invalid persisted id was not replaced: cmd %v id %q", cmd, safeID)
	}

	// A provider-only pre-v9 hint is ignored even when the selected account contains that exact
	// session. Only the current account-scoped hint may select a conversation.
	legacyID := "77777777-2222-4333-8444-555555555555"
	legacyWS := filepath.Join(t.TempDir(), "myrepo-forks", "legacy")
	if err := os.MkdirAll(filepath.Join(legacyWS, ".coop"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyWS, ".coop", "session.claude"), []byte(legacyID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacySession := filepath.Join(a.cfg.AgentDir("claude"), "projects", agents.ClaudeProjectKey(legacyWS), legacyID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(legacySession), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacySession, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = a.forkLaunchCmd(forkArgs{name: "legacy", agent: "claude"}, legacyWS, true)
	got := forkctl.ReadForkSession(legacyWS, "claude", "work")
	if got == "" || got == legacyID || !slices.Contains(cmd, "--session-id") ||
		slices.Contains(cmd, "--resume") || slices.Contains(cmd, legacyID) {
		t.Errorf("provider-only hint = id %q cmd %v, want a new account-scoped session", got, cmd)
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

func TestParseForkCreateLoopFlags(t *testing.T) {
	workerArg := testDetachedReservationArg(t)
	tests := []struct {
		args                 []string
		loop, detach, worker bool
		agent, tasks         string
	}{
		{[]string{"perf", "codex", "--loop", "--tasks", "q.md"}, true, false, false, "codex", "q.md"},
		{[]string{"perf", "-d", "--tasks", "q.md"}, true, true, false, "", "q.md"},                      // no positional agent → "" (required later)
		{[]string{"perf", "--loop", "--detach", "--tasks", "q.md"}, true, true, false, "", "q.md"},      // long form of -d
		{[]string{"perf", "gemini", "--loop", "-d", "-t", "q.md"}, true, true, false, "gemini", "q.md"}, // short -t
		{[]string{"perf", "--loop", "--tasks=q.md"}, true, false, false, "", "q.md"},                    // --tasks=VALUE form
		{[]string{"perf", "codex", "--loop", workerArg, "--tasks", "q.md"}, true, false, true, "codex", "q.md"},
	}
	for _, tc := range tests {
		fa, err := parseForkCreate(tc.args)
		if err != nil {
			t.Errorf("parseForkCreate(%v) err = %v", tc.args, err)
			continue
		}
		if fa.loop != tc.loop || fa.detach != tc.detach || fa.worker != tc.worker || fa.agent != tc.agent || fa.tasks != tc.tasks {
			t.Errorf("parseForkCreate(%v) = {loop:%v detach:%v worker:%v agent:%q tasks:%q}, want {loop:%v detach:%v worker:%v agent:%q tasks:%q}",
				tc.args, fa.loop, fa.detach, fa.worker, fa.agent, fa.tasks, tc.loop, tc.detach, tc.worker, tc.agent, tc.tasks)
		}
	}

	// --loop / -d without --tasks is allowed now — runForkLoop defaults it to every
	// project.TaskDirs queue (it knows the repo). --tasks without --loop is still an error.
	if _, err := parseForkCreate([]string{"perf", "--loop"}); err != nil {
		t.Errorf("parseForkCreate(--loop, no --tasks): want no error (defaults later), got %v", err)
	}
	if _, err := parseForkCreate([]string{"perf", "-d"}); err != nil {
		t.Errorf("parseForkCreate(-d, no --tasks): want no error (defaults later), got %v", err)
	}
	if _, err := parseForkCreate([]string{"perf", "--tasks", "q.md"}); err == nil {
		t.Error("parseForkCreate(--tasks without --loop): want error")
	}
}

func testDetachedReservationArg(t *testing.T) string {
	t.Helper()
	data, err := (forkspace.WorkerState{Claim: true, Launched: true, Pid: 42, Token: "linux-proc-v1:boot:123"}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return "--_detached=" + base64.RawURLEncoding.EncodeToString(data)
}

func TestParseForkCreateDetachedReservation(t *testing.T) {
	valid := testDetachedReservationArg(t)
	noncanonicalState := base64.RawURLEncoding.EncodeToString([]byte(forkspace.OwnerStateV1 + forkspace.StartLaunched + "42\nlinux-proc-v1:boot:123\n\n"))
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing", args: []string{"perf", "codex", "--loop", "--_detached"}},
		{name: "missing target", args: []string{"perf", "--loop", valid}},
		{name: "duplicate", args: []string{"perf", "codex", "--loop", valid, valid}},
		{name: "malformed encoding", args: []string{"perf", "codex", "--loop", "--_detached=%%%"}},
		{name: "noncanonical state", args: []string{"perf", "codex", "--loop", "--_detached=" + noncanonicalState}},
		{name: "fresh", args: []string{"perf", "codex", "--loop", valid, "--fresh"}},
		{name: "force", args: []string{"perf", "codex", "--loop", valid, "--force"}},
		{name: "yes", args: []string{"perf", "codex", "--loop", valid, "--fresh", "--yes"}},
		{name: "detach", args: []string{"perf", "codex", "--loop", valid, "--detach"}},
		{name: "continue", args: []string{"perf", "codex", "--loop", valid, "--continue"}},
		{name: "new", args: []string{"perf", "codex", "--loop", valid, "--new"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseForkCreate(tc.args); err == nil {
				t.Fatalf("parseForkCreate(%v) succeeded, want internal worker grammar refusal", tc.args)
			}
		})
	}
}

func TestDetachedWorkerHandoffRaceOrderings(t *testing.T) {
	if !forkspace.StableProcToken(forkspace.ProcStartToken(os.Getpid())) {
		t.Skip("kernel process identity unavailable")
	}
	newFixture := func(t *testing.T) (repo, name string, reservation []byte, workerArg, runtimeCLI, runtimeTrace string) {
		t.Helper()
		root := t.TempDir()
		repo, name = filepath.Join(root, "repo"), "perf"
		ws := forkspace.Workspace(repo, name)
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(repo, ".agent", "tasks", "00_todo"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
			t.Fatal(err)
		}
		state := forkspace.WorkerState{Claim: true, Launched: true, Pid: os.Getpid(), Token: forkspace.ProcStartToken(os.Getpid())}
		var err error
		reservation, err = state.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if err := forkspace.WriteWorkerState(repo, name, state); err != nil {
			t.Fatal(err)
		}
		workerArg = "--_detached=" + base64.RawURLEncoding.EncodeToString(reservation)
		runtimeTrace = filepath.Join(root, "runtime.trace")
		if err := os.WriteFile(runtimeTrace, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		runtimeCLI = filepath.Join(root, "runtime")
		if err := os.WriteFile(runtimeCLI, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$COOP_TEST_RUNTIME_TRACE\"\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return repo, name, reservation, workerArg, runtimeCLI, runtimeTrace
	}

	t.Run("stop wins before publication", func(t *testing.T) {
		repo, name, _, workerArg, runtimeCLI, runtimeTrace := newFixture(t)
		ws := forkspace.Workspace(repo, name)
		marker := filepath.Join(ws, "marker")
		if err := os.WriteFile(marker, []byte("unchanged\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ready, release := filepath.Join(t.TempDir(), "ready"), filepath.Join(t.TempDir(), "release")
		cmd := exec.Command(os.Args[0], "fork", name, "claude", "--loop", workerArg)
		var childErr strings.Builder
		cmd.Stderr = &childErr
		xdgConfig := filepath.Join(t.TempDir(), "xdg-config")
		cmd.Env = append(os.Environ(),
			detachedHandoffModeEnv+"=cli",
			detachedHandoffReadyEnv+"="+ready,
			detachedHandoffReleaseEnv+"="+release,
			"COOP_REPO="+repo,
			"COOP_CONFIG_DIR="+filepath.Join(t.TempDir(), "config"),
			"XDG_CONFIG_HOME="+xdgConfig,
			"COOP_RUNTIME="+runtimeCLI,
			"COOP_TEST_RUNTIME_TRACE="+runtimeTrace,
		)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		})
		awaitTestPath(t, ready, true)
		t.Setenv("COOP_TEST_RUNTIME_TRACE", runtimeTrace)
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: runtime.Runtime{Name: runtimeCLI}, rtSet: true}
		if code, err := a.forkctl().ForkStop([]string{name}); code != 0 || err != nil {
			t.Fatalf("stop launched reservation = (%d, %v)", code, err)
		}
		traceBefore, err := os.ReadFile(runtimeTrace)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(release, []byte("go\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err == nil {
			t.Fatal("superseded detached child exited successfully")
		}
		if !strings.Contains(childErr.String(), "detached fork start was superseded") {
			t.Fatalf("child error = %q, want superseded refusal", childErr.String())
		}
		if pathExists(forkspace.PidPath(repo, name)) {
			t.Fatal("superseded child recreated lifecycle state")
		}
		if pathExists(filepath.Join(xdgConfig, "coop", "temp-reap")) {
			t.Fatal("superseded child ran global temp housekeeping before publication")
		}
		if got := forkctl.ReadForkAgent(ws); got != "" {
			t.Fatalf("superseded child saved agent metadata %q", got)
		}
		if pathExists(filepath.Join(ws, ".agent", "tasks")) {
			t.Fatal("superseded child seeded a queue")
		}
		if got, err := os.ReadFile(marker); err != nil || string(got) != "unchanged\n" {
			t.Fatalf("workspace marker changed = (%q, %v)", got, err)
		}
		if traceAfter, err := os.ReadFile(runtimeTrace); err != nil || !bytes.Equal(traceAfter, traceBefore) {
			t.Fatalf("superseded child reached runtime = (%q, %v), before %q", traceAfter, err, traceBefore)
		}
	})

	t.Run("child wins publication", func(t *testing.T) {
		repo, name, reservation, _, runtimeCLI, runtimeTrace := newFixture(t)
		ready := filepath.Join(t.TempDir(), "ready")
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(),
			detachedHandoffModeEnv+"=publisher",
			detachedHandoffReadyEnv+"="+ready,
			detachedHandoffRepoEnv+"="+repo,
			detachedHandoffNameEnv+"="+name,
			detachedHandoffReservationEnv+"="+base64.RawURLEncoding.EncodeToString(reservation),
		)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		waitDone := make(chan error, 1)
		go func() { waitDone <- cmd.Wait() }()
		reaped := false
		t.Cleanup(func() {
			if !reaped {
				_ = cmd.Process.Kill()
				select {
				case <-waitDone:
				case <-time.After(5 * time.Second):
				}
			}
		})
		awaitTestPath(t, ready, true)
		state, err := forkspace.ReadWorkerState(repo, name)
		if err != nil || state.Claim || state.Pending || state.Pid != cmd.Process.Pid || state.Token != forkspace.ProcStartToken(cmd.Process.Pid) {
			t.Fatalf("published child state = (%+v, %v), want exact pid %d", state, err, cmd.Process.Pid)
		}
		t.Setenv("COOP_TEST_RUNTIME_TRACE", runtimeTrace)
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: runtime.Runtime{Name: runtimeCLI}, rtSet: true}
		if code, err := a.forkctl().ForkStop([]string{name}); code != 0 || err != nil {
			t.Fatalf("stop published child = (%d, %v)", code, err)
		}
		select {
		case <-waitDone:
			reaped = true
		case <-time.After(5 * time.Second):
			t.Fatal("published child was not reaped")
		}
		if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
			t.Fatal("stop left the published child alive")
		}
		if pathExists(forkspace.PidPath(repo, name)) {
			t.Fatal("stop left published child state")
		}
	})
}

func awaitTestPath(t *testing.T, path string, exists bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := os.Stat(path)
		if (err == nil) == exists || (errors.Is(err, os.ErrNotExist) && !exists) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("path %s existence did not become %v: %v", path, exists, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDetachedWorkerEarlyValidationFailureCleansOnlyItsIdentity(t *testing.T) {
	if !forkspace.StableProcToken(forkspace.ProcStartToken(os.Getpid())) {
		t.Skip("kernel process identity unavailable")
	}
	tests := []struct {
		name        string
		replacement forkspace.WorkerState
		wantState   bool
	}{
		{name: "own identity removed"},
		{name: "pending replacement preserved", replacement: forkspace.WorkerState{Pending: true}, wantState: true},
		{name: "worker replacement preserved", replacement: forkspace.WorkerState{Pid: 2147483646, Token: "linux-proc-v1:1:2"}, wantState: true},
		{name: "same pid different token preserved", replacement: forkspace.WorkerState{Pid: os.Getpid(), Token: "linux-proc-v1:replacement:1"}, wantState: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo, name := t.TempDir(), "perf"
			ws := forkspace.Workspace(repo, name)
			if err := os.MkdirAll(ws, 0o755); err != nil {
				t.Fatal(err)
			}
			presetDir := filepath.Join(repo, ".agent", "presets", "vanish")
			if err := os.MkdirAll(presetDir, 0o755); err != nil {
				t.Fatal(err)
			}
			presetFile := filepath.Join(presetDir, "preset.yaml")
			if err := os.WriteFile(presetFile, []byte("lead: {agent: claude}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			probe := &app{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
			if _, err := probe.loadRunPreset("vanish"); err != nil {
				t.Fatalf("parent preset validation: %v", err)
			}
			if err := os.WriteFile(presetFile, []byte("broken: true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			reservation := forkspace.WorkerState{Claim: true, Launched: true, Pid: os.Getpid(), Token: forkspace.ProcStartToken(os.Getpid())}
			data, err := reservation.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := forkspace.WriteWorkerState(repo, name, reservation); err != nil {
				t.Fatal(err)
			}
			a := &app{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
			if tc.wantState {
				a.afterDetachedPublish = func() {
					if err := forkspace.WriteWorkerState(repo, name, tc.replacement); err != nil {
						t.Errorf("replace published worker: %v", err)
					}
				}
			}
			arg := "--_detached=" + base64.RawURLEncoding.EncodeToString(data)
			if code, err := a.forkCreate([]string{name, "vanish", "--loop", arg}); code != 2 || err == nil {
				t.Fatalf("child repeated validation = (%d, %v), want preset failure", code, err)
			}
			got, readErr := os.ReadFile(forkspace.PidPath(repo, name))
			if !tc.wantState {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("failed child identity was retained: %q, %v", got, readErr)
				}
				return
			}
			want, err := tc.replacement.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if readErr != nil || !bytes.Equal(got, want) {
				t.Fatalf("replacement changed by child cleanup = (%q, %v), want %q", got, readErr, want)
			}
		})
	}
}

func TestParseForkCreateCredential(t *testing.T) {
	// The target pins the account + model; it sits happily among the loop flags.
	fa, err := parseForkCreate([]string{"perf", "claude:opus-4.8@work", "--loop", "--tasks", "q.md"})
	if err != nil {
		t.Fatalf("parseForkCreate err = %v", err)
	}
	if fa.credential != "work" || fa.model != "opus-4.8" {
		t.Errorf("credential=%q model=%q, want work / opus-4.8", fa.credential, fa.model)
	}
	// --profile/--credential/--model are not fork flags — each errors as an unknown arg,
	// in both space and = forms.
	for _, args := range [][]string{
		{"perf", "--profile", "work"}, {"perf", "--profile=work"},
		{"perf", "--credential", "work"}, {"perf", "--credential=work"},
		{"perf", "--model", "opus"},
	} {
		if _, err := parseForkCreate(args); err == nil {
			t.Errorf("parseForkCreate(%v): an unknown flag must error, got nil", args)
		}
	}
}

// A default `coop fork <name> --loop` (no --tasks) in a repo with NO task queue fails fast
// BEFORE any clone — no stray fork workspace is left behind — instead of cloning and only
// erroring later in the worker's log.
func TestForkLoopDefaultNoQueueFailsFast(t *testing.T) {
	repo := t.TempDir()
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.forkCreate([]string{"x", "--loop"})
	if err == nil || !strings.Contains(err.Error(), "no task queue found") {
		t.Fatalf("forkCreate(x --loop, no queue) = (%d, %v), want a 'no task queue found' error", code, err)
	}
	if pathExists(forkspace.Workspace(repo, "x")) {
		t.Error("a fork workspace was created despite the fast-fail")
	}
}
