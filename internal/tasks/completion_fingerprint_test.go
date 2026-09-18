package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// recordedFormatZeroDigest is the tree digest every format-0 fingerprint was written with, copied
// verbatim from before the device left it (completionTreeMetadataDigest at 10da44cc). It is the
// oracle for the re-derivation: a window or review opened by an older build compares only if
// legacyCompletionTreeDigest reproduces this byte for byte.
func recordedFormatZeroDigest(taskDir string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(taskDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == taskDir {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("task completion child %q has unsupported file metadata", path)
		}
		rel, err := filepath.Rel(taskDir, path)
		if err != nil {
			return err
		}
		sec, nsec := statChangeTime(stat)
		_, err = fmt.Fprintf(hash, "%d:%s\x00%d:%d:%d:%d:%d:%d\x00", len(rel), rel, uint32(info.Mode()), info.Size(), uint64(stat.Dev), uint64(stat.Ino), sec, nsec)
		return err
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func TestLegacyCompletionTreeDigestReproducesWhatOlderBuildsRecorded(t *testing.T) {
	dir := t.TempDir()
	writeTaskFile(t, filepath.Join(dir, "task.md"), "# archived\n")
	writeTaskFile(t, filepath.Join(dir, "artifacts", "proof.md"), "evidence\n")
	if err := os.Symlink("task.md", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	want, err := recordedFormatZeroDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	walk, err := walkCompletionTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := legacyCompletionTreeDigest(walk, uint64(info.Sys().(*syscall.Stat_t).Dev)); got != want {
		t.Fatalf("re-derived format-0 digest %s, older builds recorded %s", got, want)
	}
	if completionTreeDigest(walk) == want {
		t.Fatal("the current tree digest still equals the device-hashing one")
	}
}

// A fingerprint recorded before the device left the tree compares with a live one in either order,
// and still tells a changed archive apart — whichever operand a future caller happens to put first.
func TestFormatZeroFingerprintMatchesInEitherOrder(t *testing.T) {
	root := t.TempDir()
	task := taskForLease(t, root, StateDone, "archived")
	live, err := CompletionFingerprintFor(root, task)
	if err != nil {
		t.Fatal(err)
	}
	recorded := live
	recorded.walk = nil
	recorded.Device++ // recorded before a reboot renumbered the volume
	recorded.TreeFormat, recorded.Tree = 0, legacyCompletionTreeDigest(live.walk, recorded.Device)
	if !recorded.Matches(live) || !live.Matches(recorded) {
		t.Fatalf("format-0 fingerprint did not match its own archive: recorded-first %v, live-first %v",
			recorded.Matches(live), live.Matches(recorded))
	}
	writeTaskFile(t, filepath.Join(task.Dir, "log.md"), "changed\n")
	changed, err := CompletionFingerprintFor(root, task)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.Matches(changed) || changed.Matches(recorded) {
		t.Fatal("a changed archive matched a format-0 fingerprint")
	}
}
