package cli

import (
	"fmt"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// noProviderErr is coop's one actionable message when a launch names no provider (and no
// preset supplies a lead) — the implicit claude default is gone, so every agent-launching
// surface routes an empty agent here. cmd is the command word for the usage line
// ("loop", "acp", "fork", "" for a bare `coop`).
func noProviderErr(cmd string) error {
	usage := "coop " + cmd
	if cmd == "" {
		usage = "coop"
	}
	return fmt.Errorf("name the agent — %s <%s> (or a preset name); see 'coop credentials' for who's signed in",
		strings.TrimSpace(usage), strings.Join(agents.Names(), "|"))
}

// takeHeadTarget pulls a leading positional target off args (provider REQUIRED — a bare
// provider, or provider:model@account,…). Returns ok=false, leaving args untouched, when
// the first token is a flag or absent; a token whose head is a registered provider parses
// as a target (a malformed one errors); a non-provider, non-flag token is left for the
// caller to try as a preset name.
func takeHeadTarget(args []string) (t agents.Target, ok bool, rest []string, err error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return agents.Target{}, false, args, nil
	}
	head := args[0]
	beforeAt, _, _ := strings.Cut(head, "@")
	provider, _, _ := strings.Cut(beforeAt, ":")
	if !agents.Valid(provider) {
		if strings.ContainsAny(head, ":@") { // meant as a target, so surface the parse error
			_, perr := agents.ParseTarget(head)
			return agents.Target{}, false, args, perr
		}
		return agents.Target{}, false, args, nil // not a target head — caller may try it as a preset
	}
	t, err = agents.ParseTarget(head)
	if err != nil {
		return agents.Target{}, false, args, err
	}
	return t, true, args[1:], nil
}

// retiredTargetFlagErr tombstones --model/--credential (v3-clean): they're now segments of
// the positional target. "" when neither is present.
func retiredTargetFlagErr(args []string) error {
	for _, a := range args {
		f, _, _ := strings.Cut(a, "=")
		switch f {
		case "--model":
			return fmt.Errorf("--model is retired — put the model in the target: coop <cmd> <provider>:<model> (e.g. claude:opus-4.8)")
		case "--credential", "--credentials":
			return fmt.Errorf("--credential is retired — put the account in the target: coop <cmd> <provider>@<account> (e.g. claude@work)")
		}
	}
	return nil
}

// isTargetHead reports whether s begins with a registered provider (so `coop <s>` names an
// agent run, not a command/preset). Used by the top-level dispatch.
func isTargetHead(s string) bool {
	beforeAt, _, _ := strings.Cut(s, "@")
	provider, _, _ := strings.Cut(beforeAt, ":")
	return agents.Valid(provider)
}

// singleAccount returns the one account of a target for a NON-loop run (interactive, acp, a
// non-loop fork): "" for none, the account for one, an error for a ladder (@a,b only rotates
// under `coop loop`).
func singleAccount(t agents.Target) (string, error) {
	switch len(t.Accounts) {
	case 0:
		return "", nil
	case 1:
		return t.Accounts[0], nil
	default:
		return "", fmt.Errorf("an account ladder (@%s) only applies to `coop loop`; a single run takes one account", strings.Join(t.Accounts, ","))
	}
}

// foldTarget folds a positional target's model + single account into a run's one-off
// model/profile strings — acp and fork thread these through their inner box rather than
// through selectRun*. Only a non-empty segment overwrites, so a bare `provider` leaves an
// env-supplied model/account (a preset-rotation rung) intact. A >1-account ladder errors
// (loop-only). The caller takes the provider from t.Provider.
func foldTarget(t agents.Target, model, profile *string) error {
	if t.Model != "" {
		*model = t.Model
	}
	acct, err := singleAccount(t)
	if err != nil {
		return err
	}
	if acct != "" {
		*profile = acct
	}
	return nil
}

// applyTarget seeds a single run's model + account selection from a target (the loop uses the
// rotation ladder instead). A >1-account ladder is rejected (loop-only).
func (a *app) applyRunTarget(t agents.Target) error {
	acct, err := singleAccount(t)
	if err != nil {
		return err
	}
	if err := a.selectRunProfile(t.Provider, acct); err != nil {
		return err
	}
	a.selectRunModel(t.Provider, t.Model)
	return nil
}
