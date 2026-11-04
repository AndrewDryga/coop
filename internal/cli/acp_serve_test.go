package cli

import (
	"context"
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

// The scenario the router exists for: a warm box and a thread that switches. The same link reaches
// whichever box the thread uses, at the same address, and a parked box is out of reach.
func TestEditorServeLinkFollowsTheLiveBox(t *testing.T) {
	stable := freeLoopbackPort(t)
	router := startACPServeRouter([]acpctl.ServePort{{Box: 4000, Host: stable}})
	defer router.Close()
	if got := readLink(t, stable); got != "" {
		t.Fatalf("with no live box the link answered %q", got)
	}
	claude := map[int]int{4000: fakeBoxPort(t, "claude box")}
	codex := map[int]int{4000: fakeBoxPort(t, "codex box")}
	router.setLive(claude)
	if got := readLink(t, stable); got != "claude box" {
		t.Fatalf("live claude box: link answered %q", got)
	}
	// The thread switches to the warm codex box, and a refresh replacement goes the same way.
	router.setLive(codex)
	if got := readLink(t, stable); got != "codex box" {
		t.Fatalf("after the switch: link answered %q", got)
	}
	replacement := map[int]int{4000: fakeBoxPort(t, "replacement box")}
	router.setLive(replacement)
	if got := readLink(t, stable); got != "replacement box" {
		t.Fatalf("after the refresh: link answered %q", got)
	}
}

// A browser's open connection must not keep talking to a box the thread has left.
func TestEditorServeSwitchEndsConnectionsToTheOldBox(t *testing.T) {
	stable := freeLoopbackPort(t)
	router := startACPServeRouter([]acpctl.ServePort{{Box: 4000, Host: stable}})
	defer router.Close()
	router.setLive(map[int]int{4000: fakeBoxPort(t, "old")})
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
	if _, err := conn.Read(make([]byte, 8)); err == nil {
		t.Fatal("the connection to the old box stayed open across the switch")
	}
}

// Something else holding the port is told, with no link, and the session takes the port once it is
// free. The thread then hears the link works.
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
	if ports := router.Ports(); !ports[0].Taken {
		t.Fatalf("ports = %+v, want the held port marked", ports)
	}
	freed := make(chan []acpctl.ServePort, 1)
	router.mu.Lock()
	router.report = func(_, list []acpctl.ServePort) { freed <- list }
	router.mu.Unlock()
	router.setLive(map[int]int{4000: fakeBoxPort(t, "the box")})
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

func TestEditorServeRouterGivesItsPortsBack(t *testing.T) {
	stable := freeLoopbackPort(t)
	router := startACPServeRouter([]acpctl.ServePort{{Box: 4000, Host: stable}})
	router.Close()
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(stable)))
	if err != nil {
		t.Fatalf("the stable port stayed held after the session ended: %v", err)
	}
	_ = listener.Close()
	var none *acpServeRouter
	none.setLive(map[int]int{1: 2}) // a session with no serve ports has no router
	none.Close()
	if binds, err := none.allocate(); binds != nil || err != nil {
		t.Fatalf("allocate with no router = %v, %v", binds, err)
	}
}

func TestServeBindsRoundTripAndRefuseNonsense(t *testing.T) {
	binds := map[int]int{4000: 52341, 9091: 52342}
	encoded := encodeServeBinds(binds)
	if encoded != "4000=52341,9091=52342" {
		t.Fatalf("encoded = %q", encoded)
	}
	got, err := decodeServeBinds(encoded)
	if err != nil || len(got) != 2 || got[4000] != 52341 || got[9091] != 52342 {
		t.Fatalf("decoded = %v, %v", got, err)
	}
	if got, err := decodeServeBinds(""); got != nil || err != nil {
		t.Fatalf("empty = %v, %v", got, err)
	}
	for _, bad := range []string{"4000", "4000=", "=1", "a=b", "0=1", "1=70000", "4000=1,x"} {
		if _, err := decodeServeBinds(bad); err == nil {
			t.Errorf("decodeServeBinds(%q) accepted nonsense", bad)
		}
	}
}

// Each box the session spawns gets its own private ports, carried to its inner process, and they are
// never the stable port the session holds.
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
		decoded, err := decodeServeBinds(string(out))
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
