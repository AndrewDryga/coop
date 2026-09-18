package networkstate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/AndrewDryga/coop/internal/networkview"
)

// Artifact is the one exact-owned private directory of an execution. It holds
// the immutable helper-readable launch configuration and, under files/, the
// generated workload sources the box mounts individually. One directory means
// one custody state machine and one subtree for cleanup to reconcile.
//
// The directory name embeds the execution's random ID, so nothing but this
// supervisor's own interrupted attempt can occupy it. The enclosing authority
// root is never a mount source: only the launch file and selected file
// descendants are bound, the launch file to trusted helpers and never the agent.
type Artifact struct {
	Name   string            `json:"name"`
	State  string            `json:"state"`
	Device networkview.Count `json:"device,omitempty"`
	Inode  networkview.Count `json:"inode,omitempty"`
	Digest string            `json:"launch_digest,omitempty"`
	Size   int               `json:"launch_size,omitempty"`
}

const (
	artifactLaunchFile = "launch.json"
	artifactFilesDir   = "files"
)

func artifactDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func validArtifact(record Execution) error {
	artifact := record.Artifact
	bound := artifact.Inode != 0 && lowerHex(artifact.Digest, 64) && artifact.Size >= 1 && artifact.Size <= maxPrivateRecordBytes
	empty := artifact.Inode == 0 && artifact.Device == 0 && artifact.Digest == "" && artifact.Size == 0
	valid := false
	switch artifact.State {
	case "planned":
		valid = empty
	case "prepared":
		valid = bound
	case "gone":
		valid = empty || bound
	}
	if artifact.Name != "artifacts-"+record.ID || !valid {
		return errors.New("invalid network artifact custody")
	}
	return nil
}

// PrepareArtifacts publishes the whole artifact directory in one step: the
// immutable launch configuration first, then the empty generated-files root.
// It never replaces an existing file or reopens a cleaned generation, and it
// never adopts a directory it cannot prove empty. On error the caller must
// reread the record; no runtime create may follow an uncertain publication.
func (s *Store) PrepareArtifacts(ctx context.Context, id string, revision networkview.Count, launch []byte) (Execution, error) {
	if len(launch) == 0 || len(launch) > maxPrivateRecordBytes {
		return Execution{}, errors.New("network launch configuration exceeds byte envelope")
	}
	launch = bytes.Clone(launch)
	digest := artifactDigest(launch)
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
		if err := s.ownedExecution(&record); err != nil {
			return err
		}
		if err := s.authorityAvailable(); err != nil {
			return err
		}
		if record.Snapshot.Terminal || record.Artifact.State == "gone" {
			return errors.New("network artifact generation is closed")
		}
		if record.Artifact.State == "prepared" {
			// A retry after an ambiguous publication reconfirms the same bytes
			// rather than rebinding custody to a second configuration.
			if record.Artifact.Digest != digest || record.Artifact.Size != len(launch) {
				return errors.New("network launch configuration cannot be rebound")
			}
			if _, err := s.readArtifactLaunch(record.Artifact); err != nil {
				return err
			}
			return s.writeExecution(&record, false)
		}
		if err := s.root.Mkdir(record.Artifact.Name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		dir, info, err := s.openArtifact(record.Artifact)
		if err != nil {
			return err
		}
		defer dir.Close()
		// An interrupted attempt may already have published exactly these bytes.
		// Adopt only that; anything else occupying the destination is refused.
		probe := Artifact{Name: record.Artifact.Name, Digest: digest, Size: len(launch)}
		_, err = s.readArtifactLaunch(probe)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err != nil {
			if err := emptyDirectory(dir); err != nil {
				return err
			}
			if err := s.publishArtifactLaunch(dir, launch); err != nil {
				return err
			}
		}
		if err := unix.Mkdirat(int(dir.Fd()), artifactFilesDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		files, _, err := openPrivateDirectory(int(dir.Fd()), artifactFilesDir, 0)
		if err != nil {
			return err
		}
		defer files.Close()
		// Generation begins only after this record is durable, so a planned
		// artifact with generated content is not ours to adopt.
		if err := emptyDirectory(files); err != nil {
			return err
		}
		if err := s.syncDirectory(dir); err != nil {
			return err
		}
		stat := info.Sys().(*syscall.Stat_t)
		record.Artifact.Device, record.Artifact.Inode = networkview.Count(stat.Dev), networkview.Count(stat.Ino)
		record.Artifact.Digest, record.Artifact.Size = digest, len(launch)
		record.Artifact.State = "prepared"
		return s.writeExecution(&record, true)
	})
	return record, err
}

// publishArtifactLaunch writes the file, syncs it, then links it exclusively
// under its final name: a link plus a deferred unlink would leave two durable
// names after an abrupt crash. The caller syncs the directory afterwards.
func (s *Store) publishArtifactLaunch(dir *os.File, data []byte) error {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	tmp := ".publish-" + hex.EncodeToString(random)
	fd, err := unix.Openat(int(dir.Fd()), tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), tmp)
	defer unix.Unlinkat(int(dir.Fd()), tmp, 0)
	_, writeErr := f.Write(data)
	modeErr := f.Chmod(0o444)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, modeErr, syncErr, closeErr); err != nil {
		return err
	}
	return renameExclusive(dir, tmp, artifactLaunchFile)
}

// LaunchConfigPath returns only the verified prepared file. A recovery reader
// holds no authority key and cannot call this.
func (s *Store) LaunchConfigPath(id string) (string, error) {
	record, err := s.preparedArtifact(id)
	if err != nil {
		return "", err
	}
	if _, err := s.readArtifactLaunch(record.Artifact); err != nil {
		return "", err
	}
	return filepath.Join(s.path, record.Artifact.Name, artifactLaunchFile), nil
}

// RunFilesPath is a host generator capability, not an agent mount source: the
// box mounts selected descendants, never this root.
func (s *Store) RunFilesPath(id string) (string, error) {
	record, err := s.preparedArtifact(id)
	if err != nil {
		return "", err
	}
	files, err := s.openArtifactFiles(record.Artifact)
	if err != nil {
		return "", err
	}
	_ = files.Close()
	return filepath.Join(s.path, record.Artifact.Name, artifactFilesDir), nil
}

func (s *Store) preparedArtifact(id string) (Execution, error) {
	record, err := s.execution(id)
	if err != nil {
		return Execution{}, err
	}
	if err := s.ownedExecution(&record); err != nil {
		return Execution{}, err
	}
	if record.Snapshot.Terminal || record.Artifact.State != "prepared" {
		return Execution{}, errors.New("network artifacts are not prepared")
	}
	if err := s.checkRootPath(); err != nil {
		return Execution{}, err
	}
	return record, nil
}

func (s *Store) checkRootPath() error {
	bound, err := s.root.Stat(".")
	if err != nil {
		return err
	}
	current, err := os.Lstat(s.path)
	if err != nil || !os.SameFile(bound, current) || privateInfo(current, true) != nil {
		return errors.New("network authority directory identity changed")
	}
	return nil
}

// CleanupArtifacts is exact, name-bound cleanup of one random-ID subtree. Every
// container consumer must first have confirmed absence; a creating or starting
// intention is not enough. Key loss and a sealed receipt do not prevent it.
// Deletion runs outside the registry stripe: an agent can create a large
// subtree, which must not stall independent registry updates under a lock.
func (e *Evidence) CleanupArtifacts(ctx context.Context, id string, revision networkview.Count) (Execution, error) {
	s := e.files
	record, err := s.mutateExecution(ctx, id, revision, func(record *Execution) (bool, error) {
		for _, resource := range record.Resources {
			if resource.Kind == "container" && resource.State != "gone" {
				return false, errors.New("network artifacts still have possible consumers")
			}
		}
		return false, nil
	})
	if err != nil {
		return record, err
	}
	if record.Artifact.State != "gone" {
		if err := s.removeArtifact(ctx, record.Artifact); err != nil {
			return record, err
		}
	}
	// Concurrent evidence updates may have advanced the revision while children
	// were removed. Reread and CAS the conclusion; never repeat deletion for a
	// failed compare.
	current, err := s.execution(id)
	if err != nil {
		return record, err
	}
	return s.mutateExecution(ctx, id, current.Revision, func(value *Execution) (bool, error) {
		if value.Artifact.Name != record.Artifact.Name || value.Artifact.Inode != record.Artifact.Inode {
			return false, errors.New("network artifact cleanup custody changed")
		}
		if _, err := s.root.Lstat(value.Artifact.Name); !errors.Is(err, os.ErrNotExist) {
			return false, errors.New("network artifact directory absence is uncertain")
		}
		changed := value.Artifact.State != "gone"
		value.Artifact.State = "gone"
		return changed, nil
	})
}

func (s *Store) removeArtifact(ctx context.Context, artifact Artifact) error {
	dir, info, err := s.openArtifact(artifact)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	root, err := s.root.OpenRoot(artifact.Name)
	if err != nil {
		return err
	}
	defer root.Close()
	bound, err := root.Stat(".")
	if err != nil || !os.SameFile(info, bound) {
		return errors.New("network artifact directory identity changed")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		names, err := dir.Readdirnames(100)
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
			break
		}
	}
	current, err := s.root.Lstat(artifact.Name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) {
		return errors.New("network artifact directory replaced during cleanup")
	}
	if err := s.root.Remove(artifact.Name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) openArtifact(artifact Artifact) (*os.File, os.FileInfo, error) {
	dir, err := s.root.Open(".")
	if err != nil {
		return nil, nil, err
	}
	defer dir.Close()
	return openPrivateDirectory(int(dir.Fd()), artifact.Name, artifact.Inode)
}

func (s *Store) openArtifactFiles(artifact Artifact) (*os.File, error) {
	dir, _, err := s.openArtifact(artifact)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	files, _, err := openPrivateDirectory(int(dir.Fd()), artifactFilesDir, 0)
	return files, err
}

// openPrivateDirectory opens one private directory and, when inode is set, proves it is the one the
// execution record named. The device is recorded there too but not compared: recovery after a
// crash runs on the next launch, often after a reboot has renumbered the volume.
func openPrivateDirectory(parent int, name string, inode networkview.Count) (*os.File, os.FileInfo, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err == nil {
		err = privateInfo(info, true)
	}
	if err == nil {
		stat := info.Sys().(*syscall.Stat_t)
		if info.Mode().Perm() != 0o700 || inode != 0 && networkview.Count(stat.Ino) != inode {
			err = errors.New("network artifact directory identity changed")
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func emptyDirectory(file *os.File) error {
	names, err := file.Readdirnames(1)
	if len(names) != 0 || !errors.Is(err, io.EOF) {
		return errors.New("network artifact directory is not provably empty")
	}
	return nil
}

// Generic authority reads stay owner-only. This narrowly typed reader also
// verifies content custody for the world-readable *file* inside the private
// directory: exactly one link, the recorded size and the recorded digest.
func (s *Store) readArtifactLaunch(artifact Artifact) ([]byte, error) {
	dir, _, err := s.openArtifact(artifact)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), artifactLaunchFile, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), artifactLaunchFile)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 || info.Mode() != 0o444 || info.Size() != int64(artifact.Size) ||
		artifact.Size < 1 || artifact.Size > maxPrivateRecordBytes {
		return nil, errors.New("network launch configuration file identity is invalid")
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(artifact.Size)+1))
	if err != nil {
		return nil, err
	}
	if len(data) != artifact.Size || artifactDigest(data) != artifact.Digest {
		return nil, errors.New("network launch configuration content changed")
	}
	return data, nil
}
