package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

func canonicalTemp(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}
