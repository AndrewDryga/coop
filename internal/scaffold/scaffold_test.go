package scaffold

import (
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// The scaffolded asdf Dockerfile pins the agent npm packages in one ARG (it's a static embed,
// unlike the generated base image). Guard that the ARG default stays EXACTLY agents.Packages()
// — same set, same order — so adding, removing, or reordering an agent in coop can't silently
// leave an asdf-stack box installing a stale list.
func TestAsdfDockerfilePackagesMatchRegistry(t *testing.T) {
	data, err := os.ReadFile("templates/dockerfile/asdf")
	if err != nil {
		t.Fatal(err)
	}
	const marker = `ARG AGENT_PACKAGES="`
	content := string(data)
	i := strings.Index(content, marker)
	if i < 0 {
		t.Fatalf("asdf Dockerfile has no %q line", marker)
	}
	rest := content[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatal("asdf AGENT_PACKAGES ARG has no closing quote")
	}
	if got, want := rest[:j], strings.Join(agents.Packages(), " "); got != want {
		t.Errorf("asdf AGENT_PACKAGES drifted from agents.Packages():\n got: %s\nwant: %s", got, want)
	}
}

// TestGeneratedHooksShellcheckClean renders every commit gate coop writes into a user's repo —
// the .githooks/pre-commit and .claude commit gate, for all detected langs and the neutral
// fallback — and asserts shellcheck finds nothing. CI only shellchecks install.sh, so without
// this a generated hook could ship with warnings. Skipped when shellcheck isn't installed.
func TestGeneratedHooksShellcheckClean(t *testing.T) {
	sc := shellcheckPath(t)
	hooks := map[string]string{
		"pre-commit (all langs)":  preCommitHook(GateLangs),
		"claude gate (all langs)": claudeCommitGate(GateLangs),
		"pre-commit (neutral)":    preCommitHook(nil),
		"claude gate (neutral)":   claudeCommitGate(nil),
	}
	for name, body := range hooks {
		path := filepath.Join(t.TempDir(), "hook.sh")
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(sc, path).CombinedOutput(); err != nil {
			t.Errorf("%s is not shellcheck-clean:\n%s", name, out)
		}
	}
}

func shellcheckPath(t *testing.T) string {
	t.Helper()
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck not installed")
	}
	if out, err := exec.Command(sc, "--version").CombinedOutput(); err != nil {
		t.Skipf("shellcheck not usable: %v\n%s", err, out)
	}
	return sc
}

// A pre-existing broad .gitignore line (e.g. .agent/*.log) must NOT make coop init skip writing its
// block — that would drop the !rules/!skills un-ignore and leave tracked dirs ignored. The block is
// monorepo-aware: **/.agent/* (any depth) with !.agent/project.yaml.
func TestUpdateGitignoreBroadPrefixDoesNotSkipBlock(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("node_modules/\n.agent/*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (&scaffolder{repo: repo}).updateGitignore(); err != nil {
		t.Fatal(err)
	}
	gi, _ := os.ReadFile(filepath.Join(repo, ".gitignore"))
	// Knowledge (rules/skills) is un-ignored at any depth so a member may carry its own; only
	// project.yaml is top-level.
	for _, want := range []string{"**/.agent/*\n", "!**/.agent/rules/", "!**/.agent/skills/", "!**/.agent/claude/", "!.agent/project.yaml"} {
		if !strings.Contains(string(gi), want) {
			t.Errorf("coop's block missing %q after a broad .agent/*.log line:\n%s", want, gi)
		}
	}
	// Idempotent: a second run doesn't duplicate the block.
	_ = (&scaffolder{repo: repo}).updateGitignore()
	gi2, _ := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if n := strings.Count(string(gi2), "\n**/.agent/*\n"); n != 1 {
		t.Errorf("coop block written %d times, want 1:\n%s", n, gi2)
	}
}

func TestUpdateGitignoreAddsClaudeFallbackToExistingBlock(t *testing.T) {
	repo := t.TempDir()
	oldBlock := "# coop working state (commit knowledge, ignore state)\n" +
		"**/.agent/*\n" +
		"!**/.agent/rules/\n" +
		"!**/.agent/skills/\n" +
		"!**/.agent/presets/\n" +
		"!**/.agent/loop.yaml\n" +
		"!**/.agent/compose.yml\n" +
		"!.agent/project.yaml\n"
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(oldBlock), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &scaffolder{repo: repo}
	if err := s.updateGitignore(); err != nil {
		t.Fatal(err)
	}
	if err := s.updateGitignore(); err != nil {
		t.Fatal(err)
	}
	gi, err := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(gi), "!**/.agent/claude/"); n != 1 {
		t.Fatalf("Claude fallback allowlist appears %d times, want 1:\n%s", n, gi)
	}
	if n := strings.Count(string(gi), "**/.agent/*\n"); n != 1 {
		t.Fatalf("Coop block appears %d times, want 1:\n%s", n, gi)
	}
}

// TestInitSubproject: a member gets ONLY its own task queue — never the full scaffold (AGENTS.md,
// .claude/, rules), a project.yaml (the root's alone), nor the retired BACKLOG.md.
func TestInitSubproject(t *testing.T) {
	dir := t.TempDir()
	if err := InitSubproject(dir); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".agent/tasks/00_todo", ".agent/tasks/99_done", ".agent/tasks/README.md"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("member missing %s: %v", rel, err)
		}
	}
	for _, rel := range []string{"AGENTS.md", ".claude", ".agent/rules", ".agent/skills", "CLAUDE.md", ".agent/project.yaml", ".agent/BACKLOG.md"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			t.Errorf("member should NOT have %s (top-level only / retired)", rel)
		}
	}
}

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
		case !strings.Contains(s, "chown node:node /home/node/.cache"):
			t.Errorf("%s: should pre-create ~/.cache node-owned, else the coop-cache volume mounts root-owned", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestInit(t *testing.T) {
	repo := t.TempDir()
	if err := Init(repo, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
		t.Fatal(err)
	}

	// Core files exist with content.
	for _, rel := range []string{
		"AGENTS.md", ".agent/tasks/README.md",
		".agent/skills/sweep/queue-guard.sh",
		".agent/claude/settings.json", ".agent/claude/hooks/commit-gate.sh",
		".claude/settings.json", ".claude/hooks/commit-gate.sh",
		".claude/agents/deep-reasoner.md", ".claude/agents/fast-worker.md",
		".githooks/pre-commit", ".githooks/prepare-commit-msg",
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

	// BACKLOG.md is RETIRED — the backlog is now the xx_backlog task-folder drawer (coop backlog).
	// init must not scaffold the old file (nor an empty xx_backlog dir, which is created on demand).
	if _, err := os.Stat(filepath.Join(repo, ".agent/BACKLOG.md")); err == nil {
		t.Error(".agent/BACKLOG.md should no longer be scaffolded (retired for `coop backlog`)")
	}

	// The starter subagents carry the model tiering that is their whole point — the reasoning
	// specialist pinned to Opus, the mechanical worker to Sonnet — via Claude Code's native
	// frontmatter, so a lead on a bigger model spends its tokens on planning and synthesis.
	for rel, model := range map[string]string{
		".claude/agents/deep-reasoner.md": "model: opus",
		".claude/agents/fast-worker.md":   "model: sonnet",
	} {
		data, _ := os.ReadFile(filepath.Join(repo, rel))
		if !strings.Contains(string(data), model) {
			t.Errorf("%s missing its %q pin:\n%s", rel, model, data)
		}
	}

	// The four task state directories exist (the folder-mode queue, with the lifecycle-sort prefix).
	for _, st := range []string{"00_todo", "10_in_progress", "50_blocked", "99_done"} {
		if fi, err := os.Stat(filepath.Join(repo, ".agent/tasks", st)); err != nil || !fi.IsDir() {
			t.Errorf(".agent/tasks/%s should be a directory: %v", st, err)
		}
	}

	// Hooks are executable; the old project-global Stop guard is not scaffolded.
	for _, rel := range []string{".agent/claude/hooks/commit-gate.sh", ".claude/hooks/commit-gate.sh"} {
		if fi, _ := os.Stat(filepath.Join(repo, rel)); fi == nil || fi.Mode()&0o100 == 0 {
			t.Errorf("%s is missing or not executable", rel)
		}
	}
	for _, rel := range []string{".agent/claude/hooks/stop-guard.sh", ".claude/hooks/stop-guard.sh"} {
		if _, err := os.Stat(filepath.Join(repo, rel)); !os.IsNotExist(err) {
			t.Errorf("retired global hook %s should not be scaffolded", rel)
		}
	}
	sharedSettings, err := os.ReadFile(filepath.Join(repo, ".agent/claude/settings.json"))
	if err != nil || !json.Valid(sharedSettings) {
		t.Fatalf("shared Claude settings are missing or invalid JSON: %v\n%s", err, sharedSettings)
	}
	for _, want := range []string{"$CLAUDE_PROJECT_DIR/.claude/hooks/", "$CLAUDE_CONFIG_DIR/hooks/"} {
		if !strings.Contains(string(sharedSettings), want) {
			t.Errorf("shared Claude settings missing hook fallback %q:\n%s", want, sharedSettings)
		}
	}
	assertClaudeHookFallbacks(t, sharedSettings)
	projectSettings, err := os.ReadFile(filepath.Join(repo, ".claude/settings.json"))
	if err != nil || !json.Valid(projectSettings) {
		t.Fatalf("project Claude settings are missing or invalid JSON: %v\n%s", err, projectSettings)
	}
	assertNoClaudeStopHook(t, projectSettings)
	assertProjectClaudeCommitGate(t, projectSettings)
	for _, rel := range []string{".githooks/pre-commit", ".githooks/prepare-commit-msg"} {
		if fi, _ := os.Stat(filepath.Join(repo, rel)); fi == nil || fi.Mode()&0o100 == 0 {
			t.Errorf("%s is missing or not executable", rel)
		}
	}

	// CLAUDE.md / GEMINI.md are symlinks to AGENTS.md.
	for _, rel := range []string{"CLAUDE.md", "GEMINI.md"} {
		target, err := os.Readlink(filepath.Join(repo, rel))
		if err != nil || target != "AGENTS.md" {
			t.Errorf("%s should symlink to AGENTS.md, got %q (%v)", rel, target, err)
		}
	}
	// Every agent's skills dir symlinks to the one canonical .agent/skills.
	for _, rel := range []string{".claude/skills", ".codex/skills", ".gemini/skills"} {
		if target, err := os.Readlink(filepath.Join(repo, rel)); err != nil || target != "../.agent/skills" {
			t.Errorf("%s should symlink to ../.agent/skills, got %q (%v)", rel, target, err)
		}
	}

	// Skills were copied into the canonical dir.
	for _, s := range []string{"spec", "investigate", "review-board"} {
		if _, err := os.Stat(filepath.Join(repo, ".agent/skills", s, "SKILL.md")); err != nil {
			t.Errorf("skill %s not copied: %v", s, err)
		}
	}

	// The canonical cross-agent instructions teach professional use of native
	// orchestration features without depending on one agent's exact command names.
	agents, _ := os.ReadFile(filepath.Join(repo, "AGENTS.md"))
	for _, want := range []string{"/goal", "/batch", "subagents", "native", "do not invent slash commands", "Keep writes serialized"} {
		if !strings.Contains(string(agents), want) {
			t.Errorf("AGENTS.md missing agent-stack guidance %q:\n%s", want, agents)
		}
	}
	// Host-side commands stay out (an agent in the box can't run them). The in-box
	// wrappers (coop-consult/coop-delegate) MAY be named, but only availability-gated —
	// they exist only when a consult/preset run mounts them, so an unconditional
	// recommendation would send agents at a missing binary.
	for _, bad := range []string{"coop fork", "coop fleet"} {
		if strings.Contains(string(agents), bad) {
			t.Errorf("AGENTS.md should not recommend host-side orchestration %q:\n%s", bad, agents)
		}
	}
	for _, wrapper := range []string{"coop-consult", "coop-delegate"} {
		if strings.Contains(string(agents), wrapper) && !strings.Contains(string(agents), "on PATH") {
			t.Errorf("AGENTS.md names %s without gating on its presence (on PATH):\n%s", wrapper, agents)
		}
	}

	// .gitignore ignores .agent/ state at any depth and tracks knowledge (rules/skills/presets and
	// the loop.yaml config) at any depth; only project.yaml is top-level.
	gi, _ := os.ReadFile(filepath.Join(repo, ".gitignore"))
	for _, want := range []string{"**/.agent/*", "!**/.agent/rules/", "!**/.agent/skills/", "!**/.agent/presets/", "!**/.agent/claude/", "!**/.agent/loop.yaml", "!.agent/project.yaml", "!.gemini/skills"} {
		if !strings.Contains(string(gi), want) {
			t.Errorf(".gitignore missing %q:\n%s", want, gi)
		}
	}

	// The scaffold writes one committed loop.yaml, a project.yaml, and an (empty) presets/ dir.
	for _, p := range []string{".agent/loop.yaml", ".agent/project.yaml", ".agent/presets"} {
		if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(p))); err != nil {
			t.Errorf("scaffold missing %s: %v", p, err)
		}
	}
}

func TestInitSkillsSource(t *testing.T) {
	t.Run("existing agent source remains canonical", func(t *testing.T) {
		repo := t.TempDir()
		marker := filepath.Join(repo, ".agent", "skills", "project", "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("project skill\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Init(repo, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(marker); err != nil || string(got) != "project skill\n" {
			t.Fatalf("existing .agent skill changed: %v\n%s", err, got)
		}
		for _, rel := range []string{".claude/skills", ".codex/skills", ".gemini/skills"} {
			if got, err := os.Readlink(filepath.Join(repo, rel)); err != nil || got != "../.agent/skills" {
				t.Errorf("%s target = %q (%v), want ../.agent/skills", rel, got, err)
			}
		}
	})

	t.Run("real claude source is adopted without pollution", func(t *testing.T) {
		repo := t.TempDir()
		marker := filepath.Join(repo, ".claude", "skills", "project", "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("tuned skill\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Init(repo, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(repo, ".agent", "skills")); !os.IsNotExist(err) {
			t.Errorf("init created a competing .agent/skills: %v", err)
		}
		if got, err := os.ReadFile(marker); err != nil || string(got) != "tuned skill\n" {
			t.Fatalf("existing Claude skill changed: %v\n%s", err, got)
		}
		if _, err := os.Stat(filepath.Join(repo, ".claude", "skills", "spec")); !os.IsNotExist(err) {
			t.Errorf("init seeded Coop templates into project-owned Claude skills: %v", err)
		}
		agents, err := os.ReadFile(filepath.Join(repo, "AGENTS.md"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(agents), "They live once in\n`.agent/skills/`") || !strings.Contains(string(agents), "existing real `.claude/skills/`") {
			t.Errorf("generated instructions describe the wrong shared skills source:\n%s", agents)
		}
		for _, rel := range []string{".codex/skills", ".gemini/skills"} {
			if got, err := os.Readlink(filepath.Join(repo, rel)); err != nil || got != "../.claude/skills" {
				t.Errorf("%s target = %q (%v), want ../.claude/skills", rel, got, err)
			}
		}
	})

	t.Run("valid links stay and dangling links are repaired", func(t *testing.T) {
		repo := t.TempDir()
		for _, rel := range []string{".claude/skills", ".project-skills", ".codex", ".gemini"} {
			if err := os.MkdirAll(filepath.Join(repo, rel), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink("../.project-skills", filepath.Join(repo, ".codex", "skills")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../missing-skills", filepath.Join(repo, ".gemini", "skills")); err != nil {
			t.Fatal(err)
		}
		if err := Init(repo, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.Readlink(filepath.Join(repo, ".codex", "skills")); got != "../.project-skills" {
			t.Errorf("valid project skills link was repointed to %q", got)
		}
		if got, _ := os.Readlink(filepath.Join(repo, ".gemini", "skills")); got != "../.claude/skills" {
			t.Errorf("dangling skills link target = %q, want ../.claude/skills", got)
		}
	})
}

func assertClaudeHookFallbacks(t *testing.T, data []byte) {
	t.Helper()
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	assertNoClaudeStopHook(t, data)
	for event, script := range map[string]string{"PreToolUse": "commit-gate.sh"} {
		groups := settings.Hooks[event]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 || groups[0].Hooks[0].Command == "" {
			t.Fatalf("%s fallback command missing from settings: %s", event, data)
		}
		command := groups[0].Hooks[0].Command
		for _, c := range []struct {
			name        string
			projectHook bool
			userHook    bool
			want        string
		}{
			{"project executable wins", true, true, "project"},
			{"user executable is fallback", false, true, "user"},
			{"missing hooks are a no-op", false, false, ""},
		} {
			t.Run(event+"/"+c.name, func(t *testing.T) {
				projectDir := t.TempDir()
				configDir := t.TempDir()
				writeHook := func(root, rel, output string) {
					path := filepath.Join(root, rel, script)
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '"+output+"'\n"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if c.projectHook {
					writeHook(projectDir, ".claude/hooks", "project")
				}
				if c.userHook {
					writeHook(configDir, "hooks", "user")
				}
				cmd := exec.Command("sh", "-c", command)
				cmd.Env = append(os.Environ(), "CLAUDE_PROJECT_DIR="+projectDir, "CLAUDE_CONFIG_DIR="+configDir)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("fallback command failed: %v\n%s", err, out)
				}
				if string(out) != c.want {
					t.Fatalf("fallback output = %q, want %q", out, c.want)
				}
			})
		}
	}
}

func assertNoClaudeStopHook(t *testing.T, data []byte) {
	t.Helper()
	var settings struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if _, ok := settings.Hooks["Stop"]; ok {
		t.Fatalf("project-global Stop hook should be absent from settings: %s", data)
	}
}

func assertProjectClaudeCommitGate(t *testing.T, data []byte) {
	t.Helper()
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	groups := settings.Hooks["PreToolUse"]
	if len(groups) != 1 || len(groups[0].Hooks) != 1 || groups[0].Hooks[0].Command != `$CLAUDE_PROJECT_DIR/.claude/hooks/commit-gate.sh` {
		t.Fatalf("project commit gate command missing or changed: %s", data)
	}
	projectDir := t.TempDir()
	hook := filepath.Join(projectDir, ".claude/hooks/commit-gate.sh")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf project-gate\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", groups[0].Hooks[0].Command)
	cmd.Env = append(os.Environ(), "CLAUDE_PROJECT_DIR="+projectDir)
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "project-gate" {
		t.Fatalf("project commit gate command failed: %v, output %q", err, out)
	}
}

func TestInitGitHooks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "system"))
	gitInit := func(dir string) {
		t.Helper()
		if out, err := exec.Command("git", "-C", dir, "init").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
	}
	captureInit := func(dir string) (string, error) {
		t.Helper()
		oldStderr := os.Stderr
		readLog, writeLog, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stderr = writeLog
		initErr := Init(dir, "", nil, []string{"claude", "codex", "gemini"})
		_ = writeLog.Close()
		os.Stderr = oldStderr
		logged, err := io.ReadAll(readLog)
		_ = readLog.Close()
		if err != nil {
			t.Fatal(err)
		}
		return string(logged), initErr
	}

	// A fresh repo gets core.hooksPath pointed at the tracked, executable hook.
	repo := t.TempDir()
	gitInit(repo)
	if err := Init(repo, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
		t.Fatal(err)
	}
	if got := gitConfigGet(repo, "core.hooksPath"); got != ".githooks" {
		t.Errorf("core.hooksPath = %q, want .githooks", got)
	}
	if fi, err := os.Stat(filepath.Join(repo, ".githooks/pre-commit")); err != nil {
		t.Fatalf("pre-commit hook missing: %v", err)
	} else if fi.Mode()&0o100 == 0 {
		t.Error("pre-commit hook is not executable")
	}
	if fi, err := os.Stat(filepath.Join(repo, ".githooks/prepare-commit-msg")); err != nil {
		t.Fatalf("prepare-commit-msg hook missing: %v", err)
	} else if fi.Mode()&0o100 == 0 {
		t.Error("prepare-commit-msg hook is not executable")
	}
	if fi, err := os.Stat(filepath.Join(repo, ".agent/claude/hooks/commit-gate.sh")); err != nil {
		t.Fatalf("shared Claude commit gate missing: %v", err)
	} else if fi.Mode()&0o100 == 0 {
		t.Error("shared Claude commit gate is not executable")
	}
	if logged, err := captureInit(repo); err != nil {
		t.Fatal(err)
	} else if strings.Contains(logged, "chain $HOME/.config/coop/git-hooks/prepare-commit-msg") {
		t.Errorf("stock prepare-commit-msg hook received custom-hook guidance:\n%s", logged)
	}

	// The repo-local hooksPath wins over Coop's box-global hooksPath. On a host the tracked shim is a
	// no-op; in a box it must reach the mounted hook and forward Git's message path.
	home := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "HOME="+home)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	latestMessage := func() string {
		t.Helper()
		out, err := exec.Command("git", "-C", repo, "log", "-1", "--format=%B").Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	for key, value := range map[string]string{"user.name": "Test", "user.email": "test@example.com"} {
		if err := gitConfigSet(repo, key, value); err != nil {
			t.Fatal(err)
		}
	}
	tracked := filepath.Join(repo, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "tracked.txt")
	runGit("commit", "-m", "host no-op")
	if msg := latestMessage(); strings.Contains(msg, "noreply@coop.dev") {
		t.Fatalf("host commit unexpectedly ran a box hook:\n%s", msg)
	}

	boxHook := filepath.Join(home, ".config", "coop", "git-hooks", "prepare-commit-msg")
	if err := os.MkdirAll(filepath.Dir(boxHook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(boxHook, []byte("#!/bin/sh\nprintf '\\nCo-authored-by: chained <noreply@coop.dev>\\n' >> \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracked, []byte("box\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "tracked.txt")
	runGit("commit", "-m", "test chain")
	if msg := latestMessage(); !strings.Contains(msg, "chained <noreply@coop.dev>") {
		t.Fatalf("repo-local prepare-commit-msg did not chain the box hook:\n%s", msg)
	}

	// A project-owned hook in the active scaffold directory is preserved with chaining guidance.
	repo2 := t.TempDir()
	gitInit(repo2)
	customPrepare := filepath.Join(repo2, ".githooks", "prepare-commit-msg")
	if err := os.MkdirAll(filepath.Dir(customPrepare), 0o755); err != nil {
		t.Fatal(err)
	}
	const customHook = "#!/bin/sh\n# project-owned\n"
	if err := os.WriteFile(customPrepare, []byte(customHook), 0o755); err != nil {
		t.Fatal(err)
	}
	logged, err := captureInit(repo2)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitConfigGet(repo2, "core.hooksPath"); got != ".githooks" {
		t.Errorf("core.hooksPath = %q, want .githooks", got)
	}
	if got, err := os.ReadFile(customPrepare); err != nil || string(got) != customHook {
		t.Errorf("custom prepare-commit-msg hook was clobbered: %v\n%s", err, got)
	}
	if want := "chain $HOME/.config/coop/git-hooks/prepare-commit-msg"; !strings.Contains(logged, want) {
		t.Errorf("active custom hook guidance missing %q:\n%s", want, logged)
	}

	// A custom hooksPath is left untouched and gets one message covering both tracked hooks.
	repo3 := t.TempDir()
	gitInit(repo3)
	if err := gitConfigSet(repo3, "core.hooksPath", ".my-hooks"); err != nil {
		t.Fatal(err)
	}
	customPrepare = filepath.Join(repo3, ".githooks", "prepare-commit-msg")
	if err := os.MkdirAll(filepath.Dir(customPrepare), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(customPrepare, []byte(customHook), 0o755); err != nil {
		t.Fatal(err)
	}
	logged, err = captureInit(repo3)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitConfigGet(repo3, "core.hooksPath"); got != ".my-hooks" {
		t.Errorf("custom core.hooksPath was clobbered: got %q, want .my-hooks", got)
	}
	if got, err := os.ReadFile(customPrepare); err != nil || string(got) != customHook {
		t.Errorf("inactive prepare-commit-msg hook was clobbered: %v\n%s", err, got)
	}
	if want := ".githooks/pre-commit and .githooks/prepare-commit-msg"; !strings.Contains(logged, want) {
		t.Errorf("custom hooksPath guidance missing %q:\n%s", want, logged)
	}
	if strings.Contains(logged, "chain $HOME/.config/coop/git-hooks/prepare-commit-msg") {
		t.Errorf("custom hooksPath received redundant inactive-hook guidance:\n%s", logged)
	}
}

func TestInitIdempotent(t *testing.T) {
	repo := t.TempDir()
	if err := Init(repo, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
		t.Fatal(err)
	}
	// Edit a file, then re-init: it must be kept, not overwritten.
	readme := filepath.Join(repo, ".agent/tasks/README.md")
	os.WriteFile(readme, []byte("MY EDITS"), 0o644)
	sharedSettings := filepath.Join(repo, ".agent/claude/settings.json")
	sharedGate := filepath.Join(repo, ".agent/claude/hooks/commit-gate.sh")
	os.WriteFile(sharedSettings, []byte("MY CLAUDE SETTINGS"), 0o644)
	os.WriteFile(sharedGate, []byte("#!/bin/sh\n# MY CLAUDE GATE\n"), 0o755)

	// Capture the re-run's log. An unchanged symlink must read as "kept existing", not the action
	// verb "linked" (which looks like a rewrite on every subsequent init); and a kept skill must
	// carry the same leading slash the added branch prints, so the wording can't flip run-to-run.
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	err := Init(repo, "", nil, []string{"claude", "codex", "gemini"})
	_ = w.Close()
	os.Stderr = old
	logged, _ := io.ReadAll(r)
	out := string(logged)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "linked CLAUDE.md") {
		t.Errorf("re-run reported an unchanged symlink as freshly linked, want 'kept existing':\n%s", out)
	}
	if !strings.Contains(out, "kept existing CLAUDE.md") {
		t.Errorf("re-run should report the unchanged CLAUDE.md symlink as kept existing:\n%s", out)
	}
	if !strings.Contains(out, "kept existing skill /") {
		t.Errorf("re-run should render a kept skill with the same leading slash as the added branch:\n%s", out)
	}

	if b, _ := os.ReadFile(readme); string(b) != "MY EDITS" {
		t.Error("re-init clobbered an edited .agent/tasks/README.md")
	}
	if b, _ := os.ReadFile(sharedSettings); string(b) != "MY CLAUDE SETTINGS" {
		t.Error("re-init clobbered edited shared Claude settings")
	}
	if b, _ := os.ReadFile(sharedGate); string(b) != "#!/bin/sh\n# MY CLAUDE GATE\n" {
		t.Error("re-init clobbered edited shared Claude commit gate")
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
	if err := Init(repo, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
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
	// --stack asdf with a .tool-versions → the asdf Dockerfile.agent (NOT compose: sibling
	// services are opt-in via `coop init`'s prompt / --services, so Init never adds db/redis).
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, ".tool-versions"), []byte("golang 1.26.4\n"), 0o644)
	if err := Init(repo, "asdf", nil, []string{"claude", "codex", "gemini"}); err != nil {
		t.Fatal(err)
	}
	df, err := os.ReadFile(filepath.Join(repo, "Dockerfile.agent"))
	if err != nil || !strings.Contains(string(df), "asdf install") {
		t.Errorf("asdf stack Dockerfile.agent missing or wrong:\n%s", df)
	}
	if _, err := os.Stat(filepath.Join(repo, ".agent", "compose.yml")); err == nil {
		t.Error("Init must not scaffold .agent/compose.yml — sibling services are opt-in")
	}

	// A removed per-language stack is now an error pointing at .tool-versions.
	if err := Init(t.TempDir(), "go", nil, nil); err == nil {
		t.Error("--stack go should error now that language stacks are gone")
	}

	// --stack asdf without a .tool-versions is an error (nothing to install from).
	if err := Init(t.TempDir(), "asdf", nil, nil); err == nil {
		t.Error("--stack asdf without a .tool-versions should error")
	}
}

func TestInitToolVersionsAsdf(t *testing.T) {
	// No --stack but a .tool-versions present → scaffold the asdf Dockerfile that
	// installs straight from it.
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, ".tool-versions"), []byte("erlang 29.0.1\nelixir 1.20.0-otp-29\ngolang 1.26.4\n"), 0o644)
	if err := Init(repo, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
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
	if err := Init(repo2, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo2, "Dockerfile.agent")); !os.IsNotExist(err) {
		t.Error("no stack + no .tool-versions should not scaffold a Dockerfile.agent")
	}

	// A removed language stack errors even when a .tool-versions is present —
	// the bad flag is surfaced rather than silently using .tool-versions.
	repo3 := t.TempDir()
	os.WriteFile(filepath.Join(repo3, ".tool-versions"), []byte("elixir 1.20.0-otp-29\n"), 0o644)
	if err := Init(repo3, "python", nil, nil); err == nil {
		t.Error("--stack python should error regardless of .tool-versions")
	}
}

// The workflow skills live in two hand-kept copies: the canonical .agent/skills/<name>/ (coop's
// own, at the repo root) and internal/scaffold/templates/skills/<name>/ (embedded, copied into a
// user repo by `coop init`). This guards them byte-identical in BOTH directions so a later edit to
// one can't silently miss the other. The one allowed asymmetry: .agent/skills/release/ is coop-only
// (it cuts a coop release via GoReleaser/install.sh and must never ship to a user repo), so it lives
// in the canonical tree but not the templates.
func TestSkillsTemplatesMatchCanonical(t *testing.T) {
	const canonicalOnly = "release" // coop's own release skill — never scaffolded into a user repo
	canonicalRoot := filepath.Join("..", "..", ".agent", "skills")

	// Every embedded template skill file exists and is byte-identical in the canonical tree.
	err := fs.WalkDir(templates, "templates/skills", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, "templates/skills/")
		want, err := templates.ReadFile(p)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(canonicalRoot, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("template skill %s is missing from .agent/skills — copy it there: %v", rel, err)
			return nil
		}
		if string(got) != string(want) {
			t.Errorf(".agent/skills/%s drifted from the embedded template — sync the two copies", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Every canonical skill file has an embedded template — except the coop-only skill.
	err = filepath.WalkDir(canonicalRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(canonicalRoot, p)
		rel = filepath.ToSlash(rel)
		if rel == canonicalOnly || strings.HasPrefix(rel, canonicalOnly+"/") {
			return nil // coop-only: intentionally absent from the templates
		}
		if _, err := templates.ReadFile("templates/skills/" + rel); err != nil {
			t.Errorf(".agent/skills/%s has no embedded template — add it to internal/scaffold/templates/skills/ (or, if coop-only, to the canonicalOnly exclusion)", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestInitAgentDirsGating(t *testing.T) {
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	// claude-only → .claude scaffolded, .codex/.gemini NOT; .agent + AGENTS.md always.
	repo := t.TempDir()
	if err := Init(repo, "", nil, []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(repo, ".claude", "settings.json")) {
		t.Error(".claude should be scaffolded for claude")
	}
	for _, d := range []string{".codex", ".gemini", "GEMINI.md"} {
		if exists(filepath.Join(repo, d)) {
			t.Errorf("%s should NOT be scaffolded for claude-only", d)
		}
	}
	if !exists(filepath.Join(repo, ".agent", "skills")) || !exists(filepath.Join(repo, "AGENTS.md")) {
		t.Error(".agent/ and AGENTS.md are always scaffolded")
	}
	// No agents → .agent/ only, no per-agent dirs at all.
	repo2 := t.TempDir()
	if err := Init(repo2, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".claude", ".codex", ".gemini", "CLAUDE.md", "GEMINI.md"} {
		if exists(filepath.Join(repo2, d)) {
			t.Errorf("%s should NOT be scaffolded with no agents", d)
		}
	}
	if !exists(filepath.Join(repo2, ".agent", "rules")) {
		t.Error(".agent/ is always scaffolded even with no agents")
	}
	for _, rel := range []string{".agent/claude/settings.json", ".agent/claude/hooks/commit-gate.sh"} {
		if !exists(filepath.Join(repo2, rel)) {
			t.Errorf("shared Claude fallback %s should be scaffolded with no per-agent dirs", rel)
		}
	}
	if exists(filepath.Join(repo2, ".agent/claude/hooks/stop-guard.sh")) {
		t.Error("shared Claude fallback should not scaffold the retired global Stop guard")
	}
}
