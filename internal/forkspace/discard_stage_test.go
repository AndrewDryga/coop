package forkspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stagedFixture(t *testing.T, name string) (string, string) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "project")
	workspace := Workspace(repo, name)
	writeBytes(t, filepath.Join(workspace, "payload.bin"), 128<<10)
	return repo, workspace
}

// A workspace removal that dies halfway used to leave a half-deleted directory that no later pass
// could identify: its .git was already gone, so planning refused it and the bytes stayed forever.
// Renaming the exact pinned inode into coop's own control directory FIRST makes the remains
// unambiguous garbage, and makes finishing the delete a directory listing instead of a judgement.
func TestStageWorkspaceDiscardMovesTheExactInodeOutOfTheForkRoot(t *testing.T) {
	repo, workspace := stagedFixture(t, "session-1")
	handle, info, err := Pin(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	staged, err := StageWorkspaceDiscardLocked(repo, "session-1", info)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(workspace); !os.IsNotExist(err) {
		t.Fatalf("the fork path still exists after staging: %v", err)
	}
	if !strings.HasPrefix(staged, StagedDiscardDir(repo)+string(filepath.Separator)) {
		t.Fatalf("staged path %q is not inside the control directory %q", staged, StagedDiscardDir(repo))
	}
	if _, err := os.Lstat(filepath.Join(staged, "payload.bin")); err != nil {
		t.Fatalf("staging lost the bytes it was supposed to keep until purge: %v", err)
	}
	pending, err := StagedDiscards(repo, "session-1")
	if err != nil || len(pending) != 1 || pending[0] != staged {
		t.Fatalf("StagedDiscards = %v, %v, want [%s]", pending, err, staged)
	}
}

// The receipt is the absence of the bytes, not the return of RemoveAll.
func TestPurgeStagedDiscardsIsIdempotentAndScopedToItsFork(t *testing.T) {
	repo, workspace := stagedFixture(t, "session-1")
	handle, info, err := Pin(workspace)
	if err != nil {
		t.Fatal(err)
	}
	stagedOne, err := StageWorkspaceDiscardLocked(repo, "session-1", info)
	_ = handle.Close()
	if err != nil {
		t.Fatal(err)
	}
	otherWorkspace := Workspace(repo, "session-2")
	writeBytes(t, filepath.Join(otherWorkspace, "payload.bin"), 128<<10)
	otherHandle, otherInfo, err := Pin(otherWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	stagedTwo, err := StageWorkspaceDiscardLocked(repo, "session-2", otherInfo)
	_ = otherHandle.Close()
	if err != nil {
		t.Fatal(err)
	}

	removed, err := PurgeStagedDiscards(repo, "session-1")
	if err != nil || removed != 1 {
		t.Fatalf("PurgeStagedDiscards(session-1) = %d, %v, want 1, nil", removed, err)
	}
	if _, err := os.Lstat(stagedOne); !os.IsNotExist(err) {
		t.Fatalf("purged staging still exists: %v", err)
	}
	if _, err := os.Lstat(stagedTwo); err != nil {
		t.Fatalf("purging one fork removed another fork's staged bytes: %v", err)
	}
	if removed, err := PurgeStagedDiscards(repo, "session-1"); err != nil || removed != 0 {
		t.Fatalf("replayed purge = %d, %v, want 0, nil", removed, err)
	}
	if removed, err := PurgeStagedDiscards(repo, ""); err != nil || removed != 1 {
		t.Fatalf("PurgeStagedDiscards(all) = %d, %v, want 1, nil", removed, err)
	}
	if pending, err := StagedDiscards(repo, ""); err != nil || len(pending) != 0 {
		t.Fatalf("StagedDiscards after purging everything = %v, %v", pending, err)
	}
}

// A delete interrupted after the rename but before the tree was gone must finish on the next pass
// without needing anything the crash took with it.
func TestPurgeStagedDiscardsResumesAPartiallyRemovedTree(t *testing.T) {
	repo, workspace := stagedFixture(t, "session-1")
	writeBytes(t, filepath.Join(workspace, "deep", "nested.bin"), 64<<10)
	handle, info, err := Pin(workspace)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := StageWorkspaceDiscardLocked(repo, "session-1", info)
	_ = handle.Close()
	if err != nil {
		t.Fatal(err)
	}
	// The crash: one file removed, the rest of the tree still on disk.
	if err := os.Remove(filepath.Join(staged, "payload.bin")); err != nil {
		t.Fatal(err)
	}

	removed, err := PurgeStagedDiscards(repo, "")
	if err != nil || removed != 1 {
		t.Fatalf("resumed purge = %d, %v, want 1, nil", removed, err)
	}
	if _, err := os.Lstat(staged); !os.IsNotExist(err) {
		t.Fatalf("resumed purge left the tree behind: %v", err)
	}
}

// Staging is a destructive move, so it fences on the same proof discard does: the pinned inode.
func TestStageWorkspaceDiscardRefusesAReusedPathAndAMissingOne(t *testing.T) {
	repo, workspace := stagedFixture(t, "session-1")
	handle, info, err := Pin(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := os.RemoveAll(workspace); err != nil {
		t.Fatal(err)
	}
	writeBytes(t, filepath.Join(workspace, "replacement.bin"), 16<<10)

	if _, err := StageWorkspaceDiscardLocked(repo, "session-1", info); err == nil {
		t.Fatal("staging accepted a workspace that was replaced under the same path")
	}
	if _, err := os.Lstat(filepath.Join(workspace, "replacement.bin")); err != nil {
		t.Fatalf("a refused staging moved the replacement anyway: %v", err)
	}
	if _, err := StageWorkspaceDiscardLocked(repo, "absent", info); err == nil {
		t.Fatal("staging accepted a missing workspace")
	}
	if _, err := StageWorkspaceDiscardLocked(repo, "bad..name", info); err == nil {
		t.Fatal("staging accepted an invalid fork name")
	}
}

// Anything inside the private control directory is coop's own garbage, but a hand-made entry that
// does not carry the staging shape is still somebody's data: report it, never delete it.
func TestStagedDiscardsRefusesToClaimForeignControlEntries(t *testing.T) {
	repo, workspace := stagedFixture(t, "session-1")
	handle, info, err := Pin(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StageWorkspaceDiscardLocked(repo, "session-1", info); err != nil {
		t.Fatal(err)
	}
	_ = handle.Close()
	foreign := filepath.Join(StagedDiscardDir(repo), "operator-copy")
	writeBytes(t, filepath.Join(foreign, "keep.bin"), 8<<10)

	removed, err := PurgeStagedDiscards(repo, "")
	if err != nil || removed != 1 {
		t.Fatalf("purge = %d, %v, want only the staged fork removed", removed, err)
	}
	if _, err := os.Lstat(filepath.Join(foreign, "keep.bin")); err != nil {
		t.Fatalf("purge deleted an entry it could not prove it owned: %v", err)
	}
}
