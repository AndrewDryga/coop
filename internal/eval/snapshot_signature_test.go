package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTreeSignatureDetectsSameSizeAndModeChanges(t *testing.T) {
	for _, edit := range []string{"same-size content", "executable bit"} {
		t.Run(edit, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "run.sh")
			mustWrite(t, path, "exit 1\n")
			before, err := TreeSignature(dir)
			if err != nil {
				t.Fatal(err)
			}
			switch edit {
			case "same-size content":
				mustWrite(t, path, "exit 0\n")
			case "executable bit":
				if err := os.Chmod(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			after, err := TreeSignature(dir)
			if err != nil || after == before {
				t.Fatalf("%s was not detected: same=%v err=%v", edit, after == before, err)
			}
		})
	}
}

func TestTreeSignatureTracksLinksWithoutFollowingThem(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "outside.txt")
	mustWrite(t, target, "before")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	before, err := TreeSignature(dir)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, target, "changed external content")
	if same, err := TreeSignature(dir); err != nil || same != before {
		t.Fatalf("followed external link: changed=%v err=%v", same != before, err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", link); err != nil {
		t.Fatal(err)
	}
	if after, err := TreeSignature(dir); err != nil || after == before {
		t.Fatalf("changed link was not detected: same=%v err=%v", after == before, err)
	}
}

func TestTreeSignatureIgnoresHarnessBookkeeping(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "answer.txt"), "answer")
	mustWrite(t, filepath.Join(dir, ".agent/tasks/task/state.md"), "before")
	before, err := TreeSignature(dir, ".agent/tasks")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, ".agent/tasks/task/state.md"), "after state")
	if after, err := TreeSignature(dir, ".agent/tasks"); err != nil || after != before {
		t.Fatalf("harness bookkeeping counted as work: changed=%v err=%v", after != before, err)
	}
}

func TestTreeSignatureRefusesUnavailableOrOversizedContent(t *testing.T) {
	t.Run("missing workspace", func(t *testing.T) {
		if signature, err := TreeSignature(filepath.Join(t.TempDir(), "missing")); err == nil || signature != "" {
			t.Fatalf("missing workspace treated as comparable: signature=%q err=%v", signature, err)
		}
	})
	t.Run("oversized file", func(t *testing.T) {
		dir := t.TempDir()
		file, err := os.Create(filepath.Join(dir, "large"))
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(SnapshotLimit + 1); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if signature, err := TreeSignature(dir); err == nil || signature != "" {
			t.Fatalf("oversized content treated as comparable: signature=%q err=%v", signature, err)
		}
	})
	t.Run("unreadable file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "unreadable")
		mustWrite(t, path, "content")
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
		if file, err := os.Open(path); err == nil {
			file.Close()
			t.Skip("current user can read mode-000 files")
		}
		if signature, err := TreeSignature(dir); err == nil || signature != "" {
			t.Fatalf("unreadable content treated as comparable: signature=%q err=%v", signature, err)
		}
	})
}
