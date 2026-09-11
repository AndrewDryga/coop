package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/acpctl"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/ui"
)

// modelSep joins the ids in one wrapped row; modelIndent indents every line inside an agent block.
const (
	modelSep    = " · "
	modelIndent = "  "
)

// modelsOptions and modelsUsage are this command's own grammar: the options a correction may
// suggest, and the syntax a rejected argument is measured against.
var modelsOptions = []string{"--refresh"}

const modelsUsage = "coop models [<agent>] [--refresh]"

// cmdModels is the model MENU: one title-cased block per agent listing the ids you can put in a
// target, then how to start that agent with a model and where models are configured to stay. coop
// never validates a model id against this list — any id the agent's CLI accepts works — so the
// menu is a memory aid, not a contract.
//
// Keeping it current is coop's job, not a chore it teaches the user: every invocation refreshes,
// in parallel, only the catalogs it is about to render that are missing or older than
// modelsRefreshAfter, and says nothing when that works. `--refresh` forces the same fetch now,
// ignoring both freshness and the failure backoff.
func (a *app) cmdModels(args []string) (int, error) {
	refresh := false
	var rest []string
	for _, arg := range args {
		switch {
		case arg == "--refresh":
			refresh = true
		case strings.HasPrefix(arg, "-"):
			// A dash-led token is an OPTION this command doesn't have, not an agent name —
			// classifying it here is what makes the correction point at --refresh.
			return 2, unknownOptionErr(arg, "coop models", modelsOptions)
		default:
			rest = append(rest, arg)
		}
	}
	names := agents.Names()
	if len(rest) > 0 {
		if _, ok := agents.Get(rest[0]); !ok {
			return 2, unknownErr("agent", rest[0], agents.Names())
		}
		names = []string{rest[0]}
		if len(rest) > 1 {
			return 2, ui.UnexpectedArgument(rest[1], "coop models", modelsUsage)
		}
	}
	causes := a.refreshDueCatalogs(names, refresh)
	p := ui.For(os.Stdout) // stdout view — gate color on stdout so a pipe stays clean
	width := ui.TermWidth(os.Stdout) - utf8.RuneCountInString(modelIndent)
	for _, agent := range names {
		ag, _ := agents.Get(agent)
		cause, failed := causes[agent]
		ids, issue, why := a.modelMenuEntry(agent, ag, cause, failed)
		fmt.Println(p.Bold(displayAgentName(agent)))
		// Wrap the PLAIN ids, then style the separator — ANSI must never count toward a width.
		for _, row := range wrapModelIDs(ids, width) {
			fmt.Println(modelIndent + strings.Join(row, p.Dim(modelSep)))
		}
		if issue != "" {
			fmt.Println(modelIndent + p.Yellow("⚠ "+issue))
			if why != "" { // the reason, aligned under the warning's text, not under its glyph
				fmt.Println(modelIndent + "  " + p.Dim(why))
			}
		}
		// A standing COOP_<AGENT>_MODEL is configuration, not a catalog entry — say it as a fact.
		if def := a.cfg.AgentModelDefault(agent); def != "" {
			fmt.Printf("%sDefault for %s runs: %s\n", modelIndent, displayAgentName(agent), def)
		}
		fmt.Println()
	}
	// The example agent is the one that was asked for, else the first block rendered; its first
	// static id is a stable alias, so the line stays copyable whatever the live catalog holds.
	ex, _ := agents.Get(names[0])
	fmt.Printf("Start %s with a model\n", displayAgentName(names[0]))
	fmt.Printf("%s%s coop %s:%s\n\n", modelIndent, p.Cyan("→"), names[0], ex.Models()[0])
	fmt.Println("Set models for presets and loops")
	fmt.Printf("%s%s coop help models\n", modelIndent, p.Cyan("→"))
	return 0, nil
}

// wrapModelIDs packs ids into rows at most width visible columns wide, joined by modelSep, never
// splitting an id (a model id you cannot copy whole is worse than a short row). It measures plain
// text and returns the rows unjoined, so the caller can style the separator afterwards.
func wrapModelIDs(ids []string, width int) [][]string {
	sep := utf8.RuneCountInString(modelSep)
	var rows [][]string
	var row []string
	used := 0
	for _, id := range ids {
		n := utf8.RuneCountInString(id)
		switch {
		case len(row) == 0:
			row, used = []string{id}, n
		case used+sep+n <= width:
			row, used = append(row, id), used+sep+n
		default:
			rows = append(rows, row)
			row, used = []string{id}, n
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	return rows
}

// titleName renders an agent id as its block header — "claude" → "Claude" (ids are ASCII).
func titleName(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// modelChoices are the ids to offer for a cached catalog, and whether they came from it: the
// fetched list while it is still safe to present as this agent's own, else the release-bundled
// examples. Pure, so the caller can reuse the cache it already read.
func modelChoices(mc modelsCache, ag agents.Agent, now time.Time) ([]string, bool) {
	if ids := mc.ids(); len(ids) > 0 && mc.usable(now) {
		return ids, true
	}
	return ag.Models(), false
}

// agentModels are the ids to offer for an agent, cache-only — it never fetches, so shell
// completion stays instant and runtime-free.
func (a *app) agentModels(agent string, ag agents.Agent) ([]string, bool) {
	mc, _ := loadModelsCache(a.cfg, agent)
	return modelChoices(mc, ag, time.Now())
}

// modelMenuEntry picks the ids to show for an agent and, when they are not a current catalog, the
// honest warning: a headline saying WHICH kind of list is on screen, and the reason it could not
// be made current when there is one worth printing. failed reports that THIS invocation tried and
// could not (cause is its reason, "" when the error says nothing useful) — so a forced refresh
// that fails is never silent, even over a list still inside its refresh age. A catalog that is
// current and was not just refused says nothing: upkeep that worked is not news.
func (a *app) modelMenuEntry(agent string, ag agents.Agent, cause string, failed bool) (ids []string, issue, why string) {
	mc, _ := loadModelsCache(a.cfg, agent)
	now := time.Now()
	ids, cached := modelChoices(mc, ag, now)
	if !failed && mc.current(now) {
		return ids, "", ""
	}
	issue = "could not refresh — showing bundled examples"
	if cached {
		issue = "could not refresh — showing the list saved " + humanAge(mc.FetchedAt)
	}
	if cause == "" {
		cause = mc.AttemptError // an earlier failure, still inside its retry window
	}
	return ids, issue, cause
}

// refreshDueCatalogs brings the catalogs cmdModels is about to render up to date, and returns the
// human cause for each agent it could not refresh (success is silent). Only DUE agents are fetched
// — missing or older than modelsRefreshAfter — unless forced, which also ignores the backoff a
// failed attempt left behind. The fetches run in parallel: each writes its own agent's cache, so
// one slow provider never gates the others.
func (a *app) refreshDueCatalogs(names []string, forced bool) map[string]string {
	var due []string
	for _, agent := range names {
		mc, _ := loadModelsCache(a.cfg, agent)
		if forced || mc.due(time.Now()) {
			due = append(due, agent)
		}
	}
	if len(due) == 0 {
		return nil
	}
	// Detect the container runtime ONCE, before the fan-out: a.rt is a plain field, so two boxed
	// probes racing ensureRuntime would be a data race — and a runtime that is down is one cause
	// for every provider that needs it, not one bounded timeout each.
	var boxDown error
	for _, agent := range due {
		if a.fetchNeedsBox(agent) {
			boxDown = a.probeBoxRuntime()
			break
		}
	}
	causes, failed := make([]string, len(due)), make([]bool, len(due))
	var wg sync.WaitGroup
	for i, agent := range due {
		blocked := error(nil)
		if a.fetchNeedsBox(agent) {
			blocked = boxDown
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			causes[i], failed[i] = a.refreshCatalog(agent, blocked)
		}()
	}
	wg.Wait()
	out := make(map[string]string, len(due))
	for i, agent := range due {
		if failed[i] {
			out[agent] = causes[i] // present-but-empty: it failed, with nothing nameable to say
		}
	}
	return out
}

// refreshCatalog fetches one agent's catalog and persists the outcome: the models on success, the
// attempt and its cause on failure — so the next menu read backs off instead of paying the same
// bounded timeout again. blocked short-circuits the fetch with a cause already known (an
// unavailable runtime), which still counts as the attempt. It reports whether the fetch failed and
// the human cause when there is one.
func (a *app) refreshCatalog(agent string, blocked error) (string, bool) {
	var models []acpctl.Model
	err := blocked
	if err == nil {
		models, err = a.fetchModelCatalog(agent)
		if err == nil && len(models) == 0 {
			err = modelFetchError{cause: agent + " returned no models"}
		}
	}
	if err == nil {
		if writeErr := writeModelsCache(a.cfg, agent, models); writeErr != nil {
			err = modelFetchError{cause: "the cache could not be written", err: writeErr}
		} else {
			return "", false
		}
	}
	cause := modelFetchCause(agent, err)
	_ = recordModelsFetchFailure(a.cfg, agent, cause) // best-effort: a lost note costs one refetch
	return cause, true
}

// fetchNeedsBox reports whether refreshing agent has to launch a container — the ACP-only
// providers, unless a test seam has replaced the fetch.
func (a *app) fetchNeedsBox(agent string) bool {
	return a.acpModels == nil && nativeModelFetchers[agent] == nil
}

// probeBoxRuntime resolves the container runtime a boxed catalog fetch needs, naming what is
// missing so the menu can say it in one line instead of surfacing a paragraph of remedy.
func (a *app) probeBoxRuntime() error {
	if err := a.ensureRuntime(); err != nil {
		return modelFetchError{cause: "no container runtime is installed", err: err}
	}
	if err := a.rt.EnsureDaemon(); err != nil {
		return modelFetchError{cause: "Docker is not running", err: err}
	}
	return nil
}

// modelFetchError carries a fetch failure the menu can explain in a few words. The wrapped error
// keeps the full detail for anything that logs it.
type modelFetchError struct {
	cause string
	err   error
}

func (e modelFetchError) Error() string {
	if e.err == nil {
		return e.cause
	}
	return e.cause + ": " + e.err.Error()
}

func (e modelFetchError) Unwrap() error { return e.err }

// modelFetchCause reduces a failed fetch to the few words that help — "" when nothing about the
// error is actionable, because a menu that invents a reason is worse than one that admits the
// list is old.
func modelFetchCause(agent string, err error) string {
	var named modelFetchError
	switch {
	case errors.As(err, &named):
		return named.cause
	case errors.Is(err, exec.ErrNotFound):
		return "the " + agent + " CLI is not installed"
	case errors.Is(err, context.DeadlineExceeded):
		return displayAgentName(agent) + " did not answer in time"
	}
	return ""
}
