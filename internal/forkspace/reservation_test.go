package forkspace

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkspaceReservationIsExactIdempotentAndBlocksLifecycle(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	makeGenerationWorkspace(t, repo, "remote")
	identity := ensureTestGeneration(t, repo, "remote")
	record := WorkspaceReservation{
		Version: workspaceReservationVersion, Fork: identity,
		Kind: WorkspaceReservationRemoteSession, OwnerID: "remote_session", CreatedAt: time.Now().UTC(),
	}
	unlock, err := LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReserveWorkspaceLocked(repo, record); err != nil {
		unlock()
		t.Fatal(err)
	}
	assertForkStatePerm(t, StateDir(repo), 0o700)
	assertForkStatePerm(t, reservationDir(repo), 0o700)
	assertForkStatePerm(t, filepath.Join(reservationDir(repo), reservationName(identity)), 0o600)
	if err := ReserveWorkspaceLocked(repo, record); err != nil {
		unlock()
		t.Fatalf("idempotent reserve: %v", err)
	}
	if err := RequireNoWorkspaceReservationLocked(repo, identity); err == nil {
		unlock()
		t.Fatal("session reservation did not block lifecycle mutation")
	}
	other := record
	other.OwnerID = "other_session"
	if err := ReserveWorkspaceLocked(repo, other); err == nil {
		unlock()
		t.Fatal("another session replaced reservation")
	}
	if err := RemoveWorkspaceReservationIfMatchesLocked(repo, record); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	if _, ok, err := ReadWorkspaceReservation(repo, identity); err != nil || ok {
		t.Fatalf("reservation after cleanup: ok=%v err=%v", ok, err)
	}
}

func TestWorkspaceReservationDoesNotFollowRegistrySymlink(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	makeGenerationWorkspace(t, repo, "remote")
	identity := ensureTestGeneration(t, repo, "remote")
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), reservationDir(repo)); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	err = ReserveWorkspaceLocked(repo, WorkspaceReservation{
		Version: workspaceReservationVersion, Fork: identity,
		Kind: WorkspaceReservationRemoteSession, OwnerID: "session", CreatedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("symlinked reservation registry was accepted")
	}
}
