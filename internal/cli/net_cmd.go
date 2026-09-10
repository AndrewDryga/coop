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
	// netRecentRuns is how many of a project's runs `coop net` shows. It is a
	// posture view with a tail, not the listing: `coop net ls` is that.
	netRecentRuns = 5
	// netWatchDeltaLines bounds what ONE poll may append. An agent can generate
	// arbitrarily many distinct refused names; a watch is not a firehose.
	netWatchDeltaLines = 5
)

var netCommands = []string{"ls", "inspect", "watch", "receipt", "why", "explain", "approve", "setup", "recover"}

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
	case "recover":
		return a.cmdNetRecover(rest)
	default:
		return 2, unknownErr("net command", verb, netCommands)
	}
}

// cmdNetRecover settles the runs a dead supervisor left behind. It is the only
// verb besides setup/approve that changes anything, and what it changes is
// exactly one interrupted run's own resources: containers and volumes by
// recorded id and ownership labels, then a final `supervisor_lost` receipt.
func (a *app) cmdNetRecover(args []string) (int, error) {
	runID, all := "", false
	for _, arg := range args {
		switch {
		case arg == "--all":
			all = true
		case strings.HasPrefix(arg, "-"):
			return 2, unknownErr("net recover flag", arg, []string{"--all"})
		case runID != "":
			return 2, errors.New("coop net recover takes one run id, or --all")
		default:
			runID = arg
		}
	}
	if runID == "" && !all {
		return 2, errors.New("coop net recover needs a run id, or --all for every pending run")
	}
	if runID != "" && all {
		return 2, errors.New("coop net recover takes a run id or --all, not both")
	}
	if err := a.ensureRuntime(); err != nil {
		return -1, err
	}
	results, err := box.RecoverNetworkRuns(context.Background(), a.rt, runID)
	if err != nil {
		return 1, err
	}
	if len(results) == 0 {
		ui.Note("no interrupted run is waiting for cleanup")
		return 0, nil
	}
	code := 0
	for _, result := range results {
		switch {
		case result.Skipped != "":
			ui.Warn("run %s left alone — %s", result.RunID, result.Skipped)
		case len(result.Failures) != 0 || len(result.Pending) != 0:
			code = 1
			ui.Warn("run %s is not settled yet — still to clean up: %s", result.RunID, strings.Join(result.Pending, ", "))
			for _, failure := range result.Failures {
				ui.Detail("%v", failure)
			}
		case len(result.Removed) == 0:
			ui.OK("run %s was already cleaned up — nothing left to remove", result.RunID)
		default:
			ui.OK("run %s settled — removed %s it left behind", result.RunID, ui.Count(len(result.Removed), "container or volume", "containers and volumes"))
		}
		if result.Sealed {
			ui.Detail("coop net receipt %s", result.RunID)
		}
	}
	return code, nil
}

func (a *app) cmdNetSetup() (int, error) {
	if err := a.ensureRuntime(); err != nil {
		return -1, err
	}
	if err := a.rt.EnsureDaemon(); err != nil {
		return -1, err
	}
	ui.Info("setting up restricted networking for this host")
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
		ui.Warn("this project's recorded runs could not be read: %v", listErr)
	}
	return 0, nil
}

func writeNetPosture(w io.Writer, p ui.Palette, posture box.NetworkPosture, runs []netRun) {
	b := newNetBlock(w, p, "Network for "+posture.Project)
	b.field("Egress", string(posture.Mode)+" ("+netModeSource(posture.Source)+")")
	switch {
	case posture.Approval == nil:
		b.field("Approved", "nothing remembered for this project yet")
	case len(posture.Approval.Envelope) == 0:
		b.field("Approved", string(posture.Approval.Posture)+", no destinations of its own")
	default:
		b.field("Approved", string(posture.Approval.Posture)+", "+ui.Count(len(posture.Approval.Envelope), "rule"))
		for _, rule := range posture.Approval.Envelope {
			b.row(box.NetworkRuleText(rule))
		}
	}
	switch {
	case len(posture.Add) == 0 && len(posture.Remove) == 0 && len(posture.Requested) == 0:
		b.field("Requested", "nothing — .agent/project.yaml has no box.egress_rules")
	case len(posture.Add) == 0 && len(posture.Remove) == 0:
		b.field("Requested", "same as approved — nothing to review")
	default:
		pending := "not approved yet — run 'coop net approve'"
		if posture.Pending != nil {
			// The same condition Admit fails on, said once: a request outside
			// what was remembered is not a warning, it stops a launch.
			pending = "not approved yet — a filtered run refuses until you run 'coop net approve'"
		}
		b.field("Requested", pending)
		for _, rule := range posture.Add {
			b.row("+ " + box.NetworkRuleText(rule))
		}
		for _, rule := range posture.Remove {
			b.row("- " + box.NetworkRuleText(rule))
		}
	}
	switch {
	case posture.Setup == nil:
		b.field("This host", "not set up — run 'coop net setup' before a filtered run")
	case !posture.SetupCurrent():
		b.field("This host", "set up by an older coop — run 'coop net setup' again")
	default:
		b.field("This host", "set up "+posture.Setup.CompletedAt.UTC().Format(time.RFC3339))
	}
	if len(runs) == 0 {
		b.field("Recent runs", "none yet")
		b.flush(w)
		return
	}
	b.field("Recent runs", strconv.Itoa(len(runs))+", newest first")
	for _, run := range runs {
		b.row(netRunLine(p, run))
	}
	b.flush(w)
}

// netModeSource says, in the user's own words, what decided this mode. The
// constants are the posture resolver's own labels; this is the one place they
// become a sentence.
func netModeSource(source string) string {
	switch source {
	case box.PostureFromApproval:
		return "remembered for this project"
	case box.PostureFromHost:
		return "from COOP_EGRESS"
	case box.PostureFromProject:
		return "this project asks for it"
	default:
		return "coop's default"
	}
}

// netRun is one recorded run as the listing shows it: the retained summary plus
// the traffic that run actually saw.
type netRun struct {
	networkstate.ExecutionSummary
	Outcome string
}

// netRunLine is the one-line summary of a recorded run, shared by the project
// view and `ls`. A finished run says only what it did; the EXCEPTIONS — no
// receipt, cleanup still owed — are what earn a word, and neither is a claim
// that the run is still alive.
func netRunLine(p ui.Palette, run netRun) string {
	line := p.Bold(p.Cyan(run.ID)) + "  " + run.StartedAt.UTC().Format(time.RFC3339)
	var flags []string
	if !run.Final {
		flags = append(flags, "no receipt yet")
	}
	if run.CleanupPending {
		flags = append(flags, "cleanup pending")
	}
	if run.SessionID != "" {
		flags = append(flags, "session "+run.SessionID)
	}
	if len(flags) != 0 {
		line += "  " + p.Yellow(strings.Join(flags, ", "))
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
		return "nothing observed"
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
		ui.Note("no filtered runs recorded for %s — 'coop net' shows what this project may reach", scope)
	} else {
		ui.OK("%s recorded for %s", ui.Count(len(runs), "filtered run"), scope)
	}
	if page.Incomplete {
		ui.Warn("some records could not be listed, so this list is not complete")
	}
	if page.Unreadable > 0 {
		ui.Warn("%s could not be read and are not shown", ui.Count(page.Unreadable, "record"))
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
			return opts, fmt.Errorf("coop net %s reads one run, but got %q and %q", verb, opts.id, arg)
		default:
			opts.id = arg
		}
	}
	if opts.id == "" {
		return opts, fmt.Errorf("coop net %s needs a run id — list them with 'coop net ls'", verb)
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
			return 1, fmt.Errorf("run %q has no receipt yet — see what is recorded so far with 'coop net inspect %s'", opts.id, opts.id)
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
		return fmt.Errorf("no run %q was recorded here — list the runs with 'coop net ls --all'", id)
	}
	return fmt.Errorf("network run %q: %w", id, err)
}

// writeNetInspection is the human view of one run: what it was allowed to
// reach, what it actually did, and what was refused. The full coverage, loss
// and health tables live behind --json — they are an investigation, not a
// status line.
func writeNetInspection(w io.Writer, p ui.Palette, id string, inspection networkstate.Inspection) {
	observed := inspection.Observed
	b := newNetBlock(w, p, "Network run "+id)
	b.field("Egress", netUnknownIfEmpty(string(observed.Mode))+", rules "+netUnknownIfEmpty(observed.PolicyFingerprint))
	b.field("Gateway", "epoch "+netUnknownIfEmpty(observed.Epoch)+", watching "+netUnknownIfEmpty(observed.Scope))
	if observed.Sequence == 0 || observed.AsOf.IsZero() {
		never := "never — nothing was recorded for this run"
		if inspection.Freshness != "" && inspection.Freshness != networkstate.FreshnessNotObserved {
			never += " (" + inspection.Freshness + ")"
		}
		b.field("Observed", never)
	} else {
		b.field("Observed", observed.AsOf.UTC().Format(time.RFC3339)+" ("+inspection.Freshness+
			", "+netUnknownIfEmpty(observed.Availability)+")")
	}
	b.field("Read at", inspection.ReadAt.UTC().Format(time.RFC3339))
	b.field("Now", netCurrentSummary(inspection))
	if current := inspection.Current; current != nil {
		b.field("Open now", "live "+netCount(current.LiveConnections)+", unknown "+netCount(current.UnknownConnections)+
			", pending "+netCount(current.PendingConnections))
	}
	b.field("Allowed", netAllowedTotals(observed.Counters, "nothing was recorded for this run"))
	writeNetAddressGrants(b, observed)
	writeNetDenials(b, p, observed)
	if len(observed.Alerts) == 0 {
		b.field("Alerts", "none")
	} else {
		b.field("Alerts", ui.Count(len(observed.Alerts), "alert"))
		for _, alert := range observed.Alerts {
			b.row(fmt.Sprintf("%s %s/%s %s (first %s, last %s)", alert.ID, alert.Category, alert.Severity,
				alert.State, alert.FirstSeen.UTC().Format(time.RFC3339), alert.LastSeen.UTC().Format(time.RFC3339)))
		}
	}
	if observed.Loss.Unknown || observed.Loss.Records != 0 || observed.Loss.DetailTruncated {
		b.field("Missing", netLossSummary(observed.Loss))
	}
	b.field("Health", netHealthSummary(observed.Health))
	b.field("Cleanup", inspection.Cleanup)
	if inspection.Receipt == nil {
		b.field("Receipt", "not sealed yet")
	} else {
		b.field("Receipt", inspection.Receipt.Finality+", "+inspection.Receipt.Completeness)
	}
	b.flush(w)
	fmt.Fprintln(w, p.Dim("everything recorded: coop net inspect "+id+" --json"))
}

// netAllowedTotals is what got through, in the units a person reads. A nil
// counter is UNKNOWN — a metric nobody measured is not a measured zero.
func netAllowedTotals(counters *networkview.Counters, missing string) string {
	if counters == nil {
		return "UNKNOWN — " + missing
	}
	return netConnectionCount(counters.Connections) + ", " + netByteCount(counters.SentBytes) +
		" sent, " + netByteCount(counters.ReceivedBytes) + " received"
}

// netRefusedCounts is the kernel's own tally, kept apart from the decisions it
// retained detail for: a refused raw packet has no destination to name.
func netRefusedCounts(counters *networkview.Counters) string {
	if counters == nil {
		return ""
	}
	return netCount(counters.DeniedDNSQueries) + " dns, " + netCount(counters.DeniedTLS) +
		" tls, " + netCount(counters.DeniedPackets) + " raw packets"
}

// The packet filter drops a refused raw datagram without recording where it
// was going, so those packets are a count and nothing more. Saying so is the
// honest alternative to inventing a destination for them.
const netRawRefusalNote = "raw packets are counted only — the filter records no destination for them, so there is nothing to explain"

// writeNetAddressGrants shows what each raw rule actually carried. Bytes here
// are kernel packet bytes, not application payload.
func writeNetAddressGrants(b *netBlock, observed networkview.Snapshot) {
	if len(observed.AddressGrants) == 0 {
		return
	}
	b.field("Raw rules", ui.Count(len(observed.AddressGrants), "rule")+" carried traffic")
	for _, grant := range observed.AddressGrants {
		b.row(fmt.Sprintf("%s  %s, %s", grant.RuleID,
			netPlural(uint64(grant.Packets), "packet"), ui.Bytes(uint64(grant.Bytes))))
	}
}

func writeNetDenials(b *netBlock, p ui.Palette, observed networkview.Snapshot) {
	counts := netRefusedCounts(observed.Counters)
	switch {
	case len(observed.Denials) == 0 && counts == "":
		b.field("Refused", "nothing recorded")
	case len(observed.Denials) == 0:
		b.field("Refused", counts)
	default:
		if counts != "" {
			counts = " (" + counts + ")"
		}
		b.field("Refused", ui.Count(len(observed.Denials), "refusal")+counts)
	}
	for _, denial := range observed.Denials {
		b.row(fmt.Sprintf("%s %s %s — %s at %s", denial.ID, netDestination(denial.Name, denial.Peer, denial.DestinationID),
			denial.Kind, denial.Reason, denial.At.UTC().Format(time.RFC3339)))
	}
	if observed.Counters != nil && observed.Counters.DeniedPackets != nil && *observed.Counters.DeniedPackets > 0 {
		b.row(p.Dim(netRawRefusalNote))
	}
}

// writeNetReceipt renders the sealed outcome. Finality and completeness are
// INDEPENDENT: final + partial is valid after a crash, and must never read as
// a complete record.
func writeNetReceipt(w io.Writer, p ui.Palette, id string, receipt networkview.Receipt) {
	b := newNetBlock(w, p, "Receipt for run "+id)
	b.field("Sealed", receipt.Finality+", "+receipt.Completeness)
	b.field("Workload", receipt.Workload)
	b.field("Cleanup", receipt.Cleanup)
	b.field("Started", receipt.StartedAt.UTC().Format(time.RFC3339))
	if receipt.EndedAt != nil {
		b.field("Ended", receipt.EndedAt.UTC().Format(time.RFC3339))
	}
	b.field("Runtime", receipt.Runtime+", gateway "+receipt.GatewayImage)
	b.field("Collector", receipt.CollectorVersion)
	b.field("Shows", netUnknownIfEmpty(receipt.Snapshot.Projection))
	b.field("Digest", receipt.Digest)
	b.field("Allowed", netAllowedTotals(receipt.Snapshot.Counters, "no counters were sealed with this receipt"))
	writeNetAddressGrants(b, receipt.Snapshot)
	writeNetDenials(b, p, receipt.Snapshot)
	b.flush(w)
	if receipt.Snapshot.Projection != "destinations-included" {
		fmt.Fprintln(w, p.Dim("destination names are held back here — even a refused name can carry a secret; add --destinations to see them"))
	}
	fmt.Fprintln(w, p.Dim("everything recorded: coop net receipt "+id+" --json"))
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
			lines = append(lines, "…more refusals just now — 'coop net inspect' has the whole list")
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
	status := current.Freshness + ", " + netCurrentSummary(current)
	if counters := current.Observed.Counters; counters != nil {
		status += ", " + netByteCount(counters.SentBytes) + " sent and " + netByteCount(counters.ReceivedBytes) + " received so far"
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
		return nil, errors.New("no filtered run has been recorded on this host yet — start one with 'coop run --egress filtered'")
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
		return fmt.Errorf("this view is %d bytes, over the %d-byte limit — read it with --json instead", size, netEventMaxBytes)
	}
	return nil
}

// netBlock is one net view: a title and its labeled facts. The label column is
// sized to the longest label in THIS view, so a two-field block reads as tight
// as `coop credentials` instead of leaving a gap for a label it never prints.
type netBlock struct {
	p     ui.Palette
	lines []netLine
}

// netLine is either a labeled fact or a plain row indented under the last one —
// a rule, a refusal, where a grant came from.
type netLine struct{ label, value string }

func newNetBlock(w io.Writer, p ui.Palette, title string) *netBlock {
	fmt.Fprintf(w, "%s\n", p.Bold(p.Cyan(title)))
	return &netBlock{p: p}
}

func (b *netBlock) field(label, value string) {
	b.lines = append(b.lines, netLine{label: label, value: value})
}

func (b *netBlock) row(text string) { b.lines = append(b.lines, netLine{value: text}) }

// flush writes the collected fields once the widest label is known. Callers that
// print prose after the block flush first, so the two never interleave.
func (b *netBlock) flush(w io.Writer) {
	width := 0
	for _, line := range b.lines {
		if len(line.label) > width {
			width = len(line.label)
		}
	}
	for _, line := range b.lines {
		if line.label == "" {
			fmt.Fprintf(w, "    %s\n", line.value)
			continue
		}
		// Pad the PLAIN label, then dim the padded cell — styling inside a width
		// field counts the escape bytes and drifts the column.
		fmt.Fprintf(w, "  %s %s\n", b.p.Dim(padRight(line.label, width+2)), line.value)
	}
	b.lines = nil
}

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

// netPlural counts a stored counter without narrowing it to an int, so a total
// past the platform's int range still reads correctly.
func netPlural(value uint64, noun string) string {
	if value == 1 {
		return "1 " + noun
	}
	return strconv.FormatUint(value, 10) + " " + noun + "s"
}

func netConnectionCount(value *networkview.Count) string {
	if value == nil {
		return "UNKNOWN connections"
	}
	return netPlural(uint64(*value), "connection")
}

func netByteCount(value *networkview.Count) string {
	if value == nil {
		return "UNKNOWN"
	}
	return ui.Bytes(uint64(*value))
}

func netCurrentSummary(inspection networkstate.Inspection) string {
	if inspection.Current == nil {
		return "UNKNOWN — nothing read recently"
	}
	rate := inspection.Current.Rate
	if rate == nil {
		return "UNKNOWN — not measured"
	}
	return fmt.Sprintf("%s/s out, %s/s in (over %dms)",
		ui.Bytes(uint64(rate.SentPerSecond)), ui.Bytes(uint64(rate.ReceivedPerSecond)), uint64(rate.WindowMillis))
}

func netDestination(name, peer, id string) string {
	switch {
	case name != "":
		return name
	case peer != "":
		return peer
	case id != "":
		return "name withheld (" + id + ")"
	default:
		return "unknown destination"
	}
}

func netLossSummary(loss networkview.Loss) string {
	parts := []string{netPlural(uint64(loss.Records), "record") + " lost"}
	if loss.Unknown {
		parts = append(parts, "some loss is unknown")
	}
	if loss.DetailTruncated {
		parts = append(parts, "detail was truncated")
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
