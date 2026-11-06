package loop

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
)

// Two loops in ONE checkout is the failure this lock exists for: each commits its own task, and
// each one's completion range then holds the other's task-bound commit, so both are rejected and
// finished work reopens. The second loop must be refused, and the refusal must name the holder.
func TestLoopCheckoutLockRefusesASecondLoopInTheSameTree(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	repo := t.TempDir()

	release, _, err := lockLoopCheckout(cfg, repo)
	if err != nil {
		t.Fatalf("first lockLoopCheckout(%q) = %v, want it to be granted", repo, err)
	}

	started := time.Now()
	if _, _, err = lockLoopCheckout(cfg, repo); err == nil {
		t.Fatal("a second loop acquired the same checkout's lock; two loops would commit over each other")
	} else if !strings.Contains(err.Error(), "another coop loop is already working") {
		t.Errorf("second lockLoopCheckout error = %q, want it to say another loop holds the checkout", err)
	} else if !strings.Contains(err.Error(), fmt.Sprintf("pid %d", os.Getpid())) {
		t.Errorf("second lockLoopCheckout error = %q, want it to name the holding pid", err)
	}
	if waited := time.Since(started); waited > stoppingWait/2 {
		t.Errorf("a loop that is working was waited on for %s; only a stopping one may be", waited)
	}

	// Releasing hands the checkout to the next loop — a finished run must not park it forever.
	release()
	release2, _, err := lockLoopCheckout(cfg, repo)
	if err != nil {
		t.Fatalf("lockLoopCheckout after release = %v, want the checkout to be free again", err)
	}
	release2()
}

// A hard stop still tears the box and services down while it holds the checkout. A loop started
// then (a closed terminal leaves the old one tearing down out of sight) waits for it instead of
// being refused, and says what it is waiting for; one that never finishes is still named.
func TestLoopCheckoutLockWaitsForALoopThatIsStopping(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	repo := t.TempDir()
	release, markStopping, err := lockLoopCheckout(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	markStopping()
	type result struct {
		release func()
		err     error
	}
	granted := make(chan result, 1)
	notice := captureStderr(t, func() {
		go func() {
			next, _, err := lockLoopCheckout(cfg, repo)
			granted <- result{next, err}
		}()
		time.Sleep(300 * time.Millisecond)
		select {
		case got := <-granted:
			t.Errorf("the next loop did not wait for the stopping one: %v", got.err)
		default:
		}
		release()
		got := <-granted
		if got.err != nil {
			t.Errorf("the next loop after the stopping one ended = %v, want the checkout", got.err)
		} else {
			got.release()
		}
	})
	if want := fmt.Sprintf("Waiting for the loop that is stopping in this checkout (pid %d) to finish.", os.Getpid()); !strings.Contains(notice, want) {
		t.Errorf("waiting notice = %q, want %q", notice, want)
	}

	saved := stoppingWait
	stoppingWait = 200 * time.Millisecond
	t.Cleanup(func() { stoppingWait = saved })
	stuck, markStuck, err := lockLoopCheckout(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer stuck()
	markStuck()
	_ = captureStderr(t, func() {
		if _, _, err := lockLoopCheckout(cfg, repo); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("(pid %d, stopping) did not finish within", os.Getpid())) {
			t.Errorf("waiting past a stuck teardown = %v, want the stopping holder named", err)
		}
	})
}

// Parallel forks are why this is keyed on the WORKTREE, not the repo: each runs its own
// loop concurrently, and a lock that serialized them would defeat forks entirely.
func TestLoopCheckoutLockKeepsSeparateWorktreesParallel(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}

	releaseA, _, err := lockLoopCheckout(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("lock on worktree A = %v, want it granted", err)
	}
	defer releaseA()

	releaseB, _, err := lockLoopCheckout(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("lock on worktree B = %v, want concurrent forks to stay parallel", err)
	}
	defer releaseB()
}
