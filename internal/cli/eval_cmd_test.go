package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/eval"
)

func TestParseEvalRunArgs(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantSuite   string
		wantConfigs []string
		wantJobs    int
		wantRepeat  int
		wantTimeout time.Duration
		wantLoop    string
		wantErr     string
	}{
		{"suite and two configs", []string{"./s.yaml", "codex", "frontier", "--timeout", "60m"}, "./s.yaml", []string{"codex", "frontier"}, 1, 1, time.Hour, "", ""},
		{"flags interleaved", []string{"./s.yaml", "--jobs", "4", "codex", "--repeat", "3", "--timeout", "30m"}, "./s.yaml", []string{"codex"}, 4, 3, 30 * time.Minute, "", ""},
		{"loop config override", []string{"./s.yaml", "frontier", "--timeout", "60m", "--loop-config", ".agent/loop.yaml"}, "./s.yaml", []string{"frontier"}, 1, 1, time.Hour, ".agent/loop.yaml", ""},
		// A run needs an explicit --timeout covering preparation, trials and grading.
		{"no timeout", []string{"./s.yaml", "codex"}, "", nil, 0, 0, 0, "", "Missing --timeout"},
		{"dry run still needs timeout", []string{"./s.yaml", "codex", "--dry-run"}, "", nil, 0, 0, 0, "", "Missing --timeout"},
		{"missing flag value", []string{"./s.yaml", "codex", "--jobs"}, "", nil, 0, 0, 0, "", `Missing value for "--jobs"`},
		{"flag is not loop config path", []string{"./s.yaml", "codex", "--loop-config", "--dry-run", "--timeout", "10m"}, "", nil, 0, 0, 0, "", `Missing value for "--loop-config"`},
		{"repeated flag", []string{"./s.yaml", "codex", "--jobs", "2", "--jobs", "3", "--timeout", "10m"}, "", nil, 0, 0, 0, "", `Option "--jobs" can only be used once`},
		{"zero jobs", []string{"./s.yaml", "codex", "--jobs", "0", "--timeout", "10m"}, "", nil, 0, 0, 0, "", `Invalid value "0" for "--jobs"`},
		{"non-number repeat", []string{"./s.yaml", "codex", "--repeat", "lots", "--timeout", "10m"}, "", nil, 0, 0, 0, "", `Invalid value "lots" for "--repeat"`},
		{"bad timeout", []string{"./s.yaml", "codex", "--timeout", "soon"}, "", nil, 0, 0, 0, "", `Invalid value "soon" for "--timeout"`},
		{"unknown flag", []string{"./s.yaml", "codex", "--turbo", "--timeout", "10m"}, "", nil, 0, 0, 0, "", `Unknown option "--turbo"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			suite, configs, opts, err := parseEvalRunArgs(c.args)
			if (err != nil) != (c.wantErr != "") {
				t.Fatalf("err = %v, wantErr %q", err, c.wantErr)
			}
			if c.wantErr != "" {
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %q, want %q", err, c.wantErr)
				}
				return
			}
			if suite != c.wantSuite || strings.Join(configs, ",") != strings.Join(c.wantConfigs, ",") ||
				opts.Jobs != c.wantJobs || opts.Repeat != c.wantRepeat || opts.Timeout != c.wantTimeout ||
				opts.LoopConfigOverride != c.wantLoop {
				t.Errorf("parse(%v) = suite %q configs %v jobs %d repeat %d timeout %s loop %q",
					c.args, suite, configs, opts.Jobs, opts.Repeat, opts.Timeout, opts.LoopConfigOverride)
			}
		})
	}
}

func TestEvalCompareNamesTheMissingOrExtraRunID(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing before", nil, "Missing before run ID"},
		{"missing after", []string{"before"}, "Missing after run ID"},
		{"extra", []string{"before", "after", "third"}, `Unexpected argument "third"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, err := (&app{}).evalCompare(tc.args)
			if code != 2 || err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "coop eval compare <before-id> <after-id>") {
				t.Errorf("evalCompare(%q) = (%d, %v), want input refusal naming %q and the usage", tc.args, code, err, tc.want)
			}
		})
	}
}

func TestRenderEvalPlanSeparatesSections(t *testing.T) {
	p := &eval.Plan{
		Suite: &eval.Suite{Version: 1, Name: "demo", Runner: eval.RunnerAgent,
			Cases: []eval.Case{{ID: "case-one", Timeout: eval.Duration(10 * time.Minute)}}},
		Configs: []eval.Configuration{{Kind: eval.ConfigTarget, Label: "codex"}},
		Repeat:  1, Jobs: 1, Timeout: 35 * time.Minute,
	}
	frozen := []eval.FrozenConfig{{Kind: eval.ConfigTarget, Label: "codex",
		Build: eval.BuildIdentity{Version: "test-build"}}}
	out := captureStdout(t, func() { renderEvalPlan(p, frozen) })
	for _, boundary := range []string{
		"[workload " + eval.WorkloadFingerprint(p.Suite).Short() + "]\n\nConfigurations:\n",
		"build test-build]\n\nMatrix:",
		"from command start (stops preparation, trials and grading)\n\nIsolation:",
		"omitted from trials\n\nCases:\n",
	} {
		if !strings.Contains(out, boundary) {
			t.Errorf("plan lacks section boundary %q:\n%s", boundary, out)
		}
	}
}

func TestEvalRunExitCodeRequiresGradingCoverage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		counts map[eval.TrialStatus]int
		want   int
	}{
		{"all passed", map[eval.TrialStatus]int{eval.TrialPassed: 3}, 0},
		{"graded failure", map[eval.TrialStatus]int{eval.TrialPassed: 2, eval.TrialFailed: 1}, 0},
		{"all execution errors", map[eval.TrialStatus]int{eval.TrialError: 3}, 1},
		{"execution error", map[eval.TrialStatus]int{eval.TrialPassed: 2, eval.TrialError: 1}, 1},
		{"timeout", map[eval.TrialStatus]int{eval.TrialPassed: 2, eval.TrialTimedOut: 1}, 1},
		{"pending", map[eval.TrialStatus]int{eval.TrialPassed: 2, eval.TrialPending: 1}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := evalRunExitCode(eval.RunSummary{Requested: 3, Counts: tc.counts}); got != tc.want {
				t.Fatalf("exit code = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestEvalHelpRunExamplesParse(t *testing.T) {
	examples, dryRuns := 0, 0
	for _, line := range strings.Split(commandHelp["eval"]+commandHelp["eval run"]+commandHelp["eval init"], "\n") {
		if !strings.HasPrefix(line, " ") {
			continue // a page's unindented title is not an executable example
		}
		args, ok := strings.CutPrefix(strings.TrimSpace(line), "coop eval run ")
		if !ok || strings.Contains(args, "<") {
			continue // the command index is syntax, not a runnable example
		}
		examples++
		_, _, opts, err := parseEvalRunArgs(strings.Fields(args))
		if err != nil {
			t.Errorf("documented example %q: %v", line, err)
		}
		if opts.DryRun {
			dryRuns++
		}
	}
	if examples == 0 || dryRuns == 0 {
		t.Fatal("eval help needs runnable examples, including a spend-free dry run")
	}
}

func TestEvalDiscoveryAndFocusedHelp(t *testing.T) {
	for _, cfg := range []*config.Config{freshConfig(t), signedInConfig(t)} {
		if out := helpText(cfg); !strings.Contains(out, "EVALUATIONS") || !strings.Contains(out, "coop eval") {
			t.Fatalf("eval is not discoverable from root help:\n%s", out)
		}
	}
	for _, verb := range evalCommands {
		page := "eval " + verb
		want, ok := commandHelp[page]
		if !ok || !strings.Contains(want, "Usage: coop "+page) {
			t.Errorf("missing focused help for %s", page)
			continue
		}
		out := captureStdout(t, func() {
			if code, err := helpForPath([]string{"eval", verb}, &config.Config{}, true); code != 0 || err != nil {
				t.Errorf("help %s: %d, %v", page, code, err)
			}
		})
		if !strings.Contains(out, want) {
			t.Errorf("leaf help did not route to %s", page)
		}
	}
	for _, path := range []string{"README.md", "site/docs.html"} {
		data, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"eval run core codex --timeout 35m --dry-run", "eval inspect", "eval compare", "eval init", "eval runs"} {
			if !strings.Contains(string(data), want) {
				t.Errorf("%s lacks eval workflow entry %q", path, want)
			}
		}
	}
}

func TestEvalStarterAndScaffoldHintsPreviewSupportedTargets(t *testing.T) {
	a := &app{}
	listing := captureStdout(t, func() {
		if code, err := a.evalList(nil); code != 0 || err != nil {
			t.Fatalf("ls = %d, %v", code, err)
		}
	})
	if !strings.Contains(listing, "coop eval run core codex --timeout 35m --dry-run") || strings.Contains(listing, "core <target|preset>") {
		t.Fatalf("starter hint is not a supported preview:\n%s", listing)
	}
	dir := filepath.Join(t.TempDir(), "my suite")
	out := captureStdout(t, func() {
		if code, err := a.evalInit([]string{dir}); code != 0 || err != nil {
			t.Fatalf("init = %d, %v", code, err)
		}
	})
	if !strings.Contains(out, "'"+filepath.Join(dir, "suite.yaml")+"' codex --timeout 12m --dry-run") || strings.Contains(out, "<target|preset>") {
		t.Fatalf("scaffold hint is not a copyable supported preview:\n%s", out)
	}
}

func TestEvalPreflightRefusesPresetBeforeRecordingAnAgentRun(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := evalTestRepo(t)
	writePresetFile(t, repo, "demo", "lead:\n  agent: [codex]\n")
	a := &app{cfg: &config.Config{RepoOverride: repo, RuntimeName: "coop-no-such-runtime-xyz"}}
	for _, dry := range []bool{true, false} {
		args := []string{"core", "demo", "--timeout", "35m"}
		if dry {
			args = append(args, "--dry-run")
		}
		out := captureStdout(t, func() {
			code, err := a.evalRun(args)
			if code != 1 || err == nil || !strings.Contains(err.Error(), "loop suites") {
				t.Errorf("unsupported agent preset (dry=%v): code %d, err %v", dry, code, err)
			}
		})
		if strings.Contains(out, "Suite:") {
			t.Errorf("unsupported configuration was advertised as executable:\n%s", out)
		}
	}
	root, err := evalStateRoot()
	if err != nil {
		t.Fatal(err)
	}
	if ids, err := eval.ListRuns(root); err != nil || len(ids) != 0 {
		t.Fatalf("refused input left run records: %v, %v", ids, err)
	}
}

func TestEvalDryRunShowsContentFrozenWorkloadWithoutLeavingARun(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	suite := trialSuite(t)
	a := &app{cfg: &config.Config{RepoOverride: t.TempDir()}}
	staged, err := eval.StageSuite(context.Background(), t.TempDir(), suite)
	if err != nil {
		t.Fatal(err)
	}
	want := eval.WorkloadFingerprint(staged).Short()
	out := captureStdout(t, func() {
		code, err := a.evalRun([]string{suite.Path, "codex", "--timeout", "10m", "--dry-run"})
		if code != 0 || err != nil {
			t.Fatalf("dry run: %d, %v", code, err)
		}
	})
	if !strings.Contains(out, "[workload "+want+"]") {
		t.Fatalf("dry run did not show the staged content hash %s:\n%s", want, out)
	}
	root, err := evalStateRoot()
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("dry run left staged inputs or a run: %v, %v", entries, err)
	}
	if err := os.WriteFile(filepath.Join(suite.Dir, "verifiers/hello/expected.txt"), []byte("new expectation"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := captureStdout(t, func() {
		code, err := a.evalRun([]string{suite.Path, "codex", "--timeout", "10m", "--dry-run"})
		if code != 0 || err != nil {
			t.Fatalf("second dry run: %d, %v", code, err)
		}
	})
	if strings.Contains(second, "[workload "+want+"]") {
		t.Fatal("an edited verifier kept the old dry-run workload identity")
	}
}

func TestEvalExpiredPreparationLeavesNoRunOrStagedInputs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	suite := trialSuite(t)
	a := &app{cfg: &config.Config{RepoOverride: t.TempDir()}}
	code, err := a.evalRun([]string{suite.Path, "codex", "--timeout", "1ns"})
	if code != 1 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired preparation = code %d, err %v", code, err)
	}
	root, err := evalStateRoot()
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(root); err != nil && !os.IsNotExist(err) || len(entries) != 0 {
		t.Fatalf("expired preparation left state: %v, %v", entries, err)
	}
}

// A bad target or a missing preset is refused during resolution, before any plan or provider work.
func TestResolveEvalConfigurationsRefusesBadPositionals(t *testing.T) {
	cfg := &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir()}
	a := &app{cfg: cfg}
	if _, err := a.resolveEvalConfigurations([]string{"codex:bad:model:extra"}); err == nil {
		t.Error("a malformed target was accepted")
	}
	if _, err := a.resolveEvalConfigurations([]string{"no-such-preset-xyz"}); err == nil {
		t.Error("a missing preset was accepted")
	}
	if err := os.MkdirAll(cfg.AgentProfileDir("codex", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := a.resolveEvalConfigurations([]string{"codex@work,missing"}); err == nil || !strings.Contains(err.Error(), `account "missing"`) {
		t.Errorf("a missing second account was not named: %v", err)
	}
	// A well-formed bare target resolves to a target configuration.
	configs, err := a.resolveEvalConfigurations([]string{"codex"})
	if err != nil {
		t.Fatalf("codex should resolve: %v", err)
	}
	if len(configs) != 1 || configs[0].Kind != "target" || configs[0].Label != "codex" {
		t.Errorf("codex resolved to %+v", configs)
	}
	if _, err := a.resolveEvalConfigurations([]string{"codex", "codex"}); err == nil || !strings.Contains(err.Error(), "--repeat") {
		t.Errorf("duplicate paid configuration was not refused with repetition guidance: %v", err)
	}
}
