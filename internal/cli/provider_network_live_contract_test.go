//go:build providerlivee2e

package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func TestProviderNetworkLiveRenewsBeforeAccessOnlyProjection(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.ParseForm() != nil || r.PostForm.Get("refresh_token") != "original-refresh" {
			t.Errorf("unexpected credential refresh request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "renewed-access", "refresh_token": "rotated-refresh", "expires_in": 21600,
		})
	}))
	defer server.Close()
	t.Setenv("GROK_REFRESH_TOKEN_URL_OVERRIDE", server.URL)
	cfg := &config.Config{ConfigDir: t.TempDir()}
	selection := liveprovider.Selection{Provider: "grok", Account: "default", SourceDefault: true}
	profile := cfg.AgentProfileDir(selection.Provider, selection.Account)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	writeNetworkLiveGrokCredential(t, profile, time.Now().Add(31*time.Minute), "original-refresh")
	deadline := time.Now().Add(box.RestrictedCredentialHorizon + liveNetworkChildWindow + 2*time.Minute)
	if err := prepareProviderNetworkLiveCredential(cfg, selection, deadline); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("refresh requests = %d, want 1", got)
	}
	projected := filepath.Join(t.TempDir(), "projected")
	prepared, err := liveprovider.Prepare(cfg.ConfigDir, projected, []liveprovider.Selection{selection})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Revoke()
	if got := prepared.PreflightReason("grok", "default", deadline); got != "" {
		t.Fatalf("projected credential preflight = %q, want portable", got)
	}
	if err := prepared.VerifySources(); err != nil {
		t.Fatal(err)
	}
	copy, err := os.ReadFile(filepath.Join(projected, "grok", "profiles", "default", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(copy), "renewed-access") || strings.Contains(string(copy), "refresh_token") ||
		strings.Contains(string(copy), "rotated-refresh") {
		t.Fatal("isolated credential is not fresh and access-only")
	}
	source, err := os.ReadFile(filepath.Join(profile, "auth.json"))
	if err != nil || !strings.Contains(string(source), "rotated-refresh") || strings.Contains(string(source), "original-refresh") {
		t.Fatal("trusted source did not retain only the rotated refresh token")
	}
	if err := prepareProviderNetworkLiveCredential(cfg, selection, deadline); err != nil || requests.Load() != 1 {
		t.Fatalf("fresh source was refreshed again: %v, requests=%d", err, requests.Load())
	}
}

func TestProviderNetworkLiveDoesNotRenewAbsentOrUnselectedLogin(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	t.Setenv("GROK_REFRESH_TOKEN_URL_OVERRIDE", server.URL)
	cfg := &config.Config{ConfigDir: t.TempDir()}
	profile := cfg.AgentProfileDir("grok", "default")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	writeNetworkLiveGrokCredential(t, profile, time.Now().Add(31*time.Minute), "original-refresh")
	deadline := time.Now().Add(box.RestrictedCredentialHorizon + liveNetworkChildWindow + 2*time.Minute)
	for _, selection := range []liveprovider.Selection{
		{Provider: "grok", Account: "other"}, {Provider: "codex", Account: "default"},
	} {
		if err := prepareProviderNetworkLiveCredential(cfg, selection, deadline); err != nil {
			t.Fatal(err)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("unselected/markerless profile caused %d refreshes", got)
	}
}

func TestProviderNetworkLiveRenewalFailureBeforeProjection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	t.Setenv("GROK_REFRESH_TOKEN_URL_OVERRIDE", server.URL)
	cfg := &config.Config{ConfigDir: t.TempDir()}
	selection := liveprovider.Selection{Provider: "grok", Account: "default", SourceDefault: true}
	profile := cfg.AgentProfileDir("grok", "default")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	writeNetworkLiveGrokCredential(t, profile, time.Now().Add(31*time.Minute), "original-refresh")
	path := filepath.Join(profile, "auth.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareProviderNetworkLiveCredential(cfg, selection, time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("failed refresh was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("failed refresh changed the trusted credential")
	}
}

func TestProviderNetworkLiveUnrenewableLoginKeepsPrerequisiteSkip(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	selection := liveprovider.Selection{Provider: "grok", Account: "default", SourceDefault: true}
	profile := cfg.AgentProfileDir("grok", "default")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	writeNetworkLiveGrokCredential(t, profile, time.Now().Add(31*time.Minute), "")
	deadline := time.Now().Add(box.RestrictedCredentialHorizon + liveNetworkChildWindow + 2*time.Minute)
	if err := prepareProviderNetworkLiveCredential(cfg, selection, deadline); err != nil {
		t.Fatal(err)
	}
	prepared, err := liveprovider.Prepare(cfg.ConfigDir, filepath.Join(t.TempDir(), "projected"), []liveprovider.Selection{selection})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Revoke()
	if got := prepared.PreflightReason("grok", "default", deadline); got != liveprovider.ReasonCredentialRefresh {
		t.Fatalf("unrenewable login preflight = %q, want %q", got, liveprovider.ReasonCredentialRefresh)
	}
}

func writeNetworkLiveGrokCredential(t *testing.T, profile string, expires time.Time, refresh string) {
	t.Helper()
	entry := map[string]any{
		"key": "original-access", "refresh_token": refresh, "expires_at": expires.UTC().Format(time.RFC3339Nano),
		"create_time": expires.Add(-6 * time.Hour).UTC().Format(time.RFC3339Nano),
		"auth_mode":   "oidc", "oidc_issuer": "https://auth.x.ai", "oidc_client_id": "client",
		"principal_id": "principal", "principal_type": "user", "user_id": "user", "team_id": "team",
	}
	data, err := json.Marshal(map[string]any{"https://auth.x.ai::client": entry})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "auth.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

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
