package loop

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// Two loops in ONE checkout is the failure this lock exists for: each commits its own task, and
// each one's completion range then holds the other's task-bound commit, so both are rejected and
// finished work reopens. The second loop must be refused, and the refusal must name the holder.
func TestLoopCheckoutLockRefusesASecondLoopInTheSameTree(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	repo := t.TempDir()

	release, err := lockLoopCheckout(cfg, repo)
	if err != nil {
		t.Fatalf("first lockLoopCheckout(%q) = %v, want it to be granted", repo, err)
	}

	if _, err = lockLoopCheckout(cfg, repo); err == nil {
		t.Fatal("a second loop acquired the same checkout's lock; two loops would commit over each other")
	} else if !strings.Contains(err.Error(), "another coop loop is already working") {
		t.Errorf("second lockLoopCheckout error = %q, want it to say another loop holds the checkout", err)
	} else if !strings.Contains(err.Error(), fmt.Sprintf("pid %d", os.Getpid())) {
		t.Errorf("second lockLoopCheckout error = %q, want it to name the holding pid", err)
	}

	// Releasing hands the checkout to the next loop — a finished run must not park it forever.
	release()
	release2, err := lockLoopCheckout(cfg, repo)
	if err != nil {
		t.Fatalf("lockLoopCheckout after release = %v, want the checkout to be free again", err)
	}
	release2()
}

// The fleet is the reason this is keyed on the WORKTREE, not the repo: forks each run their own
// loop concurrently, and a lock that serialized them would defeat forks entirely.
func TestLoopCheckoutLockKeepsSeparateWorktreesParallel(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}

	releaseA, err := lockLoopCheckout(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("lock on worktree A = %v, want it granted", err)
	}
	defer releaseA()

	releaseB, err := lockLoopCheckout(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("lock on worktree B = %v, want concurrent forks to stay parallel", err)
	}
	defer releaseB()
}
