package networkgateway

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

const (
	MaxLeases               = 4096
	MaxProtectedRanges      = 256
	ControllerUpdateTimeout = 250 * time.Millisecond
	// Qualified amd64/arm64 kernels use HZ >= 100. Account for both timeout
	// truncation and current-tick phase before claiming a minimum lifetime.
	KernelTickAllowance = 20 * time.Millisecond
)

// ApplyRules submits one bounded atomic nft transaction. Production uses a
// deadline-bound, fixed /usr/sbin/nft -f - subprocess; no shell or agent argv.
type ApplyRules func(context.Context, string) error

type Lease struct {
	Name    string      `json:"name"`
	Peer    netip.Addr  `json:"peer"`
	Expires BootInstant `json:"expires_boot_ns"`
}

type Controller struct {
	identity    Identity
	policy      egress.Snapshot
	protected   []netip.Prefix
	apply       ApplyRules
	now         func() BootInstant
	clock       *BootClock
	mu          sync.Mutex
	leases      map[string]Lease
	installed   map[netip.Addr]BootInstant
	ready       atomic.Bool
	closed      atomic.Bool
	initialized bool
	kernel      kernelEvents
}

func NewController(identity Identity, policy egress.Snapshot, protected []netip.Prefix, clock *BootClock, apply ApplyRules) (*Controller, error) {
	if err := policy.RequireTLS443(true); err != nil {
		return nil, err
	}
	if apply == nil || len(protected) > MaxProtectedRanges || !identity.Valid() || identity.PolicyFingerprint != policy.Fingerprint || clock.Domain() != identity.Clock || !clock.instant().Valid() {
		return nil, errors.New("invalid gateway controller configuration")
	}
	for _, prefix := range protected {
		if !prefix.IsValid() || prefix != prefix.Masked() || prefix.Addr().Is4In6() {
			return nil, errors.New("invalid protected namespace prefix")
		}
	}
	return &Controller{identity: identity, policy: policy.Clone(), protected: slices.Clone(protected), apply: apply, now: clock.instant, clock: clock, leases: map[string]Lease{}}, nil
}

// Initialize runs once before any guard/agent starts. Failure leaves readiness
// false; callers destroy these exact resources rather than repair under a live
// workload. Only lease sets, never chains or counters, change after readiness.
func (c *Controller) Initialize(ctx context.Context, maintenance netip.Addr) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.initialized {
		return errors.New("gateway controller is already initialized")
	}
	c.initialized = true
	if !c.now().Valid() {
		return Failure("clock_unavailable")
	}
	if !maintenance.Is4() || !egress.PublicAnswer(maintenance, c.protected) {
		return errors.New("invalid pinned maintenance endpoint")
	}
	if err := c.apply(ctx, c.initialRules(maintenance)); err != nil {
		return Failure("enforcement_unavailable")
	}
	if c.closed.Load() {
		return Failure("enforcement_unavailable")
	}
	c.ready.Store(true)
	return nil
}

// Admit only grants the concrete peer of an already-admitted fresh name. The
// authenticated capless resolver supplies DNS evidence; the privilege holder
// independently checks scope, protected addresses, lifetime and cardinality.
func (c *Controller) Admit(ctx context.Context, lease Lease) (BootInstant, error) {
	name, err := egress.NormalizeDomain(lease.Name, false)
	if err != nil || name != lease.Name || !c.policy.Domain(name, 443).Allowed || !lease.Peer.Is4() || !egress.PublicAnswer(lease.Peer, c.protected) {
		return 0, Failure("gateway_lease_refused")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready.Load() || c.closed.Load() || ctx.Err() != nil {
		return 0, Failure("enforcement_unavailable")
	}
	now := c.now()
	if !now.Valid() {
		c.ready.Store(false)
		return 0, Failure("clock_unavailable")
	}
	if !lease.Expires.Valid() {
		return 0, Failure("dns_ttl_expired")
	}
	remaining := lease.Expires.Sub(now)
	if remaining <= ControllerUpdateTimeout+KernelTickAllowance+time.Millisecond || remaining > MaxDNSTTL {
		return 0, Failure("dns_ttl_expired")
	}
	key := name + "\x00" + lease.Peer.String()
	if prior, ok := c.leases[key]; ok && !prior.Expires.Before(lease.Expires) && c.installed[lease.Peer].After(now) {
		return minTime(lease.Expires, c.installed[lease.Peer]), nil
	}
	updated := make(map[string]Lease, len(c.leases)+1)
	for key, prior := range c.leases {
		if prior.Expires.Sub(now) > ControllerUpdateTimeout+KernelTickAllowance+time.Millisecond {
			updated[key] = prior
		}
	}
	updated[key] = lease
	if len(updated) > MaxLeases {
		return 0, Failure("gateway_lease_capacity")
	}
	// A client disconnect cannot cancel a kernel transaction after admission.
	// The transaction has its own short bound; uncertainty at that bound is a
	// real local enforcer failure, unlike cancellation of one agent tool.
	updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ControllerUpdateTimeout)
	defer cancel()
	rules, installed := leaseRules(updated, now)
	err = c.apply(updateCtx, rules)
	completed := c.now()
	if err != nil || !completed.Valid() || completed.Before(now) || completed.Sub(now) > ControllerUpdateTimeout {
		// No caller may mistake an uncertain kernel update for an installed
		// lease. The local heartbeat reports fatal readiness loss to the guard.
		c.ready.Store(false)
		return 0, Failure("enforcement_unavailable")
	}
	c.leases, c.installed = updated, installed
	if !c.ready.Load() || c.closed.Load() {
		return 0, Failure("enforcement_unavailable")
	}
	validUntil := minTime(lease.Expires, installed[lease.Peer])
	if !c.now().Before(validUntil) {
		return 0, Failure("dns_ttl_expired")
	}
	return validUntil, nil
}

func minTime(a, b BootInstant) BootInstant {
	if a.Before(b) {
		return a
	}
	return b
}

func (c *Controller) Ready() bool {
	if !c.now().Valid() {
		c.ready.Store(false)
	}
	return c.ready.Load() && !c.closed.Load()
}

// CloseAdmission is a terminal local-liveness transition, not hot policy editing.
// Established forwarding is stopped by the guard and exact agent teardown.
func (c *Controller) CloseAdmission(ctx context.Context) error {
	c.closed.Store(true)
	c.ready.Store(false)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initialized = true
	c.leases = map[string]Lease{}
	c.installed = nil
	if err := c.apply(ctx, "flush chain inet coop_net shutdown\nadd rule inet coop_net shutdown counter drop\nflush set inet coop_net leases4\n"); err != nil {
		return Failure("enforcement_unavailable")
	}
	return nil
}

func leaseRules(leases map[string]Lease, now BootInstant) (string, map[netip.Addr]BootInstant) {
	peers := map[netip.Addr]BootInstant{}
	for _, lease := range leases {
		if lease.Expires.After(peers[lease.Peer]) {
			peers[lease.Peer] = lease.Expires
		}
	}
	var elements []string
	installed := map[netip.Addr]BootInstant{}
	for peer, expires := range peers {
		// The relative kernel timeout starts at commit, not at rendering.
		// Reserve the entire bounded commit budget, including for old peers.
		// For acknowledged transactions commit <= start+budget, so the kernel
		// cannot extend DNS authority. The lower bound is returned to the guard
		// for a final freshness check after the IPC roundtrip.
		millis := (expires.Sub(now) - ControllerUpdateTimeout).Milliseconds()
		if millis > KernelTickAllowance.Milliseconds() {
			elements = append(elements, fmt.Sprintf("%s timeout %dms", peer, millis))
			installed[peer] = now.Add(time.Duration(millis)*time.Millisecond - KernelTickAllowance)
		}
	}
	slices.Sort(elements)
	result := "flush set inet coop_net leases4\n"
	if len(elements) != 0 {
		result += "add element inet coop_net leases4 { " + strings.Join(elements, ", ") + " }\n"
	}
	return result, installed
}

func (c *Controller) initialRules(maintenance netip.Addr) string {
	var protected []string
	for _, prefix := range egress.ProtectedRanges(c.protected) {
		if prefix.Addr().Is4() {
			protected = append(protected, prefix.String())
		}
	}
	slices.Sort(protected)
	protected = slices.Compact(protected)
	return fmt.Sprintf(`table inet coop_net {
 counter denied_agent { }
 counter protected_agent { }
 counter denied_ingress { }
 counter denied_service { }
 set protected4 {
  type ipv4_addr; flags interval; auto-merge;
  elements = { %s }
 }
 set leases4 { type ipv4_addr; flags timeout; size 4096; gc-interval 1s; }
 chain shutdown {
  type filter hook output priority -5; policy accept;
 }
 chain capture {
  type nat hook output priority -110; policy accept;
  meta nfproto ipv4 meta skuid 1000 tcp dport 443 ip daddr != @protected4 redirect to :15443
  meta nfproto ipv4 meta skuid 1000 udp dport 53 redirect to :15353
  meta nfproto ipv4 meta skuid 1000 tcp dport 53 redirect to :15353
 }
 chain output {
  type filter hook output priority 0; policy drop;
  meta skuid 1000 meta nfproto ipv6 counter name denied_agent reject with icmpx type admin-prohibited
  meta skuid 1000 ct state invalid counter name denied_agent drop
  meta skuid 1000 ip daddr 127.0.0.1 tcp dport { 15443, 15353 } accept
  meta skuid 1000 ip daddr 127.0.0.1 udp dport 15353 accept
  meta skuid 65532 oifname "lo" ct state established accept
  meta skuid 1000 ip daddr @protected4 counter name protected_agent reject with icmpx type admin-prohibited
  meta skuid 1000 counter name denied_agent reject with icmpx type admin-prohibited
  meta nfproto ipv6 counter name denied_service drop
  ct state invalid counter name denied_service drop
  ip daddr @protected4 counter name denied_service drop
  meta skuid 65532 tcp dport 443 ct state established accept
  meta skuid 65532 ip daddr %s tcp dport 443 accept
  meta skuid 65532 ip daddr @leases4 tcp dport 443 accept
  counter name denied_service drop
 }
 chain input {
  type filter hook input priority 0; policy drop;
  meta nfproto ipv6 counter name denied_ingress drop
  ct state invalid counter name denied_ingress drop
  iifname "lo" accept
  ip protocol icmp icmp type destination-unreachable icmp code 4 ct state related accept
  ip saddr @protected4 counter name denied_ingress drop
  tcp sport 443 ct state established accept
  counter name denied_ingress drop
 }
 chain forward {
  type filter hook forward priority 0; policy drop;
  counter name denied_ingress drop
 }
}
`, strings.Join(protected, ", "), maintenance)
}
