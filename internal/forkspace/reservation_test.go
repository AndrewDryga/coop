package forkspace

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testReservationStoreID = "store_test"

func TestWorkspaceReservationPublicationRetryRepeatsDirectorySync(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	makeGenerationWorkspace(t, repo, "publish-retry")
	identity := ensureTestGeneration(t, repo, "publish-retry")
	record := WorkspaceReservation{
		Version: workspaceReservationVersion, Fork: identity,
		Kind: WorkspaceReservationRemoteSession, OwnerStoreID: testReservationStoreID, OwnerID: "remote_publish", CreatedAt: time.Now().UTC(),
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
		Kind: WorkspaceReservationRemoteSession, OwnerStoreID: testReservationStoreID, OwnerID: "remote_remove", CreatedAt: time.Now().UTC(),
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
		Kind: WorkspaceReservationRemoteSession, OwnerStoreID: testReservationStoreID, OwnerID: "remote_session", CreatedAt: time.Now().UTC(),
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
	other = record
	other.OwnerStoreID = "store_other"
	if err := ReserveWorkspaceLocked(repo, other); err == nil {
		unlock()
		t.Fatal("another store replayed the reservation")
	}
	if err := RemoveWorkspaceReservationIfMatchesLocked(repo, other); err == nil {
		unlock()
		t.Fatal("another store removed the reservation")
	}
	if _, present, err := ReadWorkspaceReservation(repo, identity); err != nil || !present {
		unlock()
		t.Fatalf("wrong-store removal changed reservation: present=%t err=%v", present, err)
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

func TestLegacyWorkspaceReservationIsVisibleButHasNoNewAuthority(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	makeGenerationWorkspace(t, repo, "old")
	identity := ensureTestGeneration(t, repo, "old")
	legacy := WorkspaceReservation{
		Version: 1, Fork: identity, Kind: WorkspaceReservationRemoteSession,
		OwnerID: "remote_old", CreatedAt: time.Now().UTC(),
	}
	if err := os.MkdirAll(reservationDir(repo), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reservationDir(repo), reservationName(identity)), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if current, present, err := ReadWorkspaceReservation(repo, identity); err != nil || !present || current != legacy {
		t.Fatalf("legacy reservation not readable: %+v present=%t err=%v", current, present, err)
	}
	if err := RequireNoWorkspaceReservationLocked(repo, identity); err == nil || !strings.Contains(err.Error(), "recover it offline") {
		t.Fatalf("legacy reservation did not give an actionable refusal: %v", err)
	}
	if err := RemoveWorkspaceReservationIfMatchesLocked(repo, legacy); err == nil {
		t.Fatal("legacy reservation gained automatic deletion authority")
	}
	if err := ReserveWorkspaceLocked(repo, WorkspaceReservation{
		Version: WorkspaceReservationVersion, Fork: identity, Kind: WorkspaceReservationRemoteSession,
		OwnerStoreID: testReservationStoreID, OwnerID: legacy.OwnerID, CreatedAt: time.Now().UTC(),
	}); err == nil {
		t.Fatal("v1 reservation was silently relabeled as v2")
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
		Kind: WorkspaceReservationRemoteSession, OwnerStoreID: testReservationStoreID, OwnerID: "session", CreatedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("symlinked reservation registry was accepted")
	}
}
