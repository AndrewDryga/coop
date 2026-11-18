package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkctl"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/loop"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// savedTargetApp is an app signed in to Claude as default and as personal, running Docker's stand-in
// that logs every call to the file it returns, in a repo with no fork yet.
func savedTargetApp(t *testing.T, repo string) (*app, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("COOP_REPO", repo)
	t.Setenv("COOP_EGRESS", "open")
	t.Setenv("COOP_BASE_IMAGE", "coop-box")
	configDir := t.TempDir()
	t.Setenv("COOP_CONFIG_DIR", configDir)
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	rt := nativeOnlineRuntime(t, recorder)
	t.Setenv("COOP_RUNTIME", rt.Name)
	a := &app{cfg: savedTargetConfig(repo, configDir, t.TempDir()), rt: rt, rtSet: true}
	signInCred(t, a.cfg, "claude", "default")
	signInCred(t, a.cfg, "claude", "personal")
	return a, recorder
}

func savedTargetConfig(repo, configDir, boxHome string) *config.Config {
	return &config.Config{
		RepoOverride: repo, ConfigDir: configDir, BoxHome: boxHome, HomeInBox: "/home/node", Homes: true,
		BaseImage: "coop-box", Egress: "open",
	}
}

// reopened is the same coop run again: the accounts are on disk, but nothing a command selected in
// memory (an account, a model) survives into it.
func reopened(a *app) *app {
	return &app{cfg: savedTargetConfig(a.cfg.RepoOverride, a.cfg.ConfigDir, a.cfg.BoxHome), rt: a.rt, rtSet: true, forkLoopRun: a.forkLoopRun}
}

// runFork runs coop fork with args and returns what it printed.
func runFork(t *testing.T, a *app, args ...string) (output string, code int, err error) {
	t.Helper()
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() { code, err = a.forkCreate(append(args, "--egress", "open")) })
	})
	return stdout + stderr, code, err
}

func dockerRuns(t *testing.T, recorder string) int {
	t.Helper()
	data, err := os.ReadFile(recorder)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Count("\n"+string(data), "\nrun ")
}

// A fork reopened with no target runs the one it was made with, or last switched to: its model and
// its account, not the default's. The header names it, and naming a target switches and saves it.
func TestForkReopenRunsTheTargetItWasMadeWith(t *testing.T) {
	repo := initRepo(t)
	a, recorder := savedTargetApp(t, repo)
	ws := forkspace.Workspace(repo, "saved")

	if out, code, err := runFork(t, a, "saved", "claude:haiku@personal"); code != 0 || err != nil {
		t.Fatalf("coop fork saved claude:haiku@personal = (%d, %v)\n%s", code, err, out)
	}
	if got := forkctl.ReadForkTarget(ws); got != "claude:haiku@personal" {
		t.Fatalf("the fork saved %q, want the target it was made with", got)
	}
	for _, args := range [][]string{{"saved"}, {"saved", "--new"}, {"saved", "-c"}} {
		b := reopened(a)
		out, code, err := runFork(t, b, args...)
		if code != 0 || err != nil {
			t.Fatalf("coop fork %s = (%d, %v)\n%s", strings.Join(args, " "), code, err, out)
		}
		if b.cfg.ModelFor("claude") != "haiku" || b.cfg.ActiveProfile("claude") != "personal" {
			t.Errorf("coop fork %s runs Claude with model %q on account %q, want haiku on personal",
				strings.Join(args, " "), b.cfg.ModelFor("claude"), b.cfg.ActiveProfile("claude"))
		}
		if !strings.Contains(out, "  Agent: claude:haiku@personal (saved for this fork)\n") {
			t.Errorf("coop fork %s does not name the target it opens:\n%s", strings.Join(args, " "), out)
		}
		if got := forkctl.ReadForkTarget(ws); got != "claude:haiku@personal" {
			t.Errorf("coop fork %s left the saved target as %q", strings.Join(args, " "), got)
		}
		// The conversation is the saved account's, not the default account's.
		if forkctl.ReadForkSession(ws, "claude", "personal") == "" || forkctl.ReadForkSession(ws, "claude", "default") != "" {
			t.Errorf("coop fork %s kept its conversation under the wrong account", strings.Join(args, " "))
		}
	}
	if got := dockerRuns(t, recorder); got != 4 {
		t.Errorf("Docker ran %d boxes, want one for the creation and one for each reopening", got)
	}

	// Naming a target switches the fork to it, saved whole, and the next bare reopening keeps it.
	if out, code, err := runFork(t, reopened(a), "saved", "claude"); code != 0 || err != nil {
		t.Fatalf("coop fork saved claude = (%d, %v)\n%s", code, err, out)
	}
	b := reopened(a)
	out, code, err := runFork(t, b, "saved")
	if code != 0 || err != nil {
		t.Fatalf("coop fork saved after a switch = (%d, %v)\n%s", code, err, out)
	}
	if got := forkctl.ReadForkTarget(ws); got != "claude" || b.cfg.ModelFor("claude") != "" || b.cfg.ActiveProfile("claude") != "default" ||
		!strings.Contains(out, "  Agent: claude (saved for this fork)\n") {
		t.Errorf("after switching to claude the fork saved %q and ran model %q on %q:\n%s", got, b.cfg.ModelFor("claude"), b.cfg.ActiveProfile("claude"), out)
	}
}

// A saved target that cannot run now is refused by name, with how to pick another. The fork is not
// started on another account or model, and its saved target stays what it was.
func TestForkReopenRefusesASavedTargetThatCannotRun(t *testing.T) {
	repo := initRepo(t)
	a, recorder := savedTargetApp(t, repo)
	if out, code, err := runFork(t, a, "saved", "claude:haiku@personal"); code != 0 || err != nil {
		t.Fatalf("creation = (%d, %v)\n%s", code, err, out)
	}
	ws := forkspace.Workspace(repo, "saved")
	removed := reopened(a)
	if code, err := removed.removeProfile("claude", "personal", true); code != 0 || err != nil {
		t.Fatalf("removing the account = (%d, %v)", code, err)
	}
	runs := dockerRuns(t, recorder)
	for _, args := range [][]string{{"saved"}, {"saved", "--new"}, {"saved", "--loop"}, {"saved", "--loop", "-d"}, {"saved", "--fresh", "--yes"}} {
		b := reopened(a)
		code, err := b.forkCreate(append(args, "--egress", "open"))
		usage := asUsage(err)
		if code != 1 || usage == nil || usage.Headline != `Fork "saved" cannot use its saved target "claude:haiku@personal"` || usage.Cause != `No Claude account named "personal"` {
			t.Fatalf("coop fork %s = (%d, %v), want the saved target refused by name", strings.Join(args, " "), code, err)
		}
		block := usageBlock(t, err)
		for _, want := range []string{"Sign in:      coop login claude@personal\n", "Pick another: coop fork saved <target>\n"} {
			if !strings.Contains(block, want) {
				t.Errorf("coop fork %s refusal does not say how to go on (%q):\n%s", strings.Join(args, " "), want, block)
			}
		}
		if b.cfg.ModelFor("claude") != "" || b.cfg.ActiveProfile("claude") != "default" {
			t.Errorf("coop fork %s picked model %q on account %q instead of refusing", strings.Join(args, " "), b.cfg.ModelFor("claude"), b.cfg.ActiveProfile("claude"))
		}
	}
	if got := dockerRuns(t, recorder); got != runs {
		t.Errorf("a refused reopening started %d more boxes", got-runs)
	}
	if got := forkctl.ReadForkTarget(ws); got != "claude:haiku@personal" || !pathExists(ws) {
		t.Errorf("a refused reopening changed the fork: it saved %q", got)
	}
	// Naming a target is the way out, and it saves.
	b := reopened(a)
	if out, code, err := runFork(t, b, "saved", "claude"); code != 0 || err != nil {
		t.Fatalf("coop fork saved claude = (%d, %v)\n%s", code, err, out)
	}
	if got := forkctl.ReadForkTarget(ws); got != "claude" {
		t.Errorf("naming another target saved %q, want claude", got)
	}
}

// A fork's loop resumes on the target the fork was made with: the rotation of its second run holds
// the same model and account, and the saved target stays what it was. The second run adds to the
// fork's log after the first's output, with a plain line where it starts.
func TestForkLoopResumeRunsTheTargetItWasMadeWith(t *testing.T) {
	repo := initRepo(t)
	seedForkDecisionTask(t, repo, "2026-11-06-saved-target-a")
	commitForkNetworkPreflightFixture(t, repo)
	a, _ := savedTargetApp(t, repo)
	var ran, targets []string
	park := parkingForkPass(t, &ran)
	a.forkLoopRun = func(spec loop.RunSpec) (int, error) {
		targets = append(targets, spec.Rotation.Active().String())
		fmt.Fprintf(spec.Sink, "output of pass %d\n", len(targets))
		return park(spec)
	}
	ws := forkspace.Workspace(repo, "looped")
	for i, args := range [][]string{{"looped", "claude:haiku@personal", "--loop"}, {"looped", "--loop"}} {
		if i == 1 { // the first run left its task waiting for a decision; the second takes a new one
			seedForkDecisionTask(t, repo, "2026-11-06-saved-target-b")
			commitForkNetworkPreflightFixture(t, repo)
		}
		out, code, err := runFork(t, reopened(a), args...)
		if code != loopDecisionExit || err != nil {
			t.Fatalf("run %d: coop fork %s = (%d, %v), want it stopped for a decision\n%s", i+1, strings.Join(args, " "), code, err, out)
		}
	}
	if want := []string{"claude:haiku@personal", "claude:haiku@personal"}; !slices.Equal(targets, want) {
		t.Errorf("the loop passes ran %v, want the fork's own target each run", targets)
	}
	if got := forkctl.ReadForkTarget(ws); got != "claude:haiku@personal" {
		t.Errorf("the resumed loop left the saved target as %q", got)
	}
	logs := captureStdout(t, func() {
		if code, err := a.forkctl().ForkLogs([]string{"looped"}); code != 0 || err != nil {
			t.Fatalf("coop fork logs = (%d, %v)", code, err)
		}
	})
	lines := strings.Split(strings.TrimSuffix(logs, "\n"), "\n")
	if len(lines) != 4 || lines[0] != "output of pass 1" || lines[1] != "" || !forkspace.IsLogRunStart(lines[2]) || lines[3] != "output of pass 2" {
		t.Errorf("coop fork logs = %q, want the first run's output, then the line that starts the second run, then its output", logs)
	}
}

// A fork loop that was stopped resumes in the background on the target the fork was made with, as the
// board's restart line says (coop fork <name> --loop -d): the line its stop prints and the worker it
// starts both name that model and account, and the saved target stays what it was.
func TestDetachedForkLoopResumesOnItsSavedTarget(t *testing.T) {
	repo := initRepo(t)
	queue := filepath.Join(repo, ".agent", "tasks")
	if err := os.MkdirAll(filepath.Join(queue, "00_todo"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := forkNetworkPreflightApp(t, repo)
	signInCred(t, a.cfg, "claude", "personal")
	name := "kept"
	ws := forkspace.Workspace(repo, name)
	t.Setenv(detachedChildEnv, "1") // its worker finds no task, so it ends by itself
	t.Cleanup(func() {
		if pathExists(forkspace.PidPath(repo, name)) {
			if code, err := a.forkctl().ForkStop([]string{name}); code != 0 || err != nil {
				t.Errorf("cleanup fork stop = (%d, %v)", code, err)
			}
		}
	})
	want := "coop fork " + name + " claude:haiku@personal --loop -d --tasks " + shellWord(queue) + " --egress open"
	for i, args := range [][]string{
		{name, "claude:haiku@personal", "--loop", "-d", "--tasks", queue, "--egress", "open"},
		{name, "--loop", "-d", "--tasks", queue, "--egress", "open"},
	} {
		var code int
		var err error
		out := captureStdout(t, func() {
			captureStderr(t, func() { code, err = a.forkCreate(args) })
		})
		if code != 0 || err != nil {
			t.Fatalf("start %d: coop fork %s = (%d, %v)\n%s", i+1, strings.Join(args, " "), code, err, out)
		}
		if got := forkspace.ReadWorkerContinue(repo, name); got != want {
			t.Errorf("start %d noted %q, want %q", i+1, got, want)
		}
		if got := forkctl.ReadForkTarget(ws); got != "claude:haiku@personal" {
			t.Errorf("start %d left the saved target as %q", i+1, got)
		}
		wait.For(t, "the drained worker to clear its record", func() bool { return !pathExists(forkspace.PidPath(repo, name)) })
	}
}

// presetFork writes the frontier preset, whose lead is claude:haiku@personal with a critic role, and
// makes fork name from it.
func presetFork(t *testing.T, a *app, repo, name string, extra ...string) {
	t.Helper()
	writePresetFile(t, repo, "frontier", "lead:\n  agent: [claude:haiku@personal]\nroles:\n  critic:\n    mode: consult\n    agent: claude\n")
	if out, code, err := runFork(t, a, append([]string{name, "frontier"}, extra...)...); code != 0 || err != nil {
		t.Fatalf("coop fork %s frontier = (%d, %v)\n%s", name, code, err, out)
	}
	if got := forkctl.ReadForkPreset(forkspace.Workspace(repo, name)); got != "frontier" {
		t.Fatalf("the fork saved preset %q, want frontier", got)
	}
}

// A fork made from a preset reopens with that preset, not with its lead's provider bare: the lead's
// model and account, and the preset's roles. Naming a plain target switches it off the preset.
func TestForkReopenRunsThePresetItWasMadeWith(t *testing.T) {
	repo := initRepo(t)
	a, _ := savedTargetApp(t, repo)
	ws := forkspace.Workspace(repo, "perf")
	presetFork(t, a, repo, "perf")
	for _, args := range [][]string{{"perf"}, {"perf", "--new"}} {
		b := reopened(a)
		out, code, err := runFork(t, b, args...)
		if code != 0 || err != nil {
			t.Fatalf("coop fork %s = (%d, %v)\n%s", strings.Join(args, " "), code, err, out)
		}
		if b.preset == nil || b.preset.Name != "frontier" || len(b.preset.Roles) != 1 {
			t.Errorf("coop fork %s ran preset %+v, want frontier with its critic", strings.Join(args, " "), b.preset)
		}
		if b.cfg.ModelFor("claude") != "haiku" || b.cfg.ActiveProfile("claude") != "personal" {
			t.Errorf("coop fork %s runs Claude with model %q on account %q, want the preset lead's haiku on personal",
				strings.Join(args, " "), b.cfg.ModelFor("claude"), b.cfg.ActiveProfile("claude"))
		}
		if !strings.Contains(out, "  Preset: frontier (saved for this fork)\n") {
			t.Errorf("coop fork %s does not name the preset it opens:\n%s", strings.Join(args, " "), out)
		}
	}
	if out, code, err := runFork(t, reopened(a), "perf", "claude"); code != 0 || err != nil {
		t.Fatalf("coop fork perf claude = (%d, %v)\n%s", code, err, out)
	}
	b := reopened(a)
	if out, code, err := runFork(t, b, "perf"); code != 0 || err != nil || b.preset != nil || forkctl.ReadForkPreset(ws) != "" ||
		!strings.Contains(out, "  Agent: claude (saved for this fork)\n") {
		t.Errorf("after switching to claude the fork still ran a preset (%v, saved %q): (%d, %v)\n%s", b.preset, forkctl.ReadForkPreset(ws), code, err, out)
	}
}

// A saved preset that no longer loads is refused by name, never replaced by its lead's provider, on
// every way of reopening the fork.
func TestForkReopenRefusesASavedPresetThatNoLongerLoads(t *testing.T) {
	repo := initRepo(t)
	a, recorder := savedTargetApp(t, repo)
	presetFork(t, a, repo, "perf")
	if err := os.RemoveAll(filepath.Join(repo, ".agent", "presets", "frontier")); err != nil {
		t.Fatal(err)
	}
	runs := dockerRuns(t, recorder)
	for _, args := range [][]string{{"perf"}, {"perf", "--loop"}, {"perf", "--loop", "-d"}, {"perf", "--fresh", "--yes"}} {
		b := reopened(a)
		code, err := b.forkCreate(append(args, "--egress", "open"))
		usage := asUsage(err)
		if code != 1 || usage == nil || usage.Headline != `Fork "perf" cannot use its saved preset "frontier"` {
			t.Fatalf("coop fork %s = (%d, %v), want the saved preset refused by name", strings.Join(args, " "), code, err)
		}
		if block := usageBlock(t, err); !strings.Contains(block, "Pick another: coop fork perf <target>\n") {
			t.Errorf("coop fork %s refusal does not say how to go on:\n%s", strings.Join(args, " "), block)
		}
		if b.preset != nil || b.cfg.ActiveProfile("claude") != "default" {
			t.Errorf("coop fork %s picked a preset or account instead of refusing", strings.Join(args, " "))
		}
	}
	if got := dockerRuns(t, recorder); got != runs {
		t.Errorf("a refused reopening started %d more boxes", got-runs)
	}
	if got := forkctl.ReadForkPreset(forkspace.Workspace(repo, "perf")); got != "frontier" {
		t.Errorf("a refused reopening changed the saved preset to %q", got)
	}
}

// A fork loop made from a preset resumes with it: the second run's rotation is the preset lead's,
// and the loop carries the preset's roles.
func TestForkLoopResumeRunsThePresetItWasMadeWith(t *testing.T) {
	repo := initRepo(t)
	seedForkDecisionTask(t, repo, "2026-11-17-saved-preset-a")
	commitForkNetworkPreflightFixture(t, repo)
	a, _ := savedTargetApp(t, repo)
	writePresetFile(t, repo, "frontier", "lead:\n  agent: [claude:haiku@personal]\nroles:\n  critic:\n    mode: consult\n    agent: claude\n")
	var ran, targets, presets []string
	park := parkingForkPass(t, &ran)
	a.forkLoopRun = func(spec loop.RunSpec) (int, error) {
		targets = append(targets, spec.Rotation.Active().String())
		name := ""
		if spec.Preset != nil {
			name = spec.Preset.Name
		}
		presets = append(presets, name)
		return park(spec)
	}
	for i, args := range [][]string{{"looped", "frontier", "--loop"}, {"looped", "--loop"}} {
		if i == 1 {
			seedForkDecisionTask(t, repo, "2026-11-17-saved-preset-b")
			commitForkNetworkPreflightFixture(t, repo)
		}
		out, code, err := runFork(t, reopened(a), args...)
		if code != loopDecisionExit || err != nil {
			t.Fatalf("run %d: coop fork %s = (%d, %v), want it stopped for a decision\n%s", i+1, strings.Join(args, " "), code, err, out)
		}
	}
	if want := []string{"claude:haiku@personal", "claude:haiku@personal"}; !slices.Equal(targets, want) {
		t.Errorf("the loop passes ran %v, want the preset lead's target each run", targets)
	}
	if want := []string{"frontier", "frontier"}; !slices.Equal(presets, want) {
		t.Errorf("the loop passes ran presets %q, want frontier each run", presets)
	}
}

// A detached fork loop made from a preset restarts with it: the line its stop prints and the worker
// it starts both name the preset.
func TestDetachedForkLoopResumesWithItsSavedPreset(t *testing.T) {
	repo := initRepo(t)
	queue := filepath.Join(repo, ".agent", "tasks")
	if err := os.MkdirAll(filepath.Join(queue, "00_todo"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := forkNetworkPreflightApp(t, repo)
	signInCred(t, a.cfg, "claude", "personal")
	writePresetFile(t, repo, "frontier", "lead:\n  agent: [claude:haiku@personal]\nroles:\n  critic:\n    mode: consult\n    agent: claude\n")
	name := "kept"
	t.Setenv(detachedChildEnv, "1") // its worker finds no task, so it ends by itself
	t.Cleanup(func() {
		if pathExists(forkspace.PidPath(repo, name)) {
			if code, err := a.forkctl().ForkStop([]string{name}); code != 0 || err != nil {
				t.Errorf("cleanup fork stop = (%d, %v)", code, err)
			}
		}
	})
	want := "coop fork " + name + " frontier --loop -d --tasks " + shellWord(queue) + " --egress open"
	for i, args := range [][]string{
		{name, "frontier", "--loop", "-d", "--tasks", queue, "--egress", "open"},
		{name, "--loop", "-d", "--tasks", queue, "--egress", "open"},
	} {
		var code int
		var err error
		out := captureStdout(t, func() {
			captureStderr(t, func() { code, err = reopened(a).forkCreate(args) })
		})
		if code != 0 || err != nil {
			t.Fatalf("start %d: coop fork %s = (%d, %v)\n%s", i+1, strings.Join(args, " "), code, err, out)
		}
		if got := forkspace.ReadWorkerContinue(repo, name); got != want {
			t.Errorf("start %d noted %q, want %q", i+1, got, want)
		}
		wait.For(t, "the drained worker to clear its record", func() bool { return !pathExists(forkspace.PidPath(repo, name)) })
	}
}

// An editor session runs no preset yet, so a target-less coop fork <name> acp on a fork made from one
// is refused by name instead of opening the lead's provider bare; naming a target still works.
func TestForkACPRefusesAForkMadeFromAPreset(t *testing.T) {
	repo := initRepo(t)
	a, _ := savedTargetApp(t, repo)
	presetFork(t, a, repo, "perf")
	code, err := reopened(a).forkACP("perf", nil)
	usage := asUsage(err)
	if code != 1 || usage == nil || usage.Headline != `Fork "perf" was made from preset "frontier", which an editor session cannot run` {
		t.Fatalf("coop fork perf acp = (%d, %v), want the preset named", code, err)
	}
	block := usageBlock(t, err)
	for _, want := range []string{"coop fork perf\n", "coop fork perf acp <target>\n"} {
		if !strings.Contains(block, want) {
			t.Errorf("refusal does not offer %q:\n%s", want, block)
		}
	}
}
