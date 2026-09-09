package networkview

import (
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

func TestAggregateSessionNetworkRevisionsAndOrderedReplay(t *testing.T) {
	identity := sessionIdentity(true, true)
	first := sessionObservation(identity, "run-a", "epoch-a", 1, 1, "provisional", "complete")
	first.Receipt.Snapshot.Counters.SentBytes = Value(7)
	latest := sessionObservation(identity, "run-a", "epoch-a", 2, 2, "final", "complete")
	latest.Receipt.Snapshot.Counters.SentBytes = Value(11)

	got := mustAggregateSession(t, identity, []RunObservation{latest, first}, false)
	if got.RunCount != 1 || countValue(t, got.Counters.SentBytes) != 11 {
		t.Fatalf("cumulative revisions were summed instead of replaced: %+v", got)
	}
	if len(got.Runs) != 1 || got.Runs[0].Sequence != 2 {
		t.Fatalf("latest revision was not retained: %+v", got.Runs)
	}
	if got := mustAggregateSession(t, identity, []RunObservation{latest, first, first}, false); got.RunCount != 1 {
		t.Fatalf("duplicate replay was not coalesced: %+v", got)
	}
	conflictingOld := sessionObservation(identity, "run-a", "epoch-a", 1, 1, "provisional", "complete")
	conflictingOld.Receipt.Snapshot.Counters.SentBytes = Value(2)
	if _, err := AggregateSessionNetwork(identity, orderedSessionObservations([]RunObservation{latest, first, conflictingOld}), false); err == nil {
		t.Fatal("accepted conflicting stale equal revisions")
	}
	if _, err := AggregateSessionNetwork(identity, rawSessionObservations(latest, first), false); err == nil {
		t.Fatal("accepted an unordered source stream")
	}
	if _, err := AggregateSessionNetwork(identity, func(yield func(RunObservation, error) bool) {
		yield(RunObservation{}, errors.New("owner store read failed"))
	}, false); err == nil {
		t.Fatal("accepted an iterator error")
	}
}

func TestAggregateSessionNetworkRevisionTransitions(t *testing.T) {
	identity := sessionIdentity(true, true)
	provisional := sessionObservation(identity, "run-a", "epoch-a", 1, 4, "provisional", "complete")
	changedProvisional := sessionObservation(identity, "run-a", "epoch-a", 2, 4, "provisional", "complete")
	changedProvisional.Receipt.Snapshot.Counters.SentBytes = Value(9)
	if _, err := AggregateSessionNetwork(identity, orderedSessionObservations([]RunObservation{provisional, changedProvisional}), false); err == nil {
		t.Fatal("accepted a changed provisional sample at the same sequence")
	}
	final := sessionObservation(identity, "run-a", "epoch-a", 2, 4, "final", "partial")
	final.Receipt.Snapshot.Loss.Unknown = true
	if _, err := AggregateSessionNetwork(identity, orderedSessionObservations([]RunObservation{provisional, final}), false); err != nil {
		t.Fatalf("rejected final loss metadata for the same producer sample: %v", err)
	}
	changedFinal := sessionObservation(identity, "run-a", "epoch-a", 2, 4, "final", "partial")
	changedFinal.Receipt.Snapshot.Counters.SentBytes = Value(9)
	if _, err := AggregateSessionNetwork(identity, orderedSessionObservations([]RunObservation{provisional, changedFinal}), false); err == nil {
		t.Fatal("finalization rewrote cumulative counters at the same sample sequence")
	}
	changedAfterFinal := sessionObservation(identity, "run-a", "epoch-a", 3, 5, "final", "partial")
	changedAfterFinal.Receipt.Snapshot.Counters.SentBytes = Value(12)
	if _, err := AggregateSessionNetwork(identity, orderedSessionObservations([]RunObservation{final, changedAfterFinal}), false); err == nil {
		t.Fatal("accepted a changed observation after final")
	}
}

func TestAggregateSessionNetworkDoesNotInferCausalityFromWallClocks(t *testing.T) {
	identity := sessionIdentity(true, true)
	closedAt := identity.StartedAt.Add(-time.Hour)
	identity.ClosedAt = &closedAt
	observation := sessionObservation(identity, "run-a", "epoch-a", 1, 1, "final", "complete")
	observation.Receipt.StartedAt = identity.StartedAt.Add(-2 * time.Hour)
	endedAt := observation.Receipt.StartedAt.Add(time.Minute)
	observation.Receipt.EndedAt = &endedAt
	got := mustAggregateSession(t, identity, []RunObservation{observation}, false)
	if got.Finality != "final" || !got.ClosedAt.Equal(closedAt) {
		t.Fatalf("wall-clock step rejected owned final evidence: %+v", got)
	}
}

func TestAggregateSessionNetworkSealedReferencesIgnoreCleanupRevision(t *testing.T) {
	identity := sessionIdentity(true, true)
	final := sessionObservation(identity, "run-a", "epoch-a", 7, 4, "final", "partial")
	before := mustAggregateSession(t, identity, []RunObservation{final}, false)
	// Exact-owned cleanup may advance execution custody without amending evidence.
	final.Revision = 12
	after := mustAggregateSession(t, identity, []RunObservation{final}, false)
	if before.Digest != after.Digest || before.Runs[0] != after.Runs[0] {
		t.Fatalf("cleanup changed sealed aggregate: before=%+v, after=%+v", before, after)
	}
}

func TestAggregateSessionNetworkRejectsIdentityMismatches(t *testing.T) {
	identity := sessionIdentity(true, true)
	for name, change := range map[string]func(*RunObservation){
		"session":      func(o *RunObservation) { o.Receipt.SessionID = "other-session" },
		"authority":    func(o *RunObservation) { o.Receipt.AuthorityDigest = "other-authority" },
		"policy":       func(o *RunObservation) { o.Receipt.Snapshot.PolicyFingerprint = "other-policy" },
		"epoch":        func(o *RunObservation) { o.Receipt.Snapshot.Epoch = "" },
		"run identity": func(o *RunObservation) { o.Receipt.Snapshot.RunID = "other-run" },
	} {
		t.Run(name, func(t *testing.T) {
			observation := sessionObservation(identity, "run-a", "epoch-a", 1, 1, "final", "complete")
			change(&observation)
			if _, err := AggregateSessionNetwork(identity, orderedSessionObservations([]RunObservation{observation}), false); err == nil {
				t.Fatal("accepted an observation outside captured session identity")
			}
		})
	}
}

func TestAggregateSessionNetworkMissingCountersAndCoverage(t *testing.T) {
	identity := sessionIdentity(true, true)
	allMissing := sessionObservation(identity, "run-a", "epoch-a", 1, 1, "final", "complete")
	allMissing.Receipt.Snapshot.Counters = &Counters{}
	got := mustAggregateSession(t, identity, []RunObservation{allMissing}, false)
	if got.Counters == nil {
		t.Fatal("present all-null counters were discarded")
	}
	for _, value := range []*Count{
		got.Counters.SentBytes, got.Counters.ReceivedBytes, got.Counters.Connections,
		got.Counters.UpstreamFailures, got.Counters.DeniedPackets, got.Counters.ProtectedPackets,
		got.Counters.DeniedDNSQueries, got.Counters.DeniedTLS, got.Counters.MaintenanceQueries,
		got.Counters.MaintenanceFailures, got.Counters.IngressDenials,
		got.Counters.MaintenanceSentBytes, got.Counters.MaintenanceReceivedBytes,
	} {
		if value != nil {
			t.Fatal("missing counter became measured zero")
		}
	}
	nilCounters := sessionObservation(identity, "run-a", "epoch-a", 1, 1, "final", "complete")
	nilCounters.Receipt.Snapshot.Counters = nil
	if got := mustAggregateSession(t, identity, []RunObservation{nilCounters}, false); got.Counters != nil {
		t.Fatalf("nil counters became a fabricated counter set: %+v", got.Counters)
	}
	partial := sessionObservation(identity, "run-a", "epoch-a", 1, 1, "final", "complete")
	partial.Receipt.Snapshot.Counters.ReceivedBytes = nil
	got = mustAggregateSession(t, identity, []RunObservation{partial}, false)
	if got.Coverage.ProxyBytes.Status != "lower-bound" || got.Coverage.Connections.Status != "exact" || got.Counters.ReceivedBytes != nil {
		t.Fatalf("one missing source did not preserve independent coverage: %+v", got)
	}
}

func TestAggregateSessionNetworkOverflowDoesNotStopOtherCounters(t *testing.T) {
	identity := sessionIdentity(true, true)
	first := sessionObservation(identity, "run-a", "epoch-a", 1, 1, "final", "complete")
	first.Receipt.Snapshot.Counters.SentBytes = Value(^uint64(0))
	first.Receipt.Snapshot.Counters.Connections = Value(3)
	second := sessionObservation(identity, "run-b", "epoch-b", 1, 1, "final", "complete")
	second.Receipt.Snapshot.Counters.SentBytes = Value(1)
	second.Receipt.Snapshot.Counters.Connections = Value(4)
	got := mustAggregateSession(t, identity, []RunObservation{first, second}, false)
	if countValue(t, got.Counters.SentBytes) != ^uint64(0) || got.Coverage.ProxyBytes.Status != "lower-bound" ||
		!got.Loss.Unknown || !slices.Contains(got.Loss.Reasons, "counter_overflow") {
		t.Fatalf("overflow claimed exact proxy bytes: %+v", got)
	}
	if countValue(t, got.Counters.Connections) != 7 || got.Coverage.Connections.Status != "exact" {
		t.Fatalf("independent counter aggregation stopped after overflow: %+v", got)
	}
}

func TestAggregateSessionNetworkLossAccounting(t *testing.T) {
	identity := sessionIdentity(true, true)
	measured := sessionObservation(identity, "run-a", "epoch-a", 1, 1, "final", "complete")
	measured.Receipt.Snapshot.Loss = Loss{Records: 1, OmittedDetails: Value(0)}
	if got := mustAggregateSession(t, identity, []RunObservation{measured}, false); got.Loss.Unknown || got.Completeness != "partial" {
		t.Fatalf("measured loss became unknown rather than partial: %+v", got)
	}
	truncated := sessionObservation(identity, "run-a", "epoch-a", 1, 1, "final", "partial")
	truncated.Receipt.Snapshot.Loss = Loss{DetailTruncated: true}
	known := sessionObservation(identity, "run-b", "epoch-b", 1, 1, "final", "partial")
	known.Receipt.Snapshot.Loss = Loss{OmittedDetails: Value(3)}
	if got := mustAggregateSession(t, identity, []RunObservation{truncated, known}, false); got.Loss.OmittedDetails != nil || !got.Loss.DetailTruncated {
		t.Fatalf("known count concealed a truncated unknown detail count: %+v", got.Loss)
	}
	maximum := sessionObservation(identity, "run-a", "epoch-a", 1, 1, "final", "partial")
	maximum.Receipt.Snapshot.Loss = Loss{OmittedDetails: Value(^uint64(0))}
	overflow := sessionObservation(identity, "run-b", "epoch-b", 1, 1, "final", "partial")
	overflow.Receipt.Snapshot.Loss = Loss{OmittedDetails: Value(1)}
	if got := mustAggregateSession(t, identity, []RunObservation{maximum, overflow}, false); got.Loss.OmittedDetails != nil || !got.Loss.Unknown {
		t.Fatalf("overflowed omitted-detail count remained exact: %+v", got.Loss)
	}
	recordOverflow := sessionObservation(identity, "run-a", "epoch-a", 1, 1, "final", "partial")
	recordOverflow.Receipt.Snapshot.Loss = Loss{Records: ^Count(0), SuppressedAlerts: 3}
	alerts := sessionObservation(identity, "run-b", "epoch-b", 1, 1, "final", "partial")
	alerts.Receipt.Snapshot.Loss = Loss{Records: 1, SuppressedAlerts: 4}
	got := mustAggregateSession(t, identity, []RunObservation{recordOverflow, alerts}, false)
	if got.Loss.Records != ^Count(0) || got.Loss.SuppressedAlerts != 7 || !got.Loss.Unknown {
		t.Fatalf("loss overflow stopped later independent totals: %+v", got.Loss)
	}
}

func TestAggregateSessionNetworkCompletenessAndScope(t *testing.T) {
	for _, test := range []struct {
		name                         string
		closed, completeRunSet       bool
		runFinality, runCompleteness string
		wantFinality, wantComplete   string
	}{
		{"active with final run", false, true, "final", "complete", "provisional", "complete"},
		{"incomplete run roster", true, false, "final", "complete", "provisional", "partial"},
		{"provisional run", true, true, "provisional", "complete", "provisional", "complete"},
		{"final partial", true, true, "final", "partial", "final", "partial"},
		{"final complete", true, true, "final", "complete", "final", "complete"},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := sessionIdentity(test.closed, test.completeRunSet)
			observation := sessionObservation(identity, "run-a", "epoch-a", 1, 1, test.runFinality, test.runCompleteness)
			got := mustAggregateSession(t, identity, []RunObservation{observation}, false)
			if got.Finality != test.wantFinality || got.Completeness != test.wantComplete || got.Scope != "proxy-streams-and-sampled-tcp-sockets" {
				t.Fatalf("unexpected session state: %+v", got)
			}
		})
	}
	identity := sessionIdentity(true, true)
	notObserved := sessionObservation(identity, "run-a", "epoch-a", 1, 0, "final", "complete")
	if got := mustAggregateSession(t, identity, []RunObservation{notObserved}, false); got.Completeness != "unknown" || got.Scope != "not-observed" {
		t.Fatalf("absence of an observed source was overstated: %+v", got)
	}
	sampled := sessionObservation(identity, "run-a", "epoch-a", 1, 1, "final", "complete")
	unavailable := sessionObservation(identity, "run-b", "epoch-b", 1, 1, "final", "complete")
	unavailable.Receipt.Snapshot.Scope = "not-observed"
	if got := mustAggregateSession(t, identity, []RunObservation{sampled, unavailable}, false); got.Scope != "mixed" {
		t.Fatalf("mixed collector scopes were hidden: %+v", got)
	}
}

func TestAggregateSessionNetworkProjectsReferencesWithoutMutatingInputs(t *testing.T) {
	identity := sessionIdentity(true, true)
	privateName, privatePeer := "private.example.internal", "10.42.19.12:443"
	observation := sessionObservation(identity, "run-a", "epoch-a", 1, 1, "final", "partial")
	observation.Receipt.Digest = "owner-local-private-digest"
	observation.Receipt.Snapshot.Connections = []Connection{{ID: "flow", Name: privateName, Peer: privatePeer, RuleID: "private-rule"}}
	observation.Receipt.Snapshot.Denials = []Denial{{ID: "denial", Name: privateName, Peer: privatePeer,
		Candidate: &Candidate{ID: "private-candidate", Rule: egress.Rule{To: egress.Destination{Domain: privateName}}}}}
	before, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	withheld, err := observation.Receipt.Project(false)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := observation.Receipt.Project(true)
	if err != nil {
		t.Fatal(err)
	}
	got := mustAggregateSession(t, identity, []RunObservation{observation}, false)
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{privateName, privatePeer, "private-rule", "private-candidate", observation.Receipt.Digest} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("withheld projection leaked %q: %s", secret, data)
		}
	}
	if len(got.Runs) != 1 || got.Runs[0].ReceiptDigest != withheld.Digest || got.Runs[0].ReceiptDigest == exported.Digest {
		t.Fatalf("run reference did not contain exactly the withheld projection digest: %+v", got.Runs)
	}
	exportedSession := mustAggregateSession(t, identity, []RunObservation{observation}, true)
	if exportedSession.Runs[0].ReceiptDigest != exported.Digest || exportedSession.Runs[0].ReceiptDigest == got.Runs[0].ReceiptDigest {
		t.Fatalf("destination export did not use its distinct projected digest: %+v", exportedSession.Runs)
	}
	after, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("aggregation mutated owner-local input")
	}
}

func TestAggregateSessionNetworkCountsBeyondReferenceLimit(t *testing.T) {
	identity := sessionIdentity(true, true)
	const total = MaxSessionRunReferences + 1
	observations := make([]RunObservation, 0, total)
	for i := 0; i < total; i++ {
		observations = append(observations, sessionObservation(identity, fmt.Sprintf("run-%05d", i), "epoch-a", 1, 1, "final", "complete"))
	}
	got := mustAggregateSession(t, identity, observations, false)
	if got.RunCount != Count(total) || len(got.Runs) != MaxSessionRunReferences || got.OmittedReferences != 1 || countValue(t, got.Counters.SentBytes) != uint64(total) {
		t.Fatalf("reference bound became a session-attempt bound: %+v", got)
	}
}

func mustAggregateSession(t *testing.T, identity SessionNetworkIdentity, observations []RunObservation, export bool) SessionReceipt {
	t.Helper()
	got, err := AggregateSessionNetwork(identity, orderedSessionObservations(observations), export)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func orderedSessionObservations(observations []RunObservation) iter.Seq2[RunObservation, error] {
	ordered := append([]RunObservation(nil), observations...)
	slices.SortFunc(ordered, func(a, b RunObservation) int {
		if a.Receipt.ID != b.Receipt.ID {
			if a.Receipt.ID < b.Receipt.ID {
				return -1
			}
			return 1
		}
		if a.Receipt.Snapshot.Epoch != b.Receipt.Snapshot.Epoch {
			if a.Receipt.Snapshot.Epoch < b.Receipt.Snapshot.Epoch {
				return -1
			}
			return 1
		}
		if a.Revision < b.Revision {
			return -1
		}
		if a.Revision > b.Revision {
			return 1
		}
		return 0
	})
	return rawSessionObservations(ordered...)
}

func rawSessionObservations(observations ...RunObservation) iter.Seq2[RunObservation, error] {
	return func(yield func(RunObservation, error) bool) {
		for _, observation := range observations {
			if !yield(observation, nil) {
				return
			}
		}
	}
}

func sessionIdentity(closed, runsComplete bool) SessionNetworkIdentity {
	started := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	identity := SessionNetworkIdentity{ID: "session-a", PolicyFingerprint: "policy-a", AuthorityDigest: "authority-a",
		Mode: egress.Filtered, StartedAt: started, RunsComplete: runsComplete}
	if closed {
		closedAt := started.Add(time.Hour)
		identity.ClosedAt = &closedAt
	}
	return identity
}

func sessionObservation(identity SessionNetworkIdentity, runID, epoch string, revision, sequence Count, finality, completeness string) RunObservation {
	asOf := identity.StartedAt.Add(time.Duration(sequence+1) * time.Second)
	exact := MetricCoverage{Status: "exact"}
	snapshot := Snapshot{
		Version: Version, RunID: runID, Epoch: epoch, PolicyFingerprint: identity.PolicyFingerprint,
		Mode: identity.Mode, Sequence: sequence, Terminal: finality == "final", AsOf: asOf,
		Availability: "available", Scope: "proxy-streams-and-sampled-tcp-sockets",
		Health: HealthLayers{Enforcer: Health{Status: "ready"}, Gateway: Health{Status: "stopped"}, Resolver: Health{Status: "ready"}, Collector: Health{Status: "ready"}},
		Coverage: Coverage{ProxyBytes: exact, Connections: exact, UpstreamFailures: exact, KernelPackets: exact,
			GuardDenials: exact, MaintenanceQueries: exact, MaintenanceBytes: exact, SocketInventory: exact, BoundaryAttribution: exact},
		Counters: &Counters{SentBytes: Value(1), ReceivedBytes: Value(1), Connections: Value(1), UpstreamFailures: Value(1),
			DeniedPackets: Value(1), ProtectedPackets: Value(1), DeniedDNSQueries: Value(1), DeniedTLS: Value(1),
			MaintenanceQueries: Value(1), MaintenanceFailures: Value(1), IngressDenials: Value(1), MaintenanceSentBytes: Value(1), MaintenanceReceivedBytes: Value(1)},
		Loss: Loss{OmittedDetails: Value(0)}, Projection: "owner-local",
	}
	receipt := Receipt{Version: Version, ID: runID, Snapshot: snapshot, StartedAt: identity.StartedAt,
		Finality: finality, Completeness: completeness, SessionID: identity.ID, AuthorityDigest: identity.AuthorityDigest, DigestScope: "owner-local"}
	if finality == "final" {
		endedAt := asOf.Add(time.Second)
		receipt.EndedAt = &endedAt
	}
	return RunObservation{Revision: revision, Receipt: receipt}
}

func countValue(t *testing.T, value *Count) uint64 {
	t.Helper()
	if value == nil {
		t.Fatal("counter is unexpectedly missing")
	}
	return uint64(*value)
}
