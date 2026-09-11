package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/ui"
)

// The `coop net` family's approved transcripts. Each fixture under
// testdata/approved is one complete state from the CLI content review's network
// packet (items 20–26), and these tests drive the REAL renderer, parser or
// selector that produces it. A mismatch is a renderer that drifted from an
// approved decision — see the README beside the fixtures.

// TestApprovedNetHelpPages pins the family page and every leaf page, which are
// reached as `coop net <verb> --help` and `coop help net <verb>` alike.
func TestApprovedNetHelpPages(t *testing.T) {
	for fixture, key := range map[string]string{
		"20-net-help":          "net",
		"20a-net-runs-help":    "net runs",
		"20b-net-inspect-help": "net inspect",
		"20c-net-check-help":   "net check",
		"20d-net-blocked-help": "net blocked",
		"20e-net-approve-help": "net approve",
		"20f-net-watch-help":   "net watch",
		"20g-net-export-help":  "net export",
		"20h-net-forget-help":  "net forget",
		"20i-net-setup-help":   "net setup",
		"20j-net-recover-help": "net recover",
	} {
		t.Run(fixture, func(t *testing.T) {
			page, ok := commandHelp[key]
			if !ok {
				t.Fatalf("no help page registered for %q", key)
			}
			assertApprovedOutput(t, fixture, page+"\n")
		})
	}
}

// Both spellings reach the same page, and a leaf page carries no all-commands
// footer: its family page is the pointer.
func TestNetLeafHelpRoutesBothSpellings(t *testing.T) {
	cfg := freshConfig(t)
	for _, path := range [][]string{{"net", "blocked"}, {"net", "runs"}} {
		printed := captureStdout(t, func() {
			if code, err := helpForPath(path, cfg, true); code != 0 || err != nil {
				t.Errorf("coop help %s = (%d, %v)", strings.Join(path, " "), code, err)
			}
		})
		if printed != commandHelp[strings.Join(path, " ")]+"\n" {
			t.Errorf("coop help %s printed:\n%s", strings.Join(path, " "), printed)
		}
	}
	if !selfContained("net") || !selfContained("net blocked") {
		t.Error("every net page ends itself; the family page is the leaf pages' pointer")
	}
}

// --------------------------------------------------------------- access ----

func netApprovedRule(domain string) egress.Rule {
	return egress.Rule{To: egress.Destination{Domain: domain}, Protocol: "tls", Ports: []int{443}}
}

// TestApprovedNetPosture pins bare `coop net`: the YAML explanation only where
// YAML selected the mode, every other source's own truthful cause, the approved
// rules when there are any, and a pending request in place of all of it.
func TestApprovedNetPosture(t *testing.T) {
	filtered := box.NetworkAccess{Project: "/private/tmp/coop", Mode: egress.Filtered, Source: box.AccessFromProject,
		Approval: &networkstate.Approval{Posture: egress.Filtered,
			Envelope: []egress.Rule{netApprovedRule("github.com"), netApprovedRule("registry.npmjs.org")}},
		RequestedMode: egress.Filtered}
	open := box.NetworkAccess{Project: "/p", Mode: egress.Open, Source: box.AccessFromApproval,
		Approval: &networkstate.Approval{Posture: egress.Open}, RequestedMode: egress.Open}
	offline := box.NetworkAccess{Project: "/p", Mode: egress.None, Source: box.AccessFromProject, RequestedMode: egress.None}

	pending := box.NetworkAccess{Project: "/p", Mode: egress.Filtered, Source: box.AccessFromApproval,
		Approval:      &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{netApprovedRule("github.com"), netApprovedRule("old.example.com")}},
		RequestedMode: egress.Filtered,
		Add:           []egress.Rule{netApprovedRule("registry.npmjs.org")},
		Remove:        []egress.Rule{netApprovedRule("old.example.com")},
		Pending: &networkstate.PendingApproval{Reason: "this project asks for network access that has not been approved",
			Cause: ".agent/project.yaml requests changes to network access."}}
	replaced := box.NetworkAccess{Project: "/p", Mode: egress.Filtered, Source: box.AccessFromApproval, RequestedMode: egress.Filtered,
		Pending: &networkstate.PendingApproval{Reason: "the project directory at /p was replaced since it was approved",
			Cause: "This project folder was replaced after its network access was approved."}}

	for fixture, access := range map[string]box.NetworkAccess{
		"21a-net-access-filtered":         filtered,
		"21b-net-access-open":             open,
		"21c-net-access-offline":          offline,
		"21d-net-access-pending":          pending,
		"21e-net-access-pending-replaced": replaced,
	} {
		t.Run(fixture, func(t *testing.T) {
			var b bytes.Buffer
			writeNetAccess(&b, ui.Palette{}, access)
			assertApprovedOutput(t, fixture, b.String())
		})
	}
}

// A widening to open is never hidden: the access view shows the before/after
// modes and the red warning, whatever the rule diff says.
func TestNetPostureShowsAWideningToOpen(t *testing.T) {
	widening := box.NetworkAccess{Project: "/p", Mode: egress.Filtered, Source: box.AccessFromApproval,
		Approval:      &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{netApprovedRule("github.com")}},
		RequestedMode: egress.Open, Remove: []egress.Rule{netApprovedRule("github.com")},
		Pending: &networkstate.PendingApproval{Cause: ".agent/project.yaml requests unrestricted internet access."}}
	var b bytes.Buffer
	writeNetAccess(&b, ui.Palette{}, widening)
	want := "⚠ New runs need your approval\n\n" +
		"      .agent/project.yaml requests unrestricted internet access.\n\n" +
		"  - Filtered — only approved network traffic is allowed.\n" +
		"  + Unrestricted — nothing is blocked.\n" +
		"  " + netOpenWarning + "\n\n" +
		"  Access:\n  - github.com:443 · TLS  removed\n\n" +
		"  Review changes: coop net approve\n"
	if b.String() != want {
		t.Errorf("widening access:\n%s\nwant:\n%s", b.String(), want)
	}
}

// ------------------------------------------------------------------ runs ----

func netApprovedRuns(now time.Time) []netRun {
	return []netRun{
		{ExecutionSummary: networkstate.ExecutionSummary{ID: netTestRunTwo, Project: "/Users/example/Projects/coop-test",
			StartedAt: now.Add(-time.Hour), Final: true}, Outcome: "connection count unknown · at least 8 blocked attempts"},
		{ExecutionSummary: networkstate.ExecutionSummary{ID: netTestRun, Project: "/Users/example/Projects/coop-test",
			StartedAt: now.Add(-2*time.Hour - 14*time.Minute), Final: true, CleanupPending: true}, Outcome: "1 connection"},
		{ExecutionSummary: networkstate.ExecutionSummary{ID: "30d7a6cd5dabf6dacc3be65790c23c28", Project: "/Users/example/Projects/coop-test",
			StartedAt: now.Add(-2*time.Hour - 18*time.Minute)}, Outcome: netActivityUnknown},
	}
}

// TestApprovedNetRuns pins the run listing: the table with its uppercase
// header, the two empty states, the cross-project grouping, and the warning a
// listing that could not be read whole carries.
func TestApprovedNetRuns(t *testing.T) {
	now := time.Date(2026, 9, 11, 17, 3, 0, 0, time.UTC)
	runs := netApprovedRuns(now)

	t.Run("21f-net-runs-empty-project", func(t *testing.T) {
		var b bytes.Buffer
		writeNetRuns(&b, ui.Palette{}, now, nil, 0, netRunsOptions{}, networkstate.ExecutionPage{})
		assertApprovedOutput(t, "21f-net-runs-empty-project", b.String())
	})
	t.Run("21g-net-runs-empty-host", func(t *testing.T) {
		var b bytes.Buffer
		writeNetRuns(&b, ui.Palette{}, now, nil, 0, netRunsOptions{allProjects: true}, networkstate.ExecutionPage{})
		assertApprovedOutput(t, "21g-net-runs-empty-host", b.String())
	})
	t.Run("21i-net-runs-unknown-counts", func(t *testing.T) {
		var b bytes.Buffer
		writeNetRuns(&b, ui.Palette{}, now, runs, len(runs), netRunsOptions{}, networkstate.ExecutionPage{Incomplete: true})
		assertApprovedOutput(t, "21i-net-runs-unknown-counts", b.String())
	})
	t.Run("21h-net-runs-all-projects", func(t *testing.T) {
		host := []netRun{
			{ExecutionSummary: networkstate.ExecutionSummary{ID: netTestRun, Project: "/Users/example/Projects/coop-test",
				StartedAt: now.Add(-2*time.Hour - 14*time.Minute), Final: true}, Outcome: "1 connection"},
			{ExecutionSummary: networkstate.ExecutionSummary{ID: "30d7a6cd5dabf6dacc3be65790c23c28", Project: "/Users/example/Projects/coop-test",
				StartedAt: now.Add(-2*time.Hour - 18*time.Minute), Final: true}, Outcome: "no external connections"},
			{ExecutionSummary: networkstate.ExecutionSummary{ID: netTestRunTwo, Project: "/Users/example/Projects/api",
				StartedAt: now.Add(-2*time.Hour - 23*time.Minute), Final: true}, Outcome: "13 connections · 8 blocked"},
			{ExecutionSummary: networkstate.ExecutionSummary{ID: "a919fa28f0b6b6b8b3a4f9a08a1a3f2e", Project: "/Users/example/Projects/api",
				StartedAt: now.Add(-2*time.Hour - 28*time.Minute)}, Outcome: netActivityUnknown},
		}
		var b bytes.Buffer
		writeNetRuns(&b, ui.Palette{}, now, host, len(host), netRunsOptions{allProjects: true}, networkstate.ExecutionPage{Unreadable: 2})
		assertApprovedOutput(t, "21h-net-runs-all-projects", b.String())
	})
	// The count and --all appear only when there are more runs than were shown.
	t.Run("more-than-shown", func(t *testing.T) {
		var b bytes.Buffer
		writeNetRuns(&b, ui.Palette{}, now, runs, 33, netRunsOptions{}, networkstate.ExecutionPage{})
		if !strings.HasSuffix(b.String(), "\nShowing 3 of 33 · coop net runs --all\n") {
			t.Errorf("bounded listing:\n%s", b.String())
		}
		b.Reset()
		writeNetRuns(&b, ui.Palette{}, now, runs, len(runs), netRunsOptions{}, networkstate.ExecutionPage{})
		if strings.Contains(b.String(), "Showing") {
			t.Errorf("a complete listing claimed it was bounded:\n%s", b.String())
		}
	})
}

// ------------------------------------------------------------- selection ----

// TestApprovedNetRunSelection pins the three refusals a run reference can earn.
func TestApprovedNetRunSelection(t *testing.T) {
	page := networkstate.ExecutionPage{Executions: []networkstate.ExecutionSummary{
		{ID: "e644f07ab14f67cd3f5af72c89295488"}, {ID: "e644fa12b14f67cd3f5af72c89295488"}}}
	_, err := netResolveRun(page, "e644", "coop net inspect")
	if err == nil {
		t.Fatal("an ambiguous prefix was resolved")
	}
	assertApprovedOutput(t, "21j-net-run-ambiguous", usageBlock(t, err))

	_, err = netResolveRun(page, "abcd", "coop net inspect")
	if err == nil {
		t.Fatal("an unknown run was resolved")
	}
	assertApprovedOutput(t, "21k-net-run-unknown", usageBlock(t, err))

	page.Incomplete = true
	_, err = netResolveRun(page, "e644", "coop net inspect")
	if err == nil {
		t.Fatal("a prefix was trusted against an incomplete inventory")
	}
	assertApprovedOutput(t, "21l-net-run-incomplete", usageBlock(t, err))
}

// TestApprovedNetWatchSelection pins watch's own two selection refusals.
func TestApprovedNetWatchSelection(t *testing.T) {
	project := netFixtureProject(t)
	a := &app{cfg: &config.Config{RepoOverride: project}}
	sealed := networkstate.ExecutionPage{Executions: []networkstate.ExecutionSummary{{ID: netTestRun, Project: project, Final: true}}}
	_, err := a.netSelectRun("watch", sealed, "")
	if err == nil {
		t.Fatal("watch selected a finished run")
	}
	assertApprovedOutput(t, "23b-net-watch-none", usageBlock(t, err))

	two := networkstate.ExecutionPage{Executions: []networkstate.ExecutionSummary{
		{ID: "cb375d22a1be564428a533a0a838e32a", Project: project, StartedAt: time.Unix(20, 0)},
		{ID: netTestRun, Project: project, StartedAt: time.Unix(10, 0)}}}
	_, err = a.netSelectRun("watch", two, "")
	if err == nil {
		t.Fatal("watch chose between two unfinished runs")
	}
	assertApprovedOutput(t, "23c-net-watch-choose", usageBlock(t, err))
}

// ----------------------------------------------------------------- check ----

// TestApprovedNetCheckCurrent pins the one-sentence answers for a new run.
func TestApprovedNetCheckCurrent(t *testing.T) {
	answers := map[string]netCheckAnswer{
		"22a-net-check-no-rule": netCurrentCheck(box.NetworkAccess{Mode: egress.Filtered}, "example.com", 443, nil),
		"22b-net-check-offline": netCurrentCheck(box.NetworkAccess{Mode: egress.None}, "example.com", 443, nil),
		"22c-net-check-open":    netCurrentCheck(box.NetworkAccess{Mode: egress.Open}, "example.com", 443, nil),
		"22d-net-check-pending": netCurrentCheck(box.NetworkAccess{Mode: egress.Filtered,
			Pending: &networkstate.PendingApproval{Cause: ".agent/project.yaml requests changes to network access."}}, "example.com", 443, nil),
	}
	for fixture, answer := range answers {
		t.Run(fixture, func(t *testing.T) {
			var b bytes.Buffer
			writeNetCheck(&b, ui.Palette{}, answer)
			assertApprovedOutput(t, fixture, b.String())
		})
	}
}

// A provider's own access answers in its own name, and several providers share
// one sentence rather than repeating the note per agent.
func TestNetCheckNamesTheProviderThatAllowsIt(t *testing.T) {
	bundle := func(domain string) egress.Bundle {
		return egress.Bundle{Core: []egress.Rule{netApprovedRule(domain)}}
	}
	one := netCurrentCheck(box.NetworkAccess{Mode: egress.Filtered}, "api.anthropic.com", 443,
		map[string]egress.Bundle{"claude": bundle("api.anthropic.com")})
	if one.Verdict != "api.anthropic.com:443 is allowed by Claude's provider access." || one.Cause != "" {
		t.Errorf("single provider = %+v", one)
	}
	two := netCurrentCheck(box.NetworkAccess{Mode: egress.Filtered}, "shared.example.com", 443,
		map[string]egress.Bundle{"claude": bundle("shared.example.com"), "codex": bundle("shared.example.com")})
	if two.Verdict != "shared.example.com:443 is allowed for Claude and Codex." ||
		two.Cause != "Their provider access is included automatically." {
		t.Errorf("two providers = %+v", two)
	}
}

// TestApprovedNetCheckHistorical pins the verdicts that name a recorded run.
func TestApprovedNetCheckHistorical(t *testing.T) {
	port := func(n int) int { return n }
	for fixture, result := range map[string]networkstate.PolicyExplanation{
		"22e-net-check-run-project-rule": {RunID: netTestRun, Domain: "example.com", Protocol: "tls", Port: port(443),
			Allowed: true, Reason: "rule_allowed", Origins: []networkstate.PolicyOrigin{{Kind: "project"}}},
		"22f-net-check-run-provider": {RunID: netTestRun, Domain: "api.anthropic.com", Protocol: "tls", Port: port(443),
			Allowed: true, Reason: "rule_allowed", Origins: []networkstate.PolicyOrigin{{Kind: "provider", Provider: "claude", BundleVersion: "2026-09-01"}}},
		"22g-net-check-run-protected": {RunID: netTestRun, Peer: "169.254.169.254", Protocol: "tcp", Port: port(80),
			Reason: "protected_destination", Message: networkstate.DiagnosticReason("protected_destination")},
	} {
		t.Run(fixture, func(t *testing.T) {
			var b bytes.Buffer
			writeNetHistoricalCheck(&b, ui.Palette{}, result)
			assertApprovedOutput(t, fixture, b.String())
		})
	}
}

// TestApprovedNetCheckRefusals pins the two rejected-input blocks check owns.
func TestApprovedNetCheckRefusals(t *testing.T) {
	_, err := parseNetDiagnosticArgs("check", []string{"*.example.com"})
	if err == nil {
		t.Fatal("a wildcard was accepted as a destination")
	}
	assertApprovedOutput(t, "22h-net-check-invalid-address", usageBlock(t, err))

	_, err = parseNetDiagnosticArgs("check", []string{"10.0.0.1"})
	if err == nil {
		t.Fatal("an address was checked with no run or transport")
	}
	assertApprovedOutput(t, "22i-net-check-ip-needs-run", usageBlock(t, err))
}

// --------------------------------------------------------------- blocked ----

// netBlockedFixture is one retained refusal as `coop net blocked` groups it.
func netBlockedFixture(host, kind, reason string, port *int, count int, candidate *networkview.Candidate) netExplainCause {
	at := time.Date(2026, 9, 11, 16, 3, 0, 0, time.UTC)
	event := networkview.Denial{ID: "d1", Kind: kind, Reason: reason, Name: host, Port: port, At: at, Candidate: candidate}
	return netExplainCause{Count: count, Explanation: networkstate.EventExplanation{RunID: netTestRunTwo, Event: event,
		Message: networkstate.DiagnosticReason(reason)}}
}

// TestApprovedNetBlocked pins every blocked-history state: each refusal class,
// the mixed-cause grouping, and the copyable rule that only proved evidence earns.
func TestApprovedNetBlocked(t *testing.T) {
	now := time.Date(2026, 9, 11, 17, 30, 0, 0, time.UTC)
	p443 := 443
	candidate := &networkview.Candidate{Rule: egress.Rule{To: egress.Destination{Domain: "registry.example.com"},
		Protocol: "tls", Ports: []int{443}}}

	cases := map[string]netExplanation{
		"22k-net-blocked-dns": {RunID: netTestRunTwo, Host: "registry.npmjs.org",
			Causes: []netExplainCause{netBlockedFixture("registry.npmjs.org", "dns_denied", "unapproved_name", nil, 4, nil)}},
		"22l-net-blocked-protected": {RunID: netTestRunTwo, Host: "169.254.169.254",
			Causes: []netExplainCause{netBlockedFixture("", "admission_failed", "protected_destination", nil, 1, nil)}},
		"22m-net-blocked-ech": {RunID: netTestRunTwo, Host: "example.com",
			Causes: []netExplainCause{netBlockedFixture("example.com", "tls_denied", "tls_ech_unsupported", &p443, 1, nil)}},
		"22n-net-blocked-upstream": {RunID: netTestRunTwo, Host: "example.com",
			Causes: []netExplainCause{netBlockedFixture("example.com", "tls_denied", "upstream_unreachable", &p443, 1, nil)}},
		"22o-net-blocked-observation": {RunID: netTestRunTwo, Host: "example.com",
			Causes: []netExplainCause{netBlockedFixture("example.com", "tls_denied", "observation_unavailable", &p443, 1, nil)}},
		"22p-net-blocked-mixed": {RunID: netTestRunTwo, Host: "registry.example.com", Causes: []netExplainCause{
			netBlockedFixture("registry.example.com", "dns_denied", "unapproved_name", nil, 2, nil),
			netBlockedFixture("registry.example.com", "tls_denied", "unapproved_name", &p443, 1, candidate)}},
	}
	for fixture, explanation := range cases {
		t.Run(fixture, func(t *testing.T) {
			var b bytes.Buffer
			writeNetExplanation(&b, ui.Palette{}, now, explanation)
			assertApprovedOutput(t, fixture, b.String())
		})
	}
	t.Run("22j-net-blocked-none", func(t *testing.T) {
		var b bytes.Buffer
		writeNetNoBlock(&b, ui.Palette{}, "example.com", "in this project's runs", true)
		assertApprovedOutput(t, "22j-net-blocked-none", b.String())
	})
}

// Incomplete evidence replaces the copyable rule with one warning: a record
// that lost detail cannot prove the smallest rule that would have helped.
func TestNetBlockedRefusesToDraftARuleFromIncompleteEvidence(t *testing.T) {
	p443 := 443
	cause := netBlockedFixture("registry.example.com", "tls_denied", "unapproved_name", &p443, 1,
		&networkview.Candidate{Rule: egress.Rule{To: egress.Destination{Domain: "registry.example.com"}, Protocol: "tls", Ports: []int{443}}})
	cause.Explanation.DetailTruncated = true
	var b bytes.Buffer
	writeNetExplanation(&b, ui.Palette{}, time.Now(), netExplanation{RunID: netTestRunTwo, Host: "registry.example.com",
		Causes: []netExplainCause{cause}})
	if strings.Contains(b.String(), "egress_rules") {
		t.Errorf("a rule was drafted from truncated evidence:\n%s", b.String())
	}
	if !strings.HasSuffix(b.String(), "⚠ "+netIncompleteRecording+"\n") {
		t.Errorf("incomplete evidence was not warned about:\n%s", b.String())
	}
}

// Every retained reason renders a sentence of its own, and an unknown code is
// shown as itself rather than guessed at.
func TestEveryRetainedReasonHasItsOwnSentence(t *testing.T) {
	seen := map[string]string{}
	for _, reason := range []string{
		"open", "rule_allowed", "unapproved_name", "protected_destination", "unsafe_dns_answer",
		"protocol_not_allowed", "port_not_allowed", "fixed_egress_policy", "tls_name_missing", "tls_name_invalid",
		"tls_ech_unsupported", "tls_malformed", "tls_hello_too_large", "tls_inspection_timeout", "tls_inspection_unavailable",
		"dns_name_invalid", "dns_query_invalid", "dns_answer_invalid", "dns_cname_limit", "dns_answer_limit",
		"dns_upstream_invalid", "dns_unavailable", "dns_no_address", "dns_ttl_expired", "dns_capacity_exceeded",
		"gateway_connection_capacity", "gateway_lease_capacity", "gateway_lease_refused", "gateway_unavailable",
		"enforcement_unavailable", "clock_unavailable", "observation_unavailable", "upstream_unreachable", "unsupported_capability",
	} {
		message := networkstate.DiagnosticReason(reason)
		if message == "" || !strings.HasSuffix(message, ".") || strings.Contains(message, reason) {
			t.Errorf("reason %q renders %q", reason, message)
		}
		if other, ok := seen[message]; ok && other != reason {
			t.Errorf("reasons %q and %q share one sentence %q", other, reason, message)
		}
		seen[message] = reason
	}
	unknown := networkstate.DiagnosticReason("a_reason_from_the_future")
	if unknown != "This Coop version cannot explain the recorded reason: a_reason_from_the_future." {
		t.Errorf("unknown reason = %q", unknown)
	}
	if hostile := networkstate.DiagnosticReason("bad\x1b[31mcode"); strings.Contains(hostile, "\x1b") {
		t.Errorf("an unknown reason carried an escape sequence: %q", hostile)
	}
}

// ----------------------------------------------------------------- watch ----

// TestApprovedNetWatchStream pins the whole stream: the opening two lines, one
// appended line per new event, and the shared final report exactly once.
func TestApprovedNetWatchStream(t *testing.T) {
	at := func(second int) time.Time { return time.Date(2026, 9, 10, 14, 49, 30+second, 0, time.UTC) }
	live := netTestClean()
	live.Receipt, live.Freshness, live.Cleanup = nil, networkstate.FreshnessFresh, "pending"
	live.Observed.Terminal = false

	empty := live
	empty.Observed.Connections, empty.Observed.Sequence, empty.ReadAt = nil, 1, at(0)

	allowed := live
	allowed.ReadAt = at(3)

	blocked := allowed
	candidate := &networkview.Candidate{Rule: egress.Rule{To: egress.Destination{Domain: "registry.example.com"}, Protocol: "tls", Ports: []int{443}}}
	blocked.Observed.Denials = []networkview.Denial{
		{ID: "b1", Kind: "tls_denied", Reason: "unapproved_name", Name: "registry.example.com", Port: &[]int{443}[0], At: at(4), Candidate: candidate},
		{ID: "b2", Kind: "tls_denied", Reason: "unapproved_name", Name: "registry.example.com", Port: &[]int{443}[0], At: at(4), Candidate: candidate}}
	blocked.ReadAt = at(4)

	final := netTestClean()
	final.Observed.Denials = blocked.Observed.Denials
	final.Receipt.Snapshot.Denials = blocked.Observed.Denials
	final.ReadAt = at(5)

	reads := []networkstate.Inspection{empty, allowed, blocked, final}
	var out bytes.Buffer
	tick := make(chan time.Time, len(reads))
	for range reads[1:] {
		tick <- time.Time{}
	}
	step := 0
	code, err := runNetWatch(netWatchDeps{
		ctx: context.Background(), tick: tick, now: func() time.Time { return at(step) }, out: &out,
		id: netTestRun, palette: ui.Palette{},
		read: func(time.Time) (networkstate.Inspection, error) {
			value := reads[step]
			if step < len(reads)-1 {
				step++
			}
			return value, nil
		},
	})
	if code != 0 || err != nil {
		t.Fatalf("watch = (%d, %v)", code, err)
	}
	assertApprovedOutput(t, "23a-net-watch-stream", out.String())
}

// ---------------------------------------------------------------- export ----

func TestApprovedNetExportRefusals(t *testing.T) {
	err := &ui.UsageError{Headline: "Network run e644f07a has no final record yet",
		Rows: [][2]string{{"Current report:", "coop net inspect e644f07a"}}}
	assertApprovedOutput(t, "24a-net-export-unfinished", err.Render(ui.Palette{}))
}

// ---------------------------------------------------------------- forget ----

// netForgetFixture is a review of one saved approval, with no store behind it:
// the confirmation is a rendering decision, and this pins the bytes.
type netForgetFixture struct {
	approval *networkstate.Approval
	gone     bool
}

func (f netForgetFixture) Project() string                  { return "/Users/example/Projects/coop-test" }
func (f netForgetFixture) Approval() *networkstate.Approval { return f.approval }
func (f netForgetFixture) Gone() bool                       { return f.gone }
func (f netForgetFixture) Commit(context.Context) error     { return nil }

// TestApprovedNetForget pins the withdrawal confirmation, including the folder
// that no longer exists, and the two answers it can be given.
func TestApprovedNetForget(t *testing.T) {
	filtered := netForgetFixture{approval: &networkstate.Approval{Posture: egress.Filtered,
		Envelope: []egress.Rule{netApprovedRule("github.com"), netApprovedRule("registry.npmjs.org")}}}
	var b bytes.Buffer
	if err := confirmNetForget(context.Background(), filtered, &b, func() bool { return true }); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	assertApprovedOutput(t, "25a-net-forget-confirm", b.String())

	gone := netForgetFixture{approval: &networkstate.Approval{Posture: egress.Open}, gone: true}
	b.Reset()
	err := confirmNetForget(context.Background(), gone, &b, func() bool { return false })
	if !errors.Is(err, errNetDeclined) {
		t.Fatalf("declining returned %v", err)
	}
	assertApprovedOutput(t, "25b-net-forget-decline", b.String())

	// The two outcomes are fixed copy: no error marker for a declined answer.
	for _, want := range []string{"Network approval withdrawn", "Cancelled. Network approval was kept.",
		"No network approval is saved for this project."} {
		if want == "" {
			t.Error("empty outcome copy")
		}
	}
}

func TestApprovedNetForgetRefusals(t *testing.T) {
	assertApprovedOutput(t, "25c-net-forget-none", netForgetNothing+"\n")
	assertApprovedOutput(t, "25d-net-forget-non-tty", usageBlock(t, netTerminalOnly("coop net forget")))
}

// --------------------------------------------------------------- recovery ----

// TestApprovedNetRecover pins every explicit-recovery outcome.
func TestApprovedNetRecover(t *testing.T) {
	for fixture, result := range map[string]box.NetworkRecovery{
		"26b-net-recover-cleaned": {RunID: netTestRun, Removed: []string{"agent", "guard", "controller", "ipc", "observations"},
			RemovedContainers: 3, RemovedVolumes: 2},
		"26c-net-recover-already-clean": {RunID: netTestRun},
		"26d-net-recover-left-alone":    {RunID: netTestRun, Live: true, Skipped: "its supervisor (pid 42) is still running or its identity is uncertain"},
		"26e-net-recover-daemon-unavailable": {RunID: netTestRun,
			Skipped: "the runtime it ran on is unavailable: Docker is unavailable"},
		"26f-net-recover-wrong-daemon": {RunID: netTestRun,
			Skipped: "the runtime at unix:///var/run/docker.sock is a different daemon than the one this run used"},
		"26g-net-recover-partial": {RunID: netTestRun, Removed: []string{"agent", "guard"}, Pending: []string{"ipc"},
			RemovedContainers: 2, PendingVolumes: 1},
		"26h-net-recover-unverified": {RunID: netTestRun, Pending: []string{"ipc"}, PendingVolumes: 1, Unverified: true},
	} {
		t.Run(fixture, func(t *testing.T) {
			var b bytes.Buffer
			writeNetRecovery(&b, ui.Palette{}, result)
			assertApprovedOutput(t, fixture, b.String())
		})
	}
	assertApprovedOutput(t, "26a-net-recover-nothing", "No interrupted network runs need cleanup.\n")
}
