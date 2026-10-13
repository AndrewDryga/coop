package networkgateway

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestNativeBrokerDenialLatchesBeforeTeardown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	binding := NativeBrokerBinding{RunID: "run", Provider: "claude", Account: "selected", Epoch: 1}
	valid := NativeAccessSnapshot{Binding: binding, Revision: 2, Expires: time.Now().Add(time.Hour), Credential: "private-test-value"}
	loaded, releaseLoad := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	b := &NativeBroker{ctx: ctx, cancel: cancel, binding: binding, load: func(context.Context) (NativeAccessSnapshot, error) {
		if reads.Add(1) == 1 {
			close(loaded)
			<-releaseLoad
			next := valid
			next.Revoked = true
			return next, nil
		}
		return valid, nil
	}}
	// White-box scheduling barrier: occupy Once so teardown cannot enter. This
	// bypasses cleanup in this bare fixture, which owns no server/streams, and
	// tests only the admission invariant. Real cleanup has separate stream tests.
	teardownEntered, releaseTeardown, teardownDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		b.once.Do(func() { close(teardownEntered); <-releaseTeardown })
		close(teardownDone)
	}()
	select {
	case <-teardownEntered:
	case <-time.After(wait.Deadline):
		t.Fatal("teardown gate did not start")
	}
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(releaseTeardown) }); <-teardownDone })
	first := make(chan error, 1)
	go func() { _, err := b.current(); first <- err }()
	select {
	case <-loaded:
	case <-time.After(wait.Deadline):
		t.Fatal("invalid snapshot load did not start")
	}
	second := make(chan error, 1)
	close(releaseLoad)
	// The first comparison has completed before the next reader gets the lock.
	b.mu.Lock()
	stopped := b.stopped
	b.mu.Unlock()
	if !stopped {
		t.Error("comparison released the mutex without latching denial")
	}
	go func() { _, err := b.current(); second <- err }()
	select {
	case err := <-second:
		if err == nil || reads.Load() != 1 {
			t.Error("valid queued reader escaped the first denial before teardown")
		}
	case <-time.After(wait.Deadline):
		t.Error("denial waited for slow teardown")
	}
	release.Do(func() { close(releaseTeardown) })
	select {
	case err := <-first:
		if err == nil {
			t.Error("invalid snapshot admitted")
		}
	case <-time.After(wait.Deadline):
		t.Fatal("first denial did not finish")
	}
	if _, err := b.current(); err == nil || reads.Load() != 1 {
		t.Fatal("denial was not permanent")
	}
}

func TestNativeBrokerFirstDenialSurvivesCompetingCleanup(t *testing.T) {
	var diagnostic bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&diagnostic)
	defer log.SetOutput(previous)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	binding := NativeBrokerBinding{RunID: "run", Provider: "claude", Account: "selected", Epoch: 1}
	last := NativeAccessSnapshot{Binding: binding, Revision: 7, Expires: time.Now().Add(time.Hour), Credential: "private-test-value"}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	b := &NativeBroker{ctx: ctx, binding: binding, last: last,
		input:  &nativeBrokerListener{done: make(chan struct{})},
		cancel: func() { close(entered); <-release; cancel() },
		load: func(context.Context) (NativeAccessSnapshot, error) {
			next := last
			next.Revision++
			next.Revoked = true
			return next, nil
		}}
	denied := make(chan error, 1)
	go func() { _, err := b.current(); denied <- err }()
	select {
	case <-entered:
	case <-time.After(wait.Deadline):
		t.Fatal("protected teardown did not start")
	}
	if _, err := b.current(); err == nil {
		t.Error("cleanup latency reopened admission")
	}
	closed := make(chan struct{})
	go func() { b.stopProtected("closed", 99, 9, time.Hour); close(closed) }()
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-denied:
		if err == nil {
			t.Error("revoked snapshot admitted")
		}
	case <-time.After(wait.Deadline):
		t.Fatal("denial cleanup did not finish")
	}
	select {
	case <-closed:
	case <-time.After(wait.Deadline):
		t.Fatal("competing cleanup did not finish")
	}
	if !strings.Contains(diagnostic.String(), "reason=revoked previous_revision=7 revision=8 attempts=0") ||
		strings.Contains(diagnostic.String(), last.Credential) || strings.Count(diagnostic.String(), "native_broker_stopped") != 1 {
		t.Fatal("first denial metadata overwritten, duplicated or exposed credentials")
	}
	if b.last != (NativeAccessSnapshot{}) || b.stop.reason != "revoked" {
		t.Fatal("first denial or access clearing lost")
	}
}
