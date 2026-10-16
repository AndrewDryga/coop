package box

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// LabelService marks a per-run Compose service container (coop=service). With LabelHost it names the
// process whose exit ends the loop or session that started it, so a later coop can prove that run
// over and remove what it left (ReapOrphanServices).
const LabelService = "service"

// runServiceLabels are the labels coop's own override gives every service of a loop's or session's
// project, or nil for the development project `coop up` runs, which no sweep removes. The override
// is merged last, so a label the project's file plants is replaced: with an empty host, which no
// sweep acts on, when the supervisor has no provable identity.
func runServiceLabels(spec RunSpec, owner string) map[string]string {
	if owner == "" {
		return nil
	}
	return map[string]string{LabelKey: LabelService, LabelHost: serviceSupervisor(spec)}
}

// serviceSupervisor is the LabelHost value of the process whose exit ends a run's project, scoped to
// the workspace that names the project. A loop runs every box in its own process. An editor
// session's inner children come and go under one supervisor, their parent, while the session's
// project stays, so a child's label would let a sweep take a live session's services as soon as that
// child exits.
func serviceSupervisor(spec RunSpec) string {
	pid := os.Getpid()
	if spec.SupervisorID != "" {
		pid = os.Getppid()
	}
	return supervisorLabelValue(workspaceScope(spec.Repo), pid)
}

// serviceLabelsYAML is one service's labels block in a generated override.
func serviceLabelsYAML(labels map[string]string) string {
	var b strings.Builder
	b.WriteString("    labels:\n")
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		fmt.Fprintf(&b, "      %s: %s\n", strconv.Quote(key), strconv.Quote(labels[key]))
	}
	return b.String()
}

// writeServiceLabelOverride gives every service labels through an override file of coop's own.
func writeServiceLabelOverride(services []string, labels map[string]string, workspace string, exposedRoots ...string) (path string, cleanup func(), err error) {
	var b strings.Builder
	b.WriteString("services:\n")
	for _, service := range services {
		fmt.Fprintf(&b, "  %s:\n%s", strconv.Quote(service), serviceLabelsYAML(labels))
	}
	dir, err := privateWorkspaceTempDir(workspace, "coop-compose-", exposedRoots...)
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	path = filepath.Join(dir, "coop-compose-override-labels.yml")
	if err := os.WriteFile(path, []byte(b.String()), 0o400); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}
