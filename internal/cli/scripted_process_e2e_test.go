//go:build providere2e

package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

type processTrace struct {
	Version     int                  `json:"version"`
	Sequence    int                  `json:"sequence"`
	Source      string               `json:"source"`
	Event       string               `json:"event"`
	PID         int                  `json:"pid"`
	Argv        []string             `json:"argv"`
	Cwd         string               `json:"cwd"`
	Run         *processRun          `json:"run"`
	Environment []processEnv         `json:"environment"`
	ExitCode    *int                 `json:"exit_code"`
	Signal      string               `json:"signal"`
	Consult     *processConsultTrace `json:"consult"`
}

type processConsultTrace struct {
	Step        int    `json:"step"`
	Provider    string `json:"provider"`
	Delivery    string `json:"delivery"`
	Result      string `json:"result"`
	Model       string `json:"model"`
	Effort      string `json:"effort"`
	SessionHash string `json:"session_hash"`
	PromptHash  string `json:"prompt_hash"`
}

type processRun struct {
	Image        string         `json:"image"`
	Workdir      string         `json:"workdir"`
	HostWorkdir  string         `json:"host_workdir"`
	Provider     string         `json:"provider"`
	ProviderArgv []string       `json:"provider_argv"`
	Mounts       []processMount `json:"mounts"`
	Environment  []processEnv   `json:"environment"`
	EnvFiles     []string       `json:"env_files"`
	Labels       []string       `json:"labels"`
	Network      string         `json:"network"`
	Interactive  bool           `json:"interactive"`
	TTY          bool           `json:"tty"`
}

type processMount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
	Named    bool   `json:"named"`
}

type processEnv struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Redacted bool   `json:"redacted"`
}

func TestProviderScriptedProcessSmoke(t *testing.T) {
	expectedArgv := map[string][]string{
		"claude": {"claude", "--dangerously-skip-permissions"},
		"codex":  {"codex", "--dangerously-bypass-approvals-and-sandbox"},
		"gemini": {"gemini", "--yolo"},
		"grok":   {"grok", "--permission-mode", "bypassPermissions"},
	}
	credentialKey := map[string]string{
		"claude": "ANTHROPIC_API_KEY", "codex": "OPENAI_API_KEY",
		"gemini": "GEMINI_API_KEY", "grok": "XAI_API_KEY",
	}
	providers := agents.Names()
	for _, provider := range providers {
		if _, ok := expectedArgv[provider]; !ok {
			t.Fatalf("registered provider %q has no scripted process fixture contract", provider)
		}
	}
	if len(expectedArgv) != len(providers) {
		t.Fatalf("fixture contracts = %v, registered providers = %v", sortedKeys(expectedArgv), providers)
	}

	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
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

			gitBin, err := exec.LookPath("git")
			if err != nil {
				t.Fatal("git is required for the scripted process suite")
			}
			ag, _ := agents.Get(provider)
			marker, _ := ag.AuthMarker()
			profile := filepath.Join(layout.Config, provider, "profiles", "default")
			if err := os.MkdirAll(profile, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(profile, marker), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			envFile := filepath.Join(layout.Config, "env")
			envContents := fmt.Sprintf("FIXTURE_SAFE=visible\n%s=must-be-stripped\n", credentialKey[provider])
			if err := os.WriteFile(envFile, []byte(envContents), 0o600); err != nil {
				t.Fatal(err)
			}

			scenarioPath := filepath.Join(layout.Plans, provider+".json")
			scenario, err := json.Marshal(map[string]any{
				"version": 1, "provider": provider, "provider_homes": providers,
				"marker": "fixture-ok-" + provider,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(scenarioPath, append(scenario, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			path := controlledPath(layout.Bin, filepath.Dir(gitBin))
			env, err := procharness.Environment(layout, map[string]string{
				"PATH": path, "COOP_RUNTIME": fixtureBin, "COOP_IMAGE": "fixture-image",
				"COOP_HOMES": "1", "COOP_PROVIDER_FIXTURE_ROOT": layout.Root,
				"COOP_PROVIDER_FIXTURE_IMAGE": "fixture-image", "COOP_PROVIDER_FIXTURE_TRACE": layout.Trace,
				"COOP_PROVIDER_FIXTURE_SCENARIO": scenarioPath,
			})
			if err != nil {
				t.Fatal(err)
			}
			initProcessRepo(t, gitBin, layout.Repo, env)
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("scripted process trace:\n%s", readProcessFile(t, layout.Trace))
				}
			})

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			result := procharness.Run(ctx, procharness.Command{
				Path: coopBin, Args: []string{provider}, Dir: layout.Repo, Env: env,
				MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
			})
			if result.Err != nil || result.ExitCode != 0 {
				t.Fatalf("coop %s = exit %d, err %v\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", provider, result.ExitCode, result.Err, result.Stdout, result.Stderr, readProcessFile(t, layout.Trace))
			}
			if !strings.Contains(result.Stdout, "fixture-ok-"+provider) {
				t.Fatalf("coop %s stdout lost fixture marker:\n%s", provider, result.Stdout)
			}

			trace := readProcessTrace(t, layout.Trace)
			assertSequentialTrace(t, trace)
			assertDirectRuntimeInvocations(t, trace)
			run := oneProcessEvent(t, trace, "runtime", "run")
			if run.Run == nil {
				t.Fatal("runtime run event has no parsed contract")
			}
			traceRepo := processTracePath(layout.Root, layout.Repo)
			if run.Run.Provider != provider || run.Run.Image != "fixture-image" || run.Run.Workdir != traceRepo || run.Run.HostWorkdir != traceRepo {
				t.Fatalf("run identity = provider %q image %q workdir %q/%q", run.Run.Provider, run.Run.Image, run.Run.Workdir, run.Run.HostWorkdir)
			}
			traceArgv := processTraceArgv(expectedArgv[provider])
			if !reflect.DeepEqual(run.Run.ProviderArgv, traceArgv) {
				t.Fatalf("provider argv = %q, want %q", run.Run.ProviderArgv, traceArgv)
			}
			if run.Run.Network != "none" || !reflect.DeepEqual(run.Run.Labels, []string{"coop=box"}) || run.Run.Interactive || run.Run.TTY {
				t.Fatalf("runtime boundary = network %q labels %q interactive/tty %v/%v", run.Run.Network, run.Run.Labels, run.Run.Interactive, run.Run.TTY)
			}
			assertProcessMounts(t, layout, provider, "default", run.Run.Mounts)
			assertProcessEnvironment(t, run.Run.Environment, provider, credentialKey[provider])
			if len(run.Run.EnvFiles) != 1 || !recordedFixturePath(run.Run.EnvFiles[0]) {
				t.Fatalf("scoped env files = %q, want one fixture-owned file", run.Run.EnvFiles)
			}

			providerStart := oneProcessEvent(t, trace, "provider", "start")
			providerExit := oneProcessEvent(t, trace, "provider", "exit")
			runtimeExit := oneProcessEvent(t, trace, "runtime", "exit")
			if providerStart.Cwd != traceRepo || !reflect.DeepEqual(providerStart.Argv, traceArgv) {
				t.Fatalf("provider start = cwd %q argv %q", providerStart.Cwd, providerStart.Argv)
			}
			for _, event := range []*processTrace{providerExit, runtimeExit} {
				if event.ExitCode == nil || *event.ExitCode != 0 || event.Signal != "" {
					t.Fatalf("%s exit = code %v signal %q", event.Source, event.ExitCode, event.Signal)
				}
			}
			pids := map[int]bool{result.PID: true}
			for _, event := range trace {
				pids[event.PID] = true
			}
			for pid := range pids {
				awaitProcessGone(t, pid)
			}
			if info, err := os.Stat(layout.Trace); err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("trace mode = %v, %v; want 0600", info, err)
			}
		})
	}
}

func buildProcessBinary(t *testing.T, root, output, pkg string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-trimpath", "-o", output, pkg)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, out)
	}
}

func initProcessRepo(t *testing.T, gitBin, repo string, env []string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command(gitBin, args...)
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.name", "Coop Fixture")
	run("config", "user.email", "fixture@invalid")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".agent/runs/\n.agent/tasks/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md", ".gitignore")
	run("commit", "-q", "-m", "fixture root")
}

func controlledPath(paths ...string) string {
	seen := map[string]bool{}
	var out []string
	for _, path := range append(paths, "/usr/bin", "/bin") {
		if path != "" && !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	return strings.Join(out, string(os.PathListSeparator))
}

func readProcessTrace(t *testing.T, path string) []*processTrace {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var trace []*processTrace
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 128<<10)
	for scanner.Scan() {
		var event processTrace
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode process trace: %v\n%s", err, scanner.Bytes())
		}
		trace = append(trace, &event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return trace
}

func assertSequentialTrace(t *testing.T, trace []*processTrace) {
	t.Helper()
	if len(trace) != 7 {
		t.Fatalf("process trace has %d events, want 7: %#v", len(trace), trace)
	}
	for i, event := range trace {
		if event.Version != 1 || event.Sequence != i+1 {
			t.Fatalf("trace event %d has version/sequence %d/%d", i, event.Version, event.Sequence)
		}
		if event.Event == "error" {
			t.Fatalf("trace contains runtime error event: %#v", event)
		}
	}
}

func assertDirectRuntimeInvocations(t *testing.T, trace []*processTrace) {
	t.Helper()
	var got [][]string
	for _, event := range trace {
		if event.Source == "runtime" && event.Event == "invoke" {
			got = append(got, event.Argv)
		}
	}
	wantPrefix := [][]string{{"--version"}, {"image", "inspect", "fixture-image"}}
	if len(got) != 3 || !reflect.DeepEqual(got[:2], wantPrefix) || len(got[2]) == 0 || got[2][0] != "run" {
		t.Fatalf("runtime invocations = %q, want version, inspect, run", got)
	}
}

func oneProcessEvent(t *testing.T, trace []*processTrace, source, event string) *processTrace {
	t.Helper()
	var matches []*processTrace
	for _, record := range trace {
		if record.Source == source && record.Event == event {
			matches = append(matches, record)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("trace has %d %s/%s events, want one", len(matches), source, event)
	}
	return matches[0]
}

func assertProcessMounts(t *testing.T, layout procharness.Layout, provider, account string, mounts []processMount) {
	t.Helper()
	profileSource := processTracePath(layout.Root, filepath.Join(layout.Config, provider, "profiles", account))
	repoSource := processTracePath(layout.Root, layout.Repo)
	profileTarget := "<container>/home/node/." + provider
	foundProfile, foundRepo := false, false
	for _, mount := range mounts {
		if mount.Named {
			if mount.Source != "coop-cache" && mount.Source != "coop-asdf" {
				t.Errorf("unknown named mount %#v", mount)
			}
			if mount.ReadOnly {
				t.Errorf("named volume must be writable: %#v", mount)
			}
			continue
		}
		if !recordedFixturePath(mount.Source) {
			t.Errorf("recorded mount escaped fixture root: %#v", mount)
		}
		if mount.Source == repoSource && mount.Target == repoSource && !mount.ReadOnly {
			foundRepo = true
			continue
		}
		if mount.Source == profileSource && mount.Target == profileTarget && !mount.ReadOnly {
			foundProfile = true
			continue
		}
		if mount.Target == profileTarget && !mount.ReadOnly {
			t.Errorf("unexpected account mounted for %s: %#v", provider, mount)
			continue
		}
		if !mount.ReadOnly {
			t.Errorf("unexpected writable mount %#v", mount)
		} else if !strings.HasPrefix(mount.Source, "<root>/tmp/coop-") {
			t.Errorf("read-only mount did not come from generated temp state: %#v", mount)
		}
	}
	if !foundRepo {
		t.Fatalf("writable repo mount %s:%s missing from %#v", repoSource, repoSource, mounts)
	}
	if !foundProfile {
		t.Fatalf("profile mount %s:%s missing from %#v", profileSource, profileTarget, mounts)
	}
}

func recordedFixturePath(path string) bool {
	return path == "<root>" || strings.HasPrefix(path, "<root>/")
}

func processTracePath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "<outside-root>"
	}
	if rel == "." {
		return "<root>"
	}
	return "<root>/" + filepath.ToSlash(rel)
}

func processTraceArgv(args []string) []string {
	out := make([]string, len(args))
	for i, arg := range args {
		if i == 0 || processSafeFlag(arg) {
			out[i] = arg
			continue
		}
		digest := sha256.Sum256([]byte(arg))
		out[i] = fmt.Sprintf("<sha256:%x>", digest[:8])
	}
	return out
}

func processSafeFlag(value string) bool {
	if len(value) < 2 || len(value) > 64 || value[0] != '-' {
		return false
	}
	for _, r := range value[1:] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func assertProcessEnvironment(t *testing.T, env []processEnv, provider, credentialKey string) {
	t.Helper()
	values := map[string]processEnv{}
	for _, item := range env {
		values[item.Name] = item
	}
	if values["COOP_PRIMARY"].Value != provider || values["FIXTURE_SAFE"].Value != "visible" {
		t.Fatalf("scoped environment = COOP_PRIMARY %#v FIXTURE_SAFE %#v", values["COOP_PRIMARY"], values["FIXTURE_SAFE"])
	}
	if _, ok := values[credentialKey]; ok {
		t.Fatalf("marker-backed profile leaked host %s into the box environment", credentialKey)
	}
}

func awaitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for procharness.ProcessAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if procharness.ProcessAlive(pid) {
		t.Errorf("scripted process %d survived successful cleanup", pid)
	}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func readProcessFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "<unreadable: " + err.Error() + ">"
	}
	return string(data)
}
