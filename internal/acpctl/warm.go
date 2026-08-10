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
type WarmPool struct {
	mu       sync.Mutex
	spawn    func(provider string) (*acpproxy.Child, error)
	boxes    map[string]*acpproxy.Child
	inflight map[string]bool // a spawn is in flight for this provider (don't double-spawn)
	enabled  bool
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
	defer p.mu.Unlock()
	c := p.boxes[provider]
	delete(p.boxes, provider)
	return c
}

// Refill spawns a warm box for provider and stores it — unless the pool is disabled, one is already
// held, or a spawn is already in flight. Synchronous (the spawn runs outside the lock); the caller
// runs it in a goroutine so warming never adds startup latency. A box spawned after the pool was
// reaped is Stopped rather than leaked.
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
	p.mu.Unlock()

	child, err := p.spawn(provider)

	p.mu.Lock()
	delete(p.inflight, provider)
	switch {
	case err != nil || child == nil:
		// spawn failed — leave the slot empty; the factory cold-spawns on the next switch.
	case !p.enabled:
		p.stop(child) // reaped while we were spawning — don't leak it
	default:
		p.boxes[provider] = child
	}
	p.mu.Unlock()
}

// Reap stops every held box and disables the pool (called on supervisor exit). The container itself
// is also caught by the supervisor's final KillByLabel sweep; this closes the box's pipes/cidfile.
func (p *WarmPool) Reap() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.enabled = false
	for _, c := range p.boxes {
		p.stop(c)
	}
	p.boxes = map[string]*acpproxy.Child{}
}

func (p *WarmPool) stop(c *acpproxy.Child) {
	if c != nil && c.Stop != nil {
		c.Stop()
	}
}
