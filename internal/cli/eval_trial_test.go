package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/eval"
)

const trialSuiteYAML = `version: 1
name: trial-suite
runner: agent
cases:
  - id: hello
    instruction: "print hello"
    files: ./files
    verifier: ./verifiers/hello
    timeout: 5m
`

// trialSuite writes a minimal, valid agent suite: a fixture the candidate gets, and a verifier that
// must stay hidden from it.
func trialSuite(t *testing.T) *eval.Suite {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string, perm os.FileMode) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), perm); err != nil {
			t.Fatal(err)
		}
	}
	write("suite.yaml", trialSuiteYAML, 0o644)
	write("files/README.md", "# start here\n", 0o644)
	write("verifiers/hello/verify.sh", "test -f /workspace/answer.txt\n", 0o644)
	// The secret the candidate must never see.
	write("verifiers/hello/expected.txt", "hello\n", 0o644)

	suite, err := eval.Load(filepath.Join(dir, "suite.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return suite
}

func trialFor(suite *eval.Suite) eval.Trial {
	return eval.Trial{
		Case:   suite.Cases[0],
		Config: eval.FrozenConfig{Kind: eval.ConfigTarget, Label: "codex:gpt-5.6"},
	}
}

func newTrialRunner(t *testing.T, suite *eval.Suite, run boxRunner) *trialRunner {
	t.Helper()
	return &trialRunner{
		app:      &app{cfg: &config.Config{Egress: "open", Homes: true}},
		suite:    suite,
		image:    "coop-box:test",
		workRoot: t.TempDir(),
		runBox:   run,
	}
}

// The happy path, end to end without a container runtime: the workspace is materialized, the
// candidate "works" in it, the snapshot is graded, and the size shows up beside the verdict.
func TestTrialRunnerPassesAndReportsSizeBesideTheVerdict(t *testing.T) {
	suite := trialSuite(t)
	r := newTrialRunner(t, suite, func(spec box.RunSpec) (int, error) {
		if spec.Agent != "" { // the attempt: the candidate does the work
			if err := os.WriteFile(filepath.Join(spec.Repo, "answer.txt"), []byte("hello\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return 0, nil
		}
		// grading: the verifier's own check, faked as a pass
		return 0, nil
	})
	res := r.run(context.Background(), trialFor(suite))
	if res.Status != eval.TrialPassed {
		t.Fatalf("status = %q, detail %q", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "net code") {
		t.Errorf("change size was not reported beside the verdict: %q", res.Detail)
	}
}

// The isolation property the whole eval rests on: the verifier — and anything beside it — is never
// inside what the candidate is given.
func TestTrialRunnerNeverGivesTheCandidateTheVerifier(t *testing.T) {
	suite := trialSuite(t)
	attempted := false
	r := newTrialRunner(t, suite, func(spec box.RunSpec) (int, error) {
		if spec.Agent == "" {
			return 0, nil // grading; it is allowed the verifier
		}
		attempted = true
		// A candidate that looks at every mount it has must not find the grader...
		for _, arg := range spec.ExtraArgs {
			if strings.Contains(arg, "verif") {
				t.Errorf("the candidate's box mounts the verifier: %q", arg)
			}
		}
		// ...nor find it by walking its whole workspace, WHILE it is running.
		err := filepath.WalkDir(spec.Repo, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if strings.Contains(strings.ToLower(d.Name()), "verif") || d.Name() == "expected.txt" {
				t.Errorf("hidden eval material reached the candidate workspace: %s", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return 0, nil
	})
	r.run(context.Background(), trialFor(suite))
	if !attempted {
		t.Fatal("the attempt never ran")
	}
}

// A trial that did not pass keeps its graded workspace for the user to inspect; one that passed
// does not leave megabytes of "it worked" behind.
func TestTrialRunnerKeepsEvidenceOnlyWhenSomethingWentWrong(t *testing.T) {
	suite := trialSuite(t)
	for _, tc := range []struct {
		name      string
		gradeCode int
		wantKept  bool
	}{
		{"a pass leaves nothing behind", 0, false},
		{"a failure keeps the graded workspace", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTrialRunner(t, suite, func(spec box.RunSpec) (int, error) {
				if spec.Agent != "" {
					return 0, nil
				}
				return tc.gradeCode, nil
			})
			res := r.run(context.Background(), trialFor(suite))
			entries, err := os.ReadDir(r.workRoot)
			if err != nil {
				t.Fatal(err)
			}
			if kept := len(entries) > 0; kept != tc.wantKept {
				t.Errorf("kept = %v, want %v (status %q)", kept, tc.wantKept, res.Status)
			}
			if tc.wantKept && !strings.Contains(res.Detail, "kept at") {
				t.Errorf("the detail does not say where the evidence is: %q", res.Detail)
			}
		})
	}
}

// Grading happens on a SNAPSHOT: a verifier that writes cannot change what the candidate produced.
func TestTrialRunnerGradesASnapshotNotTheLiveWorkspace(t *testing.T) {
	suite := trialSuite(t)
	var candidateRepo, gradedDir string
	r := newTrialRunner(t, suite, func(spec box.RunSpec) (int, error) {
		if spec.Agent != "" {
			candidateRepo = spec.Repo
			return 0, os.WriteFile(filepath.Join(spec.Repo, "answer.txt"), []byte("hello\n"), 0o644)
		}
		gradedDir = spec.Repo
		// The verifier mutates its own copy, as a build or test run would.
		return 0, os.WriteFile(filepath.Join(spec.Repo, "verifier-artifact"), []byte("x"), 0o644)
	})
	r.run(context.Background(), trialFor(suite))

	if gradedDir == "" || gradedDir == candidateRepo {
		t.Fatalf("grading ran against the live workspace (%q vs %q)", gradedDir, candidateRepo)
	}
	if _, err := os.Stat(filepath.Join(candidateRepo, "verifier-artifact")); !os.IsNotExist(err) {
		t.Error("the verifier's write reached the candidate's recorded workspace")
	}
}

// A trial that runs out of its budget is a timeout — its own outcome, not a failure and not a
// harness error.
func TestTrialRunnerReportsATimeoutAsItsOwnOutcome(t *testing.T) {
	suite := trialSuite(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	r := newTrialRunner(t, suite, func(spec box.RunSpec) (int, error) {
		if spec.Agent != "" {
			<-ctx.Done() // the candidate is still going when the budget runs out
			return -1, ctx.Err()
		}
		return 0, nil
	})
	res := r.run(ctx, trialFor(suite))
	if res.Status != eval.TrialTimedOut {
		t.Errorf("status = %q, want timed out (detail %q)", res.Status, res.Detail)
	}
}

// A preset on an agent suite is refused with a reason, not silently evaluated as its lead alone.
func TestTrialRunnerRefusesAPresetOnAnAgentSuite(t *testing.T) {
	suite := trialSuite(t)
	r := newTrialRunner(t, suite, func(box.RunSpec) (int, error) { return 0, nil })
	tr := trialFor(suite)
	tr.Config = eval.FrozenConfig{Kind: eval.ConfigPreset, Label: "frontier"}
	res := r.run(context.Background(), tr)
	if res.Status != eval.TrialError || !strings.Contains(res.Detail, "loop suites") {
		t.Errorf("status = %q, detail = %q", res.Status, res.Detail)
	}
}

// The candidate's exit code does not decide the verdict — the verifier does. An agent that exits
// non-zero having done the work still passes.
func TestTrialRunnerLetsTheVerifierDecideNotTheAgentExitCode(t *testing.T) {
	suite := trialSuite(t)
	r := newTrialRunner(t, suite, func(spec box.RunSpec) (int, error) {
		if spec.Agent != "" {
			return 1, nil // the agent exited non-zero...
		}
		return 0, nil // ...but its work verifies
	})
	if res := r.run(context.Background(), trialFor(suite)); res.Status != eval.TrialPassed {
		t.Errorf("status = %q (detail %q); the verifier decides, not the agent's exit code", res.Status, res.Detail)
	}
}

// A pinned @account must actually be used: labelling a run with one credential and executing it on
// another would make the whole comparison a lie. More than one account is a ladder, which an eval
// must refuse rather than silently narrow.
func TestApplyEvalConfiguration(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	name, err := applyEvalConfiguration(cfg, eval.FrozenConfig{Kind: eval.ConfigTarget, Label: "codex:gpt-5.6/xhigh@work"})
	if err != nil || name != "codex" {
		t.Fatalf("agent = %q, err = %v", name, err)
	}
	// AgentDir resolves through the selected profile, so the pinned account shows up in the
	// credential home the attempt would mount.
	if dir := cfg.AgentDir("codex"); !strings.Contains(dir, "work") {
		t.Errorf("the pinned account was dropped: credential home = %q", dir)
	}

	// A ladder is refused by name.
	if _, err := applyEvalConfiguration(cfg, eval.FrozenConfig{Kind: eval.ConfigTarget, Label: "codex@work,personal"}); err == nil ||
		!strings.Contains(err.Error(), "exactly one") {
		t.Errorf("a two-account ladder should be refused, got %v", err)
	}
	// A preset is refused with the reason.
	if _, err := applyEvalConfiguration(cfg, eval.FrozenConfig{Kind: eval.ConfigPreset, Label: "frontier"}); err == nil ||
		!strings.Contains(err.Error(), "loop suites") {
		t.Errorf("a preset should be refused, got %v", err)
	}
}

// Clone must deep-copy the per-run maps, or two trials evaluating DIFFERENT configurations would
// share one selection.
func TestEvalConfigurationsDoNotLeakBetweenTrials(t *testing.T) {
	base, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	a, b := base.Clone(), base.Clone()
	if _, err := applyEvalConfiguration(a, eval.FrozenConfig{Kind: eval.ConfigTarget, Label: "codex:gpt-5.6@work"}); err != nil {
		t.Fatal(err)
	}
	if dir := b.AgentDir("codex"); strings.Contains(dir, "work") {
		t.Errorf("one trial's account selection leaked into another: %q", dir)
	}
	if dir := base.AgentDir("codex"); strings.Contains(dir, "work") {
		t.Errorf("a trial's account selection leaked into the caller's config: %q", dir)
	}
}
