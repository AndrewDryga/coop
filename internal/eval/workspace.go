package eval

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A trial runs in its OWN private workspace, never the developer's checkout. This file materializes
// one: it copies a fixture — an explicit local file tree — into a fresh directory and gives it a
// clean git repository with a single synthetic commit, so the candidate sees a normal repository
// with NO history, refs, reflogs or objects from wherever the fixture came from. v1 supports a
// plain-tree fixture only; a git-source fixture that must carry specific historical commit objects
// is outside the v1 profile (spec), so there is no history to leak in the first place. A fixture's
// own `.git` is never copied — the trial gets a repository Coop created, not one it inherited.
//
// This is the isolation floor for the candidate's WORKSPACE. The verifier is kept out of it at load
// (the suite's overlap/symlink checks); preparation (a later step) adds the credential, network and
// mount projections. Materialization writes only under dest and reads only the fixture and dest.

// materializeIdentity is the fixed, synthetic author of a trial's initial commit — never the host's
// git identity, so nothing about the developer travels into the trial.
const (
	materializeAuthorName  = "Coop Eval"
	materializeAuthorEmail = "eval@coop.invalid"
	materializeTimeout     = 2 * time.Minute
)

// PrepareWorkspace copies the fixture tree into dest (which must not already exist) and initializes a
// fresh git repository there with one synthetic commit of the whole tree. dest is the trial's
// private workspace root. The fixture is a directory of ordinary files; a `.git` entry at ANY level
// and of ANY type (dir, the worktree/submodule gitfile, or a symlink named `.git`, case-insensitive)
// is refused or skipped, so no source history, ref or object is carried in — and, critically, `git
// init`/`commit` can never adopt a source repository and write into the developer's own history. A
// top-level `.git` fails the whole fixture: a live checkout must be exported to a clean tree first,
// not silently stripped (its ambient ignored files — .env, caches, .claude/ — are not intended
// candidate input). `git add -A` honors a fixture's own `.gitignore`, so a file the fixture ignores
// stays on disk but out of the initial commit, exactly as it would in a real checkout. Returns the
// initial commit id.
func PrepareWorkspace(ctx context.Context, fixture, dest string) (string, error) {
	info, err := os.Stat(fixture)
	if err != nil {
		return "", fmt.Errorf("fixture %q: %w", fixture, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("fixture %q is not a directory", fixture)
	}
	// Resolve the fixture root through any symlinks so a symlinked root is walked (not read as one
	// empty entry) and so containment checks compare against the real path the kernel would.
	root, err := filepath.EvalSymlinks(fixture)
	if err != nil {
		return "", fmt.Errorf("resolve fixture %q: %w", fixture, err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
		return "", fmt.Errorf("fixture %q contains a .git — export a clean tree (git archive | tar -x) instead of a live checkout", fixture)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if _, err := os.Lstat(dest); err == nil {
		return "", fmt.Errorf("workspace %q already exists", dest)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := copyTree(root, dest); err != nil {
		return "", err
	}
	return initSyntheticRepo(ctx, dest)
}

// copyTree copies every regular file, directory and symlink under src into dst, preserving modes and
// symlink targets, but NEVER the source's own `.git` (a trial's repository is Coop's, not the
// fixture's). It refuses a symlink that points outside the fixture, so a fixture cannot smuggle in a
// path from the author's machine, and copies a symlink that stays inside as a symlink (real bytes,
// real mode — a fixture may legitimately ship one).
// root MUST be the symlink-resolved absolute fixture path, so every walked path and every rel is
// computed against what the kernel would resolve.
func copyTree(root, dst string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		// A `.git` of ANY type at ANY level never travels: a dir is a full repo, the worktree/
		// submodule `.git` FILE points `git` at a source repository (which init/commit would then
		// write into — the developer's own history), and the name is matched case-insensitively
		// because git itself resolves `.GIT` on a case-folding filesystem.
		if strings.EqualFold(d.Name(), ".git") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return copySymlink(root, path, target)
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		default:
			return fmt.Errorf("fixture entry %q is neither a regular file, directory nor symlink", rel)
		}
	})
}

// copySymlink reproduces a symlink only if it is RELATIVE and physically resolves (through every
// intermediate link, as the kernel would) to a path inside the fixture root. An absolute target is
// refused — it would point the trial back at the live source, not its own copy — and a `..` that
// escapes only after a link is followed is caught because EvalSymlinks resolves the real path, where
// a lexical join would not. A dangling link (EvalSymlinks errors) is refused too.
func copySymlink(root, path, target string) error {
	dest, err := os.Readlink(path)
	if err != nil {
		return err
	}
	if filepath.IsAbs(dest) {
		return fmt.Errorf("fixture symlink %q has an absolute target %q; use a relative link inside the fixture", path, dest)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("fixture symlink %q does not resolve inside the fixture: %w", path, err)
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("fixture symlink %q resolves outside the fixture; refusing to carry it into the trial", path)
	}
	return os.Symlink(dest, target)
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// initSyntheticRepo makes dest a git repository with one commit of its whole tree, under a fixed
// synthetic identity and with no host git config, so the trial's initial history is Coop's own and
// carries nothing of the developer. Returns the commit id.
func initSyntheticRepo(ctx context.Context, dest string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, materializeTimeout)
	defer cancel()
	// A hermetic HOME/XDG for every git call, removed after: so the FIXTURE cannot steer git through
	// a shipped `.config/git/ignore` or `attributes` (which GIT_CONFIG_GLOBAL does not suppress).
	home, err := os.MkdirTemp("", "coop-eval-git-home-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(home)
	steps := [][]string{
		// --template= empties the template dir, so no host template's hooks land in the trial's .git
		// (and none run on the host during commit).
		{"init", "--quiet", "--template="},
		{"add", "-A"},
		{"commit", "--quiet", "--allow-empty", "-m", "Initial eval fixture"},
	}
	for _, args := range steps {
		if out, err := runGit(ctx, dest, home, args...); err != nil {
			return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(out))
		}
	}
	head, err := runGit(ctx, dest, home, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return strings.TrimSpace(head), nil
}

// runGit runs one git command in dir with a hermetic environment: a fixed synthetic author, a fresh
// empty HOME/XDG (so nothing of the host — or the fixture — steers git through config, ignore or
// attributes files), no host git config, no prompts, no lazy fetch, no inherited GIT_* variables.
func runGit(ctx context.Context, dir, home string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_AUTHOR_NAME=" + materializeAuthorName,
		"GIT_AUTHOR_EMAIL=" + materializeAuthorEmail,
		"GIT_COMMITTER_NAME=" + materializeAuthorName,
		"GIT_COMMITTER_EMAIL=" + materializeAuthorEmail,
		// A fixed commit time keeps a materialized workspace reproducible run to run.
		"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2020-01-01T00:00:00Z",
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TasksRoot is where a Coop queue lives inside a repository. A loop scenario's queue template is
// materialized here, in the trial's own workspace — never in the developer's live queue, which is
// the whole reason a scenario ships a template instead of pointing at one.
const TasksRoot = ".agent/tasks"

// MaterializeQueue copies a case's queue template into dest's .agent/tasks. The template is an
// ordinary queue — state directories with task folders — so an author writes exactly what they
// would hand a real loop, and the loop under test discovers it the ordinary way.
//
// It is copied with the same rules as a fixture: no `.git` travels, and a symlink that escapes the
// template is refused rather than followed. An empty template is refused too — a loop scenario with
// no tasks measures nothing, and would look like a clean sweep when it finished immediately.
func MaterializeQueue(template, dest string) error {
	info, err := os.Stat(template)
	if err != nil {
		return fmt.Errorf("queue template %q: %w", template, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("queue template %q is not a directory", template)
	}
	root, err := filepath.EvalSymlinks(template)
	if err != nil {
		return fmt.Errorf("resolve queue template %q: %w", template, err)
	}
	// The queue must have work waiting in 00_todo. A template that is empty — or whose tasks are all
	// already done — makes the loop report "nothing actionable" and exit cleanly, which would then be
	// graded as a real result: a confident number from a scenario that never ran.
	todo := filepath.Join(root, "00_todo")
	if empty, err := hasNoEntries(todo); err != nil {
		return fmt.Errorf("queue template %q has no 00_todo directory; a loop scenario needs work waiting: %w", template, err)
	} else if empty {
		return fmt.Errorf("queue template %q has no tasks in 00_todo; a loop scenario with nothing to do measures nothing", template)
	}
	target := filepath.Join(dest, TasksRoot)
	// A fixture that ships its own queue would silently merge with the scenario's, and the loop
	// would work tasks the scenario never meant to include.
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("the fixture already contains %s; a scenario's queue must be the only one", TasksRoot)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return copyTree(root, target)
}

func hasNoEntries(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// PresetsRoot is where a repository keeps its presets. A loop scenario's preset is materialized
// here, inside the trial's own workspace.
const PresetsRoot = ".agent/presets"

// StagePreset copies a preset folder into a run-local staging directory, ONCE per run. Every trial
// then materializes from the staged copy, so all trials in a run — and both sides of a comparison —
// use byte-identical preset bytes even if the operator edits the real preset while the run is going.
// It is the execution counterpart of freezing the preset's fingerprint.
func StagePreset(presetDir, stageRoot, name string) (string, error) {
	dest := filepath.Join(stageRoot, name)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil // already staged for this run
	}
	root, err := filepath.EvalSymlinks(presetDir)
	if err != nil {
		return "", fmt.Errorf("resolve preset %q: %w", name, err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return "", err
	}
	if err := copyTree(root, dest); err != nil {
		return "", fmt.Errorf("stage preset %q: %w", name, err)
	}
	return dest, nil
}

// MaterializePreset places a staged preset into a trial workspace where `coop loop <name>` will find
// it. A preset normally lives in the operator's repository or their global directory; a trial's
// workspace is a fresh fixture that has neither, so without this the loop could not resolve the very
// thing the scenario exists to compare.
func MaterializePreset(staged, dest, name string) error {
	target := filepath.Join(dest, PresetsRoot, name)
	// A fixture shipping a preset of the same name would shadow the one under evaluation.
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("the fixture already contains %s/%s, which would shadow the preset being evaluated", PresetsRoot, name)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return copyTree(staged, target)
}
