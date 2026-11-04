package sessionsvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/safefile"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

const companionSnapshotRoot = "companion-snapshots"
const companionSnapshotVersion = 1
const heldSnapshotPrefix = ".cache-held-"

var errSnapshotOwnershipChanged = errors.New("snapshot receipt ownership changed")

// Each session keeps its original private directory and mutable Git metadata.
// Only verified immutable payloads are linked from this private cache. This
// preserves frozen mount/network identities and makes unlinking one session
// harmless to its siblings, including across a crash. Primary writable forks
// never use this cache or borrow any of its inodes.
type companionSnapshotReceipt struct {
	Version        int                      `json:"version"`
	Key            string                   `json:"key"`
	Commit         string                   `json:"commit"`
	Directory      sessionWorkspaceIdentity `json:"directory"`
	Identity       sessionWorkspaceIdentity `json:"identity"`
	AllocatedBytes int64                    `json:"allocated_bytes"`
	Complete       bool                     `json:"complete"`
}

func companionSnapshotKey(stateRoot string, binding session.CompanionRepository) (string, error) {
	stateRoot, err := filepath.EvalSymlinks(stateRoot)
	if err != nil {
		return "", err
	}
	repository, err := filepath.EvalSymlinks(binding.Repository)
	if err != nil {
		return "", err
	}
	// For controller sources the key deliberately excludes freshness timestamps,
	// requested branch spelling and the session name. Their frozen receipts remain
	// separate; materialization depends on identity, selected content and submodules.
	identity := any(repository)
	var modules []workerproto.JobSubmodule
	if key, ok := stagedJobSourceKey(repository); ok {
		sourceRoot, err := safefile.OpenRoot(filepath.Dir(repository))
		if err != nil {
			return "", err
		}
		data, err := safefile.ReadRegular(sourceRoot, "source.json", 256<<10)
		_ = sourceRoot.Close()
		if err != nil {
			return "", err
		}
		if len(data) > 256<<10 {
			return "", errors.New("source receipt is too large")
		}
		var source workerproto.JobSource
		if err = json.Unmarshal(data, &source); err != nil {
			return "", err
		}
		expected, err := source.StagingKey()
		if err != nil || expected != key {
			return "", errors.New("source receipt identity changed")
		}
		staged, err := stagedJobRepository(stateRoot, source)
		if err != nil || staged != repository || source.Binding.SelectedCommit != binding.BaseCommit {
			return "", errors.New("companion source is outside its frozen authority")
		}
		identity = source.RepositoryIdentity()
		modules = source.Submodules
	}
	scope, err := companionSnapshotScope(stateRoot)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(struct {
		Version    int
		Scope      string
		Identity   any
		Commit     string
		Submodules []workerproto.JobSubmodule
	}{companionSnapshotVersion, scope, identity, binding.BaseCommit, modules})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func createCachedSessionCompanion(ctx context.Context, stateRoot string, binding session.CompanionRepository) error {
	key, err := companionSnapshotKey(stateRoot, binding)
	if err != nil {
		return err
	}
	return createCachedSessionSnapshot(ctx, stateRoot, binding, key, nil, nil)
}

// The cache owns detached immutable payloads. A primary may add private fork
// metadata to its projection before publication; neither path mutates the cache.
func createCachedSessionSnapshot(ctx context.Context, stateRoot string, binding session.CompanionRepository, key string, history []string, prepare func(string) error) error {
	stateRoot, err := filepath.EvalSymlinks(stateRoot)
	if err != nil {
		return err
	}
	parent := filepath.Join(stateRoot, companionSnapshotRoot)
	if err = ensureNoSymlinkPath(parent); err != nil {
		return err
	}
	if err = ensurePrivateDirectory(parent); err != nil {
		return err
	}
	unlock, err := forkspace.LockStateContext(ctx, stateRoot, "companion-snapshot-"+key)
	if err != nil {
		return err
	}
	defer unlock()
	entry := filepath.Join(parent, key)
	if _, markerErr := os.Lstat(filepath.Join(entry, "deleting")); markerErr == nil {
		if _, err = pruneCompanionSnapshot(ctx, entry, key, false); err != nil {
			if !errors.Is(err, errSnapshotOwnershipChanged) {
				return err
			}
			if err = holdCopiedCompanionSnapshot(ctx, entry, key, binding); err != nil {
				return err
			}
		}
	}
	if _, err = os.Lstat(entry); errors.Is(err, os.ErrNotExist) {
		if err = buildCompanionSnapshot(ctx, entry, key, binding, history...); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	cached := binding
	cached.Workspace = filepath.Join(entry, "tree")
	if err = verifyCompanionSnapshot(ctx, entry, key, cached); err != nil {
		// Copying a Docker volume changes inodes, not frozen source authority. Keep
		// the old cache intact; only newly built bytes may get a new receipt.
		if !errors.Is(err, errSnapshotOwnershipChanged) {
			return err
		}
		if err = holdCopiedCompanionSnapshot(ctx, entry, key, binding); err != nil {
			return err
		}
		if err = buildCompanionSnapshot(ctx, entry, key, binding, history...); err != nil {
			return err
		}
		if err = verifyCompanionSnapshot(ctx, entry, key, cached); err != nil {
			return err
		}
	}
	if err = verifySnapshotHistory(ctx, cached.Workspace, history); err != nil {
		return err
	}
	// Recent use keeps an unreferenced hot snapshot available to a following job.
	now := time.Now()
	if err = os.Chtimes(entry, now, now); err != nil {
		return err
	}
	if err := removeSnapshotProjectionStages(ctx, entry); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(entry, ".projection-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err = projectCompanionSnapshot(ctx, cached.Workspace, stage); err != nil {
		return err
	}
	projected := binding
	projected.Workspace = stage
	if err = verifySessionCompanionContext(ctx, projected); err != nil {
		return err
	}
	if err = verifySessionSubmodules(ctx, stage, binding.BaseCommit); err != nil {
		return err
	}
	if prepare != nil {
		if err = prepare(stage); err != nil {
			return err
		}
	}
	if _, err = os.Lstat(binding.Workspace); !errors.Is(err, os.ErrNotExist) {
		return errors.New("companion destination appeared while projecting snapshot")
	}
	return os.Rename(stage, binding.Workspace)
}

func buildCompanionSnapshot(ctx context.Context, entry, key string, binding session.CompanionRepository, history ...string) error {
	stage, err := os.MkdirTemp(filepath.Dir(entry), ".snapshot-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	info, err := os.Lstat(stage)
	if err != nil {
		return err
	}
	identity, err := sessionWorkspaceIdentityFor(info)
	if err != nil {
		return err
	}
	receipt := companionSnapshotReceipt{Version: companionSnapshotVersion, Key: key, Commit: binding.BaseCommit, Directory: identity}
	if err = writeCompanionSnapshotReceipt(stage, receipt); err != nil {
		return err
	}
	cached := binding
	cached.Workspace = filepath.Join(stage, "tree")
	if err = createSessionCompanionContext(ctx, cached, history...); err != nil {
		return err
	}
	if err = verifySessionSubmodules(ctx, cached.Workspace, binding.BaseCommit); err != nil {
		return err
	}
	// Only this newly constructed independent tree is sealed. No mutable source
	// inode is shared with a session or a cache.
	err = filepath.WalkDir(cached.Workspace, func(path string, e fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Blocks < 0 {
			return errors.New("snapshot allocated bytes unavailable")
		}
		receipt.AllocatedBytes += int64(stat.Blocks) * 512
		if info.Mode().IsRegular() {
			return os.Chmod(path, info.Mode().Perm()&^0222)
		}
		return nil
	})
	if err != nil {
		return err
	}
	info, err = os.Lstat(cached.Workspace)
	if err != nil {
		return err
	}
	receipt.Identity, err = sessionWorkspaceIdentityFor(info)
	if err != nil {
		return err
	}
	// Include private entry/receipt blocks; this is an eviction budget hint,
	// never an assertion of bytes reclaimed after another link is removed.
	for _, path := range []string{stage, filepath.Join(stage, "receipt.json")} {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Blocks < 0 {
			return errors.New("snapshot allocated bytes unavailable")
		}
		receipt.AllocatedBytes += int64(stat.Blocks) * 512
	}
	receipt.Complete = true
	if err = writeCompanionSnapshotReceipt(stage, receipt); err != nil {
		return err
	}
	if err = os.Rename(stage, entry); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(entry))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func writeCompanionSnapshotReceipt(directory string, receipt companionSnapshotReceipt) error {
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err = config.WriteFileAtomic(filepath.Join(directory, "receipt.json"), data); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func verifyCompanionSnapshot(ctx context.Context, entry, key string, binding session.CompanionRepository) error {
	receipt, err := readCompanionSnapshotReceipt(entry)
	if err != nil {
		return err
	}
	if !receipt.Complete || receipt.Key != key || receipt.Commit != binding.BaseCommit {
		return errors.New("snapshot receipt identity changed")
	}
	if _, err = os.Lstat(filepath.Join(entry, "deleting")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("snapshot eviction must finish before reuse")
	}
	info, err := os.Lstat(binding.Workspace)
	if err != nil {
		return err
	}
	identity, err := sessionWorkspaceIdentityFor(info)
	if err != nil || !identity.sameDirectory(receipt.Identity) {
		return errors.New("snapshot directory identity changed")
	}
	if err = verifySessionCompanionContext(ctx, binding); err != nil {
		return err
	}
	return verifySessionSubmodules(ctx, binding.Workspace, binding.BaseCommit)
}

func readCompanionSnapshotReceipt(entry string) (companionSnapshotReceipt, error) {
	var receipt companionSnapshotReceipt
	if err := ensureNoSymlinkPath(entry); err != nil {
		return receipt, err
	}
	info, err := os.Lstat(entry)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return receipt, errors.New("snapshot directory is not private")
	}
	directory, err := sessionWorkspaceIdentityFor(info)
	if err != nil {
		return receipt, err
	}
	path := filepath.Join(entry, "receipt.json")
	info, err = os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return receipt, errors.New("snapshot receipt is missing or untrusted")
	}
	pinned, err := safefile.OpenRoot(entry)
	if err != nil {
		return receipt, err
	}
	defer pinned.Close()
	data, err := safefile.ReadRegular(pinned, "receipt.json", 4096)
	if err != nil {
		return receipt, err
	}
	if json.Unmarshal(data, &receipt) != nil || receipt.Version != companionSnapshotVersion || receipt.AllocatedBytes < 0 || !validSessionDigest(receipt.Key) || !forkspace.ValidObjectID(receipt.Commit) {
		return receipt, errors.New("snapshot receipt ownership changed")
	}
	if !directory.sameDirectory(receipt.Directory) {
		return receipt, errSnapshotOwnershipChanged
	}
	return receipt, nil
}

// The per-key lifecycle lock is held. A stale current receipt may explain a
// copy, but does not authorize deletion or adoption of any of its contents.
func holdCopiedCompanionSnapshot(ctx context.Context, entry, key string, binding session.CompanionRepository) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := os.Lstat(entry)
	if err != nil {
		return err
	}
	receipt, err := readCompanionSnapshotReceipt(entry)
	if !errors.Is(err, errSnapshotOwnershipChanged) || !receipt.Complete || receipt.Key != key || receipt.Commit != binding.BaseCommit {
		return errors.New("copied snapshot cannot be rebuilt from this authority")
	}
	if marker, markerErr := os.Lstat(filepath.Join(entry, "deleting")); markerErr == nil {
		if !marker.Mode().IsRegular() || marker.Size() != 0 {
			return errors.New("invalid snapshot eviction marker")
		}
	} else if !errors.Is(markerErr, os.ErrNotExist) {
		return markerErr
	}
	parent, err := os.OpenRoot(filepath.Dir(entry))
	if err != nil {
		return err
	}
	defer parent.Close()
	current, err := parent.Lstat(filepath.Base(entry))
	if err != nil || !os.SameFile(before, current) {
		return errors.New("copied snapshot changed before preservation")
	}
	held, err := os.MkdirTemp(filepath.Dir(entry), heldSnapshotPrefix+key+"-")
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(held)
		return err
	}
	if err := parent.Rename(filepath.Base(entry), filepath.Join(filepath.Base(held), "snapshot")); err != nil {
		_ = os.Remove(held)
		return err
	}
	if err := syncSnapshotDirectory(held); err != nil {
		return err
	}
	return syncSnapshotDirectory(filepath.Dir(entry))
}

// A .git/index may be refreshed and configuration/refs are private metadata.
// Git objects and LFS payloads, including nested submodules, are immutable.
func sharedCompanionFile(relative string) bool {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for i, p := range parts {
		if p == ".git" {
			return i+1 < len(parts) && (parts[i+1] == "objects" || parts[i+1] == "lfs")
		}
	}
	return true
}

func projectCompanionSnapshot(ctx context.Context, source, destination string) error {
	return filepath.WalkDir(source, func(path string, e fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(destination, rel)
		info, err := e.Info()
		if err != nil {
			return err
		}
		switch {
		case e.IsDir():
			return os.Mkdir(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			if info.Mode().Perm()&0222 != 0 {
				return errors.New("snapshot contains writable payload")
			}
			if sharedCompanionFile(rel) {
				return os.Link(path, target)
			}
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm()|0200)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, in)
			return errors.Join(copyErr, out.Close())
		default:
			return fmt.Errorf("unsupported snapshot entry %q", rel)
		}
	})
}

// Enrollment binds a state root to one controller and workspace. A new identity
// after explicit re-enrollment must not reuse the prior realm's cache. Only its
// public routing fields participate; provider credentials and TLS keys never do.
func companionSnapshotScope(stateRoot string) (string, error) {
	root, err := safefile.OpenRoot(stateRoot)
	if err != nil {
		return "", err
	}
	defer root.Close()
	data, err := safefile.ReadRegular(root, "identity.json", 64<<10)
	var identity struct {
		ControllerURL string `json:"controller_url"`
		WorkerID      string `json:"worker_id"`
		WorkspaceRef  string `json:"workspace_ref"`
	}
	if errors.Is(err, os.ErrNotExist) {
		return "local:" + stateRoot, nil
	}
	if err != nil {
		return "", err
	}
	if json.Unmarshal(data, &identity) != nil || identity.ControllerURL == "" || identity.WorkerID == "" || identity.WorkspaceRef == "" {
		return "", errors.New("worker source namespace is unavailable")
	}
	public, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(public)
	return hex.EncodeToString(sum[:]), nil
}

// Projection stages stay inside the private cache entry until atomic publication.
// They can never be mounted by a session. The snapshot lock excludes creation
// while this finishes a crash-left stage, releasing its otherwise orphaned links.
func removeSnapshotProjectionStages(ctx context.Context, entry string) error {
	root, err := os.OpenRoot(entry)
	if err != nil {
		return err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return err
	}
	for _, child := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if child.IsDir() && strings.HasPrefix(child.Name(), ".projection-") {
			if err = root.RemoveAll(child.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}
