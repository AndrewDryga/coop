package networkgateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

const procHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode\n"

func procRow(uid int, state string, inode int) string {
	return fmt.Sprintf("0: 020011AC:C001 01010101:01BB %s 00FFFFFF:00FFFFFF 00:00000000 00000000 %d 0 %d 1 0\n", state, uid, inode)
}

func TestSocketInventoryExcludesLocalNATLegAndQueuesAreNotBytes(t *testing.T) {
	rows, err := parseSocketInventory(strings.NewReader(procHeader+procRow(1000, "01", 1)+procRow(65532, "01", 2)+procRow(65532, "06", 0)), boundary{tlsPorts: []int{443}})
	if err != nil || len(rows) != 1 {
		t.Fatalf("inventory double-counted local NAT leg or TIME_WAIT: %#v %v", rows, err)
	}
	if rows[0].Tuple.Peer.String() != "1.1.1.1:443" || rows[0].Tuple.Local.String() != "172.17.0.2:49153" || rows[0].State != "open" || rows[0].Inode != 2 {
		t.Fatalf("wrong proc address/state parsing: %#v", rows[0])
	}
	for _, value := range []string{procHeader + strings.TrimSuffix(procRow(65532, "01", 2), "\n"), procHeader + procRow(65532, "FF", 2),
		procHeader + "short\n", strings.Repeat("x", MaxSocketBytes+1)} {
		if rows, err := parseSocketInventory(strings.NewReader(value), boundary{}); err == nil || rows != nil {
			t.Fatal("accepted partial or oversized inventory")
		}
	}
}

func TestSocketInventoryMatchesEveryFixedLocalCaptureLeg(t *testing.T) {
	dns := strings.Replace(procRow(1000, "01", 1), "01010101:01BB", "01010101:0035", 1)
	accepted := strings.Replace(procRow(65532, "01", 2), "020011AC:C001", "0100007F:3C53", 1)
	serviceProxy := strings.Replace(procRow(65532, "01", 3), "020011AC:C001", "020011AC:3C54", 1)
	proxyClient := netip.MustParseAddr("1.1.1.1")
	rows, err := parseSocketInventory(strings.NewReader(procHeader+dns+accepted+serviceProxy), boundary{serviceProxyClients: []ServiceProxyClient{{Name: "web", Address: proxyClient}}})
	if err != nil || len(rows) != 0 {
		t.Fatalf("local DNS/accepted guard legs counted external: %#v %v", rows, err)
	}
	rows, err = parseSocketInventory(strings.NewReader(procHeader+procRow(1000, "02", 3)), boundary{tlsPorts: []int{443}, protected: []netip.Prefix{netip.MustParsePrefix("1.1.1.0/24")}})
	if err != nil || len(rows) != 1 {
		t.Fatal("protected public endpoint was silently treated as TLS redirect")
	}
}

// A raw grant has no proxied leg to correlate, so its socket must be accounted
// for by the policy itself. Otherwise an allowed connection is reported as an
// unattributed evidence gap and, sampled mid-handshake, as a denial.
func TestAllowedRawSocketsAreAccountedForByTheirGrant(t *testing.T) {
	granted := transportPolicy(t, egress.Rule{To: egress.Destination{IP: "1.1.1.1"}, Protocol: "tcp", Ports: []int{853}})
	dot := strings.Replace(procRow(1000, "02", 7), "01010101:01BB", "01010101:0355", 1)
	rows, err := parseSocketInventory(strings.NewReader(procHeader+dot), boundary{policy: granted})
	if err != nil || len(rows) != 0 {
		t.Fatalf("an allowed raw socket was reported as an unattributed flow: %#v %v", rows, err)
	}
	other := strings.Replace(procRow(1000, "02", 8), "01010101:01BB", "01010101:2295", 1)
	rows, err = parseSocketInventory(strings.NewReader(procHeader+other), boundary{policy: granted})
	if err != nil || len(rows) != 1 || rows[0].Tuple.Peer.Port() != 8853 {
		t.Fatalf("a socket outside every grant was hidden: %#v %v", rows, err)
	}
	// The permanent denials still win: a grant cannot make a protected peer an
	// expected flow.
	rows, err = parseSocketInventory(strings.NewReader(procHeader+dot), boundary{policy: granted, protected: []netip.Prefix{netip.MustParsePrefix("1.1.1.1/32")}})
	if err != nil || len(rows) != 1 {
		t.Fatalf("a protected peer inside a grant was treated as expected: %#v %v", rows, err)
	}
}

type closeHookConn struct {
	net.Conn
	closeHook func()
}

func (c closeHookConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(172, 17, 0, 2), Port: 49153}
}
func (c closeHookConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(1, 1, 1, 1), Port: 443}
}
func (c closeHookConn) Close() error { c.closeHook(); return nil }

func TestMaintenanceIdentityRetiresBeforeKernelTupleCanBeReused(t *testing.T) {
	var registry maintenanceSockets
	var prior maintenanceSocket
	conn := registry.track(closeHookConn{closeHook: func() {
		if registry.stillOwned(prior) || len(registry.snapshot()) != 0 {
			t.Error("retired socket can lend ownership to reused kernel tuple")
		}
	}})
	prior = registry.snapshot()[0]
	if !registry.stillOwned(prior) {
		t.Fatal("new socket not tracked")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	other := registry.track(closeHookConn{closeHook: func() {}})
	defer other.Close()
	if registry.stillOwned(prior) {
		t.Fatal("new owner reused old identity")
	}
}

func TestMaintenanceTracksIdleHTTPConnectionAndEncryptedLegOnly(t *testing.T) {
	doh := testDoH(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(make([]byte, 12))
	}))
	if _, err := doh.Exchange(context.Background(), bytes.Repeat([]byte{1}, 12)); err != nil {
		t.Fatal(err)
	}
	owned := doh.sockets.snapshot()
	if len(owned) != 1 || owned[0].Sent <= 12 || owned[0].Received <= 12 {
		t.Fatalf("missing idle socket/TLS wire accounting: %#v", owned)
	}
	if !doh.sockets.stillOwned(owned[0]) {
		t.Fatal("query completion prematurely retired pooled socket")
	}
	doh.Close()
	wait.For(t, "maintenance idle socket closed", func() bool { return len(doh.sockets.snapshot()) == 0 })
	if doh.sockets.sent.Load() == 0 || doh.sockets.received.Load() == 0 {
		t.Fatal("close erased cumulative maintenance counts")
	}
}

func TestMaintenanceSaturationDoesNotWrap(t *testing.T) {
	var value atomic.Uint64
	var partial atomic.Bool
	value.Store(^uint64(0) - 1)
	addAtomic(&value, 2, &partial)
	if value.Load() != ^uint64(0) || !partial.Load() {
		t.Fatal("maintenance accounting wrapped")
	}
}

type delayedMeterConn struct {
	closeHookConn
	entered, release chan struct{}
}

func (c delayedMeterConn) Read(data []byte) (int, error) {
	close(c.entered)
	<-c.release
	data[0] = 1
	return 1, io.EOF
}

func TestMaintenanceShutdownJoinsLateMeterUpdateAfterSocketClose(t *testing.T) {
	var registry maintenanceSockets
	entered, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	conn := registry.track(delayedMeterConn{closeHookConn: closeHookConn{closeHook: func() { once.Do(func() { close(closed) }) }}, entered: entered, release: release})
	readDone := make(chan struct{})
	go func() { _, _ = conn.Read(make([]byte, 1)); close(readDone) }()
	select {
	case <-entered:
	case <-time.After(wait.Deadline):
		t.Fatal("read fixture did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- registry.shutdown(ctx) }()
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("shutdown did not close socket")
	}
	select {
	case err := <-done:
		t.Fatalf("shutdown missed late byte update: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("shutdown did not join read")
	}
	<-readDone
	if registry.received.Load() != 1 {
		t.Fatal("terminal barrier missed last bytes")
	}
	if _, err := conn.Write([]byte{1}); err != net.ErrClosed {
		t.Fatal("new IO started after terminal barrier")
	}
	if registry.beginDial() {
		t.Fatal("new dial started after shutdown")
	}
}

func TestMaintenanceShutdownWaitsForDetachedDial(t *testing.T) {
	var registry maintenanceSockets
	if !registry.beginDial() {
		t.Fatal("fixture dial refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- registry.shutdown(ctx) }()
	wait.For(t, "terminal dial fence", func() bool { registry.mu.Lock(); defer registry.mu.Unlock(); return registry.closed })
	select {
	case <-done:
		t.Fatal("shutdown did not join detached dial")
	default:
	}
	registry.endDial()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("dial barrier did not finish")
	}
}
