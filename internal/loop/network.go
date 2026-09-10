package loop

import (
	"cmp"
	"fmt"
	"slices"
	"sync"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/ui"
)

// A drain runs for hours and an agent can produce arbitrarily many distinct
// denied names, so the networking lines stay bounded: a few destinations per
// iteration, a few in the closing summary, and the receipt holds the rest.
const maxLoopDenials = 3

// networkAdmissionSpec describes what the WHOLE run may launch, not one
// iteration. Admission freezes ONE policy here, so every rung of every stage's
// ladder — work, pre-flight, between, signoff, verify — plus the named peers and
// the preset's roles have to be in the credential scope the provider bundles
// derive from. A rotation that later swapped in a provider admission never saw
// would meet a mid-drain denial instead of a refusal at launch.
func networkAdmissionSpec(cfg *config.Config, repo, img, agent string, p *preset.Preset, peers []agents.Target, rotations ...*ladder.Rotation) box.RunSpec {
	scope := append([]agents.Target{}, peers...)
	for _, rot := range rotations {
		if rot == nil {
			continue
		}
		scope = append(scope, rot.Targets()...)
	}
	return box.RunSpec{
		Image: img, Repo: repo, Agent: agent, Peers: scope, Preset: p,
		Homes: cfg.Homes, Network: cfg.Network, Cache: cfg.Cache,
	}
}

// networkLog is one loop run's filtered-networking bookkeeping. box.Run hands it
// each box's report from INSIDE the launch, while the live bar owns the
// terminal, so it only accumulates there; the loop prints between iterations and
// once at the end, through its own output.
type networkLog struct {
	mu        sync.Mutex
	pending   []loopNetworkRun // the iteration in flight, not yet printed
	lastRunID string           // the network run this stage's telemetry points at
	runs      int
	alerts    int
	refused   map[string]int // destination (basis) -> times refused across the run
	order     []string       // first-seen order, so equal counts read the same way twice
	closed    bool
}

// loopNetworkRun is one filtered box's report plus its ordinal in the run, so
// the printed line can say which iteration hit the boundary.
type loopNetworkRun struct {
	report box.NetworkReport
	run    int
}

func newNetworkLog() *networkLog { return &networkLog{refused: map[string]int{}} }

// record is box.RunSpec.OnNetworkReport. It runs on the launch's own goroutine
// with the bar up, so it accumulates and prints nothing.
func (l *networkLog) record(r box.NetworkReport) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runs++
	l.pending = append(l.pending, loopNetworkRun{report: r, run: l.runs})
	l.alerts += len(r.Alerts)
	for _, denial := range r.Denials {
		key := denial.Destination + " (" + denial.Basis + ")"
		if _, seen := l.refused[key]; !seen {
			l.order = append(l.order, key)
		}
		l.refused[key] += denial.Count
	}
}

// finishIteration prints what the iteration that just ended could not reach. It
// runs as runIteration's LAST deferred step — after the live bar is down and the
// ui sink is plain stderr again — so the block scrolls with the loop's own
// between-iteration lines and never repaints over the bar.
func (l *networkLog) finishIteration() {
	if l == nil {
		return
	}
	l.mu.Lock()
	pending := l.pending
	l.pending = nil
	l.lastRunID = ""
	for _, run := range pending {
		l.lastRunID = run.report.RunID
	}
	l.mu.Unlock()
	for _, run := range pending {
		printNetworkIteration(run.report, run.run)
	}
}

// runID is the filtered run this stage's telemetry row binds to, so a receipt in
// `coop net ls` and a loop stage can be related. Empty outside filtered mode.
func (l *networkLog) runID() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastRunID
}

// printNetworkIteration is the between-iterations block: what was refused, any
// alert that fired, and the one command that explains a refusal. A clean
// iteration prints nothing at all.
func printNetworkIteration(r box.NetworkReport, run int) {
	if r.Quiet() {
		return
	}
	if total := len(r.Denials) + r.Omitted; total > 0 {
		ui.Warn("iteration %d — %s refused", run, ui.Count(total, "destination was", "destinations were"))
		for _, line := range networkDenialList(r) {
			ui.Detail("%s", line)
		}
	}
	if r.RawPackets > 0 {
		// No destination was recorded for these, so the line says so instead of
		// inventing one — but the iteration did meet the boundary.
		ui.Warn("iteration %d — %s", run, r.Raw)
	}
	for _, alert := range r.Alerts {
		ui.Warn("network alert: %s", alert)
	}
	if r.Event != "" {
		ui.Detail("coop net explain %s --run %s   # why", r.Event, r.RunID)
	}
}

// networkDenialList renders one iteration's refusals as a few bounded lines —
// one destination each, the way the end-of-run summary reads.
func networkDenialList(r box.NetworkReport) []string {
	shown, omitted := r.Denials, r.Omitted
	if len(shown) > maxLoopDenials {
		omitted += len(shown) - maxLoopDenials
		shown = shown[:maxLoopDenials]
	}
	lines := make([]string, 0, len(shown)+1)
	for _, denial := range shown {
		lines = append(lines, denial.String())
	}
	if omitted > 0 {
		lines = append(lines, fmt.Sprintf("… and %d more", omitted))
	}
	return lines
}

// summary closes the run with what its boxes could not reach: totals plus the
// few destinations that dominated them, and where the evidence lives. It is
// printed once — the loop calls it before its closing banner, and the deferred
// call covers every other way out. A run that hit no boundary says nothing.
func (l *networkLog) summary() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || len(l.refused) == 0 && l.alerts == 0 {
		return
	}
	l.closed = true
	headline := fmt.Sprintf("%s refused across %s", ui.Count(len(l.refused), "destination was", "destinations were"),
		ui.Count(l.runs, "filtered run"))
	if l.alerts > 0 {
		headline += ", with " + ui.Count(l.alerts, "alert")
	}
	ui.Warn("%s", headline)
	for _, line := range topRefused(l.refused, l.order, maxLoopDenials) {
		ui.Detail("%s", line)
	}
	ui.Detail("coop net ls   # every run and its receipt")
}

// topRefused ranks destinations by how often they were refused, breaking ties by
// first appearance so two runs of the same drain read the same way.
func topRefused(counts map[string]int, order []string, max int) []string {
	ranked := slices.Clone(order)
	slices.SortStableFunc(ranked, func(a, b string) int { return cmp.Compare(counts[b], counts[a]) })
	if len(ranked) > max {
		ranked = ranked[:max]
	}
	out := make([]string, 0, len(ranked))
	for _, key := range ranked {
		out = append(out, fmt.Sprintf("%s ×%d", key, counts[key]))
	}
	return out
}
