// Package scaffold writes the Coop working set into a repo: AGENTS.md, the
// .agent/ queue and agent fallbacks, optional project adapters, the workflow
// skills, and optionally a per-project .agent/Dockerfile + .agent/compose.yml.
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
	"syscall"

	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/taskstate"
	"github.com/AndrewDryga/coop/internal/ui"
)

//go:embed all:templates
var templates embed.FS

// Init scaffolds the working set into repo. The toolchain is driven by
// .tool-versions: with no --stack a present .tool-versions auto-scaffolds the asdf
// .agent/Dockerfile; `--stack asdf` forces it. gateLangs are the stacks the commit hooks
// check (from DetectStacks, or the caller's interactive prompt); empty means a neutral gate.
// Routine per-artifact progress is NOT printed: the caller states the outcome once and lists
// the actions it left. Only an exception a person must act on (a hooks path or prepare hook
// coop refused to take over) speaks. Existing files are never clobbered.
func Init(repo, stack string, gateLangs, agentDirs []string) error {
	dockerfile, detected, err := initDockerfile(repo, stack)
	if err != nil {
		return err
	}
	s := &scaffolder{repo: repo}
	// A per-agent dir (.claude/.codex/.gemini) is scaffolded only for agents in agentDirs — the ones
	// you actually use. A repo that drops the others stays clean: a box synthesizes a missing agent's
	// skills from the repo's shared source on demand (see box.synthSkillsMounts). agentDirs is the
	// signed-in set (or `coop init --agents …`); empty means .agent/ only.
	has := func(a string) bool { return slices.Contains(agentDirs, a) }
	skillsRoot := skillsSource(repo)
	dirs := []string{
		filepath.Join(repo, ".agent", "kb", "rules"),
		skillsRoot,
		filepath.Join(repo, ".agent", "presets"), // orchestration recipes live here (coop presets init writes one)
	}
	// The .agent/claude/ fallback is only ever read when the repo has NO project .claude/ artifact
	// (see agent.claudeAgent.HomeFallbacks) — scaffolding both would commit a second, byte-identical
	// copy of the same settings + commit gate that nothing reads.
	if has("claude") {
		dirs = append(dirs, filepath.Join(repo, ".claude", "hooks"))
	} else {
		dirs = append(dirs, filepath.Join(repo, ".agent", "claude", "hooks"))
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
	if err := mkdirs(repo, dirs...); err != nil {
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
	}
	if has("claude") {
		// Claude's project settings. commit-gate.sh is generated per-stack in installGitHooks, not
		// copied verbatim. Subagents are NOT scaffolded: a preset generates its own coop-<role> in the
		// box, and a repo with its own roles doesn't want two competing sets committed.
		files = append(files, scaffFile{filepath.Join(repo, ".claude", "settings.json"), "templates/claude/settings.json", 0o644})
	} else {
		// Claude fallback adapter: coop copies this user-level into a box only when the project
		// .claude/ artifact is absent — which, with no .claude/ scaffolded, is exactly this repo.
		files = append(files, scaffFile{filepath.Join(repo, ".agent", "claude", "settings.json"), "templates/agent/claude/settings.json", 0o644})
	}
	for _, f := range files {
		if err := s.writeIfAbsent(f.dest, f.src, f.perm); err != nil {
			return err
		}
	}

	// One brain, every agent: AGENTS.md is canonical and CLAUDE.md / GEMINI.md symlink to it.
	// Workflow skills likewise keep one established source: .agent/skills normally, or an existing
	// real .claude/skills. A real instruction file, skills dir, or valid skills link is never clobbered.
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
		link := filepath.Join(repo, dir, "skills")
		target, err := filepath.Rel(filepath.Dir(link), skillsRoot)
		if err != nil {
			return err
		}
		if err := s.linkSkillsIfAbsent(target, link); err != nil {
			return err
		}
	}

	if skillsRoot == filepath.Join(repo, ".agent", "skills") {
		if err := s.copySkills(); err != nil {
			return err
		}
	}
	// The committed per-project config (serve ports, monorepo members). Never clobbers an existing one.
	if _, err := WriteProject(repo, DetectSubprojects(repo)); err != nil {
		return err
	}
	if err := s.updateGitignore(has("gemini")); err != nil {
		return err
	}
	// The shared Claude fallback needs the same stack-aware commit gate even when the repo keeps no
	// project .claude/ adapter. A selected Claude adapter receives its project copy as before.
	if err := s.installGitHooks(gateLangs, has("claude")); err != nil {
		return err
	}

	if dockerfile != "" {
		// Keep the announcement next to the write, and only when auto-detection
		// actually adds a Dockerfile rather than keeping a customized one.
		if detected {
			if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(project.DefaultDockerfile))); err != nil {
				ui.Detail("detected .tool-versions — scaffolding an asdf-driven .agent/Dockerfile")
			}
		}
		if err := s.writeContentIfAbsent(filepath.Join(repo, filepath.FromSlash(project.DefaultDockerfile)), dockerfile, 0o644); err != nil {
			return err
		}
	}

	// The result line, the optional Docker-box suggestion, and the next actions are all printed
	// by the caller (cmdInit), which has the full picture (agents, languages, services) and
	// orders them as one block.
	return nil
}

// Validate and render before any scaffold or Git hook writes. Re-init follows the
// same prerequisite contract even when a customized Dockerfile already exists.
func initDockerfile(repo, stack string) (content string, detected bool, err error) {
	switch stack {
	case "":
		if _, err := os.Stat(filepath.Join(repo, ".tool-versions")); err != nil {
			return "", false, nil
		}
		detected = true
	case "asdf":
	default:
		return "", false, fmt.Errorf("unknown --stack %q: coop provisions toolchains from .tool-versions now\n"+
			"  pin versions there and run `coop init` (auto-detected), or `coop init --stack asdf`", stack)
	}
	if _, err := os.Stat(filepath.Join(repo, ".tool-versions")); err != nil {
		return "", false, fmt.Errorf("--stack asdf needs a .tool-versions in the repo\n" +
			"  e.g. `echo 'elixir 1.18.3-otp-27' > .tool-versions`, then re-run")
	}
	content, err = asdfDockerfile(toolVersions(repo))
	return content, detected, err
}

// CheckStack validates a --stack argument and its prerequisites without writing or printing
// anything, so the caller can refuse an unusable value BEFORE it names the project it is about to
// change or asks the user a single first-run question. Init re-checks it on the write path.
func CheckStack(repo, stack string) error {
	_, _, err := initDockerfile(repo, stack)
	return err
}

// Initialized reports whether repo already carries a coop scaffold. `coop init` uses it to stay
// quiet on a re-run: with the working set already in place there is nothing an interactive prompt
// could change (every write is no-clobber), so asking again is pure friction — and the first-run
// "next steps" are the wrong advice for a repo that's been building for weeks.
func Initialized(repo string) bool {
	for _, rel := range []string{"AGENTS.md", filepath.Join(".agent", "tasks")} {
		if _, err := os.Lstat(filepath.Join(repo, rel)); err != nil {
			return false
		}
	}
	return true
}

type scaffolder struct {
	repo string
	// kept counts artifacts already in place, changed records whether anything was actually
	// written. A re-init reports "kept N existing" as one line instead of N lines that all say
	// nothing happened — the noise that made a routine `coop init` look like it did work.
	kept    int
	changed bool
}

// keep records an artifact that was already correct. Nothing is printed per-file: Init prints the
// total, so the log carries only what CHANGED.
func (s *scaffolder) keep() { s.kept++ }

// skillsSource keeps an established shared source instead of creating a competing skill tree.
func skillsSource(repo string) string {
	agentSkills := filepath.Join(repo, ".agent", "skills")
	if info, err := os.Stat(agentSkills); err == nil && info.IsDir() {
		return agentSkills
	}
	claudeSkills := filepath.Join(repo, ".claude", "skills")
	if info, err := os.Lstat(claudeSkills); err == nil && info.IsDir() {
		return claudeSkills
	}
	return agentSkills
}

func (s *scaffolder) writeIfAbsent(dest, embedPath string, perm os.FileMode) error {
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
	created, err := writeNewRepoFile(s.repo, dest, data, perm, writeAndSync)
	if err != nil {
		return err
	}
	if !created {
		s.keep()
		return nil
	}
	s.changed = true
	return nil
}

type scaffoldFileWrite func(*os.File, []byte) error

func writeAndSync(file *os.File, data []byte) error {
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

// writeNewRepoFile is the one create-only boundary for scaffolded repository files. os.Root keeps
// parent traversal inside repo; O_EXCL and O_NOFOLLOW make the final entry no-clobber. Existing
// regular files are the supported re-init no-op, while links and unsupported entries are errors.
func writeNewRepoFile(repo, dest string, data []byte, perm os.FileMode, write scaffoldFileWrite) (bool, error) {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return false, fmt.Errorf("open scaffold root %s: %w", repo, err)
	}
	defer root.Close()
	rel, err := repoRelativePath(repo, dest)
	if err != nil {
		return false, err
	}
	if parent := filepath.Dir(rel); parent != "." {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return false, fmt.Errorf("create scaffold parent for %s: %w", dest, err)
		}
	}
	file, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
	if err != nil {
		if info, statErr := root.Lstat(rel); statErr == nil {
			if info.Mode().IsRegular() {
				return false, nil
			}
			return false, fmt.Errorf("refusing to replace scaffold path %s: existing entry is not a regular file", dest)
		}
		return false, fmt.Errorf("create scaffold file %s: %w", dest, err)
	}
	created, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		_ = root.Remove(rel)
		return false, fmt.Errorf("inspect new scaffold file %s: %w", dest, statErr)
	}
	removePartial := func() {
		if current, currentErr := root.Lstat(rel); currentErr == nil && os.SameFile(created, current) {
			_ = root.Remove(rel)
		}
	}
	if err := write(file, data); err != nil {
		_ = file.Close()
		removePartial()
		return false, fmt.Errorf("write scaffold file %s: %w", dest, err)
	}
	if err := file.Close(); err != nil {
		removePartial()
		return false, fmt.Errorf("close scaffold file %s: %w", dest, err)
	}
	return true, nil
}

func repoRelativePath(repo, dest string) (string, error) {
	repoAbs, err := filepath.Abs(repo)
	if err != nil {
		return "", fmt.Errorf("resolve scaffold root %s: %w", repo, err)
	}
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return "", fmt.Errorf("resolve scaffold path %s: %w", dest, err)
	}
	rel, err := filepath.Rel(repoAbs, destAbs)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("scaffold path %s is not a file inside %s", dest, repo)
	}
	return rel, nil
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
		s.keep()
	case os.IsNotExist(err), isLink:
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			return err
		}
		s.changed = true
	default:
		s.keep()
	}
	return nil
}

// linkSkillsIfAbsent also preserves a symlink to another existing project skills directory.
func (s *scaffolder) linkSkillsIfAbsent(target, link string) error {
	if info, err := os.Lstat(link); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if resolved, err := os.Stat(link); err == nil && resolved.IsDir() {
			s.keep()
			return nil
		}
	}
	return s.linkIfAbsent(target, link)
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
		// Stat the SKILL.md, not the directory: an empty leftover folder — a half-removed skill, a
		// `git clean` that took the ignored files but left the dir — would otherwise report "kept
		// existing skill" forever while the skill stayed empty and every .../skills symlink pointed
		// at nothing. copyEmbedDir restores only what's missing, so a customized file survives.
		if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err == nil {
			s.keep()
			continue
		}
		if err := s.copyEmbedDir("templates/skills/"+name, dest); err != nil {
			return err
		}
		s.changed = true
	}
	return nil
}

// installGitHooks generates the tracked git hooks and Claude's commit gate — the project-scoped
// copy when the repo keeps a .claude/ adapter, the .agent/claude/ fallback otherwise. A repo with
// no detected stack gets a neutral gate. A user's custom hooksPath or existing hook is never clobbered.
func (s *scaffolder) installGitHooks(langs []string, projectClaude bool) error {
	if err := s.writeContentIfAbsent(filepath.Join(s.repo, ".githooks", "pre-commit"), preCommitHook(langs), 0o755); err != nil {
		return err
	}
	preparePath := filepath.Join(s.repo, ".githooks", "prepare-commit-msg")
	prepareExists, prepareIsStock := false, false
	if info, err := os.Lstat(preparePath); err == nil {
		prepareExists = true
		if info.Mode().IsRegular() {
			data, readErr := os.ReadFile(preparePath)
			prepareIsStock = readErr == nil && string(data) == prepareCommitMsgChainHook && info.Mode()&0o100 != 0
		}
	}
	if prepareExists {
		// An existing prepare hook may deliberately be a project-owned symlink. It is the one
		// scaffold path whose documented composition contract preserves non-regular entries.
		s.keep()
	} else if err := s.writeContentIfAbsent(preparePath, prepareCommitMsgChainHook, 0o755); err != nil {
		return err
	}
	// One copy of the Claude commit gate, not two: the project artifact always wins over the
	// .agent/claude/ fallback (agent.claudeAgent.HomeFallbacks), so scaffolding both commits a
	// byte-identical script the box never reads.
	claudeGate := filepath.Join(s.repo, ".agent", "claude", "hooks", "commit-gate.sh")
	if projectClaude {
		claudeGate = filepath.Join(s.repo, ".claude", "hooks", "commit-gate.sh")
	}
	if err := s.writeContentIfAbsent(claudeGate, claudeCommitGate(langs), 0o755); err != nil {
		return err
	}
	if !gitRepo(s.repo) {
		// Nothing to point core.hooksPath at yet. The caller says so once, as the action that
		// finishes setup ('git init', then 'coop init'), rather than half-explaining it here.
		return nil
	}
	switch current := gitConfigGet(s.repo, "core.hooksPath"); current {
	case "", ".githooks":
		if err := gitConfigSet(s.repo, "core.hooksPath", ".githooks"); err != nil {
			return err
		}
		if current == "" {
			s.changed = true
		} else {
			s.keep()
		}
		// The two hook exceptions still speak: coop declined to take something over, so only the
		// user can finish the composition. Everything else it did is routine and stays silent.
		if prepareExists && !prepareIsStock {
			ui.Warn("kept your .githooks/prepare-commit-msg — chain $HOME/.coop-git-hooks/prepare-commit-msg from it for coop box attribution")
		}
	default:
		ui.Warn("kept your core.hooksPath=%q — copy or chain .githooks/pre-commit and .githooks/prepare-commit-msg there", current)
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

// The stanzas coop maintains in .gitignore, each in load-bearing order — git resolves a path by
// its LAST matching rule, so a line's position is part of its meaning.
//
// **/.agent/* ignores .agent/ state at any depth, so a monorepo member's working state (its
// tasks/backlog) is ignored too. Committed KNOWLEDGE — kb/ (descriptive cards plus the normative
// rules/ inside it), skills/presets and the loop.yaml
// config — is un-ignored at any depth as well, since a large monorepo member may carry its own;
// only project.yaml is TOP-LEVEL (the single subprojects+serve config), so its un-ignore stays
// root-anchored. tasks/ needs the three-step dance because git never descends into an excluded
// directory: re-include the dir, re-exclude its contents, then rescue the one committed doc.
var (
	coopIgnoreStanza = []string{
		"# coop working state (commit knowledge, ignore state)",
		"**/.agent/*",
		"!**/.agent/kb/",
		"!**/.agent/skills/",
		"!**/.agent/presets/",
		"!**/.agent/claude/",
		"!**/.agent/loop.yaml",
		"!**/.agent/compose.yml",
		"!**/.agent/Dockerfile",
		"!.agent/project.yaml",
		"# the queue is local state, but its layout doc is a BOOT entry point — commit just that",
		"!**/.agent/tasks/",
		"**/.agent/tasks/*",
		"!**/.agent/tasks/README.md",
	}
	presetSubagentIgnoreStanza = []string{
		"# preset native subagents coop generates in the box (coop-<role>) — never committed",
		".claude/agents/coop-*.md",
	}
	geminiIgnoreStanza = []string{
		"# .gemini may be globally ignored (local Gemini state); keep just the skills symlink",
		"!.gemini/",
		".gemini/*",
		"!.gemini/skills",
	}
)

func (s *scaffolder) updateGitignore(wantGemini bool) error {
	gi := filepath.Join(s.repo, ".gitignore")
	data, _ := os.ReadFile(gi) // missing file → empty; we create it below
	orig := string(data)
	var lines []string
	if orig != "" {
		lines = strings.Split(strings.TrimSuffix(orig, "\n"), "\n")
	}

	if !hasIgnoreLine(lines, "**/.agent/*") {
		lines = appendIgnoreStanza(lines, coopIgnoreStanza)
	} else {
		// Splice into an older stanza only what it lacks, each after the line it must follow. The
		// order here is the stanza's own, so an anchor is always in place before the rule that needs it.
		for _, up := range [][2]string{
			{"!**/.agent/kb/", "**/.agent/*"},
			{"!**/.agent/skills/", "!**/.agent/kb/"},
			{"!**/.agent/presets/", "!**/.agent/skills/"},
			{"!**/.agent/claude/", "!**/.agent/presets/"},
			{"!**/.agent/loop.yaml", "!**/.agent/claude/"},
			{"!**/.agent/compose.yml", "!**/.agent/loop.yaml"},
			{"!**/.agent/Dockerfile", "!**/.agent/compose.yml"},
			{"!.agent/project.yaml", "!**/.agent/Dockerfile"},
			{"!**/.agent/tasks/", "!.agent/project.yaml"},
			{"**/.agent/tasks/*", "!**/.agent/tasks/"},
			{"!**/.agent/tasks/README.md", "**/.agent/tasks/*"},
		} {
			lines = insertIgnoreLineAfter(lines, up[0], up[1])
		}
	}
	if !hasIgnoreLine(lines, ".claude/agents/coop-*.md") {
		lines = appendIgnoreStanza(lines, presetSubagentIgnoreStanza)
	}
	// Only a repo that keeps a .gemini/ needs the rules that rescue its skills symlink from a
	// global ignore; writing them into a repo with no .gemini/ is noise.
	if wantGemini && !hasIgnoreLine(lines, "!.gemini/skills") {
		lines = appendIgnoreStanza(lines, geminiIgnoreStanza)
	}

	out := strings.Join(lines, "\n") + "\n"
	if out == orig {
		return nil // already up to date
	}
	if err := os.WriteFile(gi, []byte(out), 0o644); err != nil {
		return err
	}
	return nil
}

func hasIgnoreLine(lines []string, want string) bool {
	return slices.ContainsFunc(lines, func(l string) bool { return strings.TrimSpace(l) == want })
}

func appendIgnoreStanza(lines, stanza []string) []string {
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
		lines = append(lines, "")
	}
	return append(lines, stanza...)
}

// insertIgnoreLineAfter puts line directly after anchor — unless line is already present anywhere
// (a re-init is a no-op) or anchor is missing (nothing to anchor an ordered rule to).
func insertIgnoreLineAfter(lines []string, line, anchor string) []string {
	if hasIgnoreLine(lines, line) {
		return lines
	}
	for i, l := range lines {
		if strings.TrimSpace(l) == anchor {
			return slices.Insert(lines, i+1, line)
		}
	}
	return lines
}

func (s *scaffolder) copyEmbedDir(src, dest string) error {
	return fs.WalkDir(templates, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if d.IsDir() {
			return mkdirs(s.repo, target)
		}
		data, err := templates.ReadFile(p)
		if err != nil {
			return err
		}
		_, err = writeNewRepoFile(s.repo, target, data, 0o644, writeAndSync)
		return err
	})
}

func mkdirs(repo string, paths ...string) error {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return fmt.Errorf("open scaffold root %s: %w", repo, err)
	}
	defer root.Close()
	for _, p := range paths {
		rel, err := repoRelativePath(repo, p)
		if err != nil {
			return err
		}
		if err := root.MkdirAll(rel, 0o755); err != nil {
			return fmt.Errorf("create scaffold directory %s: %w", p, err)
		}
	}
	return nil
}
