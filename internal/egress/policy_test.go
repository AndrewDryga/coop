package egress

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func tlsRule(name string) Rule {
	return Rule{To: Destination{Domain: name}, Protocol: "tls", Ports: []int{443}}
}
func ownerKey() []byte { return bytes.Repeat([]byte{42}, 32) }

func TestRuleGrammar(t *testing.T) {
	valid := []Rule{
		tlsRule("Docs.Example.COM."), tlsRule("*.services.example.com"),
		{To: Destination{IP: "10.42.8.12"}, Protocol: "tcp", Ports: []int{5432}},
		{To: Destination{CIDR: "10.42.9.0/24"}, Protocol: "udp", Ports: []int{123}},
		{To: Destination{CIDR: "10.42.9.0/24"}, Protocol: "icmp", Types: []string{"echo-request"}},
		{To: Destination{IP: "fd12::1"}, Protocol: "icmpv6", Types: []string{"echo-reply"}, Codes: []int{0}},
		{To: Destination{Provider: "model", Features: []string{"cloud-mcp"}}},
	}
	for _, rule := range valid {
		normalized, err := NormalizeRules([]Rule{rule})
		if err != nil {
			t.Fatalf("valid rule %#v: %v", rule, err)
		}
		again, err := NormalizeRules(normalized)
		if err != nil || !reflect.DeepEqual(normalized, again) {
			t.Fatalf("not idempotent: %#v / %#v: %v", normalized, again, err)
		}
	}
	invalid := []Rule{
		{}, {To: Destination{Domain: "example.com", IP: "1.1.1.1"}, Protocol: "tls", Ports: []int{443}},
		{To: Destination{Domain: "example.com"}, Protocol: "tls"},
		{To: Destination{Domain: "example.com"}, Protocol: "tls", Ports: []int{0}},
		{To: Destination{Domain: "example.com"}, Protocol: "tls", Ports: []int{65536}},
		{To: Destination{Domain: "example.com"}, Protocol: "tcp", Ports: []int{443}},
		{To: Destination{IP: "1.1.1.1"}, Protocol: "tls", Ports: []int{443}},
		{To: Destination{IP: "::ffff:1.1.1.1"}, Protocol: "tcp", Ports: []int{443}},
		{To: Destination{IP: "fe80::1%en0"}, Protocol: "tcp", Ports: []int{443}},
		{To: Destination{CIDR: "10.0.0.1/8"}, Protocol: "tcp", Ports: []int{443}},
		{To: Destination{CIDR: "0.0.0.0/0"}, Protocol: "tcp", Ports: []int{443}},
		{To: Destination{CIDR: "::/0"}, Protocol: "tcp", Ports: []int{443}},
		{To: Destination{IP: "10.0.0.1"}, Protocol: "icmpv6", Types: []string{"echo-request"}},
		{To: Destination{IP: "10.0.0.1"}, Protocol: "icmp", Ports: []int{443}, Types: []string{"8"}},
		{To: Destination{IP: "10.0.0.1"}, Protocol: "icmp", Types: []string{"-1"}},
		{To: Destination{IP: "10.0.0.1"}, Protocol: "icmp", Types: []string{"256"}},
		{To: Destination{Provider: "model"}, Protocol: "tls", Ports: []int{443}},
		{To: Destination{Domain: "example.com", Features: []string{"cloud-mcp"}}, Protocol: "tls", Ports: []int{443}},
	}
	for _, rule := range invalid {
		if _, err := NormalizeRules([]Rule{rule}); err == nil {
			t.Errorf("accepted invalid rule %#v", rule)
		}
	}
}

func TestDomainNormalizationAndWildcardBoundary(t *testing.T) {
	for _, name := range []string{"https://example.com", "example.com:443", "example.com/path", "example.com..", "*.com", "*.co.uk", "*.github.io", "*example.com", "*.*.example.com", "example", "a\n.example.com", "é.example.com", "127.0.0.1", "-bad.example.com", strings.Repeat("a", 64) + ".com"} {
		if _, err := NormalizeDomain(name, true); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	for name, want := range map[string]bool{"a.example.com": true, "a-b.example.com": true, "example.com": false, "a.b.example.com": false, "badexample.com": false, "a.example.com.attacker.net": false, ".example.com": false} {
		if got := MatchesDomain("*.example.com", name); got != want {
			t.Errorf("match %q = %v, want %v", name, got, want)
		}
	}
	if name, err := NormalizeDomain("XN--BCHER-KVA.Example.", false); err != nil || name != "xn--bcher-kva.example" {
		t.Fatalf("punycode normalization %q %v", name, err)
	}
}

func TestStrictRulesDocument(t *testing.T) {
	valid := "egress_rules:\n  - to: {domain: Example.COM.}\n    protocol: tls\n    ports: [443, 443]\n"
	rules, err := DecodeRules([]byte(valid))
	if err != nil || len(rules) != 1 || rules[0].To.Domain != "example.com" || len(rules[0].Ports) != 1 {
		t.Fatalf("decode: %#v %v", rules, err)
	}
	for _, invalid := range []string{valid + "---\negress_rules: []\n", valid + "approved: true\n", strings.Replace(valid, "domain:", "host:", 1), strings.Replace(valid, "protocol: tls", "protocol: tls\n    protocol: tcp", 1), "", strings.Repeat(" ", MaxDocumentBytes+1)} {
		if _, err := DecodeRules([]byte(invalid)); err == nil {
			t.Errorf("accepted invalid document %.80q", invalid)
		}
	}
	tooMany := make([]Rule, MaxRules+1)
	for i := range tooMany {
		tooMany[i] = tlsRule("example.com")
	}
	if _, err := NormalizeRules(tooMany); err == nil {
		t.Fatal("accepted excessive rules before deduplication")
	}
}

func TestRulePresenceAndASCII(t *testing.T) {
	for _, rule := range []string{
		"{to: {domain: example.com, ip: ''}, protocol: tls, ports: [443]}",
		"{to: {domain: example.com, ip: null}, protocol: tls, ports: [443]}",
		"{to: {provider: model}, ports: []}",
		"{to: {provider: model}, protocol: ''}",
		"{to: {domain: example.com, features: []}, protocol: tls, ports: [443]}",
		"{to: {domain: example.com}, protocol: tls, ports: null}",
	} {
		if _, err := DecodeRules([]byte("egress_rules: [" + rule + "]")); err == nil {
			t.Errorf("accepted forbidden field presence: %s", rule)
		}
	}
	for _, name := range []string{"K.example.com", "İ.example.com", strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 62)} {
		if _, err := NormalizeDomain(name, false); err == nil {
			t.Errorf("accepted non-ASCII/overlength %q", name)
		}
	}
	maximum := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	for _, name := range []string{maximum, maximum + "."} {
		if _, err := NormalizeDomain(name, false); err != nil {
			t.Errorf("valid maximum domain: %v", err)
		}
	}
}

func TestDecisionAPIsRejectInvalidModesEvenWithGrants(t *testing.T) {
	s, err := Compile("test", Filtered, []Input{{Rules: []Rule{tlsRule("example.com"), {To: Destination{IP: "10.0.0.1"}, Protocol: "tcp", Ports: []int{443}}}, Origin: Origin{Kind: "operator"}}}, nil, false, ownerKey())
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []Mode{"", "typo", None} {
		s.Mode = mode
		if s.Domain("example.com", 443).Allowed || s.AdmitsName("example.com") || s.Address(netip.MustParseAddr("10.0.0.1"), "tcp", 443, 0, 0, nil).Allowed || s.RequireSupported() == nil {
			t.Errorf("invalid mode %q gained access", mode)
		}
	}
}

func TestSnapshotCanonicalAuthority(t *testing.T) {
	inputs := []Input{{Rules: []Rule{tlsRule("Z.Example.com"), tlsRule("a.example.com")}, Origin: Origin{Kind: "operator"}}}
	first, err := Compile("test", Filtered, inputs, nil, false, ownerKey())
	if err != nil {
		t.Fatal(err)
	}
	inputs[0].Rules[0], inputs[0].Rules[1] = inputs[0].Rules[1], inputs[0].Rules[0]
	second, err := Compile("test", Filtered, inputs, nil, false, ownerKey())
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("order changed authority: %v", err)
	}
	if err := first.Verify(ownerKey()); err != nil {
		t.Fatal(err)
	}
	other, err := Compile("test", Filtered, inputs, nil, false, bytes.Repeat([]byte{9}, 32))
	if err != nil || first.Fingerprint == other.Fingerprint || first.Grants[0].ID == other.Grants[0].ID {
		t.Fatal("authority identifiers are not owner-keyed")
	}
	copy := first.Clone()
	copy.Grants[0].Rule.Ports[0] = 8443
	if first.Grants[0].Rule.Ports[0] != 443 {
		t.Fatal("snapshot clone shares mutable grants")
	}
	if copy.Verify(ownerKey()) == nil {
		t.Fatal("accepted tampered rule")
	}
	copy = first.Clone()
	copy.ExportDestinations = true
	if copy.Verify(ownerKey()) == nil {
		t.Fatal("export disclosure not bound into fingerprint")
	}
	copy = first.Clone()
	copy.Dependencies = []Dependency{{Provider: "extra"}}
	if copy.Verify(ownerKey()) == nil {
		t.Fatal("dependency envelope not fingerprinted")
	}
	encoded, _ := json.Marshal(first)
	var decoded Snapshot
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Verify(ownerKey()) != nil {
		t.Fatal("snapshot did not survive persistence")
	}
}

func TestProviderDependenciesAreSelectedAndFeaturesExplicit(t *testing.T) {
	bundle := Bundle{Provider: "model", Client: ClientCLI, Version: "2026-09-08.1", Backend: "direct", AuthMode: "key",
		Core: []Rule{tlsRule("api.example.com")}, Features: map[string][]Rule{"cloud-mcp": {tlsRule("mcp.example.com")}}}
	core, err := Compile("test", Filtered, nil, []Bundle{bundle}, false, ownerKey())
	if err != nil || !core.Domain("api.example.com", 443).Allowed || core.Domain("mcp.example.com", 443).Allowed {
		t.Fatalf("core expansion: %#v %v", core, err)
	}
	input := []Input{{Rules: []Rule{{To: Destination{Provider: "model", Features: []string{"cloud-mcp"}}}}, Origin: Origin{Kind: "project"}}}
	full, err := Compile("test", Filtered, input, []Bundle{bundle}, false, ownerKey())
	if err != nil || !full.Domain("mcp.example.com", 443).Allowed || full.Fingerprint == core.Fingerprint {
		t.Fatalf("feature expansion: %v", err)
	}
	if _, err := Compile("test", Filtered, input, nil, false, ownerKey()); err == nil {
		t.Fatal("unselected provider gained authority")
	}
	input[0].Rules[0].To.Features = []string{"unknown"}
	if _, err := Compile("test", Filtered, input, []Bundle{bundle}, false, ownerKey()); err == nil {
		t.Fatal("unknown feature gained authority")
	}
	for _, mode := range []Mode{None, Open} {
		s, err := Compile("test", mode, nil, []Bundle{bundle}, false, ownerKey())
		if err != nil || len(s.Grants) != 0 || len(s.Dependencies) != 0 {
			t.Fatalf("implicit exception in %s: %v", mode, err)
		}
		if _, err := Compile("test", mode, input, []Bundle{bundle}, false, ownerKey()); err == nil {
			t.Fatalf("rules accepted with %s", mode)
		}
	}
	if err := full.Verify(ownerKey()); err != nil {
		t.Fatal(err)
	}
	bundle.Core = append(bundle.Core, tlsRule("new.example.com"))
	if full.Domain("new.example.com", 443).Allowed || full.Verify(ownerKey()) != nil {
		t.Fatal("bundle changes rewrote captured session authority")
	}
}

func TestProtectedAddressesOverrideGrants(t *testing.T) {
	s, err := Compile("test", Filtered, []Input{{Rules: []Rule{{To: Destination{CIDR: "10.0.0.0/8"}, Protocol: "tcp", Ports: []int{5432}}}, Origin: Origin{Kind: "operator"}}}, nil, false, ownerKey())
	if err != nil {
		t.Fatal(err)
	}
	protected := []netip.Prefix{netip.MustParsePrefix("10.42.0.0/16")}
	for address, allowed := range map[string]bool{"10.1.2.3": true, "10.42.1.1": false, "169.254.169.254": false, "127.0.0.1": false, "::ffff:10.42.1.1": false} {
		if got := s.Address(netip.MustParseAddr(address), "tcp", 5432, 0, 0, protected); got.Allowed != allowed {
			t.Errorf("%s: %#v", address, got)
		}
	}
	if s.Address(netip.MustParseAddr("10.1.2.3"), "tcp", 5433, 0, 0, protected).Allowed {
		t.Fatal("port widened")
	}
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1", "100.64.0.1", "192.0.2.1", "198.18.0.1", "224.0.0.1", "::1", "::ffff:8.8.8.8", "fd12::1", "fe80::1", "2001:db8::1"} {
		if PublicAnswer(netip.MustParseAddr(address), nil) {
			t.Errorf("unsafe DNS answer admitted: %s", address)
		}
	}
	for _, address := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !PublicAnswer(netip.MustParseAddr(address), nil) {
			t.Errorf("public DNS answer denied: %s", address)
		}
	}
}

func TestUnsupportedCapabilitiesRejectWholePolicy(t *testing.T) {
	unsupported := map[string]Rule{
		"tls on another port": {To: Destination{Domain: "example.com"}, Protocol: "tls", Ports: []int{8443}},
		"raw tcp on 443":      {To: Destination{IP: "10.0.0.1"}, Protocol: "tcp", Ports: []int{443}},
		"raw udp on 53":       {To: Destination{IP: "10.0.0.1"}, Protocol: "udp", Ports: []int{53}},
		"ipv6 address":        {To: Destination{IP: "2001:4860:4860::8888"}, Protocol: "tcp", Ports: []int{5432}},
		"icmpv6":              {To: Destination{CIDR: "2001:db8::/32"}, Protocol: "icmpv6", Types: []string{"echo-request"}},
		"icmp beyond echo":    {To: Destination{IP: "10.0.0.1"}, Protocol: "icmp", Types: []string{"3"}},
		"protected loopback":  {To: Destination{IP: "127.0.0.1"}, Protocol: "tcp", Ports: []int{5432}},
		"protected metadata":  {To: Destination{CIDR: "169.254.0.0/16"}, Protocol: "tcp", Ports: []int{80}},
		"service on captured": {To: Destination{Service: "web"}, Protocol: "tcp", Ports: []int{443}},
	}
	for name, rule := range unsupported {
		t.Run(name, func(t *testing.T) {
			s, err := Compile("test", Filtered, []Input{{Rules: []Rule{tlsRule("api.example.com"), rule}, Origin: Origin{Kind: "operator"}}}, nil, false, ownerKey())
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RequireSupported(); err == nil {
				t.Fatalf("unsupported rule silently ignored: %#v", rule)
			}
		})
	}
}

func TestSupportedTransportsAreEnforceable(t *testing.T) {
	supported := []Rule{
		tlsRule("api.example.com"), tlsRule("*.example.com"),
		{To: Destination{IP: "10.0.0.1"}, Protocol: "tcp", Ports: []int{5432}},
		{To: Destination{CIDR: "10.42.9.0/24"}, Protocol: "udp", Ports: []int{123}},
		{To: Destination{CIDR: "10.0.0.0/8"}, Protocol: "icmp", Types: []string{"echo-request"}},
		{To: Destination{Service: "web"}, Protocol: "tcp", Ports: []int{80}},
	}
	s, err := Compile("test", Filtered, []Input{{Rules: supported, Origin: Origin{Kind: "operator"}}}, nil, false, ownerKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequireSupported(); err != nil {
		t.Fatal(err)
	}
	counters := map[string]string{}
	for _, grant := range s.Grants {
		name := grant.CounterName()
		if grant.Rule.Protocol == "tls" {
			if name != "" {
				t.Errorf("a routed TLS name must not own a packet counter: %q", name)
			}
			continue
		}
		if len(name) != 30 || counters[name] != "" {
			t.Fatalf("address grant counter %q is not a unique bounded name", name)
		}
		counters[name] = grant.ID
		if back, ok := s.GrantByCounter(name); !ok || back.ID != grant.ID {
			t.Fatalf("counter %q does not map back to its grant", name)
		}
	}
	if len(counters) != 4 {
		t.Fatalf("expected one counter per address grant, got %d", len(counters))
	}
	if _, ok := s.GrantByCounter("grant_" + strings.Repeat("f", 24)); ok {
		t.Fatal("an unknown counter was attributed to a grant")
	}
}

func FuzzRulesFailClosed(f *testing.F) {
	f.Add("egress_rules: [{to: {domain: example.com}, protocol: tls, ports: [443]}]")
	f.Add("egress_rules: [{to: {cidr: 0.0.0.0/0}, protocol: tcp, ports: [22]}]")
	f.Fuzz(func(t *testing.T, input string) {
		rules, err := DecodeRules([]byte(input))
		if err != nil {
			return
		}
		again, err := NormalizeRules(rules)
		if err != nil || !reflect.DeepEqual(rules, again) {
			t.Fatal("accepted noncanonical rules")
		}
	})
}
