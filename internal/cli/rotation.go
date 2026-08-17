package cli

import (
	"fmt"
	"slices"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ladder"
)

// A loop's rotation is the ordered set of targets it cycles through on rate limits —
// expanded from the lead's `agent:` target ladder against the runnable accounts. There is
// no persisted pool anymore: the ladder lives in the preset (or the run's one positional
// target), and "rotate all accounts" is just what a no-account target expands to. The cursor
// itself is internal/ladder (pure, clock injected); this file is the EXPANSION half — what the
// config says is signed in — shared by `coop loop`, `coop acp`, and `coop fork`. What a rotation
// then DOES (point cfg at the active target, rotate or sleep on a limit) is internal/loop's
// rotation.go, because only the loop rotates.
//
// Both the ladder and its expansion are the ONE agents.Target type: a ladder entry may
// carry an account list (or none = every runnable account); expandLadder turns it into
// concrete one-account rungs. A cross-provider ladder carries a different provider per
// rung, so the loop swaps the agent as it rotates. The limit map is keyed by the rung's
// wire form, so opus@work cooling leaves fable@work (or codex) free.

// accountsFor returns agent's runnable accounts in rotation order: the marked-default account
// first, then the rest as `coop credentials` lists them (alphabetical). An inspectable credential
// that needs re-login is present but not runnable, so rotation must not spend a request on it.
func accountsFor(cfg *config.Config, agent string) []string {
	def := cfg.DefaultProfileOf(agent)
	now := time.Now()
	var out []string
	if box.ProfileCredentialReady(cfg, agent, def, now) {
		out = append(out, def)
	}
	for _, c := range box.EffectiveProfiles(cfg, agent) {
		if c != def && box.ProfileCredentialReady(cfg, agent, c, now) {
			out = append(out, c)
		}
	}
	return out
}

// expandLadder turns a target ladder into the concrete rotation rungs: an entry with no
// accounts fans out across every runnable account of its provider (default first), a pinned
// account list becomes one rung per listed account (an unavailable one is skipped — a shared
// preset may name an account you don't have). Each rung runs its own Provider (defaultAgent
// when unset — a one-off ladder inherits the run's positional agent); a cross-provider lead
// ladder rotates across agents. An empty ladder means defaultAgent's default model across all
// accounts. Order preserved, deduped first-seen. Errors only when NOTHING in the ladder is
// runnable, and name the exact credential repair where the ladder selected one.
func expandLadder(cfg *config.Config, defaultAgent string, rungs []agents.Target) ([]agents.Target, error) {
	if len(rungs) == 0 {
		rungs = []agents.Target{{}} // defaultAgent, default model, all accounts
	}
	var out []agents.Target
	seen := map[string]bool{}
	add := func(t agents.Target) {
		if !seen[t.String()] {
			seen[t.String()] = true
			out = append(out, t)
		}
	}
	var missing []string // fan-out providers with no runnable account (reported only if NOTHING resolves)
	type repair struct {
		target  string
		relogin bool
	}
	var repairs []repair // selected or fan-out credentials whose exact login fixes an empty ladder
	recordRepair := func(agent, account string, pinned bool) {
		if box.ProfileCredentialReady(cfg, agent, account, time.Now()) {
			return
		}
		present := box.ProfileAuthed(cfg, agent, account)
		if !present && !pinned {
			return
		}
		target := agent + "@" + account
		if !slices.ContainsFunc(repairs, func(r repair) bool { return r.target == target }) {
			repairs = append(repairs, repair{target: target, relogin: present})
		}
	}
	for _, e := range rungs {
		agent := e.Provider
		if agent == "" {
			agent = defaultAgent
		}
		runnable := accountsFor(cfg, agent)
		accounts := e.Accounts
		if len(accounts) == 0 {
			if len(runnable) == 0 {
				for _, account := range box.EffectiveProfiles(cfg, agent) {
					recordRepair(agent, account, false)
				}
				if !slices.Contains(missing, agent) {
					missing = append(missing, agent)
				}
				continue
			}
			accounts = runnable
		}
		for _, acct := range accounts {
			if box.ProfileCredentialReady(cfg, agent, acct, time.Now()) {
				add(agents.Target{Provider: agent, Model: e.Model, Effort: e.Effort, Accounts: []string{acct}})
			} else {
				recordRepair(agent, acct, true)
			}
		}
	}
	if len(out) == 0 {
		if len(repairs) == 1 {
			state := "is not signed in"
			if repairs[0].relogin {
				state = "requires re-login"
			}
			return nil, fmt.Errorf("credential %s %s — run: %s", agents.DisplayTarget(repairs[0].target), state, agents.LoginCommand(repairs[0].target))
		}
		if len(repairs) > 1 {
			targets, commands := make([]string, len(repairs)), make([]string, len(repairs))
			for i, repair := range repairs {
				targets[i] = agents.DisplayTarget(repair.target)
				commands[i] = agents.LoginCommand(repair.target)
			}
			return nil, fmt.Errorf("credentials need login (%s) — run: %s", strings.Join(targets, ", "), strings.Join(commands, "; "))
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("no signed-in account for %s — run: coop login %s[@account]", strings.Join(missing, ", "), missing[0])
		}
		return nil, fmt.Errorf("%s: none of the ladder's accounts are signed in — run 'coop login', or edit the preset", defaultAgent)
	}
	return out, nil
}

// buildRotation resolves a loop's rotation for agent: the run's one-off ladder when the
// positional target carried a model/account, else the preset lead's ladder, else the default. (A
// custom work.command ignores the rotation — the loop runs the raw command — so it isn't special-cased.)
func (a *app) buildRotation(agent string, rungs []agents.Target) (*ladder.Rotation, error) {
	targets, err := expandLadder(a.cfg, agent, rungs)
	if err != nil {
		return nil, err
	}
	return ladder.NewRotation(targets), nil
}

// oneOffLadder builds a single-entry ladder from a run's decomposed one-off selection — the
// model, effort, and account a fork worker (or applyOneOff) parsed out of its positional target.
// model may carry a model@account shortcut; credential pins the account. Giving the account twice
// (both model's @ and credential) is an error. Returns nil when all are empty (caller falls
// back to the preset/default). Provider stays "" — expandLadder fills the run's agent in.
func oneOffLadder(model, credential, effort string) ([]agents.Target, error) {
	if model == "" && credential == "" && effort == "" {
		return nil, nil
	}
	m, atAcct, hadAt := strings.Cut(model, "@")
	m, atAcct = strings.TrimSpace(m), strings.TrimSpace(atAcct)
	if hadAt && atAcct == "" {
		return nil, fmt.Errorf("model %q has an empty account after @ — use model@account, or drop the @", model)
	}
	cred := strings.TrimSpace(credential)
	if strings.Contains(cred, "@") {
		return nil, fmt.Errorf("account %q carries an @ — an account is a bare name; put the model in the target's model slot", cred)
	}
	if atAcct != "" && cred != "" && atAcct != cred {
		return nil, fmt.Errorf("account given twice — model %s and account %s disagree", model, credential)
	}
	if atAcct != "" {
		cred = atAcct
	}
	t := agents.Target{Model: m, Effort: effort}
	if cred != "" {
		t.Accounts = []string{cred}
	}
	return []agents.Target{t}, nil
}
