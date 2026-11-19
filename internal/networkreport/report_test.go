package networkreport

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkview"
)

// An IPv6 address joined to its port reads in brackets wherever a report shows one, so
// 2606:4700::1 port 22 never reads as the longer address 2606:4700::1:22. IPv4 and names stay bare.
func TestAnIPv6AddressWithAPortReadsInBrackets(t *testing.T) {
	port := 22
	for _, tc := range []struct{ got, want string }{
		{HostPort("2606:4700::1", "22"), "[2606:4700::1]:22"},
		{HostPort("1.1.1.1", "80"), "1.1.1.1:80"},
		{HostPort("example.com", "443"), "example.com:443"},
		{destinationKey(networkview.Connection{Peer: "[2606:4700::1]:22", Transport: "tcp"}).Label(), "[2606:4700::1]:22 · TCP"},
		{RuleText(egress.Rule{To: egress.Destination{CIDR: "2606:4700::1/128"}, Protocol: "tcp", Ports: []int{22, 2222}}), "[2606:4700::1]:22,2222 · TCP"},
		{RuleText(egress.Rule{To: egress.Destination{IP: "2606:4700::1"}, Protocol: "tcp", Ports: []int{22}}), "[2606:4700::1]:22 · TCP"},
		{RefusalLabel(networkview.Denial{Kind: "tls_denied", Name: "2606:4700::1", Port: &port, Reason: "unapproved_name"}), "[2606:4700::1]:22 · TLS"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// reasonWrite captures the code from a source line that writes a reason into a record a report can
// print, including a Failure, which safeReason turns into a denial's reason.
var reasonWrite = regexp.MustCompile(`(?:` +
	`(?:markProxyPartial|markBoundaryGap|partialCoverage|missingCoverage|Failure)\(|` +
	`addSessionLossReason\([^,]+, |append\([^,]*Reasons, |Reasons = \[\]string\{|` +
	`\b[Rr]eason(?:s\[\d+\])?\s*(?::=|=|:)\s*(?:[\w.]+,\s*|"[^"\n]*",\s*)*|` +
	`lastOutcome = |return "ready", ` +
	`)"([a-z][a-z0-9_]*)"`)

// brokerRefusal captures the code the native broker refuses a request with: refuse records it as an
// admission_failed event, which the evidence and `coop net blocked` print. The broker's other stop
// reasons and failures go only to the gateway's log, so only this write is read from its files.
var brokerRefusal = regexp.MustCompile(`\brefuse\([^,]+, [^,]+, [^,]+, "([a-z][a-z0-9_]*)"`)

// notPrintedReasons match reasonWrite without being reasons a report prints.
var notPrintedReasons = map[string]bool{
	"success":        true, // the resolver's own state; health() reports it as last_lookup_succeeded
	"stopped":        true, // a status assigned on the same line as a source's stop reason
	"unknown_reason": true, // the retag for --json; the human sentence is the message beside it
}

// TestEveryRecordedReasonHasPlainText reads the gateway's source for the reason codes it records, so
// a new code cannot reach a person raw, or as "this version cannot explain", without failing here.
func TestEveryRecordedReasonHasPlainText(t *testing.T) {
	codes := map[string]bool{}
	for _, dir := range []string{"../networkgateway", "../networkview", "../networkstate"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			base := filepath.Base(file)
			if strings.HasSuffix(base, "_test.go") {
				continue
			}
			source, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			scan := reasonWrite
			if strings.HasPrefix(base, "native_") {
				scan = brokerRefusal
			}
			for _, line := range strings.Split(string(source), "\n") {
				for _, match := range scan.FindAllStringSubmatch(line, -1) {
					codes[match[1]] = true
				}
			}
		}
	}
	// The scan itself must keep finding the codes the gateway writes, one from each kind of producer.
	for _, sure := range []string{
		"upstream_reset", "proxy_flow_stale", "socket_join_expired", "dns_query_invalid", "owned_run_set_incomplete", "gateway_start_timeout",
		"terminal_observation_unavailable", "last_lookup_exchange_failed", "native_broker_connect_refused",
	} {
		if !codes[sure] {
			t.Errorf("the source scan no longer finds %q; update reasonWrite", sure)
		}
	}
	for code := range codes {
		if notPrintedReasons[code] {
			continue
		}
		for name, render := range map[string]func(string) string{
			"refusalReasonText": refusalReasonText, "endpointReason": endpointReason, "evidenceReasonText": evidenceReasonText,
		} {
			if text := render(code); strings.Contains(text, code) || strings.Contains(text, "cannot explain") {
				t.Errorf("%s(%q) = %q, want plain words", name, code, text)
			}
		}
	}
}

// A code this build does not know is named, safely, instead of guessed at or dropped.
func TestAnUnknownReasonIsNamedNotGuessed(t *testing.T) {
	for name, render := range map[string]func(string) string{
		"refusalReasonText": refusalReasonText, "endpointReason": endpointReason, "evidenceReasonText": evidenceReasonText,
	} {
		text := render("a_reason_from_the_future")
		if text != "This Coop version cannot explain the recorded reason: a_reason_from_the_future" {
			t.Errorf("%s = %q", name, text)
		}
		if hostile := render("bad\x1b[31mcode"); strings.Contains(hostile, "\x1b") {
			t.Errorf("%s carried an escape sequence: %q", name, hostile)
		}
	}
}

// A gateway failure on a refused destination's row names the gateway: the clauses written for health
// rows say "it", which here would read as the destination.
func TestAGatewayFailureOnARefusalNamesTheGateway(t *testing.T) {
	for reason, want := range map[string]string{
		"gateway_unavailable":         "Coop's network gateway was unavailable",
		"gateway_connection_capacity": "Coop's network gateway reached its connection limit",
		"enforcement_unavailable":     "Network enforcement became unavailable",
	} {
		if got := refusalReasonText(reason); got != want {
			t.Errorf("refusalReasonText(%q) = %q, want %q", reason, got, want)
		}
	}
}
