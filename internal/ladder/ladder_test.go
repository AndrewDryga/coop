package ladder

import (
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// rts builds a claude rotation from bare account names (model empty), for the engine tests.
// String() renders each as "claude@<acct>".
func rts(creds ...string) *Rotation {
	ts := make([]agents.Target, len(creds))
	for i, c := range creds {
		ts[i] = agents.Target{Provider: "claude", Accounts: []string{c}}
	}
	return NewRotation(ts)
}

func TestRotationSingle(t *testing.T) {
	now := time.Unix(1000, 0)
	r := rts("only")
	if r.Rotates() {
		t.Error("a single-target rotation shouldn't rotate")
	}
	reset := now.Add(time.Hour)
	sleep, until := r.OnLimit(reset, 1, now)
	if sleep <= 0 || !until.Equal(reset) || r.Active().String() != "claude@only" {
		t.Errorf("single-target limit: sleep=%v until=%v active=%q, want a wait to %v on only", sleep, until, r.Active(), reset)
	}
}

func TestRotationStickyThenWaits(t *testing.T) {
	now := time.Unix(1000, 0)
	r := rts("a", "b", "c")
	if r.Active().String() != "claude@a" {
		t.Fatalf("start active = %q, want a", r.Active())
	}
	if sleep, _ := r.OnLimit(now.Add(2*time.Hour), 1, now); sleep != 0 || r.Active().String() != "claude@b" {
		t.Fatalf("after a limited: sleep=%v active=%q, want 0 + b", sleep, r.Active())
	}
	if sleep, _ := r.OnLimit(now.Add(time.Hour), 2, now); sleep != 0 || r.Active().String() != "claude@c" {
		t.Fatalf("after b limited: sleep=%v active=%q, want 0 + c", sleep, r.Active())
	}
	sleep, until := r.OnLimit(now.Add(3*time.Hour), 3, now)
	if sleep <= 0 || !until.Equal(now.Add(time.Hour)) || r.Active().String() != "claude@b" {
		t.Errorf("all limited → park on soonest-reset b (+1h): sleep=%v until=%v active=%q", sleep, until, r.Active())
	}
}

func TestRotationUnknownResetBacksOff(t *testing.T) {
	now := time.Unix(1000, 0)
	r := rts("a", "b")
	if sleep, _ := r.OnLimit(time.Time{}, 1, now); sleep != 0 || r.Active().String() != "claude@b" {
		t.Fatalf("unknown reset, b free: sleep=%v active=%q", sleep, r.Active())
	}
	if sleep, _ := r.OnLimit(time.Time{}, 2, now); sleep <= 0 || sleep > LimitMaxWait {
		t.Errorf("all limited w/ unknown reset: sleep=%v, want a bounded backoff", sleep)
	}
}

func TestRotationNonfutureResetStillCoolsTarget(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, reset := range []time.Time{time.Time{}, now.Add(-time.Hour), now} {
		r := rts("a", "b")
		if sleep, _ := r.OnLimit(reset, 3, now); sleep != 0 || r.Active().String() != "claude@b" {
			t.Fatalf("reset %v: sleep=%v active=%q, want free target b", reset, sleep, r.Active())
		}
		if until, want := r.limited["claude@a"], now.Add(4*time.Minute); !until.Equal(want) {
			t.Fatalf("reset %v: normalized cooling deadline = %v, want backoff %v", reset, until, want)
		}
	}
}

func TestRotationSelectionSkipsFutureLimitAndClearsExpired(t *testing.T) {
	now := time.Unix(1000, 0)
	r := rts("a", "b", "c")
	r.limited["claude@a"] = now.Add(time.Hour)
	if sleep, _ := r.selectTarget(1, now); sleep != 0 || r.Active().String() != "claude@b" {
		t.Fatalf("future-limited a: sleep=%v active=%q, want 0 + b", sleep, r.Active())
	}

	r = rts("a", "b", "c")
	r.limited["claude@a"] = now.Add(-time.Second)
	if sleep, _ := r.selectTarget(1, now); sleep != 0 || r.Active().String() != "claude@a" {
		t.Fatalf("expired a: sleep=%v active=%q, want 0 + a", sleep, r.Active())
	}
	if len(r.limited) != 0 {
		t.Errorf("expired marks not cleared: %v", r.limited)
	}
}

func TestRotationAdvanceOnTimeout(t *testing.T) {
	now := time.Unix(1000, 0)

	// A timeout rotates to the next usable rung WITHOUT cooling the abandoned one.
	r := rts("a", "b", "c")
	r.AdvanceOnTimeout(now)
	if r.Active().String() != "claude@b" || len(r.limited) != 0 {
		t.Fatalf("after timeout: active=%q limited=%v, want b with no cooling", r.Active(), r.limited)
	}

	// Cooling rungs are skipped; the sole usable one is the next stop.
	r = rts("a", "b", "c")
	r.limited["claude@b"] = now.Add(time.Hour)
	r.AdvanceOnTimeout(now)
	if r.Active().String() != "claude@c" {
		t.Fatalf("cooling b skipped: active=%q, want c", r.Active())
	}

	// A single rung — or every other rung cooling — retries the same rung.
	r = rts("only")
	r.AdvanceOnTimeout(now)
	if r.Active().String() != "claude@only" {
		t.Fatalf("single rung moved: active=%q", r.Active())
	}
	r = rts("a", "b")
	r.limited["claude@b"] = now.Add(time.Hour)
	r.AdvanceOnTimeout(now)
	if r.Active().String() != "claude@a" {
		t.Fatalf("all others cooling: active=%q, want a retried", r.Active())
	}

	// An expired mark is usable again — timeouts never extend cooling.
	r = rts("a", "b")
	r.limited["claude@b"] = now.Add(-time.Second)
	r.AdvanceOnTimeout(now)
	if r.Active().String() != "claude@b" || len(r.limited) != 0 {
		t.Fatalf("expired mark: active=%q limited=%v, want b cleared", r.Active(), r.limited)
	}
}

// The acceptance scenario, per (model,account) key: models: [fable, opus@work] over accounts
// {work, personal} → fable@work → fable@personal (same model, other account) → opus@work; and
// fable@work cooling never blocks opus@work.
func TestRotationSameModelFallback(t *testing.T) {
	now := time.Unix(1000, 0)
	r := NewRotation([]agents.Target{
		{Provider: "claude", Model: "fable", Accounts: []string{"work"}},
		{Provider: "claude", Model: "fable", Accounts: []string{"personal"}},
		{Provider: "claude", Model: "opus", Accounts: []string{"work"}},
	})
	if r.Active().String() != "claude:fable@work" {
		t.Fatalf("start = %q", r.Active())
	}
	if sleep, _ := r.OnLimit(now.Add(time.Hour), 1, now); sleep != 0 || r.Active().String() != "claude:fable@personal" {
		t.Fatalf("fable@work limited → fable@personal: sleep=%v active=%q", sleep, r.Active())
	}
	if sleep, _ := r.OnLimit(now.Add(time.Hour), 2, now); sleep != 0 || r.Active().String() != "claude:opus@work" {
		t.Fatalf("fable@personal limited → opus@work: sleep=%v active=%q", sleep, r.Active())
	}
	if len(r.limited) != 2 {
		t.Fatalf("limited keys = %d, want 2 distinct (model,account) targets", len(r.limited))
	}
}

// An expired credential on the first rung must not abandon a queue that another signed-in account
// could still drain — the failure that motivated auth rotation.
func TestRotationAuthFailureAdvances(t *testing.T) {
	r := rts("personal", "backup")
	if !r.OnAuthFailure() {
		t.Fatal("auth failure with a healthy rung left = false, want a switch")
	}
	if r.Active().String() != "claude@backup" {
		t.Fatalf("after personal failed auth, active = %q, want claude@backup", r.Active())
	}
	// Sticky: no reset revives a logged-out account, so the dead rung never comes back around.
	now := time.Unix(1000, 0)
	if sleep, _ := r.OnLimit(now.Add(time.Hour), 1, now); sleep <= 0 {
		t.Errorf("backup limited with personal auth-dead: sleep=%v, want a wait rather than a switch", sleep)
	}
	if r.Active().String() != "claude@backup" {
		t.Errorf("a rate limit rotated back onto the auth-dead rung: active = %q", r.Active())
	}
}

// Every rung dead is the one case that still stops the run.
func TestRotationAuthFailureExhausted(t *testing.T) {
	r := rts("personal", "backup")
	if !r.OnAuthFailure() {
		t.Fatal("first auth failure should switch to backup")
	}
	if r.OnAuthFailure() {
		t.Error("auth failure with every rung dead = true, want a stop")
	}
	failed := r.AuthFailedTargets()
	if len(failed) != 2 || failed[0].String() != "claude@personal" || failed[1].String() != "claude@backup" {
		t.Errorf("AuthFailedTargets = %v, want both accounts in rotation order", failed)
	}
}

// A single-rung run has nothing to switch to and must keep failing fast, exactly as before.
func TestRotationAuthFailureSingleRung(t *testing.T) {
	r := rts("only")
	if r.OnAuthFailure() {
		t.Error("single-rung auth failure = true, want a stop")
	}
}

// A timeout rotation must not wander onto a rung whose credential is already known dead.
func TestRotationTimeoutSkipsAuthDeadRung(t *testing.T) {
	r := rts("personal", "backup")
	r.OnAuthFailure() // personal dead, active = backup
	r.AdvanceOnTimeout(time.Unix(1000, 0))
	if r.Active().String() != "claude@backup" {
		t.Errorf("timeout advance landed on %q, want to stay on claude@backup (personal is auth-dead)", r.Active())
	}
}

// Focus resumes on a persisted rung, and leaves the cursor alone when the ladder no longer has it —
// the contract the ACP control and the sessions service both restore through.
func TestRotationFocus(t *testing.T) {
	r := rts("a", "b", "c")
	if !r.Focus("claude@c") || r.Active().String() != "claude@c" {
		t.Fatalf("focus on a present rung: active = %q, want claude@c", r.Active())
	}
	if r.Focus("claude@gone") || r.Active().String() != "claude@c" {
		t.Errorf("focus on an absent rung moved the cursor to %q, want claude@c held", r.Active())
	}
}

// Limits/SetLimits are the supervisor's snapshot pair: both copy, so a restored map can't alias
// the rotation's own and drift a persisted cooldown out from under it.
func TestRotationLimitsSnapshotCopies(t *testing.T) {
	now := time.Unix(1000, 0)
	until := now.Add(time.Hour)
	r := rts("a", "b")
	r.SetLimits(map[string]time.Time{"claude@a": until})
	if got := r.LimitedUntil("claude@a"); !got.Equal(until) {
		t.Fatalf("LimitedUntil after SetLimits = %v, want %v", got, until)
	}
	if got := r.LimitedUntil("claude@b"); !got.IsZero() {
		t.Errorf("an uncooled rung = %v, want zero", got)
	}
	snapshot := r.Limits()
	snapshot["claude@b"] = until // mutating the copy must not cool a live rung
	if !r.LimitedUntil("claude@b").IsZero() {
		t.Error("Limits handed out the live map: mutating the snapshot cooled a rung")
	}
	// SetLimits(nil) empties rather than nils, so a restore from a snapshot that carried no marks
	// leaves a working rotation instead of a nil map to write into.
	r.SetLimits(nil)
	if sleep, _ := r.OnLimit(until, 1, now); sleep != 0 || r.Active().String() != "claude@b" {
		t.Errorf("after SetLimits(nil): sleep=%v active=%q, want 0 + a free rung to rotate onto", sleep, r.Active())
	}
}
