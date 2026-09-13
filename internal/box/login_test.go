package box

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestLoginOnlyMountsSelectedCredentialAndManagedSettings(t *testing.T) {
	for _, name := range agents.Names() {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			writeCopyFixture(t, filepath.Join(repo, ".agent/project.yaml"), "box:\n  network: true\n  env:\n    PROJECT_ONLY: not-for-login\nserve:\n  ports: [4000]\n")
			writeCopyFixture(t, filepath.Join(repo, ".agent/compose.yml"), "services: [broken\n")
			cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "open", AutoUp: true}
			cfg.SetActiveProfile(name, "personal")
			cfg.MCPFile = filepath.Join(t.TempDir(), "invalid-mcp.json")
			writeCopyFixture(t, cfg.MCPFile, "{invalid shared MCP")
			writeCopyFixture(t, filepath.Join(cfg.ConfigDir, "INSTRUCTIONS.md"), "must-not-be-mounted")
			ag, _ := agents.Get(name)
			spec := RunSpec{Image: "i", Repo: repo, Agent: name, Cmd: ag.Login(cfg), Homes: true,
				Login: true, AgentCommand: true, Network: true, Serve: true, Batch: true, Quiet: true,
				ConsultLead: name, Peers: []agents.Target{{Provider: "codex"}, {Provider: "claude"}},
				Preset: &preset.Preset{Name: "must-not-load"}, AssignedTask: "must-not-stamp"}
			recorder := filepath.Join(t.TempDir(), "runtime.log")
			var rendered []string
			artifacts := defaultCompositionArtifactOps()
			artifacts.writeFile = func(parent, content string) (string, error) {
				rendered = append(rendered, content)
				return writeTempFile(parent, content)
			}
			if code, err := runWithCompositionArtifacts(cfg, recorderRuntime(t, recorder), spec, artifacts); code != 0 || err != nil {
				t.Fatalf("login = %d, %v", code, err)
			}
			data, err := os.ReadFile(recorder)
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Fields(string(data))
			if !slices.Contains(args, cfg.AgentDir(name)+":"+cfg.HomeInBox+"/."+name) || !strings.Contains(string(data), "-w /tmp") {
				t.Fatalf("login lost selected writable home or clean cwd: %s", data)
			}
			for _, other := range agents.Names() {
				if other != name && strings.Contains(string(data), cfg.AgentDir(other)+":") {
					t.Fatalf("login mounted another provider %s", other)
				}
			}
			for _, forbidden := range []string{repo + ":", "compose", "--network", " -p ", "coop-consult", "coop-delegate", "PROJECT_ONLY", "/GEMINI.md", "/CLAUDE.md", "/AGENTS.md", "/.gemini/settings.json:ro"} {
				if strings.Contains(string(data), forbidden) {
					t.Fatalf("login includes %q: %s", forbidden, data)
				}
			}
			if strings.Contains(strings.Join(rendered, "\n"), "must-not-be-mounted") {
				t.Fatal("login generated project instructions")
			}
			if name == "gemini" {
				if !strings.Contains(string(data), "GEMINI_CLI_SYSTEM_SETTINGS_PATH=/home/node/.coop-gemini-login.json") ||
					!strings.Contains(string(data), "NO_BROWSER=true") || !strings.Contains(string(data), "gemini --extensions none") {
					t.Fatalf("Gemini login lacks native isolated settings: %s", data)
				}
				var settings map[string]any
				if len(rendered) != 1 || json.Unmarshal([]byte(rendered[0]), &settings) != nil {
					t.Fatalf("login must render exactly its system settings: %d files", len(rendered))
				}
				mcp, ok := settings["mcp"].(map[string]any)
				allowed, hasAllowed := mcp["allowed"].([]any)
				if !ok || !hasAllowed || len(allowed) != 0 {
					t.Fatal("login must explicitly disable all native MCP servers")
				}
			}
		})
	}
}

func TestLoginManagedConfigFailureStopsBeforeRuntime(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	sentinel := errors.New("fixture failed to write managed config")
	artifacts := defaultCompositionArtifactOps()
	artifacts.writeFile = func(string, string) (string, error) { return "", sentinel }
	code, err := runWithCompositionArtifacts(cfg, recorderRuntime(t, recorder), RunSpec{
		Repo: t.TempDir(), Agent: "gemini", Cmd: []string{"gemini"}, Homes: true, Login: true, Quiet: true, Batch: true,
	}, artifacts)
	if code != -1 || !errors.Is(err, sentinel) {
		t.Fatalf("login = %d, %v", code, err)
	}
	if _, err := os.Stat(recorder); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("runtime launched after managed login config failure")
	}
}

func TestLoginAdmissionIgnoresUnusedMCPAndServices(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	writeCopyFixture(t, filepath.Join(repo, ".agent/project.yaml"), "box:\n  egress: offline\n")
	writeCopyFixture(t, filepath.Join(repo, ".agent/compose.yml"), "services: [broken\n")
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "open", MCPFile: filepath.Join(repo, "invalid-mcp.json")}
	writeCopyFixture(t, cfg.MCPFile, "{invalid")
	spec := RunSpec{Repo: repo, Agent: "gemini", Homes: true, Login: true}
	if capture, err := AdmitNetwork(cfg, runtime.Runtime{}, spec, NetworkAdmission{}); capture != nil || err != nil || cfg.Egress != "none" {
		t.Fatalf("login admission = %v, %v, mode %s; project offline policy must survive", capture, err, cfg.Egress)
	}
	spec.Login = false
	if _, err := AdmitNetwork(cfg, runtime.Runtime{}, spec, NetworkAdmission{}); err == nil {
		t.Fatal("ordinary admission ignored invalid project setup")
	}
}

func TestFilteredLoginRefusesServicePolicyBeforeProjectSetup(t *testing.T) {
	store, err := networkstate.Open(filepath.Join(t.TempDir(), "network"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rules := []egress.Rule{{To: egress.Destination{Service: "db"}, Protocol: "tcp", Ports: []int{5432}}}
	services := map[string]string{"db": strings.Repeat("a", 64)}
	review, err := store.ReviewApproval(repo, egress.Filtered, rules, nil, services)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(t.Context(), repo, egress.Filtered, rules, nil, services, review.Digest); err != nil {
		t.Fatal(err)
	}
	policy, err := store.Admit(repo, networkstate.Admission{Requests: rules, Services: services})
	if err != nil {
		t.Fatal(err)
	}
	capture := &CapturedEgress{Store: store, Project: repo, Fingerprint: policy.Fingerprint}
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	f, err := prepareFilteredExecution(t.Context(), &config.Config{Egress: "filtered"}, recorderRuntime(t, recorder),
		RunSpec{Repo: repo, Login: true}, capture, "must-not-read-compose.yml", nil, nil)
	if f != nil || err == nil || !strings.Contains(err.Error(), "sign-in does not start project services") {
		t.Fatalf("filtered login must refuse a service policy before setup: %v, %v", f, err)
	}
	if _, err := os.Stat(recorder); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("filtered login reached the runtime with a service-bearing policy")
	}
}
