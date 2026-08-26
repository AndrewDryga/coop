package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/acpctl"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/liveprocess"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
)

// coop's "--" separator must be consumed, not forwarded to the agent: `coop claude -- -p x` must
// reach the agent as `-p x`, not `-- -p x` (which the agent reads as positional, dropping the flag).
func TestDropDashDash(t *testing.T) {
	for _, c := range []struct{ in, want []string }{
		{[]string{"-p", "x"}, []string{"-p", "x"}},                           // no --: unchanged
		{[]string{"--", "-p", "x"}, []string{"-p", "x"}},                     // leading -- stripped
		{[]string{"a", "--", "b", "--", "c"}, []string{"a", "b", "--", "c"}}, // only the first --
		{[]string{"--"}, []string{}},                                         // lone --
	} {
		if got := dropDashDash(c.in); !slices.Equal(got, c.want) {
			t.Errorf("dropDashDash(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// The loop's leading positional is a target (provider[:model][/effort][@account]); no positional →
// no target (hasTarget=false) and the provider is required (caller errors or a preset lead
// supplies it). A malformed/unknown token errors; --model/--credential are unexpected args now.
func TestLoopTargetResolution(t *testing.T) {
	if _, has, ps, _, _, _, _, err := parseLoopArgs(nil, false); err != nil || has || ps != "" {
		t.Errorf("parseLoopArgs(nil) = (has=%v, preset=%q, %v), want (false, \"\", nil) — no implicit default", has, ps, err)
	}
	for _, ag := range agents.Names() {
		tg, has, ps, _, _, _, _, err := parseLoopArgs([]string{ag}, false)
		if err != nil || !has || ps != "" || tg.Provider != ag {
			t.Errorf("parseLoopArgs(%q) = (%+v, has=%v, preset=%q, %v), want provider=%q", ag, tg, has, ps, err, ag)
		}
	}
	if tg, has, ps, _, _, _, _, err := parseLoopArgs([]string{"claude:opus-4.8@work"}, false); err != nil || !has || ps != "" ||
		tg.Provider != "claude" || tg.Model != "opus-4.8" || len(tg.Accounts) != 1 || tg.Accounts[0] != "work" {
		t.Errorf("parseLoopArgs(claude:opus-4.8@work) = (%+v, has=%v, preset=%q, %v)", tg, has, ps, err)
	}
	// Keep the documented preset invocation tied to the parser: cmdLoop passes the words after
	// "coop loop" to parseLoopArgs, so its positional preset must remain accepted.
	const documentedPresetLoop = "coop loop frontier"
	words := strings.Fields(documentedPresetLoop)
	if tg, has, ps, _, _, _, _, err := parseLoopArgs(words[2:], false); err != nil || has || ps != "frontier" || tg.Provider != "" {
		t.Errorf("%q = (%+v, has=%v, preset=%q, %v), want positional preset frontier", documentedPresetLoop, tg, has, ps, err)
	}
	// A bare non-target word is a PRESET NAME now (its existence is validated later by
	// loadRunPreset), not an unknown-token error.
	if tg, has, ps, _, _, _, _, err := parseLoopArgs([]string{"frontier"}, false); err != nil || has || ps != "frontier" || tg.Provider != "" {
		t.Errorf("parseLoopArgs(frontier) = (%+v, has=%v, preset=%q, %v), want a preset name and no target", tg, has, ps, err)
	}
	// The model/account ride the target and a preset is the positional, so --model/--credential/
	// --preset are all unexpected args now.
	for _, bad := range [][]string{{"claude", "--model", "opus"}, {"claude", "--credential", "work"}, {"claude", "--preset", "frontier"}} {
		if _, _, _, _, _, _, _, err := parseLoopArgs(bad, false); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
			t.Errorf("parseLoopArgs(%v) should be an unexpected argument, got %v", bad, err)
		}
	}
}

func TestParseLoopArgs(t *testing.T) {
	// --peer is pre-extracted by cmdLoop (see TestExtractPeer), so parseLoopArgs never sees it — it
	// resolves the who-runs positional (a target OR a preset name) + the boolean flags only.
	cases := []struct {
		args          []string
		def           bool // the loop.yaml preflight.enabled default
		wantAgent     string
		wantModel     string
		wantPreset    string
		wantDebug     bool
		wantPreflight bool
		wantNoMCP     bool
		wantMaxTasks  int
		wantErr       bool
	}{
		{args: nil},
		{args: []string{"codex"}, wantAgent: "codex"},
		{args: []string{"--debug-on-fail"}, wantDebug: true},
		{args: []string{"--max-tasks", "1"}, wantMaxTasks: 1},
		{args: []string{"codex", "--max-tasks", "3"}, wantAgent: "codex", wantMaxTasks: 3},
		{args: []string{"--max-tasks"}, wantErr: true},
		{args: []string{"--max-tasks", "0"}, wantErr: true},
		{args: []string{"--max-tasks", "-1"}, wantErr: true},
		{args: []string{"--max-tasks", "many"}, wantErr: true},
		{args: []string{"--max-tasks", "1", "--max-tasks", "2"}, wantErr: true},
		{args: []string{"--once"}, wantErr: true},
		{args: []string{"gemini", "--debug"}, wantErr: true},        // --debug is not a known flag → error
		{args: []string{"--debug-on-fail", "codex"}, wantErr: true}, // a who must LEAD; a trailing positional errors
		// A bare non-target word is a PRESET NAME now (not an unknown-token error).
		{args: []string{"frontier"}, wantPreset: "frontier"},
		{args: []string{"frontier", "--preflight"}, wantPreset: "frontier", wantPreflight: true},
		// preflight: default off, --preflight turns it on, --no-preflight overrides a default-on.
		{args: []string{"--preflight"}, wantPreflight: true},
		{args: []string{"codex", "--preflight"}, wantAgent: "codex", wantPreflight: true},
		{def: true, wantPreflight: true},                                    // preflight.enabled default
		{args: []string{"--no-preflight"}, def: true, wantPreflight: false}, // flag overrides default-on
		// --no-mcp: this run's boxes mount no MCP (the committed form is loop.yaml mcp: false).
		{args: []string{"--no-mcp"}, wantNoMCP: true},
		{args: []string{"claude", "--no-mcp", "--preflight"}, wantAgent: "claude", wantPreflight: true, wantNoMCP: true},
		// The model/account ride the target now; --model/--credential are unexpected args (error).
		{args: []string{"codex:gpt-5"}, wantAgent: "codex", wantModel: "gpt-5"},
		{args: []string{"claude:opus@work"}, wantAgent: "claude", wantModel: "opus"},
		{args: []string{"--model", "haiku"}, wantErr: true},               // unexpected arg
		{args: []string{"claude", "--credential", "work"}, wantErr: true}, // unexpected arg
		{args: []string{"claude", "--preset", "frontier"}, wantErr: true}, // --preset retired → unexpected arg
	}
	for _, c := range cases {
		tg, _, ps, debug, preflight, noMCP, maxTasks, err := parseLoopArgs(c.args, c.def)
		if (err != nil) != c.wantErr {
			t.Errorf("parseLoopArgs(%v) err=%v, wantErr=%v", c.args, err, c.wantErr)
			continue
		}
		if !c.wantErr && (tg.Provider != c.wantAgent || tg.Model != c.wantModel || ps != c.wantPreset || debug != c.wantDebug || preflight != c.wantPreflight || noMCP != c.wantNoMCP || maxTasks != c.wantMaxTasks) {
			t.Errorf("parseLoopArgs(%v, def=%v) = (provider=%q model=%q preset=%q debug=%v preflight=%v noMCP=%v maxTasks=%d), want (%q, %q, %q, %v, %v, %v, %d)",
				c.args, c.def, tg.Provider, tg.Model, ps, debug, preflight, noMCP, maxTasks, c.wantAgent, c.wantModel, c.wantPreset, c.wantDebug, c.wantPreflight, c.wantNoMCP, c.wantMaxTasks)
		}
	}
}

func TestParseGovernor(t *testing.T) {
	a := &app{cfg: &config.Config{}}
	cases := []struct {
		name        string
		args        []string
		wantGov     string
		wantModel   string
		wantProfile string
		wantPreset  string
		wantRest    []string
	}{
		{"no governor named — empty, the caller requires one", nil, "", "", "", "", nil},
		{"positional governor", []string{"claude"}, "claude", "", "", "", nil},
		// The governor is a target: its model + account fold out for the one-off selection.
		{"governor target model+account", []string{"claude:opus-4.8@work"}, "claude", "opus-4.8", "work", "", nil},
		{"positional governor + passthrough", []string{"gemini", "exec"}, "gemini", "", "", "", []string{"exec"}},
		// A leading non-target bare word is the PRESET NAME (the who slot); the rest passes through.
		{"leading preset governs, rest passes through", []string{"frontier", "foo"}, "", "", "", "frontier", []string{"foo"}},
		{"bare preset name", []string{"frontier"}, "", "", "", "frontier", nil},
		{"-- passes the rest through verbatim", []string{"claude", "--", "-p", "hi"}, "claude", "", "", "", []string{"-p", "hi"}},
		{"--governor is gone — treated as passthrough now", []string{"--governor", "claude"}, "", "", "", "", []string{"--governor", "claude"}},
		// A SECOND agent token is NOT swallowed as the governor — only the first is; the rest passes through.
		{"second agent token passes through", []string{"codex", "gemini"}, "codex", "", "", "", []string{"gemini"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gov, model, profile, _, ps, rest, _, err := a.parseGovernor(c.args)
			if err != nil {
				t.Fatalf("parseGovernor(%v) errored: %v", c.args, err)
			}
			if gov != c.wantGov {
				t.Errorf("governor = %q, want %q", gov, c.wantGov)
			}
			if model != c.wantModel {
				t.Errorf("model = %q, want %q", model, c.wantModel)
			}
			if profile != c.wantProfile {
				t.Errorf("profile = %q, want %q", profile, c.wantProfile)
			}
			if ps != c.wantPreset {
				t.Errorf("preset = %q, want %q", ps, c.wantPreset)
			}
			if !slices.Equal(rest, c.wantRest) {
				t.Errorf("rest = %v, want %v", rest, c.wantRest)
			}
		})
	}
}

func TestCmdFusionCrossProviderPresetPinsFirstRungAndWiresRoleCouncil(t *testing.T) {
	repo := t.TempDir()
	presetDir := filepath.Join(repo, ".agent", "presets", "duo")
	if err := os.MkdirAll(presetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presetDir, "preset.yaml"), []byte(`lead:
  agent: [claude:one/high, codex:two/xhigh]
roles:
  critic:
    mode: consult
    agent: gemini
`), 0o644); err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	cfg := &config.Config{
		ConfigDir: configDir, RepoOverride: repo, HomeInBox: "/home/node", BoxHome: t.TempDir(),
		BaseImage: "test-base", ImageOverride: "test-image", Homes: true, Egress: "none",
	}
	if err := os.WriteFile(cfg.EnvFile(), []byte("GEMINI_API_KEY=test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := &app{cfg: cfg, rt: fusionRecordingRuntime(t, recorder), rtSet: true}
	var code int
	var runErr error
	out := captureStderr(t, func() { code, runErr = a.cmdFusion([]string{"duo"}) })
	if runErr != nil || code != 0 {
		t.Fatalf("cmdFusion(duo) = (%d, %v), want success; stderr:\n%s", code, runErr, out)
	}
	for _, want := range []string{"pins", "claude:one/high", "no fallback rotation"} {
		if !strings.Contains(out, want) {
			t.Errorf("terminal Fusion pin notice missing %q:\n%s", want, out)
		}
	}
	args, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"COOP_CONSULT_CRITIC_TARGETS=gemini", ":/usr/local/bin/coop-consult:ro"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("runtime assembly missing %q:\n%s", want, args)
		}
	}
}

func fusionRecordingRuntime(t *testing.T, recorder string) runtime.Runtime {
	t.Helper()
	shim := filepath.Join(t.TempDir(), "runtime")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + strconv.Quote(recorder) + "\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: shim}
}

func composeUpRuntime(t *testing.T, services []string, serviceExit int) runtime.Runtime {
	t.Helper()
	shim := filepath.Join(t.TempDir(), "runtime")
	var script strings.Builder
	script.WriteString("#!/bin/sh\ncase \"$*\" in\n  *\"config --services\"*)\n")
	for _, service := range services {
		script.WriteString("    printf '%s\\n' " + strconv.Quote(service) + "\n")
	}
	if serviceExit != 0 {
		script.WriteString("    exit " + strconv.Itoa(serviceExit) + "\n")
	}
	script.WriteString("    ;;\nesac\n")
	if err := os.WriteFile(shim, []byte(script.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: shim}
}

func TestCmdUpReportsResolvedServiceNames(t *testing.T) {
	for _, tc := range []struct {
		name     string
		services []string
	}{
		{name: "db and keycloak", services: []string{"db", "keycloak"}},
		{name: "different names", services: []string{"api", "worker"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
				t.Fatal(err)
			}
			var compose strings.Builder
			compose.WriteString("services:\n")
			for _, service := range tc.services {
				compose.WriteString("  " + service + ":\n    image: example/" + service + "\n")
			}
			if err := os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"), []byte(compose.String()), 0o644); err != nil {
				t.Fatal(err)
			}
			a := &app{
				cfg:   &config.Config{RepoOverride: repo},
				rt:    composeUpRuntime(t, tc.services, 0),
				rtSet: true,
			}
			var code int
			var runErr error
			out := captureStderr(t, func() { code, runErr = a.cmdUp(nil) })
			if code != 0 || runErr != nil {
				t.Fatalf("cmdUp = (%d, %v), want success; stderr:\n%s", code, runErr, out)
			}
			want := "the box reaches " + strings.Join(tc.services, ", ") + " by name"
			if !strings.Contains(out, want) {
				t.Errorf("cmdUp output missing %q:\n%s", want, out)
			}
			if strings.Contains(out, "redis") {
				t.Errorf("cmdUp invented an unconfigured redis service:\n%s", out)
			}
		})
	}
}

func TestCmdUpStopsWhenServiceDiscoveryFails(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"),
		[]byte("services:\n  db:\n    image: postgres:18\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{
		cfg:   &config.Config{RepoOverride: repo},
		rt:    composeUpRuntime(t, nil, 19),
		rtSet: true,
	}
	var code int
	var runErr error
	out := captureStderr(t, func() { code, runErr = a.cmdUp(nil) })
	if code == 0 || runErr == nil ||
		!strings.Contains(runErr.Error(), "compose config --services exited with code 19") ||
		!strings.Contains(runErr.Error(), "then retry: coop up") {
		t.Fatalf("cmdUp discovery failure = (%d, %v), want named failure", code, runErr)
	}
	if strings.Contains(out, "up on network") {
		t.Errorf("cmdUp printed success after discovery failed:\n%s", out)
	}
}

func TestACPInnerEmptyPresetSelectionClearsPositionalPreset(t *testing.T) {
	t.Setenv("COOP_ACP_INNER", "1")
	t.Setenv("COOP_ACP_PRESET", "")
	t.Setenv("COOP_ACP_TARGET", "claude")

	configDir := t.TempDir()
	cfg := &config.Config{
		ConfigDir: configDir, RepoOverride: t.TempDir(), HomeInBox: "/home/node", BoxHome: t.TempDir(),
		BaseImage: "test-base", ImageOverride: "test-image", Homes: true, Egress: "none",
	}
	if err := os.WriteFile(cfg.EnvFile(), []byte("GEMINI_API_KEY=test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := &app{cfg: cfg, rt: fusionRecordingRuntime(t, recorder), rtSet: true}
	code, err := a.cmdACP([]string{"fusion", "missing-positional-preset", "--peer", "gemini"})
	if err != nil || code != 0 {
		t.Fatalf("inner ACP clear = (%d, %v), want success without loading the positional preset", code, err)
	}
	if a.preset != nil {
		t.Errorf("empty COOP_ACP_PRESET must clear positional preset, retained %+v", a.preset)
	}
}

func TestACPInnerSelectedPresetAndTargetReplaceLaunchState(t *testing.T) {
	t.Setenv("COOP_ACP_INNER", "1")
	t.Setenv("COOP_ACP_PRESET", "selected")
	t.Setenv("COOP_ACP_TARGET", "codex:acp-selected/high")

	repo := t.TempDir()
	presetDir := filepath.Join(repo, ".agent", "presets", "selected")
	if err := os.MkdirAll(presetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presetDir, "preset.yaml"), []byte(`lead:
  agent: [claude:stale-first/high, codex:acp-selected/high]
roles:
  critic:
    mode: consult
    agent: gemini:role-selected
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ConfigDir: t.TempDir(), RepoOverride: repo, HomeInBox: "/home/node", BoxHome: t.TempDir(),
		BaseImage: "test-base", ImageOverride: "test-image", Homes: true, Egress: "none",
	}
	for _, provider := range []string{"claude", "codex", "gemini"} {
		signInCred(t, cfg, provider, "default")
	}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := &app{cfg: cfg, rt: fusionRecordingRuntime(t, recorder), rtSet: true}
	code, err := a.cmdACP([]string{"fusion", "missing-positional-preset"})
	if err != nil || code != 0 {
		t.Fatalf("inner ACP selected migration = (%d, %v), want success", code, err)
	}
	if a.preset == nil || a.preset.Name != "selected" {
		t.Fatalf("inner ACP loaded preset = %#v, want selected", a.preset)
	}
	if model, effort := cfg.ModelFor("codex"), cfg.EffortFor("codex"); model != "acp-selected" || effort != "high" {
		t.Fatalf("inner ACP effective target = codex:%s/%s, want codex:acp-selected/high", model, effort)
	}
	args, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"codex-acp", "COOP_CONSULT_CRITIC_TARGETS=gemini:role-selected", ":/usr/local/bin/coop-consult:ro"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("inner ACP selected assembly missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(string(args), "stale-first") {
		t.Errorf("inner ACP assembly retained the old launch rung:\n%s", args)
	}
}

func TestACPPlainInnerTargetDoesNotBecomeFusion(t *testing.T) {
	t.Setenv("COOP_ACP_INNER", "1")
	t.Setenv("COOP_ACP_TARGET", "claude")

	configDir := t.TempDir()
	cfg := &config.Config{
		ConfigDir: configDir, RepoOverride: t.TempDir(), HomeInBox: "/home/node", BoxHome: t.TempDir(),
		BaseImage: "test-base", ImageOverride: "test-image", Homes: true, Egress: "none",
	}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := &app{cfg: cfg, rt: fusionRecordingRuntime(t, recorder), rtSet: true}
	code, err := a.cmdACP([]string{"claude"})
	if err != nil || code != 0 {
		t.Fatalf("plain inner ACP target = (%d, %v), want a non-Fusion run", code, err)
	}
	args, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(configDir, "claude", "acp-sessions", "projects") + ":/home/node/.claude/projects"
	if !strings.Contains(string(args), shared) {
		t.Fatalf("plain inner ACP did not request shared session history without a supervisor:\n%s", args)
	}
}

func TestACPFusionSupervisorDoesNotPinSkippedFirstAccount(t *testing.T) {
	repo := t.TempDir()
	presetDir := filepath.Join(repo, ".agent", "presets", "rotate")
	if err := os.MkdirAll(presetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presetDir, "preset.yaml"), []byte(`lead:
  agent: [claude@ghost, codex@work]
roles:
  critic:
    mode: consult
    agent: claude
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: repo}
	signInCred(t, cfg, "claude", "default")
	signInCred(t, cfg, "codex", "work")

	called := false
	a := &app{cfg: cfg, acpSupervise: func(_ []string, ctrl *acpctl.Control) (int, error) {
		called = true
		target, presetName, ok := ctrl.SpawnTarget()
		if !ok || presetName != "rotate" || target.String() != "codex@work" {
			t.Errorf("supervisor target = (%s, %q, %v), want codex@work + rotate", target.String(), presetName, ok)
		}
		return 0, nil
	}}
	code, err := a.cmdACP([]string{"fusion", "rotate"})
	if err != nil || code != 0 || !called {
		t.Fatalf("ACP Fusion skipped-first launch = (%d, %v, supervise=%v), want success", code, err, called)
	}
	if got := cfg.ActiveProfile("claude"); got != "default" {
		t.Fatalf("outer ACP pinned skipped claude@ghost, active profile = %q", got)
	}
}

func TestSpawnBoxExportsEmptyPresetSelection(t *testing.T) {
	recorder := filepath.Join(t.TempDir(), "preset-env")
	shim := filepath.Join(t.TempDir(), "inner")
	script := "#!/bin/sh\nprintf 'set:%s' \"${COOP_ACP_PRESET-UNSET}\" > " + strconv.Quote(recorder) + "\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir()}
	ctrl := acpctl.New(cfg, "claude", "", "", t.TempDir(), acpctl.Selection{}, nil, nil, true, nil, acpHost())
	a := &app{cfg: cfg}
	child, err := a.spawnBox(context.Background(), shim, nil, "test-supervisor", ctrl,
		agents.Target{Provider: "claude"}, "", true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if data, readErr := os.ReadFile(recorder); readErr == nil {
			if got := string(data); got != "set:" {
				t.Fatalf("COOP_ACP_PRESET handoff = %q, want present-but-empty", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("inner process did not record COOP_ACP_PRESET")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestExtractPeer: --peer is REPEATABLE, one peer target per flag. A valueless occurrence errors
// (points at the repeatable form); each value is collected in order; after `--` an agent's own
// --peer passes through verbatim. The retired --consult spelling is now an ordinary passthrough arg.
func TestExtractPeer(t *testing.T) {
	cases := []struct {
		args     []string
		want     []string
		wantRest []string
		wantErr  bool
	}{
		{nil, nil, nil, false},
		{[]string{"-p", "hi"}, nil, []string{"-p", "hi"}, false},
		{[]string{"--peer", "codex"}, []string{"codex"}, nil, false},
		{[]string{"--peer", "codex:gpt-5.5", "--peer", "gemini"}, []string{"codex:gpt-5.5", "gemini"}, nil, false},
		{[]string{"--peer=codex", "-p", "hi"}, []string{"codex"}, []string{"-p", "hi"}, false},
		{[]string{"-p", "hi", "--peer", "gemini"}, []string{"gemini"}, []string{"-p", "hi"}, false},
		// A valueless --peer errors (points at the repeatable form).
		{[]string{"--peer"}, nil, nil, true},
		{[]string{"--peer", "--other"}, nil, nil, true},
		// After --, a --peer is the agent's own arg, not coop's — passed through verbatim.
		{[]string{"--", "--peer", "x"}, nil, []string{"--", "--peer", "x"}, false},
		// The retired --consult is now just an unknown/passthrough token, not a peer flag.
		{[]string{"--consult", "codex"}, nil, []string{"--consult", "codex"}, false},
	}
	for _, c := range cases {
		got, rest, err := extractPeer(c.args)
		if (err != nil) != c.wantErr {
			t.Errorf("extractPeer(%v) err=%v, wantErr=%v", c.args, err, c.wantErr)
			continue
		}
		if c.wantErr {
			continue
		}
		if !slices.Equal(got, c.want) || !slices.Equal(rest, c.wantRest) {
			t.Errorf("extractPeer(%v) = (%v, %v), want (%v, %v)", c.args, got, rest, c.want, c.wantRest)
		}
	}
}

// TestResolvePeers: a --peer value is one peer target — a known, authed provider with an optional
// :model and NO account. An @account, an unauthed provider, and an unknown provider each error
// (naming the peer); an empty list is no peers, no error.
func TestResolvePeers(t *testing.T) {
	dir := t.TempDir()
	// claude authed (a credential file); codex/gemini not signed in.
	os.MkdirAll(filepath.Join(dir, "claude", "profiles", "default"), 0o755)
	os.WriteFile(filepath.Join(dir, "claude", "profiles", "default", ".credentials.json"), []byte("{}"), 0o644)
	a := &app{cfg: &config.Config{ConfigDir: dir}}

	peers, err := a.resolvePeers("--peer", []string{"claude:opus-4.8"})
	if err != nil || len(peers) != 1 || peers[0].Provider != "claude" || peers[0].Model != "opus-4.8" {
		t.Fatalf("resolvePeers(claude:opus-4.8) = (%+v, %v)", peers, err)
	}
	if _, err := a.resolvePeers("--peer", []string{"claude@work"}); err == nil {
		t.Error("a peer with an @account must be rejected (a peer runs on its default account)")
	}
	if _, err := a.resolvePeers("--peer", []string{"codex"}); err == nil {
		t.Error("an unauthed peer must be rejected")
	}
	if _, err := a.resolvePeers("--peer", []string{"borg"}); err == nil {
		t.Error("an unknown provider must be rejected")
	}
	if peers, err := a.resolvePeers("--peer", nil); err != nil || peers != nil {
		t.Errorf("resolvePeers(nil) = (%v, %v), want (nil, nil)", peers, err)
	}
}

// TestCmdLoginTarget: the account rides the target (coop login claude@work); a stray --credential
// is an unexpected arg; a :model has no meaning for login; an account ladder is loop-only. The happy
// path parses and reaches loginTo (which then needs a TTY) — proof the target flowed through.
func TestCmdLoginTarget(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	// claude@work parses and flows to loginTo — non-TTY there, NOT a parse error.
	if code, err := a.cmdLogin([]string{"claude@work"}); code != 2 || err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Errorf("cmdLogin(claude@work) = (%d, %v), want it to parse and hit the TTY check", code, err)
	}
	if _, err := a.cmdLogin([]string{"claude", "--credential", "work"}); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("cmdLogin --credential must be an unexpected argument, got %v", err)
	}
	if _, err := a.cmdLogin([]string{"claude:opus"}); err == nil || !strings.Contains(err.Error(), "no model") {
		t.Errorf("cmdLogin claude:opus must reject the model, got %v", err)
	}
	if _, err := a.cmdLogin([]string{"claude@work,personal"}); err == nil {
		t.Error("cmdLogin claude@work,personal must reject an account ladder (loop-only)")
	}
}

func TestLaunchAgentRejectsUnknownProfile(t *testing.T) {
	// A nonexistent account in the target must error before any box work (claude@ghost), so a
	// typo never silently creates a husk.
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	code, err := a.launchAgent("claude@ghost", []string{"-p", "hi"})
	if code != 2 || err == nil {
		t.Fatalf("launchAgent claude@ghost = (%d, %v), want 2 + error", code, err)
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should name the bad account: %v", err)
	}
}

func TestSelectRunProfile(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	work := cfg.AgentProfileDir("claude", "work") // signed in
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".credentials.json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.AgentProfileDir("claude", "bare"), 0o700); err != nil { // exists, no creds
		t.Fatal(err)
	}
	a := &app{cfg: cfg}

	if err := a.selectRunProfile("claude", ""); err != nil {
		t.Errorf("empty profile should be a no-op: %v", err)
	}
	if err := a.selectRunProfile("claude", "ghost"); err == nil {
		t.Error("unknown profile should error")
	}
	if err := a.selectRunProfile("claude", "work"); err != nil {
		t.Fatalf("signed-in profile should select: %v", err)
	}
	if got := cfg.AgentDir("claude"); got != work {
		t.Errorf("active dir = %q, want %q", got, work)
	}
	if err := a.selectRunProfile("claude", "bare"); err != nil {
		t.Errorf("an existing but unsigned profile should select with a note, not error: %v", err)
	}
}

// A nonexistent account in the target must fail fast (before any box/Docker work) on fusion and
// acp too, not just a plain agent run; a stray --credential is a rejected arg on each surface.
func TestRunProfileWiringRejectsUnknown(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	if code, err := a.cmdFusion([]string{"claude@ghost"}); code != 2 || err == nil {
		t.Errorf("cmdFusion claude@ghost = (%d, %v), want 2 + error", code, err)
	}
	if code, err := a.cmdFusion([]string{"claude", "--credential", "ghost"}); code != 2 || err == nil {
		t.Errorf("cmdFusion --credential = (%d, %v), want 2 + error", code, err)
	}
	if code, err := a.cmdACP([]string{"claude@ghost"}); code != 2 || err == nil {
		t.Errorf("cmdACP claude@ghost = (%d, %v), want 2 + error", code, err)
	}
	if code, err := a.cmdACP([]string{"claude", "--credential", "ghost"}); code != 2 || err == nil {
		t.Errorf("cmdACP --credential = (%d, %v), want 2 + error", code, err)
	}
}

// A bare `coop acp` never guesses from credential order. The initial provider or preset is part of
// the editor's durable command configuration; the live toolbar may switch only after that explicit
// session has started.
func TestACPRequiresAnExplicitInitialTarget(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	// Sign Codex in to prove a usable credential is not treated as launch intent.
	os.MkdirAll(filepath.Join(dir, "codex", "profiles", "default"), 0o755)
	os.WriteFile(filepath.Join(dir, "codex", "profiles", "default", "auth.json"), []byte("{}"), 0o644)
	a := &app{cfg: cfg}
	code, err := a.cmdACP([]string{})
	if code != 2 || err == nil {
		t.Fatalf("bare cmdACP with a signed-in provider = (%d, %v), want (2, error)", code, err)
	}
	if !strings.Contains(err.Error(), "name the agent") || !strings.Contains(err.Error(), "coop acp") {
		t.Errorf("error should require an explicit ACP target, got: %v", err)
	}
}

func TestParseServices(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"none", nil},
		{"postgres", []string{"postgres"}},
		{"postgres,redis", []string{"postgres", "redis"}},
		{"redis postgres", []string{"redis", "postgres"}}, // input order preserved
		{"postgres,postgres", []string{"postgres"}},       // de-duped
		{"mongo", nil}, // unknown dropped
		{"postgres,mongo", []string{"postgres"}},
	}
	for _, c := range cases {
		if got := parseServices(c.in); !slices.Equal(got, c.want) {
			t.Errorf("parseServices(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestWriteMCPStub(t *testing.T) {
	mcp := filepath.Join(t.TempDir(), "agents", "mcp.json") // parent dir doesn't exist yet
	a := &app{cfg: &config.Config{MCPFile: mcp}}

	// Seeds an empty, well-shaped stub (creating the config dir) when absent.
	if err := a.writeMCPStub(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(mcp)
	if err != nil {
		t.Fatalf("stub not written: %v", err)
	}
	var f struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("stub is not valid JSON: %v\n%s", err, data)
	}
	if f.MCPServers == nil || len(f.MCPServers) != 0 {
		t.Errorf("stub should carry an empty mcpServers object, got %v", f.MCPServers)
	}
	// The stub is inactive end-to-end — it must not flip MCP on for runs.
	if a.cfg.MCPActive() {
		t.Error("the empty stub must leave MCPActive false")
	}

	// Idempotent: a user's filled-in config is never clobbered.
	os.WriteFile(mcp, []byte(`{"mcpServers":{"fs":{"command":"x"}}}`), 0o600)
	if err := a.writeMCPStub(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(mcp); !strings.Contains(string(b), `"fs"`) {
		t.Error("writeMCPStub clobbered an existing mcp.json")
	}

	// No MCPFile configured → a harmless no-op (tests build cfgs without one).
	if err := (&app{cfg: &config.Config{}}).writeMCPStub(); err != nil {
		t.Errorf("empty MCPFile should be a no-op, got %v", err)
	}
}

func TestInitNextSteps(t *testing.T) {
	// In a git repo (no box Dockerfile, no services) → just the edit-then-loop step.
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := initNextSteps(repo, nil); len(got) != 1 || !strings.Contains(got[0], "coop loop") {
		t.Errorf("git repo steps = %v, want only the loop step", got)
	}
	// A scaffolded .agent/Dockerfile + sibling services → build, up (naming the services), loop.
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "Dockerfile"), []byte("FROM x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := initNextSteps(repo, []string{"postgres", "redis"})
	if len(got) != 3 {
		t.Fatalf("want 3 steps, got %v", got)
	}
	if !strings.Contains(got[0], "coop build") ||
		!strings.Contains(got[1], "coop up") || !strings.Contains(got[1], "postgres + redis") ||
		!strings.Contains(got[2], "coop loop") {
		t.Errorf("steps wrong or out of order: %v", got)
	}
	// Outside a git repo, the first step is `git init` — forks and the loop need one.
	if steps := initNextSteps(t.TempDir(), nil); len(steps) == 0 || !strings.Contains(steps[0], "git init") {
		t.Errorf("non-git repo should lead with `git init`, got %v", steps)
	}
}

// `coop acp` takes an agent (or fusion [governor]) and coop flags only — a leftover token must be a
// usage error (exit 2), not silently ignored. Returns before any box/Docker work.
func TestCmdACPRejectsExtraArgs(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	for _, args := range [][]string{
		{"claude", "foo"},
		{"claude", "--nope"},
		{"claude", "--supervise"},
		{"fusion", "claude", "junk"},
	} {
		if code, err := a.cmdACP(args); code != 2 || err == nil {
			t.Errorf("cmdACP(%v) = (%d, %v), want (2, usage error)", args, code, err)
		}
	}
}

func TestCleanACPChildEnv(t *testing.T) {
	got := cleanACPChildEnv([]string{
		"PATH=/bin",
		"COOP_ACP_TARGET=gemini",
		"COOP_ACP_PRESET=frontier",
		"COOP_ACP_INNER=1",
		"COOP_ACP_SUPERVISOR=stale",
		"COOP_ACP_CIDFILE=/tmp/stale",
		"COOP_ACP_RESUME_STATE=/tmp/stale",
		liveprocess.ControlFDEnv + "=3",
		liveprocess.ProcessDirEnv + "=/tmp/stale-processes",
		liveprocess.CleanupIDEnv + "=stale-cleanup",
		liveprocess.RevokePathEnv + "=/tmp/.coop-live-revoked-00000000000000000000000000000000",
		"COOP_ACP_TRACE=1",
		"COOP_ACP_CARRY_TOKENS=123",
	})
	joined := strings.Join(got, "\n")
	for _, want := range []string{"PATH=/bin", "COOP_ACP_TRACE=1", "COOP_ACP_CARRY_TOKENS=123"} {
		if !strings.Contains(joined, want) {
			t.Errorf("clean env dropped public setting %q: %v", want, got)
		}
	}
	for _, removed := range []string{
		"COOP_ACP_TARGET", "COOP_ACP_PRESET", "COOP_ACP_INNER", "COOP_ACP_SUPERVISOR",
		"COOP_ACP_CIDFILE", "COOP_ACP_RESUME_STATE", liveprocess.ControlFDEnv, liveprocess.ProcessDirEnv,
		liveprocess.CleanupIDEnv, liveprocess.RevokePathEnv,
	} {
		if strings.Contains(joined, removed+"=") {
			t.Errorf("clean env retained internal setting %q: %v", removed, got)
		}
	}
}

// `coop run` with no command is a usage error (it doesn't default to an agent), and `coop run
// --help`/-h prints run's own page — neither enters the box (which would exec `--help` and crash).
func TestCmdRunMetaCases(t *testing.T) {
	a := &app{cfg: &config.Config{}} // meta-cases return before runInBox, so no runtime needed
	if code, err := a.cmdRun(nil); code != 2 || err == nil {
		t.Errorf("cmdRun(nil) = (%d, %v), want (2, usage error)", code, err)
	}
	if code, err := a.cmdRun([]string{"--"}); code != 2 || err == nil {
		t.Errorf("cmdRun(--) = (%d, %v), want (2, usage error)", code, err)
	}
	for _, h := range []string{"--help", "-h"} {
		old := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		code, err := a.cmdRun([]string{h})
		_ = w.Close()
		os.Stdout = old
		out, _ := io.ReadAll(r)
		if code != 0 || err != nil {
			t.Errorf("cmdRun(%q) = (%d, %v), want (0, nil)", h, code, err)
		}
		if !strings.Contains(string(out), "coop run — run a raw command") {
			t.Errorf("cmdRun(%q) should print run's help, got:\n%s", h, out)
		}
	}
}

// `coop login` requires the agent (no silent default that opens a browser) and refuses a
// non-interactive stdin instead of blocking on the paste-code prompt forever.
func TestLoginRequiresAgentAndTTY(t *testing.T) {
	// Force a non-terminal stdin so the tty guard is deterministic.
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	saved := os.Stdin
	os.Stdin = devnull
	defer func() { os.Stdin = saved }()

	a := &app{cfg: &config.Config{}}
	if code, err := a.cmdLogin(nil); code != 2 || err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("cmdLogin(nil) = (%d, %v), want (2, usage error)", code, err)
	}
	if code, err := a.loginTo("claude", ""); code != 2 || err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Errorf("loginTo(claude) non-tty = (%d, %v), want (2, interactive-terminal error)", code, err)
	}
	if code, err := a.loginTo("bogus", ""); code != 2 || err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("loginTo(bogus) = (%d, %v), want (2, unknown agent — before the tty check)", code, err)
	}
}

func TestValidProfileName(t *testing.T) {
	for _, ok := range []string{"default", "work", "personal_backup", "p1", "acc.2"} {
		if !validProfileName(ok) {
			t.Errorf("%q should be a valid profile name", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "../../x", "a/b", `a\b`, "-x"} {
		if validProfileName(bad) {
			t.Errorf("%q should be rejected (traversal/collision/flag-like)", bad)
		}
	}
}

func TestLoginRejectsBadProfileName(t *testing.T) {
	// A traversal name must be rejected before any vault/dir work — and before the tty check, so it
	// fails the same way piped or at a terminal.
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	if code, err := a.loginTo("claude", "../../escape"); code != 2 || err == nil || !strings.Contains(err.Error(), "invalid credential name") {
		t.Errorf("loginTo bad credential = (%d, %v), want (2, invalid credential name)", code, err)
	}
}

// TestStrictFlagParsing: value-bearing coop flags reject a missing value or a stray arg up
// front (exit 2) instead of silently falling back to a default or ignoring the typo. These all
// return before any runtime/scaffold work, so a bare app suffices.
func TestStrictFlagParsing(t *testing.T) {
	a := &app{cfg: &config.Config{}}
	cases := []struct {
		name string
		fn   func() (int, error)
	}{
		{"login stray arg", func() (int, error) { return a.cmdLogin([]string{"claude", "extra"}) }},
		{"init --stack no value", func() (int, error) { return a.cmdInit([]string{"--stack"}) }},
		{"init --services no value", func() (int, error) { return a.cmdInit([]string{"--services"}) }},
		{"init unknown flag", func() (int, error) { return a.cmdInit([]string{"--bogus"}) }},
	}
	for _, c := range cases {
		if code, err := c.fn(); code != 2 || err == nil {
			t.Errorf("%s = (%d, %v), want (2, error)", c.name, code, err)
		}
	}
}

// The top-level help documents coop's --peer wrapper flag and stops claiming `coop <agent>
// --help` shows coop's flags (it forwards to the agent).
func TestHelpDocumentsPeerAndAgentHelp(t *testing.T) {
	h := helpText(&config.Config{})
	if !strings.Contains(h, "--peer") {
		t.Error("top-level help should document the --peer wrapper flag")
	}
	if !strings.Contains(h, "--help is the agent's own") {
		t.Error("footer should note that for an agent, --help is the agent's own")
	}
}

// The top-level help lists every fleet verb on its own row (like the fork rows), so none
// is hidden from the main help.
func TestTopLevelListsAllGroupVerbs(t *testing.T) {
	h := helpText(&config.Config{})
	for _, verb := range []string{"init", "up", "down", "watch", "prune"} {
		if !strings.Contains(h, "coop fleet "+verb) {
			t.Errorf("top-level help should list `coop fleet %s` as its own row", verb)
		}
	}
}

// TestPromptLine: coop prompt's line shows non-zero segments only, "·"-separated, in a fixed
// order (todo, doing, blocked, looping, forks); "" when idle so an embedding prompt stays clean.
func TestPromptLine(t *testing.T) {
	if got := promptLine(tasks.TaskCounts{}, 0, 0, false); got != "" {
		t.Errorf("idle should be empty, got %q", got)
	}
	if got := promptLine(tasks.TaskCounts{Done: 9}, 0, 0, false); got != "" {
		t.Errorf("done-only isn't actionable state — should be empty, got %q", got)
	}
	if got := promptLine(tasks.TaskCounts{Todo: 3, Blocked: 1}, 2, 1, false); got != "3 todo · 1 blocked · 1 looping · 2 forks" {
		t.Errorf("got %q", got)
	}
	if got := promptLine(tasks.TaskCounts{Doing: 2}, 1, 0, false); got != "2 doing · 1 fork" { // singular fork
		t.Errorf("got %q", got)
	}
	// The unsigned nudge appends when set; alone (no other state) it's the whole line.
	if got := promptLine(tasks.TaskCounts{Todo: 1}, 0, 0, true); got != "1 todo · unsigned" {
		t.Errorf("got %q", got)
	}
	if got := promptLine(tasks.TaskCounts{}, 0, 0, true); got != "unsigned" {
		t.Errorf("unsigned alone should be the whole line, got %q", got)
	}
}

func TestSignOnExitAndPromptWarn(t *testing.T) {
	// shouldSignOnExit: only when you sign and not a fork. Dirty checkout state is isolated.
	cases := []struct{ fork, signs, want bool }{
		{false, true, true},   // sign an interactive session
		{true, true, false},   // fork → land-time re-sign owns it
		{false, false, false}, // you don't sign by default
	}
	for _, c := range cases {
		if got := shouldSignOnExit(c.fork, c.signs); got != c.want {
			t.Errorf("shouldSignOnExit(fork=%v,signs=%v) = %v, want %v", c.fork, c.signs, got, c.want)
		}
	}
	// promptSignWarn: only when you sign AND HEAD is unsigned.
	if !promptSignWarn(true, true) || promptSignWarn(true, false) || promptSignWarn(false, true) {
		t.Error("promptSignWarn should fire only when signs && headUnsigned")
	}
}

func TestScaffoldAgentSet(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()} // no agents signed in
	if got := scaffoldAgentSet(cfg, "all", true); len(got) != 3 {
		t.Errorf(`--agents all → 3 scaffoldable agents, got %v`, got)
	}
	// A named list is kept to the scaffoldable set — grok has no per-agent dir, so it's dropped.
	if got := scaffoldAgentSet(cfg, "claude,grok,codex", true); len(got) != 2 || got[0] != "claude" || got[1] != "codex" {
		t.Errorf("named list should keep scaffoldable only: %v", got)
	}
	// No flag, no credentials → empty (.agent/ only).
	if got := scaffoldAgentSet(cfg, "", false); len(got) != 0 {
		t.Errorf("no flag + no creds → empty, got %v", got)
	}
}

// ensureACPImage is the ACP path's guard against a pruned or never-built image. It must build
// exactly when one is missing — and, just as importantly, must NOT build when one is present:
// this is a "can anything run at all" check, not a freshness check, and four warm targets share
// the one supervisor that calls it.
func TestEnsureACPImageBuildsOnlyWhenMissing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		exists    bool
		wantBuild bool
	}{
		{"present image is left alone", true, false},
		{"missing image is built once", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			recorder := filepath.Join(t.TempDir(), "calls")
			shim := filepath.Join(t.TempDir(), "rt")
			inspect := "exit 1"
			if tc.exists {
				inspect = "exit 0"
			}
			script := "#!/bin/sh\n" +
				"echo \"$@\" >> " + strconv.Quote(recorder) + "\n" +
				"case \"$1$2\" in\n" +
				"  imageinspect) " + inspect + " ;;\n" +
				"esac\n" +
				"exit 0\n"
			if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			a := &app{
				cfg:   &config.Config{BaseImage: "coop-box", ConfigDir: t.TempDir(), BoxHome: t.TempDir(), RepoOverride: repo},
				rt:    runtime.Runtime{Name: shim},
				rtSet: true,
			}
			if err := a.ensureACPImage(); err != nil {
				t.Fatalf("ensureACPImage = %v, want nil", err)
			}
			calls, _ := os.ReadFile(recorder)
			// Count INVOCATIONS, not the substring: one build line carries several --build-arg.
			builds := 0
			for _, line := range strings.Split(string(calls), "\n") {
				if strings.HasPrefix(line, "build ") {
					builds++
				}
			}
			if tc.wantBuild && builds == 0 {
				t.Errorf("a missing image must be built — ACP has no other way to recover:\n%s", calls)
			}
			if !tc.wantBuild && builds != 0 {
				t.Errorf("a present image must not trigger a build:\n%s", calls)
			}
			if builds > 1 {
				t.Errorf("built %d times, want at most 1 (the supervisor is the single-flight point):\n%s", builds, calls)
			}
		})
	}
}

// A dead daemon and a missing image both fail `image inspect`, and reporting the wrong one costs
// real debugging time: a Docker restart once filled an editor log with "run 'coop build'" while
// the image was present the whole time. resolveImage must name the daemon when the daemon is why.
func TestResolveImageBlamesTheDaemonNotTheImage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		infoExit string
		want     string
	}{
		{"daemon unreachable", "1", "daemon isn't responding"},
		{"daemon up, image absent", "0", "not built"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			// Named "docker": EnsureDaemon probes only the Docker kind, which it reads off the binary.
			shim := filepath.Join(t.TempDir(), "docker")
			script := "#!/bin/sh\n" +
				"case \"$1\" in info) exit " + tc.infoExit + " ;; esac\n" +
				"case \"$1$2\" in imageinspect) exit 1 ;; esac\n" +
				"exit 0\n"
			if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			a := &app{
				cfg:   &config.Config{BaseImage: "coop-box", ConfigDir: t.TempDir(), BoxHome: t.TempDir(), RepoOverride: repo},
				rt:    runtime.Runtime{Name: shim},
				rtSet: true,
			}
			_, _, err := a.resolveImage()
			if err == nil {
				t.Fatal("resolveImage succeeded with no image; want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("resolveImage = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
