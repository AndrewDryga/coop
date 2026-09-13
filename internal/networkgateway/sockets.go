package networkgateway

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

const (
	MaxSocketInventory = 4096
	MaxSocketBytes     = 1 << 20
)

type SocketTuple struct{ Local, Peer netip.AddrPort }
type SocketRow struct {
	Tuple SocketTuple
	UID   uint32
	Inode uint64
	State string
}

type socketTable struct {
	reader io.Reader
	ipv6   bool
}

type inventoryTruncated struct{ omitted uint64 }

func (*inventoryTruncated) Error() string { return "socket_inventory_truncated" }

// The qualified upstream envelope is IPv4 TCP. /proc queue lengths are not
// transferred-byte counters and are deliberately absent from this model.
func readSocketInventory(b boundary) ([]SocketRow, error) {
	file, err := os.Open("/proc/net/tcp")
	if err != nil {
		return nil, Failure("socket_inventory_unavailable")
	}
	defer file.Close()
	tables := []socketTable{{reader: file}}
	v6, err := os.Open("/proc/net/tcp6")
	if err == nil {
		defer v6.Close()
		tables = append(tables, socketTable{reader: v6, ipv6: true})
	} else {
		return nil, Failure("socket_inventory_unavailable")
	}
	return parseSocketTables(tables, b)
}

func parseSocketInventory(reader io.Reader, b boundary) ([]SocketRow, error) {
	return parseSocketTables([]socketTable{{reader: reader}}, b)
}

func parseSocketTables(tables []socketTable, b boundary) ([]SocketRow, error) {
	var retained [3][]SocketRow
	var omitted uint64
	for _, table := range tables {
		rows, lost, err := parseSocketTable(table, b, retained)
		if err != nil {
			return nil, err
		}
		retained = rows
		omitted += lost
	}
	rows := append(retained[0], retained[1]...)
	rows = append(rows, retained[2]...)
	if omitted != 0 {
		return rows, &inventoryTruncated{omitted: omitted}
	}
	return rows, nil
}

func parseSocketTable(table socketTable, b boundary, retained [3][]SocketRow) ([3][]SocketRow, uint64, error) {
	reader := table.reader
	data, err := io.ReadAll(io.LimitReader(reader, MaxSocketBytes+1))
	if err != nil || len(data) > MaxSocketBytes || len(data) == 0 || data[len(data)-1] != '\n' {
		return retained, 0, Failure("socket_inventory_invalid")
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	if !scanner.Scan() || !strings.Contains(scanner.Text(), "local_address") || !strings.Contains(scanner.Text(), "inode") {
		return retained, 0, Failure("socket_inventory_invalid")
	}
	var omitted uint64
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			return retained, 0, Failure("socket_inventory_invalid")
		}
		local, err := procSocketAddress(fields[1], table.ipv6)
		if err != nil {
			return retained, 0, err
		}
		peer, err := procSocketAddress(fields[2], table.ipv6)
		if err != nil {
			return retained, 0, err
		}
		uid, err := strconv.ParseUint(fields[7], 10, 32)
		if err != nil {
			return retained, 0, Failure("socket_inventory_invalid")
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			return retained, 0, Failure("socket_inventory_invalid")
		}
		state := ""
		switch fields[3] {
		case "01":
			state = "open"
		case "02", "03", "0C":
			state = "connecting"
		case "04", "05", "08", "09", "0B":
			state = "closing"
		case "06", "07", "0A":
			continue // TIME_WAIT, CLOSED, LISTEN are not live external flows
		default:
			return retained, 0, Failure("socket_inventory_invalid")
		}
		if peer.Addr().IsLoopback() || peer.Addr().IsUnspecified() || peer.Port() == 0 {
			continue
		}
		// /proc retains a NAT-captured socket's original public peer. Exclude
		// only the fixed REDIRECT predicate; unexpected agent sockets remain
		// visible as unknown attempts, not silently attributed external flows.
		if b.captured(uint32(uid), local, peer) {
			continue
		}
		row := SocketRow{Tuple: SocketTuple{Local: local, Peer: peer}, UID: uint32(uid), Inode: inode, State: state}
		priority := 2
		if uid == 65532 {
			priority = 1
			if inode != 0 {
				priority = 0
			}
		}
		if len(retained[0])+len(retained[1])+len(retained[2]) == MaxSocketInventory {
			omitted++
			evict := len(retained) - 1
			for evict > priority && len(retained[evict]) == 0 {
				evict--
			}
			if evict <= priority {
				continue
			}
			retained[evict] = retained[evict][:len(retained[evict])-1]
		}
		retained[priority] = append(retained[priority], row)
	}
	if scanner.Err() != nil {
		return retained, 0, Failure("socket_inventory_invalid")
	}
	return retained, omitted, nil
}

// boundary is everything the inventory needs to recognize a socket this
// gateway EXPECTS: the permanent denials, plus the frozen policy that says
// which raw destinations this run may dial without passing through the guard.
type boundary struct {
	protected           []netip.Prefix
	policy              egress.Snapshot
	serviceProxyClients []netip.Addr
	// tlsPorts is the policy's TLS port set — what the capture chain redirects —
	// held here because the inventory asks about it once per row per sample.
	tlsPorts []int
}

// captured reports a socket the boundary accounts for. An agent socket the
// gateway redirects (a granted TLS port, DNS 53) is one; so is an agent socket
// an address grant permits directly — a raw grant has no proxied leg to
// correlate, so treating it as an unattributed flow would report an allowed
// connection as an evidence gap and, mid-handshake, as a denial that never happened.
func (b boundary) captured(uid uint32, local, peer netip.AddrPort) bool {
	if uid == 1000 && peer.Addr().Is4() {
		// The run's own namespace loopback is permitted, not a protected host
		// surface: an agent's `npm test` server is its own business, and
		// reporting it as an attempt on a protected destination would be a false
		// alert about traffic that never left the box.
		if peer.Addr().IsLoopback() {
			return true
		}
		if peer.Port() == 53 || slices.Contains(b.tlsPorts, int(peer.Port())) && !protectedSocketPeer(peer.Addr(), b.protected) {
			return true
		}
		if b.policy.Address(peer.Addr(), "tcp", int(peer.Port()), 0, 0, b.protected).Allowed {
			return true
		}
	}
	// The accepted guard-side leg is local even with a namespace peer IP. The
	// service proxy listens on the internal bridge instead of loopback, so its
	// accepted legs are matched to the exact prepared service addresses.
	return uid == 65532 && (local.Addr() == netip.AddrFrom4([4]byte{127, 0, 0, 1}) && (local.Port() == 15443 || local.Port() == 15353) ||
		local.Port() == ServiceProxyPort && slices.Contains(b.serviceProxyClients, peer.Addr()))
}

func protectedSocketPeer(peer netip.Addr, protected []netip.Prefix) bool {
	for _, prefix := range egress.ProtectedRanges(protected) {
		if prefix.Contains(peer) {
			return true
		}
	}
	return false
}

func procAddress(text string) (netip.AddrPort, error) {
	if len(text) != 13 || text[8] != ':' {
		return netip.AddrPort{}, Failure("socket_inventory_invalid")
	}
	address, err := strconv.ParseUint(text[:8], 16, 32)
	if err != nil {
		return netip.AddrPort{}, Failure("socket_inventory_invalid")
	}
	port, err := strconv.ParseUint(text[9:], 16, 16)
	if err != nil {
		return netip.AddrPort{}, Failure("socket_inventory_invalid")
	}
	var raw [4]byte
	binary.NativeEndian.PutUint32(raw[:], uint32(address))
	return netip.AddrPortFrom(netip.AddrFrom4(raw), uint16(port)), nil
}

func procSocketAddress(text string, ipv6 bool) (netip.AddrPort, error) {
	if !ipv6 {
		return procAddress(text)
	}
	if len(text) != 37 || text[32] != ':' {
		return netip.AddrPort{}, Failure("socket_inventory_invalid")
	}
	var raw [16]byte
	for i := range 4 {
		word, err := strconv.ParseUint(text[i*8:i*8+8], 16, 32)
		if err != nil {
			return netip.AddrPort{}, Failure("socket_inventory_invalid")
		}
		binary.NativeEndian.PutUint32(raw[i*4:i*4+4], uint32(word))
	}
	port, err := strconv.ParseUint(text[33:], 16, 16)
	if err != nil {
		return netip.AddrPort{}, Failure("socket_inventory_invalid")
	}
	return netip.AddrPortFrom(netip.AddrFrom16(raw).Unmap(), uint16(port)), nil
}

type maintenanceSocket struct {
	ID             uint64
	Tuple          SocketTuple
	StartedAt      time.Time
	Sent, Received uint64
}

type maintenanceSockets struct {
	mu              sync.Mutex
	next            uint64
	active          map[uint64]*maintenanceConn
	sent, received  atomic.Uint64
	partial         atomic.Bool
	closed          bool
	ioActive, dials int
	changed         chan struct{}
}

type maintenanceConn struct {
	net.Conn
	owner          *maintenanceSockets
	id             uint64
	tuple          SocketTuple
	started        time.Time
	sent, received atomic.Uint64
	once           sync.Once
}

func (m *maintenanceSockets) track(conn net.Conn) net.Conn {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = conn.Close()
		return conn
	}
	defer m.mu.Unlock()
	local, localOK := conn.LocalAddr().(*net.TCPAddr)
	peer, peerOK := conn.RemoteAddr().(*net.TCPAddr)
	if !localOK || !peerOK || len(m.active) >= 2*MaxDNSInFlight || m.next == ^uint64(0) {
		m.partial.Store(true)
		return conn // observation loss never cancels a legitimate query
	}
	if m.active == nil {
		m.active = make(map[uint64]*maintenanceConn)
	}
	m.next++
	localAddr, peerAddr := local.AddrPort(), peer.AddrPort()
	c := &maintenanceConn{Conn: conn, owner: m, id: m.next, tuple: SocketTuple{
		Local: netip.AddrPortFrom(localAddr.Addr().Unmap(), localAddr.Port()), Peer: netip.AddrPortFrom(peerAddr.Addr().Unmap(), peerAddr.Port())}, started: time.Now().UTC()}
	m.active[c.id] = c
	return c
}

func addAtomic(value *atomic.Uint64, n uint64, partial *atomic.Bool) {
	for {
		old := value.Load()
		next := old + n
		if n > ^uint64(0)-old {
			next = ^uint64(0)
			partial.Store(true)
		}
		if value.CompareAndSwap(old, next) {
			return
		}
	}
}

func (c *maintenanceConn) Read(p []byte) (int, error) {
	if !c.owner.beginIO() {
		return 0, net.ErrClosed
	}
	defer c.owner.endIO()
	n, err := c.Conn.Read(p)
	addAtomic(&c.received, uint64(n), &c.owner.partial)
	addAtomic(&c.owner.received, uint64(n), &c.owner.partial)
	return n, err
}
func (c *maintenanceConn) Write(p []byte) (int, error) {
	if !c.owner.beginIO() {
		return 0, net.ErrClosed
	}
	defer c.owner.endIO()
	n, err := c.Conn.Write(p)
	addAtomic(&c.sent, uint64(n), &c.owner.partial)
	addAtomic(&c.owner.sent, uint64(n), &c.owner.partial)
	return n, err
}

func (m *maintenanceSockets) signalLocked() {
	if m.changed == nil {
		m.changed = make(chan struct{}, 1)
	}
	select {
	case m.changed <- struct{}{}:
	default:
	}
}
func (m *maintenanceSockets) beginIO() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.ioActive++
	return true
}
func (m *maintenanceSockets) endIO() { m.mu.Lock(); m.ioActive--; m.signalLocked(); m.mu.Unlock() }
func (m *maintenanceSockets) beginDial() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.dials++
	return true
}
func (m *maintenanceSockets) endDial() { m.mu.Lock(); m.dials--; m.signalLocked(); m.mu.Unlock() }

func (m *maintenanceSockets) shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	connections := make([]*maintenanceConn, 0, len(m.active))
	for _, c := range m.active {
		connections = append(connections, c)
	}
	m.signalLocked()
	m.mu.Unlock()
	for _, c := range connections {
		_ = c.Close()
	}
	for {
		m.mu.Lock()
		settled := m.ioActive == 0 && m.dials == 0 && len(m.active) == 0
		changed := m.changed
		m.mu.Unlock()
		if settled {
			if m.partial.Load() {
				return Failure("maintenance_shutdown_unconfirmed")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			m.partial.Store(true)
			return Failure("maintenance_shutdown_unconfirmed")
		case <-changed:
		}
	}
}
func (c *maintenanceConn) Close() error {
	// Retire identity before releasing the kernel tuple. A concurrent scan
	// must not attribute a reused tuple to this connection after Close.
	c.once.Do(func() { c.owner.mu.Lock(); delete(c.owner.active, c.id); c.owner.mu.Unlock() })
	return c.Conn.Close()
}
func (m *maintenanceSockets) snapshot() []maintenanceSocket {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]maintenanceSocket, 0, len(m.active))
	for _, c := range m.active {
		result = append(result, maintenanceSocket{ID: c.id, Tuple: c.tuple, StartedAt: c.started, Sent: c.sent.Load(), Received: c.received.Load()})
	}
	return result
}
func (m *maintenanceSockets) stillOwned(s maintenanceSocket) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.active[s.ID]
	return c != nil && c.tuple == s.Tuple
}
