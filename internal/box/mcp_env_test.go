package box

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestMCPAuthenticationCapturesFinalEnvironmentWithoutSecretArgs(t *testing.T) {
	for _, override := range [][]string{nil, {"-e", "TOKEN=explicit"}, {"--env=TOKEN=inline"}, {"-eTOKEN=compact"}, {"-e", "TOKEN"}} {
		t.Run(strings.Join(override, " "), func(t *testing.T) {
			t.Setenv("TOKEN", "ambient")
			file := filepath.Join(t.TempDir(), "env")
			writeCopyFixture(t, file, "TOKEN=first\nKEEP=untouched\nTOKEN=last\n")
			options := append([]string{"--label", "--env-file", "--env-file", file}, override...)
			got, captured, err := captureRequiredMCPEnv(options, []string{"TOKEN", "TOKEN"}, defaultCompositionArtifactOps())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(captured) })
			want := "last"
			if len(override) > 0 {
				assignment := strings.TrimPrefix(override[len(override)-1], "--env=")
				assignment = strings.TrimPrefix(assignment, "-e")
				_, value, explicit := strings.Cut(assignment, "=")
				want = value
				if !explicit {
					want = "ambient"
				}
			}
			if value := EnvFileValues(captured)["TOKEN"]; value != want {
				t.Fatalf("captured value = %q, want %q", value, want)
			}
			if strings.Contains(strings.Join(got, " "), "TOKEN") || !slices.Equal(got[len(got)-2:], []string{"--env-file", captured}) {
				t.Fatalf("token leaked into argv or snapshot isn't last: %q", got)
			}
			info, err := os.Stat(captured)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatal("captured environment is not private")
			}
			writeCopyFixture(t, file, "TOKEN=changed-after-capture\n")
			t.Setenv("TOKEN", "also-changed")
			if EnvFileValues(captured)["TOKEN"] != want {
				t.Fatal("captured authentication followed mutable host state")
			}
		})
	}
}

func TestMCPAuthenticationRejectsMissingOrBlankFinalValues(t *testing.T) {
	t.Setenv("TOKEN", "not-implicitly-authorized")
	for _, options := range [][]string{nil, {"-e", "TOKEN="}, {"-e", "TOKEN=   "}, {"-e", "TOKEN=bad\nvalue"}, {"--env-file", "/missing-fixture-env"}} {
		if _, file, err := captureRequiredMCPEnv(options, []string{"TOKEN"}, defaultCompositionArtifactOps()); err == nil || file != "" || strings.Contains(err.Error(), "not-implicitly-authorized") {
			t.Fatalf("invalid environment accepted or exposed: file=%q err=%v", file, err)
		}
	}
}

func TestGeminiMCPEnvironmentAtNormalACPAndNestedLaunch(t *testing.T) {
	for name, spec := range map[string]RunSpec{
		"normal": {Agent: "gemini", AgentCommand: true, Cmd: []string{"gemini"}},
		"ACP":    {Agent: "gemini", ForceNoTTY: true, Cmd: []string{"gemini", "--acp"}},
		"nested": {Agent: "claude", ConsultLead: "claude", Peers: []agents.Target{{Provider: "gemini"}}, Cmd: []string{"claude"}},
	} {
		for _, available := range []bool{false, true} {
			t.Run(name+"/available="+strconv.FormatBool(available), func(t *testing.T) {
				cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}
				cfg.MCPFile = filepath.Join(t.TempDir(), "mcp.json")
				writeCopyFixture(t, cfg.MCPFile, `{"mcpServers":{"test":{"url":"https://example.test/mcp","bearer_token_env_var":"MCP_TOKEN"}}}`)
				t.Setenv("MCP_TOKEN", "ambient-is-not-box-authority")
				if available {
					writeCopyFixture(t, cfg.EnvFile(), "MCP_TOKEN=scoped-fixture-token\n")
				}
				spec.Repo, spec.Image = t.TempDir(), "i"
				spec.Homes, spec.Quiet, spec.Batch = true, true, true
				launched := false
				spec.OnRuntimeLaunch = func() { launched = true }
				recorder := filepath.Join(t.TempDir(), "runtime.log")
				captured := false
				artifacts := defaultCompositionArtifactOps()
				artifacts.writeFile = func(parent, content string) (string, error) {
					if content == "MCP_TOKEN=scoped-fixture-token\n" {
						captured = true
					}
					return writeTempFile(parent, content)
				}
				_, err := runWithCompositionArtifacts(cfg, recorderRuntime(t, recorder), spec, artifacts)
				if available {
					if err != nil || !launched || !captured {
						t.Fatalf("valid scoped MCP failed: launched=%v captured=%v err=%v", launched, captured, err)
					}
					data, readErr := os.ReadFile(recorder)
					if readErr != nil || strings.Contains(string(data), "scoped-fixture-token") || !strings.Contains(string(data), "/.gemini/settings.json:ro") || !strings.Contains(string(data), spec.Repo+":") {
						t.Fatalf("ordinary projection lost immutable config/repo, or leaked token: %v", readErr)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "MCP_TOKEN") || launched {
					t.Fatalf("missing token launched provider or lost diagnostic: %v", err)
				}
				if _, err := os.Stat(recorder); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("missing MCP token reached the runtime")
				}
			})
		}
	}
}

func TestMCPAuthenticationExplicitUnsetImportClearsEarlierValue(t *testing.T) {
	const key = "COOP_TEST_BARE_MCP_TOKEN"
	t.Setenv(key, "temporary")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	// Unlike a bare line in an env file, Docker's explicit -e KEY removes an earlier value
	// when no ambient value exists. Never resurrect that value to satisfy authentication.
	if _, _, err := captureRequiredMCPEnv([]string{"-e", key + "=earlier", "-e", key}, []string{key}, defaultCompositionArtifactOps()); err == nil {
		t.Fatal("unset explicit import retained authentication")
	}
}

func TestMCPAuthenticationRejectsBadMetadataAndCaptureFailure(t *testing.T) {
	for _, key := range []string{"", "TOKEN\nINJECTED", "TOKEN=bad", "1TOKEN"} {
		if _, _, err := captureRequiredMCPEnv(nil, []string{key}, defaultCompositionArtifactOps()); err == nil {
			t.Fatal("invalid requirement accepted")
		}
	}
	artifacts := defaultCompositionArtifactOps()
	artifacts.writeFile = func(string, string) (string, error) { return "", errors.New("fixture failure") }
	if _, _, err := captureRequiredMCPEnv([]string{"-e", "TOKEN=value"}, []string{"TOKEN"}, artifacts); err == nil {
		t.Fatal("capture failure accepted")
	}
}

func TestGeminiMCPDoesNotRestoreAnotherAccountsEnvironmentKey(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}
	cfg.SetActiveProfile("gemini", "nondefault")
	cfg.MCPFile = filepath.Join(t.TempDir(), "mcp.json")
	writeCopyFixture(t, cfg.MCPFile, `{"mcpServers":{"test":{"url":"https://example.test/mcp","bearer_token_env_var":"GEMINI_API_KEY"}}}`)
	writeCopyFixture(t, cfg.EnvFile(), "GEMINI_API_KEY=other-account\n")
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	_, err := Run(cfg, recorderRuntime(t, recorder), RunSpec{Repo: t.TempDir(), Image: "i", Agent: "gemini", Cmd: []string{"gemini"}, Homes: true, Quiet: true, Batch: true})
	if err == nil || !strings.Contains(err.Error(), "GEMINI_API_KEY") || strings.Contains(err.Error(), "other-account") {
		t.Fatalf("cross-account MCP environment accepted or exposed: %v", err)
	}
}
