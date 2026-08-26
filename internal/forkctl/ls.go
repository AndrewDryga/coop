package forkctl

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
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
	names := forkspace.LifecycleNames(repo)
	if len(names) == 0 {
		ui.Note("no forks yet — open one with 'coop fork <name>'")
		return 0, nil
	}
	// Size the NAME column to the longest fork name (clamped). Rune-pad EVERY cell (padRight) rather
	// than %-Ns: a glyph like ⚠/⚑ in TASKS/CHANGES (or a "…" in a truncated name) is multi-byte, so
	// %-Ns would count bytes, short-pad, and shove later columns out from under their headers.
	nw := colWidth(names, len("NAME"), 24)
	const format = "  %s %s %s %s %s %s %s %s\n"
	// Bold the whole rendered line, not each cell: bolding a cell first would put ANSI
	// escape bytes inside the width count and misalign the header against the rows.
	fmt.Print(ui.For(os.Stdout).Bold(fmt.Sprintf(format, padRight("NAME", nw), padRight("AGENT", 8), padRight("BRANCH", 12), padRight("STATE", 9), padRight("TASKS", 8), padRight("CHANGES", 15), padRight("COST", 8), "UPDATED")))
	for _, n := range names {
		s := c.gatherForkStatus(repo, n)
		fmt.Printf(format, padRight(truncate(s.Name, nw), nw), padRight(s.Agent, 8), padRight(s.Branch, 12), padRight(s.stateCell(), 9), padRight(s.tasksCell(), 8), padRight(s.changesCell(), 15), padRight(s.costCell(), 8), s.Updated)
	}
	// A fork whose name is (or became) a reserved verb is unreachable by `coop fork <name>` — that
	// spelling runs the subcommand. forkspace.ValidName now refuses such names, so this only catches
	// forks made before that guard; point at the escape hatch (path/rm still take it as an explicit
	// arg).
	for _, n := range names {
		if forkspace.Reserved(n) {
			ui.Warn("fork %q shadows the '%s' subcommand — reach it via 'coop fork path %s' or 'coop fork rm %s'", n, n, n, n)
		}
	}
	return 0, nil
}

// forkLsJSON prints the repo's workspaces (root first, then forks) as JSON, each with its path and
// per-port serve URLs — machine-readable discovery for host tooling (screenshots, config
// generation) so it never reproduces coop's host-port hash. Each URL is keyed on the WORKSPACE
// path, so a fork's URLs are its own — matching what that fork's box publishes.
func (c *Control) forkLsJSON(repo string) (int, error) {
	// Sidecar URLs need the runtime for `docker compose config`; detect it best-effort (fork ls is
	// otherwise pure-local, so no runtime → serve URLs still list, service URLs just don't).
	_ = c.ensureRuntime()
	p, _ := project.Load(repo) // serve.ports config, best-effort (a broken project.yaml → no URLs)
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
		cf := box.ComposeFile(ws, repo)
		if cf == "" {
			return nil
		}
		m := map[string]string{}
		for _, sp := range box.ServicePorts(c.rt, ws, cf) {
			m[fmt.Sprintf("%s:%d", sp.Service, sp.ContainerPort)] = fmt.Sprintf("%s://localhost:%d", sp.Scheme, sp.HostPort)
		}
		if len(m) == 0 {
			return nil
		}
		return m
	}
	type workspace struct {
		Name     string            `json:"name"`
		Path     string            `json:"path"`
		Serve    map[string]string `json:"serve,omitempty"`
		Services map[string]string `json:"services,omitempty"`
	}
	out := []workspace{{Name: "root", Path: repo, Serve: serveURLs(repo), Services: svcURLs(repo)}}
	for _, n := range forkspace.LifecycleNames(repo) {
		ws := forkspace.Workspace(repo, n)
		out = append(out, workspace{Name: n, Path: ws, Serve: serveURLs(ws), Services: svcURLs(ws)})
	}
	b, err := json.MarshalIndent(map[string]any{"workspaces": out}, "", "  ")
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
