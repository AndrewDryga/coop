package box

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/gatewayimage"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// A stored Claude login good until 2100: the host renews nothing, and the projection must strip
// the refresh token before the seed is built.
const restrictedClaudeLogin = `{"claudeAiOauth":{"accessToken":"access","expiresAt":4102444800000,"scopes":["user:inference","account:read"],"refreshToken":"REFRESH_CANARY"}}`

// dockerRecorder is a runtime whose binary is named docker — so restricted-mode support resolves
// as it does on the qualified runtime — and records every invocation's exact argv, one line per
// invocation with NUL-separated arguments, so an empty argument (`--tools ""`) survives.
func dockerRecorder(t *testing.T, recorder string) runtime.Runtime {
	t.Helper()
	shim := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n{ printf '%s\\0' \"$@\"; printf '\\n'; } >> " + strconv.Quote(recorder) + "\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: shim}
}

// recordedRun returns the argv of the one `run` invocation the recorder saw.
func recordedRun(t *testing.T, recorder string) []string {
	t.Helper()
	data, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	var runs [][]string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		args := strings.Split(strings.TrimSuffix(line, "\x00"), "\x00")
		if len(args) > 0 && args[0] == "run" {
			runs = append(runs, args)
		}
	}
	if len(runs) != 1 {
		t.Fatalf("recorded %d run invocations, want 1:\n%q", len(runs), data)
	}
	return runs[0]
}

func restrictedConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", BaseImage: "coop-box", Egress: "open"}
	profile := cfg.AgentDir("claude")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, ".credentials.json"), []byte(restrictedClaudeLogin), 0o600); err != nil {
		t.Fatal(err)
	}
	// A host-profile customization: a hook. It must NOT reach the seed.
	if err := os.WriteFile(filepath.Join(profile, "settings.json"), []byte(`{"hooks":{"PreToolUse":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// seedFrom finds the run-private seed the args bind and returns its host path.
func seedFrom(t *testing.T, args []string) string {
	t.Helper()
	for i, arg := range args {
		if arg == "-v" && strings.HasSuffix(args[i+1], ":"+restrictedSeedPath+":ro") {
			return strings.TrimSuffix(args[i+1], ":"+restrictedSeedPath+":ro")
		}
	}
	t.Fatalf("no seed mount in %q", args)
	return ""
}

const (
	ownedScratch = "rw,exec,nosuid,nodev,uid=1000,gid=1000,size=1g"
	seedScript   = `cp -R /coop/seed/. "$1"/ && shift && exec "$@"`
)

// The bare golden: no repository, no home, no cache, no asdf, no instruction/git/MCP mount — the
// read-only root, three owned tmpfs, one read-only seed bind, and the provider's own no-tools
// switch on the command.
func TestRunBareMountsOnlyScratchAndTheSeed(t *testing.T) {
	t.Setenv("TZ", "America/Merida")
	cfg := restrictedConfig(t)
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	spec := RunSpec{
		Image: "coop-box", Cmd: []string{"claude", "--dangerously-skip-permissions"}, Agent: "claude", AgentCommand: true,
		Homes: true, Cache: true, Network: true, Serve: true, // the caller's normal defaults are not honored
		Mode: agents.ModeBare, Batch: true, Quiet: true,
	}
	if code, err := Run(cfg, dockerRecorder(t, recorder), spec); err != nil || code != 0 {
		t.Fatalf("Run = %d, %v; want 0, nil", code, err)
	}
	got := recordedRun(t, recorder)
	seed := seedFrom(t, got)
	want := []string{
		"run", "--rm", "--init", "--label", "coop=box",
		// No workspace, so no host scope: an unlabeled box is reported by a sweep, never reaped.
		"-e", "TZ=America/Merida",
		"--cap-drop", "ALL",
		"--read-only",
		"--tmpfs", "/home/node:" + ownedScratch + ",mode=0700",
		"--tmpfs", "/tmp:" + ownedScratch + ",mode=1777",
		"--tmpfs", "/workspace:" + ownedScratch + ",mode=0700",
		"-e", "COOP_BOX=1",
		"-w", "/workspace",
		"-v", seed + ":/coop/seed:ro",
		"-e", "COOP_NO_ASDF=1",
		"-e", "CLAUDE_CONFIG_DIR=/home/node/.claude",
		"-e", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0",
		"-e", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"-e", "DISABLE_UPDATES=1",
		"-e", "CODEX_SQLITE_HOME=/home/node/.codex-state",
		"-e", "GEMINI_TELEMETRY_ENABLED=false",
		"-e", "GROK_TELEMETRY_ENABLED=false",
		"-e", "GROK_DISABLE_AUTOUPDATER=1",
		"coop-box",
		"sh", "-c", seedScript, "coop-seed", "/home/node",
		"claude", "--dangerously-skip-permissions", "--tools", "", "--append-system-prompt", got[len(got)-4],
		"--strict-mcp-config", "--setting-sources", "user",
	}
	if !slices.Equal(got, want) || !strings.Contains(got[len(got)-4], "tool set is empty") {
		t.Errorf("bare run:\n got %q\nwant %q", got, want)
	}
	if _, err := os.Stat(seed); !os.IsNotExist(err) {
		t.Errorf("seed %s must be removed after the run", seed)
	}
}

// The readonly golden: the repository and its secret decoys read-only, and otherwise exactly the
// bare profile without bare's empty cwd. Every -v is :ro; there is no home, cache or asdf bind.
func TestRunReadOnlyMountsRepoReadOnlyAndNothingWritable(t *testing.T) {
	t.Setenv("TZ", "America/Merida")
	cfg := restrictedConfig(t)
	cfg.Egress = "none"
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("SECRET=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	spec := RunSpec{
		Image: "coop-box", Repo: repo, Workdir: "/workspace", Cmd: []string{"claude", "--dangerously-skip-permissions"},
		Agent: "claude", AgentCommand: true, Homes: true, Cache: true, Network: true, Serve: true,
		Mode: agents.ModeReadOnly, Batch: true, Quiet: true,
	}
	if code, err := Run(cfg, dockerRecorder(t, recorder), spec); err != nil || code != 0 {
		t.Fatalf("Run = %d, %v; want 0, nil", code, err)
	}
	got := recordedRun(t, recorder)
	seed := seedFrom(t, got)
	decoy := ""
	for i, arg := range got {
		if arg == "-v" && strings.HasSuffix(got[i+1], ":/workspace/.env:ro") {
			decoy = strings.TrimSuffix(got[i+1], ":/workspace/.env:ro")
		}
	}
	want := []string{
		"run", "--rm", "--init", "--label", "coop=box",
		"--label", "coop.host=" + supervisorLabelValue(workspaceScope(repo), os.Getpid()),
		"-e", "TZ=America/Merida",
		"--cap-drop", "ALL",
		"--read-only",
		"--tmpfs", "/home/node:" + ownedScratch + ",mode=0700",
		"--tmpfs", "/tmp:" + ownedScratch + ",mode=1777",
		"-v", repo + ":/workspace:ro",
		"-v", decoy + ":/workspace/.env:ro",
		"-e", "COOP_BOX=1",
		"--network", "none",
		"-w", "/workspace",
		"-v", seed + ":/coop/seed:ro",
		"-e", "COOP_NO_ASDF=1",
		"-e", "CLAUDE_CONFIG_DIR=/home/node/.claude",
		"-e", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0",
		"-e", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"-e", "DISABLE_UPDATES=1",
		"-e", "CODEX_SQLITE_HOME=/home/node/.codex-state",
		"-e", "GEMINI_TELEMETRY_ENABLED=false",
		"-e", "GROK_TELEMETRY_ENABLED=false",
		"-e", "GROK_DISABLE_AUTOUPDATER=1",
		"coop-box",
		"sh", "-c", seedScript, "coop-seed", "/home/node",
		"claude", "--dangerously-skip-permissions", "--strict-mcp-config", "--setting-sources", "user",
	}
	if !slices.Equal(got, want) {
		t.Errorf("readonly run:\n got %q\nwant %q", got, want)
	}
	for i, arg := range got {
		if (arg == "-v" || arg == "--volume") && !strings.HasSuffix(got[i+1], ":ro") {
			t.Errorf("writable bind %q in a readonly run", got[i+1])
		}
	}
	for _, forbidden := range []string{"coop-cache:", "coop-asdf:", cfg.AgentDir("claude") + ":"} {
		if slices.ContainsFunc(got, func(arg string) bool { return strings.HasPrefix(arg, forbidden) }) {
			t.Errorf("readonly run mounts %s", forbidden)
		}
	}
}

// The ACP form the session daemon launches: the same bare profile, the adapter's own command left
// exactly as it is (its switches ride the session/new the client sends), stdin attached without a
// tty, and the run label the daemon's receipt reaps by. No activity record and no fork label.
func TestRunBareACPKeepsTheAdapterCommandAndRunLabel(t *testing.T) {
	t.Setenv("TZ", "America/Merida")
	cfg := restrictedConfig(t)
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	spec := RunSpec{
		Image: "coop-box", Cmd: []string{"claude-agent-acp"}, Agent: "claude", NetworkClient: egress.ClientACP,
		Homes: true, ForceNoTTY: true, Quiet: true, Mode: agents.ModeBare, RunID: "session-" + strings.Repeat("ab", 12),
	}
	if code, err := Run(cfg, dockerRecorder(t, recorder), spec); err != nil || code != 0 {
		t.Fatalf("Run = %d, %v; want 0, nil", code, err)
	}
	got := recordedRun(t, recorder)
	seed := seedFrom(t, got)
	want := []string{
		"run", "--rm", "--init", "--label", "coop=box",
		"--label", "coop.run=" + spec.RunID,
		"-i",
		"-e", "TZ=America/Merida",
		"--cap-drop", "ALL",
		"--read-only",
		"--tmpfs", "/home/node:" + ownedScratch + ",mode=0700",
		"--tmpfs", "/tmp:" + ownedScratch + ",mode=1777",
		"--tmpfs", "/workspace:" + ownedScratch + ",mode=0700",
		"-e", "COOP_BOX=1",
		"-w", "/workspace",
		"-v", seed + ":/coop/seed:ro",
		"-e", "COOP_NO_ASDF=1",
		"-e", "CLAUDE_CONFIG_DIR=/home/node/.claude",
		"-e", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0",
		"-e", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"-e", "DISABLE_UPDATES=1",
		"-e", "CODEX_SQLITE_HOME=/home/node/.codex-state",
		"-e", "GEMINI_TELEMETRY_ENABLED=false",
		"-e", "GROK_TELEMETRY_ENABLED=false",
		"-e", "GROK_DISABLE_AUTOUPDATER=1",
		"coop-box",
		"sh", "-c", seedScript, "coop-seed", "/home/node",
		"claude-agent-acp",
	}
	if !slices.Equal(got, want) {
		t.Errorf("bare ACP run:\n got %q\nwant %q", got, want)
	}
}

// A raw readonly command (no agent) gets the profile with no seed and no prelude: the probe form.
func TestRunReadOnlyRawCommandHasNoSeed(t *testing.T) {
	cfg := restrictedConfig(t)
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	spec := RunSpec{Image: "coop-box", Repo: t.TempDir(), Workdir: "/workspace", Cmd: []string{"touch", "/workspace/x"}, Homes: true, Mode: agents.ModeReadOnly, Batch: true, Quiet: true}
	if code, err := Run(cfg, dockerRecorder(t, recorder), spec); err != nil || code != 0 {
		t.Fatalf("Run = %d, %v; want 0, nil", code, err)
	}
	got := recordedRun(t, recorder)
	if slices.Contains(got, "sh") || slices.Contains(got, "coop-seed") || slices.ContainsFunc(got, func(a string) bool { return strings.HasSuffix(a, ":/coop/seed:ro") }) {
		t.Fatalf("raw run must carry no seed: %q", got)
	}
	if !slices.Equal(got[len(got)-3:], []string{"coop-box", "touch", "/workspace/x"}) || !slices.Contains(got, "--read-only") {
		t.Fatalf("raw command not launched verbatim under the profile: %q", got)
	}
}

// The seed is the access-only login, the adapter's first-run defaults into an EMPTY profile (the
// host profile's hook never comes along), and the mode's note — nothing else.
func TestBuildRestrictedSeedProjectsLoginAndDefaultsOnly(t *testing.T) {
	cfg := restrictedConfig(t)
	root, err := buildRestrictedSeed(cfg, "claude", agents.ModeReadOnly, "/workspace", compositionArtifactOps{parent: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	home := filepath.Join(root, "home", ".claude")
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{".claude.json", ".credentials.json", "CLAUDE.md", "settings.json"}; !slices.Equal(names, want) {
		t.Fatalf("seed files = %v, want %v", names, want)
	}
	credential, _ := os.ReadFile(filepath.Join(home, ".credentials.json"))
	if strings.Contains(string(credential), "REFRESH_CANARY") || !strings.Contains(string(credential), `"accessToken":"access"`) {
		t.Fatalf("seed must carry the access-only projection: %s", credential)
	}
	if info, _ := os.Stat(filepath.Join(home, ".credentials.json")); info.Mode().Perm() != 0o600 {
		t.Fatalf("seeded credential mode = %o, want 600", info.Mode().Perm())
	}
	settings, _ := os.ReadFile(filepath.Join(home, "settings.json"))
	if strings.Contains(string(settings), "hooks") || !strings.Contains(string(settings), "skipDangerousModePermissionPrompt") {
		t.Fatalf("seed settings must be the adapter defaults, not the host profile: %s", settings)
	}
	state, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	if !strings.Contains(string(state), `"/workspace"`) {
		t.Fatalf("seed must trust the run's workdir: %s", state)
	}
	note, _ := os.ReadFile(filepath.Join(home, "CLAUDE.md"))
	if !strings.Contains(string(note), "read-only") || !strings.Contains(string(note), "/workspace") {
		t.Fatalf("readonly note: %s", note)
	}
	if _, err := os.Stat(filepath.Join(root, "config")); !os.IsNotExist(err) {
		t.Fatal("the scratch config root must not survive in the seed")
	}
	// The host profile is untouched by the render.
	host, _ := os.ReadFile(filepath.Join(cfg.AgentDir("claude"), "settings.json"))
	if string(host) != `{"hooks":{"PreToolUse":[]}}` {
		t.Fatalf("host settings changed: %s", host)
	}
	bare, err := buildRestrictedSeed(cfg, "claude", agents.ModeBare, BareWorkdir, compositionArtifactOps{parent: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(bare)
	note, _ = os.ReadFile(filepath.Join(bare, "home", ".claude", "CLAUDE.md"))
	if !strings.Contains(string(note), "no repository") || strings.Contains(string(note), "tool") {
		t.Fatalf("bare note states the box, not the tool set (that is the adapter's channel): %s", note)
	}
}

// A login the adapter cannot make portable, or none at all, refuses before any runtime work.
func TestBuildRestrictedSeedRefusesUnportableLogin(t *testing.T) {
	cfg := restrictedConfig(t)
	profile := cfg.AgentDir("claude")
	// Refresh-only: no access token to project.
	if err := os.WriteFile(filepath.Join(profile, ".credentials.json"), []byte(`{"claudeAiOauth":{"refreshToken":"r","scopes":["user:inference"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Renewal would reach for the provider; point it at a closed loopback port instead.
	t.Setenv("CLAUDE_REFRESH_TOKEN_URL_OVERRIDE", "http://127.0.0.1:1/oauth/token")
	if _, err := buildRestrictedSeed(cfg, "claude", agents.ModeBare, BareWorkdir, compositionArtifactOps{parent: t.TempDir()}); err == nil {
		t.Fatal("a refresh-only login has no access-only projection to seed")
	}
	// No marker at all: nothing to seed, the env file (if any) is the login. The seed still renders.
	if err := os.Remove(filepath.Join(profile, ".credentials.json")); err != nil {
		t.Fatal(err)
	}
	root, err := buildRestrictedSeed(cfg, "claude", agents.ModeBare, BareWorkdir, compositionArtifactOps{parent: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if _, err := os.Stat(filepath.Join(root, "home", ".claude", ".credentials.json")); !os.IsNotExist(err) {
		t.Fatal("no marker must seed no credential")
	}
}

// Every exposure the profile does not enforce is refused by name before the runtime is touched.
func TestRunRestrictedRefusesWhatItDoesNotEnforce(t *testing.T) {
	base := func() (*config.Config, RunSpec) {
		cfg := restrictedConfig(t)
		return cfg, RunSpec{Image: "coop-box", Repo: t.TempDir(), Cmd: []string{"claude"}, Agent: "claude", AgentCommand: true, Homes: true, Mode: agents.ModeReadOnly, Batch: true, Quiet: true}
	}
	cases := []struct {
		name string
		edit func(cfg *config.Config, spec *RunSpec) runtime.Runtime
		want string
	}{
		{"unknown mode", func(cfg *config.Config, spec *RunSpec) runtime.Runtime {
			spec.Mode = "readonlyish"
			return runtime.Runtime{}
		}, "unknown execution mode"},
		{"apple container", func(cfg *config.Config, spec *RunSpec) runtime.Runtime { return runtime.Runtime{Name: "container"} }, "docker only"},
		{"filtered egress", func(cfg *config.Config, spec *RunSpec) runtime.Runtime {
			cfg.Egress = "filtered"
			return runtime.Runtime{}
		}, "restricted networking"},
		{"filtered capture", func(cfg *config.Config, spec *RunSpec) runtime.Runtime {
			spec.CapturedEgress = &CapturedEgress{}
			return runtime.Runtime{}
		}, "restricted networking"},
		{"project image", func(cfg *config.Config, spec *RunSpec) runtime.Runtime {
			spec.Image = "coop-myrepo"
			return runtime.Runtime{}
		}, "shared base image"},
		{"peers", func(cfg *config.Config, spec *RunSpec) runtime.Runtime {
			spec.ConsultLead, spec.Peers = "claude", []agents.Target{{Provider: "codex"}}
			return runtime.Runtime{}
		}, "consults no peers"},
		{"preset", func(cfg *config.Config, spec *RunSpec) runtime.Runtime {
			spec.Preset = &preset.Preset{Name: "p"}
			return runtime.Runtime{}
		}, "consults no peers"},
		{"maintenance under an agent scope", func(cfg *config.Config, spec *RunSpec) runtime.Runtime {
			spec.AgentCommand = false
			return runtime.Runtime{}
		}, "maintenance commands are not qualified"},
		{"shared sessions", func(cfg *config.Config, spec *RunSpec) runtime.Runtime {
			spec.ShareACPSessions = true
			return runtime.Runtime{}
		}, "shares no ACP transcript"},
		{"editor supervisor", func(cfg *config.Config, spec *RunSpec) runtime.Runtime {
			spec.SupervisorID = "sup-1"
			return runtime.Runtime{}
		}, "no editor supervisor"},
		{"unqualified ACP adapter", func(cfg *config.Config, spec *RunSpec) runtime.Runtime {
			spec.Agent, spec.Cmd, spec.AgentCommand, spec.NetworkClient = "codex", []string{"codex-acp"}, false, egress.ClientACP
			return runtime.Runtime{}
		}, "not qualified for a readonly session"},
		{"review", func(cfg *config.Config, spec *RunSpec) runtime.Runtime { spec.Review = true; return runtime.Runtime{} }, "review stage"},
		{"protected paths", func(cfg *config.Config, spec *RunSpec) runtime.Runtime {
			spec.RepoReadOnlyPaths = []string{".agent/tasks"}
			return runtime.Runtime{}
		}, "protected path"},
		{"readonly without repo", func(cfg *config.Config, spec *RunSpec) runtime.Runtime { spec.Repo = ""; return runtime.Runtime{} }, "needs the repository"},
		{"bare with repo", func(cfg *config.Config, spec *RunSpec) runtime.Runtime {
			spec.Mode = agents.ModeBare
			return runtime.Runtime{}
		}, "names no repository"},
		{"bind in COOP_RUN_ARGS", func(cfg *config.Config, spec *RunSpec) runtime.Runtime {
			cfg.ExtraRunArgs = []string{"-v", "/host:/box"}
			return runtime.Runtime{}
		}, "only -e KEY=VALUE"},
		{"privilege in ExtraArgs", func(cfg *config.Config, spec *RunSpec) runtime.Runtime {
			spec.ExtraArgs = []string{"--privileged"}
			return runtime.Runtime{}
		}, "only -e KEY=VALUE"},
		{"env import in ExtraArgs", func(cfg *config.Config, spec *RunSpec) runtime.Runtime {
			spec.ExtraArgs = []string{"-e", "HOME"}
			return runtime.Runtime{}
		}, "KEY=VALUE"},
		{"unqualified provider", func(cfg *config.Config, spec *RunSpec) runtime.Runtime {
			spec.Agent, spec.Cmd = "codex", []string{"codex"}
			return runtime.Runtime{}
		}, "not qualified"},
		{"bare re-enabling tools", func(cfg *config.Config, spec *RunSpec) runtime.Runtime {
			spec.Mode, spec.Repo, spec.Cmd = agents.ModeBare, "", []string{"claude", "--tools", "default"}
			return runtime.Runtime{}
		}, "--tools conflicts"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, spec := base()
			recorder := filepath.Join(t.TempDir(), "runtime-args")
			rt := c.edit(cfg, &spec)
			if rt.Name == "" {
				rt = dockerRecorder(t, recorder)
			}
			_, err := Run(cfg, rt, spec)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Run error = %v, want %q", err, c.want)
			}
			if _, statErr := os.Stat(recorder); statErr == nil {
				t.Fatal("the runtime was invoked despite the refusal")
			}
		})
	}
}

// The final options are proven against the plan, so a mount or flag added to the shared assembly
// later cannot widen a restricted run without being admitted here.
func TestValidateRestrictedOptions(t *testing.T) {
	plan := restrictedPlan{workdir: "/w", tmpfs: map[string]bool{"/home/node:x": true}, sources: map[string]bool{"/repo": true, "/seed": true}}
	good := []string{"--init", "--label", "coop=box", "-e", "TZ=UTC", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--read-only", "--tmpfs", "/home/node:x", "-v", "/repo:/w:ro", "-v", "/seed:/coop/seed:ro", "--env-file", "/f", "--network", "none", "-w", "/w"}
	if err := validateRestrictedOptions(good, plan); err != nil {
		t.Fatalf("planned options refused: %v", err)
	}
	// The one hosts entry this profile admits, and only when the launch planned one: the box's own
	// MCP credential broker. A second entry — or any entry nobody planned — refuses the launch.
	brokered := restrictedPlan{workdir: plan.workdir, tmpfs: plan.tmpfs, sources: plan.sources,
		brokerHost: "--add-host=coop-broker:172.18.0.5"}
	if err := validateRestrictedOptions(append(slices.Clone(good), brokered.brokerHost), brokered); err != nil {
		t.Fatalf("the planned broker entry was refused: %v", err)
	}
	for name, options := range map[string][]string{
		"no entry at all":     good,
		"a second entry":      append(slices.Clone(good), brokered.brokerHost, brokered.brokerHost),
		"somewhere else":      append(slices.Clone(good), "--add-host=coop-broker:10.0.0.1"),
		"another name":        append(slices.Clone(good), "--add-host=api.example:10.0.0.1"),
		"an unplanned broker": append(slices.Clone(good), brokered.brokerHost),
	} {
		against := brokered
		if name == "an unplanned broker" {
			against = plan
		}
		if err := validateRestrictedOptions(options, against); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	bad := map[string][]string{
		"writable bind":     {"-v", "/repo:/w"},
		"unplanned source":  {"-v", "/etc:/w:ro"},
		"cache volume":      {"-v", "coop-cache:/home/node/.cache"},
		"unplanned tmpfs":   {"--tmpfs", "/var:rw"},
		"mount":             {"--mount", "type=bind,source=/,target=/x"},
		"privileged":        {"--privileged"},
		"cap-add":           {"--cap-add", "SYS_ADMIN"},
		"publish":           {"-p", "127.0.0.1:1:1"},
		"services network":  {"--network", "coop-r_default"},
		"other workdir":     {"-w", "/repo"},
		"security option":   {"--security-opt", "seccomp=unconfined"},
		"dangling value":    {"-e"},
		"unknown option":    {"--user", "0"},
		"missing read-only": nil,
		"missing tmpfs":     nil,
	}
	for name, extra := range bad {
		options := slices.Clone(good)
		switch name {
		case "missing read-only":
			options = slices.DeleteFunc(options, func(o string) bool { return o == "--read-only" })
		case "missing tmpfs":
			options = options[:9]
			options = append(options, "--read-only", "-w", "/w")
		default:
			options = append(options, extra...)
		}
		if err := validateRestrictedOptions(options, plan); err == nil {
			t.Errorf("%s: accepted %q", name, extra)
		}
	}
}

// Normal mode is untouched: a full-featured spec assembles to exactly the bytes it did before the
// modes existed, and spelling the mode out changes nothing.
func TestAssembleArgsNormalModeGolden(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: dir, BaseImage: "coop-box", MCPFile: filepath.Join(dir, "mcp.json"), MCPInBox: "/home/node/.mcp.json", Egress: "open", ConsultTimeout: "30"}
	t.Setenv("TZ", "America/Merida")
	spec := RunSpec{Image: "coop-box", Repo: "/repo", Cmd: []string{"claude"}, Agent: "claude", Homes: true, Cache: true, Network: true, Serve: true, RunID: "run-1", SuperviseDescendants: true, ExtraArgs: []string{"-e", "X=1"}, claudeMCPFile: cfg.MCPFile}
	mounts := []Mount{{Kind: Bind, Source: "/repo", Target: "/workspace"}, {Kind: Decoy, Target: "/workspace/.env"}}
	assemble := func(spec RunSpec) []string {
		return assembleArgs(cfg, true, spec, mounts, "/d", "/dd", "/workspace", ttyInteractive, true,
			[]extraMount{{"/tmp/g", "/home/node/.gemini/settings.json"}}, []extraMount{{"/tmp/c", "/home/node/.claude/CLAUDE.md"}},
			[]extraMount{{"/tmp/gc", "/home/node/.gitconfig"}}, []extraMount{{"/tmp/i", "/home/node/.codex/AGENTS.md"}},
			[]extraMount{{"/tmp/s", "/home/node/.claude/skills"}}, "coop-repo_default", filepath.Join(dir, "env"), "--cap-drop", "ALL")
	}
	want := []string{
		"run", "--rm", "--init", "--label", "coop=box",
		"--label", "coop.host=" + supervisorLabelValue(workspaceScope("/repo"), os.Getpid()),
		"--label", "coop.run=run-1",
		"-it", "-e", "TERM",
		"-e", "TZ=America/Merida",
		"--cap-drop", "ALL",
		"-v", "/repo:/workspace",
		"-v", "/d:/workspace/.env:ro",
		"-v", cfg.AgentDir("claude") + ":/home/node/.claude",
		"-v", "/tmp/s:/home/node/.claude/skills",
		"-e", "COOP_PRIMARY=claude",
		"-e", "CLAUDE_CONFIG_DIR=/home/node/.claude",
		"-e", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0",
		"-e", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"-e", "DISABLE_UPDATES=1",
		"-e", "CODEX_SQLITE_HOME=/home/node/.codex-state",
		"-e", "GEMINI_TELEMETRY_ENABLED=false",
		"-e", "GROK_TELEMETRY_ENABLED=false",
		"-e", "GROK_DISABLE_AUTOUPDATER=1",
		"-e", "COOP_CONSULT_TIMEOUT=30",
		"-v", "/tmp/i:/home/node/.codex/AGENTS.md:ro",
		"-v", "/tmp/c:/home/node/.claude/CLAUDE.md:ro",
		"-v", "/tmp/gc:/home/node/.gitconfig:ro",
		"-v", cfg.MCPFile + ":/home/node/.mcp.json:ro",
		"-v", "/tmp/g:/home/node/.gemini/settings.json:ro",
		"--env-file", filepath.Join(dir, "env"),
		"-e", "X=1",
		"-e", "COOP_BOX=1",
		"-e", "COOP_SUPERVISE_DESCENDANTS=1",
		"--network", "coop-repo_default",
		"-v", "coop-cache:/home/node/.cache",
		"-v", "coop-asdf:/home/node/.asdf",
		"-w", "/workspace", "coop-box", "claude",
	}
	if got := assemble(spec); !slices.Equal(got, want) {
		t.Errorf("normal run:\n got %q\nwant %q", got, want)
	}
	spelled := spec
	spelled.Mode = agents.ModeNormal
	if got := assemble(spelled); !slices.Equal(got, want) {
		t.Errorf("an explicit normal mode changed the arguments:\n got %q\nwant %q", got, want)
	}
}

// An approved companion rides a readonly run read-only, and passes the launch proof as a planned
// source; its environment binding still reaches the box.
func TestRunReadOnlyMountsCompanionsReadOnly(t *testing.T) {
	cfg := restrictedConfig(t)
	companion, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	spec := RunSpec{
		Image: "coop-box", Repo: t.TempDir(), Workdir: "/workspace", Cmd: []string{"true"}, Homes: true,
		Mode: agents.ModeReadOnly, Batch: true, Quiet: true,
		CompanionRepositories: []CompanionRepository{{Name: "topology", HostPath: companion, BaseCommit: strings.Repeat("a", 40)}},
	}
	if code, err := Run(cfg, dockerRecorder(t, recorder), spec); err != nil || code != 0 {
		t.Fatalf("Run = %d, %v; want 0, nil", code, err)
	}
	got := recordedRun(t, recorder)
	if !slices.Contains(got, companion+":/coop/repositories/topology:ro") {
		t.Fatalf("companion read-only mount missing from %q", got)
	}
	if !slices.ContainsFunc(got, func(a string) bool { return strings.HasPrefix(a, "COOP_COMPANION_REPOSITORIES_JSON=") }) {
		t.Fatalf("companion environment missing from %q", got)
	}
}

// A restricted launch is narrated exactly like an ordinary interactive one — the secrets it hid
// (a bare run mounts nothing and says so), the one Internet access section, the agent's name, and
// why the box stopped — and stays silent in a batch embedding.
func TestRunRestrictedNarratesLikeAnInteractiveLaunch(t *testing.T) {
	cfg := restrictedConfig(t)
	cfg.Egress = "open"
	// The runtime must be named docker (the only one the profile is qualified on); the box exits 4.
	shim := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\ncase \"$1\" in run) exit 4 ;; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := RunSpec{Image: "coop-box", Cmd: []string{"sh", "-c", "exit 4"}, Mode: agents.ModeBare}
	var code int
	var err error
	got := captureStderr(t, func() { code, err = Run(cfg, runtime.Runtime{Name: shim}, spec) })
	if err != nil || code != 4 {
		t.Fatalf("Run = %d, %v; want the box's own exit 4", code, err)
	}
	want := "Protecting secrets\n  ✓ No secret paths to hide\n" +
		"\nConfiguring network access\n  ⚠ Unrestricted — nothing is blocked\n" +
		"\nStarting sh\n" +
		"\nThe Coop box has stopped — main process exited with status 4.\n"
	if got != want {
		t.Fatalf("interactive bare run narrated:\n%q\nwant:\n%q", got, want)
	}
	cfg.Egress = "none"
	readonly := RunSpec{Image: "coop-box", Repo: t.TempDir(), Workdir: "/workspace", Cmd: []string{"sh", "-c", "exit 4"}, Mode: agents.ModeReadOnly}
	got = captureStderr(t, func() { _, _ = Run(cfg, runtime.Runtime{Name: shim}, readonly) })
	if !strings.Contains(got, "Protecting secrets\n  ✓ ") || !strings.Contains(got, "\nConfiguring network access\n  ⚠ Offline — nothing outside the box can be reached\n") ||
		!strings.HasSuffix(got, "\nThe Coop box has stopped — main process exited with status 4.\n") {
		t.Fatalf("interactive readonly run narrated:\n%q", got)
	}
	readonly.Batch = true
	if got := captureStderr(t, func() { _, _ = Run(cfg, runtime.Runtime{Name: shim}, readonly) }); got != "" {
		t.Fatalf("a batch restricted run printed %q", got)
	}
}

// A repository that sits on one of the profile's scratch paths — a run from /tmp outside any
// checkout — is refused by name before anything is narrated, instead of the runtime refusing two
// mounts at one point after the launch already said what it was doing.
func TestRunReadOnlyRefusesAScratchPathRepository(t *testing.T) {
	cfg := restrictedConfig(t)
	for _, repo := range []string{"/tmp", "/tmp/project", "/home/node/x", "/workspace"} {
		spec := RunSpec{Image: "coop-box", Repo: repo, Workdir: "/workspace", Cmd: []string{"true"}, Mode: agents.ModeReadOnly, Batch: true, Quiet: true}
		_, err := Run(cfg, dockerRecorder(t, filepath.Join(t.TempDir(), "runtime-args")), spec)
		if err == nil || !strings.Contains(err.Error(), "box's own scratch") {
			t.Fatalf("repo %s: err = %v, want the scratch-path refusal", repo, err)
		}
	}
}

// readOnlySessionShim is a Docker that answers as the real one does for the helper a read-only
// session's box needs: the image is present, the helper names the address it listens on and then
// holds its stdin open, and every invocation's argv is recorded.
func readOnlySessionShim(t *testing.T, calls, boxEnv string) runtime.Runtime {
	t.Helper()
	shim := filepath.Join(t.TempDir(), "docker")
	writeRepoFile(t, shim, "#!/bin/sh\necho \"$@\" >> "+strconv.Quote(calls)+"\n"+
		"case \"$1 $2\" in \"image inspect\") echo "+gatewayimage.Fingerprint()+"; exit 0 ;; esac\n"+
		"case \"$*\" in *\" broker\") echo 172.18.0.5; cat > /dev/null; exit 0 ;; esac\n"+
		"case \"$1\" in network) exit 1 ;; ps) exit 0 ;; esac\n"+
		"prev=\nfor a in \"$@\"; do [ \"$prev\" = --env-file ] && cat \"$a\" > "+strconv.Quote(boxEnv)+"; prev=$a; done\n")
	if err := os.Chmod(shim, 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: shim}
}

// readOnlySessionFixture is the child the session daemon launches for a read-only session: the
// restricted profile, an ACP adapter, and the daemon's request for the MCP list it will send.
func readOnlySessionFixture(t *testing.T) (*config.Config, RunSpec) {
	t.Helper()
	run := openBrokerFixtureWithEnv(t, openBrokerEnv)
	cfg := run.cfg
	cfg.BaseImage = "coop-box"
	profile := cfg.AgentDir("claude")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, filepath.Join(profile, ".credentials.json"), restrictedClaudeLogin)
	claude, _ := agents.Get("claude")
	spec := run.spec
	spec.Image, spec.Workdir, spec.Mode = cfg.BaseImage, "/workspace", agents.ModeReadOnly
	spec.Cmd, spec.AgentCommand = claude.ACP(cfg), false
	spec.NetworkClient, spec.RunID = egress.ClientACP, "session-run-1"
	return cfg, spec
}

// A read-only session's secret-bearing MCP servers reach a helper beside its box, exactly as an
// ordinary open run's do: the box is given the helper's address and stand-ins, the list the daemon
// will send names the helper's listeners, and no real token enters either.
func TestAReadOnlySessionBrokersItsMCPSecretsToo(t *testing.T) {
	cfg, spec := readOnlySessionFixture(t)
	dir := t.TempDir()
	calls, boxEnv := filepath.Join(dir, "calls"), filepath.Join(dir, "box-env")
	handoff := filepath.Join(dir, "handoff.json")
	t.Setenv(SessionMCPHandoffEnv, handoff)
	if code, err := Run(cfg, readOnlySessionShim(t, calls, boxEnv), spec); err != nil || code != 0 {
		t.Fatalf("read-only session child = %d, %v", code, err)
	}
	helper, box := "", ""
	for _, line := range strings.Split(strings.TrimSpace(string(mustReadFile(t, calls))), "\n") {
		switch {
		case strings.Contains(line, "--name coop-broker-"):
			helper = line
		case strings.Contains(line, "--label coop=box"):
			box = line
		}
	}
	if !strings.Contains(helper, "--label coop=broker") || !strings.Contains(helper, gatewayimage.Tag()+" broker") {
		t.Fatalf("no helper ran beside the read-only box:\n%s", helper)
	}
	if !strings.Contains(box, "--add-host=coop-broker:172.18.0.5") {
		t.Fatalf("the box was not given the helper's address:\n%s", box)
	}
	// The profile still holds: nothing writable, and the hosts entry did not smuggle anything else in.
	for _, forbidden := range []string{"--privileged", "--cap-add", "--add-host=coop-broker:172.18.0.5 --add-host"} {
		if strings.Contains(box, forbidden) {
			t.Errorf("the read-only box was started with %q:\n%s", forbidden, box)
		}
	}
	env := string(mustReadFile(t, boxEnv))
	for _, gone := range []string{"docs-secret", "tickets-secret", "stream-secret"} {
		if strings.Contains(env, gone) {
			t.Fatalf("a brokered secret entered the read-only box: %q", env)
		}
	}
	if !strings.Contains(env, "COOP_MCP_TOKEN_0=") {
		t.Fatalf("the read-only box's environment lacks its stand-ins: %q", env)
	}
	// A restricted box loads no MCP file, so no MCP variable has a consumer in it — not even one
	// belonging to a server no route could carry. Every name the configured file references goes.
	for _, gone := range []string{"TWICE_TOKEN", "TWICE_KEY", "INSIDE_TOKEN"} {
		if strings.Contains(env, gone) {
			t.Errorf("the read-only box's environment still carries %q: %q", gone, env)
		}
	}
	if !strings.Contains(env, "KEPT=kept") {
		t.Errorf("the read-only box lost an ordinary variable: %q", env)
	}
	// What the daemon will send as session/new: listeners and stand-ins, never the private env.
	list := string(mustReadFile(t, handoff))
	for _, want := range []string{"coop-broker:15580", "session-run-1", "Bearer twice-secret"} {
		if !strings.Contains(list, want) {
			t.Errorf("the handoff lacks %q:\n%s", want, list)
		}
	}
	for _, gone := range []string{"docs-secret", "tickets-secret", "stream-secret", "docs.example"} {
		if strings.Contains(list, gone) {
			t.Errorf("the handoff carries %q:\n%s", gone, list)
		}
	}
}

// A hand run is not a session, whatever its environment says. The handoff variable belongs to the
// daemon; a stale one in an operator's shell must not make `coop <agent> --readonly` read the real
// MCP file, start a helper holding those tokens, or give its box a route to one.
func TestAHandRunReadOnlyLaunchStillLoadsNoMCP(t *testing.T) {
	cfg, spec := readOnlySessionFixture(t)
	spec.NetworkClient, spec.RunID = "", ""
	spec.Cmd, spec.AgentCommand = []string{"claude"}, true
	dir := t.TempDir()
	calls, boxEnv := filepath.Join(dir, "calls"), filepath.Join(dir, "box-env")
	handoff := filepath.Join(dir, "handoff.json")
	t.Setenv(SessionMCPHandoffEnv, handoff)
	if code, err := Run(cfg, readOnlySessionShim(t, calls, boxEnv), spec); err != nil || code != 0 {
		t.Fatalf("read-only run = %d, %v", code, err)
	}
	recorded := string(mustReadFile(t, calls))
	for _, gone := range []string{"coop-broker", "--add-host", "coop=broker"} {
		if strings.Contains(recorded, gone) {
			t.Fatalf("a hand-run readonly launch started a broker (%q):\n%s", gone, recorded)
		}
	}
	if _, err := os.Stat(handoff); !os.IsNotExist(err) {
		t.Fatalf("a hand-run readonly launch answered a handoff nobody asked for: %v", err)
	}
}

// What a read-only session gets when Coop can broker nothing — every server unbrokerable — is
// exactly today's behaviour, stated rather than assumed: no helper, and the list the daemon sends
// carries the real value, as it did before any of this. Its box still carries neither.
func TestAReadOnlySessionWithNothingToBrokerKeepsTodaysList(t *testing.T) {
	run := openBrokerFixtureWithEnv(t, "TWICE_TOKEN=twice-secret\nTWICE_KEY=twice-key\nKEPT=kept\n")
	cfg := run.cfg
	// Only the server whose secret sits in two places: no fixed route can carry it.
	writeRepoFile(t, cfg.MCPFile, `{"mcpServers":{"twice":{"type":"http","url":"https://twice.example/mcp",`+
		`"bearer_token_env_var":"TWICE_TOKEN","headers":{"X-Key":"${TWICE_KEY}"}}}}`)
	cfg.BaseImage = "coop-box"
	profile := cfg.AgentDir("claude")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, filepath.Join(profile, ".credentials.json"), restrictedClaudeLogin)
	claude, _ := agents.Get("claude")
	spec := run.spec
	spec.Image, spec.Workdir, spec.Mode = cfg.BaseImage, "/workspace", agents.ModeReadOnly
	spec.Cmd, spec.AgentCommand = claude.ACP(cfg), false
	spec.NetworkClient, spec.RunID = egress.ClientACP, "session-run-2"
	dir := t.TempDir()
	calls, boxEnv := filepath.Join(dir, "calls"), filepath.Join(dir, "box-env")
	handoff := filepath.Join(dir, "handoff.json")
	t.Setenv(SessionMCPHandoffEnv, handoff)
	if code, err := Run(cfg, readOnlySessionShim(t, calls, boxEnv), spec); err != nil || code != 0 {
		t.Fatalf("read-only session child = %d, %v", code, err)
	}
	if strings.Contains(string(mustReadFile(t, calls)), "--name coop-broker-") {
		t.Error("a helper ran for a server no route can carry")
	}
	if list := string(mustReadFile(t, handoff)); !strings.Contains(list, "Bearer twice-secret") {
		t.Errorf("the unbrokerable server lost the value it has always been sent:\n%s", list)
	}
	if env := string(mustReadFile(t, boxEnv)); strings.Contains(env, "twice-secret") || strings.Contains(env, "twice-key") {
		t.Errorf("the box carries a secret it cannot use: %q", env)
	}
}

// A session whose policy withheld the MCP file still gets an answer: an empty list, not silence —
// the daemon fails the turn when its child hands over nothing.
func TestAReadOnlySessionWithoutMCPHandsOverAnEmptyList(t *testing.T) {
	cfg, spec := readOnlySessionFixture(t)
	if err := os.Remove(cfg.MCPFile); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	handoff := filepath.Join(dir, "handoff.json")
	t.Setenv(SessionMCPHandoffEnv, handoff)
	if code, err := Run(cfg, readOnlySessionShim(t, filepath.Join(dir, "calls"), filepath.Join(dir, "box-env")), spec); err != nil || code != 0 {
		t.Fatalf("read-only session child = %d, %v", code, err)
	}
	list := string(mustReadFile(t, handoff))
	if !strings.Contains(list, `"mcpServers":[]`) {
		t.Errorf("a session without MCP was handed %s", list)
	}
}

// The helper's private directory holds every brokered secret in cleartext, so it may never be
// allocated inside anything this box can see — the repository OR a companion the read-only run
// mounts beside it. TMPDIR is the operator's, so this is a real configuration, not a contrivance.
func TestAReadOnlySessionKeepsItsHelperOutOfEveryMountedRepository(t *testing.T) {
	for name, inside := range map[string]bool{"a companion it mounts": true, "a directory it does not": false} {
		cfg, spec := readOnlySessionFixture(t)
		companion, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		spec.CompanionRepositories = []CompanionRepository{
			{Name: "design", HostPath: companion, BaseCommit: strings.Repeat("a", 40)},
		}
		tmp := t.TempDir()
		if inside {
			tmp = filepath.Join(companion, "tmp")
			if err := os.MkdirAll(tmp, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("TMPDIR", tmp)
		dir := t.TempDir()
		calls := filepath.Join(dir, "calls")
		handoff := filepath.Join(dir, "handoff.json")
		t.Setenv(SessionMCPHandoffEnv, handoff)
		code, err := Run(cfg, readOnlySessionShim(t, calls, filepath.Join(dir, "box-env")), spec)
		recorded, _ := os.ReadFile(calls)
		started := strings.Contains(string(recorded), "--name coop-broker-")
		if inside {
			if err == nil || started {
				t.Errorf("%s: the helper's secrets were allowed inside it (code %d, err %v)", name, code, err)
			}
			continue
		}
		if err != nil || code != 0 || !started {
			t.Errorf("%s: the helper did not run (code %d, err %v)", name, code, err)
		}
	}
}
