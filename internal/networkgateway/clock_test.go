package networkgateway

import (
	"context"
	"encoding/json"
	"errors"
	"golang.org/x/net/dns/dnsmessage"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func testBootNow() BootInstant { return BootInstant(time.Now().UnixNano()) }
func testBootClock() *BootClock {
	return &BootClock{domain: ClockDomain{BootID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", TimeNamespace: "12345", BootOffset: "0:0"}, read: func() (BootInstant, error) { return testBootNow(), nil }}
}

func TestResolverClockFailureAndSuspendCannotReuseCachedAuthority(t *testing.T) {
	now := BootInstant(time.Hour)
	calls := 0
	r := newTestResolver(t, answerExchange(t, func(name string) []dnsmessage.Resource {
		calls++
		return []dnsmessage.Resource{aRecord(name, "93.184.216.34", 1)}
	}))
	r.now = func() BootInstant { return now }
	if _, err := r.Resolve(context.Background(), "api.example.com"); err != nil {
		t.Fatal(err)
	}
	now = 0
	if _, err := r.Resolve(context.Background(), "api.example.com"); err != Failure("clock_unavailable") || calls != 1 {
		t.Fatalf("clock failure reused cache or queried: %v %d", err, calls)
	}
	now = BootInstant(2 * time.Hour)
	if got, err := r.Resolve(context.Background(), "api.example.com"); err != nil || got.Cached || calls != 2 {
		t.Fatalf("suspend retained stale DNS: %#v %v", got, err)
	}
}

func TestBootClockDomainMismatchCannotConstructControllerOrClient(t *testing.T) {
	clock := testBootClock()
	policy := testPolicy(t)
	identity := Identity{Clock: clock.Domain(), RunID: strings.Repeat("a", 32), Epoch: strings.Repeat("b", 32), PolicyFingerprint: policy.Fingerprint}
	if _, err := NewController(identity, 1000, policy, nil, nil, nil, nil, netip.Addr{}, nil, clock, func(context.Context, string) error { return nil }); err != nil {
		t.Fatalf("matching controller clock positive control: %v", err)
	}
	path := shortControlPath(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path + ".health", Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	var connections atomic.Int32
	serverIdentity := identity
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			connections.Add(1)
			_ = conn.SetDeadline(time.Now().Add(ControlTimeout))
			var request controlRequest
			if readControl(conn, &request) == nil && request.Version == PrivateProtocolVersion && request.Operation == "ready" && request.Identity.matchesPeer(serverIdentity) {
				_ = json.NewEncoder(conn).Encode(controlReply{Identity: serverIdentity, Version: PrivateProtocolVersion, Ready: true})
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(wait.Deadline):
			t.Error("clock health fixture did not drain")
		}
	})
	client := ControllerClient{Path: path, Identity: identity, Clock: clock}
	if err := client.Ready(t.Context()); err != nil || connections.Load() != 1 {
		t.Fatalf("matching client health positive control: %v, connections%d", err, connections.Load())
	}
	identity.Clock.TimeNamespace = "54321"
	if _, err := NewController(identity, 1000, policy, nil, nil, nil, nil, netip.Addr{}, nil, clock, func(context.Context, string) error { return nil }); err == nil {
		t.Fatal("controller accepted a different time namespace")
	}
	client.Identity = identity
	if err := client.Ready(t.Context()); err != Failure("enforcement_unavailable") || connections.Load() != 1 {
		t.Fatalf("mismatched local clock reached health: %v, connections%d", err, connections.Load())
	}
}

func TestBootInstantIsExactCheckedAndNeverAWallClockDeadline(t *testing.T) {
	for _, value := range []BootInstant{0, 1, 1<<53 + 1, 1<<63 - 1} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var got BootInstant
		if err := json.Unmarshal(data, &got); err != nil || got != value {
			t.Fatalf("boot deadline lost precision: %s %v", data, err)
		}
	}
	for _, invalid := range []string{`1`, `null`, `"-1"`, `"01"`, `"+1"`, `"1.0"`, `"9223372036854775808"`} {
		var value BootInstant
		if json.Unmarshal([]byte(invalid), &value) == nil {
			t.Fatalf("accepted invalid boot deadline %s", invalid)
		}
	}
	if BootInstant(1<<63-1).Add(time.Second) != 0 || BootInstant(1).Add(-time.Second) != 0 || BootInstant(0).Before(1) {
		t.Fatal("invalid clock arithmetic produced authority")
	}
}

func TestControllerBootClockFailureAndSuspendAreTerminal(t *testing.T) {
	for _, failure := range []string{"read failure", "suspend"} {
		t.Run(failure, func(t *testing.T) {
			now := BootInstant(time.Hour)
			clock := testBootClock()
			clock.read = func() (BootInstant, error) {
				if now == 0 {
					return 0, errors.New("synthetic clock failure")
				}
				return now, nil
			}
			policy := testPolicy(t)
			identity := Identity{Clock: clock.Domain(), RunID: strings.Repeat("a", 32), Epoch: strings.Repeat("b", 32), PolicyFingerprint: policy.Fingerprint}
			updating := false
			c, err := NewController(identity, 1000, policy, nil, nil, nil, nil, netip.Addr{}, nil, clock, func(ctx context.Context, _ string) error {
				if updating {
					if failure == "suspend" {
						now = now.Add(time.Minute)
					} else {
						now = 0
					}
					if ctx.Err() != nil {
						t.Fatal("fixture relied on Go timer expiry to notice boot-clock loss")
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil {
				t.Fatal(err)
			}
			updating = true
			if _, err := c.Admit(context.Background(), Lease{Name: "api.example.com", Port: 443, Peer: netip.MustParseAddr("93.184.216.34"), Expires: now.Add(time.Minute)}); err == nil || c.Ready() {
				t.Fatal("uncertain boot-clock update retained authority")
			}
		})
	}
}
