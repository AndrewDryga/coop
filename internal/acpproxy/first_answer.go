package acpproxy

import (
	"context"
	"time"
)

// ChildFailure says how a child that runs turns failed to start (RunOpts.ChildFailed).
type ChildFailure int32

const (
	// ChildExited ended by itself.
	ChildExited ChildFailure = iota
	// ChildSilent started its box and did not answer, so the proxy stopped it.
	ChildSilent
	// ChildNotReady was still getting its box ready at the limit, so the proxy stopped it.
	ChildNotReady
)

// childAnswerWindow is how long a child whose box has started has to answer. Only the adapter is left
// to start by then, since a filtered image provisions nothing as it starts, and that took about 2 s on a
// host at load 18, which leaves a minute plenty of room. A var, so tests can shrink it.
var childAnswerWindow = time.Minute

// failure reports why the proxy stopped this child; ChildExited if it did not.
func (c *Child) failure() ChildFailure { return ChildFailure(c.stopped.Load()) }

// stopUnanswered stops a child for not answering, and records that for the run loop to report.
func (c *Child) stopUnanswered(why ChildFailure) {
	c.stopped.Store(int32(why))
	c.Stop()
}

// watchFirstAnswer stops a child that does not answer the first thing it is asked, and the run loop then
// treats it like one that exited as it started. Without this a hung first child leaves the editor
// waiting for good, with no thread to say why in, and a hung replacement ends the session.
//
// The window runs from the moment the child's box starts, the same boundary the loop arms its provider
// watchdog at (RunSpec.OnRuntimeLaunch), and never before it is asked. What comes before that is host work
// that can rightly take minutes: pulling service images, starting Compose services (18 to 76 s measured
// on real projects), waiting for another box's service launch lock. Stopping any of those at a minute
// would throw away work the next try has to redo, so they answer only to limit, the bound a replacement's
// first answer has always had. A child that cannot say when its box starts (Launched is nil) gets limit too.
// A child that says it waits for the lock is not late, since that wait has no limit: the limit counts only
// what it spends on anything else, and its supervisor decides what a session does in the meantime.
func (p *proxy) watchFirstAnswer(ctx context.Context, c *Child, window, limit time.Duration) {
	ticker := time.NewTicker(window / 60)
	defer ticker.Stop()
	var asked, started time.Time
	for {
		var now time.Time
		select {
		case <-ctx.Done():
			return
		case now = <-ticker.C:
		}
		if c.answered.Load() {
			return
		}
		if asked.IsZero() {
			if !p.askedAnything() {
				continue
			}
			asked = now
		}
		if c.WaitsForLock != nil && c.WaitsForLock() {
			asked = now
			continue
		}
		if started.IsZero() && c.Launched != nil && c.Launched() {
			started = now
			Trace("box %s has started, %s after it was asked; it has %s to answer", c.Provider, now.Sub(asked).Round(100*time.Millisecond), window)
		}
		switch {
		case !started.IsZero() && now.Sub(started) >= window:
			c.stopUnanswered(ChildSilent)
			return
		case started.IsZero() && now.Sub(asked) >= limit:
			c.stopUnanswered(ChildNotReady)
			return
		}
	}
}

func (p *proxy) askedAnything() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.initialize) > 0
}
