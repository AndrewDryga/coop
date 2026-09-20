package box

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/networkstate"
)

func brokerFixture(t *testing.T, env string) (*config.Config, RunSpec) {
	t.Helper()
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "filtered"}
	if err := os.MkdirAll(filepath.Dir(cfg.EnvFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.EnvFile(), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, RunSpec{Agent: "claude", AgentCommand: true, Homes: true}
}

// firstBrokerRoute is the plan's first route: the older single-provider tests read one.
func firstBrokerRoute(cfg *config.Config, spec RunSpec) (*credentialRoute, error) {
	return firstBrokerRouteWithMarkers(cfg, spec, nil)
}

func firstBrokerRouteWithMarkers(cfg *config.Config, spec RunSpec, markers map[string]bool) (*credentialRoute, error) {
	plan, err := selectCredentialPlanWithMarkers(cfg, spec, markers)
	if err != nil || plan == nil {
		return nil, err
	}
	return plan.routes[0], nil
}

func onePlan(route *credentialRoute) *credentialPlan {
	plan := &credentialPlan{routes: []*credentialRoute{route}}
	for _, download := range route.spec.Downloads {
		plan.downloads = append(plan.downloads, downloadRoute{provider: route.provider, spec: download})
	}
	return plan
}

// A brokered Codex key also brokers what its client fetches for itself: with an API key the pinned
// codex looks up chatgpt.com and github.com on every start for its curated plugin store, which no
// API-key policy grants, so each start used to end in refusals nobody could act on. Coop fetches
// those PUBLIC bytes for it — the featured list and one repository's git fetch — with no credential
// of any kind, and points the client at itself through its two own levers.
func TestACodexKeyBrokersItsPluginStoreWithoutACredential(t *testing.T) {
	cfg, spec := brokerFixture(t, "OPENAI_API_KEY=provider-secret\n")
	spec.Agent = "codex"
	candidate, err := firstBrokerRoute(cfg, spec)
	if err != nil || candidate == nil {
		t.Fatalf("broker selection = %#v, %v", candidate, err)
	}
	plan := onePlan(candidate)
	routes := plan.gatewayRoutes()
	if len(routes) != 3 {
		t.Fatalf("a brokered codex key planned %d routes, want the key and its two downloads", len(routes))
	}
	store, git := routes[1], routes[2]
	if store.Kind != networkgateway.CredentialBrokerDownload || store.Upstream != "chatgpt.com" ||
		store.Header != "" || store.HeaderPrefix != "" {
		t.Fatalf("the plugin store route = %#v", store)
	}
	if git.Upstream != "github.com" || len(git.Allow) != 2 ||
		git.Allow[0] != (networkgateway.BrokerRequestLine{Method: "GET", Path: "/openai/plugins.git/info/refs", Query: "service=git-upload-pack"}) ||
		git.Allow[1] != (networkgateway.BrokerRequestLine{Method: "POST", Path: "/openai/plugins.git/git-upload-pack"}) {
		t.Fatalf("the plugin git route = %#v", git)
	}
	// The discovery path serves a push too, by query alone; that query is not in the set.
	discovery, _ := url.Parse("/openai/plugins.git/info/refs?service=git-receive-pack")
	if git.Admits("GET", discovery) {
		t.Fatal("the git route admits a push's discovery request")
	}
	// Both are shapes the gateway itself accepts (it validates the whole launch configuration).
	if err := networkgateway.CheckBrokerRoutes(routes); err != nil {
		t.Fatalf("the gateway would refuse this run's routes: %v", err)
	}
	// The client's own levers: its managed layer names the store listener, and the box's
	// Coop-owned git configuration rewrites that ONE repository (codex scrubs GIT_CONFIG_* from
	// the git it spawns, so a file is the only way in).
	f := &filteredExecution{broker: &credentialBrokerRun{plan: plan}}
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = t.TempDir()
	_, paths, err := f.credentialBrokerMounts(artifacts, cfg.HomeInBox)
	if err != nil {
		t.Fatal(err)
	}
	managed := string(mustReadFile(t, paths[0]))
	if want := `chatgpt_base_url = "http://` + networkgateway.CredentialBrokerAddress(1) + `/backend-api"`; !strings.Contains(managed, want) {
		t.Fatalf("the managed layer lacks %q:\n%s", want, managed)
	}
	rewrites := plan.gitRewrites()
	if len(rewrites) != 1 || rewrites["https://github.com/openai/plugins.git"] != "http://"+networkgateway.CredentialBrokerAddress(2)+"/openai/plugins.git" {
		t.Fatalf("git rewrites = %#v", rewrites)
	}
	// A run with no brokered key rewrites nothing and plans no download.
	var none *credentialPlan
	if len(none.gitRewrites()) != 0 {
		t.Fatal("a run without a broker rewrote a repository")
	}
}

func TestFilteredClaudeAPIKeyUsesBrokerInsteadOfProviderPolicy(t *testing.T) {
	cfg, spec := brokerFixture(t, "ANTHROPIC_API_KEY=raw-provider-secret\n")
	candidate, err := firstBrokerRoute(cfg, spec)
	if err != nil || candidate == nil || candidate.provider != "claude" || candidate.credential != "raw-provider-secret" {
		t.Fatalf("broker selection = %#v, %v", candidate, err)
	}
	bundles, err := NetworkProviderBundles(cfg, spec)
	if err != nil || len(bundles) != 0 {
		t.Fatalf("brokered provider remained in agent policy: %#v, %v", bundles, err)
	}
	f := &filteredExecution{broker: &credentialBrokerRun{plan: onePlan(candidate), substitutes: []string{strings.Repeat("a", 64)}}}
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = t.TempDir()
	envFile, err := f.credentialBrokerEnv(artifacts, cfg.EnvFile())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "raw-provider-secret") || EnvFileValues(envFile)["ANTHROPIC_API_KEY"] != strings.Repeat("a", 64) ||
		EnvFileValues(envFile)["ANTHROPIC_BASE_URL"] != "http://"+networkgateway.CredentialBrokerAddress(0) {
		t.Fatalf("broker environment leaked or omitted authority: %s", data)
	}
}

func TestFilteredProviderAPIKeysUseTheirNativeBrokerContracts(t *testing.T) {
	tests := []struct {
		provider, key, baseURL, upstream, header, prefix, path string
	}{
		{"gemini", "GEMINI_API_KEY", "GOOGLE_GEMINI_BASE_URL", "generativelanguage.googleapis.com", "x-goog-api-key", "", "/v1beta/models/"},
		{"codex", "OPENAI_API_KEY", "OPENAI_BASE_URL", "api.openai.com", "authorization", "Bearer ", "/v1/responses"},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			cfg, spec := brokerFixture(t, test.key+"=provider-secret\n")
			spec.Agent = test.provider
			candidate, err := firstBrokerRoute(cfg, spec)
			if err != nil || candidate == nil || candidate.provider != test.provider || candidate.credential != "provider-secret" {
				t.Fatalf("broker selection = %#v, %v", candidate, err)
			}
			route := onePlan(candidate).gatewayRoutes()[0]
			if route.Upstream != test.upstream || route.Header != test.header || route.HeaderPrefix != test.prefix || route.Path != test.path {
				t.Fatalf("broker route = %#v", route)
			}
			if test.provider == "codex" {
				// Every codex process in the box — lead, arms, codex-acp — reads the managed layer, and
				// the file replacing the image's must still keep the update check off.
				f := &filteredExecution{broker: &credentialBrokerRun{plan: onePlan(candidate)}}
				artifacts := defaultCompositionArtifactOps()
				artifacts.parent = t.TempDir()
				mounts, paths, err := f.credentialBrokerMounts(artifacts, cfg.HomeInBox)
				if err != nil || len(mounts) != 1 || mounts[0].box != "/etc/codex/managed_config.toml" {
					t.Fatalf("Codex broker configuration mount = %#v, %v", mounts, err)
				}
				config := string(mustReadFile(t, paths[0]))
				for _, want := range []string{"check_for_update_on_startup = false", `model_provider = "coop-broker"`,
					`base_url = "http://` + networkgateway.CredentialBrokerAddress(0) + `/v1"`, `env_key = "OPENAI_API_KEY"`, "responses_websockets = false"} {
					if !strings.Contains(config, want) {
						t.Fatalf("Codex broker configuration lacks %q:\n%s", want, config)
					}
				}
			}
			f := &filteredExecution{broker: &credentialBrokerRun{plan: onePlan(candidate), substitutes: []string{strings.Repeat("b", 64)}}}
			artifacts := defaultCompositionArtifactOps()
			artifacts.parent = t.TempDir()
			envFile, err := f.credentialBrokerEnv(artifacts, cfg.EnvFile())
			if err != nil {
				t.Fatal(err)
			}
			data := mustReadFile(t, envFile)
			if strings.Contains(string(data), "provider-secret") || EnvFileValues(envFile)[test.key] != strings.Repeat("b", 64) ||
				!strings.HasPrefix(EnvFileValues(envFile)[test.baseURL], "http://"+networkgateway.CredentialBrokerAddress(0)) {
				t.Fatalf("broker environment leaked or omitted authority: %s", data)
			}
		})
	}
}

// A route must admit what its pinned client really sends: each line was captured from the locked
// client in the image against a local listener (recipe: .agent/kb/provider-client-qualification.md),
// not read from provider docs. The first Claude route refused the beta Messages query its client
// always sends, so every brokered Claude request failed while the synthetic tests passed.
func TestCredentialBrokerRoutesAdmitWhatThePinnedClientsSend(t *testing.T) {
	type capture struct {
		version  string
		requests []string
	}
	gemini := capture{"0.59.0", []string{"POST /v1beta/models/gemini-3.1-flash-lite:generateContent",
		"POST /v1beta/models/gemini-3.1-pro-preview:streamGenerateContent?alt=sse"}}
	captured := map[string]map[egress.Client]capture{
		// claude-agent-acp runs its SDK's own claude (2.1.257), which sends the same line.
		"claude": {egress.ClientCLI: {"2.1.260", []string{"POST /v1/messages?beta=true"}},
			egress.ClientACP: {"0.75.1", []string{"POST /v1/messages?beta=true"}}},
		// codex-acp drives the same native codex.
		"codex": {egress.ClientCLI: {"0.153.4", []string{"POST /v1/responses"}},
			egress.ClientACP: {"1.10.0", []string{"POST /v1/responses"}}},
		"gemini": {egress.ClientCLI: gemini, egress.ClientACP: gemini},
	}
	for _, name := range agents.Names() {
		agent, _ := agents.Get(name)
		broker := agent.CredentialBroker()
		if !broker.Declared() {
			continue
		}
		route := onePlan(&credentialRoute{provider: name, spec: broker}).gatewayRoutes()[0]
		for _, client := range agent.LockedClients(agents.ClientPlatform{OS: "linux", Architecture: "arm64", Libc: "glibc"}) {
			seen, ok := captured[name][client.Client]
			if !ok || seen.version != client.Version {
				t.Errorf("locked %s %s client is %s, but its request lines were not captured from that version: capture them, then move this pin", name, client.Client, client.Version)
				continue
			}
			for _, line := range seen.requests {
				method, target, _ := strings.Cut(line, " ")
				parsed, err := url.ParseRequestURI(target)
				if err != nil || !route.Admits(method, parsed) {
					t.Errorf("%s's broker route refuses its own %s client's %q", name, client.Client, line)
				}
			}
		}
	}
}

func TestCredentialBrokerHandlesNamedGeminiHostKeyAndShadowsNativeStore(t *testing.T) {
	cfg, spec := brokerFixture(t, "")
	spec.Agent = "gemini"
	if err := cfg.SetDefaultProfile("gemini", "personal"); err != nil {
		t.Fatal(err)
	}
	gemini, _ := agents.Get("gemini")
	if err := SaveHostCredential(cfg, gemini, "personal", []byte("host-provider-secret")); err != nil {
		t.Fatal(err)
	}
	profile := cfg.AgentProfileDir("gemini", "personal")
	if err := os.WriteFile(filepath.Join(profile, "gemini-credentials.json"), []byte(`{"encrypted":"native-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := firstBrokerRoute(cfg, spec)
	if err != nil || candidate == nil || candidate.credential != "host-provider-secret" || candidate.shadowMarker != "gemini-credentials.json" {
		t.Fatalf("named host broker = %#v, %v", candidate, err)
	}
	f := &filteredExecution{broker: &credentialBrokerRun{plan: onePlan(candidate)}}
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = t.TempDir()
	mounts, paths, err := f.credentialBrokerMounts(artifacts, cfg.HomeInBox)
	if err != nil || len(mounts) != 1 || len(paths) != 1 || mounts[0].box != "/home/node/.gemini/gemini-credentials.json" || string(mustReadFile(t, paths[0])) != "{}\n" {
		t.Fatalf("marker shadow = %#v, %q, %v", mounts, paths, err)
	}
	mount, path := mounts[0], paths[0]
	options := assembleOptions(cfg, true, spec, nil, "/decoy", "/decoys", "/workspace", ttyNone,
		false, []extraMount{mount}, nil, nil, nil, nil, "", "")
	rendered := strings.Join(options, "\n")
	homeAt := strings.Index(rendered, cfg.AgentDir("gemini")+":"+cfg.HomeInBox+"/.gemini")
	shadowAt := strings.Index(rendered, path+":"+mount.box+":ro")
	if homeAt < 0 || shadowAt <= homeAt {
		t.Fatalf("credential marker must shadow the mounted profile: %s", rendered)
	}
}

func TestReusableAPIKeysRefuseBeforeRuntimeOutsideBrokerShape(t *testing.T) {
	for _, test := range []struct {
		provider string
		env      string
	}{
		{"claude", "ANTHROPIC_API_KEY=secret\n"},
		{"codex", "OPENAI_API_KEY=secret\n"},
		{"gemini", "GEMINI_API_KEY=secret\n"},
		{"grok", "XAI_API_KEY=secret\n"},
	} {
		t.Run(test.provider, func(t *testing.T) {
			cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "open"}
			if err := os.WriteFile(cfg.EnvFile(), []byte(test.env), 0o600); err != nil {
				t.Fatal(err)
			}
			recorder := filepath.Join(t.TempDir(), "runtime.log")
			code, err := Run(cfg, recorderRuntime(t, recorder), RunSpec{
				Image: "i", Repo: t.TempDir(), Workdir: "/workspace", Cmd: []string{test.provider},
				Agent: test.provider, AgentCommand: true, Homes: true, Batch: true, Quiet: true,
			})
			if code != -1 || err == nil {
				t.Fatalf("Run = (%d, %v), want pre-launch credential refusal", code, err)
			}
			if _, statErr := os.Stat(recorder); !os.IsNotExist(statErr) {
				t.Fatalf("credential refusal reached runtime: %v", statErr)
			}
		})
	}
}

func TestReviewProviderCredentialRefusesBeforeRuntime(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "open"}
	repo := t.TempDir()
	projectFile := filepath.Join(repo, ".agent", "project.yaml")
	if err := os.MkdirAll(filepath.Dir(projectFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectFile, []byte("review:\n  env:\n    OPENAI_API_KEY: review-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	code, err := Run(cfg, recorderRuntime(t, recorder), RunSpec{
		Image: "i", Repo: repo, Workdir: "/workspace", Cmd: []string{"codex"}, Agent: "codex",
		AgentCommand: true, Homes: true, Review: true, Batch: true, Quiet: true,
	})
	if code != -1 || err == nil || !strings.Contains(err.Error(), "cannot enter an agent box through -e") {
		t.Fatalf("review credential Run = (%d, %v)", code, err)
	}
	if _, statErr := os.Stat(recorder); !os.IsNotExist(statErr) {
		t.Fatalf("review credential refusal reached runtime: %v", statErr)
	}
}

func TestProjectAPIKeyRefusesWhenCredentialHomesAreDisabled(t *testing.T) {
	cfg, spec := brokerFixture(t, "")
	spec.Homes = false
	spec.projectEnv = map[string]string{"ANTHROPIC_API_KEY": "project-secret"}
	if candidate, err := firstBrokerRoute(cfg, spec); err == nil || candidate != nil || !strings.Contains(err.Error(), "credential homes disabled") {
		t.Fatalf("homes-disabled credential = %#v, %v", candidate, err)
	}
}

func TestProviderAPIKeyExtraEnvRefusesEvenWhenItBelongsToAnotherProvider(t *testing.T) {
	cfg, spec := brokerFixture(t, "")
	spec.ExtraArgs = []string{"-e", "OPENAI_API_KEY=secret"}
	if candidate, err := firstBrokerRoute(cfg, spec); err == nil || candidate != nil || !strings.Contains(err.Error(), "cannot enter an agent box through -e") {
		t.Fatalf("cross-provider -e credential = %#v, %v", candidate, err)
	}
}

func TestRawBoxDropsProviderKeysAndRefusesRuntimeInjection(t *testing.T) {
	cfg, _ := brokerFixture(t, "OPENAI_API_KEY=user-secret\nNORMAL=user-value\n")
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = t.TempDir()
	envFile, _, err := prepareBoxEnvFile(cfg, RunSpec{Homes: true}, artifacts,
		map[string]string{"GEMINI_API_KEY": "project-secret", "PROJECT": "value"})
	if err != nil {
		t.Fatal(err)
	}
	values := EnvFileValues(envFile)
	if values["OPENAI_API_KEY"] != "" || values["GEMINI_API_KEY"] != "" ||
		values["NORMAL"] != "user-value" || values["PROJECT"] != "value" {
		t.Fatalf("raw box environment = %#v", values)
	}
	spec := RunSpec{ExtraArgs: []string{"-e", "XAI_API_KEY=runtime-secret"}}
	if candidate, err := firstBrokerRoute(cfg, spec); err == nil || candidate != nil ||
		!strings.Contains(err.Error(), "cannot enter an agent box through -e") {
		t.Fatalf("raw runtime credential = %#v, %v", candidate, err)
	}
}

func TestCredentialBrokerRefusesUnsupportedAlternateAndStoredAPIKeys(t *testing.T) {
	for _, test := range []struct {
		provider, env string
	}{
		{"gemini", "GOOGLE_API_KEY=vertex-secret\n"},
		{"codex", "CODEX_API_KEY=alternate-secret\n"},
		{"codex", "CODEX_ACCESS_TOKEN=access-secret\n"},
		{"claude", "ANTHROPIC_AUTH_TOKEN=access-secret\n"},
		{"grok", "XAI_API_KEY=xai-secret\n"},
	} {
		t.Run(test.provider+"/"+strings.Split(test.env, "=")[0], func(t *testing.T) {
			cfg, spec := brokerFixture(t, test.env)
			spec.Agent = test.provider
			if candidate, err := firstBrokerRoute(cfg, spec); err == nil || candidate != nil || !strings.Contains(err.Error(), "cannot be brokered yet") {
				t.Fatalf("unsupported credential = %#v, %v", candidate, err)
			}
		})
	}

	t.Run("codex native API key", func(t *testing.T) {
		cfg, spec := brokerFixture(t, "")
		spec.Agent = "codex"
		profile := cfg.AgentProfileDir("codex", "default")
		if err := os.MkdirAll(profile, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(profile, "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"stored-secret"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if candidate, err := firstBrokerRoute(cfg, spec); err == nil || candidate != nil || !strings.Contains(err.Error(), "native credential file") {
			t.Fatalf("stored credential = %#v, %v", candidate, err)
		}
	})
}

func TestLoginEnvironmentDropsProviderCredentials(t *testing.T) {
	cfg, spec := brokerFixture(t, "ANTHROPIC_API_KEY=secret\nOPENAI_API_KEY=other\nNORMAL=value\n")
	spec.Login = true
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = t.TempDir()
	envFile, _, err := prepareBoxEnvFile(cfg, spec, artifacts, nil)
	if err != nil {
		t.Fatal(err)
	}
	data := string(mustReadFile(t, envFile))
	if strings.Contains(data, "API_KEY") || strings.Contains(data, "secret") || !strings.Contains(data, "NORMAL=value") {
		t.Fatalf("login environment = %q", data)
	}
}

func TestCredentialBrokerRefusesAmbiguousOrUnqualifiedClaudeAPIKeyRuns(t *testing.T) {
	for name, mutate := range map[string]func(*config.Config, *RunSpec){
		"alternate credential": func(cfg *config.Config, _ *RunSpec) {
			_ = os.WriteFile(cfg.EnvFile(), []byte("ANTHROPIC_API_KEY=raw-provider-secret\nANTHROPIC_AUTH_TOKEN=other\n"), 0o600)
		},
		"custom upstream in user env": func(cfg *config.Config, _ *RunSpec) {
			_ = os.WriteFile(cfg.EnvFile(), []byte("ANTHROPIC_API_KEY=raw-provider-secret\nANTHROPIC_BASE_URL=https://gateway.example\n"), 0o600)
		},
		"custom upstream in project env": func(_ *config.Config, spec *RunSpec) {
			spec.projectEnv = map[string]string{"ANTHROPIC_BASE_URL": "https://gateway.example"}
		},
		"not command": func(_ *config.Config, spec *RunSpec) { spec.AgentCommand = false },
		"env override": func(_ *config.Config, spec *RunSpec) {
			spec.ExtraArgs = []string{"--env=ANTHROPIC_BASE_URL=https://other.example"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, spec := brokerFixture(t, "ANTHROPIC_API_KEY=raw-provider-secret\n")
			mutate(cfg, &spec)
			if candidate, err := firstBrokerRoute(cfg, spec); err == nil || candidate != nil {
				t.Fatalf("unqualified broker selection = %#v, %v", candidate, err)
			}
		})
	}
}

func TestCredentialBrokerFreezesWritableProfileAuthMode(t *testing.T) {
	cfg, spec := brokerFixture(t, "ANTHROPIC_API_KEY=raw-provider-secret\n")
	profile := cfg.AgentProfileDir("claude", cfg.ActiveProfile("claude"))
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(profile, ".credentials.json")
	if err := os.WriteFile(marker, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	markers := profileMarkerSnapshot(cfg)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if candidate, err := firstBrokerRouteWithMarkers(cfg, spec, markers); err != nil || candidate != nil {
		t.Fatalf("frozen stored-auth selection = %#v, %v", candidate, err)
	}
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = t.TempDir()
	envFile, _, err := prepareBoxEnvFileWithMarkers(cfg, spec, artifacts, nil, markers)
	if err != nil {
		t.Fatal(err)
	}
	if value := EnvFileValues(envFile)["ANTHROPIC_API_KEY"]; value != "" {
		t.Fatalf("marker race restored reusable key %q", value)
	}
}

// An open or offline run has no gateway to keep a key outside the box, so Run refuses it before the
// runtime. Selection itself does not read the configured egress: an ACP child or a session runs
// under a captured filtered policy whatever its own egress setting says.
func TestCredentialBrokerRefusesOpenKeyAndLeavesOAuthUnchanged(t *testing.T) {
	cfg, spec := brokerFixture(t, "ANTHROPIC_API_KEY=raw-provider-secret\n")
	for _, mode := range []string{"open", "none"} {
		cfg.Egress = mode
		recorder := filepath.Join(t.TempDir(), "runtime.log")
		code, err := Run(cfg, recorderRuntime(t, recorder), RunSpec{Image: "i", Repo: t.TempDir(), Workdir: "/workspace",
			Cmd: []string{"claude"}, Agent: "claude", AgentCommand: true, Homes: true, Batch: true, Quiet: true})
		if code != -1 || err == nil || !strings.Contains(err.Error(), "requires filtered networking") {
			t.Fatalf("%s run = (%d, %v), want the key refused", mode, code, err)
		}
		if _, statErr := os.Stat(recorder); !os.IsNotExist(statErr) {
			t.Fatalf("%s refusal reached the runtime: %v", mode, statErr)
		}
	}
	// Interactive, the refusal comes before the launch names an account it will not connect.
	out := captureStderr(t, func() {
		_, err := Run(cfg, recorderRuntime(t, filepath.Join(t.TempDir(), "runtime.log")), RunSpec{Image: "i", Repo: t.TempDir(),
			Workdir: "/workspace", Cmd: []string{"claude"}, Agent: "claude", AgentCommand: true, Homes: true})
		if err == nil || !strings.Contains(err.Error(), "requires filtered networking") {
			t.Errorf("interactive open run = %v, want the key refused", err)
		}
	})
	if strings.Contains(out, "Connecting account") {
		t.Fatalf("an open run narrated the account it refuses:\n%s", out)
	}
	if candidate, err := firstBrokerRoute(cfg, spec); err != nil || candidate == nil {
		t.Fatalf("selection read the configured egress: %#v, %v", candidate, err)
	}
	// An ACP child or a session's admission revalidates under a captured filtered policy whatever
	// its own egress says, and brokers the key there.
	if bundles, err := NetworkProviderBundles(cfg, spec); err != nil || len(bundles) != 0 {
		t.Fatalf("a captured child's revalidation = %+v, %v; want the key brokered", bundles, err)
	}
	cfg.Egress = "filtered"
	profile := cfg.AgentProfileDir("claude", cfg.ActiveProfile("claude"))
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if candidate, err := firstBrokerRoute(cfg, spec); err != nil || candidate != nil {
		t.Fatalf("stored-credential run unexpectedly brokered: %#v, %v", candidate, err)
	}
}

// One broker serves every teammate shape a box can hold: the key of a peer, a preset role or a
// consult target is routed like the lead's, and an ACP or remote session is brokered like a CLI.
func TestCredentialBrokerServesEveryTeammateShape(t *testing.T) {
	for name, spec := range map[string]RunSpec{
		"a peer's key":         {Agent: "codex", AgentCommand: true, Homes: true, Peers: []agents.Target{{Provider: "claude"}}},
		"a consult lead's key": {Agent: "claude", ConsultLead: "claude", AgentCommand: true, Homes: true},
		"an ACP session":       {Agent: "claude", Homes: true, ForceNoTTY: true, ShareACPSessions: true, NetworkClient: egress.ClientACP},
		"a remote session":     {Agent: "claude", AgentCommand: true, Homes: true, ForceNoTTY: true},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, _ := brokerFixture(t, "ANTHROPIC_API_KEY=raw-provider-secret\n")
			route, err := firstBrokerRoute(cfg, spec)
			if err != nil || route == nil || route.provider != "claude" || route.credential != "raw-provider-secret" {
				t.Fatalf("teammate broker = %#v, %v", route, err)
			}
		})
	}
}

// What the broker cannot serve stays refused before a box starts: a provider with no broker contract
// (Grok) keeps its key out even as a peer, and a restricted mode — which cannot run filtered yet —
// says so instead of sending the person to a flag it would refuse.
func TestCredentialBrokerRefusesWhatItCannotServe(t *testing.T) {
	cfg, _ := brokerFixture(t, "XAI_API_KEY=xai-secret\n")
	peer := RunSpec{Agent: "claude", AgentCommand: true, Homes: true, Peers: []agents.Target{{Provider: "grok"}}}
	if plan, err := selectCredentialPlan(cfg, peer); err == nil || plan != nil || !strings.Contains(err.Error(), "cannot be brokered yet") {
		t.Fatalf("a Grok peer's key = %+v, %v", plan, err)
	}
	cfg, _ = brokerFixture(t, "ANTHROPIC_API_KEY=raw-provider-secret\n")
	restricted := RunSpec{Agent: "claude", AgentCommand: true, Homes: true, Mode: agents.ModeReadOnly}
	if plan, err := selectCredentialPlan(cfg, restricted); err == nil || plan != nil || !strings.Contains(err.Error(), "read-only and bare modes cannot use yet") {
		t.Fatalf("a restricted run's key = %+v, %v", plan, err)
	}
}

// Two providers' keys are two routes of one broker: each gets its own listener and capability, the
// box environment carries only capabilities, and a teammate on a stored login keeps it unbrokered.
func TestCredentialBrokerRoutesEveryProviderKeyAndLeavesSignedInTeammates(t *testing.T) {
	cfg, _ := brokerFixture(t, "GEMINI_API_KEY=gemini-secret\nANTHROPIC_API_KEY=claude-secret\n")
	spec := RunSpec{Agent: "gemini", AgentCommand: true, Homes: true, Peers: []agents.Target{{Provider: "claude"}, {Provider: "codex"}}}
	codexProfile := cfg.AgentProfileDir("codex", cfg.ActiveProfile("codex"))
	if err := os.MkdirAll(codexProfile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexProfile, "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"a"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := selectCredentialPlan(cfg, spec)
	if err != nil || plan == nil || len(plan.routes) != 2 || plan.routes[0].provider != "gemini" || plan.routes[1].provider != "claude" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if routes := plan.gatewayRoutes(); len(routes) != 2 || routes[0].Upstream != "generativelanguage.googleapis.com" || routes[1].Upstream != "api.anthropic.com" {
		t.Fatalf("gateway routes = %+v", routes)
	}
	f := &filteredExecution{broker: &credentialBrokerRun{plan: plan, substitutes: []string{strings.Repeat("g", 64), strings.Repeat("c", 64)}}}
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = t.TempDir()
	envFile, err := f.credentialBrokerEnv(artifacts, cfg.EnvFile())
	if err != nil {
		t.Fatal(err)
	}
	values := EnvFileValues(envFile)
	if data := string(mustReadFile(t, envFile)); strings.Contains(data, "gemini-secret") || strings.Contains(data, "claude-secret") {
		t.Fatalf("a real key reached the box environment: %s", data)
	}
	if values["GEMINI_API_KEY"] != strings.Repeat("g", 64) || values["GOOGLE_GEMINI_BASE_URL"] != "http://"+networkgateway.CredentialBrokerAddress(0) ||
		values["ANTHROPIC_API_KEY"] != strings.Repeat("c", 64) || values["ANTHROPIC_BASE_URL"] != "http://"+networkgateway.CredentialBrokerAddress(1) {
		t.Fatalf("each route's capability and listener = %#v", values)
	}
	if _, route := plan.route("codex"); route != nil || values["OPENAI_BASE_URL"] != "" {
		t.Fatalf("a signed-in teammate was brokered: %+v, %q", route, values["OPENAI_BASE_URL"])
	}
}

// Two accounts of one provider are two boxes, each with its own broker: every box's route carries
// only its own account's key.
func TestCredentialBrokerBindsEachBoxToItsOwnAccount(t *testing.T) {
	cfg, spec := brokerFixture(t, "")
	spec.Agent = "gemini"
	gemini, _ := agents.Get("gemini")
	for _, account := range []string{"work", "personal"} {
		if err := SaveHostCredential(cfg, gemini, account, []byte(account+"-secret")); err != nil {
			t.Fatal(err)
		}
	}
	for _, account := range []string{"work", "personal"} {
		cfg.SetActiveProfile("gemini", account)
		route, err := firstBrokerRoute(cfg, spec)
		if err != nil || route == nil || route.account != account || route.credential != account+"-secret" {
			t.Fatalf("%s box route = %#v, %v", account, route, err)
		}
	}
}

// The guard's secret file holds every route's key, each bound to its own capability, and the host
// forgets the keys once the file is written.
func TestCredentialBrokerSecretCoversEveryRouteOnce(t *testing.T) {
	cfg, _ := brokerFixture(t, "GEMINI_API_KEY=gemini-secret\nANTHROPIC_API_KEY=claude-secret\n")
	plan, err := selectCredentialPlan(cfg, RunSpec{Agent: "gemini", AgentCommand: true, Homes: true, Peers: []agents.Target{{Provider: "claude"}}})
	if err != nil || plan == nil || len(plan.routes) != 2 {
		t.Fatal(plan, err)
	}
	f, _ := filteredFixture(t)
	f.broker = &credentialBrokerRun{plan: plan}
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = f.runfiles
	if err := f.prepareCredentialBroker(artifacts); err != nil {
		t.Fatal(err)
	}
	data := mustReadFile(t, f.broker.configPath)
	config := networkgateway.LaunchConfig{RunID: f.record.ID, Epoch: f.record.Epoch, Brokers: plan.gatewayRoutes()}
	secrets, err := networkgateway.ReadCredentialBrokerSecrets(bytes.NewReader(data), config)
	if err != nil {
		t.Fatalf("the guard would refuse this secret: %v", err)
	}
	if len(secrets.Routes) != 2 || secrets.Routes[0].Credential != "gemini-secret" || secrets.Routes[1].Credential != "claude-secret" ||
		secrets.Routes[0].Substitute != f.broker.substitutes[0] || secrets.Routes[1].Substitute != f.broker.substitutes[1] {
		t.Fatal("the secret does not bind each route's key to its own capability")
	}
	for _, route := range plan.routes {
		if route.credential != "" {
			t.Fatalf("the host kept %s's key after writing the guard's secret", route.provider)
		}
	}
	if info, err := os.Stat(f.broker.configPath); err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("secret file = %v, %v", info, err)
	}
}

func TestCredentialBrokerSecretMountExistsOnlyOnCaplessGuard(t *testing.T) {
	f := &filteredExecution{config: "/owner/launch.json", broker: &credentialBrokerRun{configPath: "/owner/broker.json"},
		record: networkstate.Execution{Resources: []networkstate.Resource{
			{Role: "controller", ID: "controller-id"}, {Role: "ipc", Name: "ipc-volume"}, {Role: "observations", Name: "observations-volume"},
		}}}
	guard := strings.Join(f.helperOptions("guard"), " ")
	controller := strings.Join(f.helperOptions("controller"), " ")
	if !strings.Contains(guard, "/owner/broker.json") || !strings.Contains(guard, networkgateway.CredentialBrokerPath) {
		t.Fatalf("guard omitted broker secret mount: %s", guard)
	}
	if strings.Contains(controller, "/owner/broker.json") || strings.Contains(controller, networkgateway.CredentialBrokerPath) {
		t.Fatalf("privileged controller received broker secret mount: %s", controller)
	}
}
