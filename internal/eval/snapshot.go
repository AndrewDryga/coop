package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Grading happens on a SNAPSHOT, never on the live workspace: the candidate's processes are stopped,
// the workspace is copied once, and the grader runs against that copy. So a verifier that builds,
// installs or rewrites files cannot alter what was recorded, and a second grading of the same trial
// sees exactly what the first did.
//
// The snapshot is deliberately different from fixture materialization in one way: it KEEPS `.git`.
// A fixture's history is the author's and must never travel, but a finished trial's history is the
// candidate's own work — a verifier legitimately asks "did it commit?", "is the tree clean?", "how
// many commits?" — so the repository is part of what gets graded.
//
// It is also bounded and non-following: a symlink the candidate created is reproduced only when it
// stays inside the workspace; one that escapes (absolute, or resolving out through any component) is
// SKIPPED and reported, never followed. A candidate cannot make the grader read the host by leaving
// a link behind, and cannot fail its own grading run by doing so either — the skip is recorded as a
// snapshot note, which is measurement information, not a quality verdict.

// SnapshotLimit bounds how many bytes a snapshot will copy. A candidate can create an arbitrarily
// large tree (a sparse file it truncated to 100 GB, a runaway log), and filling the host's disk must
// not be one of the things a trial can do. Exceeding it fails the trial as a HARNESS error naming
// the limit, never as a model failure.
const SnapshotLimit = 2 << 30 // 2 GiB

// Snapshot is the result of copying a finished workspace for grading.
type Snapshot struct {
	Dir string
	// Skipped names workspace-relative paths that could not be carried into the snapshot: an escaping
	// symlink, or a device/socket/fifo. Recorded so an incomplete snapshot is visible rather than
	// looking like the candidate simply produced nothing there.
	Skipped []string
}

// SnapshotWorkspace copies src into dst (which must not exist) for grading. It returns the snapshot
// with any skipped entries. An unreadable source is an error — grading something that cannot be read
// is a harness failure, not a candidate failure.
func SnapshotWorkspace(src, dst string) (Snapshot, error) {
	info, err := os.Stat(src)
	if err != nil {
		return Snapshot{}, fmt.Errorf("snapshot workspace %q: %w", src, err)
	}
	if !info.IsDir() {
		return Snapshot{}, fmt.Errorf("snapshot workspace %q: not a directory", src)
	}
	root, err := filepath.EvalSymlinks(src)
	if err != nil {
		return Snapshot{}, fmt.Errorf("snapshot workspace %q: %w", src, err)
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{Dir: dst}
	var copied int64
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An entry we cannot even read (a directory the candidate chmod'd to 000) is recorded as
			// unsnapshotted, not turned into a failed trial — the rest of the work is still gradable.
			if rel, relErr := filepath.Rel(root, path); relErr == nil {
				snap.Skipped = append(snap.Skipped, rel)
			}
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			snap.Skipped = append(snap.Skipped, rel)
			return nil
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			if err := snapshotSymlink(root, path, target); err != nil {
				snap.Skipped = append(snap.Skipped, rel)
			}
			return nil
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode().IsRegular():
			if copied += info.Size(); copied > SnapshotLimit {
				return fmt.Errorf("workspace exceeds the %d GiB snapshot limit at %q; grading a tree this large is refused", SnapshotLimit>>30, rel)
			}
			if err := copyFile(path, target, info.Mode().Perm()); err != nil {
				// Unreadable content is a gap in the snapshot, not a verdict.
				snap.Skipped = append(snap.Skipped, rel)
			}
			return nil
		default:
			snap.Skipped = append(snap.Skipped, rel) // device, socket, fifo — not gradable content
			return nil
		}
	})
	if err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// snapshotSymlink reproduces a link only when it resolves INSIDE the workspace, by the same physical
// resolution the kernel does. An escaping or dangling link returns an error, which the caller turns
// into a skip note rather than a failure.
func snapshotSymlink(root, path, target string) error {
	dest, err := os.Readlink(path)
	if err != nil {
		return err
	}
	if filepath.IsAbs(dest) {
		return fmt.Errorf("absolute symlink target %q", dest)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("symlink resolves outside the workspace")
	}
	return os.Symlink(dest, target)
}

// TreeSignature summarizes a workspace's content-bearing files — every regular file outside `.git`,
// by relative path and size — so two moments can be compared cheaply. It answers exactly one
// question: did anything happen here?
//
// That question matters because "the agent exited non-zero and changed NOTHING" is almost never
// evidence about the model. It is what a refused request, an expired login, a missing binary or an
// unsupported model looks like from outside, and grading an untouched workspace would record a
// confident FAIL for a trial where the model never got to work. `.git` is excluded because the
// initial commit is Coop's own and says nothing about the candidate.
func TreeSignature(dir string, ignore ...string) (string, error) {
	h := sha256.New()
	var entries []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // unreadable entries cannot be compared; they are the same on both sides
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return nil
		}
		if strings.EqualFold(d.Name(), ".git") && d.IsDir() {
			return filepath.SkipDir
		}
		// Harness bookkeeping is not the candidate's work: the queue it was handed, the preset being
		// evaluated, the loop's own telemetry. Counting them would make every loop trial look busy.
		if isIgnored(rel, ignore) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		entries = append(entries, fmt.Sprintf("%s:%d", rel, info.Size()))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(entries)
	for _, e := range entries {
		fmt.Fprintf(h, "%d:%s", len(e), e)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// isIgnored reports whether a workspace-relative path is one of the ignored roots, or inside one.
func isIgnored(rel string, ignore []string) bool {
	clean := filepath.ToSlash(filepath.Clean(rel))
	for _, ig := range ignore {
		ig = filepath.ToSlash(filepath.Clean(ig))
		if clean == ig || strings.HasPrefix(clean, ig+"/") {
			return true
		}
	}
	return false
}
