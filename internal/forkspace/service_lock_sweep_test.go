package forkspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// releasedServiceLock takes and releases repo's service-launch lock, as a finished launch leaves it,
// and returns the lock file's path.
func releasedServiceLock(t *testing.T, repo string) string {
	t.Helper()
	path, err := ServiceLaunchLockPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := LockServiceLaunch(context.Background(), repo, false)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	return path
}

// The sweep removes the lock of a repository that is gone, and only that: a lock another process
// holds stays even though its repository is gone, a live repository keeps its lock, and a lock made
// before locks named their repository stays for a day.
func TestServiceLaunchLockSweepRemovesOnlyTheLocksOfGoneRepositories(t *testing.T) {
	t.Setenv(TestExecutionRegistryRootEnv, t.TempDir())
	parent := t.TempDir()
	live, gone, held := filepath.Join(parent, "live"), filepath.Join(parent, "gone"), filepath.Join(parent, "held")
	for _, repo := range []string{live, gone, held} {
		if err := os.Mkdir(repo, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	liveLock, goneLock := releasedServiceLock(t, live), releasedServiceLock(t, gone)
	heldLock, err := ServiceLaunchLockPath(held)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := LockServiceLaunch(context.Background(), held, false)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(liveLock)
	oldUnnamed, newUnnamed := filepath.Join(dir, "1-2"+serviceLaunchLockName), filepath.Join(dir, "1-3"+serviceLaunchLockName)
	for _, path := range []string{oldUnnamed, newUnnamed} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dayAgo := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(oldUnnamed, dayAgo, dayAgo); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{gone, held} {
		if err := os.Remove(repo); err != nil {
			t.Fatal(err)
		}
	}

	if removed, err := sweepServiceLaunchLocks(dir, 100, time.Now()); err != nil || removed != 2 {
		t.Fatalf("sweep = %d, %v; want the gone repository's lock and the day-old unnamed one", removed, err)
	}
	for path, want := range map[string]bool{liveLock: true, heldLock: true, newUnnamed: true, goneLock: false, oldUnnamed: false} {
		if _, err := os.Lstat(path); (err == nil) != want {
			t.Errorf("after the sweep %s exists = %v, want %v", filepath.Base(path), err == nil, want)
		}
	}
	unlock()
	if removed, err := sweepServiceLaunchLocks(dir, 100, time.Now()); err != nil || removed != 1 {
		t.Fatalf("sweep after the holder left = %d, %v; want its lock removed", removed, err)
	}
}

// One pass looks at no more locks than its limit, so a folder of tens of thousands goes over days.
func TestServiceLaunchLockSweepIsBounded(t *testing.T) {
	t.Setenv(TestExecutionRegistryRootEnv, t.TempDir())
	parent := t.TempDir()
	var dir string
	for _, name := range []string{"a", "b", "c"} {
		repo := filepath.Join(parent, name)
		if err := os.Mkdir(repo, 0o700); err != nil {
			t.Fatal(err)
		}
		dir = filepath.Dir(releasedServiceLock(t, repo))
		if err := os.Remove(repo); err != nil {
			t.Fatal(err)
		}
	}
	if removed, err := sweepServiceLaunchLocks(dir, 2, time.Now()); err != nil || removed != 2 {
		t.Fatalf("bounded sweep = %d, %v; want 2", removed, err)
	}
	if removed, err := sweepServiceLaunchLocks(dir, 2, time.Now()); err != nil || removed != 1 {
		t.Fatalf("next sweep = %d, %v; want the last one", removed, err)
	}
}

// A launch that opened the lock file just before the sweep removed it takes the lock again on the
// file there now, so two launches never each hold a lock on a different file.
func TestServiceLaunchLockIsTakenAgainWhenTheSweepRemovedIt(t *testing.T) {
	t.Setenv(TestExecutionRegistryRootEnv, t.TempDir())
	repo := t.TempDir()
	removedOnce := false
	defer func(saved func(string)) { serviceLaunchLockOpened = saved }(serviceLaunchLockOpened)
	serviceLaunchLockOpened = func(path string) {
		if !removedOnce {
			removedOnce = true
			if err := os.Remove(path); err != nil {
				t.Error(err)
			}
		}
	}
	unlock, err := LockServiceLaunch(context.Background(), repo, true)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	serviceLaunchLockOpened = func(string) {}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if other, err := LockServiceLaunch(ctx, repo, true); !errors.Is(err, context.DeadlineExceeded) {
		if other != nil {
			other()
		}
		t.Fatalf("a second launch took the lock the first holds: %v", err)
	}
}
