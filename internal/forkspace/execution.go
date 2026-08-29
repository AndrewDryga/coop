package forkspace

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	executionRecordVersion = 1
	executionRecordLimit   = 64 << 10
	executionRecordCount   = 4096
	executionRegistryV1    = "v1"
)

// TestExecutionRegistryRootEnv isolates the user-state fallback in tests. Production ignores it.
const TestExecutionRegistryRootEnv = "COOP_TEST_EXECUTION_REGISTRY_ROOT"

type ExecutionKind string
type ExecutionRole string

const (
	ExecutionLocalLoop       ExecutionKind = "local-loop"
	ExecutionForkLoop        ExecutionKind = "fork-loop"
	ExecutionInteractive     ExecutionKind = "interactive"
	ExecutionACP             ExecutionKind = "acp"
	ExecutionForkInteractive ExecutionKind = "fork-interactive"
	ExecutionForkACP         ExecutionKind = "fork-acp"
	ExecutionRemoteSession   ExecutionKind = "remote-session"
	ExecutionReview          ExecutionKind = "review"
	ExecutionGate            ExecutionKind = "gate"
)

const (
	ExecutionRoleSandbox        ExecutionRole = "sandbox"
	ExecutionRoleController     ExecutionRole = "controller"
	ExecutionRoleDetachedWorker ExecutionRole = "detached-worker"
	ExecutionRoleActive         ExecutionRole = "active"
	ExecutionRoleWarm           ExecutionRole = "warm"
	ExecutionRoleProbe          ExecutionRole = "probe"
	ExecutionRoleActiveTurn     ExecutionRole = "active-turn"
)

// ExecutionTaskRef is an optional task binding carried by a supervised sandbox. QueueID and TaskID
// are durable authorities when known; local/legacy work may truthfully carry only the readable ID.
type ExecutionTaskRef struct {
	QueueID    string `json:"queue_id,omitempty"`
	TaskID     string `json:"task_id,omitempty"`
	ID         string `json:"id"`
	Assignment string `json:"assignment_id,omitempty"`
}

type ExecutionSpec struct {
	Kind      ExecutionKind
	Role      ExecutionRole
	Workspace string
	Fork      *Identity
	Task      *ExecutionTaskRef
	SourceID  string
	// ReservationOwner is required only for a remote-session sandbox. Every other fork launch
	// must observe an unreserved workspace, closing the race between session ownership and box
	// publication under the same lifecycle lock.
	ReservationOwner string
}

// ExecutionRecord is host-owned liveness evidence for one box-supervising Coop process. It lives
// outside every agent-writable workspace: normally beside fork lifecycle state, with a durable
// user-state fallback when an ordinary repository's parent is read-only. One project snapshot can
// therefore enumerate local loops, fork loops, interactive boxes, ACP, and remote-session turns.
type ExecutionRecord struct {
	Version   int               `json:"version"`
	ID        string            `json:"id"`
	Kind      ExecutionKind     `json:"kind"`
	Role      ExecutionRole     `json:"role"`
	Workspace string            `json:"workspace"`
	Fork      *Identity         `json:"fork,omitempty"`
	Task      *ExecutionTaskRef `json:"task,omitempty"`
	SourceID  string            `json:"source_id,omitempty"`
	PID       int               `json:"pid"`
	Token     string            `json:"token"`
	StartedAt time.Time         `json:"started_at"`
	registry  string
}

type ExecutionObservation struct {
	Record  ExecutionRecord
	Running bool
	Stale   bool
	Active  bool
}

func executionDir(repo string) string { return filepath.Join(StateDir(repo), "executions") }

// ensureProjectExecutionDir is a test seam for the one compatibility fallback. Fork activity
// normally uses the sibling lifecycle registry; ordinary boxes must also work when that sibling
// parent cannot be created (for example, a read-only mounted repository parent).
var ensureProjectExecutionDir = ensureExecutionDir

func fallbackExecutionDir(repo string) (string, error) {
	canonical := filepath.Clean(repo)
	if resolved, err := filepath.EvalSymlinks(canonical); err == nil {
		canonical = resolved
	} else if absolute, absErr := filepath.Abs(canonical); absErr == nil {
		canonical = absolute
	} else {
		return "", absErr
	}
	root := ""
	if strings.HasSuffix(filepath.Base(os.Args[0]), ".test") {
		root = os.Getenv(TestExecutionRegistryRootEnv)
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".local", "state", "coop", "executions", executionRegistryV1)
	}
	sum := sha256.Sum256([]byte(canonical))
	return filepath.Join(root, fmt.Sprintf("%x", sum)), nil
}

func executionRegistryDirs(repo string) ([]string, error) {
	fallback, err := fallbackExecutionDir(repo)
	if err != nil {
		return []string{executionDir(repo)}, err
	}
	if fallback == executionDir(repo) {
		return []string{fallback}, nil
	}
	return []string{executionDir(repo), fallback}, nil
}

func validExecutionKind(kind ExecutionKind) bool {
	switch kind {
	case ExecutionLocalLoop, ExecutionForkLoop, ExecutionInteractive, ExecutionACP, ExecutionForkInteractive,
		ExecutionForkACP, ExecutionRemoteSession, ExecutionReview, ExecutionGate:
		return true
	default:
		return false
	}
}

func validExecutionRole(role ExecutionRole) bool {
	switch role {
	case ExecutionRoleSandbox, ExecutionRoleController, ExecutionRoleDetachedWorker,
		ExecutionRoleActive, ExecutionRoleWarm, ExecutionRoleProbe, ExecutionRoleActiveTurn:
		return true
	default:
		return false
	}
}

func executionRoleActive(role ExecutionRole) bool {
	return role != ExecutionRoleWarm && role != ExecutionRoleProbe
}

func validExecutionID(id string) bool {
	if len(id) != 32 || id != strings.ToLower(id) {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func validateExecutionRecord(record ExecutionRecord) error {
	if record.Version != executionRecordVersion || !validExecutionID(record.ID) ||
		!validExecutionKind(record.Kind) || !validExecutionRole(record.Role) || !filepath.IsAbs(record.Workspace) ||
		filepath.Clean(record.Workspace) != record.Workspace || record.PID <= 1 ||
		!StableProcToken(record.Token) || record.StartedAt.IsZero() || len(record.SourceID) > 512 {
		return errors.New("invalid project execution record")
	}
	if record.Fork != nil && (!ValidExistingName(record.Fork.Name) || !ValidGeneration(record.Fork.Generation)) {
		return errors.New("invalid fork identity in project execution record")
	}
	if record.Task != nil {
		if record.Task.ID == "" || filepath.Base(record.Task.ID) != record.Task.ID || len(record.Task.ID) > 255 {
			return errors.New("invalid task binding in project execution record")
		}
		if (record.Task.QueueID == "") != (record.Task.TaskID == "") {
			return errors.New("partial durable task binding in project execution record")
		}
		for _, id := range []string{record.Task.QueueID, record.Task.TaskID, record.Task.Assignment} {
			if id != "" && !validExecutionID(id) {
				return errors.New("invalid durable task binding in project execution record")
			}
		}
	}
	return nil
}

func decodeExecutionRecord(data []byte) (ExecutionRecord, error) {
	dec := json.NewDecoder(io.LimitReader(bytes.NewReader(data), executionRecordLimit+1))
	dec.DisallowUnknownFields()
	var record ExecutionRecord
	if err := dec.Decode(&record); err != nil {
		return ExecutionRecord{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return ExecutionRecord{}, errors.New("project execution record contains multiple JSON values")
		}
		return ExecutionRecord{}, err
	}
	if err := validateExecutionRecord(record); err != nil {
		return ExecutionRecord{}, err
	}
	return record, nil
}

func ensureExecutionDir(repo string) error {
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		return err
	}
	if info, err := os.Lstat(StateDir(repo)); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err != nil {
			return err
		}
		return errors.New("fork state root is not a real directory")
	}
	if err := os.Mkdir(executionDir(repo), 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(executionDir(repo))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err != nil {
			return err
		}
		return errors.New("project execution registry is not a real directory")
	}
	return nil
}

func ensureFallbackExecutionDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("fallback project execution registry is not a real directory")
	}
	return os.Chmod(dir, 0o700)
}

func projectExecutionRegistryUnavailable(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS)
}

func newExecutionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// BeginExecution publishes one execution before its sandbox starts. The caller must EndExecution
// with the returned exact record; a crash leaves a stale, visibly non-running observation rather
// than making ongoing project work disappear from the unified view.
func BeginExecution(repo string, spec ExecutionSpec) (ExecutionRecord, error) {
	if spec.Role == "" {
		spec.Role = ExecutionRoleSandbox
	}
	if spec.Fork != nil {
		unlock, err := LockState(repo, spec.Fork.Name)
		if err != nil {
			return ExecutionRecord{}, err
		}
		defer unlock()
		if err := ValidateGenerationWorkspace(repo, *spec.Fork); err != nil {
			return ExecutionRecord{}, err
		}
		reservation, reserved, err := ReadWorkspaceReservation(repo, *spec.Fork)
		if err != nil {
			return ExecutionRecord{}, err
		}
		if spec.Kind == ExecutionRemoteSession {
			if !reserved || reservation.Kind != WorkspaceReservationRemoteSession ||
				reservation.OwnerID != spec.ReservationOwner {
				return ExecutionRecord{}, errors.New("remote-session workspace reservation is absent or belongs to another owner")
			}
		} else if reserved {
			return ExecutionRecord{}, fmt.Errorf("fork workspace is reserved by %s %s", reservation.Kind, reservation.OwnerID)
		} else if spec.ReservationOwner != "" {
			return ExecutionRecord{}, errors.New("ordinary fork activity cannot claim a session reservation owner")
		}
		// One controller owns a fork loop from projection preparation through acceptance. Per-box
		// records remain independent, but a second controller could otherwise restore the first
		// controller's completed projection and execute the same task twice.
		if spec.Kind == ExecutionForkLoop &&
			(spec.Role == ExecutionRoleController || spec.Role == ExecutionRoleDetachedWorker) {
			observations, problems := Executions(repo)
			if len(problems) > 0 {
				return ExecutionRecord{}, errors.Join(append([]error{errors.New("sandbox activity registry is unreadable")}, problems...)...)
			}
			for _, observation := range observations {
				if observation.Record.Fork == nil || *observation.Record.Fork != *spec.Fork ||
					observation.Record.Kind != ExecutionForkLoop ||
					(observation.Record.Role != ExecutionRoleController && observation.Record.Role != ExecutionRoleDetachedWorker) {
					continue
				}
				state := "unverified"
				if observation.Running {
					state = "running"
				} else if observation.Stale {
					state = "cleanup-pending"
				}
				return ExecutionRecord{}, fmt.Errorf("fork %s already has %s loop controller %s", spec.Fork.Name, state, observation.Record.ID)
			}
		}
	}
	registry := executionDir(repo)
	if err := ensureProjectExecutionDir(repo); err != nil {
		if !projectExecutionRegistryUnavailable(err) {
			return ExecutionRecord{}, err
		}
		fallback, fallbackErr := fallbackExecutionDir(repo)
		if fallbackErr != nil {
			return ExecutionRecord{}, errors.Join(err, fallbackErr)
		}
		if fallbackErr = ensureFallbackExecutionDir(fallback); fallbackErr != nil {
			return ExecutionRecord{}, errors.Join(err, fallbackErr)
		}
		registry = fallback
	}
	id, err := newExecutionID()
	if err != nil {
		return ExecutionRecord{}, err
	}
	token := ProcStartToken(os.Getpid())
	record := ExecutionRecord{
		Version: executionRecordVersion, ID: id, Kind: spec.Kind, Role: spec.Role,
		Workspace: filepath.Clean(spec.Workspace), Fork: spec.Fork, Task: spec.Task,
		SourceID: spec.SourceID, PID: os.Getpid(), Token: token, StartedAt: time.Now().UTC(),
		registry: registry,
	}
	if err := validateExecutionRecord(record); err != nil {
		return ExecutionRecord{}, err
	}
	body, err := json.Marshal(record)
	if err != nil {
		return ExecutionRecord{}, err
	}
	root, err := os.OpenRoot(registry)
	if err != nil {
		return ExecutionRecord{}, err
	}
	defer root.Close()
	name := id + ".json"
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return ExecutionRecord{}, err
	}
	writeErr := error(nil)
	if _, err := f.Write(append(body, '\n')); err != nil {
		writeErr = err
	} else {
		writeErr = f.Sync()
	}
	if err := f.Close(); err != nil {
		writeErr = errors.Join(writeErr, err)
	}
	if writeErr != nil {
		_ = root.Remove(name)
		return ExecutionRecord{}, writeErr
	}
	return record, nil
}

func readExecutionFile(root *os.Root, name string) (ExecutionRecord, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return ExecutionRecord{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || stat.Nlink != 1 || info.Size() > executionRecordLimit {
		return ExecutionRecord{}, errors.New("project execution record is not a bounded single-link regular file")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ExecutionRecord{}, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		if err != nil {
			return ExecutionRecord{}, err
		}
		return ExecutionRecord{}, errors.New("project execution record changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, executionRecordLimit+1))
	if err != nil || len(data) > executionRecordLimit {
		if err != nil {
			return ExecutionRecord{}, err
		}
		return ExecutionRecord{}, errors.New("project execution record exceeds its size limit")
	}
	return decodeExecutionRecord(data)
}

// sameExecutionOwner compares the immutable cleanup authority. Role is deliberately mutable: a
// long-lived ACP child transitions active -> warm -> active without changing its supervising
// process, token, source, or execution ID.
func sameExecutionOwner(left, right ExecutionRecord) bool {
	left.Role = right.Role
	left.registry = ""
	right.registry = ""
	return reflect.DeepEqual(left, right)
}

// EndExecution removes only the record this exact process authority published. Replacement or
// corrupt evidence is retained for the snapshot to report; cleanup never guesses ownership from
// a filename. A host-owned role transition does not revoke the original cleanup authority.
func EndExecution(repo string, expected ExecutionRecord) error {
	if !validExecutionID(expected.ID) {
		return errors.New("invalid project execution identity")
	}
	registries := []string{expected.registry}
	if expected.registry == "" {
		var err error
		registries, err = executionRegistryDirs(repo)
		if err != nil {
			return err
		}
	}
	for _, registry := range registries {
		root, err := os.OpenRoot(registry)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		current, readErr := readExecutionFile(root, expected.ID+".json")
		if errors.Is(readErr, os.ErrNotExist) {
			_ = root.Close()
			continue
		}
		if readErr != nil {
			_ = root.Close()
			return readErr
		}
		if !sameExecutionOwner(current, expected) {
			_ = root.Close()
			return errors.New("project execution record changed before cleanup")
		}
		removeErr := root.Remove(expected.ID + ".json")
		closeErr := root.Close()
		return errors.Join(removeErr, closeErr)
	}
	return nil
}

// Executions returns valid records plus explicit per-file problems. A corrupt record never hides
// healthy siblings and never becomes an authorization input.
func Executions(repo string) ([]ExecutionObservation, []error) {
	registries, registryErr := executionRegistryDirs(repo)
	var observations []ExecutionObservation
	var problems []error
	if registryErr != nil {
		problems = append(problems, registryErr)
	}
	seen := map[string]string{}
	for _, registry := range registries {
		found, foundProblems := executionsInRegistry(registry)
		problems = append(problems, foundProblems...)
		for _, observation := range found {
			if previous, exists := seen[observation.Record.ID]; exists {
				problems = append(problems, fmt.Errorf("%s: duplicate project execution identity also present in %s", registry, previous))
				continue
			}
			seen[observation.Record.ID] = registry
			observations = append(observations, observation)
		}
	}
	if len(observations) > executionRecordCount {
		problems = append(problems, fmt.Errorf("project execution registries exceed %d total records", executionRecordCount))
	}
	sort.Slice(observations, func(i, j int) bool {
		if observations[i].Record.StartedAt.Equal(observations[j].Record.StartedAt) {
			return observations[i].Record.ID < observations[j].Record.ID
		}
		return observations[i].Record.StartedAt.Before(observations[j].Record.StartedAt)
	})
	return observations, problems
}

func executionsInRegistry(registry string) ([]ExecutionObservation, []error) {
	root, err := os.OpenRoot(registry)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	defer root.Close()
	dir, err := root.OpenFile(".", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, []error{err}
	}
	entries, err := dir.ReadDir(executionRecordCount + 1)
	closeErr := dir.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, []error{errors.Join(err, closeErr)}
	}
	if closeErr != nil {
		return nil, []error{closeErr}
	}
	if len(entries) > executionRecordCount {
		return nil, []error{fmt.Errorf("project execution registry exceeds %d entries", executionRecordCount)}
	}
	var observations []ExecutionObservation
	var problems []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			problems = append(problems, fmt.Errorf("%s: unsupported project execution registry entry", entry.Name()))
			continue
		}
		record, err := readExecutionFile(root, entry.Name())
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", entry.Name(), err))
			continue
		}
		record.registry = registry
		identity := ProcessIdentityOf(record.PID, record.Token)
		observations = append(observations, ExecutionObservation{
			Record: record, Running: identity == ProcessIdentityMatch,
			Stale: OwnerProvablyDead(identity), Active: identity == ProcessIdentityMatch && executionRoleActive(record.Role),
		})
	}
	return observations, problems
}

// RequireNoForkExecutionsLocked is the mutation-time fence for an exact generation. The caller
// holds LockState(repo, identity.Name), the same lock BeginExecution takes before publication.
// Corrupt registry evidence blocks every fork mutation because it cannot safely prove placement.
func RequireNoForkExecutionsLocked(repo string, identity Identity) error {
	observations, problems := Executions(repo)
	if len(problems) > 0 {
		return errors.Join(append([]error{errors.New("sandbox activity registry is unreadable")}, problems...)...)
	}
	for _, observation := range observations {
		if observation.Record.Fork == nil || *observation.Record.Fork != identity {
			continue
		}
		state := "unverified"
		if observation.Running {
			state = "running"
		} else if observation.Stale {
			state = "cleanup-pending"
		}
		return fmt.Errorf("fork %s has %s %s activity %s — stop or clean that exact sandbox before changing its workspace",
			identity.Name, state, observation.Record.Role, observation.Record.ID)
	}
	return nil
}

// RemoveDeadExecutionsBySource removes only exact records whose supervising process is provably
// dead. Callers invoke it after successful runtime cleanup; unknown or live evidence is retained.
func RemoveDeadExecutionsBySource(repo, sourceID string) error {
	if sourceID == "" {
		return nil
	}
	observations, problems := Executions(repo)
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	var errs []error
	for _, observation := range observations {
		if observation.Record.SourceID == sourceID && observation.Stale {
			errs = append(errs, EndExecution(repo, observation.Record))
		}
	}
	return errors.Join(errs...)
}

// RemoveDeadForkExecutionsLocked clears only provably-dead records for one exact generation and
// role. It is used by fork stop after both process and runtime cleanup have succeeded.
func RemoveDeadForkExecutionsLocked(repo string, identity Identity, role ExecutionRole) error {
	observations, problems := Executions(repo)
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	var errs []error
	for _, observation := range observations {
		if observation.Record.Fork != nil && *observation.Record.Fork == identity &&
			(role == "" || observation.Record.Role == role) && observation.Stale {
			errs = append(errs, EndExecution(repo, observation.Record))
		}
	}
	return errors.Join(errs...)
}

// UpdateExecutionRoleBySource changes only live exact records for a logical child. It lets warm
// ACP/session sandboxes remain visible without keeping the task watcher active while parked.
func UpdateExecutionRoleBySource(repo, sourceID string, role ExecutionRole) error {
	if sourceID == "" || !validExecutionRole(role) {
		return errors.New("invalid execution role update")
	}
	observations, problems := Executions(repo)
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	var matched int
	for _, observation := range observations {
		if observation.Record.SourceID != sourceID || !observation.Running {
			continue
		}
		matched++
		if err := replaceExecutionRole(repo, observation.Record, role); err != nil {
			return err
		}
	}
	if matched == 0 {
		return errors.New("live execution record is not yet available")
	}
	return nil
}

func UpdateExecutionRoleByPID(repo string, pid int, role ExecutionRole) error {
	if pid <= 1 || !validExecutionRole(role) {
		return errors.New("invalid execution process role update")
	}
	observations, problems := Executions(repo)
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	for _, observation := range observations {
		if observation.Record.PID == pid && observation.Running {
			return replaceExecutionRole(repo, observation.Record, role)
		}
	}
	return errors.New("live execution process record is not yet available")
}

func replaceExecutionRole(repo string, expected ExecutionRecord, role ExecutionRole) error {
	registry := expected.registry
	if registry == "" {
		registries, err := executionRegistryDirs(repo)
		if err != nil {
			return err
		}
		for _, candidate := range registries {
			if _, err := os.Lstat(filepath.Join(candidate, expected.ID+".json")); err == nil {
				registry = candidate
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if registry == "" {
			return os.ErrNotExist
		}
	}
	root, err := os.OpenRoot(registry)
	if err != nil {
		return err
	}
	defer root.Close()
	name := expected.ID + ".json"
	current, err := readExecutionFile(root, name)
	if err != nil {
		return err
	}
	if !sameExecutionOwner(current, expected) {
		return errors.New("project execution record changed before role update")
	}
	next := current
	next.Role = role
	body, err := json.Marshal(next)
	if err != nil {
		return err
	}
	tmp := "." + expected.ID + ".role-" + fmt.Sprint(os.Getpid(), "-", time.Now().UnixNano())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if _, err := f.Write(append(body, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}
