package networkgateway

import (
	"context"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

type blockedNativeDiagnostic struct {
	entered chan struct{}
	release chan struct{}
}

func (w blockedNativeDiagnostic) Write(data []byte) (int, error) {
	close(w.entered)
	<-w.release
	return len(data), nil
}

func TestNativeBrokerDiagnosticCannotDelayProtectedTeardown(t *testing.T) {
	writer := blockedNativeDiagnostic{entered: make(chan struct{}), release: make(chan struct{})}
	previous := log.Writer()
	log.SetOutput(writer)
	defer log.SetOutput(previous)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	closed := make(chan struct{})
	conn := &nativeBrokerConn{Conn: left, closed: func() { close(closed) }}
	b := &NativeBroker{ctx: ctx, cancel: cancel, input: &nativeBrokerListener{done: make(chan struct{})},
		last: NativeAccessSnapshot{Revision: 7, Credential: "private-test-value"}, active: map[*nativeBrokerConn]struct{}{conn: {}}}
	done := make(chan struct{})
	defer func() {
		close(writer.release)
		wait.For(t, "diagnostic completion", func() bool {
			select {
			case <-done:
				return true
			default:
				return false
			}
		})
	}()
	go func() { b.stopProtected("revoked", 8, 1, time.Millisecond); close(done) }()
	wait.For(t, "diagnostic writer", func() bool {
		select {
		case <-writer.entered:
			return true
		default:
			return false
		}
	})
	if ctx.Err() == nil {
		t.Fatal("blocked diagnostics delayed context cancellation")
	}
	select {
	case <-b.input.done:
	default:
		t.Fatal("blocked diagnostics delayed listener closure")
	}
	select {
	case <-closed:
	default:
		t.Fatal("blocked diagnostics delayed active connection closure")
	}
	b.mu.Lock()
	stopped, cleared := b.stopped, b.last == (NativeAccessSnapshot{})
	b.mu.Unlock()
	if !stopped || !cleared {
		t.Fatal("blocked diagnostics delayed credential clearing")
	}
}

func TestNativeBrokerSnapshotFailureCategoriesPreserveAdmission(t *testing.T) {
	now := time.Now()
	binding := NativeBrokerBinding{RunID: "run", Provider: "claude", Account: "selected", Epoch: 1}
	previous := NativeAccessSnapshot{Binding: binding, Revision: 2, Expires: now.Add(time.Minute), Credential: "private-test-value"}
	for _, test := range []struct {
		reason string
		change func(*NativeAccessSnapshot)
	}{
		{"", func(*NativeAccessSnapshot) {}},
		{"", func(s *NativeAccessSnapshot) { s.Revision++; s.Credential = "rotated-private-test-value" }},
		{"binding_mismatch", func(s *NativeAccessSnapshot) { s.Binding.Epoch++ }},
		{"revoked", func(s *NativeAccessSnapshot) { s.Revoked = true }},
		{"revision_missing", func(s *NativeAccessSnapshot) { s.Revision = 0 }},
		{"revision_rollback", func(s *NativeAccessSnapshot) { s.Revision-- }},
		{"expired", func(s *NativeAccessSnapshot) { s.Expires = now }},
		{"credential_invalid", func(s *NativeAccessSnapshot) { s.Credential = "" }},
		{"credential_invalid", func(s *NativeAccessSnapshot) { s.Credential = strings.Repeat("x", 32<<10+1) }},
		{"credential_invalid", func(s *NativeAccessSnapshot) { s.Credential += "\n" }},
		{"revision_mutation", func(s *NativeAccessSnapshot) { s.Credential += "changed" }},
		{"revision_mutation", func(s *NativeAccessSnapshot) { s.Expires = s.Expires.Add(time.Second) }},
	} {
		next := previous
		test.change(&next)
		if reason := nativeSnapshotFailure(binding, previous, next, now); reason != test.reason {
			t.Fatalf("category=%q want=%q", reason, test.reason)
		}
	}
}
