package cli

import (
	"fmt"
	"slices"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
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

// isTargetHead reports whether s begins with a registered provider (so `coop <s>` names an
// agent run, not a command/preset). Used by the top-level dispatch.
func isTargetHead(s string) bool {
	beforeAt, _, _ := strings.Cut(s, "@")
	provider, _, _ := strings.Cut(beforeAt, ":")
	return agents.Valid(provider)
}

// takeHeadWho pulls the leading "who runs" positional off args — the unified grammar shared by
// loop/fork/fusion/acp: a TARGET (provider[:model][/effort][@account], detected by isTargetHead)
// OR a PRESET NAME (any other bare word — its existence is validated by the caller's loadRunPreset).
// A run picks ONE, so exactly one of hasTarget / presetName!="" is set. ok=false with both empty
// leaves args untouched when the first token is a flag or absent. A malformed target head still
// errors (ParseTarget's own diagnostic), so a typo'd provider isn't silently taken as a preset.
func takeHeadWho(args []string) (t agents.Target, hasTarget bool, presetName string, rest []string, err error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return agents.Target{}, false, "", args, nil
	}
	if !isTargetHead(args[0]) {
		return agents.Target{}, false, args[0], args[1:], nil // a preset name
	}
	t, err = agents.ParseTarget(args[0])
	if err != nil {
		return agents.Target{}, false, "", args, err
	}
	return t, true, "", args[1:], nil
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

// resolvePeers parses each --peer value into a peer target and validates it: a known, authed
// provider with an optional :model and NO account — a peer runs on its default account (only the
// lead rotates accounts). kind names the flag for the error message (always "--peer" now).
// Unknown or unauthed → a usage error naming the peer + the fix; never a silent skip.
func (a *app) resolvePeers(kind string, vals []string) ([]agents.Target, error) {
	var peers []agents.Target
	for _, v := range vals {
		t, err := agents.ParseTarget(v)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", kind, v, err)
		}
		if len(t.Accounts) > 0 {
			return nil, fmt.Errorf("%s %q: a peer runs on its default account — drop the @account (name it %s or %s:<model>)", kind, v, t.Provider, t.Provider)
		}
		if !slices.Contains(box.AuthedAgents(a.cfg), t.Provider) {
			return nil, fmt.Errorf("%s %q isn't signed in — run: coop login %s (see 'coop credentials')", kind, t.Provider, t.Provider)
		}
		peers = append(peers, t)
	}
	return peers, nil
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
	a.selectRunEffort(t.Provider, t.Effort)
	return nil
}
