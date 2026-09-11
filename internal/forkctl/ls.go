package forkctl

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

func (c *Control) ForkLs(args []string) (int, error) {
	asJSON := false
	rest := make([]string, 0, len(args))
	for _, x := range args {
		if x == "--json" {
			asJSON = true
			continue
		}
		rest = append(rest, x)
	}
	if err := rejectArgs("fork ls", rest); err != nil {
		return 2, err // a stray token should fail, not be silently ignored
	}
	repo, err := box.ResolveRepo(c.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if asJSON {
		return c.forkLsJSON(repo)
	}
	projectSnapshot := tasks.ReadProjectSnapshot(repo, nil)
	nameSet := map[string]bool{}
	lifecycleNames, err := forkspace.LifecycleNames(repo)
	if err != nil {
		return -1, err
	}
	for _, name := range lifecycleNames {
		nameSet[name] = true
	}
	for _, fork := range projectSnapshot.Forks {
		nameSet[fork.Name] = true
	}
	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		listing("No forks yet.")
		listing("")
		listing("  Create one: coop fork login claude")
		return 0, nil
	}
	statuses := make([]forkStatus, 0, len(names))
	for _, name := range names {
		statuses = append(statuses, c.gatherForkStatus(repo, name))
	}
	// One compact block per fork, not an eight-column table: a table truncates names, hides what
	// its symbols mean, and pads every fork to the widest one. Each row is a labeled fact, and a
	// fact nobody recorded is left out rather than shown as a dash.
	listing("Forks")
	for _, s := range statuses {
		listing("")
		heading := "  " + s.Name + " · " + stateWords(s.stateCell())
		if s.Updated != "" {
			heading += " · updated " + s.Updated
		}
		listing("%s", heading)
		if s.Agent != "" && s.Agent != "?" {
			listing("    Agent: %s", s.Agent)
		}
		if s.Branch != "" {
			listing("    Branch: %s", s.Branch)
		}
		if tasksLine := s.tasksLine(); tasksLine != "" {
			listing("    Tasks: %s", tasksLine)
		}
		if s.Ins != 0 || s.Del != 0 || s.Dirty {
			listing("    Changes: %s", s.changesLine())
		}
		if cost := s.costLine(); cost != "" {
			listing("    Reported cost: %s", cost)
		}
	}
	// A fork whose name is (or became) a reserved verb is unreachable by `coop fork <name>` — that
	// spelling runs the subcommand. forkspace.ValidName now refuses such names, so this only catches
	// forks made before that guard; point at the escape hatch (path/rm still take it as an explicit
	// arg).
	for _, n := range names {
		if forkspace.Reserved(n) {
			forkProblem(n, "Its name is also a fork command, so coop fork "+n+" runs that command.", "Its folder: coop fork path "+n)
		}
	}
	seenProblems := map[string]bool{}
	for _, status := range statuses {
		for _, problem := range status.Problems {
			if seenProblems[status.Name+problem] {
				continue
			}
			seenProblems[status.Name+problem] = true
			forkProblem(status.Name, problem, "")
		}
	}
	for _, problem := range projectSnapshot.Problems {
		if !seenProblems[problem] {
			seenProblems[problem] = true
			ui.Note("")
			ui.Note("  %s", ui.Yellow("⚠ Could not read this project's fork activity"))
			ui.Note("")
			ui.Note("        %s", problem)
		}
	}
	listing("")
	listing("Review a fork: coop fork review <name>")
	return 0, nil
}

// listing writes one line of `coop fork ls`'s RESULT to stdout — the listing is the answer the
// command was run for, so it stays pipeable (`coop fork ls | grep`), exactly like the --json form
// and every other listing. Warnings and problems keep ui's stderr, where exceptions belong.
func listing(format string, a ...any) {
	fmt.Fprintf(os.Stdout, format+"\n", a...)
}

// forkProblem reports why ONE fork's state could not be established, nested under the listing it
// belongs to: the headline sits at the forks' own indent and its cause six spaces further in, the
// same relationship a top-level block has.
func forkProblem(name, cause, action string) {
	ui.Note("")
	ui.Note("  %s", ui.Yellow("⚠ Could not check fork "+name))
	ui.Note("")
	ui.Note("        %s", cause)
	if action != "" {
		ui.Note("")
		ui.Note("    %s", action)
	}
}

// forkLsJSON prints the repo's workspaces (root first, then forks) as JSON, each with its path and
// per-port serve URLs — machine-readable discovery for host tooling (screenshots, config
// generation) so it never reproduces coop's host-port hash. Each URL is keyed on the WORKSPACE
// path, so a fork's URLs are its own — matching what that fork's box publishes.
func (c *Control) forkLsJSON(repo string) (int, error) {
	p, err := project.Load(repo)
	if err != nil {
		return -1, err
	}
	// Sidecar URLs need the runtime for `docker compose config`; detect it best-effort (fork ls is
	// otherwise pure-local, so no runtime → serve URLs still list, service URLs just don't).
	_ = c.ensureRuntime()
	serveURLs := func(ws string) map[string]string {
		if len(p.Serve.Ports) == 0 {
			return nil
		}
		m := make(map[string]string, len(p.Serve.Ports))
		for _, port := range p.Serve.Ports {
			m[strconv.Itoa(port)] = fmt.Sprintf("http://localhost:%d", project.HostPort(ws, port))
		}
		return m
	}
	// Sidecar URLs need the compose config; skip the docker call for workspaces without a compose
	// file, and stay best-effort (no docker / parse error → omitted, never an error).
	svcURLs := func(ws string) map[string]string {
		cf := box.ComposeFileAt(ws, p.ComposeRel())
		if cf == "" {
			return nil
		}
		m := map[string]string{}
		for _, sp := range box.ServicePorts(c.rt, ws, cf, append(box.ConfigExposureRoots(c.cfg), repo)...) {
			m[fmt.Sprintf("%s:%d", sp.Service, sp.ContainerPort)] = fmt.Sprintf("%s://localhost:%d", sp.Scheme, sp.HostPort)
		}
		if len(m) == 0 {
			return nil
		}
		return m
	}
	type forkJSONStatus struct {
		State               string           `json:"state"`
		Tasks               tasks.TaskCounts `json:"tasks"`
		ActiveSandboxes     int              `json:"active_sandboxes"`
		ParkedSandboxes     int              `json:"parked_sandboxes"`
		CleanupSandboxes    int              `json:"cleanup_sandboxes"`
		UnverifiedSandboxes int              `json:"unverified_sandboxes"`
		DetachedRunning     bool             `json:"detached_running"`
		WorkspaceReserved   bool             `json:"workspace_reserved"`
		Candidate           bool             `json:"candidate"`
		Landing             bool             `json:"landing"`
		CleanupPending      bool             `json:"cleanup_pending"`
		Legacy              bool             `json:"legacy_generation"`
		Problems            []string         `json:"problems,omitempty"`
	}
	type workspace struct {
		Name     string            `json:"name"`
		Path     string            `json:"path"`
		Serve    map[string]string `json:"serve,omitempty"`
		Services map[string]string `json:"services,omitempty"`
		Status   *forkJSONStatus   `json:"status,omitempty"`
	}
	out := []workspace{{Name: "root", Path: repo, Serve: serveURLs(repo), Services: svcURLs(repo)}}
	var problems []string
	projectSnapshot := tasks.ReadProjectSnapshot(repo, nil)
	nameSet := map[string]bool{}
	lifecycleNames, err := forkspace.LifecycleNames(repo)
	if err != nil {
		return -1, err
	}
	for _, name := range lifecycleNames {
		nameSet[name] = true
	}
	for _, fork := range projectSnapshot.Forks {
		nameSet[fork.Name] = true
	}
	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, n := range names {
		ws := forkspace.Workspace(repo, n)
		s := c.gatherForkStatus(repo, n)
		status := &forkJSONStatus{
			State: s.stateCell(), Tasks: s.Counts, ActiveSandboxes: s.Active,
			ParkedSandboxes: s.Parked, CleanupSandboxes: s.CleanupSandboxes,
			UnverifiedSandboxes: s.UnverifiedSandboxes,
			DetachedRunning:     s.Running, WorkspaceReserved: s.Reserved, Candidate: s.Ready,
			Landing: s.Landing, CleanupPending: s.Cleanup, Legacy: s.Legacy, Problems: s.Problems,
		}
		out = append(out, workspace{Name: n, Path: ws, Serve: serveURLs(ws), Services: svcURLs(ws), Status: status})
		for _, problem := range s.Problems {
			problems = append(problems, "fork "+n+": "+problem)
		}
	}
	problems = append(problems, projectSnapshot.Problems...)
	payload := map[string]any{"workspaces": out}
	if len(problems) > 0 {
		payload["problems"] = problems
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return 1, err
	}
	fmt.Println(string(b))
	return 0, nil
}

// forkBranch / forkUpdated read a fork's state for `coop fork ls`.
// They run against an agent-controlled tree (post-work), so they use the hardened
// helpers — `diff`/`log` would otherwise fire a planted core.fsmonitor or diff.external.
func forkBranch(ws string) string { return gitOut(ws, "rev-parse", "--abbrev-ref", "HEAD") }

func forkUpdated(repo, ws string) string {
	// Show the fork's OWN latest commit. A fresh fork has none, so `git log -1` would report the base
	// commit it inherited from the clone — misreading a seconds-old fork as hours/days stale (and a
	// truly idle fork as fresh). When there are no commits beyond the base, fall back to the clone's
	// own age instead of the inherited time.
	if base := gitOut(repo, "rev-parse", "HEAD"); base != "" {
		if n := gitOut(ws, "rev-list", "--count", base+"..HEAD"); n != "" && n != "0" {
			if rel := gitOut(ws, "log", "-1", "--format=%cr"); rel != "" {
				return rel
			}
		}
	}
	if fi, err := os.Stat(ws); err == nil {
		return relativeAge(fi.ModTime())
	}
	return "—"
}

// relativeAge renders how long ago t was in git's `%cr` idiom, for timestamps that aren't git commits
// (a fork's clone time). Coarse buckets — it labels staleness, not exact durations.
func relativeAge(t time.Time) string {
	switch d := time.Since(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return ui.Count(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return ui.Count(int(d.Hours()), "hour") + " ago"
	default:
		return ui.Count(int(d.Hours()/24), "day") + " ago"
	}
}
