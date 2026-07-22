package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/contextc"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/ui"
)

// cmdContext compiles the committed docs relevant to a deterministic scope: canonical
// AGENTS.md/CLAUDE.md (always) plus the context.routes whose globs match a touched path. Scope is
// gathered from explicit paths, --changed (git), --task <id> (a task's declared paths), and the
// current subproject — never inferred from a free-form prompt.
//
//	coop context [--changed] [--task <id>] [--json | --rendered] [paths...]
func (a *app) cmdContext(args []string) (int, error) {
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
			if i+1 >= len(args) {
				return 2, errors.New("coop context: --task needs a task id")
			}
			taskID, i = args[i+1], i+1
		case strings.HasPrefix(arg, "--task="):
			taskID = strings.TrimPrefix(arg, "--task=")
		case strings.HasPrefix(arg, "-") && arg != "-":
			return 2, fmt.Errorf("coop context: unknown flag %q (supported: --changed, --task <id>, --json, --rendered)", arg)
		default:
			paths = append(paths, arg)
		}
	}
	if asJSON && rendered {
		return 2, errors.New("coop context: choose --json or --rendered, not both")
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
	scope, err := a.contextScope(repo, p, paths, changed, taskID)
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
func (a *app) contextScope(repo string, p *project.Project, paths []string, changed bool, taskID string) ([]string, error) {
	var scope []string
	seen := map[string]bool{}
	add := func(rel string) {
		rel = filepath.ToSlash(strings.TrimSpace(rel))
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
	if changed {
		for _, c := range gitChangedPaths(repo) {
			add(c)
		}
	}
	if taskID != "" {
		tps, err := a.taskScopePaths(repo, taskID)
		if err != nil {
			return nil, err
		}
		for _, tp := range tps {
			add(tp)
		}
	}
	return scope, nil
}

// gitChangedPaths returns the repo-relative paths git reports as changed (staged, unstaged, or
// untracked). Best-effort: a non-repo or git error yields none, not a failure.
func gitChangedPaths(repo string) []string {
	out, err := exec.Command("git", "-C", repo, "status", "--porcelain").Output()
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		pth := strings.TrimSpace(line[3:])
		if i := strings.Index(pth, " -> "); i >= 0 { // a rename: take the new path
			pth = pth[i+4:]
		}
		paths = append(paths, filepath.ToSlash(strings.Trim(pth, `"`)))
	}
	return paths
}

// taskScopePaths reads a task's declared scope: a `paths:` frontmatter field in its task.md, split
// on commas/whitespace. A task without one contributes nothing.
func (a *app) taskScopePaths(repo, id string) ([]string, error) {
	rels, err := taskQueues(a.cfg, repo, nil)
	if err != nil {
		return nil, err
	}
	var items []taskItem
	for _, rel := range rels {
		items = append(items, readTaskTree(filepath.Join(repo, rel))...)
	}
	t, err := matchTask(items, id, "coop tasks")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(t.Dir, "task.md"))
	if err != nil {
		return nil, fmt.Errorf("coop context: reading task %s: %w", t.ID, err)
	}
	fields, _ := splitFrontmatter(string(data))
	return strings.FieldsFunc(fields["paths"], func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }), nil
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
