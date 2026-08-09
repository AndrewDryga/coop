// Package ladder is the ROTATION LADDER's mechanics: the cursor over a run's ordered rungs — which
// rung is live, which is cooling, when to advance, how long to wait when every one is limited — and
// the classification that decides whether a provider's output was a rate/usage limit at all.
//
// A rung is one concrete agents.Target (exactly one account), so the limit map keyed by its wire
// form leaves opus@work cooling while fable@work (or codex) stays free. Everything here is pure:
// the clock is injected and nothing reads a config, prints, or starts a container. Building the
// ladder from a preset against the signed-in accounts, applying a rung to the config, and the
// actual sleeping are POLICY and stay with their caller — the loop, the ACP control, and the
// sessions service each hold their own, and share this.
package ladder

import (
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// Rotation is the ordered rungs + which are rate-limited + a sticky cursor that stays on one until
// it's limited, then advances. Rungs are concrete targets (exactly one account each); limited is
// keyed by Target.String() — a Target holds a slice, so the struct itself can't key a map.
// authFailed marks rungs whose credential this run proved unusable. It is deliberately NOT a
// time-keyed map like limited: a rate limit resets on its own, but only a human re-login revives a
// logged-out account, and the box mounts credentials at launch — so the mark is sticky for the run.
type Rotation struct {
	targets    []agents.Target
	limited    map[string]time.Time
	authFailed map[string]bool
	idx        int
}

func NewRotation(targets []agents.Target) *Rotation {
	return &Rotation{targets: targets, limited: map[string]time.Time{}, authFailed: map[string]bool{}}
}

// live reports whether rung i's credential still works this run. A dead one is never a rotation
// candidate again, so nothing can wander back onto it.
func (r *Rotation) live(i int) bool { return !r.authFailed[r.targets[i].String()] }

// free reports whether rung i can run right now: credential still good AND not cooling.
func (r *Rotation) free(i int) bool {
	if !r.live(i) {
		return false
	}
	_, limited := r.limited[r.targets[i].String()]
	return !limited
}

// OnAuthFailure marks the active rung's credential unusable for the rest of the run and moves to
// the first rung that still has one — a dead credential shouldn't abandon the queue while another
// account can work. The sticky mark bounds this to one rotation per rung. It targets a rung that is
// merely rate-limited (the loop will wait that out); it reports false only when EVERY rung has
// failed authentication, and the caller must stop.
func (r *Rotation) OnAuthFailure() bool {
	r.authFailed[r.targets[r.idx].String()] = true
	n := len(r.targets)
	for i := 1; i < n; i++ {
		if cand := (r.idx + i) % n; r.live(cand) {
			r.idx = cand
			return true
		}
	}
	return false
}

// AuthFailedTargets returns the rungs that failed authentication this run, in rotation order —
// the accounts a human has to restore.
func (r *Rotation) AuthFailedTargets() []agents.Target {
	var out []agents.Target
	for _, t := range r.targets {
		if r.authFailed[t.String()] {
			out = append(out, t)
		}
	}
	return out
}

func (r *Rotation) Active() agents.Target { return r.targets[r.idx] }

// Members renders the rotation in wire form (provider:model@account), for messages and tests.
func (r *Rotation) Members() []string {
	out := make([]string, len(r.targets))
	for i, t := range r.targets {
		out[i] = t.String()
	}
	return out
}

// Rotates reports whether there's more than one target to switch between.
func (r *Rotation) Rotates() bool { return len(r.targets) > 1 }

// Len is how many rungs the ladder has, for the caller's "all N targets are limited" narration.
func (r *Rotation) Len() int { return len(r.targets) }

// Focus points the cursor at the rung whose wire form is target, and reports whether it found one.
// A supervisor that persists its active target across a restart resumes on it this way instead of
// silently landing back on rung zero; a target the ladder no longer contains leaves the cursor put.
func (r *Rotation) Focus(target string) bool {
	for i := range r.targets {
		if r.targets[i].String() == target {
			r.idx = i
			return true
		}
	}
	return false
}

// LimitedUntil is when the named rung's cooldown ends — zero when it isn't cooling.
func (r *Rotation) LimitedUntil(target string) time.Time { return r.limited[target] }

// Limits copies out the cooling marks, keyed by rung wire form, so a supervisor can persist them
// across a respawn. SetLimits restores such a snapshot; both copy, so the caller can never hold a
// live handle on the rotation's own map.
func (r *Rotation) Limits() map[string]time.Time {
	out := make(map[string]time.Time, len(r.limited))
	for target, until := range r.limited {
		out[target] = until
	}
	return out
}

func (r *Rotation) SetLimits(limited map[string]time.Time) {
	restored := make(map[string]time.Time, len(limited))
	for target, until := range limited {
		restored[target] = until
	}
	r.limited = restored
}

// OnLimit records that the active target is rate-limited until resetAt (a zero resetAt
// means "unknown", so it backs off by attempt), then selects the next usable target.
// Keyed per target, so opus@work cooling leaves fable@work free. Returns the sleep before
// the next iteration — 0 when another target is free now — and, when sleeping, the time
// it's waiting until.
func (r *Rotation) OnLimit(resetAt time.Time, attempt int, now time.Time) (sleep time.Duration, until time.Time) {
	if !resetAt.After(now) {
		// A stale reset is no more informative than no reset. Drop it so repeated stale hints use
		// the normal bounded backoff instead of pinning every attempt to the minimum wait.
		resetAt = now.Add(LimitWait(LimitHint{Limited: true}, attempt, now))
	}
	r.limited[r.targets[r.idx].String()] = resetAt
	return r.selectTarget(attempt, now)
}

// selectTarget keeps the current rung when it is usable, otherwise advances to the first
// usable rung in rotation order. If every rung is still cooling, it parks on the soonest
// reset and returns the bounded wait. Expired marks are discarded as part of selection.
func (r *Rotation) selectTarget(attempt int, now time.Time) (sleep time.Duration, until time.Time) {
	r.ClearExpired(now)
	n := len(r.targets)
	for i := 0; i < n; i++ {
		if cand := (r.idx + i) % n; r.free(cand) {
			r.idx = cand
			return 0, time.Time{}
		}
	}
	// Every usable rung is cooling, so park on the soonest reset. Auth-dead rungs are skipped —
	// they have no reset to wait for, and switching to one would just fail again immediately.
	earliest := -1
	for i := range r.targets {
		if !r.live(i) {
			continue
		}
		if earliest < 0 || r.limited[r.targets[i].String()].Before(r.limited[r.targets[earliest].String()]) {
			earliest = i
		}
	}
	if earliest < 0 {
		earliest = r.idx // nothing left alive; the caller stops on that, so don't move
	}
	r.idx = earliest
	until = r.limited[r.targets[earliest].String()]
	return LimitWait(LimitHint{Limited: true, ResetAt: until}, attempt, now), until
}

// AdvanceOnTimeout moves to the next usable rung after a provider-attempt timeout. A timeout
// is not a rate limit, so the abandoned rung is NOT marked cooling — it keeps its standing and
// comes straight back around. With a single rung (or every other rung cooling) it stays put
// and the caller retries the same rung under the dedicated timeout cap.
func (r *Rotation) AdvanceOnTimeout(now time.Time) {
	r.ClearExpired(now)
	n := len(r.targets)
	for i := 1; i < n; i++ {
		if cand := (r.idx + i) % n; r.free(cand) {
			r.idx = cand
			return
		}
	}
}

func (r *Rotation) ClearExpired(now time.Time) {
	for target, until := range r.limited {
		if !until.After(now) {
			delete(r.limited, target)
		}
	}
}
