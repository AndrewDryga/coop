//go:build providere2e

package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// The operator's shared MCP file is supposed to reach EVERY client, each in the form and at the path
// that client reads — claude's `.mcp.json`, codex's and grok's `config.toml`, gemini's
// `settings.json`. Until now that was covered by per-route unit tests and nothing that ran the real
// binary, so the qualification row map carried "no process-level test asserts them".
//
// Every harness in this suite points COOP_MCP_FILE at a shared MCP file that declares no servers.
// This one adds a server there and follows it into the box, asserting on the CONTENT of the config
// the client is handed rather than on the existence of a mount.
func TestProviderScriptedSharedMCPReachesEveryClient(t *testing.T) {
	suite := newDirectProcessSuite(t)

	const server = "shared-notes"
	body := `{"mcpServers":{"` + server + `":{"command":"/bin/true","args":["--serve"]}}}`
	if err := os.WriteFile(suite.layout.MCPFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, provider := range agents.Names() {
		t.Run(provider, func(t *testing.T) {
			_, trace := suite.run(t, []string{provider}, processScenario(provider, nil, 0, ""))
			run := oneProcessEvent(t, trace, "runtime", "run")
			if run.Run == nil {
				t.Fatal("the runtime was never asked to run a box")
			}
			// The config that names the server, wherever this client reads it from. The fixture
			// captured the server names at launch time, because coop removes these generated files
			// when the run ends — reading them here would find nothing.
			var carrying []string
			for _, mount := range run.Run.Mounts {
				if !slices.Contains(mount.MCPServers, server) {
					continue
				}
				carrying = append(carrying, mount.Target)
				// It is generated config, not the operator's own file handed over directly: the box
				// must never be able to edit what the host will read back.
				if !mount.ReadOnly {
					t.Errorf("%s got its MCP config writable (%s); the box could rewrite it", provider, mount.Target)
				}
			}
			if len(carrying) == 0 {
				t.Fatalf("%s never received the shared MCP server %q:\n%#v", provider, server, run.Run.Mounts)
			}
			// And it arrives where this client looks: its own home, or claude's repo-level .mcp.json.
			for _, target := range carrying {
				home := "<container>/home/node/." + provider + "/"
				if !strings.HasPrefix(target, home) && target != "<container>/home/node/.mcp.json" {
					t.Errorf("%s was handed the shared server at %q, which is not a path it reads", provider, target)
				}
			}
		})
	}
}

// The env file holds the default agents/mcp.json's tokens whichever MCP file is active. With
// COOP_MCP_FILE on the harness's own file, the offline box the real binary starts gets none of the
// variables the default file reads a secret from, and keeps the rest of the env file.
func TestProviderScriptedDefaultMCPTokensStayOutOfAnOfflineBox(t *testing.T) {
	suite := newDirectProcessSuite(t)
	defaultFile := filepath.Join(suite.layout.Config, "mcp.json")
	body := `{"mcpServers":{"emisar":{"type":"http","url":"https://emisar.example/mcp","bearer_token_env_var":"DEFAULT_MCP_TOKEN"}}}`
	if err := os.WriteFile(defaultFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(suite.layout.Config, "env")
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envFile, append(env, "DEFAULT_MCP_TOKEN=default-secret\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	_, trace := suite.run(t, []string{"claude"}, processScenario("claude", nil, 0, ""))
	run := oneProcessEvent(t, trace, "runtime", "run")
	if run.Run == nil {
		t.Fatal("the runtime was never asked to run a box")
	}
	start := oneProcessEvent(t, trace, "provider", "start")
	for name, environment := range map[string][]processEnv{"box": run.Run.Environment, "provider": start.Environment} {
		kept := false
		for _, item := range environment {
			if item.Name == "DEFAULT_MCP_TOKEN" || strings.Contains(item.Value, "default-secret") {
				t.Fatalf("the default MCP file's token reached the %s environment: %+v", name, item)
			}
			kept = kept || item.Name == "FIXTURE_SAFE"
		}
		if !kept {
			t.Fatalf("the %s environment lost the rest of the env file", name)
		}
	}
}
