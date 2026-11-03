package workerconnector

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/AndrewDryga/coop/internal/safefile"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// Git already shares immutable object packs between staged sources. This does
// the same for verified checkout/LFS bytes after the new source passed its own
// remote, tree and submodule checks. Each descriptor keeps independent refs,
// config, index and its exact freshness receipt. No execution workspace is a
// candidate: both roots must be private connector-owned source directories.
func reuseStagedSourcePayloads(ctx context.Context, stateRoot, seed, destination string, source workerproto.JobSource) error {
	return reuseStagedSourcePayloadsWithLink(ctx, stateRoot, seed, destination, source, os.Link)
}

func reuseStagedSourcePayloadsWithLink(ctx context.Context, stateRoot, seed, destination string, source workerproto.JobSource, link func(string, string) error) error {
	if seed == "" {
		return nil
	}
	parent := filepath.Join(stateRoot, "job-sources")
	if filepath.Dir(filepath.Dir(seed)) != parent || filepath.Base(seed) != "repository" ||
		filepath.Dir(filepath.Dir(destination)) != parent || filepath.Base(destination) != "repository" {
		return ErrJobSourceIntegrity
	}
	if err := requirePrivateDirectory(filepath.Dir(seed)); err != nil {
		return nil
	}
	if err := requirePrivateDirectory(seed); err != nil {
		return nil
	}
	pinned, err := safefile.OpenRoot(filepath.Dir(seed))
	if err != nil {
		return nil
	}
	data, err := safefile.ReadRegular(pinned, "source.json", 256<<10)
	_ = pinned.Close()
	if err != nil {
		return nil
	}
	var previous workerproto.JobSource
	if json.Unmarshal(data, &previous) != nil || previous.RepositoryIdentity() != source.RepositoryIdentity() ||
		previous.Binding.SelectedCommit != source.Binding.SelectedCommit || previous.Binding.AdmittedTree != source.Binding.AdmittedTree ||
		!reflect.DeepEqual(previous.Submodules, source.Submodules) {
		return nil
	}
	if err = verifyStagedJobSource(filepath.Dir(seed), previous); err != nil {
		return nil
	}
	// A bad seed is not needed by the now-verified new copy. Do not link any of
	// its bytes, even if one ordinary tracked file happens to compare equal.
	if err = verifySourceRepository(ctx, seed, previous); err != nil {
		return nil
	}
	from, err := os.OpenRoot(seed)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer to.Close()
	return filepath.WalkDir(destination, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(destination, path)
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() || !stagedImmutablePayload(rel) {
			return nil
		}
		original, err := from.Lstat(rel)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		current, err := to.Lstat(rel)
		if err != nil {
			return err
		}
		if !original.Mode().IsRegular() || original.Size() != current.Size() || original.Mode().Perm()&0111 != current.Mode().Perm()&0111 || os.SameFile(original, current) {
			return nil
		}
		a, err := stagedPayloadDigest(ctx, from, rel)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		b, err := stagedPayloadDigest(ctx, to, rel)
		if err != nil {
			return err
		}
		if a != b {
			return nil
		}
		// Source trees are host-only and never checked out again. Future fetches
		// append immutable Git/LFS objects and leave their pinned working tree alone.
		if err = from.Chmod(rel, original.Mode().Perm()&^0222); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		temp, err := os.CreateTemp(filepath.Dir(path), ".coop-shared-payload-")
		if err != nil {
			return err
		}
		name := temp.Name()
		if err = temp.Close(); err != nil {
			return err
		}
		defer os.Remove(name)
		if err = os.Remove(name); err != nil {
			return err
		}
		if err = link(filepath.Join(seed, rel), name); err != nil {
			// GC may remove an optional old source after verification/hash.
			// The new independent file still owns the verified content; only
			// a vanished seed is a cache miss, not destination/I/O corruption.
			if errors.Is(err, os.ErrNotExist) {
				_, seedErr := os.Lstat(filepath.Join(seed, rel))
				_, destinationErr := to.Lstat(rel)
				if errors.Is(seedErr, os.ErrNotExist) && destinationErr == nil {
					return nil
				}
			}
			return err
		}
		return os.Rename(name, path)
	})
}

func stagedImmutablePayload(relative string) bool {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for i, part := range parts {
		if part == ".git" {
			return i+2 < len(parts) && parts[i+1] == "lfs" && parts[i+2] == "objects"
		}
	}
	return true
}

func stagedPayloadDigest(ctx context.Context, root *os.Root, path string) ([32]byte, error) {
	var result [32]byte
	file, err := root.Open(path)
	if err != nil {
		return result, err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, 256<<10)
	for {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		n, err := file.Read(buffer)
		if n > 0 {
			_, _ = hash.Write(buffer[:n])
		}
		if err == io.EOF {
			copy(result[:], hash.Sum(nil))
			return result, nil
		}
		if err != nil {
			return result, err
		}
	}
}

// Source staging happens before the session service can admit a workspace.
// Stop it at twice the default five-percent emergency reserve, matching the
// worker's early reclamation watermark. A verified staged hit needs no new
// materialization and is checked before this guard.
func (s *privateJobSourceStager) checkSourceStorage() error {
	measure := s.storageMeasure
	if s.storageForTest != nil {
		measure = s.storageForTest
	}
	if measure == nil {
		return nil
	} // private test stagers supply their own filesystem
	volume, err := measure(s.stateRoot)
	if err != nil {
		return fmt.Errorf("%w: measure storage: %v", errJobSourceStorage, err)
	}
	if volume.CapacityBytes <= 0 {
		return fmt.Errorf("%w: storage capacity is unavailable", errJobSourceStorage)
	}
	reserve := volume.CapacityBytes / 10
	if volume.FreeBytes <= reserve {
		return fmt.Errorf("%w: %d free bytes, need more than %d before materializing a source", errJobSourceStorage, volume.FreeBytes, reserve)
	}
	return nil
}
