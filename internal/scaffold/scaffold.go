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
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/ui"
)

//go:embed all:templates
var templates embed.FS

// Init scaffolds the working set into repo. The toolchain is driven by
// .tool-versions: with no --stack a present .tool-versions auto-scaffolds the asdf
// Dockerfile.agent (+ compose file); `--stack asdf` forces it. Existing files are
// never clobbered.
func Init(repo, stack string) error {
	s := &scaffolder{repo: repo}
	if err := mkdirs(
		filepath.Join(repo, ".agent", "rules"),
		filepath.Join(repo, ".claude", "hooks"),
		filepath.Join(repo, ".claude", "skills"),
		filepath.Join(repo, ".codex"),
	); err != nil {
		return err
	}

	files := []struct {
		dest, src string
		perm      os.FileMode
	}{
		{filepath.Join(repo, "AGENTS.md"), "templates/AGENTS.md", 0o644},
		{filepath.Join(repo, ".agent", "TASKS.md"), "templates/agent/TASKS.md", 0o644},
		{filepath.Join(repo, ".agent", "BACKLOG.md"), "templates/agent/BACKLOG.md", 0o644},
		{filepath.Join(repo, ".agent", "LOG.md"), "templates/agent/LOG.md", 0o644},
		{filepath.Join(repo, ".agent", "PENDING_DECISIONS.md"), "templates/agent/PENDING_DECISIONS.md", 0o644},
		{filepath.Join(repo, ".agent", "IDEAS.md"), "templates/agent/IDEAS.md", 0o644},
		{filepath.Join(repo, ".claude", "settings.json"), "templates/claude/settings.json", 0o644},
		{filepath.Join(repo, ".claude", "hooks", "stop-guard.sh"), "templates/claude/hooks/stop-guard.sh", 0o755},
		{filepath.Join(repo, ".claude", "hooks", "commit-gate.sh"), "templates/claude/hooks/commit-gate.sh", 0o755},
	}
	for _, f := range files {
		if err := s.writeIfAbsent(f.dest, f.src, f.perm); err != nil {
			return err
		}
	}

	// One brain, every agent: AGENTS.md is canonical; CLAUDE.md and GEMINI.md
	// symlink to it, and Codex shares Claude's skills directory. A real
	// (non-symlink) instruction file is never clobbered.
	if err := s.linkIfAbsent("AGENTS.md", filepath.Join(repo, "CLAUDE.md")); err != nil {
		return err
	}
	if err := s.linkIfAbsent("AGENTS.md", filepath.Join(repo, "GEMINI.md")); err != nil {
		return err
	}
	if err := s.linkIfAbsent("../.claude/skills", filepath.Join(repo, ".codex", "skills")); err != nil {
		return err
	}

	if err := s.copySkills(); err != nil {
		return err
	}
	if err := s.updateGitignore(); err != nil {
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
			ui.Info("detected .tool-versions — scaffolding an asdf-driven Dockerfile.agent")
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
		if err := s.writeIfAbsent(filepath.Join(repo, "compose.agent.yml"), "templates/compose.agent.yml", 0o644); err != nil {
			return err
		}
		ui.Info("asdf stack: review Dockerfile.agent, then 'coop build' and 'coop up'")
	}

	ui.Info("scaffolded AGENTS.md, .agent/, .claude/ hooks, and workflow skills into %s", repo)
	if stack != "" {
		ui.Info("then: coop up && agent   (db/redis reachable in the box)")
	}
	ui.Info("edit .agent/TASKS.md, then run 'coop loop'")
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
		ui.Info("kept existing %s", filepath.Base(dest))
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
	ui.Info("wrote %s", s.rel(dest))
	return nil
}

// linkIfAbsent creates a symlink, replacing an existing symlink but never a real
// file (which usually holds content a symlink would silently destroy).
func (s *scaffolder) linkIfAbsent(target, link string) error {
	fi, err := os.Lstat(link)
	switch {
	case os.IsNotExist(err), err == nil && fi.Mode()&os.ModeSymlink != 0:
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			return err
		}
		ui.Info("linked %s -> %s", s.rel(link), target)
	default:
		ui.Info("kept existing %s (real file, not a symlink)", filepath.Base(link))
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
		dest := filepath.Join(s.repo, ".claude", "skills", name)
		if _, err := os.Stat(dest); err == nil {
			ui.Info("kept existing skill %s", name)
			continue
		}
		if err := copyEmbedDir("templates/skills/"+name, dest); err != nil {
			return err
		}
		ui.Info("added skill /%s", name)
	}
	return nil
}

func (s *scaffolder) updateGitignore() error {
	gi := filepath.Join(s.repo, ".gitignore")
	if data, err := os.ReadFile(gi); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, ".agent/*") {
				return nil // already ignored
			}
		}
	}
	f, err := os.OpenFile(gi, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString("\n# coop working state (commit knowledge, ignore state)\n.agent/*\n!.agent/rules/\n"); err != nil {
		return err
	}
	ui.Info("updated .gitignore (.agent state ignored, rules/ tracked)")
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
