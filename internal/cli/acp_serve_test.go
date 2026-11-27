package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/acpctl"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

// A box's published port, as far as the router can tell: a listener that says whose it is.
func fakeBoxPort(t *testing.T, name string) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.WriteString(conn, name)
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// readLink opens the stable port and returns what answers, "" when the connection simply ends.
func readLink(t *testing.T, port int) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		t.Fatalf("the stable port did not answer: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 32)
	n, _ := conn.Read(buf)
	return string(buf[:n])
}

// ended says the router closed conn: a read meets its end, not the deadline.
func ended(conn net.Conn) bool {
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := conn.Read(make([]byte, 8))
	var timeout net.Error
	return err != nil && !(errors.As(err, &timeout) && timeout.Timeout())
}

// serves tells the router what its watcher would of the live box: a server starts or stops on port.
func serves(r *acpServeRouter, port int, up bool) {
	r.served(context.Background(), serveEvent{port: port, up: up})
}

// hostCanBind is what a server on the host meets when it starts on port.
func hostCanBind(t *testing.T, port int) bool {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

// The scenario the router exists for: a warm box and a thread that switches. The same link reaches
// whichever box the thread uses, at the same address, and a parked box is out of reach. A box that
// serves nothing leaves the port to the host.
func TestEditorServeLinkFollowsTheLiveBox(t *testing.T) {
	stable := freeLoopbackPort(t)
	router := startACPServeRouter([]acpctl.ServePort{{Box: 4000, Host: stable}})
	defer router.Close()
	if !hostCanBind(t, stable) {
		t.Fatal("the session held the stable port with no box at all")
	}
	claude := map[int]int{4000: fakeBoxPort(t, "claude box")}
	codex := map[int]int{4000: fakeBoxPort(t, "codex box")}
	router.setLive(claude)
	if !hostCanBind(t, stable) {
		t.Fatal("the session held the stable port while nothing served in its box")
	}
	serves(router, 4000, true)
	if got := readLink(t, stable); got != "claude box" {
		t.Fatalf("live claude box: link answered %q", got)
	}
	// The thread switches to the warm codex box, and a refresh replacement goes the same way. Each new
	// box's watcher reports the servers it finds.
	router.setLive(codex)
	router.follow(nil)
	if !hostCanBind(t, stable) {
		t.Fatal("the stable port stayed held after the thread left the box that served it")
	}
	serves(router, 4000, true)
	if got := readLink(t, stable); got != "codex box" {
		t.Fatalf("after the switch: link answered %q", got)
	}
	replacement := map[int]int{4000: fakeBoxPort(t, "replacement box")}
	router.setLive(replacement)
	router.follow(nil)
	serves(router, 4000, true)
	if got := readLink(t, stable); got != "replacement box" {
		t.Fatalf("after the refresh: link answered %q", got)
	}
}

// A server that stops gives the port back, and its open connections end. A server only on the box's
// 127.0.0.1 is unreachable through any published port, so it takes nothing either.
func TestEditorServeReleasesThePortWhenTheBoxServerStops(t *testing.T) {
	stable := freeLoopbackPort(t)
	router := startACPServeRouter([]acpctl.ServePort{{Box: 4000, Host: stable}})
	defer router.Close()
	router.setLive(map[int]int{4000: fakeBoxPort(t, "box")})
	router.served(context.Background(), serveEvent{port: 4000, up: true, loopback: true})
	if !hostCanBind(t, stable) {
		t.Fatal("a server on the box's loopback took the stable port")
	}
	serves(router, 9999, true) // not a serve port
	serves(router, 4000, true)
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(stable)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	buf := make([]byte, 3)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "box" {
		t.Fatalf("first read = %q, %v", buf, err)
	}
	if hostCanBind(t, stable) {
		t.Fatal("the session did not take the port its box serves")
	}
	serves(router, 4000, false)
	if !hostCanBind(t, stable) {
		t.Fatal("the stable port stayed held after the box server stopped")
	}
	if !ended(conn) {
		t.Fatal("a connection stayed open after the box server stopped")
	}
	// A watcher of a box the thread has left says nothing that counts.
	left, cancel := context.WithCancel(context.Background())
	cancel()
	router.served(left, serveEvent{port: 4000, up: true})
	if !hostCanBind(t, stable) {
		t.Fatal("a box the thread left took the stable port")
	}
}

// A browser's open connection must not keep talking to a box the thread has left.
func TestEditorServeSwitchEndsConnectionsToTheOldBox(t *testing.T) {
	stable := freeLoopbackPort(t)
	router := startACPServeRouter([]acpctl.ServePort{{Box: 4000, Host: stable}})
	defer router.Close()
	router.setLive(map[int]int{4000: fakeBoxPort(t, "old")})
	serves(router, 4000, true)
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(stable)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	buf := make([]byte, 3)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "old" {
		t.Fatalf("first read = %q, %v", buf, err)
	}
	router.setLive(map[int]int{4000: fakeBoxPort(t, "new")})
	if !ended(conn) {
		t.Fatal("the connection to the old box stayed open across the switch")
	}
}

// A session that connected while its project's file did not load has no ports, and learns them when the
// file is fixed. From then on they are like any others: later threads list them, a box starting now
// publishes behind them and hears which one a program on the host holds, and its servers take the rest.
func TestEditorServeRouterTakesThePortsOfAFixedFile(t *testing.T) {
	router := startACPServeRouter(nil)
	defer router.Close()
	if binds, told, err := router.allocate(); err != nil || len(binds) != 0 || told != "" {
		t.Fatalf("before the fix: binds=%v told=%q err=%v", binds, told, err)
	}
	router.add(nil)
	var reported, freed [][]acpctl.ServePort
	router.mu.Lock()
	router.report = func(ports, list []acpctl.ServePort) {
		reported, freed = append(reported, ports), append(freed, list)
	}
	router.mu.Unlock()
	router.add(nil)
	if len(reported) != 0 {
		t.Fatalf("a fixed file with no ports reported %v", reported)
	}
	occupant, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupant.Close()
	stable, held := freeLoopbackPort(t), occupant.Addr().(*net.TCPAddr).Port
	router.add([]acpctl.ServePort{{Box: 4000, Host: stable}, {Box: 5000, Host: held}})
	if len(reported) != 1 || len(reported[0]) != 2 || len(freed[0]) != 0 {
		t.Fatalf("reported=%+v freed=%+v, want both ports listed and no link said to work yet", reported, freed)
	}
	if !hostCanBind(t, stable) {
		t.Fatal("the session took a fixed file's port with nothing serving")
	}
	binds, told, err := router.allocate()
	if err != nil || binds[4000] == 0 || binds[5000] == 0 {
		t.Fatalf("binds=%v told=%q err=%v", binds, told, err)
	}
	if _, taken, err := decodeServeBinds(told); err != nil || taken[4000] || !taken[5000] {
		t.Fatalf("told=%q, want only the port the host holds said to be taken", told)
	}
	router.setLive(map[int]int{4000: fakeBoxPort(t, "the box")})
	serves(router, 4000, true)
	serves(router, 5000, true)
	if got := readLink(t, stable); got != "the box" {
		t.Fatalf("the link answered %q", got)
	}
	if ports := router.Ports(); ports[0].Taken || !ports[1].Taken {
		t.Fatalf("ports = %+v", ports)
	}
	if last := reported[len(reported)-1]; last[0].Taken || !last[1].Taken {
		t.Fatalf("later threads were told %+v", last)
	}
	// While this session listens on a port, the next box hears it free: it reaches whichever box serves.
	_, told, _ = router.allocate()
	if _, taken, err := decodeServeBinds(told); err != nil || taken[4000] || !taken[5000] {
		t.Fatalf("told=%q, want the port this session listens on said to be free", told)
	}
}

// Something else holding the port when the box server starts is told, and the session takes the port
// once it is free, while the box still serves. The thread then hears the link works.
func TestEditorServePortHeldByAnotherProgram(t *testing.T) {
	old := acpServeRetry
	acpServeRetry = 20 * time.Millisecond
	defer func() { acpServeRetry = old }()
	occupant, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupant.Close()
	stable := occupant.Addr().(*net.TCPAddr).Port
	router := startACPServeRouter([]acpctl.ServePort{{Box: 4000, Host: stable}})
	defer router.Close()
	if ports := router.Ports(); ports[0].Taken {
		t.Fatalf("ports = %+v, want nothing taken while nothing serves", ports)
	}
	freed := make(chan []acpctl.ServePort, 1)
	router.mu.Lock()
	router.report = func(_, list []acpctl.ServePort) {
		if len(list) > 0 {
			freed <- list
		}
	}
	router.mu.Unlock()
	router.setLive(map[int]int{4000: fakeBoxPort(t, "the box")})
	serves(router, 4000, true)
	if ports := router.Ports(); !ports[0].Taken {
		t.Fatalf("ports = %+v, want the held port marked", ports)
	}
	_ = occupant.Close()
	select {
	case list := <-freed:
		if len(list) != 1 || list[0].Box != 4000 || list[0].Taken {
			t.Fatalf("freed = %+v", list)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session never took the freed port")
	}
	if got := readLink(t, stable); got != "the box" {
		t.Fatalf("after the port freed: link answered %q", got)
	}
	if router.Ports()[0].Taken {
		t.Fatal("the port still reads as taken")
	}
}

// A host server that restarts gets its port back: once the box server stops, the session stops asking.
func TestEditorServeLeavesAFreedPortAloneOnceTheBoxStopsServing(t *testing.T) {
	old := acpServeRetry
	acpServeRetry = 20 * time.Millisecond
	defer func() { acpServeRetry = old }()
	occupant, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stable := occupant.Addr().(*net.TCPAddr).Port
	router := startACPServeRouter([]acpctl.ServePort{{Box: 4000, Host: stable}})
	defer router.Close()
	router.setLive(map[int]int{4000: fakeBoxPort(t, "the box")})
	serves(router, 4000, true)
	serves(router, 4000, false)
	if router.Ports()[0].Taken {
		t.Fatal("a port no box server waits for still reads as taken")
	}
	_ = occupant.Close() // the host server restarts
	time.Sleep(10 * acpServeRetry)
	if !hostCanBind(t, stable) {
		t.Fatal("the session took the port while nothing served in its box")
	}
}

func TestEditorServeRouterGivesItsPortsBack(t *testing.T) {
	stable := freeLoopbackPort(t)
	router := startACPServeRouter([]acpctl.ServePort{{Box: 4000, Host: stable}})
	router.setLive(map[int]int{4000: fakeBoxPort(t, "the box")})
	serves(router, 4000, true)
	router.Close()
	serves(router, 4000, true) // a watcher line racing the end
	if !hostCanBind(t, stable) {
		t.Fatal("the stable port stayed held after the session ended")
	}
	var none *acpServeRouter
	none.setLive(map[int]int{1: 2}) // a session with no serve ports has no router
	none.Close()
	if binds, told, err := none.allocate(); binds != nil || told != "" || err != nil {
		t.Fatalf("allocate with no router = %v, %q, %v", binds, told, err)
	}
}

func TestServeBindsRoundTripAndRefuseNonsense(t *testing.T) {
	binds := map[int]int{4000: 52341, 9091: 52342}
	encoded := encodeServeBinds(binds, []acpctl.ServePort{{Box: 4000}, {Box: 9091, Taken: true}})
	if encoded != "4000=52341,9091=52342!" {
		t.Fatalf("encoded = %q", encoded)
	}
	got, taken, err := decodeServeBinds(encoded)
	if err != nil || len(got) != 2 || got[4000] != 52341 || got[9091] != 52342 || taken[4000] || !taken[9091] {
		t.Fatalf("decoded = %v, %v, %v", got, taken, err)
	}
	if got, taken, err := decodeServeBinds(""); got != nil || taken != nil || err != nil {
		t.Fatalf("empty = %v, %v, %v", got, taken, err)
	}
	for _, bad := range []string{"4000", "4000=", "=1", "a=b", "0=1", "1=70000", "4000=1,x", "4000=!"} {
		if _, _, err := decodeServeBinds(bad); err == nil {
			t.Errorf("decodeServeBinds(%q) accepted nonsense", bad)
		}
	}
}

// Each box the session spawns gets its own private ports, carried to its inner process, and they are
// never the stable port the session forwards.
func TestSpawnedBoxesPublishOnPrivatePorts(t *testing.T) {
	stable := freeLoopbackPort(t)
	router := startACPServeRouter([]acpctl.ServePort{{Box: 4000, Host: stable}, {Box: 9091, Host: freeLoopbackPort(t)}})
	defer router.Close()
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}
	a := &app{cfg: cfg, acpServe: router}
	shim := filepath.Join(t.TempDir(), "inner")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf '%s' \"$"+acpServeBindsEnv+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var seen []int
	for range 2 {
		child, err := a.spawnBox(context.Background(), shim, nil, "serve-scope", nil, agents.Target{Provider: "claude"}, "", true, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(child.Out)
		child.Stop()
		if err != nil {
			t.Fatal(err)
		}
		decoded, _, err := decodeServeBinds(string(out))
		if err != nil || len(decoded) != 2 {
			t.Fatalf("child env = %q, %v", out, err)
		}
		for box, private := range child.ServeBinds {
			if decoded[box] != private {
				t.Fatalf("the child's env %v disagrees with its record %v", decoded, child.ServeBinds)
			}
			if private == stable {
				t.Fatal("a box was given the session's stable port")
			}
			seen = append(seen, private)
		}
	}
	slices.Sort(seen)
	if len(slices.Compact(slices.Clone(seen))) < 3 {
		t.Errorf("boxes did not get ports of their own: %v", seen)
	}
}
