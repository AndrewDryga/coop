package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/threadtext"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// boxWaitHarness drives an acpBoxWait with its boundaries faked: what readiness answers, the signs a
// waiting session watches, the rebuild, the proxy, and what the editor and the agent log hear.
type boxWaitHarness struct {
	w *acpBoxWait

	mu       sync.Mutex
	answer   error
	checks   int
	sign     string
	holding  bool
	serving  bool
	moveOK   bool
	moves    int
	forces   int
	said     []string
	told     []string // what every open thread heard
	busyNow  error
	replaces int
	notices  []string
	logged   []error
	opened   int
	yields   int
	clock    time.Time
	rebuilds int
	rebuild  chan error // each rebuild waits for one result
}

var (
	errNeedsAPerson   = &box.ProjectBuildRequired{Dockerfile: ".agent/Dockerfile", Reason: "project build inputs or settings changed"}
	errApprovedStale  = &box.ProjectBuildRequired{Dockerfile: ".agent/Dockerfile", Reason: "no explicit project build is recorded", Approved: true}
	errCouldNotLook   = errors.New("the runtime is unavailable")
	errAnotherMissing = &box.ProjectBuildRequired{Dockerfile: ".agent/Dockerfile", Reason: "the image is missing", Approved: true}
)

func newBoxWaitHarness(t *testing.T) *boxWaitHarness {
	t.Helper()
	h := &boxWaitHarness{sign: "one", moveOK: true, clock: time.Unix(1_800_000_000, 0), rebuild: make(chan error, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.w = &acpBoxWait{
		check: func(context.Context) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.checks++
			return h.answer
		},
		rebuild: func(ctx context.Context, dfRel string) error {
			h.mu.Lock()
			h.rebuilds++
			h.mu.Unlock()
			if dfRel != ".agent/Dockerfile" {
				t.Errorf("rebuild of %q", dfRel)
			}
			select {
			case err := <-h.rebuild:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		stamp: func() string {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.sign
		},
		holding: func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.holding
		},
		serving: func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.serving
		},
		move: func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.moves++
			return h.moveOK
		},
		force: func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.forces++
			return true
		},
		publish: func(text string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.said = append(h.said, text)
		},
		announce: func(text string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.said = append(h.said, text)
			h.told = append(h.told, text)
		},
		lockBusy: func() error {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.busyNow
		},
		replace: func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.replaces++
			return true
		},
		notify: func(text string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.notices = append(h.notices, text)
		},
		log: func(err error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.logged = append(h.logged, err)
		},
		opened: func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.opened++
		},
		yield: func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.yields++
		},
		now: func() time.Time {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.clock
		},
		ctx: ctx, cancel: cancel, kicks: make(chan struct{}, 1),
	}
	return h
}

func (h *boxWaitHarness) set(change func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	change()
}

func (h *boxWaitHarness) read(look func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	look()
}

func (h *boxWaitHarness) lastSaid() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.said) == 0 {
		return ""
	}
	return h.said[len(h.said)-1]
}

// settled waits for a background rebuild to finish, leaving the wait to look again.
func (h *boxWaitHarness) settled(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		h.w.mu.Lock()
		building := h.w.building
		h.w.mu.Unlock()
		if !building {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the rebuild never finished")
		}
		time.Sleep(time.Millisecond)
	}
}

// A project whose box can start hears nothing and costs nothing.
func TestBoxWaitIsSilentWhenTheBoxCanStart(t *testing.T) {
	h := newBoxWaitHarness(t)
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if h.w.blockedNow() || len(h.said) != 0 || h.rebuilds != 0 || h.opened != 0 {
		t.Fatalf("blocked=%v said=%q rebuilds=%d opened=%d", h.w.blockedNow(), h.said, h.rebuilds, h.opened)
	}
	h.w.poll()
	if h.checks != 1 || h.moves != 0 {
		t.Fatalf("a session with nothing to wait for checked %d times and moved %d", h.checks, h.moves)
	}
}

// Changed files never build by themselves. The session says what to run, and notices when it is done.
func TestBoxWaitLeavesChangedFilesToAPerson(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.answer, h.holding = errNeedsAPerson, true })
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !h.w.blockedNow() {
		t.Fatal("a child spawned now would run the project's box")
	}
	said := h.lastSaid()
	for _, want := range []string{"needs a build", "coop build --egress filtered", "wait here"} {
		if !strings.Contains(said, want) {
			t.Fatalf("the editor heard %q, want %q in it", said, want)
		}
	}
	h.w.poll()
	if h.rebuilds != 0 {
		t.Fatal("changed files started a build with nobody asking")
	}
	if h.moves != 0 {
		t.Fatal("moved off the holding child while the box still cannot start")
	}

	// Nothing changed: no reason to read the build files again, until it has been a while.
	h.w.poll()
	if h.checks != 1 {
		t.Fatalf("%d checks with no sign of a change", h.checks)
	}
	h.set(func() { h.clock = h.clock.Add(acpBoxWaitRecheck) })
	h.w.poll()
	if h.checks != 2 {
		t.Fatalf("%d checks after the recheck interval, want 2: an undone edit leaves no other sign", h.checks)
	}

	// A person's build finishes.
	h.set(func() { h.answer, h.sign = nil, "two" })
	h.w.poll()
	if h.w.blockedNow() || h.moves != 1 || h.opened != 0 || h.lastSaid() != "" {
		t.Fatalf("blocked=%v moves=%d opened=%d said=%q", h.w.blockedNow(), h.moves, h.opened, h.lastSaid())
	}
	h.set(func() { h.holding, h.serving = false, true })
	h.w.poll()
	h.w.poll()
	if h.opened != 1 {
		t.Fatalf("opened=%d once the replacement answered, want the fan-out once", h.opened)
	}
	if len(h.logged) != 1 {
		t.Fatalf("the agent log got %v, want the reason once", h.logged)
	}
}

// The move waits for the holding child to owe the editor nothing, and keeps trying.
func TestBoxWaitRetriesTheMove(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.answer, h.holding = errNeedsAPerson, true })
	_ = h.w.refresh(t.Context())
	h.set(func() { h.answer, h.sign, h.moveOK = nil, "two", false })
	h.w.poll()
	h.w.poll()
	if h.moves != 2 {
		t.Fatalf("moves=%d, want two tries", h.moves)
	}
}

// A warm box that reaches the service launch lock ahead of the session's own box holds it shared while
// it stays parked, and the session's box then never starts. So after a wait clears, no fill starts
// until the box moved onto has answered: not when the wait clears, and not when that box is spawned.
func TestBoxWaitStartsTheSessionsBoxBeforeItsWarmBoxes(t *testing.T) {
	var order []string
	h := newBoxWaitHarness(t)
	fanOut := &acpFanOut{lead: func() string { return "claude" }, waiting: h.w.waitingNow, fill: func(string) { order = append(order, "warm") }}
	h.w.opened = func() { fanOut.rebalance("claude") }
	h.set(func() { h.answer, h.holding = errNeedsAPerson, true })
	_ = h.w.refresh(t.Context())
	fanOut.start() // the holding child answered initialize
	fanOut.rebalance("claude")
	if len(order) != 0 {
		t.Fatalf("filled while the box could not start: %v", order)
	}

	h.set(func() { h.answer, h.sign = nil, "built" })
	h.w.poll()                 // the wait clears, and the session moves
	fanOut.rebalance("claude") // the factory re-centres on the replacement it just spawned
	h.w.poll()
	if len(order) != 0 {
		t.Fatalf("filled ahead of the session's own box: %v", order)
	}

	order = append(order, "lead answered")
	h.set(func() { h.holding, h.serving = false, true })
	h.w.poll()
	if !slices.Equal(order, []string{"lead answered", "warm"}) {
		t.Fatalf("start order %v, want the warm boxes right after the session's own box", order)
	}
}

// A cold box in a project with services may have to start them, so a fill after that spawn also waits for
// its answer, even long after the fan-out opened.
func TestBoxWaitKeepsThePoolShutUntilAColdBoxAnswers(t *testing.T) {
	var fills int
	h := newBoxWaitHarness(t)
	fanOut := &acpFanOut{lead: func() string { return "claude" }, waiting: h.w.waitingNow, fill: func(string) { fills++ }}
	h.w.opened = func() { fanOut.rebalance("claude") }
	fanOut.start()
	if fills != 1 {
		t.Fatalf("%d fills once the fan-out started, want 1", fills)
	}
	h.w.awaitAnswer()
	fanOut.rebalance("claude")
	h.w.poll()
	if fills != 1 {
		t.Fatalf("%d fills ahead of the box just spawned, want the first only", fills)
	}
	h.set(func() { h.serving = true })
	h.w.poll()
	if fills != 2 {
		t.Fatalf("%d fills after it answered, want one more", fills)
	}
}

// A provider switch in an open session must not hang behind the boxes parked beside the one it took. A
// parked box that started joined the services and leaves its neighbours alone. One that has not started
// waits on the lock, and only its supervisor can stop them. The refill comes after the answer either way.
func TestWarmBoxTakenInAnOpenSessionComesBeforeItsRefill(t *testing.T) {
	for _, tc := range []struct {
		name     string
		launched func() bool
		yields   int
	}{
		{"it joined and started", func() bool { return true }, 0},
		{"it still waits for the lock", func() bool { return false }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fills := 0
			h := newBoxWaitHarness(t)
			fanOut := &acpFanOut{lead: func() string { return "claude" }, waiting: h.w.waitingNow, fill: func(string) { fills++ }}
			h.w.opened = func() { fanOut.rebalance("claude") }
			fanOut.start()
			fills = 0
			h.w.warmTaken(&acpproxy.Child{Launched: tc.launched})
			fanOut.rebalance("claude")
			h.w.poll()
			if fills != 0 {
				t.Fatalf("%d fills ahead of the box the switch took", fills)
			}
			wait.For(t, "the parked boxes to stop", func() bool {
				var yields int
				h.read(func() { yields = h.yields })
				return yields == tc.yields
			})
			h.set(func() { h.serving = true })
			h.w.poll()
			if fills != 1 {
				t.Fatalf("%d fills after it answered, want one", fills)
			}
			var yields int
			h.read(func() { yields = h.yields })
			if yields != tc.yields {
				t.Fatalf("%d parked boxes stopped, want %d", yields, tc.yields)
			}
		})
	}
}

// A box of the session that only its own parked boxes keep out says so, and the supervisor stops them.
// That word is not a wait: nothing is told to a thread, and it does not read as the end of one.
func TestBoxWaitStopsParkedBoxesWhenTheSessionsBoxNeedsTheLock(t *testing.T) {
	h := newBoxWaitHarness(t)
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	child := &acpproxy.Child{}
	h.w.follow(child, read)
	if _, err := write.Write([]byte{acpLaunchWaiting, acpLaunchOwnBoxes}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var yields int
		h.read(func() { yields = h.yields })
		if yields == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the parked boxes were never stopped")
		}
		time.Sleep(time.Millisecond)
	}
	if !h.w.waitsForLock(child) {
		t.Fatal("that word read as the end of the wait")
	}
	_ = write.Close()
}

// A connect in a project with services has no timer to start the fan-out while the lead still starts
// them. Anywhere else the timer stays, so a lead that never answers leaves the others ready. The test
// fires the timer itself, so the outcome never depends on when a real one would run.
func TestFanOutWaitsForTheLeadOnlyInAProjectWithServices(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hasServices bool
		early       int
	}{{"services", true, 0}, {"no services", false, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			fills := make(chan string, 4)
			var fire func()
			fanOut := &acpFanOut{
				lead: func() string { return "claude" }, waiting: func() bool { return false }, fill: func(active string) { fills <- active },
				after: func(grace time.Duration, start func()) func() {
					if grace != leadAnswerGrace {
						t.Errorf("fan-out armed after %v, want %v", grace, leadAnswerGrace)
					}
					fire = start
					return func() {}
				},
			}
			defer fanOut.arm(tc.hasServices, leadAnswerGrace)()
			fanOut.rebalance("claude") // the lead's own spawn
			if got := len(fills); got != 0 {
				t.Fatalf("the lead's own spawn filled %d boxes, want none", got)
			}
			if (fire != nil) == tc.hasServices {
				t.Fatalf("grace timer armed = %t in a project with services = %t", fire != nil, tc.hasServices)
			}
			if fire != nil {
				fire()
			}
			if got := len(fills); got != tc.early {
				t.Fatalf("%d fills before the lead answered, want %d", got, tc.early)
			}
			fanOut.start()
			fanOut.start()
			if got := len(fills); got != 1 {
				t.Fatalf("%d fills after the lead answered, want exactly one", got)
			}
		})
	}
}

// The thread hears the box is ready when a real box has replaced a holding child, and only then: a
// move only asks for one, and it can land on another holding child.
func TestBoxWaitSaysReadyOnlyWhenARealBoxTookOver(t *testing.T) {
	h := newBoxWaitHarness(t)
	holding, real := &acpproxy.Child{HoldPrompts: true}, &acpproxy.Child{}
	h.w.started(real)
	h.w.started(holding)
	h.w.started(holding)
	if len(h.notices) != 0 {
		t.Fatalf("said %q before a real box took over", h.notices)
	}
	h.w.started(real)
	h.w.started(real)
	if !slices.Equal(h.notices, []string{acpBoxReadyNotice}) {
		t.Fatalf("said %q, want the ready notice once", h.notices)
	}
}

// Approved files rebuild in the background, once, while the session stays connected.
func TestBoxWaitRebuildsApprovedFilesOnce(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.answer, h.holding = errApprovedStale, true })
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	said := h.lastSaid()
	if !strings.Contains(said, "Rebuilding") || strings.Contains(said, "coop build") {
		t.Fatalf("the editor heard %q: a rebuild needs nothing from the person", said)
	}
	h.set(func() { h.sign = "two" })
	h.w.poll() // still building: no new question, no second build
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.rebuild <- nil
	h.settled(t)
	h.read(func() {
		if h.rebuilds != 1 {
			t.Fatalf("%d rebuilds, want one", h.rebuilds)
		}
	})
	h.set(func() { h.answer = nil })
	h.w.poll()
	if h.w.blockedNow() || h.moves != 1 || h.lastSaid() != "" {
		t.Fatalf("blocked=%v moves=%d said=%q after the rebuild", h.w.blockedNow(), h.moves, h.lastSaid())
	}
}

// A failed rebuild is told plainly, is not retried behind the person's back, and a person's own build
// still ends the wait.
func TestBoxWaitReportsAFailedRebuildAndDoesNotRetry(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.answer, h.holding = errApprovedStale, true })
	_ = h.w.refresh(t.Context())
	failure := errors.New("the build broke")
	h.rebuild <- failure
	h.settled(t)
	h.w.poll()
	said := h.lastSaid()
	if !strings.Contains(said, "could not rebuild") || !strings.Contains(said, "coop build --egress filtered") {
		t.Fatalf("the editor heard %q", said)
	}
	h.read(func() {
		if !slices.Contains(h.logged, failure) {
			t.Fatalf("the agent log got %v, want the reason", h.logged)
		}
	})
	h.set(func() { h.clock = h.clock.Add(acpBoxWaitRecheck) })
	h.w.poll()
	h.read(func() {
		if h.rebuilds != 1 {
			t.Fatalf("%d rebuilds, want no retry", h.rebuilds)
		}
	})
	h.set(func() { h.answer, h.sign = nil, "two" })
	h.w.poll()
	if h.w.blockedNow() || h.moves != 1 {
		t.Fatalf("blocked=%v moves=%d after a person's build", h.w.blockedNow(), h.moves)
	}

	// The failure does not outlive what caused it: the next time the image goes missing, Coop tries.
	h.set(func() { h.answer = errAnotherMissing })
	_ = h.w.refresh(t.Context())
	h.rebuild <- nil
	h.settled(t)
	h.read(func() {
		if h.rebuilds != 2 {
			t.Fatalf("%d rebuilds, want a new attempt", h.rebuilds)
		}
	})
}

// Files that changed under the rebuild are a new question, not a failed build.
func TestBoxWaitTreatsAnEditDuringTheRebuildAsANewQuestion(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.answer, h.holding = errApprovedStale, true })
	_ = h.w.refresh(t.Context())
	h.set(func() { h.answer = errNeedsAPerson })
	h.rebuild <- errNeedsAPerson
	h.settled(t)
	h.w.poll()
	if said := h.lastSaid(); !strings.Contains(said, "needs a build") {
		t.Fatalf("the editor heard %q", said)
	}
	h.read(func() {
		if h.rebuilds != 1 {
			t.Fatalf("%d rebuilds of changed files", h.rebuilds)
		}
	})
}

// A failure to look proves nothing: the last answer stands, and nobody is told a box needs a build.
func TestBoxWaitIgnoresAFailureToLook(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.answer = errCouldNotLook })
	if err := h.w.refresh(t.Context()); !errors.Is(err, errCouldNotLook) {
		t.Fatalf("refresh = %v", err)
	}
	if h.w.blockedNow() || len(h.said) != 0 {
		t.Fatalf("blocked=%v said=%q", h.w.blockedNow(), h.said)
	}
	h.set(func() { h.answer = errNeedsAPerson })
	_ = h.w.refresh(t.Context())
	h.set(func() { h.answer, h.sign = errCouldNotLook, "two" })
	h.w.poll()
	if !h.w.blockedNow() {
		t.Fatal("a failure to look ended the wait")
	}

	// It costs a large project a second, so a look that failed is not repeated on every tick.
	checks := h.checks
	h.w.poll()
	if h.checks != checks {
		t.Fatal("a failed look ran again with no sign of a change")
	}
	h.set(func() { h.clock = h.clock.Add(acpBoxWaitRecheck) })
	h.w.poll()
	if h.checks != checks+1 {
		t.Fatal("a failed look never ran again")
	}
}

// What the editor is told once, it is not told again until it changes.
func TestBoxWaitSaysEachThingOnce(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.answer = errNeedsAPerson })
	_ = h.w.refresh(t.Context())
	_ = h.w.refresh(t.Context())
	if len(h.said) != 1 || len(h.logged) != 1 {
		t.Fatalf("said %q, logged %v, want each once", h.said, h.logged)
	}
}

// A rebuild needs nothing from the person, so the agent log must not tell them to run a command.
func TestBoxWaitLogsARebuildAsNothingToDo(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.answer = errApprovedStale })
	_ = h.w.refresh(t.Context())
	h.rebuild <- nil
	h.settled(t)
	h.read(func() {
		if len(h.logged) != 1 || strings.Contains(h.logged[0].Error(), "coop build --egress") || !strings.Contains(h.logged[0].Error(), "rebuilding") {
			t.Fatalf("the agent log got %v", h.logged)
		}
	})
}

// The proxy's promise and the child's image come from one reading: a child either holds prompts and
// runs the handshake box, or does neither.
func TestSpawnedChildAndProxyAgreeOnHoldingPrompts(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "environment")
	self := filepath.Join(t.TempDir(), "coop")
	if err := os.WriteFile(self, []byte("#!/bin/sh\nenv > '"+seen+".tmp'\nmv '"+seen+".tmp' '"+seen+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, blocked := range []bool{false, true} {
		h := newBoxWaitHarness(t)
		if blocked {
			h.set(func() { h.answer = errNeedsAPerson })
			_ = h.w.refresh(t.Context())
		}
		_ = os.Remove(seen)
		a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}, rt: runtime.Runtime{Name: "must-not-execute"}, rtSet: true, acpBoxWait: h.w}
		child, err := a.spawnBox(t.Context(), self, []string{"acp"}, "super", nil, agents.Target{Provider: "claude"}, "", false, io.Discard, forkspace.ExecutionRoleProbe)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		var env []byte
		for time.Now().Before(deadline) {
			if env, err = os.ReadFile(seen); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		child.Stop()
		if err != nil {
			t.Fatal(err)
		}
		if child.HoldPrompts != blocked || strings.Contains(string(env), acpHandshakeEnv+"=1") != blocked {
			t.Fatalf("blocked=%v: child.HoldPrompts=%v, env has the flag: %v", blocked, child.HoldPrompts, strings.Contains(string(env), acpHandshakeEnv))
		}
	}
}

// A capture that leaves out what its project asks for serves only a child that holds prompts, even
// if the wait has heard nothing yet: the proxy's promise and the child's flag still agree.
func TestSpawnedChildUnderACaptureThatOnlyHoldsHoldsPrompts(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir(), Egress: "filtered"}
	cfg.SetEgress("filtered")
	cfg.SetActiveProfile("claude", "selected")
	seedNativeFixture(t, cfg, "claude", "selected")
	target := agents.Target{Provider: "claude", Accounts: []string{"selected"}}
	shim := filepath.Join(t.TempDir(), "inner")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf '%s\\n' \"$"+acpHandshakeEnv+"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := newBoxWaitHarness(t)
	for name, held := range map[string]*box.CapturedEgress{
		"a request nobody approved": {Pending: &networkstate.PendingApproval{Reason: "this project asks for network access that has not been approved"}},
		"a file that does not load": {BrokenFile: true},
	} {
		held.Project, held.Fingerprint, held.QualificationID = cfg.RepoOverride, strings.Repeat("a", 64), strings.Repeat("b", 64)
		a := &app{cfg: cfg, acpNetworkTargets: []agents.Target{target}, acpCapture: held, acpBoxWait: h.w}
		child, err := a.spawnBox(t.Context(), shim, nil, "super", nil, target, "", true, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(child.Out)
		child.Stop()
		if err != nil {
			t.Fatal(err)
		}
		if h.w.blockedNow() || !child.HoldPrompts || strings.TrimSpace(string(out)) != "1" {
			t.Fatalf("%s: blocked=%v child.HoldPrompts=%v handshake flag=%q", name, h.w.blockedNow(), child.HoldPrompts, out)
		}
	}
}

// Rules nobody approved wait for that person: the thread says what to run and nothing rebuilds, and
// their approval ends the wait with no reconnect. The whole reason goes to the log, and never to the
// thread, whose words are fixed.
func TestBoxWaitLeavesPendingRulesToAPerson(t *testing.T) {
	h := newBoxWaitHarness(t)
	pending := &networkstate.PendingApproval{Reason: "the Compose service \"db\" changed since it was approved"}
	h.set(func() { h.answer = pending; h.holding = true })
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatalf("a pending request is an answer, not a failure to look: %v", err)
	}
	said := h.lastSaid()
	if !h.w.blockedNow() || !strings.Contains(said, "coop approve") || !strings.Contains(said, "no agent runs here") || strings.Contains(said, "Compose") {
		t.Fatalf("blocked=%v said=%q", h.w.blockedNow(), said)
	}
	h.read(func() {
		if len(h.logged) != 1 || !errors.Is(h.logged[0], pending) || h.rebuilds != 0 {
			t.Fatalf("logged=%v rebuilds=%d", h.logged, h.rebuilds)
		}
	})
	h.w.poll()
	h.read(func() {
		if h.moves != 0 {
			t.Fatalf("moved %d times while the request waited", h.moves)
		}
	})
	h.set(func() { h.answer = nil; h.sign = "approved" })
	h.w.poll()
	if h.w.blockedNow() || h.lastSaid() != "" {
		t.Fatalf("blocked=%v said=%q after the approval", h.w.blockedNow(), h.lastSaid())
	}
	h.read(func() {
		if h.moves != 1 || h.opened != 0 {
			t.Fatalf("moves=%d opened=%d, want the session moved by itself, and no warm box ahead of it", h.moves, h.opened)
		}
	})
}

// Services with no rules wait for that person too. The thread hears the remedy in parts it can show
// whole, the log hears it as a terminal would, and settling it ends the wait with no reconnect.
func TestBoxWaitLeavesServiceRulesToAPerson(t *testing.T) {
	h := newBoxWaitHarness(t)
	refusal := servicesRefusal(t, "services:\n  db:\n    image: postgres:18\n    expose: [\"5432\"]\n", "")
	h.set(func() { h.answer = refusal; h.holding = true })
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatalf("services with no rules are an answer, not a failure to look: %v", err)
	}
	if said := h.lastSaid(); !h.w.blockedNow() || said != acpBoxWaitText(refusal, nil, false) {
		t.Fatalf("blocked=%v said=%q", h.w.blockedNow(), said)
	}
	h.read(func() {
		if len(h.logged) != 1 || !strings.Contains(h.logged[0].Error(), "Then review it: coop approve") || h.rebuilds != 0 {
			t.Fatalf("logged=%v rebuilds=%d", h.logged, h.rebuilds)
		}
	})
	h.set(func() { h.answer = nil; h.sign = "settled" })
	h.w.poll()
	if h.w.blockedNow() || h.lastSaid() != "" {
		t.Fatalf("blocked=%v said=%q once it was settled", h.w.blockedNow(), h.lastSaid())
	}
	h.read(func() {
		if h.moves != 1 || h.opened != 0 {
			t.Fatalf("moves=%d opened=%d, want the session moved by itself, and no warm box ahead of it", h.moves, h.opened)
		}
	})
}

// An approved mode the session cannot move to has one remedy, and it is not another approval.
func TestBoxWaitSaysToReconnectForAModeItCannotMoveTo(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.answer = errACPNetworkModeChanged })
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if said := h.lastSaid(); !h.w.blockedNow() || !strings.Contains(said, "Reconnect your editor") || strings.Contains(said, "coop approve") {
		t.Fatalf("blocked=%v said=%q", h.w.blockedNow(), said)
	}
}

// A real box that exits as it starts puts the session on a holding child, whatever the reason was. The
// thread hears it once, retries are spaced out, and only a box that keeps running ends it.
func TestBoxWaitHoldsAfterAFailedStartAndRetries(t *testing.T) {
	h := newBoxWaitHarness(t)
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.w.childFailed(acpproxy.ChildExited)
	said := h.lastSaid()
	if !h.w.blockedNow() || !h.w.waitingNow() || !strings.Contains(said, "could not start") || !strings.Contains(said, "agent log") {
		t.Fatalf("blocked=%v waiting=%v said=%q", h.w.blockedNow(), h.w.waitingNow(), said)
	}
	h.set(func() { h.holding = true })

	// Nothing changed and it is too soon: no retry.
	h.w.poll()
	h.read(func() {
		if h.moves != 0 || h.checks != 1 {
			t.Fatalf("a tight retry: %d moves, %d checks", h.moves, h.checks)
		}
	})

	// Every thirty seconds. The warm pool stays shut while one box is being tried.
	h.set(func() { h.clock = h.clock.Add(acpBoxWaitRecheck) })
	h.w.poll()
	h.read(func() {
		if h.moves != 1 || h.opened != 0 {
			t.Fatalf("moves=%d opened=%d, want one real box tried and no fan-out", h.moves, h.opened)
		}
	})
	if h.w.blockedNow() || !h.w.waitingNow() || h.lastSaid() != said {
		t.Fatalf("blocked=%v waiting=%v said=%q: a retry must not read as a recovery", h.w.blockedNow(), h.w.waitingNow(), h.lastSaid())
	}

	// It failed again. The thread already knows, and the log has the child's own output.
	h.set(func() { h.holding = false })
	h.w.childFailed(acpproxy.ChildExited)
	h.read(func() {
		if len(h.said) != 1 || len(h.logged) != 1 {
			t.Fatalf("said=%q logged=%v, want each once however many retries fail", h.said, h.logged)
		}
	})
	if !h.w.blockedNow() {
		t.Fatal("a child spawned after a failed retry would be a real box")
	}

	// A change to the project is worth a retry at once.
	h.set(func() { h.holding, h.sign = true, "two" })
	h.w.poll()
	h.read(func() {
		if h.moves != 2 {
			t.Fatalf("moves=%d, want the changed project tried", h.moves)
		}
	})

	// The box answered, but a moment later it could still count as failing to start.
	h.set(func() { h.holding, h.serving = false, true })
	h.w.poll()
	if !h.w.waitingNow() {
		t.Fatal("ended the failed start before the window passed")
	}
	h.set(func() { h.clock = h.clock.Add(acpproxy.ChildStartWindow) })
	h.w.poll()
	if h.w.waitingNow() || h.lastSaid() != "" {
		t.Fatalf("waiting=%v said=%q after a box kept running", h.w.waitingNow(), h.lastSaid())
	}
	h.read(func() {
		if h.opened != 1 || len(h.notices) != 1 || h.notices[0] != acpBoxReadyNotice {
			t.Fatalf("opened=%d notices=%q", h.opened, h.notices)
		}
	})
}

// A box that never answers writes no reason of its own, so the thread hears what Coop saw. It is
// retried as one that exits is, and each way of failing is said once.
func TestBoxWaitSaysWhenABoxDidNotAnswer(t *testing.T) {
	h := newBoxWaitHarness(t)
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.w.childFailed(acpproxy.ChildSilent)
	if said := h.lastSaid(); !h.w.blockedNow() || !h.w.waitingNow() || !strings.Contains(said, "did not answer") || !strings.Contains(said, "a minute") {
		t.Fatalf("blocked=%v waiting=%v said=%q", h.w.blockedNow(), h.w.waitingNow(), said)
	}
	h.read(func() {
		if len(h.logged) != 1 || !strings.Contains(h.logged[0].Error(), "did not answer") {
			t.Fatalf("logged=%v", h.logged)
		}
	})

	h.set(func() { h.holding, h.clock = true, h.clock.Add(acpBoxWaitRecheck) })
	h.w.poll()
	h.read(func() {
		if h.moves != 1 || h.opened != 0 {
			t.Fatalf("moves=%d opened=%d, want one real box tried and no fan-out", h.moves, h.opened)
		}
	})
	if h.w.blockedNow() {
		t.Fatal("the retry's child would hold prompts")
	}

	// It hung again: the thread already knows.
	h.w.childFailed(acpproxy.ChildSilent)
	h.read(func() {
		if len(h.said) != 1 || len(h.logged) != 1 {
			t.Fatalf("said=%q logged=%v, want each once however many retries hang", h.said, h.logged)
		}
	})
	if !h.w.blockedNow() {
		t.Fatal("a child spawned after a hung retry would be a real box")
	}

	// Failing another way is news, and only a box still getting ready may have been waiting on something.
	h.w.childFailed(acpproxy.ChildNotReady)
	if said := h.lastSaid(); !strings.Contains(said, "took too long") || !strings.Contains(said, "another box") {
		t.Fatalf("said=%q", said)
	}
	h.w.childFailed(acpproxy.ChildExited)
	if said := h.lastSaid(); !strings.Contains(said, "could not start") {
		t.Fatalf("said=%q", said)
	}
	h.read(func() {
		if len(h.said) != 3 || len(h.logged) != 3 {
			t.Fatalf("said=%q logged=%v", h.said, h.logged)
		}
	})
}

// What a person must settle comes first, and once it is settled the thread hears the hang again.
func TestBoxWaitPutsAHungBoxBehindWhatReadinessSays(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.w.childFailed(acpproxy.ChildSilent)
	h.set(func() { h.answer, h.holding, h.clock = errNeedsAPerson, true, h.clock.Add(acpBoxWaitRecheck) })
	h.w.poll()
	if said := h.lastSaid(); !strings.Contains(said, "needs a build") {
		t.Fatalf("said=%q", said)
	}
	h.set(func() { h.answer, h.sign = nil, "built" })
	h.w.poll()
	if said := h.lastSaid(); !strings.Contains(said, "did not answer") {
		t.Fatalf("said=%q, want the hang reported until a box keeps running", said)
	}
}

// A filtered child says when its box starts, through a file only its supervisor named: the proxy gives
// a box a minute from then, not from the spawn.
func TestSpawnedFilteredChildSaysWhenItsBoxStarts(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir(), Egress: "filtered"}
	cfg.SetEgress("filtered")
	cfg.SetActiveProfile("claude", "selected")
	seedNativeFixture(t, cfg, "claude", "selected")
	target := agents.Target{Provider: "claude", Accounts: []string{"selected"}}
	shim := filepath.Join(t.TempDir(), "inner")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nread go\n: > \"$"+acpLaunchedEnv+"\"\nread stop\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := newBoxWaitHarness(t)
	held := &box.CapturedEgress{Project: cfg.RepoOverride, Fingerprint: strings.Repeat("a", 64), QualificationID: strings.Repeat("b", 64),
		Pending: &networkstate.PendingApproval{Reason: "this project asks for network access that has not been approved"}}
	a := &app{cfg: cfg, rt: runtime.Runtime{Name: fakeDocker(t)}, rtSet: true, acpNetworkTargets: []agents.Target{target}, acpCapture: held, acpBoxWait: h.w}
	child, err := a.spawnBox(t.Context(), shim, nil, "super", nil, target, "", true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Stop()
	if child.Launched == nil || child.Launched() {
		t.Fatal("the child reported its box before it started")
	}
	if _, err := child.In.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	wait.For(t, "the child to report its box", child.Launched)
}

// An open child reports nothing itself: the cidfile it already writes says when its box has started,
// which is all a session needs when it takes a parked box (acpBoxWait.warmTaken).
func TestSpawnedOpenChildLaunchIsItsCidfile(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "inner")
	seen := filepath.Join(t.TempDir(), "environment")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nenv > '"+seen+".tmp'\nmv '"+seen+".tmp' '"+seen+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}, rt: runtime.Runtime{Name: fakeDocker(t)}, rtSet: true}
	child, err := a.spawnBox(t.Context(), shim, []string{"acp"}, "super", nil, agents.Target{Provider: "claude"}, "", false, io.Discard, forkspace.ExecutionRoleProbe)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Stop()
	wait.For(t, "the child to record its environment", func() bool {
		_, err := os.Stat(seen)
		return err == nil
	})
	env, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), acpLaunchedEnv) {
		t.Fatal("an open child was asked to report its launch")
	}
	if child.Launched == nil || child.Launched() {
		t.Fatal("an open child read as started before its box was")
	}
	found := regexp.MustCompile(`(?m)^COOP_ACP_CIDFILE=(.+)$`).FindSubmatch(env)
	if found == nil {
		t.Fatalf("no cidfile in its environment:\n%s", env)
	}
	if err := os.WriteFile(string(found[1]), []byte("cid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !child.Launched() {
		t.Fatal("an open child whose box started read as not started")
	}
}

// fakeDocker is a docker that answers nothing: these spawn no real box, and stopping one must not go
// looking on the machine's real Docker.
func fakeDocker(t *testing.T) string {
	t.Helper()
	docker := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return docker
}

// Only a supervisor's child reports, to the path its supervisor named, and only at the launch boundary.
func TestACPReportLaunchWritesTheSupervisorsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launched")
	t.Setenv(acpLaunchedEnv, path)
	t.Setenv("COOP_ACP_INNER", "")
	var spec box.RunSpec
	acpReportLaunch(&spec)
	if spec.OnRuntimeLaunch != nil {
		t.Fatal("a process no supervisor started reports its launch")
	}
	t.Setenv("COOP_ACP_INNER", "1")
	acpReportLaunch(&spec)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("reported before the launch: %v", err)
	}
	spec.OnRuntimeLaunch()
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// A box that started but never answered is not proof, however long it has been running.
func TestBoxWaitEndsAFailedStartOnlyOnABoxThatAnswered(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.w.childFailed(acpproxy.ChildExited)
	h.set(func() { h.holding, h.clock = true, h.clock.Add(acpBoxWaitRecheck) })
	h.w.poll()
	h.set(func() { h.holding, h.clock = false, h.clock.Add(10*acpproxy.ChildStartWindow) })
	h.w.poll()
	if !h.w.waitingNow() {
		t.Fatal("a box still starting ended the wait")
	}
}

// The first real box to take over after a failed start is not announced at spawn: it may exit too.
func TestBoxWaitStaysQuietAboutABoxBeingTriedAgain(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.w.started(&acpproxy.Child{HoldPrompts: true})
	h.w.childFailed(acpproxy.ChildExited)
	h.w.started(&acpproxy.Child{})
	h.read(func() {
		if len(h.notices) != 0 {
			t.Fatalf("notices=%q", h.notices)
		}
	})
}

// Readiness still has its say. While something a person must settle stands in the way, no real box is
// tried, and the thread hears that instead. Once it is settled, a real box may be tried again.
func TestBoxWaitPutsAFailedStartBehindWhatReadinessSays(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.w.childFailed(acpproxy.ChildExited)
	h.set(func() { h.answer, h.holding, h.clock = errNeedsAPerson, true, h.clock.Add(acpBoxWaitRecheck) })
	h.w.poll()
	if said := h.lastSaid(); !h.w.blockedNow() || !strings.Contains(said, "needs a build") {
		t.Fatalf("blocked=%v said=%q", h.w.blockedNow(), said)
	}
	h.read(func() {
		if h.moves != 0 {
			t.Fatalf("moved %d times past what a person must settle", h.moves)
		}
	})
	h.set(func() { h.answer, h.sign = nil, "built" })
	h.w.poll()
	if said := h.lastSaid(); h.w.blockedNow() || !strings.Contains(said, "could not start") {
		t.Fatalf("blocked=%v said=%q, want a retry and the failed start still reported", h.w.blockedNow(), said)
	}
	h.read(func() {
		if h.moves != 1 || h.opened != 0 {
			t.Fatalf("moves=%d opened=%d", h.moves, h.opened)
		}
	})
}

// A spawn's own question must not start a retry: a holding child that dies would otherwise be followed
// by a real box, which is the tight loop this all exists to avoid.
func TestBoxWaitRetriesOnlyFromThePollLoop(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.w.childFailed(acpproxy.ChildExited)
	h.set(func() { h.clock = h.clock.Add(time.Hour) })
	if err := h.w.refreshStale(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !h.w.blockedNow() {
		t.Fatal("a spawn's question let the next child be a real box")
	}
}

// A holding child that never finishes what the editor asked cannot stop the move for good: a person
// finishing a sign-in has the deadline, and a hung adapter gets replaced. What it says goes to the log.
func TestBoxWaitForcesTheMoveAtItsDeadline(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.holding, h.moveOK = true, false })
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.w.poll()
	h.set(func() { h.clock = h.clock.Add(acpBoxWaitMoveDeadline - time.Second) })
	h.w.poll()
	h.read(func() {
		if h.moves != 2 || h.forces != 0 {
			t.Fatalf("moves=%d forces=%d before the deadline", h.moves, h.forces)
		}
	})
	h.set(func() { h.clock = h.clock.Add(time.Second) })
	h.w.poll()
	h.read(func() {
		if h.forces != 1 || len(h.logged) != 1 {
			t.Fatalf("forces=%d logged=%v at the deadline", h.forces, h.logged)
		}
	})
	h.w.poll()
	h.read(func() {
		if h.forces != 1 {
			t.Fatalf("forces=%d: the clock starts again after a forced move", h.forces)
		}
	})
}

// The deadline is for wanting to move. A session that is blocked is not wanting anything yet, and a
// move that works leaves nothing to count from.
func TestBoxWaitCountsTheDeadlineOnlyWhileAMoveIsWanted(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.holding, h.moveOK = true, false })
	_ = h.w.refresh(t.Context())
	h.w.poll()
	h.set(func() { h.answer, h.sign = errNeedsAPerson, "edited" })
	_ = h.w.refresh(t.Context())
	h.w.poll()
	h.set(func() { h.clock = h.clock.Add(2 * acpBoxWaitMoveDeadline) })
	h.w.poll()
	h.set(func() { h.answer, h.sign = nil, "fixed" })
	h.w.poll()
	h.read(func() {
		if h.forces != 0 {
			t.Fatalf("forces=%d: it just became possible to move", h.forces)
		}
	})
}

// A child that holds prompts runs under rules with none of the project's in them, so a broken sidecar
// or a changed service cannot stop it, and it proves a reference to those, for this start alone.
func TestSpawnedHoldingChildRunsUnderItsOwnRules(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir(), Egress: "filtered"}
	cfg.SetEgress("filtered")
	cfg.SetActiveProfile("claude", "selected")
	seedNativeFixture(t, cfg, "claude", "selected")
	target := agents.Target{Provider: "claude", Accounts: []string{"selected"}}
	shim := filepath.Join(t.TempDir(), "inner")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf '%s\\n' \"$COOP_NETWORK_CAPTURE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	const real, holding, qualification = "aaaa", "bbbb", "cccc"
	capture := func(fingerprint string) *box.CapturedEgress {
		return &box.CapturedEgress{Project: cfg.RepoOverride, Fingerprint: strings.Repeat(fingerprint, 16), QualificationID: strings.Repeat(qualification, 16)}
	}
	spawn := func(a *app) (*acpproxy.Child, string, error) {
		t.Helper()
		child, err := a.spawnBox(t.Context(), shim, nil, "super", nil, target, "", true, io.Discard)
		if err != nil {
			return nil, "", err
		}
		out, readErr := io.ReadAll(child.Out)
		child.Stop()
		if readErr != nil {
			t.Fatal(readErr)
		}
		return child, string(out), nil
	}

	// The real box proves the real rules, and nothing asks for the others.
	admits := 0
	h := newBoxWaitHarness(t)
	a := &app{cfg: cfg, acpNetworkTargets: []agents.Target{target}, acpCapture: capture(real), acpBoxWait: h.w,
		acpHoldAdmission: func(context.Context) (*box.CapturedEgress, error) { admits++; return capture(holding), nil }}
	child, out, err := spawn(a)
	if err != nil || child.HoldPrompts || !strings.Contains(out, strings.Repeat(real, 16)) || admits != 0 {
		t.Fatalf("a real box: err=%v holds=%v out=%q admits=%d", err, child.HoldPrompts, out, admits)
	}

	// A holding child proves its own, and says so for the refresh and the warm pool.
	h.w.childFailed(acpproxy.ChildExited)
	child, out, err = spawn(a)
	if err != nil || !child.HoldPrompts || !strings.Contains(out, strings.Repeat(holding, 16)) || strings.Contains(out, strings.Repeat(real, 16)) || child.Policy != strings.Repeat(holding, 16) || admits != 1 {
		t.Fatalf("a holding child: err=%v holds=%v policy=%q out=%q admits=%d", err, child.HoldPrompts, child.Policy, out, admits)
	}

	// With none to run under, nothing starts, and the reason is the session's to end on.
	a.acpHoldAdmission = func(context.Context) (*box.CapturedEgress, error) { return nil, errCouldNotLook }
	if _, _, err := spawn(a); !errors.Is(err, errCouldNotLook) {
		t.Fatalf("no rules for a holding child = %v", err)
	}

	// A session that connected on a holding capture already has its rules.
	held := capture(holding)
	held.Pending = &networkstate.PendingApproval{Reason: "asks"}
	a.acpCapture = held
	child, out, err = spawn(a)
	if err != nil || !child.HoldPrompts || !strings.Contains(out, strings.Repeat(holding, 16)) {
		t.Fatalf("a held capture: err=%v holds=%v out=%q", err, child.HoldPrompts, out)
	}
}

// A connect that cannot look says nothing either way: the first child's own proofs decide, and the
// wait takes over if they refuse. An editor is not told the server failed for it.
func TestACPConnectsWhenItCannotLookAtTheBox(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.answer = errCouldNotLook })
	a := &app{cfg: openNetwork(&config.Config{}), acpBoxWait: h.w, acpCapture: &box.CapturedEgress{}}
	a.cfg.SetEgress("filtered")
	if err := a.ensureACPImage(); err != nil {
		t.Fatalf("connect = %v", err)
	}
	if h.w.blockedNow() {
		t.Fatal("a failure to look blocked the first real box")
	}
	// Every session has a wait now, so the wait no longer stands for the policy.
	a.acpCapture = nil
	if err := a.ensureACPImage(); err == nil || !strings.Contains(err.Error(), "needs the policy") {
		t.Fatalf("a filtered session with no admitted policy connected: %v", err)
	}
}

func writeBrokenProjectFile(t *testing.T, repo string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte("box: [broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A project file an agent broke stops a real box and must not stop the child that says so: the child
// holding prompts reads nothing from it, and neither a job nor a holding child needs the project's image.
func TestAHoldingChildResolvesItsImageWhateverTheProjectFileSays(t *testing.T) {
	repo := t.TempDir()
	writeBrokenProjectFile(t, repo)
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir(), RepoOverride: repo, BaseImage: "base"}, rt: runtime.Runtime{Name: "must-not-execute"}, rtSet: true}
	if _, _, err := a.resolveLaunchImage(); err == nil {
		t.Fatal("a real launch accepted a broken project file")
	}
	t.Setenv("COOP_ACP_INNER", "1")
	if _, err := a.dispatch([]string{"acp", "claude"}); err == nil || !strings.Contains(err.Error(), "project.yaml") {
		t.Fatalf("a child that would run the project's box, on a broken project file = %v", err)
	}
	t.Setenv(acpHandshakeEnv, "1")
	got, img, err := a.resolveLaunchImage()
	if err != nil || img != "base" {
		t.Fatalf("a holding child: repo=%q image=%q err=%v", got, img, err)
	}
	if _, err := a.dispatch([]string{"acp", "claude"}); err != nil && strings.Contains(err.Error(), "project.yaml") {
		t.Fatalf("a holding child stopped at the project file: %v", err)
	}
}

// An editor connects whatever the file says: the supervisor gets as far as admission, which holds the
// session on a capture that has nothing of the file's, and starts the session. Admitting again refuses
// until the file is fixed, and any other launch still stops at the file.
func TestACPConnectsOnABrokenProjectFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}
	signInCred(t, cfg, "claude", "default")
	writeBrokenProjectFile(t, cfg.RepoOverride)
	var options []box.NetworkAdmission
	supervised := false
	a := &app{cfg: cfg, rt: runtime.Runtime{Name: "must-not-execute"}, rtSet: true,
		admitNetwork: func(c *config.Config, _ runtime.Runtime, _ box.RunSpec, opts box.NetworkAdmission) (*box.CapturedEgress, error) {
			options = append(options, opts)
			c.SetEgress("filtered")
			return &box.CapturedEgress{Project: cfg.RepoOverride, Fingerprint: strings.Repeat("a", 64), BrokenFile: true}, nil
		},
		acpSupervise: func([]string, *acpctl.Control) (int, error) { supervised = true; return 0, nil },
	}
	if code, err := a.dispatch([]string{"acp", "claude"}); code != 0 || err != nil || !supervised {
		t.Fatalf("coop acp on a broken file = (%d, %v), supervised=%v", code, err, supervised)
	}
	if len(options) != 1 || !options[0].HoldPending {
		t.Fatalf("admission options = %+v, want a connection that may hold", options)
	}
	if _, err := a.acpAdmission(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(options) != 2 || options[1].HoldPending {
		t.Fatalf("admission options = %+v, want admitting again to refuse a file still broken", options)
	}
	for _, sub := range []string{"run", "shell", "up", "down", "build"} {
		other := &app{cfg: cfg, rt: runtime.Runtime{Name: "must-not-execute"}, rtSet: true}
		if _, err := other.dispatch([]string{sub}); err == nil || !strings.Contains(err.Error(), "project.yaml") {
			t.Fatalf("coop %s on a broken file = %v", sub, err)
		}
	}

	// Where admission cannot hold, the session ends on what it said, before anything starts.
	supervised = false
	a.admitNetwork = func(*config.Config, runtime.Runtime, box.RunSpec, box.NetworkAdmission) (*box.CapturedEgress, error) {
		_, err := project.Load(cfg.RepoOverride)
		return nil, &box.BrokenProjectFile{Err: err}
	}
	if code, err := a.dispatch([]string{"acp", "claude", "--egress", "offline"}); code == 0 || err == nil || !strings.Contains(err.Error(), "project.yaml") || supervised {
		t.Fatalf("coop acp that cannot hold = (%d, %v), supervised=%v", code, err, supervised)
	}
}

// The thread says what is wrong and where, in the shape every thread message has, and says it again when
// it changes. No build starts, no box moves, and the fixed file ends the wait by itself.
func TestBoxWaitSaysWhatIsWrongWithTheProjectFile(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() {
		h.answer = &box.BrokenProjectFile{Err: errors.New(".agent/project.yaml: yaml: line 3: did not find expected key")}
		h.holding = true
	})
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatalf("a broken file is an answer, not a failure to look: %v", err)
	}
	if !h.w.blockedNow() {
		t.Fatal("a child spawned now would run a box on a file that does not load")
	}
	said := h.lastSaid()
	threadtext.Check(t, said)
	for _, want := range []string{"project\\.yaml has a problem", "line 3: did not find expected key", "Fix the file", "no agent runs here", "wait here"} {
		if !strings.Contains(said, want) {
			t.Fatalf("the thread heard %q, want %q in it", said, want)
		}
	}
	h.w.poll()
	h.read(func() {
		if h.moves != 0 || h.rebuilds != 0 || len(h.logged) != 1 {
			t.Fatalf("moves=%d rebuilds=%d logged=%v", h.moves, h.rebuilds, h.logged)
		}
	})

	// Another mistake, seen by itself: the thread hears the new one, once.
	h.set(func() {
		h.answer, h.sign = &box.BrokenProjectFile{Err: errors.New(".agent/project.yaml: serve port 70000 out of range (1-65535)")}, "edited"
	})
	h.w.poll()
	h.w.poll()
	h.read(func() {
		if len(h.said) != 2 || !strings.Contains(h.said[1], "70000") || len(h.logged) != 2 {
			t.Fatalf("said=%q logged=%v", h.said, h.logged)
		}
	})

	// Fixed. Waiting on an approval instead is the same wait, in other words.
	pending := &networkstate.PendingApproval{Reason: "this project asks for network access that has not been approved"}
	h.set(func() { h.answer, h.sign = pending, "fixed" })
	h.w.poll()
	if said := h.lastSaid(); !h.w.blockedNow() || !strings.Contains(said, "coop approve") || strings.Contains(said, "project\\.yaml") {
		t.Fatalf("blocked=%v said=%q, want the approval, and no word of the file", h.w.blockedNow(), said)
	}
	h.set(func() { h.answer, h.sign = nil, "approved" })
	unblocked := 0
	h.w.unblocked = func() { unblocked++ }
	h.w.poll()
	if h.w.blockedNow() || h.lastSaid() != "" {
		t.Fatalf("blocked=%v said=%q after the fix", h.w.blockedNow(), h.lastSaid())
	}
	h.read(func() {
		if h.moves != 1 || h.opened != 0 || unblocked != 1 {
			t.Fatalf("moves=%d opened=%d unblocked=%d, want the session moved by itself, what the file says read first, and no warm box ahead of it", h.moves, h.opened, unblocked)
		}
	})
}

// A fixed file admission still refuses is not called broken: the agent log has the refusal.
func TestBoxWaitStopsBlamingAFileThatLoads(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.answer = &box.BrokenProjectFile{Err: errors.New("no")} })
	_ = h.w.refresh(t.Context())
	h.set(func() { h.answer, h.sign = box.ErrProjectNotAdmitted, "fixed" })
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatalf("refresh = %v", err)
	}
	if said := h.lastSaid(); !h.w.blockedNow() || !strings.Contains(said, "could not start") || strings.Contains(said, "project\\.yaml") {
		t.Fatalf("blocked=%v said=%q", h.w.blockedNow(), said)
	}
}

// Anyone who can edit the project writes this text, and an error can quote it. Whatever it holds wraps
// as ordinary text: it opens no code block, which an editor would not wrap, draws no link, image or
// heading, cannot act on a terminal, stays in its one line, and a megabyte of it stays out of the thread.
func TestBrokenFileTextKeepsTheFilesWordsInert(t *testing.T) {
	_, quoted := project.Parse([]byte("\"evil\\n[x](https://evil.example)\\n```\": 1\n"))
	if quoted == nil {
		t.Fatal("fixture: the file loaded")
	}
	hostile := "**bold** [link](https://evil.example) ![img](https://evil.example/a.png) <img src=x> www.evil.example \x1b[31m\x1b]0;title\x07 \u202eevil \u200bword coop approve"
	unescaped := regexp.MustCompile(`(^|[^\\])[\[\]()<>*_` + "`" + `#!]|[^\\]\.[^\s\\]|[^\\]:/`)
	for name, err := range map[string]error{
		"what yaml quotes":      quoted,
		"markdown and controls": errors.New(hostile),
		"a fence":               errors.New("before\n```\n\n" + hostile + "\n\n````\nafter"),
		"a heading underline":   errors.New("before\n---\nafter\n===\n# heading"),
		"a line of its own":     errors.New("- item\n\n    indented\n> quote"),
		"a megabyte":            errors.New(strings.Repeat("`[x](y) ", 1<<17)),
	} {
		t.Run(name, func(t *testing.T) {
			text := acpBoxWaitText(&box.BrokenProjectFile{Err: err}, nil, false)
			paragraphs := strings.Split(text, "\n\n")
			reason := paragraphs[len(paragraphs)-1]
			if !strings.Contains(text, "has a problem") || !strings.HasPrefix(reason, "What it says: ") {
				t.Fatalf("the file's words do not follow the fixed ones:\n%s", text)
			}
			if strings.Contains(text, "```") {
				t.Fatalf("a code block, which an editor does not wrap:\n%s", text)
			}
			if len(paragraphs) != 3 || strings.Contains(reason, "\n") {
				t.Fatalf("the file's words started a line of their own:\n%s", text)
			}
			if bad := unescaped.FindString(strings.TrimPrefix(reason, "What it says: ")); bad != "" {
				t.Fatalf("%q is still Markdown in\n%s", bad, text)
			}
			for _, r := range text {
				if r < 0x20 && r != '\n' || r == 0x7f || r == 0x202E || r == 0x200B {
					t.Fatalf("a hidden character %U reached the thread:\n%q", r, text)
				}
			}
			// Escaping at most doubles what is shown, and the fixed words are short.
			if len([]rune(text)) > 3*acpBrokenFileReasonLimit {
				t.Fatalf("the thread got %d characters of the file", len([]rune(text)))
			}
		})
	}
	// yaml's own line breaks read as spaces, not as escape codes.
	if got := brokenFileReason(errors.New("yaml: unmarshal errors:\n  line 3: no")); got != "yaml: unmarshal errors: line 3: no" {
		t.Fatalf("lines = %q", got)
	}
	// A long reason is what a code block would have run off the panel's edge with.
	threadtext.Check(t, acpBoxWaitText(&box.BrokenProjectFile{Err: errors.New(strings.Repeat("did not find expected key ", 30))}, nil, false))
	if got := brokenFileReason(errors.New(strings.Repeat("é", acpBrokenFileReasonLimit+1))); !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Fatalf("cut short = %q", got)
	}
}

// A spawn reuses an answer it just got: the question reads a large project's build files.
func TestBoxWaitSpawnUsesARecentAnswer(t *testing.T) {
	h := newBoxWaitHarness(t)
	_ = h.w.refresh(t.Context())
	_ = h.w.refreshStale(t.Context())
	if h.checks != 1 {
		t.Fatalf("%d checks, want the recent answer reused", h.checks)
	}
	h.set(func() { h.clock = h.clock.Add(acpBoxWaitFresh) })
	_ = h.w.refreshStale(t.Context())
	if h.checks != 2 {
		t.Fatalf("%d checks, want a stale answer asked again", h.checks)
	}
}

// A session that ends stops its rebuild and waits for it: a build must not outlive its session.
func TestBoxWaitCloseStopsTheRebuild(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.answer = errApprovedStale })
	_ = h.w.refresh(t.Context())
	h.w.start()
	h.w.close()
	h.w.close() // safe twice
	h.w.mu.Lock()
	building, failed := h.w.building, h.w.failed
	h.w.mu.Unlock()
	if building || failed {
		t.Fatalf("building=%v failed=%v: stopping is nobody's failure", building, failed)
	}
	// A spawn already on its way must not start a build the session will not wait for.
	rebuilds := h.rebuilds
	_ = h.w.refresh(t.Context())
	h.w.mu.Lock()
	building = h.w.building
	h.w.mu.Unlock()
	if building || h.rebuilds != rebuilds {
		t.Fatalf("a closed wait started a rebuild: building=%v", building)
	}
}

// A process with no session, such as a model probe, has no wait, and every call on it is safe.
func TestNoSessionMeansNoBoxWait(t *testing.T) {
	var w *acpBoxWait
	if w.blockedNow() || w.refresh(t.Context()) != nil || w.refreshStale(t.Context()) != nil || w.childFailedHook() != nil {
		t.Fatal("a nil wait reported something")
	}
	w.start()
	w.close()
	w.started(&acpproxy.Child{})
}

// An open or offline session waits for the lock and nothing else. A real box that keeps failing there
// is still the proxy's to count, as it always was.
func TestAnUnfilteredSessionWaitsOnlyForTheLock(t *testing.T) {
	repo, cancelled := t.TempDir(), func() {}
	defer func() { cancelled() }()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"), []byte("services:\n  db:\n    image: postgres:18\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{Egress: "open", AutoUp: true, Network: true}, rt: runtime.Runtime{Name: "docker"}}
	w := a.newACPBoxWait(&acpctl.Control{}, &acpproxy.Restarter{}, box.RunSpec{Repo: repo}, func() {}, func() {})
	cancelled = w.cancel
	if w.childFailedHook() != nil {
		t.Fatal("an unfiltered session would hold prompts for a real box that fails")
	}
	if err := w.check(t.Context()); err != nil {
		t.Fatalf("a free lock read as %v", err)
	}
	free := w.stamp()
	release, err := forkspace.LockServiceLaunch(t.Context(), repo, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var busy *box.ServiceLaunchBusy
	if err := w.check(t.Context()); !errors.As(err, &busy) {
		t.Fatalf("a taken lock read as %v", err)
	}
	if w.stamp() == free {
		t.Fatal("a waiting session would not notice the lock coming free")
	}
}

func TestACPChildEnvNeverInheritsAParentsWord(t *testing.T) {
	if got := cleanACPChildEnv([]string{acpHandshakeEnv + "=1", acpLaunchedEnv + "=/elsewhere", acpLaunchWaitEnv + "=3", "PATH=/usr/bin"}); !slices.Equal(got, []string{"PATH=/usr/bin"}) {
		t.Fatalf("cleaned = %v", got)
	}
}

var errAnotherBoxRuns = &box.ServiceLaunchBusy{Boxes: []string{"a loop"}}

// A box in the way is a wait like any other: the next child holds prompts, every open thread hears why
// at once, idle or not, and the lock coming free ends it, with no rebuild and nobody's approval.
func TestBoxWaitTellsEveryThreadAboutAnotherBox(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.answer, h.holding = errAnotherBoxRuns, true })
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatalf("a box in the way is an answer, not a failure to look: %v", err)
	}
	if !h.w.blockedNow() {
		t.Fatal("a child spawned now would wait on the lock with the editor unanswered")
	}
	h.read(func() {
		if len(h.told) != 1 || len(h.said) != 1 || len(h.logged) != 1 || h.rebuilds != 0 {
			t.Fatalf("told=%q said=%q logged=%v rebuilds=%d", h.told, h.said, h.logged, h.rebuilds)
		}
		for _, want := range []string{"Waiting for another box in this project", "Running now: a loop.", "Close it to go on sooner", "wait here"} {
			if !strings.Contains(h.told[0], want) {
				t.Fatalf("the threads heard %q, want %q in it", h.told[0], want)
			}
		}
	})
	h.w.poll()
	h.read(func() {
		if len(h.told) != 1 || h.moves != 0 {
			t.Fatalf("while nothing changed it told %q and moved %d times", h.told, h.moves)
		}
	})
	h.set(func() { h.answer, h.sign = nil, "lock free" })
	h.w.poll()
	h.read(func() {
		if h.w.blockedNow() || h.moves != 1 || h.opened != 0 || h.said[len(h.said)-1] != "" {
			t.Fatalf("blocked=%v moves=%d opened=%d said=%q", h.w.blockedNow(), h.moves, h.opened, h.said)
		}
	})
}

// waitWord starts following what a child says on the pipe its supervisor gave it.
func waitWord(t *testing.T, w *acpBoxWait, child *acpproxy.Child) *os.File {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = write.Close() })
	w.follow(child, read)
	return write
}

func say(t *testing.T, word *os.File, what byte) {
	t.Helper()
	if _, err := word.Write([]byte{what}); err != nil {
		t.Fatal(err)
	}
}

// heard waits until follow has recorded child's state, then runs what the poll loop would.
func (h *boxWaitHarness) heard(t *testing.T, child *acpproxy.Child, waiting bool) {
	t.Helper()
	wait.For(t, "the wait to hear the child", func() bool {
		h.w.mu.Lock()
		defer h.w.mu.Unlock()
		return h.w.launchWaits[child] == waiting
	})
	h.w.poll()
}

// The check before a child starts can see a free lock that is taken a moment later, and such a child
// answers nothing: with no thread yet the editor shows a spinner, and a replacement would end the session
// when its replay grace ran out. So its word puts the session on a child that holds prompts.
func TestBoxWaitReplacesTheSessionsChildThatSaysItWaits(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.busyNow = errAnotherBoxRuns })
	child := &acpproxy.Child{}
	h.w.started(child)
	word := waitWord(t, h.w, child)
	say(t, word, acpLaunchWaiting)
	h.heard(t, child, true)
	h.read(func() {
		if h.replaces != 1 || len(h.told) != 1 || !strings.Contains(h.told[0], "Running now: a loop.") {
			t.Fatalf("replaces=%d told=%q", h.replaces, h.told)
		}
	})
	if !h.w.blockedNow() {
		t.Fatal("the child replacing it would wait on the lock too")
	}
	h.w.poll()
	h.read(func() {
		if h.replaces != 1 {
			t.Fatalf("replaced %d times for one word", h.replaces)
		}
	})
}

// A parked box's wait is nobody's news, until the session takes that box while it still waits.
func TestBoxWaitIgnoresAParkedChildUntilTheSessionTakesIt(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.busyNow = errAnotherBoxRuns })
	parked := &acpproxy.Child{}
	h.w.started(&acpproxy.Child{})
	word := waitWord(t, h.w, parked)
	say(t, word, acpLaunchWaiting)
	h.heard(t, parked, true)
	h.read(func() {
		if h.replaces != 0 || len(h.told) != 0 {
			t.Fatalf("a parked box replaced the session's %d times and told %q", h.replaces, h.told)
		}
	})
	h.w.started(parked)
	h.w.poll()
	h.read(func() {
		if h.replaces != 1 || len(h.told) != 1 {
			t.Fatalf("taking a box that waits: replaces=%d told=%q", h.replaces, h.told)
		}
	})

	// One that got through before the session took it is simply running.
	h = newBoxWaitHarness(t)
	h.set(func() { h.busyNow = errAnotherBoxRuns })
	h.w.started(&acpproxy.Child{})
	through := &acpproxy.Child{}
	word = waitWord(t, h.w, through)
	say(t, word, acpLaunchWaiting)
	h.heard(t, through, true)
	say(t, word, acpLaunchDone)
	h.heard(t, through, false)
	h.w.started(through)
	h.w.poll()
	h.read(func() {
		if h.replaces != 0 || len(h.told) != 0 {
			t.Fatalf("a box that got through: replaces=%d told=%q", h.replaces, h.told)
		}
	})
}

// A lock that came free as the child said so, or that only the session's own parked boxes hold, is
// nothing a thread can act on, and the child keeps trying.
func TestBoxWaitLeavesAChildWithNobodyToWaitFor(t *testing.T) {
	h := newBoxWaitHarness(t)
	child := &acpproxy.Child{}
	h.w.started(child)
	word := waitWord(t, h.w, child)
	say(t, word, acpLaunchWaiting)
	h.heard(t, child, true)
	h.read(func() {
		if h.replaces != 0 || len(h.told) != 0 || h.w.blockedNow() {
			t.Fatalf("replaces=%d told=%q blocked=%v", h.replaces, h.told, h.w.blockedNow())
		}
	})
	if h.w.waitsForLock(child) {
		t.Fatal("a wait nobody can end is shielded from the first-answer limit for good")
	}
}

// A child's word shields it from the first-answer limit from the moment it says so, whatever the poll
// loop has got to, and for as long as the wait lasts.
func TestAChildThatSaysItWaitsIsShieldedUntilItSaysItIsDone(t *testing.T) {
	h := newBoxWaitHarness(t)
	child := &acpproxy.Child{}
	h.w.started(child)
	if h.w.waitsForLock(child) {
		t.Fatal("a child that said nothing reads as waiting")
	}
	word := waitWord(t, h.w, child)
	say(t, word, acpLaunchWaiting)
	wait.For(t, "the word to arrive", func() bool { return h.w.waitsForLock(child) })
	say(t, word, acpLaunchDone)
	wait.For(t, "the end of the wait", func() bool { return !h.w.waitsForLock(child) })
}

// A child's exit ends what it said, so a dead pipe never leaves the session believing a box waits.
func TestBoxWaitForgetsAChildThatExited(t *testing.T) {
	h := newBoxWaitHarness(t)
	child := &acpproxy.Child{}
	word := waitWord(t, h.w, child)
	say(t, word, acpLaunchWaiting)
	h.heard(t, child, true)
	if err := word.Close(); err != nil {
		t.Fatal(err)
	}
	h.heard(t, child, false)
	h.w.started(child)
	h.read(func() {
		if h.w.launchWaitNew {
			t.Fatal("a child that exited still counts as waiting")
		}
	})
}

// The real path: a child the supervisor spawns is handed the descriptor, its word reaches the wait, and
// the descriptor never reaches anything the child runs: a process that kept it open would keep the pipe
// from ending when the child does.
func TestSpawnedChildCanTellItsSupervisorItWaits(t *testing.T) {
	h := newBoxWaitHarness(t)
	shim := filepath.Join(t.TempDir(), "inner")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf w >&\"$"+acpLaunchWaitEnv+"\"\nread line\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}, rt: runtime.Runtime{Name: "must-not-execute"}, rtSet: true, acpBoxWait: h.w}
	child, err := a.spawnBox(t.Context(), shim, nil, "super", nil, agents.Target{Provider: "claude"}, "", false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	h.heard(t, child, true)
	child.Stop()
	h.heard(t, child, false)
}

// A child that holds prompts starts no services, so nothing is listening for it.
func TestSpawnedHoldingChildGetsNoWaitDescriptor(t *testing.T) {
	h := newBoxWaitHarness(t)
	h.set(func() { h.answer = errNeedsAPerson })
	_ = h.w.refresh(t.Context())
	shim := filepath.Join(t.TempDir(), "inner")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf '%s\\n' \"[$"+acpLaunchWaitEnv+"]\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}, rt: runtime.Runtime{Name: "must-not-execute"}, rtSet: true, acpBoxWait: h.w}
	child, err := a.spawnBox(t.Context(), shim, nil, "super", nil, agents.Target{Provider: "claude"}, "", false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(child.Out)
	child.Stop()
	if err != nil || !child.HoldPrompts || strings.TrimSpace(string(out)) != "[]" {
		t.Fatalf("hold=%v descriptor=%q err=%v", child.HoldPrompts, out, err)
	}
}

// An inner child keeps the descriptor to itself: docker and Compose inherit nothing else it opened, and
// this was inherited, so it has to be marked by hand.
func TestLaunchWaitReporterWritesOneByteAndHidesTheDescriptor(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	cloexec := func() bool {
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, write.Fd(), syscall.F_GETFD, 0)
		if errno != 0 {
			t.Fatal(errno)
		}
		return flags&syscall.FD_CLOEXEC != 0
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, write.Fd(), syscall.F_SETFD, 0); errno != 0 {
		t.Fatal(errno)
	}
	if cloexec() {
		t.Fatal("the fixture did not clear the flag")
	}
	t.Setenv(acpLaunchWaitEnv, strconv.Itoa(int(write.Fd())))
	report, own := acpLaunchWaitReporter()
	if report == nil || !cloexec() {
		t.Fatalf("reporter=%v, close-on-exec=%v", report != nil, cloexec())
	}
	report(true)
	report(false)
	own()
	got := make([]byte, 3)
	if _, err := io.ReadFull(read, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string([]byte{acpLaunchWaiting, acpLaunchDone, acpLaunchOwnBoxes}) {
		t.Fatalf("wrote %q", got)
	}
	for _, unset := range []string{"", "not a number", "2"} {
		t.Setenv(acpLaunchWaitEnv, unset)
		if report, _ := acpLaunchWaitReporter(); report != nil {
			t.Fatalf("%q gave a reporter, and a child would write to whatever that descriptor is", unset)
		}
	}
}

// Two sessions with services of their own in one project, each on hold with a box of its own parked. Each
// parked box holds the shared side, so neither session can start its services, and neither box stops by
// itself. A session on hold stops its own, and both go on: nobody has to close anything.
func TestTwoSessionsOnHoldDoNotWaitOnEachOthersParkedBoxes(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session := func(source string) *boxWaitHarness {
		spec := box.RunSpec{Repo: repo, SupervisorID: source}
		parked, err := forkspace.BeginExecution(repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, Role: forkspace.ExecutionRoleWarm, Workspace: repo, SourceID: source})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = forkspace.EndExecution(repo, parked) })
		unlock, err := forkspace.LockServiceLaunch(t.Context(), repo, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(unlock)
		forget := forkspace.NoteServiceLaunchHeld(repo, parked.ID)
		h := newBoxWaitHarness(t)
		var once sync.Once
		h.w.check = func(context.Context) error { return box.EditorServiceLaunchBusy(spec) }
		h.w.stamp = func() string { return box.EditorBoxStamp(nil, spec) }
		h.w.yield = func() {
			once.Do(func() {
				forget()
				unlock()
				_ = forkspace.EndExecution(repo, parked)
			})
		}
		return h
	}
	a, b := session("session-a"), session("session-b")
	for _, h := range []*boxWaitHarness{a, b} {
		if err := h.w.refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	wait.For(t, "both sessions to go on", func() bool {
		for _, h := range []*boxWaitHarness{a, b} {
			h.w.poll()
			if !h.w.blockedNow() {
				h.w.yield() // the box it starts now says that only its own parked boxes keep it out
			}
		}
		return !a.w.blockedNow() && !b.w.blockedNow()
	})
}

// A wait that goes on tells every open thread when it waits for someone else.
func TestAThreadHearsWhoItWaitsForNow(t *testing.T) {
	h := newBoxWaitHarness(t)
	var next error = &box.ServiceLaunchBusy{Boxes: []string{"a loop"}}
	h.w.check = func(context.Context) error { return next }
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	next = &box.ServiceLaunchBusy{Boxes: []string{"an editor session"}}
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.read(func() {
		if len(h.told) != 2 || !strings.Contains(h.told[1], "an editor session") {
			t.Fatalf("threads heard %q", h.told)
		}
	})
}
