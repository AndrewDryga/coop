package forkspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkspaceReservationPublicationRetryRepeatsDirectorySync(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	makeGenerationWorkspace(t, repo, "publish-retry")
	identity := ensureTestGeneration(t, repo, "publish-retry")
	record := WorkspaceReservation{
		Version: workspaceReservationVersion, Fork: identity,
		Kind: WorkspaceReservationRemoteSession, OwnerID: "remote_publish", CreatedAt: time.Now().UTC(),
	}
	previous := syncReservationDirectory
	t.Cleanup(func() { syncReservationDirectory = previous })
	failure := errors.New("synthetic reservation publish directory sync failure")
	syncReservationDirectory = func(*os.File) error { return failure }

	unlock, err := LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	firstErr := ReserveWorkspaceLocked(repo, record)
	unlock()
	if !errors.Is(firstErr, failure) {
		t.Fatalf("first reservation error = %v, want %v", firstErr, failure)
	}
	if current, present, err := ReadWorkspaceReservation(repo, identity); err != nil || !present || current.OwnerID != record.OwnerID {
		t.Fatalf("visible reservation after sync failure = %+v, present=%v err=%v", current, present, err)
	}

	syncCalls := 0
	syncReservationDirectory = func(dir *os.File) error {
		syncCalls++
		return dir.Sync()
	}
	unlock, err = LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	retryErr := ReserveWorkspaceLocked(repo, record)
	unlock()
	if retryErr != nil || syncCalls != 1 {
		t.Fatalf("reservation retry = %v with %d directory syncs; want one successful barrier", retryErr, syncCalls)
	}
}

func TestWorkspaceReservationDirectoryCreationRetryRepeatsParentBarrier(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := EnsureStateDir(repo); err != nil {
		t.Fatal(err)
	}
	previous := syncForkDirectoryEntry
	t.Cleanup(func() { syncForkDirectoryEntry = previous })
	failure := errors.New("synthetic reservation parent sync failure")
	syncForkDirectoryEntry = func(dir *os.File) error {
		if filepath.Clean(dir.Name()) == filepath.Clean(StateDir(repo)) {
			return failure
		}
		return dir.Sync()
	}
	if err := ensureReservationDir(repo); !errors.Is(err, failure) {
		t.Fatalf("first reservation directory creation = %v, want %v", err, failure)
	}
	if _, err := os.Stat(reservationDir(repo)); err != nil {
		t.Fatalf("reservation directory was not visible after failed parent barrier: %v", err)
	}

	confirmed := false
	syncForkDirectoryEntry = func(dir *os.File) error {
		if filepath.Clean(dir.Name()) == filepath.Clean(StateDir(repo)) {
			confirmed = true
		}
		return dir.Sync()
	}
	if err := ensureReservationDir(repo); err != nil {
		t.Fatal(err)
	}
	if !confirmed {
		t.Fatal("reservation creation retry did not repeat the parent barrier")
	}
}

func TestWorkspaceReservationRemovalRetryRepeatsDirectorySync(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	makeGenerationWorkspace(t, repo, "remove-retry")
	identity := ensureTestGeneration(t, repo, "remove-retry")
	record := WorkspaceReservation{
		Version: workspaceReservationVersion, Fork: identity,
		Kind: WorkspaceReservationRemoteSession, OwnerID: "remote_remove", CreatedAt: time.Now().UTC(),
	}
	unlock, err := LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReserveWorkspaceLocked(repo, record); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()

	previous := syncReservationDirectory
	t.Cleanup(func() { syncReservationDirectory = previous })
	failure := errors.New("synthetic reservation removal directory sync failure")
	syncReservationDirectory = func(*os.File) error { return failure }
	unlock, err = LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	firstErr := RemoveWorkspaceReservationIfMatchesLocked(repo, record)
	unlock()
	if !errors.Is(firstErr, failure) {
		t.Fatalf("first removal error = %v, want %v", firstErr, failure)
	}
	if _, present, err := ReadWorkspaceReservation(repo, identity); err != nil || present {
		t.Fatalf("post-unlink reservation: present=%v err=%v", present, err)
	}

	syncCalls := 0
	syncReservationDirectory = func(dir *os.File) error {
		syncCalls++
		return dir.Sync()
	}
	unlock, err = LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	retryErr := RemoveWorkspaceReservationIfMatchesLocked(repo, record)
	unlock()
	if retryErr != nil || syncCalls != 1 {
		t.Fatalf("removal retry = %v with %d directory syncs; want one successful barrier", retryErr, syncCalls)
	}
}

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
