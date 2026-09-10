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

// Checkout pops and returns the warm box for provider (nil if none). The caller then OWNS it —
// the pool no longer tracks or reaps it.
func (p *WarmPool) Checkout(provider string) *acpproxy.Child {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	c := p.boxes[provider]
	delete(p.boxes, provider)
	p.mu.Unlock()
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
	if err == nil && child != nil && p.enabled {
		p.boxes[provider] = child
		child = nil // ownership moved into the pool; there is nothing left for us to stop
	}
	p.mu.Unlock()
	// A failed spawn leaves the slot empty (the factory cold-spawns on the next switch); one that
	// finished after Reap disabled the pool is stopped here rather than leaked. Nil-safe.
	p.stop(child)
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
		for _, c := range held {
			p.stop(c)
		}
		p.fills.Wait()
	})
}

func (p *WarmPool) stop(c *acpproxy.Child) {
	if c != nil && c.Stop != nil {
		c.Stop()
	}
}
