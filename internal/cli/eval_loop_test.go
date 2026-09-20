package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/eval"
)

const loopSuiteYAML = `version: 1
name: loop-suite
runner: loop
loop_config: ./loop.yaml
cases:
  - id: evolve
    fixture: ./fixtures/app
    tasks: ./queues/evolve
    verifier: ./verifiers/evolve
    timeout: 5m
`

// loopSuite writes a minimal valid loop scenario: a fixture repository, an ordinary task queue, a
// loop recipe, and a hidden verifier.
func loopSuite(t *testing.T) *eval.Suite {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("suite.yaml", loopSuiteYAML)
	write("loop.yaml", "signoff:\n  rounds: 2\n")
	write("fixtures/app/main.go", "package main\n\nfunc main() {}\n")
	write("queues/evolve/00_todo/add-a-flag/task.md", "# Add a flag\n\nAdd --verbose.\n")
	write("verifiers/evolve/verify.sh", "exit 0\n")

	suite, err := eval.Load(filepath.Join(dir, "suite.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return suite
}

func loopRunner(t *testing.T, suite *eval.Suite, presets map[string]string) *trialRunner {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return &trialRunner{app: &app{cfg: cfg}, suite: suite, image: "coop-box:test", workRoot: t.TempDir(), presets: presets}
}

func loopWorkspace(t *testing.T, suite *eval.Suite) string {
	t.Helper()
	ws := filepath.Join(t.TempDir(), "ws")
	if _, err := eval.PrepareWorkspace(context.Background(), filepath.Join(suite.Dir, "fixtures/app"), ws); err != nil {
		t.Fatal(err)
	}
	return ws
}

// A loop scenario gets its queue where the loop reads it, and the FROZEN recipe — the bytes this run
// is comparing, not whatever the suite file says now.
func TestLoopScenarioMaterializesQueueAndFrozenRecipe(t *testing.T) {
	suite := loopSuite(t)
	r := loopRunner(t, suite, nil)
	ws := loopWorkspace(t, suite)

	err := r.materializeLoopScenario(eval.Trial{
		Case:   suite.Cases[0],
		Config: eval.FrozenConfig{Kind: eval.ConfigTarget, Label: "codex", LoopConfig: []byte("signoff:\n  rounds: 9\n")},
	}, ws)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(ws, ".agent", "tasks", "00_todo", "add-a-flag", "task.md")); err != nil {
		t.Errorf("the case's queue was not materialized where the loop reads it: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(ws, ".agent", "loop.yaml"))
	if err != nil {
		t.Fatalf("the frozen loop config was not written: %v", err)
	}
	if !strings.Contains(string(body), "rounds: 9") {
		t.Errorf("the loop would read the wrong recipe: %q", body)
	}
	// And the fixture is a repository with a commit, which the loop requires to start at all.
	if _, err := os.Stat(filepath.Join(ws, ".git")); err != nil {
		t.Errorf("the scenario workspace is not a git repository: %v", err)
	}
}

// The preset a loop scenario compares must travel INTO the trial workspace. A preset resolves from
// the repository the loop works, or a global directory; a trial is a fresh fixture with neither, so
// without this `coop loop frontier` would not find the thing being evaluated.
func TestLoopScenarioCarriesThePresetIntoTheWorkspace(t *testing.T) {
	suite := loopSuite(t)
	presetDir := filepath.Join(t.TempDir(), "frontier")
	for rel, body := range map[string]string{
		"preset.yaml":       "lead:\n  prompt: ./prompts/lead.md\n",
		"prompts/lead.md":   "You are the lead.\n",
		"prompts/critic.md": "You are the critic.\n",
	} {
		p := filepath.Join(presetDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	staged, err := eval.StagePreset(presetDir, filepath.Join(t.TempDir(), "stage"), "frontier")
	if err != nil {
		t.Fatal(err)
	}

	r := loopRunner(t, suite, map[string]string{"frontier": staged})
	ws := loopWorkspace(t, suite)
	if err := r.materializeLoopScenario(eval.Trial{
		Case:   suite.Cases[0],
		Config: eval.FrozenConfig{Kind: eval.ConfigPreset, Label: "frontier"},
	}, ws); err != nil {
		t.Fatal(err)
	}

	// Exactly where preset.Load looks: <repo>/.agent/presets/<name>/, prompts included.
	for _, rel := range []string{"preset.yaml", "prompts/lead.md", "prompts/critic.md"} {
		if _, err := os.Stat(filepath.Join(ws, ".agent", "presets", "frontier", rel)); err != nil {
			t.Errorf("the preset did not travel into the trial workspace: %s (%v)", rel, err)
		}
	}
}

// The child loop must be PINNED to the trial, not merely started in its directory. `coop loop`
// resolves its repository from configuration, so an operator with COOP_REPO set would otherwise have
// the trial run against their own checkout, with their credentials, making real commits — and then
// grade the untouched trial workspace.
func TestLoopTrialPinsTheChildToTheTrial(t *testing.T) {
	suite := loopSuite(t)
	r := loopRunner(t, suite, nil)
	ws := loopWorkspace(t, suite)

	dump := filepath.Join(t.TempDir(), "dump")
	shim := filepath.Join(t.TempDir(), "coop-shim")
	script := "#!/bin/sh\n{ echo \"ARGS=$*\"; echo \"PWD=$PWD\"; " +
		"echo \"COOP_REPO=$COOP_REPO\"; echo \"COOP_TASKS=[$COOP_TASKS]\"; echo \"COOP_IMAGE=$COOP_IMAGE\"; " +
		"echo \"COOP_MCP_FILE=[$COOP_MCP_FILE]\"; echo \"COOP_CACHE=$COOP_CACHE\"; } > " + dump + "\nexit 0\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	restore := loopExecutable
	loopExecutable = func() (string, error) { return shim, nil }
	defer func() { loopExecutable = restore }()

	// An operator whose environment points at their own repository and queue.
	t.Setenv("COOP_REPO", "/the/operators/real/checkout")
	t.Setenv("COOP_TASKS", "/the/operators/real/queue")

	out, err := r.runLoopTrial(context.Background(), eval.Trial{
		Case: suite.Cases[0], Config: eval.FrozenConfig{Kind: eval.ConfigTarget, Label: "codex"},
	}, ws)
	if err != nil {
		t.Fatalf("the loop trial did not run: %v (%s)", err, out.detail)
	}
	body, rerr := os.ReadFile(dump)
	if rerr != nil {
		t.Fatalf("the child never ran: %v", rerr)
	}
	got := string(body)
	for _, want := range []string{
		"COOP_REPO=" + ws, // the trial's repository, NOT the operator's
		"COOP_TASKS=[]",   // the queue we materialized, not a configured one
		"COOP_IMAGE=coop-box:test",
		"COOP_MCP_FILE=[]", // no operator MCP servers inside a trial
		"COOP_CACHE=0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("child environment missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "/the/operators/real/") {
		t.Errorf("the operator's own repository or queue reached the trial:\n%s", got)
	}
	if !strings.Contains(got, "ARGS=loop codex") {
		t.Errorf("the configuration was not passed to the loop: %s", got)
	}
	// The child also STARTS in the trial (compared through symlinks: macOS resolves /var to
	// /private/var, so a literal match would be testing the platform, not the wiring).
	realWS, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "PWD="+realWS) {
		t.Errorf("the child did not start in the trial workspace (want %s):\n%s", realWS, got)
	}
}

// A loop that overruns its budget is stopped and reported as a timeout, promptly — not left to pin a
// worker for the rest of the run.
func TestLoopTrialStopsAtTheDeadline(t *testing.T) {
	suite := loopSuite(t)
	r := loopRunner(t, suite, nil)
	ws := loopWorkspace(t, suite)

	shim := filepath.Join(t.TempDir(), "coop-shim")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	restore := loopExecutable
	loopExecutable = func() (string, error) { return shim, nil }
	defer func() { loopExecutable = restore }()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := r.runLoopTrial(ctx, eval.Trial{
		Case: suite.Cases[0], Config: eval.FrozenConfig{Kind: eval.ConfigTarget, Label: "codex"},
	}, ws)
	if err == nil {
		t.Error("an overrunning loop was reported as a completed run")
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Errorf("the deadline took %v to take effect", elapsed)
	}
}

// A scenario whose queue has nothing waiting measures nothing: the loop would report "nothing
// actionable", exit cleanly, and be graded as a real result.
func TestMaterializeQueueRequiresWorkWaiting(t *testing.T) {
	// No 00_todo at all.
	if err := eval.MaterializeQueue(t.TempDir(), t.TempDir()); err == nil {
		t.Error("a queue template with no 00_todo was accepted")
	}
	// 00_todo present but empty, with work only in 99_done.
	tmpl := t.TempDir()
	for _, d := range []string{"00_todo", "99_done/already-done"} {
		if err := os.MkdirAll(filepath.Join(tmpl, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmpl, "99_done", "already-done", "task.md"), []byte("# done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := eval.MaterializeQueue(tmpl, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "measures nothing") {
		t.Errorf("a queue with no work waiting was accepted: %v", err)
	}
	if err := eval.MaterializeQueue(filepath.Join(t.TempDir(), "gone"), t.TempDir()); err == nil {
		t.Error("a missing queue template was accepted")
	}
}

// A fixture that ships its own queue or a same-named preset would silently merge with — or shadow —
// what the scenario means to evaluate.
func TestMaterializeRefusesToCollideWithTheFixture(t *testing.T) {
	tmpl := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpl, "00_todo", "t"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpl, "00_todo", "t", "task.md"), []byte("# t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, eval.TasksRoot, "00_todo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := eval.MaterializeQueue(tmpl, dest); err == nil || !strings.Contains(err.Error(), "only one") {
		t.Errorf("a fixture's own queue was merged with the scenario's: %v", err)
	}

	staged := t.TempDir()
	if err := os.WriteFile(filepath.Join(staged, "preset.yaml"), []byte("lead: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest2, eval.PresetsRoot, "frontier"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := eval.MaterializePreset(staged, dest2, "frontier"); err == nil ||
		!strings.Contains(err.Error(), "shadow") {
		t.Errorf("a fixture preset shadowed the one being evaluated: %v", err)
	}
}

// Staging captures the preset ONCE per run, so an operator editing it mid-run cannot make two
// trials — or the two sides of a comparison — measure different things.
func TestStagePresetIsCapturedOncePerRun(t *testing.T) {
	src := filepath.Join(t.TempDir(), "frontier")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(src, "preset.yaml")
	if err := os.WriteFile(manifest, []byte("lead: original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(t.TempDir(), "stage")
	staged, err := eval.StagePreset(src, stage, "frontier")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("lead: edited mid-run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := eval.StagePreset(src, stage, "frontier")
	if err != nil {
		t.Fatal(err)
	}
	if again != staged {
		t.Errorf("re-staging produced a different directory: %q vs %q", again, staged)
	}
	body, err := os.ReadFile(filepath.Join(staged, "preset.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "edited mid-run") {
		t.Error("a mid-run edit reached a later trial; the two sides would measure different presets")
	}
}

// The loop's exit code is context, not the verdict. A run that finished with work remaining, or
// stopped on a human decision, is a real result about the configuration — exactly what a loop eval
// measures — so it still gets graded. Only "cut off" and "never ran" do not.
func TestLoopOutcomeDecidesWhatIsGradable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      int
		ctxErr    error
		wantGrade bool
	}{
		{"queue drained", loopExitDrained, nil, true},
		{"work remained", loopExitWorkRemains, nil, true},
		{"blocked on a human decision", loopExitBlocked, nil, true},
		{"interrupted", loopExitInterrupted, nil, false},
		{"refused to start", 2, nil, false},
		{"killed by a signal", -1, nil, false},
		{"deadline wins over any exit code", loopExitDrained, context.DeadlineExceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := loopOutcome(tc.code, tc.ctxErr)
			if gradable := err == nil; gradable != tc.wantGrade {
				t.Errorf("gradable = %v, want %v (err %v)", gradable, tc.wantGrade, err)
			}
		})
	}
}
