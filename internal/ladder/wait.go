package ladder

import "time"

// The wall-clock wait MECHANICS behind a limit pause. Deciding to wait — and narrating it — stays
// with the caller (the loop, the ACP control), but every one of them has to survive a laptop
// suspend the same way, so the how lives here once, next to the limit types it waits on.

// SleepOrWake waits up to d, returning early with false if wake fires (or is closed) — so the
// loop's pauses end promptly when a stop is requested, now that the loop catches SIGINT instead
// of dying on it. A nil wake never fires; true means it slept the full d.
func SleepOrWake(d time.Duration, wake <-chan struct{}) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-wake:
		return false
	}
}

// LimitTickCap bounds each sleep segment of a rate-limit wait. Go timers run on the MONOTONIC
// clock, which freezes while a laptop is suspended (macOS lid closed) and does not fire during
// sleep — so a single long timer resumes on wake still owing the pre-suspend remainder and
// over-waits past the real reset by roughly the closed duration. Waking at least this often
// re-derives the remaining time from the WALL clock, so reopening the lid past the reset ends the
// wait within one tick instead of counting leftover awake-time.
const LimitTickCap = time.Minute

// WaitUntilWall blocks until `deadline` passes on the WALL clock, or until stop fires. It strips
// the monotonic reading from deadline and re-compares against nowFn() each cycle, sleeping at most
// tickCap between checks, so a system-suspend gap can't inflate the wait: a frozen monotonic timer
// resumes and re-evaluates against the real clock, returning promptly once the deadline is past.
// onSegment, when non-nil, is called with the wall-clock remaining after each FULL tickCap segment
// (not the final partial one) for progress narration. Returns true if it waited to the deadline,
// false if stop fired first. nowFn defaults to time.Now when nil; it is injectable for tests.
func WaitUntilWall(deadline time.Time, tickCap time.Duration, nowFn func() time.Time, stop <-chan struct{}, onSegment func(remaining time.Duration)) bool {
	if nowFn == nil {
		nowFn = time.Now
	}
	deadline = deadline.Round(0) // drop the monotonic reading so Sub uses the wall clock
	for {
		remaining := deadline.Sub(nowFn())
		if remaining <= 0 {
			return true
		}
		seg, capped := remaining, false
		if seg > tickCap {
			seg, capped = tickCap, true
		}
		if !SleepOrWake(seg, stop) {
			return false // stop requested — bail out of the wait
		}
		if capped && onSegment != nil {
			onSegment(deadline.Sub(nowFn()))
		}
	}
}
