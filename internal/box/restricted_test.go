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
		"-e", "CODEX_SQLITE_HOME=/home/node/.codex-state",
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
		"-e", "CODEX_SQLITE_HOME=/home/node/.codex-state",
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
		"-e", "CODEX_SQLITE_HOME=/home/node/.codex-state",
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
		{"podman", func(cfg *config.Config, spec *RunSpec) runtime.Runtime { return runtime.Runtime{Name: "podman"} }, "docker only"},
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
	spec := RunSpec{Image: "coop-box", Repo: "/repo", Cmd: []string{"claude"}, Agent: "claude", Homes: true, Cache: true, Network: true, Serve: true, RunID: "run-1", SuperviseDescendants: true, ExtraArgs: []string{"-e", "X=1"}}
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
		"-e", "CODEX_SQLITE_HOME=/home/node/.codex-state",
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
