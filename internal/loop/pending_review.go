package loop

import (
	"fmt"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/loopcfg"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

func pendingReviewIDs(cohort tasks.PendingReviewCohort, phases ...tasks.PendingReviewPhase) []string {
	wanted := map[tasks.PendingReviewPhase]bool{}
	for _, phase := range phases {
		wanted[phase] = true
	}
	var ids []string
	for _, subject := range cohort.Subjects {
		if len(wanted) == 0 || wanted[subject.Phase] {
			ids = append(ids, subject.Task.Ref.ID)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func pendingReviewRecordsForIDs(cohort tasks.PendingReviewCohort, ids []string) ([]tasks.PendingReviewRecord, error) {
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	records := make([]tasks.PendingReviewRecord, 0, len(wanted))
	for _, record := range cohort.Subjects {
		if wanted[record.Task.Ref.ID] {
			records = append(records, record)
			delete(wanted, record.Task.Ref.ID)
		}
	}
	if len(wanted) > 0 {
		missing := make([]string, 0, len(wanted))
		for id := range wanted {
			missing = append(missing, id)
		}
		slices.Sort(missing)
		return nil, fmt.Errorf("pending final-review record(s) missing for %s", strings.Join(missing, ", "))
	}
	return records, nil
}

func pendingReviewAdditionalImports(cohort tasks.PendingReviewCohort, requested []string) []string {
	existing := map[string]bool{}
	for _, id := range pendingReviewIDs(cohort) {
		existing[id] = true
	}
	var additional []string
	for _, id := range slices.Compact(slices.Sorted(slices.Values(requested))) {
		if !existing[id] {
			additional = append(additional, id)
		}
	}
	return additional
}

func pendingReviewSubjectRoot(plan tasks.PendingReviewPlan, subject tasks.PendingReviewRecord) string {
	for _, queue := range plan.Queues {
		if queue.ID == subject.Task.Ref.QueueID {
			return queue.Root
		}
	}
	return ""
}

func pendingSignoffStartRound(cohort tasks.PendingReviewCohort) int {
	round, startedAtRound := 0, false
	for _, subject := range cohort.Subjects {
		if subject.Phase != tasks.PendingReviewSignoff && subject.Phase != tasks.PendingReviewReopened {
			continue
		}
		if subject.Round > round {
			round = subject.Round
			startedAtRound = subject.Phase == tasks.PendingReviewSignoff && subject.RoundStarted
		} else if subject.Round == round && subject.Phase == tasks.PendingReviewSignoff && subject.RoundStarted {
			startedAtRound = true
		}
	}
	if !startedAtRound {
		round++
	}
	if round < 1 {
		return 1
	}
	return round
}

func applyStoredReviewPlan(lc *loopcfg.Config, plan tasks.PendingReviewPlan) {
	lc.Signoff.Agent = slices.Clone(plan.Signoff.Targets)
	lc.Signoff.Prompt = plan.Signoff.Prompt
	lc.Signoff.Writes = loopcfg.ReviewWrites(plan.Signoff.Writes)
	lc.Signoff.Rounds = plan.SignoffRounds
	lc.Verify.Enabled = plan.VerifyEnabled
	lc.Verify.Agent = slices.Clone(plan.Verify.Targets)
	lc.Verify.Prompt = plan.Verify.Prompt
	lc.Verify.Writes = loopcfg.ReviewWrites(plan.Verify.Writes)
}

func pendingReviewPlanForRun(repo string, hosts []string, baseHead, configDigest, continueCommand string, lc *loopcfg.Config, signoffRot, verifyRot *ladder.Rotation, mcpDisabled bool) (tasks.PendingReviewPlan, error) {
	return tasks.NewPendingReviewPlan(
		repo, hosts, baseHead, configDigest, continueCommand,
		tasks.PendingReviewStage{Targets: signoffRot.Members(), Prompt: lc.Signoff.Prompt, Writes: string(lc.Signoff.Writes)},
		signoffRounds(lc),
		lc.Verify.Enabled,
		tasks.PendingReviewStage{Targets: verifyRot.Members(), Prompt: lc.Verify.Prompt, Writes: string(lc.Verify.Writes)},
		mcpDisabled,
	)
}

func withoutReviewIDs(all, excluded []string) []string {
	remove := map[string]bool{}
	for _, id := range excluded {
		remove[id] = true
	}
	var out []string
	for _, id := range all {
		if !remove[id] {
			out = append(out, id)
		}
	}
	return out
}

func announcePendingFinalReview(ids []string, continueCommand string) {
	if len(ids) == 0 {
		return
	}
	ui.Note("Resuming final review for %s completed in an earlier loop.", ui.Count(len(ids), "task"))
	if continueCommand != "" {
		ui.Note("If this run stops, continue with: %s", strings.TrimSpace(continueCommand))
	}
}

func notePendingFinalReview(ids []string) {
	if len(ids) > 0 {
		ui.Note("Final review remains pending for %s; the next loop will resume it.", ui.Count(len(ids), "accepted task"))
	}
}
