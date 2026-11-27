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

// acpServeRetry is how often a session asks again for a stable port something else holds while its
// box serves.
var acpServeRetry = 5 * time.Second

// acpServeRouter owns an editor session's stable serve ports. A running container cannot gain a
// published port, and a session's boxes change under one thread: warm boxes, a switch of provider or
// account, a refresh after coop approve. So each box publishes on private ports of its own, parked
// boxes hold nothing a person could reach, and this forwards each stable port to the box the thread is
// using now. It listens on a stable port only while a server in that box listens on the box port, so
// an idle box leaves the port to a server on the host or in another session. It is plain TCP on the
// host's loopback, so it carries HTTP and websockets alike.
type acpServeRouter struct {
	report  func(ports []acpctl.ServePort, freed []acpctl.ServePort)
	notify  func(string)
	runtime *containerruntime.Runtime // nil: no server watching
	stop    chan struct{}
	wg      sync.WaitGroup

	mu        sync.Mutex
	ports     []acpctl.ServePort
	live      map[int]int          // box port → the live box's private host port
	listeners map[int]net.Listener // box port → its stable port, while the watched box serves it
	conns     map[net.Conn]int     // → the box port it came in on
	stopWatch context.CancelFunc
	closed    bool
}

// startACPServeRouter listens on nothing yet: a stable port is taken once the live box serves it.
func startACPServeRouter(ports []acpctl.ServePort) *acpServeRouter {
	r := &acpServeRouter{stop: make(chan struct{}), ports: slices.Clone(ports), live: map[int]int{}, listeners: map[int]net.Listener{}, conns: map[net.Conn]int{}}
	r.wg.Add(1)
	go r.retry()
	return r
}

// add gives a router started with no ports its first, for a session that connected while its project's
// file did not load. Later threads' start messages list them.
func (r *acpServeRouter) add(ports []acpctl.ServePort) {
	if r == nil || len(ports) == 0 {
		return
	}
	r.mu.Lock()
	r.ports = slices.Clone(ports)
	r.mu.Unlock()
	r.publish(nil)
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

func (r *acpServeRouter) publish(freed []acpctl.ServePort) {
	r.mu.Lock()
	report := r.report
	r.mu.Unlock()
	if report != nil {
		report(r.Ports(), freed)
	}
}

// Ports is each stable port's state now.
func (r *acpServeRouter) Ports() []acpctl.ServePort {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ports)
}

// served follows one watcher event of the box watch belongs to: a server reachable on a box port takes
// its stable port, or marks it taken while something else holds it; a server that stops gives it back.
// A server on the box's 127.0.0.1 is no reason to hold it, since no published port reaches it.
func (r *acpServeRouter) served(watch context.Context, event serveEvent) {
	r.mu.Lock()
	i := slices.IndexFunc(r.ports, func(p acpctl.ServePort) bool { return p.Box == event.port })
	if watch.Err() != nil || r.closed || i < 0 || event.up && event.loopback { // watch ends under r.mu once the thread leaves its box
		r.mu.Unlock()
		return
	}
	if event.up {
		r.ports[i].Taken = !r.listen(i)
	} else {
		r.release(event.port)
	}
	r.mu.Unlock()
	r.publish(nil)
}

// listen takes a stable port, under r.mu.
func (r *acpServeRouter) listen(index int) bool {
	port := r.ports[index]
	if r.listeners[port.Box] != nil {
		return true
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port.Host)))
	if err != nil {
		return false
	}
	r.listeners[port.Box] = listener
	r.wg.Add(1)
	go r.accept(listener, port.Box)
	return true
}

// release gives a stable port back and ends its connections, under r.mu.
func (r *acpServeRouter) release(boxPort int) {
	if listener := r.listeners[boxPort]; listener != nil {
		_ = listener.Close()
		delete(r.listeners, boxPort)
	}
	for conn, port := range r.conns {
		if port == boxPort {
			_ = conn.Close()
		}
	}
	for i := range r.ports {
		if r.ports[i].Box == boxPort {
			r.ports[i].Taken = false // taken means a box server waits for it, and none does now
		}
	}
}

func (r *acpServeRouter) releaseAll() {
	for _, p := range r.ports {
		r.release(p.Box)
	}
}

// retry asks again for each stable port the box serves while something else holds it.
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
		r.mu.Lock()
		for i := range r.ports {
			if r.ports[i].Taken && !r.closed && r.listen(i) {
				r.ports[i].Taken = false
				freed = append(freed, r.ports[i])
			}
		}
		r.mu.Unlock()
		if len(freed) > 0 {
			r.publish(freed)
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
		go r.forward(client, listener, boxPort)
	}
}

// forward joins one connection to the live box. With no live box, or nothing listening in it, the
// connection simply ends, as it would for a box's own published port.
func (r *acpServeRouter) forward(client net.Conn, listener net.Listener, boxPort int) {
	defer r.wg.Done()
	defer client.Close()
	r.mu.Lock()
	private := r.live[boxPort]
	if private == 0 || r.closed || r.listeners[boxPort] != listener { // released while it was accepted
		r.mu.Unlock()
		return
	}
	r.conns[client] = boxPort
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
// a program on the host holds now: the session takes a free one only once the box serves it.
func (r *acpServeRouter) allocate() (map[int]int, string, error) {
	if r == nil {
		return nil, "", nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ports := slices.Clone(r.ports)
	binds := make(map[int]int, len(ports))
	// Held together until all are picked, so no two ports of this box get the same one.
	for i, port := range ports {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, "", fmt.Errorf("pick a private host port: %w", err)
		}
		defer listener.Close()
		binds[port.Box] = listener.Addr().(*net.TCPAddr).Port
		if r.listeners[port.Box] == nil {
			stable, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port.Host)))
			if ports[i].Taken = err != nil; err == nil {
				_ = stable.Close()
			}
		}
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
