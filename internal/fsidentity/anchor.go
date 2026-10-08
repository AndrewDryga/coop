// Package fsidentity binds a writable filesystem root to host-private state.
//
// A path, inode number, device number, or timestamp is not a durable identity:
// supported overlay filesystems can reuse all of them immediately. An Anchor
// instead keeps a second hard link in private state. While that link exists the
// filesystem cannot recycle the inode, and a copied marker is a different file.
package fsidentity

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const maxAnchorBody = 512

var (
	linkNames                   = linkRootNames
	syncPrivateAnchorRoot       = syncRoot
	syncCreatedPublicRoot       = syncRoot
	syncCreateCleanupPublicRoot = syncRoot
	syncRetiredPublicRoot       = syncRoot
)

// Binding names one marker inside a protected root and its second name inside
// an already-open private state root. Body identifies the intended binding for
// diagnostics and recovery; the live hard link, not these bytes, is authority.
type Binding struct {
	RootPath   string
	MarkerName string
	AnchorRoot *os.Root
	AnchorName string
	Body       []byte
}

func (b Binding) check() error {
	if !filepath.IsAbs(b.RootPath) || filepath.Base(b.MarkerName) != b.MarkerName ||
		!filepath.IsLocal(b.MarkerName) || filepath.Base(b.AnchorName) != b.AnchorName ||
		!filepath.IsLocal(b.AnchorName) || b.AnchorRoot == nil || len(b.Body) == 0 ||
		len(b.Body) > maxAnchorBody {
		return errors.New("invalid filesystem identity binding")
	}
	return nil
}

// Create establishes a new binding and returns the pinned root. Both names
// must be absent. The caller publishes its authority record only after Create
// succeeds; an error removes any names this call created.
func Create(binding Binding) (*os.Root, error) {
	if err := binding.check(); err != nil {
		return nil, err
	}
	root, rootInfo, err := openPinnedRoot(binding.RootPath)
	if err != nil {
		return nil, err
	}
	keepRoot := false
	defer func() {
		if !keepRoot {
			_ = root.Close()
		}
	}()
	stateInfo, err := pinnedRootInfo(binding.AnchorRoot)
	if err != nil {
		return nil, err
	}
	if _, err := root.Lstat(binding.MarkerName); err == nil {
		return nil, errors.New("filesystem identity marker already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if _, err := binding.AnchorRoot.Lstat(binding.AnchorName); err == nil {
		return nil, errors.New("filesystem identity anchor already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	anchor, err := binding.AnchorRoot.OpenFile(binding.AnchorName,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create private filesystem identity anchor: %w", err)
	}
	createdAnchor := true
	createdMarker := false
	cleanup := func(cause error) error {
		// Never remove the last private recovery name until absence of the public name is
		// durable. Otherwise a crash during error cleanup can resurrect an authoritative-looking
		// marker whose anchor was already lost, wedging every future Open/Create attempt.
		if createdMarker {
			removeErr := root.Remove(binding.MarkerName)
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return errors.Join(cause, removeErr)
			}
			if syncErr := syncCreateCleanupPublicRoot(root); syncErr != nil {
				return errors.Join(cause, syncErr)
			}
		}
		if createdAnchor {
			removeErr := binding.AnchorRoot.Remove(binding.AnchorName)
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return errors.Join(cause, removeErr)
			}
			return errors.Join(cause, syncRoot(binding.AnchorRoot))
		}
		return cause
	}
	if _, err := anchor.Write(binding.Body); err != nil {
		_ = anchor.Close()
		return nil, cleanup(err)
	}
	if err := anchor.Sync(); err != nil {
		_ = anchor.Close()
		return nil, cleanup(err)
	}
	if err := anchor.Close(); err != nil {
		return nil, cleanup(err)
	}
	// Make the private name durable before publishing its public hard link. If the
	// machine stops after the marker directory is synced, recovery must never see a
	// durable marker whose only private recovery name was still volatile.
	if err := syncPrivateAnchorRoot(binding.AnchorRoot); err != nil {
		return nil, cleanup(fmt.Errorf("sync private filesystem identity anchor: %w", err))
	}

	if err := linkNames(binding.AnchorRoot, binding.AnchorName, root, binding.MarkerName); err != nil {
		return nil, cleanup(fmt.Errorf("link filesystem identity marker: %w (the project and Coop state must be on one filesystem with hard-link support)", err))
	}
	createdMarker = true
	if err := syncCreatedPublicRoot(root); err != nil {
		return nil, cleanup(fmt.Errorf("sync filesystem identity marker: %w", err))
	}
	if err := syncRoot(binding.AnchorRoot); err != nil {
		return nil, cleanup(fmt.Errorf("sync private filesystem identity anchor: %w", err))
	}
	if _, err := validateOpen(root, rootInfo, stateInfo, binding); err != nil {
		return nil, cleanup(err)
	}
	createdAnchor, createdMarker = false, false
	keepRoot = true
	return root, nil
}

// Open validates an existing binding and returns the protected root pinned to
// the exact directory that carried the marker. It never creates or repairs a
// missing name.
func Open(binding Binding) (*os.Root, error) {
	root, _, err := openBinding(binding)
	return root, err
}

func openBinding(binding Binding) (*os.Root, os.FileInfo, error) {
	if err := binding.check(); err != nil {
		return nil, nil, err
	}
	root, rootInfo, err := openPinnedRoot(binding.RootPath)
	if err != nil {
		return nil, nil, err
	}
	stateInfo, err := pinnedRootInfo(binding.AnchorRoot)
	if err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	markerInfo, err := validateOpen(root, rootInfo, stateInfo, binding)
	if err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	return root, markerInfo, nil
}

// MarkerInfo returns the private anchor's live file identity after validating
// the pair. A later public-name stat alone could observe a swapped-in file;
// callers granting a shared-inode exception must compare against this identity.
func MarkerInfo(binding Binding) (os.FileInfo, error) {
	root, info, err := openBinding(binding)
	if err != nil {
		return nil, err
	}
	return info, root.Close()
}

// ReadMarker reads one prospective marker without following links. Callers use
// it only to discover the random binding name, then call Open to establish
// authority against the private anchor.
func ReadMarker(rootPath, markerName string) ([]byte, error) {
	if !filepath.IsAbs(rootPath) || filepath.Base(markerName) != markerName || !filepath.IsLocal(markerName) {
		return nil, errors.New("invalid filesystem identity marker")
	}
	root, rootInfo, err := openPinnedRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	marker, markerInfo, err := openRegular(root, markerName)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(marker, maxAnchorBody+1))
	closeErr := marker.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if len(body) > maxAnchorBody {
		return nil, fmt.Errorf("filesystem identity marker exceeds %d bytes", maxAnchorBody)
	}
	if err := sameOpenName(root, markerName, markerInfo); err != nil {
		return nil, err
	}
	if err := samePinnedRoot(rootPath, rootInfo); err != nil {
		return nil, err
	}
	return body, nil
}

// Retire removes host-private anchor state after the caller has already
// retired the authority record. It also removes the marker when the protected
// root still carries the exact binding. A missing or replaced root is expected
// during workspace teardown; in that case the private name is removed only
// after its contents and ownership prove it is Coop's anchor.
func Retire(binding Binding) error {
	if err := binding.check(); err != nil {
		return err
	}
	if root, err := Open(binding); err == nil {
		removeErr := root.Remove(binding.MarkerName)
		syncErr := syncRetiredPublicRoot(root)
		closeErr := root.Close()
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(removeErr, syncErr, closeErr)
		}
		if err := errors.Join(syncErr, closeErr); err != nil {
			return err
		}
	} else if root, _, pinErr := openPinnedRoot(binding.RootPath); pinErr == nil {
		// A previous retirement may have unlinked the marker and then failed its
		// directory sync. Open can no longer validate that one-link private anchor,
		// so repeat the public durability barrier before deleting its last name.
		// A marker that is still present is not this recovery prefix: retain the
		// private name rather than making an unexplained public marker permanent.
		_, markerErr := root.Lstat(binding.MarkerName)
		if markerErr == nil {
			closeErr := root.Close()
			return errors.Join(err, closeErr, errors.New("filesystem identity marker remains after interrupted retirement"))
		}
		if !errors.Is(markerErr, os.ErrNotExist) {
			return errors.Join(markerErr, root.Close())
		}
		if syncErr := errors.Join(syncRetiredPublicRoot(root), root.Close()); syncErr != nil {
			return syncErr
		}
	}
	anchor, info, err := openRegular(binding.AnchorRoot, binding.AnchorName)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	body, readErr := io.ReadAll(io.LimitReader(anchor, maxAnchorBody+1))
	closeErr := anchor.Close()
	_, owner, ok := fileAuthority(info)
	if readErr != nil || closeErr != nil || !ok || owner != uint32(os.Getuid()) ||
		info.Mode().Perm() != 0o600 || !bytes.Equal(body, binding.Body) {
		return errors.Join(readErr, closeErr, errors.New("private filesystem identity anchor changed before retirement"))
	}
	if err := sameOpenName(binding.AnchorRoot, binding.AnchorName, info); err != nil {
		return err
	}
	if err := binding.AnchorRoot.Remove(binding.AnchorName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncRoot(binding.AnchorRoot)
}

func validateOpen(root *os.Root, rootInfo, stateInfo os.FileInfo, binding Binding) (os.FileInfo, error) {
	marker, markerInfo, err := openRegular(root, binding.MarkerName)
	if err != nil {
		return nil, fmt.Errorf("open filesystem identity marker: %w", err)
	}
	defer marker.Close()
	anchor, anchorInfo, err := openRegular(binding.AnchorRoot, binding.AnchorName)
	if err != nil {
		return nil, fmt.Errorf("open private filesystem identity anchor: %w", err)
	}
	defer anchor.Close()
	markerLinks, markerOwner, ok := fileAuthority(markerInfo)
	anchorLinks, anchorOwner, anchorOK := fileAuthority(anchorInfo)
	if !ok || !anchorOK || markerLinks != 2 || anchorLinks != 2 ||
		markerOwner != uint32(os.Getuid()) || anchorOwner != uint32(os.Getuid()) ||
		markerInfo.Mode().Perm() != 0o600 || anchorInfo.Mode().Perm() != 0o600 ||
		!os.SameFile(markerInfo, anchorInfo) {
		return nil, errors.New("filesystem identity marker no longer matches its private two-link anchor")
	}
	body, err := io.ReadAll(io.LimitReader(marker, maxAnchorBody+1))
	if err != nil || !bytes.Equal(body, binding.Body) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("filesystem identity marker has unexpected contents")
	}
	if err := sameOpenName(root, binding.MarkerName, markerInfo); err != nil {
		return nil, err
	}
	if err := sameOpenName(binding.AnchorRoot, binding.AnchorName, anchorInfo); err != nil {
		return nil, err
	}
	if err := samePinnedRoot(binding.RootPath, rootInfo); err != nil {
		return nil, err
	}
	if err := samePinnedRoot(binding.AnchorRoot.Name(), stateInfo); err != nil {
		return nil, errors.New("private filesystem identity directory changed while validating")
	}
	return anchorInfo, nil
}

func openPinnedRoot(path string) (*os.Root, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("filesystem identity root is not a real directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := root.Lstat(".")
	if err != nil || !os.SameFile(before, opened) {
		_ = root.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("filesystem identity root changed while opening")
	}
	return root, opened, nil
}

func pinnedRootInfo(root *os.Root) (os.FileInfo, error) {
	info, err := root.Lstat(".")
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("private filesystem identity state is not a real directory")
	}
	if err := samePinnedRoot(root.Name(), info); err != nil {
		return nil, errors.New("private filesystem identity directory changed while opening")
	}
	return info, nil
}

func openRegular(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 ||
		before.Size() < 0 || before.Size() > maxAnchorBody {
		return nil, nil, errors.New("identity name is not a bounded regular file")
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		_ = file.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("identity name changed while opening")
	}
	return file, opened, nil
}

func sameOpenName(root *os.Root, name string, opened os.FileInfo) error {
	current, err := root.Lstat(name)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(current, opened) {
		return errors.New("filesystem identity name changed while validating")
	}
	return nil
}

func samePinnedRoot(path string, opened os.FileInfo) error {
	current, err := os.Lstat(path)
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(current, opened) {
		return errors.New("filesystem identity root changed while validating")
	}
	return nil
}

func fileAuthority(info os.FileInfo) (links uint64, owner uint32, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(stat.Nlink), stat.Uid, true
}

func syncRoot(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
