package agent

import (
	"fmt"
	"strings"
)

// Target is coop's single addressing scheme — WHO runs on WHAT: a provider, optionally a
// model, optionally a reasoning effort, optionally one or more accounts. It is the ONE spelling
// used on the CLI, in preset `agent:` keys, and in fleet entries, parsed by ParseTarget. The
// wire grammar is
//
//	provider[:model][/effort][@account[,account…]]
//
//	claude                     provider only          (model → the CLI default; all accounts)
//	claude:opus                provider + model
//	claude:opus/xhigh          provider + model + reasoning effort
//	codex/high@work            provider + effort + account (default model)
//	claude:opus@work,personal  provider + model + an ACCOUNT LADDER (fan out work → personal)
//
// `:` splits provider from model; `/` sets reasoning effort (a level the agent's CLI accepts —
// low/medium/high/xhigh/max — coop passes it through, the agent validates); `@` starts the
// account slot; `,` separates ACCOUNTS (and only accounts, at every level — a list of peers is
// repeated flags, never a comma list).
type Target struct {
	Provider string   // a registered agent; required
	Model    string   // "" = the agent CLI's own default model
	Effort   string   // "" = the agent CLI's own default; else a reasoning-effort level passed to the agent
	Accounts []string // nil/empty = every signed-in account (the widest ladder); else the explicit subset, in order
}

// ParseTarget parses one target token, validating SYNTAX and that the provider is a
// registered agent. It does NOT check that the accounts exist (that needs the config's
// signed-in list — the caller validates against cfg.Profiles). A malformed token returns an
// error naming the fix; this is the single place every surface funnels through, so the
// diagnostics are identical everywhere.
func ParseTarget(s string) (Target, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Target{}, fmt.Errorf("empty target — name a provider (%s), optionally provider:model@account", strings.Join(Names(), ", "))
	}
	head, accountSlot, hasAt := strings.Cut(raw, "@")
	if strings.Contains(accountSlot, "@") {
		return Target{}, fmt.Errorf("%q has more than one @ — the account rides one @ (provider:model@account,account)", raw)
	}
	// Effort binds to the model, before the account (provider:model/effort@account). A '/' in the
	// account slot is left to the account path-safety check below, which rejects it as an invalid
	// account — so `@work/high` and a traversal like `@../x` both get the account error.
	modelSpec, effort, hasSlash := strings.Cut(head, "/")
	provider, model, hasColon := strings.Cut(modelSpec, ":")
	provider = strings.TrimSpace(provider)
	if !Valid(provider) {
		return Target{}, fmt.Errorf("unknown provider %q in %q — use %s", provider, raw, strings.Join(Names(), ", "))
	}
	if hasColon {
		model = strings.TrimSpace(model)
		if model == "" {
			return Target{}, fmt.Errorf("%q has an empty model after ':' — use provider:model, or drop the ':'", raw)
		}
		if strings.ContainsAny(model, ":@/ \t") {
			return Target{}, fmt.Errorf("invalid model %q in %q — a model id has no ':' '@' '/' or spaces", model, raw)
		}
	}
	effort = strings.TrimSpace(effort)
	if hasSlash {
		if effort == "" {
			return Target{}, fmt.Errorf("%q has an empty effort after '/' — use provider:model/effort (e.g. low, medium, high, xhigh, max), or drop the '/'", raw)
		}
		if !isEffortLevel(effort) {
			return Target{}, fmt.Errorf("invalid effort %q in %q — a reasoning level is lowercase letters (e.g. low, medium, high, xhigh, max)", effort, raw)
		}
		if a, _ := Get(provider); a != nil && !SupportsEffort(a) {
			return Target{}, fmt.Errorf("%s has no reasoning-effort control — drop the /%s from %q", provider, effort, raw)
		}
	}
	t := Target{Provider: provider, Model: model, Effort: effort}
	if hasAt {
		for _, a := range strings.Split(accountSlot, ",") {
			a = strings.TrimSpace(a)
			if a == "" {
				return Target{}, fmt.Errorf("%q has an empty account — list accounts as @a,b, not a trailing/`,,` comma", raw)
			}
			// An account becomes a profile DIRECTORY name, so it must be a single path-safe
			// segment — no separators or traversal, and no leading '-' (would look like a flag).
			if strings.ContainsAny(a, ":@/\\") || a == "." || a == ".." || strings.HasPrefix(a, "-") {
				return Target{}, fmt.Errorf("invalid account %q in %q — a single path-safe segment (no ':' '@' '/' '..' or leading '-')", a, raw)
			}
			t.Accounts = append(t.Accounts, a)
		}
	}
	return t, nil
}

// isEffortLevel reports whether s is a syntactically valid reasoning-effort token: lowercase
// letters only (low/medium/high/xhigh/max/minimal, and whatever future levels an agent adds).
// coop does NOT check the value against a per-agent list — like a model id it's passed straight
// to the agent, whose own CLI rejects an unknown one, so a new level works the day it ships.
func isEffortLevel(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// Account returns the target's single account — the first listed, or "" when none. It is the
// rung view: a rotation ladder expands a multi-account target into concrete one-account rungs
// (expandLadder), so rung consumers (the loop, ACP, applyPreset) read this instead of Accounts.
func (t Target) Account() string {
	if len(t.Accounts) == 0 {
		return ""
	}
	return t.Accounts[0]
}

// String renders a target back to its wire form (provider[:model][/effort][@a,b]) — for
// messages, config round-trips, and tests. A Target with only a provider is just the provider.
func (t Target) String() string {
	s := t.Provider
	if t.Model != "" {
		s += ":" + t.Model
	}
	if t.Effort != "" {
		s += "/" + t.Effort
	}
	if len(t.Accounts) > 0 {
		s += "@" + strings.Join(t.Accounts, ",")
	}
	return s
}
