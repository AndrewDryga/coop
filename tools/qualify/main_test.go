package main

import (
	"encoding/json"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
)

func summaryLine(t *testing.T, strict bool, results []liveprovider.ProviderResult) string {
	t.Helper()
	data, err := json.Marshal(liveprovider.Summary{Schema: 1, Strict: strict, Results: results})
	if err != nil {
		t.Fatal(err)
	}
	// go test -v indents a t.Log line and prefixes its file:line.
	return "    provider_live_e2e_test.go:1: " + liveprovider.SummaryPrefix + string(data)
}

func passedEverywhere(t *testing.T, pinned map[string]string) []liveprovider.ProviderResult {
	t.Helper()
	var results []liveprovider.ProviderResult
	for _, name := range agents.Names() {
		results = append(results, liveprovider.ProviderResult{Provider: name, CLIVersion: pinned[name], Attempted: true, Passed: true, Status: liveprovider.StatusPassed})
	}
	return results
}

// A record is written only for a strict run in which every provider passed on the pinned CLI.
func TestSummaryResultsRequireEveryProviderOnThePinnedCLI(t *testing.T) {
	pinned, err := pinnedCLIVersions()
	if err != nil {
		t.Fatal(err)
	}
	if pinned["claude"] != "claude-cli 2.1.260" || len(pinned) != len(agents.Names()) {
		t.Fatalf("pinned CLI versions = %v", pinned)
	}
	good := passedEverywhere(t, pinned)
	results, err := summaryResults([]string{"=== RUN x", summaryLine(t, true, good)}, liveprovider.SummaryPrefix, pinned)
	if err != nil || results["grok"] != pinned["grok"] {
		t.Fatalf("a strict, all-passed run = %v, %v", results, err)
	}
	drifted := passedEverywhere(t, pinned)
	drifted[0].CLIVersion = drifted[0].Provider + "-cli 9.9.9"
	failed := passedEverywhere(t, pinned)
	failed[1].Passed, failed[1].Status = false, liveprovider.StatusFailed
	for _, test := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"no summary", []string{"--- FAIL: TestProviderLiveCompatibility"}, "no summary line"},
		{"not strict", []string{summaryLine(t, false, good)}, "not strict"},
		{"another CLI version", []string{summaryLine(t, true, drifted)}, "not the pinned"},
		{"a provider failed", []string{summaryLine(t, true, failed)}, "did not pass"},
		{"a provider missing", []string{summaryLine(t, true, good[1:])}, "no passing result for " + good[0].Provider},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := summaryResults(test.lines, liveprovider.SummaryPrefix, pinned); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
		})
	}
}

// A suite without a summary counts its per-provider subtests, and any failure refuses the record.
func TestTestResultsRequireEveryProviderSubtest(t *testing.T) {
	var lines []string
	for _, name := range agents.Names() {
		lines = append(lines, "    --- PASS: TestX/"+name+" (1.00s)")
	}
	if results, err := testResults(append(lines, "--- PASS: TestX (4.00s)"), "TestX"); err != nil || len(results) != len(agents.Names()) {
		t.Fatalf("all subtests passed = %v, %v", results, err)
	}
	if _, err := testResults(lines[1:], "TestX"); err == nil {
		t.Fatal("a missing provider was qualified")
	}
	if _, err := testResults(append(lines, "--- FAIL: TestOther (0.00s)"), "TestX"); err == nil {
		t.Fatal("a failing run was qualified")
	}
}

// The model+effort run is strict: every provider, registry order, each target valid for its adapter.
func TestEffortTargetsAreACompleteStrictList(t *testing.T) {
	list, err := effortTargets()
	if err != nil {
		t.Fatal(err)
	}
	targets, strict, err := liveprovider.ParseTargets(list)
	if err != nil || strict {
		t.Fatalf("ParseTargets(%q) = strict %v, %v", list, strict, err)
	}
	if err := liveprovider.ValidateStrictTargets(true, targets); err != nil {
		t.Fatalf("%q is not a complete registry-ordered list: %v", list, err)
	}
	for _, target := range targets {
		if target.Model == "" || target.Effort != qualifyEffort {
			t.Errorf("%s target %+v carries no model or effort", target.Provider, target)
		}
	}
}
