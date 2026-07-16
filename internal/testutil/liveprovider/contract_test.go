package liveprovider

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/liveprocess"
	coopruntime "github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func TestParseTargets(t *testing.T) {
	all, strict, err := ParseTargets("all")
	if err != nil || !strict {
		t.Fatalf("ParseTargets(all) = %v, %v, %v", all, strict, err)
	}
	allNames := make([]string, 0, len(all))
	for _, target := range all {
		allNames = append(allNames, target.Provider)
	}
	if !slices.Equal(allNames, agents.Names()) {
		t.Errorf("all providers = %v, want registry %v", allNames, agents.Names())
	}

	targets, strict, err := ParseTargets(" claude:opus/high@work , codex:x/high , gemini , grok:grok-4.5/max@personal ")
	if err != nil || strict {
		t.Fatalf("explicit ParseTargets = %v, %v, %v", targets, strict, err)
	}
	want := []string{"claude:opus/high@work", "codex:x/high", "gemini", "grok:grok-4.5/max@personal"}
	got := make([]string, 0, len(targets))
	for _, target := range targets {
		got = append(got, target.String())
	}
	if !slices.Equal(got, want) {
		t.Errorf("explicit targets = %v, want %v", got, want)
	}

	for _, raw := range []string{
		"", "all,codex", "codex,codex@work", "unknown", "codex@work,personal", "gemini/high",
	} {
		if _, _, err := ParseTargets(raw); err == nil {
			t.Errorf("ParseTargets(%q) succeeded", raw)
		}
	}
}

func TestSummaryContract(t *testing.T) {
	requested := []agents.Target{{Provider: "claude"}, {Provider: "codex"}}
	standard, err := NewSummary(false, requested, []ProviderResult{
		{Provider: "claude", CLIVersion: "claude-cli 1.2.3", Attempted: true, Passed: true, Status: StatusPassed},
		{Provider: "codex", Status: StatusSkipped, ReasonCode: ReasonMissingCredential},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !standard.Success() {
		t.Fatal("standard summary rejected a prerequisite skip")
	}
	if got, want := standard.Totals, (SummaryTotals{Requested: 2, Attempted: 1, Passed: 1, Skipped: 1}); got != want {
		t.Errorf("totals = %+v, want %+v", got, want)
	}
	line, err := standard.Line()
	if err != nil || !strings.HasPrefix(line, SummaryPrefix+`{"schema":1`) || strings.Count(line, "\n") != 0 {
		t.Errorf("summary line = %q, %v", line, err)
	}

	failed, err := NewSummary(false, requested[:1], []ProviderResult{{
		Provider: "claude", Attempted: true, Status: StatusFailed, ReasonCode: ReasonPromptExit,
		Phase: "prompt", ErrorClass: "process",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if failed.Success() {
		t.Fatal("attempted provider failure produced a successful summary")
	}

	all, _, err := ParseTargets("all")
	if err != nil {
		t.Fatal(err)
	}
	passing := make([]ProviderResult, 0, len(all))
	for _, target := range all {
		passing = append(passing, ProviderResult{
			Provider: target.Provider, CLIVersion: target.Provider + "-cli 1.2.3",
			Attempted: true, Passed: true, Status: StatusPassed,
		})
	}
	strict, err := NewSummary(true, all, passing)
	if err != nil || !strict.Success() {
		t.Fatalf("strict passing summary = %+v, %v", strict, err)
	}
	passing[0] = ProviderResult{Provider: all[0].Provider, Status: StatusSkipped, ReasonCode: ReasonMissingCLI}
	strict, err = NewSummary(true, all, passing)
	if err != nil {
		t.Fatal(err)
	}
	if strict.Success() {
		t.Fatal("strict summary accepted a skip")
	}
	if err := ValidateStrictTargets(true, all[:len(all)-1]); err == nil {
		t.Fatal("strict target validation accepted an incomplete paid target set")
	}

	invalid := [][]ProviderResult{
		{{Provider: "claude", Status: StatusSkipped, ReasonCode: ReasonPromptTimeout}},
		{{Provider: "claude", Passed: true, Status: StatusPassed}},
		{{Provider: "claude", Attempted: true, Passed: true, Status: StatusPassed}},
		{{Provider: "claude", Attempted: true, Passed: true, Status: StatusFailed, ReasonCode: ReasonPromptExit, Phase: "prompt", ErrorClass: "process"}},
		{{Provider: "claude", Attempted: true, Status: StatusFailed, ReasonCode: ReasonPromptExit, Phase: "Prompt", ErrorClass: "process"}},
		{{Provider: "claude", Attempted: true, Status: StatusFailed, ReasonCode: "TOKEN_CANARY", Phase: "prompt", ErrorClass: "process"}},
		{{Provider: "claude", Status: StatusFailed, ReasonCode: ReasonPromptExit, Phase: "prompt", ErrorClass: "process"}},
		{{Provider: "claude", Attempted: true, Status: StatusFailed, ReasonCode: ReasonPromptTimeout, Phase: "prompt", ErrorClass: "timeout"}},
		{{Provider: "claude", Status: StatusFailed, ReasonCode: ReasonCleanupFailed, Phase: "harness", ErrorClass: "harness"}},
	}
	for _, results := range invalid {
		if _, err := NewSummary(false, requested[:1], results); err == nil {
			t.Errorf("inconsistent summary result was accepted: %+v", results)
		}
	}
}

func TestConsultSummaryContract(t *testing.T) {
	targets, _, err := ParseTargets("all")
	if err != nil {
		t.Fatal(err)
	}
	results := make([]ProviderResult, 0, len(targets))
	for _, target := range targets {
		provider := target.Provider
		results = append(results, ProviderResult{
			Provider: provider, CLIVersion: provider + "-cli 1.2.3",
			Attempted: true, Passed: true, Status: StatusPassed,
		})
	}
	strict, err := NewConsultSummary(true, targets, results)
	if err != nil || !strict.Success() {
		t.Fatalf("strict consult summary = %+v, %v", strict, err)
	}
	for i, edge := range strict.Results {
		wantLead := targets[(i+len(targets)-1)%len(targets)].Provider
		if edge.Lead != wantLead || edge.Peer != results[i] || edge.Lead == edge.Peer.Provider {
			t.Fatalf("consult edge %d = %+v, want %s -> %s", i, edge, wantLead, results[i].Provider)
		}
	}
	line, err := strict.Line()
	if err != nil || !strings.HasPrefix(line, ConsultSummaryPrefix+`{"schema":1`) || strings.Count(line, "\n") != 0 {
		t.Fatalf("consult summary line = %q, %v", line, err)
	}

	results[0] = ProviderResult{
		Provider: results[0].Provider, CLIVersion: results[0].CLIVersion,
		Status: StatusSkipped, ReasonCode: ReasonMissingCredential,
	}
	standard, err := NewConsultSummary(false, targets, results)
	if err != nil || !standard.Success() {
		t.Fatalf("standard consult summary rejected prerequisite skip: %+v, %v", standard, err)
	}
	strict, err = NewConsultSummary(true, targets, results)
	if err != nil {
		t.Fatal(err)
	}
	if strict.Success() {
		t.Fatal("strict consult summary accepted a skip")
	}

	invalid := append([]ProviderResult(nil), results...)
	invalid[0].Provider = "gemini"
	if _, err := NewConsultSummary(false, targets, invalid); err == nil {
		t.Fatal("consult summary accepted the wrong provider order")
	}
	if err := ValidateConsultTargets(targets[:len(targets)-1]); err == nil {
		t.Fatal("consult probe accepted an incomplete target set")
	}
}

func TestChildEnvironmentIsAllowlistOnly(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "AMBIENT_TOKEN_CANARY")
	t.Setenv("COOP_CLAUDE_CMD", "AMBIENT_CMD_CANARY")
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	revokePath := filepath.Join(filepath.Dir(layout.Config), ".coop-live-revoked-00000000000000000000000000000000")
	env, err := ChildEnvironment(layout, ChildSpec{
		Path: "/safe/bin", Target: "codex@work", Marker: "MARKER", ResultFile: filepath.Join(layout.State, "result.json"),
		AttemptFile: filepath.Join(layout.State, "attempted"), Supervisor: "supervisor", ControlFD: 3, RevokePath: revokePath,
		Runtime: RuntimeSettings{
			Name: "docker", Image: "image", BaseImage: "base", HomeInBox: "/home/node", AgentPackages: "packages",
			ConnectionEnv: map[string]string{"DOCKER_HOST": "unix:///safe/runtime.sock", "CONTAINER_HOST": "FORBIDDEN_RUNTIME_CANARY"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.IsSorted(env) {
		t.Fatal("child environment is not deterministic")
	}
	joined := strings.Join(env, "\n")
	for _, required := range []string{
		"COOP_CONFIG_DIR=" + layout.Config, "COOP_EGRESS=open", "COOP_HOMES=1",
		"COOP_MCP_FILE=" + filepath.Join(layout.Config, "missing-mcp.json"),
		"COOP_RUNTIME=docker", "COOP_IMAGE=image", "COOP_TEST_LIVE_TARGET=codex@work",
		liveprocess.ControlFDEnv + "=3", liveprocess.RevokePathEnv + "=" + revokePath,
		"GIT_CONFIG_GLOBAL=" + layout.GitConfig, "HOME=" + layout.Home,
		"XDG_CONFIG_HOME=" + layout.XDGConfig, "DOCKER_HOST=unix:///safe/runtime.sock",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("child environment missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"OPENAI_API_KEY", "AMBIENT_TOKEN_CANARY", "COOP_CLAUDE_CMD", "AMBIENT_CMD_CANARY",
		"CONTAINER_HOST", "FORBIDDEN_RUNTIME_CANARY",
	} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("child environment inherited %q", forbidden)
		}
	}
	if _, err := ChildEnvironment(layout, ChildSpec{Path: "bad\x00path"}); err == nil {
		t.Fatal("NUL environment value was accepted")
	}
	if _, err := ChildEnvironment(layout, ChildSpec{ControlFD: 3}); err == nil {
		t.Fatal("process control without a retryable revocation path was accepted")
	}
	if _, err := ChildEnvironment(layout, ChildSpec{
		RevokePath: filepath.Join(filepath.Dir(layout.Config), ".coop-live-revoked-00000000000000000000000000000000"),
	}); err == nil {
		t.Fatal("credential revocation path without process control was accepted")
	}
	if _, err := ChildEnvironment(layout, ChildSpec{Runtime: RuntimeSettings{
		Name: "docker", ConnectionEnv: map[string]string{"DOCKER_HOST": "bad\x00socket"},
	}}); err == nil {
		t.Fatal("NUL runtime connection value was accepted")
	}
}

func TestConsultChildEnvironmentIsFixedAndAllowlistOnly(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "AMBIENT_TOKEN_CANARY")
	t.Setenv("COOP_GROK_CMD", "AMBIENT_CMD_CANARY")
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	attemptDir := filepath.Join(layout.State, "consult-attempts")
	cidDir := filepath.Join(layout.State, "consult-cids")
	for _, dir := range []string{attemptDir, cidDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	targets, _, err := ParseTargets("all")
	if err != nil {
		t.Fatal(err)
	}
	revokePath := filepath.Join(filepath.Dir(layout.Config), ".coop-live-revoked-00000000000000000000000000000000")
	spec := ConsultChildSpec{
		Path: "/safe/bin", Marker: "CONSULT_MARKER_123", Targets: targets,
		ResultFile: filepath.Join(layout.State, "consult-result.json"), AttemptDir: attemptDir,
		Supervisor: "consult-live-123", CIDDir: cidDir, ControlFD: 3, RevokePath: revokePath,
		PreflightReasons: map[string]string{"claude": ReasonCredentialRefresh},
		Runtime: RuntimeSettings{
			Name: "docker", ConnectionEnv: map[string]string{"DOCKER_HOST": "unix:///safe/runtime.sock"},
		},
	}
	env, err := ConsultChildEnvironment(layout, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.IsSorted(env) {
		t.Fatal("consult child environment is not deterministic")
	}
	joined := strings.Join(env, "\n")
	for _, required := range []string{
		"COOP_TEST_CONSULT_LIVE_CHILD=1",
		"COOP_TEST_CONSULT_LIVE_TARGETS=claude,codex,gemini,grok",
		"COOP_TEST_CONSULT_LIVE_PREFLIGHT_CLAUDE=" + ReasonCredentialRefresh,
		liveprocess.ControlFDEnv + "=3", liveprocess.RevokePathEnv + "=" + revokePath,
		"DOCKER_HOST=unix:///safe/runtime.sock",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("consult child environment missing %q", required)
		}
	}
	for _, forbidden := range []string{"ANTHROPIC_API_KEY", "AMBIENT_TOKEN_CANARY", "COOP_GROK_CMD", "AMBIENT_CMD_CANARY"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("consult child environment inherited %q", forbidden)
		}
	}

	invalid := []ConsultChildSpec{
		func() ConsultChildSpec { value := spec; value.Targets = targets[:3]; return value }(),
		func() ConsultChildSpec { value := spec; value.Marker = "bad marker"; return value }(),
		func() ConsultChildSpec { value := spec; value.Supervisor = "bad supervisor"; return value }(),
		func() ConsultChildSpec {
			value := spec
			value.ResultFile = filepath.Join(layout.State, "other.json")
			return value
		}(),
		func() ConsultChildSpec { value := spec; value.ControlFD = 0; return value }(),
		func() ConsultChildSpec {
			value := spec
			value.PreflightReasons = map[string]string{"codex": ReasonPromptExit}
			return value
		}(),
		func() ConsultChildSpec {
			value := spec
			value.PreflightReasons = map[string]string{"unknown": ReasonMissingCredential}
			return value
		}(),
	}
	for _, value := range invalid {
		if _, err := ConsultChildEnvironment(layout, value); err == nil {
			t.Errorf("invalid consult child contract was accepted: %+v", value)
		}
	}
}

func TestProcessEnvironmentAddsOnlyAValidatedSupervisorLabel(t *testing.T) {
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	control, processDir, err := NewProcessControl(layout, true)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	env, err := ProcessEnvironment(layout, "/safe/bin", RuntimeSettings{Name: "docker"}, ProcessSpec{
		Supervisor: "acp-live-123", ProcessDir: processDir, ControlFD: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "COOP_RUN_ARGS=--label "+SupervisorLabelKey+"=acp-live-123") {
		t.Fatalf("process environment missing supervisor label: %s", joined)
	}
	for _, required := range []string{
		liveprocess.ControlFDEnv + "=3", liveprocess.ProcessDirEnv + "=" + processDir,
		liveprocess.CleanupIDEnv + "=acp-live-123",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("process environment missing %q: %s", required, joined)
		}
	}
	if _, err := ProcessEnvironment(layout, "/safe/bin", RuntimeSettings{Name: "docker"}, ProcessSpec{Supervisor: "bad label"}); err == nil {
		t.Fatal("unsafe supervisor label was accepted")
	}
	if _, err := ProcessEnvironment(layout, "/safe/bin", RuntimeSettings{Name: "docker"}, ProcessSpec{ProcessDir: processDir}); err == nil {
		t.Fatal("process registry without authenticated descriptor was accepted")
	}
}

func TestCaptureRuntimeConnectionEnvUsesExactRuntimeAllowlist(t *testing.T) {
	ambient := map[string]string{
		"DOCKER_HOST": "unix:///docker.sock", "DOCKER_CONFIG": "/unsafe/docker-config",
		"DOCKER_AUTH_CONFIG": "AUTH_CANARY", "CONTAINERS_CONF": "/unsafe/containers.conf",
		"PODMAN_CONNECTIONS_CONF": "/unsafe/connections.json",
		"CONTAINER_HOST":          "ssh://podman.example/run/podman.sock", "CONTAINER_SSHKEY": "/safe/podman-key",
		"CONTAINERS_STORAGE_CONF": "/safe/storage.conf", "XDG_DATA_HOME": "/safe/data",
		"XDG_RUNTIME_DIR": "/safe/run", "SSH_AUTH_SOCK": "/safe/agent.sock",
		"HOME": "/FORBIDDEN_HOME", "OPENAI_API_KEY": "TOKEN_CANARY",
	}
	lookup := func(key string) (string, bool) {
		value, ok := ambient[key]
		return value, ok
	}

	unexpectedQuery := func(...string) ([]byte, error) {
		t.Fatal("explicit endpoint unexpectedly queried runtime config")
		return nil, nil
	}
	docker, err := captureRuntimeConnectionEnv("/usr/local/bin/docker", lookup, func(string) bool { return false }, unexpectedQuery)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{
		"DOCKER_HOST": "unix:///docker.sock", "SSH_AUTH_SOCK": "/safe/agent.sock",
	}; !mapsEqual(docker, want) {
		t.Errorf("Docker connection env = %v, want %v", docker, want)
	}
	podman, err := captureRuntimeConnectionEnv("podman", lookup, func(string) bool { return false }, unexpectedQuery)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"CONTAINER_HOST": "ssh://podman.example/run/podman.sock", "CONTAINER_SSHKEY": "/safe/podman-key",
		"CONTAINERS_STORAGE_CONF": "/safe/storage.conf", "XDG_DATA_HOME": "/safe/data",
		"XDG_RUNTIME_DIR": "/safe/run", "SSH_AUTH_SOCK": "/safe/agent.sock",
	} {
		if podman[key] != want {
			t.Errorf("Podman connection env %s = %q, want %q", key, podman[key], want)
		}
	}
	for _, forbidden := range []string{
		"DOCKER_AUTH_CONFIG", "DOCKER_CONFIG", "DOCKER_CONTEXT", "CONTAINERS_CONF",
		"PODMAN_CONNECTIONS_CONF", "CONTAINER_CONNECTION", "HOME", "OPENAI_API_KEY",
	} {
		if _, ok := podman[forbidden]; ok {
			t.Errorf("Podman connection env retained %s", forbidden)
		}
		if _, ok := docker[forbidden]; ok {
			t.Errorf("Docker connection env retained %s", forbidden)
		}
	}
	if _, ok := podman["DOCKER_HOST"]; ok {
		t.Error("Podman connection env retained DOCKER_HOST")
	}
	if _, ok := docker["CONTAINER_HOST"]; ok {
		t.Error("Docker connection env retained CONTAINER_HOST")
	}
	if got, err := captureRuntimeConnectionEnv("container", lookup, func(string) bool { return false }, unexpectedQuery); err != nil || len(got) != 0 {
		t.Errorf("Apple container connection env = %v, want empty", got)
	}

	derivedAmbient := map[string]string{
		"HOME": "/parent/home", "XDG_RUNTIME_DIR": "/parent/run", "SSH_AUTH_SOCK": "/parent/ssh.sock",
		"DOCKER_CONTEXT": "remote", "CONTAINER_CONNECTION": "machine",
	}
	derivedLookup := func(key string) (string, bool) {
		value, ok := derivedAmbient[key]
		return value, ok
	}
	existing := map[string]bool{
		"/parent/docker-tls/docker":                    true,
		"/parent/home/.config/containers/storage.conf": true,
		"/parent/home/.local/share":                    true,
	}
	exists := func(path string) bool { return existing[path] }
	query := func(args ...string) ([]byte, error) {
		switch strings.Join(args, " ") {
		case "context inspect remote":
			return []byte(`[{"Endpoints":{"docker":{"Host":"ssh://docker.example/run.sock","SkipTLSVerify":false}},"TLSMaterial":{},"Storage":{"TLSPath":""}}]`), nil
		case "system connection list --format json":
			return []byte(`[{"Name":"machine","URI":"ssh://core@podman.example/run/podman.sock","Identity":"/parent/podman-key","Default":true}]`), nil
		default:
			return nil, errors.New("unexpected runtime query")
		}
	}
	if got, err := captureRuntimeConnectionEnv("docker", derivedLookup, exists, query); err != nil || !mapsEqual(got, map[string]string{
		"DOCKER_HOST": "ssh://docker.example/run.sock", "SSH_AUTH_SOCK": "/parent/ssh.sock",
	}) {
		t.Errorf("resolved Docker connection env = %v, %v", got, err)
	}
	if got, err := captureRuntimeConnectionEnv("podman", derivedLookup, exists, query); err != nil || !mapsEqual(got, map[string]string{
		"CONTAINER_HOST":          "ssh://core@podman.example/run/podman.sock",
		"CONTAINER_SSHKEY":        "/parent/podman-key",
		"CONTAINERS_STORAGE_CONF": "/parent/home/.config/containers/storage.conf",
		"XDG_DATA_HOME":           "/parent/home/.local/share", "XDG_RUNTIME_DIR": "/parent/run",
		"SSH_AUTH_SOCK": "/parent/ssh.sock",
	}) {
		t.Errorf("resolved Podman connection env = %v, %v", got, err)
	}

	tlsLookup := func(key string) (string, bool) {
		if key == "DOCKER_CONTEXT" {
			return "tls", true
		}
		return "", false
	}
	tlsQuery := func(...string) ([]byte, error) {
		return []byte(`[{"Endpoints":{"docker":{"Host":"tcp://docker.example:2376","SkipTLSVerify":false}},"TLSMaterial":{"docker":["ca.pem","cert.pem","key.pem"]},"Storage":{"TLSPath":"/parent/docker-tls"}}]`), nil
	}
	if got, err := captureRuntimeConnectionEnv("docker", tlsLookup, exists, tlsQuery); err != nil || !mapsEqual(got, map[string]string{
		"DOCKER_HOST": "tcp://docker.example:2376", "DOCKER_TLS": "1",
		"DOCKER_TLS_VERIFY": "1", "DOCKER_CERT_PATH": "/parent/docker-tls/docker",
	}) {
		t.Errorf("resolved Docker TLS env = %v, %v", got, err)
	}
	unsafePodmanQuery := func(...string) ([]byte, error) {
		return []byte(`[{"Name":"machine","URI":"tcp://podman.example:8443","Default":true,"TLSCA":"/secret/ca.pem"}]`), nil
	}
	if _, err := captureRuntimeConnectionEnv("podman", derivedLookup, exists, unsafePodmanQuery); err == nil {
		t.Fatal("Podman TLS connection config was forwarded without a safe projection")
	}
}

func TestRuntimeConnectionEnvReachesRuntimeButNotContainerArgs(t *testing.T) {
	for _, runtimeName := range []string{"docker", "podman"} {
		t.Run(runtimeName, func(t *testing.T) {
			for _, key := range append(append([]string(nil), runtimeConnectionKeys["docker"]...), runtimeConnectionKeys["podman"]...) {
				t.Setenv(key, "")
			}
			for _, key := range []string{"DOCKER_CONFIG", "DOCKER_CONTEXT", "CONTAINERS_CONF", "PODMAN_CONNECTIONS_CONF", "CONTAINER_CONNECTION"} {
				t.Setenv(key, "")
			}
			layout, err := procharness.NewLayout(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			parentHome := t.TempDir()
			parentRun := filepath.Join(t.TempDir(), "run")
			sshSocket := filepath.Join(t.TempDir(), "ssh-agent.sock")
			unsafeConfig := filepath.Join(t.TempDir(), "behavior-config")
			t.Setenv("HOME", parentHome)
			t.Setenv("XDG_RUNTIME_DIR", parentRun)
			t.Setenv("SSH_AUTH_SOCK", sshSocket)
			if runtimeName == "docker" {
				if err := os.MkdirAll(unsafeConfig, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(unsafeConfig, "config.json"), []byte(`{"proxies":{"default":{"httpProxy":"http://PROXY_TOKEN_CANARY@example"}}}`), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("DOCKER_CONFIG", unsafeConfig)
				t.Setenv("DOCKER_HOST", "unix:///RUNTIME_SENTINEL.sock")
			} else {
				storage := filepath.Join(parentHome, ".config", "containers", "storage.conf")
				if err := os.MkdirAll(filepath.Dir(storage), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(storage, []byte("[storage]\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(unsafeConfig, []byte("[containers]\nenv=[\"PROXY_TOKEN_CANARY=value\"]\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CONTAINERS_CONF", unsafeConfig)
				t.Setenv("PODMAN_CONNECTIONS_CONF", unsafeConfig)
				t.Setenv("CONTAINER_HOST", "unix:///RUNTIME_SENTINEL.sock")
				if err := os.MkdirAll(filepath.Join(parentHome, ".local", "share"), 0o700); err != nil {
					t.Fatal(err)
				}
			}

			runtimePath := filepath.Join(t.TempDir(), runtimeName)
			script := `#!/bin/sh
	if [ "$1" = "run" ]; then
	  if [ -n "$DOCKER_CONFIG$DOCKER_CONTEXT$CONTAINERS_CONF$PODMAN_CONNECTIONS_CONF$CONTAINER_CONNECTION" ]; then exit 88; fi
	  printf 'home=%s\n' "$HOME" > "$COOP_TEST_LIVE_RESULT"
	  for key in DOCKER_HOST DOCKER_TLS DOCKER_TLS_VERIFY DOCKER_CERT_PATH CONTAINER_HOST CONTAINER_SSHKEY CONTAINERS_STORAGE_CONF XDG_DATA_HOME XDG_RUNTIME_DIR SSH_AUTH_SOCK; do
	    eval "value=\${$key}"
	    printf 'env_%s=%s\n' "$key" "$value" >> "$COOP_TEST_LIVE_RESULT"
  done
  shift
  for arg in "$@"; do printf 'arg=%s\n' "$arg" >> "$COOP_TEST_LIVE_RESULT"; done
fi
exit 0
`
			if err := os.WriteFile(runtimePath, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			connection, err := CaptureRuntimeConnectionEnv(runtimePath)
			if err != nil {
				t.Fatal(err)
			}
			resultFile := filepath.Join(layout.State, "runtime-result")
			env, err := ChildEnvironment(layout, ChildSpec{
				Path: os.Getenv("PATH"), Target: "codex", Marker: "marker", ResultFile: resultFile,
				AttemptFile: filepath.Join(layout.State, "attempt"), Supervisor: "supervisor",
				Runtime: RuntimeSettings{Name: runtimePath, ConnectionEnv: connection},
			})
			if err != nil {
				t.Fatal(err)
			}
			child := exec.Command(os.Args[0], "-test.run=^TestRuntimeConnectionEnvChild$")
			child.Dir, child.Env = layout.Repo, env
			if output, err := child.CombinedOutput(); err != nil {
				t.Fatalf("runtime environment child failed: %v (%s)", err, output)
			}
			data, err := os.ReadFile(resultFile)
			if err != nil {
				t.Fatal(err)
			}
			got := string(data)
			if !strings.Contains(got, "home="+layout.Home+"\n") {
				t.Fatalf("child HOME was not isolated: %q", got)
			}
			for key, value := range connection {
				if !strings.Contains(got, "env_"+key+"="+value+"\n") {
					t.Errorf("runtime process did not receive %s", key)
				}
			}
			for _, line := range strings.Split(got, "\n") {
				if !strings.HasPrefix(line, "arg=") {
					continue
				}
				for key, value := range connection {
					if strings.Contains(line, key) || strings.Contains(line, value) {
						t.Fatalf("runtime connection authority was forwarded into container args: %q", line)
					}
				}
				if strings.Contains(line, parentHome) || strings.Contains(line, sshSocket) {
					t.Fatalf("parent runtime path was forwarded into container args: %q", line)
				}
			}
			for _, forbidden := range []string{"DOCKER_CONFIG", "DOCKER_CONTEXT", "CONTAINERS_CONF", "PODMAN_CONNECTIONS_CONF", "CONTAINER_CONNECTION", "PROXY_TOKEN_CANARY"} {
				if strings.Contains(got, forbidden+"=") || strings.Contains(got, "env_"+forbidden) {
					t.Fatalf("behavior-bearing runtime config reached scrubbed child: %s", forbidden)
				}
			}
		})
	}
}

func TestRuntimeConnectionEnvChild(t *testing.T) {
	if os.Getenv("COOP_TEST_LIVE_CHILD") != "1" {
		t.Skip("helper runs only in the scrubbed runtime environment child")
	}
	cfg := config.Load()
	rt, err := coopruntime.Detect(cfg.RuntimeName)
	if err != nil {
		t.Fatal("detect fake runtime")
	}
	if code, err := box.Run(cfg, rt, box.RunSpec{
		Image: "fixture", Repo: cfg.RepoOverride, Cmd: []string{"true"}, Batch: true, Quiet: true,
	}); err != nil || code != 0 {
		t.Fatal("run fake runtime")
	}
}

func mapsEqual(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}

func TestBoundedBuffer(t *testing.T) {
	buffer := NewBoundedBuffer(5)
	if n, err := buffer.Write([]byte("123456789")); err != nil || n != 9 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if got := buffer.String(); got != "12345" || !buffer.Truncated() {
		t.Errorf("bounded buffer = %q truncated=%v", got, buffer.Truncated())
	}
}

func TestCLIVersionEmitsOnlyTrustedLabelAndVersionToken(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		outputs  []string
		want     string
	}{
		{name: "Claude shape", provider: "claude", outputs: []string{"2.1.210 (Claude Code)"}, want: "claude-cli 2.1.210"},
		{name: "Codex shape", provider: "codex", outputs: []string{"codex-cli 0.144.4"}, want: "codex-cli 0.144.4"},
		{name: "Gemini stderr shape", provider: "gemini", outputs: []string{"", "gemini 0.50.0"}, want: "gemini-cli 0.50.0"},
		{name: "Grok prerelease shape", provider: "grok", outputs: []string{"grok version v0.2.101-rc.1"}, want: "grok-cli 0.2.101-rc.1"},
		{name: "control noise is omitted", provider: "codex", outputs: []string{"\x1b[31mCodex\x1b[0m 1.2.3\x00"}, want: "codex-cli 1.2.3"},
		{name: "absolute path is omitted", provider: "gemini", outputs: []string{"/private/tmp/SECRET_CANARY/gemini 1.2.3"}, want: "gemini-cli 1.2.3"},
		{name: "KEY value line rejected", provider: "codex", outputs: []string{"TOKEN_CANARY=1.2.3"}},
		{name: "JWT rejected", provider: "claude", outputs: []string{"eyJhbGciOiJIUzI1NiJ9.eyJleHAiOjE3ODQxMTMzNDR9.signature"}},
		{name: "invalid UTF-8 rejected", provider: "grok", outputs: []string{string([]byte{'1', '.', '2', '.', '3', 0xff})}},
		{name: "unknown provider rejected", provider: "other", outputs: []string{"1.2.3"}},
		{name: "long line rejected", provider: "codex", outputs: []string{strings.Repeat("x", 600) + " 1.2.3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CLIVersion(tt.provider, tt.outputs...); got != tt.want {
				t.Errorf("CLIVersion = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRepositorySnapshotDetectsEveryMutationAxis(t *testing.T) {
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := InitRepository(layout); err != nil {
		t.Fatal(err)
	}
	baseline, err := SnapshotRepository(layout)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Status != "" || baseline.Head == "" || baseline.Reflog == "" || baseline.Refs == "" {
		t.Fatalf("incomplete baseline snapshot: %+v", baseline)
	}
	again, err := SnapshotRepository(layout)
	if err != nil || !baseline.Equal(again) {
		t.Fatalf("stable snapshot changed: equal=%v err=%v", baseline.Equal(again), err)
	}
	gitDir := filepath.Join(layout.Repo, ".git")
	gitConfig := filepath.Join(gitDir, "config")
	gitConfigBefore, err := os.ReadFile(gitConfig)
	if err != nil {
		t.Fatal(err)
	}
	fsmonitor := filepath.Join(gitDir, "coop-fsmonitor")
	fsmonitorRan := filepath.Join(gitDir, "fsmonitor-ran")
	if err := os.WriteFile(fsmonitor, []byte("#!/bin/sh\n: >\"$(dirname \"$0\")/fsmonitor-ran\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	poisonedConfig := append(gitConfigBefore, []byte("\n[core]\n\tfsmonitor = "+strconv.Quote(fsmonitor)+"\n")...)
	if err := os.WriteFile(gitConfig, poisonedConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRepository(layout, baseline); err == nil {
		t.Fatal("Git administrative mutation was accepted")
	}
	if _, err := os.Lstat(fsmonitorRan); !os.IsNotExist(err) {
		t.Fatalf("repository verification executed poisoned fsmonitor hook: %v", err)
	}
	if err := os.WriteFile(gitConfig, gitConfigBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fsmonitor); err != nil {
		t.Fatal(err)
	}

	readme := filepath.Join(layout.Repo, "README.md")
	if err := os.Chmod(readme, 0o700); err != nil {
		t.Fatal(err)
	}
	modeChanged, err := SnapshotRepository(layout)
	if err != nil {
		t.Fatal(err)
	}
	if modeChanged.Tree == baseline.Tree {
		t.Fatal("working-tree mode change was not detected")
	}
	if err := os.Chmod(readme, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.Repo, "new.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	untracked, err := SnapshotRepository(layout)
	if err != nil {
		t.Fatal(err)
	}
	if untracked.Status == baseline.Status || untracked.Tree == baseline.Tree {
		t.Fatal("untracked mutation was not detected")
	}
	if _, err := runGit(layout, "add", "new.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(layout, "commit", "-qm", "mutation"); err != nil {
		t.Fatal(err)
	}
	committed, err := SnapshotRepository(layout)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Head == baseline.Head || committed.Refs == baseline.Refs || committed.Reflog == baseline.Reflog {
		t.Fatal("commit metadata mutation was not detected")
	}
}
