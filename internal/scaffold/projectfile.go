package scaffold

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/taskstate"
)

// InitSubproject scaffolds the MINIMAL coop set for a monorepo member: just its own task queue.
// Members share the root's AGENTS.md, skills, rules, hooks, box — AND its single top-level
// .agent/project.yaml (members never get their own) — so they're pure task-queue holders. Each member
// still has its OWN tasks (per-component work) and backlog (the xx_backlog drawer, created on demand by
// `coop backlog add`); the root keeps a queue too, for changes that span members. Writes only what's absent.
// repo is the monorepo root and dir the member: progress is reported repo-relative, so a nested
// member reads as terraform/environments/va1/… and two members with the same basename stay
// distinct (rendering from the member's PARENT collapsed both to "va1/…").
func InitSubproject(repo, dir string) error {
	dirs := make([]string, 0, len(taskstate.All))
	for _, st := range taskstate.All {
		dirs = append(dirs, filepath.Join(dir, ".agent", "tasks", st))
	}
	if err := mkdirs(repo, dirs...); err != nil {
		return err
	}
	s := &scaffolder{repo: repo}
	return s.writeIfAbsent(filepath.Join(dir, ".agent", "tasks", "README.md"), "templates/agent/tasks/README.md", 0o644)
}

// DetectSubprojects returns the directories under repo that are themselves coop projects (they
// contain a .agent/ dir) — a monorepo's members. Paths are repo-relative and slash-separated
// ("terraform/environments/va1"), sorted; empty for a single project.
//
// The walk goes to ANY depth, because a member is not always a direct child: an infra repo nests
// its terraform roots (terraform/environments/va1), and requiring depth-1 meant those layouts had
// to hand-edit .agent/project.yaml forever. Three prunes keep it cheap and correct:
//   - hidden dirs (.git, .agent, .terraform, …) — never members, and the heavy ones live there
//   - the build/vendor dirs in subprojectSkipDirs, which can hold thousands of files
//   - a member's own subtree: once a directory is a member, its children are ITS business, so
//     nesting a member inside a member can't produce two overlapping queues for the same work
func DetectSubprojects(repo string) []string {
	var subs []string
	var walk func(dir, rel string)
	walk = func(dir, rel string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || subprojectSkipDirs[e.Name()] {
				continue
			}
			childRel := path.Join(rel, e.Name())
			child := filepath.Join(dir, e.Name())
			if fi, err := os.Stat(filepath.Join(child, ".agent")); err == nil && fi.IsDir() {
				subs = append(subs, childRel) // a member — do not descend into it
				continue
			}
			walk(child, childRel)
		}
	}
	walk(repo, "")
	sort.Strings(subs)
	return subs
}

// subprojectSkipDirs are directories the member walk never descends into: dependency and build
// output that can hold tens of thousands of files and never holds a coop project.
var subprojectSkipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "deps": true, "_build": true, "target": true,
	"build": true, "dist": true, "tmp": true, "coverage": true, "__pycache__": true,
}

// WriteProject writes <dir>/.agent/project.yaml if it's absent, reporting whether it wrote one. A
// non-empty subprojects list makes it a monorepo root listing them; empty writes a leaf template with
// commented serve/subprojects examples. It never clobbers an existing file (so re-running init keeps
// your edits — cmdInit notes any newly-detected members instead).
func WriteProject(dir string, subprojects []string) (bool, error) {
	dest := filepath.Join(dir, project.File)
	return writeNewRepoFile(dir, dest, []byte(projectYAML(subprojects)), 0o644, writeAndSync)
}

// RegisterSubprojects adds any detected member missing from an EXISTING project.yaml's
// subprojects: list, returning what it added. A repo grows members after its first init, and
// leaving them unlisted means coop silently ignores their queues — the old behaviour was to print
// "add these to subprojects:" and make you do it by hand, every init, forever.
//
// The edit is surgical text, not a YAML re-marshal: project.yaml is a commented template that
// documents every key, and round-tripping it through a YAML encoder would strip all of that.
// Missing file, or a subprojects: block coop can't confidently locate → returns nothing and
// changes nothing, so the caller's advisory stays the fallback.
func RegisterSubprojects(repo string, detected []string) ([]string, error) {
	return registerSubprojects(repo, detected, writeAndSync, nil)
}

var errProjectChanged = errors.New("project.yaml changed during subproject registration")

func registerSubprojects(repo string, detected []string, write scaffoldFileWrite, beforeReplace func() error) ([]string, error) {
	const maxAttempts = 3
	for attempt := 0; attempt < maxAttempts; attempt++ {
		added, err := registerSubprojectsOnce(repo, detected, write, beforeReplace)
		if !errors.Is(err, errProjectChanged) {
			return added, err
		}
	}
	return nil, fmt.Errorf("%w after %d attempts; retry coop init", errProjectChanged, maxAttempts)
}

func registerSubprojectsOnce(repo string, detected []string, write scaffoldFileWrite, beforeReplace func() error) ([]string, error) {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return nil, fmt.Errorf("open scaffold root %s: %w", repo, err)
	}
	defer root.Close()
	dest := filepath.Join(repo, project.File)
	before, present, err := readProjectFile(root, project.File, dest)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil // no project.yaml — WriteProject creates one with the members already in it
	}
	pj, err := project.Parse(before.data)
	if err != nil {
		return nil, err // malformed: don't compound it by editing
	}
	var missing []string
	for _, s := range detected {
		if !slices.Contains(pj.Subprojects, s) {
			missing = append(missing, s)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}

	lines := strings.Split(strings.TrimSuffix(string(before.data), "\n"), "\n")
	entries := make([]string, 0, len(missing))
	for _, s := range missing {
		entries = append(entries, "  - "+s)
	}

	// Case 1: a real `subprojects:` block — append after its last item, keeping the list sorted.
	if at := indexOfLine(lines, "subprojects:"); at >= 0 {
		end := at + 1
		for end < len(lines) && strings.HasPrefix(lines[end], "  - ") {
			end++
		}
		merged := append(append([]string{}, lines[at+1:end]...), entries...)
		sort.Strings(merged)
		lines = append(lines[:at+1], append(merged, lines[end:]...)...)
	} else if at := indexOfLine(lines, "# subprojects: [api, web]"); at >= 0 {
		// Case 2: the untouched placeholder from the scaffold — replace it with a real block.
		block := append([]string{"subprojects:"}, entries...)
		sort.Strings(block[1:])
		lines = append(lines[:at], append(block, lines[at+1:]...)...)
	} else {
		return nil, nil // hand-restructured file — leave it alone and let the caller advise
	}
	next := []byte(strings.Join(lines, "\n") + "\n")
	if err := replaceProjectFile(root, project.File, dest, before, next, write, beforeReplace); err != nil {
		return nil, err
	}
	return missing, nil
}

type projectFileSnapshot struct {
	info os.FileInfo
	data []byte
}

func readProjectFile(root *os.Root, rel, display string) (projectFileSnapshot, bool, error) {
	file, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if info, statErr := root.Lstat(rel); errors.Is(statErr, os.ErrNotExist) {
			return projectFileSnapshot{}, false, nil
		} else if statErr == nil && !info.Mode().IsRegular() {
			return projectFileSnapshot{}, false, fmt.Errorf("%s must be a regular file", display)
		}
		return projectFileSnapshot{}, false, fmt.Errorf("open %s: %w", display, err)
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return projectFileSnapshot{}, false, fmt.Errorf("inspect %s: %w", display, err)
	}
	if !before.Mode().IsRegular() {
		return projectFileSnapshot{}, false, fmt.Errorf("%s must be a regular file", display)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return projectFileSnapshot{}, false, fmt.Errorf("read %s: %w", display, err)
	}
	after, err := file.Stat()
	if err != nil {
		return projectFileSnapshot{}, false, fmt.Errorf("reinspect %s: %w", display, err)
	}
	named, err := root.Lstat(rel)
	if err != nil || !os.SameFile(before, after) || !os.SameFile(before, named) || int64(len(data)) != after.Size() {
		return projectFileSnapshot{}, false, errProjectChanged
	}
	return projectFileSnapshot{info: before, data: data}, true, nil
}

func replaceProjectFile(root *os.Root, rel, display string, before projectFileSnapshot, next []byte, write scaffoldFileWrite, beforeReplace func() error) error {
	tmp, err := stageProjectFile(root, rel, display, next, before.info.Mode().Perm(), write)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if beforeReplace != nil {
		if err := beforeReplace(); err != nil {
			return err
		}
	}
	current, present, err := readProjectFile(root, rel, display)
	if err != nil {
		return err
	}
	if !present || !os.SameFile(before.info, current.info) || !bytes.Equal(before.data, current.data) {
		return errProjectChanged
	}
	if err := root.Rename(tmp, rel); err != nil {
		return fmt.Errorf("replace %s: %w", display, err)
	}
	return nil
}

func stageProjectFile(root *os.Root, rel, display string, data []byte, perm os.FileMode, write scaffoldFileWrite) (string, error) {
	dir, base := filepath.Dir(rel), filepath.Base(rel)
	for attempt := 0; attempt < 100; attempt++ {
		name := filepath.Join(dir, fmt.Sprintf(".%s.coop-%d-%d.tmp", base, os.Getpid(), attempt))
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create temporary %s: %w", display, err)
		}
		created, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			_ = root.Remove(name)
			return "", fmt.Errorf("inspect temporary %s: %w", display, statErr)
		}
		removePartial := func() {
			if current, currentErr := root.Lstat(name); currentErr == nil && os.SameFile(created, current) {
				_ = root.Remove(name)
			}
		}
		if err := write(file, data); err != nil {
			_ = file.Close()
			removePartial()
			return "", fmt.Errorf("write temporary %s: %w", display, err)
		}
		if err := file.Close(); err != nil {
			removePartial()
			return "", fmt.Errorf("close temporary %s: %w", display, err)
		}
		return name, nil
	}
	return "", fmt.Errorf("create temporary %s: too many stale temporary files", display)
}

func indexOfLine(lines []string, want string) int {
	for i, l := range lines {
		if strings.TrimRight(l, " \t") == want {
			return i
		}
	}
	return -1
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
		b.WriteString("# subprojects: [api, web]\n")
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
#   dockerfile: <path>  # box image (default .agent/Dockerfile; or reuse a repo Dockerfile)
#   compose: <path>     # sidecars (default .agent/compose.yml; or point at your own)
#   env:                # committed, non-secret box defaults; agents/env wins; COOP_* is reserved
#     PGHOST: db
#     PGPORT: "5432"
#   egress: none        # "open" = npm + model API (default); "none" cuts ALL network — forensics only, agents can't work
#   auto_up: false      # auto-start the sidecar services (default true)
#   network: false      # join the sibling-services network (default true)
#   memory: 4g          # docker/podman resource caps (ignored on Apple container); default unset
#   cpus: "4"
#   pids: 2048          # the fork-bomb cap (default 4096; 0/unlimited turns it off)

# context: which committed docs 'coop context' compiles for a given scope. Canonical
# AGENTS.md/CLAUDE.md are always included; each route adds its docs when a touched path matches
# one of its globs (* within a segment, ** across segments).
# context:
#   routes:
#     - paths: [billing/**, "**/*.sql"]
#       include: [.agent/kb/billing.md]

# gate: the revalidation 'coop fork merge' runs IN THE BOX before landing a fork (rolled back on
# failure). Same shape as COOP_GATE; an explicit COOP_GATE wins. Use the gate AGENTS.md names.
# gate: <this repo's gate command>
`)
	return b.String()
}
