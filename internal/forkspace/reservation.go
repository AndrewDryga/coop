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
	workspaceReservationVersion = 1
	workspaceReservationLimit   = 16 << 10
	workspaceReservationCount   = 4096
)

const WorkspaceReservationVersion = workspaceReservationVersion

// Injectable only inside this package to qualify visible-create/unlink recovery.
var syncReservationDirectory = func(dir *os.File) error { return dir.Sync() }

type WorkspaceReservationKind string

const WorkspaceReservationRemoteSession WorkspaceReservationKind = "remote-session"

// WorkspaceReservation is durable ownership even while no sandbox process is running. It is
// projected into the project control plane so fork lifecycle commands never need the global
// session database (and never race its separate lock).
type WorkspaceReservation struct {
	Version   int                      `json:"version"`
	Fork      Identity                 `json:"fork"`
	Kind      WorkspaceReservationKind `json:"kind"`
	OwnerID   string                   `json:"owner_id"`
	CreatedAt time.Time                `json:"created_at"`
}

func reservationDir(repo string) string { return filepath.Join(StateDir(repo), "reservations") }

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
	if record.Version != workspaceReservationVersion || !ValidExistingName(record.Fork.Name) ||
		!ValidGeneration(record.Fork.Generation) || record.Kind != WorkspaceReservationRemoteSession ||
		!validReservationOwner(record.OwnerID) || record.CreatedAt.IsZero() {
		return errors.New("invalid fork workspace reservation")
	}
	return nil
}

func ensureReservationDir(repo string) error {
	if err := EnsureStateDir(repo); err != nil {
		return err
	}
	return ensurePrivateStateDir(reservationDir(repo))
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
	if err := ValidateGenerationWorkspace(repo, record.Fork); err != nil {
		return err
	}
	if err := ensureReservationDir(repo); err != nil {
		return err
	}
	root, err := os.OpenRoot(reservationDir(repo))
	if err != nil {
		return err
	}
	defer root.Close()
	name := reservationName(record.Fork)
	if current, err := readWorkspaceReservationFile(root, name); err == nil {
		if reflect.DeepEqual(current, record) || current.Fork == record.Fork && current.Kind == record.Kind && current.OwnerID == record.OwnerID {
			// A prior create may have become visible before its directory sync failed.
			// Repeat the durability barrier before acknowledging the idempotent retry.
			return syncWorkspaceReservationRoot(root)
		}
		return errors.New("fork workspace generation is reserved by another owner")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(reservationDir(repo), ".reservation-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
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
	if err := renameNoReplace(tmp, filepath.Join(reservationDir(repo), name)); err != nil {
		return err
	}
	tmp = ""
	return syncWorkspaceReservationRoot(root)
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
		return fmt.Errorf("fork %s is owned by %s %s — discard that session before changing its workspace",
			identity.Name, reservation.Kind, reservation.OwnerID)
	}
	return nil
}

// ConfirmWorkspaceReservationState repeats the directory barrier that makes a visible
// reservation create or removal durable. It is intentionally useful without an identity: after
// an unlink became visible but its sync failed, a retry can no longer discover the removed record
// in order to repeat RemoveWorkspaceReservationIfMatchesLocked. Syncing the registry itself (or
// its state-directory parent when the registry is absent) confirms that visible absence before a
// replacement owner is allowed to publish.
func ConfirmWorkspaceReservationState(repo string) error {
	root, err := os.OpenRoot(reservationDir(repo))
	if errors.Is(err, os.ErrNotExist) {
		root, err = os.OpenRoot(StateDir(repo))
	}
	// No Coop state directory means no reservation registry has ever been published by this
	// checkout. Coop never removes StateDir, so there is no deleted registry whose absence needs
	// a parent barrier in this case.
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	return syncWorkspaceReservationRoot(root)
}

// RequireForkNameAvailable reports any durable cleanup or owner that still claims a fork name.
// The caller must hold LockState(repo,name) across this check and subsequent creation for the
// result to authorize reuse. Generation-specific checks are insufficient here: a crash can leave
// a staged workspace or old-generation reservation after the public generation record is gone.
func RequireForkNameAvailable(repo, name string) error {
	if !ValidExistingName(name) {
		return fmt.Errorf("invalid fork name %q", name)
	}
	if err := ConfirmWorkspaceReservationState(repo); err != nil {
		return fmt.Errorf("confirm fork reservation state: %w", err)
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
			return fmt.Errorf("fork %s is owned by %s %s — discard that session before reusing the name",
				name, record.Kind, record.OwnerID)
		}
	}
	return nil
}

func RemoveWorkspaceReservationIfMatchesLocked(repo string, expected WorkspaceReservation) error {
	root, err := os.OpenRoot(reservationDir(repo))
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
		// A prior removal may have unlinked the record before its directory sync
		// failed. Repeat that barrier before reporting the retry complete.
		return syncWorkspaceReservationRoot(root)
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, expected) && (current.Fork != expected.Fork || current.Kind != expected.Kind || current.OwnerID != expected.OwnerID) {
		return errors.New("workspace reservation changed before removal")
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncWorkspaceReservationRoot(root)
}

func syncWorkspaceReservationRoot(root *os.Root) error {
	dir, err := root.OpenFile(".", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	return errors.Join(syncReservationDirectory(dir), dir.Close())
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
