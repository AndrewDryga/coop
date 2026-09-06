package hostsurface

import (
	"strings"
	"testing"
)

func TestClassifyNamesTheActionThatRunsIt(t *testing.T) {
	for p, want := range map[string]string{
		".envrc":                             "cd",
		".gitattributes":                     "your own git",
		".githooks/pre-commit":               "git commit",
		"tools/githooks/post-checkout":       "git commit",
		".husky/pre-push":                    "git commit",
		".pre-commit-config.yaml":            "git commit",
		"Makefile":                           "make",
		"sub/GNUmakefile":                    "make",
		"justfile":                           "just",
		".mcp.json":                          "MCP server",
		".claude/settings.json":              "Claude Code hooks",
		".claude/hooks/stop-guard.sh":        "Claude Code hooks",
		".claude/commands/deploy.md":         "Claude Code project configuration",
		".codex/config.toml":                 "agent configuration",
		".agent/skills/sweep/queue-guard.sh": "skill script",
		".agent/compose.yml":                 "your Docker",
		".agent/Dockerfile":                  "coop build",
		".agent/project.yaml":                "coop configuration",
		".vscode/tasks.json":                 "VS Code",
		".zed/tasks.json":                    "Zed",
		".idea/runConfigurations/x.xml":      "IDE run configuration",
		".github/workflows/ci.yml":           "CI",
	} {
		if got, _ := Classify("M", p); !strings.Contains(got, want) {
			t.Errorf("Classify(%q) = %q, want it to mention %q", p, got, want)
		}
	}
	for _, p := range []string{"main.go", "docs/README.md", "scripts/deploy.sh", ".agent/tasks/README.md", ".agent/kb/card.md", "internal/box/run.go", "package.json", "package-lock.json", ".vscode/extensions.json"} {
		if got, _ := Classify("M", p); got != "" {
			t.Errorf("Classify(%q) = %q, want nothing: ordinary files are not surfaces", p, got)
		}
	}
	if got, _ := Classify("D", ".githooks/pre-commit"); got != "" {
		t.Errorf("a deleted hook cannot run anything, got %q", got)
	}
	// Surfaces that run by themselves block a fork merge; ones you run deliberately only flag.
	for p, automatic := range map[string]bool{".githooks/pre-commit": true, ".claude/settings.json": true, ".agent/compose.yml": true, ".envrc": true, "Makefile": false, "justfile": false} {
		if _, got := Classify("M", p); got != automatic {
			t.Errorf("Classify(%q) automatic = %v, want %v", p, got, automatic)
		}
	}
}

func TestFindingsReadsNameStatusAndKeepsRenameTargets(t *testing.T) {
	listing := "M\tmain.go\nA\t.githooks/pre-commit\nR100\told.mk\tMakefile\nD\t.envrc\nM\t.claude/settings.json\nM\t.claude/settings.json\n"
	got := Findings(listing)
	if len(got) != 3 || got[0].Path != ".claude/settings.json" || got[1].Path != ".githooks/pre-commit" || got[2].Path != "Makefile" {
		t.Fatalf("Findings = %+v; want the hook, the settings file (once) and the renamed Makefile, sorted", got)
	}
	if Findings("") != nil {
		t.Fatal("empty listing must yield nothing")
	}
}
