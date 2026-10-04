package workerconnector

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// blitz-core's first download took 45 minutes, and the create that needed it was staged again,
// and logged, at every poll (2026-10-03). A command waiting on a first download is tried after
// 5 s, then 10, 20, 40, 80 and 120 s; other commands keep the ordinary pace.
func TestCommandWaitingOnAFirstDownloadBacksOff(t *testing.T) {
	var waits firstDownloadWaits
	start := time.Date(2026, 10, 3, 21, 38, 0, 0, time.UTC)
	downloading := fmt.Errorf("%w: fetch job source: %w", errArtifactTransfer, errJobSourceDownloading)

	now := start
	var tries []time.Duration
	for now.Sub(start) < 10*time.Minute {
		if !waits.held("create", now) {
			tries = append(tries, now.Sub(start))
			waits.settled("create", downloading, now)
		}
		now = now.Add(time.Second) // the controller delivers it again every poll
	}
	want := []time.Duration{0, 5 * time.Second, 15 * time.Second, 35 * time.Second, 75 * time.Second,
		155 * time.Second, 275 * time.Second, 395 * time.Second, 515 * time.Second}
	if fmt.Sprint(tries) != fmt.Sprint(want) {
		t.Fatalf("tried at %v, want %v", tries, want)
	}

	// The download is published: the next try succeeds and the command keeps no wait.
	now = now.Add(2 * time.Minute)
	waits.settled("create", nil, now)
	if waits.held("create", now) || len(waits.waits) != 0 {
		t.Fatalf("a settled command still waits: %+v", waits.waits)
	}

	// Another failure is not a download, and is tried again at the ordinary pace.
	waits.settled("get", errors.New("transport closed"), now)
	if waits.held("get", now) {
		t.Fatal("an ordinary transfer failure was held back")
	}
}

// A wait for a command the controller stopped delivering is dropped on a later settlement.
func TestWaitForAnUndeliveredCommandIsDropped(t *testing.T) {
	var waits firstDownloadWaits
	now := time.Date(2026, 10, 3, 21, 38, 0, 0, time.UTC)
	waits.settled("superseded", errJobSourceDownloading, now)
	waits.settled("other", nil, now.Add(10*time.Minute))
	if _, kept := waits.waits["superseded"]; kept {
		t.Fatal("a wait outlived its command's deliveries")
	}
}
