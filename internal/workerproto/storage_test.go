package workerproto

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func storageFixture(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(body))
}

func samplePoll() Poll {
	return Poll{
		Version: Version, PollRef: "poll:worker-a:1",
		Worker: WorkerHello{
			ID: "worker-a", WorkspaceRef: "workspace-a", ProtocolVersion: "1", BuildVersion: "3.0.0",
			ClockAt:       time.Date(2026, 9, 11, 4, 5, 6, 0, time.UTC),
			SandboxDigest: strings.Repeat("a", 64),
			Capacity: Capacity{
				SessionSlotsFree: 1, SessionSlotsTotal: 2, TurnSlotsFree: 1, TurnSlotsTotal: 2,
				WorkspaceSlotsFree: 1, WorkspaceSlotsTotal: 2, State: "eligible",
			},
			State: "eligible",
		},
	}
}

func sampleStorage() Storage {
	unattributed := int64(1 << 20)
	return Storage{
		Version:            StorageVersion,
		MeasuredAt:         time.Date(2026, 9, 11, 4, 5, 6, 0, time.UTC),
		CapacityBytes:      536870912000,
		FreeBytes:          107374182400,
		ReserveBytes:       26843545600,
		HighWatermarkBytes: 456340275200,
		LowWatermarkBytes:  402653184000,
		DisposableBytes:    8589934592,
		ProtectedBytes:     21474836480,
		UnattributedBytes:  &unattributed,
		Allocation:         StorageAllocationOpen,
	}
}

// Responder decodes this object; its field names, order, null shape and RFC3339 UTC stamp are the
// contract. A golden fixture is the only thing that fails when a well-meaning rename breaks a
// control plane that is not in this repository.
func TestStorageMarshalsTheExactPublishedShape(t *testing.T) {
	encoded, err := json.Marshal(sampleStorage())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), storageFixture(t, "worker_storage.json"); got != want {
		t.Fatalf("storage wire shape drifted:\n got %s\nwant %s", got, want)
	}

	refused := sampleStorage()
	refused.FreeBytes = 1 << 30
	refused.DisposableBytes = 0
	refused.ProtectedBytes = 483183820800
	refused.UnattributedBytes = nil
	refused.Allocation = StorageAllocationRefused
	reason := StorageRefusalProtectedBudget
	refused.RefusalReason = &reason
	encoded, err = json.Marshal(refused)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), storageFixture(t, "worker_storage_refused.json"); got != want {
		t.Fatalf("refused storage wire shape drifted:\n got %s\nwant %s", got, want)
	}
}

// Unknown is not zero: the field is nullable on purpose, and a null has to survive the round trip
// as "we could not attribute this", never as 0 bytes.
func TestStorageRoundTripsUnknownUnattributedBytesAsNull(t *testing.T) {
	decoded := Storage{}
	if err := json.Unmarshal([]byte(storageFixture(t, "worker_storage_refused.json")), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.UnattributedBytes != nil {
		t.Fatalf("unattributed bytes decoded as %d, want unknown", *decoded.UnattributedBytes)
	}
	if decoded.RefusalReason == nil || *decoded.RefusalReason != StorageRefusalProtectedBudget {
		t.Fatalf("refusal reason = %v", decoded.RefusalReason)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("the published refused fixture failed validation: %v", err)
	}
}

func TestStorageValidateRejectsIncoherentAccounting(t *testing.T) {
	for name, mutate := range map[string]func(*Storage){
		"unsupported version":      func(s *Storage) { s.Version = 2 },
		"missing measurement time": func(s *Storage) { s.MeasuredAt = time.Time{} },
		"free above capacity":      func(s *Storage) { s.FreeBytes = s.CapacityBytes + 1 },
		"negative bytes":           func(s *Storage) { s.DisposableBytes = -1 },
		"negative unattributed":    func(s *Storage) { negative := int64(-1); s.UnattributedBytes = &negative },
		"watermarks inverted":      func(s *Storage) { s.LowWatermarkBytes = s.HighWatermarkBytes + 1 },
		"watermark above capacity": func(s *Storage) { s.HighWatermarkBytes = s.CapacityBytes + 1 },
		"unknown allocation state": func(s *Storage) { s.Allocation = "maybe" },
		"reason without refusal": func(s *Storage) {
			reason := StorageRefusalReserveExhausted
			s.RefusalReason = &reason
		},
		"refusal without reason": func(s *Storage) { s.Allocation = StorageAllocationRefused },
		"unknown refusal reason": func(s *Storage) {
			s.Allocation = StorageAllocationRefused
			reason := "disk_is_sad"
			s.RefusalReason = &reason
		},
	} {
		storage := sampleStorage()
		mutate(&storage)
		if err := storage.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := sampleStorage().Validate(); err != nil {
		t.Fatalf("the published fixture failed validation: %v", err)
	}
}

// The field is OPTIONAL. A worker that cannot measure its disk still polls, and a worker that can
// must not have its poll rejected for carrying the measurement.
func TestWorkerHelloCarriesStorageWithoutChangingTheRestOfThePoll(t *testing.T) {
	poll := samplePoll()
	withoutStorage, err := json.Marshal(poll)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(withoutStorage), "storage") {
		t.Fatalf("an unmeasured worker published a storage key: %s", withoutStorage)
	}
	if _, err := DecodePoll(withoutStorage); err != nil {
		t.Fatalf("a poll without storage was rejected: %v", err)
	}

	storage := sampleStorage()
	poll.Worker.Storage = &storage
	encoded, err := json.Marshal(poll)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePoll(encoded)
	if err != nil {
		t.Fatalf("a poll carrying storage was rejected: %v", err)
	}
	if decoded.Worker.Storage == nil || decoded.Worker.Storage.DisposableBytes != storage.DisposableBytes {
		t.Fatalf("decoded storage = %+v", decoded.Worker.Storage)
	}

	poll.Worker.Storage.Allocation = "maybe"
	broken, err := json.Marshal(poll)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePoll(broken); err == nil {
		t.Fatal("a poll carrying incoherent storage was accepted")
	}
}
