package egress

import (
	"net/netip"
	"testing"
)

func TestAddressEnvelopeContainmentCannotChangeFamilyOrWiden(t *testing.T) {
	approved := Rule{To: Destination{CIDR: "10.42.0.0/16"}, Protocol: "tcp", Ports: []int{443, 5432}}
	for _, test := range []struct {
		cidr string
		port int
		want bool
	}{
		{"10.42.0.0/16", 443, true}, {"10.42.9.0/24", 5432, true}, {"10.42.9.12/32", 443, true},
		{"10.0.0.0/8", 443, false}, {"10.43.0.0/16", 443, false}, {"10.42.9.0/24", 80, false},
		{"2001:db8::/32", 443, false}, {"::ffff:10.42.9.12/128", 443, false},
	} {
		request := Rule{To: Destination{CIDR: test.cidr}, Protocol: "tcp", Ports: []int{test.port}}
		if got := Covers(approved, request); got != test.want {
			t.Errorf("%s:%d: got %t want %t", test.cidr, test.port, got, test.want)
		}
	}
}

func TestCloudPlatformAddressesRemainProtectedInsideExplicitGrants(t *testing.T) {
	for _, address := range []string{"100.100.100.200", "168.63.129.16"} {
		ip := netip.MustParseAddr(address)
		if !Protected(ip, nil) || PublicAnswer(ip, nil) {
			t.Errorf("cloud platform endpoint admitted: %s", ip)
		}
		policy, err := Compile("test", Filtered, []Input{{Rules: []Rule{{To: Destination{IP: address}, Protocol: "tcp", Ports: []int{80}}}, Origin: Origin{Kind: "operator"}}}, nil, false, ownerKey())
		if err != nil {
			t.Fatal(err)
		}
		if policy.Address(ip, "tcp", 80, 0, 0, nil).Allowed {
			t.Errorf("explicit grant beat protected platform endpoint: %s", ip)
		}
	}
}
