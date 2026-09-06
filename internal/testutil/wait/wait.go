// Package wait bounds the polling loops tests use to notice that a fixture reached a step: a
// subprocess wrote its marker, a lock became visible, a container was started. Those loops guard a
// BROKEN fixture — timing is not the behavior under test — so their deadline is generous: a healthy
// run pays nothing (the poll returns the moment the condition holds), and a starved host (the
// gate's race suite beside a Docker-heavy run) no longer turns a working fixture into a red gate
// that a green retry then erases. A bound that IS the behavior under test — a drain window, a
// forwarder that must not delay a failed provider — is a different thing: it stays in the test,
// tight, and attributes the phases it measures.
package wait

import (
	"os"
	"testing"
	"time"
)

// Deadline is how long a fixture guard waits before calling the fixture broken.
const Deadline = 60 * time.Second

// Poll is the interval between condition checks.
const Poll = 5 * time.Millisecond

// deadline is Deadline, shortened only by this package's own failure-path test.
var deadline = Deadline

// For polls condition until it holds or Deadline passes, then fails the test naming what did not
// happen and how long the wait was, so a broken fixture reads as one.
func For(t testing.TB, what string, condition func() bool) {
	t.Helper()
	start := time.Now()
	for {
		if condition() {
			return
		}
		if time.Since(start) > deadline {
			t.Fatalf("%s did not happen within %s", what, deadline)
			return // a recording TB (this package's own test) does not stop the goroutine
		}
		time.Sleep(Poll)
	}
}

// ForFile is For on the existence of path.
func ForFile(t testing.TB, path string) {
	t.Helper()
	For(t, path+" did not appear", func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
}
