package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkctl"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/testutil/starttoken"
)

const testForkSessionStoreID = "store_test"

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
	for _, tc := range []struct{ word, suggestion string }{{"lss", "ls"}, {"reviw", "review"}, {"list", "ls"}} {
		t.Run(tc.word, func(t *testing.T) {
			cfg := &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir(), BoxHome: t.TempDir()}
			a := &app{cfg: cfg}
			code, err := a.cmdFork([]string{tc.word})
			if code != 2 || err == nil || !strings.Contains(err.Error(), "did you mean 'coop fork "+tc.suggestion+"'") {
				t.Fatalf("cmdFork(%q) = (%d, %v), want a suggestion for %s", tc.word, code, err, tc.suggestion)
			}
			entries, err := os.ReadDir(cfg.BoxHome)
			if err != nil || len(entries) != 0 || pathExists(forkspace.Workspace(cfg.RepoOverride, tc.word)) {
				t.Fatalf("refused typo created fork state: %v/%v", entries, err)
			}
		})
	}
}

func TestForkCreateRejectsUnknownAccountBeforeClone(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir(), BoxHome: t.TempDir()}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := &app{cfg: cfg, rt: recordingRuntime(t, recorder), rtSet: true}
	if code, err := a.forkCreate([]string{"scratchfork", "claude@ghost"}); code != 2 || err == nil || err.Error() != `No Claude account named "ghost" — Accounts: coop credentials claude — Sign in: coop login claude@ghost` {
		t.Fatalf("forkCreate with a missing account = (%d, %v)", code, err)
	}
	if pathExists(recorder) || pathExists(forkspace.Workspace(cfg.RepoOverride, "scratchfork")) {
		t.Fatal("missing account reached runtime or created a workspace")
	}
	if entries, err := os.ReadDir(cfg.BoxHome); err != nil || len(entries) != 0 {
		t.Fatalf("missing account created fork state: %v/%v", entries, err)
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
	if code == 2 && strings.Contains(err.Error(), "account named") {
		t.Fatalf("env-only default was rejected before image lookup: (%d, %v)", code, err)
	}
	if pathExists(forkspace.Workspace(cfg.RepoOverride, "scratchfork")) {
		t.Error("failed image lookup must not create a fork")
	}
}

func TestParseForkCreate(t *testing.T) {
	tests := []struct {
		args         []string
		wantAgent    string
		wantFresh    bool
		wantAgentSet bool
		wantErr      bool
	}{
		{[]string{"perf"}, "", false, false, false},
		{[]string{"perf", "codex"}, "codex", false, true, false},
		{[]string{"perf", "gemini", "--fresh"}, "gemini", true, true, false},
		{[]string{}, "", false, false, true},
		{[]string{"perf", "bogus"}, "", false, false, false}, // preset, not an explicit provider
		{[]string{"ls"}, "", false, false, true},
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
		if fa.agent != tc.wantAgent || fa.fresh != tc.wantFresh || fa.agentSet != tc.wantAgentSet {
			t.Errorf("parseForkCreate(%v) = {agent:%q fresh:%v agentSet:%v}, want {agent:%q fresh:%v agentSet:%v}",
				tc.args, fa.agent, fa.fresh, fa.agentSet, tc.wantAgent, tc.wantFresh, tc.wantAgentSet)
		}
	}
}

// TestParseForkCreatePeer: --peer <target> is repeatable and rides a loop fork's args; on a
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

func TestForkLifecycleMutationWaitsForCommandPreflight(t *testing.T) {
	repo := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		// An unrelated lock waiter must not satisfy the command's barrier.
		unlockOther, err := forkspace.LockState(repo, "other")
		if err != nil {
			t.Fatal(err)
		}
		otherDone := make(chan struct{})
		defer func() {
			unlockOther()
			<-otherDone
		}()
		go func() {
			defer close(otherDone)
			unlock, err := forkspace.LockState(repo, "other")
			if err != nil {
				t.Error(err)
				return
			}
			unlock()
		}()
		synctest.Wait()
		var preflightComplete atomic.Bool
		got := runForkCommandAcrossLockedMutation(t, repo, "delayed", func() (int, error) {
			// Virtual time deterministically exceeds the old helper's 80 ms guess.
			time.Sleep(150 * time.Millisecond)
			preflightComplete.Store(true)
			unlock, err := forkspace.LockState(repo, "delayed")
			if err != nil {
				return -1, err
			}
			unlock()
			return 17, nil
		}, func() {
			if !preflightComplete.Load() {
				t.Error("lifecycle mutation ran before the command completed preflight")
			}
		})
		if got.code != 17 || got.err != nil {
			t.Fatalf("command result = %+v, want the completed lock-protected command", got)
		}
	})
}

func forkLifecycleTestStacks() []byte {
	for size := 64 << 10; ; size *= 2 {
		buf := make([]byte, size)
		if n := goruntime.Stack(buf, true); n < len(buf) {
			return buf[:n]
		}
	}
}

// Entering LockStateContext proves that the command completed its unlocked
// preflight. Elapsed time cannot prove that, and another goroutine's lock wait
// is irrelevant, so require both frames in the same stack.
// Callers must remain nonparallel: the frame identifies the helper, not an invocation.
func waitForForkCommandLock(t *testing.T, result <-chan forkCommandResult) {
	t.Helper()
	deadline := time.NewTimer(wait.Deadline) // a fixture guard, not the behavior under test
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		stacks := forkLifecycleTestStacks()
		for _, stack := range bytes.Split(stacks, []byte("\n\n")) {
			if bytes.Contains(stack, []byte("internal/forkspace.LockStateContext(")) &&
				bytes.Contains(stack, []byte("internal/cli.runForkCommandAcrossLockedMutation.func")) {
				return
			}
		}
		select {
		case got := <-result:
			t.Fatalf("fork command bypassed lifecycle lock: (%d, %v)", got.code, got.err)
		case <-deadline.C:
			t.Fatalf("fork command never entered the held lifecycle lock\n%s", stacks)
		case <-ticker.C:
		}
	}
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
	waitForForkCommandLock(t, result)
	mutate()
	unlock()
	locked = false
	select {
	case got := <-result:
		return got
	case <-time.After(2 * time.Second):
		t.Fatalf("fork command remained blocked after lifecycle unlock\n%s", forkLifecycleTestStacks())
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

func TestForkStartPreflightsMalformedStateBeforeWorkspaceMutation(t *testing.T) {
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
	if code != 1 || err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("fork start over malformed state = (%d, %v), want malformed-state refusal", code, err)
	}
	if pathExists(forkspace.Workspace(repo, "blocked")) {
		t.Fatal("unsupported state allowed fork setup before refusal")
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != string(raw) {
		t.Fatalf("unsupported state changed = %q, %v; want exact %q", got, readErr, raw)
	}
}

func TestOrdinaryForkLaunchCannotEnterRemoteSessionWorkspace(t *testing.T) {
	repo := initRepo(t)
	if _, err := forkspace.Setup(repo, "remote"); err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, "remote")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureGenerationLocked(repo, "remote")
	if err == nil {
		err = forkspace.ReserveWorkspaceLocked(repo, forkspace.WorkspaceReservation{
			Version: forkspace.WorkspaceReservationVersion, Fork: identity,
			Kind: forkspace.WorkspaceReservationRemoteSession, OwnerStoreID: testForkSessionStoreID, OwnerID: "session-owner", CreatedAt: time.Now().UTC(),
		})
	}
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	runtimeCLI := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(runtimeCLI, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}, rt: runtime.Runtime{Name: runtimeCLI}, rtSet: true}
	code, err := a.forkCreate([]string{"remote", "claude"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "owned by remote-session session-owner") {
		t.Fatalf("ordinary launch into reserved fork = (%d, %v), want session-owner refusal", code, err)
	}
}

func TestForkCreateStopsBeforeBoxWhenProviderMetadataCannotBeSaved(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "metadata")
	if err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(ws, ".coop")
	if err := os.Symlink(t.TempDir(), meta); err != nil {
		t.Fatal(err)
	}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := &app{
		cfg: &config.Config{
			RepoOverride: repo, ConfigDir: t.TempDir(), BoxHome: t.TempDir(),
			ImageOverride: "test-image", Egress: "none",
		},
		rt: recordingRuntime(t, recorder), rtSet: true,
	}
	code, runErr := a.forkCreate([]string{"metadata", "claude"})
	if code != -1 || runErr == nil || !strings.Contains(runErr.Error(), "save fork provider before launch") ||
		!strings.Contains(runErr.Error(), meta) {
		t.Fatalf("forkCreate = (%d, %v), want provider metadata refusal", code, runErr)
	}
	if args, err := os.ReadFile(recorder); err != nil || strings.Contains(string(args), "\nrun ") || strings.HasPrefix(string(args), "run ") {
		t.Fatalf("metadata failure reached box launch: %q, %v", args, err)
	}
}

func TestInteractiveForkUsesSharedNetworkAdmission(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := initRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, project.File), []byte("box:\n  egress: filtered\n  egress_rules:\n    - to: {domain: project.example}\n      protocol: tls\n      ports: [443]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", project.File)
	git(t, repo, "commit", "-qm", "request filtered network")
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	a := &app{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), BaseImage: "test-image", Egress: "open"}, rt: recordingRuntime(t, recorder), rtSet: true}
	code, err := a.forkCreate([]string{"networked", "claude", "--allow-domain", "example.com"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "Review it: coop approve") {
		t.Fatalf("interactive fork without approval = (%d, %v)", code, err)
	}
	if data, readErr := os.ReadFile(recorder); readErr != nil || strings.Contains("\n"+string(data), "\nrun ") {
		t.Fatalf("pending fork reached box runtime: %q, %v", data, readErr)
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

// A --fresh fork loop checks each peer's login before it asks to recreate the fork: a peer that
// cannot sign in is refused first, so the fork is never stopped or recloned for a loop that could
// not start, and a signed-in peer goes on to the confirmation.
func TestForkFreshChecksPeersBeforeConfirmation(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".agent", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), BoxHome: t.TempDir()}
	signInCred(t, cfg, "claude", "default") // the lead can run; Codex has no login yet
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	args := []string{"perf", "claude", "--loop", "--peer", "codex", "--fresh"}
	a := &app{cfg: cfg, rt: recordingRuntime(t, recorder), rtSet: true}
	if code, err := a.forkCreate(args); code != 1 || err == nil || !strings.HasPrefix(err.Error(), "Codex needs a usable account") {
		t.Fatalf("fork --fresh with a signed-out peer = (%d, %v), want the peer's sign-in refusal first", code, err)
	}
	signInCred(t, cfg, "codex", "default")
	a = &app{cfg: cfg, rt: recordingRuntime(t, recorder), rtSet: true}
	if code, err := a.forkCreate(args); code != 2 || err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("fork --fresh with a signed-in peer = (%d, %v), want it past the peer check to the confirmation", code, err)
	}
	if !pathExists(ws) || pathExists(recorder) {
		t.Fatal("a refused --fresh deleted the fork or reached the runtime")
	}
}

func TestForkFreshConfirmsBeforeForcedStop(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WritePid(repo, "perf", os.Getpid(), ""); err != nil {
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
	// A crashed worker's record with no workspace: only `coop fork stop` may finish its cleanup.
	if err := forkspace.WriteWorkerState(repo, "perf", forkspace.WorkerState{Pid: 2147483646, Token: starttoken.Stale()}); err != nil {
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
		if err := forkspace.WriteWorkerState(repo, "race", forkspace.WorkerState{Pid: 2147483646, Token: starttoken.Stale()}); err != nil {
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

// Fork ACP accepts at most one positional target after peer extraction. Invalid account selection
// reaches credential validation, while a retired flag, junk, or second target fails before run
// config changes.
func TestForkACPValidatesTargetArgumentsBeforeRun(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}}
	if code, err := a.forkACP("myfork", nil); code != 1 || err == nil || !strings.Contains(err.Error(), "no such fork: myfork") {
		t.Errorf("fork acp for a missing fork = (%d, %v), want (1, no such fork)", code, err)
	}
	code, err := a.forkACP("myfork", []string{"claude@ghost"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("fork acp claude@ghost = (%d, %v), want (2, an account error naming ghost)", code, err)
	}
	if strings.Contains(err.Error(), "unexpected") || strings.Contains(err.Error(), "usage:") {
		t.Errorf("a target's @account should be accepted, not rejected as an argument: %v", err)
	}
	code, err = a.forkACP("myfork", []string{"claude@ghost", "--peer", "codex", "--peer", "gemini"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "ghost") || strings.Contains(err.Error(), "usage:") {
		t.Errorf("fork ACP with repeatable peers = (%d, %v), want target account validation after peer extraction", code, err)
	}
	code, err = a.forkACP("myfork", []string{"claude", "--credential", "ghost"})
	wantUsage := "usage: coop fork myfork acp [<target>] [--peer <target>...] [--egress <mode>]"
	if code != 2 || err == nil || err.Error() != wantUsage {
		t.Errorf("fork acp --credential = (%d, %v), want (2, %q)", code, err, wantUsage)
	}
	for _, args := range [][]string{
		{"claude:opus", "codex:high"},
		{"claude:opus", "junk"},
	} {
		code, err = a.forkACP("myfork", args)
		if code != 2 || err == nil || err.Error() != wantUsage {
			t.Errorf("fork acp %v = (%d, %v), want (2, %q)", args, code, err, wantUsage)
		}
	}
	if got := a.cfg.ActiveModel("claude"); got != "" {
		t.Errorf("rejected fork ACP arguments selected claude model %q", got)
	}
	if got := a.cfg.ActiveModel("codex"); got != "" {
		t.Errorf("rejected fork ACP arguments selected codex model %q", got)
	}
}

// Without a target, fork ACP starts the agent the fork was created with, so an editor connects and
// its own selectors choose the model. A fork with no saved agent starts the first signed-in
// provider, as plain `coop acp` does; with none signed in, the editor's log gets the sign-in steps.
func TestForkACPWithoutTargetStartsTheForksAgent(t *testing.T) {
	t.Setenv("COOP_SESSION_RUN_ID", "")
	t.Setenv("COOP_ACP_INNER", "1") // inspect the box itself; the supervisor hands its child this target
	for _, tc := range []struct {
		name, saved string
		signedIn    []string
		want        string // the provider whose adapter runs; "" expects the sign-in failure
	}{
		{name: "saved agent", saved: "codex", signedIn: []string{"claude"}, want: "codex"},
		{name: "no saved agent", signedIn: []string{"gemini", "codex"}, want: "codex"},
		{name: "nothing signed in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			repo := filepath.Join(root, "repo")
			ws := forkspace.Workspace(repo, "myfork")
			for _, dir := range []string{repo, ws} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.saved != "" {
				if err := forkctl.SaveForkAgent(ws, tc.saved); err != nil {
					t.Fatal(err)
				}
			}
			cfg := &config.Config{
				RepoOverride: repo, ConfigDir: filepath.Join(root, "config"),
				BoxHome: filepath.Join(root, "box"), HomeInBox: "/home/node",
				ImageOverride: "test-image", Egress: "none",
			}
			for _, provider := range tc.signedIn {
				signInCred(t, cfg, provider, "default")
			}
			recorder := filepath.Join(root, "runtime-args")
			a := &app{cfg: cfg, rt: recordingRuntime(t, recorder), rtSet: true}
			code, err := a.forkACP("myfork", nil)
			if tc.want == "" {
				if code != 1 || err == nil || !strings.Contains(err.Error(), "coop login") {
					t.Fatalf("fork acp with nothing signed in = (%d, %v), want sign-in guidance", code, err)
				}
				if _, statErr := os.Stat(recorder); !os.IsNotExist(statErr) {
					t.Fatalf("fork acp with nothing signed in reached the runtime: %v", statErr)
				}
				return
			}
			if code != 0 || err != nil {
				t.Fatalf("fork acp without a target = (%d, %v)", code, err)
			}
			args, err := os.ReadFile(recorder)
			if err != nil {
				t.Fatal(err)
			}
			if adapter := strings.Join(acpCommand(cfg, tc.want), " "); !strings.Contains(string(args), "test-image "+adapter) {
				t.Fatalf("fork acp did not start %s's adapter %q:\n%s", tc.want, adapter, args)
			}
		})
	}
}

func TestForkACPMountsSessionCompanionsReadOnly(t *testing.T) {
	// Inspect the inner box contract; the local editor supervisor is exercised separately.
	t.Setenv("COOP_ACP_INNER", "1")
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

// A fork's ACP launch — the session daemon's turn child — is the agent's own ACP client, so an API
// key meets the broker's rules, not the refusal for commands that are not the agent. Offline, that
// rule is the refusal to put the key in a box the filtered gateway does not guard.
func TestForkACPTreatsAKeyAsTheAgentsOwnClient(t *testing.T) {
	t.Setenv("COOP_ACP_INNER", "1")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkspace.Workspace(repo, "keyed"), 0o755); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "env"), []byte("OPENAI_API_KEY=fixture-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{
		cfg: &config.Config{
			RepoOverride: repo, ConfigDir: configDir, Homes: true,
			BoxHome: filepath.Join(root, "box"), HomeInBox: "/home/node",
			ImageOverride: "test-image", Egress: "none",
		},
		rt: recordingRuntime(t, filepath.Join(root, "runtime-args")), rtSet: true,
	}
	code, err := a.forkACP("keyed", []string{"codex"})
	if code != 0 || err != nil {
		t.Fatalf("offline fork ACP = (%d, %v)", code, err)
	}
	args, err := os.ReadFile(filepath.Join(root, "runtime-args"))
	if err != nil || !strings.Contains(string(args), "--network none") || strings.Contains(string(args), "fixture-key") || strings.Contains(string(args), "native-broker") {
		t.Fatal("offline fork exposed a grant or launched a broker", err)
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
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := forkspace.Workspace(repo, "readonly")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	outputRoot := filepath.Join(workspace, ".coop-output")
	if err := os.Mkdir(outputRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	runID := "session-" + strings.Repeat("ab", 12)
	stateRoot := filepath.Join(root, "session-state")
	hostOutputRoot := filepath.Join(stateRoot, "output", "remote_1")
	if err := os.MkdirAll(hostOutputRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	privateRoot := filepath.Join(stateRoot, "acp", "remote_1")
	if err := os.MkdirAll(privateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateRoot, ".stable-workdir"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_SESSION_REPOSITORY_READ_ONLY", "1")
	t.Setenv("COOP_SESSION_RUN_ID", runID)
	t.Setenv("COOP_SESSION_STORE_ID", testForkSessionStoreID)
	t.Setenv(sessionsvc.SessionOutputRootEnv, hostOutputRoot)
	unlock, err := forkspace.LockState(repo, "readonly")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureGenerationLocked(repo, "readonly")
	if err == nil {
		err = forkspace.ReserveWorkspaceLocked(repo, forkspace.WorkspaceReservation{
			Version: forkspace.WorkspaceReservationVersion, Fork: identity,
			Kind: forkspace.WorkspaceReservationRemoteSession, OwnerStoreID: testForkSessionStoreID, OwnerID: "remote_1", CreatedAt: time.Now().UTC(),
		})
	}
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	recorder := filepath.Join(root, "runtime-args")
	a := &app{
		cfg: &config.Config{
			RepoOverride: repo, ConfigDir: filepath.Join(stateRoot, "acp", "remote_1"),
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
	if !strings.Contains(string(args), workspace+":"+sessionsvc.StableWorkdir+":ro") ||
		!strings.Contains(string(args), "-w "+sessionsvc.StableWorkdir) {
		t.Fatalf("read-only session repository was writable:\n%s", args)
	}
	outputMount := hostOutputRoot + ":" + filepath.Join(sessionsvc.StableWorkdir, ".coop-output")
	if !strings.Contains(string(args), "-v "+outputMount+" ") || strings.Contains(string(args), outputMount+":") {
		t.Fatalf("read-only session output root was not writable:\n%s", args)
	}
}

func TestRemoteForksKeepPrivateHostSourcesWithOneBoxWorkdir(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"first", "second", "legacy"} {
		workspace := forkspace.Workspace(repo, name)
		if err := os.MkdirAll(workspace, 0o755); err != nil {
			t.Fatal(err)
		}
		unlock, err := forkspace.LockState(repo, name)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := forkspace.EnsureGenerationLocked(repo, name)
		if err == nil {
			err = forkspace.ReserveWorkspaceLocked(repo, forkspace.WorkspaceReservation{
				Version: forkspace.WorkspaceReservationVersion, Fork: identity,
				Kind: forkspace.WorkspaceReservationRemoteSession, OwnerStoreID: testForkSessionStoreID, OwnerID: name, CreatedAt: time.Now().UTC(),
			})
		}
		unlock()
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("COOP_SESSION_RUN_ID", "session-"+strings.Repeat(string(rune('a'+i)), 24))
		t.Setenv("COOP_SESSION_STORE_ID", testForkSessionStoreID)
		recorder := filepath.Join(root, name+"-runtime-args")
		configDir := filepath.Join(t.TempDir(), "acp", name)
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if name != "legacy" {
			if err := os.WriteFile(filepath.Join(configDir, ".stable-workdir"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		a := &app{
			cfg: &config.Config{
				RepoOverride: repo, ConfigDir: configDir,
				BoxHome: filepath.Join(root, "box"), HomeInBox: "/home/node",
				ImageOverride: "test-image", Egress: "none",
			},
			rt: recordingRuntime(t, recorder), rtSet: true,
		}
		if code, runErr := a.forkACP(name, []string{"codex"}); runErr != nil || code != 0 {
			t.Fatalf("remote fork %s = (%d, %v)", name, code, runErr)
		}
		args, err := os.ReadFile(recorder)
		if err != nil {
			t.Fatal(err)
		}
		wantWorkdir := sessionsvc.StableWorkdir
		if name == "legacy" {
			wantWorkdir = workspace
		}
		for _, want := range []string{workspace + ":" + wantWorkdir, "-w " + wantWorkdir} {
			if !strings.Contains(string(args), want) {
				t.Errorf("remote fork %s lacks %q:\n%s", name, want, args)
			}
		}
		if name != "legacy" && strings.Contains(string(args), "-w "+workspace) {
			t.Errorf("remote fork %s exposed unique host cwd:\n%s", name, args)
		}
	}
	// A local editor session keeps the host path, and therefore its existing session history.
	t.Setenv("COOP_SESSION_RUN_ID", "")
	t.Setenv("COOP_ACP_INNER", "1")
	local := forkspace.Workspace(repo, "local")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	recorder := filepath.Join(root, "local-runtime-args")
	a := &app{
		cfg: &config.Config{
			RepoOverride: repo, ConfigDir: filepath.Join(root, "local-config"),
			BoxHome: filepath.Join(root, "box"), HomeInBox: "/home/node",
			ImageOverride: "test-image", Egress: "none",
		},
		rt: recordingRuntime(t, recorder), rtSet: true,
	}
	if code, runErr := a.forkACP("local", []string{"codex"}); runErr != nil || code != 0 {
		t.Fatalf("local fork = (%d, %v)", code, runErr)
	}
	args, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), local+":"+local) || !strings.Contains(string(args), "-w "+local) {
		t.Fatalf("local fork lost host cwd:\n%s", args)
	}
}

func TestReadOnlySessionOutputMountUsesCanonicalWritableForm(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".coop-output"), 0o750); err != nil {
		t.Fatal(err)
	}
	source, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(sessionsvc.SessionOutputRootEnv, source)
	args, root, err := readOnlySessionOutputMountArgs(workspace, sessionsvc.StableWorkdir)
	want := source + ":" + filepath.Join(sessionsvc.StableWorkdir, ".coop-output")
	if err != nil || root != source || len(args) != 2 || args[0] != "-v" || args[1] != want {
		t.Fatalf("output mount = %q, %q, %v; want canonical writable -v %s", args, root, err, want)
	}
}

func TestForkACPRejectsUnsafeReadOnlySessionOutputRootBeforeBoxLaunch(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	workspace := forkspace.Workspace(repo, "readonly")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(workspace, ".coop-output")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_SESSION_REPOSITORY_READ_ONLY", "1")
	t.Setenv(sessionsvc.SessionOutputRootEnv, t.TempDir())
	recorder := filepath.Join(root, "runtime-args")
	a := &app{
		cfg: &config.Config{
			RepoOverride: repo, ConfigDir: filepath.Join(root, "config"),
			BoxHome: filepath.Join(root, "box"), HomeInBox: "/home/node",
			ImageOverride: "test-image", Egress: "none",
		},
		rt: recordingRuntime(t, recorder), rtSet: true,
	}
	code, runErr := a.forkACP("readonly", []string{"codex"})
	if code != -1 || runErr == nil || !strings.Contains(runErr.Error(), "output root is unsafe") {
		t.Fatalf("forkACP = (%d, %v), want unsafe output-root refusal", code, runErr)
	}
	if _, err := os.Stat(recorder); !os.IsNotExist(err) {
		t.Fatalf("unsafe output root reached runtime inspection or launch: %v", err)
	}
}

func TestForkACPRejectsReadOnlySessionOutputSourceInsideWorkspace(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	workspace := forkspace.Workspace(repo, "readonly")
	outputRoot := filepath.Join(workspace, ".coop-output")
	if err := os.MkdirAll(outputRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_SESSION_REPOSITORY_READ_ONLY", "1")
	t.Setenv(sessionsvc.SessionOutputRootEnv, outputRoot)
	recorder := filepath.Join(root, "runtime-args")
	a := &app{
		cfg: &config.Config{
			RepoOverride: repo, ConfigDir: filepath.Join(root, "config"),
			BoxHome: filepath.Join(root, "box"), HomeInBox: "/home/node",
			ImageOverride: "test-image", Egress: "none",
		},
		rt: recordingRuntime(t, recorder), rtSet: true,
	}
	code, runErr := a.forkACP("readonly", []string{"codex"})
	if code != -1 || runErr == nil || !strings.Contains(runErr.Error(), "output root is unsafe") {
		t.Fatalf("forkACP = (%d, %v), want overlapping output-root refusal", code, runErr)
	}
	if _, err := os.Stat(recorder); !os.IsNotExist(err) {
		t.Fatalf("overlapping output root reached runtime inspection or launch: %v", err)
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

func TestLocalForkACPUsesSharedNetworkAdmission(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("COOP_SESSION_RUN_ID", "")
	t.Setenv(box.SessionNetworkCaptureEnv, "")
	repo := initRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, project.File), []byte("box:\n  egress: filtered\n  egress_rules:\n    - to: {domain: project.example}\n      protocol: tls\n      ports: [443]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", project.File)
	git(t, repo, "commit", "-qm", "request filtered network")
	if _, err := forkspace.Setup(repo, "networked"); err != nil {
		t.Fatal(err)
	}
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	a := &app{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), BaseImage: "test-image", Egress: "open"}, rt: recordingRuntime(t, recorder), rtSet: true}
	code, err := a.forkACP("networked", []string{"claude", "--allow-domain", "example.com"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "Review it: coop approve") {
		t.Fatalf("local fork ACP without approval = (%d, %v)", code, err)
	}
	if _, err := os.Stat(recorder); !os.IsNotExist(err) {
		t.Fatalf("pending fork ACP reached runtime inspection or launch: %v", err)
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
	if code, err := forkHelp(""); code != 0 || err != nil {
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
	home := func(provider, ws string) string {
		t.Helper()
		value, err := a.sessionHome(provider, ws)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	launch := func(fa forkArgs, workspace string, existed bool) []string {
		t.Helper()
		cmd, err := a.forkLaunchCmd(fa, workspace, existed, false)
		if err != nil {
			t.Fatalf("forkLaunchCmd: %v", err)
		}
		return cmd
	}

	// First launch of a preset agent (claude): start under a fresh coop-owned id and
	// persist it; the command carries --session-id <uuid>.
	cmd := launch(forkArgs{name: "demo", agent: "claude"}, ws, false)
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
	sess := filepath.Join(home("claude", ws), "projects", claudeKey, id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(sess), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sess, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = launch(forkArgs{name: "demo", agent: "claude"}, ws, true)
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
	if err := forkctl.SaveForkSession(overrideWS, "claude", "default", id); err != nil {
		t.Fatal(err)
	}
	overrideSession := filepath.Join(home("claude", overrideWS), "projects", agents.ClaudeProjectKey(a.cfg.Workdir), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(overrideSession), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overrideSession, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = launch(forkArgs{name: "override", agent: "claude"}, overrideWS, true)
	if !slices.Contains(cmd, "--resume") || !slices.Contains(cmd, id) {
		t.Errorf("COOP_WORKDIR re-entry cmd = %v, want --resume %s", cmd, id)
	}
	a.cfg.Workdir = ""

	// --new rotates the persisted id instead of launching a supposedly fresh session under the
	// existing conversation's id.
	cmd = launch(forkArgs{name: "demo", agent: "claude", newSession: true}, ws, true)
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
	if err := forkctl.SaveForkSession(ws2, "claude", "default", id); err != nil {
		t.Fatal(err)
	}
	cmd = launch(forkArgs{name: "ghost", agent: "claude"}, ws2, true)
	if !slices.Contains(cmd, "--session-id") || slices.Contains(cmd, "--resume") {
		t.Errorf("ghost re-entry cmd = %v, want a fresh --session-id (no live session)", cmd)
	}

	// codex can't preset an id: no session file, and a fresh start is plain Interactive.
	cmd = launch(forkArgs{name: "demo", agent: "codex"}, ws, false)
	if forkctl.ReadForkSession(ws, "codex", "default") != "" {
		t.Error("codex must not get a coop-owned session id")
	}
	if slices.Contains(cmd, "--session-id") {
		t.Errorf("codex launch must not preset a session id: %v", cmd)
	}

	// Codex mints its own UUID. After the run, Coop discovers and persists it so a newer
	// same-cwd session cannot hijack this fork under a shared container workdir.
	codexID := "019f6a60-a28e-7d22-919c-81f43bef064f"
	codexDir := filepath.Join(home("codex", ws), "sessions", "2026", "07", "16")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	codexFile := filepath.Join(codexDir, "first.jsonl")
	if err := os.WriteFile(codexFile, []byte(`{"type":"session_meta","payload":{"id":"`+codexID+`","cwd":"`+ws+`","source":"cli"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	codexAdapter, _ := agents.Get("codex")
	discoverer := codexAdapter.(agents.SessionDiscoverer)
	if err := a.rememberNewDiscoveredForkSession(ws, "codex", discoverer, nil); err != nil {
		t.Fatal(err)
	}
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
	cmd = launch(forkArgs{name: "demo", agent: "codex"}, ws, true)
	if !slices.Contains(cmd, "resume") || !slices.Contains(cmd, codexID) || slices.Contains(cmd, newerCodexID) {
		t.Errorf("Codex exact re-entry cmd = %v, want persisted %s", cmd, codexID)
	}

	// Each account gets a distinct hint. Switching accounts cannot reuse or overwrite the other
	// account's explicit conversation id.
	a.cfg.SetActiveProfile("claude", "work")
	workCmd := launch(forkArgs{name: "demo", agent: "claude"}, ws, true)
	workID := forkctl.ReadForkSession(ws, "claude", "work")
	if workID == "" || workID == newID || !slices.Contains(workCmd, workID) {
		t.Errorf("work-account command/id = %v / %q, default id %q", workCmd, workID, newID)
	}

	// The fork is provider-writable. A planted hint must not become a host path probe or argv value.
	if err := os.WriteFile(forkctl.ForkSessionFile(ws, "claude", "work"), []byte("../../outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = launch(forkArgs{name: "demo", agent: "claude"}, ws, true)
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
	legacySession := filepath.Join(home("claude", legacyWS), "projects", agents.ClaudeProjectKey(legacyWS), legacyID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(legacySession), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacySession, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = launch(forkArgs{name: "legacy", agent: "claude"}, legacyWS, true)
	got := forkctl.ReadForkSession(legacyWS, "claude", "work")
	if got == "" || got == legacyID || !slices.Contains(cmd, "--session-id") ||
		slices.Contains(cmd, "--resume") || slices.Contains(cmd, legacyID) {
		t.Errorf("provider-only hint = id %q cmd %v, want a new account-scoped session", got, cmd)
	}
}

type fixedSessionDiscoverer []string

func (d fixedSessionDiscoverer) SessionIDs(string, string) []string { return d }
func (fixedSessionDiscoverer) ProducesSession([]string) bool        { return true }

func TestForkLaunchRequiresWritableSessionMetadata(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "repo-forks", "demo")
	path := forkctl.ForkSessionFile(ws, "claude", "default")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	cmd, err := a.forkLaunchCmd(forkArgs{name: "demo", agent: "claude"}, ws, false, false)
	if err == nil || len(cmd) != 0 || !strings.Contains(err.Error(), path) {
		t.Fatalf("forkLaunchCmd = %v, %v; want path-specific pre-launch save error", cmd, err)
	}
}

func TestDiscoveredForkSessionSaveFailureIsReturned(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "repo-forks", "demo")
	path := forkctl.ForkSessionFile(ws, "codex", "default")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	id := "11111111-2222-4333-8444-555555555555"
	err := a.rememberNewDiscoveredForkSession(ws, "codex", fixedSessionDiscoverer{id}, nil)
	if err == nil || !strings.Contains(err.Error(), "codex run finished") || !strings.Contains(err.Error(), "work remains in fork demo") ||
		!strings.Contains(err.Error(), "could not save the exact session") || !strings.Contains(err.Error(), path) {
		t.Fatalf("rememberNewDiscoveredForkSession error = %v", err)
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
		{[]string{"perf", "codex", "--loop", "--_detached", "--tasks", "q.md"}, true, false, true, "codex", "q.md"},
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

func TestForkLoopContinueCommandUsesSelectedTargetOrPreset(t *testing.T) {
	target := agents.Target{Provider: "codex", Model: "gpt-5.6-terra", Effort: "xhigh", Accounts: []string{"work"}}
	for _, tc := range []struct {
		name, preset, want string
	}{
		{name: "api-fix", want: "coop fork api-fix codex:gpt-5.6-terra/xhigh@work --loop"},
		{name: "api-fix", preset: "frontier", want: "coop fork api-fix frontier --loop"},
	} {
		if got := forkLoopContinueCommand(tc.name, tc.preset, target); got != tc.want {
			t.Errorf("forkLoopContinueCommand(%q, %q) = %q, want %q", tc.name, tc.preset, got, tc.want)
		}
	}
}

func TestParseForkCreateDetachedWorker(t *testing.T) {
	if fa, err := parseForkCreate([]string{"perf", "codex", "--loop", "--_detached"}); err != nil || !fa.worker || !fa.loop {
		t.Fatalf("parseForkCreate(--_detached) = (%+v, %v), want the internal worker", fa, err)
	}
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing target", args: []string{"perf", "--loop", "--_detached"}},
		{name: "fresh", args: []string{"perf", "codex", "--loop", "--_detached", "--fresh"}},
		{name: "force", args: []string{"perf", "codex", "--loop", "--_detached", "--force"}},
		{name: "yes", args: []string{"perf", "codex", "--loop", "--_detached", "--fresh", "--yes"}},
		{name: "detach", args: []string{"perf", "codex", "--loop", "--_detached", "--detach"}},
		{name: "continue", args: []string{"perf", "codex", "--loop", "--_detached", "--continue"}},
		{name: "new", args: []string{"perf", "codex", "--loop", "--_detached", "--new"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseForkCreate(tc.args); err == nil {
				t.Fatalf("parseForkCreate(%v) succeeded, want internal worker grammar refusal", tc.args)
			}
		})
	}
}

// The detached worker's first act is to confirm that the record its parent writes under the
// lifecycle lock names it. A child whose parent died before writing it, or whose record stop already
// removed, finds none and exits before it touches metadata, the queue, the workspace, the runtime
// or the global housekeeping its parent already ran.
func TestDetachedWorkerWithoutItsRecordExitsUntouched(t *testing.T) {
	root := t.TempDir()
	repo, name := filepath.Join(root, "repo"), "perf"
	ws := forkspace.Workspace(repo, name)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".agent", "tasks", "00_todo"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(ws, "marker")
	if err := os.WriteFile(marker, []byte("unchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeTrace, runtimeCLI := filepath.Join(root, "runtime.trace"), filepath.Join(root, "runtime")
	if err := os.WriteFile(runtimeCLI, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$COOP_TEST_RUNTIME_TRACE\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	xdgConfig := filepath.Join(t.TempDir(), "xdg-config")
	// An old, unmounted coop entry the daily temp sweep would remove: a child that refuses to
	// start must not run that housekeeping either.
	childTmp := t.TempDir()
	orphan := filepath.Join(childTmp, "coop-mcp-orphan")
	if err := os.WriteFile(orphan, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline) // a fixture guard, not the behavior under test
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "fork", name, "claude", "--loop", "--_detached")
	var childErr strings.Builder
	cmd.Stderr = &childErr
	cmd.Env = append(os.Environ(),
		detachedChildEnv+"=1",
		"COOP_REPO="+repo,
		"COOP_CONFIG_DIR="+filepath.Join(t.TempDir(), "config"),
		"XDG_CONFIG_HOME="+xdgConfig,
		"TMPDIR="+childTmp,
		"COOP_RUNTIME="+runtimeCLI,
		"COOP_TEST_RUNTIME_TRACE="+runtimeTrace,
	)
	if err := cmd.Run(); err == nil {
		t.Fatal("a detached child without its record exited successfully")
	}
	if got := childErr.String(); !strings.Contains(got, "will not start") || !strings.Contains(got, "no lifecycle record names this process") {
		t.Fatalf("child error = %q, want a refusal naming the missing record", got)
	}
	if pathExists(forkspace.PidPath(repo, name)) {
		t.Fatal("the child created a lifecycle record")
	}
	if !pathExists(orphan) {
		t.Fatal("the child ran global temp housekeeping")
	}
	if got := forkctl.ReadForkAgent(ws); got != "" {
		t.Fatalf("the child saved agent metadata %q", got)
	}
	if pathExists(filepath.Join(ws, ".agent", "tasks")) {
		t.Fatal("the child seeded a queue")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "unchanged\n" {
		t.Fatalf("workspace marker changed = (%q, %v)", got, err)
	}
	if trace, err := os.ReadFile(runtimeTrace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the child reached the container runtime: (%q, %v)", trace, err)
	}
}

// A worker that confirmed its record and then fails a later check removes that record on the way
// out, so a refused start leaves nothing for `coop fork stop` to clean up.
func TestDetachedWorkerEarlyValidationFailureClearsItsRecord(t *testing.T) {
	if !forkspace.StableProcToken(forkspace.ProcStartToken(os.Getpid())) {
		t.Skip("kernel process identity unavailable")
	}
	repo, name := t.TempDir(), "perf"
	if err := os.MkdirAll(forkspace.Workspace(repo, name), 0o755); err != nil {
		t.Fatal(err)
	}
	presetDir := filepath.Join(repo, ".agent", "presets", "vanish")
	if err := os.MkdirAll(presetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The preset broke after the parent validated it, so the worker's own reload fails.
	if err := os.WriteFile(filepath.Join(presetDir, "preset.yaml"), []byte("broken: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// This test process stands in for the child its parent recorded.
	if err := forkspace.WritePid(repo, name, os.Getpid(), ""); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
	if code, err := a.forkCreate([]string{name, "vanish", "--loop", "--_detached"}); code != 1 || err == nil {
		t.Fatalf("worker with a broken preset = (%d, %v), want the preset failure", code, err)
	}
	if pathExists(forkspace.PidPath(repo, name)) {
		t.Fatal("the failed worker left its own record behind")
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
