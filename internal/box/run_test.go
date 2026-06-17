package box

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

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
	spec := RunSpec{Image: "coop-box", Repo: "/repo", Cmd: []string{"claude"}, Homes: true}
	mounts := []Mount{{Kind: Bind, Source: "/repo", Target: "/workspace"}}

	got := assembleArgs(cfg, spec, mounts, "/tmp/decoy", "/workspace", ttyNone, false, nil, nil, nil, "")
	want := []string{
		"run", "--rm", "--label", "coop=box",
		"-v", "/repo:/workspace",
		"-v", cfg.ConfigDir + "/claude:/home/node/.claude",
		"-v", cfg.ConfigDir + "/codex:/home/node/.codex",
		"-v", cfg.ConfigDir + "/gemini:/home/node/.gemini",
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
		"/d", "/workspace", ttyInteractive, false, nil, nil, nil, "")
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
	spec := RunSpec{Image: "i", Repo: "/r", Homes: true, Network: true, Cache: true}
	mcpMounts := []extraMount{{"/tmp/g", "/home/node/.gemini/settings.json"}}
	gitMounts := []extraMount{{"/tmp/gc", "/home/node/.gitconfig"}}

	got := assembleArgs(cfg, spec, []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}},
		"/d", "/workspace", ttyNone, true, mcpMounts, nil, gitMounts, "coop-r_default")
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

// TestInstructionOverrideSkipsClaude verifies a per-agent override suppresses the
// shared INSTRUCTIONS mount for that agent only.
func TestInstructionOverrideSkipsClaude(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "INSTRUCTIONS.md"), []byte("hi"), 0o644)
	os.MkdirAll(filepath.Join(dir, "claude"), 0o755)
	os.WriteFile(filepath.Join(dir, "claude", "CLAUDE.md"), []byte("override"), 0o644)
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: dir}

	got := assembleArgs(cfg, RunSpec{Image: "i", Repo: "/r", Homes: true},
		[]Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}}, "/d", "/workspace", ttyNone, false, nil, nil, nil, "")
	for _, a := range got {
		if a == filepath.Join(dir, "INSTRUCTIONS.md")+":/home/node/.claude/CLAUDE.md:ro" {
			t.Error("claude override should suppress the shared CLAUDE.md mount")
		}
	}
	// codex/gemini still get the shared instructions.
	if !containsSeq(got, []string{"-v", filepath.Join(dir, "INSTRUCTIONS.md") + ":/home/node/.codex/AGENTS.md:ro"}) {
		t.Error("codex should still get the shared instructions")
	}
}

// TestAssembleArgsFusionGovernorScoped verifies fusion mode mounts the augmented
// instruction at the governor's path and skips the governor in the shared mounts,
// while peers still get the shared instructions (so a peer can't recurse).
func TestAssembleArgsFusionGovernorScoped(t *testing.T) {
	dir := t.TempDir()
	ins := filepath.Join(dir, "INSTRUCTIONS.md")
	os.WriteFile(ins, []byte("shared"), 0o644)
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: dir}
	spec := RunSpec{Image: "i", Repo: "/r", Homes: true, FusionGovernor: "codex"}
	fusionMounts := []extraMount{{"/tmp/fusion", "/home/node/.codex/AGENTS.md"}}

	got := assembleArgs(cfg, spec, []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}},
		"/d", "/workspace", ttyStdinOnly, false, nil, fusionMounts, nil, "")

	if !containsSeq(got, []string{"-v", "/tmp/fusion:/home/node/.codex/AGENTS.md:ro"}) {
		t.Error("governor should get the fusion-augmented instruction mount")
	}
	if containsSeq(got, []string{"-v", ins + ":/home/node/.codex/AGENTS.md:ro"}) {
		t.Error("governor should be skipped in the shared-instruction mounts (no double mount)")
	}
	if !containsSeq(got, []string{"-v", ins + ":/home/node/.claude/CLAUDE.md:ro"}) {
		t.Error("claude peer should still get the shared instructions")
	}
	if !containsSeq(got, []string{"-v", ins + ":/home/node/.gemini/GEMINI.md:ro"}) {
		t.Error("gemini peer should still get the shared instructions")
	}
}

// TestAssembleArgsConsultLeadScoped: the consult lead's augmented instruction is
// mounted at its path and the lead is skipped in the shared-instruction mounts (no
// double mount), while peers still get the shared instructions (so they don't
// inherit the consult directive and recurse).
func TestAssembleArgsConsultLeadScoped(t *testing.T) {
	dir := t.TempDir()
	ins := filepath.Join(dir, "INSTRUCTIONS.md")
	os.WriteFile(ins, []byte("shared"), 0o644)
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: dir}
	spec := RunSpec{Image: "i", Repo: "/r", Homes: true, ConsultLead: "claude"}
	consultMount := []extraMount{{"/tmp/consult", "/home/node/.claude/CLAUDE.md"}}

	got := assembleArgs(cfg, spec, []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}},
		"/d", "/workspace", ttyInteractive, false, nil, consultMount, nil, "")

	if !containsSeq(got, []string{"-v", "/tmp/consult:/home/node/.claude/CLAUDE.md:ro"}) {
		t.Error("consult lead should get the augmented instruction mount")
	}
	if containsSeq(got, []string{"-v", ins + ":/home/node/.claude/CLAUDE.md:ro"}) {
		t.Error("consult lead should be skipped in the shared-instruction mounts (no double mount)")
	}
	if !containsSeq(got, []string{"-v", ins + ":/home/node/.codex/AGENTS.md:ro"}) {
		t.Error("codex peer should still get the shared instructions")
	}
	if !containsSeq(got, []string{"-v", ins + ":/home/node/.gemini/GEMINI.md:ro"}) {
		t.Error("gemini peer should still get the shared instructions")
	}
}

func TestBuildArgs(t *testing.T) {
	cfg := &config.Config{BaseImage: "coop-box"}
	repo := t.TempDir() // no Dockerfile.agent → shared base

	if a, base := buildArgs(cfg, repo, false); !base || !slices.Equal(a, []string{"build", "-t", "coop-box", "-"}) {
		t.Errorf("base cached: base=%v args=%v", base, a)
	}
	if a, _ := buildArgs(cfg, repo, true); !slices.Equal(a, []string{"build", "--pull", "--no-cache", "-t", "coop-box", "-"}) {
		t.Errorf("base fresh: args=%v", a)
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
		"/d", "/workspace", ttyNone, false, nil, nil, nil, "")
	if !containsSeq(base, asdf) {
		t.Errorf("base image should mount the asdf volume:\n%v", base)
	}

	custom := assembleArgs(cfg, RunSpec{Image: "coop-myrepo", Repo: "/r", Homes: true}, mounts,
		"/d", "/workspace", ttyNone, false, nil, nil, nil, "")
	if containsSeq(custom, asdf) {
		t.Errorf("a per-project image should not mount the asdf volume:\n%v", custom)
	}
}
