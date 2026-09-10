package box

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/ui"
)

// A denial has to explain itself where it happened. These bounds keep that
// explanation one screen, not a log: an agent can generate arbitrarily many
// distinct denied names, and the retained detail ring is bounded anyway.
const (
	maxSummaryDenials = 6
	maxSummaryAlerts  = 3
	maxNoteGrants     = 8
)

// NetworkReport is the end-of-run networking summary for one filtered run. It
// reports only what was observed: a missing counter is unknown, never zero, and
// an empty detail list is not proof that nothing was refused.
//
// It is exported because a run coop does not print for — a loop iteration, any
// batch embedding — still has to surface a refusal in ITS own output, through
// RunSpec.OnNetworkReport.
type NetworkReport struct {
	RunID   string
	Denials []NetworkDenial
	Omitted int
	Allowed string
	// Raw is the kernel's tally of refused raw packets, rendered; RawPackets is
	// how many it counted (0 when nothing was refused OR nothing was measured).
	// They are counted, never attributed: the filter drops them without
	// recording a destination.
	Raw        string
	RawPackets uint64
	Alerts     []string
	Event      string // the evidence id `coop net explain` can open, when one was retained
	Truncate   bool
}

// NetworkDenial is one refused destination as the retained evidence recorded
// it: where the box tried to go, where the refusal was seen, and how many
// attempts that grouped. The count stays a number so a caller that aggregates
// across runs — the loop's closing summary — never has to parse a rendered line.
type NetworkDenial struct {
	Destination string
	Basis       string // dns | tls | socket | admission | unknown
	Count       int
}

func (d NetworkDenial) String() string {
	line := d.Destination + " (" + d.Basis + ")"
	if d.Count > 1 {
		line += " ×" + strconv.Itoa(d.Count)
	}
	return line
}

// Quiet reports a run that never reached the boundary. Nothing refused and no
// alert costs one dim line at most — never a block, and never a line per
// iteration in an overnight drain. A refused raw packet has no destination to
// list, but it IS the boundary being hit: reporting "nothing was refused" over
// a nonzero kernel counter would be the one thing this summary must never say.
func (r NetworkReport) Quiet() bool {
	return len(r.Denials) == 0 && len(r.Alerts) == 0 && r.RawPackets == 0
}

// networkRunReport folds a run's retained evidence into the lines a human reads
// when their box could not reach something. Denials are grouped by destination
// and kind so one refused name that retried forty times is one line.
func networkRunReport(runID string, snapshot networkview.Snapshot) NetworkReport {
	out := NetworkReport{RunID: runID, Truncate: snapshot.Loss.DetailTruncated}
	groups := map[string]*NetworkDenial{}
	var ordered []*NetworkDenial // first-seen order, which is the order evidence arrived
	first, drafted := "", ""
	for _, denial := range snapshot.Denials {
		basis := denialBasis(denial.Kind)
		where := denial.Name
		if where == "" {
			where = denial.Peer
		}
		if where == "" {
			where = "name withheld"
		}
		if first == "" {
			first = denial.ID
		}
		// An event that already carries a draft rule is the one worth opening:
		// it is the refusal a human could actually act on.
		if drafted == "" && denial.Candidate != nil {
			drafted = denial.ID
		}
		key := basis + "\x00" + where
		if existing, ok := groups[key]; ok {
			existing.Count++
			continue
		}
		group := &NetworkDenial{Destination: where, Basis: basis, Count: 1}
		groups[key] = group
		ordered = append(ordered, group)
	}
	if out.Event = drafted; out.Event == "" {
		out.Event = first
	}
	for i, g := range ordered {
		if i >= maxSummaryDenials {
			out.Omitted = len(ordered) - maxSummaryDenials
			break
		}
		out.Denials = append(out.Denials, *g)
	}
	out.Allowed = allowedTraffic(snapshot.Counters)
	out.Raw = rawRefusals(snapshot.Counters)
	if snapshot.Counters != nil && snapshot.Counters.DeniedPackets != nil {
		out.RawPackets = uint64(*snapshot.Counters.DeniedPackets)
	}
	for i, alert := range snapshot.Alerts {
		if i >= maxSummaryAlerts {
			break
		}
		out.Alerts = append(out.Alerts, alertLine(alert))
	}
	return out
}

// denialBasis translates one retained event kind into the word a human uses for
// where the refusal was seen. It never invents a transport the evidence did not
// carry: a refused DNS query observed no port and no connection.
func denialBasis(kind string) string {
	switch kind {
	case "tls_denied":
		return "tls"
	case "dns_denied":
		return "dns"
	case "direct_tcp_attempt":
		return "socket"
	case "admission_failed":
		return "admission"
	default:
		return "unknown"
	}
}

func allowedTraffic(counters *networkview.Counters) string {
	if counters == nil {
		return "allowed: UNKNOWN — nothing was recorded for this run"
	}
	return fmt.Sprintf("allowed: %s, %s sent, %s received",
		connectionText(counters.Connections), byteText(counters.SentBytes), byteText(counters.ReceivedBytes))
}

// rawRefusals reports the packets the kernel refused. It deliberately names no
// destination: nothing recorded one, and a plausible guess would be a fiction.
// A measured zero says nothing worth a line; UNKNOWN still does.
func rawRefusals(counters *networkview.Counters) string {
	if counters == nil || counters.DeniedPackets == nil {
		return "refused raw packets: UNKNOWN — the kernel counters were not recorded"
	}
	if *counters.DeniedPackets == 0 {
		return ""
	}
	return fmt.Sprintf("%s refused too, with no destination recorded (%s of them to protected addresses)",
		pluralText(uint64(*counters.DeniedPackets), "raw packet"), countText(counters.ProtectedPackets))
}

// countText renders a retained counter as a decimal string, matching how it is
// stored. A nil counter is UNKNOWN — a metric nobody measured is not a zero.
func countText(value *networkview.Count) string {
	if value == nil {
		return "UNKNOWN"
	}
	return strconv.FormatUint(uint64(*value), 10)
}

// pluralText counts without narrowing a stored counter to an int, so a total
// past the platform's int range still reads correctly.
func pluralText(value uint64, noun string) string {
	if value == 1 {
		return "1 " + noun
	}
	return strconv.FormatUint(value, 10) + " " + noun + "s"
}

func connectionText(value *networkview.Count) string {
	if value == nil {
		return "UNKNOWN connections"
	}
	return pluralText(uint64(*value), "connection")
}

func byteText(value *networkview.Count) string {
	if value == nil {
		return "UNKNOWN"
	}
	return ui.Bytes(uint64(*value))
}

// alertLine states the observed facts, the window and the threshold that fired,
// and nothing else. An encrypted byte count is not evidence of intent.
func alertLine(alert networkview.Alert) string {
	line := fmt.Sprintf("%s (%s): %s over %sms, threshold %s %s", alert.Category, alert.Severity, alert.State,
		strconv.FormatUint(uint64(alert.WindowMillis), 10), strconv.FormatUint(uint64(alert.Threshold.Value), 10), alert.Threshold.Unit)
	if alert.Facts.Reason != "" {
		line += " — " + alert.Facts.Reason
	}
	return line
}

// report folds this execution's retained evidence into the run's summary. Call
// it after cleanup sealed the receipt: until then the snapshot is still being
// amended, so an earlier read would report a history nobody kept.
func (f *filteredExecution) report() NetworkReport {
	if f == nil || f.record.ID == "" {
		return NetworkReport{}
	}
	snapshot := f.record.Snapshot
	if f.record.Receipt != nil {
		snapshot = f.record.Receipt.Snapshot
	}
	return networkRunReport(f.record.ID, snapshot)
}

// print writes the run's networking outcome on stderr, where coop's own voice
// lives: never on stdout, which may be carrying provider JSON or an ACP frame.
// A clean run costs one dim line; a run that hit the boundary explains itself
// and names both ways forward — read the evidence, or ask for the destination.
func (r NetworkReport) print() {
	if r.RunID == "" {
		return
	}
	if r.Quiet() {
		ui.Detail("nothing was refused in this box (coop net inspect %s)", r.RunID)
		return
	}
	if len(r.Denials) > 0 {
		ui.Warn("%s refused in this box", ui.Count(len(r.Denials)+r.Omitted, "destination was", "destinations were"))
		for _, denial := range r.Denials {
			ui.Detail("%s", denial)
		}
		if r.Omitted > 0 {
			ui.Detail("… and %s (coop net inspect %s)", ui.Count(r.Omitted, "more"), r.RunID)
		}
	}
	for _, line := range r.Alerts {
		ui.Warn("network alert: %s", line)
	}
	ui.Detail("%s", r.Allowed)
	if r.Raw != "" {
		ui.Detail("%s", r.Raw)
	}
	if r.Truncate {
		ui.Detail("only part of the detail was kept — the list above is not the whole history")
	}
	if r.Event != "" {
		ui.Detail("coop net explain %s --run %s   # why", r.Event, r.RunID)
	}
	ui.Detail("to allow one: add it under box.egress_rules in .agent/project.yaml, then run 'coop net approve' on the host")
}

// networkInstructionNote is the Network section every agent in a filtered box
// receives up front. Whatever the box may reach is fully known before it
// starts, so stating it costs a few lines once instead of a turn spent
// rediscovering the boundary by being refused.
func networkInstructionNote(policy egress.Snapshot) string {
	if policy.Mode != egress.Filtered {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n# Network (coop restricted egress) — this list is the whole truth\n")
	b.WriteString("This box can reach ONLY these destinations. Everything else is refused at its boundary,\n")
	b.WriteString("and a refusal is that rule — not a broken tool, a dead host or a DNS fault. Don't work around it.\n")
	destinations, omitted := noteDestinations(policy)
	for _, line := range destinations {
		b.WriteString("- " + line + "\n")
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "- … and %d more allowed destination(s)\n", omitted)
	}
	b.WriteString("To ask for another one, add it under box.egress_rules in .agent/project.yaml and ask the\n")
	b.WriteString("human to run \"coop net approve\" on the host — nothing in here can allow it.\n")
	return b.String()
}

// noteDestinations lists what the agent may reach in the words it would use: a
// named grant by name, a provider's maintained core bundle as one line. The
// exact hostnames of a provider bundle are the release's business, not context
// the agent pays for on every turn.
func noteDestinations(policy egress.Snapshot) ([]string, int) {
	var named, summarized []string
	for _, grant := range policy.Grants {
		text := NetworkGrantText(grant)
		switch {
		case text == "":
		case text == NetworkRuleText(grant.Rule):
			if !slices.Contains(named, text) {
				named = append(named, text)
			}
		case !slices.Contains(summarized, text):
			summarized = append(summarized, text)
		}
	}
	slices.Sort(named)
	slices.Sort(summarized)
	all := append(named, summarized...)
	if len(all) > maxNoteGrants {
		return all[:maxNoteGrants], len(all) - maxNoteGrants
	}
	return all, 0
}

// NetworkGrantText is the ONE renderer for a grant a human reads: an
// automatically derived grant by the name of what derived it, everything else
// as the rule it came from. Callers that hold a bare rule — a diff, a drafted
// suggestion — use NetworkRuleText directly.
func NetworkGrantText(grant egress.Grant) string {
	for _, origin := range grant.Origins {
		switch origin.Kind {
		case "provider":
			if origin.Provider != "" {
				return origin.Provider + " core endpoints"
			}
			return "provider core endpoints"
		case "mcp":
			return "the MCP servers coop set up for this box"
		}
	}
	return NetworkRuleText(grant.Rule)
}

// NetworkRuleText renders one rule the way its YAML reads, so what a human
// approves, what a box is told and what a suggestion drafts all say the same
// thing. Values are already normalized ASCII by the rule grammar.
func NetworkRuleText(rule egress.Rule) string {
	var b strings.Builder
	switch {
	case rule.To.Domain != "":
		b.WriteString(rule.To.Domain)
	case rule.To.IP != "":
		b.WriteString(rule.To.IP)
	case rule.To.CIDR != "":
		b.WriteString(rule.To.CIDR)
	case rule.To.Service != "":
		b.WriteString("service " + rule.To.Service)
	case rule.To.Provider != "":
		b.WriteString(rule.To.Provider)
		if len(rule.To.Features) != 0 {
			b.WriteString(" features " + strings.Join(rule.To.Features, ","))
		}
		return b.String()
	default:
		return "(no destination)"
	}
	if rule.Protocol != "" {
		b.WriteString(" " + rule.Protocol)
	}
	if len(rule.Ports) != 0 {
		ports := make([]string, 0, len(rule.Ports))
		for _, port := range rule.Ports {
			ports = append(ports, strconv.Itoa(port))
		}
		b.WriteString("/" + strings.Join(ports, ","))
	}
	if len(rule.Types) != 0 {
		b.WriteString(" types " + strings.Join(rule.Types, ","))
	}
	if len(rule.Codes) != 0 {
		codes := make([]string, 0, len(rule.Codes))
		for _, code := range rule.Codes {
			codes = append(codes, strconv.Itoa(code))
		}
		b.WriteString(" codes " + strings.Join(codes, ","))
	}
	return b.String()
}

// NetworkRuleYAML is the copyable `egress_rules` entry for one rule — the shape
// a human pastes into .agent/project.yaml. It is a draft to review, never a
// grant: only `coop net approve` turns it into authority.
func NetworkRuleYAML(rule egress.Rule) string {
	var b strings.Builder
	b.WriteString("    - to:\n")
	switch {
	case rule.To.Domain != "":
		fmt.Fprintf(&b, "        domain: %q\n", rule.To.Domain)
	case rule.To.IP != "":
		fmt.Fprintf(&b, "        ip: %q\n", rule.To.IP)
	case rule.To.CIDR != "":
		fmt.Fprintf(&b, "        cidr: %q\n", rule.To.CIDR)
	case rule.To.Service != "":
		fmt.Fprintf(&b, "        service: %q\n", rule.To.Service)
	case rule.To.Provider != "":
		fmt.Fprintf(&b, "        provider: %q\n", rule.To.Provider)
	}
	if rule.Protocol != "" {
		fmt.Fprintf(&b, "      protocol: %s\n", rule.Protocol)
	}
	if len(rule.Ports) != 0 {
		ports := make([]string, 0, len(rule.Ports))
		for _, port := range rule.Ports {
			ports = append(ports, strconv.Itoa(port))
		}
		fmt.Fprintf(&b, "      ports: [%s]\n", strings.Join(ports, ", "))
	}
	if len(rule.Types) != 0 {
		fmt.Fprintf(&b, "      types: [%s]\n", strings.Join(rule.Types, ", "))
	}
	if len(rule.Codes) != 0 {
		codes := make([]string, 0, len(rule.Codes))
		for _, code := range rule.Codes {
			codes = append(codes, strconv.Itoa(code))
		}
		fmt.Fprintf(&b, "      codes: [%s]\n", strings.Join(codes, ", "))
	}
	return b.String()
}
