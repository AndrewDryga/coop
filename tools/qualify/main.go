// Command qualify records a qualification of the locked client set. `make provider-qualify` runs
// every strict live suite with verbose output into one directory, then this reads each suite's log
// and writes internal/agent/locked-clients/qualification.json — only when every suite satisfies its
// required provider scope and every reported CLI version is the one the manifest pins. Account
// recovery records not_configured, rather than passed, when a second account is unavailable.
// A suite's evidence is its machine-readable summary (the CLI version each provider reported) or,
// for a suite without one, its per-provider `--- PASS` lines. `-targets` prints the explicit
// model+effort targets the second live run uses. stdlib + internal packages only.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
)

// record is where the qualification is committed beside the lock it qualifies.
const record = "internal/agent/locked-clients/qualification.json"

// suites owns log parsing only. Required names, provider scopes and evidence kinds have one home
// in agents.QualificationRequirements, shared with the explicit saved-report verifier.
var suites = map[string]struct {
	summary, test string
}{
	"provider-live-e2e-all":          {summary: liveprovider.SummaryPrefix},
	"provider-live-e2e-effort":       {summary: liveprovider.SummaryPrefix},
	"provider-resume-live-e2e-all":   {summary: liveprovider.ResumeSummaryPrefix},
	"provider-loop-live-e2e-all":     {summary: liveprovider.LoopSummaryPrefix},
	"provider-consult-live-e2e-all":  {summary: liveprovider.ConsultSummaryPrefix},
	"provider-delegate-live-e2e-all": {summary: liveprovider.DelegateSummaryPrefix},
	"provider-network-live-e2e-all":  {summary: liveprovider.NetworkSummaryPrefix},
	"provider-accounts-live-e2e-all": {summary: liveprovider.AccountsSummaryPrefix},
	"acp-e2e":                        {test: "TestLiveProviderConformance"},
	"native-roles-e2e":               {test: "TestRuntimeNativeRolesAreCheckedByPinnedClients"},
	"skills-e2e":                     {test: "TestRuntimeSharedSkillsAreCheckedByPinnedClients"},
	// Claude is out of scope here, and the omission is the record's: Coop hands claude its MCP
	// servers with --mcp-config on the main invocation, which no offline command exercises, so its
	// connection is not proven by this suite. See internal/box/mcp_runtime_e2e_test.go.
	"mcp-e2e": {test: "TestRuntimeSharedMCPServersAreReachedByEveryProbeableClient"},
}

// qualifyEffort is the level the model+effort run asks every provider for.
const qualifyEffort = "high"

func main() {
	if err := command(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "qualify: %v\n", err)
		os.Exit(1)
	}
}

func command(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("qualify", flag.ContinueOnError)
	flags.SetOutput(out)
	logs := flags.String("logs", "", "directory holding <suite>.log for every suite provider-qualify ran")
	targets := flags.Bool("targets", false, "print the model+effort live targets, in registry order, and exit")
	preflight := flags.Bool("preflight", false, "refuse, before any paid suite, a run that cannot qualify Coop's own clients")
	status := flags.Bool("status", false, "show saved live-provider evidence without calling providers")
	verify := flags.Bool("verify", false, "strictly verify saved evidence against today's clients; no provider calls")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	modes := 0
	for _, selected := range []bool{*logs != "", *targets, *preflight, *status, *verify} {
		if selected {
			modes++
		}
	}
	if modes != 1 || flags.NArg() != 0 {
		return errors.New("choose exactly one of -status, -verify, -logs, -targets or -preflight")
	}
	if *status || *verify {
		return savedReport(out, *verify)
	}
	if *preflight {
		return preflightCheck()
	}
	if *targets {
		list, err := effortTargets()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, list)
		return err
	}
	return run(*logs)
}

// Saved reports are dated evidence, not release permission. This path must not load host
// credentials, detect a runtime or start a provider; even strict verification only reads a file.
func savedReport(out io.Writer, strict bool) error {
	q, err := readSavedReport()
	if err != nil {
		if strict {
			return err
		}
		_, writeErr := fmt.Fprintf(out, "Live-provider evidence: UNVERIFIED (%q)\nNo provider calls made; builds and releases do not require this report.\n", err.Error())
		return writeErr
	}
	if _, err = fmt.Fprintf(out, "Saved live-provider evidence: %s on %s\nClient identities and required evidence match; this is a dated report, not a new live test.\n", q.QualifiedOn, q.Platform); err != nil {
		return err
	}
	return printAccountCoverage(out, q)
}

func readSavedReport() (agents.Qualification, error) {
	var q agents.Qualification
	data, err := os.ReadFile(record)
	if err != nil {
		return q, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&q); err != nil {
		return q, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return q, errors.New("saved report has trailing data")
	}
	lock, clients, err := agents.QualifiedClientSet()
	if err != nil {
		return q, err
	}
	return q, agents.ValidateQualification(q, lock, clients)
}

func run(logs string) error {
	if logs == "" {
		return errors.New("-logs is required")
	}
	if err := preflightCheck(); err != nil {
		return err
	}
	pinned, err := agents.PinnedCLIVersions()
	if err != nil {
		return err
	}
	lock, clients, err := agents.QualifiedClientSet()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	rt, err := runtime.Detect(cfg.RuntimeName)
	if err != nil {
		return err
	}
	platform, _, err := rt.BuildPlatform()
	if err != nil {
		return err
	}
	q := agents.Qualification{Schema: agents.QualificationSchema, QualifiedOn: time.Now().UTC().Format(time.DateOnly), Platform: platform, Lock: lock,
		Clients: clients, Suites: make(map[string]map[string]string)}
	for _, required := range agents.QualificationRequirements() {
		suite, ok := suites[required.Name]
		if !ok {
			return fmt.Errorf("missing log reader for %s", required.Name)
		}
		lines, err := readLines(filepath.Join(logs, required.Name+".log"))
		if err != nil {
			return err
		}
		var results map[string]string
		if required.Evidence == agents.QualificationAccounts {
			results, err = accountResults(lines, pinned)
		} else if suite.summary != "" {
			results, err = summaryResults(lines, suite.summary, pinned, required.Providers)
		} else {
			results, err = testResults(lines, suite.test, required.Providers)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", required.Name, err)
		}
		q.Suites[required.Name] = results
	}
	if err := agents.ValidateQualification(q, lock, clients); err != nil {
		return err
	}
	data, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomicMode(record, append(data, '\n'), 0o644); err != nil {
		return err
	}
	printQualification(os.Stdout, q)
	return nil
}

func printQualification(w io.Writer, q agents.Qualification) {
	fmt.Fprintf(w, "recorded locked-client qualification evidence for %s — wrote %s\n", q.Platform, record)
	_ = printAccountCoverage(w, q)
}

func printAccountCoverage(w io.Writer, q agents.Qualification) error {
	for _, provider := range agents.Names() {
		if q.Suites["provider-accounts-live-e2e-all"][provider] == agents.QualificationNotConfigured {
			if _, err := fmt.Fprintf(w, "  %s: account recovery not verified — sign in a second compatible account and rerun qualification\n", provider); err != nil {
				return err
			}
		}
	}
	return nil
}

// preflightCheck refuses what would otherwise fail only after paid suites ran: an image override (the
// suites would qualify that image, not Coop's box) and a provider with no configured default
// credential. Use the normal presence policy, including env-only accounts; the live harness still
// owns safe projection and portability checks before paid work.
func preflightCheck() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return preflightConfig(cfg)
}

func preflightConfig(cfg *config.Config) error {
	if cfg.ImageOverride != "" {
		return errors.New("COOP_IMAGE is set (in the environment or coop.conf) — provider-qualify qualifies Coop's own box; unset it")
	}
	for _, name := range agents.Names() {
		profile := cfg.DefaultProfileOf(name)
		if !box.ProfileAuthed(cfg, name, profile) {
			return fmt.Errorf("%s has no credential for its default account (%s) — run `coop login %s` or configure its API key first", name, profile, name)
		}
	}
	return nil
}

// effortTargets names every provider's example model at qualifyEffort, in registry order — the
// explicit strict list the model+effort run passes as COOP_LIVE_TARGETS.
func effortTargets() (string, error) {
	var targets []string
	for _, name := range agents.Names() {
		ag, _ := agents.Get(name)
		model := ag.ExampleModel()
		if err := agents.ValidateEffort(ag, model, qualifyEffort); err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		targets = append(targets, name+":"+model+"/"+qualifyEffort)
	}
	return strings.Join(targets, ","), nil
}

// summaryResults reads a suite's only summary line: strict, every provider passed once, each on
// the pinned CLI version.
func summaryResults(lines []string, prefix string, pinned map[string]string, scope []string) (map[string]string, error) {
	raw, err := summaryJSON(lines, prefix)
	if err != nil {
		return nil, err
	}
	var summary liveprovider.Summary
	var consult liveprovider.ConsultSummary
	ring := prefix == liveprovider.ConsultSummaryPrefix || prefix == liveprovider.DelegateSummaryPrefix
	if ring {
		if err := decodeSummary(raw, &consult); err != nil {
			return nil, err
		}
		summary.Schema, summary.Strict, summary.Totals = consult.Schema, consult.Strict, consult.Totals
		summary.Results = consult.PeerResults()
	} else if err := decodeSummary(raw, &summary); err != nil {
		return nil, err
	}
	if !summary.Strict {
		return nil, errors.New("the summary is not strict — run the -all target")
	}
	results := make(map[string]string)
	for _, result := range summary.Results {
		if !result.Passed || result.Status != liveprovider.StatusPassed {
			return nil, fmt.Errorf("%s did not pass (%s)", result.Provider, result.Status)
		}
		if result.CLIVersion != pinned[result.Provider] {
			return nil, fmt.Errorf("%s ran %q, not the pinned %q", result.Provider, result.CLIVersion, pinned[result.Provider])
		}
		if results[result.Provider] != "" {
			return nil, fmt.Errorf("%s reported twice", result.Provider)
		}
		results[result.Provider] = result.CLIVersion
	}
	if err := everyProvider(results, scope); err != nil {
		return nil, err
	}
	targets, _, err := liveprovider.ParseTargets("all")
	if err != nil {
		return nil, err
	}
	validated, err := liveprovider.NewSummary(true, targets, summary.Results)
	if err != nil || summary.Schema != validated.Schema || summary.Totals != validated.Totals {
		return nil, errors.New("summary violates the live evidence contract")
	}
	if ring {
		expected, err := liveprovider.NewConsultSummary(true, targets, summary.Results)
		if err != nil || !slices.Equal(consult.Results, expected.Results) {
			return nil, errors.New("summary does not prove the complete provider ring")
		}
	}
	return results, nil
}

func accountResults(lines []string, pinned map[string]string) (map[string]string, error) {
	raw, err := summaryJSON(lines, liveprovider.AccountsSummaryPrefix)
	if err != nil {
		return nil, err
	}
	var summary liveprovider.AccountsSummary
	if err := decodeSummary(raw, &summary); err != nil {
		return nil, err
	}
	validated, err := liveprovider.NewAccountsSummary(summary.Results)
	if err != nil || summary.Schema != validated.Schema || !validated.Success() {
		return nil, errors.New("account qualification is incomplete or failed")
	}
	results := map[string]string{}
	for _, result := range validated.Results {
		if result.Status == liveprovider.StatusNotConfigured {
			results[result.Provider] = agents.QualificationNotConfigured
			continue
		}
		if result.CLIVersion != pinned[result.Provider] {
			return nil, fmt.Errorf("%s account run did not use the pinned CLI", result.Provider)
		}
		results[result.Provider] = result.CLIVersion
	}
	return results, nil
}

func summaryJSON(lines []string, prefix string) (string, error) {
	var raw string
	for _, line := range lines {
		if failedTestLine(line) {
			return "", errors.New("a test failed")
		}
		if _, after, ok := strings.Cut(line, prefix); ok {
			if raw != "" {
				return "", errors.New("duplicate summary lines")
			}
			raw = after
		}
	}
	if raw == "" {
		return "", errors.New("no summary line — the suite did not finish")
	}
	return raw, nil
}

func decodeSummary(raw string, result any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("summary has trailing data")
	}
	return nil
}

func failedTestLine(line string) bool {
	line = strings.TrimSpace(line)
	return strings.HasPrefix(line, "--- FAIL:") || line == "FAIL" || strings.HasPrefix(line, "FAIL\t")
}

// testResults reads a suite without a summary: each provider's subtest passed, and nothing failed.
func testResults(lines []string, test string, scope []string) (map[string]string, error) {
	results := make(map[string]string)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if failedTestLine(line) {
			return nil, fmt.Errorf("a test failed: %s", line)
		}
		if name, ok := strings.CutPrefix(line, "--- PASS: "+test+"/"); ok {
			provider, _, _ := strings.Cut(name, " ")
			results[provider] = liveprovider.StatusPassed
		}
	}
	return results, everyProvider(results, scope)
}

// everyProvider requires a passing result from each provider in scope — every registered one unless
// the suite declares otherwise, and never a provider that is not registered at all.
func everyProvider(results map[string]string, scope []string) error {
	want := agents.Names()
	if len(scope) > 0 {
		for _, name := range scope {
			if !slices.Contains(agents.Names(), name) {
				return fmt.Errorf("scoped to %s, which is not a registered provider", name)
			}
		}
		want = scope
	}
	for _, name := range want {
		if results[name] == "" {
			return fmt.Errorf("no passing result for %s", name)
		}
	}
	if len(results) != len(want) {
		return fmt.Errorf("results for %d providers, want %d", len(results), len(want))
	}
	return nil
}

func readLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}
