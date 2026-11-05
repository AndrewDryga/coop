package workerconnector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// A command digest is stored in every receipt and compared on replay, so its bytes are a format:
// an encoder change (the wire encoder's unescaped HTML, a reordered or added field) would make
// every saved receipt conflict with the same command after an upgrade. The expected bytes are
// spelled out here, HTML and line-separator escapes included, rather than recomputed through the
// code under test. The lease expiry is not identity: a renewed lease is the same command.
func TestCommandDigestKeepsItsCanonicalEncoding(t *testing.T) {
	command := workerproto.Command{
		CommandID: "cmd-1", WorkerID: "worker-1", SessionRef: "session-1", PlacementGeneration: 2,
		LeaseRef: "lease-1", LeaseExpiresAt: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC),
		Kind: "submit_turn", CommandVersion: 1, IdempotencyKey: "key-1",
		Payload: json.RawMessage("{ \"text\": \"<a&b>\u2028\" }"),
	}
	canonical := `{"command_id":"cmd-1","worker_id":"worker-1","session_ref":"session-1","placement_generation":2,` +
		`"lease_ref":"lease-1","kind":"submit_turn","command_version":1,` +
		`"payload":{"text":"\u003ca\u0026b\u003e\u2028"},"idempotency_key":"key-1"}`
	sum := sha256.Sum256([]byte(canonical))
	want := hex.EncodeToString(sum[:])

	got, err := commandDigest(command)
	if err != nil || got != want {
		t.Fatalf("commandDigest = %q, %v; want %q (sha256 of %s)", got, err, want, canonical)
	}
	command.LeaseExpiresAt = command.LeaseExpiresAt.Add(time.Hour)
	if renewed, err := commandDigest(command); err != nil || renewed != want {
		t.Fatalf("renewing the lease changed the command digest: %q, %v", renewed, err)
	}
}
