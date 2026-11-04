package acpproxy

import (
	"sort"
	"sync"
)

// Restarter restarts the child from outside the hooks, at a quiet moment — the supervisor moving a
// session onto a box admitted under a newly approved network policy, for one. Bind it through
// RunOpts.Restarter. Its methods are safe from any goroutine and report false while it is unbound.
type Restarter struct {
	mu sync.Mutex
	p  *proxy
}

func (r *Restarter) bind(p *proxy) {
	r.mu.Lock()
	r.p = p
	r.mu.Unlock()
}

func (r *Restarter) bound() *proxy {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.p
}

// Idle reports whether nothing is in flight: a published child has answered initialize, and no
// editor request, held prompt, coop request or replacement is pending. It is a cheap look before
// preparing a restart; only RestartIfIdle decides one.
func (r *Restarter) Idle() bool {
	p := r.bound()
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.idleLocked()
}

// RestartIfIdle restarts the child exactly as a coop-driven switch does — replayed onto the
// factory's next child, never counted as a failure — but only while Idle, and reports whether it
// did. It holds controlMu across the check and the restart, as the editor's requests are admitted
// under it, so no prompt slips between them; one that arrives after waits for the replacement.
func (r *Restarter) RestartIfIdle() bool {
	p := r.bound()
	if p == nil {
		return false
	}
	p.controlMu.Lock()
	defer p.controlMu.Unlock()
	p.mu.Lock()
	idle := p.idleLocked()
	p.mu.Unlock()
	if idle {
		Trace("restarting the idle box on request")
		p.triggerRestart()
	}
	return idle
}

// Holding reports whether the live child only holds prompts (Child.HoldPrompts): no agent is working,
// whatever the editor has waiting.
func (r *Restarter) Holding() bool {
	p := r.bound()
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.child != nil && p.child.HoldPrompts
}

// MoveOffHolding replaces a child that only holds prompts, now that the box it stood in for can start,
// and reports whether it did. Held prompts go to the replacement. It waits only for what the holding
// child still owes the editor; a prompt reserved for a resend owes nothing here, since none runs.
func (r *Restarter) MoveOffHolding() bool {
	p := r.bound()
	if p == nil {
		return false
	}
	p.controlMu.Lock()
	defer p.controlMu.Unlock()
	p.mu.Lock()
	movable := p.movableOffHoldingLocked()
	p.mu.Unlock()
	if movable {
		Trace("moving off the child that holds prompts")
		p.triggerRestart()
	}
	return movable
}

// ForceOffHolding replaces a child that only holds prompts even though it still owes the editor an
// answer: a hung adapter or a sign-in nobody finished would otherwise stop MoveOffHolding for good.
// What it owed gets the usual error, and a held prompt still goes to the replacement.
func (r *Restarter) ForceOffHolding() bool {
	p := r.bound()
	if p == nil {
		return false
	}
	p.controlMu.Lock()
	defer p.controlMu.Unlock()
	p.mu.Lock()
	holding := p.child != nil && p.child.HoldPrompts && p.candidate == nil && !p.restarting && !p.reloading.Load() && !p.shuttingDown
	p.mu.Unlock()
	if holding {
		Trace("moving off the child that holds prompts, past what it still owes")
		p.triggerRestart()
	}
	return holding
}

// Serving reports whether the live child runs turns and has answered initialize.
func (r *Restarter) Serving() bool {
	p := r.bound()
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.child != nil && !p.child.HoldPrompts && p.initResult != nil && p.initGeneration == p.generation && !p.restarting
}

// TellHeld shows Hooks.PromptHeld's current status again in every thread with a prompt waiting: what
// they wait for has changed. A thread with none hears it when it sends one.
func (r *Restarter) TellHeld() {
	p := r.bound()
	if p == nil || p.hooks == nil || p.hooks.PromptHeld == nil {
		return
	}
	p.controlMu.Lock()
	defer p.controlMu.Unlock()
	p.mu.Lock()
	ids := make([]string, 0, len(p.restartHeld))
	for sid := range p.restartHeld {
		if sid != "" { // an initialize, which belongs to no thread
			ids = append(ids, sid)
		}
	}
	p.mu.Unlock()
	sort.Strings(ids)
	for _, sid := range ids {
		if status := p.hooks.PromptHeld(sid); len(status) > 0 {
			_, _ = p.out.Write(status)
		}
	}
}

func (p *proxy) movableOffHoldingLocked() bool {
	if p.child == nil || !p.child.HoldPrompts || p.initResult == nil || p.initGeneration != p.generation || p.candidate != nil ||
		p.restarting || p.reloading.Load() || p.shuttingDown || len(p.injected) != 0 || len(p.forceByID) != 0 || len(p.forceBySess) != 0 {
		return false
	}
	for id := range p.pending {
		if request, ok := p.sessionReqs[id]; !ok || request.method != "session/prompt" || request.admitted {
			return false
		}
	}
	return true
}

func (p *proxy) idleLocked() bool {
	return p.child != nil && p.initResult != nil && p.initGeneration == p.generation && p.candidate == nil &&
		!p.restarting && !p.reloading.Load() && !p.shuttingDown && len(p.pending) == 0 &&
		len(p.restartHeld) == 0 && len(p.injected) == 0 && len(p.forceByID) == 0 && len(p.forceBySess) == 0
}
