//go:build providerlivee2e

package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

// The preflight renews the account's canonical record, which is what the vault copies. A record that
// outlives the run needs no renewal and so no provider request; the vault's copy is access-only. How
// a renewal asks the provider and keeps a rotation is covered by the native renewal tests in
// internal/box and internal/agent, whose endpoint only they can replace.
func TestProviderNetworkLiveContractProjectsAFreshCanonicalLogin(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	selection := liveprovider.Selection{Provider: "grok", Account: "default", SourceDefault: true}
	deadline := time.Now().Add(box.RestrictedCredentialHorizon + liveNetworkChildWindow + 2*time.Minute)
	importNetworkLiveGrokLogin(t, cfg, deadline.Add(3*time.Hour), "source-refresh")
	if err := prepareProviderNetworkLiveCredential(cfg, selection, deadline); err != nil {
		t.Fatal(err)
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
	_ = filepath.WalkDir(projected, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			if data, _ := os.ReadFile(path); strings.Contains(string(data), "source-refresh") {
				t.Errorf("%s carries refresh authority", path)
			}
		}
		return nil
	})
}

// An account with no canonical record has nothing to renew, and the preflight changes nothing.
func TestProviderNetworkLiveContractDoesNotRenewAbsentOrUnselectedLogin(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	deadline := time.Now().Add(box.RestrictedCredentialHorizon + liveNetworkChildWindow + 2*time.Minute)
	importNetworkLiveGrokLogin(t, cfg, deadline.Add(3*time.Hour), "source-refresh")
	before := snapshotNetworkLiveConfig(t, cfg.ConfigDir)
	for _, selection := range []liveprovider.Selection{
		{Provider: "grok", Account: "other"}, {Provider: "codex", Account: "default"},
	} {
		if err := prepareProviderNetworkLiveCredential(cfg, selection, deadline); err != nil {
			t.Fatalf("%s@%s: %v", selection.Provider, selection.Account, err)
		}
	}
	if after := snapshotNetworkLiveConfig(t, cfg.ConfigDir); after != before {
		t.Fatal("an unselected or absent login changed the config")
	}
}

// A login that expires before the run and has no refresh authority cannot be renewed: the preflight
// refuses it without asking the provider, which the suite reports as a sign-in to redo.
func TestProviderNetworkLiveContractUnrenewableLoginIsRefused(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	selection := liveprovider.Selection{Provider: "grok", Account: "default", SourceDefault: true}
	deadline := time.Now().Add(box.RestrictedCredentialHorizon + liveNetworkChildWindow + 2*time.Minute)
	importNetworkLiveGrokLogin(t, cfg, time.Now().Add(31*time.Minute), "")
	before := snapshotNetworkLiveConfig(t, cfg.ConfigDir)
	if err := prepareProviderNetworkLiveCredential(cfg, selection, deadline); err == nil || !strings.Contains(err.Error(), "sign-in") {
		t.Fatalf("preflight = %v, want a refusal that asks for a sign-in", err)
	}
	if snapshotNetworkLiveConfig(t, cfg.ConfigDir) != before {
		t.Fatal("the refusal changed the config")
	}
}

func importNetworkLiveGrokLogin(t *testing.T, cfg *config.Config, expires time.Time, refresh string) {
	t.Helper()
	stage := t.TempDir()
	writeNetworkLiveGrokCredential(t, stage, expires, refresh)
	importNativeFixture(t, cfg, "grok", "default", map[string][]byte{"auth.json": mustReadNetworkLive(t, filepath.Join(stage, "auth.json"))})
}

func mustReadNetworkLive(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func snapshotNetworkLiveConfig(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		b.WriteString(path)
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			b.Write(data)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// writeNetworkLiveGrokCredential writes a Grok login in the pinned client's shape and scope.
func writeNetworkLiveGrokCredential(t *testing.T, dir string, expires time.Time, refresh string) {
	t.Helper()
	const client = "b1a00492-073a-47ea-816f-4c329264a828"
	entry := map[string]any{
		"key": "original-access", "refresh_token": refresh, "expires_at": expires.UTC().Format(time.RFC3339Nano),
		"create_time": expires.Add(-6 * time.Hour).UTC().Format(time.RFC3339Nano),
		"auth_mode":   "oauth", "oidc_issuer": "https://auth.x.ai", "oidc_client_id": client,
		"principal_id": "principal", "principal_type": "user", "user_id": "principal", "team_id": "team",
	}
	data, err := json.Marshal(map[string]any{"https://auth.x.ai::" + client: entry})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), data, 0o600); err != nil {
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
	nativeHome, err := box.NativeHomePath(&config.Config{ConfigDir: layout.Config}, selection.Provider, selection.Account, layout.Repo, false)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(nativeHome, "mcpprobe"))
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

func TestProviderNetworkLiveContractMCPReset(t *testing.T) {
	for _, kind := range []string{"existing", "missing", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path, target := filepath.Join(root, "witness with spaces"), filepath.Join(root, "target")
			const earlier = "earlier launch\n"
			if err := os.WriteFile(target, []byte(earlier), 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "existing":
				err = os.WriteFile(path, []byte(earlier), 0o600)
			case "symlink":
				err = os.Symlink(target, path)
			case "directory":
				err = os.Mkdir(path, 0o700)
			}
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"with spaces", "", `quotes '" and $HOME; $(exit 99)`}
			started := filepath.Join(root, "started")
			command := append([]string{"/bin/sh", "-c", `touch "$1"; shift; test ! -e "$1" || exit 17; output="$1"; shift; printf '%s\000' "$@" >"$output"`, "probe", started, path}, args...)
			wrapped := providerNetworkLiveMCPCommand(path, command)
			output, runErr := exec.Command(wrapped[0], wrapped[1:]...).CombinedOutput()
			if kind == "directory" {
				if runErr == nil {
					t.Fatal("failed reset did not stop the command")
				}
				if info, err := os.Stat(path); err != nil || !info.IsDir() {
					t.Fatal("reset removed a directory")
				}
				if _, err := os.Stat(started); !os.IsNotExist(err) {
					t.Fatal("client command started after reset failed")
				}
			} else {
				if runErr != nil {
					t.Fatalf("fresh witness command failed: %v, %s", runErr, output)
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != strings.Join(args, "\x00")+"\x00" {
					t.Fatalf("reset retained stale evidence or changed client argv: %q, %v", data, err)
				}
				if _, err := os.Stat(started); err != nil {
					t.Fatal("client command did not start after reset succeeded")
				}
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != earlier {
				t.Fatal("reset changed an unrelated or symlink target")
			}
		})
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
