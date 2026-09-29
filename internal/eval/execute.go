package eval

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// This is the trial orchestration engine: given a plan, the frozen configurations, a store and an
// injected function that actually runs one trial, it schedules every (case x configuration x
// repetition) across a bounded set of workers, writes a durable record for each (pending at start,
// running before launch, finalized after), and seals the run's summary. The function that runs a
// trial — launching a box, grading — is injected by the CLI, so this package owns scheduling,
// concurrency, records and the deadline while importing no box, runtime or provider machinery.
//
// Two properties the spec insists on: scheduling is DETERMINISTIC and BALANCED — the first
// configuration never gets every case before the second starts, and which configuration leads
// rotates across repetitions — so a transient slowdown does not systematically favor one side; and
// a bounded tail of the deadline is reserved for cleanup, so new trials stop being admitted before
// the reserve and the run always has time to seal and tear down.

// Trial is one unit of work handed to the runner's function: a case under one configuration, one
// repetition. Deadline is the wall-clock time this trial must finish by; it is the trial's own
// start plus its case budget, set when the worker picks it up (not at schedule-build time), so a
// trial that waited in the queue still gets its full budget.
type Trial struct {
	Case        Case
	ConfigIndex int
	Config      FrozenConfig
	Repetition  int
	Order       int // position in the deterministic schedule, recorded so the order is readable
	Deadline    time.Time
}

// TrialResult is what the injected function reports. Detail is a short human note (why it failed, a
// timeout diagnostic); it is recorded verbatim.
type TrialResult struct {
	Status TrialStatus
	Detail string
	// Size is the change-size measurement, when one was taken. nil means it could not be measured.
	Size *TrialSize
}

// TrialFunc runs one trial to a terminal result. It must honor ctx (cancelled at the trial deadline
// or when the run is torn down) and never panic the runner — a returned error-status is how it
// reports a harness failure.
type TrialFunc func(ctx context.Context, t Trial) TrialResult

// cleanupReserve is the tail of the overall deadline held back so a run can always seal and clean up
// rather than be killed mid-teardown. New trials are not admitted once this little remains, and a
// running trial's own deadline is capped to it, so the reserve is really left. A package var so a
// test can shrink it.
var cleanupReserve = 10 * time.Second

// Execute runs the whole plan. It writes a pending record for every requested trial first (so an
// interrupted run is visibly incomplete, never silently short), then works the schedule with `jobs`
// workers until the schedule is empty or the admission deadline passes, and finally seals the
// summary over the FULL requested matrix. It returns the sealed summary.
func Execute(ctx context.Context, plan *Plan, frozen []FrozenConfig, store *Store, run TrialFunc, now func() time.Time) (RunSummary, error) {
	if now == nil {
		now = time.Now
	}
	if len(frozen) != len(plan.Configs) {
		return RunSummary{}, fmt.Errorf("have %d frozen configurations, plan has %d", len(frozen), len(plan.Configs))
	}
	schedule := buildSchedule(plan, frozen)

	// Every requested trial is pending on disk before any work, so the run's denominator is fixed
	// and an interruption leaves an honest partial record.
	for _, t := range schedule {
		if err := store.WriteTrial(pendingRecord(store.ID(), t)); err != nil {
			return RunSummary{}, err
		}
	}

	// The caller may have spent part of this budget preparing the run. Use the earlier absolute
	// deadline for both cancellation and admission; an earlier context must not gain a fresh trial
	// admission window here.
	deadline := now().Add(plan.Timeout)
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	admitUntil := deadline.Add(-cleanupReserve)
	runCtx, cancelRun := context.WithDeadline(ctx, deadline)
	defer cancelRun()

	var mu sync.Mutex
	counts := map[TrialStatus]int{}
	record := func(t Trial, status TrialStatus, detail string, size *TrialSize, started, ended time.Time) error {
		mu.Lock()
		counts[status]++
		mu.Unlock()
		rec := pendingRecord(store.ID(), t)
		rec.Status, rec.Detail, rec.StartedAt, rec.EndedAt = status, capDetail(detail), started, ended
		rec.Size = size
		return store.WriteTrial(rec)
	}

	jobs := plan.Jobs
	if jobs < 1 {
		jobs = 1
	}
	work := make(chan Trial)
	var wg sync.WaitGroup
	var recordErr error
	var recordMu sync.Mutex
	noteErr := func(err error) {
		if err == nil {
			return
		}
		recordMu.Lock()
		if recordErr == nil {
			recordErr = err
			cancelRun()
		}
		recordMu.Unlock()
	}
	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range work {
				// A trial reached after the run is done, or past the admission deadline, is not
				// started: it stays pending on disk (counted, never a false pass/fail).
				if runCtx.Err() != nil || !now().Before(admitUntil) {
					continue
				}
				started := now()
				// The trial's budget is its case timeout from its own start, but never past
				// admitUntil — so the reserve is genuinely left for this trial's own teardown even
				// if it began late in the run.
				t.Deadline = started.Add(time.Duration(t.Case.Timeout))
				if t.Deadline.After(admitUntil) {
					t.Deadline = admitUntil
				}
				// Write-before-launch: an interrupted run can then tell a trial that STARTED and was
				// killed from one that never left the queue.
				if err := store.WriteTrial(runningRecord(store.ID(), t, started)); err != nil {
					noteErr(err)
					continue
				}
				if runCtx.Err() != nil || !now().Before(admitUntil) {
					// A slow durable write can use the remaining admission budget. No provider was
					// launched, so restore pending rather than leave a false running attempt.
					noteErr(store.WriteTrial(pendingRecord(store.ID(), t)))
					continue
				}
				trialCtx, cancel := context.WithDeadline(runCtx, t.Deadline)
				res := run(trialCtx, t)
				cancel()
				status := res.Status
				if status == "" {
					status = TrialError
				}
				noteErr(record(t, status, res.Detail, res.Size, started, now()))
			}
		}()
	}
	for _, t := range schedule {
		if runCtx.Err() != nil || !now().Before(admitUntil) {
			break // stop admitting new work; the reserve keeps time to seal and clean up
		}
		select {
		case work <- t:
		case <-runCtx.Done():
		}
	}
	close(work)
	wg.Wait()
	if recordErr != nil {
		return RunSummary{}, recordErr
	}

	// The trials never admitted (deadline/cancel) are still pending on disk; reflect them in the
	// summary so requested == the full matrix and coverage is honest.
	mu.Lock()
	done := 0
	for _, n := range counts {
		done += n
	}
	if pending := len(schedule) - done; pending > 0 {
		counts[TrialPending] += pending
	}
	summary := RunSummary{Requested: len(schedule), Counts: counts}
	mu.Unlock()
	if err := store.Seal(summary); err != nil {
		return RunSummary{}, err
	}
	return summary, nil
}

// capDetail bounds a trial's detail — it can carry candidate-influenced text — so one trial cannot
// bloat the record store.
func capDetail(s string) string {
	const max = 4 << 10
	if len(s) > max {
		return s[:max] + "…(truncated)"
	}
	return s
}

func runningRecord(runID string, t Trial, started time.Time) TrialRecord {
	rec := pendingRecord(runID, t)
	rec.Status, rec.StartedAt = TrialRunning, started
	return rec
}

// buildSchedule lays out the trials in a deterministic, balanced order: repetition by repetition,
// and within each repetition it rotates which configuration leads and interleaves configurations
// across cases, so no configuration gets every case before another starts and the lead alternates
// across repetitions. The order is what a run records and what a reader can reproduce.
func buildSchedule(plan *Plan, frozen []FrozenConfig) []Trial {
	cases := plan.Suite.Cases
	n := len(frozen)
	var out []Trial
	order := 0
	for rep := 0; rep < plan.Repeat; rep++ {
		lead := 0
		if n > 0 {
			lead = rep % n // vary the first configuration across repetitions
		}
		for _, c := range cases {
			for k := 0; k < n; k++ {
				idx := (lead + k) % n
				out = append(out, Trial{Case: c, ConfigIndex: idx, Config: frozen[idx], Repetition: rep, Order: order})
				order++
			}
		}
	}
	return out
}

func pendingRecord(runID string, t Trial) TrialRecord {
	return TrialRecord{
		RunID: runID, Case: t.Case.ID, ConfigIndex: t.ConfigIndex,
		ConfigLabel: t.Config.Label, Repetition: t.Repetition, Order: t.Order, Status: TrialPending,
	}
}
