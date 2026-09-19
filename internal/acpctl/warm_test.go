package acpctl

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestWarmPool(t *testing.T) {
	var mu sync.Mutex
	spawns := map[string]int{}
	stopped := map[string]int{}
	mk := func(provider string) (*acpproxy.Child, error) {
		mu.Lock()
		spawns[provider]++
		mu.Unlock()
		return &acpproxy.Child{Provider: provider, Account: "default", Image: "img", Stop: func() { mu.Lock(); stopped[provider]++; mu.Unlock() }}, nil
	}

	p := NewWarmPool(true, mk)
	// refill warms once; a second refill while one is held is a no-op (not a double-spawn).
	p.Refill("codex")
	p.Refill("codex")
	if spawns["codex"] != 1 {
		t.Errorf("refill should spawn exactly once while a box is held, got %d", spawns["codex"])
	}
	// checkout hands the box to the caller and empties the slot.
	// A box serves only the account it started on; a switch to another account leaves it parked.
	if c := p.Checkout("codex", "work", "img"); c != nil {
		t.Error("checkout handed a default-account box to a switch to another account")
	}
	if c := p.Checkout("codex", "default", "img"); c == nil {
		t.Error("checkout should return the warmed box")
	}
	if c := p.Checkout("codex", "", "img"); c != nil {
		t.Error("checkout should empty the slot")
	}
	// After checkout, refill re-populates.
	p.Refill("codex")
	if spawns["codex"] != 2 {
		t.Errorf("refill after checkout should spawn again, got %d", spawns["codex"])
	}
	// reap Stops all held boxes and clears the pool.
	p.Refill("gemini")
	p.Reap()
	if stopped["codex"] != 1 || stopped["gemini"] != 1 {
		t.Errorf("reap should Stop every held box: codex=%d gemini=%d", stopped["codex"], stopped["gemini"])
	}
	if c := p.Checkout("codex", "", "img"); c != nil {
		t.Error("reap should have cleared the pool")
	}
	// A disabled pool warms nothing.
	d := NewWarmPool(false, mk)
	d.Refill("claude")
	if c := d.Checkout("claude", "", "img"); c != nil {
		t.Error("a disabled pool must warm nothing")
	}
}

// A box from an image a rebuild replaced can serve no switch again: it is stopped rather than reused
// or kept, and an image nobody could tell ("") reuses nothing.
func TestWarmPoolStopsABoxFromAReplacedImage(t *testing.T) {
	stopped := 0
	p := NewWarmPool(true, func(provider string) (*acpproxy.Child, error) {
		return &acpproxy.Child{Provider: provider, Account: "default", Image: "img", Stop: func() { stopped++ }}, nil
	})
	p.Refill("codex")
	if c := p.Checkout("codex", "default", "rebuilt"); c != nil || stopped != 1 {
		t.Fatalf("a box from an older image was reused or kept (stopped=%d)", stopped)
	}
	p.Refill("codex")
	if c := p.Checkout("codex", "default", ""); c != nil || stopped != 1 {
		t.Fatalf("a box was reused, or stopped, without knowing the current image (stopped=%d)", stopped)
	}
	if p.Checkout("codex", "default", "img") == nil {
		t.Fatal("a box checked against an image nobody could tell was not left parked")
	}
}

// Stopping a parked box it will not lend — a spare of the active provider, or one from a replaced
// image — is teardown the pool owns: Reap must not return while that stop is still running.
func TestWarmPoolReapWaitsForAnEvictionsStop(t *testing.T) {
	stopping, release := make(chan struct{}), make(chan struct{})
	p := NewWarmPool(true, func(provider string) (*acpproxy.Child, error) {
		return &acpproxy.Child{Provider: provider, Image: "img", Stop: func() {
			if provider == "codex" {
				close(stopping)
				<-release
			}
		}}, nil
	})
	p.Refill("codex")
	go p.Rebalance("codex", nil) // codex became active: its spare is stopped
	<-stopping
	reaped := make(chan struct{})
	go func() { p.Reap(); close(reaped) }()
	select {
	case <-reaped:
		t.Fatal("Reap returned while the spare's stop was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-reaped:
	case <-time.After(wait.Deadline):
		t.Fatal("Reap never returned after the spare stopped")
	}
}

// The editor's Provider selector always resolves an account, so a switch the pool can serve is a plain
// target at the provider's default model and effort — the target a warm box runs — whatever its account.
func TestWarmSwitch(t *testing.T) {
	for _, c := range []struct {
		name string
		t    agents.Target
		ps   string
		ok   bool
		warm bool
	}{
		{"bare provider", agents.Target{Provider: "codex"}, "", true, true},
		{"selector switch: account resolved", agents.Target{Provider: "codex", Accounts: []string{"work"}}, "", true, true},
		{"the default model and effort spelled out", agents.Target{Provider: "codex", Model: "gpt-5.6", Effort: "high", Accounts: []string{"work"}}, "", true, true},
		{"another model", agents.Target{Provider: "codex", Model: "gpt-5.6-sol"}, "", true, false},
		{"another effort", agents.Target{Provider: "codex", Effort: "xhigh"}, "", true, false},
		{"two accounts", agents.Target{Provider: "codex", Accounts: []string{"work", "personal"}}, "", true, false},
		{"preset", agents.Target{Provider: "claude"}, "frontier", true, false},
		{"no target", agents.Target{}, "", false, false},
	} {
		if got := WarmSwitch(c.t, c.ps, c.ok, "gpt-5.6", "high"); got != c.warm {
			t.Errorf("%s: WarmSwitch = %v, want %v", c.name, got, c.warm)
		}
	}
}

// After a switch the pool holds one box for each provider the session is not using: the provider just
// left is warmed, and a spare of the one now active is stopped rather than kept idle.
func TestWarmPoolRebalanceCentresOnTheActiveProvider(t *testing.T) {
	var mu sync.Mutex
	spawns, stopped := map[string]int{}, map[string]int{}
	p := NewWarmPool(true, func(provider string) (*acpproxy.Child, error) {
		mu.Lock()
		spawns[provider]++
		mu.Unlock()
		return &acpproxy.Child{Provider: provider, Account: "default", Image: "img", Stop: func() { mu.Lock(); stopped[provider]++; mu.Unlock() }}, nil
	})
	p.Rebalance("claude", []string{"codex", "gemini"}) // launch: claude active
	if spawns["codex"] != 1 || spawns["gemini"] != 1 || spawns["claude"] != 0 {
		t.Fatalf("launch warmed %v, want codex and gemini only", spawns)
	}
	if c := p.Checkout("codex", "default", "img"); c == nil { // switch claude → codex, served warm
		t.Fatal("no warm codex box")
	}
	p.Rebalance("codex", []string{"claude", "gemini"})
	if spawns["claude"] != 1 || spawns["gemini"] != 1 || spawns["codex"] != 1 {
		t.Fatalf("after the switch the pool warmed %v, want the provider left (claude) and nothing twice", spawns)
	}
	p.Refill("codex") // a spare of the active provider, as a cold switch to a pinned model would leave it
	p.Rebalance("codex", []string{"claude", "gemini"})
	if stopped["codex"] != 1 || p.Checkout("codex", "", "img") != nil {
		t.Fatalf("the active provider's spare was not stopped: stopped=%v", stopped)
	}
	p.Reap()
	if stopped["claude"] != 1 || stopped["gemini"] != 1 {
		t.Fatalf("reap left parked boxes running: stopped=%v", stopped)
	}
	// A reaped or disabled pool rebalances to nothing.
	p.Rebalance("claude", []string{"codex"})
	if spawns["codex"] != 2 {
		t.Fatalf("a reaped pool spawned: %v", spawns)
	}
}

// Reap runs on supervisor exit (a deferred call plus an explicit one on the reload path, see
// cmdACPSupervise). It must not report teardown complete while a Refill is still spawning: the
// supervisor exits, and the container, cidfile and pipes that spawn just created have nobody left
// to stop them. It must also keep the pool lock OFF the injected callbacks — the real Child.Stop
// kills a process group, waits for it, and removes a container, so a fill that needs the lock to
// clean up after itself would be stuck behind that teardown.
func TestWarmPoolReapDrainsAnInFlightRefill(t *testing.T) {
	lateSpawning := make(chan struct{})
	releaseLate := make(chan struct{})
	heldStopping := make(chan struct{})
	releaseHeld := make(chan struct{})
	lateStopping := make(chan struct{})
	releaseLateStop := make(chan struct{})

	var order struct {
		sync.Mutex
		events []string
	}
	note := func(what string) {
		order.Lock()
		order.events = append(order.events, what)
		order.Unlock()
	}

	p := NewWarmPool(true, func(provider string) (*acpproxy.Child, error) {
		if provider == "late" {
			close(lateSpawning)
			<-releaseLate
		}
		return &acpproxy.Child{Image: "img", Stop: func() {
			if provider == "held" {
				close(heldStopping)
				<-releaseHeld
				return
			}
			close(lateStopping)
			<-releaseLateStop
			note("late child stopped")
		}}, nil
	})

	p.Refill("held") // the pool is holding one warm box when teardown starts

	filled := make(chan struct{})
	go func() { p.Refill("late"); close(filled) }()
	<-lateSpawning // the second fill is past the pool lock and inside spawn

	reaped := make(chan struct{})
	go func() { p.Reap(); note("reap returned"); close(reaped) }()
	<-heldStopping // Reap has disabled the pool and is stopping what it held

	// The in-flight spawn now returns a child the disabled pool refuses, so Refill has to stop it
	// itself — which needs the pool lock. Reap is still inside a Stop callback here, so it must not
	// be holding that lock.
	close(releaseLate)
	select {
	case <-lateStopping:
	case <-time.After(wait.Deadline):
		t.Fatal("the in-flight fill could not stop its own child — Reap is running a Stop callback under the pool lock")
	}

	close(releaseHeld)
	close(releaseLateStop)
	<-filled
	select {
	case <-reaped:
	case <-time.After(wait.Deadline):
		t.Fatal("Reap never returned after the in-flight fill drained")
	}

	order.Lock()
	defer order.Unlock()
	// Stopping the late child completes strictly inside Refill, so a Reap that waits for its fills
	// can only return afterwards. The reverse order is the leak: teardown "finished" with a live box.
	if want := []string{"late child stopped", "reap returned"}; !slices.Equal(order.events, want) {
		t.Fatalf("Reap must return only after the in-flight fill stopped its child, got %v", order.events)
	}
	if c := p.Checkout("late", "", "img"); c != nil {
		t.Error("a box spawned after Reap must never be stored in the pool")
	}
}

// Child.Stop and Child.SetActive are injected callbacks that reach into coop and the container
// runtime. Running one under the pool mutex serializes teardown behind it and invites a re-entrant
// deadlock — a callback that so much as consults the pool would hang forever. This test drives the
// pool from a single goroutine, so a failed TryLock can only mean the pool itself holds the lock.
func TestWarmPoolCallbacksRunOutsideTheLock(t *testing.T) {
	var p *WarmPool
	var underLock []string
	probe := func(name string) {
		if p.mu.TryLock() {
			p.mu.Unlock()
			return
		}
		underLock = append(underLock, name)
	}
	p = NewWarmPool(true, func(string) (*acpproxy.Child, error) {
		return &acpproxy.Child{
			Image:     "img",
			Stop:      func() { probe("Stop") },
			SetActive: func(bool) { probe("SetActive") },
		}, nil
	})
	p.Refill("codex")              // parks the box: SetActive(false)
	p.Checkout("codex", "", "img") // hands it to the caller: SetActive(true)
	p.Refill("codex")              // warm again, so Reap has something to stop
	p.Reap()                       // stops it: Stop()
	if len(underLock) > 0 {
		t.Fatalf("pool callbacks ran while the pool mutex was held: %v", underLock)
	}
}

// Reap is reachable twice on the same pool (the deferred supervisor-exit call plus the explicit one
// before a reload re-exec). Once the stops moved outside the lock, "each box stopped exactly once"
// stopped being a free consequence of the mutex, so pin it — including two callers racing.
func TestWarmPoolReapStopsEachBoxExactlyOnce(t *testing.T) {
	var mu sync.Mutex
	stopped := map[string]int{}
	p := NewWarmPool(true, func(provider string) (*acpproxy.Child, error) {
		return &acpproxy.Child{Stop: func() { mu.Lock(); stopped[provider]++; mu.Unlock() }}, nil
	})
	p.Refill("codex")
	p.Refill("gemini")

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); p.Reap() }()
	}
	wg.Wait()
	p.Reap() // and a third, after the fact

	mu.Lock()
	defer mu.Unlock()
	if stopped["codex"] != 1 || stopped["gemini"] != 1 {
		t.Fatalf("every held box must be stopped exactly once, got %v", stopped)
	}
}
