package networkgateway

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func testController(t *testing.T, apply ApplyRules) *Controller {
	t.Helper()
	return newTestController(t, testPolicy(t), apply)
}

func newTestController(t *testing.T, policy egress.Snapshot, apply ApplyRules) *Controller {
	t.Helper()
	clock := testBootClock()
	c, err := NewController(Identity{Clock: clock.Domain(), RunID: strings.Repeat("a", 32), Epoch: strings.Repeat("b", 32), PolicyFingerprint: policy.Fingerprint}, policy, nil, nil, nil, netip.Addr{}, clock, apply)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTerminalCloseWinsAnInFlightSuccessfulLeaseCommit(t *testing.T) {
	applying, release := make(chan struct{}), make(chan struct{})
	c := testController(t, func(_ context.Context, rules string) error {
		if strings.HasPrefix(rules, "flush set") {
			close(applying)
			<-release
		}
		return nil
	})
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil {
		t.Fatal(err)
	}
	fixed := testBootNow()
	c.now = func() BootInstant { return fixed }
	admitted := make(chan error, 1)
	go func() {
		_, err := c.Admit(context.Background(), Lease{Name: "api.example.com", Port: 443, Peer: netip.MustParseAddr("93.184.216.34"), Expires: fixed.Add(time.Minute)})
		admitted <- err
	}()
	select {
	case <-applying:
	case <-time.After(wait.Deadline):
		t.Fatal("fixture apply did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- c.CloseAdmission(context.Background()) }()
	wait.For(t, "terminal readiness loss before waiting on nft", func() bool { return !c.Ready() })
	close(release)
	select {
	case err := <-admitted:
		if err == nil {
			t.Fatal("in-flight lease acknowledged after terminal close")
		}
	case <-time.After(wait.Deadline):
		t.Fatal("admission fixture leaked")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(wait.Deadline):
		t.Fatal("close fixture leaked")
	}
}

func TestLeaseRulesNeverEmitSubTickTimeoutOrClaimRoundingAsLifetime(t *testing.T) {
	now := testBootNow()
	peer := netip.MustParseAddr("93.184.216.34")
	rules, until := leaseRules(map[string]Lease{"short": {Peer: peer, Port: 443, Expires: now.Add(ControllerUpdateTimeout + 19*time.Millisecond)}}, now)
	if strings.Contains(rules, "add element") || len(until) != 0 {
		t.Fatal("sub-tick timeout could become permanent on older kernels")
	}
	rules, until = leaseRules(map[string]Lease{"normal": {Peer: peer, Port: 443, Expires: now.Add(time.Second)}}, now)
	if !strings.Contains(rules, "timeout 750ms") || until[netip.AddrPortFrom(peer, 443)] != now.Add(730*time.Millisecond) {
		t.Fatal("missing conservative tick allowance")
	}
}

func TestControllerInstallsDenyBeforeReadinessAndNeverReinitializes(t *testing.T) {
	var rules string
	c := testController(t, func(_ context.Context, value string) error { rules = value; return nil })
	if c.Ready() {
		t.Fatal("ready before kernel acknowledgement")
	}
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil || !c.Ready() {
		t.Fatalf("initialization: %v", err)
	}
	for _, required := range []string{"hook input priority 0; policy drop", "hook output priority 0; policy drop", "hook forward priority 0; policy drop",
		"priority -110", "meta nfproto ipv6", "meta skuid 65532 meta l4proto tcp ct state established accept",
		"meta skuid 65532 ip daddr . tcp dport @leases4 accept", "set leases4", "type ipv4_addr . inet_service", "flags timeout", "size 4096"} {
		if !strings.Contains(rules, required) {
			t.Errorf("missing enforcement invariant %q", required)
		}
	}
	if strings.Contains(rules, "ct state established,related accept") {
		t.Fatal("broad established bypass")
	}
	// The run's own namespace loopback is permitted for the agent and for the
	// guard's established replies. An UNSCOPED loopback accept would hand every
	// uid in the namespace a bypass, so every such rule names its uid.
	for _, line := range strings.Split(rules, "\n") {
		if strings.Contains(line, `oifname "lo" accept`) && !strings.Contains(line, "meta skuid") {
			t.Fatalf("unscoped loopback bypass: %s", line)
		}
	}
	if err := c.CloseAdmission(context.Background()); err != nil || c.Ready() {
		t.Fatal("terminal admission close failed")
	}
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err == nil {
		t.Fatal("controller repaired a terminal generation")
	}
}

func TestControllerLeaseValidationAndAtomicSharedIPRefresh(t *testing.T) {
	var batches []string
	c := testController(t, func(_ context.Context, value string) error { batches = append(batches, value); return nil })
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil {
		t.Fatal(err)
	}
	now := testBootNow()
	c.now = func() BootInstant { return now }
	lease := Lease{Name: "api.example.com", Port: 443, Peer: netip.MustParseAddr("93.184.216.34"), Expires: now.Add(10 * time.Second)}
	if until, err := c.Admit(context.Background(), lease); err != nil || !until.Equal(lease.Expires.Add(-ControllerUpdateTimeout-KernelTickAllowance)) {
		t.Fatal(err)
	}
	if len(batches) != 2 || batches[1] != "flush set inet coop_net leases4\nadd element inet coop_net leases4 { 93.184.216.34 . 443 timeout 9750ms }\n" {
		t.Fatalf("non-atomic or unexpected lease batch: %q", batches)
	}
	if _, err := c.Admit(context.Background(), lease); err != nil || len(batches) != 2 {
		t.Fatal("unchanged fresh lease caused kernel churn")
	}
	now = now.Add(2 * time.Second)
	other := Lease{Name: "one.services.example.com", Port: 443, Peer: lease.Peer, Expires: now.Add(20 * time.Second)}
	if _, err := c.Admit(context.Background(), other); err != nil || !strings.Contains(batches[2], "timeout 19750ms") || strings.Count(batches[2], lease.Peer.String()) != 1 {
		t.Fatalf("shared-IP lease union: %v %q", err, batches)
	}
	now = now.Add(2 * time.Second)
	third := Lease{Name: "two.services.example.com", Port: 443, Peer: netip.MustParseAddr("1.0.0.1"), Expires: now.Add(5 * time.Second)}
	if _, err := c.Admit(context.Background(), third); err != nil || !strings.Contains(batches[3], "93.184.216.34 . 443 timeout 17750ms") {
		t.Fatalf("refresh restarted old TTL: %v %q", err, batches)
	}
	for _, invalid := range []Lease{
		{Name: "other.example.net", Port: 443, Peer: lease.Peer, Expires: now.Add(time.Second)},
		{Name: "API.EXAMPLE.COM", Port: 443, Peer: lease.Peer, Expires: now.Add(time.Second)},
		{Name: lease.Name, Port: 443, Peer: netip.MustParseAddr("169.254.169.254"), Expires: now.Add(time.Second)},
		{Name: lease.Name, Port: 443, Peer: netip.MustParseAddr("::ffff:1.1.1.1"), Expires: now.Add(time.Second)},
		{Name: lease.Name, Port: 443, Peer: lease.Peer, Expires: now},
		{Name: lease.Name, Port: 443, Peer: lease.Peer, Expires: now.Add(MaxDNSTTL + time.Second)},
		// The port is checked against the frozen policy here too: the guard read
		// it from the kernel, and the privilege holder never takes that on trust.
		{Name: lease.Name, Port: 8443, Peer: lease.Peer, Expires: now.Add(time.Second)},
		{Name: lease.Name, Peer: lease.Peer, Expires: now.Add(time.Second)},
	} {
		before := len(batches)
		if _, err := c.Admit(context.Background(), invalid); err == nil || len(batches) != before {
			t.Fatalf("invalid lease reached kernel: %#v", invalid)
		}
	}
}

func TestControllerKernelFailureLosesReadinessInsteadOfAcknowledgingGrant(t *testing.T) {
	fail := false
	c := testController(t, func(context.Context, string) error {
		if fail {
			return errors.New("synthetic nft failure")
		}
		return nil
	})
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil {
		t.Fatal(err)
	}
	fail = true
	lease := Lease{Name: "api.example.com", Port: 443, Peer: netip.MustParseAddr("93.184.216.34"), Expires: testBootNow().Add(time.Minute)}
	if _, err := c.Admit(context.Background(), lease); err == nil || c.Ready() || len(c.leases) != 0 {
		t.Fatal("uncertain transaction acknowledged or retained readiness")
	}
	fail = false
	if _, err := c.Admit(context.Background(), lease); err == nil {
		t.Fatal("controller resumed granting after readiness loss")
	}
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err == nil {
		t.Fatal("controller reinitialized under a potentially live agent")
	}
}

func TestControllerAcceptedUpdateOutlivesOnlyItsRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	applying := false
	c := testController(t, func(updateCtx context.Context, _ string) error {
		if applying {
			cancel()
			if updateCtx.Err() != nil {
				return updateCtx.Err()
			}
			if _, bounded := updateCtx.Deadline(); !bounded {
				t.Fatal("accepted transaction has no controller-owned deadline")
			}
		}
		return nil
	})
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil {
		t.Fatal(err)
	}
	applying = true
	if _, err := c.Admit(ctx, Lease{Name: "api.example.com", Port: 443, Peer: netip.MustParseAddr("93.184.216.34"), Expires: testBootNow().Add(time.Minute)}); err != nil || !c.Ready() {
		t.Fatalf("one disconnected tool disabled the enforcer: %v", err)
	}
}

func TestControllerDelayedCommitCannotExtendOldDNSAuthority(t *testing.T) {
	now := testBootNow()
	var applying bool
	var batch string
	c := testController(t, func(_ context.Context, rules string) error {
		batch = rules
		if applying {
			now = now.Add(200 * time.Millisecond)
		}
		return nil
	})
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil {
		t.Fatal(err)
	}
	c.now = func() BootInstant { return now }
	applying = true
	start := now
	lease := Lease{Name: "api.example.com", Port: 443, Peer: netip.MustParseAddr("93.184.216.34"), Expires: start.Add(time.Second)}
	until, err := c.Admit(context.Background(), lease)
	if err != nil || !until.Equal(start.Add(730*time.Millisecond)) || !strings.Contains(batch, "timeout 750ms") {
		t.Fatalf("short-TTL conservative admission: %v %s %s", err, until, batch)
	}
	// Add another peer just before the old DNS answer expires. The old entry
	// must be omitted rather than reinstalled for a relative stale lifetime.
	now = start.Add(850 * time.Millisecond)
	other := Lease{Name: "one.services.example.com", Port: 443, Peer: netip.MustParseAddr("1.0.0.1"), Expires: now.Add(time.Minute)}
	if _, err := c.Admit(context.Background(), other); err != nil || strings.Contains(batch, lease.Peer.String()) {
		t.Fatalf("delayed update extended old DNS: %v %s", err, batch)
	}
	if _, err := c.Admit(context.Background(), lease); err == nil {
		t.Fatal("expired lease fast path succeeded")
	}
}

func TestControllerLateSuccessfulApplyStillLosesReadiness(t *testing.T) {
	now := testBootNow()
	applying := false
	c := testController(t, func(context.Context, string) error {
		if applying {
			now = now.Add(ControllerUpdateTimeout + time.Millisecond)
		}
		return nil
	})
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil {
		t.Fatal(err)
	}
	c.now = func() BootInstant { return now }
	applying = true
	if _, err := c.Admit(context.Background(), Lease{Name: "api.example.com", Port: 443, Peer: netip.MustParseAddr("93.184.216.34"), Expires: now.Add(time.Minute)}); err == nil || c.Ready() {
		t.Fatal("late acknowledgement retained enforcement readiness")
	}
}
