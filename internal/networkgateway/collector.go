package networkgateway

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/networkview"
)

const (
	CollectorVersion      = "gateway-v1"
	MaxClosedDetails      = 128
	MaxDenialDetails      = 128
	MaxUnknownDetails     = 128
	MaxPendingJoins       = 128
	ObservationInterval   = time.Second
	ObservationStaleAfter = 3 * time.Second
)

type collectedFlow struct {
	row           networkview.Connection
	peer          netip.AddrPort
	tuple         SocketTuple
	connectionID  string
	connected     bool
	lastBoot      BootInstant
	meterBoot     BootInstant
	meterDuration *uint64
	inode         uint64
	privateClosed BootInstant
}

type socketAttemptKey struct {
	Tuple SocketTuple
	UID   uint32
	Inode uint64
}

type pendingSocket struct {
	firstSeen BootInstant
	expired   bool
}

// retainedSocket is the identity of a socket whose owner is gone but whose
// kernel remnant outlives it: the exact tuple its owner reported, the inode when
// some sample bound one, and the boot instant the owner released it. The
// inventory alone cannot tell such a remnant from a socket nobody admitted. It
// explains a socket; it never attributes bytes or ownership.
type retainedSocket struct {
	inode    uint64 // 0 when no sample ever bound one
	released BootInstant
}

// maintenanceIdentity is a live resolver socket an inventory has bound to an
// exact kernel inode. A maintenance connection is born knowing only its tuple,
// and that alone cannot tell its remnant from a stranger's socket at the same
// tuple once the resolver releases it.
type maintenanceIdentity struct {
	tuple SocketTuple
	inode uint64
}

// Collector owns bounded retained evidence, never egress authority. It consumes
// each source once; snapshots are detached cached values and cannot cause I/O.
type Collector struct {
	mu                          sync.Mutex
	sampleMu                    sync.Mutex
	identity                    Identity
	clock                       *BootClock
	key                         [32]byte
	started                     BootInstant
	guard                       *GuardEvents
	envoy                       *EnvoyEvents
	resolver                    *Resolver // agent-policy resolver; candidate attribution remains bound to it
	resolvers                   []*Resolver
	doh                         *DoH
	controller                  ControllerClient
	inventory                   func() ([]SocketRow, error)
	flows                       map[string]*collectedFlow
	closed                      []networkview.Connection
	closedSockets               map[SocketTuple]retainedSocket
	maintenanceInodes           map[uint64]maintenanceIdentity
	retiredMaintenance          map[SocketTuple]retainedSocket
	denials                     []networkview.Denial
	guardCursor, envoyCursor    uint64
	guardTotals                 GuardTotals
	envoyTotals                 EnvoyTotals
	sent, received, connections networkview.Count
	upstreamFailures            networkview.Count
	proxyPartial                bool
	proxyPartialReason          string
	proxyReasons                []string
	detailLost                  networkview.Count
	detailUnknown               bool
	inventorySequence           networkview.Count
	inventoryAt                 *time.Time
	inventoryGap                bool
	previousAttempts            map[socketAttemptKey]struct{}
	previousUnknown             map[socketAttemptKey]struct{}
	pending                     map[socketAttemptKey]pendingSocket
	boundary                    boundary
	boundaryGap                 string
	unexpectedAgent             bool
	unverifiedAttempt           bool
	unexpectedOwner             bool
	inodeUnavailable            bool
	historyIdentityLost         bool
	terminal                    bool
	alerts                      *alertDetector
	lastKernel                  KernelSample
	kernelPartial               bool
	lastGuardAt, lastEnvoyAt    *time.Time
	snapshot                    networkview.Snapshot
	wake                        chan struct{} // a sample wanted before the next tick (Wake)
}

func NewCollector(g *Guard, e *EnvoyEvents, doh *DoH, extraResolvers ...*Resolver) (*Collector, error) {
	if g == nil || e == nil || doh == nil || e.clock.Domain() != g.clock.Domain() {
		return nil, Failure("collector_configuration_invalid")
	}
	resolvers := []*Resolver{g.resolver}
	for _, resolver := range extraResolvers {
		if resolver == nil || resolver.domain != g.clock.Domain() {
			return nil, Failure("collector_configuration_invalid")
		}
		resolvers = append(resolvers, resolver)
	}
	c := &Collector{identity: g.controller.Identity, clock: g.clock, started: g.clock.instant(), guard: g.events,
		envoy: e, resolver: g.resolver, resolvers: resolvers, doh: doh, controller: g.controller, boundary: g.boundary(),
		inventory: func() ([]SocketRow, error) { return readSocketInventory(g.boundary()) }, flows: make(map[string]*collectedFlow),
		wake: make(chan struct{}, 1)}
	if _, err := rand.Read(c.key[:]); err != nil || !c.started.Valid() {
		return nil, Failure("collector_unavailable")
	}
	c.snapshot = networkview.Snapshot{Version: networkview.Version, RunID: c.identity.RunID, Epoch: c.identity.Epoch,
		PolicyFingerprint: c.identity.PolicyFingerprint, Mode: g.policy.Mode, Availability: "starting",
		Scope: "proxy-streams-and-sampled-tcp-sockets", Projection: "owner-local"}
	return c, nil
}

func (c *Collector) opaque(kind, value string) string {
	h := hmac.New(sha256.New, c.key[:])
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s", c.identity.RunID, c.identity.Epoch, kind, value)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func (c *Collector) Snapshot() networkview.Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.snapshot.Project(true)
	s.Projection = "owner-local"
	return s
}

func (c *Collector) Run(ctx context.Context, ready func() bool) {
	ticker := time.NewTicker(ObservationInterval)
	defer ticker.Stop()
	for {
		c.Sample(ctx, ready())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-c.wake:
		}
	}
}

// Wake asks Run for a sample now rather than at its next tick. The gateway turning ready is news a
// launch is polling for — it starts nothing until a snapshot says so — and the tick is a second away.
// It never blocks: a wake already pending covers this one, and once Run has returned nothing samples.
func (c *Collector) Wake() {
	if c == nil {
		return
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Collector) Sample(ctx context.Context, ready bool) {
	c.sample(ctx, ready, false, 0)
}

func (c *Collector) sample(ctx context.Context, ready, terminal bool, cutoff BootInstant) {
	c.sampleMu.Lock()
	defer c.sampleMu.Unlock()
	// Ownership spans the inventory read. A retired/reused tuple cannot inherit
	// maintenance attribution just because it points at the resolver's IP.
	owned := c.doh.sockets.snapshot()
	rows, inventoryErr := c.inventory()
	owned = slices.DeleteFunc(owned, func(s maintenanceSocket) bool { return !c.doh.sockets.stillOwned(s) })
	budget := KernelSampleTimeout
	if cutoff.Valid() {
		budget = ControlTimeout
	}
	bounded, cancel := context.WithTimeout(ctx, budget)
	kernel, kernelErr := c.controller.countersAfter(bounded, cutoff)
	if terminal && !cutoff.Valid() {
		kernelErr = Failure("terminal_clock_unavailable")
	}
	cancel()
	proxies, envoyTotals := c.envoy.Drain(MaxGuardEvents)
	// Guard registration precedes the write that lets Envoy observe a flow.
	// Drain proxy first so every consumed proxy event's admission registration
	// is already eligible for the following guard drain (unless explicitly lost).
	guards, guardTotals := c.guard.Drain(MaxGuardEvents)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ingest(guards, guardTotals, proxies, envoyTotals)
	c.terminal = terminal
	c.publish(kernel, kernelErr, rows, inventoryErr, owned, ready)
}

func (c *Collector) ingest(guards []GuardEvent, gt GuardTotals, proxies []EnvoyEvent, et EnvoyTotals) {
	for _, event := range guards {
		if event.Sequence <= c.guardCursor {
			continue
		}
		c.guardCursor = event.Sequence // consumed cursor, not producer watermark
		at := event.At
		c.lastGuardAt = &at
		switch event.Kind {
		case "flow_registered":
			if _, exists := c.flows[event.FlowID]; exists {
				c.markProxyPartial("duplicate_flow_registration")
				continue
			}
			// One drain can contain endings for old flows and registrations
			// for replacements. Bound transient joins separately, then compact
			// after processing the proxy endings in this same sample.
			if len(c.flows) >= MaxGuardFlows+MaxGuardEvents {
				c.loseDetail()
				c.markProxyPartial("flow_join_capacity")
				continue
			}
			at := event.At
			peer := netip.AddrPortFrom(event.Peer, uint16(event.Port))
			c.flows[event.FlowID] = &collectedFlow{peer: peer, lastBoot: event.BootAt,
				row: networkview.Connection{ID: c.opaque("flow", event.FlowID), DestinationID: c.opaque("destination", event.Name),
					State: "connecting", Transport: "tls", Name: event.Name, NameSource: "sni", Service: event.Service, Peer: peer.String(),
					RuleID: event.RuleID, StartedAt: &at, ObservedAt: event.At}}
		case "private_flow_closed":
			if f := c.flows[event.FlowID]; f != nil {
				f.privateClosed = event.BootAt
			}
		case "tls_denied", "dns_denied", "admission_failed":
			row := networkview.Denial{ID: c.opaque("guard", fmt.Sprint(event.Sequence)), Source: "guard", Sequence: networkview.Count(event.Sequence),
				At: event.At, Kind: event.Kind, Reason: event.Reason, Name: event.Name, Service: event.Service, Basis: "observed"}
			if event.Name != "" {
				row.DestinationID = c.opaque("destination", event.Name)
			}
			if event.Kind == "tls_denied" && event.Port != 0 {
				// The port a refused attempt was made on is the kernel's redirect
				// record, so it is evidence like the name — including when it is
				// this guard's own listener, which is what a direct dial reports.
				port := event.Port
				row.Port = &port
			}
			row.Candidate = c.candidate(row)
			c.denials = append(c.denials, row)
			if len(c.denials) > MaxDenialDetails {
				c.denials = c.denials[1:]
				c.loseDetail()
			}
		}
	}
	for _, event := range proxies {
		if event.Sequence <= c.envoyCursor {
			continue
		}
		c.envoyCursor = event.Sequence
		at := event.At
		c.lastEnvoyAt = &at
		f := c.flows[event.FlowID]
		if f == nil {
			// Without a retained admission owner we cannot distinguish an old
			// evicted flow from missing start evidence. Never add its cumulative
			// bytes again; inventory still exposes any currently live socket.
			c.markProxyPartial("proxy_flow_owner_missing")
			continue
		}
		if f.connectionID != "" && f.connectionID != event.ConnectionID {
			c.markProxyPartial("proxy_connection_identity_changed")
			f.row.Partial = true
			continue
		}
		f.connectionID = event.ConnectionID
		if event.Peer.IsValid() && event.Peer != f.peer {
			c.markProxyPartial("proxy_peer_mismatch")
			f.row.Partial = true
			continue
		}
		f.row.ObservedAt = event.At
		if event.Peer.IsValid() && event.Local.IsValid() {
			f.tuple = SocketTuple{Local: event.Local, Peer: event.Peer}
		}
		if !f.connected && (event.Phase == "TcpUpstreamConnected" || event.ConnectMillis != nil) {
			f.connected = true
			if !networkview.Add(&c.connections, 1) {
				c.markProxyPartial("connection_counter_saturated")
			}
			f.row.ConnectMillis = asCount(event.ConnectMillis)
		}
		failed := !f.connected && event.Phase == "TcpConnectionEnd" && (slices.Contains(event.Flags, "UF") || slices.Contains(event.Flags, "URX"))
		switch event.Phase {
		case "TcpUpstreamConnected":
			f.row.State = "open"
		case "TcpPeriodic":
			if f.connected {
				f.row.State = "open"
			}
		case "TcpConnectionEnd":
			f.row.State = "closed"
			if failed {
				f.row.State, f.row.Reason = "failed", "upstream_establishment_exhausted"
				if slices.Contains(event.Flags, "UF") {
					f.row.Reason = "upstream_connection_failed"
				}
				if !networkview.Add(&c.upstreamFailures, 1) {
					c.markProxyPartial("upstream_failure_counter_saturated")
				}
			} else if f.connected {
				switch event.CloseType {
				case "RemoteReset":
					f.row.Reason = "upstream_reset"
				case "LocalReset":
					f.row.Reason = "local_reset"
				case "Normal":
					f.row.Reason = "normal_close"
				}
				if slices.Contains(event.Flags, "UR") {
					f.row.Reason = "upstream_reset"
				}
			}
		}
		// Envoy's start-phase zero is not a measured byte meter. A confirmed
		// upstream lifecycle is necessary before accepting cumulative values.
		if (f.connected || failed) && event.Phase != "TcpConnectionStart" {
			sent, sentOK := c.meter(&f.row.SentBytes, &c.sent, event.Sent)
			received, receivedOK := c.meter(&f.row.ReceivedBytes, &c.received, event.Received)
			f.row.Rate = nil
			if !sentOK || !receivedOK {
				f.row.Partial = true
				c.markProxyPartial("proxy_meter_discontinuity")
			} else {
				f.row.Rate = producerRate(f.meterDuration, event.DurationMillis, f.meterBoot, event.BootAt, sent, received)
			}
			f.meterBoot = 0
			f.meterDuration = nil
			if sentOK && receivedOK {
				f.meterBoot = event.BootAt
				f.meterDuration = event.DurationMillis
			}
		}
		f.lastBoot = event.BootAt
		if event.Phase == "TcpConnectionEnd" {
			if !f.connected && !failed {
				f.row.Partial = true
				f.row.Reason = "upstream_outcome_unavailable"
				c.markProxyPartial("proxy_connected_event_missing")
			}
			f.row.Rate = nil
			c.closed = append(c.closed, f.row)
			if f.tuple.Peer.IsValid() {
				c.closedSockets = retainRemnant(c.closedSockets, f.tuple, f.inode, event.BootAt)
			}
			if len(c.closed) > MaxClosedDetails {
				c.closed = c.closed[1:]
				c.loseDetail()
			}
			delete(c.flows, event.FlowID)
		}
	}
	if gt.Lost > 0 || et.Lost > 0 || et.UnknownLoss || gt.Saturated || et.Saturated {
		c.markProxyPartial("source_event_loss")
	}
	if len(c.flows) > MaxGuardFlows {
		ids := make([]string, 0, len(c.flows))
		for id := range c.flows {
			ids = append(ids, id)
		}
		slices.SortFunc(ids, func(a, b string) int {
			left, right := c.flows[a], c.flows[b]
			if left.privateClosed.Valid() != right.privateClosed.Valid() {
				if left.privateClosed.Valid() {
					return -1
				}
				return 1
			}
			if left.lastBoot < right.lastBoot {
				return -1
			}
			if left.lastBoot > right.lastBoot {
				return 1
			}
			if a < b {
				return -1
			}
			if a > b {
				return 1
			}
			return 0
		})
		for _, id := range ids[:len(ids)-MaxGuardFlows] {
			delete(c.flows, id)
			c.loseDetail()
			c.markProxyPartial("flow_join_capacity")
		}
	}
	if gt.Sequence < c.guardTotals.Sequence || et.Sequence < c.envoyTotals.Sequence {
		c.markProxyPartial("source_sequence_regressed")
		return // never replace cumulative evidence with an older producer snapshot
	}
	c.guardTotals, c.envoyTotals = gt, et
}

func (c *Collector) loseDetail() {
	if !networkview.Add(&c.detailLost, 1) {
		c.detailUnknown = true
	}
}

func (c *Collector) markProxyPartial(reason string) {
	c.proxyPartial = true
	if c.proxyPartialReason == "" {
		c.proxyPartialReason = reason
	}
	if !slices.Contains(c.proxyReasons, reason) {
		if len(c.proxyReasons) < 16 {
			c.proxyReasons = append(c.proxyReasons, reason)
		} else {
			c.proxyReasons[15] = "additional_proxy_gaps"
		}
	}
}

func (c *Collector) markBoundaryGap(reason string) {
	if c.boundaryGap == "" {
		c.boundaryGap = reason
	}
}

// retainRemnant keeps a released socket's identity for the few samples its
// kernel socket can outlive its owner — a proxy-ended flow, or a maintenance
// connection the resolver let go. It is bounded by the joins it can explain, and
// the oldest release is the first to go: losing an entry only costs an
// explanation, it can never invent one.
func retainRemnant(set map[SocketTuple]retainedSocket, tuple SocketTuple, inode uint64, at BootInstant) map[SocketTuple]retainedSocket {
	if set == nil {
		set = make(map[SocketTuple]retainedSocket)
	}
	if _, replaced := set[tuple]; !replaced && len(set) >= MaxPendingJoins {
		oldest, found := SocketTuple{}, false
		for key, value := range set {
			if !found || value.released < set[oldest].released {
				oldest, found = key, true
			}
		}
		delete(set, oldest)
	}
	set[tuple] = retainedSocket{inode: inode, released: at}
	return set
}

// reconcileMaintenance keeps the resolver's own sockets identifiable across the
// sample that retires them. While a connection is owned, the first inventory
// showing its exact tuple — claimed by nothing else — binds its inode; when the
// resolver later releases it, that bound identity is retained the way a proxy
// close is, so the remnant the kernel keeps is explained as maintenance instead
// of an agent-flow gap. A connection no sample ever bound is retained as
// nothing: its bytes are already in the maintenance counters, and its peer
// address alone would explain any socket to the resolver's upstream.
func (c *Collector) reconcileMaintenance(owned []maintenanceSocket, rows []SocketRow, matched map[SocketTuple]int, now BootInstant) {
	if len(owned) == 0 && len(c.maintenanceInodes) == 0 {
		return
	}
	live := make(map[uint64]struct{}, len(owned))
	for _, m := range owned {
		live[m.ID] = struct{}{}
	}
	for id, identity := range c.maintenanceInodes {
		if _, owns := live[id]; owns {
			continue
		}
		delete(c.maintenanceInodes, id)
		c.retiredMaintenance = retainRemnant(c.retiredMaintenance, identity.tuple, identity.inode, now)
	}
	for _, m := range owned {
		if _, bound := c.maintenanceInodes[m.ID]; bound || matched[m.Tuple] != 1 {
			continue // a tuple two owners claim proves no identity
		}
		for _, row := range rows {
			if row.Tuple != m.Tuple || row.UID != 65532 || row.Inode == 0 {
				continue
			}
			if c.maintenanceInodes == nil {
				c.maintenanceInodes = make(map[uint64]maintenanceIdentity, len(owned))
			}
			c.maintenanceInodes[m.ID] = maintenanceIdentity{tuple: m.Tuple, inode: row.Inode}
			break
		}
	}
}

// maintenanceAccountsFor reports a socket the gateway's own resolver explains:
// the exact tuple AND inode of a maintenance connection this collector watched
// it release. Those bytes are already in the maintenance counters, so calling
// the remnant an unattributed socket would report the gateway's own accounted
// DNS leg as an agent-flow gap. Identity is the whole contract — a connection no
// sample bound explains nothing, and another socket at the same peer, even the
// pinned DoH upstream, is a stranger until its inode says otherwise. Like a
// retained close it explains a REMNANT, so it expires on the same bound and an
// unreadable clock folds nothing.
func (c *Collector) maintenanceAccountsFor(key socketAttemptKey, now BootInstant) bool {
	retired, held := c.retiredMaintenance[key.Tuple]
	if !held {
		return false
	}
	if !now.Valid() || !retired.released.Valid() {
		return false
	}
	if now.Before(retired.released) || now.Sub(retired.released) > ObservationStaleAfter {
		delete(c.retiredMaintenance, key.Tuple)
		return false
	}
	return key.UID == 65532 && key.Inode != 0 && key.Inode == retired.inode
}

// closeAccountsFor reports a socket the proxy's own evidence already explains:
// the exact upstream tuple Envoy reported for a flow it ended, and — once any
// sample bound one — the exact inode. A lingering remnant of an accounted flow
// is not a boundary gap. The first socket a close explains pins its identity,
// so one ended stream can never explain a second socket at the same tuple, and
// a socket no closed flow claims stays unattributed.
//
// A close explains a REMNANT — the few samples a kernel socket outlives its
// stream. Past the staleness bound it explains nothing and is evicted, so an
// ephemeral port reused minutes later cannot hide a real gap. An unreadable
// clock cannot bound anything, so it folds nothing and retains everything.
func (c *Collector) closeAccountsFor(key socketAttemptKey, now BootInstant) bool {
	closed, retained := c.closedSockets[key.Tuple]
	if !retained {
		return false
	}
	if !now.Valid() || !closed.released.Valid() {
		return false
	}
	if now.Before(closed.released) || now.Sub(closed.released) > ObservationStaleAfter {
		delete(c.closedSockets, key.Tuple)
		return false
	}
	if key.UID != 65532 || key.Inode == 0 || closed.inode != 0 && closed.inode != key.Inode {
		return false
	}
	closed.inode = key.Inode
	c.closedSockets[key.Tuple] = closed
	return true
}

func (c *Collector) socketID(key socketAttemptKey) string {
	return c.opaque("socket", fmt.Sprintf("%s/%s/%d/%d", key.Tuple.Local, key.Tuple.Peer, key.UID, key.Inode))
}

func (c *Collector) firstUnknown(key socketAttemptKey, completeInventory bool) bool {
	if _, seen := c.previousUnknown[key]; seen {
		return false
	}
	if completeInventory {
		return true // replaced with this inventory after publication
	}
	if c.previousUnknown == nil {
		c.previousUnknown = make(map[socketAttemptKey]struct{})
	}
	if len(c.previousUnknown) == MaxSocketInventory {
		// Incomplete inventories cannot prove which old identities disappeared.
		// Omit new history explicitly instead of cycling the retained ring.
		c.historyIdentityLost = true
		c.markBoundaryGap("socket_history_identity_capacity")
		return false
	}
	c.previousUnknown[key] = struct{}{}
	return true
}

func (c *Collector) retainUnknown(key socketAttemptKey, reason string, at time.Time) {
	id := c.socketID(key)
	if slices.ContainsFunc(c.closed, func(row networkview.Connection) bool { return row.ID == id }) {
		return
	}
	// Keep the first example, not a per-socket transition log; later security
	// classes also have independent sticky flags. This timestamp is when
	// reconciliation failed, not an invented socket start or close.
	c.closed = append(c.closed, networkview.Connection{ID: id, DestinationID: c.opaque("peer", key.Tuple.Peer.String()),
		State: "unknown", Reason: reason, Transport: "tcp", NameSource: "unattributed-history", Peer: key.Tuple.Peer.String(), ObservedAt: at, Partial: true})
	if len(c.closed) > MaxClosedDetails {
		c.closed = c.closed[1:]
		c.loseDetail()
	}
}
func asCount(n *uint64) *networkview.Count {
	if n == nil {
		return nil
	}
	return networkview.Value(*n)
}

func (c *Collector) meter(current **networkview.Count, total *networkview.Count, observed *uint64) (uint64, bool) {
	if observed == nil {
		return 0, false
	}
	previous := uint64(0)
	if *current != nil {
		previous = uint64(**current)
	}
	if *observed < previous {
		return 0, false
	}
	delta := *observed - previous
	*current = networkview.Value(*observed)
	return delta, networkview.Add(total, delta)
}

func producerRate(previous, now *uint64, receivedBefore, receivedNow BootInstant, sent, received uint64) *networkview.Rate {
	if previous == nil || now == nil || *now <= *previous || *now-*previous > uint64(ObservationStaleAfter.Milliseconds()) || !receivedNow.After(receivedBefore) {
		return nil
	}
	millis := *now - *previous
	arrival := receivedNow.Sub(receivedBefore)
	// Producer duration is the measurement interval. Catch-up log batches or
	// delivery gaps are not instantaneous upload bursts; reset that window.
	if arrival < time.Duration(millis)*time.Millisecond/2 || arrival > time.Duration(millis)*time.Millisecond*2 {
		return nil
	}
	seconds := float64(millis) / 1000
	return &networkview.Rate{SentPerSecond: float64(sent) / seconds, ReceivedPerSecond: float64(received) / seconds,
		WindowMillis: networkview.Count(millis), Measurement: "proxy-monotonic-window"}
}

// addressGrants attributes each kernel counter back to the grant that owns it.
// A counter with no matching grant is dropped rather than shown against a
// destination nobody approved.
func (c *Collector) addressGrants(counts map[string]GrantCount) []networkview.AddressGrantObservation {
	var out []networkview.AddressGrantObservation
	for counter, count := range counts {
		grant, ok := c.resolver.policy.GrantByCounter(counter)
		if !ok {
			continue
		}
		out = append(out, networkview.AddressGrantObservation{RuleID: grant.ID, Packets: count.Packets, Bytes: count.Bytes})
	}
	slices.SortFunc(out, func(a, b networkview.AddressGrantObservation) int { return strings.Compare(a.RuleID, b.RuleID) })
	return out
}

func grantsRegressed(prior, current map[string]GrantCount) bool {
	for counter, was := range prior {
		now, ok := current[counter]
		if !ok || now.Packets < was.Packets || now.Bytes < was.Bytes {
			return true
		}
	}
	return false
}

func mergeGrants(prior, current map[string]GrantCount) map[string]GrantCount {
	out := maps.Clone(current)
	if out == nil {
		out = map[string]GrantCount{}
	}
	for counter, was := range prior {
		now := out[counter]
		out[counter] = GrantCount{Packets: max(now.Packets, was.Packets), Bytes: max(now.Bytes, was.Bytes)}
	}
	return out
}

func exactCoverage() networkview.MetricCoverage { return networkview.MetricCoverage{Status: "exact"} }
func partialCoverage(reason string) networkview.MetricCoverage {
	return networkview.MetricCoverage{Status: "lower-bound", Reason: reason}
}
func missingCoverage(reason string) networkview.MetricCoverage {
	return networkview.MetricCoverage{Status: "unavailable", Reason: reason}
}

func (c *Collector) publish(kernel KernelSample, kernelErr error, rows []SocketRow, inventoryErr error, owned []maintenanceSocket, ready bool) {
	now := c.clock.instant()
	var truncation *inventoryTruncated
	if errors.As(inventoryErr, &truncation) {
		inventoryErr = nil
		c.inventoryGap = true
		c.markBoundaryGap("socket_inventory_truncated")
	}
	completeInventory := inventoryErr == nil && truncation == nil
	s := c.snapshot
	if !networkview.Add(&s.Sequence, 1) {
		c.markProxyPartial("observation_sequence_saturated")
	}
	s.AsOf = time.Now().UTC()
	if now.Valid() && !now.Before(c.started) {
		s.ElapsedMillis = networkview.Count(now.Sub(c.started).Milliseconds())
	}
	s.Availability = "available"
	s.Terminal = c.terminal
	s.Health = networkview.HealthLayers{Enforcer: networkview.Health{Status: "ready"}, Gateway: networkview.Health{Status: "ready"},
		Resolver: networkview.Health{Status: "ready"}, Collector: networkview.Health{Status: "ready"}}
	if c.terminal && c.envoyTotals.Stopped {
		s.Health.Gateway = networkview.Health{Status: "stopped"}
	} else if !ready {
		s.Health.Gateway = networkview.Health{Status: "unavailable", Reason: "gateway_not_ready"}
		s.Availability = "degraded"
	}
	if kernelErr != nil {
		s.Health.Enforcer = networkview.Health{Status: "unknown", Reason: "controller_observation_unavailable"}
	} else if !kernel.EnforcerReady {
		s.Health.Enforcer = networkview.Health{Status: "unavailable", Reason: "enforcement_unavailable"}
		s.Availability = "degraded"
	}
	s.Coverage = networkview.Coverage{ProxyBytes: exactCoverage(), Connections: exactCoverage(), UpstreamFailures: exactCoverage(), GuardDenials: exactCoverage(),
		KernelPackets: missingCoverage("kernel_counters_unavailable"), MaintenanceQueries: exactCoverage(), MaintenanceBytes: exactCoverage(), SocketInventory: exactCoverage(), BoundaryAttribution: exactCoverage()}
	s.Counters = &networkview.Counters{SentBytes: networkview.Value(uint64(c.sent)), ReceivedBytes: networkview.Value(uint64(c.received)),
		Connections: networkview.Value(uint64(c.connections)), UpstreamFailures: networkview.Value(uint64(c.upstreamFailures)), DeniedDNSQueries: networkview.Value(c.guardTotals.DeniedDNS), DeniedTLS: networkview.Value(c.guardTotals.DeniedTLS)}
	var queries, failures uint64
	s.Health.Resolver.Status, s.Health.Resolver.Reason = "ready", "resolver_initialized"
	resolverSaturated := false
	for _, resolver := range c.resolvers {
		q, f := resolver.MaintenanceCounts()
		if ^uint64(0)-queries < q || ^uint64(0)-failures < f {
			queries, failures, resolverSaturated = ^uint64(0), ^uint64(0), true
		} else {
			queries, failures = queries+q, failures+f
		}
		status, reason := resolver.health()
		if status != "ready" {
			s.Health.Resolver.Status, s.Health.Resolver.Reason = status, reason
		}
		resolverSaturated = resolverSaturated || resolver.counterSaturated.Load()
	}
	if s.Health.Resolver.Status != "ready" {
		s.Availability = "degraded"
	}
	s.Counters.MaintenanceQueries, s.Counters.MaintenanceFailures = networkview.Value(queries), networkview.Value(failures)
	if resolverSaturated {
		s.Coverage.MaintenanceQueries = partialCoverage("counter_saturated")
	}
	s.Counters.MaintenanceSentBytes = networkview.Value(c.doh.sockets.sent.Load())
	s.Counters.MaintenanceReceivedBytes = networkview.Value(c.doh.sockets.received.Load())
	if c.doh.sockets.partial.Load() {
		s.Coverage.MaintenanceBytes = partialCoverage("maintenance_observation_gap")
	}
	if c.guardTotals.Saturated {
		s.Coverage.GuardDenials = partialCoverage("counter_saturated")
	}
	kernelFresh := kernelErr == nil && kernel.Counters != nil && kernel.BootAt.Valid() && now.Valid() && !now.Before(kernel.BootAt) && now.Sub(kernel.BootAt) <= ObservationStaleAfter
	if kernelFresh {
		prior, k := c.lastKernel.Counters, kernel.Counters
		if prior != nil && (k.DeniedAgent < prior.DeniedAgent || k.ProtectedAgent < prior.ProtectedAgent || k.DeniedIngress < prior.DeniedIngress || k.DeniedService < prior.DeniedService || kernel.Sequence < c.lastKernel.Sequence || grantsRegressed(prior.Grants, k.Grants)) {
			c.kernelPartial = true
		}
		if prior != nil && c.kernelPartial {
			value := *k
			value.DeniedAgent, value.ProtectedAgent = max(k.DeniedAgent, prior.DeniedAgent), max(k.ProtectedAgent, prior.ProtectedAgent)
			value.DeniedIngress, value.DeniedService = max(k.DeniedIngress, prior.DeniedIngress), max(k.DeniedService, prior.DeniedService)
			value.Grants = mergeGrants(prior.Grants, k.Grants)
			k, kernel.Counters = &value, &value
		}
		s.AddressGrants = c.addressGrants(k.Grants)
		denied := k.DeniedAgent
		if !networkview.Add(&denied, uint64(k.ProtectedAgent)) {
			c.kernelPartial = true
		}
		s.Counters.DeniedPackets, s.Counters.ProtectedPackets, s.Counters.IngressDenials = &denied, networkview.Value(uint64(k.ProtectedAgent)), networkview.Value(uint64(k.DeniedIngress))
		s.Coverage.KernelPackets = exactCoverage()
		if c.kernelPartial {
			s.Coverage.KernelPackets = partialCoverage("kernel_counter_discontinuity")
		}
		c.lastKernel = kernel
	} else {
		s.Availability = "degraded"
	}
	s.Connections = nil
	if inventoryErr != nil {
		c.inventoryGap = true
		c.markBoundaryGap("socket_inventory_unavailable")
	} else {
		exact := networkview.Add(&c.inventorySequence, 1)
		c.inventoryAt = &s.AsOf
		current := make(map[socketAttemptKey]struct{})
		rows = slices.DeleteFunc(slices.Clone(rows), func(row SocketRow) bool {
			if row.UID != 1000 || row.State != "connecting" || c.boundary.captured(row.UID, row.Tuple.Local, row.Tuple.Peer) {
				return false
			}
			key := socketAttemptKey{Tuple: row.Tuple, UID: row.UID, Inode: row.Inode}
			if !kernelFresh || !kernel.EnforcerReady {
				if _, seen := c.previousAttempts[key]; seen {
					current[key] = struct{}{}
				}
				return false
			}
			current[key] = struct{}{}
			if _, seen := c.previousAttempts[key]; !seen {
				if !exact {
					c.loseDetail()
					c.detailUnknown = true
					return true
				}
				port := int(row.Tuple.Peer.Port())
				reason := "fixed_egress_policy"
				if protectedSocketPeer(row.Tuple.Peer.Addr(), c.resolver.protected) {
					reason = "protected_destination"
				}
				c.denials = append(c.denials, networkview.Denial{ID: c.opaque("socket-attempt", fmt.Sprintf("%s/%s/%d/%d/%d", row.Tuple.Local, row.Tuple.Peer, row.UID, row.Inode, c.inventorySequence)),
					Source: "socket-inventory", Sequence: c.inventorySequence, Basis: "observed-socket-and-captured-policy", Kind: "direct_tcp_attempt", Reason: reason, Peer: row.Tuple.Peer.String(), Port: &port,
					DestinationID: c.opaque("peer", row.Tuple.Peer.String()), At: s.AsOf})
				if len(c.denials) > MaxDenialDetails {
					c.denials = c.denials[1:]
					c.loseDetail()
				}
			}
			return true
		})
		c.previousAttempts = current
	}
	// An orphaned closing TCP control block may outlive every application fd.
	// Keep it visible, without inventing stream ownership or a zero-byte meter.
	remnants := 0
	rows = slices.DeleteFunc(slices.Clone(rows), func(row SocketRow) bool {
		if row.Inode != 0 || row.State != "closing" {
			return false
		}
		remnants++
		if remnants <= MaxUnknownDetails {
			s.Connections = append(s.Connections, networkview.Connection{ID: c.opaque("kernel-closing", fmt.Sprintf("%s/%s/%d", row.Tuple.Local, row.Tuple.Peer, row.UID)),
				DestinationID: c.opaque("peer", row.Tuple.Peer.String()), State: "kernel-closing", Reason: "kernel_closing_remnant", Transport: "tcp",
				NameSource: "socket-inventory", Peer: row.Tuple.Peer.String(), ObservedAt: s.AsOf, Partial: true})
		}
		return true
	})
	matched := make(map[SocketTuple]int, len(c.flows)+len(owned))
	for _, f := range c.flows {
		if f.tuple.Peer.IsValid() {
			matched[f.tuple]++
		}
	}
	for _, m := range owned {
		matched[m.Tuple]++
	}
	c.reconcileMaintenance(owned, rows, matched, now)
	// A proxied flow's upstream socket can outlive its stream: the inventory can
	// precede an authoritative close consumed in this same sample, and the kernel
	// keeps the socket into later ones. The gateway's own resolver leaves the same
	// remnant behind when it releases a maintenance connection. Fold a retained
	// close or a retired maintenance identity over its exact tuple/inode; never
	// classify an accounted leg as a new unknown external socket, and never fold
	// away a socket a live claim is still joining.
	rows = slices.DeleteFunc(slices.Clone(rows), func(row SocketRow) bool {
		key := socketAttemptKey{Tuple: row.Tuple, UID: row.UID, Inode: row.Inode}
		if pending, joined := c.pending[key]; row.Inode == 0 || matched[row.Tuple] != 0 || joined && pending.expired {
			return false // an expired join stays visible; a later close cannot retract it
		}
		if !c.closeAccountsFor(key, now) && !c.maintenanceAccountsFor(key, now) {
			return false
		}
		delete(c.pending, key)
		return true
	})
	present := make(map[SocketTuple]SocketRow, len(rows))
	for _, row := range rows {
		if row.UID == 65532 {
			present[row.Tuple] = row
		}
	}
	s.StaleConnections = 0
	if c.terminal && !c.envoyTotals.Stopped {
		c.markProxyPartial("proxy_shutdown_unconfirmed")
	}
	for id, f := range c.flows {
		row := f.row
		if c.terminal || c.envoyTotals.Stopped {
			row.Partial, row.Rate, row.Reason = true, nil, "proxy_end_missing"
			row.State = "unknown"
			if c.envoyTotals.Stopped {
				row.State = "closed"
			}
			c.markProxyPartial("proxy_end_missing")
			c.closed = append(c.closed, row)
			if len(c.closed) > MaxClosedDetails {
				c.closed = c.closed[1:]
				c.loseDetail()
			}
			delete(c.flows, id)
			continue
		}
		if f.tuple.Peer.IsValid() && matched[f.tuple] != 1 {
			row.Partial = true
			row.State = "unknown"
		}
		socket, exists := present[f.tuple]
		if f.connected && !exists {
			row.Partial, row.Rate = true, nil
			row.Reason = "socket_observation_unavailable"
			if inventoryErr == nil && truncation == nil {
				row.Reason = "socket_absent"
			}
		}
		if exists && matched[f.tuple] == 1 && f.inode == 0 && !f.privateClosed.Valid() {
			f.inode = socket.Inode
		}
		if exists && f.inode != 0 && f.inode != socket.Inode {
			row.State = "unknown"
			row.Partial = true
			matched[f.tuple] = 0
			c.markBoundaryGap("socket_inode_changed")
		} else if exists && f.inode == 0 {
			row.State = "unknown"
			row.Reason = "attribution_pending"
			row.Partial = true
			matched[f.tuple] = 0
		}
		if !now.Valid() || !f.lastBoot.Valid() || now.Sub(f.lastBoot) > ObservationStaleAfter {
			row.State = "stale"
			row.Partial = true
			row.Rate = nil
			if f.connected {
				c.markProxyPartial("proxy_flow_stale")
			}
		}
		if !now.Valid() || !f.meterBoot.Valid() || now.Sub(f.meterBoot) > ObservationStaleAfter {
			row.Rate = nil
		}
		if inventoryErr != nil {
			row.Partial = true
		}
		if row.State == "stale" {
			s.StaleConnections++
		}
		if f.privateClosed.Valid() && now.Sub(f.privateClosed) > ObservationStaleAfter && (!exists || f.inode != socket.Inode) {
			row.Partial = true
			row.Rate = nil
			row.State = "stale"
			if inventoryErr == nil && truncation == nil && f.tuple.Peer.IsValid() && !exists {
				row.State = "closed"
			}
			c.markProxyPartial("proxy_close_event_missing")
			delete(c.flows, id)
			c.closed = append(c.closed, row)
			if len(c.closed) > MaxClosedDetails {
				c.closed = c.closed[1:]
				c.loseDetail()
			}
			continue
		}
		s.Connections = append(s.Connections, row)
	}
	for _, m := range owned {
		row := networkview.Connection{ID: c.opaque("maintenance", fmt.Sprint(m.ID)), DestinationID: c.opaque("maintenance-destination", m.Tuple.Peer.String()), State: "open",
			Transport: "tcp", NameSource: "trusted-maintenance", Peer: m.Tuple.Peer.String(), StartedAt: &m.StartedAt, ObservedAt: s.AsOf,
			SentBytes: networkview.Value(m.Sent), ReceivedBytes: networkview.Value(m.Received)}
		if _, exists := present[m.Tuple]; !exists || matched[m.Tuple] != 1 || inventoryErr != nil {
			row.Partial = true
			row.State = "unknown"
		}
		s.Connections = append(s.Connections, row)
	}
	unknown := 0
	if c.pending == nil {
		c.pending = make(map[socketAttemptKey]pendingSocket)
	}
	currentUnknown := make(map[socketAttemptKey]struct{})
	currentIDs := make(map[string]struct{})
	for _, row := range rows {
		key := socketAttemptKey{Tuple: row.Tuple, UID: row.UID, Inode: row.Inode}
		if row.UID == 65532 && matched[row.Tuple] == 1 {
			delete(c.pending, key)
			continue
		}
		unknown++
		currentUnknown[key] = struct{}{}
		reason := "attribution_pending"
		if row.UID != 65532 || row.Inode == 0 {
			c.markBoundaryGap("unattributed_socket")
			switch {
			case row.UID == 1000 && row.State == "connecting":
				c.unverifiedAttempt = true
				reason = "agent_attempt_unverified"
			case row.UID == 1000:
				c.unexpectedAgent = true
				reason = "unexpected_agent_connection"
				s.Health.Enforcer = networkview.Health{Status: "unknown", Reason: reason}
			case row.UID != 65532:
				c.unexpectedOwner = true
				reason = "unexpected_socket_owner"
			default:
				c.inodeUnavailable = true
				reason = "socket_inode_unavailable"
			}
			if c.firstUnknown(key, completeInventory) {
				c.retainUnknown(key, reason, s.AsOf)
			}
		} else if _, exists := c.pending[key]; !exists {
			if len(c.pending) < MaxPendingJoins {
				c.pending[key] = pendingSocket{firstSeen: now}
			} else {
				c.markBoundaryGap("socket_join_capacity")
				reason = "socket_join_capacity"
			}
		} else if c.pending[key].expired {
			reason = "unattributed_socket"
		}
		id := c.socketID(key)
		currentIDs[id] = struct{}{}
		if unknown > MaxUnknownDetails {
			continue
		}
		s.Connections = append(s.Connections, networkview.Connection{ID: id,
			DestinationID: c.opaque("peer", row.Tuple.Peer.String()), State: "unknown", Reason: reason, Transport: "tcp", NameSource: "unattributed", Peer: row.Tuple.Peer.String(), ObservedAt: s.AsOf, Partial: true})
	}
	if completeInventory {
		c.previousUnknown = currentUnknown
	}
	// A trustworthy join in this sample beats its expiry check. Once an earlier
	// sample expires a key, a later coincidence cannot erase that history gap.
	s.PendingConnections = 0
	for key, pending := range c.pending {
		_, present := currentUnknown[key]
		// The proxy ending a flow — or the resolver releasing a maintenance
		// connection — retires that upstream socket, so it can no longer join a
		// live one. The release IS the accounting: expiring it as unattributed
		// would report a measured stream, or the gateway's own metered DNS leg, as
		// evidence loss.
		if !pending.expired && (c.closeAccountsFor(key, now) || c.maintenanceAccountsFor(key, now)) {
			delete(c.pending, key)
			continue
		}
		if !pending.expired && (c.terminal || !now.Valid() || !pending.firstSeen.Valid() || now.Before(pending.firstSeen) || now.Sub(pending.firstSeen) >= ObservationStaleAfter) {
			c.markBoundaryGap("unattributed_socket")
			reason := "socket_join_expired"
			if c.terminal {
				reason = "socket_join_terminal"
			} else if !now.Valid() || !pending.firstSeen.Valid() || now.Before(pending.firstSeen) {
				reason = "socket_join_clock_unavailable"
			}
			c.retainUnknown(key, reason, s.AsOf)
			pending.expired = true
			c.pending[key] = pending
		}
		if pending.expired && (c.terminal || completeInventory && !present) {
			delete(c.pending, key)
		} else if !pending.expired {
			s.PendingConnections++
		}
	}
	s.LiveConnections = nil
	s.UnknownConnections = nil
	s.KernelClosingSockets = nil
	if inventoryErr == nil {
		s.LiveConnections = networkview.Value(uint64(len(rows)))
		s.UnknownConnections = networkview.Value(uint64(unknown))
		s.KernelClosingSockets = networkview.Value(uint64(remnants))
		if truncation != nil {
			s.Coverage.SocketInventory = partialCoverage("socket_inventory_truncated")
		}
	} else {
		s.Coverage.SocketInventory = missingCoverage("socket_inventory_unavailable")
		s.Availability = "degraded"
	}
	if c.proxyPartial {
		s.Coverage.ProxyBytes, s.Coverage.Connections = partialCoverage(c.proxyPartialReason), partialCoverage(c.proxyPartialReason)
		s.Coverage.UpstreamFailures = partialCoverage(c.proxyPartialReason)
	}
	if c.boundaryGap != "" {
		s.Coverage.BoundaryAttribution = partialCoverage(c.boundaryGap)
	} else if s.PendingConnections > 0 {
		s.Coverage.BoundaryAttribution = partialCoverage("socket_join_pending")
	}
	for _, row := range c.closed {
		if _, current := currentIDs[row.ID]; !current {
			s.Connections = append(s.Connections, row)
		}
	}
	slices.SortFunc(s.Connections, func(a, b networkview.Connection) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	s.Denials = slices.Clone(c.denials)
	omitted := c.detailLost
	omittedExact := networkview.Add(&omitted, uint64(max(0, unknown-MaxUnknownDetails)+max(0, remnants-MaxUnknownDetails)))
	if truncation != nil && !networkview.Add(&omitted, truncation.omitted) {
		omittedExact = false
	}
	s.Loss = networkview.Loss{Records: networkview.Count(c.guardTotals.Lost), Unknown: c.envoyTotals.UnknownLoss || c.proxyPartial || c.detailUnknown || c.boundaryGap != "",
		DetailTruncated: c.detailLost > 0 || unknown > MaxUnknownDetails || remnants > MaxUnknownDetails || truncation != nil || c.historyIdentityLost}
	if omittedExact && !c.detailUnknown && !c.historyIdentityLost {
		s.Loss.OmittedDetails = &omitted
	}
	if !networkview.Add(&s.Loss.Records, c.envoyTotals.Lost) {
		s.Loss.Unknown = true
	}
	if c.proxyPartial || c.envoyTotals.UnknownLoss || c.detailUnknown {
		s.Loss.Reasons = append(s.Loss.Reasons, c.proxyReasons...)
		if len(c.proxyReasons) == 0 {
			s.Loss.Reasons = append(s.Loss.Reasons, "proxy_observation_gap")
		}
	}
	if c.boundaryGap != "" {
		s.Loss.Reasons = append(s.Loss.Reasons, c.boundaryGap)
	}
	// Current health can recover; these historical observations cannot. A
	// prior sampling gap must never mask a later security-relevant socket.
	if c.unexpectedAgent {
		s.Loss.Reasons = append(s.Loss.Reasons, "unexpected_agent_connection")
	}
	if c.unverifiedAttempt {
		s.Loss.Reasons = append(s.Loss.Reasons, "agent_attempt_unverified")
	}
	if c.unexpectedOwner {
		s.Loss.Reasons = append(s.Loss.Reasons, "unexpected_socket_owner")
	}
	if c.inodeUnavailable {
		s.Loss.Reasons = append(s.Loss.Reasons, "socket_inode_unavailable")
	}
	if c.historyIdentityLost && c.boundaryGap != "socket_history_identity_capacity" {
		s.Loss.Reasons = append(s.Loss.Reasons, "socket_history_identity_capacity")
	}
	if inventoryErr != nil {
		s.Loss.Reasons = append(s.Loss.Reasons, "socket_inventory_unavailable")
	}
	if kernelErr != nil || !kernelFresh {
		s.Loss.Reasons = append(s.Loss.Reasons, "kernel_counters_unavailable")
	}
	if c.proxyPartial || c.boundaryGap != "" || inventoryErr != nil || !kernelFresh {
		s.Health.Collector = networkview.Health{Status: "degraded", Reason: "observation_gap"}
		s.Availability = "degraded"
	}
	s.Rate = nil
	if !c.proxyPartial {
		var aggregate networkview.Rate
		complete, measured := true, false
		for _, row := range s.Connections {
			if row.Transport != "tls" || row.State == "closed" {
				continue
			}
			if row.Rate == nil {
				complete = false
				break
			}
			aggregate.SentPerSecond += row.Rate.SentPerSecond
			aggregate.ReceivedPerSecond += row.Rate.ReceivedPerSecond
			if !measured || row.Rate.WindowMillis < aggregate.WindowMillis {
				aggregate.WindowMillis = row.Rate.WindowMillis
			}
			aggregate.MaxWindowMillis = max(aggregate.MaxWindowMillis, row.Rate.WindowMillis)
			measured = true
		}
		if complete && measured {
			aggregate.Measurement = "sum-latest-flow-windows"
			s.Rate = &aggregate
		}
	}
	s.Sources = []networkview.Source{
		{ID: "guard", Sequence: networkview.Count(c.guardCursor), ObservedAt: &s.AsOf, LastEventAt: c.lastGuardAt, Status: "live", Lost: networkview.Count(c.guardTotals.Lost), Unknown: c.guardTotals.Saturated},
		{ID: "proxy", Sequence: networkview.Count(c.envoyCursor), ObservedAt: &s.AsOf, LastEventAt: c.lastEnvoyAt, Status: "live", Lost: networkview.Count(c.envoyTotals.Lost), Unknown: c.envoyTotals.UnknownLoss},
		{ID: "kernel", Sequence: kernel.Sequence, ObservedAt: &kernel.At, Status: "live"},
		{ID: "socket-inventory", Sequence: c.inventorySequence, ObservedAt: c.inventoryAt, Status: "sampled", Unknown: c.inventoryGap, Reason: "sampled-not-packet-correlated"},
	}
	if c.envoyTotals.Stopped {
		s.Sources[1].Status, s.Sources[1].Reason = "stopped", c.envoyTotals.StopReason
	}
	if !kernelFresh {
		s.Sources[2].Status, s.Sources[2].ObservedAt, s.Sources[2].Reason = "unavailable", nil, "kernel_counters_unavailable"
	}
	if inventoryErr != nil {
		s.Sources[3].Status, s.Sources[3].Reason = "unavailable", "socket_inventory_unavailable"
	} else if truncation != nil {
		s.Sources[3].Status, s.Sources[3].Reason = "partial", "socket_inventory_truncated"
	}
	if c.alerts == nil {
		c.alerts = &alertDetector{identity: c.identity, started: c.started}
	}
	c.alerts.observe(now, &s)
	c.snapshot = s
}
