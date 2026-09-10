package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/ui"
)

func TestNetListFlagsRejectAnythingElse(t *testing.T) {
	opts, err := parseNetListFlags([]string{"--all", "--json"})
	if err != nil || !opts.all || !opts.json {
		t.Fatalf("parseNetListFlags = (%+v, %v)", opts, err)
	}
	if _, err := parseNetListFlags([]string{"--project", "/tmp"}); err == nil {
		t.Error("an unknown ls flag was accepted")
	}
}

func TestNetRunArgsNeedOneIdAndScopeDestinationsToReceipt(t *testing.T) {
	opts, err := parseNetRunArgs("inspect", []string{"abc", "--json"})
	if err != nil || opts.id != "abc" || !opts.json {
		t.Fatalf("parseNetRunArgs = (%+v, %v)", opts, err)
	}
	if _, err := parseNetRunArgs("inspect", []string{"--json"}); err == nil {
		t.Error("inspect without a run id was accepted")
	}
	if _, err := parseNetRunArgs("inspect", []string{"a", "b"}); err == nil {
		t.Error("inspect with two ids was accepted")
	}
	// The receipt is the shareable artifact, so it alone carries the operator's
	// opt-in to unredact destinations.
	if opts, err := parseNetRunArgs("receipt", []string{"abc", "--destinations"}); err != nil || !opts.destinations {
		t.Errorf("receipt --destinations = (%+v, %v)", opts, err)
	}
	if _, err := parseNetRunArgs("inspect", []string{"abc", "--destinations"}); err == nil {
		t.Error("inspect accepted --destinations")
	}
}

func TestNetListingDTOPublishesOnlyItsAllowlist(t *testing.T) {
	runs := []networkstate.ExecutionSummary{{ID: "a", Epoch: "e", Project: "/private/host/secret-project",
		StartedAt: time.Unix(1, 0).UTC(), Final: true}}
	dto := netListingDTO(runs, networkstate.ExecutionPage{Unreadable: 2, Incomplete: true})
	var buf bytes.Buffer
	if err := netWriteJSON(&buf, dto); err != nil {
		t.Fatal(err)
	}
	data := buf.String()
	// The retained record carries the owner's project path. Publishing it by
	// growing into a serialized type is exactly what the allowlist prevents.
	if strings.Contains(data, "secret-project") {
		t.Errorf("listing DTO leaked the project path: %s", data)
	}
	for _, want := range []string{`"id":"a"`, `"final":true`, `"unreadable":2`, `"incomplete":true`} {
		if !strings.Contains(data, want) {
			t.Errorf("listing DTO is missing %s: %s", want, data)
		}
	}
}

func TestNetFilterProjectMatchesThroughSymlinks(t *testing.T) {
	runs := []networkstate.ExecutionSummary{{ID: "a", Project: "/private/tmp/x"}, {ID: "b", Project: "/other"}}
	got := netFilterProject(runs, "/private/tmp/x")
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("netFilterProject = %+v", got)
	}
	if got := netFilterProject(runs, "/nothing/here"); got != nil {
		t.Errorf("unrelated project matched: %+v", got)
	}
}

// A watch prints one full block to open, coalesced lines while the run is
// alive, and one full block when the receipt seals. It never repaints.
func TestWatchCoalescesUntilTheReceiptSeals(t *testing.T) {
	reads := []networkstate.Inspection{
		{Freshness: networkstate.FreshnessNotObserved, Revision: 1},
		{Freshness: networkstate.FreshnessFresh, Revision: 2, Observed: networkview.Snapshot{Sequence: 1,
			Denials: []networkview.Denial{{ID: "d1", Name: "example.org", Reason: "unapproved_name"}}}},
		{Freshness: networkstate.FreshnessTerminal, Revision: 3, Observed: networkview.Snapshot{Sequence: 2},
			Receipt: &networkview.Receipt{ID: "run1", Finality: "final", Completeness: "partial"}},
	}
	var out bytes.Buffer
	tick := make(chan time.Time, len(reads))
	for range reads {
		tick <- time.Unix(0, 0)
	}
	i := 0
	code, err := runNetWatch(netWatchDeps{
		ctx: context.Background(), tick: tick, now: func() time.Time { return time.Unix(int64(i), 0) },
		out: &out, id: "run1", palette: ui.Palette{},
		read: func(time.Time) (networkstate.Inspection, error) {
			value := reads[i]
			if i < len(reads)-1 {
				i++
			}
			return value, nil
		},
	})
	if code != 0 || err != nil {
		t.Fatalf("runNetWatch = (%d, %v)", code, err)
	}
	text := out.String()
	if blocks := strings.Count(text, "Network run run1"); blocks != 2 {
		t.Errorf("printed %d full blocks, want the opening one and the sealed one:\n%s", blocks, text)
	}
	if !strings.Contains(text, "refused example.org (unapproved_name)") {
		t.Errorf("the refusal was not appended:\n%s", text)
	}
	if !strings.Contains(text, "final, partial") {
		t.Errorf("final + partial must not read as a complete record:\n%s", text)
	}
}

func TestWatchExitsImmediatelyOnAnAlreadySealedRun(t *testing.T) {
	var out bytes.Buffer
	code, err := runNetWatch(netWatchDeps{
		ctx: context.Background(), tick: make(chan time.Time), now: time.Now, out: &out, id: "run1", palette: ui.Palette{},
		read: func(time.Time) (networkstate.Inspection, error) {
			return networkstate.Inspection{Receipt: &networkview.Receipt{ID: "run1", Finality: "final"}}, nil
		},
	})
	if code != 0 || err != nil {
		t.Fatalf("runNetWatch = (%d, %v)", code, err)
	}
	if strings.Count(out.String(), "Network run run1") != 1 {
		t.Errorf("a sealed run printed more than its one block:\n%s", out.String())
	}
}

func TestWatchReportsAReadFailureInsteadOfLoopingOnIt(t *testing.T) {
	code, err := runNetWatch(netWatchDeps{
		ctx: context.Background(), tick: make(chan time.Time), now: time.Now, out: &bytes.Buffer{}, id: "gone",
		palette: ui.Palette{},
		read: func(time.Time) (networkstate.Inspection, error) {
			return networkstate.Inspection{}, networkstate.ErrEvidenceUnavailable
		},
	})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "coop net ls --all") {
		t.Fatalf("runNetWatch = (%d, %v), want a failure naming the listing", code, err)
	}
}

func TestWatchStopsWhenTheReaderCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, err := runNetWatch(netWatchDeps{
		ctx: ctx, tick: make(chan time.Time), now: time.Now, out: &bytes.Buffer{}, id: "run1", palette: ui.Palette{},
		read: func(time.Time) (networkstate.Inspection, error) { return networkstate.Inspection{}, nil },
	})
	if code != 0 || err != nil {
		t.Fatalf("cancelled watch = (%d, %v)", code, err)
	}
}

func TestWatchDeltaCoalescesRepeatsAndBoundsItself(t *testing.T) {
	repeat := func(n int) networkstate.Inspection {
		var denials []networkview.Denial
		for i := range n {
			denials = append(denials, networkview.Denial{ID: string(rune('a' + i)), Name: "example.org", Reason: "unapproved_name"})
		}
		return networkstate.Inspection{Observed: networkview.Snapshot{Denials: denials}}
	}
	lines := netWatchDelta(networkstate.Inspection{}, repeat(4))
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "×4") {
		t.Errorf("four retries of one name = %q, want one counted line", lines)
	}
	var many []networkview.Denial
	for i := range netWatchDeltaLines + 3 {
		many = append(many, networkview.Denial{ID: string(rune('a' + i)), Name: "h" + string(rune('a'+i)) + ".example", Reason: "unapproved_name"})
	}
	lines = netWatchDelta(networkstate.Inspection{}, networkstate.Inspection{Observed: networkview.Snapshot{Denials: many}})
	if len(lines) != netWatchDeltaLines+1 || !strings.Contains(lines[len(lines)-1], "coop net inspect") {
		t.Errorf("a refusal storm was not bounded: %q", lines)
	}
	// Already-reported evidence never repeats: replay is idempotent.
	if lines := netWatchDelta(repeat(4), repeat(4)); lines != nil {
		t.Errorf("unchanged evidence produced %q", lines)
	}
}

// The two counts are different units and are never added together. A run
// nothing observed says so rather than showing a pair of zeros.
func TestRunOutcomeCountsAllowedAndRefusedSeparately(t *testing.T) {
	if got := netRunOutcome(networkstate.Inspection{}); got != "nothing observed" {
		t.Errorf("unobserved run = %q", got)
	}
	connections := networkview.Count(3)
	observed := networkstate.Inspection{Observed: networkview.Snapshot{Sequence: 1,
		Counters: &networkview.Counters{Connections: &connections},
		Denials:  []networkview.Denial{{ID: "a"}, {ID: "b"}}}}
	if got := netRunOutcome(observed); got != "3 allowed, 2 refused" {
		t.Errorf("outcome = %q", got)
	}
	observed.Observed.Counters = nil
	observed.Observed.Loss.DetailTruncated = true
	if got := netRunOutcome(observed); got != "UNKNOWN allowed, 2+ refused" {
		t.Errorf("truncated outcome = %q, want UNKNOWN allowed and a lower bound", got)
	}
}

// Unavailable is never zero. An inspection with no counters must say so.
func TestInspectionNeverPrintsAnUnmeasuredZero(t *testing.T) {
	var b bytes.Buffer
	writeNetInspection(&b, ui.Palette{}, "run1", networkstate.Inspection{Freshness: networkstate.FreshnessNotObserved})
	text := b.String()
	for _, want := range []string{"UNKNOWN — nothing was recorded for this run", "never — nothing was recorded for this run", "not sealed yet",
		"enforcer UNKNOWN", "coop net inspect run1 --json"} {
		if !strings.Contains(text, want) {
			t.Errorf("inspection is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "0 B sent") {
		t.Errorf("inspection fabricated a zero total:\n%s", text)
	}
}

func TestReceiptViewSeparatesFinalityFromCompletenessAndSaysWhatIsWithheld(t *testing.T) {
	receipt := networkview.Receipt{ID: "run1", Finality: "final", Completeness: "partial", Workload: "killed",
		Cleanup: "pending", Digest: "abc", DigestScope: "destinations-withheld",
		Snapshot: networkview.Snapshot{Projection: "destinations-withheld",
			Denials: []networkview.Denial{{ID: "d1", DestinationID: "opaque", Kind: "tls_denied", Reason: "unapproved_name"}}}}
	var b bytes.Buffer
	writeNetReceipt(&b, ui.Palette{}, "run1", receipt)
	text := b.String()
	for _, want := range []string{"final, partial", "name withheld (opaque)", "--destinations",
		"UNKNOWN — no counters were sealed with this receipt"} {
		if !strings.Contains(text, want) {
			t.Errorf("receipt view is missing %q:\n%s", want, text)
		}
	}
}

func TestNetProjectRefusesToGuessOutsideAProject(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if _, err := netProject(""); err == nil || !strings.Contains(err.Error(), "coop net ls --all") {
		t.Fatalf("netProject outside a project = %v, want a pointer at the explicit scope", err)
	}
	if got, err := netProject(dir); err != nil || got != dir {
		t.Errorf("netProject(%q) = (%q, %v)", dir, got, err)
	}
}

func TestNetRunErrPointsAtTheListing(t *testing.T) {
	err := netRunErr("abc", networkstate.ErrEvidenceUnavailable)
	if !strings.Contains(err.Error(), "coop net ls --all") || !strings.Contains(err.Error(), `"abc"`) {
		t.Errorf("netRunErr = %v", err)
	}
	if err := netRunErr("abc", errors.New("boom")); !strings.Contains(err.Error(), "boom") {
		t.Errorf("netRunErr dropped the cause: %v", err)
	}
}

func TestNetEventBoundRefusesAnOversizedView(t *testing.T) {
	if err := netEventBound(netEventMaxBytes + 1); err == nil {
		t.Error("an oversized view was accepted")
	}
	if err := netEventBound(10); err != nil {
		t.Errorf("netEventBound(10) = %v", err)
	}
}

func TestPostureViewNamesTheDecisionAndWhatIsPending(t *testing.T) {
	rule := func(domain string) egress.Rule {
		return egress.Rule{To: egress.Destination{Domain: domain}, Protocol: "tls", Ports: []int{443}}
	}
	posture := box.NetworkPosture{Project: "/private/tmp/p", Mode: egress.Filtered, Source: box.PostureFromApproval,
		Approval:  &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{rule("example.com")}},
		Requested: []egress.Rule{rule("docs.example.com")},
		Add:       []egress.Rule{rule("docs.example.com")}, Remove: []egress.Rule{rule("example.com")},
		Pending: errors.New("network_approval_required: project request exceeds the approved envelope"),
		Setup:   &networkstate.Qualification{Contract: networkstate.QualificationContract, CompletedAt: time.Unix(0, 0).UTC()}}
	var b bytes.Buffer
	writeNetPosture(&b, ui.Palette{}, posture, []netRun{{ExecutionSummary: networkstate.ExecutionSummary{ID: "r1",
		StartedAt: time.Unix(0, 0).UTC()}, Outcome: "1 allowed, 5 refused"}})
	text := b.String()
	for _, want := range []string{"filtered (remembered for this project)", "+ docs.example.com tls/443",
		"- example.com tls/443", "a filtered run refuses until you run 'coop net approve'",
		"set up 1970-01-01T00:00:00Z", "r1", "no receipt yet", "1 allowed, 5 refused"} {
		if !strings.Contains(text, want) {
			t.Errorf("posture view is missing %q:\n%s", want, text)
		}
	}
}

// A launch can refuse for something the rule diff cannot show. The view says
// so in its own line, with the command that fixes it — otherwise a project
// whose directory was replaced reads as ready to go.
func TestPostureViewSaysWhenTheApprovedDirectoryWasReplaced(t *testing.T) {
	replaced := "the project directory at /private/tmp/p was replaced since it was approved — review it with 'coop net approve'"
	posture := box.NetworkPosture{Project: "/private/tmp/p", Mode: egress.Filtered, Source: box.PostureFromApproval,
		Approval: &networkstate.Approval{Posture: egress.Filtered}, Pending: errors.New(replaced),
		Setup: &networkstate.Qualification{Contract: networkstate.QualificationContract, CompletedAt: time.Unix(0, 0).UTC()}}
	var b bytes.Buffer
	writeNetPosture(&b, ui.Palette{}, posture, nil)
	for _, want := range []string{"Blocked", replaced} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("posture view is missing %q:\n%s", want, b.String())
		}
	}
	// A project with nothing pending gets no such line.
	var clean bytes.Buffer
	posture.Pending = nil
	writeNetPosture(&clean, ui.Palette{}, posture, nil)
	if strings.Contains(clean.String(), "Blocked") {
		t.Errorf("a project with nothing pending was reported blocked:\n%s", clean.String())
	}
}

// A host with no setup record must be told to run setup, and a record from
// another contract is a record, not a launch capability.
func TestPostureViewGuidesAnEmptyHostAndAStaleRecord(t *testing.T) {
	var fresh bytes.Buffer
	writeNetPosture(&fresh, ui.Palette{}, box.NetworkPosture{Project: "/p", Mode: egress.Open, Source: box.PostureFromDefault}, nil)
	for _, want := range []string{"nothing remembered for this project yet", "no box.egress_rules",
		"run 'coop net setup'", "Recent runs   none yet"} {
		if !strings.Contains(fresh.String(), want) {
			t.Errorf("empty-host view is missing %q:\n%s", want, fresh.String())
		}
	}
	var stale bytes.Buffer
	writeNetPosture(&stale, ui.Palette{}, box.NetworkPosture{Project: "/p", Mode: egress.Filtered, Source: box.PostureFromProject,
		Setup: &networkstate.Qualification{Contract: "some-older-contract"}}, nil)
	if !strings.Contains(stale.String(), "set up by an older coop — run 'coop net setup' again") {
		t.Errorf("stale-record view:\n%s", stale.String())
	}
}
