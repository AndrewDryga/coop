package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// CheckoutFingerprint answers the question a no-change completion actually turns on: did anything
// about this checkout's CONTENT change?
//
// `git status --porcelain` cannot answer it. Its lines name a path and a state, so a worker that
// starts a task with supported pre-existing work — a modified file, something staged — can rewrite
// those bytes, leave the line reading `M internal/loop/loop.go` exactly as it was, and claim it
// changed nothing. Both the pre-move check and the post-exit one compared those labels, so such an
// edit reached completion without ever being reviewed as a change.
//
// The fingerprint covers the three places content can hide:
//
//   - the status lines themselves, so a new path or a changed state still fails;
//   - `git diff` and `git diff --cached`, which carry the actual bytes of every tracked change,
//     unstaged and staged;
//   - untracked files, which no diff sees.
//
// The diffs are taken with `--binary`, or a changed image or archive would render as the content-free
// "Binary files differ" and slip through; with `--no-textconv` and `--no-ext-diff`, so a
// repository-defined driver can neither mask the bytes nor execute on the host; and with
// `--ignore-submodules=dirty`, so this never descends into an agent-writable child repository.
func CheckoutFingerprint(repo string) (string, error) {
	sum := sha256.New()
	section := func(name, body string) {
		fmt.Fprintf(sum, "\x00%s\x00%s", name, body)
	}
	status, err := gitOutErr(repo, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return "", fmt.Errorf("inspect checkout state: %w", err)
	}
	section("status", status)
	for _, diff := range []struct {
		name string
		args []string
	}{
		{"worktree", []string{"diff", "--binary", "--no-textconv", "--no-ext-diff", "--ignore-submodules=dirty"}},
		{"index", []string{"diff", "--cached", "--binary", "--no-textconv", "--no-ext-diff", "--ignore-submodules=dirty"}},
	} {
		body, err := gitOutErr(repo, diff.args...)
		if err != nil {
			return "", fmt.Errorf("inspect %s content: %w", diff.name, err)
		}
		section(diff.name, body)
	}
	untracked, err := untrackedFingerprint(repo)
	if err != nil {
		return "", err
	}
	section("untracked", untracked)
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// untrackedFingerprint digests every untracked file's content, which the diffs above never see.
//
// It reads them itself rather than asking Git to hash them, for one reason: an untracked SYMLINK
// must be fingerprinted by where it points, never by what it points AT. Following one would read a
// file outside the repository — the confinement this checkout's isolation rests on — and would also
// make an unrelated change on the host look like a change inside the task.
func untrackedFingerprint(repo string) (string, error) {
	listing, err := gitOutErr(repo, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", fmt.Errorf("list untracked content: %w", err)
	}
	sum := sha256.New()
	for _, rel := range strings.Split(listing, "\x00") {
		if rel == "" {
			continue
		}
		path := filepath.Join(repo, filepath.FromSlash(rel))
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // raced away between the listing and here; the status line already covers it
			}
			return "", fmt.Errorf("inspect untracked %s: %w", rel, err)
		}
		fmt.Fprintf(sum, "\x00%s\x00%d\x00", rel, info.Mode()&os.ModeType)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return "", fmt.Errorf("read untracked link %s: %w", rel, err)
			}
			io.WriteString(sum, target)
		case info.Mode().IsRegular():
			file, err := os.Open(path)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return "", fmt.Errorf("read untracked %s: %w", rel, err)
			}
			_, copyErr := io.Copy(sum, file)
			file.Close()
			if copyErr != nil {
				return "", fmt.Errorf("read untracked %s: %w", rel, copyErr)
			}
		}
		// Anything else (a device, a socket) has no content a task could smuggle code through, and
		// its presence is already in the status line.
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
