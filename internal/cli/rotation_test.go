package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/acpctl"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
)

// signInCred writes a minimal adapter-valid credential so rotation sees a runnable account, not
// merely a marker file. Keep every registered shape here because this helper is shared by the CLI's
// cross-provider credential matrix.
func signInCred(t *testing.T, cfg *config.Config, agent, name string) {
	t.Helper()
	seedNativeFixture(t, cfg, agent, name)
}

func signInDeadCred(t *testing.T, cfg *config.Config, agent, name string) {
	t.Helper()
	dir := cfg.AgentProfileDir(agent, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ag, _ := agents.Get(agent)
	file, _ := ag.AuthMarker()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// expandLadder: bare model → all signed-in accounts (marked-default first, then the rest);
// pinned → one target (unauthed pinned skipped); empty ladder → default model, all accounts.
func TestExpandLadder(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, cfg, "claude", "work")
	signInCred(t, cfg, "claude", "personal")
	if err := cfg.SetDefaultProfile("claude", "personal"); err != nil { // default first
		t.Fatal(err)
	}

	// Bare model fans out, marked-default (personal) first.
	got, err := expandLadder(cfg, "claude", []agents.Target{{Model: "opus"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude:opus@personal", "claude:opus@work"}; !slices.Equal(members(got), want) {
		t.Errorf("bare model fan-out = %v, want %v (default first)", members(got), want)
	}
	// Ladder: bare then pinned; dedup, order preserved.
	got, _ = expandLadder(cfg, "claude", []agents.Target{{Model: "fable"}, {Model: "opus", Accounts: []string{"work"}}})
	if want := []string{"claude:fable@personal", "claude:fable@work", "claude:opus@work"}; !slices.Equal(members(got), want) {
		t.Errorf("ladder = %v, want %v", members(got), want)
	}
	// Empty ladder → default model (empty) across accounts (provider shown, no model).
	got, _ = expandLadder(cfg, "claude", nil)
	if want := []string{"claude@personal", "claude@work"}; !slices.Equal(members(got), want) {
		t.Errorf("empty ladder = %v, want the accounts on the default model", members(got))
	}
	// A pinned account that isn't signed in is skipped, not an error, as long as something remains.
	got, _ = expandLadder(cfg, "claude", []agents.Target{{Model: "opus", Accounts: []string{"ghost"}}, {Model: "fable"}})
	if slices.Contains(members(got), "claude:opus@ghost") {
		t.Errorf("unsigned pinned account should be skipped: %v", members(got))
	}
	// No signed-in accounts at all → error.
	if _, err := expandLadder(&config.Config{ConfigDir: t.TempDir()}, "claude", nil); err == nil {
		t.Error("no signed-in account should error")
	}
}

func TestExpandLadderSkipsCredentialsThatNeedRelogin(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, cfg, "claude", "personal")
	signInDeadCred(t, cfg, "claude", "personal_backup")
	if err := cfg.SetDefaultProfile("claude", "personal"); err != nil {
		t.Fatal(err)
	}

	if got := accountsFor(cfg, "claude"); !slices.Equal(got, []string{"personal"}) {
		t.Fatalf("runnable Claude accounts = %v, want only personal", got)
	}
	targets, err := expandLadder(cfg, "claude", nil)
	if err != nil || !slices.Equal(members(targets), []string{"claude@personal"}) {
		t.Fatalf("rotation with dead fallback = %v, %v", members(targets), err)
	}

	_, err = expandLadder(cfg, "claude", []agents.Target{{Accounts: []string{"personal_backup"}}})
	if err == nil || !strings.Contains(err.Error(), "credential claude@personal_backup requires re-login") ||
		!strings.Contains(err.Error(), "coop login claude@personal_backup") {
		t.Fatalf("dead pinned credential error = %v, want exact re-login remedy", err)
	}

	_, err = expandLadder(cfg, "claude", []agents.Target{{Accounts: []string{"missing"}}})
	if err == nil || !strings.Contains(err.Error(), "credential claude@missing is not signed in") ||
		!strings.Contains(err.Error(), "coop login claude@missing") || strings.Contains(err.Error(), "personal_backup") {
		t.Fatalf("missing pinned credential error = %v, want only its exact login remedy", err)
	}

	signInDeadCred(t, cfg, "claude", "unsafe;echo nope")
	_, err = expandLadder(cfg, "claude", []agents.Target{{Accounts: []string{"unsafe;echo nope"}}})
	if err == nil || !strings.Contains(err.Error(), "coop login 'claude@unsafe;echo nope'") {
		t.Fatalf("unsafe credential recovery command = %v, want a shell-quoted target", err)
	}

	signInDeadCred(t, cfg, "claude", "line\nnext")
	_, err = expandLadder(cfg, "claude", []agents.Target{{Accounts: []string{"line\nnext"}}})
	if err == nil || strings.Contains(err.Error(), "line\nnext") ||
		!strings.Contains(err.Error(), `"claude@line\nnext"`) ||
		!strings.Contains(err.Error(), `coop login $'claude@line\nnext'`) {
		t.Fatalf("control-character credential recovery = %q, want escaped display and command", err)
	}
}

// A ladder with an account to run still runs when another provider's accounts could not be listed:
// the failed listing is named only when nothing in the ladder resolves.
func TestExpandLadderRunsWhenAnotherProviderCannotBeListed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory regardless of its mode")
	}
	cfg := &config.Config{ConfigDir: t.TempDir()}
	credentials := filepath.Join(cfg.ConfigDir, "claude", "credentials")
	if err := os.MkdirAll(credentials, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(credentials, 0o100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(credentials, 0o700) })
	signInCred(t, cfg, "codex", "work")

	got, err := expandLadder(cfg, "claude", []agents.Target{{}, {Provider: "codex"}})
	if err != nil || !slices.Equal(members(got), []string{"codex@work"}) {
		t.Fatalf("ladder = %v, %v; want [codex@work] and no error", members(got), err)
	}
	// Without the Codex rung nothing runs, and the refusal names Claude's folder.
	if _, err := expandLadder(cfg, "claude", nil); err == nil || !strings.HasPrefix(err.Error(), "Could not list every Claude account") {
		t.Fatalf("Claude-only ladder = %v, want the failed listing", err)
	}
}

// refuse runs one command on its own app, over a copy of cfg, and ends it the way the process does,
// returning the exit code, the refusal as one line, and everything the person saw. Nothing may start:
// an editor session fails the test, and the runtime shim records any call in recorder.
func refuse(t *testing.T, cfg *config.Config, recorder string, run func(*app, []string) (int, error), args []string) (exit int, refusal, stderr string) {
	t.Helper()
	a := &app{cfg: cfg.Clone(), rt: recordingRuntime(t, recorder), rtSet: true,
		acpSupervise: func([]string, *acpctl.Control) (int, error) {
			t.Error("an editor session started after a refusal")
			return 0, nil
		}}
	stderr = captureStderr(t, func() {
		code, err := run(a, args)
		switch usage := asUsage(err); {
		case usage != nil:
			refusal = usage.Error()
		case err != nil:
			refusal = err.Error()
		}
		exit = reportExit(code, err)
	})
	return exit, refusal, stderr
}

// A loop, or an editor session with peers, that finds no account to run while an agent's accounts
// could not all be listed names that failure and its cause, as every command naming an account does
// (TestAccountRefusalsNameAFailedListing), never a sign-in, and exits 1: the input was fine. Once the
// folder lists again, a missing account keeps its sign-in advice and exits 1 too, since a missing
// login is not rejected input either.
func TestLadderRefusalsNameAFailedListing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory regardless of its mode")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := freshConfig(t)
	// No Claude account can run, and solo is in a credentials folder that can be searched and not
	// listed. Each preset's lead pins one account.
	credentials := filepath.Join(cfg.ConfigDir, "claude", "credentials")
	if err := os.MkdirAll(filepath.Join(credentials, "solo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(credentials, 0o100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(credentials, 0o700) })
	for _, account := range []string{"solo", "ghost"} {
		writePresetFile(t, cfg.RepoOverride, "lead-"+account, "lead:\n  agent: [claude@"+account+"]\n")
	}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	const (
		anyAccount = "no signed-in account for claude. Run: coop login claude[@<account>]"
		ghost      = "credential claude@ghost is not signed in — run: coop login claude@ghost"
	)
	cases := []struct {
		name   string
		run    func(*app, []string) (int, error)
		args   func(account string) []string
		advice string // the refusal once the folder lists and the account is missing, unchanged
	}{
		{"loop", (*app).cmdLoop, func(string) []string { return []string{"claude"} }, anyAccount},
		{"loop-account", (*app).cmdLoop, func(account string) []string { return []string{"claude@" + account} }, ghost},
		{"loop-preset", (*app).cmdLoop, func(account string) []string { return []string{"lead-" + account} }, ghost},
		{"acp-peer", (*app).cmdACP, func(string) []string { return []string{"claude", "--peer", "codex"} }, anyAccount},
		{"acp-peer-preset", (*app).cmdACP, func(account string) []string { return []string{"lead-" + account, "--peer", "codex"} }, ghost},
	}
	want := "Could not list every Claude account — open " + credentials + ": permission denied"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exit, refusal, stderr := refuse(t, cfg, recorder, tc.run, tc.args("solo"))
			if exit != 1 || refusal != want || strings.Contains(stderr, "coop login") {
				t.Fatalf("refusal = (exit %d, %q), want (1, %q) and no sign-in:\n%s", exit, refusal, want, stderr)
			}
		})
	}
	if err := os.Chmod(credentials, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name+"-missing", func(t *testing.T) {
			if exit, refusal, _ := refuse(t, cfg, recorder, tc.run, tc.args("ghost")); exit != 1 || refusal != tc.advice {
				t.Fatalf("missing account = (exit %d, %q), want (1, %q)", exit, refusal, tc.advice)
			}
		})
	}
	if pathExists(recorder) {
		t.Fatal("a refused run reached the container runtime")
	}
}

// A --peer refusal exits by what went wrong, whichever command carries it: a peer with no usable
// login fails the job like a lead with none (exit 1, with the sign-in it needs), while a peer that
// is not allowed as typed is rejected input (exit 2), named before any account is checked and
// wherever it sits among the peers. Nothing reaches the runtime either way, and no fork is created.
func TestPeerRefusalsSplitAMissingLoginFromRejectedInput(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := freshConfig(t)
	signInCred(t, cfg, "claude", "default") // the lead can run; Codex has no login, and ghost is no account
	writePresetFile(t, cfg.RepoOverride, "lead", "lead:\n  agent: claude\n")
	writePresetFile(t, cfg.RepoOverride, "lead-ghost", "lead:\n  agent: [claude@ghost]\n")
	// A task queue, so a fork loop gets as far as its peers.
	if err := os.MkdirAll(filepath.Join(cfg.RepoOverride, ".agent", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	launch := func(a *app, args []string) (int, error) { return a.launchAgent(args[0], args[1:]) }
	// As the dispatcher runs `coop <preset>`: a preset that does not load is its own refusal.
	launchPreset := func(a *app, args []string) (int, error) {
		p, ok, err := a.presetNamed(args[0])
		if !ok || err != nil {
			return 2, fmt.Errorf("preset %s did not load: %v", args[0], err)
		}
		return a.launchPreset(p, args[1:])
	}
	// No fork exists here: a fork loop checks every peer before it creates one, and fork ACP checks a
	// peer typed wrong before it looks for the fork it serves.
	forkACP := func(a *app, args []string) (int, error) { return a.forkACP("nofork", args) }
	forkLoop := func(a *app, args []string) (int, error) {
		return a.forkCreate(append([]string{"nofork", args[0], "--loop"}, args[1:]...))
	}
	const (
		signIn   = "Codex needs a usable account — Sign in: coop login codex — Help: coop help credentials"
		rejected = "Peers share this run's selected accounts — "
	)
	target := func(account string) string {
		if account == "" {
			return "claude"
		}
		return "claude@" + account
	}
	preset := func(account string) string {
		if account == "" {
			return "lead"
		}
		return "lead-" + account
	}
	commands := []struct {
		name string
		who  func(account string) string // the run's positional target or preset, leading on account
		run  func(*app, []string) (int, error)
		fork bool // checks a peer's login only inside a fork that exists
	}{
		{"launch", target, launch, false}, {"preset", preset, launchPreset, false},
		{"loop", target, (*app).cmdLoop, false}, {"acp", target, (*app).cmdACP, false},
		{"fork-acp", target, forkACP, true}, {"fork-loop", target, forkLoop, false},
	}
	for _, tc := range []struct {
		name, account string // account is the lead's, "" for its default
		peers         []string
		exit          int
		refusal       string // the whole refusal, or the start of one that ends in the command's own page
	}{
		{"signed-out", "", []string{"codex"}, 1, signIn},
		// ghost is no account, so a peer typed wrong must be named before the lead is checked.
		{"with-an-account", "ghost", []string{"codex@work"}, 2, rejected},
		{"with-an-account-after-a-signed-out-peer", "ghost", []string{"codex", "codex@work"}, 2, rejected},
		{"with-an-account-before-a-signed-out-peer", "ghost", []string{"codex@work", "codex"}, 2, rejected},
	} {
		for _, command := range commands {
			if command.fork && tc.exit != 2 {
				continue
			}
			t.Run(command.name+"-"+tc.name, func(t *testing.T) {
				args := []string{command.who(tc.account)}
				for _, peer := range tc.peers {
					args = append(args, "--peer", peer)
				}
				exit, refusal, _ := refuse(t, cfg, recorder, command.run, args)
				if exit != tc.exit || !strings.HasPrefix(refusal, tc.refusal) {
					t.Fatalf("%q = (exit %d, %q), want (%d, %q)", args, exit, refusal, tc.exit, tc.refusal)
				}
				if command.name == "fork-loop" && pathExists(forkspace.Home(cfg.RepoOverride)) {
					t.Fatalf("%q left a fork folder or fork state behind", args)
				}
			})
		}
	}
	if pathExists(recorder) {
		t.Fatal("a refused run reached the container runtime")
	}
}

// Effort rides the ladder into each rotation target, and applyRunTarget lands it in cfg so
// EffortFor (and thus the agent command) sees it — the CLI glue for a /effort target. What the
// loop then does with the rotation's active target is internal/loop's
// TestApplyTargetThreadsEffortToConfig.
func TestEffortThreadsToConfig(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, cfg, "codex", "work")
	a := &app{cfg: cfg}

	// Loop path: the positional target IS the ladder, and expanding it preserves the effort.
	rungs := []agents.Target{{Provider: "codex", Model: "gpt-5.6-sol", Effort: "high", Accounts: []string{"work"}}}
	rot, err := a.buildRotation("codex", rungs)
	if err != nil {
		t.Fatal(err)
	}
	if got := rot.Members(); !slices.Contains(got, "codex:gpt-5.6-sol/high@work") {
		t.Fatalf("rotation targets = %v, want one carrying /high", got)
	}

	// Single-run path: applyRunTarget lands the effort in the top tier, alongside the model.
	cfg2 := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, cfg2, "codex", "work")
	a2 := &app{cfg: cfg2}
	if err := a2.applyRunTarget(agents.Target{Provider: "codex", Model: "gpt-5.6-sol", Effort: "xhigh", Accounts: []string{"work"}}, "coop codex"); err != nil {
		t.Fatal(err)
	}
	if got := cfg2.EffortFor("codex"); got != "xhigh" {
		t.Errorf("after applyRunTarget, EffortFor(codex) = %q, want xhigh", got)
	}
	if got := cfg2.ModelFor("codex"); got != "gpt-5.6-sol" {
		t.Errorf("model rides alongside: ModelFor(codex) = %q, want gpt-5.6-sol", got)
	}
}

// A cross-provider ladder fans each rung across ITS OWN provider's accounts, so the loop rotates
// across agents; a rung whose provider has no signed-in account is skipped, not fatal, as long as
// another rung resolves.
func TestExpandLadderCrossProvider(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, cfg, "claude", "work")
	signInCred(t, cfg, "codex", "work")

	got, err := expandLadder(cfg, "claude", []agents.Target{{Model: "opus"}, {Provider: "codex", Model: "gpt-5"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude:opus@work", "codex:gpt-5@work"}; !slices.Equal(members(got), want) {
		t.Errorf("cross-provider ladder = %v, want %v (rotates across agents)", members(got), want)
	}
	// codex signed out → its rung is skipped, claude's remains (not a fatal "nothing signed in").
	noCodex := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, noCodex, "claude", "work")
	got, err = expandLadder(noCodex, "claude", []agents.Target{{Model: "opus"}, {Provider: "codex", Model: "gpt-5"}})
	if err != nil || !slices.Equal(members(got), []string{"claude:opus@work"}) {
		t.Errorf("unsigned provider rung should be skipped: got %v (%v)", members(got), err)
	}
}

func members(ts []agents.Target) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.String()
	}
	return out
}

// oneOffLadder parses a decomposed one-off (model, effort, account) into a single ladder entry,
// model-first with the model@account shortcut; conflicting accounts error.
func TestOneOffLadder(t *testing.T) {
	if l, _ := oneOffLadder("", "", ""); l != nil {
		t.Errorf("no one-off → nil ladder, got %v", l)
	}
	l, err := oneOffLadder("opus@work", "", "high")
	if err != nil || len(l) != 1 || l[0].Model != "opus" || l[0].Account() != "work" {
		t.Errorf("model opus@work = %+v (%v)", l, err)
	}
	if err != nil || len(l) != 1 || l[0].Effort != "high" {
		t.Fatalf("oneOffLadder effort = %#v, %v", l, err)
	}
	l, _ = oneOffLadder("opus", "work", "")
	if l[0].Model != "opus" || l[0].Account() != "work" {
		t.Errorf("model opus + account work = %+v", l)
	}
	if _, err := oneOffLadder("opus@work", "personal", ""); err == nil {
		t.Error("account given twice (model @work + account personal) should error")
	}
	if _, err := oneOffLadder("opus@", "", ""); err == nil {
		t.Error("empty account after @ should error")
	}
}
