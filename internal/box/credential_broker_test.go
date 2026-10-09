package box

import (
	"bytes"
	"encoding/json"
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
	"github.com/AndrewDryga/coop/internal/runtime"
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

func nativeBrokerPlanFixture(t *testing.T, cfg *config.Config, spec RunSpec) *nativeRun {
	t.Helper()
	run, err := planNativeAccounts(t.Context(), cfg, runtime.Runtime{}, spec, false)
	if err != nil || run == nil {
		t.Fatalf("native plan: %v", err)
	}
	return run
}

func nativeBrokerPreparedFixture(t *testing.T, run *nativeRun) (networkgateway.NativeRunConfig, map[string]networkgateway.NativeAccessSnapshot) {
	t.Helper()
	if err := run.prepare(t.Context(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	run.cancel()
	run.workers.Wait()
	t.Cleanup(func() {
		if err := run.close(); err != nil {
			t.Error(err)
		}
	})
	var cfg networkgateway.NativeRunConfig
	if err := json.Unmarshal(mustReadFile(t, filepath.Join(run.dir, "config.json")), &cfg); err != nil {
		t.Fatal(err)
	}
	snapshots := map[string]networkgateway.NativeAccessSnapshot{}
	for _, account := range run.accounts {
		var snapshot networkgateway.NativeAccessSnapshot
		if err := json.Unmarshal(mustReadFile(t, filepath.Join(run.dir, account.agent.Name()+".json")), &snapshot); err != nil {
			t.Fatal(err)
		}
		snapshots[account.agent.Name()] = snapshot
	}
	return cfg, snapshots
}

func assertNativePublicSeed(t *testing.T, seed agents.NativeBrokerSeed, secrets ...string) {
	t.Helper()
	// JSON byte fields are base64: inspect actual file/helper bytes too.
	public := seed.Marker + string(seed.Helper)
	for _, data := range seed.Files {
		public += string(data)
	}
	for key, value := range seed.Env {
		public += key + "=" + value + "\n"
	}
	if seed.Marker == "" {
		t.Fatal("native seed has no public selector")
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(public, secret) {
			t.Fatal("reusable credential reached public seed")
		}
	}
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
	run := nativeBrokerPlanFixture(t, cfg, spec)
	if len(run.accounts) != 1 {
		t.Fatal("unexpected selected accounts")
	}
	assertNativePublicSeed(t, run.accounts[0].seed, "provider-secret")
	featured, websocket := false, false
	for _, route := range run.accounts[0].routes {
		if route.Host == "github.com" {
			t.Fatal("public git host became protected TLS origin")
		}
		if route.Host == "chatgpt.com" && route.Method == "GET" && route.Path == "/backend-api/plugins/featured" && route.Query == "platform=codex" {
			if !route.CredentialFree || route.Header != "" || route.AccountHeader != "" {
				t.Fatal("public catalog acquired credentials")
			}
			featured = true
		}
		websocket = websocket || route.Host == "api.openai.com" && route.Method == "GET" && route.Path == "/v1/responses"
	}
	if !featured || !websocket {
		t.Fatal("native catalog or websocket missing")
	}
	downloads := run.downloads()
	if len(downloads) != 1 {
		t.Fatal("public git downloads", downloads)
	}
	git := downloads[0]
	if git.Kind != networkgateway.CredentialBrokerDownload || git.Upstream != "github.com" || git.Header != "" || git.HeaderPrefix != "" || len(git.Allow) != 2 ||
		git.Allow[0] != (networkgateway.BrokerRequestLine{Method: "GET", Path: "/openai/plugins.git/info/refs", Query: "service=git-upload-pack"}) ||
		git.Allow[1] != (networkgateway.BrokerRequestLine{Method: "POST", Path: "/openai/plugins.git/git-upload-pack"}) {
		t.Fatal("public git route", git)
	}
	push, _ := url.Parse("/openai/plugins.git/info/refs?service=git-receive-pack")
	if git.Admits("GET", push) {
		t.Fatal("push discovery allowed")
	}
	if err := networkgateway.CheckBrokerRoutes(downloads); err != nil {
		t.Fatal(err)
	}
	rewrites := run.gitRewrites()
	if len(rewrites) != 1 || rewrites["https://github.com/openai/plugins.git"] != "http://"+networkgateway.NativeProxyAddress+"/openai/plugins.git" {
		t.Fatal("git rewrites", rewrites)
	}
	var none *nativeRun
	if len(none.gitRewrites()) != 0 || len(none.downloads()) != 0 {
		t.Fatal("unselected provider rewrote git")
	}
}

func TestFilteredClaudeAPIKeyUsesBrokerInsteadOfProviderPolicy(t *testing.T) {
	cfg, spec := brokerFixture(t, "ANTHROPIC_API_KEY=raw-provider-secret\nNORMAL=value\n")
	run := nativeBrokerPlanFixture(t, cfg, spec)
	if len(run.accounts) != 1 || run.accounts[0].agent.Name() != "claude" || run.accounts[0].ephemeral == nil {
		t.Fatal("env key not selected privately")
	}
	assertNativePublicSeed(t, run.accounts[0].seed, "raw-provider-secret")
	spec.native = run
	bundles, err := NetworkProviderBundles(cfg, spec)
	if err != nil || len(bundles) != 0 {
		t.Fatal("protected origin in workload policy", bundles, err)
	}
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = t.TempDir()
	envFile, _, err := prepareBoxEnvFile(cfg, spec, artifacts, nil)
	if err != nil {
		t.Fatal(err)
	}
	values := EnvFileValues(envFile)
	if values["ANTHROPIC_API_KEY"] != "" || values["NORMAL"] != "value" || strings.Contains(string(mustReadFile(t, envFile)), "raw-provider-secret") {
		t.Fatal("box env leaked host key")
	}
	plan, snapshots := nativeBrokerPreparedFixture(t, run)
	if len(plan.Accounts) != 1 || snapshots["claude"].Credential != "raw-provider-secret" || snapshots["claude"].Revoked {
		t.Fatal("private snapshot missing key")
	}
	if strings.Contains(string(mustReadFile(t, filepath.Join(run.dir, "config.json"))), "raw-provider-secret") {
		t.Fatal("route config leaked key")
	}
	args, err := run.agentArgs(artifacts)
	rendered := strings.Join(args, "\n")
	if err != nil || strings.Contains(rendered, "raw-provider-secret") || strings.Contains(rendered, run.dir) || !strings.Contains(rendered, "HTTPS_PROXY=http://"+networkgateway.NativeProxyAddress) || strings.Contains(rendered, "ANTHROPIC_BASE_URL=") {
		t.Fatal("native origin or custody changed", err)
	}
}

func TestFilteredProviderAPIKeysUseTheirNativeBrokerContracts(t *testing.T) {
	for _, tc := range []struct{ provider, key, upstream, header, prefix, path string }{
		{"gemini", "GEMINI_API_KEY", "generativelanguage.googleapis.com", "X-Goog-Api-Key", "", "/v1beta/models/"},
		{"codex", "OPENAI_API_KEY", "api.openai.com", "Authorization", "Bearer ", "/v1/responses"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			cfg, spec := brokerFixture(t, tc.key+"=provider-secret\n")
			spec.Agent = tc.provider
			run := nativeBrokerPlanFixture(t, cfg, spec)
			if len(run.accounts) != 1 {
				t.Fatal("unexpected accounts")
			}
			account := run.accounts[0]
			assertNativePublicSeed(t, account.seed, "provider-secret")
			found := false
			for _, route := range account.routes {
				found = found || route.Host == tc.upstream && route.Method == "POST" && route.Path == tc.path && route.Header == tc.header && route.HeaderPrefix == tc.prefix
			}
			if !found {
				t.Fatal("native model route absent", account.routes)
			}
			if tc.provider == "codex" {
				if account.seed.Env["COOP_NATIVE_CODEX_FAMILY"] != "apikey" || account.seed.Env["CODEX_API_KEY"] != account.seed.Marker {
					t.Fatal("native API selection missing")
				}
				for _, data := range account.seed.Files {
					if strings.Contains(string(data), "responses_websockets") || strings.Contains(string(data), "model_provider") {
						t.Fatal("seed changed native transport")
					}
				}
				websocket := false
				for _, route := range account.routes {
					websocket = websocket || route.Host == tc.upstream && route.Method == "GET" && route.Path == "/v1/responses"
				}
				if !websocket {
					t.Fatal("native websocket missing")
				}
			}
			plan, snapshots := nativeBrokerPreparedFixture(t, run)
			if len(plan.Accounts) != 1 || snapshots[tc.provider].Credential != "provider-secret" || snapshots[tc.provider].Revoked {
				t.Fatal("snapshot omitted key")
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
	gemini := capture{"0.62.0", []string{"POST /v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse"}}
	captured := map[string]map[egress.Client]capture{
		// claude-agent-acp runs its SDK's own claude (2.1.284), which sends the same line.
		"claude": {egress.ClientCLI: {"2.1.285", []string{"POST /v1/messages?beta=true"}},
			egress.ClientACP: {"0.84.0", []string{"POST /v1/messages?beta=true"}}},
		// codex-acp drives the same native codex.
		"codex": {egress.ClientCLI: {"0.159.2", []string{"POST /v1/responses"}},
			egress.ClientACP: {"2.0.1", []string{"POST /v1/responses"}}},
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

func TestCredentialBrokerKeepsNamedGeminiKeyOutsideCompleteHome(t *testing.T) {
	cfg, spec := brokerFixture(t, "")
	spec.Agent = "gemini"
	cfg.SetActiveProfile("gemini", "personal")
	ag, _ := agents.Get("gemini")
	if err := SaveHostCredential(cfg, ag, "personal", []byte("host-provider-secret")); err != nil {
		t.Fatal(err)
	}
	legacy := cfg.AgentProfileDir("gemini", "personal")
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(legacy, "gemini-credentials.json")
	retained := []byte("inert-retained-unknown-cache")
	if err := os.WriteFile(cache, retained, 0600); err != nil {
		t.Fatal(err)
	}
	home := scopedFixtureHome(t, cfg, "gemini", t.TempDir(), false)
	cfg = cfg.WithNativeHomes(map[string]string{"gemini": home})
	run, err := planNativeRun(t.Context(), cfg, runtime.Runtime{}, spec)
	if err != nil || run == nil || len(run.accounts) != 1 || run.accounts[0].account != "personal" || run.accounts[0].ephemeral != nil {
		t.Fatal("named key not selected", err)
	}
	assertNativePublicSeed(t, run.accounts[0].seed, "host-provider-secret", string(retained))
	if _, err := os.Stat(filepath.Join(home, "gemini-credentials.json")); !os.IsNotExist(err) {
		t.Fatal("legacy encrypted store copied", err)
	}
	if !bytes.Equal(mustReadFile(t, cache), retained) {
		t.Fatal("retained cache modified")
	}
	options := assembleOptions(cfg, true, spec, nil, "/decoy", "/decoys", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, "", "")
	rendered := strings.Join(options, "\n")
	if !strings.Contains(rendered, home) || strings.Contains(rendered, legacy) || strings.Contains(rendered, "host-credentials") {
		t.Fatal("mount escaped complete native home")
	}
	_, snapshots := nativeBrokerPreparedFixture(t, run)
	if snapshots["gemini"].Credential != "host-provider-secret" || snapshots["gemini"].Revoked {
		t.Fatal("guard snapshot missing")
	}
}

func TestReusableAPIKeysRefuseBeforeRuntimeOutsideBrokerShape(t *testing.T) {
	for _, tc := range []struct{ provider, env string }{
		{"claude", "ANTHROPIC_API_KEY=secret\nANTHROPIC_BASE_URL=https://unselected.example\n"},
		{"codex", "OPENAI_API_KEY=secret\nOPENAI_BASE_URL=https://unselected.example\n"},
		{"gemini", "GEMINI_API_KEY=secret\nGOOGLE_GEMINI_BASE_URL=https://unselected.example\n"}, {"grok", "XAI_API_KEY=secret\n"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			cfg, spec := brokerFixture(t, tc.env)
			cfg.Egress = "open"
			spec.Agent = tc.provider
			recorder := filepath.Join(t.TempDir(), "runtime.log")
			run, err := planNativeAccounts(t.Context(), cfg, recorderRuntime(t, recorder), spec, false)
			if err == nil {
				spec.native = run
				_, err = selectCredentialPlan(cfg, spec)
			}
			if err == nil {
				t.Fatal("unsupported origin/family admitted")
			}
			if _, err := os.Stat(recorder); !os.IsNotExist(err) {
				t.Fatal("refusal invoked runtime", err)
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
			if candidate, err := firstBrokerRoute(cfg, spec); err == nil || candidate != nil {
				t.Fatalf("unsupported credential = %#v, %v", candidate, err)
			}
		})
	}

	t.Run("stored Codex API key is canonical and private", func(t *testing.T) {
		cfg, spec := brokerFixture(t, "")
		spec.Agent = "codex"
		importCanonicalFixture(t, cfg, "codex", "default", map[string][]byte{"auth.json": []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"stored-secret"}`)})
		run := nativeBrokerPlanFixture(t, cfg, spec)
		if run.accounts[0].ephemeral != nil || run.accounts[0].record.Selection != "apikey" {
			t.Fatal("stored key not canonical")
		}
		assertNativePublicSeed(t, run.accounts[0].seed, "stored-secret")
		_, snapshots := nativeBrokerPreparedFixture(t, run)
		if snapshots["codex"].Credential != "stored-secret" || snapshots["codex"].Revoked {
			t.Fatal("guard snapshot missing")
		}
		if err := RemoveNativeAccount(t.Context(), cfg, "codex", "default"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg.EnvFile(), []byte("OPENAI_API_KEY=stale-env-secret\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if next, err := planNativeAccounts(t.Context(), cfg, runtime.Runtime{}, spec, false); err == nil || next != nil {
			t.Fatal("env resurrected removed account")
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
		"env override": func(_ *config.Config, spec *RunSpec) {
			spec.ExtraArgs = []string{"--env=ANTHROPIC_BASE_URL=https://other.example"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, spec := brokerFixture(t, "ANTHROPIC_API_KEY=raw-provider-secret\n")
			mutate(cfg, &spec)
			if candidate, err := planNativeAccounts(t.Context(), cfg, runtime.Runtime{}, spec, false); err == nil || candidate != nil {
				t.Fatalf("unqualified broker selection = %#v, %v", candidate, err)
			}
		})
	}
}

func TestCredentialBrokerFreezesWritableProfileAuthMode(t *testing.T) {
	cfg, spec := brokerFixture(t, "ANTHROPIC_API_KEY=raw-provider-secret\n")
	seedCanonicalFixture(t, cfg, "claude", "default")
	repo := t.TempDir()
	home, err := PrepareNativeHome(t.Context(), cfg, runtime.Runtime{}, "claude", "default", repo, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg = cfg.WithNativeHomes(map[string]string{"claude": home})
	run, err := planNativeRun(t.Context(), cfg, runtime.Runtime{}, spec)
	if err != nil || run == nil || len(run.accounts) != 1 || run.accounts[0].record.Selection != "claude-oauth" || run.accounts[0].accessOverride != "" {
		t.Fatal("stale global key replaced subscription selection", err)
	}
	assertNativePublicSeed(t, run.accounts[0].seed, "raw-provider-secret", "ACCESS_CANARY-claude-default", "REFRESH_CANARY-claude-default")
	marker := filepath.Join(home, ".credentials.json")
	changed := []byte("{}")
	if err := os.WriteFile(marker, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if next, err := planNativeRun(t.Context(), cfg, runtime.Runtime{}, spec); err == nil || next != nil {
		t.Fatal("divergent native auth mode admitted")
	}
	if !bytes.Equal(mustReadFile(t, marker), changed) {
		t.Fatal("divergent native credential overwritten")
	}
	spec.native = run
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = t.TempDir()
	envFile, _, err := prepareBoxEnvFile(cfg, spec, artifacts, nil)
	if err != nil || EnvFileValues(envFile)["ANTHROPIC_API_KEY"] != "" {
		t.Fatal("auth mode change restored reusable key", err)
	}
}

// An open or offline run has no gateway to keep a key outside the box, so Run refuses it before the
// runtime. Selection itself does not read the configured egress: an ACP child or a session runs
// under a captured filtered policy whatever its own egress setting says.
func TestCredentialBrokerRefusesOpenKeyAndLeavesOAuthUnchanged(t *testing.T) {
	for _, mode := range []string{"open", "filtered"} {
		t.Run(mode, func(t *testing.T) {
			cfg, spec := brokerFixture(t, "ANTHROPIC_API_KEY=raw-provider-secret\n")
			cfg.Egress = mode
			run := nativeBrokerPlanFixture(t, cfg, spec)
			assertNativePublicSeed(t, run.accounts[0].seed, "raw-provider-secret")
			_, snapshots := nativeBrokerPreparedFixture(t, run)
			if snapshots["claude"].Credential != "raw-provider-secret" || snapshots["claude"].Revoked {
				t.Fatal("supported native key not held privately")
			}
		})
	}
	for _, tc := range []struct{ provider, key string }{{"claude", "ANTHROPIC_API_KEY"}, {"codex", "OPENAI_API_KEY"}, {"gemini", "GEMINI_API_KEY"}} {
		t.Run("offline/"+tc.provider, func(t *testing.T) {
			cfg, spec := brokerFixture(t, tc.key+"=offline-host-secret\nNORMAL=value\n")
			cfg.Egress, spec.Agent = "none", tc.provider
			if run, err := planNativeAccounts(t.Context(), cfg, runtime.Runtime{}, spec, false); err != nil || run != nil {
				t.Fatal("offline run started native proxy", err)
			}
			artifacts := defaultCompositionArtifactOps()
			artifacts.parent = t.TempDir()
			envFile, _, err := prepareBoxEnvFile(cfg, spec, artifacts, map[string]string{tc.key: "offline-project-secret"})
			if err != nil {
				t.Fatal(err)
			}
			values := EnvFileValues(envFile)
			if values[tc.key] != "" || values["NORMAL"] != "value" || strings.Contains(string(mustReadFile(t, envFile)), "offline-host-secret") || strings.Contains(string(mustReadFile(t, envFile)), "offline-project-secret") {
				t.Fatal("offline environment leaked reusable provider credentials")
			}
		})
	}
	t.Run("offline stored Gemini key", func(t *testing.T) {
		cfg, spec := brokerFixture(t, "NORMAL=value\n")
		cfg.Egress, spec.Agent = "none", "gemini"
		gemini, _ := agents.Get("gemini")
		if err := SaveHostCredential(cfg, gemini, "default", []byte("offline-stored-secret")); err != nil {
			t.Fatal(err)
		}
		artifacts := defaultCompositionArtifactOps()
		artifacts.parent = t.TempDir()
		envFile, _, err := prepareBoxEnvFile(cfg, spec, artifacts, nil)
		if err != nil || EnvFileValues(envFile)["GEMINI_API_KEY"] != "" || EnvFileValues(envFile)["NORMAL"] != "value" {
			t.Fatal("offline host vault key entered workload env", err)
		}
	})
	t.Run("subscription suppresses stale API environment", func(t *testing.T) {
		cfg, spec := brokerFixture(t, "ANTHROPIC_API_KEY=stale-api-secret\n")
		seedCanonicalFixture(t, cfg, "claude", "default")
		run := nativeBrokerPlanFixture(t, cfg, spec)
		if len(run.accounts) != 1 || run.accounts[0].record.Selection != "claude-oauth" || run.accounts[0].accessOverride != "" || run.accounts[0].ephemeral != nil {
			t.Fatal("native subscription silently became API billing")
		}
		assertNativePublicSeed(t, run.accounts[0].seed, "stale-api-secret", "ACCESS_CANARY-claude-default", "REFRESH_CANARY-claude-default")
		_, snapshots := nativeBrokerPreparedFixture(t, run)
		if snapshots["claude"].Credential != "ACCESS_CANARY-claude-default" {
			t.Fatal("subscription access replaced by API key")
		}
	})
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
			run := nativeBrokerPlanFixture(t, cfg, spec)
			if len(run.accounts) != 1 || run.accounts[0].agent.Name() != "claude" || run.accounts[0].account != "default" || run.accounts[0].ephemeral == nil {
				t.Fatal("teammate native authority selection differs from lead")
			}
			assertNativePublicSeed(t, run.accounts[0].seed, "raw-provider-secret")
		})
	}
}

// What the broker cannot serve stays refused before a box starts: a provider with no broker contract
// (Grok) keeps its key out even as a peer, and a restricted mode — which cannot run filtered yet —
// says so instead of sending the person to a flag it would refuse.
func TestCredentialBrokerRefusesWhatItCannotServe(t *testing.T) {
	cfg, _ := brokerFixture(t, "XAI_API_KEY=xai-secret\n")
	peer := RunSpec{Agent: "claude", AgentCommand: true, Homes: true, Peers: []agents.Target{{Provider: "grok"}}}
	if plan, err := selectCredentialPlan(cfg, peer); err == nil || plan != nil {
		t.Fatal("unsupported Grok API key peer admitted")
	}
	for _, mode := range []string{"filtered", "open"} {
		t.Run(mode, func(t *testing.T) {
			cfg, spec := brokerFixture(t, "ANTHROPIC_API_KEY=raw-provider-secret\n")
			cfg.Egress, spec.Mode = mode, agents.ModeReadOnly
			run := nativeBrokerPlanFixture(t, cfg, spec)
			if len(run.accounts) != 1 {
				t.Fatal("restricted native authority missing")
			}
			assertNativePublicSeed(t, run.accounts[0].seed, "raw-provider-secret")
			_, snapshots := nativeBrokerPreparedFixture(t, run)
			if snapshots["claude"].Credential != "raw-provider-secret" || snapshots["claude"].Revoked {
				t.Fatal("restricted key was not held guard-only")
			}
		})
	}
}

// Two providers' keys are two routes of one broker: each gets its own listener and capability, the
// box environment carries only capabilities, and a teammate on a stored login keeps it unbrokered.
func TestCredentialBrokerRoutesEveryProviderKeyAndLeavesSignedInTeammates(t *testing.T) {
	cfg, _ := brokerFixture(t, "GEMINI_API_KEY=gemini-secret\nANTHROPIC_API_KEY=claude-secret\nNORMAL=value\n")
	seedCanonicalFixture(t, cfg, "codex", "default")
	spec := RunSpec{Agent: "gemini", AgentCommand: true, Homes: true, Peers: []agents.Target{{Provider: "claude"}, {Provider: "codex"}}}
	run := nativeBrokerPlanFixture(t, cfg, spec)
	if len(run.accounts) != 3 || run.accounts[0].agent.Name() != "gemini" || run.accounts[1].agent.Name() != "claude" || run.accounts[2].agent.Name() != "codex" || run.accounts[2].record.Selection != "chatgpt" {
		t.Fatal("mixed API and subscription selection changed", run.accounts)
	}
	codex := run.accounts[2]
	codexState, err := codex.inspect(codex.record)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range run.accounts {
		assertNativePublicSeed(t, account.seed, "gemini-secret", "claude-secret", codexState.AccessToken, "REFRESH_CANARY-codex-default")
	}
	spec.native = run
	if bundles, err := NetworkProviderBundles(cfg, spec); err != nil || len(bundles) != 0 {
		t.Fatal("native account got ordinary workload provider grants", err)
	}
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = t.TempDir()
	envFile, _, err := prepareBoxEnvFile(cfg, spec, artifacts, nil)
	if err != nil {
		t.Fatal(err)
	}
	values := EnvFileValues(envFile)
	if values["GEMINI_API_KEY"] != "" || values["ANTHROPIC_API_KEY"] != "" || values["OPENAI_API_KEY"] != "" || values["NORMAL"] != "value" {
		t.Fatal("mixed run exposed provider keys")
	}
	plan, snapshots := nativeBrokerPreparedFixture(t, run)
	if len(plan.Accounts) != 3 || snapshots["gemini"].Credential != "gemini-secret" || snapshots["claude"].Credential != "claude-secret" || snapshots["codex"].Credential != codexState.AccessToken {
		t.Fatal("guard selected different account credentials")
	}
	for _, snapshot := range snapshots {
		if snapshot.Revoked || snapshot.Binding.RunID != run.runID || snapshot.Binding.Account != "default" {
			t.Fatal("account not bound to exact native run")
		}
	}
	args, err := run.agentArgs(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	rendered := strings.Join(args, "\n")
	if strings.Count(rendered, "HTTPS_PROXY=http://"+networkgateway.NativeProxyAddress) != 1 || strings.Contains(rendered, "gemini-secret") || strings.Contains(rendered, "claude-secret") || strings.Contains(rendered, codexState.AccessToken) || strings.Contains(rendered, run.dir) {
		t.Fatal("mixed run lost single entrance or guard custody")
	}
	accountHeader := false
	for _, account := range plan.Accounts {
		if account.Binding.Provider != "codex" {
			continue
		}
		for _, origin := range account.Origins {
			if origin.Host == "chatgpt.com" {
				accountHeader = origin.AccountHeaders["Chatgpt-Account-Id"] == codexState.AccountID && origin.ClientAccountHeaders["Chatgpt-Account-Id"] == agents.NativeBrokerAccount
			}
		}
	}
	if !accountHeader {
		t.Fatal("native subscription lost selected ChatGPT account header")
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
		t.Run(account, func(t *testing.T) {
			cfg.SetActiveProfile("gemini", account)
			run := nativeBrokerPlanFixture(t, cfg, spec)
			if len(run.accounts) != 1 || run.accounts[0].account != account || run.accounts[0].ephemeral != nil {
				t.Fatal("wrong native account selected")
			}
			assertNativePublicSeed(t, run.accounts[0].seed, "work-secret", "personal-secret")
			plan, snapshots := nativeBrokerPreparedFixture(t, run)
			snapshot := snapshots["gemini"]
			if len(plan.Accounts) != 1 || plan.Accounts[0].Binding.Account != account || plan.Accounts[0].Binding.RunID != run.runID || snapshot.Binding != plan.Accounts[0].Binding || snapshot.Credential != account+"-secret" || snapshot.Revoked {
				t.Fatal("box crossed native account boundary")
			}
		})
	}
}

// The guard's secret file holds every route's key, each bound to its own capability, and the host
// forgets the keys once the file is written.
func TestCredentialBrokerSecretCoversEveryRouteOnce(t *testing.T) {
	// Static bearer/MCP transport remains separate from native provider selection.
	gemini, _ := agents.Get("gemini")
	claude, _ := agents.Get("claude")
	plan := &credentialPlan{routes: []*credentialRoute{
		{provider: "gemini", account: "default", spec: gemini.CredentialBroker(), credential: "gemini-secret"},
		{provider: "claude", account: "default", spec: claude.CredentialBroker(), credential: "claude-secret"},
	}}
	f, _ := filteredFixture(t)
	f.broker = &credentialBrokerRun{plan: plan}
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = f.runfiles
	if err := f.prepareCredentialBroker(artifacts); err != nil {
		t.Fatal(err)
	}
	data := mustReadFile(t, f.broker.configPath)
	cfg := networkgateway.LaunchConfig{RunID: f.record.ID, Epoch: f.record.Epoch, Brokers: plan.gatewayRoutes()}
	secrets, err := networkgateway.ReadCredentialBrokerSecrets(bytes.NewReader(data), cfg)
	if err != nil {
		t.Fatalf("guard refused static broker secret: %v", err)
	}
	if len(secrets.Routes) != 2 || secrets.Routes[0].Credential != "gemini-secret" || secrets.Routes[1].Credential != "claude-secret" || secrets.Routes[0].Substitute != f.broker.substitutes[0] || secrets.Routes[1].Substitute != f.broker.substitutes[1] {
		t.Fatal("secret did not bind every static route credential once")
	}
	for _, route := range plan.routes {
		if route.credential != "" {
			t.Fatalf("host kept %s static transport key", route.provider)
		}
	}
	if info, err := os.Stat(f.broker.configPath); err != nil || info.Mode().Perm() != 0444 {
		t.Fatalf("secret file mode: %v %v", info, err)
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
