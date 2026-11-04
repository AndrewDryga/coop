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
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
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
	held := &box.CapturedEgress{Project: cfg.RepoOverride, Fingerprint: strings.Repeat("a", 64), QualificationID: strings.Repeat("b", 64),
		Pending: &networkstate.PendingApproval{Reason: "this project asks for network access that has not been approved"}}
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
		t.Fatalf("blocked=%v child.HoldPrompts=%v handshake flag=%q", h.w.blockedNow(), child.HoldPrompts, out)
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
		if h.moves != 1 || h.opened != 1 {
			t.Fatalf("moves=%d opened=%d, want the session moved by itself", h.moves, h.opened)
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
		if h.moves != 1 || h.opened != 1 {
			t.Fatalf("moves=%d opened=%d, want the session moved by itself", h.moves, h.opened)
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

// An open session has no wait to report to, so nothing changes for its children.
func TestSpawnedOpenChildHasNoLaunchToReport(t *testing.T) {
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
	if child.Launched != nil || strings.Contains(string(env), acpLaunchedEnv) {
		t.Fatalf("an open child has a launch to report: %v", child.Launched != nil)
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
	a := &app{cfg: openNetwork(&config.Config{}), acpBoxWait: h.w}
	a.cfg.SetEgress("filtered")
	if err := a.ensureACPImage(); err != nil {
		t.Fatalf("connect = %v", err)
	}
	if h.w.blockedNow() {
		t.Fatal("a failure to look blocked the first real box")
	}
}

// A project file an agent broke stops a real box and must not stop the child that says so: the child
// holding prompts reads nothing from it, and neither a job nor a holding child needs the project's image.
func TestAHoldingChildResolvesItsImageWhateverTheProjectFileSays(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte("box: [broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir(), RepoOverride: repo, BaseImage: "base"}, rt: runtime.Runtime{Name: "must-not-execute"}, rtSet: true}
	if _, _, err := a.resolveLaunchImage(); err == nil {
		t.Fatal("a real launch accepted a broken project file")
	}
	if _, err := a.dispatch([]string{"acp", "claude"}); err == nil || !strings.Contains(err.Error(), "project.yaml") {
		t.Fatalf("coop acp with a broken project file = %v", err)
	}
	t.Setenv("COOP_ACP_INNER", "1")
	t.Setenv(acpHandshakeEnv, "1")
	got, img, err := a.resolveLaunchImage()
	if err != nil || img != "base" {
		t.Fatalf("a holding child: repo=%q image=%q err=%v", got, img, err)
	}
	if _, err := a.dispatch([]string{"acp", "claude"}); err != nil && strings.Contains(err.Error(), "project.yaml") {
		t.Fatalf("a holding child stopped at the project file: %v", err)
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
	if got := cleanACPChildEnv([]string{acpHandshakeEnv + "=1", acpLaunchedEnv + "=/elsewhere", "PATH=/usr/bin"}); !slices.Equal(got, []string{"PATH=/usr/bin"}) {
		t.Fatalf("cleaned = %v", got)
	}
}
