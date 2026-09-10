package networkgateway

import (
	"net/netip"
	"sync"
	"time"
)

const MaxGuardEvents = 1024

// GuardEvent contains admission facts, never payload bytes or raw parser errors.
// It is owner-private until the explicit networkview projection is applied.
type GuardEvent struct {
	Sequence uint64
	At       time.Time
	BootAt   BootInstant
	Kind     string
	FlowID   string
	Name     string
	RuleID   string
	Peer     netip.Addr
	// Port is the connection's original destination port as the kernel recorded
	// it: the upstream port of a registered flow, and the port a refused attempt
	// was made on.
	Port   int
	Reason string
}

type GuardTotals struct {
	Sequence          uint64
	Lost              uint64
	DeniedTLS         uint64
	DeniedDNS         uint64
	AdmissionFailures uint64
	Saturated         bool
}

// GuardEvents never blocks a forwarding socket on an observer. Assignment and
// enqueue share one short lock so concurrently admitted flows cannot reorder
// source sequences. Loss remains available even when the example queue is full.
type GuardEvents struct {
	mu     sync.Mutex
	totals GuardTotals
	queue  chan GuardEvent
	clock  *BootClock
}

func NewGuardEvents(clock *BootClock) *GuardEvents {
	return &GuardEvents{queue: make(chan GuardEvent, MaxGuardEvents), clock: clock}
}

func (g *GuardEvents) emit(event GuardEvent) {
	g.mu.Lock()
	defer g.mu.Unlock()
	increment := func(value *uint64) {
		if *value == ^uint64(0) {
			g.totals.Saturated = true
		} else {
			*value++
		}
	}
	if g.totals.Sequence == ^uint64(0) {
		// Never reuse a source sequence: an observer could mistake a new
		// record for replay. Saturation is terminal detail loss, not a wrap.
		g.totals.Saturated = true
		increment(&g.totals.Lost)
		return
	}
	increment(&g.totals.Sequence)
	event.Sequence, event.At = g.totals.Sequence, time.Now().UTC()
	event.BootAt = g.clock.instant()
	switch event.Kind {
	case "tls_denied":
		increment(&g.totals.DeniedTLS)
	case "dns_denied":
		increment(&g.totals.DeniedDNS)
	case "admission_failed":
		increment(&g.totals.AdmissionFailures)
	}
	select {
	case g.queue <- event:
	default:
		increment(&g.totals.Lost)
	}
}

func (g *GuardEvents) Drain(limit int) ([]GuardEvent, GuardTotals) {
	g.mu.Lock()
	defer g.mu.Unlock()
	limit = max(0, min(limit, MaxGuardEvents))
	events := make([]GuardEvent, 0, min(limit, len(g.queue)))
	for range limit {
		select {
		case event := <-g.queue:
			events = append(events, event)
		default:
			return events, g.totals
		}
	}
	return events, g.totals
}
