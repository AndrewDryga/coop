package box

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/runtime"
)

type cancelledVolumeDaemon struct {
	*filteredDaemonFixture
	cancel context.CancelFunc
}

func (d cancelledVolumeDaemon) CreateVolume(ctx context.Context, ref runtime.DockerRef) (runtime.DockerVolume, error) {
	volume, err := d.filteredDaemonFixture.CreateVolume(ctx, ref)
	d.cancel()
	return volume, err
}

func TestFilteredConfirmedVolumeSurvivesCallerCancellation(t *testing.T) {
	f, daemon := filteredFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.docker = cancelledVolumeDaemon{filteredDaemonFixture: daemon, cancel: cancel}
	if err := f.createVolume(ctx, "ipc"); !errors.Is(err, context.Canceled) {
		t.Fatal("successful create hid caller cancellation", err)
	}
	record, err := f.store.Execution(f.record.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range record.Resources {
		if resource.Role == "ipc" && (resource.State != "created" || resource.ID != resource.Name) {
			t.Fatal("confirmed volume identity lost after cancellation", resource)
		}
	}
	if gone, err := f.cleanup("launch_failed"); err != nil || !gone || len(daemon.volumes) != 0 {
		t.Fatal("cancelled create left cleanup debt", gone, err)
	}
}

func TestFilteredUnsubmittedCreationLeavesNoCleanupDebt(t *testing.T) {
	for _, role := range []string{"ipc", "observations", "controller", "guard", "agent"} {
		t.Run(role, func(t *testing.T) {
			f, d := filteredFixture(t)
			d.notAttempted = role
			_, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard)
			if !errors.Is(err, runtime.ErrDockerCreateNotAttempted) {
				t.Fatal("fixture did not stop before create submission", err)
			}
			gone, err := f.cleanup("launch_failed")
			if err != nil || !gone || len(d.volumes) != 0 || len(d.containers) != 0 {
				t.Fatal("unsubmitted create leaked runtime custody", gone, err)
			}
			record, err := f.store.Execution(f.record.ID)
			if err != nil || record.Receipt == nil || record.Receipt.Cleanup != "complete" {
				t.Fatal("unsubmitted create left a pending receipt", err)
			}
		})
	}
}

func TestFilteredTransientHealthProbeDoesNotKillHealthyWork(t *testing.T) {
	f, d := filteredFixture(t)
	d.holdAgent, d.transientProbes = true, 2
	d.probeRecovered = make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.launch(ctx, RunSpec{}, nil, nil, io.Discard, io.Discard); done <- err }()
	select {
	case <-d.probeRecovered:
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation was lost", err)
		}
	case err := <-done:
		t.Fatal("transient daemon loss killed healthy work", err)
	case <-ctx.Done():
		<-done
		t.Fatal("fixture health never recovered")
	}
	if gone, err := f.cleanup("cancelled"); err != nil || !gone || d.stopGrace["agent"] != 10 {
		t.Fatal("provider lost its bounded graceful stop", gone, err, d.stopGrace)
	}
}

// A topology the run cannot follow still ends it: an unreadable or empty host inventory, a kernel
// update the controller refuses, or an envelope past the qualified cap. Growth that CAN be applied
// is covered by the tests below.
func TestFilteredHostTopologyDriftRemainsFailClosed(t *testing.T) {
	for _, kind := range []string{"unavailable", "empty", "refused", "capped"} {
		t.Run(kind, func(t *testing.T) {
			f, d := filteredFixture(t)
			if err := f.reconcileTopology(context.Background()); err != nil {
				t.Fatal("initial fixture topology", err)
			}
			installed := slices.Clone(f.protected)
			failure := errors.New("fixture host inventory unavailable")
			f.hostAddresses = func() ([]netip.Prefix, error) {
				switch kind {
				case "unavailable":
					return nil, failure
				case "empty":
					return nil, nil
				case "capped":
					var many []netip.Prefix
					for i := 0; i <= networkgateway.MaxProtectedRanges; i++ {
						many = append(many, netip.PrefixFrom(netip.AddrFrom4([4]byte{198, 51, byte(i / 256), byte(i % 256)}), 32))
					}
					return many, nil
				}
				return []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32"), netip.MustParsePrefix("192.0.2.2/32")}, nil
			}
			if kind == "refused" {
				d.applyErr = errors.New("fixture nft refused the element")
			}
			err := f.reconcileTopology(context.Background())
			if err == nil {
				t.Fatal("unreconcilable topology was ignored")
			}
			if kind == "refused" && !strings.Contains(err.Error(), "192.0.2.2/32") {
				t.Fatal("refusal does not name the address", err)
			}
			if !slices.Equal(f.protected, installed) {
				t.Fatal("a failed reconciliation changed the recorded envelope")
			}
			_, err = f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard)
			if err == nil || f.startAttempted {
				t.Fatal("unreconcilable topology started workload", err)
			}
			if gone, err := f.cleanup("launch_failed"); err != nil || !gone || len(d.containers) != 0 || len(d.volumes) != 0 {
				t.Fatal("topology refusal lost cleanup", err)
			}
		})
	}
}

// A new host address is added to the live kernel set and the record, and the run goes on.
func TestFilteredNewHostAddressGrowsProtectionInPlace(t *testing.T) {
	f, d := filteredFixture(t)
	seeded := slices.Clone(f.protected)
	added := netip.MustParsePrefix("192.0.2.2/32")
	f.hostAddresses = func() ([]netip.Prefix, error) {
		return []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32"), added}, nil
	}
	if err := f.reconcileTopology(context.Background()); err != nil {
		t.Fatal("a new host address ended the run instead of growing its protection", err)
	}
	want := "/usr/sbin/nft " + networkgateway.ProtectedSetUpdate(append(slices.Clone(seeded), added))
	if len(d.applied) != 1 || d.applied[0] != want {
		t.Fatalf("kernel update = %v, want exactly %q", d.applied, want)
	}
	if !slices.Contains(f.protected, added) || len(f.protected) != len(seeded)+1 {
		t.Fatal("grown envelope lost an address", f.protected)
	}
	for _, prior := range seeded {
		if !slices.Contains(f.protected, prior) {
			t.Fatal("growth dropped an installed prefix", prior)
		}
	}
	record, err := f.store.Execution(f.record.ID)
	if err != nil || !slices.Contains(record.Protected, added) {
		t.Fatal("grown envelope was not recorded for a later `why`", err, record.Protected)
	}
	if _, err := f.store.RecordProtected(context.Background(), record.ID, record.Revision, seeded); err == nil {
		t.Fatal("the recorded envelope shrank")
	}
	if err := f.reconcileTopology(context.Background()); err != nil || len(d.applied) != 1 {
		t.Fatal("an unchanged topology re-rendered the set", err, d.applied)
	}
	code, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard)
	if err != nil || code != 7 || len(d.applied) != 1 {
		t.Fatal("grown protection stopped normal work", code, err, d.applied)
	}
	if gone, err := f.cleanup("exited"); err != nil || !gone || len(d.containers) != 0 || len(d.volumes) != 0 {
		t.Fatal("growth lost cleanup", err)
	}
}

// Another filtered run's network is a subnet AND a gateway; both are protected, not just the host
// address OrbStack adds for it.
func TestFilteredNewRuntimeNetworkGrowsProtectionInPlace(t *testing.T) {
	f, d := filteredFixture(t)
	d.extraNetworks = []runtime.DockerNetwork{{Name: "coop-net-other", Subnets: []netip.Prefix{netip.MustParsePrefix("192.168.166.0/24")},
		Gateways: []netip.Addr{netip.MustParseAddr("192.168.166.1")}}}
	if err := f.reconcileTopology(context.Background()); err != nil {
		t.Fatal("a new runtime network ended the run", err)
	}
	if len(d.applied) != 1 || !strings.Contains(d.applied[0], "192.168.166.0/24") || !strings.Contains(d.applied[0], "192.168.166.1/32") {
		t.Fatalf("kernel update = %v, want the new subnet and its gateway", d.applied)
	}
	if !slices.Contains(f.protected, netip.MustParsePrefix("192.168.166.0/24")) || !slices.Contains(f.protected, netip.MustParsePrefix("192.168.166.1/32")) {
		t.Fatal("grown envelope lacks the new network", f.protected)
	}
}

// The gateway drops IPv6 outright, so a new IPv6 address (a rotating privacy address, a VPN) is
// recorded and needs no kernel update — and must not end the run.
func TestFilteredNewIPv6HostAddressNeedsNoKernelUpdate(t *testing.T) {
	f, d := filteredFixture(t)
	added := netip.MustParsePrefix("2001:db8::1/128")
	f.hostAddresses = func() ([]netip.Prefix, error) {
		return []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32"), added}, nil
	}
	if err := f.reconcileTopology(context.Background()); err != nil || len(d.applied) != 0 {
		t.Fatal("an IPv6 address ended the run or touched the IPv4 set", err, d.applied)
	}
	if !slices.Contains(f.protected, added) {
		t.Fatal("new IPv6 address was not recorded", f.protected)
	}
	if err := f.reconcileTopology(context.Background()); err != nil || len(d.applied) != 0 {
		t.Fatal("recorded IPv6 address was reported again", err, d.applied)
	}
}

// The watch grows the envelope under live work, through the controller, without stopping the box.
func TestFilteredWatchGrowsProtectionWhileWorkRuns(t *testing.T) {
	f, d := filteredFixture(t)
	d.holdAgent = true
	added := netip.MustParsePrefix("192.0.2.9/32")
	var grow atomic.Bool
	f.hostAddresses = func() ([]netip.Prefix, error) {
		host := []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}
		if grow.Load() {
			host = append(host, added)
		}
		return host, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.launch(ctx, RunSpec{}, nil, nil, io.Discard, io.Discard); done <- err }()
	<-d.attached
	grow.Store(true)
	deadline := time.Now().Add(10 * time.Second)
	for {
		d.mu.Lock()
		applied := len(d.applied)
		d.mu.Unlock()
		if applied == 1 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the watch never grew the protected set")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("growth changed the run's outcome", err)
	}
	if !strings.Contains(d.applied[0], added.String()) || !slices.Contains(f.protected, added) {
		t.Fatal("grown envelope lacks the address the watch saw", d.applied, f.protected)
	}
	if record, err := f.store.Execution(f.record.ID); err != nil || !slices.Contains(record.Protected, added) {
		t.Fatal("watch growth was not recorded", err)
	}
	if gone, err := f.cleanup("cancelled"); err != nil || !gone {
		t.Fatal("growth lost cleanup", gone, err)
	}
}

func TestFilteredRemovedHostAddressKeepsOriginalProtection(t *testing.T) {
	f, d := filteredFixture(t)
	seeded := len(f.protected)
	removed := netip.MustParsePrefix("192.0.2.2/32")
	f.protected = append(f.protected, removed)
	f.hostAddresses = func() ([]netip.Prefix, error) { return []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}, nil }
	if err := f.reconcileTopology(context.Background()); err != nil {
		t.Fatal("removing an address falsely weakened the retained deny set", err)
	}
	if len(f.protected) != seeded+1 || !slices.Contains(f.protected, removed) || len(d.applied) != 0 {
		t.Fatal("topology check changed installed protection", f.protected, d.applied)
	}
	f.hostAddresses = func() ([]netip.Prefix, error) { return append([]netip.Prefix{}, f.protected...), nil }
	if err := f.reconcileTopology(context.Background()); err != nil {
		t.Fatal("original protected address could not reappear", err)
	}
	code, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard)
	if err != nil || code != 7 {
		t.Fatal("safe interface removal stopped normal work", code, err)
	}
	if gone, err := f.cleanup("exited"); err != nil || !gone || len(d.containers) != 0 || len(d.volumes) != 0 {
		t.Fatal("interface removal lost cleanup", err)
	}
}

func TestFilteredDuplicateHardeningCannotCauseFalseRefusal(t *testing.T) {
	f, d := filteredFixture(t)
	options := []string{"--cap-drop", "ALL", "--security-opt", "no-new-privileges"}
	code, err := f.launch(context.Background(), RunSpec{}, options, nil, io.Discard, io.Discard)
	if err != nil || code != 7 {
		t.Fatal("equivalent duplicate hardening was rejected", code, err)
	}
	if len(d.containers[f.ref("agent").Name].CapDrop) != 2 {
		t.Fatal("fixture normalized away the adversarial daemon shape")
	}
	if gone, err := f.cleanup("exited"); err != nil || !gone {
		t.Fatal("cleanup", err)
	}
}

func TestFilteredNamedVolumesAreAdmittedOnlyAfterExposureInspection(t *testing.T) {
	cfg := &config.Config{BaseImage: "base", HomeInBox: "/home/node"}
	spec := RunSpec{Image: "base", Repo: t.TempDir(), Homes: true, Cache: true}
	options := assembleOptions(cfg, false, spec, nil, "", "", spec.Repo, ttyNone, false, nil, nil, nil, nil, nil, "", "")
	joined := strings.Join(options, " ")
	if !strings.Contains(joined, "coop-cache:") || !strings.Contains(joined, "coop-asdf:") {
		t.Fatal("ordinary named volumes disappeared from the workload plan", joined)
	}
	// A daemon-managed volume has no host path to expose, so it is admitted.
	f, d := filteredFixture(t)
	plan := []string{"-v", "coop-cache:/home/node/.cache", "-v", "coop-asdf:/home/node/.asdf"}
	if err := f.validateMounts(plan, nil, nil); err != nil {
		t.Fatal("managed named volume refused", err)
	}
	if !slices.Contains(d.log, "volume-exposure") {
		t.Fatal("named volumes were admitted without inspecting the daemon")
	}
	// A local-driver volume backed by a host path inside the project is a way
	// back into agent-writable storage; refuse it rather than bind it blind.
	f, _ = filteredFixture(t)
	nested := filepath.Join(f.record.Project, "cache")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	f.docker.(*filteredDaemonFixture).volumeExposure = runtime.VolumeExposure{Sources: []string{nested}, BindSources: []string{nested}}
	if err := f.validateMounts(plan, nil, nil); err == nil {
		t.Fatal("named volume rooted in agent-writable storage was admitted")
	}
}

func TestFilteredReadinessPreservesCallerCancellation(t *testing.T) {
	for _, expired := range []bool{false, true} {
		f, _ := filteredFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		want := context.Canceled
		if expired {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			want = context.DeadlineExceeded
		} else {
			cancel()
		}
		err := f.waitReady(ctx)
		cancel()
		if !errors.Is(err, want) {
			t.Fatal("readiness hid caller cancellation", err)
		}
	}
}
