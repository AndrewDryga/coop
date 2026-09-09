package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/ui"
)

// The `coop net` reads answer from retained host evidence and nothing else: no
// container, no daemon probe, no DNS lookup, no policy change, and no directory
// created to report that nothing was found. `setup` and `approve` are the two
// verbs that write, and both are explicit host operations.

const (
	// netEventMaxBytes bounds ONE rendered view or watch event — not the stream,
	// since a long watch on a busy run is honest work. It matches the retained
	// record envelope the evidence is read from, so an event over it means the
	// evidence is anomalous, not that the run was too busy.
	netEventMaxBytes = 4 << 20
	netWatchPoll     = time.Second
	netLabelWidth    = 14
	// netRecentRuns is how many of a project's runs `coop net` shows. It is a
	// posture view with a tail, not the listing: `coop net ls` is that.
	netRecentRuns = 5
	// netWatchDeltaLines bounds what ONE poll may append. An agent can generate
	// arbitrarily many distinct refused names; a watch is not a firehose.
	netWatchDeltaLines = 5
)

var netCommands = []string{"ls", "inspect", "watch", "receipt", "why", "explain", "approve", "setup"}

// cmdNet routes the restricted-networking family. Bare `coop net` is this
// project's posture, not a listing: what a box may reach is the question a
// human actually arrives with, and `ls` is the only listing spelling.
func (a *app) cmdNet(args []string) (int, error) {
	if len(args) == 0 {
		return a.cmdNetPosture()
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "setup":
		if err := rejectArgs("net setup", rest); err != nil {
			return 2, err
		}
		return a.cmdNetSetup()
	case "ls":
		return a.cmdNetLs(rest)
	case "inspect", "watch", "receipt":
		return a.cmdNetRun(verb, rest)
	case "why", "explain":
		return netDiagnostic(verb, rest)
	case "approve":
		return a.cmdNetApprove(rest)
	default:
		return 2, unknownErr("net command", verb, netCommands)
	}
}

func (a *app) cmdNetSetup() (int, error) {
	if err := a.ensureRuntime(); err != nil {
		return -1, err
	}
	if err := a.rt.EnsureDaemon(); err != nil {
		return -1, err
	}
	ui.Info("setting up restricted networking for this host's container runtime")
	if _, err := box.SetupNetwork(context.Background(), a.cfg, a.rt, os.Stderr, os.Stderr); err != nil {
		return 1, err
	}
	ui.Steps("coop run --egress filtered --allow-domain example.com -- curl https://example.com")
	return 0, nil
}

// ---------------------------------------------------------------- posture ---

func (a *app) cmdNetPosture() (int, error) {
	repo, err := netProject(a.cfg.RepoOverride)
	if err != nil {
		return 1, err
	}
	posture, err := box.ProjectNetworkPosture(context.Background(), a.cfg, repo)
	if err != nil {
		return 1, err
	}
	runs, listErr := netProjectRuns(posture.Project)
	p := ui.For(os.Stdout)
	if err := netRender(os.Stdout, func(b *bytes.Buffer) { writeNetPosture(b, p, posture, runs) }); err != nil {
		return 1, err
	}
	if listErr != nil {
		ui.Warn("recorded runs are unavailable: %v", listErr)
	}
	return 0, nil
}

func writeNetPosture(w io.Writer, p ui.Palette, posture box.NetworkPosture, runs []netRun) {
	fmt.Fprintf(w, "%s\n", p.Bold(p.Cyan("network posture")))
	field := func(label, value string) { netField(w, p, netLabelWidth, label, value) }
	field("Project", posture.Project)
	field("Egress", string(posture.Mode)+" (from the "+posture.Source+")")
	switch {
	case posture.Approval == nil:
		field("Approved", "nothing remembered for this project yet")
	case len(posture.Approval.Envelope) == 0:
		field("Approved", string(posture.Approval.Posture)+", no project rules")
	default:
		field("Approved", string(posture.Approval.Posture)+", "+ui.Count(len(posture.Approval.Envelope), "rule"))
		for _, rule := range posture.Approval.Envelope {
			netRow(w, box.NetworkRuleText(rule))
		}
	}
	switch {
	case len(posture.Add) == 0 && len(posture.Remove) == 0 && len(posture.Requested) == 0:
		field("Requested", "no box.egress_rules in .agent/project.yaml")
	case len(posture.Add) == 0 && len(posture.Remove) == 0:
		field("Requested", "matches the approval — nothing pending")
	default:
		pending := "pending review — run 'coop net approve'"
		if posture.Pending != nil {
			// The same condition Admit fails on, said once: a request outside
			// the remembered envelope is not a warning, it stops a launch.
			pending = "pending review — a filtered launch refuses until you run 'coop net approve'"
		}
		field("Requested", pending)
		for _, rule := range posture.Add {
			netRow(w, "+ "+box.NetworkRuleText(rule))
		}
		for _, rule := range posture.Remove {
			netRow(w, "- "+box.NetworkRuleText(rule))
		}
	}
	switch {
	case posture.Setup == nil:
		field("This host", "not set up — run 'coop net setup' before a filtered launch")
	case !posture.SetupCurrent():
		field("This host", "set up for another contract ("+posture.Setup.Contract+") — re-run 'coop net setup'")
	default:
		field("This host", "set up "+posture.Setup.CompletedAt.UTC().Format(time.RFC3339))
	}
	if len(runs) == 0 {
		field("Recent runs", "none recorded for this project")
		return
	}
	field("Recent runs", ui.Count(len(runs), "run")+" (newest first)")
	for _, run := range runs {
		netRow(w, netRunLine(p, run))
	}
}

// netRun is one recorded run as the listing shows it: the retained summary plus
// the traffic that run actually saw.
type netRun struct {
	networkstate.ExecutionSummary
	Outcome string
}

// netRunLine is the one-line summary of a recorded run, shared by the posture
// view and `ls`. "no final receipt" is a fact about the evidence, NOT a claim
// that the run is still alive.
func netRunLine(p ui.Palette, run netRun) string {
	line := p.Bold(p.Cyan(run.ID)) + "  " + run.StartedAt.UTC().Format(time.RFC3339) + "  " + netFinality(run.Final)
	if run.CleanupPending {
		line += ", cleanup pending"
	}
	if run.SessionID != "" {
		line += ", session " + run.SessionID
	}
	if run.Outcome != "" {
		line += "  " + run.Outcome
	}
	return line
}

// netRunOutcome is what got through and what did not, from that run's own
// retained evidence. The two counts are different units and are never added
// together; a run nothing observed says so instead of showing two zeros.
func netRunOutcome(inspection networkstate.Inspection) string {
	observed := inspection.Observed
	if observed.Sequence == 0 {
		return "not observed"
	}
	allowed := "UNKNOWN"
	if observed.Counters != nil {
		allowed = netCount(observed.Counters.Connections)
	}
	refused := strconv.Itoa(len(observed.Denials))
	if observed.Loss.DetailTruncated {
		refused += "+" // the ring dropped detail; this is a lower bound
	}
	return allowed + " allowed, " + refused + " refused"
}

func netFinality(final bool) string {
	if final {
		return "sealed receipt"
	}
	return "no final receipt"
}

// --------------------------------------------------------------- listings ---

type netListOptions struct{ all, json bool }

func parseNetListFlags(args []string) (netListOptions, error) {
	var opts netListOptions
	for _, arg := range args {
		switch arg {
		case "--all":
			opts.all = true
		case "--json":
			opts.json = true
		default:
			return opts, unknownErr("net ls flag", arg, []string{"--all", "--json"})
		}
	}
	return opts, nil
}

func (a *app) cmdNetLs(args []string) (int, error) {
	opts, err := parseNetListFlags(args)
	if err != nil {
		return 2, err
	}
	project := ""
	if !opts.all {
		repo, err := netProject(a.cfg.RepoOverride)
		if err != nil {
			return 1, err
		}
		if project, err = filepath.EvalSymlinks(repo); err != nil {
			return 1, err
		}
	}
	page, err := netExecutions()
	if err != nil {
		return 1, err
	}
	runs := page.Executions
	if !opts.all {
		runs = netFilterProject(runs, project)
	}
	if opts.json {
		return 0, netWriteJSON(os.Stdout, netListingDTO(runs, page))
	}
	listed, err := netRunOutcomes(runs)
	if err != nil {
		return 1, err
	}
	p := ui.For(os.Stdout)
	if err := netRender(os.Stdout, func(b *bytes.Buffer) {
		for _, run := range listed {
			fmt.Fprintf(b, "%s\n", netRunLine(p, run))
		}
	}); err != nil {
		return 1, err
	}
	scope := "this project"
	if opts.all {
		scope = "every project"
	}
	if len(runs) == 0 {
		ui.Note("no filtered runs recorded for %s — 'coop net' shows this project's posture", scope)
	} else {
		ui.OK("%s recorded for %s", ui.Count(len(runs), "filtered run"), scope)
	}
	if page.Incomplete {
		ui.Warn("this listing is incomplete — some retained records could not be enumerated")
	}
	if page.Unreadable > 0 {
		ui.Warn("%s could not be read and are not shown", ui.Count(page.Unreadable, "retained record"))
	}
	return 0, nil
}

type netRunJSON struct {
	ID             string    `json:"id"`
	Epoch          string    `json:"gateway_epoch"`
	SessionID      string    `json:"session_id,omitempty"`
	AttemptID      string    `json:"attempt_id,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	Final          bool      `json:"final"`
	CleanupPending bool      `json:"cleanup_pending"`
}

type netListingJSON struct {
	Version    int          `json:"version"`
	Runs       []netRunJSON `json:"runs"`
	Incomplete bool         `json:"incomplete"`
	Unreadable int          `json:"unreadable"`
}

// netListingDTO is an ALLOWLIST. networkstate.ExecutionSummary is owner-private
// storage — it carries the project path — so every published field is named
// here on purpose and a field added to the record never appears by growing into
// a serialized type.
func netListingDTO(runs []networkstate.ExecutionSummary, page networkstate.ExecutionPage) netListingJSON {
	out := netListingJSON{Version: networkview.Version, Runs: []netRunJSON{}, Incomplete: page.Incomplete, Unreadable: page.Unreadable}
	for _, run := range runs {
		out.Runs = append(out.Runs, netRunJSON{ID: run.ID, Epoch: run.Epoch, SessionID: run.SessionID,
			AttemptID: run.AttemptID, StartedAt: run.StartedAt.UTC(), Final: run.Final, CleanupPending: run.CleanupPending})
	}
	return out
}

// netExecutions reads every retained run, newest first. Paging is internal: the
// store's cursor is lexical over random ids, so a user-facing page would show
// an arbitrary hundred rather than the recent ones anybody asked for.
func netExecutions() (networkstate.ExecutionPage, error) {
	evidence, err := openNetEvidence()
	if errors.Is(err, fs.ErrNotExist) {
		return networkstate.ExecutionPage{}, nil // an accurate empty page, and no directory created to say so
	}
	if err != nil {
		return networkstate.ExecutionPage{}, err
	}
	defer evidence.Close()
	var all networkstate.ExecutionPage
	after := ""
	for range 128 {
		page, err := evidence.Executions(after)
		if err != nil {
			return networkstate.ExecutionPage{}, err
		}
		all.Executions = append(all.Executions, page.Executions...)
		all.Incomplete = all.Incomplete || page.Incomplete
		all.Unreadable += page.Unreadable
		if page.Next == "" || page.Next == after {
			break
		}
		after = page.Next
	}
	slices.SortStableFunc(all.Executions, func(x, y networkstate.ExecutionSummary) int {
		return y.StartedAt.Compare(x.StartedAt)
	})
	return all, nil
}

// netProjectRuns is the posture view's tail: the project's most recent runs,
// each with its own outcome read from its own evidence.
func netProjectRuns(project string) ([]netRun, error) {
	page, err := netExecutions()
	if err != nil {
		return nil, err
	}
	summaries := netFilterProject(page.Executions, project)
	if len(summaries) > netRecentRuns {
		summaries = summaries[:netRecentRuns]
	}
	return netRunOutcomes(summaries)
}

// netRunOutcomes reads each run's own evidence for what got through and what
// did not. A record that cannot be projected still belongs in the list; it just
// carries no outcome, which is not the same as a run that saw no traffic.
func netRunOutcomes(summaries []networkstate.ExecutionSummary) ([]netRun, error) {
	if len(summaries) == 0 {
		return nil, nil
	}
	evidence, err := openNetEvidence()
	if err != nil {
		return nil, err
	}
	defer evidence.Close()
	runs := make([]netRun, 0, len(summaries))
	for _, summary := range summaries {
		run := netRun{ExecutionSummary: summary}
		if inspection, err := evidence.Inspect(summary.ID, time.Now(), true); err == nil {
			run.Outcome = netRunOutcome(inspection)
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// netFilterProject keeps the runs recorded for one project. Both sides resolve
// through symlinks so /var and /private/var are one project; a project whose
// tree was removed still matches by its cleaned absolute path, because deleting
// a checkout does not delete its evidence.
func netFilterProject(runs []networkstate.ExecutionSummary, project string) []networkstate.ExecutionSummary {
	var out []networkstate.ExecutionSummary
	for _, run := range runs {
		if run.Project == project || netResolvedPath(run.Project) == netResolvedPath(project) {
			out = append(out, run)
		}
	}
	return out
}

func netResolvedPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

// ------------------------------------------------------------- single run ---

type netRunOptions struct {
	json         bool
	destinations bool
	id           string
}

func parseNetRunArgs(verb string, args []string) (netRunOptions, error) {
	var opts netRunOptions
	valid := []string{"--json"}
	if verb == "receipt" {
		valid = append(valid, "--destinations")
	}
	for _, arg := range args {
		switch {
		case arg == "--json":
			opts.json = true
		case arg == "--destinations" && verb == "receipt":
			opts.destinations = true
		case strings.HasPrefix(arg, "-"):
			return opts, unknownErr("net "+verb+" flag", arg, valid)
		case opts.id != "":
			return opts, fmt.Errorf("net %s reads one run (got %q and %q) — see 'coop net --help'", verb, opts.id, arg)
		default:
			opts.id = arg
		}
	}
	if opts.id == "" {
		return opts, fmt.Errorf("net %s needs a run id — list them with 'coop net ls'", verb)
	}
	return opts, nil
}

func (a *app) cmdNetRun(verb string, args []string) (int, error) {
	opts, err := parseNetRunArgs(verb, args)
	if err != nil {
		return 2, err
	}
	evidence, err := openNetRunEvidence()
	if err != nil {
		return 1, err
	}
	defer evidence.Close()
	if verb == "watch" {
		return netWatch(evidence, opts.id, opts.json)
	}
	// A receipt is the shareable artifact, so its default projection is the
	// redacted one: even a denied name can encode a secret. --destinations is
	// the operator saying, on their own machine, that they want the names.
	inspection, err := evidence.Inspect(opts.id, time.Now(), verb != "receipt" || opts.destinations)
	if err != nil {
		return 1, netRunErr(opts.id, err)
	}
	if verb == "receipt" {
		if inspection.Receipt == nil {
			return 1, fmt.Errorf("network run %q has no sealed receipt yet — inspect the provisional evidence with 'coop net inspect %s'", opts.id, opts.id)
		}
		if opts.json {
			return 0, netWriteJSON(os.Stdout, inspection.Receipt)
		}
		p := ui.For(os.Stdout)
		return 0, netRender(os.Stdout, func(b *bytes.Buffer) { writeNetReceipt(b, p, opts.id, *inspection.Receipt) })
	}
	if opts.json {
		return 0, netWriteJSON(os.Stdout, inspection)
	}
	p := ui.For(os.Stdout)
	return 0, netRender(os.Stdout, func(b *bytes.Buffer) { writeNetInspection(b, p, opts.id, inspection) })
}

func netRunErr(id string, err error) error {
	if errors.Is(err, networkstate.ErrEvidenceUnavailable) || errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("network run %q: no retained evidence — list what was recorded with 'coop net ls --all'", id)
	}
	return fmt.Errorf("network run %q: %w", id, err)
}

// writeNetInspection is the human view of one run: short labeled fields, with
// requested policy, effective capture and observed traffic kept apart. The full
// coverage, loss and health tables live behind --json — they are an
// investigation, not a status line.
func writeNetInspection(w io.Writer, p ui.Palette, id string, inspection networkstate.Inspection) {
	observed := inspection.Observed
	fmt.Fprintf(w, "%s\n", p.Bold(p.Cyan("network run "+id)))
	field := func(label, value string) { netField(w, p, netLabelWidth, label, value) }
	field("Requested", "egress "+string(observed.Mode)+", policy "+netUnknownIfEmpty(observed.PolicyFingerprint))
	field("Effective", "gateway epoch "+netUnknownIfEmpty(observed.Epoch)+", scope "+netUnknownIfEmpty(observed.Scope))
	field("Read at", inspection.ReadAt.UTC().Format(time.RFC3339))
	if observed.Sequence == 0 || observed.AsOf.IsZero() {
		field("Observed", "never observed (freshness "+inspection.Freshness+")")
	} else {
		field("Observed", observed.AsOf.UTC().Format(time.RFC3339)+" (freshness "+inspection.Freshness+
			", availability "+netUnknownIfEmpty(observed.Availability)+")")
	}
	field("Now", netCurrentSummary(inspection))
	if current := inspection.Current; current != nil {
		field("Connections", "live "+netCount(current.LiveConnections)+", unknown "+netCount(current.UnknownConnections)+
			", pending "+netCount(current.PendingConnections))
	}
	if counters := observed.Counters; counters == nil {
		field("Totals", "UNKNOWN (no counters retained)")
	} else {
		field("Totals", fmt.Sprintf("sent %s bytes, received %s bytes over %s connection(s)",
			netCount(counters.SentBytes), netCount(counters.ReceivedBytes), netCount(counters.Connections)))
		field("Denied", fmt.Sprintf("packets %s, dns %s, tls %s", netCount(counters.DeniedPackets),
			netCount(counters.DeniedDNSQueries), netCount(counters.DeniedTLS)))
	}
	writeNetDenials(w, p, observed)
	if len(observed.Alerts) == 0 {
		field("Alerts", "none retained")
	} else {
		field("Alerts", ui.Count(len(observed.Alerts), "retained alert"))
		for _, alert := range observed.Alerts {
			netRow(w, fmt.Sprintf("%s %s/%s %s (first %s, last %s)", alert.ID, alert.Category, alert.Severity,
				alert.State, alert.FirstSeen.UTC().Format(time.RFC3339), alert.LastSeen.UTC().Format(time.RFC3339)))
		}
	}
	if observed.Loss.Unknown || observed.Loss.Records != 0 || observed.Loss.DetailTruncated {
		field("Evidence", netLossSummary(observed.Loss))
	}
	field("Health", netHealthSummary(observed.Health))
	field("Cleanup", inspection.Cleanup)
	if inspection.Receipt == nil {
		field("Receipt", "none sealed yet (not a claim that the run is alive)")
	} else {
		field("Receipt", "sealed "+inspection.Receipt.Finality+", "+inspection.Receipt.Completeness)
	}
	fmt.Fprintln(w, p.Dim("Full coverage, loss and per-source health: coop net inspect "+id+" --json"))
}

func writeNetDenials(w io.Writer, p ui.Palette, observed networkview.Snapshot) {
	field := func(label, value string) { netField(w, p, netLabelWidth, label, value) }
	if len(observed.Denials) == 0 {
		field("Refused", "none retained")
		return
	}
	field("Refused", ui.Count(len(observed.Denials), "retained decision"))
	for _, denial := range observed.Denials {
		netRow(w, fmt.Sprintf("%s %s %s — %s at %s", denial.ID, netDestination(denial.Name, denial.Peer, denial.DestinationID),
			denial.Kind, denial.Reason, denial.At.UTC().Format(time.RFC3339)))
	}
}

// writeNetReceipt renders the sealed outcome. Finality and completeness are
// INDEPENDENT: final + partial is valid after a crash, and must never read as
// clean evidence.
func writeNetReceipt(w io.Writer, p ui.Palette, id string, receipt networkview.Receipt) {
	fmt.Fprintf(w, "%s\n", p.Bold(p.Cyan("sealed receipt "+receipt.ID)))
	field := func(label, value string) { netField(w, p, netLabelWidth, label, value) }
	field("Run", id)
	field("Finality", receipt.Finality+" (completeness "+receipt.Completeness+")")
	field("Workload", receipt.Workload)
	field("Cleanup", receipt.Cleanup)
	field("Started", receipt.StartedAt.UTC().Format(time.RFC3339))
	if receipt.EndedAt != nil {
		field("Ended", receipt.EndedAt.UTC().Format(time.RFC3339))
	}
	field("Runtime", receipt.Runtime+", gateway "+receipt.GatewayImage)
	field("Collector", receipt.CollectorVersion)
	field("Projection", netUnknownIfEmpty(receipt.Snapshot.Projection))
	field("Digest", receipt.Digest+" ("+receipt.DigestScope+")")
	if counters := receipt.Snapshot.Counters; counters != nil {
		field("Totals", fmt.Sprintf("sent %s bytes, received %s bytes over %s connection(s)",
			netCount(counters.SentBytes), netCount(counters.ReceivedBytes), netCount(counters.Connections)))
		field("Denied", fmt.Sprintf("packets %s, dns %s, tls %s", netCount(counters.DeniedPackets),
			netCount(counters.DeniedDNSQueries), netCount(counters.DeniedTLS)))
	} else {
		field("Totals", "UNKNOWN (no counters were sealed with this receipt)")
	}
	writeNetDenials(w, p, receipt.Snapshot)
	if receipt.Snapshot.Projection != "destinations-included" {
		fmt.Fprintln(w, p.Dim("Destinations are withheld in this projection — even a refused name can encode a secret."))
		fmt.Fprintln(w, p.Dim("Add --destinations for the local operator view; the digest above covers this projection only."))
	}
	fmt.Fprintln(w, p.Dim("Full receipt fields: coop net receipt "+id+" --json"))
}

// ----------------------------------------------------------------- watch ----

// netWatch follows one run until its receipt is sealed. The human view APPENDS
// coalesced lines and never repaints; --json emits one bounded NDJSON snapshot
// per change, each carrying its own freshness and loss.
func netWatch(evidence *networkstate.Evidence, id string, asJSON bool) (int, error) {
	// Cancellation is a context, not a state change: Ctrl-C stops reading and
	// leaves the evidence and the run exactly as they were.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ticker := time.NewTicker(netWatchPoll)
	defer ticker.Stop()
	return runNetWatch(netWatchDeps{
		ctx: ctx, tick: ticker.C, now: time.Now, out: os.Stdout, json: asJSON, id: id, palette: ui.For(os.Stdout),
		read: func(now time.Time) (networkstate.Inspection, error) { return evidence.Inspect(id, now, true) },
	})
}

// netWatchDeps are the loop's seams — the clock, the tick channel, the cancel
// context and the one read — so the state machine is testable by pushing ticks
// instead of sleeping, with no runtime and no evidence root.
type netWatchDeps struct {
	ctx     context.Context
	tick    <-chan time.Time
	now     func() time.Time
	read    func(now time.Time) (networkstate.Inspection, error)
	out     io.Writer
	json    bool
	id      string
	palette ui.Palette
}

func runNetWatch(d netWatchDeps) (int, error) {
	previous, err := d.read(d.now())
	if err != nil {
		return 1, netRunErr(d.id, err)
	}
	if err := netWatchEmit(d, nil, previous); err != nil {
		return netWatchFailure(err)
	}
	if previous.Receipt != nil {
		return 0, nil
	}
	for {
		select {
		case <-d.ctx.Done():
			return 0, nil
		case <-d.tick:
			current, err := d.read(d.now())
			if err != nil {
				return 1, netRunErr(d.id, err)
			}
			// A terminal snapshot is not the end: cleanup and sealing may still
			// be in flight, so the watch reads on until the receipt exists.
			final := current.Receipt != nil
			if netEvidenceChanged(previous, current) || final {
				before := previous
				if err := netWatchEmit(d, &before, current); err != nil {
					return netWatchFailure(err)
				}
			}
			previous = current
			if final {
				return 0, nil
			}
		}
	}
}

func netWatchEmit(d netWatchDeps, previous *networkstate.Inspection, current networkstate.Inspection) error {
	if d.json {
		data, err := json.Marshal(current)
		if err != nil {
			return err
		}
		data = append(data, '\n')
		if err := netEventBound(len(data)); err != nil {
			return err
		}
		return netWriteAll(d.out, data)
	}
	var b bytes.Buffer
	// The full block opens the watch and closes it; in between the view
	// COALESCES — one appended line per poll that carries something new, so a
	// long watch reads as history instead of the same screen twenty times.
	if previous == nil || current.Receipt != nil {
		fmt.Fprintln(&b)
		writeNetInspection(&b, d.palette, d.id, current)
	} else {
		stamp := d.palette.Dim(current.ReadAt.UTC().Format(time.RFC3339))
		for _, line := range netWatchDelta(*previous, current) {
			fmt.Fprintf(&b, "%s %s\n", stamp, line)
		}
		fmt.Fprintf(&b, "%s %s\n", stamp, netWatchStatus(current))
	}
	if err := netEventBound(b.Len()); err != nil {
		return err
	}
	return netWriteAll(d.out, b.Bytes())
}

// netWatchDelta names what appeared since the last poll. A refusal or an alert
// is the reason anybody watches, so it gets its own line and its own name; the
// rest is one status line.
func netWatchDelta(previous, current networkstate.Inspection) []string {
	seen := map[string]bool{}
	for _, denial := range previous.Observed.Denials {
		seen[denial.ID] = true
	}
	// One refused name retried four times in one second is ONE line: coalesce by
	// destination and reason, and count the repeats.
	var order []string
	repeats := map[string]int{}
	for _, denial := range current.Observed.Denials {
		if seen[denial.ID] {
			continue
		}
		key := "refused " + netDestination(denial.Name, denial.Peer, denial.DestinationID) + " (" + denial.Reason + ")"
		if repeats[key] == 0 {
			order = append(order, key)
		}
		repeats[key]++
	}
	var lines []string
	for _, key := range order {
		if len(lines) >= netWatchDeltaLines {
			lines = append(lines, "…more refusals this poll; 'coop net inspect' has the full list")
			break
		}
		if repeats[key] > 1 {
			key += " ×" + strconv.Itoa(repeats[key])
		}
		lines = append(lines, key)
	}
	for _, alert := range previous.Observed.Alerts {
		seen[alert.ID] = true
	}
	for _, alert := range current.Observed.Alerts {
		if !seen[alert.ID] {
			lines = append(lines, "alert "+alert.Category+"/"+alert.Severity+" "+alert.State)
		}
	}
	return lines
}

// netWatchStatus is the one-line heartbeat. UNKNOWN stays UNKNOWN: a poll that
// measured nothing must not read as a measured zero.
func netWatchStatus(current networkstate.Inspection) string {
	status := "freshness " + current.Freshness + ", " + netCurrentSummary(current)
	if counters := current.Observed.Counters; counters != nil {
		status += ", total sent " + netCount(counters.SentBytes) + " received " + netCount(counters.ReceivedBytes)
	}
	return status
}

// netWatchFailure ends the watch on a failed update. A closed pipe means the
// reader walked away: that ends promptly and successfully, since nothing on the
// host was left half-done.
func netWatchFailure(err error) (int, error) {
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrClosed) {
		return 0, nil
	}
	return 1, err
}

// netEvidenceChanged reports whether a poll carries new evidence. The reader's
// own ReadAt is excluded: a poll that only advanced the clock is not a change,
// so an idle run stays quiet.
func netEvidenceChanged(previous, current networkstate.Inspection) bool {
	return !netSameInspection(previous, current)
}

func netSameInspection(a, b networkstate.Inspection) bool {
	a.ReadAt, b.ReadAt = time.Time{}, time.Time{}
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	if leftErr != nil || rightErr != nil {
		return false // an unencodable read is never "unchanged"; the emit path reports it
	}
	return bytes.Equal(left, right)
}

// ---------------------------------------------------------------- shared ----

// openNetEvidence opens the host's retained evidence read-only. The path is
// DERIVED, never created: a host that never ran a filtered box has no evidence
// root, and a query must not make one to report that it found nothing.
func openNetEvidence() (*networkstate.Evidence, error) {
	path, err := box.NetworkStatePath()
	if err != nil {
		return nil, err
	}
	return networkstate.OpenEvidence(path, nil)
}

func openNetRunEvidence() (*networkstate.Evidence, error) {
	evidence, err := openNetEvidence()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("no filtered run has been recorded on this host — 'coop net ls --all' lists what was")
	}
	return evidence, err
}

// netProject is the project a scoped verb acts on: the override, else the Git
// top level. Outside a project there is no scope to guess, and guessing the cwd
// would bind an approval to whatever directory the shell happened to be in.
func netProject(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	args := append(append([]string{}, forkspace.GitHardening...), "rev-parse", "--show-toplevel")
	out, err := exec.Command("git", args...).Output()
	top := strings.TrimSpace(string(out))
	if err != nil || top == "" {
		return "", errors.New("not inside a project — 'coop net ls --all' lists every recorded run")
	}
	return top, nil
}

// netRender renders a bounded human view and performs ONE checked write: a
// one-shot query that could not reach its reader must fail, not print a partial
// block and claim success.
func netRender(w io.Writer, render func(*bytes.Buffer)) error {
	var b bytes.Buffer
	render(&b)
	if err := netEventBound(b.Len()); err != nil {
		return err
	}
	return netWriteAll(w, b.Bytes())
}

func netWriteJSON(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := netEventBound(len(data)); err != nil {
		return err
	}
	return netWriteAll(w, data)
}

func netWriteAll(w io.Writer, data []byte) error {
	n, err := w.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func netEventBound(size int) error {
	if size > netEventMaxBytes {
		return fmt.Errorf("this network view is %d bytes, over the %d-byte evidence envelope", size, netEventMaxBytes)
	}
	return nil
}

func netField(w io.Writer, p ui.Palette, width int, label, value string) {
	// Pad the PLAIN label, then dim the padded cell — styling inside a width
	// field counts the escape bytes and drifts the column.
	fmt.Fprintf(w, "  %s %s\n", p.Dim(padRight(label+":", width)), value)
}

func netRow(w io.Writer, text string) { fmt.Fprintf(w, "    %s\n", text) }

func netUnknownIfEmpty(value string) string {
	if value == "" {
		return "UNKNOWN"
	}
	return value
}

// netCount renders a retained counter as a decimal string, matching how it is
// stored: a total above 2^53 must not round on its way to a reader. A nil
// counter is UNKNOWN — a metric nobody measured is not a measured zero.
func netCount(value *networkview.Count) string {
	if value == nil {
		return "UNKNOWN"
	}
	return strconv.FormatUint(uint64(*value), 10)
}

func netCurrentSummary(inspection networkstate.Inspection) string {
	if inspection.Current == nil {
		return "UNKNOWN (no fresh observation)"
	}
	rate := inspection.Current.Rate
	if rate == nil {
		return "UNKNOWN (not measured)"
	}
	return fmt.Sprintf("sent %.0f B/s, received %.0f B/s (window %dms)",
		rate.SentPerSecond, rate.ReceivedPerSecond, uint64(rate.WindowMillis))
}

func netDestination(name, peer, id string) string {
	switch {
	case name != "":
		return name
	case peer != "":
		return peer
	case id != "":
		return "destination withheld (" + id + ")"
	default:
		return "destination unknown"
	}
}

func netLossSummary(loss networkview.Loss) string {
	parts := []string{"records lost " + strconv.FormatUint(uint64(loss.Records), 10)}
	if loss.Unknown {
		parts = append(parts, "unknown loss")
	}
	if loss.DetailTruncated {
		parts = append(parts, "detail truncated")
	}
	if len(loss.Reasons) > 0 {
		parts = append(parts, strings.Join(loss.Reasons, ", "))
	}
	return strings.Join(parts, "; ")
}

func netHealthSummary(health networkview.HealthLayers) string {
	parts := make([]string, 0, 4)
	for _, layer := range []struct {
		name  string
		value networkview.Health
	}{
		{"enforcer", health.Enforcer}, {"gateway", health.Gateway},
		{"resolver", health.Resolver}, {"collector", health.Collector},
	} {
		parts = append(parts, layer.name+" "+netUnknownIfEmpty(layer.value.Status))
	}
	return strings.Join(parts, ", ")
}
