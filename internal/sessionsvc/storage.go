package sessionsvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// Why a fork's storage is being kept. Only "disposable", "owned_orphan" and "staged_discard" are
// bytes anybody may take back; every other category is a refusal with a reason attached.
const (
	StorageCategoryActive        = "active"
	StorageCategoryGrace         = "grace"
	StorageCategoryDisposable    = "disposable"
	StorageCategoryOwnedOrphan   = "owned_orphan"
	StorageCategoryProtected     = "protected"
	StorageCategoryStagedDiscard = "staged_discard"
	StorageCategoryControl       = "control"
	StorageCategoryUnattributed  = "unattributed"
)

const (
	// The approved per-worker ceiling for inactive disposable forks.
	DefaultStorageDisposableBudget = 10 << 30
	// The documented grace an ordinary completed session's workspace gets before its storage is
	// anybody's to reclaim, matching the control plane's own 15-minute close grace.
	DefaultStorageGraceWindow = 15 * time.Minute
	// A full tree walk is expensive; a heartbeat is not allowed to cost one. measured_at carries
	// the staleness so a reader never has to guess.
	DefaultStorageMeasureInterval = 5 * time.Minute
	// Cleanup concurrency is bounded by doing one scan at a time and at most this many candidates
	// in it, so reclamation can never crowd out the runtime work it exists to protect.
	DefaultStorageMaxReclaimPerPass = 4

	storageForkRootEntries = 4096
)

// StorageLimits is the configured storage policy for one worker.
//
// The watermarks are USED-byte levels on the accounted volume: allocation closes at or above the
// high one and reopens only at or below the low one, which is what stops a worker from flapping
// open the instant a single fork is reclaimed. ReserveBytes is a FREE-byte floor underneath both,
// and it is never spent on new work: cleanup, control and recovery of existing work run inside it.
//
// The defaults are derived from the volume's measured capacity rather than written down as
// absolutes, because the same worker binary runs on very different disks. They keep ONE reserve
// free and, once that is breached, require TWO before taking new work again: on a 500 GiB volume
// that is a 25 GiB reserve, allocation closing at 475 GiB used and reopening at 450 GiB.
//
// The watermarks deliberately track the reserve rather than a fixed "percent full" line. A worker
// does not own the whole volume, and refusing every session because unrelated data fills the host
// disk is not this policy's job — keeping room for the work coop itself accepts is.
type StorageLimits struct {
	ReserveBytes          int64
	HighWatermarkBytes    int64
	LowWatermarkBytes     int64
	DisposableBudgetBytes int64
	ProtectedBudgetBytes  int64
	GraceWindow           time.Duration
	MeasureInterval       time.Duration
	MaxReclaimPerPass     int
}

// DefaultStorageLimits derives a policy from measured capacity. A capacity of zero — an unreadable
// volume — yields only the capacity-independent fields, and the caller must not evaluate pressure
// against it.
func DefaultStorageLimits(capacityBytes int64) StorageLimits {
	limits := StorageLimits{
		DisposableBudgetBytes: DefaultStorageDisposableBudget,
		GraceWindow:           DefaultStorageGraceWindow,
		MeasureInterval:       DefaultStorageMeasureInterval,
		MaxReclaimPerPass:     DefaultStorageMaxReclaimPerPass,
	}
	if capacityBytes <= 0 {
		return limits
	}
	limits.ReserveBytes = capacityBytes / 20
	limits.HighWatermarkBytes = capacityBytes - limits.ReserveBytes
	limits.LowWatermarkBytes = capacityBytes - 2*limits.ReserveBytes
	// Protected storage that alone reaches the level reclamation would have to return the volume
	// to is protected storage reclamation can never fix. Refuse new work and say why, rather than
	// deleting protected data to satisfy a quota.
	limits.ProtectedBudgetBytes = limits.LowWatermarkBytes
	return limits
}

// Validate rejects a policy that cannot be satisfied. capacityBytes of zero checks only the
// internal coherence, for the configuration path that runs before any volume is measured.
func (l StorageLimits) Validate(capacityBytes int64) error {
	if l.ReserveBytes < 0 {
		return errors.New("storage reserve cannot be negative")
	}
	if l.LowWatermarkBytes <= 0 || l.LowWatermarkBytes >= l.HighWatermarkBytes {
		return errors.New("storage watermarks must be an ordered low/high pair")
	}
	if l.DisposableBudgetBytes <= 0 {
		return errors.New("storage disposable budget must be positive")
	}
	if l.ProtectedBudgetBytes <= 0 {
		return errors.New("storage protected budget must be positive")
	}
	if l.GraceWindow < 0 {
		return errors.New("storage grace window cannot be negative")
	}
	if l.MeasureInterval <= 0 {
		return errors.New("storage measure interval must be positive")
	}
	if l.MaxReclaimPerPass <= 0 {
		return errors.New("storage reclamation must be bounded to a positive number of candidates")
	}
	if capacityBytes > 0 && (l.ReserveBytes > capacityBytes || l.HighWatermarkBytes > capacityBytes) {
		return errors.New("storage limits exceed the measured volume capacity")
	}
	return nil
}

// LoadStorageLimits reads the optional `storage:` block from the operator's session policy file.
// The second result is false when the file carries no block, which means the derived defaults
// apply. An incoherent block is refused HERE, at the file the operator is looking at, rather than
// the first time their volume fills up.
func LoadStorageLimits(path string) (StorageLimits, bool, error) {
	data, err := readSessionPolicyFile(path)
	if err != nil {
		return StorageLimits{}, false, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var raw rawSessionPolicyFile
	if err := decoder.Decode(&raw); err != nil {
		return StorageLimits{}, false, fmt.Errorf("decode session storage policy: %w", err)
	}
	if raw.Storage == nil {
		return StorageLimits{}, false, nil
	}
	grace, err := time.ParseDuration(raw.Storage.GraceWindow)
	if err != nil {
		return StorageLimits{}, false, fmt.Errorf("session storage grace_window: %w", err)
	}
	measure, err := time.ParseDuration(raw.Storage.MeasureInterval)
	if err != nil {
		return StorageLimits{}, false, fmt.Errorf("session storage measure_interval: %w", err)
	}
	limits := StorageLimits{
		ReserveBytes: raw.Storage.ReserveBytes, HighWatermarkBytes: raw.Storage.HighWatermarkBytes,
		LowWatermarkBytes: raw.Storage.LowWatermarkBytes, DisposableBudgetBytes: raw.Storage.DisposableBudgetBytes,
		ProtectedBudgetBytes: raw.Storage.ProtectedBudgetBytes, GraceWindow: grace,
		MeasureInterval: measure, MaxReclaimPerPass: raw.Storage.MaxReclaimPerPass,
	}
	if err := limits.Validate(0); err != nil {
		return StorageLimits{}, false, fmt.Errorf("session storage policy: %w", err)
	}
	return limits, true, nil
}

func (l StorageLimits) budget() StorageBudget {
	return StorageBudget{
		ReserveBytes: l.ReserveBytes, HighWatermarkBytes: l.HighWatermarkBytes,
		LowWatermarkBytes: l.LowWatermarkBytes, DisposableBudgetBytes: l.DisposableBudgetBytes,
		ProtectedBudgetBytes: l.ProtectedBudgetBytes,
		GraceSeconds:         int64(l.GraceWindow / time.Second),
		MeasureSeconds:       int64(l.MeasureInterval / time.Second),
		MaxReclaimPerPass:    l.MaxReclaimPerPass,
	}
}

// StorageBudget is the operator-readable projection of StorageLimits.
type StorageBudget struct {
	ReserveBytes          int64 `json:"reserve_bytes"`
	HighWatermarkBytes    int64 `json:"high_watermark_bytes"`
	LowWatermarkBytes     int64 `json:"low_watermark_bytes"`
	DisposableBudgetBytes int64 `json:"disposable_budget_bytes"`
	ProtectedBudgetBytes  int64 `json:"protected_budget_bytes"`
	GraceSeconds          int64 `json:"grace_seconds"`
	MeasureSeconds        int64 `json:"measure_seconds"`
	MaxReclaimPerPass     int   `json:"max_reclaim_per_pass"`
}

// StorageTotals is the full breakdown behind the two numbers the control plane sees. Every figure
// is EXCLUSIVE bytes — what removing that content would actually return — except
// BaselineSharedBytes, which is the multiply-linked baseline the forks share and nobody can
// reclaim by discarding a fork.
type StorageTotals struct {
	BaselineSharedBytes int64 `json:"baseline_shared_bytes"`
	ActiveBytes         int64 `json:"active_bytes"`
	GraceBytes          int64 `json:"grace_bytes"`
	DisposableBytes     int64 `json:"disposable_bytes"`
	OwnedOrphanBytes    int64 `json:"owned_orphan_bytes"`
	ProtectedBytes      int64 `json:"protected_bytes"`
	StagedDiscardBytes  int64 `json:"staged_discard_bytes"`
	ControlBytes        int64 `json:"control_bytes"`
	UnattributedBytes   int64 `json:"unattributed_bytes"`
	PrivateStateBytes   int64 `json:"private_state_bytes"`
	CompanionBytes      int64 `json:"companion_bytes"`
	CheckpointBytes     int64 `json:"checkpoint_bytes"`
	ReviewArtifactBytes int64 `json:"review_artifact_bytes"`
	Unknown             bool  `json:"unknown"`
}

// reclaimable is everything a discard — the control plane's or the worker's own orphan scan —
// could still return to the volume.
func (t StorageTotals) reclaimable() int64 {
	return t.DisposableBytes + t.OwnedOrphanBytes + t.StagedDiscardBytes
}

// retained is everything this worker is holding on purpose. The shared baseline belongs here: the
// parent checkout keeps those links alive, so discarding every fork would not return them.
func (t StorageTotals) retained() int64 {
	return t.BaselineSharedBytes + t.ActiveBytes + t.GraceBytes + t.ProtectedBytes + t.ControlBytes +
		t.PrivateStateBytes + t.CompanionBytes + t.CheckpointBytes + t.ReviewArtifactBytes
}

// StorageFork is one directory under a fork root and the evidence that put it in its category.
type StorageFork struct {
	Repository     string `json:"repository"`
	Name           string `json:"name"`
	Generation     string `json:"generation,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	Category       string `json:"category"`
	Reason         string `json:"reason,omitempty"`
	AllocatedBytes int64  `json:"allocated_bytes"`
	SharedBytes    int64  `json:"shared_bytes"`
	ExclusiveBytes int64  `json:"exclusive_bytes"`
	AgeSeconds     int64  `json:"age_seconds"`
	Unknown        bool   `json:"unknown,omitempty"`
}

// StorageRoot is one accounted volume.
type StorageRoot struct {
	Repository    string `json:"repository"`
	Path          string `json:"path"`
	CapacityBytes int64  `json:"capacity_bytes"`
	FreeBytes     int64  `json:"free_bytes"`
}

// StorageReport is the worker's own inventory. Storage is exactly what rides on the hello; the
// rest is detail for whoever is looking at this worker directly.
type StorageReport struct {
	Storage  workerproto.Storage `json:"storage"`
	Budget   StorageBudget       `json:"budget"`
	Totals   StorageTotals       `json:"totals"`
	Roots    []StorageRoot       `json:"roots"`
	Forks    []StorageFork       `json:"forks"`
	Problems []string            `json:"problems"`
}

// StorageRetained is a reclamation candidate that was left alone, and why.
type StorageRetained struct {
	Repository string `json:"repository"`
	Name       string `json:"name"`
	Generation string `json:"generation,omitempty"`
	Reason     string `json:"reason"`
}

// StorageReclaim is one bounded reclamation pass.
type StorageReclaim struct {
	Reclaimed      []string          `json:"reclaimed"`
	ReclaimedBytes int64             `json:"reclaimed_bytes"`
	StagedPurged   int               `json:"staged_purged"`
	Examined       int               `json:"examined"`
	Retained       []StorageRetained `json:"retained"`
	Problems       []string          `json:"problems"`
}

// StoragePressureError refuses one NEW fork and names the limit that refused it, so a controller
// can tell "this worker is full" from "this request was wrong" without parsing prose.
type StoragePressureError struct {
	Reason               string
	Repository           string
	FreeBytes            int64
	UsedBytes            int64
	ReserveBytes         int64
	HighWatermarkBytes   int64
	LowWatermarkBytes    int64
	ProtectedBytes       int64
	ProtectedBudgetBytes int64
}

func (e *StoragePressureError) Error() string { return e.detail() }

// Unwrap gives the refusal the retryable service code, so it reaches a controller as
// storage_unavailable rather than an internal failure it would stop retrying.
func (e *StoragePressureError) Unwrap() error {
	return &session.Error{Code: session.CodeStorageUnavailable, Detail: e.detail()}
}

func (e *StoragePressureError) detail() string {
	switch {
	case e.Reason == workerproto.StorageRefusalProtectedBudget:
		return fmt.Sprintf(
			"refusing a new workspace: %d bytes of protected storage exceed the %d-byte budget, and protected storage is never deleted to make room",
			e.ProtectedBytes, e.ProtectedBudgetBytes)
	case e.FreeBytes < e.ReserveBytes:
		return fmt.Sprintf("refusing a new workspace: %d free bytes are below the %d-byte reserve",
			e.FreeBytes, e.ReserveBytes)
	default:
		return fmt.Sprintf(
			"refusing a new workspace: %d used bytes reached the %d-byte high watermark; allocation reopens under the %d-byte low watermark",
			e.UsedBytes, e.HighWatermarkBytes, e.LowWatermarkBytes)
	}
}

// storagePressure is the sticky allocation decision. It is one small state machine so the number
// published on the heartbeat and the number an allocation is refused against can never disagree.
type storagePressure struct {
	closed bool
	reason string
}

func (p *storagePressure) evaluate(limits StorageLimits, capacity, free, protected int64) (string, *string) {
	used := capacity - free
	switch {
	case free < limits.ReserveBytes:
		p.closed, p.reason = true, workerproto.StorageRefusalReserveExhausted
	case protected > limits.ProtectedBudgetBytes:
		p.closed, p.reason = true, workerproto.StorageRefusalProtectedBudget
	case used >= limits.HighWatermarkBytes:
		// The protocol has two reasons and this is the space one; the watermark close is still an
		// "out of room" refusal, just an earlier one than the bare reserve floor.
		p.closed, p.reason = true, workerproto.StorageRefusalReserveExhausted
	case p.closed && used > limits.LowWatermarkBytes:
		// Hysteresis: a refusal holds until the volume is back under the LOW watermark, so one
		// reclaimed fork cannot reopen allocation just to close it again on the next create.
	default:
		p.closed, p.reason = false, ""
	}
	if !p.closed {
		return workerproto.StorageAllocationOpen, nil
	}
	reason := p.reason
	return workerproto.StorageAllocationRefused, &reason
}

// storageAccountant is the service's storage state: the configured policy, the sticky pressure
// decision, the last measurement, and the admissions currently in flight.
type storageAccountant struct {
	mu            sync.Mutex
	measureMu     sync.Mutex
	reclaimMu     sync.Mutex
	configured    *StorageLimits
	pressure      storagePressure
	report        *StorageReport
	measuredAt    time.Time
	inflight      int
	meanForkBytes int64
	reclaimCursor int
}

// setStorageLimits replaces the policy and drops the cached measurement, so the next report is
// classified under the new one.
func (s *Service) setStorageLimits(limits StorageLimits) error {
	if err := limits.Validate(0); err != nil {
		return err
	}
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	bound := limits
	s.storage.configured = &bound
	s.storage.report = nil
	return nil
}

func (s *Service) storageLimitsLocked(capacityBytes int64) StorageLimits {
	if s.storage.configured != nil {
		return *s.storage.configured
	}
	return DefaultStorageLimits(capacityBytes)
}

// storageRepositories is every fork root this worker owns: the repositories its policies serve,
// plus any repository a stored session still names, so a policy edited out of the file cannot hide
// the forks it left behind.
func (s *Service) storageRepositories(sessions []session.Session) []string {
	seen := map[string]bool{}
	var repos []string
	add := func(repo string) {
		if repo == "" || !filepath.IsAbs(repo) || seen[repo] {
			return
		}
		seen[repo] = true
		repos = append(repos, repo)
	}
	for _, policy := range s.policies {
		add(policy.Repository)
	}
	for _, sess := range sessions {
		add(sess.Repository)
	}
	sort.Strings(repos)
	return repos
}

// storageMeasurePath is the existing path whose volume a repository's forks land on.
func storageMeasurePath(repo string) string {
	home := forkspace.Home(repo)
	if info, err := os.Lstat(home); err == nil && info.IsDir() {
		return home
	}
	return repo
}

// StorageReport returns the worker's storage accounting, re-measuring only when the cached one is
// older than the configured interval. A tree walk is far too expensive to run on every heartbeat,
// so measured_at carries the staleness instead of the walk carrying the cost.
func (s *Service) StorageReport(ctx context.Context) (StorageReport, error) {
	if report, ok := s.cachedStorageReport(); ok {
		return report, nil
	}
	s.storage.measureMu.Lock()
	defer s.storage.measureMu.Unlock()
	if report, ok := s.cachedStorageReport(); ok {
		return report, nil
	}
	report, err := s.measureStorage(ctx)
	if err != nil {
		return StorageReport{}, err
	}
	s.storage.mu.Lock()
	bound := report
	s.storage.report = &bound
	s.storage.measuredAt = time.Now()
	s.storage.meanForkBytes = meanForkBytes(report.Forks)
	s.storage.mu.Unlock()
	return report, nil
}

func (s *Service) cachedStorageReport() (StorageReport, bool) {
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	if s.storage.report == nil {
		return StorageReport{}, false
	}
	// The accountant's own stamp, not the published one: a measurement that found no readable
	// volume publishes no storage object, and it must still be cached or every heartbeat would
	// pay for a full tree walk.
	limits := s.storageLimitsLocked(s.storage.report.Storage.CapacityBytes)
	if time.Since(s.storage.measuredAt) >= limits.MeasureInterval {
		return StorageReport{}, false
	}
	return *s.storage.report, true
}

func meanForkBytes(forks []StorageFork) int64 {
	total, counted := int64(0), int64(0)
	for _, fork := range forks {
		if fork.Generation == "" {
			continue // not a fork coop created: control state, staged garbage, or a foreign directory
		}
		total += fork.ExclusiveBytes
		counted++
	}
	if counted == 0 {
		return 0
	}
	return total / counted
}

func (s *Service) measureStorage(ctx context.Context) (StorageReport, error) {
	// measured_at is published at second resolution; the classification compares against the real
	// clock, because a truncated "now" can land BEFORE a session that closed this second and turn
	// an eligible workspace back into one inside its grace window.
	now := time.Now().UTC()
	measuredAt := now.Truncate(time.Second)
	sessions, err := s.store.ListSessionsForRecovery(ctx)
	if err != nil {
		return StorageReport{}, err
	}
	byFork := make(map[string]session.Session, len(sessions))
	for _, sess := range sessions {
		byFork[storageForkKey(sess.Repository, sess.ForkName, sess.ForkGeneration)] = sess
	}

	report := StorageReport{}
	scan := forkspace.NewUsageScan()
	// Classification needs only the grace window, which does not depend on capacity; the
	// capacity-bound limits are resolved once the tightest volume is known below.
	limits := s.storageLimitsSnapshot()
	for _, repo := range s.storageRepositories(sessions) {
		s.measureForkRoot(scan, repo, byFork, limits, now, &report)
	}
	s.measurePrivateState(scan, &report)

	volume, ok := s.accountedVolume(&report)
	if !ok {
		// No volume could be read. Publish nothing rather than a zeroed disk: unknown is not zero.
		report.Budget = limits.budget()
		return report, nil
	}
	limits = s.storageLimitsFor(volume.CapacityBytes)
	unattributed := report.Totals.UnattributedBytes
	published := &unattributed
	if report.Totals.Unknown {
		published = nil
	}
	allocation, reason := s.evaluateStoragePressure(limits, volume.CapacityBytes, volume.FreeBytes, report.Totals.retained())
	report.Budget = limits.budget()
	report.Storage = workerproto.Storage{
		Version: workerproto.StorageVersion, MeasuredAt: measuredAt,
		CapacityBytes: volume.CapacityBytes, FreeBytes: volume.FreeBytes,
		ReserveBytes: limits.ReserveBytes, HighWatermarkBytes: limits.HighWatermarkBytes,
		LowWatermarkBytes: limits.LowWatermarkBytes,
		DisposableBytes:   report.Totals.reclaimable(), ProtectedBytes: report.Totals.retained(),
		UnattributedBytes: published, Allocation: allocation, RefusalReason: reason,
	}
	if err := report.Storage.Validate(); err != nil {
		// Either a bug or limits an operator wrote that do not fit the volume this daemon actually
		// measured. Advertise nothing rather than numbers describing no real disk, and say so
		// somewhere an operator will see without polling the endpoint.
		report.Storage = workerproto.Storage{}
		report.Problems = append(report.Problems, "storage accounting is incoherent: "+err.Error())
		s.log.Warn("worker storage accounting is incoherent and will not be advertised",
			"capacity_bytes", volume.CapacityBytes, "error", err)
	}
	return report, nil
}

func (s *Service) storageLimitsSnapshot() StorageLimits {
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	return s.storageLimitsLocked(0)
}

func (s *Service) storageLimitsFor(capacityBytes int64) StorageLimits {
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	return s.storageLimitsLocked(capacityBytes)
}

func (s *Service) evaluateStoragePressure(limits StorageLimits, capacity, free, protected int64) (string, *string) {
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	return s.storage.pressure.evaluate(limits, capacity, free, protected)
}

// accountedVolume is the tightest volume behind this worker's roots. Admission is bounded by
// whichever filesystem runs out first, and capacity/free have to come from the same statfs or the
// pair describes no real disk.
func (s *Service) accountedVolume(report *StorageReport) (forkspace.Filesystem, bool) {
	tightest, found := forkspace.Filesystem{}, false
	for _, root := range report.Roots {
		volume := forkspace.Filesystem{CapacityBytes: root.CapacityBytes, FreeBytes: root.FreeBytes}
		if volume.CapacityBytes <= 0 {
			continue
		}
		if !found || volume.FreeBytes < tightest.FreeBytes {
			tightest, found = volume, true
		}
	}
	return tightest, found
}

func storageForkKey(repo, name, generation string) string {
	return repo + "\x00" + name + "\x00" + generation
}

func (s *Service) measureForkRoot(
	scan *forkspace.UsageScan,
	repo string,
	byFork map[string]session.Session,
	limits StorageLimits,
	now time.Time,
	report *StorageReport,
) {
	root := StorageRoot{Repository: repo, Path: storageMeasurePath(repo)}
	if volume, err := forkspace.MeasureFilesystem(root.Path); err == nil {
		root.CapacityBytes, root.FreeBytes = volume.CapacityBytes, volume.FreeBytes
	} else {
		report.Problems = append(report.Problems, err.Error())
	}
	report.Roots = append(report.Roots, root)

	home := forkspace.Home(repo)
	entries, err := os.ReadDir(home)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		report.Problems = append(report.Problems, err.Error())
		report.Totals.Unknown = true
		return
	}
	if len(entries) > storageForkRootEntries {
		report.Problems = append(report.Problems, fmt.Sprintf("fork root %q exceeds %d entries", home, storageForkRootEntries))
		report.Totals.Unknown = true
		return
	}
	for _, entry := range entries {
		path := filepath.Join(home, entry.Name())
		switch {
		case entry.Name() == filepath.Base(forkspace.StagedDiscardDir(repo)):
			// Proven garbage mid-removal. It is reclaimable, and it is measured separately so an
			// operator can tell "coop is cleaning up" from "coop is holding this".
			usage := s.measureTree(scan, path, report)
			report.Totals.StagedDiscardBytes += usage.ExclusiveBytes()
			report.Totals.BaselineSharedBytes += usage.SharedBytes
			report.Forks = append(report.Forks, storageOwnEntry(repo, entry.Name(), usage,
				StorageCategoryStagedDiscard, "workspaces proven removable whose deletion has not finished"))
		case entry.Name() == filepath.Base(forkspace.StateDir(repo)):
			usage := s.measureTree(scan, path, report)
			report.Totals.ControlBytes += usage.ExclusiveBytes()
			report.Totals.BaselineSharedBytes += usage.SharedBytes
			report.Forks = append(report.Forks, storageOwnEntry(repo, entry.Name(), usage,
				StorageCategoryControl, "coop's own fork lifecycle, generation, reservation and execution records"))
		default:
			s.measureForkEntry(scan, repo, entry.Name(), path, entry.IsDir(), byFork, limits, now, report)
		}
	}
}

// storageOwnEntry is the inventory line for a directory coop itself owns under a fork root.
func storageOwnEntry(repo, name string, usage forkspace.Usage, category, reason string) StorageFork {
	return StorageFork{
		Repository: repo, Name: name, Category: category, Reason: reason,
		AllocatedBytes: usage.AllocatedBytes, SharedBytes: usage.SharedBytes,
		ExclusiveBytes: usage.ExclusiveBytes(), Unknown: usage.Unknown,
	}
}

func (s *Service) measureTree(scan *forkspace.UsageScan, path string, report *StorageReport) forkspace.Usage {
	usage, err := scan.Measure(path)
	if errors.Is(err, os.ErrNotExist) {
		return forkspace.Usage{}
	}
	if err != nil {
		report.Problems = append(report.Problems, err.Error())
		report.Totals.Unknown = true
		return forkspace.Usage{}
	}
	if usage.Unknown {
		report.Totals.Unknown = true
	}
	return usage
}

func (s *Service) measureForkEntry(
	scan *forkspace.UsageScan,
	repo, name, path string,
	isDir bool,
	byFork map[string]session.Session,
	limits StorageLimits,
	now time.Time,
	report *StorageReport,
) {
	usage := forkspace.Usage{}
	if isDir {
		usage = s.measureTree(scan, path, report)
	} else {
		// A loose file or symlink in a fork root is not a workspace and has no measured block
		// count here. Report it as storage nobody accounted for rather than inventing a number in
		// different units from every other total.
		usage.Unknown = true
		report.Totals.Unknown = true
	}
	fork := StorageFork{
		Repository: repo, Name: name,
		AllocatedBytes: usage.AllocatedBytes, SharedBytes: usage.SharedBytes,
		ExclusiveBytes: usage.ExclusiveBytes(), Unknown: usage.Unknown,
	}
	identity, owned, err := forkspace.ReadGeneration(repo, name)
	if err != nil {
		report.Problems = append(report.Problems, fmt.Sprintf("%s: %v", name, err))
	}
	if !isDir || !owned || err != nil || !forkspace.ValidExistingName(name) {
		// Nothing proves coop created this. It is storage, it is reported, and it is never
		// reclaimed on a guess.
		fork.Category, fork.Reason = StorageCategoryUnattributed, "no coop generation record proves ownership"
		report.Totals.UnattributedBytes += fork.ExclusiveBytes
		report.Totals.BaselineSharedBytes += fork.SharedBytes
		report.Forks = append(report.Forks, fork)
		return
	}
	fork.Generation = string(identity.Generation)
	var owner *session.Session
	if sess, ok := byFork[storageForkKey(repo, name, string(identity.Generation))]; ok {
		owner = &sess
	} else if sess, ok := byFork[storageForkKey(repo, name, "")]; ok {
		// A session bound before fork generations existed still owns its workspace.
		owner = &sess
	}
	if owner != nil {
		fork.SessionID = owner.ID
		fork.AgeSeconds = int64(now.Sub(owner.UpdatedAt).Seconds())
	} else if created, err := forkspace.GenerationCreatedAt(repo, identity); err == nil {
		fork.AgeSeconds = int64(now.Sub(created).Seconds())
	}
	fork.Category, fork.Reason = s.classifyFork(repo, identity, owner, limits, now)
	switch fork.Category {
	case StorageCategoryActive:
		report.Totals.ActiveBytes += fork.ExclusiveBytes
	case StorageCategoryGrace:
		report.Totals.GraceBytes += fork.ExclusiveBytes
	case StorageCategoryDisposable:
		report.Totals.DisposableBytes += fork.ExclusiveBytes
	case StorageCategoryOwnedOrphan:
		report.Totals.OwnedOrphanBytes += fork.ExclusiveBytes
	default:
		report.Totals.ProtectedBytes += fork.ExclusiveBytes
	}
	report.Totals.BaselineSharedBytes += fork.SharedBytes
	report.Forks = append(report.Forks, fork)
}

// classifyFork decides what may happen to one owned fork, cheapest and most conclusive proof
// first. Everything it cannot positively clear stays protected.
func (s *Service) classifyFork(
	repo string,
	identity forkspace.Identity,
	owner *session.Session,
	limits StorageLimits,
	now time.Time,
) (string, string) {
	if forkspace.NeedsStop(repo, identity.Name) {
		return StorageCategoryProtected, "a worker or its cleanup is still pending"
	}
	if err := forkspace.RequireNoForkExecutionsLocked(repo, identity); err != nil {
		return StorageCategoryProtected, "sandbox activity is registered against this fork"
	}
	if _, err := os.Lstat(forkspace.LandIntentPath(repo, identity)); err == nil {
		return StorageCategoryProtected, "an interrupted fork land still owns this workspace"
	}
	if active, err := tasks.ForkTaskState(repo, identity); err != nil || active {
		return StorageCategoryProtected, "canonical task authority lives in this workspace"
	}
	if owner != nil {
		if s.sessionQuarantined(owner.ID) {
			return StorageCategoryProtected, "its session is quarantined"
		}
		if owner.State != session.SessionClosed && owner.State != session.SessionDiscarded {
			return StorageCategoryActive, "a live session owns it"
		}
		if now.Sub(owner.UpdatedAt) < limits.GraceWindow {
			return StorageCategoryGrace, "inside the close grace window"
		}
		if dirty, reason := s.forkIsDirty(repo, identity.Name); dirty {
			return StorageCategoryProtected, reason
		}
		return StorageCategoryDisposable, "its session is finished and its workspace is clean"
	}
	if _, reserved, err := forkspace.ReadWorkspaceReservation(repo, identity); err != nil || reserved {
		return StorageCategoryProtected, "a workspace reservation still names an owner"
	}
	created, err := forkspace.GenerationCreatedAt(repo, identity)
	if err != nil {
		return StorageCategoryProtected, "its generation record cannot be read"
	}
	if now.Sub(created) < limits.GraceWindow {
		// A generation only seconds old is far more likely to be a create still in flight than
		// garbage, and the session row it belongs to is written after the workspace.
		return StorageCategoryProtected, "an unbound generation younger than the reclaim age"
	}
	if dirty, reason := s.forkIsDirty(repo, identity.Name); dirty {
		return StorageCategoryProtected, reason
	}
	return StorageCategoryOwnedOrphan, "no session names this generation"
}

// forkIsDirty reports uncommitted work. An unreadable status counts as dirty: a workspace coop
// cannot inspect is one it cannot prove is empty.
func (s *Service) forkIsDirty(repo, name string) (bool, string) {
	workspace := forkspace.Workspace(repo, name)
	status, truncated, err := runSessionWorkspaceGit(workspace, sessionWorkspaceGitOutputLimit,
		"status", "--porcelain=v2", "--untracked-files=all", "--no-renames", "-z")
	if err != nil {
		return true, "its workspace status cannot be read"
	}
	if truncated || len(status) > 0 {
		return true, "it holds uncommitted work"
	}
	return false, ""
}

// measurePrivateState accounts the per-session stores that outlive a turn: the agent's private
// state, companion checkouts, retained workspace checkpoints and review patches. They are counted
// by Lstat only — nothing here opens a file, because this state is credential-bearing.
func (s *Service) measurePrivateState(scan *forkspace.UsageScan, report *StorageReport) {
	root := StorageRoot{Path: s.stateRoot}
	if volume, err := forkspace.MeasureFilesystem(s.stateRoot); err == nil {
		root.CapacityBytes, root.FreeBytes = volume.CapacityBytes, volume.FreeBytes
		report.Roots = append(report.Roots, root)
	}
	for name, into := range map[string]*int64{
		"acp":                   &report.Totals.PrivateStateBytes,
		"repositories":          &report.Totals.CompanionBytes,
		"workspace-checkpoints": &report.Totals.CheckpointBytes,
		"review-artifacts":      &report.Totals.ReviewArtifactBytes,
	} {
		usage := s.measureTree(scan, filepath.Join(s.stateRoot, name), report)
		*into += usage.ExclusiveBytes()
		report.Totals.BaselineSharedBytes += usage.SharedBytes
	}
}

// admitNewFork is the atomic pre-allocation pressure check. It re-reads free space under the
// accountant's lock — a statfs, not a walk — counts the admissions already in flight against the
// mean measured fork, and refuses a NEW workspace that would push the volume past its limits.
//
// It deliberately does NOT bound what an already-running task writes: once a fork exists, its
// agent can fill the disk, and nothing here pretends otherwise.
func (s *Service) admitNewFork(repo, _ string) (func(), error) {
	release := func() {}
	volume, err := forkspace.MeasureFilesystem(storageMeasurePath(repo))
	if err != nil {
		// A worker that cannot read its own volume keeps working. Turning an unreadable statfs
		// into a refusal would make a monitoring failure an outage.
		s.log.Warn("worker storage could not be measured before allocation", "repository", repo, "error", err)
		return release, nil
	}
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	limits := s.storageLimitsLocked(volume.CapacityBytes)
	protected := int64(0)
	if s.storage.report != nil {
		protected = s.storage.report.Storage.ProtectedBytes
	}
	projected := volume.FreeBytes - int64(s.storage.inflight)*s.storage.meanForkBytes
	if projected < 0 {
		projected = 0
	}
	allocation, reason := s.storage.pressure.evaluate(limits, volume.CapacityBytes, projected, protected)
	if allocation == workerproto.StorageAllocationRefused && reason != nil {
		return nil, &StoragePressureError{
			Reason: *reason, Repository: repo, FreeBytes: projected, UsedBytes: volume.CapacityBytes - projected,
			ReserveBytes: limits.ReserveBytes, HighWatermarkBytes: limits.HighWatermarkBytes,
			LowWatermarkBytes: limits.LowWatermarkBytes,
			ProtectedBytes:    protected, ProtectedBudgetBytes: limits.ProtectedBudgetBytes,
		}
	}
	s.storage.inflight++
	return func() {
		s.storage.mu.Lock()
		s.storage.inflight--
		s.storage.mu.Unlock()
	}, nil
}

// ReclaimOwnedOrphans finishes interrupted removals and reclaims fork storage this worker can
// prove is garbage: coop's own generation record, no session naming it, no reservation, no live
// worker or sandbox activity, past the reclaim age, and a clean tree fully contained by its
// parent. Every other candidate is REPORTED, never deleted. The pass is bounded and single-flight,
// so cleanup can never crowd out the runtime work it exists to protect.
func (s *Service) ReclaimOwnedOrphans(ctx context.Context) (StorageReclaim, error) {
	if !s.storage.reclaimMu.TryLock() {
		return StorageReclaim{}, nil
	}
	defer s.storage.reclaimMu.Unlock()
	sessions, err := s.store.ListSessionsForRecovery(ctx)
	if err != nil {
		return StorageReclaim{}, err
	}
	byFork := make(map[string]session.Session, len(sessions))
	for _, sess := range sessions {
		byFork[storageForkKey(sess.Repository, sess.ForkName, sess.ForkGeneration)] = sess
		byFork[storageForkKey(sess.Repository, sess.ForkName, "")] = sess
	}
	limits := s.storageLimitsSnapshot()
	result := StorageReclaim{}
	for _, repo := range s.storageRepositories(sessions) {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		purged, err := forkspace.PurgeStagedDiscards(repo, "")
		result.StagedPurged += purged
		if err != nil {
			result.Problems = append(result.Problems, err.Error())
		}
		s.reclaimRepositoryOrphans(ctx, repo, byFork, limits, &result)
	}
	return result, nil
}

// reclaimStorageOnce is the maintenance loop's bounded reclamation pass. It shares the existing
// cleanup tick rather than adding a second cleanup daemon, and it logs only when it did something.
func (s *Service) reclaimStorageOnce(ctx context.Context) {
	result, err := s.ReclaimOwnedOrphans(ctx)
	if err != nil && ctx.Err() == nil {
		s.log.Error("worker storage reclamation failed", "error", err)
		return
	}
	for _, problem := range result.Problems {
		s.log.Warn("worker storage reclamation problem", "problem", problem)
	}
	if len(result.Reclaimed) > 0 || result.StagedPurged > 0 {
		s.log.Info("reclaimed owned fork storage",
			"forks", len(result.Reclaimed), "bytes", result.ReclaimedBytes, "staged_purged", result.StagedPurged)
	}
}

func (s *Service) reclaimRepositoryOrphans(
	ctx context.Context,
	repo string,
	byFork map[string]session.Session,
	limits StorageLimits,
	result *StorageReclaim,
) {
	identities, problems := forkspace.GenerationIdentities(repo)
	for _, problem := range problems {
		result.Problems = append(result.Problems, problem.Error())
	}
	if len(identities) == 0 {
		return
	}
	// A rotating start keeps one permanently protected candidate from consuming every pass and
	// starving the reclaimable ones behind it.
	s.storage.mu.Lock()
	start := s.storage.reclaimCursor % len(identities)
	s.storage.mu.Unlock()
	examined := 0
	for offset := range identities {
		if examined >= limits.MaxReclaimPerPass || ctx.Err() != nil {
			break
		}
		identity := identities[(start+offset)%len(identities)]
		if _, owned := byFork[storageForkKey(repo, identity.Name, string(identity.Generation))]; owned {
			continue
		}
		if _, owned := byFork[storageForkKey(repo, identity.Name, "")]; owned {
			continue
		}
		examined++
		result.Examined++
		reclaimed, reason := s.reclaimOrphan(repo, identity, limits)
		if reason != "" {
			result.Retained = append(result.Retained, StorageRetained{
				Repository: repo, Name: identity.Name, Generation: string(identity.Generation), Reason: reason,
			})
			continue
		}
		result.Reclaimed = append(result.Reclaimed, identity.Name+"."+string(identity.Generation))
		result.ReclaimedBytes += reclaimed
	}
	s.storage.mu.Lock()
	s.storage.reclaimCursor += examined
	s.storage.mu.Unlock()
}

// reclaimOrphan removes one proven orphan and reports the bytes it returned, or leaves it where it
// is and reports why. The bytes are measured before the removal; measuring afterwards would always
// report zero.
func (s *Service) reclaimOrphan(repo string, identity forkspace.Identity, limits StorageLimits) (int64, string) {
	created, err := forkspace.GenerationCreatedAt(repo, identity)
	if err != nil {
		return 0, "its generation record cannot be read: " + err.Error()
	}
	if time.Since(created) < limits.GraceWindow {
		return 0, "it is younger than the reclaim age"
	}
	if _, reserved, err := forkspace.ReadWorkspaceReservation(repo, identity); err != nil || reserved {
		return 0, "a workspace reservation still names an owner"
	}
	workspace := forkspace.Workspace(repo, identity.Name)
	reclaimed := int64(0)
	if usage, err := forkspace.MeasureUsage(workspace); err == nil {
		reclaimed = usage.ExclusiveBytes()
	}
	plan, err := planSessionWorkspaceDiscard(repo, workspace, false, false)
	if err != nil {
		return 0, "it cannot be planned for discard: " + err.Error()
	}
	// The scan chose this generation. A plan that resolved a different one — a fork recreated
	// under the same name between the scan and now — must never be executed under its authority.
	if plan.Fork == nil || *plan.Fork != identity {
		return 0, fmt.Sprintf("its generation changed between the scan and the plan: planned %v, intended %s",
			plan.Fork, identity.Generation)
	}
	if err := discardSessionWorkspace(plan); err != nil {
		return 0, "its discard was refused: " + err.Error()
	}
	return reclaimed, ""
}
