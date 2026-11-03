package networkstate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkview"
)

// observedFixture publishes one accepted observation and returns the retained
// execution plus the evidence reader every inspection test reads through.
func observedFixture(t *testing.T, mutate func(*networkview.Snapshot)) (*Store, *Evidence, Execution) {
	t.Helper()
	s, record := executionFixture(t)
	snapshot := record.Snapshot
	mutate(&snapshot)
	record, err := s.AcceptSnapshot(context.Background(), record.ID, record.Revision, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return s, fixtureEvidence(t, s), record
}

// inventory records every retained name with its size and modification time, so
// a read that publishes, truncates or reseals anything is visible.
type inspectionFileState struct {
	Mode     os.FileMode
	Modified time.Time
	Size     int64
	Digest   [32]byte
}

func inventory(t *testing.T, s *Store) map[string]inspectionFileState {
	t.Helper()
	entries, err := os.ReadDir(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]inspectionFileState{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		var digest [32]byte
		if info.Mode().IsRegular() {
			data, err := s.root.ReadFile(entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			digest = sha256.Sum256(data)
		}
		state[entry.Name()] = inspectionFileState{info.Mode(), info.ModTime(), info.Size(), digest}
	}
	return state
}

// A raw traffic row is counted per rule. An inspection that includes destinations names each rule
// from the run's own captured policy, so a person reads 10.42.8.0/24:5432 instead of a keyed ID.
// One that withholds destinations names nothing, the name never reaches JSON, and a policy copy
// that is gone leaves the rows unnamed instead of failing the read.
func TestInspectionNamesRawRulesFromTheRunsOwnPolicy(t *testing.T) {
	s := openStore(t)
	project := t.TempDir()
	mode := egress.Filtered
	raw := egress.Rule{To: egress.Destination{CIDR: "10.42.8.0/24"}, Protocol: "tcp", Ports: []int{5432}}
	policy, err := s.Admit(project, Admission{InvocationMode: &mode,
		Operator: []egress.Input{{Rules: []egress.Rule{raw}, Origin: egress.Origin{Kind: "operator", Name: "rules-file"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.Grants) != 1 {
		t.Fatalf("the fixture policy has %d grants, want the one raw rule", len(policy.Grants))
	}
	grant := policy.Grants[0]
	ctx := context.Background()
	record, err := executionSmoke(t, s).CreateExecution(ctx, ExecutionSpec{Project: project, PolicyFingerprint: policy.Fingerprint,
		Runtime: "docker", DaemonID: "fixture-daemon", Endpoint: "unix:///fixture.sock", GatewayImage: "sha256:" + strings.Repeat("a", 64), ClientImage: "sha256:" + strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := record.Snapshot
	snapshot.Sequence, snapshot.Availability = 1, "available"
	snapshot.AddressGrants = []networkview.AddressGrantObservation{{RuleID: grant.ID, Packets: 3, Bytes: 180}}
	if record, err = s.AcceptSnapshot(ctx, record.ID, record.Revision, snapshot); err != nil {
		t.Fatal(err)
	}
	evidence := fixtureEvidence(t, s)
	named, err := evidence.Inspect(record.ID, time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := named.AddressRules[grant.ID]; !ok || !reflect.DeepEqual(got, grant.Rule) {
		t.Fatalf("the raw rule was not named from the run's policy: %+v", named.AddressRules)
	}
	if data, err := json.Marshal(named); err != nil || strings.Contains(string(data), "10.42.8.0") {
		t.Fatalf("the rule's destination reached JSON: %v\n%s", err, data)
	}
	if withheld, err := evidence.Inspect(record.ID, time.Now(), false); err != nil || withheld.AddressRules != nil {
		t.Fatalf("a view that withholds destinations named rules: %+v, %v", withheld.AddressRules, err)
	}
	if err := os.Remove(filepath.Join(s.Path(), "snapshot-"+policy.Fingerprint+".json")); err != nil {
		t.Fatal(err)
	}
	if gone, err := evidence.Inspect(record.ID, time.Now(), true); err != nil || gone.AddressRules != nil {
		t.Fatalf("a run whose policy copy is gone = %+v, %v; want the rows unnamed and the read intact", gone.AddressRules, err)
	}
}

func TestInspectionAgesObservationsWithoutInflatingRetainedEvidence(t *testing.T) {
	base := time.Now().UTC()
	s, evidence, record := observedFixture(t, func(snapshot *networkview.Snapshot) {
		snapshot.Sequence, snapshot.Availability, snapshot.AsOf = 1, "available", base
		snapshot.Counters = &networkview.Counters{SentBytes: networkview.Value(1<<53 + 7)}
		snapshot.LiveConnections, snapshot.PendingConnections = networkview.Value(2), 3
		snapshot.Rate = &networkview.Rate{SentPerSecond: 1.5, WindowMillis: 1000}
	})
	fresh, err := evidence.Inspect(record.ID, base, true)
	if err != nil || fresh.Freshness != FreshnessFresh || fresh.Current == nil {
		t.Fatal("recent observation was not reported as current", err)
	}
	if *fresh.Current.LiveConnections != 2 || *fresh.Current.PendingConnections != 3 || fresh.Current.Rate.SentPerSecond != 1.5 ||
		fresh.Current.UnknownConnections != nil || fresh.Current.KernelClosingSockets != nil {
		t.Fatal("current measurements were invented or dropped")
	}
	stale, err := evidence.Inspect(record.ID, base.Add(4*time.Second), true)
	if err != nil || stale.Freshness != FreshnessStale || stale.Current != nil {
		t.Fatal("aged observation still claimed current metrics", err)
	}
	if *stale.Observed.Counters.SentBytes != 1<<53+7 {
		t.Fatal("aging changed retained cumulative evidence")
	}
	again, err := evidence.Inspect(record.ID, base.Add(time.Second), true)
	if err != nil || again.Freshness != FreshnessFresh || !equalJSON(again.Observed, fresh.Observed) || !equalJSON(again.Current, fresh.Current) {
		t.Fatal("fresh/stale/fresh reading inflated or altered evidence", err)
	}
	boundary, err := evidence.Inspect(record.ID, base.Add(ObservationMaxAge), true)
	if err != nil || boundary.Freshness != FreshnessFresh {
		t.Fatal("exact age boundary was discarded", err)
	}
	past, err := evidence.Inspect(record.ID, base.Add(ObservationMaxAge+time.Nanosecond), true)
	if err != nil || past.Freshness != FreshnessStale || past.Current != nil {
		t.Fatal("observation older than the bound stayed current", err)
	}
	future, err := evidence.Inspect(record.ID, base.Add(-time.Nanosecond), true)
	if err != nil || future.Freshness != FreshnessStale || future.Current != nil {
		t.Fatal("observation from the future was trusted as current", err)
	}
	retained, err := s.Execution(record.ID)
	if err != nil || !equalJSON(retained, record) {
		t.Fatal("inspection changed retained custody", err)
	}
}

func TestInspectionWithoutObservationsOrMetricsClaimsNothing(t *testing.T) {
	s, record := executionFixture(t)
	evidence := fixtureEvidence(t, s)
	unobserved, err := evidence.Inspect(record.ID, time.Now().UTC(), true)
	if err != nil || unobserved.Freshness != FreshnessNotObserved || unobserved.Current != nil || unobserved.Cleanup != "pending" {
		t.Fatal("an execution with no observation reported a measurement", err)
	}
	base := time.Now().UTC()
	snapshot := record.Snapshot
	snapshot.Sequence, snapshot.Availability, snapshot.AsOf = 1, "available", base
	if _, err := s.AcceptSnapshot(context.Background(), record.ID, record.Revision, snapshot); err != nil {
		t.Fatal(err)
	}
	empty, err := evidence.Inspect(record.ID, base, true)
	if err != nil || empty.Freshness != FreshnessFresh || empty.Current == nil {
		t.Fatal("fresh observation lost its current view", err)
	}
	if empty.Current.Rate != nil || empty.Current.LiveConnections != nil || empty.Current.UnknownConnections != nil || empty.Current.KernelClosingSockets != nil {
		t.Fatal("missing metrics were inferred as measured values")
	}
	data, err := json.Marshal(empty.Current)
	if err != nil || !strings.Contains(string(data), `"live_connections":null`) {
		t.Fatal("an unknown metric did not encode as null", err, string(data))
	}
	if _, err := evidence.Inspect(record.ID, time.Time{}, true); err == nil {
		t.Fatal("inspection accepted a zero read time")
	}
	if _, err := evidence.Inspect(strings.Repeat("a", 32), time.Now().UTC(), true); err == nil {
		t.Fatal("inspection invented evidence for an unknown execution")
	}
}

func TestInspectionRedactsPrivateEvidenceAndSharesNoMutableState(t *testing.T) {
	base := time.Now().UTC()
	s, evidence, record := observedFixture(t, func(snapshot *networkview.Snapshot) {
		snapshot.Sequence, snapshot.Availability, snapshot.AsOf = 1, "available", base
		snapshot.Counters = &networkview.Counters{SentBytes: networkview.Value(1<<53 + 7)}
		snapshot.LiveConnections = networkview.Value(1)
		snapshot.Connections = []networkview.Connection{{ID: "c1", DestinationID: "d1", State: "open", Transport: "tcp",
			Name: "private.example", Peer: "203.0.113.7:443", RuleID: "rule-private", ObservedAt: base, SentBytes: networkview.Value(9)}}
		snapshot.Denials = []networkview.Denial{{ID: "n1", Source: "guard", Sequence: 1, Basis: "policy", At: base, Kind: "tls",
			Reason: "not-allowed", Name: "blocked.example", Peer: "203.0.113.9:443",
			Candidate: &networkview.Candidate{ID: "cand-private", EvidenceID: "ev-private", PolicyFingerprint: strings.Repeat("a", 64),
				AppliesTo: "project", Rule: egress.Rule{To: egress.Destination{Domain: "blocked.example"}}}}}
	})
	sealed, err := s.SealExecution(context.Background(), record.ID, record.Revision, "exited")
	if err != nil {
		t.Fatal(err)
	}
	withheld, err := evidence.Inspect(record.ID, base, false)
	if err != nil || withheld.Observed.Projection != "destinations-withheld" || withheld.Receipt == nil {
		t.Fatal("redacted projection was not produced", err)
	}
	if withheld.Observed.Connections[0].Name != "" || withheld.Observed.Connections[0].Peer != "" || withheld.Observed.Connections[0].RuleID != "" ||
		withheld.Observed.Denials[0].Name != "" || withheld.Observed.Denials[0].Candidate != nil || !withheld.Observed.Denials[0].Withheld {
		t.Fatal("private destinations survived redaction")
	}
	encoded, err := json.Marshal(withheld)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private.example", "blocked.example", "203.0.113.7", "cand-private", "rule-private",
		sealed.Receipt.Digest, record.Project, record.Endpoint, record.DaemonID, record.Supervisor.StartToken,
		record.Artifact.Name, record.Resources[0].Name} {
		if secret != "" && strings.Contains(string(encoded), secret) {
			t.Fatalf("inspection leaked private evidence: %s", secret)
		}
	}
	if !strings.Contains(string(encoded), `"sent_bytes":"9007199254740999"`) {
		t.Fatal("a counter above 2^53 was not encoded as a decimal string", string(encoded))
	}
	shared, err := evidence.Inspect(record.ID, base, true)
	if err != nil || shared.Observed.Connections[0].Name != "private.example" || shared.Observed.Denials[0].Candidate == nil {
		t.Fatal("explicit destination export lost evidence", err)
	}
	*shared.Observed.Counters.SentBytes = 1
	shared.Observed.Connections[0].Name = "mutated.example"
	shared.Receipt.Snapshot.Loss.Reasons = append(shared.Receipt.Snapshot.Loss.Reasons, "mutated")
	after, err := evidence.Inspect(record.ID, base, true)
	if err != nil || *after.Observed.Counters.SentBytes != 1<<53+7 || after.Observed.Connections[0].Name != "private.example" ||
		len(after.Receipt.Snapshot.Loss.Reasons) != len(sealed.Receipt.Snapshot.Loss.Reasons) {
		t.Fatal("a reader mutated shared retained state", err)
	}
	retained, err := s.Execution(record.ID)
	if err != nil || !equalJSON(retained, sealed) {
		t.Fatal("inspection rewrote retained evidence", err)
	}
}

func TestInspectionOfTerminalOrSealedEvidenceMakesNoLivenessClaim(t *testing.T) {
	base := time.Now().UTC()
	s, evidence, record := observedFixture(t, func(snapshot *networkview.Snapshot) {
		snapshot.Sequence, snapshot.Availability, snapshot.AsOf, snapshot.Terminal = 1, "available", base, true
		snapshot.LiveConnections = networkview.Value(4)
	})
	terminal, err := evidence.Inspect(record.ID, base.Add(time.Hour), true)
	if err != nil || terminal.Freshness != FreshnessTerminal || terminal.Current != nil {
		t.Fatal("a terminal epoch aged into a stale heartbeat claim", err)
	}
	fresh, err := evidence.Inspect(record.ID, base, true)
	if err != nil || fresh.Freshness != FreshnessTerminal || fresh.Current != nil {
		t.Fatal("a terminal epoch claimed current measurements", err)
	}
	if _, err := s.SealExecution(context.Background(), record.ID, record.Revision, "exited"); err != nil {
		t.Fatal(err)
	}
	sealed, err := evidence.Inspect(record.ID, base, true)
	if err != nil || sealed.Freshness != FreshnessTerminal || sealed.Current != nil || sealed.Receipt == nil {
		t.Fatal("sealed evidence lost its receipt or claimed liveness", err)
	}
	// A receipt over a NONTERMINAL observation ends aging just the same.
	other, interrupted := executionFixture(t)
	if _, err := other.SealExecution(context.Background(), interrupted.ID, interrupted.Revision, "launch_failed"); err != nil {
		t.Fatal(err)
	}
	final, err := fixtureEvidence(t, other).Inspect(interrupted.ID, time.Now().UTC(), true)
	if err != nil || final.Freshness != FreshnessTerminal || final.Current != nil || final.Observed.Terminal {
		t.Fatal("a sealed nonterminal observation kept aging", err)
	}
}

func TestInspectionCleanupAfterFinalLeavesTheSealedReceiptUnchanged(t *testing.T) {
	s, record := executionFixture(t)
	evidence := fixtureEvidence(t, s)
	ctx := context.Background()
	record, err := s.SealExecution(ctx, record.ID, record.Revision, "launch_failed")
	if err != nil || record.Receipt.Cleanup != "pending" {
		t.Fatal("seal without confirmed absence claimed cleanup", err)
	}
	sealed := *record.Receipt
	now := time.Now().UTC()
	pending, err := evidence.Inspect(record.ID, now, true)
	if err != nil || pending.Cleanup != "pending" {
		t.Fatal("uncleaned resources reported complete cleanup", err)
	}
	for _, resource := range record.Resources {
		record, err = evidence.ConfirmResourceGone(ctx, record.ID, record.Revision, record.DaemonID, resource.Role, resource.Name)
		if err != nil {
			t.Fatal(err)
		}
	}
	if record, err = evidence.CleanupArtifacts(ctx, record.ID, record.Revision); err != nil {
		t.Fatal(err)
	}
	complete, err := evidence.Inspect(record.ID, now, true)
	if err != nil || complete.Cleanup != "complete" {
		t.Fatal("confirmed absence did not reach the inspection wrapper", err)
	}
	if complete.Receipt.Cleanup != "pending" || !reflect.DeepEqual(*record.Receipt, sealed) {
		t.Fatal("cleanup rewrote the sealed receipt")
	}
	if complete.Receipt.Digest != pending.Receipt.Digest || complete.Receipt.Workload != sealed.Workload || complete.Receipt.Completeness != sealed.Completeness {
		t.Fatal("projected receipt outcome or digest moved with cleanup")
	}
}

func TestInspectionReadsEvidenceAfterKeyAndProjectLossWithoutWriting(t *testing.T) {
	base := time.Now().UTC()
	s, _, record := observedFixture(t, func(snapshot *networkview.Snapshot) {
		snapshot.Sequence, snapshot.Availability, snapshot.AsOf = 1, "available", base
		snapshot.LiveConnections = networkview.Value(5)
	})
	if err := s.root.Remove("owner.key"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(record.Project); err != nil {
		t.Fatal(err)
	}
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Close()
	before := inventory(t, s)
	got, err := evidence.Inspect(record.ID, base, false)
	if err != nil || got.Freshness != FreshnessFresh || *got.Current.LiveConnections != 5 || got.Revision != record.Revision {
		t.Fatal("key/project loss hid readable evidence", err)
	}
	if !reflect.DeepEqual(before, inventory(t, s)) {
		t.Fatal("inspection wrote to the evidence root")
	}
}
