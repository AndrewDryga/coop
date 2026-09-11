package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// targetUsage renders a rejected target as coop's one refusal block. The parser already knows the
// headline, the constraint and the page that explains the rule (internal/agent returns them as
// data, never as terminal output); the command adds only what it alone knows — its own name, for
// an unknown agent's correction, and its usage line for a target that is missing entirely. A
// non-target error passes through untouched.
func targetUsage(err error, command, usage string) error {
	var te *agents.TargetError
	if !errors.As(err, &te) {
		return err
	}
	switch te.Kind {
	case agents.TargetEmpty:
		return ui.MissingArgument("agent", command, usage)
	case agents.TargetUnknownProvider:
		suggestion := ""
		if guess, ok := nearestCommand(te.Provider, agents.Names()); ok {
			suggestion = command + " " + strings.Replace(te.Target, te.Provider, guess, 1)
		}
		return ui.UnknownValue("agent", te.Target, command, te.Cause, suggestion)
	}
	return &ui.UsageError{Headline: te.Headline, Cause: te.Cause, Rows: [][2]string{{"Help:", "coop help " + te.Help}}}
}

// noAccountErr refuses a launch that named an account this agent has no login for: the two
// commands that resolve it — the agent's accounts, and signing this one in.
func noAccountErr(agent, account string) error {
	return &ui.UsageError{
		Headline: fmt.Sprintf("No %s account named %q", titleName(agent), agents.DisplayTarget(account)),
		Rows: [][2]string{
			{"Accounts:", "coop credentials " + agent},
			{"Sign in:", agents.LoginCommand(agent + "@" + account)},
		},
	}
}

// needsAccountErr refuses a launch whose agent has no usable login at all — only where the run
// actually requires one (a peer coop is about to mount, not a heads-up before an interactive run,
// which stays the warning nudgeIfUnauthed prints).
func needsAccountErr(agent string) error {
	return &ui.UsageError{
		Headline: titleName(agent) + " needs a usable account",
		Rows: [][2]string{
			{"Sign in:", "coop login " + agent},
			{"Help:", "coop help credentials"},
		},
	}
}

// noProviderErr is coop's one actionable message when a launch names no target (and no
// preset supplies a lead) — the implicit claude default is gone, so every agent-launching
// surface routes an empty selection here. cmd is the command word for the usage line
// ("loop", "acp", "fork", "" for a bare `coop`).
func noProviderErr(cmd string) error {
	usage := "coop " + cmd
	if cmd == "" {
		usage = "coop"
	}
	return fmt.Errorf("name the target or preset — %s <target|preset>; sign in with 'coop login <agent>' or see 'coop credentials'",
		strings.TrimSpace(usage))
}

// isTargetHead reports whether s is written as a target: a registered provider, or anything in
// target syntax (a model, effort, or account segment), so `coop <s>` names an agent run, not a
// command/preset — and a typo'd provider in `nope:model` fails as an unknown provider instead of
// being looked up as a preset. Used by the top-level dispatch and takeHeadWho.
func isTargetHead(s string) bool {
	head := strings.TrimSpace(s)
	if strings.ContainsAny(head, ":/@") {
		return true
	}
	return agents.Valid(head)
}

// takeHeadWho pulls the leading "who runs" positional off args — the unified grammar shared by
// loop/fork/acp: a TARGET (provider[:model][/effort][@account], detected by isTargetHead)
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
// non-loop fork): "" for none, the account for one, a refusal for a ladder (@a,b only rotates
// under `coop loop`). command is the run being refused, so the block names what the user typed.
func singleAccount(t agents.Target, command string) (string, error) {
	switch len(t.Accounts) {
	case 0:
		return "", nil
	case 1:
		return t.Accounts[0], nil
	default:
		return "", &ui.UsageError{
			Headline: fmt.Sprintf("%q takes one account", command),
			Cause:    fmt.Sprintf("Account lists such as %q are supported by coop loop.", "@"+strings.Join(t.Accounts, ",")),
			Rows:     [][2]string{{"Help:", "coop help loop"}},
		}
	}
}

// foldTarget folds a positional target's model + single account into a run's one-off
// model/profile strings — acp and fork thread these through their inner box rather than
// through selectRun*. Only a non-empty segment overwrites, so a bare `provider` leaves an
// env-supplied model/account (a preset-rotation rung) intact. A >1-account ladder errors
// (loop-only). The caller takes the provider from t.Provider.
func foldTarget(t agents.Target, command string, model, profile *string) error {
	if t.Model != "" {
		*model = t.Model
	}
	acct, err := singleAccount(t, command)
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
// lead rotates accounts). command is the run the peers belong to ("coop claude"), so a refusal
// points at that run's own page. Unknown or unauthed → a usage refusal naming the peer + the fix;
// never a silent skip.
func (a *app) resolvePeers(command string, vals []string) ([]agents.Target, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	// One credential scan for the whole slice: AuthedAgents walks every provider's profile and
	// reads the env file, and its answer cannot change between two peers of one launch. Passing
	// the result in is what keeps it out of the loop.
	return resolvePeerTargets(command, vals, box.AuthedAgents(a.cfg))
}

// resolvePeerTargets validates each --peer value against one already-taken list of signed-in
// providers. A refusal quotes the peer AS TYPED, model and all: "codex" when the user wrote
// "codex:gpt-5.6-sol" sends them looking for a peer they did not name.
func resolvePeerTargets(command string, vals, authed []string) ([]agents.Target, error) {
	var peers []agents.Target
	for _, v := range vals {
		t, err := agents.ParseTarget(v)
		if err != nil {
			return nil, targetUsage(err, command, command+" --peer <agent>[:<model>]")
		}
		if len(t.Accounts) > 0 {
			return nil, &ui.UsageError{
				Headline: "A peer uses its default account",
				Cause: fmt.Sprintf("Remove %q from %q.", "@"+strings.Join(t.Accounts, ","),
					"--peer "+agents.DisplayTarget(v)),
				Rows: [][2]string{{"Help:", ui.HelpCommand(command)}},
			}
		}
		if !slices.Contains(authed, t.Provider) {
			return nil, needsAccountErr(t.Provider)
		}
		peers = append(peers, t)
	}
	return peers, nil
}

// applyTarget seeds a single run's model + account selection from a target (the loop uses the
// rotation ladder instead). A >1-account ladder is rejected (loop-only).
func (a *app) applyRunTarget(t agents.Target, command string) error {
	acct, err := singleAccount(t, command)
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
