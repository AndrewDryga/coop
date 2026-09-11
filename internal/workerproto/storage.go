package workerproto

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// StorageVersion is the version of the storage object itself, separate from the poll's protocol
// version: the accounting can grow a field long before the whole worker contract does, and a
// control plane that only understands version 1 has to be able to say so.
const StorageVersion = 1

const (
	StorageAllocationOpen    = "open"
	StorageAllocationRefused = "refused"

	StorageRefusalReserveExhausted = "reserve_exhausted"
	StorageRefusalProtectedBudget  = "protected_storage_exceeds_budget"
)

var (
	storageAllocations    = []string{StorageAllocationOpen, StorageAllocationRefused}
	storageRefusalReasons = []string{StorageRefusalReserveExhausted, StorageRefusalProtectedBudget}
)

// Storage is the worker's own account of the disk it executes on, carried on every hello.
//
// It is deliberately small and self-describing: the control plane needs to know how much space
// remains, how much of what the worker holds it may ask to have back, how much it must not, and
// whether this worker will accept another fork right now. The detailed per-fork inventory behind
// these totals stays on the worker's owner-private API — it names sessions and paths, and the
// control plane already knows its own sessions.
//
// UnattributedBytes is a POINTER because unknown is not zero: a null says the worker found storage
// under its fork root it could not attribute, or could not finish measuring, and a control plane
// must not subtract it from anything.
type Storage struct {
	Version            int       `json:"version"`
	MeasuredAt         time.Time `json:"measured_at"`
	CapacityBytes      int64     `json:"capacity_bytes"`
	FreeBytes          int64     `json:"free_bytes"`
	ReserveBytes       int64     `json:"reserve_bytes"`
	HighWatermarkBytes int64     `json:"high_watermark_bytes"`
	LowWatermarkBytes  int64     `json:"low_watermark_bytes"`
	DisposableBytes    int64     `json:"disposable_bytes"`
	ProtectedBytes     int64     `json:"protected_bytes"`
	UnattributedBytes  *int64    `json:"unattributed_bytes"`
	Allocation         string    `json:"allocation"`
	RefusalReason      *string   `json:"refusal_reason"`
}

// Validate confirms one storage advertisement before a worker publishes it or a controller trusts
// it. The watermarks are USED-byte thresholds on the accounted volume — allocation closes at the
// high one and only reopens under the low one — while the reserve is a FREE-byte floor, so the
// coherence checks differ between them on purpose.
func (s Storage) Validate() error { return s.validate() }

func (s Storage) validate() error {
	if s.Version != StorageVersion {
		return fmt.Errorf("unsupported worker storage version %d", s.Version)
	}
	if s.MeasuredAt.IsZero() {
		return errors.New("worker storage measurement time is required")
	}
	if s.CapacityBytes <= 0 || s.FreeBytes < 0 || s.FreeBytes > s.CapacityBytes {
		return errors.New("worker storage capacity and free space are incoherent")
	}
	if s.ReserveBytes < 0 || s.ReserveBytes > s.CapacityBytes {
		return errors.New("worker storage reserve is outside its capacity")
	}
	if s.LowWatermarkBytes <= 0 || s.LowWatermarkBytes >= s.HighWatermarkBytes || s.HighWatermarkBytes > s.CapacityBytes {
		return errors.New("worker storage watermarks are not an ordered pair inside capacity")
	}
	if s.DisposableBytes < 0 || s.ProtectedBytes < 0 {
		return errors.New("worker storage byte totals cannot be negative")
	}
	if s.UnattributedBytes != nil && *s.UnattributedBytes < 0 {
		return errors.New("worker unattributed storage cannot be negative")
	}
	if !slices.Contains(storageAllocations, s.Allocation) {
		return errors.New("invalid worker storage allocation state")
	}
	if (s.Allocation == StorageAllocationRefused) != (s.RefusalReason != nil) {
		return errors.New("worker storage refusal reason does not match its allocation state")
	}
	if s.RefusalReason != nil && !slices.Contains(storageRefusalReasons, *s.RefusalReason) {
		return errors.New("invalid worker storage refusal reason")
	}
	return nil
}
