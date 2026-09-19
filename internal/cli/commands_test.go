package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/testutil/wait"

	"github.com/AndrewDryga/coop/internal/acpctl"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/liveprocess"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/scaffold"
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
	// --preset are options loop does not have — rejected as options, not as stray positionals.
	for _, bad := range [][]string{{"claude", "--model", "opus"}, {"claude", "--credential", "work"}, {"claude", "--preset", "frontier"}} {
		_, _, _, _, _, _, _, err := parseLoopArgs(bad, false)
		if err == nil || !strings.Contains(err.Error(), `for "coop loop"`) || !strings.Contains(err.Error(), "Unknown option") {
			t.Errorf("parseLoopArgs(%v) should be an unknown option for coop loop, got %v", bad, err)
		}
	}
}

func TestResolveWorkAgentKeepsBarePresetTargetInMixedLadder(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, ".agent", "presets", "bare")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte("lead: {agent: claude}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}

	agent, p, targets, err := a.resolveWorkAgent([]string{"bare", "codex:gpt-5.6-sol/xhigh"})
	if err != nil {
		t.Fatal(err)
	}
	if agent != "claude" || p == nil || p.Name != "bare" {
		t.Fatalf("resolved lead = (%q, %+v), want bare preset led by claude", agent, p)
	}
	want := []string{"claude", "codex:gpt-5.6-sol/xhigh"}
	if len(targets) != len(want) {
		t.Fatalf("resolved targets = %v, want %v", targets, want)
	}
	for i, target := range targets {
		if got := target.String(); got != want[i] {
			t.Errorf("resolved target[%d] = %q, want %q", i, got, want[i])
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

func TestLoopContinueCommandPreservesExplicitQueues(t *testing.T) {
	target, err := agents.ParseTarget("claude:opus@personal")
	if err != nil {
		t.Fatal(err)
	}
	got := loopContinueCommand(target, true, "", []string{
		".agent/tasks",
		"apps/customer portal/.agent/tasks",
		"queue'; touch nope; '",
	})
	want := "coop loop claude:opus@personal --tasks .agent/tasks --tasks 'apps/customer portal/.agent/tasks' --tasks 'queue'\"'\"'; touch nope; '\"'\"''"
	if got != want {
		t.Fatalf("continue command = %q, want %q", got, want)
	}
	if got := loopContinueCommand(agents.Target{}, false, "frontier", []string{"api/.agent/tasks"}); got != "coop loop frontier --tasks api/.agent/tasks" {
		t.Fatalf("preset continuation = %q", got)
	}
	if got := loopContinueCommand(agents.Target{}, false, "", nil); got != "coop loop" {
		t.Fatalf("default continuation = %q", got)
	}
}

func TestLaunchPresetPinsFirstRungAndWiresConsultRole(t *testing.T) {
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
	signInCred(t, cfg, "gemini", "default")
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := &app{cfg: cfg, rt: recordingRuntime(t, recorder), rtSet: true}
	p, err := a.loadRunPreset("duo")
	if err != nil {
		t.Fatal(err)
	}
	code, runErr := a.launchPreset(p, nil)
	if runErr != nil || code != 0 {
		t.Fatalf("launchPreset(duo) = (%d, %v), want success", code, runErr)
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

func recordingRuntime(t *testing.T, recorder string) runtime.Runtime {
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

func recycleRuntime(t *testing.T, mode string) (runtime.Runtime, string) {
	t.Helper()
	dir := t.TempDir()
	shim := filepath.Join(dir, "runtime")
	trace := filepath.Join(dir, "trace")
	if err := os.WriteFile(shim, []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$COOP_TEST_RUNTIME_TRACE"
if [ "$1" = ps ]; then
	if [ "$COOP_TEST_RECYCLE_MODE" = query-failure ]; then
		echo 'daemon query broke' >&2
		exit 42
	fi
	case "$*" in
		*" -a "*)
			case "$COOP_TEST_RECYCLE_MODE" in success|partial) printf 'supervised-a\nsupervised-b\n' ;; esac
			;;
		*"label=coop.supervised=1"*)
			case "$COOP_TEST_RECYCLE_MODE" in success|partial) printf 'supervised-a\nsupervised-b\n' ;; esac
			;;
		*"label=coop=box"*)
			case "$COOP_TEST_RECYCLE_MODE" in success|partial) printf 'supervised-a\nsupervised-b\nother\n' ;; esac
			;;
	esac
fi
if [ "$1" = rm ] && [ "$COOP_TEST_RECYCLE_MODE" = partial ] && [ "$3" = supervised-b ]; then
	echo 'container remove broke' >&2
	exit 43
fi
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_RUNTIME_TRACE", trace)
	t.Setenv("COOP_TEST_RECYCLE_MODE", mode)
	return runtime.Runtime{Name: shim}, trace
}

func TestRecycleBoxesDistinguishesNoMatchFromFailure(t *testing.T) {
	t.Run("no match", func(t *testing.T) {
		rt, _ := recycleRuntime(t, "no-match")
		a := &app{rt: rt, rtSet: true}
		supervised, others, recycleErr := a.recycleBoxes("")
		if recycleErr != nil || supervised != 0 || others != 0 {
			t.Fatalf("empty recycle = (%d, %d, %v), want a quiet zero", supervised, others, recycleErr)
		}
	})

	t.Run("success counts both effects", func(t *testing.T) {
		rt, _ := recycleRuntime(t, "success")
		a := &app{rt: rt, rtSet: true}
		supervised, others, recycleErr := a.recycleBoxes("")
		if recycleErr != nil {
			t.Fatal(recycleErr)
		}
		if supervised != 2 || others != 1 {
			t.Errorf("recycle = (%d supervised, %d other), want (2, 1)", supervised, others)
		}
	})

	t.Run("query failure happens before removal", func(t *testing.T) {
		rt, trace := recycleRuntime(t, "query-failure")
		a := &app{rt: rt, rtSet: true}
		_, _, recycleErr := a.recycleBoxes("")
		if recycleErr == nil || !strings.Contains(recycleErr.Error(), "daemon query broke") ||
			!strings.Contains(recycleErr.Error(), "did not respond while listing") {
			t.Fatalf("query failure error = %v; want the step and the runtime diagnostic", recycleErr)
		}
		calls, err := os.ReadFile(trace)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(calls), "rm -f") {
			t.Fatalf("query failure mutated containers:\n%s", calls)
		}
	})

	t.Run("partial removal reports progress", func(t *testing.T) {
		rt, _ := recycleRuntime(t, "partial")
		a := &app{rt: rt, rtSet: true}
		supervised, _, recycleErr := a.recycleBoxes("")
		if recycleErr == nil || !strings.Contains(recycleErr.Error(), "1 removed before failure") ||
			!strings.Contains(recycleErr.Error(), "container remove broke") {
			t.Fatalf("partial removal error = %v, want count and runtime diagnostic", recycleErr)
		}
		if supervised != 0 {
			t.Fatalf("partial removal reported %d restarted sessions; a failed removal counts none", supervised)
		}
	})
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
			want := "✓ Services ready: " + strings.Join(tc.services, ", ")
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
		!strings.Contains(out, "Compose config --services exited with status 19.") ||
		!strings.Contains(out, "then run coop up again.") {
		t.Fatalf("cmdUp discovery failure = (%d, %v), want named failure:\n%s", code, runErr, out)
	}
	if strings.Contains(out, "Services ready") {
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
	signInCred(t, cfg, "gemini", "default")
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := &app{cfg: cfg, rt: recordingRuntime(t, recorder), rtSet: true}
	code, err := a.cmdACP([]string{"missing-positional-preset", "--peer", "gemini"})
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
	a := &app{cfg: cfg, rt: recordingRuntime(t, recorder), rtSet: true}
	code, err := a.cmdACP([]string{"missing-positional-preset"})
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

func TestACPPlainInnerTargetDoesNotLoadPreset(t *testing.T) {
	t.Setenv("COOP_ACP_INNER", "1")
	t.Setenv("COOP_ACP_TARGET", "claude")

	configDir := t.TempDir()
	cfg := &config.Config{
		ConfigDir: configDir, RepoOverride: t.TempDir(), HomeInBox: "/home/node", BoxHome: t.TempDir(),
		BaseImage: "test-base", ImageOverride: "test-image", Homes: true, Egress: "none",
	}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := &app{cfg: cfg, rt: recordingRuntime(t, recorder), rtSet: true}
	code, err := a.cmdACP([]string{"claude"})
	if err != nil || code != 0 {
		t.Fatalf("plain inner ACP target = (%d, %v), want an ordinary run", code, err)
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

func TestACPPresetSupervisorDoesNotPinSkippedFirstAccount(t *testing.T) {
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
	code, err := a.cmdACP([]string{"rotate"})
	if err != nil || code != 0 || !called {
		t.Fatalf("ACP preset skipped-first launch = (%d, %v, supervise=%v), want success", code, err, called)
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
	ctrl := acpctl.New(cfg, "claude", "", "", t.TempDir(), acpctl.Selection{}, nil, nil, acpHost())
	a := &app{cfg: cfg}
	child, err := a.spawnBox(context.Background(), shim, nil, "test-supervisor", ctrl,
		agents.Target{Provider: "claude"}, "", true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Stop()
	// Wait for the CONTENT, not the file: the shim's redirection creates the file before printf
	// writes it, and a loaded host widens that window.
	var recorded []byte
	wait.For(t, "the inner process recording COOP_ACP_PRESET", func() bool {
		data, err := os.ReadFile(recorder)
		recorded = data
		return err == nil && len(data) > 0
	})
	if string(recorded) != "set:" {
		t.Fatalf("COOP_ACP_PRESET handoff = %q, want present-but-empty", recorded)
	}
}

// A filtered ACP child tears its own gateway down when its run is cancelled, so it is asked with SIGTERM
// and waited for; one that ignores the request, and a child with nothing of its own to tear down, is
// killed.
func TestStopACPChildLetsAFilteredChildTearDownFirst(t *testing.T) {
	start := func(t *testing.T, script string) (int, chan struct{}) {
		t.Helper()
		cmd := exec.Command("sh", "-c", script)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan struct{})
		go func() { _ = cmd.Wait(); close(exited) }()
		return cmd.Process.Pid, exited
	}
	marker := filepath.Join(t.TempDir(), "tore-down")
	ready := filepath.Join(t.TempDir(), "ready")
	awaitReady := func(t *testing.T) {
		t.Helper()
		wait.For(t, "the child installing its handler", func() bool { _, err := os.Stat(ready); return err == nil })
		_ = os.Remove(ready)
	}

	pid, exited := start(t, "trap 'echo done > "+marker+"; exit 0' TERM; : > "+ready+"; while :; do sleep 0.05; done")
	awaitReady(t)
	began := time.Now()
	stopACPChild(pid, 10*time.Second)
	<-exited
	if _, err := os.Stat(marker); err != nil || time.Since(began) > 5*time.Second {
		t.Fatalf("the child was not let to tear down on SIGTERM (marker: %v, took %s)", err, time.Since(began))
	}

	pid, exited = start(t, "trap '' TERM; : > "+ready+"; while :; do sleep 0.05; done")
	awaitReady(t)
	began = time.Now()
	stopACPChild(pid, 300*time.Millisecond)
	select {
	case <-exited:
	case <-time.After(wait.Deadline):
		t.Fatal("a child that ignored SIGTERM was never killed")
	}
	if time.Since(began) < 300*time.Millisecond {
		t.Fatal("a child that ignored SIGTERM was killed before its grace ran out")
	}

	_ = os.Remove(marker)
	pid, exited = start(t, "trap 'echo done > "+marker+"; exit 0' TERM; : > "+ready+"; while :; do sleep 0.05; done")
	awaitReady(t)
	stopACPChild(pid, 0)
	<-exited
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a child with nothing to tear down got SIGTERM instead of being killed")
	}
}

// Stopping the editor cancels the warm fills still in flight: a fill that has not launched its box
// launches nothing, so the pool's reap does not wait for a box no switch will use.
func TestSpawnBoxLaunchesNothingForACancelledFill(t *testing.T) {
	launched := filepath.Join(t.TempDir(), "launched")
	shim := filepath.Join(t.TempDir(), "inner")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n: > "+strconv.Quote(launched)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir()}
	ctrl := acpctl.New(cfg, "codex", "", "", t.TempDir(), acpctl.Selection{}, nil, nil, acpHost())
	a := &app{cfg: cfg}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	child, err := a.spawnBox(cancelled, shim, nil, "warm-supervisor", ctrl, agents.Target{Provider: "codex", Accounts: []string{"default"}}, "", true, io.Discard, forkspace.ExecutionRoleWarm)
	if child != nil {
		child.Stop()
	}
	if err == nil {
		t.Fatal("a cancelled warm fill spawned a box")
	}
	if _, statErr := os.Stat(launched); statErr == nil {
		t.Fatal("a cancelled warm fill launched its inner process")
	}
}

// A warm box never waits out a rate limit: the pool's spawn would sleep until the reset, and closing
// the editor waits for every spawn the pool has in flight. It is refused at once instead, and the pool
// leaves that provider's slot empty; an active spawn still waits, as before.
func TestSpawnBoxRefusesAWarmBoxOnACoolingAccount(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "inner")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir()}
	ctrl := acpctl.New(cfg, "codex", "", "", t.TempDir(), acpctl.Selection{}, nil, nil, acpHost())
	snapshot := ctrl.Snapshot()
	snapshot.Limited = map[string]time.Time{"codex@default": time.Now().Add(time.Hour)}
	ctrl.Restore(snapshot)
	if !ctrl.Cooling("codex", "default") || ctrl.Cooling("codex", "work") {
		t.Fatal("Cooling does not report the account waiting out its limit")
	}
	a := &app{cfg: cfg}
	target := agents.Target{Provider: "codex", Accounts: []string{"default"}}
	done := make(chan error, 1)
	go func() {
		child, err := a.spawnBox(context.Background(), shim, nil, "warm-supervisor", ctrl, target, "", true, io.Discard, forkspace.ExecutionRoleWarm)
		if child != nil {
			child.Stop()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "waiting out a rate limit") {
			t.Fatalf("a warm spawn on a cooling account = %v, want it refused", err)
		}
	case <-time.After(wait.Deadline):
		t.Fatal("a warm spawn is sleeping until the account's reset")
	}
}

func TestSpawnBoxKeepsNativeAuthInOpenACPAndRefusesItInFilteredACP(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "inner")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		provider string
		prepare  func(*testing.T, *config.Config)
	}{
		{name: "Gemini OAuth", provider: "gemini", prepare: func(t *testing.T, cfg *config.Config) {
			dir := cfg.AgentProfileDir("gemini", "default")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "gemini-credentials.json"), []byte(`{"encrypted":"host-bound"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "Grok API key", provider: "grok", prepare: func(t *testing.T, cfg *config.Config) {
			if err := os.WriteFile(cfg.EnvFile(), []byte("XAI_API_KEY=host-key\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{ConfigDir: t.TempDir()}
			tc.prepare(t, cfg)
			target := agents.Target{Provider: tc.provider, Accounts: []string{"default"}}
			ctrl := acpctl.New(cfg, tc.provider, "", "", t.TempDir(), acpctl.Selection{Account: "default"}, nil, nil, acpHost())
			a := &app{cfg: cfg}
			child, err := a.spawnBox(context.Background(), shim, nil, "open-supervisor", ctrl, target, "", true, io.Discard)
			if err != nil {
				t.Fatalf("open ACP rejected native auth: %v", err)
			}
			child.Stop()

			a.acpCapture = &box.CapturedEgress{}
			if child, err := a.spawnBox(context.Background(), shim, nil, "filtered-supervisor", ctrl, target, "", true, io.Discard); err == nil {
				child.Stop()
				t.Fatal("filtered ACP accepted an unqualified native authentication family")
			}
		})
	}
}

func TestFilteredACPAccountBindingsPinTheSupervisorsProfiles(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	cfg.SetActiveProfile("gemini", "new-default")
	bindings, err := applyACPAccountBindings(cfg, `{"codex":{"account":"work","default":"default"},"gemini":{"account":"portable","default":"default"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ActiveProfile("codex"); got != "work" {
		t.Fatalf("Codex binding = %q, want work", got)
	}
	if got := cfg.ActiveProfile("gemini"); got != "portable" {
		t.Fatalf("Gemini binding followed changed default: %q", got)
	}
	for _, raw := range []string{"", `{}`, `{"gemini":{}}`, `{"unknown":{"account":"default","default":"default"}}`} {
		if _, err := applyACPAccountBindings(cfg, raw); err == nil {
			t.Fatalf("malformed account binding %q was accepted", raw)
		}
	}
	p := &preset.Preset{LeadTargets: []agents.Target{{Provider: "codex"}}, Roles: []preset.Role{{
		Name: "critic", Mode: preset.ModeConsult, Targets: []agents.Target{{Provider: "gemini"}},
	}}}
	if err := validateACPAccountBindings(cfg, box.RunSpec{Agent: "codex", Homes: true, Preset: p}, bindings); err != nil {
		t.Fatal(err)
	}
	p.Roles = append(p.Roles, preset.Role{Name: "new", Mode: preset.ModeConsult, Targets: []agents.Target{{Provider: "grok"}}})
	if err := validateACPAccountBindings(cfg, box.RunSpec{Agent: "codex", Homes: true, Preset: p}, bindings); err == nil {
		t.Fatal("a role added after supervisor validation had no exact account binding")
	}
	drift := &config.Config{ConfigDir: t.TempDir()}
	if err := drift.SetDefaultProfile("gemini", "portable"); err != nil {
		t.Fatal(err)
	}
	if _, err := applyACPAccountBindings(drift, `{"gemini":{"account":"portable","default":"default"}}`); err == nil {
		t.Fatal("a changed default was allowed to reinterpret provider-wide env authority")
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
		got, rest, err := extractPeer("coop run", c.args)
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

	peers, err := a.resolvePeers("coop claude", []string{"claude:opus-4.8"})
	if err != nil || len(peers) != 1 || peers[0].Provider != "claude" || peers[0].Model != "opus-4.8" {
		t.Fatalf("resolvePeers(claude:opus-4.8) = (%+v, %v)", peers, err)
	}
	if _, err := a.resolvePeers("coop claude", []string{"claude@work"}); err == nil {
		t.Error("a peer with an @account must be rejected (a peer runs on its default account)")
	}
	if _, err := a.resolvePeers("coop claude", []string{"codex"}); err == nil {
		t.Error("an unauthed peer must be rejected")
	}
	if _, err := a.resolvePeers("coop claude", []string{"borg"}); err == nil {
		t.Error("an unknown provider must be rejected")
	}
	if peers, err := a.resolvePeers("coop claude", nil); err != nil || peers != nil {
		t.Errorf("resolvePeers(nil) = (%v, %v), want (nil, nil)", peers, err)
	}
}

// A refusal must name the peer the user actually typed. Reporting only the provider ("codex") for
// a `--peer codex:gpt-5.6-sol` sends them hunting for a peer they never named. And the signed-in
// set is taken ONCE for the whole slice: resolvePeerTargets receives it, so nothing in the loop can
// re-scan the credential store per peer.
func TestResolvePeersNamesTheFullPeerAndScansOnce(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "claude", "profiles", "default"), 0o755)
	os.WriteFile(filepath.Join(dir, "claude", "profiles", "default", ".credentials.json"), []byte("{}"), 0o644)
	a := &app{cfg: &config.Config{ConfigDir: dir}}

	_, err := a.resolvePeers("coop claude", []string{"codex:gpt-5.6-sol"})
	if err == nil || !strings.Contains(err.Error(), "Codex needs a usable account") {
		t.Fatalf("unauthed peer error = %v, want the shared account refusal", err)
	}
	if !strings.Contains(err.Error(), "coop login codex") {
		t.Errorf("the remedy must still name the provider to log into, got %v", err)
	}
	// The list handed in is the only authority: claude IS signed in on disk, so a refusal here can
	// only come from the passed slice — the scan is an input, never re-derived inside the loop.
	if _, err := resolvePeerTargets("coop claude", []string{"claude:opus-4.8"}, nil); err == nil ||
		!strings.Contains(err.Error(), "Claude needs a usable account") {
		t.Errorf("resolvePeerTargets ignored the signed-in list it was given: %v", err)
	}
	peers, err := resolvePeerTargets("coop claude", []string{"claude:opus-4.8", "codex"}, []string{"claude", "codex"})
	if err != nil || len(peers) != 2 {
		t.Fatalf("every peer in the given list = (%+v, %v), want both accepted", peers, err)
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
	if _, err := a.cmdLogin([]string{"claude", "--credential", "work"}); err == nil || !strings.Contains(err.Error(), `Unknown option "--credential" for "coop login"`) {
		t.Errorf("cmdLogin --credential must be an unknown option, got %v", err)
	}
	if _, err := a.cmdLogin([]string{"claude:opus"}); err == nil || !strings.Contains(err.Error(), "does not take a model") {
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

// A nonexistent account in the target must fail fast (before any box/Docker work) on ACP too,
// not just a plain agent run; a stray --credential is rejected on each surface.
func TestRunProfileWiringRejectsUnknown(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	if code, err := a.cmdACP([]string{"claude@ghost"}); code != 2 || err == nil {
		t.Errorf("cmdACP claude@ghost = (%d, %v), want 2 + error", code, err)
	}
	if code, err := a.cmdACP([]string{"claude", "--credential", "ghost"}); code != 2 || err == nil {
		t.Errorf("cmdACP --credential = (%d, %v), want 2 + error", code, err)
	}
}

func TestParseExplicitList(t *testing.T) {
	cases := []struct {
		name    string
		option  listOption
		in      string
		want    []string
		wantErr string
	}{
		{name: "services none", option: initServices, in: "none"},
		{name: "services normalized and de-duplicated", option: initServices, in: "Redis, POSTGRES redis", want: []string{"redis", "postgres"}},
		{name: "unknown service", option: initServices, in: "postgres,mongo", wantErr: `Invalid value "mongo" for "--services" in "coop init"`},
		{name: "none is standalone", option: initServices, in: "postgres,none", wantErr: `Value "none" must be used alone for "--services" in "coop init"`},
		{name: "agents all", option: initAgentsOption(), in: "ALL", want: []string{"claude", "codex", "gemini"}},
		{name: "agents normalized and de-duplicated", option: initAgentsOption(), in: "Codex claude,codex", want: []string{"codex", "claude"}},
		{name: "unknown agent", option: initAgentsOption(), in: "claude,grok", wantErr: `Invalid value "grok" for "--agents" in "coop init"`},
		{name: "all is standalone", option: initAgentsOption(), in: "all,codex", wantErr: `Value "all" must be used alone for "--agents" in "coop init"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.option.parse(tc.in)
			if tc.wantErr != "" {
				// A rejected value names the value, its option and the owning command; an unknown
				// one also states the tokens the parser really accepts.
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parse() = (%v, %v), want error containing %q", got, err, tc.wantErr)
				}
				if strings.Contains(tc.wantErr, "Invalid value") && !strings.Contains(err.Error(), tc.option.choices()) {
					t.Errorf("an invalid value must list the accepted choices %q, got: %v", tc.option.choices(), err)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("parse() = (%v, %v), want (%v, nil)", got, err, tc.want)
			}
		})
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

func TestInitActions(t *testing.T) {
	// A ready repo (git, an agent signed in, no box Dockerfile, no services) has only the two
	// jobs every new project has.
	cfg := &config.Config{ConfigDir: t.TempDir()}
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	titles := func(groups []initAction) []string {
		var out []string
		for _, g := range groups {
			out = append(out, g.title)
		}
		return out
	}
	got := initActions(cfg, repo, nil, []string{"codex"}, true)
	if len(got) != 3 || !strings.HasPrefix(titles(got)[0], "Sign in") {
		t.Fatalf("ready repo actions = %v", titles(got))
	}
	// A bare `coop loop` has no target in a fresh project, so the hint names the agent this
	// project actually set up — otherwise the suggested command fails on the first try.
	if !strings.Contains(got[1].actions[0], "coop doctor") || got[2].actions[1] != "coop loop codex" {
		t.Errorf("fresh repo should verify then start working with a runnable loop: %+v", got)
	}
	if got[0].actions[0] != "coop login codex" {
		t.Errorf("sign-in should name the same agent the loop hint does: %+v", got[0])
	}
	// A re-init keeps only the actions real state still needs — no first-run advice.
	if got := initActions(cfg, repo, nil, []string{"codex"}, false); len(got) != 1 || !strings.HasPrefix(got[0].title, "Sign in") {
		t.Errorf("re-init actions = %v, want only the sign-in job", titles(got))
	}
	// A scaffolded .agent/Dockerfile + services → build, then start them (named), before the
	// first-run jobs.
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "Dockerfile"), []byte("FROM x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = initActions(cfg, repo, []string{"postgres", "redis"}, []string{"codex"}, true)
	if want := []string{"Sign in to an agent", "Build the box", "Start postgres and redis", "Verify the sandbox", "Start working"}; !slices.Equal(titles(got), want) {
		t.Errorf("actions = %v, want %v", titles(got), want)
	}
	if got[1].actions[0] != "coop build" || got[1].note != "Review .agent/Dockerfile, then run:" || got[2].actions[0] != "coop up" {
		t.Errorf("build/up actions wrong: %+v", got)
	}
	// Outside a git repo the first job is the one that finishes setup — the exact two commands,
	// in order, because only the re-init can set core.hooksPath.
	got = initActions(cfg, t.TempDir(), nil, []string{"codex"}, true)
	if len(got) == 0 || !strings.HasPrefix(got[0].title, "Finish setup") || !slices.Equal(got[0].actions, []string{"git init", "coop init"}) {
		t.Errorf("non-git repo should lead with git init then coop init, got %+v", got)
	}
}

// The collapsed result names only the agents this project selected, and the vendor each of them
// may reach — never all three in a single-agent project.
func TestInitResultSentences(t *testing.T) {
	cases := []struct {
		agents        []string
		shared, reach string
	}{
		{nil, "Instructions, skills, and one task queue are ready for any agent.", ""},
		{[]string{"claude"}, "Claude has instructions, skills, and one task queue.", "Claude can reach Anthropic."},
		{[]string{"claude", "codex"}, "Claude and Codex share instructions, skills, and one task queue.", "Claude can reach Anthropic and Codex can reach OpenAI."},
		{
			[]string{"claude", "codex", "gemini"},
			"Claude, Codex, and Gemini share instructions, skills, and one task queue.",
			"Claude can reach Anthropic, Codex can reach OpenAI, and Gemini can reach Google.",
		},
	}
	for _, c := range cases {
		if got := initSharedLine(c.agents); got != c.shared {
			t.Errorf("initSharedLine(%v) = %q, want %q", c.agents, got, c.shared)
		}
		if got := initProviderLine(c.agents); got != c.reach {
			t.Errorf("initProviderLine(%v) = %q, want %q", c.agents, got, c.reach)
		}
	}
}

// `coop acp` takes a target or preset and coop flags only — a leftover token must be a
// usage error (exit 2), not silently ignored. Returns before any box/Docker work.
func TestCmdACPRejectsExtraArgs(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	for _, args := range [][]string{
		{"claude", "foo"},
		{"claude", "--nope"},
		{"claude", "--supervise"},
		{"unknown-preset", "junk"},
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
		if !strings.Contains(string(out), "coop run — run a command in the box") {
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
	if code, err := a.cmdLogin(nil); code != 2 || err == nil || !strings.Contains(err.Error(), `Missing agent for "coop login"`) {
		t.Errorf("cmdLogin(nil) = (%d, %v), want (2, missing-agent error)", code, err)
	}
	if code, err := a.loginTo("claude", ""); code != 2 || err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Errorf("loginTo(claude) non-tty = (%d, %v), want (2, interactive-terminal error)", code, err)
	}
	if code, err := a.loginTo("bogus", ""); code != 2 || err == nil || !strings.Contains(err.Error(), "Unknown agent") {
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
	if code, err := a.loginTo("claude", "../../escape"); code != 2 || err == nil || !strings.Contains(err.Error(), "Invalid account name") {
		t.Errorf("loginTo bad credential = (%d, %v), want (2, invalid account name)", code, err)
	}
}

// TestStrictFlagParsing: value-bearing coop flags reject a missing value or a stray arg up
// front (exit 2) instead of silently falling back to a default or ignoring the typo. These all
// return before runtime/scaffold work; explicit fixtures keep a regression out of the source tree.
func TestStrictFlagParsing(t *testing.T) {
	cases := []struct {
		name string
		fn   func(*app) (int, error)
	}{
		{"login stray arg", func(a *app) (int, error) { return a.cmdLogin([]string{"claude", "extra"}) }},
		{"init --stack no value", func(a *app) (int, error) { return a.cmdInit([]string{"--stack"}) }},
		{"init --services= no value", func(a *app) (int, error) { return a.cmdInit([]string{"--services="}) }},
		{"init unknown flag", func(a *app) (int, error) { return a.cmdInit([]string{"--bogus"}) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, _ := initApp(t, t.TempDir())
			if code, err := c.fn(a); code != 2 || err == nil {
				t.Errorf("(%d, %v), want (2, error)", code, err)
			}
			for _, root := range []string{a.cfg.RepoOverride, a.cfg.ConfigDir} {
				if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
					t.Fatalf("parser refusal changed its fixture: entries=%v err=%v", entries, err)
				}
			}
		})
	}
}

func TestInitServicesSpellingsWithoutTerminal(t *testing.T) {
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close() })
	t.Cleanup(stub(&os.Stdin, stdin))
	for _, initialized := range []bool{false, true} {
		for _, flag := range []string{"--services", "--services="} {
			t.Run(strconv.FormatBool(initialized)+"/"+flag, func(t *testing.T) {
				root := t.TempDir()
				repo, cfgDir := filepath.Join(root, "repo"), filepath.Join(root, "config")
				t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
				if err := os.Mkdir(repo, 0o700); err != nil {
					t.Fatal(err)
				}
				a := &app{cfg: &config.Config{RepoOverride: repo, ConfigDir: cfgDir, MCPFile: filepath.Join(cfgDir, "mcp.json")}}
				if initialized {
					captureStderr(t, func() {
						if code, err := a.cmdInit(nil); code != 0 || err != nil {
							t.Fatalf("fixture init = (%d, %v)", code, err)
						}
					})
				}
				before := snapshotInitTree(t, root)
				var code int
				var callErr error
				captureStderr(t, func() { code, callErr = a.cmdInit([]string{flag}) })
				if !initialized && flag == "--services" {
					if code != 0 || callErr != nil || !scaffold.Initialized(repo) || !fileExists(a.cfg.MCPFile) {
						t.Fatalf("bare services did not initialize its explicit fixture: (%d, %v)", code, callErr)
					}
					for _, rel := range []string{".git", ".agent/compose.yml"} {
						if pathExists(filepath.Join(repo, rel)) {
							t.Errorf("nonterminal init unexpectedly created %s", rel)
						}
					}
					return
				}
				if code != 2 || callErr == nil {
					t.Fatalf("refusal = (%d, %v), want (2, error)", code, callErr)
				}
				after := snapshotInitTree(t, root)
				if len(before) != len(after) {
					t.Fatal("refusal changed the fixture inventory")
				}
				for name, want := range before {
					got, ok := after[name]
					if !ok || !os.SameFile(want.info, got.info) || want.info.Mode() != got.info.Mode() || want.data != got.data || want.link != got.link {
						t.Fatalf("refusal changed %s", name)
					}
				}
			})
		}
	}
}

// The menu documents coop's --peer wrapper flag and closes with the two pointers a reader
// continues from. `coop <agent> --help` is COOP's page now (the agent's own is behind `--`), so
// the menu must not claim otherwise.
func TestHelpDocumentsPeerAndAgentHelp(t *testing.T) {
	h := helpText(&config.Config{})
	if !strings.Contains(h, "--peer") {
		t.Error("the menu should document the --peer wrapper flag")
	}
	if !strings.Contains(h, "Help and examples: coop help <command>") {
		t.Error("the menu should close with the help pointer")
	}
	if strings.Contains(h, "--help is the agent's own") {
		t.Error("`coop <agent> --help` is coop's own page now; the menu must not say otherwise")
	}
}

// TestPromptLine: coop prompt's line shows non-zero segments only, "·"-separated, in a fixed
// order (todo, in progress, blocked, forks); "" when idle so an embedding prompt stays clean.
// Running loops QUALIFY the fork count — a loop runs in a fork, so two separate numbers would
// read as two separate populations.
func TestPromptLine(t *testing.T) {
	if got := promptLine(tasks.TaskCounts{}, 0, 0, false); got != "" {
		t.Errorf("idle should be empty, got %q", got)
	}
	if got := promptLine(tasks.TaskCounts{Done: 9}, 0, 0, false); got != "" {
		t.Errorf("done-only isn't actionable state — should be empty, got %q", got)
	}
	if got := promptLine(tasks.TaskCounts{Todo: 3, Blocked: 1}, 2, 1, false); got != "3 todo · 1 blocked · 2 forks (1 running)" {
		t.Errorf("got %q", got)
	}
	if got := promptLine(tasks.TaskCounts{Doing: 2}, 1, 0, false); got != "2 in progress · 1 fork" { // singular fork, no running loop to qualify it
		t.Errorf("got %q", got)
	}
	// The unsigned nudge appends when set; alone (no other state) it's the whole line.
	if got := promptLine(tasks.TaskCounts{Todo: 1}, 0, 0, true); got != "1 todo · unsigned commit" {
		t.Errorf("got %q", got)
	}
	if got := promptLine(tasks.TaskCounts{}, 0, 0, true); got != "unsigned commit" {
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
	// No flag, no credentials → empty (.agent/ only).
	if got := scaffoldAgentSet(cfg); len(got) != 0 {
		t.Errorf("no flag + no creds → empty, got %v", got)
	}
}

func TestInitRejectsUnknownExplicitNamesBeforeWrites(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "service", args: []string{"--services", "postgres,mongo"}, want: "Choose postgres, redis, or none."},
		{name: "agent", args: []string{"--agents", "claude,grok"}, want: "Choose claude, codex, gemini, or all."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			a := &app{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
			code, err := a.cmdInit(tc.args)
			if code != 2 || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("cmdInit(%v) = (%d, %v), want valid-choice error", tc.args, code, err)
			}
			if _, statErr := os.Stat(filepath.Join(repo, ".agent")); !os.IsNotExist(statErr) {
				t.Fatalf("invalid explicit list wrote .agent state: %v", statErr)
			}
		})
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
		{"daemon unreachable", "1", "Docker is unavailable"},
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

func TestResolveImageUsesTheBaseImageForLogin(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{
		cfg:           &config.Config{BaseImage: "coop-box", RepoOverride: repo},
		rt:            recordingRuntime(t, filepath.Join(t.TempDir(), "runtime-args")),
		rtSet:         true,
		loginProvider: "grok",
	}
	_, image, err := a.resolveImage()
	if err != nil {
		t.Fatal(err)
	}
	if image != "coop-box" {
		t.Fatalf("login image = %q, want the shared base image", image)
	}
}

func TestRepoCommandsRejectInvalidProjectBeforeRuntimeDetection(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, project.File), []byte("box:\n  egres: none\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "runtime-called")
	shim := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\ntouch "+strconv.Quote(marker)+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo, RuntimeName: shim}}
	code, err := a.dispatch([]string{"run", "--", "true"})
	if code != -1 || err == nil || !strings.Contains(err.Error(), project.File) {
		t.Fatalf("dispatch = (%d, %v), want policy error", code, err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("runtime detection ran before policy validation: %v", statErr)
	}
	a = &app{cfg: &config.Config{RepoOverride: repo, RuntimeName: shim}}
	code, err = a.cmdUpdate([]string{"--box-only"})
	if code != -1 || err == nil || !strings.Contains(err.Error(), project.File) {
		t.Fatalf("update --box-only = (%d, %v), want policy error", code, err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("box update detected the runtime before policy validation: %v", statErr)
	}
}

func TestACPOuterRejectsInvalidProjectBeforeSupervisor(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, project.File), []byte("serve: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	a := &app{
		cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()},
		acpSupervise: func([]string, *acpctl.Control) (int, error) {
			called = true
			return 0, nil
		},
	}
	code, err := a.cmdACP([]string{"codex"})
	if code != -1 || err == nil || !strings.Contains(err.Error(), project.File) {
		t.Fatalf("cmdACP = (%d, %v), want policy error", code, err)
	}
	if called {
		t.Fatal("ACP supervisor started before policy validation")
	}
}

// A bad flag, an unknown provider in the positional target, or a malformed loop.yaml is a usage
// error and reads as one even where no container runtime is installed; the runtime is looked up
// only once the run is otherwise valid.
func TestLoopReportsUsageBeforeRuntimeDiscovery(t *testing.T) {
	repo := t.TempDir()
	if err := tasks.ScaffoldStateDirs(filepath.Join(repo, tasks.TasksRoot)); err != nil {
		t.Fatal(err)
	}
	a := func() *app {
		return &app{cfg: &config.Config{RuntimeName: "coop-no-such-runtime-xyz", ConfigDir: t.TempDir(), RepoOverride: repo}}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"loop", "--max-tasks", "x"}, "Use a whole number greater than 0."},
		{[]string{"loop", "--max-tasks", "0"}, "Use a whole number greater than 0."},
		{[]string{"loop", "nope:target"}, `Unknown agent "nope:target"`},
	} {
		code, err := a().dispatch(c.args)
		if code != 2 || err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "runtime") {
			t.Errorf("coop %s = (%d, %v); want a usage error (2) saying %q with no runtime lookup", strings.Join(c.args, " "), code, err, c.want)
		}
	}
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "loop.yaml"), []byte("work: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, err := a().dispatch([]string{"loop", "codex"}); code != 2 || err == nil || !strings.Contains(err.Error(), "loop.yaml") || strings.Contains(err.Error(), "runtime") {
		t.Errorf("coop loop with a malformed loop.yaml = (%d, %v); want its parse error (2), no runtime lookup", code, err)
	}
	if err := os.Remove(filepath.Join(repo, ".agent", "loop.yaml")); err != nil {
		t.Fatal(err)
	}
	// Valid usage on a signed-in account still needs the runtime before any box work.
	signed := a()
	signInCred(t, signed.cfg, "codex", signed.cfg.DefaultProfileOf("codex"))
	if code, err := signed.dispatch([]string{"loop", "codex"}); code == 2 || err == nil || !strings.Contains(err.Error(), "runtime") {
		t.Errorf("coop loop codex with no runtime = (%d, %v); want the runtime error once usage is valid", code, err)
	}
}

// A stale/activity observation is no longer a workspace-wide refusal. The mount-launch barrier,
// held by actual boxes, owns the dangerous start window instead.
func TestCmdUpAllowsAnIndependentStackBesideARecordedBox(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"),
		[]byte("services:\n  db:\n    image: postgres:18\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	running, err := forkspace.BeginExecution(repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop, Workspace: repo})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forkspace.EndExecution(repo, running) })
	a := &app{
		cfg:   &config.Config{RepoOverride: repo},
		rt:    composeUpRuntime(t, []string{"db"}, 0),
		rtSet: true,
	}
	var code int
	var runErr error
	out := captureStderr(t, func() { code, runErr = a.cmdUp(nil) })
	if code != 0 || runErr != nil || !strings.Contains(out, "Waiting for a safe service-launch window") ||
		!strings.Contains(out, "local-loop") {
		t.Fatalf("cmdUp beside a recorded box = (%d, %v); want a named wait and success\n%s", code, runErr, out)
	}
	if err := forkspace.EndExecution(repo, running); err != nil {
		t.Fatal(err)
	}
	out = captureStderr(t, func() { code, runErr = a.cmdUp(nil) })
	if code != 0 || runErr != nil {
		t.Fatalf("cmdUp after the box ended = (%d, %v); want success\n%s", code, runErr, out)
	}
}

// Without a terminal, `coop up` cannot ask about a secret-looking bind: it starts the services with
// decoys and says which file is hidden and how to approve it. Once approved, it stays quiet.
func TestCmdUpWarnsAboutHiddenServiceSecretsWithoutATerminal(t *testing.T) {
	t.Setenv(box.ServiceStateRootEnv, t.TempDir())
	repo := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("certs/tls.key", "-----BEGIN PRIVATE KEY-----\n")
	write(".agent/compose.yml", "services:\n  keycloak:\n    image: example/keycloak\n    volumes:\n      - \"../certs/tls.key:/certs/tls.key:ro\"\n")
	a := &app{cfg: &config.Config{RepoOverride: repo}, rt: composeUpRuntime(t, []string{"keycloak"}, 0), rtSet: true}
	var code int
	var runErr error
	out := captureStderr(t, func() { code, runErr = a.cmdUp(nil) })
	if code != 0 || runErr != nil {
		t.Fatalf("cmdUp = (%d, %v), want the services to start with decoys; stderr:\n%s", code, runErr, out)
	}
	for _, want := range []string{"⚠ Services will receive empty files", "  certs/tls.key", "To approve access, run coop up at a terminal."} {
		if !strings.Contains(out, want) {
			t.Errorf("cmdUp did not explain the hidden file (%q):\n%s", want, out)
		}
	}
	review, err := box.ReviewServiceSecrets(repo, filepath.Join(repo, ".agent", "compose.yml"))
	if err != nil || review == nil {
		t.Fatalf("review = %+v, err=%v", review, err)
	}
	if err := review.Approve(); err != nil {
		t.Fatal(err)
	}
	out = captureStderr(t, func() { code, runErr = a.cmdUp(nil) })
	if code != 0 || runErr != nil || strings.Contains(out, "empty files") {
		t.Fatalf("approved cmdUp = (%d, %v); stderr:\n%s", code, runErr, out)
	}
}

// `coop acp <agent> --egress filtered` is in the contract, and used to die on
// "unexpected argument": the flags were never parsed and the ACP path never
// admitted. The supervisor admits ONCE, and each child it spawns receives a
// reference to that one capture — never authority, and never its own admission.
func TestACPParsesTheNetworkFlagsAndAdmitsInTheSupervisor(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	cfg := &config.Config{
		ConfigDir: t.TempDir(), RepoOverride: repo, HomeInBox: "/home/node", BoxHome: t.TempDir(),
		BaseImage: "test-base", Homes: true, Egress: "open",
	}
	profileDir := cfg.AgentProfileDir("claude", "default")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credential := `{"claudeAiOauth":{"accessToken":"access","expiresAt":4102444800000,"scopes":["user:inference"]}}`
	if err := os.WriteFile(filepath.Join(profileDir, ".credentials.json"), []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}
	supervised := false
	a := &app{cfg: cfg, rt: recordingRuntime(t, filepath.Join(t.TempDir(), "runtime-args")), rtSet: true,
		acpSupervise: func([]string, *acpctl.Control) (int, error) { supervised = true; return 0, nil }}
	code, err := a.cmdACP([]string{"claude", "--egress", "filtered"})
	// This fixture's runtime is not Docker, so admission refuses at the runtime
	// preflight — which is the point: the flag reached admission instead of being
	// rejected as an argument, and no child was spawned.
	if err == nil || !strings.Contains(err.Error(), "restricted networking needs docker") {
		t.Fatalf("acp --egress filtered = (%d, %v), want a network admission refusal", code, err)
	}
	if supervised {
		t.Fatal("the supervisor started before the network was admitted")
	}
	if a.network.Mode == nil || *a.network.Mode != egress.Filtered {
		t.Fatalf("the network flags were not parsed: %+v", a.network)
	}
	// An open ACP session admits nothing and holds no capture.
	a = &app{cfg: cfg, rt: a.rt, rtSet: true,
		acpSupervise: func([]string, *acpctl.Control) (int, error) { supervised = true; return 0, nil }}
	if code, err := a.cmdACP([]string{"claude", "--egress", "open"}); err != nil || code != 0 || !supervised {
		t.Fatalf("acp --egress open = (%d, %v), want the ordinary supervised session", code, err)
	}
	if a.acpCapture != nil {
		t.Fatal("an open ACP session captured a policy")
	}
	if value, err := a.acpChildCapture("abc"); err != nil || value != "" {
		t.Fatalf("an open session handed a child %q (%v)", value, err)
	}
}

// The child's reference names the supervisor's snapshot and its own attempt, and
// an inherited one never rides in: this supervisor mints what its children get.
func TestACPChildCaptureIsAPerChildReference(t *testing.T) {
	a := &app{cfg: &config.Config{}, acpCapture: &box.CapturedEgress{
		Project: t.TempDir(), Fingerprint: strings.Repeat("a", 64), QualificationID: strings.Repeat("b", 64),
	}}
	first, err := a.acpChildCapture("sup1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.acpChildCapture("sup1")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two children shared one attempt identity")
	}
	for _, want := range []string{a.acpCapture.Project, a.acpCapture.Fingerprint, a.acpCapture.QualificationID, `"session_id":"acp-sup1"`} {
		if !strings.Contains(first, want) {
			t.Errorf("child capture %s is missing %q", first, want)
		}
	}
	cleaned := cleanACPChildEnv([]string{box.SessionNetworkCaptureEnv + "=inherited", acpAccountBindingsEnv + `={"gemini":"other"}`, "PATH=/usr/bin"})
	if slices.ContainsFunc(cleaned, func(value string) bool { return strings.HasPrefix(value, box.SessionNetworkCaptureEnv+"=") }) {
		t.Fatalf("an inherited network capture reached the child: %v", cleaned)
	}
	if slices.ContainsFunc(cleaned, func(value string) bool { return strings.HasPrefix(value, acpAccountBindingsEnv+"=") }) {
		t.Fatalf("inherited ACP account bindings reached the child: %v", cleaned)
	}
}
