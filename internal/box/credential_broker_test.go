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

func TestCredentialBrokerLeavesOAuthAndOpenRunsUnchanged(t *testing.T) {
	cfg, spec := brokerFixture(t, "ANTHROPIC_API_KEY=raw-provider-secret\n")
	cfg.Egress = "open"
	if candidate, err := selectCredentialBroker(cfg, spec); err != nil || candidate != nil {
		t.Fatalf("open run unexpectedly brokered: %#v, %v", candidate, err)
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
