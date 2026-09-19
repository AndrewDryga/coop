package networkgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func testLaunch(t *testing.T) LaunchConfig {
	t.Helper()
	return LaunchConfig{Version: 1, RunID: strings.Repeat("a", 32), Epoch: strings.Repeat("b", 32), Policy: testPolicy(t)}
}

func TestGuardReadinessCannotReopenDuringShutdown(t *testing.T) {
	var gateway GuardRuntime
	gateway.stopReady()
	gateway.markReady() // delayed listener callback after another component failed
	if gateway.phase.Load() != gatewayStopped {
		t.Fatal("late callback reopened a stopping generation")
	}
	var normal GuardRuntime
	normal.markReady()
	if normal.phase.Load() != gatewayReady {
		t.Fatal("healthy startup did not publish readiness")
	}
	normal.stopReady()
	normal.markReady()
	if normal.phase.Load() != gatewayStopped {
		t.Fatal("stopped generation became ready again")
	}
}

// The collector publishes readiness the moment the gateway is marked ready, not at its next tick: a
// launch starts nothing until a snapshot says ready, and the tick is a second away. Once the observer
// has stopped, a late wake samples nothing, so the terminal sample stays the last one.
func TestGuardReadinessIsPublishedAtOnce(t *testing.T) {
	c, _ := collectorFixture(t)
	gateway := &GuardRuntime{collector: c}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx, func() bool { return gateway.phase.Load() == gatewayReady })
	}()
	wait.For(t, "the collector's first sample", func() bool { return c.Snapshot().Sequence >= 1 })
	if c.Snapshot().Health.Gateway.Status == "ready" {
		t.Fatal("a gateway not yet marked ready was published ready")
	}
	began := time.Now()
	gateway.markReady()
	wait.For(t, "a ready sample", func() bool { return c.Snapshot().Health.Gateway.Status == "ready" })
	if took := time.Since(began); took > 500*time.Millisecond {
		t.Fatalf("readiness waited %s for the collector's tick", took)
	}
	cancel()
	<-done
	c.sample(context.Background(), false, true, c.clock.instant())
	final := c.Snapshot()
	c.Wake()
	// Nothing is left to take the wake: it stays queued, and no sample follows the terminal one.
	if after := c.Snapshot(); !final.Terminal || after.Sequence != final.Sequence || len(c.wake) != 1 {
		t.Fatalf("a wake after the observer stopped was taken (%d queued) or sampled past the terminal sample: %d, then %d", len(c.wake), final.Sequence, after.Sequence)
	}
}

func TestGatewayLaunchConfigurationIsBoundedAndConcrete(t *testing.T) {
	config := testLaunch(t)
	data, _ := json.Marshal(config)
	if _, err := ReadLaunchConfig(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{nil, append(bytes.Clone(data), []byte("{}")...), bytes.Repeat([]byte(" "), MaxLaunchConfigBytes+1),
		bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1,"command":"forbidden"`), 1)} {
		if _, err := ReadLaunchConfig(bytes.NewReader(bad)); err == nil {
			t.Fatal("invalid launch configuration accepted")
		}
	}
	config.Policy.Grants[0].Rule.To.Domain = "API.EXAMPLE.COM"
	if config.Validate() == nil {
		t.Fatal("noncanonical launch grant accepted")
	}
}

// An MCP route is one streamable-HTTP endpoint: its exact path, the protocol's three methods, a
// bearer token and no query — nothing that widens it into a prefix or a second credential shape.
func TestGatewayLaunchConfigurationBoundsAnMCPRoute(t *testing.T) {
	config := testLaunch(t)
	mcp := CredentialBrokerRoute{Name: "mcp-1", Kind: CredentialBrokerMCP, Upstream: "mcp.example.com", Header: "authorization",
		HeaderPrefix: "Bearer ", Methods: []string{"POST", "GET", "DELETE"}, Path: "/mcp", Port: 443}
	config.Brokers = []CredentialBrokerRoute{mcp}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*CredentialBrokerRoute){
		"a path prefix":     func(r *CredentialBrokerRoute) { r.PathPrefix = true },
		"a query":           func(r *CredentialBrokerRoute) { r.AllowQuery = true },
		"another header":    func(r *CredentialBrokerRoute) { r.Header = "x-api-key" },
		"a bare token":      func(r *CredentialBrokerRoute) { r.HeaderPrefix = "" },
		"another method":    func(r *CredentialBrokerRoute) { r.Methods = []string{"POST", "PUT"} },
		"a repeated method": func(r *CredentialBrokerRoute) { r.Methods = []string{"POST", "POST"} },
		"no method":         func(r *CredentialBrokerRoute) { r.Methods = nil },
		"an uppercase name": func(r *CredentialBrokerRoute) { r.Name = "MCP-1" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := config
			route := mcp
			mutate(&route)
			changed.Brokers = []CredentialBrokerRoute{route}
			if changed.Validate() == nil {
				t.Fatal("invalid MCP route accepted")
			}
		})
	}
}

func TestGatewayLaunchConfigurationKeepsBrokerSeparateAndReserved(t *testing.T) {
	config := testLaunch(t)
	claude := CredentialBrokerRoute{Name: "claude", Kind: CredentialBrokerProvider, Upstream: "api.anthropic.com", Header: "x-api-key", Methods: []string{"POST"}, Path: "/v1/messages", Port: 443}
	config.Brokers = []CredentialBrokerRoute{claude, claude} // two accounts, two listeners
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*LaunchConfig){
		"wildcard upstream": func(c *LaunchConfig) { c.Brokers[1].Upstream = "*.anthropic.com" },
		"arbitrary method":  func(c *LaunchConfig) { c.Brokers[1].Methods = []string{"CONNECT"} },
		"arbitrary path":    func(c *LaunchConfig) { c.Brokers[1].Path = "/v1/messages?next=elsewhere" },
		"other port":        func(c *LaunchConfig) { c.Brokers[1].Port = 8443 },
		"serve collision": func(c *LaunchConfig) {
			c.Serve = []int{CredentialBrokerPort + 1}
			c.Ingress = netip.MustParseAddr("172.17.0.1")
		},
		"too many routes": func(c *LaunchConfig) {
			for len(c.Brokers) <= MaxCredentialBrokerRoutes {
				c.Brokers = append(c.Brokers, claude)
			}
		},
		"unknown kind":     func(c *LaunchConfig) { c.Brokers[1].Kind = "proxy" },
		"provider methods": func(c *LaunchConfig) { c.Brokers[1].Methods = []string{"POST", "GET"} },
	} {
		t.Run(name, func(t *testing.T) {
			changed := config
			changed.Brokers = append([]CredentialBrokerRoute(nil), config.Brokers...)
			mutate(&changed)
			if changed.Validate() == nil {
				t.Fatal("invalid broker route accepted")
			}
		})
	}
}

func TestGatewayLaunchConfigurationBindsServiceProxyClientsToProtectedAddresses(t *testing.T) {
	config := testLaunch(t)
	config.Protected = []netip.Prefix{netip.MustParsePrefix("172.31.0.0/16")}
	config.ServiceProxyClients = []ServiceProxyClient{{Name: "web", Address: netip.MustParseAddr("172.31.0.16")}}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*LaunchConfig){
		"outside internal network": func(c *LaunchConfig) { c.ServiceProxyClients[0].Address = netip.MustParseAddr("1.1.1.1") },
		"missing name":             func(c *LaunchConfig) { c.ServiceProxyClients[0].Name = "" },
		"duplicate":                func(c *LaunchConfig) { c.ServiceProxyClients = append(c.ServiceProxyClients, c.ServiceProxyClients[0]) },
		"serve collision": func(c *LaunchConfig) {
			c.Serve = []int{ServiceProxyPort}
			c.Ingress = netip.MustParseAddr("172.17.0.1")
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := config
			changed.Protected = slices.Clone(config.Protected)
			changed.ServiceProxyClients = slices.Clone(config.ServiceProxyClients)
			mutate(&changed)
			if changed.Validate() == nil {
				t.Fatal("invalid service proxy client configuration accepted")
			}
		})
	}
}

func TestGatewayFrozenWildcardDoesNotReapplyCurrentSuffixCatalog(t *testing.T) {
	config := testLaunch(t)
	// The helper receives host-authenticated, immutable launch bytes. It has
	// neither the owner's key nor authority to re-approve them under a new PSL.
	config.Policy.Grants[0].Rule.To.Domain = "*.github.io"
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadLaunchConfig(bytes.NewReader(data))
	if err != nil || !loaded.Policy.Domain("project.github.io", 443).Allowed {
		t.Fatal("helper changed historical wildcard authority", err)
	}
	for _, bad := range []string{"*.GITHUB.IO", "*.*.github.io", "*.github.io..", "*.github.io/", "*.github.io\n"} {
		config.Policy.Grants[0].Rule.To.Domain = bad
		if config.Validate() == nil {
			t.Fatalf("frozen validation admitted malformed rule %q", bad)
		}
	}
}

func TestEnvoyHealthIsPrivateBoundedAndNotRedirectable(t *testing.T) {
	for _, tc := range []struct {
		status  int
		body    string
		advance bool
		allowed bool
	}{
		{200, "LIVE\n", false, true}, {200, "OTHER\n", false, false},
		{503, "LIVE\n", false, false}, {302, "LIVE\n", false, false},
		{200, strings.Repeat("x", 64), false, false}, {200, "LIVE\n", true, false},
	} {
		t.Run(fmt.Sprintf("%d_%d_%v", tc.status, len(tc.body), tc.advance), func(t *testing.T) {
			path := shortControlPath(t)
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			now := testBootNow()
			clock := testBootClock()
			// The handler communicates by the HTTP response; synchronization of
			// clock advancement is explicit rather than racing a shared fixture.
			clock.read = func() (BootInstant, error) { return now, nil }
			server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/ready" || r.Host != "gateway" {
					t.Error("health request widened its fixed target")
				}
				w.Header().Set("Location", "http://127.0.0.1:1/not-allowed")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})}
			defer server.Close()
			go func() { _ = server.Serve(listener) }()
			reads := 0
			clock.read = func() (BootInstant, error) {
				reads++
				if reads > 1 && tc.advance {
					return now.Add(2 * time.Second), nil
				}
				return now, nil
			}
			health := newEnvoyHealth(path, clock)
			defer health.transport.CloseIdleConnections()
			if err := health.check(context.Background()); (err == nil) != tc.allowed {
				t.Fatalf("health acceptance: %v", err)
			}
		})
	}
}

func envoyLine() string {
	return `{"flow_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","connection_id":"9007199254740993","phase":"TcpPeriodic","peer":"93.184.216.34:443","local":"172.18.0.2:40000","sent":"18446744073709551615","received":"0","duration_ms":"1000","connect_ms":"10","flags":"-","close_type":"Normal"}`
}

func TestEnvoyEvidencePreservesCountersAndBoundsLoss(t *testing.T) {
	events := NewEnvoyEvents(testBootClock())
	line := envoyLine() + "\n"
	for _, chunk := range []string{line[:13], line[13:]} {
		if n, err := events.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatal("observer backpressured forwarding")
		}
	}
	got, totals := events.Drain(1)
	if len(got) != 1 || *got[0].Sent != ^uint64(0) || *got[0].Received != 0 || totals.Lost != 0 || got[0].Sequence != 1 || !got[0].BootAt.Valid() {
		t.Fatalf("counter precision/evidence lost: %#v %#v", got, totals)
	}
	_, _ = events.Write(bytes.Repeat([]byte("x"), MaxEnvoyEventBytes*100))
	if len(events.pending) > MaxEnvoyEventBytes {
		t.Fatal("unbounded partial record")
	}
	_, _ = events.Write([]byte("\n" + line))
	got, totals = events.Drain(2)
	if len(got) != 1 || got[0].Sequence != 3 || totals.Malformed != 1 || totals.Lost != 1 {
		t.Fatal("oversized line prevented recovery or hid gap")
	}
	for range MaxGuardEvents + 3 {
		_, _ = events.Write([]byte(line))
	}
	got, totals = events.Drain(MaxGuardEvents * 2)
	if len(got) != MaxGuardEvents || totals.Lost != 4 {
		t.Fatal("full observer queue did not retain bounded loss")
	}
	_, _ = events.Write([]byte("partial"))
	events.Finish(nil)
	events.Finish(nil)
	_, totals = events.Drain(0)
	if !totals.Stopped || totals.Lost != 5 {
		t.Fatal("final partial frame hidden or counted twice")
	}
}

func TestEnvoyEvidencePreservesIncompleteDrainWithoutPartialTail(t *testing.T) {
	events := NewEnvoyEvents(testBootClock())
	_, _ = events.Write([]byte(envoyLine() + "\n"))
	events.Finish(exec.ErrWaitDelay)
	_, totals := events.Drain(1)
	if !totals.Stopped || !totals.UnknownLoss || totals.StopReason != "drain_incomplete" || totals.Lost != 0 {
		t.Fatal("forced pipe close became a clean source completion or invented loss count")
	}
}

func TestEnvoyEvidenceRecordsRealProcessDrainTimeout(t *testing.T) {
	input, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer release.Close()
	completion, completed, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer completion.Close()
	defer completed.Close()
	events := NewEnvoyEvents(testBootClock())
	ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
	defer cancel()
	// The leader exits but its descendant retains stdout until this test
	// releases fd3. This is a deterministic drain timeout, not a sleep race.
	command := exec.CommandContext(ctx, "/bin/sh", "-c", `(read -r value <&3; printf done >&4) & exit 0`)
	command.ExtraFiles = []*os.File{input, completed}
	command.Stdout, command.Stderr = events, io.Discard
	command.WaitDelay = 50 * time.Millisecond
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	_ = input.Close()
	_ = completed.Close()
	waitErr := command.Wait()
	events.Finish(waitErr)
	_ = release.Close()
	if err := completion.SetReadDeadline(time.Now().Add(wait.Deadline)); err != nil {
		t.Fatal(err)
	}
	finished, err := io.ReadAll(completion)
	if err != nil || string(finished) != "done" {
		t.Fatalf("drain fixture descendant did not exit: %q %v", finished, err)
	}
	_, totals := events.Drain(1)
	if !errors.Is(waitErr, exec.ErrWaitDelay) || !totals.UnknownLoss || totals.StopReason != "drain_incomplete" {
		t.Fatalf("real drain timeout hidden: %v %#v", waitErr, totals)
	}
}

func TestEnvoyEvidenceRefusesUnsafeAndAmbiguousValues(t *testing.T) {
	for _, bad := range []string{
		strings.Replace(envoyLine(), `"sent":"18446744073709551615"`, `"sent":9007199254740993`, 1),
		strings.Replace(envoyLine(), `"received":"0"`, `"received":"00"`, 1),
		strings.Replace(envoyLine(), "TcpPeriodic", "arbitrary console text", 1),
		strings.Replace(envoyLine(), "93.184.216.34:443", "http://private/path", 1),
		strings.Replace(envoyLine(), strings.Repeat("a", 32), "malicious\\\"field", 1),
		envoyLine() + "{}",
	} {
		if _, err := parseEnvoyEvent([]byte(bad)); err == nil {
			t.Fatal("invalid observation accepted")
		}
	}
	missing := strings.ReplaceAll(strings.ReplaceAll(envoyLine(), `"18446744073709551615"`, `"-"`), `"0"`, `"-"`)
	got, err := parseEnvoyEvent([]byte(missing))
	if err != nil || got.Sent != nil || got.Received != nil {
		t.Fatal("unknown meter became zero")
	}
}
