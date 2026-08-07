package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
)

// The sweep runs once a day, not once a command.
//
// The leak accumulates over days. Asking the container runtime what is mounted
// on every invocation would put an inspect round trip on a hot path to solve a
// slow problem, and would multiply across parallel coops.
func TestTempReapIsDailyNotPerCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temp-reap")
	now := time.Now()

	if !tempReapDue(path, now) {
		t.Fatal("a machine that has never swept was not due")
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if tempReapDue(path, now) {
		t.Error("swept again immediately; parallel coops would all sweep at once")
	}
	if tempReapDue(path, now.Add(23*time.Hour)) {
		t.Error("swept again within the day")
	}
	if !tempReapDue(path, now.Add(25*time.Hour)) {
		t.Error("did not become due after a day")
	}
}

// The marker is written before the sweep, not after.
//
// Writing it afterwards would let every coop started in the same moment decide
// it was due and sweep together — and a sweep that crashes would then never
// mark itself, so every subsequent run would retry it forever.
func TestTempReapMarksBeforeSweeping(t *testing.T) {
	home := t.TempDir()
	cfg := &config.Config{BoxHome: home, RuntimeName: "definitely-not-a-runtime"}
	startTempReap(cfg)

	if _, err := os.Stat(tempReapPath(cfg)); err != nil {
		t.Fatalf("the attempt was not marked before sweeping: %v", err)
	}
	if tempReapDue(tempReapPath(cfg), time.Now()) {
		t.Error("still due immediately after starting a sweep")
	}
}
