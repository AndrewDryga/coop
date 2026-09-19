// Command qualify records a qualification of the locked client set. `make provider-qualify` runs
// every strict live suite with verbose output into one directory, then this reads each suite's log
// and writes internal/agent/locked-clients/qualification.json — only when every suite passed for
// every registered provider and every CLI version a suite reported is the one the manifest pins.
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
	"os"
	"path/filepath"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
)

// record is where the qualification is committed beside the lock it qualifies.
const record = "internal/agent/locked-clients/qualification.json"

// suites names each Makefile target provider-qualify runs and how its per-provider result is read:
// a summary prefix, or the test whose one-subtest-per-provider results must all pass.
var suites = []struct{ name, summary, test string }{
	{name: "provider-live-e2e-all", summary: liveprovider.SummaryPrefix},
	{name: "provider-live-e2e-effort", summary: liveprovider.SummaryPrefix}, // -targets: each example model at high effort
	{name: "provider-resume-live-e2e-all", summary: liveprovider.ResumeSummaryPrefix},
	{name: "provider-loop-live-e2e-all", summary: liveprovider.LoopSummaryPrefix},
	{name: "provider-consult-live-e2e-all", summary: liveprovider.ConsultSummaryPrefix},
	{name: "provider-network-live-e2e-all", summary: liveprovider.NetworkSummaryPrefix},
	{name: "acp-e2e", test: "TestLiveProviderConformance"},
	{name: "native-roles-e2e", test: "TestRuntimeNativeRolesAreDiscoveredByEveryPinnedClient"},
}

// qualifyEffort is the level the model+effort run asks every provider for.
const qualifyEffort = "high"

func main() {
	logs := flag.String("logs", "", "directory holding <suite>.log for every suite provider-qualify ran")
	targets := flag.Bool("targets", false, "print the model+effort live targets, in registry order, and exit")
	preflight := flag.Bool("preflight", false, "refuse, before any paid suite, a run that cannot qualify Coop's own clients")
	flag.Parse()
	if *preflight {
		if err := preflightCheck(); err != nil {
			fmt.Fprintf(os.Stderr, "qualify: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if *targets {
		list, err := effortTargets()
		if err != nil {
			fmt.Fprintf(os.Stderr, "qualify: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(list)
		return
	}
	if err := run(*logs); err != nil {
		fmt.Fprintf(os.Stderr, "qualify: %v\n", err)
		os.Exit(1)
	}
}

func run(logs string) error {
	if logs == "" {
		return errors.New("-logs is required")
	}
	if err := preflightCheck(); err != nil {
		return err
	}
	pinned, err := pinnedCLIVersions()
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
	q := agents.Qualification{Schema: 1, QualifiedOn: time.Now().UTC().Format(time.DateOnly), Platform: platform, Lock: lock,
		Clients: clients, Suites: make(map[string]map[string]string)}
	for _, suite := range suites {
		lines, err := readLines(filepath.Join(logs, suite.name+".log"))
		if err != nil {
			return err
		}
		var results map[string]string
		if suite.summary != "" {
			results, err = summaryResults(lines, suite.summary, pinned)
		} else {
			results, err = testResults(lines, suite.test)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", suite.name, err)
		}
		q.Suites[suite.name] = results
	}
	data, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(record, append(data, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("qualified the locked clients on every provider — wrote %s\n", record)
	return nil
}

// preflightCheck refuses what would otherwise fail only after paid suites ran: an image override (the
// suites would qualify that image, not Coop's box) and a provider with no signed-in default account
// (a strict suite fails on the skip). It checks that the account folder exists, never its contents.
func preflightCheck() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.ImageOverride != "" {
		return errors.New("COOP_IMAGE is set (in the environment or coop.conf) — provider-qualify qualifies Coop's own box; unset it")
	}
	for _, name := range agents.Names() {
		profile := cfg.DefaultProfileOf(name)
		if info, err := os.Stat(cfg.AgentProfileDir(name, profile)); err != nil || !info.IsDir() {
			return fmt.Errorf("%s has no signed-in default account (%s) — run `coop login %s` first", name, profile, name)
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

// pinnedCLIVersions is each provider's CLI version as its summary reports it ("claude-cli 2.1.260").
// Only the CLI is compared: an ACP adapter carries its own, different version.
func pinnedCLIVersions() (map[string]string, error) {
	closure, err := agents.LockedClientClosure(agents.ClientPlatform{OS: "linux", Architecture: "amd64", Libc: "glibc"})
	if err != nil {
		return nil, err
	}
	pinned := make(map[string]string)
	for _, client := range closure.Clients {
		if client.Client == egress.ClientCLI {
			pinned[client.Provider] = client.Provider + "-cli " + client.Version
		}
	}
	return pinned, nil
}

// summaryResults reads a suite's last summary line: strict, every provider passed once, each on
// the pinned CLI version.
func summaryResults(lines []string, prefix string, pinned map[string]string) (map[string]string, error) {
	var raw string
	for _, line := range lines {
		if _, after, ok := strings.Cut(line, prefix); ok {
			raw = after
		}
	}
	if raw == "" {
		return nil, errors.New("no summary line — the suite did not finish")
	}
	var summary struct {
		Strict  bool                          `json:"strict"`
		Results []liveprovider.ProviderResult `json:"results"`
	}
	var consult liveprovider.ConsultSummary
	if prefix == liveprovider.ConsultSummaryPrefix {
		if err := json.Unmarshal([]byte(raw), &consult); err != nil {
			return nil, err
		}
		summary.Strict = consult.Strict
		for _, edge := range consult.Results {
			summary.Results = append(summary.Results, edge.Peer)
		}
	} else if err := json.Unmarshal([]byte(raw), &summary); err != nil {
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
	return results, everyProvider(results)
}

// testResults reads a suite without a summary: each provider's subtest passed, and nothing failed.
func testResults(lines []string, test string) (map[string]string, error) {
	results := make(map[string]string)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "--- FAIL:") {
			return nil, fmt.Errorf("a test failed: %s", line)
		}
		if name, ok := strings.CutPrefix(line, "--- PASS: "+test+"/"); ok {
			provider, _, _ := strings.Cut(name, " ")
			results[provider] = liveprovider.StatusPassed
		}
	}
	return results, everyProvider(results)
}

func everyProvider(results map[string]string) error {
	for _, name := range agents.Names() {
		if results[name] == "" {
			return fmt.Errorf("no passing result for %s", name)
		}
	}
	if len(results) != len(agents.Names()) {
		return fmt.Errorf("results for %d providers, want %d", len(results), len(agents.Names()))
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
