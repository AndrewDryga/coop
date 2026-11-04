package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/acpctl"
	containerruntime "github.com/AndrewDryga/coop/internal/runtime"
)

// acpServeBindsEnv tells an inner ACP child which private host port each serve port publishes on.
const acpServeBindsEnv = "COOP_ACP_SERVE_BINDS"

// acpServeRetry is how often a session asks again for a stable port something else holds.
var acpServeRetry = 5 * time.Second

// acpServeRouter owns an editor session's stable serve ports. A running container cannot gain a
// published port, and a session's boxes change under one thread: warm boxes, a switch of provider or
// account, a refresh after coop approve. So each box publishes on private ports of its own, parked
// boxes hold nothing a person could reach, and this forwards each stable port to the box the thread is
// using now. It is plain TCP on the host's loopback, so it carries HTTP and websockets alike.
type acpServeRouter struct {
	report  func(ports []acpctl.ServePort, freed []acpctl.ServePort)
	notify  func(string)
	runtime *containerruntime.Runtime // nil: no server watching
	stop    chan struct{}
	wg      sync.WaitGroup

	mu        sync.Mutex
	ports     []acpctl.ServePort
	live      map[int]int // box port → the live box's private host port
	listeners []net.Listener
	conns     map[net.Conn]struct{}
	stopWatch context.CancelFunc
	closed    bool
}

// startACPServeRouter takes the stable ports it can, and keeps asking for the rest: a program on the
// host, or another session in this checkout, may hold one for now.
func startACPServeRouter(ports []acpctl.ServePort) *acpServeRouter {
	r := &acpServeRouter{stop: make(chan struct{}), live: map[int]int{}, conns: map[net.Conn]struct{}{}}
	r.take(ports)
	return r
}

// add gives a router started with no ports its first, for a session that connected while its project's
// file did not load. A thread already open hears the links that work, since its start message could not
// name them.
func (r *acpServeRouter) add(ports []acpctl.ServePort) {
	if r == nil || len(ports) == 0 {
		return
	}
	r.take(ports)
	r.mu.Lock()
	report := r.report
	r.mu.Unlock()
	if report != nil {
		current := r.Ports()
		report(current, slices.DeleteFunc(slices.Clone(current), func(p acpctl.ServePort) bool { return p.Taken }))
	}
}

// take listens on the router's first ports, and keeps asking for any it cannot have yet.
func (r *acpServeRouter) take(ports []acpctl.ServePort) {
	r.mu.Lock()
	r.ports = slices.Clone(ports)
	r.mu.Unlock()
	taken := false
	for i := range ports {
		if !r.listen(i) {
			r.mu.Lock()
			r.ports[i].Taken = true
			r.mu.Unlock()
			taken = true
		}
	}
	if taken {
		r.wg.Add(1)
		go r.retry()
	}
}

// reportTo keeps the editor told: later threads' start messages show the current state, and a thread
// already open hears when a link starts to work.
func (r *acpServeRouter) reportTo(ctrl *acpctl.Control) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.notify = ctrl.Notify
	r.report = func(ports, freed []acpctl.ServePort) {
		ctrl.SetServePorts(ports)
		for _, p := range freed {
			ctrl.Notify(fmt.Sprintf("Port %d now reaches this box: [localhost:%d](http://localhost:%d)", p.Box, p.Host, p.Host))
		}
	}
	r.mu.Unlock()
}

// Ports is each stable port's state now.
func (r *acpServeRouter) Ports() []acpctl.ServePort {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ports)
}

func (r *acpServeRouter) listen(index int) bool {
	r.mu.Lock()
	port := r.ports[index]
	r.mu.Unlock()
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port.Host)))
	if err != nil {
		return false
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = listener.Close()
		return false
	}
	r.listeners = append(r.listeners, listener)
	r.mu.Unlock()
	r.wg.Add(1)
	go r.accept(listener, port.Box)
	return true
}

func (r *acpServeRouter) retry() {
	defer r.wg.Done()
	ticker := time.NewTicker(acpServeRetry)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
		}
		var freed []acpctl.ServePort
		waiting := false
		for i := range r.Ports() {
			r.mu.Lock()
			taken := r.ports[i].Taken
			r.mu.Unlock()
			if !taken {
				continue
			}
			if !r.listen(i) {
				waiting = true
				continue
			}
			r.mu.Lock()
			r.ports[i].Taken = false
			freed = append(freed, r.ports[i])
			r.mu.Unlock()
		}
		r.mu.Lock()
		report := r.report
		r.mu.Unlock()
		if len(freed) > 0 && report != nil {
			report(r.Ports(), freed)
		}
		if !waiting {
			return
		}
	}
}

func (r *acpServeRouter) accept(listener net.Listener, boxPort int) {
	defer r.wg.Done()
	for {
		client, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-r.stop:
				return
			case <-time.After(100 * time.Millisecond): // out of file descriptors, say: this passes
			}
			continue
		}
		r.wg.Add(1)
		go r.forward(client, boxPort)
	}
}

// forward joins one connection to the live box. With no live box, or nothing listening in it, the
// connection simply ends, as it would for a box's own published port.
func (r *acpServeRouter) forward(client net.Conn, boxPort int) {
	defer r.wg.Done()
	defer client.Close()
	r.mu.Lock()
	private := r.live[boxPort]
	if private == 0 || r.closed {
		r.mu.Unlock()
		return
	}
	r.conns[client] = struct{}{}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.conns, client)
		r.mu.Unlock()
	}()
	target, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(private)), 5*time.Second)
	if err != nil {
		return
	}
	defer target.Close()
	done := make(chan struct{}, 2)
	relay := func(to, from net.Conn) {
		_, _ = io.Copy(to, from)
		if closer, ok := to.(*net.TCPConn); ok {
			_ = closer.CloseWrite() // the other direction may still have an answer to send
		}
		done <- struct{}{}
	}
	go relay(target, client)
	go relay(client, target)
	<-done
	select {
	case <-done:
	case <-r.stop:
	case <-time.After(30 * time.Second): // half-closed and idle: a dev server that never finished
	}
}

// setLive points every stable port at the box the thread now uses. Connections to the box before it
// end: left open, a browser's websocket would keep talking to a box the thread has left.
func (r *acpServeRouter) setLive(binds map[int]int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.live) == len(binds) && func() bool {
		for box, private := range binds {
			if r.live[box] != private {
				return false
			}
		}
		return true
	}() {
		return
	}
	r.live = binds
	for conn := range r.conns {
		_ = conn.Close()
	}
}

// allocate picks a private host port for each serve port of a box about to start. Docker binds it a
// moment later; a port lost in that moment is the same skipped-port warning any publish can meet.
// The second result is what the box's inner process is told, which also says which stable ports
// something else holds.
func (r *acpServeRouter) allocate() (map[int]int, string, error) {
	if r == nil {
		return nil, "", nil
	}
	ports := r.Ports()
	binds := make(map[int]int, len(ports))
	// Held together until all are picked, so no two ports of this box get the same one.
	for _, port := range ports {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, "", fmt.Errorf("pick a private host port: %w", err)
		}
		defer listener.Close()
		binds[port.Box] = listener.Addr().(*net.TCPAddr).Port
	}
	return binds, encodeServeBinds(binds, ports), nil
}

// Close stops forwarding and gives the stable ports back.
func (r *acpServeRouter) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.stop)
	if r.stopWatch != nil {
		r.stopWatch()
	}
	for _, listener := range r.listeners {
		_ = listener.Close()
	}
	for conn := range r.conns {
		_ = conn.Close()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// encodeServeBinds and decodeServeBinds carry a box's private ports to its inner process, each with
// whether something else holds its stable port: 4000=52341,9091=52342!
func encodeServeBinds(binds map[int]int, ports []acpctl.ServePort) string {
	entries := make([]string, 0, len(binds))
	for box, private := range binds {
		entry := fmt.Sprintf("%d=%d", box, private)
		if slices.ContainsFunc(ports, func(p acpctl.ServePort) bool { return p.Box == box && p.Taken }) {
			entry += "!"
		}
		entries = append(entries, entry)
	}
	slices.Sort(entries)
	return strings.Join(entries, ",")
}

func decodeServeBinds(value string) (map[int]int, map[int]bool, error) {
	if value == "" {
		return nil, nil, nil
	}
	binds, taken := map[int]int{}, map[int]bool{}
	for _, entry := range strings.Split(value, ",") {
		box, private, ok := strings.Cut(entry, "=")
		private, held := strings.CutSuffix(private, "!")
		boxPort, boxErr := strconv.Atoi(box)
		privatePort, privateErr := strconv.Atoi(private)
		if !ok || boxErr != nil || privateErr != nil || boxPort < 1 || boxPort > 65535 || privatePort < 1 || privatePort > 65535 {
			return nil, nil, fmt.Errorf("%s is malformed", acpServeBindsEnv)
		}
		binds[boxPort] = privatePort
		if held {
			taken[boxPort] = true
		}
	}
	return binds, taken, nil
}
