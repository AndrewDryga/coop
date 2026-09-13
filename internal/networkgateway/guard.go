package networkgateway

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

const (
	MaxGuardFlows         = 512
	MaxDNSConnections     = 32
	GuardAdmissionTimeout = 10 * time.Second
	GuardTLSAddress       = "127.0.0.1:15443"
	GuardDNSAddress       = "127.0.0.1:15353"
	ServiceProxyPort      = 15444
	ServiceProxyAddress   = "0.0.0.0:15444"
	MaxServiceProxyHeader = 16 << 10
	EnvoyDataSocket       = "/private/data.sock"
)

// destinationReader is the kernel's record of where a redirected connection
// was going. A guard holds its own reader so a test can drive the port
// decisions without a redirecting kernel, swapping it while the guard serves
// without a data race; production guards only ever read the kernel.
type destinationReader func(net.Conn) (netip.AddrPort, error)

// Guard owns only capless data-plane work. Policy and clocks are frozen before
// construction. Envoy process supervision and host resource ownership are above
// this server; no agent-facing handler can create privileges or change routing.
type Guard struct {
	policy              egress.Snapshot
	clock               *BootClock
	resolver            *Resolver
	controller          ControllerClient
	events              *GuardEvents
	peerCursor          atomic.Uint64
	original            atomic.Pointer[destinationReader]
	serviceProxyClients []netip.Addr
}

func (g *Guard) setDestinationReader(read destinationReader) { g.original.Store(&read) }

func NewGuard(policy egress.Snapshot, clock *BootClock, resolver *Resolver, controller ControllerClient, events *GuardEvents) (*Guard, error) {
	if err := policy.RequireSupported(); err != nil {
		return nil, err
	}
	if resolver == nil || events == nil || events.clock.Domain() != clock.Domain() || !clock.instant().Valid() || clock.Domain() != controller.Identity.Clock ||
		controller.Clock.Domain() != clock.Domain() || resolver.domain != clock.Domain() || controller.Identity.PolicyFingerprint != policy.Fingerprint || resolver.policy.Fingerprint != policy.Fingerprint {
		return nil, Failure("gateway_configuration_invalid")
	}
	g := &Guard{policy: policy.Clone(), clock: clock, resolver: resolver, controller: controller, events: events}
	g.setDestinationReader(readOriginalDestination)
	return g, nil
}

// boundary is the socket-attribution view of this guard's frozen authority:
// which addresses are permanently denied, and which raw destinations a grant
// lets the agent dial without a proxied leg to correlate.
func (g *Guard) boundary() boundary {
	return boundary{protected: g.resolver.protected, policy: g.policy, tlsPorts: g.policy.TLSPorts(), serviceProxyClients: g.serviceProxyClients}
}

func (g *Guard) Serve(ctx context.Context, ready func()) error {
	tlsListener, err := net.Listen("tcp4", GuardTLSAddress)
	if err != nil {
		return Failure("gateway_listener_unavailable")
	}
	defer tlsListener.Close()
	dnsListener, err := net.Listen("tcp4", GuardDNSAddress)
	if err != nil {
		return Failure("gateway_listener_unavailable")
	}
	defer dnsListener.Close()
	dnsPacket, err := net.ListenPacket("udp4", GuardDNSAddress)
	if err != nil {
		return Failure("gateway_listener_unavailable")
	}
	defer dnsPacket.Close()
	var serviceProxy net.Listener
	if len(g.serviceProxyClients) != 0 {
		serviceProxy, err = net.Listen("tcp4", ServiceProxyAddress)
		if err != nil {
			return Failure("gateway_listener_unavailable")
		}
		defer serviceProxy.Close()
	}
	return g.serve(ctx, tlsListener, dnsListener, dnsPacket, serviceProxy, EnvoyDataSocket, ready)
}

func (g *Guard) serve(ctx context.Context, tlsListener, dnsListener net.Listener, dnsPacket net.PacketConn, serviceProxy net.Listener, dataSocket string, ready func()) error {
	if err := g.controller.Ready(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	closeServiceProxy := func() {
		if serviceProxy != nil {
			_ = serviceProxy.Close()
		}
	}
	stop := context.AfterFunc(ctx, func() { _ = tlsListener.Close(); _ = dnsListener.Close(); _ = dnsPacket.Close(); closeServiceProxy() })
	var workers sync.WaitGroup
	defer func() {
		cancel()
		_ = tlsListener.Close()
		_ = dnsListener.Close()
		_ = dnsPacket.Close()
		closeServiceProxy()
		workers.Wait()
		stop()
	}()
	failed := make(chan error, 5)
	workers.Go(func() { failed <- g.controller.Heartbeat(ctx) })
	workers.Go(func() {
		failed <- g.accept(ctx, tlsListener, MaxGuardFlows, func(ctx context.Context, conn net.Conn) { g.forward(ctx, conn, dataSocket) })
	})
	workers.Go(func() { failed <- g.accept(ctx, dnsListener, MaxDNSConnections, g.dnsTCP) })
	workers.Go(func() { failed <- g.dnsUDP(ctx, dnsPacket) })
	if serviceProxy != nil {
		workers.Go(func() {
			failed <- g.accept(ctx, serviceProxy, MaxGuardFlows, func(ctx context.Context, conn net.Conn) { g.serviceProxy(ctx, conn, dataSocket) })
		})
	}
	if ready != nil {
		ready()
	}
	select {
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	case err := <-failed:
		cancel()
		return err
	}
}

func (g *Guard) accept(ctx context.Context, listener net.Listener, limit int, serve func(context.Context, net.Conn)) error {
	ctx, cancel := context.WithCancel(ctx)
	slots := make(chan struct{}, limit)
	var workers sync.WaitGroup
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer func() { cancel(); _ = listener.Close(); workers.Wait(); stop() }()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return Failure("gateway_listener_stopped")
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = conn.Close()
			g.events.emit(GuardEvent{Kind: "admission_failed", Reason: "gateway_connection_capacity"})
			continue
		}
		workers.Go(func() {
			defer func() { <-slots; _ = conn.Close() }()
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			defer stop()
			serve(ctx, conn)
		})
	}
}

// destination is where the kernel recorded this connection was going before the
// capture chain redirected it. A connection whose original destination is this
// listener was dialed straight at the guard: nothing redirected it, so no port
// was ever declared, and a client must not get to pick one. Both refusals carry
// the observed port so the refusal reads as an attempt, not as an absence.
func (g *Guard) destination(client net.Conn) (netip.AddrPort, error) {
	local, ok := client.LocalAddr().(*net.TCPAddr)
	original, err := (*g.original.Load())(client)
	if !ok || err != nil || !original.Addr().Is4() {
		return netip.AddrPort{}, Failure("gateway_destination_unknown")
	}
	if original == netip.AddrPortFrom(local.AddrPort().Addr().Unmap(), local.AddrPort().Port()) {
		return original, Failure("tls_direct_dial_refused")
	}
	return original, nil
}

func (g *Guard) forward(ctx context.Context, client net.Conn, dataSocket string) {
	admission, cancel := context.WithTimeout(ctx, GuardAdmissionTimeout)
	defer cancel()
	destination, err := g.destination(client)
	if err != nil {
		g.events.emit(GuardEvent{Kind: "tls_denied", Port: int(destination.Port()), Reason: safeReason(err)})
		return
	}
	port := int(destination.Port())
	hello, err := Inspect(admission, client, g.policy, port)
	if err != nil {
		g.events.emit(GuardEvent{Kind: "tls_denied", Name: hello.Name, Port: port, Reason: safeReason(err)})
		return
	}
	g.forwardTLS(admission, client, dataSocket, port, hello)
}

type prefixedConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixedConn) Read(data []byte) (int, error) {
	if len(c.prefix) != 0 {
		n := copy(data, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(data)
}

func (g *Guard) serviceProxy(ctx context.Context, client net.Conn, dataSocket string) {
	admission, cancel := context.WithTimeout(ctx, GuardAdmissionTimeout)
	defer cancel()
	deadline, _ := admission.Deadline()
	_ = client.SetReadDeadline(deadline)
	limited := &io.LimitedReader{R: client, N: MaxServiceProxyHeader + 1}
	reader := bufio.NewReaderSize(limited, 4096)
	request, err := http.ReadRequest(reader)
	if err != nil || request.Method != http.MethodConnect || request.ContentLength > 0 || len(request.TransferEncoding) != 0 {
		g.events.emit(GuardEvent{Kind: "tls_denied", Reason: "tls_proxy_request_invalid"})
		proxyStatus(client, "400 Bad Request")
		return
	}
	_ = request.Body.Close()
	host, portText, err := net.SplitHostPort(request.Host)
	port64, portErr := strconv.ParseUint(portText, 10, 16)
	name, nameErr := egress.NormalizeDomain(host, false)
	port := int(port64)
	decision := g.policy.Domain(name, port)
	if err != nil || portErr != nil || nameErr != nil || !decision.Allowed {
		reason := "unapproved_name"
		if err != nil || portErr != nil || nameErr != nil {
			reason = "tls_proxy_request_invalid"
		}
		g.events.emit(GuardEvent{Kind: "tls_denied", Name: name, Port: port, Reason: reason})
		proxyStatus(client, "403 Forbidden")
		return
	}
	pending, _ := reader.Peek(reader.Buffered())
	pending = append([]byte(nil), pending...)
	_ = client.SetReadDeadline(time.Time{})
	if !proxyStatus(client, "200 Connection Established") {
		return
	}
	stream := &prefixedConn{Conn: client, prefix: pending}
	hello, err := Inspect(admission, stream, g.policy, port)
	if err != nil || hello.Name != name || hello.RuleID != decision.RuleID {
		reason := safeReason(err)
		if err == nil {
			reason = "tls_name_mismatch"
		}
		g.events.emit(GuardEvent{Kind: "tls_denied", Name: hello.Name, Port: port, Reason: reason})
		return
	}
	g.forwardTLS(admission, stream, dataSocket, port, hello)
}

func proxyStatus(conn net.Conn, status string) bool {
	if err := conn.SetWriteDeadline(time.Now().Add(GuardAdmissionTimeout)); err != nil {
		return false
	}
	err := writeAll(conn, []byte("HTTP/1.1 "+status+"\r\nContent-Length: 0\r\n\r\n"))
	_ = conn.SetWriteDeadline(time.Time{})
	return err == nil
}

func (g *Guard) forwardTLS(ctx context.Context, client net.Conn, dataSocket string, port int, hello Hello) {
	admission, cancel := context.WithTimeout(ctx, GuardAdmissionTimeout)
	defer cancel()
	resolution, err := g.resolver.Resolve(admission, hello.Name)
	if err != nil {
		g.events.emit(GuardEvent{Kind: "admission_failed", Name: hello.Name, Reason: safeReason(err)})
		return
	}
	if len(resolution.Addresses) == 0 {
		g.events.emit(GuardEvent{Kind: "admission_failed", Name: hello.Name, Reason: "dns_no_address"})
		return
	}
	peer := resolution.Addresses[(g.peerCursor.Add(1)-1)%uint64(len(resolution.Addresses))]
	var until BootInstant
	refreshed := false
	for {
		until, err = g.controller.Admit(admission, Lease{Name: resolution.Name, Peer: peer, Port: port, Expires: resolution.Expires})
		if err == Failure("dns_ttl_expired") && !refreshed {
			// The controller reserves a kernel-commit margin before DNS expiry.
			// Do not turn that safe margin into a recurring short-TTL outage.
			refreshed = true
			resolution, err = g.resolver.refresh(admission, resolution)
			if err != nil || len(resolution.Addresses) == 0 {
				if err == nil {
					err = Failure("dns_no_address")
				}
				break
			}
			peer = resolution.Addresses[(g.peerCursor.Add(1)-1)%uint64(len(resolution.Addresses))]
			continue
		}
		if err != Failure("gateway_lease_capacity") {
			break
		}
		// Bounded contention retry belongs to this flow, not the heartbeat.
		select {
		case <-admission.Done():
			err = Failure("gateway_lease_capacity")
		case <-time.After(10 * time.Millisecond):
			continue
		}
		break
	}
	if err != nil {
		g.events.emit(GuardEvent{Kind: "admission_failed", Name: hello.Name, Reason: safeReason(err)})
		return
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		g.events.emit(GuardEvent{Kind: "admission_failed", Reason: "gateway_unavailable"})
		return
	}
	flowID := hex.EncodeToString(random[:])
	header, err := ProxyHeader(netip.AddrPortFrom(peer, uint16(port)), flowID)
	if err != nil {
		g.events.emit(GuardEvent{Kind: "admission_failed", Reason: safeReason(err)})
		return
	}
	private, err := (&net.Dialer{}).DialContext(admission, "unix", dataSocket)
	if err != nil {
		g.events.emit(GuardEvent{Kind: "admission_failed", Name: hello.Name, Reason: "gateway_unavailable"})
		return
	}
	defer private.Close()
	stop := context.AfterFunc(ctx, func() { _ = private.Close() })
	defer stop()
	g.events.emit(GuardEvent{Kind: "flow_registered", FlowID: flowID, Name: hello.Name, RuleID: hello.RuleID, Peer: peer, Port: port})
	defer g.events.emit(GuardEvent{Kind: "private_flow_closed", FlowID: flowID})
	if err := g.replay(admission, private, header, hello.Bytes, minTime(until, resolution.Expires)); err != nil {
		g.events.emit(GuardEvent{Kind: "admission_failed", FlowID: flowID, Name: hello.Name, Reason: safeReason(err)})
		return
	}
	hello.Bytes = nil
	cancel() // admission deadline must not shorten a legitimate streaming flow
	pipeBoth(client, private)
}

func (g *Guard) replay(admission context.Context, private net.Conn, header, hello []byte, until BootInstant) error {
	deadline, _ := admission.Deadline()
	now := g.clock.instant()
	if !now.Before(until) || admission.Err() != nil {
		return Failure("dns_ttl_expired")
	}
	if replayDeadline := time.Now().Add(until.Sub(now)); deadline.IsZero() || replayDeadline.Before(deadline) {
		deadline = replayDeadline
	}
	if err := private.SetWriteDeadline(deadline); err != nil {
		return Failure("gateway_unavailable")
	}
	if err := writeAll(private, header); err != nil {
		return Failure("gateway_unavailable")
	}
	if !g.clock.instant().Before(until) {
		return Failure("dns_ttl_expired")
	}
	if err := writeAll(private, hello); err != nil {
		return Failure("gateway_unavailable")
	}
	if !g.clock.instant().Before(until) || admission.Err() != nil {
		return Failure("dns_ttl_expired")
	}
	if err := private.SetWriteDeadline(time.Time{}); err != nil {
		return Failure("gateway_unavailable")
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func pipeBoth(a, b net.Conn) {
	var workers sync.WaitGroup
	copyHalf := func(destination, source net.Conn) {
		_, err := io.Copy(destination, source)
		if err != nil {
			_ = a.Close()
			_ = b.Close()
			return
		}
		if half, ok := destination.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		} else {
			_ = destination.Close()
		}
	}
	workers.Go(func() { copyHalf(a, b) })
	workers.Go(func() { copyHalf(b, a) })
	workers.Wait()
}

func (g *Guard) dnsTCP(ctx context.Context, conn net.Conn) {
	for range 32 {
		if err := conn.SetDeadline(time.Now().Add(DNSQueryTimeout)); err != nil {
			return
		}
		var prefix [2]byte
		if _, err := io.ReadFull(conn, prefix[:]); err != nil {
			return
		}
		length := int(binary.BigEndian.Uint16(prefix[:]))
		if length < 12 || length > MaxDNSMessage {
			g.events.emit(GuardEvent{Kind: "dns_denied", Reason: "dns_query_invalid"})
			return
		}
		query := make([]byte, length)
		if _, err := io.ReadFull(conn, query); err != nil {
			return
		}
		reply := g.dnsAnswer(ctx, query)
		if len(reply) == 0 {
			return
		}
		binary.BigEndian.PutUint16(prefix[:], uint16(len(reply)))
		if writeAll(conn, prefix[:]) != nil || writeAll(conn, reply) != nil {
			return
		}
	}
}

func (g *Guard) dnsUDP(ctx context.Context, conn net.PacketConn) error {
	ctx, cancel := context.WithCancel(ctx)
	slots := make(chan struct{}, MaxDNSInFlight)
	var workers sync.WaitGroup
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer func() { cancel(); _ = conn.Close(); workers.Wait(); stop() }()
	for {
		buffer := make([]byte, MaxDNSMessage+1)
		n, peer, err := conn.ReadFrom(buffer)
		if err != nil {
			return Failure("gateway_listener_stopped")
		}
		if n > MaxDNSMessage {
			g.events.emit(GuardEvent{Kind: "dns_denied", Reason: "dns_query_invalid"})
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			g.events.emit(GuardEvent{Kind: "admission_failed", Reason: "dns_capacity_exceeded"})
			continue
		}
		workers.Go(func() {
			defer func() { <-slots }()
			reply := g.dnsAnswer(ctx, buffer[:n])
			if len(reply) > 0 {
				_, _ = conn.WriteTo(reply, peer)
			}
		})
	}
}

func (g *Guard) dnsAnswer(ctx context.Context, query []byte) []byte {
	reply, reason := g.resolver.Answer(ctx, query)
	if reason != "" {
		// Only normalized question metadata is retained, never raw DNS bytes. A
		// single label is not a domain, but it is the whole point of a refused
		// sidecar lookup: record it so the summary names it instead of withholding.
		name := ""
		if message, err := parseQuery(query); err == nil {
			question := message.Questions[0].Name.String()
			if name, _ = egress.NormalizeDomain(question, false); name == "" {
				name, _ = egress.NormalizeLabel(question)
			}
		}
		kind := "admission_failed"
		if reason == "unapproved_name" || reason == "dns_type_unsupported" || reason == "dns_name_invalid" || reason == "dns_query_invalid" {
			kind = "dns_denied"
		}
		g.events.emit(GuardEvent{Kind: kind, Name: name, Reason: reason})
	}
	return reply
}

func safeReason(err error) string {
	var reason Failure
	if errors.As(err, &reason) {
		return string(reason)
	}
	return "gateway_unavailable"
}
