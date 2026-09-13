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
	"github.com/AndrewDryga/coop/internal/networkreport"
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
	netRecentRuns = 25
	// netWatchDeltaLines bounds what ONE poll may append. An agent can generate
	// arbitrarily many distinct refused names; a watch is not a firehose.
	netWatchDeltaLines = 5
)

// netCommands is the family in the order its help groups them: what a new run
// may reach, what recorded runs did, and the repair coop normally does itself.
var netCommands = []string{"approve", "check", "forget", "runs", "inspect", "blocked", "watch", "export", "setup", "recover"}

// cmdNet routes the restricted-networking family. Bare `coop net` is this
// project's access — what a new run may reach and why — not a listing: `runs`
// owns history.
func (a *app) cmdNet(args []string) (int, error) {
	if len(args) == 0 {
		return a.cmdNetAccess()
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
	case "check", "blocked":
		return a.netDiagnostic(verb, rest)
	case "approve":
		return a.cmdNetApprove(rest)
	case "forget":
		return a.cmdNetForget(rest)
	case "recover":
		return a.cmdNetRecover(rest)
	default:
		return 2, unknownSubcommandErr("net", verb, netCommands)
	}
}

// cmdNetRecover settles the runs a dead supervisor left behind, on demand.
// What it changes is exactly one interrupted run's own resources: containers
// and volumes by recorded id and ownership labels, then a final
// `supervisor_lost` receipt. `coop net inspect` makes the same bounded attempt
// on its own before it reports a cleanup as incomplete.
func (a *app) cmdNetRecover(args []string) (int, error) {
	const command, usage = "coop net recover", "coop net recover [<run>]"
	ref := ""
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "-"):
			return 2, unknownOptionErr(arg, command, nil)
		case ref != "":
			return 2, ui.UnexpectedArgument(arg, command, usage)
		default:
			ref = arg
		}
	}
	page, err := netExecutions()
	if err != nil {
		return 1, err
	}
	runID := ""
	if ref != "" {
		if runID, err = netResolveRun(page, ref, command); err != nil {
			return 1, err
		}
	}
	needsRuntime, found := page.Incomplete, runID == ""
	for _, run := range page.Executions {
		if runID != "" && run.ID != runID {
			continue
		}
		found = true
		needsRuntime = needsRuntime || run.CleanupPending
	}
	if !found && !page.Incomplete {
		return 1, netUnknownRunErr(ref)
	}
	if !needsRuntime {
		ui.Note("No interrupted network runs need cleanup.")
		return 0, nil
	}
	if err := a.ensureRuntime(); err != nil {
		return -1, err
	}
	results, err := box.RecoverNetworkRuns(context.Background(), a.rt, runID)
	if err != nil {
		return 1, err
	}
	if len(results) == 0 {
		ui.Note("No interrupted network runs need cleanup.")
		return 0, nil
	}
	code := 0
	p := ui.For(os.Stdout)
	for _, result := range results {
		if err := netRender(os.Stdout, func(b *bytes.Buffer) { writeNetRecovery(b, p, result) }); err != nil {
			return 1, err
		}
		if len(result.Failures) != 0 || len(result.Pending) != 0 {
			code = 1
		}
	}
	return code, nil
}

// writeNetRecovery reports what one pass settled. A live or unverifiable
// supervisor is not a failure — its cleanup is that process's job — and an
// external blocker names the one thing only the operator can do about it.
func writeNetRecovery(w io.Writer, p ui.Palette, result box.NetworkRecovery) {
	id := networkreport.ShortID(result.RunID)
	switch {
	case result.Live:
		fmt.Fprintf(w, "Network run %s was left alone.\n\n  %s\n", id,
			"Its supervising process is still running, or Coop could not verify that it stopped.")
	case result.Skipped != "":
		writeNetCleanupIncomplete(w, p, id, []string{netSentence(netCleanupBlocker(result.Skipped))}, netCleanupRemedy(result.Skipped))
	case len(result.Failures) != 0:
		causes := make([]string, 0, len(result.Failures))
		for _, failure := range result.Failures {
			causes = append(causes, netSentence(failure.Error()))
		}
		writeNetCleanupIncomplete(w, p, id, causes, "Full details: coop net inspect "+id+" --json")
	case len(result.Pending) != 0:
		writeNetCleanupIncomplete(w, p, id, netCleanupCauses(result), "Full details: coop net inspect "+id+" --json")
	case len(result.Removed) == 0:
		fmt.Fprintf(w, "Network run %s is already cleaned up.\n", id)
	default:
		fmt.Fprintf(w, "%s %s\n  %s\n", p.Green("✓"), p.Bold("Cleaned up network run "+id), netRemovedText(result))
		fmt.Fprintf(w, "\nView the record\n  %s\n", p.Cyan("coop net inspect "+id))
	}
}

// netCleanupCauses states what a partial pass actually did and what is left,
// each as its own fact: what was removed is past, what remains is present, and
// ownership coop could not prove is neither.
func netCleanupCauses(result box.NetworkRecovery) []string {
	var out []string
	if n := result.RemovedContainers; n != 0 {
		out = append(out, ui.Count(n, "temporary container")+" "+netWas(n)+" removed.")
	}
	if n := result.RemovedVolumes; n != 0 {
		out = append(out, ui.Count(n, "temporary volume")+" "+netWas(n)+" removed.")
	}
	if result.Unverified {
		return append(out, "Coop could not verify ownership of a remaining container.")
	}
	if n := result.PendingContainers; n != 0 {
		out = append(out, netRemaining(n, "temporary container"))
	}
	if n := result.PendingVolumes; n != 0 {
		out = append(out, netRemaining(n, "temporary volume"))
	}
	return out
}

// netRemaining says one leftover resource is still in use — "A temporary
// volume", not "1 temporary volume", because this is prose, not a count.
func netRemaining(n int, noun string) string {
	if n == 1 {
		return "A " + noun + " is still in use."
	}
	return ui.Count(n, noun) + " are still in use."
}

func netWas(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

// writeNetCleanupIncomplete is the one shape an unfinished cleanup takes: the
// warning, the actual causes six spaces in, and the single action that follows.
func writeNetCleanupIncomplete(w io.Writer, p ui.Palette, id string, causes []string, action string) {
	fmt.Fprintf(w, "%s %s\n\n", p.Yellow("⚠"), p.Yellow("Cleanup incomplete for network run "+id))
	for _, cause := range causes {
		fmt.Fprintf(w, "      %s\n", cause)
	}
	fmt.Fprintf(w, "\n  %s\n", action)
}

// netCleanupRemedy is what the operator can do about an external blocker. The
// recovery layer reports the daemon it could not use; everything else is coop's
// own retry, which it performs without being asked.
func netCleanupRemedy(skipped string) string {
	switch {
	case strings.Contains(skipped, "different daemon"):
		return "Reconnect to the original Docker daemon."
	case strings.Contains(skipped, "unavailable"), strings.Contains(skipped, "not available"):
		return "Start Docker; Coop will retry automatically."
	}
	return netRetryRemedy + "."
}

// netRemovedText counts what this pass actually removed. Separate kind counts
// are used only where the record resolved each resource to a kind; otherwise
// they are counted together rather than guessed apart.
func netRemovedText(result box.NetworkRecovery) string {
	containers, volumes := result.RemovedContainers, result.RemovedVolumes
	switch {
	case containers+volumes != len(result.Removed):
		return "Removed " + ui.Count(len(result.Removed), "temporary container and volume", "temporary containers and volumes") + "."
	case volumes == 0:
		return "Removed " + ui.Count(containers, "temporary container") + "."
	case containers == 0:
		return "Removed " + ui.Count(volumes, "temporary volume") + "."
	}
	return "Removed " + ui.Count(containers, "temporary container") + " and " + ui.Count(volumes, "temporary volume") + "."
}

// cmdNetSetup qualifies this host now. A filtered launch does the same on its
// own when it has to; this is for paying the cost ahead of an unattended run,
// or for rechecking a host after a Docker or coop upgrade.
func (a *app) cmdNetSetup() (int, error) {
	if err := a.ensureRuntime(); err != nil {
		return -1, err
	}
	if err := a.rt.EnsureDaemon(); err != nil {
		return -1, err
	}
	_, err := box.SetupNetwork(context.Background(), a.cfg, a.rt, os.Stderr, os.Stderr)
	if errors.Is(err, box.ErrNetworkSetupFailed) {
		return 1, nil // the transcript already named the failed check and the verdict
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// ---------------------------------------------------------------- access ----

func (a *app) cmdNetAccess() (int, error) {
	repo, err := netProject(a.cfg.RepoOverride)
	if err != nil {
		return 1, err
	}
	access, err := box.ProjectNetworkAccess(context.Background(), a.cfg, repo)
	if err != nil {
		return 1, err
	}
	p := ui.For(os.Stdout)
	return 0, netRender(os.Stdout, func(b *bytes.Buffer) { writeNetAccess(b, p, access) })
}

// netPendingNotice is what `coop init` adds after its scaffold, and only when
// this project's network request is actually pending — the same read-only check
// every launch makes, so a fresh or unchanged project costs zero lines. It
// needs no Docker and no store: a host that never ran a filtered box has
// nothing to compare against and stays silent.
func (a *app) netPendingNotice(repo string) {
	access, err := box.ProjectNetworkAccess(context.Background(), a.cfg, repo)
	if err != nil {
		ui.Warn("this project's network access could not be checked: %v", err)
		return
	}
	if access.Pending == nil {
		return
	}
	ui.Warn("%s", netPendingHeadline)
	ui.Note("  %s", access.Pending.Sentence())
	ui.Note("  %s", netPendingReview)
}

// The pending block's two fixed lines: a request nobody approved is the one
// thing that stops a new run, and one command settles it.
const (
	netPendingHeadline = "New runs need your approval"
	netPendingReview   = "Review changes: coop net approve"
)

// writeNetAccess answers the one question a person arrives with: what can a
// new run reach, and why. A pending request REPLACES that answer — describing a
// policy no run can start under would be a fiction — and otherwise the view is
// the mode in plain words, the approved network rules when there are any, and
// where to read more. No project header, no path, no setup or run history: a
// launch sets the host up itself, and `coop net runs` owns history.
func writeNetAccess(w io.Writer, p ui.Palette, access box.NetworkAccess) {
	if access.Pending != nil {
		writeNetPending(w, p, access)
		return
	}
	fmt.Fprintln(w, netModeExplanation(access.Mode, access.Source))
	// A provider bundle is included automatically, so it is never listed here as
	// something this project asked for.
	if access.Mode == egress.Filtered && access.Approval != nil && len(access.Approval.Envelope) != 0 {
		fmt.Fprintf(w, "\n%s\n", p.Bold(p.Cyan("APPROVED NETWORK RULES")))
		for _, rule := range access.Approval.Envelope {
			fmt.Fprintf(w, "  %s\n", netRuleText(rule))
		}
	}
	fmt.Fprintf(w, "\nFor more details see:\n  %s\n", p.Cyan("coop help net"))
}

// writeNetPending is the same condition a launch refuses on, said where the
// operator can act. It leads with the blocker, states the actual cause the
// admission check returned, then shows what the review would change: a mode
// change in the approve command's own words — with the red warning an
// unrestricted request earns — and the rule diff.
func writeNetPending(w io.Writer, p ui.Palette, access box.NetworkAccess) {
	fmt.Fprintf(w, "%s %s\n", p.Yellow("⚠"), p.Yellow(netPendingHeadline))
	fmt.Fprintf(w, "\n      %s\n", access.Pending.Sentence())
	var approved []egress.Rule
	var approvedMode egress.Mode
	if access.Approval != nil {
		approved, approvedMode = access.Approval.Envelope, access.Approval.Posture
	}
	if access.RequestedMode != approvedMode && (access.Approval != nil || access.RequestedMode == egress.Open) {
		fmt.Fprintln(w)
		writeNetModeChange(w, p, approvedMode, access.RequestedMode)
	}
	if rows := netRuleDiffRows(approved, access.Add, access.Remove, netAccessLabels); len(rows) != 0 {
		fmt.Fprintf(w, "\n  %s\n", "Access:")
		writeNetRuleRows(w, p, rows, 2)
	}
	fmt.Fprintf(w, "\n  %s\n", p.Cyan(netPendingReview))
}

// netModeExplanation is what a new run may reach, and why. The YAML explanation
// is used only where the project file actually selected the mode; every other
// source keeps its own truthful cause, so a host setting, a stored approval or
// coop's default is never attributed to a file that did not decide it.
func netModeExplanation(mode egress.Mode, source string) string {
	if source == box.AccessFromProject && mode == egress.Filtered {
		return "Network access is restricted according to the configuration in .agent/project.yaml.\n" +
			"Only approved network traffic and agent provider connections are allowed."
	}
	access := "use filtered internet access"
	switch mode {
	case egress.Open:
		access = "have unrestricted internet access"
	case egress.None:
		access = "are offline"
	}
	// Where the setting came from, said as the place a person would go to change it. "because that
	// is what you approved" was true but read as a justification for a decision under discussion;
	// the reader is looking for which of the four sources is in force.
	cause := "by default"
	switch source {
	case box.AccessFromApproval:
		cause = "according to approved settings for this project"
	case box.AccessFromHost:
		cause = "according to COOP_EGRESS"
	case box.AccessFromProject:
		cause = "according to .agent/project.yaml"
	}
	line := "New runs " + access + " " + cause + "."
	switch mode {
	case egress.Filtered:
		line += "\nOnly approved network traffic and agent provider connections are allowed."
	case egress.None:
		line += "\nAn agent cannot reach its provider."
	}
	return line
}

// netModeChange writes the access mode as a diff when it changes: what it was,
// what it becomes, and the red warning an unrestricted request earns. The words
// are the shared mode descriptions, so approve and the access view agree.
func writeNetModeChange(w io.Writer, p ui.Palette, before, after egress.Mode) {
	if before != "" && before != after {
		fmt.Fprintf(w, "  %s\n", p.Dim("- "+netModeDescription(before)))
		fmt.Fprintf(w, "  %s\n", p.Yellow("+ "+netModeDescription(after)))
	} else {
		// Nothing to diff against: the mode is stated once, with no marker.
		fmt.Fprintf(w, "  %s\n", netModeDescription(after))
	}
	if after == egress.Open {
		fmt.Fprintf(w, "  %s\n", p.Red(netOpenWarning))
	}
}

// netRuleLabels is what each row of a rule diff is called. The access view and
// the approval prompt ask different questions — what is pending versus what you
// are about to decide — so they label the same three rows differently.
type netRuleLabels struct{ retained, added, removed string }

var (
	netAccessLabels   = netRuleLabels{retained: "approved", added: "requested", removed: "removed"}
	netApprovalLabels = netRuleLabels{retained: "already approved", added: "new request", removed: "no longer requested"}
)

type netRuleRow struct{ marker, text, label string }

// netRuleDiffRows is the one shape a rule review takes: what stays, what is
// being added, what is being removed — in that order, because that is the order
// the question is asked. Within a group rows sort by the rule text, so the same
// request always reads the same.
func netRuleDiffRows(approved, add, remove []egress.Rule, labels netRuleLabels) []netRuleRow {
	group := func(rules []egress.Rule, marker, label string, skip func(egress.Rule) bool) []netRuleRow {
		var rows []netRuleRow
		for _, rule := range rules {
			if skip != nil && skip(rule) {
				continue
			}
			rows = append(rows, netRuleRow{marker: marker, text: netRuleText(rule), label: label})
		}
		slices.SortStableFunc(rows, func(x, y netRuleRow) int { return strings.Compare(x.text, y.text) })
		return rows
	}
	retained := group(approved, " ", labels.retained, func(rule egress.Rule) bool {
		return slices.ContainsFunc(remove, func(r egress.Rule) bool { return netRuleText(r) == netRuleText(rule) })
	})
	rows := append(retained, group(add, "+", labels.added, nil)...)
	return append(rows, group(remove, "-", labels.removed, nil)...)
}

// writeNetRuleRows prints a rule diff with every label starting past the widest
// rule, measured on PLAIN text so color never shifts the column. Additions are
// yellow because they expand access, removals dim because they reduce it —
// never green for a new grant, and the markers carry the meaning without color.
func writeNetRuleRows(w io.Writer, p ui.Palette, rows []netRuleRow, indent int) {
	width := 0
	for _, r := range rows {
		width = max(width, utf8.RuneCountInString(r.text))
	}
	pad := strings.Repeat(" ", indent)
	for _, r := range rows {
		line := pad + r.marker + " " + padRight(r.text, width+2) + r.label
		switch r.marker {
		case "+":
			line = p.Yellow(line)
		case "-":
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
		target += " · " + networkreport.TransportLabel(rule.Protocol)
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
			return opts, unknownOptionErr(arg, "coop net runs", []string{"--all", "--all-projects", "--json"})
		}
	}
	if opts.all && opts.allProjects {
		return opts, ui.ConflictingOptions("--all", "--all-projects", "coop net runs")
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
	return 0, netRender(os.Stdout, func(b *bytes.Buffer) { writeNetRuns(b, p, now, listed, total, opts, page) })
}

// writeNetRuns is the bounded history: one row per run, newest first, with the
// short id every run command accepts. The cross-project view labels each
// project by name and path so no run is unattributed; the project view is one
// table, and says so when it is showing only the newest of more.
func writeNetRuns(w io.Writer, p ui.Palette, now time.Time, runs []netRun, total int, opts netRunsOptions, page networkstate.ExecutionPage) {
	// A listing that could not be read whole says so as part of the answer, so
	// the rows above are never read as the complete history.
	defer func() {
		if page.Incomplete {
			fmt.Fprintf(w, "\n%s %s\n", p.Yellow("⚠"), p.Yellow("Some records could not be listed"))
		}
		if page.Unreadable > 0 {
			fmt.Fprintf(w, "\n%s %s\n", p.Yellow("⚠"), p.Yellow(ui.Count(page.Unreadable, "record")+" could not be read"))
		}
	}()
	if opts.allProjects {
		fmt.Fprintf(w, "%s\n", p.Bold(p.Cyan("Network runs on this host")))
		if len(runs) == 0 {
			fmt.Fprintln(w, "\nNo network runs recorded on this host yet.")
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
			writeNetRunRows(w, p, now, byProject[path], false)
		}
		return
	}
	fmt.Fprintf(w, "%s\n", p.Bold(p.Cyan("Recent network runs")))
	if len(runs) == 0 {
		fmt.Fprintln(w, "\nNo network runs recorded for this project yet.")
		return
	}
	fmt.Fprintln(w)
	writeNetRunRows(w, p, now, runs, true)
	if total > len(runs) {
		fmt.Fprintf(w, "\n%s\n", p.Dim(fmt.Sprintf("Showing %d of %d · coop net runs --all", len(runs), total)))
	}
}

// writeNetRunRows prints the run table. Columns are padded on PLAIN text before
// anything is styled, so the ACTIVITY column holds with color on or off.
func writeNetRunRows(w io.Writer, p ui.Palette, now time.Time, runs []netRun, header bool) {
	width := 0
	for _, run := range runs {
		width = max(width, utf8.RuneCountInString(networkreport.When(now, run.StartedAt)))
	}
	if header {
		fmt.Fprintf(w, "  %s%s%s\n", padRight("RUN", networkreport.ShortIDLen+2), padRight("WHEN", width+2), "ACTIVITY")
	}
	for _, run := range runs {
		line := "  " + p.Bold(p.Cyan(networkreport.ShortID(run.ID))) + "  " + padRight(networkreport.When(now, run.StartedAt), width+2) + run.Outcome
		// A finished run says only what it did. The EXCEPTIONS — no receipt,
		// cleanup still owed — earn a word, and neither claims the run is alive.
		var flags []string
		if !run.Final {
			flags = append(flags, "final record missing")
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

// netRunOutcome is what got through and what did not, from that run's own
// retained evidence. The two counts are different units and are never added
// together; a measured zero says so in words, an unmeasured one says which
// measurement is missing, and a truncated denial list is stated as the lower
// bound it is rather than printed as a total.
func netRunOutcome(inspection networkstate.Inspection) string {
	observed := inspection.Observed
	if observed.Sequence == 0 {
		return netActivityUnknown
	}
	allowed := "connection count unknown"
	if counters := observed.Counters; counters != nil && counters.Connections != nil {
		allowed = networkreport.ConnectionCount(counters.Connections)
		if *counters.Connections == 0 {
			allowed = "no external connections"
		}
	}
	if blocked := len(observed.Denials); blocked != 0 {
		if observed.Loss.DetailTruncated {
			return allowed + " · at least " + ui.Count(blocked, "blocked attempt")
		}
		return allowed + " · " + strconv.Itoa(blocked) + " blocked"
	}
	if counters := observed.Counters; counters != nil && counters.DeniedPackets != nil && *counters.DeniedPackets != 0 {
		return allowed + " · " + networkreport.Plural(uint64(*counters.DeniedPackets), "raw packet") + " blocked"
	}
	return allowed
}

// netActivityUnknown is a row for a run whose evidence says nothing about what
// it did — not a run that did nothing.
const netActivityUnknown = "activity unknown"

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
		run := netRun{ExecutionSummary: summary, Outcome: netActivityUnknown}
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
// command is the full path the refusal names, so the Usage row is the one the
// reader actually typed.
func netResolveRun(page networkstate.ExecutionPage, ref, command string) (string, error) {
	if netEvidenceID(ref) {
		return ref, nil
	}
	if page.Incomplete {
		return "", netIncompleteInventoryErr(command)
	}
	if ref == "" || len(ref) > 32 || strings.Trim(ref, "0123456789abcdef") != "" {
		return "", netUnknownRunErr(ref)
	}
	var matches []string
	for _, run := range page.Executions {
		if strings.HasPrefix(run.ID, ref) {
			matches = append(matches, run.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", netUnknownRunErr(ref)
	}
	prefixes := make([]string, 0, len(matches))
	for _, id := range matches {
		prefixes = append(prefixes, netUniquePrefix(id, page.Executions))
	}
	return "", &ui.UsageError{
		Headline: fmt.Sprintf("Run ID %q matches %d runs", ref, len(matches)),
		Choices:  prefixes,
		Rows:     [][2]string{{"Usage:", command + " <run>"}, {"Help:", ui.HelpCommand(command)}},
	}
}

// netUnknownRunErr refuses a run nobody recorded here. The action is the ONE
// listing that would show a run from any project, since a run named from
// elsewhere is the usual reason a project-scoped search finds nothing.
func netUnknownRunErr(ref string) error {
	return &ui.UsageError{
		Headline: fmt.Sprintf("No network run matches %q", ref),
		Rows:     [][2]string{{"Runs:", "coop net runs --all-projects"}},
	}
}

// netIncompleteInventoryErr refuses to pick a run from a list that could not be
// read whole: a prefix that looks unique in half an inventory is not unique.
func netIncompleteInventoryErr(command string) error {
	return &ui.UsageError{
		Headline: "Could not select a network run",
		Cause:    "Some records could not be read. Use a full run ID.",
		Rows:     [][2]string{{"Help:", ui.HelpCommand(command)}},
	}
}

// netUniquePrefix is the shortest display prefix that names this run alone
// among the recorded ones, never shorter than the ordinary short id.
func netUniquePrefix(id string, runs []networkstate.ExecutionSummary) string {
	for n := networkreport.ShortIDLen; n < len(id); n++ {
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
	command := "coop net " + verb
	if ref != "" {
		return netResolveRun(page, ref, command)
	}
	repo, err := netProject(a.cfg.RepoOverride)
	if err != nil || page.Incomplete {
		return "", netIncompleteInventoryErr(command)
	}
	runs := netFilterProject(page.Executions, netResolvedPath(repo))
	if verb == "watch" {
		// "Unfinished" is the honest word: a missing receipt does not prove the
		// supervisor is alive, so this never claims a run is active.
		var unfinished, prefixes []string
		for _, run := range runs {
			if !run.Final {
				unfinished = append(unfinished, run.ID)
				prefixes = append(prefixes, netUniquePrefix(run.ID, page.Executions))
			}
		}
		switch len(unfinished) {
		case 0:
			return "", &ui.UsageError{
				Headline: "No unfinished network run for this project",
				Rows:     [][2]string{{"Latest report:", "coop net inspect"}},
			}
		case 1:
			return unfinished[0], nil
		}
		return "", &ui.UsageError{
			Headline: "Choose a network run to watch",
			Choices:  prefixes,
			Rows:     [][2]string{{"Usage:", command + " <run>"}, {"Help:", ui.HelpCommand(command)}},
		}
	}
	if len(runs) == 0 {
		return "", &ui.UsageError{
			Headline: "No network run recorded for this project yet",
			Rows:     [][2]string{{"Runs:", "coop net runs --all-projects"}},
		}
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
	command := "coop net " + verb
	for _, arg := range args {
		switch {
		case arg == "--json":
			opts.json = true
		case strings.HasPrefix(arg, "-"):
			return opts, unknownOptionErr(arg, command, []string{"--json"})
		case opts.id != "":
			return opts, ui.UnexpectedArgument(arg, command, command+" [<run>] [--json]")
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
	evidence, err := openNetRunEvidence(opts.id)
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
	view := networkreport.View{ID: id}
	// A run whose cleanup is still owed and that nothing is observing is what
	// recovery exists for. Coop tries once itself, then reads the run again, so
	// a dead supervisor's leftovers are reported only when something external
	// kept coop from settling them.
	if inspection.Cleanup == "pending" && !networkreport.RunLive(inspection) {
		view.Cleanup = a.netSettleCleanup(id)
		if again, err := evidence.Inspect(id, time.Now(), true); err == nil {
			inspection = again
		}
	}
	p := ui.For(os.Stdout)
	return 0, netRender(os.Stdout, func(b *bytes.Buffer) { networkreport.WriteRun(b, p, view, inspection) })
}

// netSettleCleanup is coop's one bounded recovery attempt for a run. It goes
// through the same ownership-checked path as `coop net recover` — a departed
// supervisor, the exact daemon the run used, removals by recorded id — and
// reports what it learned in the operator's terms. There is no retry loop here:
// the next inspect, loop or fork start makes the next attempt.
func (a *app) netSettleCleanup(id string) networkreport.Cleanup {
	if err := a.ensureRuntime(); err != nil {
		return networkreport.Cleanup{Blocker: err.Error(), Remedy: "Coop will retry automatically once a container runtime is available"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), orphanSweepTimeout)
	defer cancel()
	results, err := box.RecoverNetworkRuns(ctx, a.rt, id)
	if err != nil {
		return networkreport.Cleanup{Blocker: err.Error(), Remedy: "Coop will retry automatically"}
	}
	for _, result := range results {
		if result.RunID == id {
			return netCleanupOf(result)
		}
	}
	return networkreport.Cleanup{}
}

// netCleanupOf translates a recovery result. A live supervisor means cleanup is
// that process's job and nothing is owed yet; anything else that stopped the
// attempt is named as it is, with the one thing only the operator can do.
func netCleanupOf(result box.NetworkRecovery) networkreport.Cleanup {
	switch {
	case result.Live:
		return networkreport.Cleanup{Expected: true}
	case result.Skipped != "":
		// A daemon that is stopped, and a daemon replaced by another at the
		// recorded endpoint, are both "that daemon is not there"; the blocker
		// says which, and the remedy is the one thing only the operator can do.
		return networkreport.Cleanup{Blocker: netCleanupBlocker(result.Skipped), Remedy: strings.TrimSuffix(netCleanupRemedy(result.Skipped), ".")}
	case len(result.Failures) != 0:
		return networkreport.Cleanup{Blocker: result.Failures[0].Error(), Remedy: netRetryRemedy}
	case len(result.Pending) != 0:
		return networkreport.Cleanup{Blocker: strings.TrimSuffix(strings.Join(netCleanupCauses(result), " "), "."), Remedy: netRetryRemedy}
	}
	return networkreport.Cleanup{}
}

const netRetryRemedy = "Coop will retry automatically"

// netCleanupBlocker states an external blocker in the operator's words. The
// recovery layer's text names the runtime it could not use; this is the one
// place those become the sentence a report prints.
func netCleanupBlocker(skipped string) string {
	switch {
	case strings.Contains(skipped, "different daemon"):
		return "Docker is connected to a different daemon than this run used"
	case strings.Contains(skipped, "unavailable"), strings.Contains(skipped, "not available"):
		return "Docker is unavailable"
	}
	return skipped
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
// secret. --include-addresses is the operator saying, on their own machine,
// that they want the remote hostnames and IP addresses in it.
func (a *app) cmdNetExport(args []string) (int, error) {
	const command, usage = "coop net export", "coop net export <run> [--include-addresses]"
	ref, include := "", false
	for _, arg := range args {
		switch {
		case arg == "--include-addresses":
			include = true
		case strings.HasPrefix(arg, "-"):
			return 2, unknownOptionErr(arg, command, []string{"--include-addresses"})
		case ref != "":
			return 2, ui.UnexpectedArgument(arg, command, usage)
		default:
			ref = arg
		}
	}
	if ref == "" {
		return 2, ui.MissingArgument("run", command, usage)
	}
	evidence, err := openNetRunEvidence(ref)
	if err != nil {
		return 1, err
	}
	defer evidence.Close()
	page, err := netExecutionsOf(evidence)
	if err != nil {
		return 1, err
	}
	id, err := netResolveRun(page, ref, command)
	if err != nil {
		return 1, err
	}
	inspection, err := evidence.Inspect(id, time.Now(), include)
	if err != nil {
		return 1, netRunErr(id, err)
	}
	if inspection.Receipt == nil {
		short := networkreport.ShortID(id)
		return 1, &ui.UsageError{
			Headline: "Network run " + short + " has no final record yet",
			Rows:     [][2]string{{"Current report:", "coop net inspect " + short}},
		}
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
	short := networkreport.ShortID(d.id)
	switch {
	case current.Receipt != nil:
		if previous != nil {
			fmt.Fprintln(&b)
		}
		networkreport.WriteRun(&b, d.palette, networkreport.View{ID: short}, current)
	case previous == nil:
		fmt.Fprintf(&b, "%s\n", d.palette.Bold(d.palette.Cyan("Network run "+short)))
		fmt.Fprintf(&b, "%s\n\n", "Press Ctrl-C to stop watching.")
		netWatchAppend(&b, d.palette, current.ReadAt, netWatchDelta(networkstate.Inspection{}, current, short))
	default:
		netWatchAppend(&b, d.palette, current.ReadAt, netWatchDelta(*previous, current, networkreport.ShortID(d.id)))
	}
	if err := netEventBound(b.Len()); err != nil {
		return err
	}
	return netWriteAll(d.out, b.Bytes())
}

func netWatchAppend(b *bytes.Buffer, p ui.Palette, at time.Time, lines []string) {
	stamp := p.Dim(at.UTC().Format("15:04:05"))
	for _, line := range lines {
		fmt.Fprintf(b, "%s  %s\n", stamp, line)
	}
}

// netWatchDelta names what appeared since the last poll: connections the
// workload opened, attempts that were blocked, alerts that fired. Repeats of
// one destination in one poll are ONE counted line, and each kind is bounded.
func netWatchDelta(previous, current networkstate.Inspection, id string) []string {
	seen := map[string]bool{}
	for _, c := range previous.Observed.Connections {
		seen[c.ID] = true
	}
	var lines []string
	fresh := networkview.Snapshot{}
	for _, c := range current.Observed.Connections {
		if !seen[c.ID] && c.NameSource == networkreport.WorkloadName {
			fresh.Connections = append(fresh.Connections, c)
		}
	}
	var opened []string
	for _, group := range networkreport.WorkloadDestinations(fresh) {
		count := 0
		for _, peer := range group.Peers {
			count += peer.Connections
		}
		line := "Allowed " + group.Label()
		if count > 1 {
			line += " · " + ui.Count(count, "connection")
		}
		opened = append(opened, line)
	}
	lines = append(lines, netWatchBound(opened, "More events recorded", id)...)
	for _, denial := range previous.Observed.Denials {
		seen[denial.ID] = true
	}
	for _, denial := range current.Observed.Denials {
		if !seen[denial.ID] {
			fresh.Denials = append(fresh.Denials, denial)
		}
	}
	var blocked []string
	for _, group := range networkreport.RefusalGroups(fresh.Denials) {
		line := "Blocked " + group.Label
		if group.Count > 1 {
			line += " · " + ui.Count(group.Count, "attempt")
		}
		blocked = append(blocked, line)
	}
	lines = append(lines, netWatchBound(blocked, "More events recorded", id)...)
	for _, alert := range previous.Observed.Alerts {
		seen[alert.ID] = true
	}
	for _, alert := range current.Observed.Alerts {
		if !seen[alert.ID] {
			lines = append(lines, "Alert: "+networkreport.AlertText(alert))
		}
	}
	return lines
}

// netWatchBound caps what one poll may append. The overflow line points at the
// full record rather than claiming the list continues below: the detail itself
// may already be truncated, so "the whole list" would be a promise.
func netWatchBound(lines []string, more, id string) []string {
	if len(lines) <= netWatchDeltaLines {
		return lines
	}
	return append(lines[:netWatchDeltaLines], more+" — coop net inspect "+id)
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

func openNetRunEvidence(ref string) (*networkstate.Evidence, error) {
	evidence, err := openNetEvidence()
	if errors.Is(err, fs.ErrNotExist) {
		if ref != "" {
			return nil, netUnknownRunErr(ref)
		}
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

func netPortList(ports []int) string {
	out := make([]string, 0, len(ports))
	for _, port := range ports {
		out = append(out, strconv.Itoa(port))
	}
	return strings.Join(out, ",")
}
