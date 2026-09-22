package liveprovider

import (
	"encoding/json"
	"errors"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

const AccountsSummaryPrefix = "COOP_PROVIDER_ACCOUNTS_LIVE_SUMMARY "
const StatusNotConfigured = agents.QualificationNotConfigured

// AccountsSummary records availability without naming accounts. Every configured pair must pass;
// only a provider with fewer than two stored accounts can be not_configured, never a bad pair.
type AccountsSummary struct {
	Schema  int              `json:"schema"`
	Results []ProviderResult `json:"results"`
}

func NewAccountsSummary(results []ProviderResult) (AccountsSummary, error) {
	providers := agents.Names()
	if len(results) != len(providers) {
		return AccountsSummary{}, errors.New("account qualification requires every registered provider")
	}
	for i, result := range results {
		if result.Provider != providers[i] {
			return AccountsSummary{}, errors.New("account qualification provider order mismatch")
		}
		if result.Status == StatusNotConfigured {
			if result != (ProviderResult{Provider: result.Provider, Status: StatusNotConfigured}) {
				return AccountsSummary{}, errors.New("unconfigured account row carries execution evidence")
			}
			continue
		}
		if result.Status != StatusPassed && result.Status != StatusFailed {
			return AccountsSummary{}, errors.New("configured account qualification cannot be skipped")
		}
		if _, err := NewSummary(false, []agents.Target{{Provider: result.Provider}}, []ProviderResult{result}); err != nil {
			return AccountsSummary{}, err
		}
	}
	return AccountsSummary{Schema: 1, Results: append([]ProviderResult(nil), results...)}, nil
}

func (s AccountsSummary) Success() bool {
	for _, result := range s.Results {
		if result.Status != StatusPassed && result.Status != StatusNotConfigured {
			return false
		}
	}
	return true
}

func (s AccountsSummary) Line() (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return AccountsSummaryPrefix + string(data), nil
}
