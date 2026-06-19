package scaffold

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDetectStacks(t *testing.T) {
	write := func(repo, rel, content string) {
		t.Helper()
		full := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// go.mod → [go]
	repo := t.TempDir()
	write(repo, "go.mod", "module x\n")
	if got := DetectStacks(repo); !slices.Equal(got, []string{"go"}) {
		t.Errorf("go.mod → %v, want [go]", got)
	}
	// a *.tf file → [terraform]
	repo = t.TempDir()
	write(repo, "main.tf", "resource \"null_resource\" \"x\" {}\n")
	if got := DetectStacks(repo); !slices.Equal(got, []string{"terraform"}) {
		t.Errorf("*.tf → %v, want [terraform]", got)
	}
	// .tool-versions drives detection too, in GateLangs order.
	repo = t.TempDir()
	write(repo, ".tool-versions", "elixir 1.18.3-otp-27\nterraform 1.14.0\n")
	if got := DetectStacks(repo); !slices.Equal(got, []string{"terraform", "elixir"}) {
		t.Errorf(".tool-versions → %v, want [terraform elixir]", got)
	}
	// Nothing detected → none (the no-pollute case — coop won't guess).
	if got := DetectStacks(t.TempDir()); got != nil {
		t.Errorf("empty repo → %v, want nil", got)
	}
}

func TestGateGeneration(t *testing.T) {
	// A detected stack's check goes into the hook for that stack only.
	pre := preCommitHook([]string{"terraform"})
	if !strings.Contains(pre, "command -v terraform") || strings.Contains(pre, "command -v gofmt") {
		t.Errorf("terraform pre-commit should check terraform, not go:\n%s", pre)
	}
	if !strings.Contains(pre, "exit 1") { // a git hook blocks on a nonzero exit
		t.Error("pre-commit hook should block with exit 1")
	}
	// The Claude gate blocks the tool call with exit 2.
	claude := claudeCommitGate([]string{"go"})
	if !strings.Contains(claude, "command -v gofmt") || !strings.Contains(claude, "exit 2") {
		t.Errorf("claude gate should gofmt-check and block with exit 2:\n%s", claude)
	}
	// No stack → a neutral, inert gate (no active check) that still exits 0.
	neutral := preCommitHook(nil)
	if strings.Contains(neutral, "command -v gofmt") {
		t.Errorf("neutral gate must not impose a check:\n%s", neutral)
	}
	if !strings.Contains(neutral, "intentionally empty") || !strings.HasSuffix(strings.TrimSpace(neutral), "exit 0") {
		t.Errorf("neutral gate should be documented-but-inert and end in exit 0:\n%s", neutral)
	}
	// The exit code is parameterized; an unknown lang yields no block.
	if !strings.Contains(gateSnippet("go", "1"), "exit 1") || !strings.Contains(gateSnippet("go", "2"), "exit 2") {
		t.Error("gateSnippet should carry the requested exit code")
	}
	if gateSnippet("nonsense", "1") != "" {
		t.Error("unknown lang → empty snippet")
	}
}
