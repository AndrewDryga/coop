package scaffold

import (
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
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck not installed")
	}
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

// A pre-existing broad .gitignore line (e.g. .agent/*.log) must NOT make coop init skip writing its
// block — that would drop the !rules/!skills un-ignore and leave tracked dirs ignored.
func TestUpdateGitignoreBroadPrefixDoesNotSkipBlock(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("node_modules/\n.agent/*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (&scaffolder{repo: repo}).updateGitignore(); err != nil {
		t.Fatal(err)
	}
	gi, _ := os.ReadFile(filepath.Join(repo, ".gitignore"))
	for _, want := range []string{".agent/*\n", "!.agent/rules/", "!.agent/skills/"} {
		if !strings.Contains(string(gi), want) {
			t.Errorf("coop's block missing %q after a broad .agent/*.log line:\n%s", want, gi)
		}
	}
	// And it stays idempotent: a second run doesn't duplicate the exact marker.
	_ = (&scaffolder{repo: repo}).updateGitignore()
	gi2, _ := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if n := strings.Count(string(gi2), "\n.agent/*\n"); n != 1 {
		t.Errorf("coop block written %d times, want 1:\n%s", n, gi2)
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
	if err := Init(repo, "", nil); err != nil {
		t.Fatal(err)
	}

	// Core files exist with content.
	for _, rel := range []string{
		"AGENTS.md", ".agent/tasks/README.md", ".agent/BACKLOG.md",
		".claude/settings.json", ".claude/hooks/stop-guard.sh", ".claude/hooks/commit-gate.sh",
		".githooks/pre-commit",
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

	// The four task state directories exist (the folder-mode queue, with the lifecycle-sort prefix).
	for _, st := range []string{"00_todo", "10_in_progress", "50_blocked", "xx_done"} {
		if fi, err := os.Stat(filepath.Join(repo, ".agent/tasks", st)); err != nil || !fi.IsDir() {
			t.Errorf(".agent/tasks/%s should be a directory: %v", st, err)
		}
	}

	// Hooks are executable.
	if fi, _ := os.Stat(filepath.Join(repo, ".claude/hooks/stop-guard.sh")); fi != nil && fi.Mode()&0o100 == 0 {
		t.Error("stop-guard.sh is not executable")
	}
	if fi, _ := os.Stat(filepath.Join(repo, ".githooks/pre-commit")); fi != nil && fi.Mode()&0o100 == 0 {
		t.Error(".githooks/pre-commit is not executable")
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
	for _, s := range []string{"spec", "investigate"} {
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
	for _, bad := range []string{"coop fork", "coop fleet", "coop-consult"} {
		if strings.Contains(string(agents), bad) {
			t.Errorf("AGENTS.md should not recommend host-side/non-native orchestration %q:\n%s", bad, agents)
		}
	}

	// .gitignore carries the working-state rule (skills + rules tracked).
	gi, _ := os.ReadFile(filepath.Join(repo, ".gitignore"))
	for _, want := range []string{".agent/*", "!.agent/rules/", "!.agent/skills/", "!.gemini/skills"} {
		if !strings.Contains(string(gi), want) {
			t.Errorf(".gitignore missing %q:\n%s", want, gi)
		}
	}
}

func TestGlobalInstructionsExampleTeachesAgentStack(t *testing.T) {
	path := filepath.Join("..", "..", "agents", "INSTRUCTIONS.md.example")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/goal", "/batch", "subagents", "native", "runtime does not have", "separate workspaces"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("global INSTRUCTIONS example missing agent-stack guidance %q:\n%s", want, data)
		}
	}
	for _, bad := range []string{"coop fork", "coop fleet", "coop-consult", "separate fork"} {
		if strings.Contains(string(data), bad) {
			t.Errorf("global INSTRUCTIONS example should not recommend host-side/non-native orchestration %q:\n%s", bad, data)
		}
	}
}

func TestInitGitHooks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	gitInit := func(dir string) {
		t.Helper()
		if out, err := exec.Command("git", "-C", dir, "init").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
	}

	// A fresh repo gets core.hooksPath pointed at the tracked, executable hook.
	repo := t.TempDir()
	gitInit(repo)
	if err := Init(repo, "", nil); err != nil {
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

	// A user's custom hooksPath is left untouched (re-init is non-destructive).
	repo2 := t.TempDir()
	gitInit(repo2)
	if err := gitConfigSet(repo2, "core.hooksPath", ".my-hooks"); err != nil {
		t.Fatal(err)
	}
	if err := Init(repo2, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := gitConfigGet(repo2, "core.hooksPath"); got != ".my-hooks" {
		t.Errorf("custom core.hooksPath was clobbered: got %q, want .my-hooks", got)
	}
}

func TestInitIdempotent(t *testing.T) {
	repo := t.TempDir()
	if err := Init(repo, "", nil); err != nil {
		t.Fatal(err)
	}
	// Edit a file, then re-init: it must be kept, not overwritten.
	readme := filepath.Join(repo, ".agent/tasks/README.md")
	os.WriteFile(readme, []byte("MY EDITS"), 0o644)

	// Capture the re-run's log. An unchanged symlink must read as "kept existing", not the action
	// verb "linked" (which looks like a rewrite on every subsequent init); and a kept skill must
	// carry the same leading slash the added branch prints, so the wording can't flip run-to-run.
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	err := Init(repo, "", nil)
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
	if err := Init(repo, "", nil); err != nil {
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
	if err := Init(repo, "asdf", nil); err != nil {
		t.Fatal(err)
	}
	df, err := os.ReadFile(filepath.Join(repo, "Dockerfile.agent"))
	if err != nil || !strings.Contains(string(df), "asdf install") {
		t.Errorf("asdf stack Dockerfile.agent missing or wrong:\n%s", df)
	}
	if _, err := os.Stat(filepath.Join(repo, "compose.agent.yml")); err == nil {
		t.Error("Init must not scaffold compose.agent.yml — sibling services are opt-in")
	}

	// A removed per-language stack is now an error pointing at .tool-versions.
	if err := Init(t.TempDir(), "go", nil); err == nil {
		t.Error("--stack go should error now that language stacks are gone")
	}

	// --stack asdf without a .tool-versions is an error (nothing to install from).
	if err := Init(t.TempDir(), "asdf", nil); err == nil {
		t.Error("--stack asdf without a .tool-versions should error")
	}
}

func TestInitToolVersionsAsdf(t *testing.T) {
	// No --stack but a .tool-versions present → scaffold the asdf Dockerfile that
	// installs straight from it.
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, ".tool-versions"), []byte("erlang 29.0.1\nelixir 1.20.0-otp-29\ngolang 1.26.4\n"), 0o644)
	if err := Init(repo, "", nil); err != nil {
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
	if err := Init(repo2, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo2, "Dockerfile.agent")); !os.IsNotExist(err) {
		t.Error("no stack + no .tool-versions should not scaffold a Dockerfile.agent")
	}

	// A removed language stack errors even when a .tool-versions is present —
	// the bad flag is surfaced rather than silently using .tool-versions.
	repo3 := t.TempDir()
	os.WriteFile(filepath.Join(repo3, ".tool-versions"), []byte("elixir 1.20.0-otp-29\n"), 0o644)
	if err := Init(repo3, "python", nil); err == nil {
		t.Error("--stack python should error regardless of .tool-versions")
	}
}
