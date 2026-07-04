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

	// A plain `coop claude` mounts only its own credential home — never the Codex/Gemini ones.
	got := assembleArgs(cfg, spec, mounts, "/tmp/decoy", "/tmp/decoydir", "/workspace", ttyNone, false, nil, nil, nil, nil, "", "")
	want := []string{
		"run", "--rm", "--label", "coop=box",
		"-v", "/repo:/workspace",
		"-v", cfg.AgentDir("claude") + ":/home/node/.claude", // active-profile dir (profiles/default)
		"-e", "CLAUDE_CONFIG_DIR=/home/node/.claude",
		"-e", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0",
		"-w", "/workspace", "coop-box", "claude",
	}
	if !slices.Equal(got, want) {
		t.Errorf("assembleArgs minimal:\n got %v\nwant %v", got, want)
	}
}

func TestAssembleArgsInteractiveTTY(t *testing.T) {
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: t.TempDir()}
	got := assembleArgs(cfg, RunSpec{Image: "i", Repo: "/r"}, []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}},
		"/d", "/dd", "/workspace", ttyInteractive, false, nil, nil, nil, nil, "", "")
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
		"/d", "/dd", "/workspace", ttyNone, true, mcpMounts, nil, gitMounts, instructionMounts, "coop-r_default", filepath.Join(dir, "env"))
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

// TestAssembleArgsEgressFailsClosed: the box gets full/services networking ONLY when Egress is
// exactly "open"; "none", a typo, or an unnormalized empty value all yield --network none, so a
// missed config.normalizeEgress can never silently grant outbound at the box boundary.
func TestAssembleArgsEgressFailsClosed(t *testing.T) {
	netArgs := func(egress, servicesNet string) []string {
		cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: t.TempDir(), Egress: egress}
		spec := RunSpec{Image: "i", Repo: "/r", Agent: "claude", Homes: true, Network: true}
		return assembleArgs(cfg, spec, []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}},
			"/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, servicesNet, "")
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
			"/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, "", "")
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
		"/d", "/dd", "/workspace", ttyStdinOnly, false, nil, fusionMounts, nil, instructionMounts, "", "")
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
}

// TestLeadInstructionMount: a consult lead is ALWAYS excluded from instructionPlan, so it must
// still receive its base instructions here even with no authenticated peer — otherwise it would
// run with none (no box env note, no INSTRUCTIONS.md). With a peer authed, the second-opinion
// directive is injected and coop-consult is wired.
func TestLeadInstructionMount(t *testing.T) {
	dir := t.TempDir()
	// Only claude signed in → authedPeers(claude) is empty. Creds live in profiles/default (the
	// flat vault is retired — migrateFlatVaults moves it there at startup).
	if err := os.MkdirAll(filepath.Join(dir, "claude", "profiles", "default"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude", "profiles", "default", ".credentials.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: dir, HomeInBox: "/home/node"}

	content, file, wired, ok := leadInstructionMount(cfg, "claude", nil)
	if !ok || file != "CLAUDE.md" {
		t.Fatalf("leadInstructionMount ok=%v file=%q, want true CLAUDE.md", ok, file)
	}
	if !strings.Contains(content, "Environment (coop box)") {
		t.Errorf("a consult lead with no peers must still get the box env note, got:\n%s", content)
	}
	if wired {
		t.Error("no authed peer → no consult directive, so wired must be false")
	}

	// With a peer signed in, the directive is injected and coop-consult is wired.
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte("OPENAI_API_KEY=real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	content, _, wired, _ = leadInstructionMount(cfg, "claude", nil)
	if !wired {
		t.Error("with an authed peer, expected the consult directive to be wired")
	}
	if !strings.Contains(content, "second opinion") {
		t.Errorf("with an authed peer, expected the second-opinion directive, got:\n%s", content)
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
		mounts, "/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, "", "")
	if !containsSeq(with, []string{"--label", "coop.fork=perf"}) {
		t.Errorf("a fork-loop box must be labeled coop.fork=perf: %v", with)
	}
	without := assembleArgs(cfg, RunSpec{Image: "i", Repo: "/r", Agent: "claude", Homes: true},
		mounts, "/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, "", "")
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
		"/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, "", "")
	if !containsSeq(base, asdf) {
		t.Errorf("base image should mount the asdf volume:\n%v", base)
	}

	custom := assembleArgs(cfg, RunSpec{Image: "coop-myrepo", Repo: "/r", Homes: true}, mounts,
		"/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, "", "")
	if containsSeq(custom, asdf) {
		t.Errorf("a per-project image should not mount the asdf volume:\n%v", custom)
	}
}

func TestAssembleArgsSupervisedLabel(t *testing.T) {
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: t.TempDir()}
	mounts := []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}}

	sup := assembleArgs(cfg, RunSpec{Image: "i", Repo: "/r", SupervisorID: "abc123"}, mounts,
		"/d", "/dd", "/workspace", ttyStdinOnly, false, nil, nil, nil, nil, "", "")
	if !containsSeq(sup, []string{"--label", "coop.supervised=1"}) {
		t.Errorf("supervised run should be tagged coop.supervised=1 so build/update restart it: %v", sup)
	}
	if !containsSeq(sup, []string{"--label", "coop.sup=abc123"}) {
		t.Errorf("supervised run should carry coop.sup=<id> for precise teardown: %v", sup)
	}

	plain := assembleArgs(cfg, RunSpec{Image: "i", Repo: "/r"}, mounts,
		"/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, "", "")
	if slices.Contains(plain, "coop.supervised=1") {
		t.Errorf("non-supervised run must not carry the supervised label: %v", plain)
	}
}

func TestAssembleArgsEgress(t *testing.T) {
	mounts := []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}}
	args := func(egress, networkName string) []string {
		c := &config.Config{HomeInBox: "/home/node", ConfigDir: t.TempDir(), Egress: egress}
		return assembleArgs(c, RunSpec{Image: "i", Repo: "/r"}, mounts, "/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, networkName, "")
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
		LeadModels: []preset.ModelTarget{{Model: "claude-fable-5"}},
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

	content, file, wired, ok := leadInstructionMount(cfg, "claude", frontierPreset())
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
	if _, _, wired, _ := leadInstructionMount(cfg, "claude", delegateOnly); wired {
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

// assembleAgentsDir builds a temp dir with the repo's existing subagents PLUS the generated
// ones, and never mutates the repo (the dir is what gets mounted over .claude/agents).
func TestAssembleAgentsDir(t *testing.T) {
	repo := t.TempDir()
	agents := filepath.Join(repo, ".claude", "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, "deep-reasoner.md"), []byte("hand-authored"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := assembleAgentsDir(repo, []genFile{{"coop-thinker.md", "generated"}})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	// The temp dir carries both the copied existing subagent and the generated one.
	for _, f := range []string{"deep-reasoner.md", "coop-thinker.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("assembled dir missing %s: %v", f, err)
		}
	}
	// The repo's own .claude/agents is untouched — no coop-thinker.md written there.
	if _, err := os.Stat(filepath.Join(agents, "coop-thinker.md")); !os.IsNotExist(err) {
		t.Errorf("repo .claude/agents must not gain coop-thinker.md (err=%v)", err)
	}
}
