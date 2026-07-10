package cli

import (
	"fmt"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/ui"
)

// A loop's rotation is the ordered set of (model, account) targets it cycles through on
// rate limits — expanded from the lead's model-first `models:` ladder against the
// signed-in accounts. There is no persisted pool anymore: the ladder lives in the preset
// (or a one-off --model/--credential), and "rotate all accounts" is just what a bare model
// expands to. Pure (clock injected), so the policy is unit-tested without sleeping.

// runTarget is one concrete thing a loop iteration runs: a model on an account. A bare
// ladder model fans out to one runTarget per signed-in account; a pinned model@account is
// one. The limit map is keyed by runTarget, so opus@work cooling leaves fable@work free.
type runTarget struct {
	credential string
	model      string // "" = the account's default model resolves (mark/env/agent default)
}

func (t runTarget) String() string {
	if t.model == "" {
		return t.credential
	}
	return t.model + "@" + t.credential
}

// rotation is the loop's ordered targets + which are rate-limited + a sticky cursor that
// stays on one until it's limited, then advances.
type rotation struct {
	targets []runTarget
	limited map[runTarget]time.Time
	idx     int
}

func newRotation(targets []runTarget) *rotation {
	return &rotation{targets: targets, limited: map[runTarget]time.Time{}}
}

// accountsFor returns agent's signed-in accounts in rotation order: the marked-default
// account first, then the rest as `coop credentials` lists them (alphabetical). Empty when
// none are signed in.
func accountsFor(cfg *config.Config, agent string) []string {
	def := cfg.DefaultProfileOf(agent)
	var out []string
	if box.ProfileAuthed(cfg, agent, def) {
		out = append(out, def)
	}
	for _, c := range cfg.Profiles(agent) {
		if c != def && box.ProfileAuthed(cfg, agent, c) {
			out = append(out, c)
		}
	}
	return out
}

// expandLadder turns a model-first ladder into the concrete rotation targets: a bare model
// fans out across every signed-in account (default first), a pinned model@account is one
// target (skipped if that account isn't signed in — a shared preset may name an account you
// don't have). An empty ladder means the agent's default model across all accounts (the
// zero-config default). Order preserved, deduped first-seen. Errors when nothing is
// signed in — the loop can't run unauthenticated.
func expandLadder(cfg *config.Config, agent string, ladder []preset.ModelTarget) ([]runTarget, error) {
	accounts := accountsFor(cfg, agent)
	if len(accounts) == 0 {
		return nil, fmt.Errorf("%s has no signed-in account — run: coop login %s[@account]", agent, agent)
	}
	if len(ladder) == 0 {
		ladder = []preset.ModelTarget{{}} // default model, all accounts
	}
	var out []runTarget
	seen := map[runTarget]bool{}
	add := func(t runTarget) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, e := range ladder {
		if e.Credential != "" {
			if box.ProfileAuthed(cfg, agent, e.Credential) {
				add(runTarget{credential: e.Credential, model: e.Model})
			}
			continue
		}
		for _, acct := range accounts {
			add(runTarget{credential: acct, model: e.Model})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: none of the models ladder's accounts are signed in — run 'coop login', or edit the preset", agent)
	}
	return out, nil
}

// buildRotation resolves a loop's rotation for agent: a one-off override ladder when the
// caller passed --model/--credential, else the preset lead's ladder, else the default. A
// custom COOP_LOOP_CMD isn't an agent, so it never rotates — just its marked default account.
func (a *app) buildRotation(agent string, ladder []preset.ModelTarget) (*rotation, error) {
	if len(a.cfg.LoopCmd) > 0 {
		return newRotation([]runTarget{{credential: a.cfg.DefaultProfileOf(agent)}}), nil
	}
	targets, err := expandLadder(a.cfg, agent, ladder)
	if err != nil {
		return nil, err
	}
	return newRotation(targets), nil
}

// applyTarget points cfg at the rotation's active target: the account the next iteration
// mounts, and its model (empty clears the tier, so a bare-model target falls through to the
// account's mark / env / agent default). The one choke point for loop start + each rotation.
func (a *app) applyTarget(agent string, r *rotation) {
	t := r.active()
	a.cfg.SetActiveProfile(agent, t.credential)
	a.cfg.SetTargetModel(agent, t.model)
}

// rotateOnLimit handles a rate limit with more than one target: advance, point cfg at the
// new active target, and either switch immediately (a free rotation is progress) or, when
// every target is limited, sleep until the soonest reset.
func (a *app) rotateOnLimit(agent string, r *rotation, resetAt time.Time, waits *int, wake <-chan struct{}) {
	prev := r.active()
	sleep, until := r.onLimit(resetAt, *waits, time.Now())
	a.applyTarget(agent, r)
	if sleep > 0 {
		ui.Info("all %d %s targets are rate limited — waiting for the soonest reset", len(r.targets), agent)
		sleepForLimit(sleep, until, wake)
		return
	}
	ui.Info("%s target %q rate limited — switching to %q", agent, prev, r.active())
	*waits = 0 // only consecutive all-limited waits count toward the stop cap
}

func (r *rotation) active() runTarget { return r.targets[r.idx] }

// members renders the rotation in wire form (model@account), for messages and tests.
func (r *rotation) members() []string {
	out := make([]string, len(r.targets))
	for i, t := range r.targets {
		out[i] = t.String()
	}
	return out
}

// rotates reports whether there's more than one target to switch between.
func (r *rotation) rotates() bool { return len(r.targets) > 1 }

// onLimit records that the active target is rate-limited until resetAt (a zero resetAt
// means "unknown", so it backs off by attempt), then advances to the next usable target.
// Keyed per target, so opus@work cooling leaves fable@work free. Returns the sleep before
// the next iteration — 0 when another target is free now — and, when sleeping, the time
// it's waiting until.
func (r *rotation) onLimit(resetAt time.Time, attempt int, now time.Time) (sleep time.Duration, until time.Time) {
	if resetAt.IsZero() {
		resetAt = now.Add(limitWait(limitHint{limited: true}, attempt, now))
	}
	r.limited[r.targets[r.idx]] = resetAt
	n := len(r.targets)
	for i := 1; i <= n; i++ {
		cand := (r.idx + i) % n
		if t, ok := r.limited[r.targets[cand]]; !ok || !t.After(now) {
			r.idx = cand
			return 0, time.Time{}
		}
	}
	earliest := 0
	for i := range r.targets {
		if r.limited[r.targets[i]].Before(r.limited[r.targets[earliest]]) {
			earliest = i
		}
	}
	r.idx = earliest
	until = r.limited[r.targets[earliest]]
	return limitWait(limitHint{limited: true, resetAt: until}, attempt, now), until
}

// oneOffLadder builds a single-entry ladder from --model / --credential for a run that
// isn't using a preset's models. --model may carry a model@account shortcut; --credential
// pins the account. Giving the account twice (both --model's @ and --credential) is an
// error. Returns nil when neither flag is set (caller falls back to the preset/default).
func oneOffLadder(modelFlag, credFlag string) ([]preset.ModelTarget, error) {
	if modelFlag == "" && credFlag == "" {
		return nil, nil
	}
	model, atAcct, hadAt := strings.Cut(modelFlag, "@")
	model, atAcct = strings.TrimSpace(model), strings.TrimSpace(atAcct)
	if hadAt && atAcct == "" {
		return nil, fmt.Errorf("--model %q has an empty account after @ — use --model model@account, or drop the @", modelFlag)
	}
	cred := strings.TrimSpace(credFlag)
	if strings.Contains(cred, "@") {
		return nil, fmt.Errorf("--credential takes an account name, not model@account — put the model in --model (e.g. --model %s)", cred)
	}
	if atAcct != "" && cred != "" && atAcct != cred {
		return nil, fmt.Errorf("account given twice — --model %s and --credential %s disagree", modelFlag, credFlag)
	}
	if atAcct != "" {
		cred = atAcct
	}
	return []preset.ModelTarget{{Model: model, Credential: cred}}, nil
}

// targetLadder turns one positional target into the loop's model-first ladder: no accounts
// → a bare model that fans across every signed-in account (expandLadder's zero-config
// default); an account ladder @a,b → one entry per account, in order. The provider is the
// run's agent, carried separately (expandLadder takes it as a param).
func targetLadder(t agents.Target) []preset.ModelTarget {
	if len(t.Accounts) == 0 {
		return []preset.ModelTarget{{Model: t.Model}}
	}
	out := make([]preset.ModelTarget, len(t.Accounts))
	for i, acct := range t.Accounts {
		out[i] = preset.ModelTarget{Model: t.Model, Credential: acct}
	}
	return out
}
