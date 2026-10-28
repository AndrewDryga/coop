package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// refreshHarness drives an acpNetworkRefresh with the supervisor's boundaries faked: the approval
// generation, admission, the selection the next spawn would use, the parked replacement spawn, the
// proxy's idle check and restart, and what the editor and the agent log are told.
type refreshHarness struct {
	r          *acpNetworkRefresh
	generation uint64
	genErr     error
	admit      func() (*box.CapturedEgress, error)
	admits     int
	busy       bool
	selection  acpSpawn
	restarts   int
	spawnErr   error
	spawns     []acpSpawn
	children   []*acpproxy.Child
	exits      map[*acpproxy.Child]chan struct{}
	stops      map[*acpproxy.Child]int
	active     map[*acpproxy.Child]bool
	drains     int
	reports    []error
	notices    []string
	clock      time.Time
}

var refreshSelection = acpSpawn{target: agents.Target{Provider: "claude", Accounts: []string{"work"}}, ok: true}

func newRefreshHarness(t *testing.T) *refreshHarness {
	t.Helper()
	h := &refreshHarness{generation: 4, selection: refreshSelection, clock: time.Unix(1_800_000_000, 0),
		exits: map[*acpproxy.Child]chan struct{}{}, stops: map[*acpproxy.Child]int{}, active: map[*acpproxy.Child]bool{}}
	startup := &box.CapturedEgress{Fingerprint: "old-policy", ApprovalGeneration: 4}
	h.r = &acpNetworkRefresh{
		generation: func() (uint64, error) { return h.generation, h.genErr },
		admit:      func() (*box.CapturedEgress, error) { h.admits++; return h.admit() },
		peek:       func() (acpSpawn, bool) { return h.selection, h.selection.ok },
		spawn: func(want acpSpawn) (*acpproxy.Child, error) {
			h.spawns = append(h.spawns, want)
			if h.spawnErr != nil {
				return nil, h.spawnErr
			}
			return h.child(h.r.current().Fingerprint), nil
		},
		idle: func() bool { return !h.busy },
		restart: func() bool {
			if h.busy {
				return false
			}
			h.restarts++
			return true
		},
		drain:   func() { h.drains++ },
		log:     func(err error) { h.reports = append(h.reports, err) },
		notify:  func(text string) { h.notices = append(h.notices, text) },
		now:     func() time.Time { return h.clock },
		startup: startup, capture: startup, seen: 4,
	}
	// The box serving the editor now, admitted under the startup policy.
	h.r.started(h.child("old-policy"))
	return h
}

func (h *refreshHarness) child(policy string) *acpproxy.Child {
	exit := make(chan struct{})
	c := &acpproxy.Child{Policy: policy, WaitDone: exit}
	c.Stop = func() { h.stops[c]++ }
	c.SetActive = func(active bool) { h.active[c] = active }
	h.children = append(h.children, c)
	h.exits[c] = exit
	return c
}

// approves makes the next admission return a capture of policy.
func (h *refreshHarness) approves(policy string) {
	h.generation++
	h.admit = func() (*box.CapturedEgress, error) { return &box.CapturedEgress{Fingerprint: policy}, nil }
}

// refuses makes the next admission fail with err.
func (h *refreshHarness) refuses(err error) {
	h.generation++
	h.admit = func() (*box.CapturedEgress, error) { return nil, err }
}

// swapsIn completes the swap the way the proxy and the factory do: the restart already happened, and
// the factory takes the prepared box and records it.
func (h *refreshHarness) swapsIn(t *testing.T) *acpproxy.Child {
	t.Helper()
	got := h.r.take(h.selection)
	if got == nil {
		t.Fatal("the factory found no prepared replacement")
	}
	h.r.started(got)
	return got
}

// reviewNeeded is the refusal admission returns when the project asks for rules nobody approved.
func reviewNeeded(reason string) error {
	return fmt.Errorf("Claude cannot start because %w\n\n  Review it: coop approve", &networkstate.PendingApproval{Reason: reason})
}

// The happy path, in order: an approval moves the generation; nothing happens while a turn runs;
// once idle the session admits again, drains the warm pool and starts the replacement parked; only
// after that replacement kept running for the settle time does the proxy restart, the factory hands
// over exactly that box, and the editor is told once.
func TestACPRefreshReplacesTheIdleBoxAfterItsReplacementStarted(t *testing.T) {
	h := newRefreshHarness(t)
	h.r.check()
	if h.admits != 0 || len(h.spawns) != 0 {
		t.Fatal("an unchanged generation admitted or spawned")
	}
	h.approves("new-policy")
	h.busy = true
	h.r.check()
	if h.admits != 0 {
		t.Fatal("the session admitted again while a turn was running")
	}
	h.busy = false
	h.r.check()
	if h.admits != 1 || h.r.current().Fingerprint != "new-policy" || h.drains != 1 {
		t.Fatalf("admits=%d policy=%q drains=%d, want the new policy adopted and the pool drained once", h.admits, h.r.current().Fingerprint, h.drains)
	}
	if len(h.spawns) != 1 || !h.spawns[0].same(refreshSelection) || h.restarts != 0 {
		t.Fatalf("spawns=%v restarts=%d, want one parked replacement for the live selection and no restart yet", h.spawns, h.restarts)
	}
	h.r.check()
	if h.restarts != 0 {
		t.Fatal("the box was replaced before its replacement had kept running for the settle time")
	}
	h.clock = h.clock.Add(acpRefreshSettle)
	h.r.check()
	if h.restarts != 1 {
		t.Fatalf("restarts=%d, want the idle box replaced once", h.restarts)
	}
	replacement := h.children[1]
	if got := h.swapsIn(t); got != replacement || !h.active[replacement] || h.stops[replacement] != 0 {
		t.Fatalf("the factory got %p (active=%v stops=%d), want the prepared replacement %p", got, h.active[replacement], h.stops[replacement], replacement)
	}
	h.r.check()
	if len(h.spawns) != 1 || h.restarts != 1 || len(h.reports) != 0 {
		t.Fatalf("spawns=%d restarts=%d reports=%v after the swap, want nothing more", len(h.spawns), h.restarts, h.reports)
	}
	if !slices.Equal(h.notices, []string{acpRefreshMoved}) {
		t.Fatalf("the editor was told %q, want the move once", h.notices)
	}
}

// A turn that starts while the replacement waits defers the restart; the parked replacement waits
// with it instead of being thrown away.
func TestACPRefreshDefersTheSwapWhileATurnRuns(t *testing.T) {
	h := newRefreshHarness(t)
	h.approves("new-policy")
	h.r.check()
	h.clock = h.clock.Add(acpRefreshSettle)
	h.busy = true
	h.r.check()
	h.r.check()
	replacement := h.children[1]
	if h.restarts != 0 || h.stops[replacement] != 0 || len(h.spawns) != 1 {
		t.Fatalf("restarts=%d stops=%d spawns=%d during a turn, want the replacement kept waiting", h.restarts, h.stops[replacement], len(h.spawns))
	}
	h.busy = false
	h.r.check()
	if h.restarts != 1 || h.r.take(refreshSelection) != replacement {
		t.Fatal("the waiting replacement did not serve the swap once the turn ended")
	}
}

// A refused admission — the project changed again, setup is missing — keeps the running box,
// reports once, widens nothing and retries only when the generation moves.
func TestACPRefreshKeepsTheBoxWhenAdmissionFails(t *testing.T) {
	h := newRefreshHarness(t)
	h.refuses(reviewNeeded("these rules changed"))
	for range 3 {
		h.r.check()
	}
	if h.admits != 1 || len(h.reports) != 1 || h.r.current().Fingerprint != "old-policy" || len(h.spawns) != 0 || h.drains != 0 || h.restarts != 0 {
		t.Fatalf("admits=%d reports=%d policy=%q spawns=%d drains=%d restarts=%d, want one refused admission and nothing replaced",
			h.admits, len(h.reports), h.r.current().Fingerprint, len(h.spawns), h.drains, h.restarts)
	}
	if len(h.notices) != 1 || !strings.Contains(h.notices[0], "Run coop approve") {
		t.Fatalf("notices = %q, want the review notice once", h.notices)
	}
	h.approves("new-policy")
	h.r.check()
	if h.admits != 2 || h.r.current().Fingerprint != "new-policy" {
		t.Fatal("a later approval was not adopted after a refused one")
	}
}

// The blocker this guards: an approval was adopted and its replacement prepared, then the approval
// was withdrawn before the idle swap. The refusal must drop both — never swap the session onto the
// withdrawn rules — and the warm pool, refilled under them, is drained again.
func TestACPRefreshRefusalAfterAnAdoptionNeverSwitchesOntoIt(t *testing.T) {
	h := newRefreshHarness(t)
	h.approves("new-policy")
	h.r.check()
	replacement := h.children[1]
	h.busy = true // a turn starts before the swap
	h.refuses(fmt.Errorf("%w: %w", errACPApprovalWithdrawn, reviewNeeded("this project's network approval was withdrawn on this machine")))
	h.clock = h.clock.Add(acpRefreshSettle)
	h.r.check()
	h.busy = false
	h.r.check()
	h.clock = h.clock.Add(acpRefreshSettle)
	h.r.check()
	if h.restarts != 0 || h.stops[replacement] != 1 || h.r.current().Fingerprint != "old-policy" || h.drains != 2 {
		t.Fatalf("restarts=%d replacement stops=%d policy=%q drains=%d, want no swap, the replacement stopped, the live rules kept and the pool drained again",
			h.restarts, h.stops[replacement], h.r.current().Fingerprint, h.drains)
	}
	if len(h.notices) != 1 || !strings.Contains(h.notices[0], "withdrawn") {
		t.Fatalf("notices = %q, want the withdrawal", h.notices)
	}
	if got := h.r.take(refreshSelection); got != nil {
		t.Fatal("a respawn could still take a box under the withdrawn rules")
	}
}

// The same refusal met by a respawn — the factory adopts before it looks for a box — drops the
// waiting replacement there and then, so the respawn starts under the rules the live box ran.
func TestACPRefreshRespawnThatMeetsARefusalDropsTheReplacement(t *testing.T) {
	h := newRefreshHarness(t)
	h.approves("new-policy")
	h.r.check()
	replacement := h.children[1]
	h.refuses(fmt.Errorf("%w: %w", errACPApprovalWithdrawn, reviewNeeded("this project's network approval was withdrawn on this machine")))
	h.busy = true // the live box crashed mid-turn
	h.r.adopt()
	if h.stops[replacement] != 1 || h.r.current().Fingerprint != "old-policy" {
		t.Fatalf("replacement stops=%d policy=%q after the respawn's refused admission, want it dropped and the live rules kept",
			h.stops[replacement], h.r.current().Fingerprint)
	}
}

// While the generation cannot be read nothing switches — a withdrawal may be what it would say: a
// waiting replacement is dropped, and the swap resumes once the counter reads again.
func TestACPRefreshSwitchesNothingWhileTheGenerationIsUnreadable(t *testing.T) {
	h := newRefreshHarness(t)
	h.approves("new-policy")
	h.r.check()
	replacement := h.children[1]
	h.genErr = errors.New("network approval generation is malformed")
	h.clock = h.clock.Add(acpRefreshSettle)
	h.r.check()
	if h.restarts != 0 || h.stops[replacement] != 1 {
		t.Fatalf("restarts=%d stops=%d while the generation was unreadable, want no switch and the replacement dropped", h.restarts, h.stops[replacement])
	}
	h.genErr = nil
	h.r.check()
	h.clock = h.clock.Add(acpRefreshSettle)
	h.r.check()
	if h.restarts != 1 || h.swapsIn(t) != h.children[2] {
		t.Fatalf("restarts=%d, want the swap resumed with a fresh replacement once the generation read again", h.restarts)
	}
}

// Every approval on the host moves the one generation. Another project's leaves this session's
// policy unchanged, and nothing is replaced for it.
func TestACPRefreshIgnoresAnotherProjectsApproval(t *testing.T) {
	h := newRefreshHarness(t)
	startup := h.r.current()
	h.approves("old-policy")
	h.r.check()
	if h.admits != 1 || h.r.current() != startup || h.drains != 0 || len(h.spawns) != 0 || len(h.reports) != 0 {
		t.Fatalf("an unchanged policy replaced something: admits=%d drains=%d spawns=%d reports=%v", h.admits, h.drains, len(h.spawns), h.reports)
	}
}

// A refusal repeats for every approval on the host; the editor hears it once until something else
// was said.
func TestACPRefreshShowsARepeatedRefusalOnce(t *testing.T) {
	h := newRefreshHarness(t)
	for range 3 {
		h.refuses(reviewNeeded("these rules changed"))
		h.r.check()
	}
	if h.admits != 3 || len(h.reports) != 3 || len(h.notices) != 1 {
		t.Fatalf("admits=%d reports=%d notices=%q, want every refusal logged and shown once", h.admits, len(h.reports), h.notices)
	}
}

// The replacement is prepared for the selection the next spawn would use, not the one the live box
// started with: a model changed in the editor's dropdown — in place, no spawn — is what the factory
// will ask for. A selection that changes while the replacement waits makes it stale.
func TestACPRefreshPreparesForTheSelectionTheNextSpawnUses(t *testing.T) {
	h := newRefreshHarness(t)
	h.selection = acpSpawn{target: agents.Target{Provider: "claude", Model: "claude-fable-5", Accounts: []string{"work"}}, ok: true}
	h.approves("new-policy")
	h.r.check()
	if len(h.spawns) != 1 || !h.spawns[0].same(h.selection) {
		t.Fatalf("prepared for %v, want the selection the next spawn uses %v", h.spawns, h.selection)
	}
	first := h.children[1]
	h.selection.target.Effort = "high" // changed again while the replacement waits
	h.clock = h.clock.Add(acpRefreshSettle)
	h.r.check()
	if h.restarts != 0 || h.stops[first] != 1 {
		t.Fatalf("restarts=%d stops=%d, want the stale replacement stopped before any restart", h.restarts, h.stops[first])
	}
	h.r.check()
	h.clock = h.clock.Add(acpRefreshSettle)
	h.r.check()
	if len(h.spawns) != 2 || !h.spawns[1].same(h.selection) || h.restarts != 1 || h.swapsIn(t) != h.children[2] {
		t.Fatalf("spawns=%v restarts=%d, want one replacement for the new selection handed over", h.spawns, h.restarts)
	}
	// While the next spawn would move the lead to another provider, there is nothing to prepare.
	h.selection = acpSpawn{}
	h.approves("newer-policy")
	h.r.check()
	if len(h.spawns) != 2 {
		t.Fatal("a replacement was prepared for a selection no peek could tell")
	}
}

// Make-before-break: a replacement that cannot start, or stops while starting, never takes the
// running box with it. It is reported once and not retried until the generation moves again.
func TestACPRefreshKeepsTheBoxWhenItsReplacementFails(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		h := newRefreshHarness(t)
		h.spawnErr = errors.New("selected ACP target is no longer available")
		h.approves("new-policy")
		for range 3 {
			h.r.check()
		}
		if len(h.spawns) != 1 || len(h.reports) != 1 || h.restarts != 0 {
			t.Fatalf("spawns=%d reports=%d restarts=%d, want one failed attempt and the box kept", len(h.spawns), len(h.reports), h.restarts)
		}
		h.spawnErr = nil
		h.approves("new-policy") // approved again: worth another attempt
		h.r.check()
		if len(h.spawns) != 2 {
			t.Fatal("a new approval did not retry the replacement")
		}
	})
	t.Run("account cooling", func(t *testing.T) {
		h := newRefreshHarness(t)
		h.spawnErr = fmt.Errorf("claude account %q is %w", "work", errACPAccountCooling)
		h.approves("new-policy")
		h.r.check()
		h.r.check()
		if len(h.spawns) != 2 || len(h.reports) != 0 {
			t.Fatalf("spawns=%d reports=%v, want the replacement retried quietly while the account cools", len(h.spawns), h.reports)
		}
	})
	t.Run("stopped while starting", func(t *testing.T) {
		h := newRefreshHarness(t)
		h.approves("new-policy")
		h.r.check()
		replacement := h.children[1]
		close(h.exits[replacement])
		h.clock = h.clock.Add(acpRefreshSettle)
		h.r.check()
		h.r.check()
		if h.restarts != 0 || h.stops[replacement] != 1 || len(h.reports) != 1 || len(h.spawns) != 1 {
			t.Fatalf("restarts=%d stops=%d reports=%d spawns=%d, want the dead replacement dropped and the box kept",
				h.restarts, h.stops[replacement], len(h.reports), len(h.spawns))
		}
	})
}

// A respawn can take the waiting replacement between a check reading it and finding it exited. It is
// the live box then: the late check neither stops it nor reports a failure that did not happen.
func TestACPRefreshNeverFailsAReplacementTheFactoryTook(t *testing.T) {
	h := newRefreshHarness(t)
	h.approves("new-policy")
	h.r.check()
	replacement := h.swapsIn(t)
	h.r.fail("new-policy", replacement, errACPReplacementStopped)
	if h.stops[replacement] != 0 || len(h.reports) != 0 {
		t.Fatalf("stops=%d reports=%v, want the live box left to the proxy", h.stops[replacement], h.reports)
	}
}

// A switch while the replacement waits: that replacement serves another selection, so the factory
// stops it and starts the new selection cold — under the new policy, so nothing else is replaced.
func TestACPRefreshDropsAReplacementForAnotherSelection(t *testing.T) {
	h := newRefreshHarness(t)
	h.approves("new-policy")
	h.r.check()
	replacement := h.children[1]
	other := acpSpawn{target: agents.Target{Provider: "codex", Accounts: []string{"default"}}, ok: true}
	if got := h.r.take(other); got != nil || h.stops[replacement] != 1 {
		t.Fatalf("a replacement for another selection was handed over (%v) or kept (stops=%d)", got, h.stops[replacement])
	}
	h.selection = other
	h.r.started(h.child(h.r.current().Fingerprint))
	h.clock = h.clock.Add(acpRefreshSettle)
	h.r.check()
	if h.restarts != 0 || len(h.spawns) != 1 {
		t.Fatalf("restarts=%d spawns=%d, want nothing left to replace", h.restarts, len(h.spawns))
	}
}

// A respawn is already replacing the box, so it adopts a changed approval even mid-turn: the box it
// starts proves the newest policy and needs no second swap.
func TestACPRefreshAdoptsAtRespawn(t *testing.T) {
	h := newRefreshHarness(t)
	h.approves("new-policy")
	h.busy = true
	h.r.adopt()
	if h.r.current().Fingerprint != "new-policy" {
		t.Fatal("a respawn started under the old policy after an approval")
	}
	h.r.started(h.child(h.r.current().Fingerprint))
	h.busy = false
	h.clock = h.clock.Add(acpRefreshSettle)
	h.r.check()
	if h.admits != 1 || len(h.spawns) != 0 || h.restarts != 0 {
		t.Fatalf("admits=%d spawns=%d restarts=%d, want the respawned box to need nothing more", h.admits, len(h.spawns), h.restarts)
	}
}

// There is no way back once the session moved: a box under the new policy that exits — the agent can
// make its own box exit — is respawned under the same policy, never under the rules the old box ran,
// which would widen a session whose new approval narrowed them.
func TestACPRefreshNeverMovesBackToTheRulesItLeft(t *testing.T) {
	h := newRefreshHarness(t)
	h.approves("narrower-policy")
	h.r.check()
	h.clock = h.clock.Add(acpRefreshSettle)
	h.r.check()
	h.swapsIn(t)
	h.r.adopt() // the factory respawning the box that just exited
	if h.r.current().Fingerprint != "narrower-policy" || h.r.take(refreshSelection) != nil {
		t.Fatalf("after the new box exited the session would start under %q", h.r.current().Fingerprint)
	}
}

// An approval that resolves to open or offline cannot be applied by replacing filtered boxes.
func TestACPRefreshReportsAModeChange(t *testing.T) {
	h := newRefreshHarness(t)
	h.generation++
	h.admit = func() (*box.CapturedEgress, error) { return nil, nil }
	h.r.check()
	if len(h.reports) != 1 || !errors.Is(h.reports[0], errACPNetworkModeChanged) || h.r.current().Fingerprint != "old-policy" {
		t.Fatalf("reports=%v policy=%q", h.reports, h.r.current().Fingerprint)
	}
	if len(h.notices) != 1 || !strings.Contains(h.notices[0], "Reconnect the editor") {
		t.Fatalf("mode change notices = %q", h.notices)
	}
}

// Closing the session stops a replacement still waiting and admits nothing afterwards.
func TestACPRefreshCloseStopsAWaitingReplacement(t *testing.T) {
	h := newRefreshHarness(t)
	h.approves("new-policy")
	h.r.check()
	replacement := h.children[1]
	h.r.close()
	if h.stops[replacement] != 1 {
		t.Fatal("a waiting replacement outlived the session")
	}
	h.approves("newer-policy")
	h.r.adopt()
	if h.admits != 1 {
		t.Fatal("a closed refresh admitted again")
	}
}

// The editor gets fixed sentences with Coop as the subject and one next step: an admission error can
// name hosts and paths, which stay in the agent log.
func TestACPRefreshNoticeKeepsDetailsInTheLog(t *testing.T) {
	const detail = "storage.googleapis.com:443 /Users/someone/project"
	for _, tc := range []struct {
		err  error
		want string
	}{
		{reviewNeeded("these rules changed: " + detail), "Run coop approve"},
		{box.ErrNetworkSetupRequired, "Run coop net setup, then reconnect the editor"},
		{fmt.Errorf("%w: %w", errACPApprovalWithdrawn, reviewNeeded(detail)), "approval was withdrawn"},
		{errACPNetworkModeChanged, "network access changed"},
		{errors.New("docker exploded " + detail), "The reason is in the agent log"},
	} {
		notice := acpRefreshNotice(tc.err)
		if !strings.Contains(notice, tc.want) || strings.Contains(notice, "googleapis") || strings.Contains(notice, "/Users/") || !strings.HasPrefix(notice, "Coop ") {
			t.Errorf("notice for %q = %q, want %q, Coop as the subject and no detail", tc.err, notice, tc.want)
		}
	}
}

// A refresh admits exactly what the supervisor admitted at startup — the same lead, preset, flags and
// pre-admission configuration, and the provider/account scope frozen then — never a scope derived
// again from the repository or the host's accounts, and without setting the host up.
func TestACPRefreshAdmitsTheFrozenStartupScope(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}
	signInCred(t, cfg, "claude", "default")
	var specs []box.RunSpec
	var options []box.NetworkAdmission
	var marked []bool
	a := &app{cfg: cfg, rt: runtime.Runtime{Name: "must-not-execute"}, rtSet: true,
		admitNetwork: func(c *config.Config, _ runtime.Runtime, spec box.RunSpec, opts box.NetworkAdmission) (*box.CapturedEgress, error) {
			specs, options, marked = append(specs, spec), append(options, opts), append(marked, c.Explicit("COOP_EGRESS"))
			c.SetEgress("filtered") // what admission does to the configuration it is given
			return &box.CapturedEgress{Project: cfg.RepoOverride, Fingerprint: strings.Repeat("a", 64)}, nil
		},
		acpSupervise: func([]string, *acpctl.Control) (int, error) { return 0, nil },
	}
	if code, err := a.cmdACP([]string{"claude", "--egress", "filtered"}); code != 0 || err != nil {
		t.Fatalf("filtered acp = (%d, %v)", code, err)
	}
	if a.acpAdmission == nil || len(specs) != 1 {
		t.Fatal("a filtered supervisor kept no way to admit again")
	}
	// After startup the host signs in another account, an agent adds a preset, and the app's own
	// fields move on: a refresh that read any of them now would admit something else.
	signInCred(t, cfg, "claude", "second")
	dir := filepath.Join(cfg.RepoOverride, ".agent", "presets", "late")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte("lead:\n  agent: claude\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	same := func(x, y agents.Target) bool { return x.String() == y.String() }
	if rederived, err := a.acpNetworkScope(cfg.RepoOverride, agents.Target{Provider: "claude"}, nil, nil); err != nil || slices.EqualFunc(rederived, specs[0].Peers, same) {
		t.Fatalf("fixture: a re-derived scope (%v, %v) must differ from the admitted one %v", rederived, err, specs[0].Peers)
	}
	a.preset, a.network, a.acpNetworkTargets = &preset.Preset{}, networkFlags{}, nil
	if _, err := a.acpAdmission(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || !slices.EqualFunc(specs[1].Peers, specs[0].Peers, same) {
		t.Fatalf("the refresh admitted scope %v, want the frozen %v", specs[1].Peers, specs[0].Peers)
	}
	if specs[1].Agent != specs[0].Agent || specs[1].Repo != specs[0].Repo || specs[1].Preset != specs[0].Preset || specs[1].Ctx == nil {
		t.Fatalf("the refresh admitted another launch: %+v, want %+v", specs[1], specs[0])
	}
	if options[0].ExistingQualification || !options[1].ExistingQualification || options[1].InvocationMode == nil || *options[1].InvocationMode != egress.Filtered {
		t.Fatalf("admission options = %+v then %+v, want the same flags and no host setup on refresh", options[0], options[1])
	}
	if marked[1] {
		t.Fatal("the refresh admitted from the configuration startup admission had already marked")
	}
}

// The refusal comes from the owner store before anything runs: an agent that edited the project file
// after the approval, or a withdrawn approval, never reaches the runtime — and a withdrawal is named
// as one, not as the review the project also needs.
func TestACPReadmitRefusesAChangedOrWithdrawnProjectBeforeTheRuntime(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := func(domains ...string) {
		body := "box:\n  egress: filtered\n  egress_rules:\n"
		for _, domain := range domains {
			body += "    - to: {domain: " + domain + "}\n      protocol: tls\n      ports: [443]\n"
		}
		if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: repo}
	project("proxy.golang.org")
	review, err := box.ReviewProjectNetworkContext(context.Background(), cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := review.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = review.Close()
	calls := filepath.Join(t.TempDir(), "runtime-calls")
	a := &app{cfg: cfg, rt: recordingRuntime(t, calls), rtSet: true}
	spec := box.RunSpec{Repo: repo, Workdir: repo, Agent: "claude", NetworkClient: egress.ClientACP}

	project("proxy.golang.org", "storage.googleapis.com")
	_, err = a.acpReadmit(t.Context(), cfg, spec, box.NetworkAdmission{})
	if err == nil || !strings.Contains(err.Error(), "Review it: coop approve") || !strings.Contains(acpRefreshNotice(err), "Run coop approve") {
		t.Fatalf("re-admitting an edited project = %v", err)
	}

	project("proxy.golang.org")
	forget, err := box.ReviewProjectNetworkForget(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := forget.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = forget.Close()
	root, err := box.NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a.acpCapture = &box.CapturedEgress{Store: store, Project: repo, Fingerprint: strings.Repeat("a", 64)}
	a.acpAdmission = func(ctx context.Context) (*box.CapturedEgress, error) {
		return a.acpReadmit(ctx, cfg, spec, box.NetworkAdmission{})
	}
	r := a.newACPNetworkRefresh(nil, &acpproxy.Restarter{}, func() {}, nil)
	defer r.stop()
	_, err = r.admit()
	if !errors.Is(err, errACPApprovalWithdrawn) {
		t.Fatalf("a withdrawn project was admitted again or not named withdrawn: %v", err)
	}
	if notice := acpRefreshNotice(err); !strings.Contains(notice, "approval was withdrawn") || !strings.Contains(notice, "kept this session's network rules") {
		t.Fatalf("withdrawn project notice = %q (%v)", notice, err)
	}
	if ran, err := os.ReadFile(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused re-admission ran the runtime: %q", ran)
	}
}

// The next child after a refresh proves the newly admitted policy, and says so for the warm pool.
func TestACPChildProvesTheRefreshedPolicy(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir(), Egress: "filtered"}
	cfg.SetEgress("filtered")
	cfg.SetActiveProfile("claude", "selected")
	seedNativeFixture(t, cfg, "claude", "selected")
	target := agents.Target{Provider: "claude", Accounts: []string{"selected"}}
	startup := &box.CapturedEgress{Project: cfg.RepoOverride, Fingerprint: strings.Repeat("a", 64), QualificationID: strings.Repeat("b", 64)}
	refreshed := &box.CapturedEgress{Project: cfg.RepoOverride, Fingerprint: strings.Repeat("c", 64), QualificationID: strings.Repeat("d", 64)}
	a := &app{cfg: cfg, acpNetworkTargets: []agents.Target{target}, acpCapture: startup}
	a.acpRefresh = &acpNetworkRefresh{startup: startup, capture: refreshed}
	shim := filepath.Join(t.TempDir(), "inner")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf '%s\\n' \"$COOP_NETWORK_CAPTURE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	child, err := a.spawnBox(t.Context(), shim, nil, "refresh", nil, target, "", true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(child.Out)
	child.Stop()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), strings.Repeat("c", 64)) || strings.Contains(string(out), strings.Repeat("a", 64)) || child.Policy != strings.Repeat("c", 64) {
		t.Fatalf("child capture %s (policy %q), want the refreshed policy only", out, child.Policy)
	}
}

func TestACPRefreshWorkerUsesTurnEndAndPollingAndJoins(t *testing.T) {
	for _, cue := range []string{"turn end", "poll"} {
		t.Run(cue, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newRefreshHarness(t)
				ctx, cancel := context.WithCancel(t.Context())
				h.r.stop = cancel
				h.r.stopped = ctx.Done()
				h.r.now = time.Now
				cfg := &config.Config{ConfigDir: t.TempDir()}
				ctrl := acpctl.New(cfg, "claude", "", "", t.TempDir(), acpctl.Selection{}, nil, nil, acpHost())
				h.approves("new-policy")
				closeWorker := h.r.start(ctrl)
				defer closeWorker()
				synctest.Wait()
				finishTurn := func(id string) {
					hooks := ctrl.Hooks()
					hooks.PromptForwarded([]byte(`{"jsonrpc":"2.0","id":"`+id+`","method":"session/prompt","params":{"sessionId":"S","prompt":[{"type":"text","text":"hi"}]}}`), false)
					hooks.ToEditor([]byte(`{"jsonrpc":"2.0","id":"` + id + `","result":{"stopReason":"end_turn"}}`))
				}
				if cue == "turn end" {
					finishTurn("first")
				} else {
					time.Sleep(acpRefreshInterval)
				}
				synctest.Wait()
				if h.admits != 1 || len(h.spawns) != 1 || h.r.prepared == nil {
					t.Fatalf("worker did not adopt on %s: admits=%d spawns=%d", cue, h.admits, len(h.spawns))
				}
				pending := h.r.prepared
				closeWorker()
				closeWorker()
				synctest.Wait()
				if h.stops[pending] != 1 {
					t.Fatalf("pending child stopped %d times", h.stops[pending])
				}
				h.approves("after-close")
				finishTurn("second")
				time.Sleep(2 * acpRefreshInterval)
				synctest.Wait()
				if h.admits != 1 || len(h.spawns) != 1 {
					t.Fatal("worker continued after close returned")
				}
			})
		})
	}
}
