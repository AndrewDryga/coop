package loop

import (
	"reflect"
	"testing"

	"github.com/AndrewDryga/coop/internal/loopcfg"
	"github.com/AndrewDryga/coop/internal/tasks"
)

func TestPendingReviewHonorsConfiguredModelsWithoutChangingAcceptance(t *testing.T) {
	plan := tasks.PendingReviewPlan{
		Signoff:       tasks.PendingReviewStage{Targets: []string{"codex:old"}, Prompt: "saved review", Writes: "tasks"},
		SignoffRounds: 3,
		VerifyEnabled: true,
		Verify:        tasks.PendingReviewStage{Targets: []string{"codex:old"}, Prompt: "saved verification", Writes: "tasks"},
	}
	for _, configured := range []bool{false, true} {
		lc := loopcfg.Config{
			Signoff: loopcfg.Signoff{Prompt: "different review", Writes: loopcfg.ReviewWritesRepo, Rounds: 20},
			Verify:  loopcfg.Verify{Enabled: false, Prompt: "different verification", Writes: loopcfg.ReviewWritesRepo},
		}
		wantModels := []string{"codex:old"}
		if configured {
			wantModels = []string{"claude:claude-opus-5/high"}
			lc.Signoff.Agent = []string{"claude:claude-opus-5/high"}
			lc.Verify.Agent = []string{"claude:claude-opus-5/high"}
		}
		applyStoredReviewPlan(&lc, plan)
		if !reflect.DeepEqual(lc.Signoff.Agent, wantModels) || !reflect.DeepEqual(lc.Verify.Agent, wantModels) {
			t.Fatalf("configured=%t: models = %v / %v, want %v", configured, lc.Signoff.Agent, lc.Verify.Agent, wantModels)
		}
		if lc.Signoff.Prompt != plan.Signoff.Prompt || lc.Signoff.Writes != loopcfg.ReviewWritesTasks || lc.Signoff.Rounds != 3 ||
			!lc.Verify.Enabled || lc.Verify.Prompt != plan.Verify.Prompt || lc.Verify.Writes != loopcfg.ReviewWritesTasks {
			t.Fatalf("configured=%t: saved acceptance changed: %+v", configured, lc)
		}
		lc.Signoff.Agent[0] = "mutated"
		lc.Verify.Agent[0] = "mutated"
		if plan.Signoff.Targets[0] != "codex:old" || plan.Verify.Targets[0] != "codex:old" {
			t.Fatal("runtime selection mutated the original review plan")
		}
	}
}

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
