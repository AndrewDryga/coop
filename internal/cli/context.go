package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/contextc"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// cmdContext compiles the committed docs relevant to a deterministic scope: canonical
// AGENTS.md/CLAUDE.md (always) plus the context.routes whose globs match a touched path. Scope is
// gathered from explicit paths, --changed (git), --task <id> (a task's declared paths), and the
// current subproject — never inferred from a free-form prompt.
//
//	coop context [--changed] [--task <id> [--tasks <path>...]] [--json | --rendered] [<path>...]
func (a *app) cmdContext(args []string) (int, error) {
	// Validate original token boundaries before queue-flag extraction can turn
	// `--task --tasks queue id` into the different, apparently valid `--task id`.
	for i := 0; i < len(args); i++ {
		if v, n, ok, err := flagValue(args, i, "--task"); ok {
			if err != nil || v == "" || strings.HasPrefix(v, "-") {
				return 2, errors.New("coop context: --task needs a task id")
			}
			i += n - 1
		}
	}
	queueFlags, args, err := tasks.ExtractTasksFlags(args)
	if err != nil {
		return 2, err
	}
	for _, path := range queueFlags {
		if path == "" {
			return 2, errors.New("coop context: --tasks needs a queue path")
		}
	}
	var changed, asJSON, rendered bool
	var taskID string
	var paths []string
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--changed":
			changed = true
		case arg == "--json":
			asJSON = true
		case arg == "--rendered":
			rendered = true
		case arg == "--task":
			taskID, i = args[i+1], i+1
		case strings.HasPrefix(arg, "--task="):
			taskID = strings.TrimPrefix(arg, "--task=")
		case strings.HasPrefix(arg, "-") && arg != "-":
			return 2, fmt.Errorf("coop context: unknown flag %q (supported: --changed, --task <id>, --tasks <path>, --json, --rendered)", arg)
		default:
			paths = append(paths, arg)
		}
	}
	if asJSON && rendered {
		return 2, errors.New("coop context: choose --json or --rendered, not both")
	}
	if len(queueFlags) > 0 && taskID == "" {
		return 2, errors.New("coop context: --tasks requires --task <id>")
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	// Config comes from the resolved repo's committed project.yaml — which a fork carries too (it's
	// committed), so a fork gets the parent's routes while its scope (below) comes from its own tree.
	p, err := project.Load(repo)
	if err != nil {
		return 2, err
	}
	scope, err := a.contextScope(repo, p, paths, changed, taskID, queueFlags)
	if err != nil {
		return 2, err
	}
	sel, err := contextc.Compile(repo, p.Context.Routes, scope)
	if err != nil {
		return 1, err
	}
	switch {
	case asJSON:
		return contextJSON(scope, sel)
	case rendered:
		return contextRendered(repo, sel)
	default:
		contextReport(scope, sel)
		return 0, nil
	}
}

// contextScope gathers the deterministic touched-path set, repo-relative and deduped: the current
// subproject (from cwd), any explicit paths, --changed git paths, and a task's declared paths.
func (a *app) contextScope(repo string, p *project.Project, paths []string, changed bool, taskID string, queueFlags []string) ([]string, error) {
	var scope []string
	seen := map[string]bool{}
	add := func(rel string) {
		rel = filepath.ToSlash(rel)
		if rel == "" || rel == "." || seen[rel] {
			return
		}
		seen[rel] = true
		scope = append(scope, rel)
	}
	cwd, _ := os.Getwd()
	// Current subproject: if cwd sits inside a declared subproject, that subproject dir is in scope.
	if rel, err := filepath.Rel(repo, cwd); err == nil && !strings.HasPrefix(rel, "..") {
		for _, sub := range p.Subprojects {
			if rel == sub || strings.HasPrefix(rel, sub+string(filepath.Separator)) {
				add(sub)
			}
		}
	}
	// Explicit paths are repo-relative (that's what --changed/--task emit too). Absolute paths are
	// rejected, and nothing may escape the repo.
	for _, arg := range paths {
		if filepath.IsAbs(arg) {
			return nil, fmt.Errorf("coop context: %q is absolute — pass a repo-relative path", arg)
		}
		clean := filepath.Clean(arg)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("coop context: %q is outside the repo", arg)
		}
		add(clean)
	}
	// Resolve identity and metadata before optional Git work, but retain the
	// original append order: scope order also determines route explanations.
	var taskPaths []string
	if taskID != "" {
		var err error
		taskPaths, err = a.taskScopePaths(repo, taskID, queueFlags)
		if err != nil {
			return nil, err
		}
	}
	if changed {
		changedPaths, err := gitChangedPaths(repo)
		if err != nil {
			return nil, err
		}
		for _, c := range changedPaths {
			add(c)
		}
	}
	for _, tp := range taskPaths {
		add(tp)
	}
	return scope, nil
}

// gitChangedPaths returns the repo-relative paths git reports as changed (staged, unstaged, or
// untracked). A requested --changed scope must not look empty when Git actually failed.
func gitChangedPaths(repo string) ([]string, error) {
	args := append([]string{"-C", repo}, forkspace.GitHardening...)
	args = append(args, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if detail := strings.TrimSpace(string(exitErr.Stderr)); detail != "" {
				return nil, fmt.Errorf("git status: %w: %s", err, detail)
			}
		}
		return nil, fmt.Errorf("git status: %w", err)
	}
	return parseGitStatusPaths(out)
}

func parseGitStatusPaths(out []byte) ([]string, error) {
	if len(out) == 0 {
		return nil, nil
	}
	if out[len(out)-1] != 0 {
		return nil, errors.New("git status returned a malformed non-NUL-terminated record")
	}
	records := bytes.Split(out[:len(out)-1], []byte{0})
	var paths []string
	for i := 0; i < len(records); i++ {
		record := records[i]
		if len(record) < 4 || record[2] != ' ' {
			return nil, errors.New("git status returned a malformed porcelain record")
		}
		paths = append(paths, filepath.ToSlash(string(record[3:])))
		if record[0] == 'R' || record[0] == 'C' || record[1] == 'R' || record[1] == 'C' {
			i++ // -z emits the source path as the next bare record; scope keeps the target path.
			if i >= len(records) || len(records[i]) == 0 {
				return nil, errors.New("git status returned a rename without its source path")
			}
		}
	}
	return paths, nil
}

// taskScopePaths reads a task's declared scope: a `paths:` frontmatter list in its task.md — a YAML
// flow list, a block list, or a bare space/comma-separated scalar. A task without one contributes
// nothing.
func (a *app) taskScopePaths(repo, id string, queueFlags []string) ([]string, error) {
	rels, err := tasks.TaskQueues(a.cfg, repo, queueFlags)
	if err != nil {
		return nil, err
	}
	t, err := tasks.FindTaskAcrossQueues(repo, rels, id)
	if err != nil {
		return nil, err
	}
	metadata, err := tasks.OpenTaskMetadataRoot(t.Dir)
	if err != nil {
		return nil, fmt.Errorf("coop context: reading task %s: %w", t.ID, err)
	}
	defer metadata.Close()
	data, err := tasks.ReadTaskMetadataFile(metadata, "task.md")
	if err != nil {
		return nil, fmt.Errorf("coop context: reading task %s: %w", t.ID, err)
	}
	return tasks.FrontmatterList(string(data), "paths"), nil
}

func contextReport(scope []string, sel []contextc.Selected) {
	p := ui.For(os.Stdout)
	if len(scope) == 0 {
		fmt.Println(p.Dim("scope: (none) — canonical instructions only"))
	} else {
		fmt.Println(p.Dim("scope: " + strings.Join(scope, ", ")))
	}
	w := 0
	for _, s := range sel {
		if len(s.File) > w {
			w = len(s.File)
		}
	}
	for _, s := range sel {
		fmt.Printf("  %s  %s\n", padRight(s.File, w), p.Faint(s.Reason))
	}
	fmt.Printf("\n  %s\n", p.Dim(fmt.Sprintf("%d file(s)", len(sel))))
}

func contextJSON(scope []string, sel []contextc.Selected) (int, error) {
	if scope == nil {
		scope = []string{}
	}
	if sel == nil {
		sel = []contextc.Selected{}
	}
	b, err := json.MarshalIndent(map[string]any{"scope": scope, "files": sel}, "", "  ")
	if err != nil {
		return 1, err
	}
	fmt.Println(string(b))
	return 0, nil
}

// contextRendered prints the compiled context itself: each file's content under a header, in
// selection order — the text an agent would read, canonical instructions first and whole.
func contextRendered(repo string, sel []contextc.Selected) (int, error) {
	for _, s := range sel {
		data, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(s.File)))
		if err != nil {
			return 1, fmt.Errorf("coop context: reading %s: %w", s.File, err)
		}
		fmt.Printf("===== %s =====\n%s\n\n", s.File, strings.TrimRight(string(data), "\n"))
	}
	return 0, nil
}
