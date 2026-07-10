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
