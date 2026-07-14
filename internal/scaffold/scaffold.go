// Package scaffold writes the Coop working set into a repo: AGENTS.md, the
// .agent/ queue and agent fallbacks, optional project adapters, the workflow
// skills, and optionally a per-project Dockerfile.agent + .agent/compose.yml.
// Every template is embedded in the binary, so one `coop` binary can scaffold
// any repo with no extra files.
package scaffold

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/taskstate"
	"github.com/AndrewDryga/coop/internal/ui"
)

//go:embed all:templates
var templates embed.FS

// Init scaffolds the working set into repo. The toolchain is driven by
// .tool-versions: with no --stack a present .tool-versions auto-scaffolds the asdf
// Dockerfile.agent; `--stack asdf` forces it. gateLangs are the stacks the commit hooks
// check (from DetectStacks, or the caller's interactive prompt); empty means a neutral gate.
// Per-file progress prints as faint ui.Detail lines; the caller prints the summary and the
// next-step actions. Existing files are never clobbered.
func Init(repo, stack string, gateLangs, agentDirs []string) error {
	s := &scaffolder{repo: repo}
	// A per-agent dir (.claude/.codex/.gemini) is scaffolded only for agents in agentDirs — the ones
	// you actually use. A repo that drops the others stays clean: a box synthesizes a missing agent's
	// skills from .agent/ on demand (see box.synthSkillsMounts). agentDirs is the signed-in set (or
	// `coop init --agents …`); empty means .agent/ only.
	has := func(a string) bool { return slices.Contains(agentDirs, a) }
	dirs := []string{
		filepath.Join(repo, ".agent", "rules"),
		filepath.Join(repo, ".agent", "skills"),
		filepath.Join(repo, ".agent", "presets"), // orchestration recipes live here (coop presets init writes one)
		filepath.Join(repo, ".agent", "claude", "hooks"),
	}
	if has("claude") {
		dirs = append(dirs, filepath.Join(repo, ".claude", "hooks"))
	}
	if has("codex") {
		dirs = append(dirs, filepath.Join(repo, ".codex"))
	}
	if has("gemini") {
		dirs = append(dirs, filepath.Join(repo, ".gemini"))
	}
	// The task-queue state dirs come from the shared taskstate package, so `coop init` can never
	// scaffold a name the cli can't read. The numeric prefix sorts `ls .agent/tasks` by lifecycle.
	for _, st := range taskstate.All {
		dirs = append(dirs, filepath.Join(repo, ".agent", "tasks", st))
	}
	if err := mkdirs(dirs...); err != nil {
		return err
	}

	type scaffFile struct {
		dest, src string
		perm      os.FileMode
	}
	files := []scaffFile{
		{filepath.Join(repo, "AGENTS.md"), "templates/AGENTS.md", 0o644},
		{filepath.Join(repo, ".agent", "tasks", "README.md"), "templates/agent/tasks/README.md", 0o644},
		// One committed loop config (fully commented → no behavior change until you uncomment a key).
		{filepath.Join(repo, ".agent", "loop.yaml"), "templates/agent/loop.yaml", 0o644},
		// Claude fallback adapter: coop copies these user-level into a box only when the matching
		// project artifact is absent. The project .claude/ adapter below remains authoritative.
		{filepath.Join(repo, ".agent", "claude", "settings.json"), "templates/agent/claude/settings.json", 0o644},
		{filepath.Join(repo, ".agent", "claude", "hooks", "stop-guard.sh"), "templates/claude/hooks/stop-guard.sh", 0o755},
	}
	if has("claude") {
		// Claude's settings + stop-guard hook, and the starter subagents for the orchestrator pattern
		// (the lead delegates reasoning-heavy phases to an Opus-pinned specialist and mechanical work to
		// a Sonnet-pinned one). Native Claude Code files — inert until a task fits their frontmatter.
		// commit-gate.sh is generated per-stack in installGitHooks, not copied verbatim.
		files = append(files,
			scaffFile{filepath.Join(repo, ".claude", "settings.json"), "templates/claude/settings.json", 0o644},
			scaffFile{filepath.Join(repo, ".claude", "hooks", "stop-guard.sh"), "templates/claude/hooks/stop-guard.sh", 0o755},
			scaffFile{filepath.Join(repo, ".claude", "agents", "deep-reasoner.md"), "templates/claude/agents/deep-reasoner.md", 0o644},
			scaffFile{filepath.Join(repo, ".claude", "agents", "fast-worker.md"), "templates/claude/agents/fast-worker.md", 0o644},
		)
	}
	for _, f := range files {
		if err := s.writeIfAbsent(f.dest, f.src, f.perm); err != nil {
			return err
		}
	}

	// One brain, every agent: AGENTS.md is canonical and CLAUDE.md / GEMINI.md
	// symlink to it. The workflow skills likewise live once, in .agent/skills, and
	// each agent's skills dir (.claude / .codex / .gemini) symlinks to it. A real
	// (non-symlink) instruction file or skills dir is never clobbered.
	if has("claude") {
		if err := s.linkIfAbsent("AGENTS.md", filepath.Join(repo, "CLAUDE.md")); err != nil {
			return err
		}
	}
	if has("gemini") {
		if err := s.linkIfAbsent("AGENTS.md", filepath.Join(repo, "GEMINI.md")); err != nil {
			return err
		}
	}
	for _, dir := range []string{".claude", ".codex", ".gemini"} {
		if !has(strings.TrimPrefix(dir, ".")) {
			continue
		}
		if err := s.linkIfAbsent("../.agent/skills", filepath.Join(repo, dir, "skills")); err != nil {
			return err
		}
	}

	if err := s.copySkills(); err != nil {
		return err
	}
	// The committed per-project config (serve ports, monorepo members). Never clobbers an existing one.
	if _, err := WriteProject(repo, DetectSubprojects(repo)); err != nil {
		return err
	}
	if err := s.updateGitignore(); err != nil {
		return err
	}
	// The shared Claude fallback needs the same stack-aware commit gate even when the repo keeps no
	// project .claude/ adapter. A selected Claude adapter receives its project copy as before.
	if err := s.installGitHooks(gateLangs, has("claude")); err != nil {
		return err
	}

	// The toolchain is driven by .tool-versions (asdf). With no --stack, auto-detect
	// a .tool-versions and scaffold the asdf Dockerfile from it. The only explicit
	// stack is "asdf"; the per-language stacks are gone — pin versions in
	// .tool-versions instead, and coop provisions them.
	switch stack {
	case "":
		if _, err := os.Stat(filepath.Join(repo, ".tool-versions")); err == nil {
			stack = "asdf"
			ui.Detail("detected .tool-versions — scaffolding an asdf-driven Dockerfile.agent")
		}
	case "asdf":
		// scaffolded below
	default:
		return fmt.Errorf("unknown --stack %q: coop provisions toolchains from .tool-versions now\n"+
			"  pin versions there and run `coop init` (auto-detected), or `coop init --stack asdf`", stack)
	}
	if stack == "asdf" {
		if _, err := os.Stat(filepath.Join(repo, ".tool-versions")); err != nil {
			return fmt.Errorf("--stack asdf needs a .tool-versions in the repo\n" +
				"  e.g. `echo 'elixir 1.18.3-otp-27' > .tool-versions`, then re-run")
		}
		if err := s.writeIfAbsent(filepath.Join(repo, "Dockerfile.agent"), "templates/dockerfile/asdf", 0o644); err != nil {
			return err
		}
	}

	// The "scaffolded into …" summary, the optional Docker-box suggestion, and the next-step
	// actions are all printed by the caller (cmdInit), which has the full picture (services,
	// mcp) and orders them as one block after the faint per-file log.
	return nil
}

type scaffolder struct{ repo string }

func (s *scaffolder) rel(p string) string {
	if r, err := filepath.Rel(s.repo, p); err == nil {
		return r
	}
	return p
}

func (s *scaffolder) writeIfAbsent(dest, embedPath string, perm os.FileMode) error {
	if _, err := os.Lstat(dest); err == nil {
		ui.Detail("kept existing %s", s.rel(dest))
		return nil // present: don't even read the template
	}
	data, err := templates.ReadFile(embedPath)
	if err != nil {
		return err
	}
	return s.writeNewFile(dest, data, perm)
}

// writeContentIfAbsent writes generated content to dest (like writeIfAbsent, but from a
// string rather than an embedded template), never clobbering an existing file.
func (s *scaffolder) writeContentIfAbsent(dest, content string, perm os.FileMode) error {
	return s.writeNewFile(dest, []byte(content), perm)
}

// writeNewFile writes data to dest with perm (creating parent dirs), unless dest already
// exists — then it's left untouched. Either way it reports what it did. Shared tail of the
// two IfAbsent wrappers, which differ only in their byte source.
func (s *scaffolder) writeNewFile(dest string, data []byte, perm os.FileMode) error {
	if _, err := os.Lstat(dest); err == nil {
		ui.Detail("kept existing %s", s.rel(dest))
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dest, data, perm); err != nil {
		return err
	}
	ui.Detail("wrote %s", s.rel(dest))
	return nil
}

// linkIfAbsent creates a symlink, replacing an existing symlink but never a real
// file (which usually holds content a symlink would silently destroy).
func (s *scaffolder) linkIfAbsent(target, link string) error {
	fi, err := os.Lstat(link)
	isLink := err == nil && fi.Mode()&os.ModeSymlink != 0
	current := ""
	if isLink {
		current, _ = os.Readlink(link)
	}
	switch {
	case isLink && current == target:
		// Already the symlink we'd create — a re-run is a no-op, so say so rather than report
		// "linked" (an action verb that reads like a rewrite) on every subsequent init.
		ui.Detail("kept existing %s", s.rel(link))
	case os.IsNotExist(err), isLink:
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			return err
		}
		ui.Detail("linked %s -> %s", s.rel(link), target)
	default:
		ui.Detail("kept existing %s (real file, not a symlink)", s.rel(link))
	}
	return nil
}

func (s *scaffolder) copySkills() error {
	entries, err := templates.ReadDir("templates/skills")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		dest := filepath.Join(s.repo, ".agent", "skills", name)
		if _, err := os.Stat(dest); err == nil {
			ui.Detail("kept existing skill /%s", name)
			continue
		}
		if err := copyEmbedDir("templates/skills/"+name, dest); err != nil {
			return err
		}
		ui.Detail("added skill /%s", name)
	}
	return nil
}

// installGitHooks generates the tracked .githooks/pre-commit gate (every committer) and Claude's
// shared fallback commit gate, plus the project-scoped Claude copy when requested. Each carries the
// detected stack checks. A repo with no detected stack gets a neutral gate. A user's custom
// hooksPath is never clobbered.
func (s *scaffolder) installGitHooks(langs []string, projectClaude bool) error {
	if len(langs) > 0 {
		ui.Detail("commit gate: %s", strings.Join(langs, ", "))
	} else {
		ui.Detail("commit gate: no language detected — left neutral (edit .githooks/pre-commit to add checks)")
	}
	if err := s.writeContentIfAbsent(filepath.Join(s.repo, ".githooks", "pre-commit"), preCommitHook(langs), 0o755); err != nil {
		return err
	}
	if err := s.writeContentIfAbsent(filepath.Join(s.repo, ".agent", "claude", "hooks", "commit-gate.sh"), claudeCommitGate(langs), 0o755); err != nil {
		return err
	}
	if projectClaude {
		if err := s.writeContentIfAbsent(filepath.Join(s.repo, ".claude", "hooks", "commit-gate.sh"), claudeCommitGate(langs), 0o755); err != nil {
			return err
		}
	}
	if !gitRepo(s.repo) {
		ui.Detail("not a git repo yet — after 'git init', run: git config core.hooksPath .githooks")
		return nil
	}
	switch current := gitConfigGet(s.repo, "core.hooksPath"); current {
	case "", ".githooks":
		if err := gitConfigSet(s.repo, "core.hooksPath", ".githooks"); err != nil {
			return err
		}
		ui.Detail("set core.hooksPath=.githooks (pre-commit format gate for every committer)")
	default:
		ui.Detail("kept your core.hooksPath=%q; coop's gate is in .githooks/pre-commit", current)
	}
	return nil
}

// gitRepo reports whether repo is inside a git work tree.
func gitRepo(repo string) bool {
	return exec.Command("git", "-C", repo, "rev-parse", "--git-dir").Run() == nil
}

// gitConfigGet returns the effective (local or global) value of a git config key, or "".
func gitConfigGet(repo, key string) string {
	out, err := exec.Command("git", "-C", repo, "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitConfigSet sets a git config key in repo's local config.
func gitConfigSet(repo, key, value string) error {
	return exec.Command("git", "-C", repo, "config", "--local", key, value).Run()
}

func (s *scaffolder) updateGitignore() error {
	gi := filepath.Join(s.repo, ".gitignore")
	data, _ := os.ReadFile(gi) // missing file → empty; we create it below
	content := string(data)
	orig := content
	// **/.agent/* ignores .agent/ state at any depth, so a monorepo member's working state (its
	// tasks/backlog) is ignored too. Committed KNOWLEDGE — rules/skills/presets and the loop.yaml
	// config — is un-ignored at any depth as well, since a large monorepo member may carry its own;
	// only project.yaml is TOP-LEVEL (the single subprojects+serve config), so its un-ignore stays
	// root-anchored.
	const block = "\n# coop working state (commit knowledge, ignore state)\n**/.agent/*\n!**/.agent/rules/\n!**/.agent/skills/\n!**/.agent/presets/\n!**/.agent/claude/\n!**/.agent/loop.yaml\n!**/.agent/compose.yml\n!.agent/project.yaml\n" +
		"\n# preset native subagents coop generates in the box (coop-<role>) — never committed\n.claude/agents/coop-*.md\n" +
		"\n# .gemini may be globally ignored (local Gemini state); keep just the skills symlink\n!.gemini/\n.gemini/*\n!.gemini/skills\n"
	if !strings.Contains(content, "**/.agent/*") {
		content += block // no coop block yet — append the whole thing
	}
	// Upgrade an existing Coop block without duplicating it. Older scaffolds predate the shared
	// Claude fallback and would otherwise keep the new source ignored forever.
	if !strings.Contains(content, "!**/.agent/claude/") {
		content = strings.Replace(content, "!**/.agent/skills/\n", "!**/.agent/skills/\n!**/.agent/claude/\n", 1)
	}
	if content == orig {
		return nil // already up to date
	}
	if err := os.WriteFile(gi, []byte(content), 0o644); err != nil {
		return err
	}
	ui.Detail("updated .gitignore (.agent state ignored at any depth; rules/skills/presets/claude/loop + project.yaml tracked)")
	return nil
}

func copyEmbedDir(src, dest string) error {
	return fs.WalkDir(templates, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := templates.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func mkdirs(paths ...string) error {
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return err
		}
	}
	return nil
}
