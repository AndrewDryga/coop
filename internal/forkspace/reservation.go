package forkspace

import (
	"bytes"
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
	"unicode"
)

const (
	workspaceReservationVersion = 2
	workspaceReservationLimit   = 16 << 10
	workspaceReservationCount   = 4096
)

const WorkspaceReservationVersion = workspaceReservationVersion

// Injectable only inside this package to qualify an entry released between the registry's
// directory read and the read of the entry itself.
var afterReservationDirectoryRead = func() {}

type WorkspaceReservationKind string

const WorkspaceReservationRemoteSession WorkspaceReservationKind = "remote-session"

// WorkspaceReservation is durable ownership even while no sandbox process is running. It is
// projected into the project control plane so fork lifecycle commands never need the global
// session database (and never race its separate lock).
type WorkspaceReservation struct {
	Version      int                      `json:"version"`
	Fork         Identity                 `json:"fork"`
	Kind         WorkspaceReservationKind `json:"kind"`
	OwnerStoreID string                   `json:"owner_store_id,omitempty"`
	OwnerID      string                   `json:"owner_id"`
	CreatedAt    time.Time                `json:"created_at"`
}

// MatchesSessionOwner is the only authorization comparison for a session reservation. A v1
// record remains visible to fork guards but cannot gain a store owner by inference.
func (r WorkspaceReservation) MatchesSessionOwner(storeID, sessionID string) bool {
	return r.Version == workspaceReservationVersion && r.Kind == WorkspaceReservationRemoteSession &&
		storeID != "" && r.OwnerStoreID == storeID && r.OwnerID == sessionID
}

func sameWorkspaceReservationOwner(a, b WorkspaceReservation) bool {
	return a.Version == b.Version && a.Fork == b.Fork && a.Kind == b.Kind &&
		a.OwnerStoreID == b.OwnerStoreID && a.OwnerID == b.OwnerID
}

// reservationFolder holds the workspace reservations, in .coop.
const reservationFolder = "reservations"

func reservationDir(repo string) string { return filepath.Join(StateDir(repo), reservationFolder) }

func reservationName(identity Identity) string {
	return identity.Name + "." + string(identity.Generation) + ".json"
}

func validReservationOwner(value string) bool {
	if value == "" || len(value) > 256 || filepath.Base(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func validateWorkspaceReservation(record WorkspaceReservation) error {
	if (record.Version != 1 && record.Version != workspaceReservationVersion) || !ValidExistingName(record.Fork.Name) ||
		!ValidGeneration(record.Fork.Generation) || record.Kind != WorkspaceReservationRemoteSession ||
		!validReservationOwner(record.OwnerID) || record.CreatedAt.IsZero() {
		return errors.New("invalid fork workspace reservation")
	}
	if record.Version == 1 && record.OwnerStoreID != "" ||
		record.Version == workspaceReservationVersion && !validReservationOwner(record.OwnerStoreID) {
		return errors.New("invalid fork workspace reservation owner store")
	}
	return nil
}

func ensureReservationDir(repo string) error {
	root, err := OpenStateRoot(repo, reservationFolder, true)
	if err != nil {
		return err
	}
	return root.Close()
}

func decodeWorkspaceReservation(data []byte) (WorkspaceReservation, error) {
	dec := json.NewDecoder(io.LimitReader(bytes.NewReader(data), workspaceReservationLimit+1))
	dec.DisallowUnknownFields()
	var record WorkspaceReservation
	if err := dec.Decode(&record); err != nil {
		return WorkspaceReservation{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return WorkspaceReservation{}, errors.New("workspace reservation contains multiple JSON values")
		}
		return WorkspaceReservation{}, err
	}
	return record, validateWorkspaceReservation(record)
}

func readWorkspaceReservationFile(root *os.Root, name string) (WorkspaceReservation, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return WorkspaceReservation{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || stat.Nlink != 1 ||
		info.Size() < 0 || info.Size() > workspaceReservationLimit {
		return WorkspaceReservation{}, errors.New("workspace reservation is not a bounded single-link regular file")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return WorkspaceReservation{}, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		if err != nil {
			return WorkspaceReservation{}, err
		}
		return WorkspaceReservation{}, errors.New("workspace reservation changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, workspaceReservationLimit+1))
	if err != nil || len(data) > workspaceReservationLimit {
		if err != nil {
			return WorkspaceReservation{}, err
		}
		return WorkspaceReservation{}, errors.New("workspace reservation exceeds its size limit")
	}
	return decodeWorkspaceReservation(data)
}

// ReserveWorkspaceLocked publishes an exact owner while the caller holds LockState. Replaying the
// same immutable record is idempotent; a different session can never adopt the generation.
func ReserveWorkspaceLocked(repo string, record WorkspaceReservation) error {
	if err := validateWorkspaceReservation(record); err != nil {
		return err
	}
	if record.Version != workspaceReservationVersion {
		return errors.New("cannot publish an old-format workspace reservation")
	}
	if err := ValidateGenerationWorkspace(repo, record.Fork); err != nil {
		return err
	}
	root, err := OpenStateRoot(repo, reservationFolder, true)
	if err != nil {
		return err
	}
	defer root.Close()
	name := reservationName(record.Fork)
	if current, err := readWorkspaceReservationFile(root, name); err == nil {
		if reflect.DeepEqual(current, record) || sameWorkspaceReservationOwner(current, record) {
			return nil
		}
		return errors.New("fork workspace generation is reserved by another owner")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, tmp, err := CreateTempIn(root, ".reservation-")
	if err != nil {
		return err
	}
	defer func() {
		if tmp != "" {
			_ = root.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	writeErr := error(nil)
	if _, err := f.Write(append(body, '\n')); err != nil {
		writeErr = err
	} else {
		writeErr = f.Sync()
	}
	writeErr = errors.Join(writeErr, f.Close())
	if writeErr != nil {
		return writeErr
	}
	if err := PublishNoReplace(root, tmp, name); err != nil {
		return err
	}
	tmp = ""
	return nil
}

func ReadWorkspaceReservation(repo string, identity Identity) (WorkspaceReservation, bool, error) {
	root, err := os.OpenRoot(reservationDir(repo))
	if errors.Is(err, os.ErrNotExist) {
		return WorkspaceReservation{}, false, nil
	}
	if err != nil {
		return WorkspaceReservation{}, false, err
	}
	defer root.Close()
	record, err := readWorkspaceReservationFile(root, reservationName(identity))
	if errors.Is(err, os.ErrNotExist) {
		return WorkspaceReservation{}, false, nil
	}
	if err != nil {
		return WorkspaceReservation{}, false, err
	}
	if record.Fork != identity {
		return WorkspaceReservation{}, false, errors.New("workspace reservation identity mismatch")
	}
	return record, true, nil
}

func RequireNoWorkspaceReservationLocked(repo string, identity Identity) error {
	reservation, ok, err := ReadWorkspaceReservation(repo, identity)
	if err != nil {
		return err
	}
	if ok {
		return reservationConflictError(identity.Name, reservation)
	}
	return nil
}

// RequireForkNameAvailable reports any durable cleanup or owner that still claims a fork name.
// The caller must hold LockState(repo,name) across this check and subsequent creation for the
// result to authorize reuse. Generation-specific checks are insufficient here: a crash can leave
// a staged workspace or old-generation reservation after the public generation record is gone.
func RequireForkNameAvailable(repo, name string) error {
	if !ValidExistingName(name) {
		return fmt.Errorf("invalid fork name %q", name)
	}
	staged, err := StagedDiscards(repo, name)
	if err != nil {
		return err
	}
	if len(staged) != 0 {
		return fmt.Errorf("fork %s is still being discarded at %s — finish that discard before reusing the name", name, staged[0])
	}
	records, problems := WorkspaceReservations(repo)
	if len(problems) != 0 {
		return errors.Join(problems...)
	}
	for _, record := range records {
		if record.Fork.Name == name {
			return reservationConflictError(name, record)
		}
	}
	return nil
}

func reservationConflictError(name string, record WorkspaceReservation) error {
	if record.Version == 1 {
		return fmt.Errorf("fork %s has an old session reservation without a proven store owner — inspect and recover it offline", name)
	}
	return fmt.Errorf("fork %s is owned by %s %s — ask the owning session service to discard it",
		name, record.Kind, record.OwnerID)
}

func RemoveWorkspaceReservationIfMatchesLocked(repo string, expected WorkspaceReservation) error {
	if err := validateWorkspaceReservation(expected); err != nil {
		return err
	}
	if expected.Version != workspaceReservationVersion {
		return errors.New("cannot remove an old-format workspace reservation")
	}
	root, err := OpenStateRoot(repo, reservationFolder, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	name := reservationName(expected.Fork)
	current, err := readWorkspaceReservationFile(root, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, expected) && !sameWorkspaceReservationOwner(current, expected) {
		return errors.New("workspace reservation changed before removal")
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func WorkspaceReservations(repo string) ([]WorkspaceReservation, []error) {
	root, err := os.OpenRoot(reservationDir(repo))
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
	entries, err := dir.ReadDir(workspaceReservationCount + 1)
	closeErr := dir.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, []error{errors.Join(err, closeErr)}
	}
	if closeErr != nil {
		return nil, []error{closeErr}
	}
	if len(entries) > workspaceReservationCount {
		return nil, []error{fmt.Errorf("workspace reservation registry exceeds %d entries", workspaceReservationCount)}
	}
	afterReservationDirectoryRead()
	var records []WorkspaceReservation
	var problems []error
	for _, entry := range entries {
		// An interrupted pre-publication write has no authority. It is deliberately
		// hidden and ignored; a successful caller removes its own temporary file.
		if strings.HasPrefix(entry.Name(), ".reservation-") {
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			problems = append(problems, fmt.Errorf("%s: unsupported workspace reservation entry", entry.Name()))
			continue
		}
		record, err := readWorkspaceReservationFile(root, entry.Name())
		if errors.Is(err, os.ErrNotExist) {
			// Its owner released it after the directory was read. A read a moment later would not
			// list it, and a reservation that no longer exists holds nothing; reporting it failed
			// an unrelated session create with ENOENT.
			continue
		}
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", entry.Name(), err))
			continue
		}
		if entry.Name() != reservationName(record.Fork) {
			problems = append(problems, fmt.Errorf("%s: workspace reservation filename mismatch", entry.Name()))
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Fork.Name != records[j].Fork.Name {
			return records[i].Fork.Name < records[j].Fork.Name
		}
		return records[i].Fork.Generation < records[j].Fork.Generation
	})
	return records, problems
}
