package networkgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func openMCPRoute() CredentialBrokerRoute {
	return CredentialBrokerRoute{Name: "mcp-0", Kind: CredentialBrokerMCP, Upstream: "mcp.example.com", Header: "authorization",
		HeaderPrefix: "Bearer ", Methods: []string{"POST", "GET", "DELETE"}, Path: "/mcp", Port: 443}
}

func openBrokerSecret() CredentialBrokerSecret {
	return CredentialBrokerSecret{Name: "mcp-0", Substitute: strings.Repeat("s", 64), Credential: "real-mcp-token"}
}

// An open helper carries MCP routes only: a provider key has no gateway there to hold its agent to
// the route, so it stays a filtered run's.
func TestOpenBrokerConfigCarriesOnlyMCPRoutes(t *testing.T) {
	valid := OpenBrokerConfig{Version: 1, RunID: strings.Repeat("a", 32), Epoch: strings.Repeat("b", 32), Brokers: []CredentialBrokerRoute{openMCPRoute()}}
	data, _ := json.Marshal(valid)
	if _, err := ReadOpenBrokerConfig(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*OpenBrokerConfig){
		"a provider route": func(c *OpenBrokerConfig) {
			c.Brokers = []CredentialBrokerRoute{{Name: "claude", Kind: CredentialBrokerProvider, Upstream: "api.anthropic.com",
				Header: "x-api-key", Methods: []string{"POST"}, Path: "/v1/messages", Port: 443}}
		},
		"no route":        func(c *OpenBrokerConfig) { c.Brokers = nil },
		"another version": func(c *OpenBrokerConfig) { c.Version = 2 },
		"a bad run":       func(c *OpenBrokerConfig) { c.RunID = "run" },
		"a bad route":     func(c *OpenBrokerConfig) { c.Brokers[0].Header = "cookie" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := valid
			changed.Brokers = append([]CredentialBrokerRoute(nil), valid.Brokers...)
			mutate(&changed)
			data, _ := json.Marshal(changed)
			if _, err := ReadOpenBrokerConfig(bytes.NewReader(data)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if _, err := ReadOpenBrokerConfig(strings.NewReader(strings.TrimSuffix(string(data), "}") + `,"policy":{}}`)); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

func TestOpenBrokerListensOnItsOneAddress(t *testing.T) {
	address := func(cidr string) net.Addr {
		ip, network, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		network.IP = ip
		return network
	}
	host, err := oneIPv4([]net.Addr{address("172.18.0.5/16"), address("fd00::5/64"), address("169.254.0.9/16")})
	if err != nil || host != netip.MustParseAddr("172.18.0.5") {
		t.Fatalf("host = %v, %v", host, err)
	}
	for name, addresses := range map[string][]net.Addr{
		"none":         nil,
		"only IPv6":    {address("fd00::5/64")},
		"two networks": {address("172.18.0.5/16"), address("172.19.0.5/16")},
	} {
		if _, err := oneIPv4(addresses); err == nil {
			t.Errorf("%s: an address was chosen", name)
		}
	}
	third, err := newOpenCredentialBroker(openMCPRoute(), host, 2, openBrokerSecret())
	if err != nil || third.listen != "172.18.0.5:15582" || third.address != "coop-broker:15582" {
		t.Fatalf("route 2 listens on %q and answers to %q (%v)", third.listen, third.address, err)
	}
}

// The helper answers only requests addressed to its own listener, and replaces only its route's
// stand-in — the same checks a filtered guard's broker makes.
func TestOpenBrokerServesOnlyItsOwnListener(t *testing.T) {
	host := netip.MustParseAddr("172.18.0.5")
	b, err := newOpenCredentialBroker(openMCPRoute(), host, 0, openBrokerSecret())
	if err != nil {
		t.Fatal(err)
	}
	b.setProxy(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer real-mcp-token" || request.URL.Host != "mcp.example.com" {
			t.Fatalf("upstream request = %s %#v", request.URL, request.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}))
	send := func(hostHeader, token string) int {
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
		request.Host = hostHeader
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		b.handler().ServeHTTP(recorder, request)
		return recorder.Code
	}
	if b.listen != "172.18.0.5:15580" {
		t.Fatalf("route 0 listens on %s", b.listen)
	}
	if code := send("coop-broker:15580", strings.Repeat("s", 64)); code != http.StatusOK {
		t.Fatalf("its own listener answered %d", code)
	}
	for _, other := range []string{"172.18.0.5:15580", "coop-broker:15581", "127.0.0.1:15580"} {
		if code := send(other, strings.Repeat("s", 64)); code != http.StatusForbidden {
			t.Fatalf("Host %s answered %d", other, code)
		}
	}
	if code := send("coop-broker:15580", strings.Repeat("t", 64)); code != http.StatusUnauthorized {
		t.Fatalf("a wrong stand-in answered %d", code)
	}
	provider := CredentialBrokerRoute{Name: "claude", Kind: CredentialBrokerProvider, Upstream: "api.anthropic.com", Header: "x-api-key",
		Methods: []string{"POST"}, Path: "/v1/messages", Port: 443}
	for name, build := range map[string]func() (*credentialBroker, error){
		"a provider route": func() (*credentialBroker, error) {
			return newOpenCredentialBroker(provider, host, 0, CredentialBrokerSecret{Name: "claude"})
		},
		"another route's secret": func() (*credentialBroker, error) {
			return newOpenCredentialBroker(openMCPRoute(), host, 0, CredentialBrokerSecret{Name: "mcp-1"})
		},
		"an IPv6 host": func() (*credentialBroker, error) {
			return newOpenCredentialBroker(openMCPRoute(), netip.MustParseAddr("fd00::5"), 0, openBrokerSecret())
		},
	} {
		if _, err := build(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The direct dial reaches the route's one upstream on 443 and nothing else, whatever the proxy asks.
func TestOpenBrokerDialsOnlyItsUpstream(t *testing.T) {
	b, err := newOpenCredentialBroker(openMCPRoute(), netip.MustParseAddr("172.18.0.5"), 0, openBrokerSecret())
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct{ network, address string }{
		{"tcp", "evil.example.com:443"}, {"tcp", "mcp.example.com:8443"}, {"udp", "mcp.example.com:443"}, {"tcp", "10.0.0.1:443"},
	} {
		if _, err := b.dialDirect(context.Background(), target.network, target.address); err != Failure("credential_broker_request_refused") {
			t.Errorf("dial %s %s = %v", target.network, target.address, err)
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.dialDirect(cancelled, "tcp", "mcp.example.com:443"); err != Failure("credential_broker_upstream_unavailable") {
		t.Fatalf("the route's own upstream = %v, want a dial attempt", err)
	}
}

// A stopped helper exits cleanly; a listener it cannot open fails it, so the host never waits on a
// helper that serves nothing.
func TestServeOpenBrokersStopsCleanlyAndFailsOnAnOccupiedListener(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	host := netip.MustParseAddr("172.18.0.5")
	broker := func(address string) *credentialBroker {
		b, err := newOpenCredentialBroker(openMCPRoute(), host, 0, openBrokerSecret())
		if err != nil {
			t.Fatal(err)
		}
		b.listen = address
		return b
	}
	if err := serveOpenBrokers(context.Background(), []*credentialBroker{broker(occupied.Addr().String())}, nil); err != Failure("credential_broker_listener_unavailable") {
		t.Fatalf("an occupied listener = %v", err)
	}
	// Ready fires once — after the LAST listener accepts, so the host is never pointed at a helper
	// that is still binding.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	var ready atomic.Int32
	if err := serveOpenBrokers(ctx, []*credentialBroker{broker("127.0.0.1:0"), broker("127.0.0.1:0")}, func() { ready.Add(1) }); err != nil {
		t.Fatalf("a stopped helper = %v", err)
	}
	if ready.Load() != 1 {
		t.Fatalf("ready fired %d times", ready.Load())
	}
}
