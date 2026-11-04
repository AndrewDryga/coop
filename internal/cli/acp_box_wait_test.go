package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
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
	moveOK   bool
	moves    int
	said     []string
	notices  []string
	logged   []error
	opened   int
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
		move: func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.moves++
			return h.moveOK
		},
		publish: func(text string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.said = append(h.said, text)
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
	if h.w.blockedNow() || h.moves != 1 || h.opened != 1 || h.lastSaid() != "" {
		t.Fatalf("blocked=%v moves=%d opened=%d said=%q", h.w.blockedNow(), h.moves, h.opened, h.lastSaid())
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

// Open and offline sessions have no wait, and every call on it is safe.
func TestNoBoxWaitForAnUnfilteredSession(t *testing.T) {
	var w *acpBoxWait
	if w.blockedNow() || w.refresh(t.Context()) != nil || w.refreshStale(t.Context()) != nil {
		t.Fatal("a nil wait reported something")
	}
	w.start()
	w.close()
	w.started(&acpproxy.Child{})
	a := &app{}
	if a.newACPBoxWait(nil, nil, box.RunSpec{}, nil) != nil {
		t.Fatal("an unfiltered session got a wait")
	}
}

func TestACPChildEnvNeverInheritsTheHandshakeFlag(t *testing.T) {
	if got := cleanACPChildEnv([]string{acpHandshakeEnv + "=1", "PATH=/usr/bin"}); !slices.Equal(got, []string{"PATH=/usr/bin"}) {
		t.Fatalf("cleaned = %v", got)
	}
}
