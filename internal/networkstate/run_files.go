package networkstate

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/processidentity"
)

// RunFiles owns mutable generated copies, not the immutable helper policy file.
// Only individually selected descendants mount; the root also contains host-only
// environment files and must never be exposed wholesale to the workload.
type RunFiles struct {
	Name   string            `json:"name"`
	State  string            `json:"state"`
	Device networkview.Count `json:"device"`
	Inode  networkview.Count `json:"inode"`
}

func validRunFiles(record Execution) error {
	files := record.RunFiles
	if files.Name != "runfiles-"+record.ID || !slices.Contains([]string{"planned", "creating", "preparing", "ready", "cleaning", "gone"}, files.State) ||
		(files.State == "planned" || files.State == "creating") && (files.Device != 0 || files.Inode != 0) ||
		(files.State == "preparing" || files.State == "ready") && files.Inode == 0 || files.Inode == 0 && files.Device != 0 {
		return errors.New("invalid network workload-file custody")
	}
	return nil
}

// PrepareRunFiles binds a directory before any composition writes. An abrupt
// mkdir-before-ready death can leave only an empty directory: no path is handed
// to generators until its identity publication is confirmed durable.
func (s *Store) PrepareRunFiles(ctx context.Context, id string, revision networkview.Count) (Execution, error) {
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
		if err := s.ownedRunFilesExecution(&record); err != nil {
			return err
		}
		if err := s.authorityAvailable(); err != nil {
			return err
		}
		consumerPlanned := !record.SessionWorkloadGone
		if record.Purpose != SessionUnobservedPurpose {
			consumerPlanned = record.Resources[2].State == "planned"
		}
		if record.Snapshot.Terminal || !slices.Contains([]string{"planned", "creating", "preparing"}, record.RunFiles.State) || !consumerPlanned {
			return errors.New("network workload composition generation is closed")
		}
		if record.RunFiles.State == "planned" {
			if _, err := s.root.Lstat(record.RunFiles.Name); !errors.Is(err, os.ErrNotExist) {
				return errors.New("network workload-file destination is occupied or uncertain")
			}
			record.RunFiles.State = "creating"
			if err := s.writeExecution(&record, true); err != nil {
				return err
			}
		}
		if record.RunFiles.State == "creating" {
			if err := s.root.Mkdir(record.RunFiles.Name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
		}
		file, info, err := s.openRunFiles(record.RunFiles)
		if err != nil {
			return err
		}
		defer file.Close()
		changed := record.RunFiles.State == "creating"
		if changed {
			if err := emptyRunFiles(file); err != nil {
				return err
			}
			stat := info.Sys().(*syscall.Stat_t)
			record.RunFiles.Device, record.RunFiles.Inode = networkview.Count(stat.Dev), networkview.Count(stat.Ino)
			record.RunFiles.State = "preparing"
		}
		return s.writeExecution(&record, changed)
	})
	return record, err
}

// RunFilesPath is a host generator capability, not an agent mount source.
func (s *Store) RunFilesPath(id string) (string, error) {
	record, err := s.execution(id)
	if err != nil {
		return "", err
	}
	if err := s.ownedRunFilesExecution(&record); err != nil {
		return "", err
	}
	if record.Snapshot.Terminal || record.RunFiles.State != "preparing" {
		return "", errors.New("network workload files are not open for composition")
	}
	file, _, err := s.openRunFiles(record.RunFiles)
	if err != nil {
		return "", err
	}
	_ = file.Close()
	if err := s.checkRootPath(); err != nil {
		return "", err
	}
	return filepath.Join(s.path, record.RunFiles.Name), nil
}

// FinishRunFiles is called only after synchronous generators have finished and
// the box has validated its complete mount plan. It permanently closes generation.
func (s *Store) FinishRunFiles(ctx context.Context, id string, revision networkview.Count) (Execution, error) {
	return s.mutateExecution(ctx, id, revision, func(record *Execution) (bool, error) {
		if err := s.ownedRunFilesExecution(record); err != nil {
			return false, err
		}
		if err := s.authorityAvailable(); err != nil {
			return false, err
		}
		if record.Snapshot.Terminal || !slices.Contains([]string{"preparing", "ready"}, record.RunFiles.State) {
			return false, errors.New("network workload composition generation is closed")
		}
		file, _, err := s.openRunFiles(record.RunFiles)
		if err != nil {
			return false, err
		}
		_ = file.Close()
		changed := record.RunFiles.State != "ready"
		record.RunFiles.State = "ready"
		return changed, nil
	})
}

// CleanupRunFiles must follow quiescence of host-side generators AND confirmed
// agent absence. Deletion is outside the registry stripe: an agent can create a
// large subtree, which must not stall independent registry updates under a lock.
// Every retry uses the same irreversible cleaning intention and exact inode.
func (e *Evidence) CleanupRunFiles(ctx context.Context, id string, revision networkview.Count) (Execution, error) {
	s := e.files
	record, err := s.mutateExecution(ctx, id, revision, func(record *Execution) (bool, error) {
		consumerGone := record.SessionWorkloadGone
		if record.Purpose != SessionUnobservedPurpose {
			consumerGone = record.Resources[2].State == "gone"
		}
		if !consumerGone {
			return false, errors.New("network workload files still have a possible agent consumer")
		}
		if slices.Contains([]string{"planned", "creating", "preparing"}, record.RunFiles.State) &&
			(record.Supervisor.PID != os.Getpid() || record.Supervisor.StartToken != processidentity.StartToken(os.Getpid())) {
			state := processidentity.Inspect(record.Supervisor.PID, record.Supervisor.StartToken)
			if state != processidentity.Gone && state != processidentity.Mismatch {
				return false, errors.New("network workload generators may still be active")
			}
		}
		if record.RunFiles.State == "gone" || record.RunFiles.State == "cleaning" {
			return false, nil
		}
		if record.RunFiles.State == "planned" {
			if _, err := s.root.Lstat(record.RunFiles.Name); !errors.Is(err, os.ErrNotExist) {
				return false, errors.New("unowned directory occupies planned network workload destination")
			}
			// No mkdir was ever authorized; do not turn absent planned custody
			// into the name-based reconciliation of an uncertain creating intent.
			record.RunFiles.State = "gone"
			return true, nil
		}
		record.RunFiles.State = "cleaning"
		return true, nil
	})
	if err != nil {
		return record, err
	}
	file, info, err := s.openRunFiles(record.RunFiles)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return record, err
	}
	if err == nil {
		defer file.Close()
		if record.RunFiles.State == "gone" {
			return record, errors.New("file appeared after network workload cleanup")
		}
		if record.RunFiles.Inode == 0 {
			// A creating crash predates all writes; never adopt a nonempty tree.
			if err := emptyRunFiles(file); err != nil {
				return record, err
			}
		} else {
			root, err := s.root.OpenRoot(record.RunFiles.Name)
			if err != nil {
				return record, err
			}
			defer root.Close()
			bound, err := root.Stat(".")
			if err != nil || !os.SameFile(info, bound) {
				return record, errors.New("network workload directory identity changed")
			}
			if err := clearRunFiles(ctx, root, file); err != nil {
				return record, err
			}
		}
		current, err := s.root.Lstat(record.RunFiles.Name)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return record, err
		}
		if err == nil {
			if !os.SameFile(info, current) {
				return record, errors.New("network workload directory replaced during cleanup")
			}
			if err := s.root.Remove(record.RunFiles.Name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return record, err
			}
		}
	}
	// Concurrent evidence updates may have advanced the revision while removing
	// children. Reread/CAS the conclusion, never repeat deletion because of a CAS.
	current, err := s.execution(id)
	if err != nil {
		return record, err
	}
	return s.mutateExecution(ctx, id, current.Revision, func(value *Execution) (bool, error) {
		if value.RunFiles.Name != record.RunFiles.Name || value.RunFiles.Device != record.RunFiles.Device || value.RunFiles.Inode != record.RunFiles.Inode ||
			!slices.Contains([]string{"cleaning", "gone"}, value.RunFiles.State) {
			return false, errors.New("network workload cleanup custody changed")
		}
		if _, err := s.root.Lstat(value.RunFiles.Name); !errors.Is(err, os.ErrNotExist) {
			return false, errors.New("network workload directory absence is uncertain")
		}
		changed := value.RunFiles.State != "gone"
		value.RunFiles.State = "gone"
		return changed, nil
	})
}

func (s *Store) ownedRunFilesExecution(record *Execution) error {
	if record.Purpose != SessionUnobservedPurpose {
		return s.ownedExecution(record)
	}
	if len(s.key) != 32 || record.Supervisor.PID != os.Getpid() ||
		record.Supervisor.StartToken != processidentity.StartToken(os.Getpid()) || record.Receipt != nil || record.SessionWorkloadGone {
		return errors.New("ordinary session files are not owned by this live supervisor")
	}
	return nil
}

func clearRunFiles(ctx context.Context, root *os.Root, directory *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		names, err := directory.Readdirnames(100)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := root.RemoveAll(name); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func (s *Store) openRunFiles(files RunFiles) (*os.File, os.FileInfo, error) {
	dir, err := s.root.Open(".")
	if err != nil {
		return nil, nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), files.Name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), files.Name)
	info, err := file.Stat()
	if err == nil {
		err = privateInfo(info, true)
	}
	if err == nil {
		stat := info.Sys().(*syscall.Stat_t)
		if info.Mode().Perm() != 0o700 || files.Inode != 0 && (networkview.Count(stat.Ino) != files.Inode || networkview.Count(stat.Dev) != files.Device) {
			err = errors.New("network workload directory identity changed")
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func emptyRunFiles(file *os.File) error {
	names, err := file.Readdirnames(1)
	if len(names) != 0 || !errors.Is(err, io.EOF) {
		return errors.New("unbound network workload directory is not provably empty")
	}
	return nil
}
