package box

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
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
	}
	spec := RunSpec{Image: "coop-box", Repo: "/repo", Cmd: []string{"claude"}, Agent: "claude", Homes: true}
	mounts := []Mount{{Kind: Bind, Source: "/repo", Target: "/workspace"}}

	// A plain `coop claude` mounts only its own credential home — never the Codex/Gemini ones.
	got := assembleArgs(cfg, spec, mounts, "/tmp/decoy", "/tmp/decoydir", "/workspace", ttyNone, false, nil, nil, nil, nil, "", "")
	want := []string{
		"run", "--rm", "--label", "coop=box",
		"-v", "/repo:/workspace",
		"-v", cfg.ConfigDir + "/claude:/home/node/.claude",
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
	os.MkdirAll(filepath.Join(dir, "claude"), 0o755)
	os.WriteFile(filepath.Join(dir, "claude", "CLAUDE.md"), []byte("OVERRIDE"), 0o644)
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: dir}

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
	fusionMounts := []extraMount{{"/tmp/fusion", "/home/node/.codex/AGENTS.md"}}
	instructionMounts := []extraMount{
		{"/tmp/claude-ins", "/home/node/.claude/CLAUDE.md"},
		{"/tmp/gemini-ins", "/home/node/.gemini/GEMINI.md"},
	}
	got := assembleArgs(cfg, RunSpec{Image: "i", Repo: "/r", Homes: true, FusionGovernor: "codex"}, mounts,
		"/d", "/dd", "/workspace", ttyStdinOnly, false, nil, fusionMounts, nil, instructionMounts, "", "")
	for _, want := range [][]string{
		{"-v", "/tmp/fusion:/home/node/.codex/AGENTS.md:ro"},
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
	repo := t.TempDir() // no Dockerfile.agent → shared base

	// Stable build pins the FROM image; fresh (coop update) floats it and adds --pull --no-cache.
	if a, base := buildArgs(cfg, repo, false); !base || !slices.Equal(a, []string{"build", "--build-arg", "NODE_IMAGE=" + pinnedNodeImage, "-t", "coop-box", "-"}) {
		t.Errorf("base cached: base=%v args=%v", base, a)
	}
	if a, _ := buildArgs(cfg, repo, true); !slices.Equal(a, []string{"build", "--pull", "--no-cache", "--build-arg", "NODE_IMAGE=" + floatingNodeImage, "-t", "coop-box", "-"}) {
		t.Errorf("base fresh: args=%v", a)
	}
	// COOP_AGENT_PACKAGES pins the agent npm specs via a build arg.
	pinned := &config.Config{BaseImage: "coop-box", AgentPackages: "@anthropic-ai/claude-code@1.2.3"}
	if a, _ := buildArgs(pinned, repo, false); !containsSeq(a, []string{"--build-arg", "AGENT_PACKAGES=@anthropic-ai/claude-code@1.2.3"}) {
		t.Errorf("pinned packages not forwarded: %v", a)
	}

	// A Dockerfile.agent → per-project image built from a context, with the fresh flags.
	os.WriteFile(filepath.Join(repo, "Dockerfile.agent"), []byte("FROM x\n"), 0o644)
	a, base := buildArgs(cfg, repo, true)
	if base {
		t.Error("with a Dockerfile.agent, base should be false")
	}
	if !containsSeq(a, []string{"build", "--pull", "--no-cache"}) ||
		!containsSeq(a, []string{"-f", filepath.Join(repo, "Dockerfile.agent"), repo}) {
		t.Errorf("custom fresh: args=%v", a)
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
