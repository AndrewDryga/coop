package forkspace

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const stagedDiscardCount = 4096

var stagedDiscardSuffix = regexp.MustCompile(`^[0-9a-f]{16}$`)

// StagedDiscardDir is where a workspace waits between "proven removable" and "gone".
//
// Deleting a workspace in place is not resumable: a crash partway through leaves a directory that
// still occupies the fork's name but can no longer be inspected — no .git, no branch, no HEAD — so
// every later plan refuses it and its bytes are stranded for good. Renaming the proven inode into
// this owner-private directory first is a single atomic operation on the same filesystem. After it
// the fork name is free, and what remains is unambiguously coop's own garbage that any later pass
// can finish removing without re-proving anything.
//
// It is a sibling of the fork workspaces rather than a child of .coop so that storage accounting
// can measure control state and mid-removal garbage as two separate trees instead of subtracting
// one from the other.
func StagedDiscardDir(repo string) string { return filepath.Join(Home(repo), ".discarding") }

func ensureStagedDiscardDir(repo string) error {
	if err := os.MkdirAll(Home(repo), 0o755); err != nil {
		return err
	}
	return ensurePrivateStateDir(StagedDiscardDir(repo))
}

func stagedDiscardName(name string) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return name + "." + hex.EncodeToString(raw[:]), nil
}

// stagedDiscardFork recovers the fork name from a staged entry, and reports false for anything
// that does not carry the exact staging shape — an operator's own copy parked in the control
// directory is data, not garbage.
func stagedDiscardFork(entry string) (string, bool) {
	cut := strings.LastIndex(entry, ".")
	if cut <= 0 || !stagedDiscardSuffix.MatchString(entry[cut+1:]) {
		return "", false
	}
	name := entry[:cut]
	if !ValidExistingName(name) {
		return "", false
	}
	return name, true
}

// StageWorkspaceDiscardLocked moves the exact pinned workspace inode into the staging directory.
// The caller must hold LockState(repo, name) and must have proven the plan; pinned is the
// os.FileInfo from Pin, so a workspace unlinked and recreated under the same name between the
// proof and this move is refused instead of destroyed.
func StageWorkspaceDiscardLocked(repo, name string, pinned os.FileInfo) (string, error) {
	if !ValidExistingName(name) {
		return "", fmt.Errorf("invalid fork name %q", name)
	}
	if pinned == nil {
		return "", errors.New("staging a workspace discard requires its pinned identity")
	}
	workspace := Workspace(repo, name)
	if !SamePinned(workspace, pinned) {
		return "", fmt.Errorf("refusing to stage %q: the workspace is missing or was replaced", workspace)
	}
	if err := ensureStagedDiscardDir(repo); err != nil {
		return "", err
	}
	staged, err := stagedDiscardName(name)
	if err != nil {
		return "", err
	}
	target := filepath.Join(StagedDiscardDir(repo), staged)
	if err := renameNoReplace(workspace, target); err != nil {
		return "", fmt.Errorf("stage fork workspace %q for discard: %w", workspace, err)
	}
	return target, nil
}

// StagedDiscards lists the staged trees still holding storage. name scopes the answer to one fork;
// "" reports every one.
func StagedDiscards(repo, name string) ([]string, error) {
	entries, err := os.ReadDir(StagedDiscardDir(repo))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read staged discard directory: %w", err)
	}
	if len(entries) > stagedDiscardCount {
		return nil, fmt.Errorf("staged discard directory exceeds %d entries", stagedDiscardCount)
	}
	var staged []string
	for _, entry := range entries {
		fork, ok := stagedDiscardFork(entry.Name())
		if !ok || !entry.IsDir() || name != "" && fork != name {
			continue
		}
		staged = append(staged, filepath.Join(StagedDiscardDir(repo), entry.Name()))
	}
	sort.Strings(staged)
	return staged, nil
}

// PurgeStagedDiscards finishes the removals staging began and returns how many trees it retired.
// It is idempotent, resumable after an interrupted delete, and it confirms the tree is physically
// gone before counting it — a caller may only report a discard receipt once this says so.
func PurgeStagedDiscards(repo, name string) (int, error) {
	staged, err := StagedDiscards(repo, name)
	if err != nil {
		return 0, err
	}
	removed := 0
	var problems []error
	for _, path := range staged {
		if err := removeStagedDiscard(path); err != nil {
			problems = append(problems, err)
			continue
		}
		removed++
	}
	if len(problems) > 0 {
		return removed, errors.Join(problems...)
	}
	// Both tidy-ups mirror Destroy. An empty staging directory would keep telling an operator
	// something is mid-removal, and it would keep the fork root alive past the last fork it held.
	if entries, err := os.ReadDir(StagedDiscardDir(repo)); err == nil && len(entries) == 0 &&
		os.Remove(StagedDiscardDir(repo)) == nil {
		if entries, err := os.ReadDir(Home(repo)); err == nil && len(entries) == 0 {
			_ = os.Remove(Home(repo))
		}
	}
	return removed, nil
}

func removeStagedDiscard(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("staged discard %q is not a real directory", path)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove staged discard %q: %w", path, err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("staged discard %q remains after removal", path)
		}
		return fmt.Errorf("verify staged discard %q removal: %w", path, err)
	}
	return nil
}
