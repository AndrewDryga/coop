package box

import (
	"context"
	"errors"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

// Quota inspection selects the same authority as a run but does not impose the filtered API-key
// broker's separate network/support restrictions. It never returns credential values to CLI.
func UsageQuotaAuthority(cfg *config.Config, provider, account string) (agents.UsageQuotaInput, error) {
	ag, ok := agents.Get(provider)
	if !ok {
		return agents.UsageQuotaInput{}, errors.New("unknown quota provider")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	selected, exists, err := previewNativeAuthority(ctx, cfg, ag, account)
	if err != nil {
		return agents.UsageQuotaInput{}, err
	}
	if exists && (selected.canonical || selected.record == nil) {
		input := agents.UsageQuotaInput{APIKey: selected.state.APIKey}
		if selected.canonical {
			input.Current = func(ctx context.Context, deadline time.Time) (agents.NativeCredentialState, map[string][]byte, error) {
				record, err := renewNativeAccount(ctx, cfg, ag, account, deadline, deadline)
				if err != nil {
					return agents.NativeCredentialState{}, nil, err
				}
				state, err := ag.NativeCredentials().Inspect(record.Artifacts, time.Now())
				return state, record.Artifacts, err
			}
		}
		return input, nil
	}
	home := cfg.AgentProfileDir(provider, account)
	input := agents.UsageQuotaInput{}
	marker := profileMarkerPresent(ag, home)
	values, err := effectiveCredentialEnv(cfg, RunSpec{Homes: true}, ag, provider, account, marker)
	if err != nil {
		return input, errors.New("selected authentication authority is unavailable")
	}
	for _, key := range ag.ActiveCredentialEnvKeys(home, marker) {
		if strings.TrimSpace(values[key]) == "" {
			continue
		}
		if input.EnvKey != "" {
			return input, errors.New("more than one authentication authority is selected")
		}
		input.EnvKey = key
		input.APIKey = true
	}
	return input, nil
}

// An import of retained history by an older build that never finished leaves the home unusable.
func checkRunHistorySettled(cfg *config.Config, spec RunSpec) error {
	if !spec.Homes || spec.Login {
		return nil
	}
	for _, name := range credentialScope(cfg, spec) {
		home, err := cfg.NativeHome(name)
		if err != nil {
			continue
		}
		if err := checkNativeHistorySettled(home); err != nil {
			return err
		}
	}
	return nil
}

// AccountQuota reads one account's provider allowance, as `coop usage` shows it, within ctx's
// deadline. An account that is not signed in is not read and answers a SignInError; so does one
// whose provider refused the sign-in during the read, which renews the account's token as a run
// would. The CLI and the session daemon share it, so both answer the same way.
func AccountQuota(ctx context.Context, cfg *config.Config, provider, account string) (agents.UsageQuota, error) {
	ag, ok := agents.Get(provider)
	if !ok {
		return agents.UsageQuota{}, errors.New("unknown quota provider")
	}
	login := provider + "@" + account
	if need := ProfileSignIn(cfg, provider, account, time.Now()); need != agents.SignedIn {
		return agents.UsageQuota{}, &agents.SignInError{Need: need, Target: login}
	}
	input, err := UsageQuotaAuthority(cfg, provider, account)
	if err != nil {
		return agents.UsageQuota{}, errors.New("this credential's sign-in could not be checked")
	}
	quota, err := ag.Usage().Quota(ctx, input)
	// The lookup's renewal may have just recorded that the provider refused this sign-in, so the
	// local answer decides again. A quota service that refuses the access means the same.
	if err != nil {
		if need := ProfileSignIn(cfg, provider, account, time.Now()); need != agents.SignedIn {
			err = &agents.SignInError{Need: need, Target: login, Err: err}
		} else if errors.Is(err, agents.ErrUsageSignIn) {
			err = &agents.SignInError{Need: agents.SignInAgain, Target: login, Err: err}
		}
	}
	return quota, err
}
