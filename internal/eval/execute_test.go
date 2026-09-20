package eval

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func atomicLoad(p *int64) int64     { return atomic.LoadInt64(p) }
func atomicStore(p *int64, v int64) { atomic.StoreInt64(p, v) }
func atomicAdd(p *int32, v int32)   { atomic.AddInt32(p, v) }

func agentPlan(t *testing.T, cases int, repeat, jobs int, timeout time.Duration) (*Plan, []FrozenConfig) {
	t.Helper()
	body := "version: 1\nname: s\nrunner: agent\ncases:\n"
	for i := 0; i < cases; i++ {
		id := string(rune('a' + i))
		body += "  - id: " + id + "\n    instruction: x\n    verifier: ./v/" + id + "\n    timeout: 5m\n"
	}
	extra := make([]string, cases)
	for i := range extra {
		extra[i] = "v/" + string(rune('a'+i))
	}
	suite, err := Load(writeSuite(t, body, extra...))
	if err != nil {
		t.Fatal(err)
	}
	configs := []Configuration{{Kind: ConfigTarget, Label: "codex"}, {Kind: ConfigPreset, Label: "frontier"}}
	plan, err := BuildPlan(suite, configs, Options{Jobs: jobs, Repeat: repeat, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	frozen := []FrozenConfig{{Kind: ConfigTarget, Label: "codex"}, {Kind: ConfigPreset, Label: "frontier"}}
	return plan, frozen
}

// The schedule is deterministic and balanced: within a repetition no configuration gets every case
// before the other starts, and the leading configuration rotates across repetitions.
func TestBuildScheduleIsBalancedAndRotates(t *testing.T) {
	plan, frozen := agentPlan(t, 2, 2, 1, time.Hour) // cases a,b; 2 configs; 2 repeats
	sched := buildSchedule(plan, frozen)
	if len(sched) != 2*2*2 {
		t.Fatalf("schedule has %d trials, want 8", len(sched))
	}
	// Repetition 0 leads with config 0; within case a, both configs appear before case b's configs.
	if sched[0].Repetition != 0 || sched[0].ConfigIndex != 0 || sched[1].ConfigIndex != 1 {
		t.Errorf("rep 0 not interleaved config-first: %+v %+v", sched[0], sched[1])
	}
	// Repetition 1 leads with config 1 (rotated).
	rep1 := sched[4]
	if rep1.Repetition != 1 || rep1.ConfigIndex != 1 {
		t.Errorf("rep 1 did not rotate its lead: %+v", rep1)
	}
}

func TestExecuteRunsEveryTrialBoundedAndSeals(t *testing.T) {
	plan, frozen := agentPlan(t, 3, 2, 2, time.Hour) // 3 cases x 2 configs x 2 = 12 trials, 2 workers
	root := t.TempDir()
	store, err := CreateRun(root, RunRecord{ID: "run1", Suite: "s", Runner: RunnerAgent, Cases: []string{"a", "b", "c"}})
	if err != nil {
		t.Fatal(err)
	}
	var running, maxRunning int32
	var ran int32
	run := func(ctx context.Context, tr Trial) TrialResult {
		n := atomic.AddInt32(&running, 1)
		for {
			m := atomic.LoadInt32(&maxRunning)
			if n <= m || atomic.CompareAndSwapInt32(&maxRunning, m, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		atomic.AddInt32(&running, -1)
		atomic.AddInt32(&ran, 1)
		return TrialResult{Status: TrialPassed}
	}
	sum, err := Execute(context.Background(), plan, frozen, store, run, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ran != 12 {
		t.Errorf("ran %d trials, want 12", ran)
	}
	if maxRunning > 2 {
		t.Errorf("concurrency %d exceeded the 2-worker bound", maxRunning)
	}
	if sum.Requested != 12 || sum.Counts[TrialPassed] != 12 {
		t.Errorf("summary = %+v", sum)
	}
	// Every trial has a finalized record on disk, and the run is sealed.
	trials, err := LoadTrials(root, "run1")
	if err != nil || len(trials) != 12 {
		t.Fatalf("loaded %d trials: %v", len(trials), err)
	}
	for _, tr := range trials {
		if tr.Status != TrialPassed {
			t.Errorf("trial %s not finalized: %s", TrialKey(tr.Case, tr.ConfigIndex, tr.Repetition), tr.Status)
		}
	}
	if _, ok, _ := LoadSummary(root, "run1"); !ok {
		t.Error("run not sealed")
	}
}

// A run whose overall context is already cancelled admits no trials: they stay pending and are
// counted, never a false pass or fail.
func TestExecuteLeavesUnadmittedTrialsPending(t *testing.T) {
	plan, frozen := agentPlan(t, 2, 1, 1, time.Hour)
	root := t.TempDir()
	store, _ := CreateRun(root, RunRecord{ID: "run2", Suite: "s", Runner: RunnerAgent})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var ran int32
	run := func(ctx context.Context, tr Trial) TrialResult {
		atomic.AddInt32(&ran, 1)
		return TrialResult{Status: TrialPassed}
	}
	sum, err := Execute(ctx, plan, frozen, store, run, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ran != 0 {
		t.Errorf("ran %d trials on a cancelled run, want 0", ran)
	}
	if sum.Requested != 4 || sum.Counts[TrialPending] != 4 {
		t.Errorf("cancelled run summary = %+v, want 4 pending", sum)
	}
	// Every trial is on disk as pending.
	trials, _ := LoadTrials(root, "run2")
	for _, tr := range trials {
		if tr.Status != TrialPending {
			t.Errorf("trial %v not pending", tr)
		}
	}
}

// Once the admission deadline passes, a worker does not start the next trial — it stays pending,
// and the reserve is honored. Driven by an injected clock the first trial advances past admitUntil.
func TestExecuteStopsAdmittingPastTheReserve(t *testing.T) {
	prev := cleanupReserve
	cleanupReserve = 30 * time.Minute // with a 1h plan timeout, admitUntil is 30m after start
	defer func() { cleanupReserve = prev }()

	plan, frozen := agentPlan(t, 1, 1, 1, time.Hour) // 1 case x 2 configs x 1 = 2 trials, 1 worker
	root := t.TempDir()
	store, _ := CreateRun(root, RunRecord{ID: "run3", Suite: "s", Runner: RunnerAgent})

	base := time.Now() // real, so the run context (real clock) is a genuine future deadline
	var advanced int64
	clock := func() time.Time {
		if atomicLoad(&advanced) == 1 {
			return base.Add(45 * time.Minute) // past admitUntil (30m)
		}
		return base
	}
	var ran int32
	run := func(ctx context.Context, tr Trial) TrialResult {
		atomicAdd(&ran, 1)
		atomicStore(&advanced, 1) // after the first trial, the clock is past the reserve
		return TrialResult{Status: TrialPassed}
	}
	sum, err := Execute(context.Background(), plan, frozen, store, run, clock)
	if err != nil {
		t.Fatal(err)
	}
	if ran != 1 {
		t.Errorf("ran %d trials, want 1 (the second is past the reserve)", ran)
	}
	if sum.Counts[TrialPassed] != 1 || sum.Counts[TrialPending] != 1 || sum.Requested != 2 {
		t.Errorf("summary = %+v, want 1 passed + 1 pending of 2", sum)
	}
}
