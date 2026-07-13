package cli

import (
	"sync"
	"testing"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
)

func TestWarmPool(t *testing.T) {
	var mu sync.Mutex
	spawns := map[string]int{}
	stopped := map[string]int{}
	mk := func(provider string) (*acpproxy.Child, error) {
		mu.Lock()
		spawns[provider]++
		mu.Unlock()
		return &acpproxy.Child{Stop: func() { mu.Lock(); stopped[provider]++; mu.Unlock() }}, nil
	}

	p := newWarmPool(true, mk)
	// refill warms once; a second refill while one is held is a no-op (not a double-spawn).
	p.refill("codex")
	p.refill("codex")
	if spawns["codex"] != 1 {
		t.Errorf("refill should spawn exactly once while a box is held, got %d", spawns["codex"])
	}
	// checkout hands the box to the caller and empties the slot.
	if c := p.checkout("codex"); c == nil {
		t.Error("checkout should return the warmed box")
	}
	if c := p.checkout("codex"); c != nil {
		t.Error("checkout should empty the slot")
	}
	// After checkout, refill re-populates.
	p.refill("codex")
	if spawns["codex"] != 2 {
		t.Errorf("refill after checkout should spawn again, got %d", spawns["codex"])
	}
	// reap Stops all held boxes and clears the pool.
	p.refill("gemini")
	p.reap()
	if stopped["codex"] != 1 || stopped["gemini"] != 1 {
		t.Errorf("reap should Stop every held box: codex=%d gemini=%d", stopped["codex"], stopped["gemini"])
	}
	if c := p.checkout("codex"); c != nil {
		t.Error("reap should have cleared the pool")
	}
	// A disabled pool warms nothing.
	d := newWarmPool(false, mk)
	d.refill("claude")
	if c := d.checkout("claude"); c != nil {
		t.Error("a disabled pool must warm nothing")
	}
}

func TestBareProviderSwitch(t *testing.T) {
	if !bareProviderSwitch(agents.Target{Provider: "codex"}, "", true) {
		t.Error("a bare provider target IS a default provider switch — should consult the pool")
	}
	for _, c := range []struct {
		name string
		t    agents.Target
		ps   string
		ok   bool
	}{
		{"account-pinned", agents.Target{Provider: "codex", Accounts: []string{"work"}}, "", true},
		{"model-pinned", agents.Target{Provider: "codex", Model: "gpt-5.6-sol"}, "", true},
		{"preset", agents.Target{Provider: "claude"}, "frontier", true},
		{"no target", agents.Target{}, "", false},
	} {
		if bareProviderSwitch(c.t, c.ps, c.ok) {
			t.Errorf("%s must NOT be a bare switch (cold-spawns)", c.name)
		}
	}
}
