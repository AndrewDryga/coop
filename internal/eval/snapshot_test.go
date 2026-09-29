package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The snapshot is what gets graded, so it must be faithful: the candidate's files AND its git
// history travel (a verifier legitimately asks "did it commit?"), and the original is untouched.
func TestSnapshotWorkspaceKeepsGitAndLeavesTheOriginal(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "main.go"), "package main\n")
	if err := os.MkdirAll(filepath.Join(src, ".git", "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(src, ".git", "HEAD"), "ref: refs/heads/main\n")
	mustWrite(t, filepath.Join(src, "sub", "deep.txt"), "deep\n")

	snap, err := SnapshotWorkspace(context.Background(), src, filepath.Join(t.TempDir(), "snap"))
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"main.go", ".git/HEAD", "sub/deep.txt"} {
		if _, err := os.Stat(filepath.Join(snap.Dir, rel)); err != nil {
			t.Errorf("%s did not travel into the snapshot: %v", rel, err)
		}
	}
	if len(snap.Skipped) != 0 {
		t.Errorf("nothing should have been skipped: %v", snap.Skipped)
	}

	// Grading writes into the snapshot; the recorded workspace must not change.
	mustWrite(t, filepath.Join(snap.Dir, "grader-wrote.txt"), "x\n")
	if _, err := os.Stat(filepath.Join(src, "grader-wrote.txt")); !os.IsNotExist(err) {
		t.Error("a write into the snapshot reached the original workspace")
	}
}

// A candidate leaves an escaping symlink behind. The snapshot must neither follow it (the grader
// would read the host) nor abort (that would turn a candidate's mess into a harness failure) — it
// is skipped and recorded.
func TestSnapshotWorkspaceSkipsEscapingSymlinks(t *testing.T) {
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), "host secret\n")
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "ok.txt"), "fine\n")
	mustWrite(t, filepath.Join(src, "inside.txt"), "inside\n")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(src, "abs.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink("../"+filepath.Base(outside)+"/secret.txt", filepath.Join(src, "rel.txt")); err != nil {
		t.Fatal(err)
	}
	// A link that stays inside is legitimate work and must be preserved.
	if err := os.Symlink("inside.txt", filepath.Join(src, "local.txt")); err != nil {
		t.Fatal(err)
	}

	snap, err := SnapshotWorkspace(context.Background(), src, filepath.Join(t.TempDir(), "snap"))
	if err != nil {
		t.Fatalf("an escaping symlink must not fail the snapshot: %v", err)
	}
	for _, rel := range []string{"abs.txt", "rel.txt"} {
		if _, err := os.Lstat(filepath.Join(snap.Dir, rel)); !os.IsNotExist(err) {
			t.Errorf("escaping symlink %s was carried into the snapshot", rel)
		}
		if !contains(snap.Skipped, rel) {
			t.Errorf("escaping symlink %s was not recorded as skipped: %v", rel, snap.Skipped)
		}
	}
	if fi, err := os.Lstat(filepath.Join(snap.Dir, "local.txt")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("an in-workspace symlink should be preserved: %v", err)
	}
	// And the host file is nowhere in the snapshot.
	data, err := os.ReadFile(filepath.Join(snap.Dir, "ok.txt"))
	if err != nil || string(data) != "fine\n" {
		t.Errorf("ordinary files must still travel: %q %v", data, err)
	}
}

// An unreadable workspace is a harness error — grading something that cannot be read is never a
// candidate failure.
func TestSnapshotWorkspaceRefusesAMissingSource(t *testing.T) {
	if _, err := SnapshotWorkspace(context.Background(), filepath.Join(t.TempDir(), "gone"), filepath.Join(t.TempDir(), "snap")); err == nil {
		t.Error("snapshotting a missing workspace returned no error")
	}
}

func TestSnapshotCancellationIsNotASkippedEntry(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "work.go"), "package work\n")
	dst := filepath.Join(t.TempDir(), "snap")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snap, err := SnapshotWorkspace(ctx, src, dst)
	if err != context.Canceled || snap.Dir != "" || len(snap.Skipped) != 0 {
		t.Fatalf("canceled snapshot became a partial result: %+v, %v", snap, err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("canceled snapshot left a grading tree: %v", err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TreeSignature answers one question — did anything happen here? — and must ignore the .git Coop
// created itself, or every trial would look like work was done.
func TestTreeSignatureDetectsWorkAndIgnoresGit(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "one\n")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")

	base, err := TreeSignature(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := TreeSignature(context.Background(), dir); again != base {
		t.Error("the signature is not stable for an unchanged tree")
	}
	// Coop's own git activity is not the candidate's work.
	mustWrite(t, filepath.Join(dir, ".git", "COMMIT_EDITMSG"), "anything\n")
	if changed, _ := TreeSignature(context.Background(), dir); changed != base {
		t.Error("a change inside .git counted as candidate work")
	}
	// A new file is work.
	mustWrite(t, filepath.Join(dir, "answer.txt"), "hello\n")
	if changed, _ := TreeSignature(context.Background(), dir); changed == base {
		t.Error("creating a file did not change the signature")
	}
	// So is editing one to a different size.
	base2, _ := TreeSignature(context.Background(), dir)
	mustWrite(t, filepath.Join(dir, "a.txt"), "one much longer line\n")
	if changed, _ := TreeSignature(context.Background(), dir); changed == base2 {
		t.Error("editing a file did not change the signature")
	}
}
