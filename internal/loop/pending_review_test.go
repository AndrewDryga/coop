package loop

import (
	"reflect"
	"strings"
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

// An accepted task sits in the review baseline. When the host sends it back to signoff because its
// commit changed after the review, the next round must review it again; a task that simply waits
// for its first signoff is not in the baseline and is left alone.
func TestRequeueChangedReviewsReturnsOnlyAcceptedTasks(t *testing.T) {
	subject := func(id string, phase tasks.PendingReviewPhase) tasks.PendingReviewRecord {
		return tasks.PendingReviewRecord{Task: tasks.TaskInstance{Ref: tasks.TaskRef{ID: id}}, Phase: phase}
	}
	baseline := map[string]string{"changed": "/done/changed", "verified": "/done/verified", "older": "/done/older"}
	cohort := tasks.PendingReviewCohort{Subjects: []tasks.PendingReviewRecord{
		subject("changed", tasks.PendingReviewSignoff),
		subject("verified", tasks.PendingReviewVerify),
		subject("fresh", tasks.PendingReviewSignoff),
	}}
	got := requeueChangedReviews(baseline, cohort)
	if !reflect.DeepEqual(got, []string{"changed"}) {
		t.Fatalf("requeued = %v, want [changed]", got)
	}
	if _, ok := baseline["changed"]; ok || len(baseline) != 2 {
		t.Fatalf("baseline after requeue = %v, want only the changed task removed", baseline)
	}
	if again := requeueChangedReviews(baseline, cohort); again != nil {
		t.Fatalf("a second requeue = %v, want nothing", again)
	}
}

// Each cycle of a ping-pong: verification reopens T2, its accepted rework amends its commit, and that
// change sends the accepted T3 back to signoff. Restarting the round count for T3 every cycle would
// keep T2 below the round cap forever; a completion another session landed still restarts it.
func TestRequeuedTasksKeepCountingTowardTheRoundCap(t *testing.T) {
	const roundCap = 3
	round := 1
	for cycle := 0; signoffRoundOutcome(round, roundCap, true) != signoffCapReached; cycle++ {
		if cycle == 10 {
			t.Fatalf("the ping-pong never reached the round cap of %d", roundCap)
		}
		round++ // the next round reviews T2's rework
		round = nextRoundAfterPending(round, []string{"t3"}, []string{"t3"})
	}
	if got := nextRoundAfterPending(5, []string{"host", "t3"}, []string{"t3"}); got != 1 {
		t.Fatalf("round after another session's completion = %d, want a new count at 1", got)
	}
}

// A loop start that dropped a review says so once, naming each task, and says nothing otherwise.
func TestNoteDroppedFinalReviewsNamesEachTask(t *testing.T) {
	if out := captureStderr(t, func() { noteDroppedFinalReviews(nil) }); out != "" {
		t.Fatalf("nothing dropped printed %q", out)
	}
	one := captureStderr(t, func() { noteDroppedFinalReviews([]string{"left-task"}) })
	for _, want := range []string{"Final review could not resume for 1 task", "This task left 99_done/ before its review finished.", "returned it to the queue", "Task: left-task"} {
		if !strings.Contains(one, want) {
			t.Errorf("notice for one task lacks %q:\n%s", want, one)
		}
	}
	two := captureStderr(t, func() { noteDroppedFinalReviews([]string{"a-task", "b-task"}) })
	if strings.Count(two, "Final review could not resume") != 1 || !strings.Contains(two, "for 2 tasks") ||
		!strings.Contains(two, "Task: a-task") || !strings.Contains(two, "Task: b-task") || !strings.Contains(two, "returned them") {
		t.Errorf("notice for two tasks:\n%s", two)
	}
}
