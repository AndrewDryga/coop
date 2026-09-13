package networkgateway

import (
	"bytes"
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

func transportPolicy(t *testing.T, rules ...egress.Rule) egress.Snapshot {
	t.Helper()
	policy, err := egress.Compile("test", egress.Filtered, []egress.Input{{Rules: rules, Origin: egress.Origin{Kind: "operator"}}},
		nil, false, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

// counterFor finds the kernel counter one destination's grant owns, so the
// expected rule text below is written the way the controller derives it.
func counterFor(t *testing.T, policy egress.Snapshot, protocol, destination string) string {
	t.Helper()
	for _, grant := range policy.Grants {
		to := grant.Rule.To
		if grant.Rule.Protocol == protocol && (to.CIDR == destination || to.Service == destination) {
			return grant.CounterName()
		}
	}
	t.Fatalf("no %s grant for %s", protocol, destination)
	return ""
}

// serveIngress is the bridge gateway a published port's host traffic arrives
// from. Fixtures use one fixed address; a run reads it from the daemon.
var serveIngress = netip.MustParseAddr("172.17.0.1")

func transportController(t *testing.T, policy egress.Snapshot, services []ServiceBinding, serve []int) *Controller {
	t.Helper()
	clock := testBootClock()
	ingress := netip.Addr{}
	if len(serve) != 0 {
		ingress = serveIngress
	}
	c, err := NewController(Identity{Clock: clock.Domain(), RunID: strings.Repeat("a", 32), Epoch: strings.Repeat("b", 32),
		PolicyFingerprint: policy.Fingerprint}, policy, nil, services, nil, serve, ingress, nil, clock, func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestServiceProxyIngressIsLimitedToPreparedServiceAddresses(t *testing.T) {
	policy := transportPolicy(t, egress.Rule{To: egress.Destination{Domain: "api.example.com"}, Protocol: "tls", Ports: []int{443}})
	client := netip.MustParseAddr("172.31.4.8")
	clock := testBootClock()
	c, err := NewController(Identity{Clock: clock.Domain(), RunID: strings.Repeat("a", 32), Epoch: strings.Repeat("b", 32),
		PolicyFingerprint: policy.Fingerprint}, policy, []netip.Prefix{netip.MustParsePrefix("172.31.0.0/16")}, nil,
		[]netip.Addr{client}, nil, netip.Addr{}, nil, clock, func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	rules := c.initialRules(netip.MustParseAddr("1.1.1.1"))
	for _, line := range []string{
		"  ip saddr 172.31.4.8 tcp dport 15444 ct state new,established accept",
		"  ip daddr 172.31.4.8 tcp sport 15444 ct state established accept",
	} {
		if !strings.Contains(rules, line+"\n") {
			t.Errorf("service proxy rule missing:\n%s\n--- got ---\n%s", line, rules)
		}
	}
}

// The rendered ruleset IS the enforcement contract: every accepted grant is an
// exact kernel rule, its return path is scoped to the same destination, and the
// protected drop still precedes all of them.
func TestRenderedRulesEnforceEveryAcceptedTransport(t *testing.T) {
	policy := transportPolicy(t,
		egress.Rule{To: egress.Destination{Domain: "api.example.com"}, Protocol: "tls", Ports: []int{443}},
		egress.Rule{To: egress.Destination{IP: "1.1.1.1"}, Protocol: "tcp", Ports: []int{853}},
		egress.Rule{To: egress.Destination{CIDR: "10.42.9.0/24"}, Protocol: "udp", Ports: []int{123, 5353}},
		egress.Rule{To: egress.Destination{CIDR: "10.0.0.0/8"}, Protocol: "icmp", Types: []string{"echo-request"}},
		egress.Rule{To: egress.Destination{Service: "web"}, Protocol: "tcp", Ports: []int{80}},
	)
	service := netip.MustParseAddr("172.31.4.7")
	binding := ServiceBinding{Name: "web", RuleID: "", Address: service}
	for _, grant := range policy.Grants {
		if grant.Rule.To.Service == "web" {
			binding.RuleID = grant.ID
		}
	}
	rules := transportController(t, policy, []ServiceBinding{binding}, []int{8000}).initialRules(netip.MustParseAddr("1.1.1.1"))
	expected := []string{
		"  meta skuid 1000 ip daddr 1.1.1.1/32 tcp dport { 853 } counter name " + counterFor(t, policy, "tcp", "1.1.1.1/32") + " accept",
		"  ip saddr 1.1.1.1/32 tcp sport { 853 } ct state established accept",
		"  meta skuid 1000 ip daddr 10.42.9.0/24 udp dport { 123, 5353 } counter name " + counterFor(t, policy, "udp", "10.42.9.0/24") + " accept",
		"  ip saddr 10.42.9.0/24 udp sport { 123, 5353 } ct state established accept",
		"  meta skuid 1000 ip daddr 10.0.0.0/8 icmp type echo-request counter name " + counterFor(t, policy, "icmp", "10.0.0.0/8") + " accept",
		"  ip saddr 10.0.0.0/8 icmp type echo-reply ct state established,related accept",
		"  meta skuid 1000 ip daddr 172.31.4.7 tcp dport { 80 } counter name " + counterFor(t, policy, "tcp", "web") + " accept",
		"  ip saddr 172.31.4.7 tcp sport { 80 } ct state established accept",
		"  ip saddr 172.17.0.1 tcp dport { 8000 } ct state new,established accept",
		"  ip daddr 172.17.0.1 tcp sport { 8000 } ct state established accept",
		" counter " + counterFor(t, policy, "icmp", "10.0.0.0/8") + " { }",
	}
	for _, line := range expected {
		if !strings.Contains(rules, line+"\n") {
			t.Errorf("rendered ruleset is missing:\n%s\n--- got ---\n%s", line, rules)
		}
	}
	// A TLS name is routed by the guard, never opened as a packet-filter hole.
	if strings.Contains(rules, "api.example.com") {
		t.Error("a TLS domain grant leaked into the packet filter")
	}
	// An ADDRESS grant renders inside the protected-drop/deny window, so the
	// permanent denials still win inside it. An approved SERVICE renders before
	// that drop: its container address is inside one of the runtime's own
	// subnets, which this run protects, and that one address is what a human
	// approved.
	protected := strings.Index(rules, "meta skuid 1000 ip daddr @protected4 counter name protected_agent")
	deny := strings.Index(rules, "meta skuid 1000 counter name denied_agent reject")
	for _, line := range expected[:6] {
		at := strings.Index(rules, line)
		if !strings.HasPrefix(strings.TrimSpace(line), "ip saddr") && (at < protected || at > deny) {
			t.Errorf("address grant is outside the protected-drop/deny window: %s", line)
		}
	}
	if at := strings.Index(rules, expected[6]); at < 0 || at > protected {
		t.Errorf("the approved service grant does not precede the protected drop: %s", expected[6])
	}
	// The served port's reply goes TO the protected bridge gateway, so it has to
	// precede that drop too — and it carries no skuid, because a listening
	// socket's SYN-ACK is the kernel's. Conntrack is what scopes it.
	if at := strings.Index(rules, expected[9]); at < 0 || at > protected {
		t.Errorf("the served port's reply does not precede the protected drop: %s", expected[9])
	}
	if strings.Contains(rules, "meta skuid 1000 tcp sport") {
		t.Error("the served port's reply still requires the agent's uid, which a SYN-ACK does not carry")
	}
	ingressProtected := strings.Index(rules, "ip saddr @protected4 counter name denied_ingress drop")
	ingressDeny := strings.LastIndex(rules, "counter name denied_ingress drop")
	for _, line := range expected[:6] {
		at := strings.Index(rules, line)
		if strings.HasPrefix(strings.TrimSpace(line), "ip saddr ") && (at < ingressProtected || at > ingressDeny) {
			t.Errorf("return rule is outside the protected-drop/deny window: %s", line)
		}
	}
	for _, line := range []string{expected[7], expected[8]} {
		if at := strings.Index(rules, line); at < 0 || at > ingressProtected {
			t.Errorf("approved sidecar/served-port ingress does not precede the protected drop: %s", line)
		}
	}
	// The agent's own namespace loopback is permitted before the protected drop:
	// a local test server is not an attempt on a protected host surface.
	for _, line := range []string{`  meta skuid 1000 oifname "lo" accept`, "  meta skuid 1000 ip daddr 127.0.0.0/8 accept"} {
		if at := strings.Index(rules, line); at < 0 || at > protected {
			t.Errorf("the run's own loopback is not permitted before the protected drop: %s", line)
		}
	}
}

// A policy with no address grant must render exactly the TLS-only ruleset it
// always did: the new sections are empty, not "empty-ish".
func TestATLSOnlyPolicyRendersNoPacketFilterGrants(t *testing.T) {
	policy := transportPolicy(t, egress.Rule{To: egress.Destination{Domain: "api.example.com"}, Protocol: "tls", Ports: []int{443}})
	rules := transportController(t, policy, nil, nil).initialRules(netip.MustParseAddr("1.1.1.1"))
	for _, unwanted := range []string{"grant_", "ct state new,established", "icmp type echo-request"} {
		if strings.Contains(rules, unwanted) {
			t.Errorf("a TLS-only policy rendered %q:\n%s", unwanted, rules)
		}
	}
	// The only return rules a TLS-only policy renders are the guard's own; a
	// per-grant one is scoped to its source address, and there is no grant.
	for _, line := range strings.Split(rules, "\n") {
		if strings.Contains(line, "ip saddr") && !strings.Contains(line, "@protected4") {
			t.Errorf("a TLS-only policy rendered a per-grant return rule: %s", line)
		}
	}
	if !strings.Contains(rules, " counter denied_service { }\n set protected4 {") {
		t.Errorf("the counter block gained a stray line:\n%s", rules)
	}
	if !strings.Contains(rules, "protected_agent reject with icmpx type admin-prohibited\n  meta skuid 1000 counter name denied_agent") {
		t.Errorf("the agent deny no longer follows the protected drop directly:\n%s", rules)
	}
	if !strings.Contains(rules, "tcp sport { 443 } ct state established accept\n  counter name denied_ingress drop") {
		t.Errorf("the ingress deny no longer follows the TLS return rule directly:\n%s", rules)
	}
}

// The capture chain IS the policy's TLS port set, and the lease set is keyed by
// address AND port: nothing in the kernel grants a port the policy did not.
func TestRenderedRulesCaptureExactlyTheGrantedTLSPorts(t *testing.T) {
	policy := transportPolicy(t,
		egress.Rule{To: egress.Destination{Domain: "dot.example.com"}, Protocol: "tls", Ports: []int{853, 8443}},
		egress.Rule{To: egress.Destination{Domain: "api.example.com"}, Protocol: "tls", Ports: []int{443}},
	)
	rules := transportController(t, policy, nil, nil).initialRules(netip.MustParseAddr("1.1.1.1"))
	for _, line := range []string{
		"  meta nfproto ipv4 meta skuid 1000 tcp dport { 443, 853, 8443 } ip daddr != @protected4 redirect to :15443",
		" set leases4 { type ipv4_addr . inet_service; flags timeout; size 4096; gc-interval 1s; }",
		"  meta skuid 65532 ip daddr . tcp dport @leases4 accept",
		"  meta skuid 65532 meta l4proto tcp ct state established accept",
		"  tcp sport { 443, 853, 8443 } ct state established accept",
	} {
		if !strings.Contains(rules, line+"\n") {
			t.Errorf("rendered ruleset is missing:\n%s\n--- got ---\n%s", line, rules)
		}
	}
	// A port nobody granted is captured nowhere, and the guard's upstream leg
	// matches a leased address AND port, never an address on a bare port.
	for _, unwanted := range []string{"8444", "@leases4 tcp dport", "ip daddr @leases4"} {
		if strings.Contains(rules, unwanted) {
			t.Errorf("rendered ruleset contains %q:\n%s", unwanted, rules)
		}
	}
	// A policy with no tls grant captures no TLS port at all — an empty set
	// would be a kernel syntax error, and a default one would be an invention.
	raw := transportPolicy(t, egress.Rule{To: egress.Destination{IP: "1.1.1.1"}, Protocol: "tcp", Ports: []int{853}})
	rendered := transportController(t, raw, nil, nil).initialRules(netip.MustParseAddr("1.1.1.1"))
	if strings.Contains(rendered, "redirect to :15443") || !strings.Contains(rendered, "  tcp sport { 443 } ct state established accept") {
		t.Errorf("a policy with no tls grant still captured a TLS port:\n%s", rendered)
	}
	// The elements the controller installs carry the port with the address.
	now := testBootNow()
	elements, installed := leaseRules(map[string]Lease{
		"a": {Name: "dot.example.com", Peer: netip.MustParseAddr("93.184.216.34"), Port: 853, Expires: now.Add(10 * time.Second)},
		"b": {Name: "dot.example.com", Peer: netip.MustParseAddr("93.184.216.34"), Port: 8443, Expires: now.Add(10 * time.Second)},
	}, now)
	if !strings.Contains(elements, "{ 93.184.216.34 . 8443 timeout 9750ms, 93.184.216.34 . 853 timeout 9750ms }") || len(installed) != 2 {
		t.Fatalf("one address on two granted ports is two leases: %q %v", elements, installed)
	}
}

// A serve port may not collide with a port this policy captures: the agent's own
// connection to it would be redirected to the guard instead of reaching it.
func TestServePortsCannotCollideWithACapturedTLSPort(t *testing.T) {
	policy := transportPolicy(t, egress.Rule{To: egress.Destination{Domain: "dot.example.com"}, Protocol: "tls", Ports: []int{8443}})
	clock := testBootClock()
	identity := Identity{Clock: clock.Domain(), RunID: strings.Repeat("a", 32), Epoch: strings.Repeat("b", 32), PolicyFingerprint: policy.Fingerprint}
	for _, port := range []int{443, 53, 8443} {
		if _, err := NewController(identity, policy, nil, nil, nil, []int{port}, serveIngress, nil, clock, func(context.Context, string) error { return nil }); err == nil {
			t.Errorf("serve port %d was accepted alongside the capture", port)
		}
	}
	if _, err := NewController(identity, policy, nil, nil, nil, []int{8000}, serveIngress, nil, clock, func(context.Context, string) error { return nil }); err != nil {
		t.Fatal("an uncaptured serve port was refused", err)
	}
}

// A wide grant is not a hole in the permanent denials: the protected set is
// still dropped first, and its packets are still counted as protected.
func TestGrantedCIDRCannotBeatAProtectedAddress(t *testing.T) {
	policy := transportPolicy(t, egress.Rule{To: egress.Destination{CIDR: "10.0.0.0/8"}, Protocol: "tcp", Ports: []int{5432}})
	host := netip.MustParsePrefix("10.7.7.7/32")
	clock := testBootClock()
	c, err := NewController(Identity{Clock: clock.Domain(), RunID: strings.Repeat("a", 32), Epoch: strings.Repeat("b", 32),
		PolicyFingerprint: policy.Fingerprint}, policy, []netip.Prefix{host}, nil, nil, nil, netip.Addr{}, nil, clock, func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	rules := c.initialRules(netip.MustParseAddr("1.1.1.1"))
	if !strings.Contains(rules, "10.7.7.7/32") {
		t.Fatal("the protected host address is missing from the kernel's protected set")
	}
	protected := strings.Index(rules, "meta skuid 1000 ip daddr @protected4 counter name protected_agent")
	grant := strings.Index(rules, "meta skuid 1000 ip daddr 10.0.0.0/8 tcp dport { 5432 }")
	if protected < 0 || grant < 0 || protected > grant {
		t.Fatal("a granted CIDR was allowed to precede the protected drop")
	}
	if policy.Address(netip.MustParseAddr("10.7.7.7"), "tcp", 5432, 0, 0, []netip.Prefix{host}).Allowed {
		t.Fatal("policy evaluation let a granted CIDR cover a protected address")
	}
}

// Service grants exist only as an exact launch-time address binding. A missing,
// duplicated or unapproved binding is a configuration failure, never a guess.
func TestServiceGrantsRequireAnExactBinding(t *testing.T) {
	policy := transportPolicy(t, egress.Rule{To: egress.Destination{Service: "web"}, Protocol: "tcp", Ports: []int{80}})
	ruleID := policy.Grants[0].ID
	address := netip.MustParseAddr("172.31.4.7")
	cases := map[string][]ServiceBinding{
		"missing":     nil,
		"wrong name":  {{Name: "other", RuleID: ruleID, Address: address}},
		"unknown ID":  {{Name: "web", RuleID: strings.Repeat("c", 32), Address: address}},
		"protected":   {{Name: "web", RuleID: ruleID, Address: netip.MustParseAddr("127.0.0.1")}},
		"ipv6":        {{Name: "web", RuleID: ruleID, Address: netip.MustParseAddr("fd00::1")}},
		"duplicated":  {{Name: "web", RuleID: ruleID, Address: address}, {Name: "web", RuleID: ruleID, Address: address}},
		"unrequested": {{Name: "web", RuleID: ruleID, Address: address}, {Name: "other", RuleID: strings.Repeat("d", 32), Address: address}},
	}
	for name, services := range cases {
		if _, err := addressGrants(policy, services); err == nil {
			t.Errorf("%s service binding was accepted", name)
		}
	}
	grants, err := addressGrants(policy, []ServiceBinding{{Name: "web", RuleID: ruleID, Address: address}})
	if err != nil || len(grants) != 1 || grants[0].target != "172.31.4.7" || grants[0].display != "web" {
		t.Fatal("an exactly bound service grant was not rendered", grants, err)
	}
}
