package forkspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/starttoken"
)

// A fork's waiting box is shown for as long as it waits and only for its own fork. A dead process's
// note says nothing, and the listing that finds it removes it.
func TestLaunchWaitsShowsOnlyTheLiveWaitsOfAFork(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if boxes, waiting := LaunchWaits(repo, "a"); waiting || boxes != nil {
		t.Fatalf("a fork with no notes waits for %v", boxes)
	}
	forget := NoteLaunchWait(repo, "a", []string{"a loop in fork a"})
	other := NoteLaunchWait(repo, "a", []string{"a gate", "a loop in fork a"})
	if boxes, waiting := LaunchWaits(repo, "a"); !waiting || !slices.Equal(boxes, []string{"a gate", "a loop in fork a"}) &&
		!slices.Equal(boxes, []string{"a loop in fork a", "a gate"}) {
		t.Fatalf("fork a waits = %v, %v; want both boxes once", boxes, waiting)
	}
	if boxes, waiting := LaunchWaits(repo, "b"); waiting {
		t.Fatalf("fork b shows fork a's wait for %v", boxes)
	}
	forget()
	other()
	if boxes, waiting := LaunchWaits(repo, "a"); waiting {
		t.Fatalf("an ended wait still shows %v", boxes)
	}

	body, err := json.Marshal(launchWait{Fork: "a", PID: 2147483646, Token: starttoken.Stale(), Boxes: []string{"a loop"}})
	if err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(StateDir(repo), forkLaunchWaitFolder, "dead.json")
	if err := os.WriteFile(dead, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if boxes, waiting := LaunchWaits(repo, "a"); waiting {
		t.Fatalf("a dead process's note shows %v", boxes)
	}
	if _, err := os.Lstat(dead); !os.IsNotExist(err) {
		t.Fatalf("a dead process's note was left behind: %v", err)
	}
}
