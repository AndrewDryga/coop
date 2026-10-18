package box

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestProjectReviewSnapshotPreservesLiveAndReapsAbandoned(t *testing.T) {
	parent := t.TempDir()
	live, lease, err := newProjectReviewSnapshot(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	abandoned, dead, err := newProjectReviewSnapshot(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	_ = dead.Close() // Models killed host before a Docker client exists.
	unknown := filepath.Join(parent, projectReviewPrefix+"unknown")
	if err := os.Mkdir(unknown, 0o700); err != nil {
		t.Fatal(err)
	}
	next, nextLease, err := newProjectReviewSnapshot(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	defer nextLease.Close()
	defer os.RemoveAll(next)
	if _, err := os.Stat(live); err != nil {
		t.Fatal("live review was removed", err)
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatal("abandoned snapshot survived", err)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatal("ambiguous bundle was removed", err)
	}
}

func TestProjectReviewClosePreservesAnInheritedBuildLease(t *testing.T) {
	parent := t.TempDir()
	dir, lease, err := newProjectReviewSnapshot(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sh", "-c", "read line")
	child.ExtraFiles = []*os.File{lease}
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); _ = child.Wait() }()
	prepared := &PreparedProjectBuild{cleanupDir: dir, dir: filepath.Join(dir, "context"), lease: lease}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("surviving Docker child lost its context", err)
	}
	_ = input.Close()
	if err := child.Wait(); err == nil {
		t.Fatal("read unexpectedly succeeded at EOF")
	}
	next, nextLease, err := newProjectReviewSnapshot(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	defer nextLease.Close()
	defer os.RemoveAll(next)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("departed child left a permanent snapshot", err)
	}
}

func TestConcurrentProjectReviewsKeepIndependentCustody(t *testing.T) {
	parent := t.TempDir()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			dir, lease, err := newProjectReviewSnapshot(t.Context(), parent)
			if err != nil {
				t.Error(err)
				return
			}
			defer lease.Close()
			if _, err := os.Stat(dir); err != nil {
				t.Error("concurrent reaper removed a live snapshot", err)
			}
		})
	}
	wg.Wait()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if root, lock, err := projectReviewCustody(ctx, parent); err == nil {
		_ = lock.Close()
		_ = root.Close()
		t.Fatal("cancelled review acquired custody")
	}
}
