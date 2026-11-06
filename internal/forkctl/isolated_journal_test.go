package forkctl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

// isolatedRecords lists what isolated forks keep in the fork records folder: candidates, previews
// and the record of a last publication.
func isolatedRecords(t *testing.T, repo string) []string {
	t.Helper()
	entries, err := os.ReadDir(forkspace.StateDir(repo))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".isolated-") {
			found = append(found, entry.Name())
		}
	}
	return found
}

func isolatedHead(t *testing.T, repo string) string {
	t.Helper()
	head, err := observed(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// A merge the parent alone can refuse changed nothing, so it leaves nothing to finish: no journal, no
// kept copy, and the next merge asks again and runs the checks again. Before, the refusal came after
// the journal was written, so the fork showed as merging and the next merge finished it unasked.
func TestIsolatedRefusalBeforeAnyChangeLeavesNoJournalAndTheNextMergeAsksAgain(t *testing.T) {
	for _, tc := range []struct {
		name       string
		duringAsk  bool // the file arrives after the first question, not before the merge
		asks, runs int  // what the refused merge spent
	}{
		{"file in the way before the question", false, 0, 0},
		{"file in the way after the question", true, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, ws, identity, c := isolatedTestFork(t)
			isolatedTestCommit(t, ws, "base.txt", "from the fork\n")
			inTheWay := filepath.Join(repo, "base.txt")
			planted := !tc.duringAsk
			if planted {
				writeTaskFile(t, inTheWay, "untracked in the parent\n")
			}
			asks, runs := 0, 0
			c.gateOK = func(_, _, _ string) bool { runs++; return true }
			ask := func(isolatedCandidate) bool {
				asks++
				if !planted {
					planted = true
					writeTaskFile(t, inTheWay, "untracked in the parent\n")
				}
				return true
			}
			before, err := captureIsolatedParent(repo)
			if err != nil {
				t.Fatal(err)
			}

			result, err := c.mergeIsolatedLocked(repo, "fixture-image", identity.Name, identity, false, false, ask)
			if err == nil || result.landed || !strings.Contains(err.Error(), `"base.txt"`) {
				t.Fatalf("a file in the way did not refuse the merge: %+v, %v", result, err)
			}
			if asks != tc.asks || runs != tc.runs {
				t.Fatalf("the refused merge asked %d times and ran the checks %d times, want %d and %d", asks, runs, tc.asks, tc.runs)
			}
			if pending, err := ForkHasPendingLand(repo, identity); err != nil || pending {
				t.Fatalf("the refusal left a journal, so the fork would show as merging: %v, %v", pending, err)
			}
			if left := isolatedRecords(t, repo); len(left) != 0 {
				t.Fatalf("the refusal kept private copies: %v", left)
			}
			if got, err := os.ReadFile(inTheWay); err != nil || string(got) != "untracked in the parent\n" {
				t.Fatalf("the refusal touched the file in the way: %q, %v", got, err)
			}
			if after, err := captureIsolatedParent(repo); err != nil || !sameIsolatedParent(before, after) {
				t.Fatalf("the refusal changed the parent: %v", err)
			}

			if err := os.Remove(inTheWay); err != nil {
				t.Fatal(err)
			}
			asks, runs = 0, 0
			result, err = c.mergeIsolatedLocked(repo, "fixture-image", identity.Name, identity, false, false, ask)
			defer result.approval.close()
			if err != nil || !result.landed {
				t.Fatalf("the merge after the file moved: %+v, %v", result, err)
			}
			if asks != 1 || runs != 1 {
				t.Fatalf("the next merge asked %d times and ran the checks %d times, want once each", asks, runs)
			}
			if got, err := os.ReadFile(inTheWay); err != nil || string(got) != "from the fork\n" {
				t.Fatalf("the fork's file did not land: %q, %v", got, err)
			}
		})
	}
}

// A publication that really began keeps its journal, so the next merge finishes it, and says what it
// is finishing because it asks nothing.
func TestIsolatedInterruptedPublicationStillFinishesFromItsJournal(t *testing.T) {
	repo, ws, identity, c := isolatedTestFork(t)
	isolatedTestCommit(t, ws, "new.txt", "published\n")
	c.afterLandFastForward = func() error { return errors.New("injected interruption after the branch moved") }
	result, err := c.mergeOne(repo, "", identity.Name, false)
	if err == nil || !result.landed {
		t.Fatalf("the interruption did not follow the branch move: %+v, %v", result, err)
	}
	if pending, err := ForkHasPendingLand(repo, identity); err != nil || !pending {
		t.Fatalf("an interrupted publication kept no journal: %v, %v", pending, err)
	}
	c.afterLandFastForward = nil
	var code int
	notes := captureStderr(t, func() {
		code, err = c.ForkMerge([]string{identity.Name}) // no --yes: finishing a journal asks nothing
	})
	if err != nil || code != 0 {
		t.Fatalf("finishing from the journal: %d, %v", code, err)
	}
	if want := "Finishing the interrupted publication of fork isolated into main."; !strings.Contains(notes, want) {
		t.Fatalf("the merge did not say what it was finishing, want %q in:\n%s", want, notes)
	}
	if pending, err := ForkHasPendingLand(repo, identity); err != nil || pending {
		t.Fatalf("the finished publication kept its journal: %v, %v", pending, err)
	}
}

// Deleting a fork takes what only it could use: its log, the record of its last publication and the
// private copies its reviews kept. Another fork's records, and a fork that never had a generation,
// are none of its business.
func TestForkDeletionRemovesItsPreviewsSourceRecordAndLog(t *testing.T) {
	repo, ws, identity, c := isolatedTestFork(t)
	isolatedTestCommit(t, ws, "kept.txt", "kept\n")
	if _, err := forkspace.SetupIsolatedContext(t.Context(), repo, "bystander", isolatedHead(t, repo)); err != nil {
		t.Fatal(err)
	}
	other, err := forkspace.EnsureIsolatedGenerationLocked(repo, "bystander")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := forkspace.Setup(repo, "plain"); err != nil {
		t.Fatal(err)
	}
	has := func(kind string, fork forkspace.Identity) bool {
		for _, entry := range isolatedRecords(t, repo) {
			if strings.HasPrefix(entry, forkspace.IsolatedRecordPrefix(kind, fork)) {
				return true
			}
		}
		return false
	}
	// Reviews keep their copies, as `review --open` and a review command do, for both isolated forks.
	for _, fork := range []forkspace.Identity{identity, other} {
		candidate, err := captureIsolatedCandidate(repo, fork.Name, fork, false, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := candidate.committedPreview(repo); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{identity.Name, other.Name, "plain"} {
		file, err := forkspace.OpenLog(repo, name)
		if err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
	}
	result, err := c.mergeOne(repo, "", identity.Name, false) // keeps the fork and records its source
	if err != nil || !result.landed {
		t.Fatalf("publication: %+v, %v", result, err)
	}
	result.approval.close()
	for _, kind := range []string{"candidate", "preview", "source"} {
		if !has(kind, identity) {
			t.Fatalf("the fixture kept no %s for the fork: %v", kind, isolatedRecords(t, repo))
		}
	}

	if code, err := c.ForkRm([]string{"plain", "--yes"}); code != 0 || err != nil {
		t.Fatalf("rm plain: %d, %v", code, err)
	}
	if !has("candidate", identity) || !has("preview", identity) || !has("source", identity) || !has("candidate", other) {
		t.Fatalf("deleting a fork with no generation removed isolated records: %v", isolatedRecords(t, repo))
	}
	if _, err := os.Lstat(forkspace.LogPath(repo, "plain")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("deleting an ordinary fork left its log: %v", err)
	}
	if code, err := c.ForkRm([]string{identity.Name, "--yes"}); code != 0 || err != nil {
		t.Fatalf("rm %s: %d, %v", identity.Name, code, err)
	}
	for _, kind := range []string{"candidate", "preview", "source"} {
		if has(kind, identity) {
			t.Errorf("deleting the fork left its %s: %v", kind, isolatedRecords(t, repo))
		}
	}
	if _, err := os.Lstat(forkspace.LogPath(repo, identity.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("deleting the fork left its log: %v", err)
	}
	if !has("candidate", other) || !has("preview", other) {
		t.Errorf("deleting one fork removed another fork's private copies: %v", isolatedRecords(t, repo))
	}
	if _, err := os.Lstat(forkspace.LogPath(repo, other.Name)); err != nil {
		t.Errorf("deleting one fork removed another fork's log: %v", err)
	}
}

// `review --open` says where its copy is and how to remove it before the fork is deleted.
func TestIsolatedReviewOpenSaysHowTheKeptCopyGoesAway(t *testing.T) {
	repo, ws, identity, c := isolatedTestFork(t)
	isolatedTestCommit(t, ws, "kept.txt", "kept\n")
	capture := filepath.Join(t.TempDir(), "preview-path")
	tool := filepath.Join(t.TempDir(), "editor")
	writeTaskFile(t, tool, "#!/bin/sh\nprintf '%s' \"$1\" > '"+capture+"'\n")
	if err := os.Chmod(tool, 0o700); err != nil {
		t.Fatal(err)
	}
	c.cfg.Editor = tool
	notes := captureStderr(t, func() {
		if code, err := c.forkReviewIsolated(repo, ws, identity.Name, identity, false, false, true, false); code != 0 || err != nil {
			t.Fatalf("review --open: %d, %v", code, err)
		}
	})
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	preview := string(data)
	if want := "Deleting the fork removes the copy. To remove it sooner, run: rm -r " + preview; !strings.Contains(notes, want) {
		t.Errorf("review --open did not say how the copy goes away, want %q in:\n%s", want, notes)
	}
	if !strings.HasPrefix(filepath.Base(preview), forkspace.IsolatedRecordPrefix("candidate", identity)) {
		t.Errorf("the kept copy %s is not named for its fork, so deleting the fork could not find it", preview)
	}
}
