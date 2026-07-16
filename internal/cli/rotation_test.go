package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

// rts builds a claude rotation from bare account names (model empty), for the engine tests.
// String() renders each as "claude@<acct>".
func rts(creds ...string) *rotation {
	ts := make([]agents.Target, len(creds))
	for i, c := range creds {
		ts[i] = agents.Target{Provider: "claude", Accounts: []string{c}}
	}
	return newRotation(ts)
}

// signInCred writes a fake credential for agent so box.ProfileAuthed sees it signed in — using
// the agent's OWN auth marker (claude's .credentials.json, codex's auth.json, …), not a hardcoded
// one, so it works for a cross-provider rotation test too.
func signInCred(t *testing.T, cfg *config.Config, agent, name string) {
	t.Helper()
	dir := cfg.AgentProfileDir(agent, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ag, ok := agents.Get(agent)
	if !ok {
		t.Fatalf("unknown agent %q", agent)
	}
	file, _ := ag.AuthMarker()
	if err := os.WriteFile(filepath.Join(dir, file), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRotationSingle(t *testing.T) {
	now := time.Unix(1000, 0)
	r := rts("only")
	if r.rotates() {
		t.Error("a single-target rotation shouldn't rotate")
	}
	reset := now.Add(time.Hour)
	sleep, until := r.onLimit(reset, 1, now)
	if sleep <= 0 || !until.Equal(reset) || r.active().String() != "claude@only" {
		t.Errorf("single-target limit: sleep=%v until=%v active=%q, want a wait to %v on only", sleep, until, r.active(), reset)
	}
}

func TestRotationStickyThenWaits(t *testing.T) {
	now := time.Unix(1000, 0)
	r := rts("a", "b", "c")
	if r.active().String() != "claude@a" {
		t.Fatalf("start active = %q, want a", r.active())
	}
	if sleep, _ := r.onLimit(now.Add(2*time.Hour), 1, now); sleep != 0 || r.active().String() != "claude@b" {
		t.Fatalf("after a limited: sleep=%v active=%q, want 0 + b", sleep, r.active())
	}
	if sleep, _ := r.onLimit(now.Add(time.Hour), 2, now); sleep != 0 || r.active().String() != "claude@c" {
		t.Fatalf("after b limited: sleep=%v active=%q, want 0 + c", sleep, r.active())
	}
	sleep, until := r.onLimit(now.Add(3*time.Hour), 3, now)
	if sleep <= 0 || !until.Equal(now.Add(time.Hour)) || r.active().String() != "claude@b" {
		t.Errorf("all limited → park on soonest-reset b (+1h): sleep=%v until=%v active=%q", sleep, until, r.active())
	}
}

func TestRotationUnknownResetBacksOff(t *testing.T) {
	now := time.Unix(1000, 0)
	r := rts("a", "b")
	if sleep, _ := r.onLimit(time.Time{}, 1, now); sleep != 0 || r.active().String() != "claude@b" {
		t.Fatalf("unknown reset, b free: sleep=%v active=%q", sleep, r.active())
	}
	if sleep, _ := r.onLimit(time.Time{}, 2, now); sleep <= 0 || sleep > limitMaxWait {
		t.Errorf("all limited w/ unknown reset: sleep=%v, want a bounded backoff", sleep)
	}
}

func TestRotationNonfutureResetStillCoolsTarget(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, reset := range []time.Time{now.Add(-time.Hour), now} {
		r := rts("a", "b")
		if sleep, _ := r.onLimit(reset, 1, now); sleep != 0 || r.active().String() != "claude@b" {
			t.Fatalf("reset %v: sleep=%v active=%q, want free target b", reset, sleep, r.active())
		}
		if until := r.limited["claude@a"]; !until.After(now) {
			t.Fatalf("reset %v: normalized cooling deadline = %v, want future", reset, until)
		}
	}
}

func TestRotationSelectionSkipsFutureLimitAndClearsExpired(t *testing.T) {
	now := time.Unix(1000, 0)
	r := rts("a", "b", "c")
	r.limited["claude@a"] = now.Add(time.Hour)
	if sleep, _ := r.selectTarget(1, now); sleep != 0 || r.active().String() != "claude@b" {
		t.Fatalf("future-limited a: sleep=%v active=%q, want 0 + b", sleep, r.active())
	}

	r = rts("a", "b", "c")
	r.limited["claude@a"] = now.Add(-time.Second)
	if sleep, _ := r.selectTarget(1, now); sleep != 0 || r.active().String() != "claude@a" {
		t.Fatalf("expired a: sleep=%v active=%q, want 0 + a", sleep, r.active())
	}
	if len(r.limited) != 0 {
		t.Errorf("expired marks not cleared: %v", r.limited)
	}
}

func TestRememberPreflightLimitAdvancesWorkRotation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	reset := now.Add(time.Hour)
	r := rts("personal", "backup", "third")
	out := fmt.Sprintf("Claude AI usage limit reached|%d", reset.Unix())

	sleep, until, limited := rememberPreflightLimit(r, classifyIteration("claude", 1, nil, out, streamNotUsed, now), now)
	if !limited || sleep != 0 || !until.IsZero() || r.active().String() != "claude@backup" {
		t.Fatalf("preflight limit: limited=%v sleep=%v until=%v active=%q, want true, 0, zero, backup", limited, sleep, until, r.active())
	}
	if got := r.limited["claude@personal"]; !got.Equal(reset) {
		t.Errorf("personal limited until %v, want %v", got, reset)
	}

	// Successful prose and output exhaustion are not provider limits and must not rotate.
	for _, tc := range []struct {
		name string
		code int
		out  string
	}{
		{"successful prose", 0, out},
		{"output exhaustion", 1, "maximum output length reached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rts("personal", "backup", "third")
			classification := classifyIteration("claude", tc.code, nil, tc.out, streamNotUsed, now)
			if _, _, limited := rememberPreflightLimit(r, classification, now); limited || r.active().String() != "claude@personal" {
				t.Errorf("limited=%v active=%q, want false + personal", limited, r.active())
			}
		})
	}
}

// The acceptance scenario, per (model,account) key: models: [fable, opus@work] over accounts
// {work, personal} → fable@work → fable@personal (same model, other account) → opus@work; and
// fable@work cooling never blocks opus@work.
func TestRotationSameModelFallback(t *testing.T) {
	now := time.Unix(1000, 0)
	r := newRotation([]agents.Target{
		{Provider: "claude", Model: "fable", Accounts: []string{"work"}},
		{Provider: "claude", Model: "fable", Accounts: []string{"personal"}},
		{Provider: "claude", Model: "opus", Accounts: []string{"work"}},
	})
	if r.active().String() != "claude:fable@work" {
		t.Fatalf("start = %q", r.active())
	}
	if sleep, _ := r.onLimit(now.Add(time.Hour), 1, now); sleep != 0 || r.active().String() != "claude:fable@personal" {
		t.Fatalf("fable@work limited → fable@personal: sleep=%v active=%q", sleep, r.active())
	}
	if sleep, _ := r.onLimit(now.Add(time.Hour), 2, now); sleep != 0 || r.active().String() != "claude:opus@work" {
		t.Fatalf("fable@personal limited → opus@work: sleep=%v active=%q", sleep, r.active())
	}
	if len(r.limited) != 2 {
		t.Fatalf("limited keys = %d, want 2 distinct (model,account) targets", len(r.limited))
	}
}

// expandLadder: bare model → all signed-in accounts (marked-default first, then the rest);
// pinned → one target (unauthed pinned skipped); empty ladder → default model, all accounts.
func TestExpandLadder(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, cfg, "claude", "work")
	signInCred(t, cfg, "claude", "personal")
	if err := cfg.SetDefaultProfile("claude", "personal"); err != nil { // default first
		t.Fatal(err)
	}

	// Bare model fans out, marked-default (personal) first.
	got, err := expandLadder(cfg, "claude", []agents.Target{{Model: "opus"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude:opus@personal", "claude:opus@work"}; !slices.Equal(members(got), want) {
		t.Errorf("bare model fan-out = %v, want %v (default first)", members(got), want)
	}
	// Ladder: bare then pinned; dedup, order preserved.
	got, _ = expandLadder(cfg, "claude", []agents.Target{{Model: "fable"}, {Model: "opus", Accounts: []string{"work"}}})
	if want := []string{"claude:fable@personal", "claude:fable@work", "claude:opus@work"}; !slices.Equal(members(got), want) {
		t.Errorf("ladder = %v, want %v", members(got), want)
	}
	// Empty ladder → default model (empty) across accounts (provider shown, no model).
	got, _ = expandLadder(cfg, "claude", nil)
	if want := []string{"claude@personal", "claude@work"}; !slices.Equal(members(got), want) {
		t.Errorf("empty ladder = %v, want the accounts on the default model", members(got))
	}
	// A pinned account that isn't signed in is skipped, not an error, as long as something remains.
	got, _ = expandLadder(cfg, "claude", []agents.Target{{Model: "opus", Accounts: []string{"ghost"}}, {Model: "fable"}})
	if slices.Contains(members(got), "claude:opus@ghost") {
		t.Errorf("unsigned pinned account should be skipped: %v", members(got))
	}
	// No signed-in accounts at all → error.
	if _, err := expandLadder(&config.Config{ConfigDir: t.TempDir()}, "claude", nil); err == nil {
		t.Error("no signed-in account should error")
	}
}

// Effort rides the ladder into each rotation target, and applyTarget/applyRunTarget land it in
// cfg so EffortFor (and thus the agent command) sees it — the full CLI glue for a /effort target.
func TestEffortThreadsToConfig(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, cfg, "codex", "work")
	a := &app{cfg: cfg}

	// Loop path: the positional target IS the ladder; applyTarget sets the rotation-target tier.
	ladder := []agents.Target{{Provider: "codex", Model: "gpt-5.6-sol", Effort: "high", Accounts: []string{"work"}}}
	rot, err := a.buildRotation("codex", ladder)
	if err != nil {
		t.Fatal(err)
	}
	if got := members(rot.targets); !slices.Contains(got, "codex:gpt-5.6-sol/high@work") {
		t.Fatalf("rotation targets = %v, want one carrying /high", got)
	}
	a.applyTarget(rot)
	if got := cfg.EffortFor("codex"); got != "high" {
		t.Errorf("after applyTarget, EffortFor(codex) = %q, want high", got)
	}

	// Single-run path: applyRunTarget lands the effort in the top tier, alongside the model.
	cfg2 := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, cfg2, "codex", "work")
	a2 := &app{cfg: cfg2}
	if err := a2.applyRunTarget(agents.Target{Provider: "codex", Model: "gpt-5.6-sol", Effort: "xhigh", Accounts: []string{"work"}}); err != nil {
		t.Fatal(err)
	}
	if got := cfg2.EffortFor("codex"); got != "xhigh" {
		t.Errorf("after applyRunTarget, EffortFor(codex) = %q, want xhigh", got)
	}
	if got := cfg2.ModelFor("codex"); got != "gpt-5.6-sol" {
		t.Errorf("model rides alongside: ModelFor(codex) = %q, want gpt-5.6-sol", got)
	}
}

// A cross-provider ladder fans each rung across ITS OWN provider's accounts, so the loop rotates
// across agents; a rung whose provider has no signed-in account is skipped, not fatal, as long as
// another rung resolves.
func TestExpandLadderCrossProvider(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, cfg, "claude", "work")
	signInCred(t, cfg, "codex", "work")

	got, err := expandLadder(cfg, "claude", []agents.Target{{Model: "opus"}, {Provider: "codex", Model: "gpt-5"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude:opus@work", "codex:gpt-5@work"}; !slices.Equal(members(got), want) {
		t.Errorf("cross-provider ladder = %v, want %v (rotates across agents)", members(got), want)
	}
	// codex signed out → its rung is skipped, claude's remains (not a fatal "nothing signed in").
	noCodex := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, noCodex, "claude", "work")
	got, err = expandLadder(noCodex, "claude", []agents.Target{{Model: "opus"}, {Provider: "codex", Model: "gpt-5"}})
	if err != nil || !slices.Equal(members(got), []string{"claude:opus@work"}) {
		t.Errorf("unsigned provider rung should be skipped: got %v (%v)", members(got), err)
	}
}

func members(ts []agents.Target) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.String()
	}
	return out
}

// applyTarget points cfg at the target's account and model; a bare target clears the model tier.
func TestApplyTarget(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	r := newRotation([]agents.Target{{Provider: "claude", Model: "sonnet", Accounts: []string{"work"}}, {Provider: "claude", Accounts: []string{"other"}}})
	a.applyTarget(r)
	if a.cfg.ActiveProfile("claude") != "work" || a.cfg.ModelFor("claude") != "sonnet" {
		t.Errorf("first target: account=%q model=%q, want work/sonnet", a.cfg.ActiveProfile("claude"), a.cfg.ModelFor("claude"))
	}
	r.onLimit(time.Now().Add(time.Hour), 1, time.Now())
	a.applyTarget(r)
	if a.cfg.ModelFor("claude") != "" {
		t.Errorf("bare target: model = %q, want cleared", a.cfg.ModelFor("claude"))
	}
}

// oneOffLadder parses a decomposed one-off (model, account) into a single ladder entry,
// model-first with the model@account shortcut; conflicting accounts error.
func TestOneOffLadder(t *testing.T) {
	if l, _ := oneOffLadder("", ""); l != nil {
		t.Errorf("no one-off → nil ladder, got %v", l)
	}
	l, err := oneOffLadder("opus@work", "")
	if err != nil || len(l) != 1 || l[0].Model != "opus" || l[0].Account() != "work" {
		t.Errorf("model opus@work = %+v (%v)", l, err)
	}
	l, _ = oneOffLadder("opus", "work")
	if l[0].Model != "opus" || l[0].Account() != "work" {
		t.Errorf("model opus + account work = %+v", l)
	}
	if _, err := oneOffLadder("opus@work", "personal"); err == nil {
		t.Error("account given twice (model @work + account personal) should error")
	}
	if _, err := oneOffLadder("opus@", ""); err == nil {
		t.Error("empty account after @ should error")
	}
}
