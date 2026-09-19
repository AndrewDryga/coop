package networkgateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

type serviceProxyTestConn struct {
	net.Conn
	peer netip.Addr
}

func (c serviceProxyTestConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: c.peer.AsSlice()}
}

func TestServiceProxyRoutesOnlyAnApprovedMatchingTLSName(t *testing.T) {
	fixture := startGuardFixture(t)
	serviceAddress := netip.MustParseAddr("172.31.0.16")
	fixture.guard.serviceProxyClients = []ServiceProxyClient{{Name: "web", Address: serviceAddress}}
	server, client := net.Pipe()
	server = serviceProxyTestConn{Conn: server, peer: serviceAddress}
	t.Cleanup(func() { _ = client.Close() })
	done := make(chan struct{})
	go func() {
		fixture.guard.serviceProxy(context.Background(), server, fixture.private.Addr().String())
		close(done)
	}()
	_ = client.SetDeadline(time.Now().Add(wait.Deadline))
	if _, err := io.WriteString(client, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := bufio.NewReader(client).ReadString('\n')
	if err != nil || response != "HTTP/1.1 200 Connection Established\r\n" {
		t.Fatalf("CONNECT response = %q, %v", response, err)
	}
	hello := clientHello(t, "api.example.com")
	if _, err := client.Write(hello); err != nil {
		t.Fatal(err)
	}
	_ = fixture.private.SetDeadline(time.Now().Add(wait.Deadline))
	private, err := fixture.private.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = private.Close() })
	header := make([]byte, 63)
	if _, err := io.ReadFull(private, header); err != nil {
		t.Fatal(err)
	}
	replayed := make([]byte, len(hello))
	if _, err := io.ReadFull(private, replayed); err != nil || !bytes.Equal(replayed, hello) {
		t.Fatalf("service proxy changed ClientHello: %v", err)
	}
	_ = private.Close()
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(wait.Deadline):
		t.Fatal("service proxy flow leaked")
	}

	deniedServer, deniedClient := net.Pipe()
	deniedServer = serviceProxyTestConn{Conn: deniedServer, peer: serviceAddress}
	defer deniedClient.Close()
	go fixture.guard.serviceProxy(context.Background(), deniedServer, fixture.private.Addr().String())
	_ = deniedClient.SetDeadline(time.Now().Add(wait.Deadline))
	if _, err := io.WriteString(deniedClient, "CONNECT forbidden.example.com:443 HTTP/1.1\r\nHost: forbidden.example.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err = bufio.NewReader(deniedClient).ReadString('\n')
	if err != nil || response != "HTTP/1.1 403 Forbidden\r\n" {
		t.Fatalf("denied CONNECT response = %q, %v", response, err)
	}
	var events []GuardEvent
	wait.For(t, "service traffic evidence", func() bool {
		batch, _ := fixture.guard.events.Drain(MaxGuardEvents)
		events = append(events, batch...)
		return slices.ContainsFunc(events, func(event GuardEvent) bool { return event.Kind == "flow_registered" }) &&
			slices.ContainsFunc(events, func(event GuardEvent) bool { return event.Kind == "tls_denied" })
	})
	for _, event := range events {
		if (event.Kind == "flow_registered" || event.Kind == "tls_denied") && event.Service != "web" {
			t.Fatalf("service event lost its source: %#v", event)
		}
	}
}

type guardFixture struct {
	guard      *Guard
	tls, dns   net.Listener
	udp        net.PacketConn
	private    *net.UnixListener
	done       <-chan error
	controller context.CancelFunc
	// port is the destination port the fixture's fake capture reports, which is
	// what an admitted flow must carry all the way into the PROXY header.
	port uint16
}

// captureTo installs the kernel record a redirect would have left on every
// accepted connection. Nothing redirects on a test host, so this is the only
// way to exercise the guard's port decisions; it is restored after the test.
func captureTo(t *testing.T, g *Guard, destination func(net.Conn) (netip.AddrPort, error)) {
	t.Helper()
	previous := *g.original.Load()
	g.setDestinationReader(destination)
	t.Cleanup(func() { g.setDestinationReader(previous) })
}

func startGuardFixture(t *testing.T) guardFixture {
	return startGuardClockFixture(t, testBootClock(), 60)
}

func startGuardClockFixture(t *testing.T, clock *BootClock, ttl uint32) guardFixture {
	return startGuardPolicyFixture(t, testPolicy(t), clock, ttl, 443)
}

func startGuardPolicyFixture(t *testing.T, policy egress.Snapshot, clock *BootClock, ttl uint32, port uint16) guardFixture {
	t.Helper()
	c := newTestController(t, policy, func(context.Context, string) error { return nil })
	c.clock, c.now, c.identity.Clock = clock, clock.instant, clock.Domain()
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil {
		t.Fatal(err)
	}
	control, stopController, _ := startControlFixture(t, c, func(*net.UnixConn) bool { return true })
	r, err := NewResolver(c.policy, nil, nil, clock, answerExchange(t, func(name string) []dnsmessage.Resource {
		return []dnsmessage.Resource{aRecord(name, "93.184.216.34", ttl)}
	}))
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGuard(c.policy, c.clock, r, control, NewGuardEvents(c.clock))
	if err != nil {
		t.Fatal(err)
	}
	captureTo(t, g, func(net.Conn) (netip.AddrPort, error) {
		return netip.AddrPortFrom(netip.MustParseAddr("93.184.216.34"), port), nil
	})
	listen := func() net.Listener {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		return listener
	}
	fixture := guardFixture{guard: g, tls: listen(), dns: listen(), controller: stopController, port: port}
	fixture.udp, err = net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.udp.Close() })
	fixture.private, err = net.ListenUnix("unix", &net.UnixAddr{Name: shortControlPath(t), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.private.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done, ready := make(chan error, 1), make(chan struct{})
	fixture.done = done
	go func() {
		done <- g.serve(ctx, fixture.tls, fixture.dns, fixture.udp, nil, fixture.private.Addr().String(), func() { close(ready) })
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(wait.Deadline):
			t.Error("guard fixture leaked")
		}
	})
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("guard startup: %v", err)
	case <-time.After(wait.Deadline):
		t.Fatal("guard did not start")
	}
	return fixture
}

func guardClient(t *testing.T, address string) *net.TCPConn {
	t.Helper()
	conn, err := net.Dial("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(wait.Deadline)); err != nil {
		t.Fatal(err)
	}
	return conn.(*net.TCPConn)
}

func guardPrivate(t *testing.T, fixture guardFixture, hello []byte) (*net.TCPConn, *net.UnixConn, string) {
	t.Helper()
	client := guardClient(t, fixture.tls.Addr().String())
	if err := writeAll(client, hello); err != nil {
		t.Fatal(err)
	}
	_ = fixture.private.SetDeadline(time.Now().Add(wait.Deadline))
	private, err := fixture.private.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = private.Close() })
	_ = private.SetDeadline(time.Now().Add(wait.Deadline))
	header := make([]byte, 63)
	if _, err := io.ReadFull(private, header); err != nil {
		t.Fatal(err)
	}
	flowID := string(header[31:])
	wanted, err := ProxyHeader(netip.AddrPortFrom(netip.MustParseAddr("93.184.216.34"), fixture.port), flowID)
	if err != nil || !bytes.Equal(header, wanted) {
		t.Fatal("private PROXY destination or flow ID differs from admitted peer")
	}
	replayed := make([]byte, len(hello))
	if _, err := io.ReadFull(private, replayed); err != nil || !bytes.Equal(replayed, hello) {
		t.Fatalf("ClientHello changed: %v", err)
	}
	return client, private, flowID
}

func TestGuardReplaysExactAdmissionAndPreservesStreamingHalfClose(t *testing.T) {
	fixture := startGuardFixture(t)
	client, private, flowID := guardPrivate(t, fixture, clientHello(t, "api.example.com"))
	payload := bytes.Repeat([]byte("opaque post-hello stream"), 1000)
	if err := writeAll(client, payload); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(private)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("forward stream/half-close: %v", err)
	}
	if err := writeAll(private, []byte("response after client half-close")); err != nil {
		t.Fatal(err)
	}
	_ = private.CloseWrite()
	got, err = io.ReadAll(client)
	if err != nil || string(got) != "response after client half-close" {
		t.Fatalf("response half-close: %q %v", got, err)
	}
	var events []GuardEvent
	wait.For(t, "closed private flow evidence", func() bool {
		batch, _ := fixture.guard.events.Drain(MaxGuardEvents)
		events = append(events, batch...)
		return len(events) >= 2
	})
	if len(events) != 2 || events[0].Kind != "flow_registered" || events[1].Kind != "private_flow_closed" || events[0].FlowID != flowID || events[1].FlowID != flowID || events[0].Service != "" {
		t.Fatalf("flow evidence: %#v", events)
	}
}

// shortAdmission shrinks the admission budget for one test. It must run before the fixture starts:
// the guard's goroutines are created after this write, and the fixture's cleanups stop them before
// the restore.
func shortAdmission(t *testing.T, budget time.Duration) {
	t.Helper()
	saved := admissionTimeout
	admissionTimeout = budget
	t.Cleanup(func() { admissionTimeout = saved })
}

// exchangeBothWays proves an admitted flow still carries bytes in each direction.
func exchangeBothWays(t *testing.T, client, private net.Conn) {
	t.Helper()
	for _, leg := range []struct {
		from, to net.Conn
		payload  string
	}{{client, private, "request after the admission budget"}, {private, client, "response after the admission budget"}} {
		if err := writeAll(leg.from, []byte(leg.payload)); err != nil {
			t.Fatalf("write after the admission budget: %v", err)
		}
		got := make([]byte, len(leg.payload))
		if _, err := io.ReadFull(leg.to, got); err != nil || string(got) != leg.payload {
			t.Fatalf("read after the admission budget = %q, %v", got, err)
		}
	}
}

// A guarded flow lives as long as its two ends keep it open; the admission budget bounds admission
// alone. Every guarded TLS flow used to close when that budget ran out — ten seconds in, mid-
// response — because the private leg's close was wired to the admission deadline.
func TestGuardFlowOutlivesTheAdmissionBudget(t *testing.T) {
	shortAdmission(t, 200*time.Millisecond)
	fixture := startGuardFixture(t)
	client, private, _ := guardPrivate(t, fixture, clientHello(t, "api.example.com"))
	time.Sleep(700 * time.Millisecond) // well past the budget
	exchangeBothWays(t, client, private)
}

// The budget still bounds admission: a client that never finishes its ClientHello is let go as soon
// as it runs out, not left holding a guard connection slot.
func TestGuardStalledAdmissionIsStillRefusedOnTime(t *testing.T) {
	shortAdmission(t, 200*time.Millisecond)
	fixture := startGuardFixture(t)
	client := guardClient(t, fixture.tls.Addr().String())
	_ = client.SetReadDeadline(time.Now().Add(wait.Deadline))
	started := time.Now()
	if n, err := client.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("a stalled admission read %d bytes, %v; want the guard to close it", n, err)
	}
	// Well under HelloTimeout (5s): only the admission budget can have closed it this soon.
	if waited := time.Since(started); waited > 2*time.Second {
		t.Fatalf("a stalled admission held the connection for %s", waited)
	}
}

// The service proxy path hands its flows to the same forwarding, and outlives its budget the same way.
func TestServiceProxyFlowOutlivesTheAdmissionBudget(t *testing.T) {
	shortAdmission(t, 200*time.Millisecond)
	fixture := startGuardFixture(t)
	serviceAddress := netip.MustParseAddr("172.31.0.16")
	fixture.guard.serviceProxyClients = []ServiceProxyClient{{Name: "web", Address: serviceAddress}}
	server, client := net.Pipe()
	server = serviceProxyTestConn{Conn: server, peer: serviceAddress}
	t.Cleanup(func() { _ = client.Close() })
	done := make(chan struct{})
	go func() {
		fixture.guard.serviceProxy(context.Background(), server, fixture.private.Addr().String())
		close(done)
	}()
	_ = client.SetDeadline(time.Now().Add(wait.Deadline))
	if _, err := io.WriteString(client, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if response, err := bufio.NewReader(client).ReadString('\n'); err != nil || response != "HTTP/1.1 200 Connection Established\r\n" {
		t.Fatalf("CONNECT response = %q, %v", response, err)
	}
	hello := clientHello(t, "api.example.com")
	if _, err := client.Write(hello); err != nil {
		t.Fatal(err)
	}
	_ = fixture.private.SetDeadline(time.Now().Add(wait.Deadline))
	private, err := fixture.private.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = private.Close() })
	_ = private.SetDeadline(time.Now().Add(wait.Deadline))
	preamble := make([]byte, 63+len(hello))
	if _, err := io.ReadFull(private, preamble); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond) // well past the budget
	exchangeBothWays(t, client, private)
	_ = private.Close()
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(wait.Deadline):
		t.Fatal("service proxy flow leaked after both ends closed")
	}
}

func TestGuardListenerOrControllerLossClosesExistingStream(t *testing.T) {
	for _, failure := range []string{"tls", "dns_tcp", "dns_udp", "controller"} {
		t.Run(failure, func(t *testing.T) {
			fixture := startGuardFixture(t)
			client, private, _ := guardPrivate(t, fixture, clientHello(t, "api.example.com"))
			switch failure {
			case "tls":
				_ = fixture.tls.Close()
			case "dns_tcp":
				_ = fixture.dns.Close()
			case "dns_udp":
				_ = fixture.udp.Close()
			case "controller":
				fixture.controller()
			}
			select {
			case err := <-fixture.done:
				if err == nil {
					t.Fatal("component loss reported success")
				}
			case <-time.After(wait.Deadline):
				t.Fatal("failed listener waited forever on its active flow")
			}
			for _, conn := range []net.Conn{client, private} {
				var buffer [1]byte
				if n, err := conn.Read(buffer[:]); n != 0 || err == nil {
					t.Fatal("component loss left streaming socket open")
				}
			}
		})
	}
}

func TestGuardRefusesBeforePrivateDial(t *testing.T) {
	fixture := startGuardFixture(t)
	for _, wire := range [][]byte{
		clientHello(t, "forbidden.example.com"), clientHello(t, ""),
		addExtension(t, clientHello(t, "api.example.com"), echExtension, nil),
		[]byte("GET / HTTP/1.1\r\n\r\n"),
	} {
		client := guardClient(t, fixture.tls.Addr().String())
		if err := writeAll(client, wire); err != nil {
			t.Fatal(err)
		}
		_ = client.CloseWrite()
		var buffer [1]byte
		if n, err := client.Read(buffer[:]); n != 0 || err == nil {
			t.Fatal("refused TLS wrote data")
		}
	}
	events, totals := fixture.guard.events.Drain(MaxGuardEvents)
	if totals.DeniedTLS != 4 || len(events) != 4 {
		t.Fatalf("missing refusal accounting: %#v %#v", totals, events)
	}
	queries, _ := fixture.guard.resolver.MaintenanceCounts()
	if queries != 0 {
		t.Fatal("refused hello leaked resolver traffic")
	}
	for _, event := range events {
		if event.Kind != "tls_denied" || event.FlowID != "" {
			t.Fatal("refused hello registered a private flow")
		}
	}
}

// The upstream port comes from the kernel's redirect record and from nowhere
// else: a name granted on 8443 is routed to 8443, and the SAME name on a port
// the policy does not grant is refused even though the name is approved.
func TestGuardRoutesTheCapturedPortAndRefusesAnUngrantedOne(t *testing.T) {
	policy := transportPolicy(t, egress.Rule{To: egress.Destination{Domain: "api.example.com"}, Protocol: "tls", Ports: []int{853, 8443}})
	fixture := startGuardPolicyFixture(t, policy, testBootClock(), 60, 8443)
	client, _, _ := guardPrivate(t, fixture, clientHello(t, "api.example.com"))
	_ = client.Close()
	events, _ := fixture.guard.events.Drain(MaxGuardEvents)
	if len(events) == 0 || events[0].Kind != "flow_registered" || events[0].Port != 8443 {
		t.Fatalf("the admitted flow lost the captured port: %#v", events)
	}
	// Same guard, same name, a port this policy does not grant for it.
	captureTo(t, fixture.guard, func(net.Conn) (netip.AddrPort, error) {
		return netip.AddrPortFrom(netip.MustParseAddr("93.184.216.34"), 443), nil
	})
	refused := guardClient(t, fixture.tls.Addr().String())
	if err := writeAll(refused, clientHello(t, "api.example.com")); err != nil {
		t.Fatal(err)
	}
	_ = refused.CloseWrite()
	var buffer [1]byte
	if n, err := refused.Read(buffer[:]); n != 0 || err == nil {
		t.Fatal("an ungranted port was forwarded")
	}
	wait.For(t, "refusal evidence for the ungranted port", func() bool {
		batch, _ := fixture.guard.events.Drain(MaxGuardEvents)
		events = append(events, batch...)
		return slices.ContainsFunc(events, func(e GuardEvent) bool {
			return e.Kind == "tls_denied" && e.Name == "api.example.com" && e.Port == 443 && e.Reason == "unapproved_name"
		})
	})
}

// spec §5: a direct dial cannot select an upstream port. A connection nobody
// redirected reports THIS listener as its original destination, so the guard
// refuses it and counts it instead of falling back to a port of its own.
func TestGuardRefusesADirectDialToItsListener(t *testing.T) {
	fixture := startGuardFixture(t)
	captureTo(t, fixture.guard, func(conn net.Conn) (netip.AddrPort, error) {
		return conn.LocalAddr().(*net.TCPAddr).AddrPort(), nil
	})
	client := guardClient(t, fixture.tls.Addr().String())
	if err := writeAll(client, clientHello(t, "api.example.com")); err != nil {
		t.Fatal(err)
	}
	_ = client.CloseWrite()
	var buffer [1]byte
	if n, err := client.Read(buffer[:]); n != 0 || err == nil {
		t.Fatal("a direct dial to the guard was forwarded")
	}
	events, totals := fixture.guard.events.Drain(MaxGuardEvents)
	listener := uint16(netip.MustParseAddrPort(fixture.tls.Addr().String()).Port())
	if totals.DeniedTLS != 1 || len(events) != 1 || events[0].Kind != "tls_denied" ||
		events[0].Reason != "tls_direct_dial_refused" || events[0].Port != int(listener) || events[0].Name != "" {
		t.Fatalf("direct dial accounting: %#v %#v", totals, events)
	}
	queries, _ := fixture.guard.resolver.MaintenanceCounts()
	if queries != 0 {
		t.Fatal("a direct dial reached the resolver")
	}
}

// An unreadable destination is not a reason to guess one.
func TestGuardRefusesAConnectionWithNoKernelDestination(t *testing.T) {
	fixture := startGuardFixture(t)
	captureTo(t, fixture.guard, func(net.Conn) (netip.AddrPort, error) {
		return netip.AddrPort{}, Failure("gateway_destination_unknown")
	})
	client := guardClient(t, fixture.tls.Addr().String())
	if err := writeAll(client, clientHello(t, "api.example.com")); err != nil {
		t.Fatal(err)
	}
	_ = client.CloseWrite()
	var buffer [1]byte
	if n, err := client.Read(buffer[:]); n != 0 || err == nil {
		t.Fatal("a connection with no kernel record was forwarded")
	}
	events, _ := fixture.guard.events.Drain(MaxGuardEvents)
	if len(events) != 1 || events[0].Kind != "tls_denied" || events[0].Reason != "gateway_destination_unknown" || events[0].Port != 0 {
		t.Fatalf("unknown destination accounting: %#v", events)
	}
}

func TestGuardServesBoundedDNSOverTCPAndUDP(t *testing.T) {
	fixture := startGuardFixture(t)
	for _, transport := range []string{"tcp4", "udp4"} {
		for _, name := range []string{"api.example.com", "denied.example.com"} {
			query, _ := makeQuery(name)
			wire, _ := query.Pack()
			address := fixture.dns.Addr().String()
			if transport == "udp4" {
				address = fixture.udp.LocalAddr().String()
			} else {
				wire = append([]byte{byte(len(wire) >> 8), byte(len(wire))}, wire...)
			}
			conn, err := net.Dial(transport, address)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			_ = conn.SetDeadline(time.Now().Add(wait.Deadline))
			if err := writeAll(conn, wire); err != nil {
				t.Fatal(err)
			}
			reply := make([]byte, MaxDNSMessage)
			if transport == "tcp4" {
				var prefix [2]byte
				if _, err := io.ReadFull(conn, prefix[:]); err != nil {
					t.Fatal(err)
				}
				reply = reply[:int(binary.BigEndian.Uint16(prefix[:]))]
				_, err = io.ReadFull(conn, reply)
			} else {
				var n int
				n, err = conn.Read(reply)
				reply = reply[:n]
			}
			if err != nil {
				t.Fatal(err)
			}
			var answer dnsmessage.Message
			if err := answer.Unpack(reply); err != nil || answer.ID != query.ID {
				t.Fatalf("DNS response: %v", err)
			}
			if name == "api.example.com" && len(answer.Answers) != 1 || name != "api.example.com" && answer.RCode != dnsmessage.RCodeRefused {
				t.Fatalf("DNS policy not enforced over %s: %#v", transport, answer)
			}
		}
	}
	bomb := make([]byte, 12)
	binary.BigEndian.PutUint16(bomb[4:6], 65535)
	if reply := fixture.guard.dnsAnswer(context.Background(), bomb); reply != nil {
		t.Fatal("header bomb accepted by diagnostic parser")
	}
	events, totals := fixture.guard.events.Drain(MaxGuardEvents)
	if totals.DeniedDNS != 3 || len(events) != 3 || events[2].Name != "" || events[2].Reason != "dns_query_invalid" {
		t.Fatalf("DNS diagnostics: %#v %#v", totals, events)
	}
}

func TestGuardEventQueueBoundsSequencesAndReportsLoss(t *testing.T) {
	queue := NewGuardEvents(testBootClock())
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range MaxGuardEvents {
				queue.emit(GuardEvent{Kind: "tls_denied"})
			}
		})
	}
	workers.Wait()
	events, totals := queue.Drain(MaxGuardEvents * 8)
	if len(events) != MaxGuardEvents || totals.Sequence != 8*MaxGuardEvents || totals.Lost != 7*MaxGuardEvents || totals.DeniedTLS != 8*MaxGuardEvents {
		t.Fatalf("unbounded/lost accounting: %d %#v", len(events), totals)
	}
	for i, event := range events {
		if event.Sequence != uint64(i+1) {
			t.Fatal("concurrent event order differed from sequence")
		}
	}
	queue.totals.Sequence = ^uint64(0)
	queue.emit(GuardEvent{Kind: "tls_denied"})
	events, totals = queue.Drain(1)
	if len(events) != 0 || !totals.Saturated || totals.Sequence != ^uint64(0) {
		t.Fatal("sequence saturation reused an identity")
	}
}

func TestGuardRefusesResolverFromDifferentClockDomain(t *testing.T) {
	fixture := startGuardFixture(t)
	r := newTestResolver(t, func(context.Context, []byte) ([]byte, error) { return nil, io.EOF })
	r.domain.TimeNamespace = "67890"
	g := fixture.guard
	if _, err := NewGuard(g.policy, g.clock, r, g.controller, NewGuardEvents(g.clock)); err == nil {
		t.Fatal("guard accepted resolver TTLs from another time namespace")
	}
}

func TestGuardRefreshesShortTTLWithinKernelAdmissionMargin(t *testing.T) {
	var instant atomic.Int64
	instant.Store(int64(testBootNow()))
	clock := testBootClock()
	clock.read = func() (BootInstant, error) { return BootInstant(instant.Load()), nil }
	fixture := startGuardClockFixture(t, clock, 1)
	if _, err := fixture.guard.resolver.Resolve(context.Background(), "api.example.com"); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		instant.Add(int64(800 * time.Millisecond))
		client, private, _ := guardPrivate(t, fixture, clientHello(t, "api.example.com"))
		_ = client.Close()
		_ = private.Close()
	}
	queries, _ := fixture.guard.resolver.MaintenanceCounts()
	if queries != 5 {
		t.Fatalf("short-TTL cache became outage or query storm: %d", queries)
	}
}

type replayClockConn struct {
	wireConn
	onWrite  func()
	deadline time.Time
}

func (c *replayClockConn) Write(p []byte) (int, error) {
	c.onWrite()
	return c.wireConn.Write(p)
}

func (c *replayClockConn) SetWriteDeadline(value time.Time) error {
	c.deadline = value
	return nil
}

func TestGuardReplayRechecksBootClockAfterStalledWrite(t *testing.T) {
	for _, advanceAfter := range []int{1, 2} {
		now := testBootNow()
		until := now.Add(time.Second)
		clock := testBootClock()
		clock.read = func() (BootInstant, error) { return now, nil }
		g := Guard{clock: clock}
		writes := 0
		conn := &replayClockConn{onWrite: func() {
			writes++
			if writes == advanceAfter {
				now = until
			}
		}}
		ctx, cancel := context.WithTimeout(context.Background(), GuardAdmissionTimeout)
		started := time.Now()
		err := g.replay(ctx, conn, []byte("private header"), []byte("hello"), until)
		cancel()
		if err != Failure("dns_ttl_expired") || conn.deadline.IsZero() || conn.deadline.After(started.Add(2*time.Second)) || writes != advanceAfter {
			t.Fatalf("stalled replay reached streaming or retained 10s deadline: %v %v %d", err, conn.deadline, writes)
		}
	}
}

// A refused sidecar lookup is the one destination a human most wants named: the
// label IS the question. Only the resolver's own bounded label grammar reaches
// the summary, so a hostile question stays withheld rather than printed back.
func TestGuardRefusedSingleLabelQueryIsNamedByItsLabel(t *testing.T) {
	fixture := startGuardFixture(t)
	cases := []struct{ question, name string }{
		{"other", "other"}, {"OTHER", "other"}, {"ev\x07il", ""}, {"denied.example.com", "denied.example.com"},
	}
	for _, c := range cases {
		query, err := makeQuery(c.question)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := query.Pack()
		if err != nil {
			t.Fatal(err)
		}
		reply := fixture.guard.dnsAnswer(context.Background(), wire)
		var answer dnsmessage.Message
		if err := answer.Unpack(reply); err != nil || answer.RCode != dnsmessage.RCodeRefused {
			t.Fatalf("%q was not refused: %v %#v", c.question, err, answer)
		}
	}
	events, totals := fixture.guard.events.Drain(MaxGuardEvents)
	if int(totals.DeniedDNS) != len(cases) || len(events) != len(cases) {
		t.Fatalf("refusal accounting: %#v %#v", totals, events)
	}
	for i, event := range events {
		if event.Kind != "dns_denied" || event.Name != cases[i].name {
			t.Fatalf("query %q recorded as %q (%s)", cases[i].question, event.Name, event.Kind)
		}
	}
}

// Contention is waited out only within the flow's admission budget: a controller that stays busy
// fails the flow with that reason instead of holding it forever.
func TestLeaseContentionEndsWithTheAdmissionBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var cursor atomic.Uint64
	resolution := Resolution{Name: "api.example.com", Addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}, Expires: testBootNow().Add(time.Minute)}
	asks := 0
	_, _, _, err := admitLease(ctx, nil, &cursor, resolution, 443, func(context.Context, Lease) (BootInstant, error) {
		asks++
		return 0, Failure("gateway_lease_capacity")
	})
	if err != Failure("gateway_lease_capacity") || asks < 2 {
		t.Fatalf("a busy controller ended with %v after %d asks", err, asks)
	}
}
