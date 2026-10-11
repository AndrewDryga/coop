package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const projectReviewPrefix = "coop-project-review-"

// Custody serializes allocation and reaping, not the user's review or the build. The parent
// is the validated owner-private network store, outside every mount.
func projectReviewCustody(ctx context.Context, parent string) (*os.Root, *os.File, error) {
	beforeRoot, err := os.Lstat(parent)
	if err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, nil, err
	}
	lock, err := root.OpenFile("project-reviews.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err == nil {
		var info os.FileInfo
		info, err = lock.Stat()
		if err == nil && !privateProjectReviewFile(info) {
			err = errors.New("project review custody file is unsafe")
		}
		if err == nil {
			wait, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			for {
				err = wait.Err()
				if err != nil {
					break
				}
				err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
				if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
					break
				}
				select {
				case <-wait.Done():
				case <-time.After(20 * time.Millisecond):
				}
			}
		}
		if err == nil {
			current, currentErr := root.Lstat("project-reviews.lock")
			currentRoot, rootErr := os.Lstat(parent)
			anchored, anchoredErr := root.Stat(".")
			if currentErr != nil || rootErr != nil || anchoredErr != nil || !os.SameFile(info, current) || !os.SameFile(beforeRoot, currentRoot) || !os.SameFile(beforeRoot, anchored) {
				err = errors.New("project review custody changed while opening")
			}
		}
	}
	if err != nil {
		if lock != nil {
			_ = lock.Close()
		}
		_ = root.Close()
		return nil, nil, err
	}
	return root, lock, nil
}

func privateProjectReviewFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().IsRegular() && info.Mode().Perm() == 0o600
}

func newProjectReviewSnapshot(ctx context.Context, parent string) (string, *os.File, error) {
	root, custody, err := projectReviewCustody(ctx, parent)
	if err != nil {
		return "", nil, err
	}
	defer root.Close()
	defer custody.Close()
	dir, err := root.Open(".")
	if err != nil {
		return "", nil, err
	}
	names, err := dir.Readdirnames(-1)
	_ = dir.Close()
	if err != nil {
		return "", nil, err
	}
	for _, name := range names {
		if _, err := reapProjectReviewSnapshot(root, name); err != nil {
			return "", nil, err
		}
	}
	private, err := os.MkdirTemp(parent, projectReviewPrefix)
	if err != nil {
		return "", nil, err
	}
	lease, err := os.OpenFile(filepath.Join(private, "lease"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		err = syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err != nil {
		if lease != nil {
			_ = lease.Close()
		}
		_ = root.RemoveAll(filepath.Base(private))
		return "", nil, err
	}
	return private, lease, nil
}

// Age and container mounts cannot prove a pending review/build is dead. The host and Docker
// client share this lease; only an independent, nonblocking acquisition permits deletion.
func reapProjectReviewSnapshot(root *os.Root, name string) (bool, error) {
	return reapOwnedProjectReviewSnapshot(root, name, nil, nil)
}

func reapOwnedProjectReviewSnapshot(root *os.Root, name string, ownedDir, ownedLease os.FileInfo) (bool, error) {
	if !strings.HasPrefix(name, projectReviewPrefix) || filepath.Base(name) != name {
		return false, nil
	}
	dir, err := root.Lstat(name)
	if err != nil || !dir.IsDir() || dir.Mode().Perm() != 0o700 {
		return false, nil
	}
	dirStat, ok := dir.Sys().(*syscall.Stat_t)
	if !ok || dirStat.Uid != uint32(os.Geteuid()) {
		return false, nil
	}
	leaseName := filepath.Join(name, "lease")
	before, err := root.Lstat(leaseName)
	if err != nil || !privateProjectReviewFile(before) {
		return false, nil
	}
	if ownedDir != nil && (!os.SameFile(ownedDir, dir) || ownedLease == nil || !os.SameFile(ownedLease, before)) {
		return false, errors.New("project review snapshot ownership changed; replacement preserved")
	}
	lease, err := root.OpenFile(leaseName, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, nil
	}
	defer lease.Close()
	if err := syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false, nil
	}
	after, err := lease.Stat()
	current, currentErr := root.Lstat(leaseName)
	currentDir, dirErr := root.Lstat(name)
	if err != nil || currentErr != nil || dirErr != nil || !os.SameFile(before, after) || !os.SameFile(before, current) || !os.SameFile(dir, currentDir) {
		return false, nil
	}
	err = root.RemoveAll(name)
	return err == nil, err
}
