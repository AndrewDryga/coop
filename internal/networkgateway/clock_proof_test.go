package networkgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestClockProofRequiresCompleteCanonicalOffsetsAndStableNamespace(t *testing.T) {
	boot := testBootClock().Domain().BootID
	for _, test := range []struct {
		name, offsets                             string
		changed, namespaceError, readError, valid bool
	}{
		{name: "zero", offsets: "monotonic 0 0\nboottime 0 0\n", valid: true},
		{name: "nonzero", offsets: "monotonic -1 100\nboottime 2 3\n", valid: true},
		{name: "missing", offsets: "monotonic 0 0\n"},
		{name: "duplicate", offsets: "monotonic 0 0\nboottime 0 0\nboottime 0 0\n"},
		{name: "unknown", offsets: "monotonic 0 0\nboottime 0 0\nother 0 0\n"},
		{name: "noncanonical", offsets: "monotonic 0 0\nboottime +0 0\n"},
		{name: "negative-zero", offsets: "monotonic 0 0\nboottime -0 0\n"},
		{name: "overflow", offsets: "monotonic 0 0\nboottime 9223372036854775808 0\n"},
		{name: "nanos", offsets: "monotonic 0 0\nboottime 0 1000000000\n"},
		{name: "negative-nanos", offsets: "monotonic 0 0\nboottime 0 -1\n"},
		{name: "oversized", offsets: strings.Repeat(" ", 4097)},
		{name: "namespace-change", offsets: "monotonic 0 0\nboottime 0 0\n", changed: true},
		{name: "namespace-error", namespaceError: true},
		{name: "read-error", readError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reads := 0
			domain, err := qualifyClockDomain(boot, func() (clockNamespace, error) {
				reads++
				if test.namespaceError {
					return clockNamespace{}, errors.New("inert namespace failure")
				}
				inode := uint64(12345)
				if test.changed && reads > 1 {
					inode++
				}
				return clockNamespace{device: 5, inode: inode}, nil
			}, func() ([]byte, error) {
				if test.readError {
					return nil, errors.New("inert offset failure")
				}
				return []byte(test.offsets), nil
			})
			if (err == nil) != test.valid || test.valid && !domain.Valid() {
				t.Fatalf("clock proof valid=%t error=%v", domain.Valid(), err)
			}
		})
	}
}

func TestClockPeerEquivalenceRetainsAllIdentityDenials(t *testing.T) {
	c := testController(t, func(context.Context, string) error { return nil })
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil {
		t.Fatal(err)
	}
	client, _, _ := startControlFixture(t, c, func(*net.UnixConn) bool { return true })
	for _, test := range []struct {
		name   string
		change func(*Identity)
	}{
		{"boot", func(i *Identity) { i.Clock.BootID = "cccccccc-bbbb-cccc-dddd-eeeeeeeeeeee" }},
		{"unknown-offset", func(i *Identity) { i.Clock.BootOffset = "" }},
		{"offset", func(i *Identity) { i.Clock.BootOffset = "0:1" }},
		{"noncanonical-offset", func(i *Identity) { i.Clock.BootOffset = "00:0" }},
		{"run", func(i *Identity) { i.RunID = strings.Repeat("f", 32) }},
		{"epoch", func(i *Identity) { i.Epoch = strings.Repeat("f", 32) }},
		{"policy", func(i *Identity) { i.PolicyFingerprint = strings.Repeat("f", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			wrong := client
			wrong.Identity.Clock.TimeNamespace = "67890"
			test.change(&wrong.Identity)
			clock := *client.Clock
			clock.domain = wrong.Identity.Clock
			wrong.Clock = &clock
			if wrong.Ready(context.Background()) == nil {
				t.Fatal("unsafe peer reported ready")
			}
		})
	}
	if _, err := client.call(context.Background(), controlRequest{Version: 1, Operation: "ready"}); err == nil {
		t.Fatal("legacy protocol admitted unqualified clock")
	}
	one := client.Identity.Clock
	one.BootOffset = "1:2"
	other := one
	if !one.matchesPeer(other) {
		t.Fatal("same qualified nonzero namespace refused")
	}
	other.TimeNamespace = "67890"
	if one.matchesPeer(other) {
		t.Fatal("nonzero clock crossed namespaces")
	}
}

func TestObservationPeerEquivalenceDoesNotRebindHostCustody(t *testing.T) {
	c, now := collectorFixture(t)
	publishFixture(c, *now, nil)
	g := &GuardRuntime{Identity: c.identity, collector: c}
	path := shortControlPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.serveObservation(ctx, path, func(*net.UnixConn) bool { return true }) }()
	t.Cleanup(func() { cancel(); <-done })
	peer := g.Identity
	peer.Clock.TimeNamespace = "67890"
	// observeAt uses private peer validation, while the host retains exact identity.
	wait.For(t, "equivalent observer listener", func() bool { _, err := os.Lstat(path); return err == nil })
	value, err := observeAt(ctx, path, peer)
	if err != nil || value.Identity != g.Identity {
		t.Fatalf("equivalent observer lost guard provenance: %v", err)
	}
	data, err := encodeObservation(g.observation())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadObservation(bytes.NewReader(data), peer); err == nil {
		t.Fatal("host custody accepted a different guard namespace")
	}
	if _, err := ReadObservation(bytes.NewReader(data), g.Identity); err != nil {
		t.Fatal(err)
	}
}

func TestControllerRejectsUnqualifiedClockFramesWithoutAuthority(t *testing.T) {
	for _, endpoint := range []string{"lease", "health"} {
		t.Run(endpoint, func(t *testing.T) {
			var applies, heartbeats atomic.Int32
			c := testController(t, func(context.Context, string) error { applies.Add(1); return nil })
			if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil {
				t.Fatal(err)
			}
			baseline := applies.Load()
			listener, err := controlListener(shortControlPath(t))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- c.controlConnections(ctx, listener, endpoint, func(*net.UnixConn) bool { return true }, func() { heartbeats.Add(1) })
			}()
			t.Cleanup(func() {
				cancel()
				_ = listener.Close()
				select {
				case <-done:
				case <-time.After(wait.Deadline):
					t.Error("clock denial fixture leaked")
				}
			})
			request := controlRequest{Identity: c.identity, Version: PrivateProtocolVersion, Operation: "heartbeat"}
			if endpoint == "lease" {
				request.Operation = "lease"
				request.Lease = &Lease{Name: "api.example.com", Port: 443, Peer: netip.MustParseAddr("93.184.216.34"), Expires: testBootNow().Add(time.Minute)}
			}
			for _, offset := range []string{"", "00:0", "0:1", "1:0", "0:1000000000"} {
				wrong := request
				wrong.Identity.Clock.TimeNamespace = "67890"
				wrong.Identity.Clock.BootOffset = offset
				// Send raw frames, bypassing the client's own proof validation.
				if reply := rawClockControl(t, listener.Addr().String(), wrong); reply.Ready {
					t.Fatalf("unsafe offset %q admitted", offset)
				}
			}
			if applies.Load() != baseline || heartbeats.Load() != 0 {
				t.Fatal("denied clock proof mutated leases or renewed heartbeat authority")
			}
			request.Identity.Clock.TimeNamespace = "67890"
			if !rawClockControl(t, listener.Addr().String(), request).Ready {
				t.Fatal("qualified zero-offset control refused")
			}
			if endpoint == "health" && heartbeats.Load() != 1 || endpoint == "lease" && applies.Load() <= baseline {
				t.Fatal("positive control did not exercise authority")
			}
		})
	}
}

func rawClockControl(t *testing.T, path string, request controlRequest) controlReply {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, wait.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(wait.Deadline)); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		t.Fatal(err)
	}
	var reply controlReply
	if err := readControl(conn, &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func TestControllerRejectsLegacyReply(t *testing.T) {
	path := shortControlPath(t) + ".health"
	listener, err := controlListener(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	c := testController(t, func(context.Context, string) error { return nil })
	clock, identity := c.clock, c.identity
	done := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(wait.Deadline)); err != nil {
			done <- err
			return
		}
		var request controlRequest
		if err := readControl(conn, &request); err != nil {
			done <- err
			return
		}
		done <- json.NewEncoder(conn).Encode(controlReply{Identity: identity, Version: 1, Ready: true})
	}()
	client := ControllerClient{Path: strings.TrimSuffix(path, ".health"), Identity: identity, Clock: clock}
	if client.Ready(context.Background()) == nil {
		t.Fatal("legacy reply admitted unqualified clock")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(wait.Deadline):
		t.Fatal("legacy reply fixture did not finish")
	}
}
