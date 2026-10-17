package forkspace

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// SweepServiceLaunchLocks removes the service-launch locks whose repository is gone, looking at no
// more than limit of them, and reports how many it removed. Every box launch takes the lock of its
// repository's folder, so each throwaway repository, such as a doctor fixture, an eval trial or a
// session's job source, used to leave one behind for good. A test binary sweeps only the root it
// was pointed at.
func SweepServiceLaunchLocks(limit int) (int, error) {
	root, err := executionRegistryRoot()
	if err != nil {
		return 0, err
	}
	return sweepServiceLaunchLocks(filepath.Join(root, "service-locks"), limit, time.Now())
}

// sweepServiceLaunchLocks removes a lock only while holding it exclusively, taken without waiting,
// so never one another process holds; one about to take it takes it again on the file there next
// (LockServiceLaunch). A lock that names its repository goes when that path is gone or is now
// another folder. A lock older than a day that names none was made before locks named theirs: its
// repository cannot be checked, and the next launch for it only makes it again.
func sweepServiceLaunchLocks(dir string, limit int, now time.Time) (int, error) {
	locks, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	defer locks.Close()
	folder, err := locks.Open(".")
	if err != nil {
		return 0, err
	}
	names, err := folder.Readdirnames(-1)
	_ = folder.Close()
	if err != nil {
		return 0, err
	}
	slices.Sort(names)
	removed, examined := 0, 0
	var errs []error
	for _, name := range names {
		key, ok := strings.CutSuffix(name, serviceLaunchLockName)
		if !ok {
			continue
		}
		if examined == limit {
			break
		}
		examined++
		gone, err := removeGoneServiceLaunchLock(locks, name, key, now)
		if err != nil {
			errs = append(errs, err)
		}
		if gone {
			removed++
		}
	}
	return removed, errors.Join(errs...)
}

func removeGoneServiceLaunchLock(locks *os.Root, name, key string, now time.Time) (bool, error) {
	f, err := locks.OpenFile(name, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false, nil // already gone, or not a plain file: nothing of the sweep's
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false, nil
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false, nil // held
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	body := make([]byte, 4097)
	n, err := f.ReadAt(body, 0)
	if (err != nil && !errors.Is(err, io.EOF)) || n == len(body) {
		return false, nil
	}
	if !serviceLockRepositoryGone(strings.TrimSuffix(string(body[:n]), "\n"), key, info, now) {
		return false, nil
	}
	if current, err := locks.Lstat(name); err != nil || !os.SameFile(info, current) {
		return false, nil
	}
	if err := locks.Remove(name); err != nil {
		return false, err
	}
	return true, nil
}

// serviceLockRepositoryGone keeps what it cannot judge: a recorded path that is not absolute, or a
// repository it could not look at.
func serviceLockRepositoryGone(repo, key string, lock os.FileInfo, now time.Time) bool {
	if repo == "" {
		return now.Sub(lock.ModTime()) > 24*time.Hour
	}
	if !filepath.IsAbs(repo) || strings.ContainsAny(repo, "\n\x00") {
		return false
	}
	info, err := os.Stat(repo)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && fmt.Sprintf("%x-%x", uint64(stat.Dev), uint64(stat.Ino)) != key
}
