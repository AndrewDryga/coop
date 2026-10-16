package sessionsvc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// A Work session with sixteen read-only companions waited six minutes for its
// creation on a colima worker, cloning them one after another (2026-10-15).
// Preparation now runs several at once, still returning them in the job's order.
func TestSessionCompanionsPrepareSeveralAtOnceInJobOrder(t *testing.T) {
	stateRoot, bindings := companionBindings(t, 6)

	var mu sync.Mutex
	var opened sync.Once
	running, most := 0, 0
	release := make(chan struct{})
	testCompanionPrepareStarted = func(string) {
		mu.Lock()
		running++
		most = max(most, running)
		if running == companionPrepareParallelism {
			opened.Do(func() { close(release) })
		}
		mu.Unlock()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		mu.Lock()
		running--
		mu.Unlock()
	}
	t.Cleanup(func() { testCompanionPrepareStarted = nil })

	prepared, err := ensureSessionCompanionsContext(
		context.Background(), stateRoot, "remote_00000000000000000000000000000000", bindings,
	)
	if err != nil {
		t.Fatal(err)
	}
	if most != companionPrepareParallelism {
		t.Fatalf("at most %d companions prepared at once, want %d", most, companionPrepareParallelism)
	}
	if len(prepared) != len(bindings) {
		t.Fatalf("prepared %d companions, want %d", len(prepared), len(bindings))
	}
	for index, companion := range prepared {
		if companion.Name != bindings[index].Name {
			t.Fatalf("companion %d is %q, want the job's order %q", index, companion.Name, bindings[index].Name)
		}
		if !pathExists(filepath.Join(companion.Workspace, "README.md")) {
			t.Fatalf("companion %q has no checkout", companion.Name)
		}
	}
}

// A failure returns the companions already prepared, for the creation to roll
// back, and names the one that failed.
func TestSessionCompanionFailureReturnsThePreparedOnesForRollback(t *testing.T) {
	stateRoot, bindings := companionBindings(t, 3)
	bindings[1].BaseCommit = strings.Repeat("e", 40)

	prepared, err := ensureSessionCompanionsContext(
		context.Background(), stateRoot, "remote_00000000000000000000000000000000", bindings,
	)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("ensure companion %q", bindings[1].Name)) {
		t.Fatalf("error = %v, want the failed companion named", err)
	}
	for _, companion := range prepared {
		if companion.Name == bindings[1].Name {
			t.Fatal("the failed companion is listed as prepared")
		}
		if !pathExists(companion.Workspace) {
			t.Fatalf("prepared companion %q has no workspace to roll back", companion.Name)
		}
	}
	if pathExists(bindings[1].Workspace) {
		t.Fatal("the failed companion left a workspace behind")
	}
}

func companionBindings(t *testing.T, count int) (string, []session.CompanionRepository) {
	t.Helper()
	stateRoot := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	bindings := make([]session.CompanionRepository, 0, count)
	for index := range count {
		repository, git := gitrepo.New(t)
		if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("companion\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "README.md")
		git("commit", "--quiet", "-m", "readme")
		name := fmt.Sprintf("companion-%d", index)
		workspace, err := sessionCompanionWorkspace(stateRoot, "remote_00000000000000000000000000000000", name)
		if err != nil {
			t.Fatal(err)
		}
		bindings = append(bindings, session.CompanionRepository{
			Name: name, Repository: repository, Workspace: workspace,
			BaseCommit: gitOut(repository, "rev-parse", "HEAD"),
		})
	}
	return stateRoot, bindings
}
