package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/loopcfg"
)

func TestReviewRepoReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		writes   loopcfg.ReviewWrites
		readOnly bool
	}{
		{name: "default", readOnly: true},
		{name: "explicit tasks", writes: loopcfg.ReviewWritesTasks, readOnly: true},
		{name: "explicit repository", writes: loopcfg.ReviewWritesRepo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if readOnly := reviewRepoReadOnly(tc.writes); readOnly != tc.readOnly {
				t.Errorf("reviewRepoReadOnly(%q) = %v, want %v", tc.writes, readOnly, tc.readOnly)
			}
		})
	}
}

// TestReviewLadder: a review stage's ladder keeps each rung's PROVIDER, model, effort, and the
// fallback rungs — the fix for stepModel, which kept only (model, effort) off the first rung and
// dropped the provider, so a claude-led run's `codex:…` signoff resolved to `claude --model
// <a-codex-model>` and the cross-vendor reviewer was never actually run.
func TestReviewLadder(t *testing.T) {
	ladder, err := reviewLadder([]string{"codex:gpt-5.6-sol/xhigh", "claude:claude-fable-5/xhigh"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ladder) != 2 {
		t.Fatalf("both rungs must survive (the fallback too), got %d", len(ladder))
	}
	// Rung 0 keeps its provider — NOT discarded onto the work provider.
	if ladder[0].Provider != "codex" || ladder[0].Model != "gpt-5.6-sol" || ladder[0].Effort != "xhigh" {
		t.Errorf("rung 0 = %+v, want codex / gpt-5.6-sol / xhigh", ladder[0])
	}
	// Rung 1 (the fallback) survives with its own provider — stepModel dropped it entirely.
	if ladder[1].Provider != "claude" || ladder[1].Model != "claude-fable-5" {
		t.Errorf("rung 1 = %+v, want claude / claude-fable-5", ladder[1])
	}
	// An empty ladder yields no rungs — the caller falls back to the work rotation.
	if got, _ := reviewLadder(nil); len(got) != 0 {
		t.Errorf("empty ladder → no rungs, got %v", got)
	}
}

func TestReviewSubjectSnapshotsRejectLifecycleGenerationChange(t *testing.T) {
	root := t.TempDir()
	task := taskForLease(t, root, stateDone, "task-a")
	snapshots, err := snapshotReviewSubjects([]string{root}, []string{task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateReviewSubjects([]string{root}, snapshots); err != nil {
		t.Fatalf("unchanged review subject failed validation: %v", err)
	}

	oldDir := filepath.Join(t.TempDir(), task.ID)
	if err := os.Rename(task.Dir, oldDir); err != nil {
		t.Fatal(err)
	}
	taskForLease(t, root, stateDone, task.ID)
	if err := validateReviewSubjects([]string{root}, snapshots); err == nil ||
		!strings.Contains(err.Error(), "changed completion generation") {
		t.Fatalf("replacement review subject validation = %v, want generation change", err)
	}
}
