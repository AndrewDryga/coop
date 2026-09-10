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
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/ui"
)

// The `coop net` reads answer from retained host evidence and nothing else: no
// container, no daemon probe, no DNS lookup, no policy change, and no directory
// created to report that nothing was found. `setup`, `approve` and `forget`
// write, and `recover` — explicit, or coop's own bounded attempt before it
// reports an interrupted run's cleanup as incomplete — removes exactly what
// one dead run recorded.

const (
	// netEventMaxBytes bounds ONE rendered view or watch event — not the stream,
	// since a long watch on a busy run is honest work. It matches the retained
	// record envelope the evidence is read from, so an event over it means the
	// evidence is anomalous, not that the run was too busy.
	netEventMaxBytes = 4 << 20
	netWatchPoll     = time.Second
	// netRecentRuns is how many of a project's runs `coop net runs` shows by
	// default: the ones anybody asks about, not the whole history.
	netRecentRuns = 5
	// netWatchDeltaLines bounds what ONE poll may append. An agent can generate
	// arbitrarily many distinct refused names; a watch is not a firehose.
	netWatchDeltaLines = 5
)

// netCommands is the family in the order its help groups them: what a new run
// may reach, what recorded runs did, and the repair coop normally does itself.
var netCommands = []string{"approve", "check", "forget", "runs", "inspect", "explain", "watch", "export", "setup", "recover"}

// cmdNet routes the restricted-networking family. Bare `coop net` is this
// project's access — what a new run may reach and why — not a listing: `runs`
// owns history.
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
	case "runs":
		return a.cmdNetRuns(rest)
	case "inspect", "watch":
		return a.cmdNetRun(verb, rest)
	case "export":
		return a.cmdNetExport(rest)
	case "check", "explain":
		return a.netDiagnostic(verb, rest)
	case "approve":
		return a.cmdNetApprove(rest)
	case "forget":
		return a.cmdNetForget(rest)
	case "recover":
		return a.cmdNetRecover(rest)
	default:
		return 2, unknownErr("net command", verb, netCommands)
	}
}

// cmdNetRecover settles the runs a dead supervisor left behind, on demand.
// What it changes is exactly one interrupted run's own resources: containers
// and volumes by recorded id and ownership labels, then a final
// `supervisor_lost` receipt. `coop net inspect` makes the same bounded attempt
// on its own before it reports a cleanup as incomplete.
func (a *app) cmdNetRecover(args []string) (int, error) {
	ref := ""
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "-"):
			return 2, fmt.Errorf("coop net recover takes a run, or nothing for every interrupted run — not %q", arg)
		case ref != "":
			return 2, errors.New("coop net recover takes one run, or nothing for every interrupted run")
		default:
			ref = arg
		}
	}
	runID := ""
	if ref != "" {
		page, err := netExecutions()
		if err != nil {
			return 1, err
		}
		if runID, err = netResolveRun(page, ref); err != nil {
			return 1, err
		}
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
		id := netShortID(result.RunID)
		switch {
		case result.Skipped != "":
			ui.Warn("run %s left alone — %s", id, result.Skipped)
		case len(result.Failures) != 0 || len(result.Pending) != 0:
			code = 1
			ui.Warn("run %s is not settled yet — still to clean up: %s", id, strings.Join(result.Pending, ", "))
			for _, failure := range result.Failures {
				ui.Detail("%v", failure)
			}
		case len(result.Removed) == 0:
			ui.OK("run %s was already cleaned up — nothing left to remove", id)
		default:
			ui.OK("run %s settled — removed %s it left behind", id, ui.Count(len(result.Removed), "container or volume", "containers and volumes"))
		}
		if result.Sealed {
			ui.Detail("coop net inspect %s", id)
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
	return 0, nil
}

// ---------------------------------------------------------------- access ----

func (a *app) cmdNetPosture() (int, error) {
	repo, err := netProject(a.cfg.RepoOverride)
	if err != nil {
		return 1, err
	}
	posture, err := box.ProjectNetworkPosture(context.Background(), a.cfg, repo)
	if err != nil {
		return 1, err
	}
	p := ui.For(os.Stdout)
	return 0, netRender(os.Stdout, func(b *bytes.Buffer) { writeNetPosture(b, p, posture) })
}

// writeNetPosture answers the one question a person arrives with: what can a
// new run reach, and why. It says what decided the mode, names provider access
// in one example, lists approved project access only when there is any, and
// shows a pending request only when one exists. Healthy setup and run history
// cost no lines: setup is machinery and `coop net runs` owns history.
func writeNetPosture(w io.Writer, p ui.Palette, posture box.NetworkPosture) {
	fmt.Fprintf(w, "%s\n  %s\n\n", p.Bold(p.Cyan("Network access for "+filepath.Base(posture.Project))), p.Dim(posture.Project))
	fmt.Fprintln(w, netModeSentence(posture.Mode, posture.Source))
	switch posture.Mode {
	case egress.Filtered:
		fmt.Fprintln(w, "When you start an agent, it can reach its provider — for example, Claude can reach Anthropic.")
		fmt.Fprintln(w, "Everything else is blocked.")
	case egress.None:
		fmt.Fprintln(w, "No external destination is reachable, so an agent cannot reach its provider.")
	}
	pending := netApprovalPending(posture)
	var approved []egress.Rule
	if posture.Approval != nil {
		approved = posture.Approval.Envelope
	}
	if posture.Mode == egress.Filtered && len(approved) != 0 && !pending {
		fmt.Fprintln(w, "\nThis project can also reach:")
		for _, rule := range approved {
			fmt.Fprintf(w, "  %s\n", netRuleText(rule))
		}
	}
	if pending {
		// The same condition a launch refuses on, said where the operator can
		// act: the request and its approval differ, so no new run starts.
		fmt.Fprintf(w, "\n%s %s\n", p.Yellow("⚠"), p.Yellow("New runs cannot start until this project's network request is approved"))
		if posture.Pending != nil && len(posture.Add) == 0 && len(posture.Remove) == 0 {
			fmt.Fprintf(w, "  %s\n", posture.Pending.Error())
		}
		writeNetRuleDiff(w, p, approved, posture.Add, posture.Remove)
		fmt.Fprintf(w, "  Review it: %s\n", p.Cyan("coop net approve"))
	}
	// Host qualification is machinery, not a decision, so a current record is
	// silent. A host that cannot run a filtered box yet is the exception.
	switch {
	case posture.Mode != egress.Filtered:
	case posture.Setup == nil:
		fmt.Fprintf(w, "\n%s %s\n  %s\n", p.Yellow("⚠"), p.Yellow("This host is not set up for filtered runs yet"), "Prepare it once: coop net setup")
	case !posture.SetupCurrent():
		fmt.Fprintf(w, "\n%s %s\n  %s\n", p.Yellow("⚠"), p.Yellow("This host was set up by an older coop"), "Prepare it again: coop net setup")
	}
}

// netApprovalPending is the one condition a launch refuses on: the project's
// request and its approval differ, or something no rule diff can show — a
// replaced directory, a drifted service — needs review.
func netApprovalPending(posture box.NetworkPosture) bool {
	return posture.Pending != nil || len(posture.Add) != 0 || len(posture.Remove) != 0
}

// netModeSentence is the causal sentence: the mode in human words, and the
// input that decided it. The constants are the posture resolver's own labels;
// this is the one place they become prose.
func netModeSentence(mode egress.Mode, source string) string {
	access := "use filtered internet access"
	switch mode {
	case egress.Open:
		access = "have unrestricted internet access"
	case egress.None:
		access = "are offline"
	}
	cause := "because that is coop's default"
	switch source {
	case box.PostureFromApproval:
		cause = "because that is what was approved for this project"
	case box.PostureFromHost:
		cause = "because COOP_EGRESS says so"
	case box.PostureFromProject:
		cause = "because .agent/project.yaml says so"
	}
	return "New runs " + access + " " + cause + "."
}

// writeNetRuleDiff is the one shape a rule review takes: the approved baseline,
// the additions and the removals, each row saying what it is. Rows sort by the
// rule text so the same request always reads the same. Additions are yellow
// because they expand access, removals dim because they reduce it — never
// green for a new grant, and the markers carry the meaning without color.
func writeNetRuleDiff(w io.Writer, p ui.Palette, approved, add, remove []egress.Rule) {
	type row struct{ marker, text, label string }
	var rows []row
	for _, rule := range approved {
		if !slices.ContainsFunc(remove, func(r egress.Rule) bool { return netRuleText(r) == netRuleText(rule) }) {
			rows = append(rows, row{" ", netRuleText(rule), "already approved"})
		}
	}
	for _, rule := range add {
		rows = append(rows, row{"+", netRuleText(rule), "new request"})
	}
	for _, rule := range remove {
		rows = append(rows, row{"-", netRuleText(rule), "no longer requested"})
	}
	if len(rows) == 0 {
		return
	}
	slices.SortStableFunc(rows, func(x, y row) int { return strings.Compare(x.text, y.text) })
	width := 0
	for _, r := range rows {
		width = max(width, utf8.RuneCountInString(r.text))
	}
	fmt.Fprintln(w, "  Access:")
	for _, r := range rows {
		// Pad the PLAIN text, then style the finished line.
		line := "    " + r.marker + " " + padRight(r.text, width+2) + " " + r.label
		switch r.marker {
		case "+":
			line = p.Yellow(line)
		default:
			line = p.Dim(line)
		}
		fmt.Fprintln(w, line)
	}
}

// netRuleText renders a rule the way a destination row reads — the name or
// address, its ports, and the transport — so what a run reached and what a
// project may reach look like the same thing.
func netRuleText(rule egress.Rule) string {
	var target string
	switch {
	case rule.To.Domain != "":
		target = rule.To.Domain
	case rule.To.IP != "":
		target = rule.To.IP
	case rule.To.CIDR != "":
		target = rule.To.CIDR
	case rule.To.Service != "":
		target = "service " + rule.To.Service
	case rule.To.Provider != "":
		target = rule.To.Provider + " provider access"
		if len(rule.To.Features) != 0 {
			target += " (" + strings.Join(rule.To.Features, ", ") + ")"
		}
		return target
	default:
		target = "(no destination)"
	}
	if len(rule.Ports) != 0 {
		target += ":" + netPortList(rule.Ports)
	}
	if rule.Protocol != "" {
		target += " · " + netTransportLabel(rule.Protocol)
	}
	if len(rule.Types) != 0 {
		target += " " + strings.Join(rule.Types, ",")
	}
	return target
}

// ------------------------------------------------------------------ runs ----

type netRunsOptions struct{ all, allProjects, json bool }

func parseNetRunsFlags(args []string) (netRunsOptions, error) {
	var opts netRunsOptions
	for _, arg := range args {
		switch arg {
		case "--all":
			opts.all = true
		case "--all-projects":
			opts.allProjects = true
		case "--json":
			opts.json = true
		default:
			return opts, unknownErr("net runs flag", arg, []string{"--all", "--all-projects", "--json"})
		}
	}
	if opts.all && opts.allProjects {
		return opts, errors.New("--all is every run of this project and --all-projects is every project's — pass one")
	}
	return opts, nil
}

// netRun is one recorded run as the listing shows it: the retained summary plus
// what that run actually did, read from its own evidence.
type netRun struct {
	networkstate.ExecutionSummary
	Outcome string
}

func (a *app) cmdNetRuns(args []string) (int, error) {
	opts, err := parseNetRunsFlags(args)
	if err != nil {
		return 2, err
	}
	page, err := netExecutions()
	if err != nil {
		return 1, err
	}
	runs, project := page.Executions, ""
	if !opts.allProjects {
		repo, err := netProject(a.cfg.RepoOverride)
		if err != nil {
			return 1, err
		}
		project = netResolvedPath(repo)
		runs = netFilterProject(runs, project)
	}
	total := len(runs)
	if !opts.all && !opts.allProjects && len(runs) > netRecentRuns {
		runs = runs[:netRecentRuns]
	}
	if opts.json {
		return 0, netWriteJSON(os.Stdout, netListingDTO(runs, page))
	}
	listed, err := netRunOutcomes(runs)
	if err != nil {
		return 1, err
	}
	p, now := ui.For(os.Stdout), time.Now()
	if err := netRender(os.Stdout, func(b *bytes.Buffer) { writeNetRuns(b, p, now, project, listed, total, opts) }); err != nil {
		return 1, err
	}
	if page.Incomplete {
		ui.Warn("some records could not be listed, so this list is not complete")
	}
	if page.Unreadable > 0 {
		ui.Warn("%s could not be read and are not shown", ui.Count(page.Unreadable, "record"))
	}
	return 0, nil
}

// writeNetRuns is the bounded history: one row per run, newest first, with the
// short id every run command accepts. The cross-project view labels each
// project by name and path so no run is unattributed.
func writeNetRuns(w io.Writer, p ui.Palette, now time.Time, project string, runs []netRun, total int, opts netRunsOptions) {
	if opts.allProjects {
		fmt.Fprintf(w, "%s\n", p.Bold(p.Cyan("Network runs on this host")))
		if len(runs) == 0 {
			fmt.Fprintln(w, "\n  no filtered run has been recorded on this host yet")
			return
		}
		var order []string
		byProject := map[string][]netRun{}
		for _, run := range runs {
			if _, seen := byProject[run.Project]; !seen {
				order = append(order, run.Project)
			}
			byProject[run.Project] = append(byProject[run.Project], run)
		}
		for _, path := range order {
			fmt.Fprintf(w, "\n%s\n  %s\n", p.Bold(filepath.Base(path)), p.Dim(path))
			writeNetRunRows(w, p, now, byProject[path])
		}
		return
	}
	fmt.Fprintf(w, "%s\n\n", p.Bold(p.Cyan("Recent network runs for "+filepath.Base(project))))
	if len(runs) == 0 {
		fmt.Fprintln(w, "  no filtered run has been recorded for this project yet")
		return
	}
	writeNetRunRows(w, p, now, runs)
	if total > len(runs) {
		fmt.Fprintf(w, "\n%s\n", p.Dim(fmt.Sprintf("Showing %d of %d · coop net runs --all", len(runs), total)))
	}
}

func writeNetRunRows(w io.Writer, p ui.Palette, now time.Time, runs []netRun) {
	width := 0
	for _, run := range runs {
		width = max(width, len(netWhen(now, run.StartedAt)))
	}
	for _, run := range runs {
		line := "  " + p.Bold(p.Cyan(netShortID(run.ID))) + "  " + padRight(netWhen(now, run.StartedAt), width) + "   " + run.Outcome
		// A finished run says only what it did. The EXCEPTIONS — no receipt,
		// cleanup still owed — earn a word, and neither claims the run is alive.
		var flags []string
		if !run.Final {
			flags = append(flags, "no receipt yet")
		}
		if run.CleanupPending {
			flags = append(flags, "cleanup pending")
		}
		if len(flags) != 0 {
			line += " · " + p.Yellow(strings.Join(flags, " · "))
		}
		fmt.Fprintln(w, line)
	}
}

// netWhen says when a run started the way a person reads a clock: today's and
// yesterday's runs by time of day, older ones by date, all in the reader's
// zone.
func netWhen(now, at time.Time) string {
	at, now = at.In(now.Location()), now.In(now.Location())
	day := func(t time.Time) string { return t.Format("2006-01-02") }
	switch day(at) {
	case day(now):
		return "today " + at.Format("15:04")
	case day(now.AddDate(0, 0, -1)):
		return "yesterday " + at.Format("15:04")
	}
	return at.Format("2006-01-02 15:04")
}

// netRunOutcome is what got through and what did not, from that run's own
// retained evidence. The two counts are different units and are never added
// together; a measured zero says so in words, an unmeasured one stays UNKNOWN.
func netRunOutcome(inspection networkstate.Inspection) string {
	observed := inspection.Observed
	if observed.Sequence == 0 {
		return "nothing observed"
	}
	allowed := "UNKNOWN connections"
	if counters := observed.Counters; counters != nil {
		if counters.Connections != nil && *counters.Connections == 0 {
			allowed = "no external connections"
		} else {
			allowed = netConnectionCount(counters.Connections)
		}
	}
	if blocked := len(observed.Denials); blocked != 0 {
		text := strconv.Itoa(blocked)
		if observed.Loss.DetailTruncated {
			text += "+" // the ring dropped detail; this is a lower bound
		}
		return allowed + " · " + text + " blocked"
	}
	if counters := observed.Counters; counters != nil && counters.DeniedPackets != nil && *counters.DeniedPackets != 0 {
		return allowed + " · " + netPlural(uint64(*counters.DeniedPackets), "raw packet") + " blocked"
	}
	return allowed
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
	return netExecutionsOf(evidence)
}

func netExecutionsOf(evidence *networkstate.Evidence) (networkstate.ExecutionPage, error) {
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

// ------------------------------------------------------------- identity ----

// netResolveRun turns the run a human typed into the one full id it names. A
// full id passes through; a shorter hex prefix must match exactly one recorded
// run — a collision is refused with the prefixes that would settle it, never
// guessed. --json keeps full ids, so automation is never asked to guess either.
func netResolveRun(page networkstate.ExecutionPage, ref string) (string, error) {
	if netEvidenceID(ref) {
		return ref, nil
	}
	if ref == "" || len(ref) > 32 || strings.Trim(ref, "0123456789abcdef") != "" {
		return "", fmt.Errorf("%q is not a run id — 'coop net runs' shows them", ref)
	}
	var matches []string
	for _, run := range page.Executions {
		if strings.HasPrefix(run.ID, ref) {
			matches = append(matches, run.ID)
		}
	}
	switch {
	case len(matches) == 1 && !page.Incomplete:
		return matches[0], nil
	case len(matches) == 0 && !page.Incomplete:
		return "", fmt.Errorf("no recorded run starts with %q — 'coop net runs' shows them", ref)
	case page.Incomplete:
		return "", errors.New("the list of recorded runs could not be read whole, so a prefix cannot be trusted — use the full run id")
	}
	prefixes := make([]string, 0, len(matches))
	for _, id := range matches {
		prefixes = append(prefixes, netUniquePrefix(id, page.Executions))
	}
	return "", fmt.Errorf("%q matches %d runs — say which: %s", ref, len(matches), strings.Join(prefixes, ", "))
}

// netUniquePrefix is the shortest display prefix that names this run alone
// among the recorded ones, never shorter than the ordinary short id.
func netUniquePrefix(id string, runs []networkstate.ExecutionSummary) string {
	for n := netShortIDLen; n < len(id); n++ {
		prefix := id[:n]
		unique := !slices.ContainsFunc(runs, func(run networkstate.ExecutionSummary) bool {
			return run.ID != id && strings.HasPrefix(run.ID, prefix)
		})
		if unique {
			return prefix
		}
	}
	return id
}

// netSelectRun picks the run a verb acts on when none was named: the newest
// recorded for this project, or for a watch the one that is still active. It
// never picks across projects and never picks among several active runs.
func (a *app) netSelectRun(verb string, page networkstate.ExecutionPage, ref string) (string, error) {
	if ref != "" {
		return netResolveRun(page, ref)
	}
	repo, err := netProject(a.cfg.RepoOverride)
	if err != nil {
		return "", fmt.Errorf("coop net %s needs a run outside a project — 'coop net runs --all-projects' shows them", verb)
	}
	if page.Incomplete {
		return "", errors.New("the list of recorded runs could not be read whole — name the run")
	}
	runs := netFilterProject(page.Executions, netResolvedPath(repo))
	if verb == "watch" {
		var active, prefixes []string
		for _, run := range runs {
			if !run.Final {
				active = append(active, run.ID)
				prefixes = append(prefixes, netUniquePrefix(run.ID, page.Executions))
			}
		}
		switch len(active) {
		case 0:
			return "", errors.New("no filtered run is active in this project — 'coop net inspect' shows the newest recorded one")
		case 1:
			return active[0], nil
		}
		return "", fmt.Errorf("%d runs are active in this project — say which: %s", len(active), strings.Join(prefixes, ", "))
	}
	if len(runs) == 0 {
		return "", errors.New("no filtered run has been recorded for this project yet — 'coop net runs --all-projects' shows every project's")
	}
	return runs[0].ID, nil
}

// ------------------------------------------------------------- single run ---

type netRunOptions struct {
	json bool
	id   string
}

func parseNetRunArgs(verb string, args []string) (netRunOptions, error) {
	var opts netRunOptions
	for _, arg := range args {
		switch {
		case arg == "--json":
			opts.json = true
		case strings.HasPrefix(arg, "-"):
			return opts, unknownErr("net "+verb+" flag", arg, []string{"--json"})
		case opts.id != "":
			return opts, fmt.Errorf("coop net %s reads one run, but got %q and %q", verb, opts.id, arg)
		default:
			opts.id = arg
		}
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
	page, err := netExecutionsOf(evidence)
	if err != nil {
		return 1, err
	}
	id, err := a.netSelectRun(verb, page, opts.id)
	if err != nil {
		return 1, err
	}
	if verb == "watch" {
		return netWatch(evidence, id, opts.json)
	}
	// The local operator view: this is the host, not an outbound export, so
	// destination names are not withheld from the person who ran the box.
	inspection, err := evidence.Inspect(id, time.Now(), true)
	if err != nil {
		return 1, netRunErr(id, err)
	}
	if opts.json {
		return 0, netWriteJSON(os.Stdout, inspection)
	}
	view := netRunView{ID: id}
	// A run whose cleanup is still owed and that nothing is observing is what
	// recovery exists for. Coop tries once itself, then reads the run again, so
	// a dead supervisor's leftovers are reported only when something external
	// kept coop from settling them.
	if inspection.Cleanup == "pending" && !netRunLive(inspection) {
		view.Cleanup = a.netSettleCleanup(id)
		if again, err := evidence.Inspect(id, time.Now(), true); err == nil {
			inspection = again
		}
	}
	p := ui.For(os.Stdout)
	return 0, netRender(os.Stdout, func(b *bytes.Buffer) { writeNetRun(b, p, view, inspection) })
}

// netSettleCleanup is coop's one bounded recovery attempt for a run. It goes
// through the same ownership-checked path as `coop net recover` — a departed
// supervisor, the exact daemon the run used, removals by recorded id — and
// reports what it learned in the operator's terms. There is no retry loop here:
// the next inspect, loop or fork start makes the next attempt.
func (a *app) netSettleCleanup(id string) netCleanup {
	if err := a.ensureRuntime(); err != nil {
		return netCleanup{Blocker: err.Error(), Remedy: "Coop will retry automatically once a container runtime is available"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), orphanSweepTimeout)
	defer cancel()
	results, err := box.RecoverNetworkRuns(ctx, a.rt, id)
	if err != nil {
		return netCleanup{Blocker: err.Error(), Remedy: "Coop will retry automatically"}
	}
	for _, result := range results {
		if result.RunID == id {
			return netCleanupOf(result)
		}
	}
	return netCleanup{}
}

// netCleanupOf translates a recovery result. A live supervisor means cleanup is
// that process's job and nothing is owed yet; anything else that stopped the
// attempt is named as it is, with the one thing only the operator can do.
func netCleanupOf(result box.NetworkRecovery) netCleanup {
	switch {
	case result.Live:
		return netCleanup{Expected: true}
	case result.Skipped != "":
		// A daemon that is stopped, and a daemon replaced by another at the
		// recorded endpoint, are both "that daemon is not there"; the Skipped
		// text says which.
		return netCleanup{Blocker: result.Skipped, Remedy: "Coop will retry automatically once that Docker daemon is back"}
	case len(result.Failures) != 0:
		return netCleanup{Blocker: result.Failures[0].Error(), Remedy: "Coop will retry automatically"}
	case len(result.Pending) != 0:
		return netCleanup{Blocker: "still to remove: " + strings.Join(result.Pending, ", "), Remedy: "Coop will retry automatically"}
	}
	return netCleanup{}
}

func netRunErr(id string, err error) error {
	if errors.Is(err, networkstate.ErrEvidenceUnavailable) || errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no run %q was recorded here — 'coop net runs' shows the recorded ones", id)
	}
	return fmt.Errorf("network run %q: %w", id, err)
}

// ---------------------------------------------------------------- export ----

// cmdNetExport writes the sealed record as the shareable JSON artifact. Its
// default projection is the redacted one: even a blocked name can encode a
// secret. --include-destinations is the operator saying, on their own machine,
// that they want the names and addresses in it.
func (a *app) cmdNetExport(args []string) (int, error) {
	ref, include := "", false
	for _, arg := range args {
		switch {
		case arg == "--include-destinations":
			include = true
		case strings.HasPrefix(arg, "-"):
			return 2, unknownErr("net export flag", arg, []string{"--include-destinations"})
		case ref != "":
			return 2, fmt.Errorf("coop net export writes one run, but got %q and %q", ref, arg)
		default:
			ref = arg
		}
	}
	if ref == "" {
		return 2, errors.New("coop net export needs a run — 'coop net runs' shows them")
	}
	evidence, err := openNetRunEvidence()
	if err != nil {
		return 1, err
	}
	defer evidence.Close()
	page, err := netExecutionsOf(evidence)
	if err != nil {
		return 1, err
	}
	id, err := netResolveRun(page, ref)
	if err != nil {
		return 1, err
	}
	inspection, err := evidence.Inspect(id, time.Now(), include)
	if err != nil {
		return 1, netRunErr(id, err)
	}
	if inspection.Receipt == nil {
		return 1, fmt.Errorf("run %s has no sealed record yet — 'coop net inspect %s' shows what is recorded so far", netShortID(id), netShortID(id))
	}
	return 0, netWriteJSON(os.Stdout, inspection.Receipt)
}

// ----------------------------------------------------------------- watch ----

// netWatch follows one run until its receipt is sealed. The human view APPENDS
// what newly happened and never repaints; --json emits one bounded NDJSON
// snapshot per change, each carrying its own freshness and loss.
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

// netWatchEmit writes one step of the stream. The human stream opens with the
// heading and whatever the run already did, appends only what is new on every
// later change, and closes with the shared run projection exactly once — no
// heartbeat, no status ledger, no repeated report.
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
	switch {
	case current.Receipt != nil:
		if previous != nil {
			fmt.Fprintln(&b)
		}
		writeNetRun(&b, d.palette, netRunView{ID: d.id}, current)
	case previous == nil:
		fmt.Fprintf(&b, "%s\n", d.palette.Bold(d.palette.Cyan("Network run "+d.id)))
		fmt.Fprintf(&b, "%s\n", d.palette.Dim("● Live — following until the record is sealed; Ctrl-C stops reading and changes nothing"))
		netWatchAppend(&b, d.palette, current.ReadAt, netWatchDelta(networkstate.Inspection{}, current))
	default:
		netWatchAppend(&b, d.palette, current.ReadAt, netWatchDelta(*previous, current))
	}
	if err := netEventBound(b.Len()); err != nil {
		return err
	}
	return netWriteAll(d.out, b.Bytes())
}

func netWatchAppend(b *bytes.Buffer, p ui.Palette, at time.Time, lines []string) {
	stamp := p.Dim(at.UTC().Format("15:04:05"))
	for _, line := range lines {
		fmt.Fprintf(b, "%s %s\n", stamp, line)
	}
}

// netWatchDelta names what appeared since the last poll: connections the
// workload opened, attempts that were blocked, alerts that fired. Repeats of
// one destination in one poll are ONE counted line, and each kind is bounded.
func netWatchDelta(previous, current networkstate.Inspection) []string {
	seen := map[string]bool{}
	for _, c := range previous.Observed.Connections {
		seen[c.ID] = true
	}
	var lines []string
	fresh := networkview.Snapshot{}
	for _, c := range current.Observed.Connections {
		if !seen[c.ID] && c.NameSource == netWorkloadName {
			fresh.Connections = append(fresh.Connections, c)
		}
	}
	var opened []string
	for _, group := range netWorkloadDestinations(fresh) {
		count := 0
		for _, peer := range group.peers {
			count += peer.connections
		}
		line := "allowed " + group.label()
		if count > 1 {
			line += " ×" + strconv.Itoa(count)
		}
		opened = append(opened, line)
	}
	lines = append(lines, netWatchBound(opened, "…more connections just now")...)
	for _, denial := range previous.Observed.Denials {
		seen[denial.ID] = true
	}
	for _, denial := range current.Observed.Denials {
		if !seen[denial.ID] {
			fresh.Denials = append(fresh.Denials, denial)
		}
	}
	var blocked []string
	for _, group := range netRefusalGroups(fresh.Denials) {
		line := "blocked " + group.label
		if group.count > 1 {
			line += " ×" + strconv.Itoa(group.count)
		}
		blocked = append(blocked, line)
	}
	lines = append(lines, netWatchBound(blocked, "…more blocked attempts just now")...)
	for _, alert := range previous.Observed.Alerts {
		seen[alert.ID] = true
	}
	for _, alert := range current.Observed.Alerts {
		if !seen[alert.ID] {
			lines = append(lines, "alert: "+netAlertText(alert))
		}
	}
	return lines
}

func netWatchBound(lines []string, more string) []string {
	if len(lines) <= netWatchDeltaLines {
		return lines
	}
	return append(lines[:netWatchDeltaLines], more+" — 'coop net inspect' has the whole list")
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
		return "", errors.New("not inside a project — 'coop net runs --all-projects' shows every recorded run")
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
// sized to the longest label in THIS view, so a one-field block reads as tight
// as `coop credentials` instead of leaving a gap for a label it never prints.
type netBlock struct {
	p     ui.Palette
	lines []netLine
}

// netLine is either a labeled fact or a plain row indented under the last one —
// a rule, a refusal, a destination and the peers beneath it. depth is in
// two-space steps, so a destination's peers sit one level under their host.
type netLine struct {
	label, value string
	depth        int
}

func newNetBlock(w io.Writer, p ui.Palette, title string) *netBlock {
	fmt.Fprintf(w, "%s\n", p.Bold(p.Cyan(title)))
	return netBlockOf(p)
}

// netBlockOf is the same block without a title, for a view that writes its own
// heading — the run projection prints a live-context line under its own.
func netBlockOf(p ui.Palette) *netBlock { return &netBlock{p: p} }

func (b *netBlock) field(label, value string) {
	b.lines = append(b.lines, netLine{label: label, value: value})
}

func (b *netBlock) row(text string) { b.rowAt(2, text) }

func (b *netBlock) rowAt(depth int, text string) {
	b.lines = append(b.lines, netLine{value: text, depth: depth})
}

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
			fmt.Fprintf(w, "%s%s\n", strings.Repeat("  ", line.depth), line.value)
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

// netPlural counts a stored counter without narrowing it to an int, so a total
// past the platform's int range still reads correctly.
func netPlural(value uint64, noun string) string {
	if value == 1 {
		return "1 " + noun
	}
	return strconv.FormatUint(value, 10) + " " + noun + "s"
}

// A nil counter is UNKNOWN — a metric nobody measured is not a measured zero.
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

func netDestination(name, peer, id string) string {
	switch {
	case name != "":
		return name
	case peer != "":
		return peer
	case id != "":
		return "name withheld (" + netShortID(id) + ")"
	default:
		return "unknown destination"
	}
}

func netPortList(ports []int) string {
	out := make([]string, 0, len(ports))
	for _, port := range ports {
		out = append(out, strconv.Itoa(port))
	}
	return strings.Join(out, ",")
}
