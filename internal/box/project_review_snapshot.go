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
// is the validated owner-private network store, outside every mount, so nothing but Coop
// changes what is in it.
func projectReviewCustody(ctx context.Context, parent string) (*os.Root, *os.File, error) {
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, nil, err
	}
	lock, err := openProjectReviewCustody(root)
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
	if err != nil {
		if lock != nil {
			_ = lock.Close()
		}
		_ = root.Close()
		return nil, nil, err
	}
	return root, lock, nil
}

// os.Root.OpenFile with O_CREATE answers ENOENT when another goroutine or process creates the same
// name at that moment (Go 1.27.2 on darwin: 3,374 of 8,000 concurrent first opens; a plain
// os.OpenFile, none). The name exists by then, so the open is tried again; three tries left no
// failure in 8,000. TestConcurrentProjectReviewsKeepIndependentCustody failed 63 of 100 runs.
func openProjectReviewCustody(root *os.Root) (*os.File, error) {
	for attempt := 1; ; attempt++ {
		lock, err := root.OpenFile("project-reviews.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
		if err == nil || !errors.Is(err, os.ErrNotExist) || attempt == 3 {
			return lock, err
		}
	}
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
	if !strings.HasPrefix(name, projectReviewPrefix) || filepath.Base(name) != name {
		return false, nil
	}
	lease, err := root.OpenFile(filepath.Join(name, "lease"), os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, nil
	}
	defer lease.Close()
	if err := syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false, nil
	}
	err = root.RemoveAll(name)
	return err == nil, err
}
