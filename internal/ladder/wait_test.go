package ladder

import (
	"sync"
	"testing"
	"time"
)

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
		if !WaitUntilWall(base.Add(time.Hour), 20*time.Millisecond, clk.now, nil, nil) {
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
		if WaitUntilWall(time.Now().Add(time.Hour), time.Minute, nil, stop, nil) {
			t.Error("want false when stop fires")
		}
	})

	// A deadline already in the past returns immediately without sleeping.
	t.Run("past deadline is a no-op", func(t *testing.T) {
		clk := &fakeClock{times: []time.Time{base}}
		start := time.Now()
		if !WaitUntilWall(base.Add(-time.Hour), time.Minute, clk.now, nil, nil) {
			t.Fatal("want true")
		}
		if el := time.Since(start); el > 500*time.Millisecond {
			t.Errorf("past deadline slept %s, want ~0", el)
		}
	})
}

// SleepOrWake is the segment primitive WaitUntilWall is built on: a nil wake sleeps the whole
// duration, a closed one returns immediately, and a non-positive duration never touches a timer.
func TestSleepOrWake(t *testing.T) {
	if !SleepOrWake(0, nil) {
		t.Error("a non-positive duration must report a full sleep")
	}
	if !SleepOrWake(time.Millisecond, nil) {
		t.Error("a nil wake never fires, so the full sleep must report true")
	}
	wake := make(chan struct{})
	close(wake)
	start := time.Now()
	if SleepOrWake(time.Hour, wake) {
		t.Error("a fired wake must report the sleep was cut short")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("a fired wake waited %s, want ~0", el)
	}
}
