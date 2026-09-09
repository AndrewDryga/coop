package networkstate

import (
	"bytes"
	"context"
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

// PrepareLaunchConfig durably records the digest before publishing a single
// helper-readable file. It never replaces an existing file or reopens a cleaned
// generation. On error the caller must reread the record before retrying; no
// runtime create may follow an uncertain intent/file/ready publication.
func (s *Store) PrepareLaunchConfig(ctx context.Context, id string, revision networkview.Count, data []byte) (Execution, error) {
	if len(data) == 0 || len(data) > maxPrivateRecordBytes {
		return Execution{}, errors.New("network launch configuration exceeds byte envelope")
	}
	data = bytes.Clone(data)
	digest := launchDigest(data)
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
		if record.Snapshot.Terminal || record.LaunchConfig.State == "gone" {
			return errors.New("network launch configuration generation is closed")
		}
		artifact := &record.LaunchConfig
		if artifact.State == "planned" {
			if _, err := s.root.Lstat(artifact.Name); !errors.Is(err, os.ErrNotExist) {
				return errors.New("network launch configuration destination is occupied or uncertain")
			}
			artifact.State, artifact.Digest, artifact.Size = "creating", digest, len(data)
			if err := s.writeExecution(&record, true); err != nil {
				return err
			}
		} else if artifact.Digest != digest || artifact.Size != len(data) {
			return errors.New("network launch configuration cannot be rebound")
		}
		_, _, err = s.readLaunchArtifact(*artifact)
		if errors.Is(err, os.ErrNotExist) && artifact.State == "creating" {
			if err = s.publishMode(artifact.Name, data, false, 0o444); err != nil {
				return err
			}
			_, _, err = s.readLaunchArtifact(*artifact)
		}
		if err != nil {
			return err
		}
		changed := artifact.State != "ready"
		artifact.State = "ready"
		return s.writeExecution(&record, changed)
	})
	return record, err
}

// LaunchConfigPath returns only the derived, verified ready file. The enclosing
// authority directory is never a mount source; only this file is bound read-only
// to the trusted helpers, never to the agent. A recovery reader cannot call this.
func (s *Store) LaunchConfigPath(id string) (string, error) {
	record, err := s.execution(id)
	if err != nil {
		return "", err
	}
	if err := s.ownedExecution(&record); err != nil {
		return "", err
	}
	if record.Snapshot.Terminal || record.LaunchConfig.State != "ready" {
		return "", errors.New("network launch configuration is not ready")
	}
	if _, _, err := s.readLaunchArtifact(record.LaunchConfig); err != nil {
		return "", err
	}
	if err := s.checkRootPath(); err != nil {
		return "", err
	}
	return filepath.Join(s.path, record.LaunchConfig.Name), nil
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

// RemoveLaunchConfig is exact, nonrecursive cleanup. Every potential container
// consumer must first have confirmed absence; a creating/starting intention is
// not enough. Key loss and a sealed receipt do not prevent cleanup.
func (e *Evidence) RemoveLaunchConfig(ctx context.Context, id string, revision networkview.Count) (Execution, error) {
	return e.files.mutateExecution(ctx, id, revision, func(record *Execution) (bool, error) {
		if record.Purpose == SessionUnobservedPurpose {
			return false, errors.New("unobserved session execution has no launch artifact")
		}
		for _, resource := range record.Resources {
			if resource.Kind == "container" && resource.State != "gone" {
				return false, errors.New("network launch configuration still has possible consumers")
			}
		}
		artifact := record.LaunchConfig
		_, info, err := e.files.readLaunchArtifact(artifact)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if err == nil {
			if artifact.State == "gone" || artifact.State == "planned" {
				return false, errors.New("unowned file occupies network launch configuration destination")
			}
			current, err := e.files.root.Lstat(artifact.Name)
			if err != nil || !os.SameFile(info, current) {
				return false, errors.New("network launch configuration identity changed")
			}
			if err := e.files.root.Remove(artifact.Name); err != nil {
				return false, err
			}
		}
		record.LaunchConfig.State = "gone"
		return artifact.State != "gone", nil
	})
}

func launchDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// Generic authority reads stay owner-only. This one narrowly typed reader also
// verifies content custody for the world-readable *file* under the private root.
func (s *Store) readLaunchArtifact(artifact LaunchArtifact) ([]byte, os.FileInfo, error) {
	dir, err := s.root.Open(".")
	if err != nil {
		return nil, nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), artifact.Name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(fd), artifact.Name)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 || info.Mode() != 0o444 || info.Size() != int64(artifact.Size) || artifact.Size < 1 || artifact.Size > maxPrivateRecordBytes {
		return nil, nil, errors.New("network launch configuration file identity is invalid")
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(artifact.Size)+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) != artifact.Size || launchDigest(data) != artifact.Digest {
		return nil, nil, errors.New("network launch configuration content changed")
	}
	return data, info, nil
}
