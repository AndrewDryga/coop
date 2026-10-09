package box

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

type nativeSelection struct {
	record    *accountAuthority
	state     agents.NativeCredentialState
	canonical bool
}

func validateNativeOverrides(cfg *config.Config, spec RunSpec, ag agents.Agent) error {
	broker := ag.CredentialBroker()
	if broker.BaseURLEnv == "" {
		return nil
	}
	for _, args := range [][]string{cfg.ExtraRunArgs, spec.ExtraArgs} {
		if extraEnvAssigns(args, broker.BaseURLEnv) {
			return fmt.Errorf("native credential broker owns %s; remove the runtime override", broker.BaseURLEnv)
		}
	}
	for _, value := range []string{EnvFileValues(cfg.NativeAuthorityConfig().EnvFile())[broker.BaseURLEnv], spec.projectEnv[broker.BaseURLEnv]} {
		base := strings.TrimSuffix(strings.TrimSpace(value), "/")
		if base != "" && base != "https://"+broker.Upstream && base != "https://"+broker.Upstream+broker.ClientBasePath {
			return fmt.Errorf("native credential broker requires the original %s origin; remove custom %s", ag.DisplayName(), broker.BaseURLEnv)
		}
	}
	return nil
}

// Selection reads authority but never starts a migration or publishes an env key.
// Existing canonical failure always wins over stale legacy or environment input.
func previewNativeAuthority(ctx context.Context, cfg *config.Config, ag agents.Agent, account string) (nativeSelection, bool, error) {
	cfg = cfg.NativeAuthorityConfig()
	record, canonical, err := readNativeAccount(ctx, cfg, ag, account)
	selected := nativeSelection{record: record, canonical: canonical}
	if err != nil {
		return selected, canonical, err
	}
	if canonical && (record == nil || record.Revoked) {
		return selected, true, errors.New("native account is removed or needs host recovery")
	}
	if !canonical {
		if check := ag.NativeCredentials().LegacyCheck; check != nil {
			if err := check(cfg.AgentProfileDir(ag.Name(), account)); err != nil {
				return selected, false, err
			}
		}
		sources, err := legacyAccountSources(cfg, ag, account)
		if err != nil {
			return selected, false, err
		}
		serving := false
		for _, source := range sources {
			serving = serving || source.Retire
		}
		if serving {
			record, err = normalizeNativeAccount(ag, cutoverFiles(sources))
			if err != nil {
				return selected, true, err
			}
			selected.record = record
		}
	}
	if record != nil {
		selected.state, err = ag.NativeCredentials().Inspect(record.Artifacts, time.Now())
		if err != nil || !selected.state.Ready {
			return selected, true, errors.New("native account needs host sign-in or renewal")
		}
	}
	state, exists, err := nativeEnvironment(cfg, ag, account, selected.state)
	if err != nil {
		return selected, record != nil, err
	}
	if exists {
		selected.state = state
	}
	return selected, record != nil || exists, nil
}

func nativeEnvironment(cfg *config.Config, ag agents.Agent, account string, current agents.NativeCredentialState) (agents.NativeCredentialState, bool, error) {
	if account != cfg.DefaultProfileOf(ag.Name()) || current.Selection != "" && !current.APIKey {
		return current, false, nil // subscription markers suppress stale provider-wide keys
	}
	values := EnvFileValues(cfg.EnvFile())
	key, value := "", ""
	keys := ag.CredentialEnvKeys()
	if current.Selection == "" {
		home := cfg.AgentProfileDir(ag.Name(), account)
		keys = ag.ActiveCredentialEnvKeys(home, profileMarkerPresent(ag, home))
	}
	for _, name := range keys {
		if strings.TrimSpace(values[name]) == "" {
			continue
		}
		if key != "" {
			return current, false, errors.New("more than one provider environment credential is selected")
		}
		key, value = name, values[name]
	}
	if key == "" || ag.NativeCredentials().Environment == nil {
		return current, false, nil
	}
	broker := ag.CredentialBroker()
	if base := strings.TrimSpace(values[broker.BaseURLEnv]); base != "" {
		base = strings.TrimSuffix(base, "/")
		if base != "https://"+broker.Upstream && base != "https://"+broker.Upstream+broker.ClientBasePath {
			return current, false, errors.New("native credential broker requires the provider's original origin")
		}
	}
	state, err := ag.NativeCredentials().Environment(current.Selection, key, value)
	return state, err == nil, err
}
