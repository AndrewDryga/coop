package scaffold

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func captureScaffoldStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	log, err := os.CreateTemp(t.TempDir(), "stderr-")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = log
	defer func() {
		os.Stderr = old
		_ = log.Close()
	}()
	runErr := fn()
	os.Stderr = old
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data), runErr
}

// The scaffolded asdf Dockerfile builds on Coop's box, so an init-generated project runs the clients
// Coop qualifies — and a filtered launch, which requires that derivation, can run it at all. It
// installs no client of its own, and the launcher directory it keeps first on PATH (a static embed
// cannot import it) is the one the images install.
func TestAsdfDockerfileInheritsTheQualifiedClients(t *testing.T) {
	got, err := asdfDockerfile(map[string]bool{"nodejs": true})
	if err != nil {
		t.Fatal(err)
	}
	body := dockerfileInstructions(got)
	if !strings.Contains(body, "ARG COOP_BASE_IMAGE=coop-box\nFROM ${COOP_BASE_IMAGE}\n") {
		t.Errorf("the asdf box does not build on Coop's box:\n%s", body)
	}
	for _, own := range []string{"npm install", "@latest", "nodesource", "install.sh", "FROM debian"} {
		if strings.Contains(body, own) {
			t.Errorf("the asdf box installs its own clients (%q):\n%s", own, body)
		}
	}
	if want := `PATH="` + agents.LauncherDir + `:/home/node/.asdf/shims:`; strings.Count(body, want) != 2 {
		t.Errorf("the asdf box does not keep %s first on PATH for the agent and login shells:\n%s", agents.LauncherDir, body)
	}
}

// The box image carries the system packages THIS repo's pinned tools need — and nothing else.
// Shipping erlang's build deps into a Terraform repo is dead weight; shipping none of python's
// into a checkov repo is a failed `coop build` (which is exactly how this was found).
func TestAsdfDockerfileFitsThePinnedToolchain(t *testing.T) {
	for _, tc := range []struct {
		name          string
		tools         []string
		want, notWant []string
	}{{
		name:    "terraform + a pip-backed plugin",
		tools:   []string{"terraform", "checkov"},
		want:    []string{"gnupg", "python3-pip"},
		notWant: []string{"autoconf", "libncurses-dev", "KERL_", "mix local.hex", "build-essential"},
	}, {
		name:    "erlang + elixir",
		tools:   []string{"erlang", "elixir"},
		want:    []string{"autoconf", "m4", "libncurses-dev", "KERL_BUILD_DOCS", "mix local.hex"},
		notWant: []string{"python3-pip", "gnupg"},
	}, {
		name:    "python builds from source",
		tools:   []string{"python"},
		want:    []string{"build-essential", "libffi-dev", "zlib1g-dev"},
		notWant: []string{"KERL_", "mix local.hex", "autoconf"},
	}, {
		name:  "a binary-download toolchain needs nothing extra",
		tools: []string{"golang", "kubectl"},
		// The universal base only — no compiler, no interpreter, no seed step.
		notWant: []string{"build-essential", "python3-pip", "KERL_", "mix local.hex", "gnupg"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			set := map[string]bool{}
			for _, x := range tc.tools {
				set[x] = true
			}
			got, err := asdfDockerfile(set)
			if err != nil {
				t.Fatal(err)
			}
			// Assert on the INSTRUCTIONS, never the prose: the template's comment names
			// python3-pip as the example a reader should add, which would match a naive
			// whole-file search and make "carries nothing extra" pass by accident.
			body := dockerfileInstructions(got)
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Errorf("%v box is missing %q:\n%s", tc.tools, w, body)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(body, w) {
					t.Errorf("%v box carries %q it never uses:\n%s", tc.tools, w, body)
				}
			}
			if strings.Contains(got, "@SYSTEM_PACKAGES@") || strings.Contains(got, "@TOOLCHAIN_ENV@") || strings.Contains(got, "@TOOLCHAIN_SEED@") {
				t.Errorf("unsubstituted placeholder left in the rendered Dockerfile:\n%s", got)
			}
			// An empty slot must not leave a line that is nothing but a continuation.
			for i, line := range strings.Split(got, "\n") {
				if strings.TrimSpace(line) == "\\" || strings.TrimSpace(line) == "&& \\" {
					t.Errorf("empty substitution left a dangling continuation at line %d:\n%s", i+1, got)
				}
			}
		})
	}
}

// dockerfileInstructions strips comment lines so an assertion about what the image INSTALLS
// can't be satisfied (or defeated) by the template's explanatory prose.
func dockerfileInstructions(dockerfile string) string {
	var kept []string
	for _, line := range strings.Split(dockerfile, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// The profile drop-in belongs after the expensive toolchain layer: login shells keep the shims,
// while editing this small layer does not rebuild every version pinned in .tool-versions.
func TestAsdfDockerfileKeepsToolchainsOnLoginPath(t *testing.T) {
	content, err := asdfDockerfile(map[string]bool{"erlang": true, "elixir": true})
	if err != nil {
		t.Fatal(err)
	}
	const dropIn = `RUN printf 'export PATH="/opt/coop/bin:/home/node/.asdf/shims:$PATH"\n' > /etc/profile.d/zz-asdf.sh`
	installAt := strings.Index(content, ` && MAKEFLAGS="-j$(nproc)" asdf install`)
	rootAt := strings.LastIndex(content, "\nUSER root\n")
	dropInAt := strings.Index(content, dropIn)
	nodeAt := strings.LastIndex(content, "\nUSER node\n")
	lastUserAt := strings.LastIndex(content, "\nUSER ")
	if installAt < 0 || rootAt < installAt || dropInAt < rootAt || nodeAt < dropInAt || nodeAt != lastUserAt {
		t.Errorf("asdf login-path drop-in must follow the install layer inside a root/node bracket:\n%s", content)
	}
}

func testNativeCommitGate(name string, langs []string) string {
	ag, _ := agents.Get(name)
	gate := ag.Scaffold().CommitGate
	return gate.Prefix + gateBody(langs, gate.FailureCode) + gate.Suffix
}

// TestGeneratedHooksShellcheckClean renders every commit gate coop writes into a user's repo —
// the .githooks/pre-commit and .claude commit gate, for all detected langs and the neutral
// fallback — and asserts shellcheck finds nothing. CI only shellchecks install.sh, so without
// this a generated hook could ship with warnings. Skipped when shellcheck isn't installed.
func TestGeneratedHooksShellcheckClean(t *testing.T) {
	sc := shellcheckPath(t)
	hooks := map[string]string{
		"pre-commit (all langs)":  preCommitHook(GateLangs),
		"claude gate (all langs)": testNativeCommitGate("claude", GateLangs),
		"pre-commit (neutral)":    preCommitHook(nil),
		"claude gate (neutral)":   testNativeCommitGate("claude", nil),
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
// block — that would drop the !kb/!skills un-ignore and leave tracked dirs ignored. The block is
// monorepo-aware: **/.agent/* (any depth) with !.agent/project.yaml.
func TestUpdateGitignoreBroadPrefixDoesNotSkipBlock(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("node_modules/\n.agent/*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (&scaffolder{repo: repo}).updateGitignore([]string{"gemini"}, writeAndSync); err != nil {
		t.Fatal(err)
	}
	gi, _ := os.ReadFile(filepath.Join(repo, ".gitignore"))
	// Knowledge (kb, including rules, plus skills) is un-ignored at any depth so a member may carry
	// its own; only project.yaml is top-level.
	for _, want := range []string{"**/.agent/*\n", "!**/.agent/kb/", "!**/.agent/skills/", "!**/.agent/claude/", "!**/.agent/Dockerfile", "!**/.agent/Dockerfile.dockerignore", "!.agent/project.yaml"} {
		if !strings.Contains(string(gi), want) {
			t.Errorf("coop's block missing %q after a broad .agent/*.log line:\n%s", want, gi)
		}
	}
	// Idempotent: a second run doesn't duplicate the block.
	_ = (&scaffolder{repo: repo}).updateGitignore([]string{"gemini"}, writeAndSync)
	gi2, _ := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if n := strings.Count(string(gi2), "\n**/.agent/*\n"); n != 1 {
		t.Errorf("coop block written %d times, want 1:\n%s", n, gi2)
	}
}

func TestUpdateGitignoreCompletesMinimalCurrentBlock(t *testing.T) {
	repo := t.TempDir()
	partialBlock := "# coop working state (commit knowledge, ignore state)\n" +
		"**/.agent/*\n"
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(partialBlock), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &scaffolder{repo: repo}
	if err := s.updateGitignore([]string{"gemini"}, writeAndSync); err != nil {
		t.Fatal(err)
	}
	if err := s.updateGitignore([]string{"gemini"}, writeAndSync); err != nil {
		t.Fatal(err)
	}
	gi, err := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	ordered := []string{
		"**/.agent/*", "!**/.agent/kb/", "!**/.agent/skills/", "!**/.agent/presets/",
		"!**/.agent/claude/", "!**/.agent/loop.yaml", "!**/.agent/compose.yml",
		"!**/.agent/Dockerfile", "!**/.agent/Dockerfile.dockerignore", "!.agent/project.yaml", "!**/.agent/tasks/",
		"**/.agent/tasks/*", "!**/.agent/tasks/README.md",
	}
	last := -1
	for _, line := range ordered {
		if n := strings.Count(string(gi), line+"\n"); n != 1 {
			t.Fatalf("current rule %q appears %d times, want 1:\n%s", line, n, gi)
		}
		at := strings.Index(string(gi), line+"\n")
		if at <= last {
			t.Fatalf("current rule %q is out of load-bearing order:\n%s", line, gi)
		}
		last = at
	}
}

// The .gemini rules exist to rescue that dir's skills symlink from a global ignore — a repo that
// doesn't keep a .gemini/ shouldn't be given rules for one.
func TestUpdateGitignoreSkipsGeminiRulesWhenUnused(t *testing.T) {
	repo := t.TempDir()
	if err := (&scaffolder{repo: repo}).updateGitignore(nil, writeAndSync); err != nil {
		t.Fatal(err)
	}
	gi, _ := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if strings.Contains(string(gi), ".gemini") {
		t.Errorf("gemini rules written for a repo with no .gemini/:\n%s", gi)
	}
	// …and adding gemini later still gets them, exactly once.
	if err := (&scaffolder{repo: repo}).updateGitignore([]string{"gemini"}, writeAndSync); err != nil {
		t.Fatal(err)
	}
	gi, _ = os.ReadFile(filepath.Join(repo, ".gitignore"))
	if n := strings.Count(string(gi), "!.gemini/skills"); n != 1 {
		t.Errorf("gemini rules appear %d times after enabling gemini, want 1:\n%s", n, gi)
	}
}

// An empty leftover skill directory used to make init report "kept existing skill" forever while
// the skill stayed empty — and every .../skills symlink pointed at nothing.
func TestInitRestoresEmptySkillDir(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent", "skills", "spec"), 0o755); err != nil {
		t.Fatal(err)
	}
	logged, err := captureScaffoldStderr(t, func() error {
		_, err := Init(repo, "", nil, []string{"claude"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(repo, ".agent/skills/spec/SKILL.md")); err != nil || fi.Size() == 0 {
		t.Fatalf("an empty skill dir was left empty: %v", err)
	}
	if logged != "" {
		t.Errorf("restoring a skill is routine scaffold work and says nothing:\n%s", logged)
	}
	// A skill with its SKILL.md is still left alone, edits and all.
	custom := filepath.Join(repo, ".agent/skills/work/SKILL.md")
	os.WriteFile(custom, []byte("MY SKILL"), 0o644)
	if _, err := Init(repo, "", nil, []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(custom); string(b) != "MY SKILL" {
		t.Error("re-init clobbered a customized skill")
	}
}

// A member gets only its own task queue — never the full scaffold, root project.yaml, or retired
// single-file backlog.
func TestInitSubproject(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "member")
	logged, err := captureScaffoldStderr(t, func() error { return InitSubproject(root, dir) })
	if err != nil {
		t.Fatal(err)
	}
	if logged != "" {
		t.Errorf("a member scaffold prints no per-file log, got %q", logged)
	}
	for _, rel := range []string{".agent/tasks/00_todo", ".agent/tasks/99_done", ".agent/tasks/README.md"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("member missing %s: %v", rel, err)
		}
	}
	for _, rel := range []string{"AGENTS.md", ".claude", ".agent/kb/rules", ".agent/skills", "CLAUDE.md", ".agent/project.yaml", ".agent/BACKLOG.md"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			t.Errorf("member should NOT have %s (top-level only / retired)", rel)
		}
	}
}

// TestDockerfileTemplatesTrustAnyWorktree guards the real-path-mount contract:
// coop mounts the repo at its real host path and sets the workdir itself, so every
// stack image must trust any worktree (safe.directory '*'), not a fixed /workspace
// (the stale pre-2.0 path, which leaves git with "dubious ownership" on runtimes
// that preserve host uid). A template built on Coop's box inherits that and the
// node-owned ~/.cache from the base, which internal/box's image tests hold.
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
		case strings.Contains(s, "\nFROM ${COOP_BASE_IMAGE}\n"):
			return nil
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
	logged, err := captureScaffoldStderr(t, func() error {
		_, err := Init(repo, "", nil, []string{"claude", "codex", "gemini"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// The caller owns the result. Scaffolding says nothing at all — the per-file/symlink/skill/
	// gitignore ledger it used to print is exactly the wall `coop init` no longer shows.
	if logged != "" {
		t.Errorf("scaffold printed a progress ledger:\n%s", logged)
	}

	// Core files exist with content.
	for _, rel := range []string{
		"AGENTS.md", ".agent/tasks/README.md",
		".agent/skills/sweep/queue-guard.sh",
		".claude/settings.json", ".claude/hooks/commit-gate.sh",
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

	// Subagents are the repo's own business: a preset generates its coop-<role> in the box, and a
	// repo with its own roles doesn't want a competing starter set committed alongside them.
	if _, err := os.Stat(filepath.Join(repo, ".claude/agents")); err == nil {
		t.Error(".claude/agents should not be scaffolded (presets generate coop-<role> in the box)")
	}

	// Exactly ONE copy of the Claude commit gate + settings: the project .claude/ artifact always
	// wins over the .agent/claude/ fallback, so scaffolding both commits a file nothing reads.
	for _, rel := range []string{".agent/claude/settings.json", ".agent/claude/hooks/commit-gate.sh"} {
		if _, err := os.Stat(filepath.Join(repo, rel)); err == nil {
			t.Errorf("%s duplicates the project .claude/ artifact that shadows it", rel)
		}
	}

	// The four task state directories exist (the folder-mode queue, with the lifecycle-sort prefix).
	for _, st := range []string{"00_todo", "10_in_progress", "50_blocked", "99_done"} {
		if fi, err := os.Stat(filepath.Join(repo, ".agent/tasks", st)); err != nil || !fi.IsDir() {
			t.Errorf(".agent/tasks/%s should be a directory: %v", st, err)
		}
	}

	// Hooks are executable; the old project-global Stop guard is not scaffolded.
	for _, rel := range []string{".githooks/pre-commit", ".claude/hooks/commit-gate.sh"} {
		if fi, _ := os.Stat(filepath.Join(repo, rel)); fi == nil || fi.Mode()&0o100 == 0 {
			t.Errorf("%s is missing or not executable", rel)
		}
	}
	for _, rel := range []string{".agent/claude/hooks/stop-guard.sh", ".claude/hooks/stop-guard.sh"} {
		if _, err := os.Stat(filepath.Join(repo, rel)); !os.IsNotExist(err) {
			t.Errorf("retired global hook %s should not be scaffolded", rel)
		}
	}
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
	for _, want := range []string{"/goal", "/batch", "subagents", "native", "do not invent slash commands", "Keep writes serialized", "static and bounded"} {
		if !strings.Contains(string(agents), want) {
			t.Errorf("AGENTS.md missing agent-stack guidance %q:\n%s", want, agents)
		}
	}
	for _, want := range []string{"Knowledge (`kb/`, including", "`kb/rules/`"} {
		if !strings.Contains(string(agents), want) {
			t.Errorf("AGENTS.md missing current knowledge-tree guidance %q:\n%s", want, agents)
		}
	}
	for _, retired := range []string{"Knowledge (`rules/`", "\n- `rules/`"} {
		if strings.Contains(string(agents), retired) {
			t.Errorf("AGENTS.md still advertises retired sibling rules path %q:\n%s", retired, agents)
		}
	}
	// Host-side commands stay out (an agent in the box can't run them). The in-box
	// wrappers (coop-consult/coop-delegate) MAY be named, but only availability-gated —
	// they exist only when a consult/preset run mounts them, so an unconditional
	// recommendation would send agents at a missing binary.
	for _, bad := range []string{"coop fork"} {
		if strings.Contains(string(agents), bad) {
			t.Errorf("AGENTS.md should not recommend host-side orchestration %q:\n%s", bad, agents)
		}
	}
	for _, wrapper := range []string{"coop-consult", "coop-delegate"} {
		if strings.Contains(string(agents), wrapper) && !strings.Contains(string(agents), "on PATH") {
			t.Errorf("AGENTS.md names %s without gating on its presence (on PATH):\n%s", wrapper, agents)
		}
	}

	// .gitignore ignores .agent/ state at any depth and tracks knowledge (kb/skills/presets and
	// the loop.yaml config) at any depth; only project.yaml is top-level.
	gi, _ := os.ReadFile(filepath.Join(repo, ".gitignore"))
	for _, want := range []string{"**/.agent/*", "!**/.agent/kb/", "!**/.agent/skills/", "!**/.agent/presets/", "!**/.agent/claude/", "!**/.agent/loop.yaml", "!.agent/project.yaml", "!.gemini/skills"} {
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
		if _, err := Init(repo, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
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
		if _, err := Init(repo, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
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
		if _, err := Init(repo, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
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
	// The hook exceptions travel back to the caller as notices rather than printing here, so the
	// assertions below read them instead of stderr.
	captureInit := func(dir string) (string, error) {
		t.Helper()
		notices, err := Init(dir, "", nil, []string{"claude", "codex", "gemini"})
		var b strings.Builder
		for _, n := range notices {
			fmt.Fprintf(&b, "%s\n%s\n%s\n", n.Headline, n.Reason, n.Action)
		}
		return b.String(), err
	}

	// A fresh repo gets core.hooksPath pointed at the tracked, executable hook.
	repo := t.TempDir()
	gitInit(repo)
	if _, err := Init(repo, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
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
	// With a project .claude/ scaffolded, THAT copy is the Claude commit gate — the .agent/claude/
	// fallback would only be a byte-identical file the box never reads.
	if fi, err := os.Stat(filepath.Join(repo, ".claude/hooks/commit-gate.sh")); err != nil {
		t.Fatalf("project Claude commit gate missing: %v", err)
	} else if fi.Mode()&0o100 == 0 {
		t.Error("project Claude commit gate is not executable")
	}
	if logged, err := captureInit(repo); err != nil {
		t.Fatal(err)
	} else if strings.Contains(logged, "Chain $HOME/.coop-git-hooks/prepare-commit-msg from it") {
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

	boxHook := filepath.Join(home, ".coop-git-hooks", "prepare-commit-msg")
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

	// A symlink is project-owned. Init must neither rewrite nor chmod a shared target outside the
	// repository.
	symlinkRepo := t.TempDir()
	gitInit(symlinkRepo)
	symlinkPrepare := filepath.Join(symlinkRepo, ".githooks", "prepare-commit-msg")
	if err := os.MkdirAll(filepath.Dir(symlinkPrepare), 0o755); err != nil {
		t.Fatal(err)
	}
	sharedHook := filepath.Join(t.TempDir(), "shared-prepare-commit-msg")
	const sharedHookBody = "#!/bin/sh\n# project-owned shared hook\n"
	if err := os.WriteFile(sharedHook, []byte(sharedHookBody), 0o644); err != nil {
		t.Fatal(err)
	}
	// WriteFile applies the process umask; establish the mode Init must preserve.
	if err := os.Chmod(sharedHook, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sharedHook, symlinkPrepare); err != nil {
		t.Fatal(err)
	}
	logged, err := captureInit(symlinkRepo)
	if err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(symlinkPrepare); err != nil || target != sharedHook {
		t.Fatalf("project-owned prepare hook symlink changed: target %q, err %v", target, err)
	}
	if got, err := os.ReadFile(sharedHook); err != nil || string(got) != sharedHookBody {
		t.Fatalf("project-owned prepare hook target was rewritten: %v\n%s", err, got)
	}
	if info, err := os.Stat(sharedHook); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("project-owned prepare hook target mode changed: %v, mode %v", err, info)
	}
	if want := "Chain $HOME/.coop-git-hooks/prepare-commit-msg from it"; !strings.Contains(logged, want) {
		t.Errorf("project-owned symlink guidance missing %q:\n%s", want, logged)
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
	logged, err = captureInit(repo2)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitConfigGet(repo2, "core.hooksPath"); got != ".githooks" {
		t.Errorf("core.hooksPath = %q, want .githooks", got)
	}
	if got, err := os.ReadFile(customPrepare); err != nil || string(got) != customHook {
		t.Errorf("custom prepare-commit-msg hook was clobbered: %v\n%s", err, got)
	}
	if want := "Chain $HOME/.coop-git-hooks/prepare-commit-msg from it"; !strings.Contains(logged, want) {
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
	if want := "Add or chain .githooks/pre-commit and .githooks/prepare-commit-msg there."; !strings.Contains(logged, want) {
		t.Errorf("custom hooksPath guidance missing %q:\n%s", want, logged)
	}
	if strings.Contains(logged, "Chain $HOME/.coop-git-hooks/prepare-commit-msg from it") {
		t.Errorf("custom hooksPath received redundant inactive-hook guidance:\n%s", logged)
	}
}

func TestInitIdempotent(t *testing.T) {
	repo := t.TempDir()
	if _, err := Init(repo, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
		t.Fatal(err)
	}
	// Edit a file, then re-init: it must be kept, not overwritten.
	readme := filepath.Join(repo, ".agent/tasks/README.md")
	os.WriteFile(readme, []byte("MY EDITS"), 0o644)
	claudeSettings := filepath.Join(repo, ".claude/settings.json")
	claudeGate := filepath.Join(repo, ".claude/hooks/commit-gate.sh")
	os.WriteFile(claudeSettings, []byte("MY CLAUDE SETTINGS"), 0o644)
	os.WriteFile(claudeGate, []byte("#!/bin/sh\n# MY CLAUDE GATE\n"), 0o755)

	// A re-run that changes nothing says nothing: not the action verbs (an unchanged symlink read
	// as "linked", a present file as "wrote") and not a "kept N" tally either, which is still a
	// line whose whole content is that nothing happened.
	out, err := captureScaffoldStderr(t, func() error {
		_, err := Init(repo, "", nil, []string{"claude", "codex", "gemini"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("a re-run that changed nothing printed:\n%s", out)
	}

	if b, _ := os.ReadFile(readme); string(b) != "MY EDITS" {
		t.Error("re-init clobbered an edited .agent/tasks/README.md")
	}
	if b, _ := os.ReadFile(claudeSettings); string(b) != "MY CLAUDE SETTINGS" {
		t.Error("re-init clobbered edited project Claude settings")
	}
	if b, _ := os.ReadFile(claudeGate); string(b) != "#!/bin/sh\n# MY CLAUDE GATE\n" {
		t.Error("re-init clobbered edited project Claude commit gate")
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
	if _, err := Init(repo, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(real); err == nil {
		t.Error("real CLAUDE.md was replaced with a symlink")
	}
	if b, _ := os.ReadFile(real); string(b) != "# my project rules" {
		t.Error("real CLAUDE.md content was changed")
	}
}

// A box can fill the disk to fail a scaffold write, then swap the file's parent for a link to a
// folder .coopignore hides from it. The swapped path now names the hidden file, so a cleanup that
// walks the path again would check or remove that file instead of the partial one.
func TestWriteNewRepoFileCleanupIgnoresSwappedParent(t *testing.T) {
	repo := t.TempDir()
	hidden := filepath.Join(repo, "private", "README.md")
	if err := os.MkdirAll(filepath.Dir(hidden), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hidden, []byte("hidden notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(repo, ".agent", "tasks")
	moved := filepath.Join(repo, ".agent", "moved")
	created, err := writeNewRepoFile(repo, filepath.Join(parent, "README.md"), []byte("complete"), 0o644, func(file *os.File, _ []byte) error {
		if _, err := file.Write([]byte("par")); err != nil {
			return err
		}
		if err := os.Rename(parent, moved); err != nil {
			return err
		}
		if err := os.Symlink(filepath.Join("..", "private"), parent); err != nil {
			return err
		}
		return syscall.ENOSPC
	})
	if created || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("writeNewRepoFile = (%v, %v), want the full-disk failure", created, err)
	}
	if got, err := os.ReadFile(hidden); err != nil || string(got) != "hidden notes\n" {
		t.Fatalf("cleanup reached the hidden file through the swapped parent: %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(moved, "README.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial file was not removed from the folder it was written in: %v", err)
	}
}

// A box can swap a scaffold file's parent for a FIFO between init creating the folder and opening
// it. Opening a FIFO waits for a writer, so the open must refuse it instead of freezing coop init.
// The swap races the open: a child process repeats the write against a swapper.
func TestWriteNewRepoFileFailsFastOnAFIFOParent(t *testing.T) {
	if repo := os.Getenv("COOP_SCAFFOLD_FIFO_REPO"); repo != "" {
		parent := filepath.Join(repo, ".agent", "tasks")
		stop := swapInFIFOs(parent)
		defer stop()
		dest := filepath.Join(parent, "README.md")
		write := func(file *os.File, data []byte) error {
			_, err := file.Write(data)
			return err
		}
		for range 1000 {
			if created, _ := writeNewRepoFile(repo, dest, []byte("x"), 0o644, write); created {
				_ = os.Remove(dest)
			}
		}
		return
	}
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	runFIFOChild(t, repo, "writes racing a FIFO parent")
}

// The in-place edits (init --services, subproject registration) open their file's folder one real
// folder at a time. A box can leave a FIFO where that folder should be, or swap one in after the
// edit looked at the folder and before it opened it. Opening a FIFO waits for a writer, so each
// edit must refuse it at once instead of freezing coop init, and name the folder rather than the
// "<dir>/." the open uses or the system call that made it. A FIFO that was there all along is not
// a folder; nothing changed while coop opened it.
func TestInPlaceEditsFailFastOnAFIFOFolder(t *testing.T) {
	if repo := os.Getenv("COOP_SCAFFOLD_FIFO_REPO"); repo != "" {
		agent := filepath.Join(repo, ".agent")
		edit := func() []error {
			_, registerErr := RegisterSubprojects(repo, []string{"infra"})
			added, composeErr := AddComposeServices(repo, ".agent/compose.yml", []string{"postgres"})
			if added.Created {
				_ = os.Remove(filepath.Join(agent, "compose.yml"))
			}
			return []error{registerErr, composeErr}
		}
		t.Log("a FIFO already at .agent")
		if err := syscall.Mkfifo(agent, 0o600); err != nil {
			t.Fatal(err)
		}
		for i, err := range edit() {
			want := "open " + []string{".agent/project.yaml", ".agent/compose.yml"}[i] + ": .agent is not a folder"
			if err == nil || err.Error() != want {
				t.Errorf("edit with a FIFO at .agent = %v, want %q", err, want)
			}
		}
		root, err := os.OpenRoot(repo)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		dir, err := openDir(root, ".agent")
		if err == nil {
			_ = dir.Close()
		}
		if !errors.Is(err, syscall.ENOTDIR) || err.Error() != "open .agent: not a directory" {
			t.Errorf("opening a FIFO as a folder = %v, want ENOTDIR naming .agent", err)
		}
		t.Log("a FIFO swapped in while the edits open .agent")
		if err := os.Remove(agent); err != nil {
			t.Fatal(err)
		}
		stop := swapInFIFOs(agent)
		defer stop()
		for range 1000 {
			for _, err := range edit() {
				if err != nil && strings.Contains(err.Error(), "/./") {
					t.Fatalf("edit error names the folder handle: %v", err)
				}
			}
		}
		return
	}
	runFIFOChild(t, t.TempDir(), "edits with a FIFO for their folder")
}

// coop init reads .tool-versions and compares the prepare-commit-msg hook with its own, and
// `coop help` lists the services in the Compose file. A box can leave a FIFO, a link to a device or
// a link to a file outside the project at any of them. Opening a FIFO waits for a writer, a device
// can be read without end, and an outside file is not the project's, so each read refuses all three
// at once and names the file. A hook coop cannot read this way is not its own, so init keeps it;
// installGitHooks reads only an entry Lstat found to be a regular file, and the read here stands
// for one swapped after that check.
func TestReadsRefuseAFIFODeviceOrLinkOutAtOnce(t *testing.T) {
	if repo := os.Getenv("COOP_SCAFFOLD_FIFO_REPO"); repo != "" {
		// What each read would accept, so following a link to a copy of it would get through.
		accepted := map[string]string{
			".tool-versions":               "golang 1.26.4\n",
			".agent/compose.yml":           "services:\n  db:\n    image: postgres\n",
			".githooks/prepare-commit-msg": prepareCommitMsgChainHook,
		}
		for _, kind := range []string{"a FIFO", "a link out to a device", "a link out to a file"} {
			t.Run(kind, func(t *testing.T) {
				dir := filepath.Join(repo, strings.ReplaceAll(kind, " ", "-"))
				for rel, body := range accepted {
					path := filepath.Join(dir, filepath.FromSlash(rel))
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					var err error
					switch kind {
					case "a FIFO":
						err = syscall.Mkfifo(path, 0o600)
					case "a link out to a device":
						err = os.Symlink(os.DevNull, path)
					default: // what the read would accept, but outside dir, the project here
						outside := filepath.Join(repo, strings.ReplaceAll(rel, "/", "-"))
						if err = os.WriteFile(outside, []byte(body), 0o755); err == nil {
							err = os.Symlink(outside, path)
						}
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				refused := func(err error, file string) {
					t.Helper()
					want := file + " must be a regular file"
					if kind != "a FIFO" {
						want = file + " is a link to a file outside the project"
					}
					if !errors.Is(err, ErrNotRegular) || err.Error() != want {
						t.Errorf("reading %s = %v, want %q", file, err, want)
					}
				}
				_, err := toolVersions(dir)
				refused(err, ".tool-versions")
				refused(CheckStack(dir, ""), ".tool-versions")
				if got := DetectStacks(dir); got != nil {
					t.Errorf("DetectStacks = %v, want no stack from a .tool-versions coop cannot read", got)
				}
				_, err = ComposeServiceNames(dir, ".agent/compose.yml")
				refused(err, ".agent/compose.yml")
				if isStockPrepareHook(dir) {
					t.Error("a prepare-commit-msg hook coop cannot read counts as its own")
				}
			})
		}
		return
	}
	runFIFOChild(t, t.TempDir(), "reads of a FIFO, a device or a link out of the project")
}

// A project may keep its .tool-versions or Compose file as a link to a file inside it, as monorepos
// often do. A read that only reads follows such a link, as `coop up` does for the Compose file.
func TestReadsFollowALinkThatStaysInTheProject(t *testing.T) {
	repo := t.TempDir()
	for rel, body := range map[string]string{
		"shared/tool-versions": "golang 1.26.4\n",
		"shared/compose.yml":   "services:\n  db:\n    image: postgres\n",
	} {
		path := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("shared/tool-versions", filepath.Join(repo, ".tool-versions")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../shared/compose.yml", filepath.Join(repo, ".agent", "compose.yml")); err != nil {
		t.Fatal(err)
	}
	if tools, err := toolVersions(repo); err != nil || !tools["golang"] {
		t.Errorf("toolVersions through an in-project link = %v, %v; want golang", tools, err)
	}
	if names, err := ComposeServiceNames(repo, ".agent/compose.yml"); err != nil || !slices.Equal(names, []string{"db"}) {
		t.Errorf("ComposeServiceNames through an in-project link = %v, %v; want [db]", names, err)
	}
}

// A box can leave a sparse file of any size where a file coop only reads belongs. The read stops
// past 1 MiB instead of filling the host's memory, and says why.
func TestReadsRefuseAFileOverOneMiB(t *testing.T) {
	repo := t.TempDir()
	for _, rel := range []string{".tool-versions", ".agent/compose.yml"} {
		path := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, maxReadBytes+1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := toolVersions(repo); err == nil || err.Error() != ".tool-versions is larger than 1 MiB" {
		t.Errorf("toolVersions = %v, want the 1 MiB refusal", err)
	}
	if _, err := ComposeServiceNames(repo, ".agent/compose.yml"); err == nil || err.Error() != ".agent/compose.yml is larger than 1 MiB" {
		t.Errorf("ComposeServiceNames = %v, want the 1 MiB refusal", err)
	}
}

// The walk opens each folder from its parent, which has no path limit of its own, so a box could
// nest folders without end and make it hold a folder open and a longer path for every level. It
// stops descending past maxWalkPath; a member that deep is one no command could open anyway.
func TestDetectSubprojectsStopsAtTheLongestPath(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "api", ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	// One level past the bound, each a member, built a level at a time from its parent as a box could:
	// the whole path is longer than macOS lets one system call take.
	name := strings.Repeat("d", 200)
	levels := maxWalkPath/(len(name)+1) + 1
	dir, err := os.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	for range levels {
		if err := dir.Mkdir(name, 0o755); err != nil {
			t.Fatal(err)
		}
		child, err := dir.OpenRoot(name)
		_ = dir.Close()
		if err != nil {
			t.Fatal(err)
		}
		dir = child
		if err := dir.Mkdir(".agent", 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = dir.Close()
	subs, err := DetectSubprojects(repo)
	if err != nil {
		t.Fatal(err)
	}
	// api, and every level whose path inside the repository is shorter than the bound.
	if want := levels; !slices.Contains(subs, "api") || len(subs) != want {
		t.Errorf("DetectSubprojects found %d members, want %d: api and all but the deepest level", len(subs), want)
	}
	for _, sub := range subs {
		if len(sub) >= maxWalkPath {
			t.Errorf("the walk registered a member %d bytes deep, past its %d-byte bound", len(sub), maxWalkPath)
		}
	}
}

// The subproject walk lists a folder, then opens each folder in it. A box can swap one for a FIFO or
// a link in between. Opening a FIFO waits for a writer and a link can lead out of the repository, so
// the walk must stop at once and name the folder, instead of freezing coop init or registering task
// queues from another project.
func TestDetectSubprojectsFailsFastOnASwappedFolder(t *testing.T) {
	if repo := os.Getenv("COOP_SCAFFOLD_FIFO_REPO"); repo != "" {
		previous := afterListed
		t.Cleanup(func() { afterListed = previous })
		outside := t.TempDir()
		if err := os.MkdirAll(filepath.Join(outside, "api", ".agent"), 0o755); err != nil {
			t.Fatal(err)
		}
		infra := filepath.Join(repo, "infra")
		for _, swap := range []string{"a FIFO", "a link"} {
			t.Run(swap, func(t *testing.T) {
				if err := os.Mkdir(infra, 0o755); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(infra) })
				afterListed = func(dirRel string) {
					if dirRel != "" {
						return
					}
					err := os.Remove(infra)
					if err == nil && swap == "a FIFO" {
						err = syscall.Mkfifo(infra, 0o600)
					} else if err == nil {
						err = os.Symlink(outside, infra)
					}
					if err != nil {
						t.Error(err)
					}
				}
				subs, err := DetectSubprojects(repo)
				if want := "the folder infra changed while coop was opening it"; err == nil || err.Error() != want {
					t.Fatalf("walk with infra swapped for %s = %v, %v; want %q", swap, subs, err, want)
				}
			})
		}
		return
	}
	runFIFOChild(t, t.TempDir(), "walks with a folder swapped after its listing")
}

// swapInFIFOs keeps replacing the empty folder dir with a FIFO and back until the returned stop is
// called.
func swapInFIFOs(dir string) (stop func()) {
	var stopped atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stopped.Load() {
			_ = os.Remove(dir)
			_ = syscall.Mkfifo(dir, 0o600)
			_ = os.Remove(dir)
			_ = os.Mkdir(dir, 0o755)
		}
	}()
	return func() {
		stopped.Store(true)
		<-done
	}
}

// runFIFOChild runs the calling test again in a child process with COOP_SCAFFOLD_FIFO_REPO set to
// repo, and fails unless the child passes within wait.Deadline. An open waiting on a FIFO cannot be
// cancelled in-process, so a hang shows up as the child missing the deadline.
func runFIFOChild(t *testing.T, repo, what string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), "COOP_SCAFFOLD_FIFO_REPO="+repo)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v (deadline: %v)\n%s", what, err, ctx.Err(), output)
	}
}

// The FIFO guard opens the parent as "<dir>/.", which still follows a parent link that stays
// inside the repository: a scaffold file lands where it always did.
func TestWriteNewRepoFileFollowsAnInRepoParentLink(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("config", filepath.Join(repo, ".agent")); err != nil {
		t.Fatal(err)
	}
	if wrote, err := WriteProject(repo, nil); err != nil || !wrote {
		t.Fatalf("WriteProject through an in-repo parent link = (%v, %v)", wrote, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "config", "project.yaml")); err != nil {
		t.Fatalf("project.yaml did not land in the link's target: %v", err)
	}
}

// The parent handle spells the files it opens "<dir>/./<name>"; a write error names the repo path.
func TestWriteNewRepoFileErrorsNameTheRepoPath(t *testing.T) {
	repo := t.TempDir()
	dest := filepath.Join(repo, ".agent", "compose.yml")
	_, err := writeNewRepoFile(repo, dest, []byte("x"), 0o644, func(file *os.File, data []byte) error {
		if err := file.Close(); err != nil {
			return err
		}
		_, err := file.Write(data) // fails naming the file, as a full disk would
		return err
	})
	if err == nil || strings.Contains(err.Error(), "/./") || !strings.Contains(err.Error(), "write "+dest+":") {
		t.Fatalf("write error = %v, want it to name %s", err, dest)
	}
}

// An in-place edit stages its copy through the folder handle too, so a failed write names the copy
// beside the file, not the "<dir>/./<name>" the handle reports.
func TestInPlaceEditErrorsNameTheRepoPath(t *testing.T) {
	repo := t.TempDir()
	if _, err := WriteProject(repo, []string{"portal"}); err != nil {
		t.Fatal(err)
	}
	_, err := registerSubprojects(repo, []string{"portal", "infra"}, func(file *os.File, data []byte) error {
		if err := file.Close(); err != nil {
			return err
		}
		_, err := file.Write(data) // fails naming the file, as a full disk would
		return err
	})
	want := "write " + filepath.Join(".agent", ".project.yaml.coop-")
	if err == nil || strings.Contains(err.Error(), "/./") || !strings.Contains(err.Error(), want) {
		t.Fatalf("write error = %v, want it to name %s…", err, want)
	}
}

// linkSkillsIfAbsent follows a stale skills link to decide whether to keep it, and a box can swap
// .claude for a link to another folder while it looks. Removing the stale link by path again
// would then take the entry named skills from that folder: one .coopignore hides from the box, or
// one outside the repository such as ~/.claude.
func TestLinkIfAbsentIgnoresSwappedParent(t *testing.T) {
	for _, outside := range []bool{false, true} {
		t.Run(fmt.Sprintf("outside=%v", outside), func(t *testing.T) {
			repo := t.TempDir()
			folder, swap := filepath.Join(repo, "private"), "private"
			if outside {
				folder = t.TempDir()
				swap = folder
			}
			entry := filepath.Join(folder, "skills")
			if err := os.MkdirAll(folder, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(entry, []byte("hidden notes\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			parent, moved := filepath.Join(repo, ".claude"), filepath.Join(repo, ".claude-moved")
			if err := os.Mkdir(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../missing-skills", filepath.Join(parent, "skills")); err != nil {
				t.Fatal(err)
			}
			s := &scaffolder{repo: repo}
			err := s.linkIfAbsent("../.agent/skills", filepath.Join(parent, "skills"), func() bool {
				if err := os.Rename(parent, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(swap, parent); err != nil {
					t.Fatal(err)
				}
				return false
			})
			if got, err := os.ReadFile(entry); err != nil || string(got) != "hidden notes\n" {
				t.Errorf("the removal reached %s through the swapped parent: %q, %v", entry, got, err)
			}
			if err != nil {
				t.Fatalf("linkIfAbsent: %v", err)
			}
			if got, err := os.Readlink(filepath.Join(moved, "skills")); err != nil || got != "../.agent/skills" {
				t.Fatalf("the checked link = %q, %v; want it replaced where init checked it", got, err)
			}
		})
	}
}

// The handle names a link by its bare name; a link coop cannot create names the path init prints.
func TestLinkIfAbsentErrorsNameTheLink(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores folder permissions, so the link cannot be made to fail this way")
	}
	repo := t.TempDir()
	parent := filepath.Join(repo, ".claude")
	if err := os.Mkdir(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	link := filepath.Join(parent, "skills")
	err := (&scaffolder{repo: repo}).linkIfAbsent("../.agent/skills", link, nil)
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) || pathErr.Path != link || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("link error = %v, want a permission failure naming %s", err, link)
	}
}

func TestInitStack(t *testing.T) {
	// --stack asdf with a .tool-versions → the asdf .agent/Dockerfile (NOT compose: sibling
	// services are opt-in via `coop init`'s prompt / --services, so Init never adds db/redis).
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, ".tool-versions"), []byte("golang 1.26.4\n"), 0o644)
	if _, err := Init(repo, "asdf", nil, []string{"claude", "codex", "gemini"}); err != nil {
		t.Fatal(err)
	}
	df, err := os.ReadFile(filepath.Join(repo, ".agent/Dockerfile"))
	if err != nil || !strings.Contains(string(df), "asdf install") {
		t.Errorf("asdf stack .agent/Dockerfile missing or wrong:\n%s", df)
	}
	if _, err := os.Stat(filepath.Join(repo, ".agent", "compose.yml")); err == nil {
		t.Error("Init must not scaffold .agent/compose.yml — sibling services are opt-in")
	}

	// A removed per-language stack is now an error pointing at .tool-versions.
	if _, err := Init(t.TempDir(), "go", nil, nil); err == nil {
		t.Error("--stack go should error now that language stacks are gone")
	}

	// --stack asdf without a .tool-versions is an error (nothing to install from).
	if _, err := Init(t.TempDir(), "asdf", nil, nil); err == nil {
		t.Error("--stack asdf without a .tool-versions should error")
	}
}

func TestInitToolVersionsAsdf(t *testing.T) {
	// No --stack but a .tool-versions present → scaffold the asdf Dockerfile that
	// installs straight from it.
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, ".tool-versions"), []byte("erlang 29.0.1\nelixir 1.20.0-otp-29\ngolang 1.26.4\n"), 0o644)
	if _, err := Init(repo, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
		t.Fatal(err)
	}
	df, err := os.ReadFile(filepath.Join(repo, ".agent/Dockerfile"))
	if err != nil {
		t.Fatalf("asdf .agent/Dockerfile not written: %v", err)
	}
	for _, want := range []string{"asdf install", ".tool-versions"} {
		if !strings.Contains(string(df), want) {
			t.Errorf("asdf Dockerfile missing %q:\n%s", want, df)
		}
	}

	// No --stack and no .tool-versions → no Dockerfile is scaffolded.
	repo2 := t.TempDir()
	if _, err := Init(repo2, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo2, ".agent/Dockerfile")); !os.IsNotExist(err) {
		t.Error("no stack + no .tool-versions should not scaffold a .agent/Dockerfile")
	}

	// A removed language stack errors even when a .tool-versions is present —
	// the bad flag is surfaced rather than silently using .tool-versions.
	repo3 := t.TempDir()
	os.WriteFile(filepath.Join(repo3, ".tool-versions"), []byte("elixir 1.20.0-otp-29\n"), 0o644)
	if _, err := Init(repo3, "python", nil, nil); err == nil {
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
	// coop-only skills — never scaffolded into a user repo.
	//   release       — cuts coop's own versioned release
	//   rules-propose — leans on coop's rule-card format (.agent/kb/rules/README.md) and
	//                   `make rules-check`, neither of which `coop init` writes; shipping it
	//                   would point a fresh repo at files it doesn't have (scaffold-fits-the-repo).
	canonicalOnly := []string{"release", "rules-propose"}
	coopOnly := func(rel string) bool {
		for _, s := range canonicalOnly {
			if rel == s || strings.HasPrefix(rel, s+"/") {
				return true
			}
		}
		return false
	}
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
		if coopOnly(rel) {
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
	if _, err := Init(repo, "", nil, []string{"claude"}); err != nil {
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
	if _, err := Init(repo2, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".claude", ".codex", ".gemini", "CLAUDE.md", "GEMINI.md"} {
		if exists(filepath.Join(repo2, d)) {
			t.Errorf("%s should NOT be scaffolded with no agents", d)
		}
	}
	if !exists(filepath.Join(repo2, ".agent", "kb", "rules")) {
		t.Error(".agent/ is always scaffolded even with no agents")
	}
	// With no project .claude/ to shadow it, the fallback IS the repo's Claude adapter — so it must
	// be scaffolded here, and its hook command must resolve either way (the box copies it to the
	// USER-level ~/.claude, where $CLAUDE_PROJECT_DIR/.claude/hooks/ may not exist).
	for _, rel := range []string{".agent/claude/settings.json", ".agent/claude/hooks/commit-gate.sh"} {
		if !exists(filepath.Join(repo2, rel)) {
			t.Errorf("shared Claude fallback %s should be scaffolded with no per-agent dirs", rel)
		}
	}
	sharedSettings, err := os.ReadFile(filepath.Join(repo2, ".agent/claude/settings.json"))
	if err != nil || !json.Valid(sharedSettings) {
		t.Fatalf("shared Claude settings are missing or invalid JSON: %v\n%s", err, sharedSettings)
	}
	for _, want := range []string{"$CLAUDE_PROJECT_DIR/.claude/hooks/", "$CLAUDE_CONFIG_DIR/hooks/"} {
		if !strings.Contains(string(sharedSettings), want) {
			t.Errorf("shared Claude settings missing hook fallback %q:\n%s", want, sharedSettings)
		}
	}
	assertClaudeHookFallbacks(t, sharedSettings)
	if exists(filepath.Join(repo2, ".agent/claude/hooks/stop-guard.sh")) {
		t.Error("shared Claude fallback should not scaffold the retired global Stop guard")
	}
}
