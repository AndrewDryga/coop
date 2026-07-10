package agent

import (
	"fmt"
	"strings"
)

// Target is coop's single addressing scheme — WHO runs on WHAT: a provider, optionally a
// model, optionally one or more accounts. It is the ONE spelling used on the CLI, in preset
// `agent:` keys, and in fleet entries, parsed by ParseTarget. The wire grammar is
//
//	provider[:model][@account[,account…]]
//
//	claude                     provider only          (model → the CLI default; all accounts)
//	claude:opus            provider + model
//	claude@work                provider + account     (model → the CLI default)
//	claude:opus@work       provider + model + account
//	claude:opus@work,personal  provider + model + an ACCOUNT LADDER (fan out work → personal)
//
// `:` splits provider from model; `@` starts the account slot; `,` separates ACCOUNTS (and
// only accounts, at every level — a list of peers is repeated flags, never a comma list).
type Target struct {
	Provider string   // a registered agent; required
	Model    string   // "" = the agent CLI's own default model
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
	provider, model, hasColon := strings.Cut(head, ":")
	provider = strings.TrimSpace(provider)
	if !Valid(provider) {
		return Target{}, fmt.Errorf("unknown provider %q in %q — use %s", provider, raw, strings.Join(Names(), ", "))
	}
	if hasColon {
		model = strings.TrimSpace(model)
		if model == "" {
			return Target{}, fmt.Errorf("%q has an empty model after ':' — use provider:model, or drop the ':'", raw)
		}
		if strings.ContainsAny(model, ":@ \t") {
			return Target{}, fmt.Errorf("invalid model %q in %q — a model id has no ':' '@' or spaces", model, raw)
		}
	}
	t := Target{Provider: provider, Model: model}
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

// String renders a target back to its wire form (provider[:model][@a,b]) — for messages,
// config round-trips, and tests. A Target with no model/accounts is just the provider.
func (t Target) String() string {
	s := t.Provider
	if t.Model != "" {
		s += ":" + t.Model
	}
	if len(t.Accounts) > 0 {
		s += "@" + strings.Join(t.Accounts, ",")
	}
	return s
}
