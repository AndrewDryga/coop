package acpproxy

import "sync"

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

func (p *proxy) idleLocked() bool {
	return p.child != nil && p.initResult != nil && p.initGeneration == p.generation && p.candidate == nil &&
		!p.restarting && !p.reloading.Load() && !p.shuttingDown && len(p.pending) == 0 &&
		len(p.restartHeld) == 0 && len(p.injected) == 0 && len(p.forceByID) == 0 && len(p.forceBySess) == 0
}
