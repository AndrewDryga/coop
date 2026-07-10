package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/preset"
)

// rts builds a claude rotation from bare account names (model empty), for the engine tests.
// String() renders each as "claude@<acct>".
func rts(creds ...string) *rotation {
	ts := make([]runTarget, len(creds))
	for i, c := range creds {
		ts[i] = runTarget{provider: "claude", credential: c}
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

// The acceptance scenario, per (model,account) key: models: [fable, opus@work] over accounts
// {work, personal} → fable@work → fable@personal (same model, other account) → opus@work; and
// fable@work cooling never blocks opus@work.
func TestRotationSameModelFallback(t *testing.T) {
	now := time.Unix(1000, 0)
	r := newRotation([]runTarget{
		{provider: "claude", model: "fable", credential: "work"},
		{provider: "claude", model: "fable", credential: "personal"},
		{provider: "claude", model: "opus", credential: "work"},
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
	got, err := expandLadder(cfg, "claude", []preset.ModelTarget{{Model: "opus"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude:opus@personal", "claude:opus@work"}; !slices.Equal(members(got), want) {
		t.Errorf("bare model fan-out = %v, want %v (default first)", members(got), want)
	}
	// Ladder: bare then pinned; dedup, order preserved.
	got, _ = expandLadder(cfg, "claude", []preset.ModelTarget{{Model: "fable"}, {Model: "opus", Credential: "work"}})
	if want := []string{"claude:fable@personal", "claude:fable@work", "claude:opus@work"}; !slices.Equal(members(got), want) {
		t.Errorf("ladder = %v, want %v", members(got), want)
	}
	// Empty ladder → default model (empty) across accounts (provider shown, no model).
	got, _ = expandLadder(cfg, "claude", nil)
	if want := []string{"claude@personal", "claude@work"}; !slices.Equal(members(got), want) {
		t.Errorf("empty ladder = %v, want the accounts on the default model", members(got))
	}
	// A pinned account that isn't signed in is skipped, not an error, as long as something remains.
	got, _ = expandLadder(cfg, "claude", []preset.ModelTarget{{Model: "opus", Credential: "ghost"}, {Model: "fable"}})
	if slices.Contains(members(got), "claude:opus@ghost") {
		t.Errorf("unsigned pinned account should be skipped: %v", members(got))
	}
	// No signed-in accounts at all → error.
	if _, err := expandLadder(&config.Config{ConfigDir: t.TempDir()}, "claude", nil); err == nil {
		t.Error("no signed-in account should error")
	}
}

// A cross-provider ladder fans each rung across ITS OWN provider's accounts, so the loop rotates
// across agents; a rung whose provider has no signed-in account is skipped, not fatal, as long as
// another rung resolves.
func TestExpandLadderCrossProvider(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, cfg, "claude", "work")
	signInCred(t, cfg, "codex", "work")

	got, err := expandLadder(cfg, "claude", []preset.ModelTarget{{Model: "opus"}, {Provider: "codex", Model: "gpt-5"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude:opus@work", "codex:gpt-5@work"}; !slices.Equal(members(got), want) {
		t.Errorf("cross-provider ladder = %v, want %v (rotates across agents)", members(got), want)
	}
	// codex signed out → its rung is skipped, claude's remains (not a fatal "nothing signed in").
	noCodex := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, noCodex, "claude", "work")
	got, err = expandLadder(noCodex, "claude", []preset.ModelTarget{{Model: "opus"}, {Provider: "codex", Model: "gpt-5"}})
	if err != nil || !slices.Equal(members(got), []string{"claude:opus@work"}) {
		t.Errorf("unsigned provider rung should be skipped: got %v (%v)", members(got), err)
	}
}

func members(ts []runTarget) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.String()
	}
	return out
}

// applyTarget points cfg at the target's account and model; a bare target clears the model tier.
func TestApplyTarget(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	r := newRotation([]runTarget{{provider: "claude", model: "sonnet", credential: "work"}, {provider: "claude", credential: "other"}})
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

// oneOffLadder parses --model/--credential into a single ladder entry, model-first with the
// @account shortcut; conflicting accounts error.
func TestOneOffLadder(t *testing.T) {
	if l, _ := oneOffLadder("", ""); l != nil {
		t.Errorf("no flags → nil ladder, got %v", l)
	}
	l, err := oneOffLadder("opus@work", "")
	if err != nil || len(l) != 1 || l[0].Model != "opus" || l[0].Credential != "work" {
		t.Errorf("--model opus@work = %+v (%v)", l, err)
	}
	l, _ = oneOffLadder("opus", "work")
	if l[0].Model != "opus" || l[0].Credential != "work" {
		t.Errorf("--model opus --credential work = %+v", l)
	}
	if _, err := oneOffLadder("opus@work", "personal"); err == nil {
		t.Error("account given twice (--model @work + --credential personal) should error")
	}
	if _, err := oneOffLadder("opus@", ""); err == nil {
		t.Error("empty account after @ should error")
	}
}
