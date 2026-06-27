// Package scaffold writes the Coop working set into a repo: AGENTS.md, the
// .agent/ queue, .claude/ settings + hooks, the workflow skills, and optionally
// a per-project Dockerfile.agent + compose.agent.yml. Every template is embedded
// in the binary, so one `coop` binary can scaffold any repo with no extra files.
package scaffold

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
func Init(repo, stack string, gateLangs []string) error {
	s := &scaffolder{repo: repo}
	if err := mkdirs(
		filepath.Join(repo, ".agent", "rules"),
		filepath.Join(repo, ".agent", "skills"),
		// State dirs carry a numeric sort prefix so `ls .agent/tasks` lists them in lifecycle
		// order; names must match the cli package's state constants (taskdir.go) verbatim.
		filepath.Join(repo, ".agent", "tasks", "00_todo"),
		filepath.Join(repo, ".agent", "tasks", "10_in_progress"),
		filepath.Join(repo, ".agent", "tasks", "50_blocked"),
		filepath.Join(repo, ".agent", "tasks", "xx_done"),
		filepath.Join(repo, ".claude", "hooks"),
		filepath.Join(repo, ".codex"),
		filepath.Join(repo, ".gemini"),
	); err != nil {
		return err
	}

	files := []struct {
		dest, src string
		perm      os.FileMode
	}{
		{filepath.Join(repo, "AGENTS.md"), "templates/AGENTS.md", 0o644},
		{filepath.Join(repo, ".agent", "tasks", "README.md"), "templates/agent/tasks/README.md", 0o644},
		{filepath.Join(repo, ".agent", "BACKLOG.md"), "templates/agent/BACKLOG.md", 0o644},
		{filepath.Join(repo, ".claude", "settings.json"), "templates/claude/settings.json", 0o644},
		{filepath.Join(repo, ".claude", "hooks", "stop-guard.sh"), "templates/claude/hooks/stop-guard.sh", 0o755},
		// commit-gate.sh is generated per-stack in installGitHooks, not copied verbatim.
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
	if err := s.linkIfAbsent("AGENTS.md", filepath.Join(repo, "CLAUDE.md")); err != nil {
		return err
	}
	if err := s.linkIfAbsent("AGENTS.md", filepath.Join(repo, "GEMINI.md")); err != nil {
		return err
	}
	for _, dir := range []string{".claude", ".codex", ".gemini"} {
		if err := s.linkIfAbsent("../.agent/skills", filepath.Join(repo, dir, "skills")); err != nil {
			return err
		}
	}

	if err := s.copySkills(); err != nil {
		return err
	}
	if err := s.updateGitignore(); err != nil {
		return err
	}
	if err := s.installGitHooks(gateLangs); err != nil {
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
		ui.Detail("kept existing %s", filepath.Base(dest))
		return nil
	}
	data, err := templates.ReadFile(embedPath)
	if err != nil {
		return err
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

// writeContentIfAbsent writes generated content to dest (like writeIfAbsent, but from a
// string rather than an embedded template), never clobbering an existing file.
func (s *scaffolder) writeContentIfAbsent(dest, content string, perm os.FileMode) error {
	if _, err := os.Lstat(dest); err == nil {
		ui.Detail("kept existing %s", filepath.Base(dest))
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dest, []byte(content), perm); err != nil {
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
		ui.Detail("kept existing %s", filepath.Base(link))
	case os.IsNotExist(err), isLink:
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			return err
		}
		ui.Detail("linked %s -> %s", s.rel(link), target)
	default:
		ui.Detail("kept existing %s (real file, not a symlink)", filepath.Base(link))
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

// installGitHooks generates the tracked .githooks/pre-commit gate (every committer — Codex,
// Gemini, a plain `git commit`) and the .claude/hooks/commit-gate.sh (Claude), each carrying
// the format checks for the repo's detected stacks (langs), then points git at .githooks via
// core.hooksPath. A repo with no detected stack gets a neutral gate — coop never imposes a
// check it doesn't use. A user's custom hooksPath is never clobbered.
func (s *scaffolder) installGitHooks(langs []string) error {
	if len(langs) > 0 {
		ui.Detail("commit gate: %s", strings.Join(langs, ", "))
	} else {
		ui.Detail("commit gate: no language detected — left neutral (edit .githooks/pre-commit to add checks)")
	}
	if err := s.writeContentIfAbsent(filepath.Join(s.repo, ".githooks", "pre-commit"), preCommitHook(langs), 0o755); err != nil {
		return err
	}
	if err := s.writeContentIfAbsent(filepath.Join(s.repo, ".claude", "hooks", "commit-gate.sh"), claudeCommitGate(langs), 0o755); err != nil {
		return err
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
	if data, err := os.ReadFile(gi); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			// Match coop's exact marker line, not a prefix — a pre-existing broad rule like
			// `.agent/*.log` must NOT make us skip the block (which would drop the !rules/!skills
			// un-ignore and the .gemini symlink rules, leaving tracked dirs ignored).
			if strings.TrimSpace(line) == ".agent/*" {
				return nil // coop's block is already present
			}
		}
	}
	f, err := os.OpenFile(gi, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	const block = "\n# coop working state (commit knowledge, ignore state)\n.agent/*\n!.agent/rules/\n!.agent/skills/\n" +
		"\n# .gemini may be globally ignored (local Gemini state); keep just the skills symlink\n!.gemini/\n.gemini/*\n!.gemini/skills\n"
	if _, err := f.WriteString(block); err != nil {
		return err
	}
	ui.Detail("updated .gitignore (.agent state ignored; rules/ + skills/ tracked)")
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
