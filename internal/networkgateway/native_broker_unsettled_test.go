package networkgateway

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func (f *nativeTransportFixture) failReads(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadErr = err
}

func (f *nativeTransportFixture) stopped() bool {
	f.broker.mu.Lock()
	defer f.broker.mu.Unlock()
	return f.broker.stopped
}

func nativeStatus(t *testing.T, f *nativeTransportFixture) int {
	t.Helper()
	response, _ := nativeResponse(t, f.client, f.request(t, "/http"))
	return response.StatusCode
}

// A loaded host's VM bind mount once took 1233 ms for one read of the access document (attempts=1,
// past the 1 s budget). That latched the broker, and the agent's own provider got 403 for the rest
// of the run with nothing in the run's record. A read that ran out of time refuses only while it
// lasts, and is recorded; a document that says revoked still stops the broker for good.
func TestNativeBrokerSlowReadRefusesWithoutLatchingAndIsRecorded(t *testing.T) {
	f := newNativeTransportFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "native") }))
	events := NewGuardEvents(nil)
	f.broker.events = events
	if status := nativeStatus(t, f); status != http.StatusOK {
		t.Fatalf("first request: %d", status)
	}
	f.failReads(&nativeDocumentFailure{reason: "document_canceled", attempts: 1})
	if status := nativeStatus(t, f); status != http.StatusForbidden {
		t.Fatalf("an unread access was served: %d", status)
	}
	if f.stopped() {
		t.Fatal("one slow read stopped the broker for the rest of the run")
	}
	f.failReads(nil)
	if status := nativeStatus(t, f); status != http.StatusOK {
		t.Fatalf("access did not return after a read succeeded: %d", status)
	}
	refusals, _ := events.Drain(MaxGuardEvents)
	recorded := false
	for _, event := range refusals {
		recorded = recorded || event.Kind == "admission_failed" && event.Name == "example.com" && event.Reason == "native_account_unavailable"
	}
	if !recorded {
		t.Fatalf("the refusal was not recorded: %+v", refusals)
	}

	f.change(func(s *NativeAccessSnapshot) { s.Revision++; s.Revoked = true })
	// The latch also cuts the open stream, so this request may end in an error rather than a 403.
	if response, err := f.client.Do(f.request(t, "/http")); err == nil {
		_ = response.Body.Close()
		if response.StatusCode == http.StatusOK {
			t.Fatal("revoked access was served")
		}
	}
	f.change(func(s *NativeAccessSnapshot) { s.Revision++; s.Revoked = false })
	if !f.stopped() {
		t.Fatal("a revoked document did not stop the broker")
	}
	if response, err := f.client.Do(f.request(t, "/http")); err == nil {
		_ = response.Body.Close()
		if response.StatusCode == http.StatusOK {
			t.Fatal("a revoked broker revived")
		}
	}
}

// The last lease the host published bounds how long reads may keep failing: once it lapses unread,
// the broker stops exactly as it does for an expired document.
func TestNativeBrokerLeaseLapsedWhileUnreadStops(t *testing.T) {
	f := newNativeTransportFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "native") }))
	f.change(func(s *NativeAccessSnapshot) { s.Revision++; s.Expires = time.Now().Add(2 * time.Second) })
	if status := nativeStatus(t, f); status != http.StatusOK {
		t.Fatalf("first request: %d", status)
	}
	f.failReads(&nativeDocumentFailure{reason: "document_canceled", attempts: 1})
	if f.stopped() {
		t.Fatal("stopped before the lease lapsed")
	}
	wait.For(t, "the unread lease to stop the broker", f.stopped)
	f.failReads(nil)
	f.change(func(s *NativeAccessSnapshot) { s.Revision++; s.Expires = time.Now().Add(time.Hour) })
	if response, err := f.client.Do(f.request(t, "/http")); err == nil {
		_ = response.Body.Close()
		if response.StatusCode == http.StatusOK {
			t.Fatal("a broker whose lease lapsed unread revived")
		}
	}
}

// Only a read that ran out of time is unsettled; what a document says, or an I/O error, still latches.
func TestNativeReadUnsettledIsOnlyTheDeadline(t *testing.T) {
	for reason, want := range map[string]bool{
		"load_deadline": true, "document_canceled": true, "document_missing": true, "document_incomplete": true,
		"document_io": false, "document_schema": false, "context_canceled": false, "load_failed": false,
		"revoked": false, "expired": false, "revision_rollback": false, "binding_mismatch": false,
	} {
		if nativeReadUnsettled(reason) != want {
			t.Errorf("nativeReadUnsettled(%q) = %v", reason, !want)
		}
	}
}
