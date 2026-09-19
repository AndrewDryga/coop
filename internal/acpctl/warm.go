package acpctl

import (
	"sync"

	"github.com/AndrewDryga/coop/internal/acpproxy"
)

// WarmPool keeps an already-initialized box warm per signed-in provider, so an ACP provider switch
// swaps to a hot adapter (paying only the proxy's replay) instead of cold-booting a container + node
// adapter (~5s). It lives entirely behind cmdACPSupervise's factory: the factory checks it out for a
// plain provider switch and cold-spawns otherwise, so the acpproxy contract is untouched and a miss
// degrades to today's behavior. `spawn` is injected — the real one execs a `coop acp` inner for the
// provider's default target; tests pass a fake.
//
// The pool owns boxes, not the mutex: Child.Stop and Child.SetActive are injected callbacks that
// kill process groups and remove containers, so they always run with the lock released.
type WarmPool struct {
	mu       sync.Mutex
	spawn    func(provider string) (*acpproxy.Child, error)
	boxes    map[string]*acpproxy.Child
	inflight map[string]bool // a spawn is in flight for this provider (don't double-spawn)
	enabled  bool
	fills    sync.WaitGroup // in-flight Refills, so Reap drains them instead of racing them
	reap     sync.Once      // teardown runs once, however many exit paths reach it
}

func NewWarmPool(enabled bool, spawn func(provider string) (*acpproxy.Child, error)) *WarmPool {
	return &WarmPool{spawn: spawn, boxes: map[string]*acpproxy.Child{}, inflight: map[string]bool{}, enabled: enabled}
}

// Checkout pops and returns the warm box for provider when it runs on account (any account when
// account is empty) from image, the box image a cold start would use now; otherwise nil. A box on
// another account stays parked — it is still the right box for a switch to its own account — but one
// from another image is stopped: a rebuild made it stale, and it can serve no switch again. An image
// that cannot be told ("") reuses nothing and stops nothing. The caller then OWNS a returned box — the
// pool no longer tracks or reaps it.
func (p *WarmPool) Checkout(provider, account, image string) *acpproxy.Child {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	c := p.boxes[provider]
	var stale *acpproxy.Child
	switch {
	case c == nil || image == "":
		c = nil
	case c.Image != image:
		stale, c = c, nil
		delete(p.boxes, provider)
	case account != "" && c.Account != account:
		c = nil
	default:
		delete(p.boxes, provider)
	}
	if stale != nil {
		p.fills.Add(1) // Reap must not report teardown done while this stop is still running
	}
	p.mu.Unlock()
	if stale != nil {
		p.stop(stale)
		p.fills.Done()
	}
	if c != nil && c.SetActive != nil {
		c.SetActive(true)
	}
	return c
}

// Refill spawns a warm box for provider and stores it — unless the pool is disabled, one is already
// held, or a spawn is already in flight. Synchronous (the spawn runs outside the lock); the caller
// runs it in a goroutine so warming never adds startup latency. A box spawned after the pool was
// reaped is Stopped rather than leaked, and Reap waits for that to finish.
func (p *WarmPool) Refill(provider string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if !p.enabled || p.boxes[provider] != nil || p.inflight[provider] {
		p.mu.Unlock()
		return
	}
	p.inflight[provider] = true
	// Registered under the same lock that guards `enabled`, so Reap can disable the pool and then
	// wait with no gap for a fill to slip through: a fill either hasn't passed the check above yet
	// (and will bail) or is already counted here.
	p.fills.Add(1)
	p.mu.Unlock()
	defer p.fills.Done()

	child, err := p.spawn(provider)
	if err == nil && child != nil && child.SetActive != nil {
		child.SetActive(false) // park it — a foreign callback, so never under the lock
	}

	p.mu.Lock()
	delete(p.inflight, provider)
	parked := ""
	if err == nil && child != nil && p.enabled {
		p.boxes[provider] = child
		parked = child.Provider + "@" + child.Account
		child = nil // ownership moved into the pool; there is nothing left for us to stop
	}
	p.mu.Unlock()
	if parked != "" {
		// Parked, not proven up: the box may still be starting, and one that dies is found by the
		// replay after a checkout, which then starts another cold.
		acpproxy.Trace("warm pool: %s parked", parked)
	}
	// A failed spawn leaves the slot empty (the factory cold-spawns on the next switch); one that
	// finished after Reap disabled the pool is stopped here rather than leaked. Nil-safe.
	p.stop(child)
}

// Rebalance keeps the pool at one box for each provider the session is not using: it stops a parked
// box for the provider that just became active — a spare nobody can switch to while it is the active
// one — and warms each of others, the providers a switch could go to next, including the one just
// left. Synchronous, like Refill; the caller runs it in a goroutine.
func (p *WarmPool) Rebalance(active string, others []string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	spare := p.boxes[active]
	delete(p.boxes, active)
	if spare != nil {
		p.fills.Add(1) // Reap must not report teardown done while this stop is still running
	}
	p.mu.Unlock()
	if spare != nil {
		p.stop(spare)
		p.fills.Done()
	}
	for _, provider := range others {
		if provider != active {
			p.Refill(provider)
		}
	}
}

// Reap stops every held box, disables the pool, and waits for every in-flight Refill (called on
// supervisor exit). The container itself is also caught by the supervisor's final exact-label
// removal; this closes the box's pipes/cidfile. Returning means nothing of the pool's is still
// running — a fill that spawned mid-teardown has already stopped its own child. Idempotent, and a
// second caller observes the same completed teardown rather than starting another.
func (p *WarmPool) Reap() {
	if p == nil {
		return
	}
	p.reap.Do(func() {
		p.mu.Lock()
		p.enabled = false
		held := p.boxes
		p.boxes = map[string]*acpproxy.Child{}
		p.mu.Unlock()
		// Parked boxes are independent runs — at most one per provider — so they stop together: a
		// filtered box's own teardown takes about two seconds, and the editor waits for all of them.
		var stops sync.WaitGroup
		for _, c := range held {
			stops.Go(func() { p.stop(c) })
		}
		stops.Wait()
		p.fills.Wait()
	})
}

func (p *WarmPool) stop(c *acpproxy.Child) {
	if c != nil && c.Stop != nil {
		c.Stop()
	}
}
