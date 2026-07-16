//go:build providere2e

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

type directProviderContract struct {
	base            []string
	modelEnv        string
	effortEnv       string
	supportsEffort  bool
	effortFlag      func(string) []string
	commandOverride string
}

var directProviderContracts = map[string]directProviderContract{
	"claude": {
		base: []string{"claude", "--dangerously-skip-permissions"}, modelEnv: "ANTHROPIC_MODEL",
		effortEnv: "CLAUDE_CODE_EFFORT_LEVEL", supportsEffort: true,
		effortFlag:      func(level string) []string { return []string{"--effort", level} },
		commandOverride: "claude --dangerously-skip-permissions --model baked-claude --effort low",
	},
	"codex": {
		base: []string{"codex", "--dangerously-bypass-approvals-and-sandbox"}, supportsEffort: true,
		effortFlag:      func(level string) []string { return []string{"-c", "model_reasoning_effort=" + level} },
		commandOverride: "codex --dangerously-bypass-approvals-and-sandbox --model baked-codex -c model_reasoning_effort=low",
	},
	"gemini": {
		base: []string{"gemini", "--yolo"}, modelEnv: "GEMINI_MODEL",
		commandOverride: "gemini --yolo --model baked-gemini",
	},
	"grok": {
		base: []string{"grok", "--permission-mode", "bypassPermissions"}, supportsEffort: true,
		effortFlag:      func(level string) []string { return []string{"--reasoning-effort", level} },
		commandOverride: "grok --permission-mode bypassPermissions --model baked-grok --reasoning-effort low",
	},
}

type directProcessSuite struct {
	layout       procharness.Layout
	coopBin      string
	env          []string
	scenarioPath string
	providers    []string
	allCredKeys  []string
	repoHead     string
}

func TestProviderScriptedDirectMatrix(t *testing.T) {
	suite := newDirectProcessSuite(t)
	for _, provider := range suite.providers {
		provider := provider
		contract := directProviderContracts[provider]
		t.Run(provider, func(t *testing.T) {
			t.Run("marked default and configured precedence", func(t *testing.T) {
				result, trace := suite.run(t, []string{provider}, processScenario(provider, nil, 0, ""))
				assertDirectSuccess(t, result, provider, "fixture-ok-"+provider)
				assertDirectRunContract(t, suite, trace, provider, "personal", directExpectedArgv(contract, "env-"+provider, directDefaultEffort(contract), nil), "env-"+provider, directDefaultEffort(contract))
			})

			t.Run("explicit target account and forwarding", func(t *testing.T) {
				target := provider + ":target-" + provider
				if contract.supportsEffort {
					target += "/high"
				}
				target += "@work"
				extra := []string{"--fixture-flag", "fixture-value"}
				result, trace := suite.run(t, append([]string{target, "--"}, extra...), processScenario(provider, nil, 0, ""))
				assertDirectSuccess(t, result, provider, "fixture-ok-"+provider)
				assertDirectRunContract(t, suite, trace, provider, "work", directExpectedArgv(contract, "target-"+provider, directTargetEffort(contract), extra), "target-"+provider, directTargetEffort(contract))
			})

			if contract.supportsEffort {
				t.Run("effort only target", func(t *testing.T) {
					result, trace := suite.run(t, []string{provider + "/high@work"}, processScenario(provider, nil, 0, ""))
					assertDirectSuccess(t, result, provider, "fixture-ok-"+provider)
					assertDirectRunContract(t, suite, trace, provider, "work", directExpectedArgv(contract, "env-"+provider, "high", nil), "env-"+provider, "high")
				})
			}

			t.Run("exit propagation", func(t *testing.T) {
				result, trace := suite.run(t, []string{provider}, processScenario(provider, nil, 23, ""))
				if result.Err != nil || result.ExitCode != 23 {
					t.Fatalf("coop %s exit = %d, err %v\nstdout:\n%s\nstderr:\n%s", provider, result.ExitCode, result.Err, result.Stdout, result.Stderr)
				}
				for _, source := range []string{"provider", "runtime"} {
					event := oneProcessEvent(t, trace, source, "exit")
					if event.ExitCode == nil || *event.ExitCode != 23 {
						t.Fatalf("%s exit trace = %#v", source, event)
					}
				}
			})

			t.Run("cancellation", func(t *testing.T) {
				suite.cancelReadyProvider(t, provider)
			})

			for _, suffix := range []string{"@ghost", "@work,personal"} {
				suffix := suffix
				t.Run("reject "+strings.TrimPrefix(suffix, "@"), func(t *testing.T) {
					result, trace := suite.run(t, []string{provider + suffix}, processScenario(provider, nil, 0, ""))
					if result.ExitCode != 2 || result.Err != nil || len(trace) != 0 {
						t.Fatalf("coop %s%s = exit %d err %v trace %d\n%s", provider, suffix, result.ExitCode, result.Err, len(trace), result.Stderr)
					}
				})
			}
		})
	}

	t.Run("gemini effort fails before runtime", func(t *testing.T) {
		result, trace := suite.run(t, []string{"gemini/high@work"}, processScenario("gemini", nil, 0, ""))
		if result.ExitCode != 2 || result.Err != nil || len(trace) != 0 || !strings.Contains(result.Stderr, "no reasoning-effort control") {
			t.Fatalf("gemini effort rejection = exit %d err %v trace %d\nstderr:\n%s", result.ExitCode, result.Err, len(trace), result.Stderr)
		}
	})

	t.Run("malformed target fails before runtime", func(t *testing.T) {
		result, trace := suite.run(t, []string{"claude:"}, processScenario("claude", nil, 0, ""))
		if result.ExitCode != 2 || result.Err != nil || len(trace) != 0 || !strings.Contains(result.Stderr, "empty model") {
			t.Fatalf("malformed target rejection = exit %d err %v trace %d\nstderr:\n%s", result.ExitCode, result.Err, len(trace), result.Stderr)
		}
	})

	t.Run("plain output passes through", func(t *testing.T) {
		output := "plain-provider-bytes"
		result, _ := suite.run(t, []string{"codex"}, processScenario("codex", &output, 0, ""))
		if result.Err != nil || result.ExitCode != 0 || result.Stdout != output {
			t.Fatalf("plain output = exit %d err %v stdout %q", result.ExitCode, result.Err, result.Stdout)
		}
	})

	t.Run("silent success stays silent", func(t *testing.T) {
		output := ""
		result, _ := suite.run(t, []string{"grok"}, processScenario("grok", &output, 0, ""))
		if result.Err != nil || result.ExitCode != 0 || result.Stdout != "" {
			t.Fatalf("silent output = exit %d err %v stdout %q", result.ExitCode, result.Err, result.Stdout)
		}
	})
}

func newDirectProcessSuite(t *testing.T) *directProcessSuite {
	t.Helper()
	providers := agents.Names()
	for _, provider := range providers {
		contract, ok := directProviderContracts[provider]
		if !ok {
			t.Fatalf("registered provider %q has no direct process contract", provider)
		}
		ag, _ := agents.Get(provider)
		if len(contract.base) == 0 || contract.commandOverride == "" {
			t.Fatalf("provider %q has an incomplete argv contract", provider)
		}
		if contract.modelEnv != ag.ModelEnv() || contract.effortEnv != ag.EffortEnv() {
			t.Fatalf("provider %q env contract = model %q effort %q, adapter = %q/%q", provider, contract.modelEnv, contract.effortEnv, ag.ModelEnv(), ag.EffortEnv())
		}
		if contract.supportsEffort != agents.SupportsEffort(ag) || contract.supportsEffort != (contract.effortFlag != nil) {
			t.Fatalf("provider %q effort capability contract is incomplete", provider)
		}
		if contract.supportsEffort && !reflect.DeepEqual(contract.effortFlag("probe"), ag.Effort().Args("probe")) {
			t.Fatalf("provider %q effort argv contract = %q, adapter = %q", provider, contract.effortFlag("probe"), ag.Effort().Args("probe"))
		}
	}
	if len(providers) != len(directProviderContracts) {
		t.Fatalf("direct contracts = %v, providers = %v", sortedKeys(directProviderContracts), providers)
	}
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	moduleRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	moduleRoot, err = filepath.EvalSymlinks(moduleRoot)
	if err != nil {
		t.Fatal(err)
	}
	coopBin := filepath.Join(layout.Bin, "coop")
	fixtureBin := filepath.Join(layout.Bin, "providerfixture")
	buildProcessBinary(t, moduleRoot, coopBin, ".")
	buildProcessBinary(t, moduleRoot, fixtureBin, "./internal/cli/testdata/providerfixture")
	for _, alias := range append(append([]string(nil), providers...), "timeout", "flock", "setsid") {
		if err := os.Link(fixtureBin, filepath.Join(layout.Bin, alias)); err != nil {
			t.Fatalf("create fixed provider fixture alias %s: %v", alias, err)
		}
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("git is required for the scripted process suite")
	}
	jqBin, err := exec.LookPath("jq")
	if err != nil {
		t.Fatal("jq is required for the scripted consult process suite")
	}
	shBin, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal("sh is required for the scripted consult process suite")
	}

	var defaults, conf, envFile strings.Builder
	credentialSet := map[string]bool{}
	for _, provider := range providers {
		ag, _ := agents.Get(provider)
		marker, _ := ag.AuthMarker()
		for _, account := range []string{"default", "personal", "work"} {
			profile := filepath.Join(layout.Config, provider, "profiles", account)
			if err := os.MkdirAll(profile, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(profile, marker), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Fprintf(&defaults, "%s=personal\n", provider)
		fmt.Fprintf(&conf, "COOP_%s_MODEL=env-%s", strings.ToUpper(provider), provider)
		if directProviderContracts[provider].supportsEffort {
			conf.WriteString("/medium")
		}
		conf.WriteByte('\n')
		fmt.Fprintf(&conf, "COOP_%s_CMD=%s\n", strings.ToUpper(provider), directProviderContracts[provider].commandOverride)
		for _, key := range ag.CredentialEnvKeys() {
			credentialSet[key] = true
		}
	}
	if err := os.WriteFile(filepath.Join(layout.Config, "defaults"), []byte(defaults.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.Config, "missing.conf"), []byte(conf.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	allCredKeys := sortedKeys(credentialSet)
	envFile.WriteString("FIXTURE_SAFE=visible\n")
	envFile.WriteString("COOP_CONSULT_TIMEOUT=2\n")
	for _, key := range allCredKeys {
		fmt.Fprintf(&envFile, "%s=must-be-stripped\n", key)
	}
	if err := os.WriteFile(filepath.Join(layout.Config, "env"), []byte(envFile.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	scenarioPath := filepath.Join(layout.Plans, "direct.json")
	if err := os.WriteFile(scenarioPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := procharness.Environment(layout, map[string]string{
		"PATH": controlledPath(layout.Bin, filepath.Dir(gitBin), filepath.Dir(jqBin), filepath.Dir(shBin)), "COOP_RUNTIME": fixtureBin,
		"COOP_IMAGE": "fixture-image", "COOP_HOMES": "1", "COOP_PROVIDER_FIXTURE_ROOT": layout.Root,
		"COOP_PROVIDER_FIXTURE_IMAGE": "fixture-image", "COOP_PROVIDER_FIXTURE_TRACE": layout.Trace,
		"COOP_PROVIDER_FIXTURE_SCENARIO": scenarioPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	initProcessRepo(t, gitBin, layout.Repo, env)
	head := exec.Command(gitBin, "rev-parse", "HEAD")
	head.Dir, head.Env = layout.Repo, env
	headOut, err := head.Output()
	if err != nil {
		t.Fatal(err)
	}
	return &directProcessSuite{layout: layout, coopBin: coopBin, env: env, scenarioPath: scenarioPath, providers: providers, allCredKeys: allCredKeys, repoHead: strings.TrimSpace(string(headOut))}
}

func (s *directProcessSuite) run(t *testing.T, args []string, scenario any) (procharness.Result, []*processTrace) {
	t.Helper()
	s.reset(t, scenario)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result := procharness.Run(ctx, procharness.Command{
		Path: s.coopBin, Args: args, Dir: s.layout.Repo, Env: s.env,
		MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
	})
	trace := readProcessTrace(t, s.layout.Trace)
	if t.Failed() {
		t.Logf("scripted process trace:\n%s", readProcessFile(t, s.layout.Trace))
	}
	return result, trace
}

func (s *directProcessSuite) reset(t *testing.T, scenario any) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(s.layout.State, "runtime-containers")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(s.layout.Tmp, "coop-consult-state")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.layout.Trace, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.layout.State, "loop-cursor.json"), []byte("{\"index\":0}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(scenario)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.scenarioPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (s *directProcessSuite) cancelReadyProvider(t *testing.T, provider string) {
	t.Helper()
	s.reset(t, processScenario(provider, nil, 0, "wait"))
	process, err := procharness.Start(procharness.Command{
		Path: s.coopBin, Args: []string{provider}, Dir: s.layout.Repo, Env: s.env,
		MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer process.Cleanup()
	ready := awaitProcessEvent(t, s.layout.Trace, "provider", "ready", 5*time.Second)
	if err := process.SignalGroup(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	exit := awaitProcessEvent(t, s.layout.Trace, "provider", "exit", 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := process.Wait(ctx)
	if result.Err != nil || result.ExitCode == 0 {
		t.Fatalf("canceled coop %s = exit %d err %v\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", provider, result.ExitCode, result.Err, result.Stdout, result.Stderr, readProcessFile(t, s.layout.Trace))
	}
	if ready.PID != exit.PID || exit.Signal != syscall.SIGINT.String() || exit.ExitCode == nil || *exit.ExitCode != 130 {
		t.Fatalf("provider cancellation = ready pid %d, exit %#v", ready.PID, exit)
	}
	pids := map[int]bool{process.PID(): true}
	for _, event := range readProcessTrace(t, s.layout.Trace) {
		pids[event.PID] = true
	}
	for pid := range pids {
		awaitProcessGone(t, pid)
	}
}

func processScenario(provider string, output *string, exitCode int, behavior string) map[string]any {
	scenario := map[string]any{
		"version": 1, "provider": provider, "provider_homes": agents.Names(), "exit_code": exitCode,
	}
	if output != nil {
		scenario["output"] = *output
	}
	if behavior != "" {
		scenario["behavior"] = behavior
	}
	return scenario
}

func directExpectedArgv(contract directProviderContract, model, effort string, extra []string) []string {
	argv := append([]string(nil), contract.base...)
	if model != "" {
		argv = append(argv, "--model", model)
	}
	if effort != "" {
		argv = append(argv, contract.effortFlag(effort)...)
	}
	return append(argv, extra...)
}

func directDefaultEffort(contract directProviderContract) string {
	if contract.supportsEffort {
		return "medium"
	}
	return ""
}

func directTargetEffort(contract directProviderContract) string {
	if contract.supportsEffort {
		return "high"
	}
	return ""
}

func assertDirectSuccess(t *testing.T, result procharness.Result, provider, marker string) {
	t.Helper()
	if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, marker) {
		t.Fatalf("coop %s = exit %d err %v\nstdout:\n%s\nstderr:\n%s", provider, result.ExitCode, result.Err, result.Stdout, result.Stderr)
	}
}

func assertDirectRunContract(t *testing.T, suite *directProcessSuite, trace []*processTrace, provider, account string, argv []string, model, effort string) {
	t.Helper()
	assertSequentialTrace(t, trace)
	assertDirectRuntimeInvocations(t, trace)
	run := oneProcessEvent(t, trace, "runtime", "run")
	wantArgv := processTraceArgv(argv)
	if run.Run == nil || run.Run.Provider != provider || !reflect.DeepEqual(run.Run.ProviderArgv, wantArgv) {
		t.Fatalf("runtime provider contract = %#v, want %s %q", run.Run, provider, wantArgv)
	}
	assertProcessMounts(t, suite.layout, provider, account, run.Run.Mounts)
	assertDirectEnvironment(t, run.Run.Environment, suite.allCredKeys, provider, directProviderContracts[provider], model, effort)
	start := oneProcessEvent(t, trace, "provider", "start")
	if !reflect.DeepEqual(start.Argv, wantArgv) {
		t.Fatalf("provider argv = %q, want %q", start.Argv, wantArgv)
	}
	assertDirectEnvironment(t, start.Environment, suite.allCredKeys, provider, directProviderContracts[provider], model, effort)
}

func assertDirectEnvironment(t *testing.T, env []processEnv, credentialKeys []string, provider string, contract directProviderContract, model, effort string) {
	t.Helper()
	values := map[string]processEnv{}
	for _, item := range env {
		values[item.Name] = item
	}
	if values["COOP_PRIMARY"].Value != provider || values["FIXTURE_SAFE"].Value != "visible" {
		t.Fatalf("direct environment primary/safe = %#v / %#v", values["COOP_PRIMARY"], values["FIXTURE_SAFE"])
	}
	for _, key := range credentialKeys {
		if _, ok := values[key]; ok {
			t.Fatalf("direct environment leaked credential key %s", key)
		}
	}
	if contract.modelEnv != "" && values[contract.modelEnv].Value != model {
		t.Fatalf("%s = %#v, want %q", contract.modelEnv, values[contract.modelEnv], model)
	}
	if contract.effortEnv != "" && values[contract.effortEnv].Value != effort {
		t.Fatalf("%s = %#v, want %q", contract.effortEnv, values[contract.effortEnv], effort)
	}
}

func awaitProcessEvent(t *testing.T, path, source, event string, timeout time.Duration) *processTrace {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if line == "" {
					continue
				}
				var record processTrace
				if json.Unmarshal([]byte(line), &record) == nil && record.Source == source && record.Event == event {
					return &record
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s/%s\ntrace:\n%s", source, event, readProcessFile(t, path))
	return nil
}
