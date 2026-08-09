package cli

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
)

func decideIterationOutput(provider string, code int, err error, output string, now time.Time, fails, waits, retries *int) (loopAction, time.Duration, time.Time) {
	classification := classifyIteration(provider, code, err, output, streamNotUsed, now)
	return decideIteration(classification, now, fails, waits, retries)
}

func TestIterationAuthenticationRejectsNarration(t *testing.T) {
	for _, provider := range agents.Names() {
		agent, _ := agents.Get(provider)
		// EVERY signal, not just the first: a newly added signal is exactly where a too-broad
		// match would slip in and start reading ordinary prose as a terminal failure.
		for _, signal := range agent.LiveCredentials().AuthSignals {
			if iterationAuthentication(provider, "review "+signal+" handling before retrying") {
				t.Errorf("%s treated ordinary authentication prose (%q) as a terminal failure", provider, signal)
			}
		}
	}
}

func TestDecideIteration(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)

	// Success resets both counters and advances.
	fails, waits, retries := 3, 2, 0
	if a, _, _ := decideIterationOutput("claude", 0, nil, "done", now, &fails, &waits, &retries); a != actContinue {
		t.Errorf("success: action = %d, want actContinue", a)
	}
	if fails != 0 || waits != 0 {
		t.Errorf("success should reset counters, got fails=%d waits=%d", fails, waits)
	}

	// A rate limit bumps only waits and asks to wait (not fail).
	fails, waits = 0, 0
	a, wait, _ := decideIterationOutput("claude", 1, nil, "Claude AI usage limit reached|1700000000", now, &fails, &waits, &retries)
	if a != actWait || waits != 1 || fails != 0 || wait <= 0 {
		t.Errorf("limit: action=%d wait=%v fails=%d waits=%d, want actWait/>0/0/1", a, wait, fails, waits)
	}

	// The newer human-readable weekly limit is a wait, not a failure.
	fails, waits = 0, 0
	if a, _, _ := decideIterationOutput("claude", 1, nil, "You've hit your weekly limit · resets Jun 18, 8pm (UTC)", now, &fails, &waits, &retries); a != actWait || waits != 1 || fails != 0 {
		t.Errorf("weekly limit: action=%d waits=%d fails=%d, want actWait/1/0", a, waits, fails)
	}

	// A non-limit failure bumps fails and asks to retry.
	fails, waits = 0, 0
	if a, _, _ := decideIterationOutput("claude", 1, nil, "Error: boom", now, &fails, &waits, &retries); a != actRetry || fails != 1 {
		t.Errorf("failure: action=%d fails=%d, want actRetry/1", a, fails)
	}

	// Consecutive non-limit failures stop at the cap.
	fails, waits = maxLoopFailures-1, 0
	if a, _, _ := decideIterationOutput("claude", 1, errors.New("x"), "boom", now, &fails, &waits, &retries); a != actStop {
		t.Errorf("at failure cap: action = %d, want actStop", a)
	}

	// Consecutive rate-limit waits stop at the cap.
	fails, waits = 0, maxLimitWaits
	if a, _, _ := decideIterationOutput("claude", 1, nil, "rate limit exceeded", now, &fails, &waits, &retries); a != actStop {
		t.Errorf("at limit cap: action = %d, want actStop", a)
	}

	// A SINGLE output limit resumes immediately (the fast path) and leaves fails/waits untouched.
	fails, waits, retries = 2, 3, 0
	if a, wait, _ := decideIterationOutput("claude", 1, nil, "Output Limit Reached: maximum output length", now, &fails, &waits, &retries); a != actRetryNow || wait != 0 || fails != 2 || waits != 3 || retries != 1 {
		t.Errorf("output limit: action=%d wait=%v fails=%d waits=%d retries=%d, want actRetryNow/0/2/3/1", a, wait, fails, waits, retries)
	}
}

// TestDecideIterationOutputLimitCapped: the output-limit path used to return actRetryNow forever,
// respawning the box and burning quota with no give-up (introduced by eb36c66). Now a consecutive
// RUN of output limits backs off after the first and stops at the cap, while a single one still
// resumes at once and an intervening different outcome resets the run.
func TestDecideIterationOutputLimitCapped(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	out := "finish_reason: length"
	var fails, waits, retries int

	// First hit: immediate resume. Subsequent consecutive hits: a short backoff, until the cap.
	for i := 1; i <= maxOutputRetries; i++ {
		a, wait, _ := decideIterationOutput("claude", 1, nil, out, now, &fails, &waits, &retries)
		if a != actRetryNow {
			t.Fatalf("hit %d: action=%d, want actRetryNow", i, a)
		}
		wantWait := outputRetryBackoff
		if i == 1 {
			wantWait = 0
		}
		if wait != wantWait {
			t.Errorf("hit %d: wait=%v, want %v", i, wait, wantWait)
		}
	}
	// One past the cap: give up instead of resuming forever and preserve the terminal count for
	// the diagnostic.
	if a, _, _ := decideIterationOutput("claude", 1, nil, out, now, &fails, &waits, &retries); a != actOutputStop {
		t.Fatalf("past the output-limit cap: action=%d, want actOutputStop", a)
	}
	if retries != maxOutputRetries+1 {
		t.Errorf("terminal output-limit count = %d, want %d", retries, maxOutputRetries+1)
	}

	// A single output limit followed by a NON-output outcome resets the run: a fresh run gets the
	// full budget again, so a lone hit here and there never accumulates toward the cap.
	fails, waits, retries = 0, 0, 0
	decideIterationOutput("claude", 1, nil, out, now, &fails, &waits, &retries) // retries -> 1
	decideIterationOutput("claude", 1, nil, "Error: boom", now, &fails, &waits, &retries)
	if retries != 0 {
		t.Errorf("a non-output failure must reset the output-limit run, got retries=%d", retries)
	}
	if a, _, _ := decideIterationOutput("claude", 1, nil, out, now, &fails, &waits, &retries); a != actRetryNow || retries != 1 {
		t.Errorf("after a reset, a fresh output limit resumes at once: action=%d retries=%d", a, retries)
	}
}

func TestDecideIterationAuthenticationStopsWithoutRotation(t *testing.T) {
	var fails, waits, retries int
	if action, _, _ := decideIterationOutput("codex", 1, nil, "authentication required", time.Now(), &fails, &waits, &retries); action != actAuthStop {
		t.Fatalf("authentication action = %d, want actAuthStop", action)
	}
	if fails != 0 || waits != 0 || retries != 0 {
		t.Fatalf("authentication counters = %d/%d/%d, want unchanged", fails, waits, retries)
	}
}

func TestReviewStopRequested(t *testing.T) {
	open := make(chan struct{})
	if reviewStopRequested(context.Background(), open) {
		t.Fatal("open wake channel requested a review stop")
	}
	close(open)
	if !reviewStopRequested(context.Background(), open) {
		t.Fatal("closed wake channel did not request a review stop")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !reviewStopRequested(ctx, nil) {
		t.Fatal("canceled context did not request a review stop")
	}
}

func TestIterationOutcome(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	cases := []struct {
		name     string
		provider string
		code     int
		err      error
		output   string
		stream   providerStreamOutcome
		want     string
	}{
		{name: "success", provider: "codex", want: "success"},
		{name: "auth", provider: "codex", code: 1, output: "authentication required", want: "authentication"},
		// Verbatim from a real loop: an expired claude refresh token. This read as "process_failure"
		// once and burned the loop's whole retry budget on a rung no retry could fix.
		{name: "claude expired oauth session", provider: "claude", code: 1,
			output: "Failed to authenticate: OAuth session expired and could not be refreshed", want: "authentication"},
		{name: "rate", provider: "codex", code: 1, output: "rate limit exceeded", want: "rate_limit"},
		{name: "output", provider: "codex", code: 1, output: "maximum output length", want: "output_limit"},
		{name: "process", provider: "codex", code: 1, output: "boom", want: "process_failure"},
		{name: "drained background handoff", provider: "codex", code: box.DescendantsDrainedExit, want: "background_drained"},
		{name: "timed out background handoff", provider: "codex", code: box.DescendantsTimedOutExit, want: "background_timeout"},
		{name: "malformed", provider: "codex", err: errors.New("bad stream"), output: "malformed provider stream event", stream: streamMalformed, want: "malformed_stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyIteration(tc.provider, tc.code, tc.err, tc.output, tc.stream, now).outcome; got != tc.want {
				t.Fatalf("iterationOutcome = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestProgressStall: the loop's stall guard resets when a task completes (Done advances) and
// stops only after maxStalls consecutive iterations complete nothing.
func TestProgressStall(t *testing.T) {
	// Done advanced → baseline moves up, stalls reset, never stop.
	if b, s, stop := progressStall(3, 2, 4); b != 3 || s != 0 || stop {
		t.Errorf("progress: got (%d,%d,%v), want (3,0,false)", b, s, stop)
	}
	// No progress → baseline held, stalls increment, no stop below the cap.
	if b, s, stop := progressStall(2, 2, 0); b != 2 || s != 1 || stop {
		t.Errorf("first stall: got (%d,%d,%v), want (2,1,false)", b, s, stop)
	}
	// maxStalls consecutive no-progress iterations → stop.
	if _, s, stop := progressStall(2, 2, maxStalls-1); s != maxStalls || !stop {
		t.Errorf("at cap: got stalls=%d stop=%v, want %d/true", s, stop, maxStalls)
	}
	// A completion resets the counter even at the cap.
	if _, s, stop := progressStall(5, 2, maxStalls-1); s != 0 || stop {
		t.Errorf("recovery: got stalls=%d stop=%v, want 0/false", s, stop)
	}
	// The loop feeds the SETTLED count (done+blocked), so blocking a one-way door (done flat,
	// blocked up → settled changes) is progress and must reset the stall, even at the cap.
	if b, s, stop := progressStall(1, 0, maxStalls-1); b != 1 || s != 0 || stop {
		t.Errorf("a settled change (e.g. a block) must reset the stall: got (%d,%d,%v), want (1,0,false)", b, s, stop)
	}
	// A Done DECREASE (an audit reopened an [x], or a torn read) is movement, not a stall — it
	// re-baselines and resets, so it can't falsely trip the cap on the next iteration.
	if b, s, stop := progressStall(3, 5, maxStalls-1); b != 3 || s != 0 || stop {
		t.Errorf("decrease: got (%d,%d,%v), want (3,0,false)", b, s, stop)
	}
}

// TestReviewRoundCap covers the batch-scaled cap: half the tasks worked, floored at 3 and
// ceilinged at COOP_MAX_REVIEW_ROUNDS — so a tiny batch still gets 3 rounds and a big overnight
// batch caps at the ceiling.
func TestReviewRoundCap(t *testing.T) {
	const max = 5
	cases := []struct{ tasks, want int }{
		{0, 3},   // nothing worked → the floor
		{4, 3},   // 4/2=2 → floored to 3
		{6, 3},   // 6/2=3 → exactly the floor
		{8, 4},   // 8/2=4 → between floor and ceiling
		{10, 5},  // 10/2=5 → the ceiling
		{100, 5}, // huge batch → still the ceiling
	}
	for _, c := range cases {
		if got := signoffRoundCap(c.tasks, max); got != c.want {
			t.Errorf("signoffRoundCap(%d, %d) = %d, want %d", c.tasks, max, got, c.want)
		}
	}
	// A ceiling below the floor (COOP_MAX_REVIEW_ROUNDS=1, a one-shot review) still wins.
	if got := signoffRoundCap(100, 1); got != 1 {
		t.Errorf("signoffRoundCap(100, 1) = %d, want 1 (a sub-floor ceiling wins)", got)
	}
}

// TestReviewRoundOutcome covers the three loop-until-accepted convergence paths: a review that
// reopens nothing is accepted immediately; a review that reopens work re-drains while rounds
// remain; and one that never converges is capped (block the stuck task → exit 3).
func TestReviewRoundOutcome(t *testing.T) {
	const cap = 3
	// Accept immediately: round 1, nothing reopened → done in one review pass.
	if got := signoffRoundOutcome(1, cap, false); got != signoffAccepted {
		t.Errorf("round 1, nothing reopened: got %v, want signoffAccepted", got)
	}
	// A clean review accepts at ANY round (e.g. after a reopen-then-fix), not just the first.
	if got := signoffRoundOutcome(cap, cap, false); got != signoffAccepted {
		t.Errorf("clean review at the last round: got %v, want signoffAccepted", got)
	}
	// Reopen with rounds remaining → drain again.
	for r := 1; r < cap; r++ {
		if got := signoffRoundOutcome(r, cap, true); got != signoffContinue {
			t.Errorf("round %d/%d reopened: got %v, want signoffContinue", r, cap, got)
		}
	}
	// Never converges: still reopening AT the cap → block the stuck task.
	if got := signoffRoundOutcome(cap, cap, true); got != signoffCapReached {
		t.Errorf("round %d/%d still reopening: got %v, want signoffCapReached", cap, cap, got)
	}
	// A cap of 1 (COOP_MAX_REVIEW_ROUNDS=1) is a one-shot review: reopen on round 1 → block now.
	if got := signoffRoundOutcome(1, 1, true); got != signoffCapReached {
		t.Errorf("cap 1, reopened: got %v, want signoffCapReached", got)
	}
}

// fakeClock returns a scripted sequence of wall-clock readings (advancing one per call, then
// holding the last), so a test can jump the clock forward mid-wait to simulate a laptop suspend.
type fakeClock struct {
	mu    sync.Mutex
	times []time.Time
	i     int
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.times[c.i]
	if c.i < len(c.times)-1 {
		c.i++
	}
	return t
}

func TestWaitUntilWall(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)

	// A suspend that jumps the wall clock PAST the deadline mid-wait ends the wait within one tick
	// — it must NOT keep counting the (frozen-monotonic) leftover. tickCap is tiny so the single
	// real sleep is negligible; the clock jump does the rest.
	t.Run("suspend jump ends promptly", func(t *testing.T) {
		clk := &fakeClock{times: []time.Time{base, base.Add(2 * time.Hour)}}
		start := time.Now()
		if !waitUntilWall(base.Add(time.Hour), 20*time.Millisecond, clk.now, nil, nil) {
			t.Fatal("want true (reached deadline)")
		}
		if el := time.Since(start); el > 2*time.Second {
			t.Errorf("took %s — should return within a tick of the jump, not wait leftover time", el)
		}
	})

	// stop fires → returns false without waiting out the deadline.
	t.Run("stop bails", func(t *testing.T) {
		stop := make(chan struct{})
		close(stop)
		if waitUntilWall(time.Now().Add(time.Hour), time.Minute, nil, stop, nil) {
			t.Error("want false when stop fires")
		}
	})

	// A deadline already in the past returns immediately without sleeping.
	t.Run("past deadline is a no-op", func(t *testing.T) {
		clk := &fakeClock{times: []time.Time{base}}
		start := time.Now()
		if !waitUntilWall(base.Add(-time.Hour), time.Minute, clk.now, nil, nil) {
			t.Fatal("want true")
		}
		if el := time.Since(start); el > 500*time.Millisecond {
			t.Errorf("past deadline slept %s, want ~0", el)
		}
	})
}

// sleepForLimitAt sleeps in short segments and re-derives the remaining against the injected wall
// clock, so a suspend gap can't inflate it. Here the clock lands just short of the reset, sleeps a
// tiny real segment, then reads past it — the wait ends instead of counting the full hour.
func TestSleepForLimitAtEndsAtReset(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	resetAt := base.Add(time.Hour)
	clk := &fakeClock{times: []time.Time{
		base, // start anchor → deadline = base + 1h
		base.Add(time.Hour - 10*time.Millisecond), // first re-check: 10ms remaining
		base.Add(time.Hour + 5*time.Millisecond),  // next re-check: past the reset → done
	}}
	start := time.Now()
	sleepForLimitAt(time.Hour, resetAt, nil, clk.now)
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("sleepForLimitAt took %s — should end at the wall-clock reset, not wait the full hour", el)
	}
}

func TestTailWriter(t *testing.T) {
	w := &tailWriter{max: 10}
	w.Write([]byte("12345"))
	w.Write([]byte("67890ABCDE"))
	if got := w.String(); got != "67890ABCDE" {
		t.Errorf("tail = %q, want last 10 bytes %q", got, "67890ABCDE")
	}

	// Safe under the concurrent stdout/stderr copy goroutines os/exec uses.
	cw := &tailWriter{max: 1 << 12}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				cw.Write([]byte(strings.Repeat("x", 8)))
			}
		}()
	}
	wg.Wait()
	if len(cw.String()) > cw.max {
		t.Errorf("tail grew past max: %d > %d", len(cw.String()), cw.max)
	}
}
