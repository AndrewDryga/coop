package loop

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
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
		NetworkAdmission: true,
	}
}

// networkLog is one loop run's filtered-networking bookkeeping. box.Run hands it
// each box's report from INSIDE the launch, while the live bar owns the
// terminal, so it only accumulates there; the loop prints between iterations and
// once at the end, through its own output.
type networkLog struct {
	mu        sync.Mutex
	stage     string           // what the box in flight is doing, as its report will name it
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
	stage  string
}

// setStage names the attempt whose box is about to launch, so a refusal is reported against the
// work a person recognizes ("Task attempt 2") rather than an internal filtered-run counter.
func (l *networkLog) setStage(stage string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stage = stage
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
	stage := l.stage
	if stage == "" {
		stage = "This attempt"
	}
	l.pending = append(l.pending, loopNetworkRun{report: r, stage: stage})
	l.alerts += len(r.Alerts)
	for _, denial := range r.Denials {
		key := denial.Destination
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
		printNetworkIteration(run.report, run.stage)
	}
}

// runID is the filtered run this stage's telemetry row binds to, so a receipt in
// `coop net runs` and a loop stage can be related. Empty outside filtered mode.
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
func printNetworkIteration(r box.NetworkReport, stage string) {
	if r.Quiet() {
		return
	}
	if total := len(r.Denials) + r.Omitted; total > 0 {
		rows := []string{}
		rows = append(rows, networkDenialList(r)...)
		ui.Alert(fmt.Sprintf("%s could not reach %s", stage, ui.Count(total, "remote address", "remote addresses")),
			strings.Join(rows, "\n"), explainRow(r)...)
	}
	if r.RawPackets > 0 {
		// No destination was recorded for these, so the block says so instead of inventing one —
		// but the attempt did meet the boundary, and that absence is itself evidence.
		ui.Alert(fmt.Sprintf("Blocked %s in %s", ui.Count(int(r.RawPackets), "raw network packet"), strings.ToLower(stage)),
			"No remote address details were recorded for these packets.")
	}
	for _, alert := range r.Alerts {
		ui.Alert("The network filter raised an alert", alert)
	}
	if r.Event != "" {
		ui.Detail("To see why: coop net blocked %s --run %s", r.Event, r.RunID)
	}
}

// explainRow is the one command that explains a refusal — the destination lookup, pointed at the
// exact run that recorded it. It is omitted when no destination was retained: a lookup with
// nothing to look up is worse than no pointer at all.
func explainRow(r box.NetworkReport) [][2]string {
	if len(r.Denials) == 0 || r.RunID == "" {
		return nil
	}
	return [][2]string{{"Explain:", "coop net blocked " + r.Denials[0].Destination + " --run " + r.RunID}}
}

// denialRow is one refused destination as a person reads it: where the box tried to go, where the
// refusal was observed, and how many attempts that grouped. The protocol word comes from the
// retained evidence — a refused DNS query is never called a connection.
func denialRow(d box.NetworkDenial) string {
	return d.Destination + " · " + protocolWord(d.Basis) + " · blocked " + ui.Count(d.Count, "time")
}

// protocolWord upper-cases the acronyms a person expects to see that way and leaves every other
// basis in its own words.
func protocolWord(basis string) string {
	switch basis {
	case "dns":
		return "DNS"
	case "tls":
		return "TLS"
	}
	return basis
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
		lines = append(lines, denialRow(denial))
	}
	if omitted > 0 {
		lines = append(lines, fmt.Sprintf("… %d more remote addresses", omitted))
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
	headline := fmt.Sprintf("Traffic to %s was blocked across %s",
		ui.Count(len(l.refused), "remote address", "remote addresses"), ui.Count(l.runs, "network run"))
	if l.alerts > 0 {
		headline += ", with " + ui.Count(l.alerts, "alert")
	}
	ui.Alert(headline, strings.Join(topRefused(l.refused, l.order, maxLoopDenials), "\n"),
		[2]string{"Run details:", "coop net runs"})
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
		out = append(out, fmt.Sprintf("%s · blocked %s", key, ui.Count(counts[key], "time")))
	}
	return out
}
