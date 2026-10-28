package networkgateway

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestPrivateObservationReadsCacheWithoutProbingOrRenewing(t *testing.T) {
	c, now := collectorFixture(t)
	publishFixture(c, *now, nil)
	var probes atomic.Int32
	c.inventory = func() ([]SocketRow, error) { probes.Add(1); return nil, nil }
	g := &GuardRuntime{Identity: c.identity, collector: c}
	path := shortControlPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.serveObservation(ctx, path, func(*net.UnixConn) bool { return true }); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(wait.Deadline):
			t.Error("observer fixture leaked")
		}
	})
	// Bind publishes the socket before listen is ready. Under a loaded race
	// gate, observing that file let the first client lose the startup race.
	wait.For(t, "private observation listener", func() bool {
		conn, err := net.DialTimeout("unix", path, ControlTimeout)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
	for range 20 {
		value, err := observeAt(context.Background(), path, c.identity)
		if err != nil || value.Network.Sequence != 1 {
			t.Fatalf("cached observation: %#v %v", value, err)
		}
	}
	if probes.Load() != 0 || g.phase.Load() != gatewayStarting {
		t.Fatal("read triggered sampling or changed readiness")
	}
	wrong := c.identity
	wrong.Epoch = strings.Repeat("f", 32)
	if _, err := observeAt(context.Background(), path, wrong); err == nil {
		t.Fatal("observation crossed generation")
	}
}

func TestOversizeObservationRetainsTerminalCountersAndExplicitlyOmitsDetail(t *testing.T) {
	c, now := collectorFixture(t)
	c.terminal, c.envoyTotals.Stopped = true, true
	publishFixture(c, *now, nil)
	value := RuntimeObservation{Version: PrivateProtocolVersion, Identity: c.identity, Network: c.Snapshot()}
	value.Network.Connections = []networkview.Connection{{ID: "large", Name: strings.Repeat("x", MaxSnapshotBytes)}}
	data, err := encodeObservation(value)
	if err != nil {
		t.Fatal(err)
	}
	reduced, err := ReadObservation(bytes.NewReader(data), c.identity)
	if err != nil || !reduced.Network.Terminal || !reduced.Network.Loss.DetailTruncated || *reduced.Network.Loss.OmittedDetails != 1 || len(reduced.Network.Connections) != 0 || reduced.Network.Counters == nil {
		t.Fatal("detail saturation lost terminal custody or invented an empty inventory")
	}
	if len(value.Network.Connections) != 1 || *value.Network.Loss.OmittedDetails != 0 || value.Network.Loss.DetailTruncated {
		t.Fatal("transport fallback mutated the retained snapshot")
	}
	path := t.TempDir()
	if err := writeFinalObservation(path, value); err != nil {
		t.Fatal("large detail prevented a final file:", err)
	}
}

func TestFinalObservationSurvivesWriterAndCannotReplaceGeneration(t *testing.T) {
	c, now := collectorFixture(t)
	publishFixture(c, *now, nil)
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := prepareObservationDirectory(path); err != nil {
		t.Fatal(err)
	}
	value := RuntimeObservation{Version: PrivateProtocolVersion, Identity: c.identity, Network: c.Snapshot()}
	if err := writeFinalObservation(path, value); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(filepath.Join(path, "final.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got, err := ReadObservation(file, c.identity)
	if err != nil || got.Network.Sequence != value.Network.Sequence {
		t.Fatalf("final observation lost: %#v %v", got, err)
	}
	if err := writeFinalObservation(path, value); err == nil {
		t.Fatal("replaced final generation")
	}
	if err := prepareObservationDirectory(path); err == nil {
		t.Fatal("reused observation volume")
	}
	if _, err := os.Lstat(filepath.Join(path, ".final.tmp")); !os.IsNotExist(err) {
		t.Fatal("writer left temp evidence")
	}
	info, err := file.Stat()
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private final observation permissions")
	}
}

func TestObservationFramingRefusesOversizeMalformedAndForeignIdentity(t *testing.T) {
	c, now := collectorFixture(t)
	publishFixture(c, *now, nil)
	value := RuntimeObservation{Version: PrivateProtocolVersion, Identity: c.identity, Network: c.Snapshot()}
	valid, err := encodeObservation(value)
	if err != nil {
		t.Fatal(err)
	}
	foreign := value
	foreign.Identity.RunID, foreign.Identity.Epoch = strings.Repeat("c", 32), strings.Repeat("d", 32)
	foreign.Network.RunID, foreign.Network.Epoch = foreign.Identity.RunID, foreign.Identity.Epoch
	foreignBytes, err := encodeObservation(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadObservation(strings.NewReader(string(foreignBytes)), foreign.Identity); err != nil {
		t.Fatalf("coherent foreign positive control: %v", err)
	}
	if _, err := ReadObservation(strings.NewReader(string(foreignBytes)), c.identity); err == nil {
		t.Fatal("accepted a coherent foreign run/epoch")
	}
	for _, data := range []string{strings.Repeat(" ", MaxSnapshotBytes) + "\n", string(valid[:len(valid)-1]), "{}\n",
		strings.Replace(string(valid), `"version":2`, `"version":1`, 1)} {
		if _, err := ReadObservation(strings.NewReader(data), c.identity); err == nil {
			t.Fatal("accepted invalid observation frame")
		}
	}
}

func TestFinalObservationWriteFailureSurvivesOrdinaryCancellation(t *testing.T) {
	c, now := collectorFixture(t)
	publishFixture(c, *now, nil)
	g := &GuardRuntime{Identity: c.identity, collector: c}
	err := g.finishObservation(filepath.Join(t.TempDir(), "missing"), context.Canceled)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, Failure("observation_storage_unavailable")) {
		t.Fatalf("terminal storage outcome was swallowed: %v", err)
	}
}
