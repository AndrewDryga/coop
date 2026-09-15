package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
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

func TestFilteredClaudeAPIKeyUsesBrokerInsteadOfProviderPolicy(t *testing.T) {
	cfg, spec := brokerFixture(t, "ANTHROPIC_API_KEY=raw-provider-secret\n")
	candidate, err := selectCredentialBroker(cfg, spec)
	if err != nil || candidate == nil || candidate.provider != "claude" || candidate.credential != "raw-provider-secret" {
		t.Fatalf("broker selection = %#v, %v", candidate, err)
	}
	bundles, err := NetworkProviderBundles(cfg, spec)
	if err != nil || len(bundles) != 0 {
		t.Fatalf("brokered provider remained in agent policy: %#v, %v", bundles, err)
	}
	f := &filteredExecution{broker: &credentialBrokerRun{candidate: candidate, substitute: strings.Repeat("a", 64)}}
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
		EnvFileValues(envFile)["ANTHROPIC_BASE_URL"] != "http://"+networkgateway.CredentialBrokerAddress {
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
			candidate, err := selectCredentialBroker(cfg, spec)
			if err != nil || candidate == nil || candidate.provider != test.provider || candidate.credential != "provider-secret" {
				t.Fatalf("broker selection = %#v, %v", candidate, err)
			}
			route := candidate.route()
			if route.Upstream != test.upstream || route.Header != test.header || route.HeaderPrefix != test.prefix || route.Path != test.path {
				t.Fatalf("broker route = %#v", route)
			}
			if test.provider == "codex" {
				command := strings.Join(candidate.command([]string{"codex", "exec", "prompt"}), " ")
				if !strings.Contains(command, `model_provider="coop-broker"`) ||
					!strings.Contains(command, `base_url="http://`+networkgateway.CredentialBrokerAddress+`/v1"`) ||
					!strings.Contains(command, "features.responses_websockets=false") {
					t.Fatalf("Codex broker command = %s", command)
				}
			}
			f := &filteredExecution{broker: &credentialBrokerRun{candidate: candidate, substitute: strings.Repeat("b", 64)}}
			artifacts := defaultCompositionArtifactOps()
			artifacts.parent = t.TempDir()
			envFile, err := f.credentialBrokerEnv(artifacts, cfg.EnvFile())
			if err != nil {
				t.Fatal(err)
			}
			data := mustReadFile(t, envFile)
			if strings.Contains(string(data), "provider-secret") || EnvFileValues(envFile)[test.key] != strings.Repeat("b", 64) ||
				!strings.HasPrefix(EnvFileValues(envFile)[test.baseURL], "http://"+networkgateway.CredentialBrokerAddress) {
				t.Fatalf("broker environment leaked or omitted authority: %s", data)
			}
		})
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
	candidate, err := selectCredentialBroker(cfg, spec)
	if err != nil || candidate == nil || candidate.credential != "host-provider-secret" || candidate.shadowMarker != "gemini-credentials.json" {
		t.Fatalf("named host broker = %#v, %v", candidate, err)
	}
	f := &filteredExecution{broker: &credentialBrokerRun{candidate: candidate}}
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = t.TempDir()
	mount, path, err := f.credentialBrokerMarkerMount(artifacts, cfg.HomeInBox)
	if err != nil || path == "" || mount.box != "/home/node/.gemini/gemini-credentials.json" || string(mustReadFile(t, path)) != "{}\n" {
		t.Fatalf("marker shadow = %#v, %q, %v", mount, path, err)
	}
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
	if candidate, err := selectCredentialBroker(cfg, spec); err == nil || candidate != nil || !strings.Contains(err.Error(), "credential homes disabled") {
		t.Fatalf("homes-disabled credential = %#v, %v", candidate, err)
	}
}

func TestProviderAPIKeyExtraEnvRefusesEvenWhenItBelongsToAnotherProvider(t *testing.T) {
	cfg, spec := brokerFixture(t, "")
	spec.ExtraArgs = []string{"-e", "OPENAI_API_KEY=secret"}
	if candidate, err := selectCredentialBroker(cfg, spec); err == nil || candidate != nil || !strings.Contains(err.Error(), "cannot enter an agent box through -e") {
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
	if candidate, err := selectCredentialBroker(cfg, spec); err == nil || candidate != nil ||
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
			if candidate, err := selectCredentialBroker(cfg, spec); err == nil || candidate != nil || !strings.Contains(err.Error(), "cannot be brokered yet") {
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
		if candidate, err := selectCredentialBroker(cfg, spec); err == nil || candidate != nil || !strings.Contains(err.Error(), "native credential file") {
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
		"acp":         func(_ *config.Config, spec *RunSpec) { spec.ForceNoTTY = true },
		"peer":        func(_ *config.Config, spec *RunSpec) { spec.Peers = []agents.Target{{Provider: "codex"}} },
		"not command": func(_ *config.Config, spec *RunSpec) { spec.AgentCommand = false },
		"env override": func(_ *config.Config, spec *RunSpec) {
			spec.ExtraArgs = []string{"--env=ANTHROPIC_BASE_URL=https://other.example"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, spec := brokerFixture(t, "ANTHROPIC_API_KEY=raw-provider-secret\n")
			mutate(cfg, &spec)
			if candidate, err := selectCredentialBroker(cfg, spec); err == nil || candidate != nil {
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
	if candidate, err := selectCredentialBrokerWithMarkers(cfg, spec, markers); err != nil || candidate != nil {
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

func TestCredentialBrokerRefusesOpenKeyAndLeavesOAuthUnchanged(t *testing.T) {
	cfg, spec := brokerFixture(t, "ANTHROPIC_API_KEY=raw-provider-secret\n")
	cfg.Egress = "open"
	if candidate, err := selectCredentialBroker(cfg, spec); err == nil || candidate != nil || !strings.Contains(err.Error(), "requires filtered networking") {
		t.Fatalf("open run retained a reusable key: %#v, %v", candidate, err)
	}
	cfg.Egress = "filtered"
	profile := cfg.AgentProfileDir("claude", cfg.ActiveProfile("claude"))
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if candidate, err := selectCredentialBroker(cfg, spec); err != nil || candidate != nil {
		t.Fatalf("stored-credential run unexpectedly brokered: %#v, %v", candidate, err)
	}
}

func TestCredentialBrokerRefusesRawClaudeKeyMountedOnlyAsPeer(t *testing.T) {
	cfg, _ := brokerFixture(t, "ANTHROPIC_API_KEY=raw-provider-secret\n")
	spec := RunSpec{Agent: "codex", AgentCommand: true, Homes: true, Peers: []agents.Target{{Provider: "claude"}}}
	if _, err := selectCredentialBroker(cfg, spec); err == nil || !strings.Contains(err.Error(), "as a peer") {
		t.Fatalf("raw Claude peer credential was not refused: %v", err)
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
