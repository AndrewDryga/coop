package liveprovider

import (
	agents "github.com/AndrewDryga/coop/internal/agent"
	"testing"
)

func TestAccountsSummaryRequiresCompleteOrderedHonestResults(t *testing.T) {
	fresh := func() []ProviderResult {
		var rows []ProviderResult
		for _, name := range agents.Names() {
			rows = append(rows, ProviderResult{Provider: name, Status: StatusNotConfigured})
		}
		return rows
	}
	rows := fresh()
	summary, err := NewAccountsSummary(rows)
	if err != nil || !summary.Success() {
		t.Fatalf("unconfigured summary: %+v %v", summary, err)
	}
	for _, mutate := range []func([]ProviderResult) []ProviderResult{
		func(r []ProviderResult) []ProviderResult { return r[:len(r)-1] },
		func(r []ProviderResult) []ProviderResult { r[0], r[1] = r[1], r[0]; return r },
		func(r []ProviderResult) []ProviderResult { r[0].CLIVersion = "fixture-cli 1.2.3"; return r },
		func(r []ProviderResult) []ProviderResult { r[0].Attempted = true; return r },
		func(r []ProviderResult) []ProviderResult {
			r[0].Status = StatusSkipped
			r[0].ReasonCode = ReasonMissingCredential
			return r
		},
	} {
		if _, err := NewAccountsSummary(mutate(fresh())); err == nil {
			t.Fatal("invalid account summary accepted")
		}
	}
	rows = fresh()
	name := rows[0].Provider
	rows[0] = ProviderResult{Provider: name, CLIVersion: name + "-cli 1.2.3", Attempted: true, Passed: true, Status: StatusPassed}
	summary, err = NewAccountsSummary(rows)
	if err != nil || !summary.Success() {
		t.Fatalf("passed summary: %+v %v", summary, err)
	}
	rows[0] = ProviderResult{Provider: name, Attempted: true, Status: StatusFailed, ReasonCode: ReasonPromptExit, Phase: "prompt", ErrorClass: "process"}
	failed, err := NewAccountsSummary(rows)
	if err != nil || failed.Success() || !summary.Success() {
		t.Fatalf("failed/copied summary: %+v %v", failed, err)
	}
}
