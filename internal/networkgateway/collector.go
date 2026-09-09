package networkgateway

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"slices"
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
	peer          netip.Addr
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
	resolver                    *Resolver
	doh                         *DoH
	controller                  ControllerClient
	inventory                   func() ([]SocketRow, error)
	flows                       map[string]*collectedFlow
	closed                      []networkview.Connection
	closedJoins                 map[SocketTuple]uint64
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
}

func NewCollector(g *Guard, e *EnvoyEvents, doh *DoH) (*Collector, error) {
	if g == nil || e == nil || doh == nil || e.clock.Domain() != g.clock.Domain() {
		return nil, Failure("collector_configuration_invalid")
	}
	c := &Collector{identity: g.controller.Identity, clock: g.clock, started: g.clock.instant(), guard: g.events,
		envoy: e, resolver: g.resolver, doh: doh, controller: g.controller,
		inventory: func() ([]SocketRow, error) { return readSocketInventory(g.resolver.protected) }, flows: make(map[string]*collectedFlow)}
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
		}
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
	c.closedJoins = make(map[SocketTuple]uint64)
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
			c.flows[event.FlowID] = &collectedFlow{peer: event.Peer, lastBoot: event.BootAt,
				row: networkview.Connection{ID: c.opaque("flow", event.FlowID), DestinationID: c.opaque("destination", event.Name),
					State: "connecting", Transport: "tls", Name: event.Name, NameSource: "sni", Peer: netip.AddrPortFrom(event.Peer, 443).String(),
					RuleID: event.RuleID, StartedAt: &at, ObservedAt: event.At}}
		case "private_flow_closed":
			if f := c.flows[event.FlowID]; f != nil {
				f.privateClosed = event.BootAt
			}
		case "tls_denied", "dns_denied", "admission_failed":
			row := networkview.Denial{ID: c.opaque("guard", fmt.Sprint(event.Sequence)), Source: "guard", Sequence: networkview.Count(event.Sequence),
				At: event.At, Kind: event.Kind, Reason: event.Reason, Name: event.Name, Basis: "observed"}
			if event.Name != "" {
				row.DestinationID = c.opaque("destination", event.Name)
			}
			if event.Kind == "tls_denied" {
				port := 443
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
		if event.Peer.IsValid() && (event.Peer.Addr() != f.peer || event.Peer.Port() != 443) {
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
			if f.inode != 0 && f.tuple.Peer.IsValid() {
				c.closedJoins[f.tuple] = f.inode
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
	queries, failures := c.resolver.MaintenanceCounts()
	s.Health.Resolver.Status, s.Health.Resolver.Reason = c.resolver.health()
	if s.Health.Resolver.Status != "ready" {
		s.Availability = "degraded"
	}
	s.Counters.MaintenanceQueries, s.Counters.MaintenanceFailures = networkview.Value(queries), networkview.Value(failures)
	if c.resolver.counterSaturated.Load() {
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
		if prior != nil && (k.DeniedAgent < prior.DeniedAgent || k.ProtectedAgent < prior.ProtectedAgent || k.DeniedIngress < prior.DeniedIngress || k.DeniedService < prior.DeniedService || kernel.Sequence < c.lastKernel.Sequence) {
			c.kernelPartial = true
		}
		if prior != nil && c.kernelPartial {
			value := *k
			value.DeniedAgent, value.ProtectedAgent = max(k.DeniedAgent, prior.DeniedAgent), max(k.ProtectedAgent, prior.ProtectedAgent)
			value.DeniedIngress, value.DeniedService = max(k.DeniedIngress, prior.DeniedIngress), max(k.DeniedService, prior.DeniedService)
			k, kernel.Counters = &value, &value
		}
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
			if row.UID != 1000 || row.State != "connecting" || capturedSocket(row.UID, row.Tuple.Local, row.Tuple.Peer, c.resolver.protected) {
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
	// The inventory can precede an authoritative close consumed in this same
	// sample. Fold that newer close over only its exact retained inode/tuple;
	// never classify the old sampled leg as a new unknown external socket.
	rows = slices.DeleteFunc(slices.Clone(rows), func(row SocketRow) bool {
		if row.UID == 65532 && row.Inode != 0 && c.closedJoins[row.Tuple] == row.Inode {
			delete(c.pending, socketAttemptKey{Tuple: row.Tuple, UID: row.UID, Inode: row.Inode})
			return true
		}
		return false
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
