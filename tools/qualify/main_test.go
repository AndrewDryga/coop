package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
)

func TestSavedProviderReportIsOptionalAndNeverLoadsHostConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	// A broken host config would fail live preflight. Saved-report commands must not read it.
	t.Setenv("COOP_CONFIG_DIR", "/dev/null/not-a-directory")
	for _, args := range [][]string{{"-status"}, {"-verify"}} {
		var out bytes.Buffer
		err := command(args, &out)
		if args[0] == "-status" {
			if err != nil || !strings.Contains(out.String(), "UNVERIFIED") || !strings.Contains(out.String(), "No provider calls made") {
				t.Fatalf("missing report must be informational: %v, %s", err, &out)
			}
		} else if err == nil || !os.IsNotExist(err) {
			t.Fatalf("explicit verification must refuse missing evidence, not load host config: %v", err)
		}
	}
}

func TestSavedProviderReportVerification(t *testing.T) {
	lock, clients, err := agents.QualifiedClientSet()
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := agents.PinnedCLIVersions()
	if err != nil {
		t.Fatal(err)
	}
	valid := agents.Qualification{Schema: 2, QualifiedOn: "2026-10-14", Platform: "linux/arm64", Lock: lock, Clients: clients, Suites: map[string]map[string]string{}}
	for _, suite := range agents.QualificationRequirements() {
		row := map[string]string{}
		for _, provider := range suite.Providers {
			row[provider] = pinned[provider]
			if suite.Evidence == agents.QualificationPassedTest {
				row[provider] = "passed"
			}
		}
		valid.Suites[suite.Name] = row
	}
	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	valid.Suites["provider-accounts-live-e2e-all"]["grok"] = agents.QualificationNotConfigured
	partial, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"valid", data, ""},
		{"unconfigured second account", partial, ""},
		{"malformed", []byte("{"), "unexpected EOF"},
		{"unknown field", bytes.Replace(data, []byte(`"schema":2`), []byte(`"schema":2,"made_up":true`), 1), "unknown field"},
		{"trailing value", append(append([]byte(nil), data...), []byte("{}")...), "trailing data"},
		{"stale lock", bytes.Replace(data, []byte(lock), []byte(strings.Repeat("0", 64)), 1), "locked dependency set changed"},
		{"false test result", bytes.Replace(data, []byte(`"passed"`), []byte(`"failed"`), 1), "failed"},
		{"missing scopes", []byte(`{"schema":2,"qualified_on":"2026-10-14","platform":"linux/arm64"}`), "locked dependency set changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if err := os.MkdirAll(filepath.Dir(record), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(record, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err := command([]string{"-verify"}, &out)
			if tc.want == "" {
				if err != nil || !strings.Contains(out.String(), "2026-10-14 on linux/arm64") || !strings.Contains(out.String(), "not a new live test") {
					t.Fatalf("valid dated report: %v, %s", err, &out)
				}
				for _, mode := range []string{"-status", "-verify"} {
					out.Reset()
					if err := command([]string{mode}, &out); err != nil {
						t.Fatal(err)
					}
					wantGap := tc.name == "unconfigured second account"
					if strings.Contains(out.String(), "grok: account recovery not verified") != wantGap || strings.Contains(out.String(), "wrote ") {
						t.Fatalf("%s hid recovery coverage or claimed a new recording: %s", mode, &out)
					}
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("expected refusal containing %q, got %v", tc.want, err)
				}
				out.Reset()
				if err := command([]string{"-status"}, &out); err != nil || !strings.Contains(out.String(), "UNVERIFIED") {
					t.Fatalf("invalid report must remain informational in status mode: %v, %s", err, &out)
				}
			}
		})
	}
}

func TestSavedProviderReportModeCannotFallThroughToLivePreflight(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{
		{"-status", "-preflight"}, {"-verify", "-logs", "logs"}, {"-status", "-targets"},
		{"-status", "-verify"}, {"-status", "extra"}, {},
	} {
		var out bytes.Buffer
		if err := command(args, &out); err == nil || !strings.Contains(err.Error(), "choose exactly one") || out.Len() != 0 {
			t.Fatalf("mixed/empty mode %v was not refused before execution: %v, %s", args, err, &out)
		}
	}
	var help bytes.Buffer
	if err := command([]string{"-help"}, &help); err != nil || !strings.Contains(help.String(), "-status") || !strings.Contains(help.String(), "-verify") {
		t.Fatalf("help should explain free report modes and succeed: %v, %s", err, &help)
	}
}

func TestQualificationReportNamesAccountCoverageGaps(t *testing.T) {
	for _, missing := range [][]string{nil, {"claude", "grok"}} {
		name := "all accounts verified"
		if len(missing) != 0 {
			name = "missing second accounts"
		}
		t.Run(name, func(t *testing.T) {
			accounts := map[string]string{}
			for _, provider := range agents.Names() {
				accounts[provider] = "pinned-cli-version"
			}
			for _, provider := range missing {
				accounts[provider] = agents.QualificationNotConfigured
			}
			q := agents.Qualification{Platform: "linux/arm64", Suites: map[string]map[string]string{"provider-accounts-live-e2e-all": accounts}}
			var out bytes.Buffer
			printQualification(&out, q)
			text := out.String()
			if !strings.Contains(text, "recorded locked-client qualification evidence for linux/arm64") || !strings.Contains(text, record) {
				t.Fatalf("missing qualification receipt: %s", text)
			}
			if strings.Contains(text, "qualified the locked clients on every provider") || strings.Count(text, "account recovery not verified") != len(missing) {
				t.Fatalf("misleading recovery coverage: %s", text)
			}
			for _, provider := range missing {
				if !strings.Contains(text, provider+": account recovery not verified — sign in a second compatible account and rerun qualification") {
					t.Fatalf("missing recovery action for %s: %s", provider, text)
				}
			}
		})
	}
}

func TestQualificationPreflightAcceptsEnvOnlyDefaults(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	var lines []string
	for _, provider := range agents.Names() {
		ag, _ := agents.Get(provider)
		keys := ag.CredentialEnvKeys()
		if len(keys) == 0 {
			t.Fatalf("missing env fixture for %s", provider)
		}
		lines = append(lines, keys[0]+"=SYNTHETIC_QUALIFICATION_KEY")
	}
	if err := os.WriteFile(cfg.EnvFile(), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preflightConfig(cfg); err != nil {
		t.Fatalf("supported env-only defaults refused: %v", err)
	}
	cfg.ImageOverride = "foreign-image"
	if preflightConfig(cfg) == nil {
		t.Fatal("foreign image qualified")
	}
	cfg.ImageOverride = ""
	if err := os.WriteFile(cfg.EnvFile(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if preflightConfig(cfg) == nil {
		t.Fatal("missing credentials accepted")
	}
}

func TestProviderQualifyKeepsLogsInOneRecipeShell(t *testing.T) {
	data, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	start, end := -1, -1
	for i, line := range lines {
		if strings.Contains(line, `@logs="$$(mktemp -d)"`) {
			start = i
		}
		if strings.Contains(line, `go run ./tools/qualify -logs "$$logs"`) {
			end = i
		}
	}
	if start < 0 || end <= start {
		t.Fatal("provider-qualify log directory and recorder recipe not found")
	}
	// Make starts a new shell at every recipe boundary, discarding the logs variable.
	for i := start; i <= end; i++ {
		if !strings.HasPrefix(lines[i], "\t") || (i < end && !strings.HasSuffix(strings.TrimSpace(lines[i]), `\`)) {
			t.Fatalf("provider-qualify recipe splits before the recorder at Makefile:%d", i+1)
		}
	}
}

func summaryLine(t *testing.T, strict bool, results []liveprovider.ProviderResult) string {
	t.Helper()
	summary := liveprovider.Summary{Schema: 1, Strict: strict, Results: results}
	summary.Totals.Requested = len(results)
	for _, result := range results {
		if result.Attempted {
			summary.Totals.Attempted++
		}
		switch result.Status {
		case liveprovider.StatusPassed:
			summary.Totals.Passed++
		case liveprovider.StatusSkipped:
			summary.Totals.Skipped++
		case liveprovider.StatusFailed:
			summary.Totals.Failed++
		}
	}
	data, err := json.Marshal(summary)
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
	pinned, err := agents.PinnedCLIVersions()
	if err != nil {
		t.Fatal(err)
	}
	if pinned["claude"] != "claude-cli 2.1.285" || len(pinned) != len(agents.Names()) {
		t.Fatalf("pinned CLI versions = %v", pinned)
	}
	good := passedEverywhere(t, pinned)
	results, err := summaryResults([]string{"=== RUN x", summaryLine(t, true, good)}, liveprovider.SummaryPrefix, pinned, nil)
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
		{"no summary", []string{"=== RUN TestProviderLiveCompatibility"}, "no summary line"},
		{"failed test", []string{summaryLine(t, true, good), "--- FAIL: TestProviderLiveCompatibility"}, "test failed"},
		{"not strict", []string{summaryLine(t, false, good)}, "not strict"},
		{"another CLI version", []string{summaryLine(t, true, drifted)}, "not the pinned"},
		{"a provider failed", []string{summaryLine(t, true, failed)}, "did not pass"},
		{"a provider missing", []string{summaryLine(t, true, good[1:])}, "no passing result for " + good[0].Provider},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := summaryResults(test.lines, liveprovider.SummaryPrefix, pinned, nil); err == nil || !strings.Contains(err.Error(), test.want) {
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
	if results, err := testResults(append(lines, "--- PASS: TestX (4.00s)"), "TestX", nil); err != nil || len(results) != len(agents.Names()) {
		t.Fatalf("all subtests passed = %v, %v", results, err)
	}
	if _, err := testResults(lines[1:], "TestX", nil); err == nil {
		t.Fatal("a missing provider was qualified")
	}
	if _, err := testResults(append(lines, "--- FAIL: TestOther (0.00s)"), "TestX", nil); err == nil {
		t.Fatal("a failing run was qualified")
	}
}

// A scoped suite records exactly the providers it claims. The scope is the record's own statement
// about what went unproven, so it has to be as strict in both directions as the default: a provider
// it names must pass, one it does not name must not appear, and a name no adapter answers to is a
// typo that would otherwise silently shrink what qualification means.
func TestScopedSuitesRecordExactlyTheProvidersTheyClaim(t *testing.T) {
	scope := []string{"codex", "grok"}
	var lines []string
	for _, name := range scope {
		lines = append(lines, "    --- PASS: TestX/"+name+" (1.00s)")
	}
	results, err := testResults(lines, "TestX", scope)
	if err != nil || len(results) != len(scope) {
		t.Fatalf("a scoped suite with every provider in scope = %v, %v", results, err)
	}
	if _, err := testResults(lines[1:], "TestX", scope); err == nil {
		t.Fatal("a provider the suite claims to cover was allowed to be missing")
	}
	extra := append(lines, "    --- PASS: TestX/claude (1.00s)")
	if _, err := testResults(extra, "TestX", scope); err == nil {
		t.Fatal("a provider outside the declared scope was recorded anyway")
	}
	// The typo has to be caught even when the log OBLIGES it: a suite whose subtest is named after a
	// provider no adapter answers to otherwise satisfies every count and records a provider that does
	// not exist.
	typo := []string{"    --- PASS: TestX/codex (1.00s)", "    --- PASS: TestX/not-a-provider (1.00s)"}
	if _, err := testResults(typo, "TestX", []string{"codex", "not-a-provider"}); err == nil {
		t.Fatal("a scope naming an unregistered provider was accepted")
	}
	// The default is unchanged: no scope still means every registered provider.
	if _, err := testResults(lines, "TestX", nil); err == nil {
		t.Fatal("an unscoped suite qualified without every provider")
	}
}

// Every suite's declared scope has to name real providers, or the record would quietly claim less
// than it says while the recorder stays green.
func TestEveryRequiredSuiteHasOneMatchingReader(t *testing.T) {
	required := agents.QualificationRequirements()
	if len(suites) != len(required) {
		t.Fatal("suite readers differ from required coverage")
	}
	for _, suite := range required {
		reader, ok := suites[suite.Name]
		if !ok || (reader.summary == "") == (reader.test == "") {
			t.Fatalf("missing or ambiguous reader for %s", suite.Name)
		}
		if (suite.Evidence == agents.QualificationPassedTest) != (reader.test != "") ||
			(suite.Evidence == agents.QualificationAccounts) != (reader.summary == liveprovider.AccountsSummaryPrefix) {
			t.Fatalf("reader evidence differs from required %s", suite.Name)
		}
		for _, name := range suite.Providers {
			if _, ok := agents.Get(name); !ok {
				t.Errorf("suite %s is scoped to %q, which no adapter answers to", suite.Name, name)
			}
		}
	}
}

func TestDelegateSummaryRecordsTheCompleteRing(t *testing.T) {
	pinned, err := agents.PinnedCLIVersions()
	if err != nil {
		t.Fatal(err)
	}
	targets, _, err := liveprovider.ParseTargets("all")
	if err != nil {
		t.Fatal(err)
	}
	summary, err := liveprovider.NewConsultSummary(true, targets, passedEverywhere(t, pinned))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	line := liveprovider.DelegateSummaryPrefix + string(body)
	if results, err := summaryResults([]string{line}, liveprovider.DelegateSummaryPrefix, pinned, nil); err != nil || len(results) != len(targets) {
		t.Fatalf("complete delegate ring: %v, %v", results, err)
	}
	for name, lines := range map[string][]string{
		"duplicate summary":  {line, line},
		"unknown field":      {strings.Replace(line, `"schema":1`, `"schema":1,"account":"PRIVATE_CANARY"`, 1)},
		"old schema":         {strings.Replace(line, `"schema":1`, `"schema":0`, 1)},
		"non-strict":         {strings.Replace(line, `"strict":true`, `"strict":false`, 1)},
		"bad edge":           {strings.Replace(line, `"lead":"grok"`, `"lead":"claude"`, 1)},
		"unattempted":        {strings.Replace(line, `"attempted":true`, `"attempted":false`, 1)},
		"duplicate provider": {strings.Replace(line, `"provider":"codex"`, `"provider":"claude"`, 1)},
		"failed footer":      {line, "FAIL\tgithub.com/AndrewDryga/coop/internal/cli"},
		"bad JSON":           {line + "{}"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := summaryResults(lines, liveprovider.DelegateSummaryPrefix, pinned, nil); err == nil {
				t.Fatal("invalid delegate qualification accepted")
			}
		})
	}
}

func TestAccountSummaryRecordsOnlyGenuineAbsenceOrPassedPinnedCLI(t *testing.T) {
	pinned, err := agents.PinnedCLIVersions()
	if err != nil {
		t.Fatal(err)
	}
	results := passedEverywhere(t, pinned)
	results[len(results)-1] = liveprovider.ProviderResult{Provider: "grok", Status: liveprovider.StatusNotConfigured}
	summary, err := liveprovider.NewAccountsSummary(results)
	if err != nil {
		t.Fatal(err)
	}
	line, err := summary.Line()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := accountResults([]string{line}, pinned); err != nil || got["grok"] != agents.QualificationNotConfigured || got["codex"] != pinned["codex"] {
		t.Fatalf("account availability evidence: %v, %v", got, err)
	}
	for name, lines := range map[string][]string{
		"duplicate":             {line, line},
		"old schema":            {strings.Replace(line, `"schema":1`, `"schema":0`, 1)},
		"account disclosure":    {strings.Replace(line, `"provider":"claude"`, `"provider":"claude","account":"PRIVATE_CANARY"`, 1)},
		"bad provider":          {strings.Replace(line, `"provider":"grok"`, `"provider":"unknown"`, 1)},
		"skipped":               {strings.Replace(line, `"status":"not_configured"`, `"status":"skipped"`, 1)},
		"absence with evidence": {strings.Replace(line, `"status":"not_configured"`, `"status":"not_configured","cli_version":"grok-cli 1.0.0"`, 1)},
		"unattempted":           {strings.Replace(line, `"attempted":true`, `"attempted":false`, 1)},
		"wrong CLI":             {strings.Replace(line, pinned["codex"], "codex-cli 0.0.0", 1)},
		"failed test":           {line, "--- FAIL: TestProviderAccountsLiveCompatibility"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := accountResults(lines, pinned); err == nil {
				t.Fatal("invalid account qualification accepted")
			}
		})
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
