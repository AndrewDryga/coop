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
		Agents:    []string{"claude", "codex", "gemini"},
		ConfigDir: t.TempDir(), // empty: no env/instructions/mcp
	}
	spec := RunSpec{Image: "coop-box", Repo: "/repo", Cmd: []string{"claude"}, Homes: true}
	mounts := []Mount{{Kind: Bind, Source: "/repo", Target: "/workspace"}}

	got := assembleArgs(cfg, spec, mounts, "/tmp/decoy", "/workspace", ttyNone, false, nil, "")
	want := []string{
		"run", "--rm",
		"-v", "/repo:/workspace",
		"-v", cfg.ConfigDir + "/claude:/home/node/.claude",
		"-v", cfg.ConfigDir + "/codex:/home/node/.codex",
		"-v", cfg.ConfigDir + "/gemini:/home/node/.gemini",
		"-e", "CLAUDE_CONFIG_DIR=/home/node/.claude",
		"-w", "/workspace", "coop-box", "claude",
	}
	if !slices.Equal(got, want) {
		t.Errorf("assembleArgs minimal:\n got %v\nwant %v", got, want)
	}
}

func TestAssembleArgsInteractiveTTY(t *testing.T) {
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: t.TempDir()}
	got := assembleArgs(cfg, RunSpec{Image: "i", Repo: "/r"}, []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}},
		"/d", "/workspace", ttyInteractive, false, nil, "")
	if !slices.Contains(got, "-it") {
		t.Errorf("interactive run should pass -it: %v", got)
	}
}

func TestAssembleArgsWiresHomesEnvInstructionsMCP(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "env"), []byte("X=1"), 0o644)
	os.WriteFile(filepath.Join(dir, "INSTRUCTIONS.md"), []byte("hi"), 0o644)
	os.WriteFile(filepath.Join(dir, "mcp.json"), []byte("{}"), 0o644)
	cfg := &config.Config{
		HomeInBox: "/home/node",
		Agents:    []string{"claude", "codex", "gemini"},
		ConfigDir: dir,
		MCPFile:   filepath.Join(dir, "mcp.json"),
		MCPInBox:  "/home/node/.mcp.json",
	}
	spec := RunSpec{Image: "i", Repo: "/r", Homes: true, Network: true, Cache: true}
	mcpMounts := []extraMount{{"/tmp/g", "/home/node/.gemini/settings.json"}}

	got := assembleArgs(cfg, spec, []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}},
		"/d", "/workspace", ttyNone, true, mcpMounts, "coop-r_default")
	joined := slices.Clone(got)

	mustContain := func(seq ...string) {
		t.Helper()
		if !containsSeq(joined, seq) {
			t.Errorf("args missing %v in:\n%v", seq, joined)
		}
	}
	mustContain("-e", "CLAUDE_CONFIG_DIR=/home/node/.claude")
	mustContain("--env-file", filepath.Join(dir, "env"))
	mustContain("-v", filepath.Join(dir, "INSTRUCTIONS.md")+":/home/node/.claude/CLAUDE.md:ro")
	mustContain("-v", cfg.MCPFile+":/home/node/.mcp.json:ro")
	mustContain("-v", "/tmp/g:/home/node/.gemini/settings.json:ro")
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
	cfg := &config.Config{HomeInBox: "/home/node", Agents: []string{"claude", "codex", "gemini"}, ConfigDir: dir}

	got := assembleArgs(cfg, RunSpec{Image: "i", Repo: "/r", Homes: true},
		[]Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}}, "/d", "/workspace", ttyNone, false, nil, "")
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
