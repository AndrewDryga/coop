package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"golang.org/x/sys/unix"
)

const (
	projectionManifestVersion = 1
	ProjectionManifestFile    = ".coop-execution.json"
	projectionMaxFiles        = 4096
	projectionMaxFileBytes    = 64 << 20
	projectionMaxTotalBytes   = 256 << 20
)

type projectionManifest struct {
	Version       int               `json:"version"`
	Task          TaskInstance      `json:"task"`
	Fork          forkspaceIdentity `json:"fork"`
	AssignmentID  string            `json:"assignment_id"`
	CanonicalRoot string            `json:"canonical_root"`
	CreatedAt     time.Time         `json:"created_at"`
}

// forkspaceIdentity mirrors the two-string wire shape without making projection validation accept
// arbitrary fields through an interface. Conversion stays explicit at the host boundary.
type forkspaceIdentity struct {
	Name       string `json:"name"`
	Generation string `json:"generation"`
}

type ProjectionResult struct {
	Item   Item
	State  string
	Digest string
}

func boundedRegular(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || stat.Nlink != 1 {
		return nil, fmt.Errorf("%q is not a single-link regular file", path)
	}
	if info.Size() < 0 || info.Size() > projectionMaxFileBytes {
		return nil, fmt.Errorf("%q exceeds the %d-byte projection file limit", path, projectionMaxFileBytes)
	}
	return info, nil
}

func openProjectionRoot(dir string) (*os.Root, os.FileInfo, error) {
	before, err := os.Lstat(dir)
	if err != nil {
		return nil, nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("projection root %q is not a real directory", dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		_ = root.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("projection root changed while opening")
	}
	return root, before, nil
}

func projectionRootRegular(root *os.Root, rel string) (os.FileInfo, error) {
	info, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || stat.Nlink != 1 {
		return nil, fmt.Errorf("projection entry %q is not a single-link regular file", rel)
	}
	if info.Size() < 0 || info.Size() > projectionMaxFileBytes {
		return nil, fmt.Errorf("projection entry %q exceeds the %d-byte file limit", rel, projectionMaxFileBytes)
	}
	return info, nil
}

func copyProjectionRootFile(srcRoot, dstRoot *os.Root, srcRel, dstRel string, before os.FileInfo) error {
	in, err := srcRoot.OpenFile(srcRel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	after, err := in.Stat()
	if err != nil || !os.SameFile(before, after) {
		if err != nil {
			return err
		}
		return errors.New("projection source changed while opening")
	}
	out, err := dstRoot.OpenFile(dstRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, before.Mode().Perm()&0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyN(out, in, before.Size()+1)
	switch {
	case copyErr == nil:
		copyErr = fmt.Errorf("projection source %q grew while copying", srcRel)
	case errors.Is(copyErr, io.EOF):
		copyErr = nil
	}
	return errors.Join(copyErr, out.Sync(), out.Close())
}

func copyProjectionFile(src, dst string) error {
	srcRoot, _, err := openProjectionRoot(filepath.Dir(src))
	if err != nil {
		return err
	}
	defer srcRoot.Close()
	name := filepath.Base(src)
	before, err := projectionRootRegular(srcRoot, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	dstRoot, _, err := openProjectionRoot(filepath.Dir(dst))
	if err != nil {
		return err
	}
	defer dstRoot.Close()
	return copyProjectionRootFile(srcRoot, dstRoot, name, filepath.Base(dst), before)
}

func walkProjectionRoot(root *os.Root, visit func(string, os.FileInfo) error) error {
	entriesSeen := 0
	var walk func(string, os.FileInfo) error
	walk = func(rel string, expected os.FileInfo) error {
		dir, err := root.Open(rel)
		if err != nil {
			return err
		}
		opened, statErr := dir.Stat()
		if statErr != nil || !opened.IsDir() || !os.SameFile(expected, opened) {
			_ = dir.Close()
			if statErr != nil {
				return statErr
			}
			return fmt.Errorf("projection directory %q changed while opening", rel)
		}
		entries, readErr := dir.ReadDir(projectionMaxFiles + 1)
		closeErr := dir.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return errors.Join(readErr, closeErr)
		}
		if closeErr != nil {
			return closeErr
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			entriesSeen++
			if entriesSeen > projectionMaxFiles {
				return fmt.Errorf("task projection exceeds its %d-entry limit", projectionMaxFiles)
			}
			child := entry.Name()
			if rel != "." {
				child = filepath.Join(rel, child)
			}
			info, err := root.Lstat(child)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("task projection contains symlink %q", child)
			}
			if err := visit(child, info); err != nil {
				return err
			}
			if info.IsDir() {
				if err := walk(child, info); err != nil {
					return err
				}
			}
		}
		return nil
	}
	rootInfo, err := root.Stat(".")
	if err != nil {
		return err
	}
	return walk(".", rootInfo)
}

func copyProjectionTree(src, dst string) error {
	beforeDigest, err := hashProjectionTree(src)
	if err != nil {
		return err
	}
	srcRoot, _, err := openProjectionRoot(src)
	if err != nil {
		return err
	}
	defer srcRoot.Close()
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	dstRoot, _, err := openProjectionRoot(dst)
	if err != nil {
		return err
	}
	defer dstRoot.Close()
	total := int64(0)
	if err := walkProjectionRoot(srcRoot, func(rel string, info os.FileInfo) error {
		if info.IsDir() {
			return dstRoot.Mkdir(rel, 0o755)
		}
		before, err := projectionRootRegular(srcRoot, rel)
		if err != nil {
			return err
		}
		total += before.Size()
		if total > projectionMaxTotalBytes {
			return errors.New("task projection exceeds its bounded byte limit")
		}
		return copyProjectionRootFile(srcRoot, dstRoot, rel, rel, before)
	}); err != nil {
		return err
	}
	afterDigest, err := hashProjectionTree(src)
	if err != nil {
		return err
	}
	destinationDigest, err := hashProjectionTree(dst)
	if err != nil {
		return err
	}
	if beforeDigest != afterDigest || beforeDigest != destinationDigest {
		return errors.New("task projection changed while the host copied it")
	}
	return nil
}

// walkOpenedProjectionRoot keeps an open handle for every directory while descending. Unlike a
// pathname walk, an agent cannot swap a checked parent for a symlink between lstat and the child
// read: each child is resolved relative to the already-open parent inode.
func walkOpenedProjectionRoot(root *os.Root, visit func(string, *os.Root, string, os.FileInfo) error) error {
	entriesSeen := 0
	var walk func(*os.Root, string) error
	walk = func(current *os.Root, prefix string) error {
		dir, err := current.Open(".")
		if err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(projectionMaxFiles + 1)
		closeErr := dir.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return errors.Join(readErr, closeErr)
		}
		if closeErr != nil {
			return closeErr
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			entriesSeen++
			if entriesSeen > projectionMaxFiles {
				return fmt.Errorf("task projection exceeds its %d-entry limit", projectionMaxFiles)
			}
			name := entry.Name()
			if name == "." || name == ".." || filepath.Base(name) != name {
				return errors.New("task projection contains an invalid path component")
			}
			info, err := current.Lstat(name)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("task projection contains symlink %q", filepath.Join(prefix, name))
			}
			rel := name
			if prefix != "" {
				rel = filepath.Join(prefix, name)
			}
			if err := visit(rel, current, name, info); err != nil {
				return err
			}
			if !info.IsDir() {
				continue
			}
			child, err := current.OpenRoot(name)
			if err != nil {
				return err
			}
			opened, statErr := child.Stat(".")
			if statErr != nil || !opened.IsDir() || !os.SameFile(info, opened) {
				_ = child.Close()
				if statErr != nil {
					return statErr
				}
				return fmt.Errorf("projection directory %q changed while opening", rel)
			}
			walkErr := walk(child, rel)
			closeErr := child.Close()
			if err := errors.Join(walkErr, closeErr); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(root, "")
}

func hashOpenedProjectionTree(root *os.Root) (string, error) {
	hash := sha256.New()
	total := int64(0)
	err := walkOpenedProjectionRoot(root, func(rel string, parent *os.Root, name string, info os.FileInfo) error {
		if info.IsDir() {
			fmt.Fprintf(hash, "d\x00%s\x00", filepath.ToSlash(rel))
			return nil
		}
		before, err := projectionRootRegular(parent, name)
		if err != nil {
			return err
		}
		total += before.Size()
		if total > projectionMaxTotalBytes {
			return errors.New("task projection exceeds its bounded byte limit")
		}
		fmt.Fprintf(hash, "f\x00%s\x00%d\x00", filepath.ToSlash(rel), before.Size())
		f, err := parent.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		after, statErr := f.Stat()
		if statErr != nil || !os.SameFile(before, after) {
			_ = f.Close()
			if statErr != nil {
				return statErr
			}
			return errors.New("task projection changed while hashing")
		}
		_, copyErr := io.CopyN(hash, f, before.Size()+1)
		if copyErr == nil {
			copyErr = errors.New("task projection file grew while hashing")
		} else if errors.Is(copyErr, io.EOF) {
			copyErr = nil
		}
		if err := errors.Join(copyErr, f.Close()); err != nil {
			return err
		}
		hash.Write([]byte{0})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyOpenedProjectionTree(srcRoot, dstRoot *os.Root) error {
	beforeDigest, err := hashOpenedProjectionTree(srcRoot)
	if err != nil {
		return err
	}
	total := int64(0)
	var copyDir func(*os.Root, *os.Root) error
	copyDir = func(src, dst *os.Root) error {
		dir, err := src.Open(".")
		if err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(projectionMaxFiles + 1)
		closeErr := dir.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return errors.Join(readErr, closeErr)
		}
		if closeErr != nil {
			return closeErr
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			name := entry.Name()
			info, err := src.Lstat(name)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("task projection contains symlink %q", name)
			}
			if info.IsDir() {
				if err := dst.Mkdir(name, 0o755); err != nil {
					return err
				}
				srcChild, err := src.OpenRoot(name)
				if err != nil {
					return err
				}
				srcOpened, err := srcChild.Stat(".")
				if err != nil || !os.SameFile(info, srcOpened) {
					_ = srcChild.Close()
					if err != nil {
						return err
					}
					return errors.New("projection source directory changed while opening")
				}
				dstInfo, err := dst.Lstat(name)
				if err != nil {
					_ = srcChild.Close()
					return err
				}
				dstChild, err := dst.OpenRoot(name)
				if err != nil {
					_ = srcChild.Close()
					return err
				}
				dstOpened, err := dstChild.Stat(".")
				if err != nil || !os.SameFile(dstInfo, dstOpened) {
					_ = srcChild.Close()
					_ = dstChild.Close()
					if err != nil {
						return err
					}
					return errors.New("projection destination directory changed while opening")
				}
				err = copyDir(srcChild, dstChild)
				err = errors.Join(err, srcChild.Close(), dstChild.Close())
				if err != nil {
					return err
				}
				continue
			}
			before, err := projectionRootRegular(src, name)
			if err != nil {
				return err
			}
			total += before.Size()
			if total > projectionMaxTotalBytes {
				return errors.New("task projection exceeds its bounded byte limit")
			}
			if err := copyProjectionRootFile(src, dst, name, name, before); err != nil {
				return err
			}
		}
		return nil
	}
	if err := copyDir(srcRoot, dstRoot); err != nil {
		return err
	}
	afterDigest, err := hashOpenedProjectionTree(srcRoot)
	if err != nil {
		return err
	}
	destinationDigest, err := hashOpenedProjectionTree(dstRoot)
	if err != nil {
		return err
	}
	if beforeDigest != afterDigest || beforeDigest != destinationDigest {
		return errors.New("task projection changed while the host copied it")
	}
	return nil
}

func copyPathTreeToOpened(src string, dstRoot *os.Root) error {
	srcRoot, _, err := openProjectionRoot(src)
	if err != nil {
		return err
	}
	defer srcRoot.Close()
	return copyOpenedProjectionTree(srcRoot, dstRoot)
}

func copyOpenedTreeToPath(srcRoot *os.Root, dst string) error {
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	dstRoot, _, err := openProjectionRoot(dst)
	if err != nil {
		return err
	}
	defer dstRoot.Close()
	return copyOpenedProjectionTree(srcRoot, dstRoot)
}

func openAssignedProjection(authorityRepo string, owner ForkTaskOwner) (*os.Root, error) {
	rel, err := forkProjectionRel(authorityRepo, owner)
	if err != nil {
		return nil, err
	}
	workspace, err := forkspace.OpenGenerationWorkspaceRoot(authorityRepo, owner.Fork)
	if err != nil {
		return nil, err
	}
	defer workspace.Close()
	return openRealSubroot(workspace, rel)
}

func projectionManifestForAssignment(assignment ForkAssignment) (projectionManifest, error) {
	canonical, err := canonicalTaskRoot(assignment.Task.Root)
	if err != nil {
		return projectionManifest{}, err
	}
	record, ok, err := ReadTaskOwnerRecord(assignment.Task.Root, assignment.Task.Item.ID)
	if err != nil || !ok || record.Kind != TaskOwnerFork || record.Fork == nil ||
		!sameForkAssignment(*record.Fork, assignment.Owner) {
		return projectionManifest{}, errors.New("task assignment changed before projection materialization")
	}
	return projectionManifest{
		Version: projectionManifestVersion, Task: *record.Task,
		Fork:         forkspaceIdentity{Name: assignment.Owner.Fork.Name, Generation: string(assignment.Owner.Fork.Generation)},
		AssignmentID: assignment.Owner.AssignmentID, CanonicalRoot: canonical, CreatedAt: time.Now().UTC(),
	}, nil
}

func writeProjectionManifestRoot(root *os.Root, assignment ForkAssignment) error {
	manifest, err := projectionManifestForAssignment(assignment)
	if err != nil {
		return err
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return AtomicWriteTaskFile(root, ProjectionManifestFile, append(body, '\n'))
}

// MaterializeForkProjection creates the four-state execution queue and copies only the assigned
// canonical task. Canonical siblings are never exposed. Existing exact projections are retained for
// crash resume and validated before reuse.
func MaterializeForkProjection(assignment ForkAssignment) error {
	if assignment.Outcome != ForkAssignmentSelected || assignment.Lease == nil {
		return errors.New("cannot materialize an unselected fork assignment")
	}
	if existing, err := openAssignedProjection(assignment.AuthorityRepo, assignment.Owner); err == nil {
		_ = existing.Close()
		if _, err := ValidateForkProjection(assignment.AuthorityRepo, assignment.Task.Root, assignment.Task.Item.ID, assignment.Owner); err != nil {
			return err
		}
		if err := ensureForkProposalOutbox(assignment.AuthorityRepo, assignment.Owner); err != nil {
			return err
		}
		if assignment.Owner.Phase == ForkAssignmentPreparing {
			_, err := UpdateForkTaskAssignment(assignment.Task.Root, assignment.Task.Item.ID, assignment.Owner, func(owner *ForkTaskOwner) error {
				owner.Phase = ForkAssignmentWorking
				return nil
			})
			return err
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	rel, err := forkProjectionRel(assignment.AuthorityRepo, assignment.Owner)
	if err != nil {
		return err
	}
	workspace, err := forkspace.OpenGenerationWorkspaceRoot(assignment.AuthorityRepo, assignment.Owner.Fork)
	if err != nil {
		return err
	}
	defer workspace.Close()
	parent, err := ensureRealSubroot(workspace, filepath.Dir(rel), 0o755)
	if err != nil {
		return err
	}
	defer parent.Close()
	tmpName := ""
	var tmpRoot *os.Root
	for attempt := 0; attempt < 16; attempt++ {
		suffix, randomErr := forkspace.NewGeneration()
		if randomErr != nil {
			return randomErr
		}
		tmpName = ".tasks-" + string(suffix)
		if err := parent.Mkdir(tmpName, 0o700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return err
		}
		info, err := parent.Lstat(tmpName)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.Join(err, errors.New("temporary task projection is not a real directory"))
		}
		tmpRoot, err = parent.OpenRoot(tmpName)
		if err != nil {
			return err
		}
		opened, statErr := tmpRoot.Stat(".")
		if statErr != nil || !os.SameFile(info, opened) {
			_ = tmpRoot.Close()
			return errors.Join(statErr, errors.New("temporary task projection changed while opening"))
		}
		break
	}
	if tmpRoot == nil {
		return errors.New("could not allocate a temporary task projection")
	}
	cleanup := true
	defer func() {
		if tmpRoot != nil {
			_ = tmpRoot.Close()
		}
		if cleanup {
			_ = parent.RemoveAll(tmpName)
		}
	}()
	for _, state := range TaskStates {
		if err := tmpRoot.Mkdir(state, 0o755); err != nil {
			return err
		}
	}
	canonicalQueue, _, err := openProjectionRoot(assignment.Task.Root)
	if err != nil {
		return err
	}
	queueInfo, err := projectionRootRegular(canonicalQueue, QueueIdentityFile)
	if err == nil {
		err = copyProjectionRootFile(canonicalQueue, tmpRoot, QueueIdentityFile, QueueIdentityFile, queueInfo)
	}
	err = errors.Join(err, canonicalQueue.Close())
	if err != nil {
		return err
	}
	inProgress, err := openRealSubroot(tmpRoot, StateInProgress)
	if err != nil {
		return err
	}
	if err := inProgress.Mkdir(assignment.Task.Item.ID, 0o755); err != nil {
		_ = inProgress.Close()
		return err
	}
	taskRoot, err := openRealSubroot(inProgress, assignment.Task.Item.ID)
	if err != nil {
		_ = inProgress.Close()
		return err
	}
	err = copyPathTreeToOpened(assignment.Task.Item.Dir, taskRoot)
	err = errors.Join(err, taskRoot.Close(), inProgress.Close())
	if err != nil {
		return err
	}
	if err := writeProjectionManifestRoot(tmpRoot, assignment); err != nil {
		return err
	}
	if err := tmpRoot.Close(); err != nil {
		return err
	}
	tmpRoot = nil
	if err := parent.Rename(tmpName, filepath.Base(rel)); err != nil {
		return err
	}
	cleanup = false
	if err := ensureForkProposalOutbox(assignment.AuthorityRepo, assignment.Owner); err != nil {
		return err
	}
	_, err = UpdateForkTaskAssignment(assignment.Task.Root, assignment.Task.Item.ID, assignment.Owner, func(owner *ForkTaskOwner) error {
		owner.Phase = ForkAssignmentWorking
		return nil
	})
	return err
}

func readProjectionManifest(root *os.Root) (projectionManifest, error) {
	data, err := ReadTaskMetadataFile(root, ProjectionManifestFile)
	if err != nil {
		return projectionManifest{}, err
	}
	var manifest projectionManifest
	if err := decodeIdentityRecord(data, &manifest); err != nil {
		return projectionManifest{}, err
	}
	if manifest.Version != projectionManifestVersion || manifest.Task.Ref.ID == "" ||
		manifest.Fork.Name == "" || manifest.Fork.Generation == "" ||
		!validAssignmentID(manifest.AssignmentID) || !filepath.IsAbs(manifest.CanonicalRoot) || manifest.CreatedAt.IsZero() {
		return projectionManifest{}, errors.New("invalid task projection manifest")
	}
	return manifest, nil
}

func hashProjectionTree(dir string) (string, error) {
	hash := sha256.New()
	root, _, err := openProjectionRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	total := int64(0)
	err = walkProjectionRoot(root, func(rel string, info os.FileInfo) error {
		if info.IsDir() {
			fmt.Fprintf(hash, "d\x00%s\x00", filepath.ToSlash(rel))
			return nil
		}
		before, err := projectionRootRegular(root, rel)
		if err != nil {
			return err
		}
		total += before.Size()
		if total > projectionMaxTotalBytes {
			return errors.New("task projection exceeds its bounded byte limit")
		}
		fmt.Fprintf(hash, "f\x00%s\x00%d\x00", filepath.ToSlash(rel), before.Size())
		f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		after, statErr := f.Stat()
		if statErr != nil || !os.SameFile(before, after) {
			_ = f.Close()
			if statErr != nil {
				return statErr
			}
			return errors.New("task projection changed while hashing")
		}
		_, copyErr := io.CopyN(hash, f, before.Size()+1)
		if copyErr == nil {
			copyErr = errors.New("task projection file grew while hashing")
		} else if errors.Is(copyErr, io.EOF) {
			copyErr = nil
		}
		closeErr := f.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		hash.Write([]byte{0})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readProjectionQueueIdentity(root *os.Root) (queueIdentityRecord, error) {
	data, err := ReadTaskMetadataFile(root, QueueIdentityFile)
	if err != nil {
		return queueIdentityRecord{}, err
	}
	var record queueIdentityRecord
	if err := decodeIdentityRecord(data, &record); err != nil {
		return queueIdentityRecord{}, err
	}
	if err := validateIdentity(record.Version, record.ID, record.CreatedAt); err != nil {
		return queueIdentityRecord{}, err
	}
	return record, nil
}

func readProjectionTaskIdentity(root *os.Root) (taskIdentityRecord, error) {
	data, err := ReadTaskMetadataFile(root, TaskIdentityFile)
	if err != nil {
		return taskIdentityRecord{}, err
	}
	var record taskIdentityRecord
	if err := decodeIdentityRecord(data, &record); err != nil {
		return taskIdentityRecord{}, err
	}
	if err := validateIdentity(record.Version, record.ID, record.CreatedAt); err != nil {
		return taskIdentityRecord{}, err
	}
	return record, nil
}

func openProjectedTask(root *os.Root, state, id string) (*os.Root, error) {
	if !slices.Contains(TaskStates, state) || id == "" || filepath.Base(id) != id || id == "." || id == ".." {
		return nil, errors.New("invalid projected task location")
	}
	stateRoot, err := openRealSubroot(root, state)
	if err != nil {
		return nil, err
	}
	defer stateRoot.Close()
	return openRealSubroot(stateRoot, id)
}

func validateForkProjectionOpened(canonicalRoot, id string, owner ForkTaskOwner, root *os.Root) (ProjectionResult, error) {
	manifest, err := readProjectionManifest(root)
	if err != nil {
		return ProjectionResult{}, err
	}
	canonical, err := canonicalTaskRoot(canonicalRoot)
	if err != nil {
		return ProjectionResult{}, err
	}
	if manifest.CanonicalRoot != canonical || manifest.AssignmentID != owner.AssignmentID ||
		manifest.Fork.Name != owner.Fork.Name || manifest.Fork.Generation != string(owner.Fork.Generation) ||
		manifest.Task.Ref.ID != id {
		return ProjectionResult{}, errors.New("task projection does not match its assignment")
	}
	queue, err := readProjectionQueueIdentity(root)
	if err != nil || queue.ID != manifest.Task.Ref.QueueID {
		return ProjectionResult{}, errors.Join(err, errors.New("task projection queue identity changed"))
	}
	foundState := ""
	for _, state := range TaskStates {
		stateRoot, err := openRealSubroot(root, state)
		if err != nil {
			return ProjectionResult{}, err
		}
		dir, err := stateRoot.Open(".")
		if err != nil {
			_ = stateRoot.Close()
			return ProjectionResult{}, err
		}
		entries, readErr := dir.ReadDir(2)
		closeErr := dir.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			_ = stateRoot.Close()
			return ProjectionResult{}, errors.Join(readErr, closeErr)
		}
		if closeErr != nil {
			_ = stateRoot.Close()
			return ProjectionResult{}, closeErr
		}
		for _, entry := range entries {
			if entry.Name() != id || foundState != "" {
				_ = stateRoot.Close()
				return ProjectionResult{}, errors.New("task projection contains work outside its one assigned task")
			}
			info, err := stateRoot.Lstat(entry.Name())
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				_ = stateRoot.Close()
				return ProjectionResult{}, errors.Join(err, errors.New("projected task is not a real directory"))
			}
			foundState = state
		}
		if err := stateRoot.Close(); err != nil {
			return ProjectionResult{}, err
		}
	}
	if foundState == "" {
		return ProjectionResult{}, errors.New("task projection lost its assigned task")
	}
	taskRoot, err := openProjectedTask(root, foundState, id)
	if err != nil {
		return ProjectionResult{}, err
	}
	defer taskRoot.Close()
	taskIdentity, err := readProjectionTaskIdentity(taskRoot)
	if err != nil || taskIdentity.ID != manifest.Task.Ref.TaskID {
		return ProjectionResult{}, errors.Join(err, errors.New("task projection identity changed"))
	}
	digest, err := hashOpenedProjectionTree(taskRoot)
	if err != nil {
		return ProjectionResult{}, err
	}
	item := Item{ID: id, State: foundState, Dir: filepath.Join(owner.Projection, foundState, id)}
	data, err := ReadTaskMetadataFile(taskRoot, "task.md")
	if err != nil {
		return ProjectionResult{}, fmt.Errorf("read projected task.md: %w", err)
	}
	_, body := SplitFrontmatter(string(data))
	item.Subtasks = scanSubtasks(body)
	return ProjectionResult{Item: item, State: foundState, Digest: digest}, nil
}

func ValidateForkProjection(authorityRepo, canonicalRoot, id string, owner ForkTaskOwner) (ProjectionResult, error) {
	root, err := openAssignedProjection(authorityRepo, owner)
	if err != nil {
		return ProjectionResult{}, err
	}
	defer root.Close()
	return validateForkProjectionOpened(canonicalRoot, id, owner, root)
}

func moveProjectedTask(root *os.Root, fromState, toState, id string) error {
	from, err := openRealSubroot(root, fromState)
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := openRealSubroot(root, toState)
	if err != nil {
		return err
	}
	defer to.Close()
	before, err := from.Lstat(id)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return errors.Join(err, errors.New("projected task source is not a real directory"))
	}
	if _, err := to.Lstat(id); err == nil {
		return errors.New("projected task destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fromDir, err := from.Open(".")
	if err != nil {
		return err
	}
	defer fromDir.Close()
	toDir, err := to.Open(".")
	if err != nil {
		return err
	}
	defer toDir.Close()
	if err := unix.Renameat(int(fromDir.Fd()), id, int(toDir.Fd()), id); err != nil {
		return err
	}
	after, err := to.Lstat(id)
	if err != nil || !os.SameFile(before, after) {
		return errors.Join(err, errors.New("projected task changed during state transition"))
	}
	return nil
}

// PrepareForkProjectionForRun closes the crash window between an execution-local completion and
// host acceptance. A done folder whose canonical assignment is still working was not known to have
// passed final signoff, so it is restored for a fresh range-bound loop attempt. Once acceptance has
// moved the owner to reviewing/ready this function is never called.
func PrepareForkProjectionForRun(authorityRepo, root, id string, expected ForkTaskOwner) error {
	projection, err := openAssignedProjection(authorityRepo, expected)
	if err != nil {
		return err
	}
	defer projection.Close()
	result, err := validateForkProjectionOpened(root, id, expected, projection)
	if err != nil {
		return err
	}
	if result.State == StateBlocked && expected.Phase == ForkAssignmentPaused {
		if err := moveProjectedTask(projection, StateBlocked, StateInProgress, id); err != nil {
			return fmt.Errorf("restore unblocked fork projection: %w", err)
		}
		restored, err := openProjectedTask(projection, StateInProgress, id)
		if err != nil {
			return err
		}
		defer restored.Close()
		return normalizeTaskStateRoot(
			id,
			restored,
			"in progress — canonical decision was unblocked",
			"resume implementation in this same fork generation",
			"blocked projection retained its artifacts and decision context",
			"only exact candidate landing completes the canonical task",
		)
	}
	if result.State != StateDone {
		return nil
	}
	if expected.Phase != ForkAssignmentPreparing && expected.Phase != ForkAssignmentWorking && expected.Phase != ForkAssignmentPaused {
		return fmt.Errorf("done projection is already in assignment phase %s", expected.Phase)
	}
	// The next projection lease clears the old completion receipt under the same global authority
	// flock. Avoid reopening a sandbox-controlled pathname here merely to clear it early.
	if err := moveProjectedTask(projection, StateDone, StateInProgress, id); err != nil {
		return fmt.Errorf("restore interrupted projected completion: %w", err)
	}
	restored, err := openProjectedTask(projection, StateInProgress, id)
	if err != nil {
		return err
	}
	defer restored.Close()
	return normalizeTaskStateRoot(
		id,
		restored,
		"in progress — completion signoff interrupted",
		"re-run the fork loop to validate and sign off this exact candidate",
		"implementation was committed before the host accepted completion",
		"projected done is not canonical completion until exact fork landing",
	)
}

var projectionSyncTopLevel = map[string]bool{
	"task.md": true, "spec.md": true, "log.md": true, "state.md": true, "decision.md": true,
	"screenshots": true, "artifacts": true, "tmp": true,
}

func syncProjectionTree(src, dst string, completed bool) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	sourceNames := make(map[string]bool, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name == TaskIdentityFile {
			continue
		}
		if !projectionSyncTopLevel[name] || completed && name == "tmp" {
			return fmt.Errorf("task projection contains unsupported top-level entry %q", name)
		}
		sourceNames[name] = true
		from, to := filepath.Join(src, name), filepath.Join(dst, name)
		info, err := os.Lstat(from)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("task projection contains symlink %q", from)
		}
		if info.IsDir() {
			if err := syncProjectionDirectory(from, to); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("task projection contains unsupported entry %q", from)
		}
		if target, err := os.Lstat(to); err == nil && target.IsDir() {
			if err := removeCanonicalProjectionEntry(to); err != nil {
				return err
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := replaceProjectionFile(from, to); err != nil {
			return err
		}
	}
	destination, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	for _, entry := range destination {
		name := entry.Name()
		if name == TaskIdentityFile || !projectionSyncTopLevel[name] || sourceNames[name] {
			continue
		}
		if err := removeCanonicalProjectionEntry(filepath.Join(dst, name)); err != nil {
			return err
		}
	}
	return nil
}

// syncProjectionDirectory updates a canonical artifact directory without the create-only copy
// primitive used for initial materialization. Every source entry is revalidated and every file is
// atomically replaced, so resuming a fork may update an artifact it copied on the first attempt.
func syncProjectionDirectory(src, dst string) error {
	srcInfo, err := os.Lstat(src)
	if err != nil || !srcInfo.IsDir() || srcInfo.Mode()&os.ModeSymlink != 0 {
		if err != nil {
			return err
		}
		return fmt.Errorf("task projection directory %q is not a real directory", src)
	}
	if dstInfo, err := os.Lstat(dst); err == nil {
		if !dstInfo.IsDir() || dstInfo.Mode()&os.ModeSymlink != 0 {
			if err := removeCanonicalProjectionEntry(dst); err != nil {
				return err
			}
			if err := os.Mkdir(dst, 0o755); err != nil {
				return err
			}
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(dst, 0o755); err != nil {
			return err
		}
	} else {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	sourceNames := make(map[string]bool, len(entries))
	for _, entry := range entries {
		sourceNames[entry.Name()] = true
		from, to := filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())
		info, err := os.Lstat(from)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("task projection contains symlink %q", from)
		}
		if info.IsDir() {
			if target, err := os.Lstat(to); err == nil && (!target.IsDir() || target.Mode()&os.ModeSymlink != 0) {
				if err := removeCanonicalProjectionEntry(to); err != nil {
					return err
				}
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := syncProjectionDirectory(from, to); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("task projection contains unsupported entry %q", from)
		}
		if target, err := os.Lstat(to); err == nil && target.IsDir() {
			if err := removeCanonicalProjectionEntry(to); err != nil {
				return err
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := replaceProjectionFile(from, to); err != nil {
			return err
		}
	}
	destination, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	for _, entry := range destination {
		if sourceNames[entry.Name()] {
			continue
		}
		if err := removeCanonicalProjectionEntry(filepath.Join(dst, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func removeCanonicalProjectionEntry(path string) error {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	parent := filepath.Dir(path)
	root, err := os.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer root.Close()
	name := filepath.Base(path)
	tmp := fmt.Sprintf(".%s-remove-%d-%d", name, os.Getpid(), time.Now().UnixNano())
	if err := root.Rename(name, tmp); err != nil {
		return err
	}
	moved, err := root.Lstat(tmp)
	if err != nil || !os.SameFile(before, moved) {
		restoreErr := root.Rename(tmp, name)
		if err != nil {
			return errors.Join(err, restoreErr)
		}
		return errors.Join(errors.New("canonical task entry changed before replacement"), restoreErr)
	}
	return os.RemoveAll(filepath.Join(parent, tmp))
}

func replaceProjectionFile(src, dst string) error {
	before, err := boundedRegular(src)
	if err != nil {
		return err
	}
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	after, err := in.Stat()
	if err != nil || !os.SameFile(before, after) {
		if err != nil {
			return err
		}
		return errors.New("projection source changed while opening")
	}
	parent := filepath.Dir(dst)
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		if err != nil {
			return err
		}
		return fmt.Errorf("canonical task parent %q is not a real directory", parent)
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer root.Close()
	tmp := fmt.Sprintf(".%s-%d-%d", filepath.Base(dst), os.Getpid(), time.Now().UnixNano())
	out, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, before.Mode().Perm()&0o755)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	_, copyErr := io.CopyN(out, in, before.Size()+1)
	switch {
	case copyErr == nil:
		copyErr = fmt.Errorf("projection source %q grew while copying", src)
	case errors.Is(copyErr, io.EOF):
		copyErr = nil
	}
	if err := errors.Join(copyErr, out.Sync(), out.Close()); err != nil {
		return err
	}
	return root.Rename(tmp, filepath.Base(dst))
}

// AcceptForkProjection synchronizes bounded task metadata back to the one canonical folder and
// records the execution overlay. Projected done becomes reviewing; only a later exact candidate
// land may move the canonical folder to done.
func AcceptForkProjection(authorityRepo, root, id string, expected ForkTaskOwner) (ProjectionResult, error) {
	if !filepath.IsAbs(authorityRepo) || filepath.Clean(authorityRepo) != authorityRepo {
		return ProjectionResult{}, errors.New("projection acceptance requires a clean absolute authority repo")
	}
	unlockFork, err := forkspace.LockState(authorityRepo, expected.Fork.Name)
	if err != nil {
		return ProjectionResult{}, err
	}
	defer unlockFork()
	if err := forkspace.ValidateGenerationWorkspace(authorityRepo, expected.Fork); err != nil {
		return ProjectionResult{}, err
	}
	return acceptForkProjectionLocked(authorityRepo, root, id, expected)
}

// acceptForkProjectionLocked performs acceptance while the caller holds the fork lifecycle lock.
// AssignForkTask uses it to replay a durable blocking intent before selecting more work.
func acceptForkProjectionLocked(authorityRepo, root, id string, expected ForkTaskOwner) (ProjectionResult, error) {
	projection, err := openAssignedProjection(authorityRepo, expected)
	if err != nil {
		return ProjectionResult{}, err
	}
	defer projection.Close()
	lock, err := lockTaskOwner(root, id)
	if err != nil {
		return ProjectionResult{}, err
	}
	defer lock.Close()
	record, ok, err := lock.Read()
	if err != nil || !ok || record.Kind != TaskOwnerFork || record.Fork == nil ||
		!sameForkAssignment(*record.Fork, expected) {
		return ProjectionResult{}, errors.Join(err, errors.New("task assignment changed before projection acceptance"))
	}
	manifest, err := readProjectionManifest(projection)
	if err != nil || record.Task == nil || manifest.Task != *record.Task {
		return ProjectionResult{}, errors.Join(err, errors.New("task projection manifest does not match canonical assignment identity"))
	}
	result, snapshot, err := snapshotForkProjection(authorityRepo, root, id, expected, projection)
	if err != nil {
		return ProjectionResult{}, err
	}
	defer os.RemoveAll(filepath.Dir(snapshot))
	if record.Fork.Phase == ForkAssignmentBlocking {
		if result.State != StateBlocked {
			return ProjectionResult{}, errors.New("blocking assignment no longer has a blocked projection")
		}
		if record.Fork.ProjectionDigest != result.Digest {
			return ProjectionResult{}, errors.New("blocked projection changed after its durable transition intent")
		}
	}
	canonical, exists, err := CurrentTask(root, id)
	if err != nil {
		return ProjectionResult{}, err
	}
	if !exists {
		return ProjectionResult{}, errors.New("canonical task disappeared before projection acceptance")
	}
	if err := syncProjectionTree(snapshot, canonical.Dir, result.State == StateDone); err != nil {
		return ProjectionResult{}, err
	}
	switch result.State {
	case StateDone:
		record.Fork.Phase = ForkAssignmentReviewing
		record.Fork.ProjectionDigest = result.Digest
		if err := NormalizeTaskState(id, canonical.Dir, "candidate ready for project landing", "review and land with coop fork merge "+record.Fork.Fork.Name, "implementation and fork review complete", "canonical task stays in progress until exact landing"); err != nil {
			return ProjectionResult{}, err
		}
	case StateBlocked:
		if canonical.State != StateInProgress && canonical.State != StateBlocked {
			return ProjectionResult{}, fmt.Errorf("blocked projection has canonical task in unsupported state %s", canonical.State)
		}
		if record.Fork.Phase != ForkAssignmentBlocking {
			record.Fork.Phase = ForkAssignmentBlocking
			record.Fork.ProjectionDigest = result.Digest
			record.Fork.UpdatedAt = time.Now().UTC()
			if err := lock.Write(record); err != nil {
				return ProjectionResult{}, err
			}
		}
		if canonical.State != StateBlocked {
			if err := MoveTaskDir(root, canonical, StateBlocked); err != nil {
				return ProjectionResult{}, err
			}
			canonical.Dir = filepath.Join(root, StateBlocked, id)
		}
		record.Fork.Phase = ForkAssignmentBlocked
		record.Fork.ProjectionDigest = result.Digest
		if err := NormalizeTaskState(id, canonical.Dir, "blocked in fork "+record.Fork.Fork.Name, "resolve decision.md, then resume the same fork generation", "fork synced its blocked state", "assignment is retained"); err != nil {
			return ProjectionResult{}, err
		}
	case StateInProgress:
		record.Fork.Phase = ForkAssignmentPaused
		record.Fork.ProjectionDigest = result.Digest
	default:
		return ProjectionResult{}, fmt.Errorf("assigned projection ended in unsupported state %s", result.State)
	}
	record.Fork.UpdatedAt = time.Now().UTC()
	if err := lock.Write(record); err != nil {
		return ProjectionResult{}, err
	}
	return result, nil
}

// snapshotForkProjection securely copies and verifies the mutable projection once into host-owned
// state. Acceptance hashes and synchronizes these exact bytes, never a second read of the sandbox
// tree after recording its digest.
func snapshotForkProjection(authorityRepo, root, id string, expected ForkTaskOwner, projection *os.Root) (ProjectionResult, string, error) {
	before, err := validateForkProjectionOpened(root, id, expected, projection)
	if err != nil {
		return ProjectionResult{}, "", err
	}
	stateRoot := forkspace.StateDir(authorityRepo)
	if err := forkspace.EnsureStateDir(authorityRepo); err != nil {
		return ProjectionResult{}, "", err
	}
	snapshots := filepath.Join(stateRoot, "projection-snapshots")
	if err := ensureRealDirectory(snapshots, 0o700); err != nil {
		return ProjectionResult{}, "", err
	}
	tmp, err := os.MkdirTemp(snapshots, "."+expected.AssignmentID+"-")
	if err != nil {
		return ProjectionResult{}, "", err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmp)
		}
	}()
	snapshot := filepath.Join(tmp, id)
	taskRoot, err := openProjectedTask(projection, before.State, id)
	if err != nil {
		return ProjectionResult{}, "", err
	}
	copyErr := copyOpenedTreeToPath(taskRoot, snapshot)
	copyErr = errors.Join(copyErr, taskRoot.Close())
	if copyErr != nil {
		return ProjectionResult{}, "", copyErr
	}
	digest, err := hashProjectionTree(snapshot)
	if err != nil {
		return ProjectionResult{}, "", err
	}
	after, err := validateForkProjectionOpened(root, id, expected, projection)
	if err != nil {
		return ProjectionResult{}, "", err
	}
	if before.State != after.State || before.Digest != after.Digest || digest != before.Digest {
		return ProjectionResult{}, "", errors.New("task projection changed while the host captured it")
	}
	// Check the exact captured bytes acceptance will consume, not metadata
	// read separately from the mutable projection's digest. Recovery only
	// validates identity, so an unfinished done projection can still resume.
	if before.State == StateDone {
		if err := requireCurrentCompletedChecklist(snapshot); err != nil {
			return ProjectionResult{}, "", err
		}
	}
	before.Item.Dir = snapshot
	before.Digest = digest
	cleanup = false
	return before, snapshot, nil
}

func ProjectionQueueRel(workspace, projection string) (string, error) {
	rel, err := filepath.Rel(workspace, projection)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("task projection is outside its fork workspace")
	}
	return rel, nil
}
