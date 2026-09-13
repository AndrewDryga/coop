package loop

import (
	"testing"

	"github.com/AndrewDryga/coop/internal/tasks"
)

func TestPendingSignoffStartRoundPreservesPersistedProgress(t *testing.T) {
	for _, tc := range []struct {
		name    string
		records []tasks.PendingReviewRecord
		want    int
	}{
		{name: "new cohort", want: 1},
		{name: "interrupted started round", records: []tasks.PendingReviewRecord{{Phase: tasks.PendingReviewSignoff, Round: 2, RoundStarted: true}}, want: 2},
		{name: "reopened completed round", records: []tasks.PendingReviewRecord{{Phase: tasks.PendingReviewReopened, Round: 2}}, want: 3},
		{name: "highest round controls", records: []tasks.PendingReviewRecord{
			{Phase: tasks.PendingReviewSignoff, Round: 1, RoundStarted: true},
			{Phase: tasks.PendingReviewReopened, Round: 2},
		}, want: 3},
		{name: "verification only", records: []tasks.PendingReviewRecord{{Phase: tasks.PendingReviewVerify, Round: 2}}, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pendingSignoffStartRound(tasks.PendingReviewCohort{Subjects: tc.records}); got != tc.want {
				t.Fatalf("start round = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPendingReviewAdditionalImportsKeepCohortContextImmutable(t *testing.T) {
	cohort := tasks.PendingReviewCohort{Subjects: []tasks.PendingReviewRecord{
		{Task: tasks.TaskInstance{Ref: tasks.TaskRef{ID: "already-pending"}}},
	}}
	got := pendingReviewAdditionalImports(cohort, []string{"older-task", "already-pending", "older-task"})
	if len(got) != 1 || got[0] != "older-task" {
		t.Fatalf("additional imports = %v, want [older-task]", got)
	}
}
