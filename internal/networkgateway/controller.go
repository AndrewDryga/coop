package networkgateway

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
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

// Lease is one admitted name/address/port triple. The port is the destination
// the KERNEL recorded for the redirected connection, not one a client asked
// for: the guard reads it back with SO_ORIGINAL_DST and the controller checks
// it against the frozen policy again before any element is installed.
type Lease struct {
	Name    string      `json:"name"`
	Peer    netip.Addr  `json:"peer"`
	Port    int         `json:"port"`
	Expires BootInstant `json:"expires_boot_ns"`
}

func (l Lease) destination() netip.AddrPort {
	return netip.AddrPortFrom(l.Peer, uint16(l.Port))
}

type Controller struct {
	identity            Identity
	policy              egress.Snapshot
	broker              *CredentialBrokerRoute
	protected           []netip.Prefix
	apply               ApplyRules
	now                 func() BootInstant
	clock               *BootClock
	mu                  sync.Mutex
	grants              []addressGrant
	serviceProxyClients []ServiceProxyClient
	serve               []int
	ingress             netip.Addr
	leases              map[string]Lease
	installed           map[netip.AddrPort]BootInstant
	ready               atomic.Bool
	closed              atomic.Bool
	initialized         bool
	kernel              kernelEvents
}

func NewController(identity Identity, policy egress.Snapshot, protected []netip.Prefix, services []ServiceBinding, serviceProxyClients []ServiceProxyClient,
	serve []int, ingress netip.Addr, broker *CredentialBrokerRoute, clock *BootClock, apply ApplyRules) (*Controller, error) {
	if err := policy.RequireSupported(); err != nil {
		return nil, err
	}
	grants, err := addressGrants(policy, services)
	if err != nil {
		return nil, err
	}
	if err := validServePorts(serve, policy.TLSPorts()); err != nil {
		return nil, err
	}
	if !validServiceProxyClients(protected, serviceProxyClients) || len(serviceProxyClients) != 0 && slices.Contains(serve, ServiceProxyPort) {
		return nil, errors.New("invalid service proxy client configuration")
	}
	if len(serve) != 0 && !ingress.Is4() {
		return nil, errors.New("published serve ports require the bridge gateway address host traffic arrives from")
	}
	if broker != nil && (slices.Contains(serve, CredentialBrokerPort) || slices.Contains(policy.TLSPorts(), CredentialBrokerPort)) {
		return nil, errors.New("credential broker port collides with the agent network contract")
	}
	if apply == nil || len(protected) > MaxProtectedRanges || !identity.Valid() || identity.PolicyFingerprint != policy.Fingerprint || clock.Domain() != identity.Clock || !clock.instant().Valid() || broker != nil && !broker.valid() {
		return nil, errors.New("invalid gateway controller configuration")
	}
	for _, prefix := range protected {
		if !prefix.IsValid() || prefix != prefix.Masked() || prefix.Addr().Is4In6() {
			return nil, errors.New("invalid protected namespace prefix")
		}
	}
	var brokerCopy *CredentialBrokerRoute
	if broker != nil {
		value := *broker
		brokerCopy = &value
	}
	return &Controller{identity: identity, policy: policy.Clone(), broker: brokerCopy, protected: slices.Clone(protected), grants: grants,
		serviceProxyClients: slices.Clone(serviceProxyClients), serve: slices.Clone(serve),
		ingress: ingress, apply: apply, now: clock.instant, clock: clock, leases: map[string]Lease{}}, nil
}

func validServiceProxyClients(protected []netip.Prefix, clients []ServiceProxyClient) bool {
	if len(clients) > 512 {
		return false
	}
	for i, client := range clients {
		if client.Name == "" || !client.Address.Is4() || slices.ContainsFunc(clients[:i], func(other ServiceProxyClient) bool {
			return other.Name == client.Name || other.Address == client.Address
		}) || !slices.ContainsFunc(protected, func(prefix netip.Prefix) bool { return prefix.Contains(client.Address) }) {
			return false
		}
	}
	return true
}

// addressGrant is one packet-filter grant as the kernel sees it: an exact IPv4
// destination, the transport it permits and the counter its packets land on.
// A `service:` grant is the same thing with the address the host resolved for
// that container at launch — never a name the box could re-point.
type addressGrant struct {
	counter, target, protocol, display string
	ports                              []int
	// service marks a grant whose destination is ONE approved sidecar. Those
	// render before the protected drop, because the runtime's own network
	// subnets are protected and the approved container lives inside one.
	service bool
}

func addressGrants(policy egress.Snapshot, services []ServiceBinding) ([]addressGrant, error) {
	bound := map[string]ServiceBinding{}
	for _, binding := range services {
		if !binding.Address.Is4() || egress.Protected(binding.Address, nil) || bound[binding.RuleID].RuleID != "" {
			return nil, errors.New("invalid or duplicate service address binding")
		}
		bound[binding.RuleID] = binding
	}
	var out []addressGrant
	for _, grant := range policy.Grants {
		counter := grant.CounterName()
		if counter == "" {
			continue
		}
		rule := grant.Rule
		item := addressGrant{counter: counter, protocol: rule.Protocol, ports: rule.Ports}
		if rule.To.Service != "" {
			binding, ok := bound[grant.ID]
			if !ok || binding.Name != rule.To.Service {
				return nil, errors.New("an approved service grant has no launch address binding")
			}
			delete(bound, grant.ID)
			item.target, item.display, item.service = binding.Address.String(), rule.To.Service, true
		} else {
			prefix, err := netip.ParsePrefix(rule.To.CIDR)
			if err != nil || prefix != prefix.Masked() || !prefix.Addr().Is4() {
				return nil, errors.New("address grant destination is not a canonical IPv4 prefix")
			}
			item.target, item.display = prefix.String(), prefix.String()
		}
		out = append(out, item)
	}
	if len(bound) != 0 {
		return nil, errors.New("service address binding does not match any approved grant")
	}
	slices.SortFunc(out, func(a, b addressGrant) int { return strings.Compare(a.counter, b.counter) })
	return out, nil
}

// A published serve port is host ingress to the box, not egress: it opens the
// exact container ports the project asked to serve and nothing else. It may not
// be a port this run captures — 53 and 443 always, plus every port a tls grant
// names — because the agent's own connection to it would be redirected instead.
func validServePorts(ports, captured []int) error {
	if len(ports) > egress.MaxConstraints {
		return errors.New("too many published serve ports")
	}
	for i, port := range ports {
		if port < 1 || port > 65535 || port == 443 || port == 53 || slices.Contains(captured, port) || slices.Index(ports, port) != i {
			return errors.New("published serve ports must be unique, in 1..65535 and outside the gateway's captured TLS/DNS ports")
		}
	}
	return nil
}

func portSet(ports []int) string {
	out := make([]string, 0, len(ports))
	for _, port := range ports {
		out = append(out, strconv.Itoa(port))
	}
	return "{ " + strings.Join(out, ", ") + " }"
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
	if err != nil || name != lease.Name || !c.policy.Domain(name, lease.Port).Allowed || !lease.Peer.Is4() || !egress.PublicAnswer(lease.Peer, c.protected) {
		return 0, Failure("gateway_lease_refused")
	}
	return c.installLease(ctx, lease)
}

// AdmitBroker is a separate helper-only authority path. The agent cannot reach the private
// controller socket, and the controller checks the immutable launch route rather than trusting a
// caller-supplied label or unioning the provider into the agent policy.
func (c *Controller) AdmitBroker(ctx context.Context, lease Lease) (BootInstant, error) {
	name, err := egress.NormalizeDomain(lease.Name, false)
	if err != nil || name != lease.Name || c.broker == nil || name != c.broker.Upstream || lease.Port != c.broker.Port ||
		!lease.Peer.Is4() || !egress.PublicAnswer(lease.Peer, c.protected) {
		return 0, Failure("gateway_lease_refused")
	}
	return c.installLease(ctx, lease)
}

func (c *Controller) installLease(ctx context.Context, lease Lease) (BootInstant, error) {
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
	key := lease.Name + "\x00" + lease.destination().String()
	if prior, ok := c.leases[key]; ok && !prior.Expires.Before(lease.Expires) && c.installed[lease.destination()].After(now) {
		return minTime(lease.Expires, c.installed[lease.destination()]), nil
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
	err := c.apply(updateCtx, rules)
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
	validUntil := minTime(lease.Expires, installed[lease.destination()])
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

func leaseRules(leases map[string]Lease, now BootInstant) (string, map[netip.AddrPort]BootInstant) {
	peers := map[netip.AddrPort]BootInstant{}
	for _, lease := range leases {
		if lease.Expires.After(peers[lease.destination()]) {
			peers[lease.destination()] = lease.Expires
		}
	}
	var elements []string
	installed := map[netip.AddrPort]BootInstant{}
	for peer, expires := range peers {
		// The relative kernel timeout starts at commit, not at rendering.
		// Reserve the entire bounded commit budget, including for old peers.
		// For acknowledged transactions commit <= start+budget, so the kernel
		// cannot extend DNS authority. The lower bound is returned to the guard
		// for a final freshness check after the IPC roundtrip.
		millis := (expires.Sub(now) - ControllerUpdateTimeout).Milliseconds()
		if millis > KernelTickAllowance.Milliseconds() {
			elements = append(elements, fmt.Sprintf("%s . %d timeout %dms", peer.Addr(), peer.Port(), millis))
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
	// The set of ports the policy grants TLS on IS the capture chain: the agent's
	// TCP to one of them is redirected to the guard, which reads the port back
	// from the kernel. A policy with no tls grant captures no TLS port at all.
	// Upstream replies arrive from those same ports, plus 443 for the guard's
	// own pinned DoH resolver.
	captured, capture := c.policy.TLSPorts(), ""
	if len(captured) != 0 {
		capture = fmt.Sprintf("  meta nfproto ipv4 meta skuid 1000 tcp dport %s ip daddr != @protected4 redirect to :15443\n", portSet(captured))
	}
	replies := append(slices.Clone(captured), 443)
	slices.Sort(replies)
	replies = slices.Compact(replies)
	var counters, egressRules, ingressRules, serviceEgress, serviceIngress strings.Builder
	for _, grant := range c.grants {
		fmt.Fprintf(&counters, " counter %s { }\n", grant.counter)
		// An address grant renders AFTER the protected drop, so a granted CIDR
		// never reaches a host, metadata or runtime address inside it. An
		// approved sidecar renders BEFORE it: its container address lives in one
		// of the runtime's own protected subnets, and that ONE address is
		// exactly what a human approved. Return traffic is scoped to the same
		// destination and to conntrack, never a bare port.
		out, back := &egressRules, &ingressRules
		if grant.service {
			out, back = &serviceEgress, &serviceIngress
		}
		switch grant.protocol {
		case "tcp", "udp":
			fmt.Fprintf(out, "  meta skuid 1000 ip daddr %s %s dport %s counter name %s accept\n", grant.target, grant.protocol, portSet(grant.ports), grant.counter)
			fmt.Fprintf(back, "  ip saddr %s %s sport %s ct state established accept\n", grant.target, grant.protocol, portSet(grant.ports))
		case "icmp":
			fmt.Fprintf(out, "  meta skuid 1000 ip daddr %s icmp type echo-request counter name %s accept\n", grant.target, grant.counter)
			fmt.Fprintf(back, "  ip saddr %s icmp type echo-reply ct state established,related accept\n", grant.target)
		}
	}
	if len(c.serve) != 0 {
		// Published serve ports are ingress the operator asked for: the HOST
		// reaches this exact container port through the bridge gateway it is
		// NAT'd from, and the server's replies leave on that established flow
		// only. A sibling container on the same bridge is not that source.
		//
		// Both rules render BEFORE the protected drop, because the peer IS the
		// protected gateway. The reply carries no skuid — a listening socket's
		// SYN-ACK is the kernel's, not the agent's — so conntrack is what scopes
		// it: only a flow the ingress rule above admitted can be established.
		fmt.Fprintf(&serviceIngress, "  ip saddr %s tcp dport %s ct state new,established accept\n", c.ingress, portSet(c.serve))
		fmt.Fprintf(&serviceEgress, "  ip daddr %s tcp sport %s ct state established accept\n", c.ingress, portSet(c.serve))
	}
	for _, client := range c.serviceProxyClients {
		fmt.Fprintf(&serviceIngress, "  ip saddr %s tcp dport %d ct state new,established accept\n", client.Address, ServiceProxyPort)
		fmt.Fprintf(&serviceEgress, "  ip daddr %s tcp sport %d ct state established accept\n", client.Address, ServiceProxyPort)
	}
	return fmt.Sprintf(`table inet coop_net {
 counter denied_agent { }
 counter protected_agent { }
 counter denied_ingress { }
 counter denied_service { }
%s set protected4 {
  type ipv4_addr; flags interval; auto-merge;
  elements = { %s }
 }
 set leases4 { type ipv4_addr . inet_service; flags timeout; size 4096; gc-interval 1s; }
 chain shutdown {
  type filter hook output priority -5; policy accept;
 }
 chain capture {
  type nat hook output priority -110; policy accept;
%s  meta nfproto ipv4 meta skuid 1000 udp dport 53 redirect to :15353
  meta nfproto ipv4 meta skuid 1000 tcp dport 53 redirect to :15353
 }
 chain output {
  type filter hook output priority 0; policy drop;
  meta skuid 1000 meta nfproto ipv6 counter name denied_agent reject with icmpx type admin-prohibited
  meta skuid 1000 ct state invalid counter name denied_agent drop
  meta skuid 1000 oifname "lo" accept
  meta skuid 1000 ip daddr 127.0.0.0/8 accept
  meta skuid 65532 oifname "lo" ct state established accept
%s  meta skuid 1000 ip daddr @protected4 counter name protected_agent reject with icmpx type admin-prohibited
%s  meta skuid 1000 counter name denied_agent reject with icmpx type admin-prohibited
  meta nfproto ipv6 counter name denied_service drop
  ct state invalid counter name denied_service drop
  ip daddr @protected4 counter name denied_service drop
  meta skuid 65532 meta l4proto tcp ct state established accept
  meta skuid 65532 ip daddr %s tcp dport 443 accept
  meta skuid 65532 ip daddr . tcp dport @leases4 accept
  counter name denied_service drop
 }
 chain input {
  type filter hook input priority 0; policy drop;
  meta nfproto ipv6 counter name denied_ingress drop
  ct state invalid counter name denied_ingress drop
  iifname "lo" accept
  ip protocol icmp icmp type destination-unreachable icmp code 4 ct state related accept
%s  ip saddr @protected4 counter name denied_ingress drop
  tcp sport %s ct state established accept
%s  counter name denied_ingress drop
 }
 chain forward {
  type filter hook forward priority 0; policy drop;
  counter name denied_ingress drop
 }
}
`, counters.String(), strings.Join(protected, ", "), capture, serviceEgress.String(), egressRules.String(), maintenance,
		serviceIngress.String(), portSet(replies), ingressRules.String())
}
