package scaffold

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/taskstate"
)

// InitSubproject scaffolds the MINIMAL coop set for a monorepo member: just its own task queue.
// Members share the root's AGENTS.md, skills, rules, hooks, box — AND its single top-level
// .agent/project.yaml (members never get their own) — so they're pure task-queue holders. Each member
// still has its OWN tasks (per-component work) and backlog (the xx_backlog drawer, created on demand by
// `coop backlog add`); the root keeps a queue too, for changes that span members. Writes only what's absent.
func InitSubproject(dir string) error {
	dirs := make([]string, 0, len(taskstate.All))
	for _, st := range taskstate.All {
		dirs = append(dirs, filepath.Join(dir, ".agent", "tasks", st))
	}
	if err := mkdirs(dirs...); err != nil {
		return err
	}
	s := &scaffolder{repo: dir}
	return s.writeIfAbsent(filepath.Join(dir, ".agent", "tasks", "README.md"), "templates/agent/tasks/README.md", 0o644)
}

// DetectSubprojects returns repo's direct child directories that are themselves coop projects (they
// contain a .agent/ dir) — a monorepo's members. Sorted; empty for a single project. Hidden dirs
// (.git, .agent, …) are skipped, and only depth-1 children are considered (deeper layouts are a
// hand-edit of .agent/project.yaml).
func DetectSubprojects(repo string) []string {
	entries, err := os.ReadDir(repo)
	if err != nil {
		return nil
	}
	var subs []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if fi, err := os.Stat(filepath.Join(repo, e.Name(), ".agent")); err == nil && fi.IsDir() {
			subs = append(subs, e.Name())
		}
	}
	sort.Strings(subs)
	return subs
}

// WriteProject writes <dir>/.agent/project.yaml if it's absent, reporting whether it wrote one. A
// non-empty subprojects list makes it a monorepo root listing them; empty writes a leaf template with
// commented serve/subprojects examples. It never clobbers an existing file (so re-running init keeps
// your edits — cmdInit notes any newly-detected members instead).
func WriteProject(dir string, subprojects []string) (bool, error) {
	dest := filepath.Join(dir, project.File)
	if _, err := os.Stat(dest); err == nil {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(dest, []byte(projectYAML(subprojects)), 0o644)
}

func projectYAML(subprojects []string) string {
	var b strings.Builder
	b.WriteString("# coop project config — committed with the repo (unlike the rest of .agent/).\n\n")
	if len(subprojects) > 0 {
		b.WriteString("# Monorepo members: coop aggregates each one's .agent/tasks queue automatically,\n")
		b.WriteString("# so you never hand-maintain COOP_TASKS.\n")
		b.WriteString("subprojects:\n")
		for _, s := range subprojects {
			b.WriteString("  - " + s + "\n")
		}
	} else {
		b.WriteString("# A monorepo? List member dirs (each its own coop project with a .agent/):\n")
		b.WriteString("# subprojects: [runner, packs]\n")
	}
	b.WriteString("\n# Ports a dev server in the box listens on — coop publishes each to a stable host\n")
	b.WriteString("# port so you can open it in your browser (bind the server to 0.0.0.0 in the box):\n")
	b.WriteString("# serve:\n")
	b.WriteString("#   ports: [5173]\n")
	b.WriteString(`
# box: the posture every run in this repo inherits. An explicit COOP_* env/conf setting still
# wins for a one-off, so a committed value can only TIGHTEN the default (egress's default is the
# loosest — "open" — so a repo can pin none but never widen your explicit none).
# box:
#   egress: none        # outbound network: "open" (npm + model API) or "none" (offline)
#   auto_up: false      # auto-start .agent/compose.yml sibling services (default true)
#   network: false      # join the sibling-services network (default true)
#   memory: 4g          # docker/podman resource caps (ignored on Apple container); default unset
#   cpus: "4"
#   pids: 2048          # the fork-bomb cap (default 4096; 0/unlimited turns it off)

# gate: the revalidation 'coop fork merge' runs IN THE BOX before landing a fork (rolled back on
# failure). Same shape as COOP_GATE; an explicit COOP_GATE wins.
# gate: make check
`)
	return b.String()
}
