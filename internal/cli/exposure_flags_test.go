package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestExtractExposureFlags(t *testing.T) {
	mode, rest, err := extractExposureFlags([]string{"--peer", "codex", "--readonly", "-p", "hi", "--", "--bare"})
	if err != nil || mode != agents.ModeReadOnly || !slices.Equal(rest, []string{"--peer", "codex", "-p", "hi", "--", "--bare"}) {
		t.Fatalf("readonly parse = %q, %q, %v", mode, rest, err)
	}
	mode, rest, err = extractExposureFlags([]string{"--bare"})
	if err != nil || mode != agents.ModeBare || len(rest) != 0 {
		t.Fatalf("bare parse = %q, %q, %v", mode, rest, err)
	}
	mode, rest, err = extractExposureFlags([]string{"-p", "hi"})
	if err != nil || mode != agents.ModeNormal || !slices.Equal(rest, []string{"-p", "hi"}) {
		t.Fatalf("normal parse = %q, %q, %v", mode, rest, err)
	}
	for _, args := range [][]string{{"--readonly", "--bare"}, {"--bare", "--readonly"}, {"--bare", "--bare"}, {"--readonly", "-p", "x", "--readonly"}} {
		if _, _, err := extractExposureFlags(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
}

// dockerShim is a runtime binary named docker whose every invocation is recorded, so the
// restricted launch resolves the qualified runtime and the image check passes.
func dockerShim(t *testing.T, recorder string) runtime.Runtime {
	t.Helper()
	shim := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + strconv.Quote(recorder) + "\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: shim}
}

func restrictedApp(t *testing.T, recorder string) *app {
	t.Helper()
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", BaseImage: "coop-box", Homes: true, Egress: "open"}
	profile := cfg.AgentDir("claude")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	login := `{"claudeAiOauth":{"accessToken":"access","expiresAt":4102444800000,"scopes":["user:inference"],"refreshToken":"REFRESH_CANARY"}}`
	if err := os.WriteFile(filepath.Join(profile, ".credentials.json"), []byte(login), 0o600); err != nil {
		t.Fatal(err)
	}
	return &app{cfg: cfg, rt: dockerShim(t, recorder), rtSet: true}
}

func recordedRunLine(t *testing.T, recorder string) string {
	t.Helper()
	data, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "run ") {
			return line
		}
	}
	t.Fatalf("no run recorded:\n%s", data)
	return ""
}

// `coop claude --bare` works outside any Git repository: the cwd is never resolved, let alone
// mounted, and the provider's own no-tools switch rides the command.
func TestLaunchAgentBareRunsOutsideAnyRepository(t *testing.T) {
	outside := t.TempDir()
	t.Chdir(outside)
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := restrictedApp(t, recorder)
	if code, err := a.launchAgent("claude", []string{"--bare", "--", "-p", "hi"}); err != nil || code != 0 {
		t.Fatalf("launchAgent = (%d, %v), want (0, nil)", code, err)
	}
	line := recordedRunLine(t, recorder)
	for _, want := range []string{"--read-only", "--tmpfs /workspace:", "-w /workspace", ":/coop/seed:ro", "--tools  --append-system-prompt", "--strict-mcp-config --setting-sources user", "-p hi"} {
		if !strings.Contains(line, want) {
			t.Errorf("bare run missing %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, outside+":") {
		t.Errorf("bare run mounted the working directory:\n%s", line)
	}
	if a.mode != agents.ModeBare {
		t.Errorf("mode = %q", a.mode)
	}
}

// `coop claude --readonly` mounts the resolved repository read-only under the same profile.
func TestLaunchAgentReadOnlyMountsRepoReadOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // network admission's authority root, never the real one
	repo := t.TempDir()
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := restrictedApp(t, recorder)
	a.cfg.RepoOverride = repo
	if code, err := a.launchAgent("claude", []string{"--readonly"}); err != nil || code != 0 {
		t.Fatalf("launchAgent = (%d, %v), want (0, nil)", code, err)
	}
	line := recordedRunLine(t, recorder)
	for _, want := range []string{"--read-only", "-v " + repo + ":" + repo + ":ro", "-w " + repo, ":/coop/seed:ro", "--strict-mcp-config --setting-sources user"} {
		if !strings.Contains(line, want) {
			t.Errorf("readonly run missing %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "--tools") || strings.Contains(line, "coop-cache:") || strings.Contains(line, "coop-asdf:") {
		t.Errorf("readonly run keeps its tools and mounts no volume:\n%s", line)
	}
}

// Every contradiction is a usage error reported before any runtime work.
func TestRestrictedLaunchUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		run  func(a *app) (int, error)
		want string
	}{
		{"both flags", func(a *app) (int, error) { return a.launchAgent("claude", []string{"--readonly", "--bare"}) }, "exclusive"},
		{"login", func(a *app) (int, error) { return a.launchAgent("claude", []string{"--bare", "login"}) }, "login"},
		{"peers", func(a *app) (int, error) {
			a.mode = agents.ModeReadOnly
			return a.runInBoxMode([]string{"claude"}, "claude", []agents.Target{{Provider: "codex"}}, false)
		}, "no peers"},
		{"image override", func(a *app) (int, error) {
			a.cfg.ImageOverride = "custom"
			return a.launchAgent("claude", []string{"--readonly"})
		}, "COOP_IMAGE"},
		{"bare with a domain grant", func(a *app) (int, error) {
			return a.launchAgent("claude", []string{"--bare", "--allow-domain", "example.com"})
		}, "--egress open or none"},
		{"bare filtered", func(a *app) (int, error) {
			return a.launchAgent("claude", []string{"--bare", "--egress", "filtered"})
		}, "--egress open or none"},
		{"run with both", func(a *app) (int, error) { return a.cmdRun([]string{"--bare", "--readonly", "--", "ls"}) }, "exclusive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recorder := filepath.Join(t.TempDir(), "runtime-args")
			a := restrictedApp(t, recorder)
			a.rt, a.rtSet = runtime.Runtime{}, false // a usage error never reaches runtime detection
			code, err := c.run(a)
			if code != 2 || err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("= (%d, %v), want (2, error naming %q)", code, err, c.want)
			}
			if _, statErr := os.Stat(recorder); statErr == nil {
				t.Fatal("the runtime was invoked")
			}
		})
	}
}

// `coop acp claude --bare` (the session daemon's bare child) serves the adapter under the bare
// profile with no supervisor, no project and the adapter command untouched: its no-tools switch
// rides the session/new the client sends. The run receipt from the daemon's environment becomes
// the box label the daemon reaps by.
func TestCmdACPBareServesTheAdapterWithoutAProject(t *testing.T) {
	t.Chdir(t.TempDir())
	runID := "session-" + strings.Repeat("ab", 12)
	t.Setenv("COOP_SESSION_RUN_ID", runID)
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := restrictedApp(t, recorder)
	if code, err := a.cmdACP([]string{"claude", "--bare"}); err != nil || code != 0 {
		t.Fatalf("cmdACP = (%d, %v), want (0, nil)", code, err)
	}
	line := recordedRunLine(t, recorder)
	for _, want := range []string{"--label coop.run=" + runID, " -i ", "--read-only", "--tmpfs /workspace:", "-w /workspace", ":/coop/seed:ro"} {
		if !strings.Contains(line, want) {
			t.Errorf("bare ACP run missing %q:\n%s", want, line)
		}
	}
	if !strings.HasSuffix(line, " coop-box sh -c cp -R /coop/seed/. \"$1\"/ && shift && exec \"$@\" coop-seed /home/node claude-agent-acp") {
		t.Errorf("bare ACP run must end in the plain adapter command:\n%s", line)
	}
	if strings.Contains(line, "--tools") || strings.Contains(line, "coop.sup=") || strings.Contains(line, "-it") {
		t.Errorf("bare ACP run carries a CLI switch, a supervisor label or a tty:\n%s", line)
	}
}

// The ACP forms refuse by name what they do not offer: --readonly on a plain editor session, a
// preset or peers with --bare, and --bare on a fork (it names no fork).
func TestACPExposureUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		run  func(a *app) (int, error)
		want string
	}{
		{"acp readonly", func(a *app) (int, error) { return a.cmdACP([]string{"claude", "--readonly"}) }, "does not take --readonly"},
		{"acp bare preset", func(a *app) (int, error) { return a.cmdACP([]string{"frontier", "--bare"}) }, "name its lead directly"},
		{"acp bare peer", func(a *app) (int, error) { return a.cmdACP([]string{"claude", "--bare", "--peer", "codex"}) }, "no peers"},
		{"acp bare no target", func(a *app) (int, error) { return a.cmdACP([]string{"--bare"}) }, "name the target"},
		{"acp both", func(a *app) (int, error) { return a.cmdACP([]string{"claude", "--bare", "--readonly"}) }, "exclusive"},
		{"fork acp bare", func(a *app) (int, error) { return a.forkACP("myfork", []string{"claude", "--bare"}) }, "names no fork"},
		{"fork acp readonly peer", func(a *app) (int, error) {
			return a.forkACP("myfork", []string{"claude", "--readonly", "--peer", "codex"})
		}, "no peers"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recorder := filepath.Join(t.TempDir(), "runtime-args")
			a := restrictedApp(t, recorder)
			a.rt, a.rtSet = runtime.Runtime{}, false
			code, err := c.run(a)
			if code != 2 || err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("= (%d, %v), want (2, error naming %q)", code, err, c.want)
			}
			if _, statErr := os.Stat(recorder); statErr == nil {
				t.Fatal("the runtime was invoked")
			}
		})
	}
}

// A restricted raw command is the probe form: `coop run --readonly -- <cmd>` runs it verbatim
// under the profile, with no seed and no agent switch.
func TestCmdRunReadOnlyRawCommand(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := restrictedApp(t, recorder)
	a.cfg.RepoOverride = t.TempDir()
	if code, err := a.cmdRun([]string{"--readonly", "--", "touch", "x"}); err != nil || code != 0 {
		t.Fatalf("cmdRun = (%d, %v), want (0, nil)", code, err)
	}
	line := recordedRunLine(t, recorder)
	if !strings.HasSuffix(line, " coop-box touch x") || !strings.Contains(line, "--read-only") || strings.Contains(line, "/coop/seed") {
		t.Fatalf("raw readonly run:\n%s", line)
	}
}
