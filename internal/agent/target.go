package agent

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Target is coop's single addressing scheme — WHO runs on WHAT: a provider, optionally a
// model, optionally a reasoning effort, optionally one or more accounts. It is the ONE spelling
// used on the CLI and in preset `agent:` keys, parsed by ParseTarget. The
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

// LoginCommand renders a copy-pasteable shell command for one provider@account target. Profile
// directories can predate today's CLI validation or be created by hand, so quote an unsafe target
// instead of letting its name become shell syntax in an actionable error.
func LoginCommand(target string) string {
	printable := true
	for _, r := range target {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			strings.ContainsRune("_@.+-", r) {
			continue
		}
		if !unicode.IsPrint(r) {
			printable = false
			break
		}
	}
	if !printable {
		quoted := strconv.QuoteToASCII(target)
		return "coop login $'" + strings.ReplaceAll(quoted[1:len(quoted)-1], `'`, `\'`) + "'"
	}
	for _, r := range target {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			strings.ContainsRune("_@.+-", r) {
			continue
		}
		return "coop login '" + strings.ReplaceAll(target, "'", `'"'"'`) + "'"
	}
	return "coop login " + target
}

// DisplayTarget keeps credential diagnostics one-line and terminal-safe even if a legacy profile
// directory or hand-written preset contains control characters.
func DisplayTarget(target string) string {
	for _, r := range target {
		if !unicode.IsPrint(r) {
			return strconv.QuoteToASCII(target)
		}
	}
	return target
}

// TargetErrorKind says which slot of the grammar a token broke, because each one is refused
// differently by the surface that owns the terminal: an empty token is a MISSING argument (whose
// usage line only the command knows), an unrecognized provider is named against the command that
// was typed, and everything else already knows its own headline.
type TargetErrorKind int

const (
	TargetInvalid         TargetErrorKind = iota // a syntax rule the token broke
	TargetEmpty                                  // no target at all
	TargetUnknownProvider                        // the provider slot names no registered agent
)

// TargetError is a rejected target as DATA — the headline, the constraint behind it, and the help
// page that explains the rule — so the CLI renders coop's one refusal block from it and this
// package keeps no presentation dependency (see .agent/kb/rules/internal-import-dag.md). A
// consumer that only logs an error gets the same facts on one line from Error().
type TargetError struct {
	Kind     TargetErrorKind
	Target   string // the token as typed, made display-safe
	Provider string // the provider slot, for TargetUnknownProvider
	Headline string // what was refused ("Invalid agent target \"claude:\"")
	Cause    string // the constraint, one sentence per line
	Help     string // the command path whose page explains this rule: "models", "login"
}

func (e *TargetError) Error() string {
	if e.Cause == "" {
		return e.Headline
	}
	return e.Headline + " — " + strings.Join(strings.Split(e.Cause, "\n"), " ")
}

// ParseTarget parses one target token, validating SYNTAX and that the provider is a
// registered agent. It does NOT check that the accounts exist (that needs the config's
// signed-in list — the caller validates against cfg.Profiles). A malformed token returns a
// *TargetError naming the fix; this is the single place every surface funnels through, so the
// diagnostics are identical everywhere.
func ParseTarget(s string) (Target, error) {
	raw := strings.TrimSpace(s)
	bad := func(headline, cause, help string) (Target, error) {
		return Target{}, &TargetError{Target: DisplayTarget(raw), Headline: headline, Cause: cause, Help: help}
	}
	invalid := func(cause string) (Target, error) {
		return bad(fmt.Sprintf("Invalid agent target %q", DisplayTarget(raw)), cause, "models")
	}
	if raw == "" {
		return Target{}, &TargetError{Kind: TargetEmpty, Headline: "Missing agent target", Cause: "Name an agent, such as " + Names()[0] + ".", Help: "models"}
	}
	head, accountSlot, hasAt := strings.Cut(raw, "@")
	if strings.Contains(accountSlot, "@") {
		return invalid(`Use one "@" before the account name.`)
	}
	// Effort binds to the model, before the account (provider:model/effort@account). A '/' in the
	// account slot is left to the account path-safety check below, which rejects it as an invalid
	// account — so `@work/high` and a traversal like `@../x` both get the account error.
	modelSpec, effort, hasSlash := strings.Cut(head, "/")
	provider, model, hasColon := strings.Cut(modelSpec, ":")
	provider = strings.TrimSpace(provider)
	if !Valid(provider) {
		return Target{}, &TargetError{
			Kind:     TargetUnknownProvider,
			Target:   DisplayTarget(raw),
			Provider: DisplayTarget(provider),
			Headline: fmt.Sprintf("Unknown agent %q", DisplayTarget(raw)),
			Cause:    "Choose " + list(Names(), "or") + ".",
			Help:     "models",
		}
	}
	if hasColon {
		model = strings.TrimSpace(model)
		if model == "" {
			return invalid(`Add a model after ":" or remove the colon.`)
		}
		if strings.ContainsAny(model, ":@/ \t") {
			return invalid(`A model name cannot contain ":", "@", "/", or spaces.`)
		}
	}
	effort = strings.TrimSpace(effort)
	if hasSlash {
		if effort == "" {
			return invalid(`Add a reasoning effort after "/" or remove the slash.`)
		}
		if !isEffortLevel(effort) {
			return invalid(`Write the reasoning effort in lowercase letters, such as "high".`)
		}
		a, _ := Get(provider) // registered: Valid(provider) held above
		if !SupportsEffort(a) {
			// The agent's own name title-cased, not DisplayName() — the refusal is about the
			// token the user typed ("gemini"), not about the product behind it.
			return bad(strings.ToUpper(provider[:1])+provider[1:]+" does not support a reasoning-effort setting",
				fmt.Sprintf("Remove %q from %q.", "/"+effort, DisplayTarget(raw)), "models")
		}
		if err := ValidateEffort(a, model, effort); err != nil {
			cause := err.Error()
			return invalid(strings.ToUpper(cause[:1]) + cause[1:] + ".")
		}
	}
	t := Target{Provider: provider, Model: model, Effort: effort}
	if hasAt {
		for _, a := range strings.Split(accountSlot, ",") {
			a = strings.TrimSpace(a)
			if a == "" {
				return invalid(`Add an account after "@" or remove the at sign.`)
			}
			// An account becomes a profile DIRECTORY name, so it must be a single path-safe
			// segment — no separators or traversal, and no leading '-' (would look like a flag).
			if strings.ContainsAny(a, ":@/\\") || a == "." || a == ".." || strings.HasPrefix(a, "-") {
				return bad(fmt.Sprintf("Invalid account name %q", DisplayTarget(a)),
					"Use one name, without \":\", \"@\", \"/\", or \"\\\\\".\nThe name cannot be \".\", \"..\", or start with \"-\".", "login")
			}
			t.Accounts = append(t.Accounts, a)
		}
	}
	return t, nil
}

// list punctuates a set the way a sentence does ("claude, codex, gemini, or grok"). ui.List says
// the same thing for the surfaces that own the terminal; this package cannot import it.
func list(items []string, conj string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " " + conj + " " + items[1]
	}
	return strings.Join(items[:len(items)-1], ", ") + ", " + conj + " " + items[len(items)-1]
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
