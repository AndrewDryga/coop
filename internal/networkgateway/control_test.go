package networkgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestPrivateControllerAPIAuthenticatesBoundsAndTerminates(t *testing.T) {
	var mu sync.Mutex
	var batches []string
	c := testController(t, func(_ context.Context, rules string) error {
		mu.Lock()
		batches = append(batches, rules)
		mu.Unlock()
		return nil
	})
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := shortControlPath(t)
	done := make(chan error, 1)
	go func() { done <- c.serveControl(ctx, path, func(*net.UnixConn) bool { return true }); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(wait.Deadline):
			t.Error("controller fixture leaked")
		}
	})
	client := ControllerClient{Path: path, Identity: c.identity, Clock: c.clock}
	deadline := time.Now().Add(wait.Deadline)
	for client.Ready(ctx) != nil {
		select {
		case err := <-done:
			t.Fatalf("server startup failed (%d-byte socket path): %v", len(path), err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("private server did not become ready")
		}
		time.Sleep(time.Millisecond)
	}
	lease := Lease{Name: "api.example.com", Port: 443, Peer: netip.MustParseAddr("93.184.216.34"), Expires: testBootNow().Add(time.Minute)}
	until, err := client.Admit(ctx, lease)
	if err != nil || !until.After(testBootNow()) || until.After(lease.Expires) {
		t.Fatalf("lease reply: %s %v", until, err)
	}
	lease.Name = "forbidden.example.net"
	if _, err := client.Admit(ctx, lease); err != Failure("gateway_lease_refused") {
		t.Fatalf("API changed refusal: %v", err)
	}
	if _, err := client.call(ctx, controlRequest{Version: 1, Operation: "open"}); err == nil {
		t.Fatal("unknown operation succeeded")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(wait.Deadline):
		t.Fatal("controller server did not stop")
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(batches[len(batches)-1], "add rule inet coop_net shutdown counter drop") || c.Ready() {
		t.Fatal("controller shutdown left established traffic enabled")
	}
}

func TestControllerSequentialRepliesReleaseKernelSlot(t *testing.T) {
	c := testController(t, func(context.Context, string) error { return nil })
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil {
		t.Fatal(err)
	}
	client, _, _ := startControlFixture(t, c, func(*net.UnixConn) bool { return true })
	for range 100 {
		lease := Lease{Name: "api.example.com", Port: 443, Peer: netip.MustParseAddr("93.184.216.34"), Expires: testBootNow().Add(time.Minute)}
		if _, err := client.Admit(context.Background(), lease); err != nil {
			t.Fatalf("sequential accepted request was spuriously busy: %v", err)
		}
		lease.Name = "forbidden.example.net"
		if _, err := client.Admit(context.Background(), lease); err != Failure("gateway_lease_refused") {
			t.Fatalf("reply arrived before mutation slot was released: %v", err)
		}
	}
}

func startControlFixture(t *testing.T, c *Controller, authenticate func(*net.UnixConn) bool) (ControllerClient, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	path := shortControlPath(t)
	done := make(chan error, 1)
	go func() { done <- c.serveControl(ctx, path, authenticate); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(wait.Deadline):
			t.Error("controller fixture leaked")
		}
	})
	client := ControllerClient{Path: path, Identity: c.identity, Clock: c.clock}
	wait.For(t, "controller private health listener", func() bool { return client.Ready(ctx) == nil })
	return client, cancel, done
}

func TestLeaseSlowFramesCannotStarveIndependentHeartbeat(t *testing.T) {
	c := testController(t, func(context.Context, string) error { return nil })
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	client, _, _ := startControlFixture(t, c, func(*net.UnixConn) bool { accepted.Add(1); return true })
	baseline := accepted.Load()
	for range maxControlClients {
		conn, err := net.Dial("unix", client.Path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if _, err := conn.Write([]byte("{")); err != nil {
			t.Fatal(err)
		}
	}
	wait.For(t, "all lease slots occupied", func() bool { return accepted.Load() >= baseline+maxControlClients })
	if _, err := client.call(context.Background(), controlRequest{Version: 1, Operation: "heartbeat"}); err != nil || !c.Ready() {
		t.Fatalf("lease slowframes killed heartbeat: %v", err)
	}
	// Even an authenticated request cannot route lease mutation through the
	// separate reserved listener by supplying a different operation field.
	conn, err := net.Dial("unix", client.Path+".health")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(ControlTimeout))
	request := controlRequest{Identity: c.identity, Version: 1, Operation: "lease", Lease: &Lease{Name: "api.example.com", Port: 443, Peer: netip.MustParseAddr("93.184.216.34"), Expires: testBootNow().Add(time.Minute)}}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		t.Fatal(err)
	}
	var reply controlReply
	if err := readControl(conn, &reply); err != nil || reply.Ready {
		t.Fatalf("lease entered health pool: %v %#v", err, reply)
	}
}

func TestPrivateControllerRejectsSwappedRunAndReportsShutdownUncertainty(t *testing.T) {
	c := testController(t, func(_ context.Context, rules string) error {
		if strings.HasPrefix(rules, "flush chain") {
			return errors.New("synthetic shutdown failure")
		}
		return nil
	})
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil {
		t.Fatal(err)
	}
	client, cancel, done := startControlFixture(t, c, func(*net.UnixConn) bool { return true })
	wrong := client
	wrong.Identity.Epoch = strings.Repeat("c", 32)
	if wrong.Ready(context.Background()) == nil {
		t.Fatal("wrong gateway epoch reported ready")
	}
	if _, err := wrong.Admit(context.Background(), Lease{Name: "api.example.com", Port: 443, Peer: netip.MustParseAddr("93.184.216.34"), Expires: testBootNow().Add(time.Minute)}); err == nil {
		t.Fatal("wrong gateway epoch granted a lease")
	}
	c.mu.Lock()
	count := len(c.leases)
	c.mu.Unlock()
	if count != 0 {
		t.Fatal("wrong epoch mutated controller")
	}
	cancel()
	select {
	case err := <-done:
		if err != Failure("enforcement_shutdown_unconfirmed") {
			t.Fatalf("shutdown uncertainty hidden: %v", err)
		}
	case <-time.After(wait.Deadline):
		t.Fatal("shutdown fixture leaked")
	}
}

func TestControlFramingRefusesLargeUnknownAndTrailingMessages(t *testing.T) {
	for _, invalid := range []string{
		`{"version":1,"operation":"ready","open":true}` + "\n",
		`{"version":1,"operation":"ready"} {}` + "\n",
		`{"version":1,"operation":"ready"}`,
		strings.Repeat(" ", maxControlBytes) + "{}\n",
	} {
		var request controlRequest
		if readControl(bytes.NewBufferString(invalid), &request) == nil {
			t.Fatalf("accepted invalid control message %q", invalid[:min(80, len(invalid))])
		}
	}
}

func TestControlPeerRefusalNeverReachesLeaseMutation(t *testing.T) {
	c := testController(t, func(context.Context, string) error { return nil })
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	path := shortControlPath(t)
	done := make(chan error, 1)
	go func() { done <- c.serveControl(ctx, path, func(*net.UnixConn) bool { return false }); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(wait.Deadline):
			t.Error("controller fixture leaked")
		}
	})
	deadline := time.Now().Add(wait.Deadline)
	for {
		select {
		case err := <-done:
			t.Fatalf("server startup failed (%d-byte socket path): %v", len(path), err)
		default:
		}
		conn, err := net.Dial("unix", path)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(wait.Deadline))
			_, _ = conn.Write([]byte("{\"version\":1,\"operation\":\"ready\"}\n"))
			var reply [1]byte
			n, _ := conn.Read(reply[:])
			_ = conn.Close()
			if n != 0 {
				t.Fatal("unauthenticated peer received successful response")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("controller fixture did not start")
		}
		time.Sleep(time.Millisecond)
	}
}

func shortControlPath(t *testing.T) string {
	t.Helper()
	// macOS sockaddr_un has a 104-byte path, while t.TempDir includes the
	// test name below an already long per-user temporary directory.
	root, err := os.MkdirTemp("/tmp", "coop-net-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	return filepath.Join(root, "c.sock")
}

func TestReadinessPollingDoesNotKeepDeadGuardAlive(t *testing.T) {
	c := testController(t, func(context.Context, string) error { return nil })
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	path := shortControlPath(t)
	done := make(chan error, 1)
	go func() { done <- c.serveControl(ctx, path, func(*net.UnixConn) bool { return true }); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(wait.Deadline):
			t.Error("controller fixture leaked")
		}
	})
	client := ControllerClient{Path: path, Identity: c.identity, Clock: c.clock}
	wait.For(t, "controller ready", func() bool { return client.Ready(ctx) == nil })
	if _, err := client.call(ctx, controlRequest{Version: 1, Operation: "heartbeat"}); err != nil {
		t.Fatal(err)
	}
	// No more actual guard heartbeats. An observer polling ready cannot hide
	// a paused/dead guard. This wait tests the declared local liveness window.
	started := time.Now()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(HeartbeatTimeout + ControlTimeout + 2*HeartbeatInterval)
	defer deadline.Stop()
	for {
		select {
		case <-done:
			if time.Since(started) < HeartbeatTimeout-100*time.Millisecond || c.Ready() {
				t.Fatal("liveness cutoff was early or not terminal")
			}
			return
		case <-ticker.C:
			_ = client.Ready(ctx)
		case <-deadline.C:
			t.Fatal("readiness polling extended guard heartbeat authority")
		}
	}
}
