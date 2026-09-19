//go:build linux

package networkgateway

import (
	"bufio"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A resolver connection dialed and released between two samples leaves a kernel socket no sample
// saw the resolver own. The registry read that socket's inode from its own fd — the number
// /proc/net/tcp lists for it — so the collector explains its remnant by exact tuple AND inode, while
// an agent socket to the same peer, another inode at that tuple, and the identity past the
// staleness bound stay unexplained.
func TestCollectorExplainsAShortLivedResolverSocketByItsOwnInode(t *testing.T) {
	for _, scenario := range []string{"its remnant", "an agent socket to the same peer", "another inode at its tuple"} {
		t.Run(scenario, func(t *testing.T) {
			c, now := collectorFixture(t)
			tuple, inode := releasedResolverSocket(t, c)
			row := SocketRow{Tuple: tuple, UID: 65532, Inode: inode, State: "closing"}
			switch scenario {
			case "an agent socket to the same peer":
				row = SocketRow{Tuple: SocketTuple{Local: netip.MustParseAddrPort("172.17.0.2:49999"), Peer: tuple.Peer}, UID: 1000, Inode: inode + 1, State: "open"}
			case "another inode at its tuple":
				row.Inode = inode + 2
			}
			sampleReleasedFixture(c, *now, []SocketRow{row})
			explained := c.snapshot.PendingConnections == 0 && *c.snapshot.UnknownConnections == 0
			if want := scenario == "its remnant"; explained != want {
				t.Fatalf("%s explained = %v, want %v (pending=%d)", scenario, explained, want, c.snapshot.PendingConnections)
			}
			if scenario != "its remnant" {
				return
			}
			// The kernel lists the remnant for a few samples; past the staleness bound its
			// tuple may be a stranger's, so the released identity explains nothing.
			*now = now.Add(ObservationStaleAfter + time.Second)
			sampleReleasedFixture(c, *now, []SocketRow{row})
			if c.snapshot.PendingConnections != 1 {
				t.Fatalf("a released identity explained a socket past the staleness bound: pending=%d", c.snapshot.PendingConnections)
			}
		})
	}
}

// releasedResolverSocket dials a real TCP connection through the resolver's registry and releases
// it before any sample, returning its tuple and the inode /proc/net/tcp listed for it while open.
func releasedResolverSocket(t *testing.T, c *Collector) (SocketTuple, uint64) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			accepted <- conn
		}
	}()
	dialed, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	t.Cleanup(func() { server.Close() })
	conn := c.doh.sockets.track(dialed).(*maintenanceConn)
	inode := procSocketInode(t, conn.tuple)
	if inode == 0 || conn.inode != inode {
		t.Fatalf("the registry read inode %d from the socket's fd; /proc/net/tcp lists %d", conn.inode, inode)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	return conn.tuple, inode
}

// procSocketInode is the inode /proc/net/tcp lists for tuple.
func procSocketInode(t *testing.T, tuple SocketTuple) uint64 {
	t.Helper()
	file, err := os.Open("/proc/net/tcp")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			continue
		}
		local, localErr := procSocketAddress(fields[1], false)
		peer, peerErr := procSocketAddress(fields[2], false)
		if localErr != nil || peerErr != nil || local != tuple.Local || peer != tuple.Peer {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return inode
	}
	return 0
}
