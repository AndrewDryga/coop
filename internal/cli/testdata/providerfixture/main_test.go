package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/fusion"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func TestParseRuntimeAcceptsTheNarrowRunDialect(t *testing.T) {
	root := canonicalTemp(t)
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"run", "--rm", "--label", "coop=box", "-e", "TZ=UTC",
		"-v", repo + ":" + repo, "--network", "none", "-w", repo,
		"fixture-image", "claude", "--dangerously-skip-permissions",
	}
	got, err := parseRuntime(root, "fixture-image", args)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "run" || got.Run.Image != "fixture-image" || got.Run.Workdir != repo {
		t.Fatalf("parsed run = %#v", got)
	}
	if len(got.Run.Mounts) != 1 || got.Run.Mounts[0].Source != repo || got.Run.Mounts[0].ReadOnly {
		t.Fatalf("parsed mounts = %#v", got.Run.Mounts)
	}
	wantTail := []string{"claude", "--dangerously-skip-permissions"}
	if strings.Join(got.Run.ProviderArgv, "\x00") != strings.Join(wantTail, "\x00") {
		t.Fatalf("provider argv = %q, want %q", got.Run.ProviderArgv, wantTail)
	}
}

func TestParseRuntimeKeepsLogicalProviderSeparateFromExecutable(t *testing.T) {
	root := canonicalTemp(t)
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{"run", "-v", repo + ":" + repo, "-w", repo, "fixture-image", "qwen-code", "--yolo"}
	got, err := parseRuntimeForProvider(root, "fixture-image", args, "qwen")
	if err != nil {
		t.Fatal(err)
	}
	if got.Run.Provider != "qwen" || !reflect.DeepEqual(got.Run.ProviderArgv, []string{"qwen-code", "--yolo"}) {
		t.Fatalf("logical provider/executable = %q/%q", got.Run.Provider, got.Run.ProviderArgv)
	}
}

func TestParseRuntimeRejectsUnknownSyntaxAndImageConfusion(t *testing.T) {
	root := canonicalTemp(t)
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := [][]string{
		{"pull", "fixture-image"},
		{"run", "--privileged", "fixture-image", "claude"},
		{"run", "fixture-image", "claude", "fixture-image"},
		{"run", "other-image", "claude"},
		{"run", "--label", "fixture-image", "claude"},
		{"run", "fixture-image", "/bin/sh", "-c", "id"},
	}
	for _, args := range cases {
		if _, err := parseRuntime(root, "fixture-image", args); err == nil {
			t.Errorf("parseRuntime(%q) succeeded", args)
		}
	}
}

func TestParseRuntimeRejectsEscapedAndSymlinkMountSources(t *testing.T) {
	root := canonicalTemp(t)
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := canonicalTemp(t)
	link := filepath.Join(root, "repo-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{outside, link} {
		args := []string{"run", "-v", source + ":" + repo, "-w", repo, "fixture-image", "codex"}
		if _, err := parseRuntime(root, "fixture-image", args); err == nil {
			t.Errorf("mount source %q was accepted", source)
		}
	}
}

func TestParseRuntimeSupportsOnlyExplicitLifecycleProbes(t *testing.T) {
	root := canonicalTemp(t)
	cases := []struct {
		args []string
		kind string
	}{
		{[]string{"--version"}, "version"},
		{[]string{"info"}, "info"},
		{[]string{"image", "inspect", "fixture-image"}, "inspect"},
		{[]string{"ps", "-q", "--filter", "label=coop=box"}, "ps"},
		{[]string{"rm", "-f", "fixture-id"}, "remove"},
		{[]string{"kill", "fixture-id"}, "kill"},
	}
	for _, tc := range cases {
		got, err := parseRuntime(root, "fixture-image", tc.args)
		if err != nil || got.Kind != tc.kind {
			t.Errorf("parseRuntime(%q) = (%q, %v), want (%q, nil)", tc.args, got.Kind, err, tc.kind)
		}
	}
	for _, args := range [][]string{
		{"--version", "extra"},
		{"info", "extra"},
		{"image", "inspect", "other-image"},
		{"ps", "--all"},
		{"rm", "-f"},
		{"kill", "--all"},
	} {
		if _, err := parseRuntime(root, "fixture-image", args); err == nil {
			t.Errorf("parseRuntime(%q) accepted unsupported lifecycle syntax", args)
		}
	}
}

func TestParseRuntimeReadsOnlyRootOwnedEnvFiles(t *testing.T) {
	root := canonicalTemp(t)
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(root, "env")
	if err := os.WriteFile(envFile, []byte("SAFE=one\n# comment\nSAFE=two\nOPENAI_API_KEY=fake\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"run", "--env-file", envFile, "-v", repo + ":" + repo, "-w", repo, "fixture-image", "codex"}
	got, err := parseRuntime(root, "fixture-image", args)
	if err != nil {
		t.Fatal(err)
	}
	if got.Run.Env["SAFE"] != "two" || got.Run.Env["OPENAI_API_KEY"] != "fake" || !reflect.DeepEqual(got.Run.EnvFiles, []string{envFile}) {
		t.Fatalf("parsed env file = env %#v files %#v", got.Run.Env, got.Run.EnvFiles)
	}
	outside := filepath.Join(canonicalTemp(t), "env")
	if err := os.WriteFile(outside, []byte("SAFE=no\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args[2] = outside
	if _, err := parseRuntime(root, "fixture-image", args); err == nil {
		t.Fatal("outside env file was accepted")
	}
}

func TestParseRuntimeRejectsWorkdirTraversalAndUnsafeMountPolicy(t *testing.T) {
	root := canonicalTemp(t)
	repo := filepath.Join(root, "repo")
	state := filepath.Join(root, "state")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := [][]string{
		{"run", "-v", repo + ":/workspace", "-w", "/workspace/../state", "fixture-image", "future-provider"},
		{"run", "-v", repo + ":/workspace:ro", "-w", "/workspace", "fixture-image", "future-provider"},
		{"run", "-v", repo + ":/workspace", "-v", state + ":/etc/fixture", "-w", "/workspace", "fixture-image", "future-provider"},
		{"run", "-v", repo + ":/workspace", "-v", state + ":/home/node/.future-provider/config", "-w", "/workspace", "fixture-image", "future-provider"},
		{"run", "-v", repo + ":/workspace", "-v", state + ":/home/node/.future-provider/config:ro", "-w", "/workspace", "fixture-image", "future-provider"},
		{"run", "-v", repo + ":/workspace", "-v", "coop-cache:/home/node/.cache:ro", "-w", "/workspace", "fixture-image", "future-provider"},
	}
	for _, args := range cases {
		if _, err := parseRuntime(root, "fixture-image", args); err == nil {
			t.Errorf("parseRuntime accepted unsafe workdir/mount policy: %q", args)
		}
	}
}

func TestParseRuntimeScopesGeneratedConfigsToScenarioProviderHomes(t *testing.T) {
	root := canonicalTemp(t)
	repo := filepath.Join(root, "repo")
	tmp := filepath.Join(root, "tmp")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(tmp, "coop-mcp-contract")
	if err := os.WriteFile(generated, []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := func(target string) []string {
		return []string{
			"run", "-v", repo + ":/workspace", "-v", generated + ":" + target + ":ro",
			"-w", "/workspace", "fixture-image", "future-provider",
		}
	}
	if _, err := parseRuntime(root, "fixture-image", args("/home/node/.future-provider/AGENTS.md"), "future-provider"); err != nil {
		t.Fatalf("scenario-declared provider home was rejected: %v", err)
	}
	for _, target := range []string{"/home/node/.ssh/config", "/home/node/.other-provider/AGENTS.md"} {
		if _, err := parseRuntime(root, "fixture-image", args(target), "future-provider"); err == nil {
			t.Errorf("undeclared generated config target %q was accepted", target)
		}
	}
}

func TestParseRuntimeAcceptsOnlyCurrentGeneratedGitTargets(t *testing.T) {
	root := canonicalTemp(t)
	repo := filepath.Join(root, "repo")
	tmp := filepath.Join(root, "tmp")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	configSource := filepath.Join(tmp, "coop-mcp-git")
	if err := os.WriteFile(configSource, []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hooksSource := filepath.Join(tmp, "coop-githooks-contract")
	if err := os.Mkdir(hooksSource, 0o700); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		source string
		target string
		wantOK bool
	}{
		{"current ignore", configSource, "/home/node/.coop-gitignore", true},
		{"old nested ignore", configSource, "/home/node/.config/git/ignore", false},
		{"current hooks", hooksSource, "/home/node/.coop-git-hooks", true},
		{"old nested hooks", hooksSource, "/home/node/.config/coop/git-hooks", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{
				"run", "-v", repo + ":/workspace", "-v", tc.source + ":" + tc.target + ":ro",
				"-w", "/workspace", "fixture-image", "future-provider",
			}
			_, err := parseRuntime(root, "fixture-image", args, "future-provider")
			if (err == nil) != tc.wantOK {
				t.Fatalf("parseRuntime error = %v, want accepted %v", err, tc.wantOK)
			}
		})
	}
}

func TestParseRuntimeRejectsDuplicateTTYSemantics(t *testing.T) {
	root := canonicalTemp(t)
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{{"-i", "-it"}, {"-t", "-it"}, {"-it", "-i"}, {"-it", "-t"}} {
		args := append([]string{"run"}, flags...)
		args = append(args, "-v", repo+":"+repo, "-w", repo, "fixture-image", "future-provider")
		if _, err := parseRuntime(root, "fixture-image", args); err == nil {
			t.Errorf("parseRuntime accepted duplicate TTY semantics: %q", flags)
		}
	}
}

func TestTraceRedactsRawAndParsedEnvironmentByDefault(t *testing.T) {
	root := canonicalTemp(t)
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := []string{"run", "--label", "run=label-secret", "-e", "DATABASE_URL=postgres://secret", "-e", "COOP_PRIMARY=future-provider", "-v", repo + ":" + repo, "-w", repo, "fixture-image", "future-provider", "--prompt", "provider-secret"}
	got := traceRuntimeArgv(root, "fixture-image", raw)
	joined := strings.Join(got, " ")
	for _, secret := range []string{"label-secret", "postgres://secret", "provider-secret", root} {
		if strings.Contains(joined, secret) {
			t.Fatalf("sanitized runtime argv leaked %q: %q", secret, got)
		}
	}
	if !strings.Contains(joined, "DATABASE_URL=<redacted>") || !strings.Contains(joined, "<root>/repo") || !strings.Contains(joined, traceValue("provider-secret")) {
		t.Fatalf("sanitized runtime argv = %q", got)
	}
	env := traceEnvironment(map[string]string{"DATABASE_URL": "postgres://secret", "COOP_PRIMARY": "future-provider", "FIXTURE_SAFE": "visible"})
	values := map[string]envTrace{}
	for _, item := range env {
		values[item.Name] = item
	}
	if !values["DATABASE_URL"].Redacted || values["DATABASE_URL"].Value != "" {
		t.Fatalf("DATABASE_URL trace = %#v", values["DATABASE_URL"])
	}
	if values["COOP_PRIMARY"].Value != "future-provider" || values["FIXTURE_SAFE"].Value != "visible" {
		t.Fatalf("safe trace values = %#v", values)
	}
	structured := traceRunCommand(root, runCommand{
		Image: "fixture-image", Workdir: repo, HostWorkdir: repo, Provider: "future-provider",
		ProviderArgv: []string{"future-provider", "--prompt", "provider-secret"},
		Mounts:       []mount{{Source: repo, Target: repo}},
		EnvFiles:     []string{filepath.Join(root, "env-secret")},
		Labels:       []string{"run=label-secret"}, Network: "network-secret",
	})
	encoded, err := json.Marshal(structured)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{root, "provider-secret", "label-secret", "network-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("structured trace leaked %q: %s", secret, encoded)
		}
	}
}

func TestTraceRuntimeArgvHandlesMalformedInputWithoutValues(t *testing.T) {
	root := canonicalTemp(t)
	for _, args := range [][]string{
		{"run", "-e"},
		{"run", "-e", "malformed-secret"},
		{"run", "--label"},
		{"run", "--unknown", "malformed-secret"},
		{"run", "-v", "malformed-secret:/home/node/x", "fixture-image", "future-provider"},
		{"run", "-v", "coop-cache:/home/node/.cache:malformed-secret", "fixture-image", "future-provider"},
		{"run", "fixture-image", "../malformed-secret"},
		{"pull", "malformed-secret"},
		{"--version", "malformed-secret"},
		{"info", "malformed-secret"},
	} {
		got := strings.Join(traceRuntimeArgv(root, "fixture-image", args), " ")
		if strings.Contains(got, "malformed-secret") {
			t.Errorf("malformed runtime trace leaked a value for %q: %q", args, got)
		}
	}
}

func TestProviderTokenIsRegistryNeutralAndNeverAPath(t *testing.T) {
	for _, value := range []string{"claude", "future-provider", "agent2"} {
		if got, err := providerToken(value); err != nil || got != value {
			t.Errorf("providerToken(%q) = (%q, %v)", value, got, err)
		}
	}
	for _, value := range []string{"", "/bin/sh", "../agent", "Upper", "bad_name", "-flag"} {
		if _, err := providerToken(value); err == nil {
			t.Errorf("providerToken(%q) succeeded", value)
		}
	}
}

func TestFixturePropagatesProviderExitCode(t *testing.T) {
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	moduleRoot, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(layout.Bin, "providerfixture")
	build := exec.Command("go", "build", "-trimpath", "-o", fixture, "./internal/cli/testdata/providerfixture")
	build.Dir = moduleRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
	scenario := filepath.Join(layout.Plans, "exit.json")
	if err := os.WriteFile(scenario, []byte(`{"version":1,"provider":"future-provider","exit_code":23}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := procharness.Environment(layout, map[string]string{
		"COOP_PROVIDER_FIXTURE_ROOT": layout.Root, "COOP_PROVIDER_FIXTURE_IMAGE": "fixture-image",
		"COOP_PROVIDER_FIXTURE_TRACE": layout.Trace, "COOP_PROVIDER_FIXTURE_SCENARIO": scenario,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := procharness.Run(ctx, procharness.Command{
		Path: fixture,
		Args: []string{"run", "-v", layout.Repo + ":" + layout.Repo, "-w", layout.Repo, "fixture-image", "future-provider"},
		Dir:  layout.Repo, Env: env, MaxOutput: 64 << 10,
	})
	if result.Err != nil || result.ExitCode != 23 {
		t.Fatalf("fixture exit = %d, %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
	}
	trace, err := os.ReadFile(layout.Trace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(trace), `"event":"error"`) || !strings.Contains(string(trace), `"exit_code":23`) {
		t.Fatalf("exit trace =\n%s", trace)
	}
}

func TestReadScenarioAcceptsOnlyClosedBoundedBehavior(t *testing.T) {
	root := canonicalTemp(t)
	path := filepath.Join(root, "scenario.json")
	write := func(body string) error {
		if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := readScenario(root, path)
		return err
	}
	for _, body := range []string{
		`{"version":1,"provider":"future-provider"}`,
		`{"version":1,"provider":"future-provider","behavior":"complete","output":"plain bytes"}`,
		`{"version":1,"provider":"future-provider","behavior":"wait"}`,
	} {
		if err := write(body); err != nil {
			t.Errorf("valid scenario rejected: %v\n%s", err, body)
		}
	}
	for _, body := range []string{
		`{"version":1,"provider":"future-provider","behavior":"shell"}`,
		`{"version":1,"provider":"future-provider","behavior":"wait","exit_code":3}`,
		`{"version":1,"provider":"future-provider","marker":"a","output":"b"}`,
		`{"version":1,"provider":"future-provider","marker":"line\nfeed"}`,
		`{"version":1,"provider":"future-provider","output":"line\nfeed"}`,
		`{"version":1,"provider":"future-provider","command":"sh -c id"}`,
		`{"version":1,"provider":"future-provider"} {"version":1,"provider":"future-provider"}`,
	} {
		if err := write(body); err == nil {
			t.Errorf("unsafe/open scenario accepted:\n%s", body)
		}
	}
	oversized := `{"version":1,"provider":"future-provider"}` + strings.Repeat(" ", 64<<10)
	if err := write(oversized); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized scenario error = %v, want explicit size rejection", err)
	}
}

func TestReadLoopScenarioAcceptsOnlyClosedV6Attempts(t *testing.T) {
	root := canonicalTemp(t)
	path := filepath.Join(root, "scenario.json")
	write := func(body string) error {
		t.Helper()
		if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := readScenario(root, path)
		return err
	}
	valid := `{"version":6,"provider":"codex","provider_homes":["codex"],"loop":{"task_id":"loop-task-codex","attempts":[{"target":"codex:loop-model@work","stage":"work","result":"complete"}]}}`
	if err := write(valid); err != nil {
		t.Fatalf("valid loop scenario rejected: %v", err)
	}
	for _, result := range []string{"complete-delay", "complete-gated", "complete-reopen-archive", "complete-extra-unbound", "complete-extra-bound", "complete-extra-finalized", "complete-wait", "unbound", "unbound-extra-finalized", "unbound-wait", "unbound-log-symlink", "unbound-state-symlink", "repair-binding", "rate-limit", "output-limit", "authentication", "ordinary", "ambiguous-limit-prose", "ambiguous-auth-prose", "malformed", "truncated", "wait"} {
		body := strings.Replace(valid, `"result":"complete"`, `"result":"`+result+`"`, 1)
		if err := write(body); err != nil {
			t.Fatalf("closed loop result %q rejected: %v", result, err)
		}
	}
	for _, stage := range []string{"between", "signoff", "verify"} {
		for _, result := range []string{"pass", "reopen", "reopen-ordinary", "rate-limit", "output-limit", "authentication", "ordinary", "malformed", "truncated", "wait"} {
			body := strings.Replace(valid, `"stage":"work"`, `"stage":"`+stage+`"`, 1)
			body = strings.Replace(body, `"result":"complete"`, `"result":"`+result+`"`, 1)
			if err := write(body); err != nil {
				t.Fatalf("closed %s result %q rejected: %v", stage, result, err)
			}
		}
	}
	for _, body := range []string{
		strings.Replace(valid, `"version":6`, `"version":5`, 1),
		strings.Replace(valid, `,"stage":"work"`, "", 1),
		strings.Replace(valid, `"stage":"work"`, `"stage":"preflight"`, 1),
		strings.Replace(valid, `"result":"complete"`, `"result":"pass"`, 1),
		strings.Replace(strings.Replace(valid, `"stage":"work"`, `"stage":"signoff"`, 1), `"result":"complete"`, `"result":"repair-binding"`, 1),
		strings.Replace(valid, `"task_id":"loop-task-codex"`, `"task_id":"../escape"`, 1),
		strings.Replace(valid, `"provider":"codex"`, `"provider":"claude"`, 1),
		strings.Replace(valid, `"target":"codex:loop-model@work"`, `"target":"claude:loop-model@work"`, 1),
		strings.Replace(valid, `"target":"codex:loop-model@work"`, `"target":"codex@work,personal"`, 1),
		strings.Replace(valid, `"attempts":[{"target":"codex:loop-model@work","stage":"work","result":"complete"}]`, `"attempts":[]`, 1),
		strings.TrimSuffix(valid, "}") + `,"command":"sh -c id"}`,
		strings.TrimSuffix(valid, "}") + `,"marker":"mixed"}`,
		strings.TrimSuffix(valid, "}") + `,"consult":{"calls":[],"steps":[]}}`,
		strings.Replace(valid, `"result":"complete"`, `"result":"shell"`, 1),
	} {
		if err := write(body); err == nil {
			t.Errorf("unsafe/open loop scenario accepted:\n%s", body)
		}
	}
}

func TestVerifyLoopPromptRequiresStageMarkerAndTask(t *testing.T) {
	taskID := "loop-task-codex"
	tests := []struct {
		stage, provider string
		argv            []string
	}{
		{"work", "codex", []string{"codex", "exec", "Work task " + taskID + ", already claimed in 10_in_progress/."}},
		{"between", "claude", []string{"claude", "-p", "FIXTURE BETWEEN " + taskID, "--output-format", "stream-json"}},
		{"signoff", "gemini", []string{"gemini", "-p", "FIXTURE SIGNOFF " + taskID}},
		{"verify", "grok", []string{"grok", "-p", "FIXTURE VERIFY " + taskID}},
	}
	for _, test := range tests {
		if err := verifyLoopPrompt(test.stage, taskID, test.provider, test.argv); err != nil {
			t.Errorf("valid %s prompt rejected: %v", test.stage, err)
		}
		withoutTask := slices.Clone(test.argv)
		withoutTask[len(withoutTask)-1] = strings.ReplaceAll(withoutTask[len(withoutTask)-1], taskID, "other-task")
		if test.provider == "claude" {
			withoutTask[2] = strings.ReplaceAll(withoutTask[2], taskID, "other-task")
		}
		if err := verifyLoopPrompt(test.stage, taskID, test.provider, withoutTask); err == nil {
			t.Errorf("%s prompt without task id accepted", test.stage)
		}
	}
	prompts := map[string]string{
		"work":    "Work task " + taskID + ", already claimed in 10_in_progress/.",
		"between": "FIXTURE BETWEEN " + taskID,
		"signoff": "FIXTURE SIGNOFF " + taskID,
		"verify":  "FIXTURE VERIFY " + taskID,
	}
	for expected := range prompts {
		for actual, prompt := range prompts {
			if expected == actual {
				continue
			}
			if err := verifyLoopPrompt(expected, taskID, "codex", []string{"codex", "exec", prompt}); err == nil {
				t.Errorf("%s prompt accepted as %s", actual, expected)
			}
		}
	}
}

func TestVerifyLoopLeaseRejectsUnheldAndMismatchedMetadata(t *testing.T) {
	root := canonicalTemp(t)
	task := filepath.Join(root, "task")
	tmp := filepath.Join(task, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(tmp, "lease.lock")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	meta := loopLeaseMetadata{
		Version: 1, RunID: "run", ControllerPID: os.Getpid(), Provider: "codex",
		Target: "codex:model@work", AcquiredAt: time.Now(), HeartbeatAt: time.Now(),
	}
	writeMeta := func() {
		t.Helper()
		data, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, "lease.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMeta()
	if err := verifyLoopLease(root, task, "codex", meta.Target); err == nil {
		t.Fatal("unheld loop lease accepted")
	}
	lock, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	meta.Provider = "claude"
	writeMeta()
	if err := verifyLoopLease(root, task, "codex", "codex:model@work"); err == nil {
		t.Fatal("mismatched loop lease metadata accepted")
	}
}

func TestReadConsultScenarioAcceptsOnlyClosedV2Plans(t *testing.T) {
	root := canonicalTemp(t)
	path := filepath.Join(root, "scenario.json")
	write := func(body string) error {
		t.Helper()
		if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := readScenario(root, path)
		return err
	}
	valid := `{"version":2,"provider":"claude","provider_homes":["claude","codex"],"consult":{"calls":[{"target":"codex","mode":"fresh","prompt":"question","exit_code":0}],"steps":[{"provider":"codex","delivery":"fresh","result":"usable","prompt":"question","reply":"answer","model":"fixture-model","effort":"high","usage_in":3,"usage_out":2}]}}`
	if err := write(valid); err != nil {
		t.Fatalf("valid consult scenario rejected: %v", err)
	}
	missingScope := `{"version":2,"provider":"claude","provider_homes":["claude"],"consult":{"calls":[{"target":"codex","mode":"fresh","prompt":"question","exit_code":2}],"steps":[]}}`
	if err := write(missingScope); err != nil {
		t.Fatalf("zero-dispatch consult scenario rejected: %v", err)
	}

	invalid := []string{
		strings.Replace(valid, `"version":2`, `"version":1`, 1),
		strings.Replace(valid, `"mode":"fresh"`, `"mode":"--fresh"`, 1),
		strings.Replace(valid, `"result":"usable"`, `"result":"command"`, 1),
		strings.Replace(valid, `"provider":"codex","delivery"`, `"provider":"claude","delivery"`, 1),
		strings.Replace(valid, `"provider_homes":["claude","codex"]`, `"provider_homes":["claude"]`, 1),
		strings.Replace(valid, `"reply":"answer"`, `"reply":""`, 1),
		strings.Replace(valid, `"usage_in":3`, `"usage_in":-1`, 1),
		strings.Replace(valid, `"consult":{`, `"marker":"direct","consult":{`, 1),
		strings.Replace(valid, `"usage_out":2`, `"usage_out":2,"command":"sh -c id"`, 1),
		strings.Replace(valid, `"consult":{`, `"consult":{"block_task":"../escape",`, 1),
	}
	for _, body := range invalid {
		if err := write(body); err == nil {
			t.Errorf("unsafe/open consult scenario accepted:\n%s", body)
		}
	}
}

func TestParseConsultInvocationPinsEveryAdapterGrammar(t *testing.T) {
	cases := []struct {
		provider string
		args     []string
		want     consultInvocation
	}{
		{"claude", []string{"-p", "--permission-mode", "plan", "--session-id", "session-1", "--model", "opus", "--effort", "high", "question"}, consultInvocation{Delivery: "fresh", Session: "session-1", Model: "opus", Effort: "high", Prompt: "question"}},
		{"gemini", []string{"--approval-mode", "plan", "--resume", "session-2", "--model", "flash", "-p", "follow-up"}, consultInvocation{Delivery: "resume", Session: "session-2", Model: "flash", Prompt: "follow-up"}},
		{"grok", []string{"--tools", "Read,Grep", "--session-id", "session-3", "--model", "grok-model", "--reasoning-effort", "xhigh", "-p", "question"}, consultInvocation{Delivery: "fresh", Session: "session-3", Model: "grok-model", Effort: "xhigh", Prompt: "question"}},
		{"codex", []string{"exec", "resume", "session-4", "-c", "sandbox_mode=read-only", "--model", "codex-model", "-c", "model_reasoning_effort=high", "--json", "follow-up"}, consultInvocation{Delivery: "resume", Session: "session-4", Model: "codex-model", Effort: "high", Prompt: "follow-up"}},
	}
	for _, tc := range cases {
		got, err := parseConsultInvocation(tc.provider, tc.args)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseConsultInvocation(%s) = (%#v, %v), want %#v", tc.provider, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		provider string
		args     []string
	}{
		{"claude", []string{"-p", "--permission-mode", "plan", "--model", "opus", "--session-id", "session", "question"}},
		{"gemini", []string{"--approval-mode", "plan", "--session-id", "session", "question", "-p"}},
		{"grok", []string{"--tools", "", "--session-id", "session", "-p", "question"}},
		{"codex", []string{"exec", "resume", "session", "--json", "question", "-c", "sandbox_mode=read-only"}},
	} {
		if _, err := parseConsultInvocation(tc.provider, tc.args); err == nil {
			t.Errorf("parseConsultInvocation(%s, %q) accepted reordered/unsafe argv", tc.provider, tc.args)
		}
	}
}

func TestValidateConsultWrapperMountRequiresExactPrivateGeneratedFile(t *testing.T) {
	root := canonicalTemp(t)
	tmp := filepath.Join(root, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tmp, "coop-mcp-wrapper")
	if err := os.WriteFile(path, []byte(fusion.ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	m := mount{Source: path, Target: fusion.ConsultWrapperPath, ReadOnly: true}
	if err := validateConsultWrapperMount(root, m); err != nil {
		t.Fatalf("exact consult wrapper rejected: %v", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateConsultWrapperMount(root, m); err == nil {
		t.Fatal("non-executable-for-all consult wrapper mode accepted")
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateConsultWrapperMount(root, m); err == nil {
		t.Fatal("wrong consult wrapper bytes accepted")
	}
	if err := os.WriteFile(path, []byte(fusion.ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(tmp, "second-link")); err != nil {
		t.Fatal(err)
	}
	if err := validateConsultWrapperMount(root, m); err == nil {
		t.Fatal("hardlinked consult wrapper accepted")
	}
}

func TestReadDelegateScenarioAcceptsOnlyClosedV3Plans(t *testing.T) {
	root := canonicalTemp(t)
	path := filepath.Join(root, "scenario.json")
	write := func(body string) error {
		t.Helper()
		if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := readScenario(root, path)
		return err
	}
	valid := `{"version":3,"provider":"claude","provider_homes":["claude","codex"],"delegate":{"contract":"closed contract","calls":[{"role":"worker","prompt":"question","exit_code":0}],"steps":[{"provider":"codex","result":"edit","prompt":"closed contract\n\n---\n\nYour task:\n\nquestion","model":"fixture-model","effort":"high"}]}}`
	if err := write(valid); err != nil {
		t.Fatalf("valid delegate scenario rejected: %v", err)
	}
	invalid := []string{
		strings.Replace(valid, `"version":3`, `"version":2`, 1),
		strings.Replace(valid, `"result":"edit"`, `"result":"command"`, 1),
		strings.Replace(valid, `"provider_homes":["claude","codex"]`, `"provider_homes":["claude"]`, 1),
		strings.Replace(valid, `"role":"worker"`, `"role":"../worker"`, 1),
		strings.Replace(valid, `"delegate":{`, `"marker":"direct","delegate":{`, 1),
		strings.Replace(valid, `"model":"fixture-model"`, `"model":"fixture-model","command":"sh -c id"`, 1),
		strings.Replace(valid, `"result":"edit"`, `"result":"consult"`, 1),
		strings.Replace(valid, `"steps":[`, `"consult":{"steps":[]},"steps":[`, 1),
	}
	for _, body := range invalid {
		if err := write(body); err == nil {
			t.Errorf("unsafe/open delegate scenario accepted:\n%s", body)
		}
	}
}

func TestParseDelegateInvocationPinsEveryAdapterGrammar(t *testing.T) {
	cases := []struct {
		provider string
		args     []string
		want     delegateInvocation
	}{
		{"claude", []string{"-p", "--dangerously-skip-permissions", "--model", "opus", "--effort", "high", "question"}, delegateInvocation{Prompt: "question", Model: "opus", Effort: "high"}},
		{"codex", []string{"exec", "--dangerously-bypass-approvals-and-sandbox", "--model", "codex-model", "-c", "model_reasoning_effort=high", "question"}, delegateInvocation{Prompt: "question", Model: "codex-model", Effort: "high"}},
		{"gemini", []string{"--yolo", "--model", "flash", "-p", "question"}, delegateInvocation{Prompt: "question", Model: "flash"}},
		{"grok", []string{"--permission-mode", "bypassPermissions", "--model", "grok-model", "--reasoning-effort", "xhigh", "-p", "question"}, delegateInvocation{Prompt: "question", Model: "grok-model", Effort: "xhigh"}},
	}
	for _, tc := range cases {
		got, err := parseDelegateInvocation(tc.provider, tc.args)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseDelegateInvocation(%s) = (%#v, %v), want %#v", tc.provider, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		provider string
		args     []string
	}{
		{"claude", []string{"-p", "--model", "opus", "--dangerously-skip-permissions", "question"}},
		{"codex", []string{"exec", "--model", "codex-model", "--dangerously-bypass-approvals-and-sandbox", "question"}},
		{"gemini", []string{"--yolo", "-p", "question", "--model", "flash"}},
		{"grok", []string{"--permission-mode", "plan", "-p", "question"}},
	} {
		if _, err := parseDelegateInvocation(tc.provider, tc.args); err == nil {
			t.Errorf("parseDelegateInvocation(%s, %q) accepted reordered/read-only argv", tc.provider, tc.args)
		}
	}
}

func TestValidateDelegateWrapperMountRequiresExactPrivateGeneratedFile(t *testing.T) {
	root := canonicalTemp(t)
	tmp := filepath.Join(root, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tmp, "coop-delegate-wrapper")
	if err := os.WriteFile(path, []byte(preset.DelegateWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	m := mount{Source: path, Target: preset.DelegateWrapperPath, ReadOnly: true}
	if err := validateDelegateWrapperMount(root, m); err != nil {
		t.Fatalf("exact delegate wrapper rejected: %v", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateDelegateWrapperMount(root, m); err == nil {
		t.Fatal("non-executable-for-all delegate wrapper mode accepted")
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateDelegateWrapperMount(root, m); err == nil {
		t.Fatal("wrong delegate wrapper bytes accepted")
	}
	if err := os.WriteFile(path, []byte(preset.DelegateWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(tmp, "delegate-second-link")); err != nil {
		t.Fatal(err)
	}
	if err := validateDelegateWrapperMount(root, m); err == nil {
		t.Fatal("hardlinked delegate wrapper accepted")
	}
}

func canonicalTemp(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}
