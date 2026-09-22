//go:build providerlivee2e

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func TestProviderNetworkLiveContractFailureEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, reason, phase, class string
		attempted                  bool
	}{
		{"version before spend", liveprovider.ReasonVersionProbe, "version", "harness", false},
		{"harness before spend", liveprovider.ReasonHarnessFailed, "harness", "harness", false},
		{"harness after spend", liveprovider.ReasonHarnessFailed, "harness", "harness", true},
		{"prompt timeout", liveprovider.ReasonPromptTimeout, "prompt", "timeout", true},
		{"resume timeout", liveprovider.ReasonPromptTimeout, "resume", "timeout", true},
		{"mcp timeout", liveprovider.ReasonPromptTimeout, "mcp", "timeout", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := providerNetworkLiveFailure(liveprovider.ProviderResult{Provider: "codex", Attempted: tc.attempted}, tc.reason, tc.phase, tc.class, 1)
			if result.Attempted != tc.attempted || result.TimedOut != (tc.class == "timeout") {
				t.Fatal("failure changed observed attempt or timeout evidence")
			}
			if _, err := liveprovider.NewSummary(false, []agents.Target{{Provider: "codex"}}, []liveprovider.ProviderResult{result}); err != nil {
				t.Fatalf("real failure cannot be reported: %v", err)
			}
		})
	}
}

func TestProviderNetworkLiveContractFixture(t *testing.T) {
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	selection := liveprovider.Selection{Provider: "gemini", Account: "work"}
	profile := filepath.Join(layout.Config, "gemini", "profiles", "work")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := prepareProviderNetworkLiveMCP(layout, selection, "/home/node", "linux/"+runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(layout.State, "provider-network-mcp.json") {
		t.Fatalf("MCP source config is not the fixed control path: %s", path)
	}
	info, err := os.Stat(filepath.Join(profile, "mcpprobe"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("probe executable missing: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	server := config.Servers["coop-probe"]
	if len(config.Servers) != 1 || server.Command != "/home/node/.gemini/mcpprobe" || server.Env["COOP_PROBE_LOG"] != "/home/node/.gemini/.coop-network-mcp.log" {
		t.Fatalf("MCP config does not use the selected provider home: %s", data)
	}
}

func TestProviderNetworkLiveContractMCPWitness(t *testing.T) {
	const valid = "launched\ninitialize\nanswered initialize\nnotifications/initialized\ntools/list\ntools/call coop_probe_tool\nanswered tools/call coop_probe_tool\n"
	if !providerNetworkLiveMCPWitness(valid) {
		t.Fatal("complete MCP tool witness rejected")
	}
	for name, log := range map[string]string{
		"empty":          "",
		"no answer":      strings.ReplaceAll(valid, "answered initialize\n", ""),
		"pipelined":      strings.ReplaceAll(valid, "answered initialize\nnotifications/initialized", "notifications/initialized\nanswered initialize"),
		"no catalog":     strings.ReplaceAll(valid, "tools/list\n", ""),
		"no tool":        strings.ReplaceAll(valid, "tools/call coop_probe_tool\nanswered tools/call coop_probe_tool\n", ""),
		"wrong tool":     strings.ReplaceAll(valid, "tools/call coop_probe_tool", "tools/call unexpected"),
		"no tool answer": strings.ReplaceAll(valid, "answered tools/call coop_probe_tool\n", ""),
		"duplicate tool": valid + "tools/call coop_probe_tool\nanswered tools/call coop_probe_tool\n",
		"cross process":  strings.ReplaceAll(valid, "tools/call coop_probe_tool\n", "launched\ntools/call coop_probe_tool\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if providerNetworkLiveMCPWitness(log) {
				t.Fatal("incomplete or invalid witness accepted")
			}
		})
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "witness")
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyProviderNetworkLiveMCP(root, path); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(root, "linked")
	if err := os.Symlink(path, linked); err != nil {
		t.Fatal(err)
	}
	if err := verifyProviderNetworkLiveMCP(root, linked); err == nil {
		t.Fatal("symlink witness accepted")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 65537)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyProviderNetworkLiveMCP(root, path); err == nil {
		t.Fatal("unbounded witness accepted")
	}
}
