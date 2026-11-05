package forkspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testReservationStoreID = "store_test"

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

// A version 1 reservation, from Coop v10.1.2 or earlier, is refused by name: every read of it fails,
// so the fork name stays blocked and nothing may replace, relabel or remove it, and the file is
// left exactly as it was.
func TestVersion1WorkspaceReservationIsRefusedByName(t *testing.T) {
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
	path := filepath.Join(reservationDir(repo), reservationName(identity))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, present, err := ReadWorkspaceReservation(repo, identity); err == nil || present || !strings.Contains(err.Error(), "version 1 fork workspace reservation") {
		t.Fatalf("ReadWorkspaceReservation = present %t, %v; want a refusal naming version 1", present, err)
	}
	if err := RequireNoWorkspaceReservationLocked(repo, identity); err == nil || !strings.Contains(err.Error(), "v12.1.0") {
		t.Fatalf("RequireNoWorkspaceReservationLocked = %v; want a refusal naming the converting build", err)
	}
	if err := RequireForkNameAvailable(repo, "old"); err == nil {
		t.Fatal("a version 1 reservation stopped blocking its fork name")
	}
	if err := RemoveWorkspaceReservationIfMatchesLocked(repo, legacy); err == nil {
		t.Fatal("a version 1 reservation gained automatic deletion authority")
	}
	if err := ReserveWorkspaceLocked(repo, WorkspaceReservation{
		Version: WorkspaceReservationVersion, Fork: identity, Kind: WorkspaceReservationRemoteSession,
		OwnerStoreID: testReservationStoreID, OwnerID: legacy.OwnerID, CreatedAt: time.Now().UTC(),
	}); err == nil {
		t.Fatal("a version 1 reservation was silently relabeled as version 2")
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(body) {
		t.Fatalf("the version 1 reservation changed: %q, %v", after, err)
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

// An eval's CreateRemoteSession failed with ENOENT: another session released its reservation
// between this scan's directory read and its read of that entry, and the vanished entry was
// reported as a registry problem that refused the unrelated create. A released entry is absent;
// an entry that is still there but unreadable is still a problem.
func TestWorkspaceReservationReleasedDuringScanIsAbsentNotAProblem(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	reserve := func(name, owner string) WorkspaceReservation {
		t.Helper()
		makeGenerationWorkspace(t, repo, name)
		identity := ensureTestGeneration(t, repo, name)
		record := WorkspaceReservation{
			Version: workspaceReservationVersion, Fork: identity,
			Kind: WorkspaceReservationRemoteSession, OwnerStoreID: testReservationStoreID, OwnerID: owner, CreatedAt: time.Now().UTC(),
		}
		unlock, err := LockState(repo, name)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		if err := ReserveWorkspaceLocked(repo, record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	kept := reserve("kept", "remote_kept")
	released := reserve("released", "remote_released")

	previous := afterReservationDirectoryRead
	t.Cleanup(func() { afterReservationDirectoryRead = previous })
	afterReservationDirectoryRead = func() {
		if err := os.Remove(filepath.Join(reservationDir(repo), reservationName(released.Fork))); err != nil {
			t.Fatal(err)
		}
	}
	records, problems := WorkspaceReservations(repo)
	if len(problems) != 0 {
		t.Fatalf("a reservation released during the scan was reported as a problem: %v", problems)
	}
	if len(records) != 1 || records[0].OwnerID != kept.OwnerID {
		t.Fatalf("records after a release during the scan = %+v, want only %q", records, kept.OwnerID)
	}

	// An entry replaced by something that is not a reservation still refuses.
	afterReservationDirectoryRead = func() {
		path := filepath.Join(reservationDir(repo), reservationName(kept.Fork))
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, problems := WorkspaceReservations(repo); len(problems) != 1 {
		t.Fatalf("an entry replaced during the scan gave problems %v, want one", problems)
	}
}
