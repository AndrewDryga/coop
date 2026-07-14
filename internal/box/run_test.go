package box

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/fusion"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/project"
)

func TestDecideTTY(t *testing.T) {
	cases := []struct {
		spec     RunSpec
		stdinTTY bool
		want     ttyMode
	}{
		{RunSpec{ForceNoTTY: true}, true, ttyStdinOnly},               // ACP wins even on a tty
		{RunSpec{Batch: true}, true, ttyNone},                         // batch never gets a tty
		{RunSpec{}, true, ttyInteractive},                             // interactive terminal
		{RunSpec{}, false, ttyNone},                                   // piped/non-tty
		{RunSpec{ForceNoTTY: true, Batch: true}, false, ttyStdinOnly}, // ACP precedence
	}
	for i, c := range cases {
		if got := decideTTY(c.spec, c.stdinTTY); got != c.want {
			t.Errorf("case %d: decideTTY = %d, want %d", i, got, c.want)
		}
	}
}

func TestResolveWorkdir(t *testing.T) {
	cases := []struct {
		name        string
		specWorkdir string
		cfgWorkdir  string
		repo        string
		want        string
	}{
		{"default is the real repo path", "", "", "/Users/x/proj", "/Users/x/proj"},
		{"COOP_WORKDIR overrides the default", "", "/workspace", "/Users/x/proj", "/workspace"},
		{"explicit spec wins over both", "/probe", "/workspace", "/Users/x/proj", "/probe"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := RunSpec{Repo: c.repo, Workdir: c.specWorkdir}
			cfg := &config.Config{Workdir: c.cfgWorkdir}
			if got := resolveWorkdir(spec, cfg); got != c.want {
				t.Errorf("resolveWorkdir = %q, want %q", got, c.want)
			}
		})
	}
}

func TestAssembleArgsMinimal(t *testing.T) {
	cfg := &config.Config{
		HomeInBox: "/home/node",
		ConfigDir: t.TempDir(), // empty: no env/instructions/mcp
		Egress:    "open",      // the production default (config.Load); else the box fails closed to --network none
	}
	spec := RunSpec{Image: "coop-box", Repo: "/repo", Cmd: []string{"claude"}, Agent: "claude", Homes: true}
	mounts := []Mount{{Kind: Bind, Source: "/repo", Target: "/workspace"}}
	t.Setenv("TZ", "America/Merida") // pin hostTimezone so the exact-args check is machine-independent

	// A plain `coop claude` mounts only its own credential home — never the Codex/Gemini ones.
	got := assembleArgs(cfg, spec, mounts, "/tmp/decoy", "/tmp/decoydir", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, "", "")
	want := []string{
		"run", "--rm", "--label", "coop=box",
		"-e", "TZ=America/Merida",
		"-v", "/repo:/workspace",
		"-v", cfg.AgentDir("claude") + ":/home/node/.claude", // active-profile dir (profiles/default)
		"-e", "CLAUDE_CONFIG_DIR=/home/node/.claude",
		"-e", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0",
		"-e", "CODEX_SQLITE_HOME=/home/node/.codex-state", // every agent's BoxEnv is exported (inert here)
		"-w", "/workspace", "coop-box", "claude",
	}
	if !slices.Equal(got, want) {
		t.Errorf("assembleArgs minimal:\n got %v\nwant %v", got, want)
	}
}

// TestAssembleArgsHostTimezone: every box (any mode, Homes or not) carries the host's
// timezone, so in-box agents render clock times ("try again at 4:28 PM") on the host's
// wall clock — coop parses that prose back host-local when scheduling a rate-limit wait.
func TestAssembleArgsHostTimezone(t *testing.T) {
	t.Setenv("TZ", "Europe/Kyiv")
	if got := hostTimezone(); got != "Europe/Kyiv" {
		t.Fatalf("hostTimezone must honor $TZ first, got %q", got)
	}
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: t.TempDir()}
	got := assembleArgs(cfg, RunSpec{Image: "i", Repo: "/r"}, []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}},
		"/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, "", "")
	if !containsSeq(got, []string{"-e", "TZ=Europe/Kyiv"}) {
		t.Errorf("box args must carry the host timezone: %v", got)
	}
}

func TestAssembleArgsInteractiveTTY(t *testing.T) {
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: t.TempDir()}
	got := assembleArgs(cfg, RunSpec{Image: "i", Repo: "/r"}, []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}},
		"/d", "/dd", "/workspace", ttyInteractive, false, nil, nil, nil, nil, nil, "", "")
	if !slices.Contains(got, "-it") {
		t.Errorf("interactive run should pass -it: %v", got)
	}
	if !containsSeq(got, []string{"-e", "TERM"}) {
		t.Errorf("interactive run should propagate TERM for color: %v", got)
	}
}

func TestAssembleArgsWiresHomesEnvInstructionsMCP(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "env"), []byte("X=1"), 0o644)
	os.WriteFile(filepath.Join(dir, "INSTRUCTIONS.md"), []byte("hi"), 0o644)
	os.WriteFile(filepath.Join(dir, "mcp.json"), []byte("{}"), 0o644)
	cfg := &config.Config{
		HomeInBox: "/home/node",
		ConfigDir: dir,
		MCPFile:   filepath.Join(dir, "mcp.json"),
		MCPInBox:  "/home/node/.mcp.json",
		Egress:    "open", // production default; required for the services-net join below
	}
	spec := RunSpec{Image: "i", Repo: "/r", Agent: "claude", Homes: true, Network: true, Cache: true}
	mcpMounts := []extraMount{{"/tmp/g", "/home/node/.gemini/settings.json"}}
	gitMounts := []extraMount{{"/tmp/gc", "/home/node/.gitconfig"}}
	instructionMounts := []extraMount{{filepath.Join(dir, "INSTRUCTIONS.md"), "/home/node/.claude/CLAUDE.md"}}

	got := assembleArgs(cfg, spec, []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}},
		"/d", "/dd", "/workspace", ttyNone, true, mcpMounts, nil, gitMounts, instructionMounts, nil, "coop-r_default", filepath.Join(dir, "env"))
	joined := slices.Clone(got)

	mustContain := func(seq ...string) {
		t.Helper()
		if !containsSeq(joined, seq) {
			t.Errorf("args missing %v in:\n%v", seq, joined)
		}
	}
	mustContain("-e", "CLAUDE_CONFIG_DIR=/home/node/.claude")
	mustContain("-e", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0")
	mustContain("--env-file", filepath.Join(dir, "env"))
	mustContain("-v", filepath.Join(dir, "INSTRUCTIONS.md")+":/home/node/.claude/CLAUDE.md:ro")
	mustContain("-v", cfg.MCPFile+":/home/node/.mcp.json:ro")
	mustContain("-v", "/tmp/g:/home/node/.gemini/settings.json:ro")
	mustContain("-v", "/tmp/gc:/home/node/.gitconfig:ro")
	mustContain("--network", "coop-r_default")
	mustContain("-v", "coop-cache:/home/node/.cache")
}

func TestRunMountsGeminiSettingsForGeminiScope(t *testing.T) {
	cases := []struct {
		name        string
		agent       string
		consultLead string
		peers       []agents.Target
		mcpBody     string
		wantMounts  int
		forbidMount string
	}{
		{"gemini without MCP", "gemini", "", nil, "", 1, ""},
		{"gemini peer without MCP", "claude", "claude", []agents.Target{{Provider: "gemini"}}, "", 1, ""},
		{"gemini with MCP has one merged mount", "gemini", "", nil, `{"mcpServers":{"x":{"command":"true"}}}`, 1, ""},
		{"claude without MCP", "claude", "", nil, "", 0, ""},
		{"codex without MCP", "codex", "", nil, "", 0, ""},
		{"codex with empty MCP stub", "codex", "", nil, `{"mcpServers":{}}`, 0, ":/home/node/.codex/config.toml:ro"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			configDir := t.TempDir()
			mcpFile := filepath.Join(configDir, "mcp.json")
			if c.mcpBody != "" {
				if err := os.WriteFile(mcpFile, []byte(c.mcpBody), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cfg := &config.Config{
				ConfigDir: configDir,
				HomeInBox: "/home/node",
				MCPFile:   mcpFile,
				MCPInBox:  "/home/node/.mcp.json",
				Egress:    "none",
			}
			recorder := filepath.Join(t.TempDir(), "runtime-args")
			spec := RunSpec{
				Image:       "i",
				Repo:        t.TempDir(),
				Cmd:         []string{"true"},
				Agent:       c.agent,
				Homes:       true,
				Batch:       true,
				Quiet:       true,
				ConsultLead: c.consultLead,
				Peers:       c.peers,
			}
			if code, err := Run(cfg, recorderRuntime(t, recorder), spec); err != nil || code != 0 {
				t.Fatalf("Run = %d, %v; want 0, nil", code, err)
			}
			args, err := os.ReadFile(recorder)
			if err != nil {
				t.Fatal(err)
			}
			const mountTarget = ":/home/node/.gemini/settings.json:ro"
			if got := strings.Count(string(args), mountTarget); got != c.wantMounts {
				t.Errorf("gemini settings mounts = %d, want %d in:\n%s", got, c.wantMounts, args)
			}
			if c.forbidMount != "" && strings.Contains(string(args), c.forbidMount) {
				t.Errorf("inactive MCP must not generate mount %q in:\n%s", c.forbidMount, args)
			}
		})
	}
}

// TestAssembleArgsEgressFailsClosed: the box gets full/services networking ONLY when Egress is
// exactly "open"; "none", a typo, or an unnormalized empty value all yield --network none, so a
// missed config.normalizeEgress can never silently grant outbound at the box boundary.
func TestAssembleArgsEgressFailsClosed(t *testing.T) {
	netArgs := func(egress, servicesNet string) []string {
		cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: t.TempDir(), Egress: egress}
		spec := RunSpec{Image: "i", Repo: "/r", Agent: "claude", Homes: true, Network: true}
		return assembleArgs(cfg, spec, []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}},
			"/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, servicesNet, "")
	}
	// "open" joins a services net; "open" with no services leaves the default bridge (no --network).
	if !containsSeq(netArgs("open", "coop-r_default"), []string{"--network", "coop-r_default"}) {
		t.Error(`Egress "open" should join the services network`)
	}
	if slices.Contains(netArgs("open", ""), "--network") {
		t.Error(`Egress "open" with no services should leave the default bridge (no --network)`)
	}
	// Anything else fails closed to --network none — even a typo, and even with a services net present.
	for _, egress := range []string{"none", "None", "off", "", "yes", "full"} {
		if !containsSeq(netArgs(egress, "coop-r_default"), []string{"--network", "none"}) {
			t.Errorf("Egress %q must fail closed to --network none", egress)
		}
	}
}

// TestAssembleArgsConsultTimeout: a valid COOP_CONSULT_TIMEOUT is forwarded into the box so the
// coop-consult wrapper's per-peer timeout is tunable per-run; empty/invalid is dropped so the
// wrapper's built-in default applies.
func TestAssembleArgsConsultTimeout(t *testing.T) {
	mk := func(timeout string) []string {
		cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: t.TempDir(), ConsultTimeout: timeout}
		spec := RunSpec{Image: "i", Repo: "/r", Agent: "claude", Homes: true}
		return assembleArgs(cfg, spec, []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}},
			"/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, "", "")
	}
	if !containsSeq(mk("3600"), []string{"-e", "COOP_CONSULT_TIMEOUT=3600"}) {
		t.Error("valid timeout should be forwarded as -e COOP_CONSULT_TIMEOUT")
	}
	for _, bad := range []string{"", "0", "-5", "30m", "abc"} {
		if containsSeq(mk(bad), []string{"-e", "COOP_CONSULT_TIMEOUT=" + bad}) {
			t.Errorf("invalid timeout %q should not be forwarded", bad)
		}
	}
}

// containsSeq reports whether want appears as a contiguous subsequence of s.
func containsSeq(s, want []string) bool {
	if len(want) == 0 {
		return true
	}
	for i := 0; i+len(want) <= len(s); i++ {
		if slices.Equal(s[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

// TestInstructionOverrideUsed: an agent with a per-agent override gets it (after the box
// note); an agent without one gets the shared INSTRUCTIONS.md.
func TestInstructionOverrideUsed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "INSTRUCTIONS.md"), []byte("SHARED"), 0o644)
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: dir}
	// The per-agent override lives in the agent's active-profile dir (profiles/default now).
	os.MkdirAll(cfg.AgentDir("claude"), 0o755)
	os.WriteFile(filepath.Join(cfg.AgentDir("claude"), "CLAUDE.md"), []byte("OVERRIDE"), 0o644)

	if c := agentBaseInstructions(cfg, "claude", "CLAUDE.md"); !strings.Contains(c, "OVERRIDE") || strings.Contains(c, "SHARED") {
		t.Errorf("claude should use its per-agent override, not the shared file:\n%s", c)
	}
	if x := agentBaseInstructions(cfg, "codex", "AGENTS.md"); !strings.Contains(x, "SHARED") {
		t.Errorf("codex (no override) should use the shared file:\n%s", x)
	}
}

// TestAssembleArgsMountsInstructions: assembleArgs read-only-mounts the per-agent instruction
// mounts and the fusion/consult augmented mount it is given. The selection (which agents, lead
// excluded) and the content live in instructionPlan/agentBaseInstructions, tested below.
func TestAssembleArgsMountsInstructions(t *testing.T) {
	cfg := &config.Config{HomeInBox: "/home/node"}
	mounts := []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}}
	fusionMounts := []extraMount{
		{"/tmp/fusion", "/home/node/.codex/AGENTS.md"},
		// The coop-consult wrapper mounts at an absolute path on PATH, not under HOME.
		{"/tmp/coop-consult", fusion.ConsultWrapperPath},
	}
	instructionMounts := []extraMount{
		{"/tmp/claude-ins", "/home/node/.claude/CLAUDE.md"},
		{"/tmp/gemini-ins", "/home/node/.gemini/GEMINI.md"},
	}
	got := assembleArgs(cfg, RunSpec{Image: "i", Repo: "/r", Homes: true, FusionGovernor: "codex"}, mounts,
		"/d", "/dd", "/workspace", ttyStdinOnly, false, nil, fusionMounts, nil, instructionMounts, nil, "", "")
	for _, want := range [][]string{
		{"-v", "/tmp/fusion:/home/node/.codex/AGENTS.md:ro"},
		{"-v", "/tmp/coop-consult:" + fusion.ConsultWrapperPath + ":ro"},
		{"-v", "/tmp/claude-ins:/home/node/.claude/CLAUDE.md:ro"},
		{"-v", "/tmp/gemini-ins:/home/node/.gemini/GEMINI.md:ro"},
	} {
		if !containsSeq(got, want) {
			t.Errorf("assembleArgs missing instruction mount %v", want)
		}
	}
}

// TestInstructionPlan: every non-lead agent gets a plan item carrying the box env note; the
// fusion governor and consult lead are excluded (they get their augmented file instead).
func TestInstructionPlan(t *testing.T) {
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: t.TempDir()}
	if got := instructionPlan(cfg, RunSpec{}); got != nil {
		t.Errorf("no homes → no plan, got %v", got)
	}
	plan := instructionPlan(cfg, RunSpec{Homes: true})
	if len(plan) != len(agents.Names()) {
		t.Fatalf("plan has %d items, want one per agent (%d)", len(plan), len(agents.Names()))
	}
	for _, it := range plan {
		if !strings.Contains(it.content, "Environment (coop box)") {
			t.Errorf("%s plan content missing the box env note", it.agent)
		}
	}
	for _, it := range instructionPlan(cfg, RunSpec{Homes: true, FusionGovernor: "codex"}) {
		if it.agent == "codex" {
			t.Error("fusion governor must be excluded from instructionPlan")
		}
	}
	for _, it := range instructionPlan(cfg, RunSpec{Homes: true, ConsultLead: "claude"}) {
		if it.agent == "claude" {
			t.Error("consult lead must be excluded from instructionPlan")
		}
	}
}

// TestEnsureAgentHomesScoped: a run pre-creates home dirs ONLY for the agents it mounts
// (credentialScope) — pre-creating every agent's active-profile dir was a husk factory: a
// deleted profile dir (an empty "default") reappeared on every box run, even runs that
// never involved that agent. A raw run (no agent) creates nothing.
func TestEnsureAgentHomesScoped(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	ensureAgentHomes(cfg, RunSpec{Homes: true, Agent: "claude"}, "/workspace")
	if !dirExists(filepath.Join(dir, "claude")) {
		t.Error("the launched agent's home was not created")
	}
	for _, other := range []string{"codex", "gemini"} {
		if dirExists(filepath.Join(dir, other)) {
			t.Errorf("%s's home was created for a claude-only run — the husk-profile factory is back", other)
		}
	}
	raw := t.TempDir()
	ensureAgentHomes(&config.Config{ConfigDir: raw}, RunSpec{Homes: true}, "/workspace")
	if entries, _ := os.ReadDir(raw); len(entries) != 0 {
		t.Errorf("a raw run (no agent) created agent homes: %v", entries)
	}
}

// TestModelEnvArgs: a scoped agent's resolved model is exported into the box — its own env
// var (claude's ANTHROPIC_MODEL, for the flagless ACP adapter) always, and COOP_PEER_MODEL_<AGENT>
// only on a fusion/consult run (where the coop-consult wrapper expands it into each peer's
// --model). No resolved model → nothing exported; codex has no ModelEnv, so only the wrapper var.
func TestModelEnvArgs(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}

	// No model anywhere → no env args at all.
	if got := modelEnvArgs(cfg, RunSpec{Homes: true, Agent: "claude"}, []string{"claude"}); got != nil {
		t.Errorf("no model → no env args, got %v", got)
	}

	cfg.SetActiveModel("claude", "opus")
	cfg.SetActiveModel("codex", "gpt-5")

	// A plain run exports the agent's own env var but NOT the wrapper var (no consult wired).
	got := modelEnvArgs(cfg, RunSpec{Homes: true, Agent: "claude"}, []string{"claude"})
	if want := []string{"-e", "ANTHROPIC_MODEL=opus"}; !slices.Equal(got, want) {
		t.Errorf("plain run env args = %v, want %v", got, want)
	}

	// A consult run exports the wrapper var per scoped agent too; codex (no ModelEnv) gets
	// only the wrapper var.
	got = modelEnvArgs(cfg, RunSpec{Homes: true, Agent: "claude", ConsultLead: "claude"}, []string{"claude", "codex"})
	want := []string{"-e", "ANTHROPIC_MODEL=opus", "-e", "COOP_PEER_MODEL_CLAUDE=opus", "-e", "COOP_PEER_MODEL_CODEX=gpt-5"}
	if !slices.Equal(got, want) {
		t.Errorf("consult run env args = %v, want %v", got, want)
	}

	// An EXPLICIT peer target's :model pins COOP_PEER_MODEL_<X>, overriding the config default
	// (codex's active model is gpt-5, but the --peer codex:gpt-5.5 target wins).
	got = modelEnvArgs(cfg, RunSpec{Homes: true, Agent: "claude", ConsultLead: "claude",
		Peers: []agents.Target{{Provider: "codex", Model: "gpt-5.5"}}}, []string{"claude", "codex"})
	want = []string{"-e", "ANTHROPIC_MODEL=opus", "-e", "COOP_PEER_MODEL_CLAUDE=opus", "-e", "COOP_PEER_MODEL_CODEX=gpt-5.5"}
	if !slices.Equal(got, want) {
		t.Errorf("explicit peer model env args = %v, want %v", got, want)
	}
}

// TestLeadInstructionMount: a consult lead is ALWAYS excluded from instructionPlan, so it must
// still receive its base instructions here even with NO named peer — otherwise it would run with
// none (no box env note, no INSTRUCTIONS.md). With a peer NAMED, the second-opinion directive is
// injected and coop-consult is wired.
func TestLeadInstructionMount(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "claude", "profiles", "default"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: dir, HomeInBox: "/home/node"}

	content, file, wired, ok := leadInstructionMount(cfg, "claude", nil, nil)
	if !ok || file != "CLAUDE.md" {
		t.Fatalf("leadInstructionMount ok=%v file=%q, want true CLAUDE.md", ok, file)
	}
	if !strings.Contains(content, "Environment (coop box)") {
		t.Errorf("a consult lead with no peers must still get the box env note, got:\n%s", content)
	}
	if wired {
		t.Error("no named peer → no consult directive, so wired must be false")
	}

	// With a peer NAMED, the directive is injected and coop-consult is wired.
	content, _, wired, _ = leadInstructionMount(cfg, "claude", nil, []string{"codex"})
	if !wired {
		t.Error("with a named peer, expected the consult directive to be wired")
	}
	if !strings.Contains(content, "second opinion") {
		t.Errorf("with a named peer, expected the second-opinion directive, got:\n%s", content)
	}
}

// TestAgentBaseInstructions: the box env note is always present and comes first; the user's
// shared INSTRUCTIONS.md, when present, follows it.
func TestAgentBaseInstructions(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: dir}
	got := agentBaseInstructions(cfg, "claude", "CLAUDE.md")
	if !strings.Contains(got, "Environment (coop box)") || !strings.Contains(got, "python (= python3)") {
		t.Errorf("box note missing/incomplete with no user file:\n%s", got)
	}
	os.WriteFile(filepath.Join(dir, "INSTRUCTIONS.md"), []byte("MY RULE"), 0o644)
	got = agentBaseInstructions(cfg, "claude", "CLAUDE.md")
	if i, j := strings.Index(got, "Environment (coop box)"), strings.Index(got, "MY RULE"); i < 0 || j < 0 || i > j {
		t.Errorf("want box note then the user rule, got:\n%s", got)
	}
}

func TestBoxLimits(t *testing.T) {
	// docker/podman get the caps; values come from config.
	cfg := &config.Config{Pids: "4096", Memory: "4g", CPUs: "2", NoNewPrivileges: true}
	for _, rt := range []string{"docker", "podman"} {
		got := boxLimits(cfg, rt)
		for _, want := range [][]string{
			{"--security-opt", "no-new-privileges"}, {"--pids-limit", "4096"},
			{"--memory", "4g"}, {"--cpus", "2"}, {"--cap-drop", "ALL"},
		} {
			if !containsSeq(got, want) {
				t.Errorf("%s: boxLimits missing %v in %v", rt, want, got)
			}
		}
	}
	// Apple `container` (and any non-OCI/unknown runtime) gets none — its CLI differs.
	if got := boxLimits(cfg, "container"); got != nil {
		t.Errorf("container runtime should get no docker flags, got %v", got)
	}
	// Off switches drop the resource/privilege flags, but --cap-drop ALL is unconditional (the
	// agent workloads need no capabilities) — so the floor is exactly that.
	off := &config.Config{Pids: "unlimited", NoNewPrivileges: false}
	if got := boxLimits(off, "docker"); !slices.Equal(got, []string{"--cap-drop", "ALL"}) {
		t.Errorf("with everything off, boxLimits should be just --cap-drop ALL, got %v", got)
	}
}

func TestBuildArgs(t *testing.T) {
	cfg := &config.Config{BaseImage: "coop-box"}

	// Stable build pins the FROM image; fresh (coop update) floats it and adds --pull --no-cache.
	if a := baseBuildArgs(cfg, false); !slices.Equal(a, []string{"build", "--build-arg", "NODE_IMAGE=" + pinnedNodeImage, "-t", "coop-box", "-"}) {
		t.Errorf("base cached: args=%v", a)
	}
	if a := baseBuildArgs(cfg, true); !slices.Equal(a, []string{"build", "--pull", "--no-cache", "--build-arg", "NODE_IMAGE=" + floatingNodeImage, "-t", "coop-box", "-"}) {
		t.Errorf("base fresh: args=%v", a)
	}
	// COOP_AGENT_PACKAGES pins the agent npm specs via a build arg.
	pinned := &config.Config{BaseImage: "coop-box", AgentPackages: "@anthropic-ai/claude-code@1.2.3"}
	if a := baseBuildArgs(pinned, false); !containsSeq(a, []string{"--build-arg", "AGENT_PACKAGES=@anthropic-ai/claude-code@1.2.3"}) {
		t.Errorf("pinned packages not forwarded: %v", a)
	}
	// (A repo with a Dockerfile.agent builds from a shadow-filtered staged context — see
	// TestStageBuildContext; that path lives in Build, not baseBuildArgs.)
}

// A detached fork loop's box is labeled coop.fork=<name> so `coop fork stop` can KillByLabel it
// after SIGKILL (preventing an orphaned container); a non-fork box has no such label.
func TestAssembleArgsForkLabel(t *testing.T) {
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: t.TempDir(), Egress: "open"}
	mounts := []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}}
	with := assembleArgs(cfg, RunSpec{Image: "i", Repo: "/r", Agent: "claude", Homes: true, ForkName: "perf"},
		mounts, "/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, "", "")
	if !containsSeq(with, []string{"--label", "coop.fork=perf"}) {
		t.Errorf("a fork-loop box must be labeled coop.fork=perf: %v", with)
	}
	without := assembleArgs(cfg, RunSpec{Image: "i", Repo: "/r", Agent: "claude", Homes: true},
		mounts, "/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, "", "")
	if slices.Contains(without, "coop.fork=") || containsSeq(without, []string{"--label", "coop.fork=perf"}) {
		t.Errorf("a non-fork box must not carry a coop.fork label: %v", without)
	}
}

// TestAssembleArgsAsdfVolume: the base image mounts the persistent ~/.asdf volume
// (for runtime .tool-versions installs); a per-project image does not.
func TestAssembleArgsAsdfVolume(t *testing.T) {
	cfg := &config.Config{HomeInBox: "/home/node", BaseImage: "coop-box", ConfigDir: t.TempDir()}
	mounts := []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}}
	asdf := []string{"-v", "coop-asdf:/home/node/.asdf"}

	base := assembleArgs(cfg, RunSpec{Image: "coop-box", Repo: "/r", Homes: true}, mounts,
		"/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, "", "")
	if !containsSeq(base, asdf) {
		t.Errorf("base image should mount the asdf volume:\n%v", base)
	}

	custom := assembleArgs(cfg, RunSpec{Image: "coop-myrepo", Repo: "/r", Homes: true}, mounts,
		"/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, "", "")
	if containsSeq(custom, asdf) {
		t.Errorf("a per-project image should not mount the asdf volume:\n%v", custom)
	}
}

func TestAssembleArgsSupervisedLabel(t *testing.T) {
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: t.TempDir()}
	mounts := []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}}

	sup := assembleArgs(cfg, RunSpec{Image: "i", Repo: "/r", SupervisorID: "abc123"}, mounts,
		"/d", "/dd", "/workspace", ttyStdinOnly, false, nil, nil, nil, nil, nil, "", "")
	if !containsSeq(sup, []string{"--label", "coop.supervised=1"}) {
		t.Errorf("supervised run should be tagged coop.supervised=1 so build/update restart it: %v", sup)
	}
	if !containsSeq(sup, []string{"--label", "coop.sup=abc123"}) {
		t.Errorf("supervised run should carry coop.sup=<id> for precise teardown: %v", sup)
	}

	plain := assembleArgs(cfg, RunSpec{Image: "i", Repo: "/r"}, mounts,
		"/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, "", "")
	if slices.Contains(plain, "coop.supervised=1") {
		t.Errorf("non-supervised run must not carry the supervised label: %v", plain)
	}
}

func TestAssembleArgsEgress(t *testing.T) {
	mounts := []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}}
	args := func(egress, networkName string) []string {
		c := &config.Config{HomeInBox: "/home/node", ConfigDir: t.TempDir(), Egress: egress}
		return assembleArgs(c, RunSpec{Image: "i", Repo: "/r"}, mounts, "/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, networkName, "")
	}
	// COOP_EGRESS=none → --network none, overriding any services-net join.
	if got := args("none", "coop-x_default"); !containsSeq(got, []string{"--network", "none"}) {
		t.Errorf("COOP_EGRESS=none should force --network none:\n%v", got)
	}
	// Default (open) with no services net → no --network flag (runtime default bridge, full outbound).
	if got := args("open", ""); containsSeq(got, []string{"--network"}) {
		t.Errorf("default egress + no services net should add no --network:\n%v", got)
	}
	// Default (open) WITH a services net → joins it.
	if got := args("open", "coop-x_default"); !containsSeq(got, []string{"--network", "coop-x_default"}) {
		t.Errorf("default egress should join the services net:\n%v", got)
	}
}

// frontierPreset is a loaded-preset literal for box tests (no YAML round-trip needed):
// one role per mode, matching the docs' frontier recipe.
func frontierPreset() *preset.Preset {
	return &preset.Preset{
		Name: "frontier", LeadAgent: "claude",
		LeadLadder: []agents.Target{{Provider: "claude", Model: "claude-fable-5"}},
		Roles: []preset.Role{
			{Name: "critic", Mode: preset.ModeConsult, Agent: "codex", Model: "gpt-5.5"},
			{Name: "fast", Mode: preset.ModeDelegate, Agent: "gemini", Model: "gemini-3.5-flash"},
			{Name: "thinker", Mode: preset.ModeNative, Agent: "claude", Model: "claude-opus-4-8", Subagent: "deep-reasoner"},
		},
	}
}

// A preset lead's instruction file carries the generated routing contract (exact role
// invocations) instead of the generic second-opinion directive, keeps the base, and
// wires coop-consult only because a consult role exists.
func TestLeadInstructionMountPreset(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "INSTRUCTIONS.md"), []byte("BASE RULES"), 0o644)
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: dir}

	content, file, wired, ok := leadInstructionMount(cfg, "claude", frontierPreset(), nil)
	if !ok || file != "CLAUDE.md" {
		t.Fatalf("mount = (file=%q, ok=%v)", file, ok)
	}
	if !wired {
		t.Error("a consult role must wire coop-consult")
	}
	for _, want := range []string{`preset "frontier"`, "coop-consult critic --fresh", "coop-delegate fast", "@deep-reasoner", "BASE RULES"} {
		if !strings.Contains(content, want) {
			t.Errorf("preset lead instructions missing %q:\n%s", want, content)
		}
	}
	// The generic directive is replaced by the preset block, not stacked on top of it.
	if strings.Contains(content, "second opinion is available") {
		t.Error("preset should replace the generic consult directive")
	}

	// A delegate-only preset wires no consult (nothing read-only to call).
	delegateOnly := &preset.Preset{Name: "d", LeadAgent: "claude",
		Roles: []preset.Role{{Name: "fast", Mode: preset.ModeDelegate, Agent: "gemini"}}}
	if _, _, wired, _ := leadInstructionMount(cfg, "claude", delegateOnly, nil); wired {
		t.Error("a delegate-only preset must not mount coop-consult")
	}
}

// A preset scopes credentials precisely: the lead plus its (authed) consult/delegate
// role agents — never the blanket all-authed-peers widening, and never a native role.
func TestCredentialScopePreset(t *testing.T) {
	dir := t.TempDir()
	for _, agent := range []string{"claude", "codex", "gemini"} {
		os.MkdirAll(filepath.Join(dir, agent, "profiles", "default"), 0o755)
	}
	// All three authed (codex uses auth.json, gemini its own marker file, claude .credentials.json).
	os.WriteFile(filepath.Join(dir, "claude", "profiles", "default", ".credentials.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "codex", "profiles", "default", "auth.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "gemini", "profiles", "default", "gemini-credentials.json"), []byte("{}"), 0o644)
	cfg := &config.Config{ConfigDir: dir}

	p := frontierPreset()
	scope := credentialScope(cfg, RunSpec{Homes: true, Agent: "claude", ConsultLead: "claude", Preset: p})
	if !slices.Equal(scope, []string{"claude", "codex", "gemini"}) {
		t.Errorf("preset scope = %v, want [claude codex gemini] (lead + consult + delegate)", scope)
	}

	// A consult-role-only preset must not drag in agents with no role (no blanket widening).
	oneRole := &preset.Preset{Name: "one", LeadAgent: "claude",
		Roles: []preset.Role{{Name: "critic", Mode: preset.ModeConsult, Agent: "codex"}}}
	scope = credentialScope(cfg, RunSpec{Homes: true, Agent: "claude", ConsultLead: "claude", Preset: oneRole})
	if !slices.Equal(scope, []string{"claude", "codex"}) {
		t.Errorf("one-role preset scope = %v, want [claude codex] — gemini has no role", scope)
	}
}

// A preset with a consult role exports COOP_PEER_MODEL_<AGENT> for scoped agents whose
// model resolves — that's how the consult wrapper learns each role's model.
func TestModelEnvArgsPreset(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	cfg.SetActiveModel("codex", "gpt-5.5")
	spec := RunSpec{Preset: frontierPreset()}
	args := modelEnvArgs(cfg, spec, []string{"codex"})
	if !containsSeq(args, []string{"-e", "COOP_PEER_MODEL_CODEX=gpt-5.5"}) {
		t.Errorf("preset consult run should export the peer model: %v", args)
	}
}

// generatedSubagentFiles returns a coop-<role>.md (name + content) only for the un-referenced
// native role, with the role's model in its frontmatter — not for a referenced native role
// (subagent set) or non-native roles.
func TestGeneratedSubagentFiles(t *testing.T) {
	p := &preset.Preset{Roles: []preset.Role{
		{Name: "thinker", Mode: preset.ModeNative, Agent: "claude", Model: "claude-opus-4-8", When: []string{"architecture"}},
		{Name: "critic", Mode: preset.ModeNative, Agent: "claude", Subagent: "deep-reasoner"},
		{Name: "fast", Mode: preset.ModeDelegate, Agent: "gemini"},
	}}
	got := generatedSubagentFiles(p)
	if len(got) != 1 || got[0].name != "coop-thinker.md" {
		t.Fatalf("want only coop-thinker.md, got %+v", got)
	}
	if !strings.Contains(got[0].content, "name: coop-thinker") || !strings.Contains(got[0].content, "model: claude-opus-4-8") {
		t.Errorf("content missing frontmatter:\n%s", got[0].content)
	}
	if generatedSubagentFiles(nil) != nil {
		t.Error("nil preset should yield no files")
	}
}

// assembleAgentsDir builds a temp dir with ONLY the generated coop-<role> files — the user-level
// agents mount. The repo's own .claude/agents is deliberately NOT copied in: it stays the live repo
// mount, so deleting/editing the user's subagents never drags coop's preset roles along.
func TestAssembleAgentsDir(t *testing.T) {
	dir, err := assembleAgentsDir([]genFile{{"coop-thinker.md", "generated"}})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "coop-thinker.md" {
		t.Fatalf("assembled dir should hold only the generated files, got %v", entries)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "coop-thinker.md")); string(b) != "generated" {
		t.Errorf("generated content mismatch: %q", b)
	}
}

// TestPresetRoleMountsNativeTargetsUserAgents: the generated native-role dir mounts as the box's
// USER-level ~/.claude/agents — never over the repo's .claude/agents, which stays the live repo
// mount the user owns.
func TestPresetRoleMountsNativeTargetsUserAgents(t *testing.T) {
	cfg := &config.Config{HomeInBox: "/home/node"}
	p := &preset.Preset{LeadAgent: "claude", Roles: []preset.Role{
		{Name: "thinker", Mode: preset.ModeNative, Agent: "claude", Model: "claude-opus-4-8"},
	}}
	mounts, _, _, tmpDirs := presetRoleMounts(cfg, RunSpec{Homes: true, Preset: p, ConsultLead: "claude", Repo: t.TempDir()}, "/w")
	defer func() {
		for _, d := range tmpDirs {
			os.RemoveAll(d)
		}
	}()
	var target string
	for _, m := range mounts {
		if strings.HasSuffix(m.box, "/.claude/agents") {
			target = m.box
		}
	}
	if target != "/home/node/.claude/agents" {
		t.Fatalf("native agents mount target = %q, want /home/node/.claude/agents (user-level, not the repo's)", target)
	}
}

// A preset box gets the loop's run id as -e COOP_RUN_ID (so a consult peer can log its usage); an
// empty RunID (outside a loop) injects nothing.
func TestPresetRoleMountsRunID(t *testing.T) {
	cfg := &config.Config{HomeInBox: "/home/node"}
	p := &preset.Preset{LeadAgent: "claude", Roles: []preset.Role{
		{Name: "thinker", Mode: preset.ModeNative, Agent: "claude", Model: "claude-opus-4-8"},
	}}
	args := func(id string) []string {
		_, a, _, dirs := presetRoleMounts(cfg, RunSpec{Homes: true, Preset: p, ConsultLead: "claude", Repo: t.TempDir(), RunID: id}, "/w")
		for _, d := range dirs {
			os.RemoveAll(d)
		}
		return a
	}
	if !containsSeq(args("abc123"), []string{"-e", "COOP_RUN_ID=abc123"}) {
		t.Error("a set RunID should inject -e COOP_RUN_ID")
	}
	if containsSeq(args(""), []string{"-e", "COOP_RUN_ID="}) {
		t.Error("an empty RunID must inject no COOP_RUN_ID")
	}
}

// bp returns a pointer to b — for project.Box's *bool toggles in tests.
func bp(b bool) *bool { return &b }

// TestApplyProjectPolicy: the committed .agent/project.yaml box: policy overlays cfg ONLY where the
// user didn't explicitly set the matching COOP_* — an explicit setting always wins, egress can only
// tighten, and the shared Config is never mutated.
func TestApplyProjectPolicy(t *testing.T) {
	// No box: section → cfg returned unchanged (same pointer).
	base := &config.Config{Egress: "open", AutoUp: true, Pids: "4096"}
	if got := applyProjectPolicy(base, &project.Project{}, &RunSpec{}); got != base {
		t.Error("an empty box: policy must return cfg untouched")
	}

	// Policy fills UNSET knobs: egress tightens to none, caps + auto_up apply, network toggles spec.
	cfg := &config.Config{Egress: "open", AutoUp: true, Pids: "4096", Memory: ""}
	spec := RunSpec{Network: true}
	pol := &project.Project{Box: project.Box{Egress: "none", AutoUp: bp(false), Network: bp(false), Memory: "2g", Pids: "1024"}}
	got := applyProjectPolicy(cfg, pol, &spec)
	if got.Egress != "none" || got.AutoUp || got.Memory != "2g" || got.Pids != "1024" {
		t.Errorf("policy not applied to unset knobs: %+v", got)
	}
	if spec.Network {
		t.Error("box.network:false must turn off spec.Network")
	}
	if cfg.Egress != "open" || !cfg.AutoUp || cfg.Pids != "4096" {
		t.Errorf("the shared Config must NOT be mutated, got %+v", cfg)
	}

	// An EXPLICIT env setting beats the file — the repo can't override the user's own choice.
	t.Setenv("COOP_EGRESS", "open")
	t.Setenv("COOP_MEMORY", "8g")
	expl := config.Load() // Egress=open, Memory=8g, both explicit
	got2 := applyProjectPolicy(expl, &project.Project{Box: project.Box{Egress: "none", Memory: "2g"}}, &RunSpec{})
	if got2.Egress != "open" {
		t.Errorf("explicit COOP_EGRESS=open must beat box.egress:none, got %q", got2.Egress)
	}
	if got2.Memory != "8g" {
		t.Errorf("explicit COOP_MEMORY must beat box.memory, got %q", got2.Memory)
	}
}

func TestSynthSkillsMounts(t *testing.T) {
	repo := t.TempDir()
	mkdir := func(p string) {
		if err := os.MkdirAll(filepath.Join(repo, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// No .agent/skills → nothing to synthesize.
	names := []string{"claude", "codex", "grok"}
	if got, _ := synthSkillsMounts(repo, "/home/node", names); got != nil {
		t.Errorf("no .agent/skills → no mounts, got %v", got)
	}
	// .agent/skills present, no per-agent skills dirs → synthesize for skills-capable agents only.
	mkdir(".agent/skills")
	got, _ := synthSkillsMounts(repo, "/home/node", names)
	boxPaths := map[string]bool{}
	for _, m := range got {
		boxPaths[m.box] = true
	}
	if !boxPaths["/home/node/.claude/skills"] || !boxPaths["/home/node/.codex/skills"] {
		t.Errorf("claude+codex skills should be synthesized: %v", got)
	}
	if boxPaths["/home/node/.grok/skills"] {
		t.Error("grok is not skills-capable — must not synthesize a skills mount")
	}
	// The repo's OWN skills dir wins — no synthesis for that agent (project beats user).
	mkdir(".claude/skills")
	got, _ = synthSkillsMounts(repo, "/home/node", names)
	for _, m := range got {
		if m.box == "/home/node/.claude/skills" {
			t.Errorf("repo has .claude/skills → must not synthesize a user-level one: %v", got)
		}
	}
}
