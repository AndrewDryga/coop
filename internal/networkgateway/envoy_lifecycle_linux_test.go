//go:build linux

package networkgateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

type fixtureMeterListener struct {
	net.Listener
	sent, received atomic.Uint64
}

func (l *fixtureMeterListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &fixtureMeterConn{Conn: conn, owner: l}, nil
}

type fixtureMeterConn struct {
	net.Conn
	owner *fixtureMeterListener
}

func (c *fixtureMeterConn) Read(data []byte) (int, error) {
	n, err := c.Conn.Read(data)
	c.owner.received.Add(uint64(n))
	return n, err
}
func (c *fixtureMeterConn) Write(data []byte) (int, error) {
	n, err := c.Conn.Write(data)
	c.owner.sent.Add(uint64(n))
	return n, err
}

// Runs inside the exact helper image with network=none, cap-drop=ALL, private
// tmpfs and no credentials. Loopback here is a source-accounting fixture, not an
// allowed production destination or proof of the controller security boundary.
func TestPinnedEnvoyLifecycleAndAsymmetricByteAccounting(t *testing.T) {
	if os.Getenv("COOP_GATEWAY_IMAGE_TESTS") != "1" {
		t.Skip("opt-in isolated helper-image source qualification")
	}
	clock, err := OpenBootClock()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
	defer cancel()
	events := NewEnvoyEvents(clock)
	var diagnostics bytes.Buffer
	cmd := exec.CommandContext(ctx, "envoy", "--disable-hot-restart", "--concurrency", "1", "--log-level", "error", "--file-flush-interval-msec", "100", "--file-flush-min-size-kb", "1", "--config-yaml", EnvoyBootstrap)
	cmd.Stdout, cmd.Stderr = events, &diagnostics
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		err := cmd.Wait()
		events.Finish(err)
		if err != nil {
			t.Errorf("fixture Envoy exit: %v %s", err, diagnostics.String())
		}
	}()
	health := newEnvoyHealth(EnvoyAdminSocket, clock)
	defer health.transport.CloseIdleConnections()
	wait.For(t, "fixture Envoy ready", func() bool { return health.check(ctx) == nil })

	requestBody, responseBody := bytes.Repeat([]byte("a"), 31*1024+7), bytes.Repeat([]byte("b"), 257*1024+13)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil || !bytes.Equal(body, requestBody) {
			t.Error("asymmetric request fixture changed")
		}
		_, _ = w.Write(responseBody)
	}))
	_ = server.Listener.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:443")
	if err != nil {
		t.Fatal(err)
	}
	meter := &fixtureMeterListener{Listener: listener}
	server.Listener = meter
	server.StartTLS()
	defer server.Close()
	cert := server.Certificate()
	if len(cert.DNSNames) == 0 {
		t.Fatal("TLS fixture certificate lacks name")
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)

	dial := func(flow string) *tls.Conn {
		t.Helper()
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", EnvoyDataSocket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if err := conn.SetDeadline(time.Now().Add(wait.Deadline)); err != nil {
			t.Fatal(err)
		}
		// Envoy dials the address:port the header names, so it must be this fixture server's own.
		upstream := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(listener.Addr().(*net.TCPAddr).Port))
		header, err := ProxyHeader(upstream, flow)
		if err != nil || writeAll(conn, header) != nil {
			t.Fatal("private fixture header failed")
		}
		return tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: cert.DNSNames[0], MinVersion: tls.VersionTLS12})
	}
	awaitEnd := func(flow string) EnvoyEvent {
		t.Helper()
		var end EnvoyEvent
		wait.For(t, "fixture terminal proxy event", func() bool {
			rows, totals := events.Drain(MaxGuardEvents)
			if totals.Lost != 0 || totals.UnknownLoss {
				t.Fatalf("pinned lifecycle lost records: %+v", totals)
			}
			for _, row := range rows {
				if row.FlowID == flow && row.Phase == "TcpConnectionEnd" {
					end = row
				}
			}
			return end.Phase != ""
		})
		return end
	}
	flow := strings.Repeat("a", 32)
	conn := dial(flow)
	request, err := http.NewRequest("POST", "https://"+cert.DNSNames[0]+"/fixture", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Close = true
	if err := request.Write(conn); err != nil {
		t.Fatal(err)
	}
	// Read through TLS EOF before closing the client: no unmeasured trailing
	// client close-notify is mixed into the upstream server's completed meter.
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Fatal(err)
	}
	// TLS EOF is not downstream TCP EOF. End the private write leg without
	// adding a close-notify after the independent upstream meter has stopped.
	if err := conn.NetConn().(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	end := awaitEnd(flow)
	if end.ConnectMillis == nil || end.Sent == nil || end.Received == nil || *end.Sent != meter.received.Load() || *end.Received != meter.sent.Load() || *end.Received <= *end.Sent*4 {
		t.Fatalf("single-leg encrypted bytes/direction mismatch: event=%+v upstream_received=%d upstream_sent=%d", end, meter.received.Load(), meter.sent.Load())
	}
	t.Logf("asymmetric proxy_sent=%d proxy_received=%d connect_ms=%d close=%s flags=%v", *end.Sent, *end.Received, *end.ConnectMillis, end.CloseType, end.Flags)
	_ = conn.Close()
	server.Close()

	flow = strings.Repeat("b", 32)
	failed := dial(flow)
	if err := failed.HandshakeContext(ctx); err == nil {
		t.Fatal("closed fixture upstream unexpectedly connected")
	}
	end = awaitEnd(flow)
	if end.ConnectMillis != nil || !slices.Contains(end.Flags, "UF") || !slices.Contains(end.Flags, "URX") {
		t.Fatalf("pinned refusal lacks expected positive failure witness: %+v", end)
	}
	t.Logf("refused upstream connect_ms=absent close=%s flags=%v", end.CloseType, end.Flags)
	_ = failed.Close()
}
