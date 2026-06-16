package scaffold

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDockerfileTemplatesTrustAnyWorktree guards the real-path-mount contract:
// coop mounts the repo at its real host path and sets the workdir itself, so every
// stack image must trust any worktree (safe.directory '*'), not a fixed /workspace
// (the stale pre-2.0 path, which leaves git with "dubious ownership" on runtimes
// that preserve host uid).
func TestDockerfileTemplatesTrustAnyWorktree(t *testing.T) {
	err := fs.WalkDir(templates, "templates/dockerfile", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		df, err := templates.ReadFile(p)
		if err != nil {
			return err
		}
		switch s := string(df); {
		case !strings.Contains(s, "safe.directory"):
			t.Errorf("%s: no git safe.directory — git won't work on the host-path mount", p)
		case strings.Contains(s, "safe.directory /workspace"):
			t.Errorf("%s: stale safe.directory /workspace; real-path mounts need '*'", p)
		case !strings.Contains(s, "safe.directory '*'"):
			t.Errorf("%s: should trust any worktree with safe.directory '*'", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestInit(t *testing.T) {
	repo := t.TempDir()
	if err := Init(repo, ""); err != nil {
		t.Fatal(err)
	}

	// Core files exist with content.
	for _, rel := range []string{
		"AGENTS.md", ".agent/TASKS.md", ".agent/LOG.md", ".agent/BACKLOG.md",
		".agent/IDEAS.md", ".agent/PENDING_DECISIONS.md",
		".claude/settings.json", ".claude/hooks/stop-guard.sh", ".claude/hooks/commit-gate.sh",
	} {
		fi, err := os.Stat(filepath.Join(repo, rel))
		if err != nil {
			t.Errorf("%s missing: %v", rel, err)
			continue
		}
		if fi.Size() == 0 {
			t.Errorf("%s is empty", rel)
		}
	}

	// Hooks are executable.
	if fi, _ := os.Stat(filepath.Join(repo, ".claude/hooks/stop-guard.sh")); fi != nil && fi.Mode()&0o100 == 0 {
		t.Error("stop-guard.sh is not executable")
	}

	// CLAUDE.md / GEMINI.md are symlinks to AGENTS.md.
	for _, rel := range []string{"CLAUDE.md", "GEMINI.md"} {
		target, err := os.Readlink(filepath.Join(repo, rel))
		if err != nil || target != "AGENTS.md" {
			t.Errorf("%s should symlink to AGENTS.md, got %q (%v)", rel, target, err)
		}
	}
	// Codex shares Claude's skills dir.
	if target, err := os.Readlink(filepath.Join(repo, ".codex/skills")); err != nil || target != "../.claude/skills" {
		t.Errorf(".codex/skills should symlink to ../.claude/skills, got %q (%v)", target, err)
	}

	// Skills were copied.
	if _, err := os.Stat(filepath.Join(repo, ".claude/skills/spec/SKILL.md")); err != nil {
		t.Errorf("skill not copied: %v", err)
	}

	// .gitignore carries the working-state rule.
	gi, _ := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if !strings.Contains(string(gi), ".agent/*") || !strings.Contains(string(gi), "!.agent/rules/") {
		t.Errorf(".gitignore missing working-state rules:\n%s", gi)
	}
}

func TestInitIdempotent(t *testing.T) {
	repo := t.TempDir()
	if err := Init(repo, ""); err != nil {
		t.Fatal(err)
	}
	// Edit a file, then re-init: it must be kept, not overwritten.
	tasks := filepath.Join(repo, ".agent/TASKS.md")
	os.WriteFile(tasks, []byte("MY EDITS"), 0o644)
	if err := Init(repo, ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(tasks); string(b) != "MY EDITS" {
		t.Error("re-init clobbered an edited TASKS.md")
	}
	// .gitignore rule must not be duplicated.
	gi, _ := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if n := strings.Count(string(gi), ".agent/*"); n != 1 {
		t.Errorf(".agent/* appears %d times in .gitignore, want 1", n)
	}
}

func TestInitKeepsRealInstructionFile(t *testing.T) {
	repo := t.TempDir()
	// A real CLAUDE.md (not a symlink) must survive init untouched.
	real := filepath.Join(repo, "CLAUDE.md")
	os.WriteFile(real, []byte("# my project rules"), 0o644)
	if err := Init(repo, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(real); err == nil {
		t.Error("real CLAUDE.md was replaced with a symlink")
	}
	if b, _ := os.ReadFile(real); string(b) != "# my project rules" {
		t.Error("real CLAUDE.md content was changed")
	}
}

func TestInitStack(t *testing.T) {
	repo := t.TempDir()
	if err := Init(repo, "go"); err != nil {
		t.Fatal(err)
	}
	df, err := os.ReadFile(filepath.Join(repo, "Dockerfile.agent"))
	if err != nil || !strings.Contains(string(df), "golang") {
		t.Errorf("go stack Dockerfile.agent missing or wrong:\n%s", df)
	}
	if _, err := os.Stat(filepath.Join(repo, "compose.agent.yml")); err != nil {
		t.Errorf("compose.agent.yml missing: %v", err)
	}

	// Unknown stack falls back to the node toolchain.
	repo2 := t.TempDir()
	if err := Init(repo2, "rust"); err != nil {
		t.Fatal(err)
	}
	df2, _ := os.ReadFile(filepath.Join(repo2, "Dockerfile.agent"))
	if !strings.Contains(string(df2), "FROM node:24") {
		t.Errorf("unknown stack should fall back to node:\n%s", df2)
	}
}

func TestInitToolVersionsAsdf(t *testing.T) {
	// No --stack but a .tool-versions present → scaffold the asdf Dockerfile that
	// installs straight from it.
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, ".tool-versions"), []byte("erlang 29.0.1\nelixir 1.20.0-otp-29\ngolang 1.26.4\n"), 0o644)
	if err := Init(repo, ""); err != nil {
		t.Fatal(err)
	}
	df, err := os.ReadFile(filepath.Join(repo, "Dockerfile.agent"))
	if err != nil {
		t.Fatalf("asdf Dockerfile.agent not written: %v", err)
	}
	for _, want := range []string{"asdf install", ".tool-versions"} {
		if !strings.Contains(string(df), want) {
			t.Errorf("asdf Dockerfile missing %q:\n%s", want, df)
		}
	}

	// No --stack and no .tool-versions → no Dockerfile is scaffolded.
	repo2 := t.TempDir()
	if err := Init(repo2, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo2, "Dockerfile.agent")); !os.IsNotExist(err) {
		t.Error("no stack + no .tool-versions should not scaffold a Dockerfile.agent")
	}

	// An explicit --stack wins over .tool-versions auto-detect.
	repo3 := t.TempDir()
	os.WriteFile(filepath.Join(repo3, ".tool-versions"), []byte("elixir 1.20.0-otp-29\n"), 0o644)
	if err := Init(repo3, "go"); err != nil {
		t.Fatal(err)
	}
	df3, _ := os.ReadFile(filepath.Join(repo3, "Dockerfile.agent"))
	if !strings.Contains(string(df3), "FROM golang") || strings.Contains(string(df3), "asdf install") {
		t.Errorf("explicit --stack should win over .tool-versions:\n%s", df3)
	}
}
