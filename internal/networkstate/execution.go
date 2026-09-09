package networkstate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/processidentity"
)

const (
	ExecutionVersion     = 1
	ExecutionPageSize    = 100
	ExecutionLockTimeout = 5 * time.Second
)

var (
	ErrExecutionConflict   = errors.New("network execution revision conflict; reread before retrying")
	ErrSnapshotStale       = errors.New("network snapshot is older than retained evidence; discard this observation")
	ErrEvidenceUnavailable = errors.New("network evidence is unavailable or expired")
)

type Supervisor struct {
	PID        int    `json:"pid"`
	StartToken string `json:"start_token"`
}

// Resource names exist in the intent before any runtime operation. An empty ID
// after a creating transition means unknown outcome, never confirmed absence.
type Resource struct {
	Role  string `json:"role"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	ID    string `json:"id,omitempty"`
	State string `json:"state"`
}

// Execution is OWNER-PRIVATE storage, never a public DTO. In particular Project,
// Supervisor and runtime identities must not pass through worker/session JSON.
type Execution struct {
	Version               int                  `json:"version"`
	Revision              networkview.Count    `json:"revision"`
	ID                    string               `json:"id"`
	Epoch                 string               `json:"epoch"`
	Project               string               `json:"project"`
	Scope                 string               `json:"scope"`
	Supervisor            Supervisor           `json:"supervisor"`
	Runtime               string               `json:"runtime"`
	DaemonID              string               `json:"daemon_id"`
	Endpoint              string               `json:"endpoint"`
	GatewayImage          string               `json:"gateway_image"`
	ClientImage           string               `json:"client_image"`
	QualificationID       string               `json:"qualification_id,omitempty"`
	QualificationContract string               `json:"qualification_contract"`
	Purpose               string               `json:"purpose"`
	WorkloadStarted       bool                 `json:"workload_started,omitempty"`
	ReadySequence         networkview.Count    `json:"ready_sequence,omitempty"`
	SessionID             string               `json:"session_id,omitempty"`
	AttemptID             string               `json:"attempt_id,omitempty"`
	BundleReferences      []string             `json:"bundle_references"`
	StartedAt             time.Time            `json:"started_at"`
	Resources             []Resource           `json:"resources"`
	Artifact              Artifact             `json:"artifact"`
	Snapshot              networkview.Snapshot `json:"snapshot"`
	ObserverAfterWorkload bool                 `json:"observer_after_workload"`
	Receipt               *networkview.Receipt `json:"receipt,omitempty"`
}

type ExecutionSpec struct {
	Project, PolicyFingerprint, Runtime, DaemonID, Endpoint, GatewayImage string
	SessionID, AttemptID                                                  string
	QualificationID, ClientImage                                          string
}

// Evidence cannot create resources, grant authority, or replace live snapshots.
// Its root is opened without creating/regenerating an owner key. Inspection and
// exact-owned cleanup do not depend on the source project still existing.
type Evidence struct{ files *Store }

func OpenEvidence(path string, exposed []string) (*Evidence, error) {
	files, err := openFiles(path, exposed, false)
	if err != nil {
		return nil, err
	}
	return &Evidence{files: files}, nil
}
func (e *Evidence) Close() error                           { return e.files.Close() }
func (e *Evidence) Execution(id string) (Execution, error) { return e.files.execution(id) }
func (s *Store) Execution(id string) (Execution, error)    { return s.execution(id) }

func randomExecutionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
func lowerHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' && char < 'a' || char > 'f' {
			return false
		}
	}
	return true
}

func (s *Store) CreateExecution(ctx context.Context, spec ExecutionSpec) (Execution, error) {
	return s.createExecution(ctx, spec, nil)
}

func (s *Store) createExecution(ctx context.Context, spec ExecutionSpec, smoke *QualificationSmoke) (Execution, error) {
	if err := s.authorityAvailable(); err != nil {
		return Execution{}, err
	}
	policy, err := s.LoadSnapshot(spec.Project, spec.PolicyFingerprint)
	if err != nil {
		return Execution{}, err
	}
	if policy.Mode != egress.Filtered {
		return Execution{}, errors.New("gateway execution requires a filtered capture")
	}
	if err := policy.RequireTLS443(true); err != nil {
		return Execution{}, err
	}
	var candidate CandidateSpec
	if smoke == nil {
		qualification, err := s.Qualification(spec.QualificationID)
		if err != nil {
			return Execution{}, err
		}
		if err := qualification.RequireLaunch(policy); err != nil {
			return Execution{}, err
		}
		candidate = qualification.Candidate
	} else {
		candidate = smoke.candidate
	}
	if spec.Runtime != "docker" || spec.DaemonID != candidate.Runtime.DaemonID || spec.Endpoint != candidate.Runtime.Endpoint ||
		spec.GatewayImage != candidate.GatewayImage || spec.ClientImage != candidate.ClientImage {
		return Execution{}, errors.New("network execution differs from its exact candidate image pair or runtime")
	}
	project, err := canonicalPath(spec.Project)
	if err != nil {
		return Execution{}, err
	}
	if spec.Runtime != "docker" || spec.DaemonID == "" || len(spec.DaemonID) > 128 || !localEndpoint(spec.Endpoint) || !strings.HasPrefix(spec.GatewayImage, "sha256:") || !lowerHex(strings.TrimPrefix(spec.GatewayImage, "sha256:"), 64) {
		return Execution{}, errors.New("network execution requires exact Docker daemon and helper image identity")
	}
	id, err := randomExecutionID()
	if err != nil {
		return Execution{}, err
	}
	epoch, err := randomExecutionID()
	if err != nil {
		return Execution{}, err
	}
	owner := Supervisor{PID: os.Getpid(), StartToken: processidentity.StartToken(os.Getpid())}
	if !processidentity.Stable(owner.StartToken) {
		return Execution{}, errors.New("network supervisor identity unavailable")
	}
	now := time.Now().UTC()
	record := Execution{Version: ExecutionVersion, Revision: 1, ID: id, Epoch: epoch, Project: project, Scope: policy.Scope,
		Supervisor: owner, Runtime: spec.Runtime, DaemonID: spec.DaemonID, Endpoint: spec.Endpoint, GatewayImage: spec.GatewayImage, SessionID: spec.SessionID,
		AttemptID: spec.AttemptID, StartedAt: now,
		Purpose:               "workload",
		ClientImage:           spec.ClientImage,
		QualificationID:       spec.QualificationID,
		QualificationContract: QualificationContract,
		Artifact:              Artifact{Name: "artifacts-" + id, State: "planned"},
		Snapshot: networkview.Snapshot{Version: networkview.Version, RunID: id, Epoch: epoch, Mode: policy.Mode,
			PolicyFingerprint: policy.Fingerprint, Availability: "starting", AsOf: now, Scope: "not-observed", Projection: "owner-local"}}
	if smoke != nil {
		record.Purpose = "qualification"
	}
	for _, role := range []string{"controller", "guard", "agent", "ipc", "observations"} {
		kind := "container"
		if role == "ipc" || role == "observations" {
			kind = "volume"
		}
		record.Resources = append(record.Resources, Resource{Role: role, Kind: kind, Name: "coop-net-" + id + "-" + role, State: "planned"})
	}
	for _, dependency := range policy.Dependencies {
		record.BundleReferences = append(record.BundleReferences, dependencyReference(dependency))
	}
	if err := validExecution(record); err != nil {
		return Execution{}, err
	}
	err = s.lockExecution(ctx, id, func() error {
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return s.publish("execution-"+id+".json", data, false)
	})
	// Preserve the intended identity on an ambiguous fsync failure. The caller
	// can reread this ID; it must not create another runtime resource blindly.
	return record, err
}

func (s *Store) execution(id string) (Execution, error) {
	if !lowerHex(id, 32) {
		return Execution{}, ErrEvidenceUnavailable
	}
	data, err := s.read("execution-"+id+".json", maxPrivateRecordBytes)
	if errors.Is(err, os.ErrNotExist) {
		return Execution{}, ErrEvidenceUnavailable
	}
	if err != nil {
		return Execution{}, err
	}
	var record Execution
	if err := strictJSON(data, &record); err != nil {
		return Execution{}, err
	}
	if record.ID != id {
		return Execution{}, errors.New("invalid network execution identity")
	}
	if err := validExecution(record); err != nil {
		return Execution{}, err
	}
	return record, nil
}

func validExecution(record Execution) error {
	if record.Version != ExecutionVersion || !lowerHex(record.ID, 32) || !lowerHex(record.Epoch, 32) || record.Revision == 0 ||
		!lowerHex(record.Scope, 64) || !filepath.IsAbs(record.Project) || filepath.Clean(record.Project) != record.Project ||
		record.Supervisor.PID <= 1 || !processidentity.Stable(record.Supervisor.StartToken) || record.StartedAt.IsZero() || record.Runtime != "docker" ||
		!safeRecordToken(record.DaemonID, 128) || !localEndpoint(record.Endpoint) || !strings.HasPrefix(record.GatewayImage, "sha256:") || !lowerHex(strings.TrimPrefix(record.GatewayImage, "sha256:"), 64) ||
		record.Snapshot.Version != networkview.Version || record.Snapshot.RunID != record.ID || record.Snapshot.Epoch != record.Epoch || record.Snapshot.Mode != egress.Filtered ||
		!lowerHex(record.Snapshot.PolicyFingerprint, 64) || len(record.Resources) != 5 || len(record.BundleReferences) > egress.MaxGrants ||
		record.SessionID != "" && !safeRecordToken(record.SessionID, 128) || record.AttemptID != "" && !safeRecordToken(record.AttemptID, 128) {
		return errors.New("invalid network execution identity")
	}
	if !imageDigest(record.ClientImage) || !safeRecordToken(record.QualificationContract, 128) {
		return errors.New("network execution lacks exact launch binding")
	}
	if record.ReadySequence > record.Snapshot.Sequence || record.WorkloadStarted && record.Resources[2].ID == "" {
		return errors.New("invalid network execution startup evidence")
	}
	switch record.Purpose {
	case "workload":
		if !lowerHex(record.QualificationID, 64) {
			return errors.New("invalid qualified workload binding")
		}
	case "qualification":
		if record.QualificationID != "" {
			return errors.New("invalid host preflight binding")
		}
	default:
		return errors.New("unknown network execution purpose")
	}
	if err := validArtifact(record); err != nil {
		return err
	}
	for i, role := range []string{"controller", "guard", "agent", "ipc", "observations"} {
		resource := record.Resources[i]
		kind := "container"
		if i >= 3 {
			kind = "volume"
		}
		if resource.Role != role || resource.Kind != kind || resource.Name != "coop-net-"+record.ID+"-"+role ||
			!slices.Contains([]string{"planned", "creating", "created", "starting", "started", "gone"}, resource.State) ||
			(kind == "volume" && (resource.State == "starting" || resource.State == "started")) ||
			(resource.State == "planned" || resource.State == "creating") && resource.ID != "" ||
			(resource.State == "created" || resource.State == "starting" || resource.State == "started") && resource.ID == "" ||
			resource.ID != "" && (kind == "container" && !lowerHex(resource.ID, 64) || kind == "volume" && resource.ID != resource.Name) {
			return errors.New("invalid network resource intent or binding")
		}
	}
	if receipt := record.Receipt; receipt != nil {
		copy := *receipt
		if receipt.ID != record.ID || receipt.Snapshot.RunID != record.ID || receipt.Snapshot.Epoch != record.Epoch ||
			receipt.Snapshot.PolicyFingerprint != record.Snapshot.PolicyFingerprint || receipt.Finality != "final" || receipt.EndedAt == nil ||
			copy.SealDigest() != nil || copy.Digest != receipt.Digest {
			return errors.New("invalid sealed network receipt")
		}
	}
	return nil
}

func localEndpoint(value string) bool {
	u, err := url.Parse(value)
	return len(value) <= 4096 && err == nil && u.Scheme == "unix" && u.Host == "" && u.User == nil && u.Opaque == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" &&
		filepath.IsAbs(u.Path) && filepath.Clean(u.Path) == u.Path && u.Path != "/" && !strings.ContainsAny(u.Path, "\x00\r\n")
}

func safeRecordToken(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, char := range value {
		if char < 33 || char > 126 {
			return false
		}
	}
	return true
}

// Locks are striped across 64 stable inodes, never unlinked by retention. This
// bounds lock files without an ever-growing inode set or split-lock GC race.
// Only bounded read/CAS/publish work holds a lock; runtime calls never do.
func (s *Store) lockExecution(ctx context.Context, id string, operation func() error) error {
	if !lowerHex(id, 32) {
		return ErrEvidenceUnavailable
	}
	return s.lockRecord(ctx, "execution", id, operation)
}

// Approval review uses a separate bounded stripe set, so a human prompt never
// holds an execution lock and publication cannot race another approval writer.
func (s *Store) lockRecord(ctx context.Context, kind, id string, operation func() error) error {
	if (kind != "execution" && kind != "approval") || (!lowerHex(id, 32) && !lowerHex(id, 64)) {
		return errors.New("invalid network record lock")
	}
	if ctx == nil {
		return errors.New("network record lock requires context")
	}
	ctx, cancel := context.WithTimeout(ctx, ExecutionLockTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	value, _ := hex.DecodeString(id[:2])
	name := fmt.Sprintf("%s-lock-%02x", kind, value[0]%64)
	dir, err := s.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	// Make simultaneous first creators race on exclusive publication. Reopen
	// only a confirmed existing inode; never retry a failed create as authority.
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_CREAT|unix.O_EXCL, 0o600)
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(int(dir.Fd()), name, flags, 0)
	}
	if err != nil {
		return fmt.Errorf("open network %s lock: %w", kind, err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if err := privateInfo(info, false); err != nil {
		return err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("acquire network %s lock: %w", kind, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	current, err := s.root.Lstat(name)
	if err != nil || !os.SameFile(info, current) {
		return fmt.Errorf("network %s lock identity changed", kind)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return operation()
}

func (s *Store) mutateExecution(ctx context.Context, id string, revision networkview.Count, operation func(*Execution) (bool, error)) (Execution, error) {
	var record Execution
	err := s.lockExecution(ctx, id, func() error {
		var err error
		record, err = s.execution(id)
		if err != nil {
			return err
		}
		if record.Revision != revision {
			return ErrExecutionConflict
		}
		changed, err := operation(&record)
		if err != nil {
			return err
		}
		return s.writeExecution(&record, changed)
	})
	return record, err
}

// The caller holds the execution stripe across every bounded filesystem step.
func (s *Store) writeExecution(record *Execution, changed bool) error {
	if !changed {
		dir, err := s.root.Open(".")
		if err != nil {
			return err
		}
		defer dir.Close()
		return s.syncDirectory(dir) // resolves a prior ambiguous rename
	}
	if !networkview.Add(&record.Revision, 1) {
		return errors.New("network execution revision exhausted")
	}
	if err := validExecution(*record); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.publish("execution-"+record.ID+".json", data, true)
}

type ExecutionPage struct {
	Executions []ExecutionSummary
	Next       string
	Incomplete bool
	Unreadable int
}

type ExecutionSummary struct {
	ID, Epoch, Project, SessionID, AttemptID string
	StartedAt                                time.Time
	Final, CleanupPending                    bool
}

// Bounded owner inspection is deliberately explicit about incompleteness. A
// caller must not infer that no live execution exists from a truncated page.
func (e *Evidence) Executions(after string) (ExecutionPage, error) {
	if after != "" && !lowerHex(after, 32) {
		return ExecutionPage{}, errors.New("invalid network execution cursor")
	}
	dir, err := e.files.root.Open(".")
	if err != nil {
		return ExecutionPage{}, err
	}
	defer dir.Close()
	var page ExecutionPage
	var ids []string
	scanComplete := false
	for inspected := 0; inspected < 10000; {
		names, err := dir.Readdirnames(100)
		if err != nil && !errors.Is(err, io.EOF) {
			return page, err
		}
		inspected += len(names)
		for _, name := range names {
			if !strings.HasPrefix(name, "execution-") || !strings.HasSuffix(name, ".json") {
				continue
			}
			id := strings.TrimSuffix(strings.TrimPrefix(name, "execution-"), ".json")
			if !lowerHex(id, 32) {
				page.Incomplete, page.Unreadable = true, page.Unreadable+1
				continue
			}
			if id > after {
				ids = append(ids, id)
			}
		}
		if errors.Is(err, io.EOF) {
			scanComplete = true
			break
		}
		if inspected >= 10000 {
			page.Incomplete = true
		}
	}
	slices.Sort(ids)
	if len(ids) > ExecutionPageSize {
		ids, page.Next = ids[:ExecutionPageSize], ids[ExecutionPageSize-1]
	}
	if !scanComplete {
		// A lexical cursor over a partial directory inventory would silently
		// skip unscanned lower IDs. Offer partial evidence, never a false cursor.
		page.Next = ""
	}
	for _, id := range ids {
		record, err := e.Execution(id)
		if err != nil {
			page.Incomplete, page.Unreadable = true, page.Unreadable+1
			continue
		}
		summary := ExecutionSummary{ID: record.ID, Epoch: record.Epoch, Project: record.Project, SessionID: record.SessionID, AttemptID: record.AttemptID,
			StartedAt: record.StartedAt, Final: record.Receipt != nil}
		summary.CleanupPending = inspectedCleanup(record) != "complete"
		page.Executions = append(page.Executions, summary)
	}
	return page, nil
}
