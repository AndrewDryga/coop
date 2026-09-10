package cli

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/ui"
)

// The human view of one run leads with TRAFFIC — which destinations the
// workload reached and how much each carried — and then says only what went
// wrong. Everything that went right is silent: every inspectable run is
// filtered by admission, so printing the mode, the policy fingerprint, four
// healthy layers, a sealed receipt and a finished cleanup makes a normal result
// long while burying the one thing anybody came for. A line on screen means
// something happened. `--json` keeps every field this view drops.

// netRunView is the heading context one projection is rendered in. Standalone
// `coop net inspect` writes an unprefixed stdout view; a result that follows
// agent output carries coop's voice anchor in Prefix. There is one body.
type netRunView struct {
	ID     string
	Prefix string
	// Cleanup is what coop's one bounded recovery attempt learned about a run
	// whose cleanup was still owed. Zero when nothing was owed or attempted.
	Cleanup netCleanup
}

// netCleanup is the outcome of that attempt in the operator's terms: the
// EXTERNAL blocker that stopped it and the one thing only they can do about
// it. Expected means a live supervisor still owns the run, so its cleanup is
// in progress rather than incomplete.
type netCleanup struct {
	Expected        bool
	Blocker, Remedy string
}

func writeNetRun(w io.Writer, p ui.Palette, view netRunView, inspection networkstate.Inspection) {
	observed := inspection.Observed
	fmt.Fprintf(w, "%s%s\n", view.Prefix, p.Bold(p.Cyan("Network run "+view.ID)))
	// Totals under a live run are still moving, so they would otherwise read as
	// final. A terminal run needs no lifecycle line at all.
	switch {
	case netRunLive(inspection):
		fmt.Fprintf(w, "%s\n", p.Dim("● Live — totals are still changing"))
	case netRunActive(view, inspection) && observed.Sequence == 0:
		fmt.Fprintf(w, "%s\n", p.Dim("● Live — nothing observed yet"))
	case netRunActive(view, inspection):
		fmt.Fprintf(w, "%s\n", p.Dim("● Live — last observed "+netWhen(inspection.ReadAt, observed.AsOf)))
	}
	fmt.Fprintln(w)
	b := netBlockOf(p)
	b.field("Allowed", netAllowedAggregate(observed))
	writeNetDestinations(b, observed)
	b.flush(w)
	writeNetOtherEndpoints(w, p, observed)
	writeNetRawTraffic(w, observed)
	writeNetWarnings(w, p, netRunExceptions(p, view, inspection))
	fmt.Fprintf(w, "\n%s\n", p.Dim("Full details: coop net inspect "+view.ID+" --json"))
}

// netRunLive reports whether this run is still being observed. A sealed receipt
// or a terminal epoch ends observation; a stale read does not prove the box
// stopped, but its totals are no longer "still changing" either.
func netRunLive(inspection networkstate.Inspection) bool {
	return !netRunTerminal(inspection) && inspection.Freshness == networkstate.FreshnessFresh
}

func netRunTerminal(inspection networkstate.Inspection) bool {
	return inspection.Receipt != nil || inspection.Observed.Terminal
}

// netRunActive reports whether the run is still somebody's: it is being
// observed now, or the supervisor that owns it is still running — which coop's
// recovery attempt learned, since it never touches a live one. A run that is
// neither has stopped without sealing a record, and the view says so.
func netRunActive(view netRunView, inspection networkstate.Inspection) bool {
	return netRunLive(inspection) || view.Cleanup.Expected && !netRunTerminal(inspection)
}

// ---------------------------------------------------------- destinations ----

// netAllowedAggregate is what got through, in the units a person reads. A nil
// counter is UNKNOWN — a metric nobody measured is not a measured zero — and a
// measured zero says so in words instead of showing a row of zeroes.
func netAllowedAggregate(observed networkview.Snapshot) string {
	counters := observed.Counters
	switch {
	case counters == nil && observed.Sequence == 0:
		return "UNKNOWN — nothing was recorded for this run"
	case counters == nil:
		return "UNKNOWN — no totals were recorded for this run"
	case counters.Connections != nil && *counters.Connections == 0:
		return "no external connections"
	}
	return netConnectionCount(counters.Connections) + " · " + netByteCount(counters.SentBytes) +
		" sent · " + netByteCount(counters.ReceivedBytes) + " received"
}

// netWorkloadName is the one provenance test this view turns on. The collector
// gives a proxy-observed flow the name it validated from the ClientHello; every
// other source — the kernel socket sample, coop's own resolver, a socket it
// could not match — is an observation ABOUT the run, not proof the workload
// opened it. Only the first belongs under `Allowed`.
const netWorkloadName = "sni"

// netDestinationGroup is one destination the workload reached: the name, port
// and transport a person recognizes, with every distinct peer beneath it.
type netDestinationGroup struct {
	name, port, transport string
	peers                 []*netPeerTotals
}

func (g netDestinationGroup) label() string {
	label := g.name
	if g.port != "" {
		label += ":" + g.port
	}
	return label + " · " + netTransportLabel(g.transport)
}

// netPeerTotals is what one peer saw: the connections that were established
// and what they carried, plus the attempts the policy allowed but the peer
// never answered, and the ones still being made.
type netPeerTotals struct {
	peer                            string
	connections, failed, connecting int
	failure                         string // the first failed attempt's reason code
	sent, received                  netTotal
}

// row states the peer's facts in one line. Established connections lead; a
// destination that only ever failed says so with the reason, because a policy
// that allowed it is not a host that answered.
func (t netPeerTotals) row() string {
	var parts []string
	if t.connections != 0 {
		parts = append(parts, ui.Count(t.connections, "connection")+" · "+t.sent.String()+" sent · "+t.received.String()+" received")
	}
	if t.failed != 0 {
		failed := ui.Count(t.failed, "attempt") + " failed"
		if t.connections == 0 {
			failed += " — " + netRefusalReasonText(t.failure)
		}
		parts = append(parts, failed)
	}
	if t.connecting != 0 {
		parts = append(parts, strconv.Itoa(t.connecting)+" connecting")
	}
	return t.peer + " · " + strings.Join(parts, " · ")
}

// netTotal keeps a byte sum honest. A group with one unmeasured member is
// UNKNOWN rather than a precise total that quietly counted it as zero, and a
// saturated counter stays an explicit lower bound.
type netTotal struct {
	value          uint64
	unknown, bound bool
}

func (t *netTotal) add(value *networkview.Count) {
	if value == nil {
		t.unknown = true
		return
	}
	count := networkview.Count(t.value)
	if !networkview.Add(&count, uint64(*value)) || uint64(*value) == ^uint64(0) {
		t.bound = true // saturated here, or already saturated upstream
	}
	t.value = uint64(count)
}

func (t netTotal) String() string {
	switch {
	case t.unknown:
		return "UNKNOWN"
	case t.bound:
		return "≥ " + ui.Bytes(t.value)
	}
	return ui.Bytes(t.value)
}

func writeNetDestinations(b *netBlock, observed networkview.Snapshot) {
	for _, group := range netWorkloadDestinations(observed) {
		b.row(group.label())
		for _, peer := range group.peers {
			b.rowAt(3, peer.row())
		}
	}
}

// writeNetOtherEndpoints lists the sockets coop saw but could not attribute to
// anything — in their own block, after Allowed: an address appearing with no
// explanation is worse than one labeled row, yet nothing here reads as allowed
// workload traffic.
func writeNetOtherEndpoints(w io.Writer, p ui.Palette, observed networkview.Snapshot) {
	others := netOtherEndpoints(observed)
	if len(others) == 0 {
		return
	}
	fmt.Fprintln(w, "\n  Other observed endpoints")
	for _, other := range others {
		row := other.peer + " · " + netTransportLabel(other.transport)
		if other.count > 1 {
			row += " ×" + strconv.Itoa(other.count)
		}
		fmt.Fprintf(w, "    %s · %s\n", row, p.Dim(other.reason))
	}
}

// writeNetRawTraffic shows what each raw rule carried. AddressGrants is
// per-RULE kernel accounting, not a host list: the filter counts what a grant
// passed without recording which address inside a CIDR it went to, so no
// destination is shown and a grant that carried nothing gets no row.
func writeNetRawTraffic(w io.Writer, observed networkview.Snapshot) {
	var grants []networkview.AddressGrantObservation
	for _, grant := range observed.AddressGrants {
		if grant.Packets != 0 || grant.Bytes != 0 {
			grants = append(grants, grant)
		}
	}
	if len(grants) == 0 {
		return
	}
	fmt.Fprintln(w, "\n  Raw traffic — counted per rule, no destination is recorded")
	for _, grant := range grants {
		fmt.Fprintf(w, "    rule %s · %s · %s\n", netShortID(grant.RuleID), netPlural(uint64(grant.Packets), "packet"), ui.Bytes(uint64(grant.Bytes)))
	}
}

// netWorkloadDestinations groups every proven workload connection by the
// destination a person recognizes, then by resolved peer, so a host retried
// twenty times is one row with a count instead of twenty. Distinct peers are
// never merged: one name behind two addresses is two facts. Order is the
// (name, port, transport) tuple, then the peer — stable whatever order the
// evidence arrived in.
func netWorkloadDestinations(observed networkview.Snapshot) []netDestinationGroup {
	index := map[string]*netDestinationGroup{}
	var groups []*netDestinationGroup
	for _, c := range observed.Connections {
		if c.NameSource != netWorkloadName {
			continue
		}
		key := netDestinationKey(c)
		group := index[key.label()]
		if group == nil {
			group = &key
			index[key.label()], groups = group, append(groups, group)
		}
		totals := netPeerOf(group, netPeerLabel(c.Peer, c.DestinationID))
		switch c.State {
		case "failed":
			totals.failed++
			if totals.failure == "" {
				totals.failure = c.Reason
			}
		case "connecting":
			totals.connecting++
		default:
			totals.connections++
			totals.sent.add(c.SentBytes)
			totals.received.add(c.ReceivedBytes)
		}
	}
	slices.SortFunc(groups, func(x, y *netDestinationGroup) int {
		return cmp.Or(strings.Compare(x.name, y.name), strings.Compare(x.port, y.port), strings.Compare(x.transport, y.transport))
	})
	out := make([]netDestinationGroup, 0, len(groups))
	for _, group := range groups {
		slices.SortFunc(group.peers, func(x, y *netPeerTotals) int { return strings.Compare(x.peer, y.peer) })
		out = append(out, *group)
	}
	return out
}

func netPeerOf(group *netDestinationGroup, peer string) *netPeerTotals {
	for _, existing := range group.peers {
		if existing.peer == peer {
			return existing
		}
	}
	totals := &netPeerTotals{peer: peer}
	group.peers = append(group.peers, totals)
	return totals
}

// netPeerLabel names a peer without inventing one: a withheld address keeps
// its opaque id so two withheld peers still read as two.
func netPeerLabel(peer, destinationID string) string {
	if peer == "" && destinationID != "" {
		return "address withheld (" + netShortID(destinationID) + ")"
	}
	return netUnknownIfEmpty(peer)
}

// netDestinationKey is the destination as the human asked for it: the observed
// hostname where there is one, the peer address where there legitimately is
// not. It never invents a name for an address.
func netDestinationKey(c networkview.Connection) netDestinationGroup {
	name, port := c.Name, netPeerPort(c.Peer)
	if name == "" {
		if host, _, ok := netSplitPeer(c.Peer); ok {
			name = host
		} else {
			name, port = netPeerLabel(c.Peer, c.DestinationID), ""
		}
	}
	return netDestinationGroup{name: name, port: port, transport: c.Transport}
}

func netSplitPeer(peer string) (host, port string, ok bool) {
	index := strings.LastIndex(peer, ":")
	if index <= 0 || index == len(peer)-1 {
		return "", "", false
	}
	host, port = peer[:index], peer[index+1:]
	if strings.Trim(port, "0123456789") != "" {
		return "", "", false
	}
	return strings.Trim(host, "[]"), port, true
}

func netPeerPort(peer string) string {
	_, port, _ := netSplitPeer(peer)
	return port
}

func netTransportLabel(transport string) string {
	switch transport {
	case "":
		return "UNKNOWN"
	case "tls", "tcp", "udp", "dns":
		return strings.ToUpper(transport)
	}
	return transport
}

// netOtherEndpoint is a remote endpoint this run observed without proving what
// owned it. The reason is the point of the row.
type netOtherEndpoint struct {
	peer, transport, reason string
	count                   int
}

// netOtherEndpoints keeps the sockets nothing could attribute. Coop's own
// resolver connection (`trusted-maintenance`, metered in the maintenance
// counters) and an ownerless closing kernel control block (`socket-inventory`)
// are explained internals: they stay in --json and cost no line here.
func netOtherEndpoints(observed networkview.Snapshot) []netOtherEndpoint {
	index := map[string]*netOtherEndpoint{}
	var order []*netOtherEndpoint
	for _, c := range observed.Connections {
		if c.NameSource != "unattributed" && c.NameSource != "unattributed-history" {
			continue
		}
		peer, reason := netPeerLabel(c.Peer, c.DestinationID), netEndpointReason(c.Reason)
		key := peer + "\x00" + c.Transport + "\x00" + reason
		if index[key] == nil {
			row := &netOtherEndpoint{peer: peer, transport: c.Transport, reason: reason}
			index[key], order = row, append(order, row)
		}
		index[key].count++
	}
	slices.SortFunc(order, func(x, y *netOtherEndpoint) int {
		return cmp.Or(strings.Compare(x.peer, y.peer), strings.Compare(x.transport, y.transport), strings.Compare(x.reason, y.reason))
	})
	out := make([]netOtherEndpoint, 0, len(order))
	for _, row := range order {
		out = append(out, *row)
	}
	return out
}

// netEndpointReason says, in the operator's words, why coop saw this socket but
// cannot call it workload traffic. The codes are the collector's.
func netEndpointReason(reason string) string {
	switch reason {
	case "attribution_pending":
		return "not matched to a connection yet"
	case "unattributed_socket":
		return "never matched to a connection"
	case "agent_attempt_unverified":
		return "an agent connection attempt that could not be verified"
	case "unexpected_agent_connection":
		return "an agent connection outside the gateway's capture"
	case "unexpected_socket_owner":
		return "opened by a process coop did not expect"
	case "socket_inode_unavailable":
		return "its owner could not be read"
	case "socket_join_capacity":
		return "too many sockets were waiting to be matched"
	case "":
		return "no connection detail was recorded"
	}
	return reason
}

// ------------------------------------------------------------ exceptions ----

// netWarning is one thing that actually happened. Sections with nothing to say
// print nothing at all: there are no placeholders in this view.
type netWarning struct {
	headline string
	rows     []string
}

// netRunExceptions appends only facts that occurred, in the order an operator
// acts on them: what was blocked, what was flagged, what was not observed, what
// did not run normally, and what is still owed.
func netRunExceptions(p ui.Palette, view netRunView, inspection networkstate.Inspection) []netWarning {
	var out []netWarning
	if blocked := netBlockedWarning(p, view, inspection.Observed); blocked != nil {
		out = append(out, *blocked)
	}
	if alerts := netAlertWarning(inspection.Observed); alerts != nil {
		out = append(out, *alerts)
	}
	if gap := netEvidenceWarning(view, inspection); gap != nil {
		out = append(out, *gap)
	}
	out = append(out, netHealthWarnings(inspection)...)
	if cleanup := netCleanupWarning(view, inspection); cleanup != nil {
		out = append(out, *cleanup)
	}
	return out
}

// netRefusalGroup coalesces the repeats of one blocked destination. The exact
// event identity stays in the evidence and in --json; a human argues with the
// hostname they recognize.
type netRefusalGroup struct {
	name, label string
	count       int
	candidate   bool // the evidence carries an exact rule that would have allowed it
}

func netRefusalGroups(denials []networkview.Denial) []netRefusalGroup {
	index := map[string]*netRefusalGroup{}
	var order []*netRefusalGroup
	for _, denial := range denials {
		label := netRefusalLabel(denial)
		if index[label] == nil {
			row := &netRefusalGroup{name: denial.Name, label: label}
			index[label], order = row, append(order, row)
		}
		index[label].count++
		index[label].candidate = index[label].candidate || denial.Candidate != nil
	}
	out := make([]netRefusalGroup, 0, len(order))
	for _, row := range order {
		out = append(out, *row)
	}
	return out
}

// netRefusalLabel is one blocked destination the way its row reads: the name
// (or address) with the port the attempt carried, the boundary that refused
// it, and the reason only when it is not the ordinary "no rule allows this".
func netRefusalLabel(denial networkview.Denial) string {
	destination := netDestination(denial.Name, denial.Peer, denial.DestinationID)
	if denial.Name != "" && denial.Port != nil {
		destination += ":" + strconv.Itoa(*denial.Port)
	}
	label := destination + " · " + netRefusalKind(denial.Kind)
	if denial.Reason != "" && denial.Reason != "unapproved_name" {
		label += " — " + netRefusalReasonText(denial.Reason)
	}
	return label
}

func netRefusalKind(kind string) string {
	switch kind {
	case "dns_denied":
		return "DNS"
	case "tls_denied":
		return "TLS"
	case "direct_tcp_attempt":
		return "direct connection"
	case "admission_failed":
		return "admission"
	}
	return kind
}

// netRefusalReasonText translates a retained reason code the way `explain`
// would, in one clause. An unknown code is shown as itself: a guess would be a
// worse answer than the code.
func netRefusalReasonText(reason string) string {
	switch reason {
	case "protected_destination", "unsafe_dns_answer":
		return "a protected address"
	case "fixed_egress_policy", "protocol_not_allowed", "port_not_allowed":
		return "no rule allows that protocol and port"
	case "tls_ech_unsupported":
		return "encrypted ClientHello is not supported"
	case "tls_name_missing", "tls_name_invalid":
		return "the connection carried no usable name"
	case "tls_direct_dial_refused":
		return "dialed straight at the gateway"
	case "upstream_unreachable", "upstream_connection_failed":
		return "the destination did not answer"
	case "upstream_establishment_exhausted":
		return "the destination could not be reached"
	case "observation_unavailable":
		return "it could not be observed"
	}
	return reason
}

func netBlockedWarning(p ui.Palette, view netRunView, observed networkview.Snapshot) *netWarning {
	groups := netRefusalGroups(observed.Denials)
	packets := uint64(0)
	if observed.Counters != nil && observed.Counters.DeniedPackets != nil {
		packets = uint64(*observed.Counters.DeniedPackets)
	}
	if len(groups) == 0 && packets == 0 {
		return nil
	}
	// One destination refused at two boundaries is still one destination; the
	// rows beneath say how. Raw packets are a kernel tally, never attributed:
	// the filter drops a refused datagram without recording where it was going.
	var destinations []string
	for _, denial := range observed.Denials {
		destinations = appendUnique(destinations, netDestination(denial.Name, denial.Peer, denial.DestinationID))
	}
	warning := &netWarning{headline: ui.Count(len(destinations), "destination") + " " + netWas(len(destinations)) + " blocked"}
	if len(groups) == 0 {
		warning.headline = netPlural(packets, "raw packet") + " " + netWas(int(min(packets, 2))) + " blocked with no destination recorded"
	}
	for _, group := range groups {
		row := group.label
		if group.count > 1 {
			row += " ×" + strconv.Itoa(group.count)
		}
		warning.rows = append(warning.rows, row)
	}
	if len(groups) != 0 && packets != 0 {
		warning.rows = append(warning.rows, netPlural(packets, "raw packet")+" "+netWas(int(min(packets, 2)))+" blocked with no destination recorded")
	}
	// The explain action names the thing a human recognizes. Approval guidance
	// is offered only when the evidence itself proves a rule that would have
	// allowed the attempt — a DNS refusal or a protected address proves none.
	named, candidate := "", false
	for _, group := range groups {
		if group.name == "" {
			continue
		}
		if named == "" || group.candidate && !candidate {
			named, candidate = group.name, group.candidate
		}
	}
	if named != "" {
		warning.rows = append(warning.rows, p.Dim("coop net explain "+named+" --run "+netShortID(view.ID)+"   # why"))
	}
	if candidate {
		warning.rows = append(warning.rows, p.Dim("To allow it: add the rule shown by 'coop net explain', then run 'coop net approve'"))
	}
	return warning
}

func netAlertWarning(observed networkview.Snapshot) *netWarning {
	suppressed := uint64(observed.Loss.SuppressedAlerts)
	if len(observed.Alerts) == 0 && suppressed == 0 {
		return nil
	}
	warning := &netWarning{headline: ui.Count(len(observed.Alerts), "alert") + " " + netWas(len(observed.Alerts)) + " raised"}
	for _, alert := range observed.Alerts {
		warning.rows = append(warning.rows, netAlertText(alert))
	}
	// A suppressed alert is not an absent one: say how many never made it into
	// the record rather than let the list read as the whole story.
	if suppressed != 0 {
		row := netPlural(suppressed, "more alert") + " exceeded this run's alert budget and " + netWas(int(min(suppressed, 2))) + " not recorded"
		if len(observed.Alerts) == 0 {
			warning.headline, row = netPlural(suppressed, "alert")+" exceeded this run's alert budget and "+netWas(int(min(suppressed, 2)))+" not recorded", ""
		}
		if row != "" {
			warning.rows = append(warning.rows, row)
		}
	}
	return warning
}

// netAlertText states what the detector thresholded, in human units: the
// category as a phrase, the observed value, the window and the threshold. An
// encrypted byte count is not evidence of intent, so it is stated, not judged.
func netAlertText(alert networkview.Alert) string {
	threshold := strconv.FormatUint(uint64(alert.Threshold.Value), 10)
	facts := alert.Facts
	var what, observed string
	switch alert.Category {
	case "protected_destination":
		what, observed = "traffic aimed at a protected address", netPlural(uint64(facts.Packets), "packet")
	case "denial_burst_dns":
		what, observed = "a burst of blocked DNS lookups", strconv.FormatUint(uint64(facts.DNSQueries), 10)
	case "denial_burst_tls":
		what, observed = "a burst of blocked TLS connections", strconv.FormatUint(uint64(facts.DeniedTLS), 10)
	case "denial_burst_packets":
		what, observed = "a burst of blocked packets", strconv.FormatUint(uint64(facts.Packets), 10)
	case "new_connections":
		what, observed = "many new connections", strconv.FormatUint(uint64(facts.Connections), 10)
	case "outbound_volume":
		what, observed, threshold = "a large upload", ui.Bytes(uint64(facts.SentBytes))+" sent", ui.Bytes(uint64(alert.Threshold.Value))
	case "outbound_rate_rise":
		what, observed, threshold = "an upload rate rise", ui.Bytes(uint64(facts.SentBytes))+" sent", threshold+"× the earlier rate"
	case "health_enforcer", "health_gateway", "health_resolver", "health_collector":
		layer := netLayerName(strings.TrimPrefix(alert.Category, "health_"))
		row := layer + " was " + netUnknownIfEmpty(facts.HealthStatus) + " for " + netDuration(uint64(alert.WindowMillis)) + " (" + alert.Severity + ")"
		if facts.Reason != "" {
			row += " — " + netEvidenceReasonText(facts.Reason)
		}
		return row
	default:
		what, observed = alert.Category, strconv.FormatUint(uint64(facts.Packets), 10)+" "+alert.Threshold.Unit
	}
	row := what + " (" + alert.Severity + ") — " + observed
	if alert.WindowMillis != 0 {
		row += " in " + netDuration(uint64(alert.WindowMillis))
	}
	return row + ", over a threshold of " + threshold
}

// netEvidenceWarning is the one place a gap in the record is stated: the rows
// above may then be a lower bound, and the view must say so rather than let
// them read as an exhaustive host list.
func netEvidenceWarning(view netRunView, inspection networkstate.Inspection) *netWarning {
	observed := inspection.Observed
	loss := observed.Loss
	var reasons []string
	add := func(text string) { reasons = appendUnique(reasons, text) }
	if !netRunTerminal(inspection) && !netRunActive(view, inspection) {
		// No receipt, no terminal marker, nothing observing now and no
		// supervisor alive: the run stopped without sealing a record.
		if observed.Sequence == 0 || observed.AsOf.IsZero() {
			add("nothing was observed for this run")
		} else {
			add("no final record was sealed — the last observation was at " + observed.AsOf.UTC().Format(time.RFC3339))
		}
	}
	if loss.Records != 0 {
		add(netPlural(uint64(loss.Records), "observation record") + " " + netWas(int(min(uint64(loss.Records), 2))) + " lost")
	}
	if loss.Unknown {
		add("an unknown number of observations may have been lost")
	}
	if loss.DetailTruncated {
		text := "only part of the connection and blocked-attempt detail was kept"
		if loss.OmittedDetails != nil && *loss.OmittedDetails != 0 {
			text += " (" + netPlural(uint64(*loss.OmittedDetails), "detail") + " dropped)"
		}
		add(text)
	}
	for _, reason := range loss.Reasons {
		add(netEvidenceReasonText(reason))
	}
	for _, gap := range netCoverageGaps(observed.Coverage) {
		add(gap)
	}
	if collector := observed.Health.Collector; collector.Status != "" && collector.Status != "ready" && collector.Status != "stopped" && len(reasons) == 0 {
		add("the observer was " + collector.Status + " — " + netEvidenceReasonText(collector.Reason))
	}
	if len(reasons) == 0 && inspection.Receipt != nil && inspection.Receipt.Completeness != "complete" {
		add("the record was sealed with " + netUnknownIfEmpty(inspection.Receipt.Completeness) + " evidence")
	}
	if len(reasons) == 0 {
		return nil
	}
	warning := &netWarning{headline: "Some network activity may be missing"}
	if len(reasons) == 1 {
		warning.headline += " — " + reasons[0]
		return warning
	}
	warning.rows = reasons
	return warning
}

// netCoverageGaps names each measurement that is a lower bound or missing, in
// the operator's words. A retained total can be inexact while another source
// stays exact, so they are listed one by one rather than blurred into "partial".
func netCoverageGaps(coverage networkview.Coverage) []string {
	var out []string
	for _, metric := range []struct {
		name  string
		value networkview.MetricCoverage
	}{
		{"the byte totals", coverage.ProxyBytes}, {"the connection count", coverage.Connections},
		{"the upstream failure count", coverage.UpstreamFailures}, {"the packet counts", coverage.KernelPackets},
		{"the list of blocked attempts", coverage.GuardDenials}, {"coop's own maintenance counts", coverage.MaintenanceQueries},
		{"coop's own maintenance bytes", coverage.MaintenanceBytes}, {"the socket sample", coverage.SocketInventory},
		{"connection ownership", coverage.BoundaryAttribution},
	} {
		var text string
		switch metric.value.Status {
		case "", "exact":
			continue
		case "lower-bound":
			text = metric.name + " are a lower bound"
		default:
			text = metric.name + " could not be measured"
		}
		if metric.value.Reason != "" {
			text += " — " + netEvidenceReasonText(metric.value.Reason)
		}
		out = append(out, text)
	}
	return out
}

// netEvidenceReasonText translates the collector's loss and coverage codes. The
// codes are retained as they are in --json; an unknown one is shown verbatim.
func netEvidenceReasonText(reason string) string {
	switch reason {
	case "":
		return "the reason was not recorded"
	case "observation_gap":
		return "an observation was missed"
	case "unattributed_socket":
		return "a socket could not be matched to a connection"
	case "unexpected_agent_connection":
		return "the agent opened a connection outside the gateway's capture"
	case "agent_attempt_unverified":
		return "an agent connection attempt could not be verified"
	case "unexpected_socket_owner":
		return "a socket belonged to a process coop did not expect"
	case "socket_inode_unavailable":
		return "a socket's owner could not be read"
	case "socket_inode_changed":
		return "a socket changed identity while it was being matched"
	case "socket_join_capacity":
		return "too many sockets were waiting to be matched"
	case "socket_history_identity_capacity":
		return "this run's socket history filled up"
	case "socket_inventory_unavailable":
		return "the socket table could not be read"
	case "socket_inventory_truncated":
		return "the socket table was read only in part"
	case "kernel_counters_unavailable":
		return "the kernel counters could not be read"
	case "kernel_terminal_sample_unavailable":
		return "the final observation was not recorded"
	case "snapshot_detail_limit":
		return "the record's detail limit was reached"
	case "alert_sequence_or_suppression_saturated":
		return "the alert counter saturated"
	case "proxy_close_event_missing":
		return "a connection's close was never observed"
	case "gateway_not_ready":
		return "it never became ready"
	case "gateway_unavailable":
		return "it could not be reached"
	case "enforcement_unavailable":
		return "the packet filter could not be read"
	case "controller_observation_unavailable":
		return "the controller could not be observed"
	case "controller_request_refused":
		return "the controller refused a request"
	case "gateway_connection_capacity":
		return "it ran out of connection capacity"
	}
	return reason
}

func netLayerName(layer string) string {
	switch layer {
	case "enforcer":
		return "the packet filter"
	case "gateway":
		return "the gateway"
	case "resolver":
		return "DNS resolution"
	case "collector":
		return "the observer"
	}
	return layer
}

// netHealthWarnings reports an enforcement layer that did not run normally. A
// gateway the ordinary teardown stopped is NOT unhealthy: reporting a terminal
// run's stopped layers would put a warning on every clean result. The observer
// is an evidence matter and is reported with the evidence gap instead.
func netHealthWarnings(inspection networkstate.Inspection) []netWarning {
	terminal := netRunTerminal(inspection)
	var out []netWarning
	for _, layer := range []struct {
		name  string
		value networkview.Health
	}{
		{"enforcer", inspection.Observed.Health.Enforcer},
		{"gateway", inspection.Observed.Health.Gateway},
		{"resolver", inspection.Observed.Health.Resolver},
	} {
		if layer.value.Status == "" || layer.value.Status == "ready" || layer.value.Status == "stopped" && terminal {
			continue
		}
		name := netLayerName(layer.name)
		headline := strings.ToUpper(name[:1]) + name[1:] + " was " + layer.value.Status
		if layer.value.Reason != "" {
			headline += " — " + netEvidenceReasonText(layer.value.Reason)
		}
		out = append(out, netWarning{headline: headline})
	}
	return out
}

// netCleanupWarning reports cleanup ONLY when it is still owed after coop's own
// bounded recovery attempt. A live run's containers are supposed to exist, a
// supervisor that is still running owns its own cleanup, and a successful
// automatic recovery is silent.
func netCleanupWarning(view netRunView, inspection networkstate.Inspection) *netWarning {
	if inspection.Cleanup != "pending" || netRunLive(inspection) || view.Cleanup.Expected {
		return nil
	}
	warning := &netWarning{headline: "Cleanup incomplete"}
	if view.Cleanup.Blocker != "" {
		warning.headline += " — " + view.Cleanup.Blocker
	}
	// The remedy names the external thing only the operator can fix; coop
	// retries the recovery itself, so it never asks for that — and a view that
	// made no attempt of its own still says who will.
	remedy := view.Cleanup.Remedy
	if remedy == "" {
		remedy = "Coop will retry automatically"
	}
	warning.rows = append(warning.rows, remedy)
	return warning
}

// writeNetWarnings renders the exceptions a view collected. A section with
// nothing to say prints nothing: there are no placeholder rows here.
func writeNetWarnings(w io.Writer, p ui.Palette, warnings []netWarning) {
	for _, warning := range warnings {
		fmt.Fprintf(w, "\n%s %s\n", p.Yellow("⚠"), p.Yellow(warning.headline))
		for _, row := range warning.rows {
			// Exactly two ASCII spaces, so the continuation aligns under the
			// warning text with the glyph and its space above it.
			fmt.Fprintf(w, "  %s\n", row)
		}
	}
}

// ---------------------------------------------------------------- units -----

// netShortIDLen is the display prefix of a random 32-hex identity: enough to
// name one run or rule on this host, short enough to read and retype. Every
// command that takes a run resolves any unique prefix; --json keeps the full id.
const netShortIDLen = 8

func netShortID(id string) string {
	if len(id) <= netShortIDLen {
		return id
	}
	return id[:netShortIDLen]
}

func netWas(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

// netDuration renders an observation window the way a person says it.
func netDuration(millis uint64) string {
	switch {
	case millis < 1000:
		return strconv.FormatUint(millis, 10) + "ms"
	case millis < 60_000:
		return strconv.FormatUint((millis+500)/1000, 10) + "s"
	}
	seconds := (millis + 500) / 1000
	if seconds%60 == 0 {
		return strconv.FormatUint(seconds/60, 10) + "m"
	}
	return strconv.FormatUint(seconds/60, 10) + "m " + strconv.FormatUint(seconds%60, 10) + "s"
}

func appendUnique(out []string, value string) []string {
	if slices.Contains(out, value) {
		return out
	}
	return append(out, value)
}
