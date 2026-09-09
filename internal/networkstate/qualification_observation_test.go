package networkstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkview"
)

func addQualificationHistory(s *networkview.Snapshot) {
	s.Availability = "degraded"
	s.Health.Collector = networkview.Health{Status: "degraded", Reason: "observation_gap"}
	s.Coverage.BoundaryAttribution = networkview.MetricCoverage{Status: "lower-bound", Reason: "unattributed_socket"}
	s.Loss.Unknown, s.Loss.Reasons = true, []string{"unattributed_socket"}
	s.Connections = append(s.Connections, networkview.Connection{ID: "fixture-history", DestinationID: "fixture-destination", ObservedAt: s.AsOf,
		State: "unknown", Transport: "tcp", NameSource: "unattributed-history", Reason: "socket_join_expired", Partial: true})
}

func TestQualificationUnobservedInitFailureRetainsMissingMeasurements(t *testing.T) {
	trial, _, proofs := qualificationFixture(t, nil)
	q, err := trial.Complete(nil, proofs)
	if err != nil {
		t.Fatal(err)
	}
	for _, proof := range q.Proofs {
		if proof.Case != "init-failure" {
			continue
		}
		r, err := trial.store.Execution(proof.RunID)
		if err != nil {
			t.Fatal(err)
		}
		s := r.Receipt.Snapshot
		if s.Scope != "not-observed" || s.Sequence != 0 || s.Counters != nil || s.Coverage != (networkview.Coverage{}) {
			t.Fatal("unobserved startup failure invented measurements", s)
		}
		if proof.Observation.Scope != s.Scope || proof.Observation.Coverage != s.Coverage {
			t.Fatal("qualification changed the sealed unobserved frame")
		}
		observed := *proof.Observation
		observed.Scope = "proxy-streams-and-sampled-tcp-sockets"
		if err := validateQualificationObservation(&observed); err == nil {
			t.Fatal("observed frame accepted empty coverage")
		}
		if err := validateFunctionalQualification(r); err == nil {
			t.Fatal("missing observations qualified a functional case")
		}
		return
	}
	t.Fatal("qualification omitted init-failure")
}

func TestQualificationFunctionalSampledHistoryStaysPartial(t *testing.T) {
	trial := executionTrial(t, openStore(t))
	spec := qualificationExecutionSpec(t, trial)
	proof := qualificationProofFixture(t, trial, spec, "short-flow", nil, addQualificationHistory)
	r, err := trial.store.Execution(proof.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateQualificationOutcome(r); err != nil {
		t.Fatal("documented sampled history gap rejected", err)
	}
	if r.Receipt.Completeness != "partial" || r.Receipt.Snapshot.Coverage.BoundaryAttribution.Status != "lower-bound" {
		t.Fatal("partial attribution relabeled complete")
	}
	observation, err := qualificationObservation(r.Receipt)
	if err != nil || !slices.Equal(observation.HistoryReasons, []string{"socket_join_expired"}) {
		t.Fatal("history reasons not retained", err)
	}
	observation.Loss.Reasons[0] = "changed"
	*observation.Loss.OmittedDetails = 99
	if r.Receipt.Snapshot.Loss.Reasons[0] != "unattributed_socket" || *r.Receipt.Snapshot.Loss.OmittedDetails != 0 {
		t.Fatal("qualification summary aliases receipt")
	}
	r.TrialCase = "observation-baseline"
	if err := validateQualificationOutcome(r); err == nil {
		t.Fatal("baseline accepted partial ownership attribution")
	}
}

func TestQualificationFunctionalRejectsEveryMissingCounter(t *testing.T) {
	trial, _, proofs := qualificationFixture(t, nil)
	r, err := trial.store.Execution(proofs[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < reflect.TypeFor[networkview.Counters]().NumField(); i++ {
		copy := r
		receipt := *r.Receipt
		copy.Receipt = &receipt
		counters := *r.Receipt.Snapshot.Counters
		copy.Receipt.Snapshot.Counters = &counters
		reflect.ValueOf(&counters).Elem().Field(i).SetZero()
		if err := validateQualificationOutcome(copy); err == nil {
			t.Fatalf("missing %s counter qualified", reflect.TypeFor[networkview.Counters]().Field(i).Name)
		}
	}
}

func TestQualificationFunctionalRequiresEveryExactSourceDimension(t *testing.T) {
	trial, _, proofs := qualificationFixture(t, nil)
	r, err := trial.store.Execution(proofs[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range reflect.TypeFor[networkview.Coverage]().NumField() {
		copy := r
		receipt := *r.Receipt
		copy.Receipt = &receipt
		metric := reflect.ValueOf(&copy.Receipt.Snapshot.Coverage).Elem().Field(i).Addr().Interface().(*networkview.MetricCoverage)
		*metric = networkview.MetricCoverage{Status: "lower-bound", Reason: "source-gap"}
		if err := validateQualificationOutcome(copy); err == nil {
			t.Fatalf("degraded %s source qualified", reflect.TypeFor[networkview.Coverage]().Field(i).Name)
		}
	}
}

func TestQualificationV2RequiresBothNewCases(t *testing.T) {
	trial, _, proofs := qualificationFixture(t, nil)
	for _, name := range []string{"observation-baseline", "short-flow"} {
		without := slices.DeleteFunc(slices.Clone(proofs), func(p QualificationProof) bool { return p.Case == name })
		if _, err := trial.Complete(nil, without); err == nil {
			t.Fatal("qualified without mandatory case", name)
		}
	}
}

func TestQualificationMCPProjectionRequiresASelectedClient(t *testing.T) {
	trial, spec, proofs := qualificationFixture(t, nil)
	q, err := trial.Complete(nil, proofs)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := trial.store.LoadSnapshot(spec.Project, spec.PolicyFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.RequireLaunch(policy, "none"); err != nil {
		t.Fatal("raw launch without MCP should require no client witness", err)
	}
	if err := q.RequireLaunch(policy, strings.Repeat("a", 64)); err == nil {
		t.Fatal("enabled MCP projection qualified with no selected client")
	}
}

func TestQualificationHistoryCannotExcuseOtherObservationFaults(t *testing.T) {
	trial := executionTrial(t, openStore(t))
	proof := qualificationProofFixture(t, trial, qualificationExecutionSpec(t, trial), "short-flow", nil, addQualificationHistory)
	r, err := trial.store.Execution(proof.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*networkview.Snapshot){
		"missing history":        func(s *networkview.Snapshot) { s.Connections = nil },
		"invented history bytes": func(s *networkview.Snapshot) { s.Connections[0].SentBytes = networkview.Value(0) },
		"inode reuse":            func(s *networkview.Snapshot) { s.Connections[0].Reason = "socket_inode_changed" },
		"clock":                  func(s *networkview.Snapshot) { s.Connections[0].Reason = "socket_join_clock_unavailable" },
		"security beside gap":    func(s *networkview.Snapshot) { s.Loss.Reasons = append(s.Loss.Reasons, "unexpected_socket_owner") },
		"capacity": func(s *networkview.Snapshot) {
			s.Loss.Reasons = append(s.Loss.Reasons, "socket_history_identity_capacity")
		},
		"source loss":       func(s *networkview.Snapshot) { s.Sources[0].Lost = 1 },
		"source reset":      func(s *networkview.Snapshot) { s.Sources[1].Unknown = true },
		"missing source":    func(s *networkview.Snapshot) { s.Sources = s.Sources[1:] },
		"duplicate source":  func(s *networkview.Snapshot) { s.Sources[1] = s.Sources[0] },
		"source stale":      func(s *networkview.Snapshot) { at := s.AsOf.Add(-time.Minute); s.Sources[2].ObservedAt = &at },
		"detail truncation": func(s *networkview.Snapshot) { s.Loss.DetailTruncated = true },
		"omission":          func(s *networkview.Snapshot) { s.Loss.OmittedDetails = networkview.Value(1) },
		"unknown omission":  func(s *networkview.Snapshot) { s.Loss.OmittedDetails = nil },
		"pending":           func(s *networkview.Snapshot) { s.PendingConnections = 1 },
		"stale":             func(s *networkview.Snapshot) { s.StaleConnections = 1 },
		"proxy gap":         func(s *networkview.Snapshot) { s.Coverage.ProxyBytes.Status = "lower-bound" },
	} {
		t.Run(name, func(t *testing.T) {
			var copy Execution
			data, _ := json.Marshal(r)
			if err := json.Unmarshal(data, &copy); err != nil {
				t.Fatal(err)
			}
			change(&copy.Receipt.Snapshot)
			if err := validateQualificationOutcome(copy); err == nil {
				t.Fatal("sampled gap excused an independent observation fault")
			}
		})
	}
}

func TestQualificationObservationComesFromSealedReceipt(t *testing.T) {
	trial, _, proofs := qualificationFixture(t, nil)
	q, err := trial.Complete(nil, proofs)
	if err != nil {
		t.Fatal(err)
	}
	for _, proof := range q.Proofs {
		r, err := trial.store.Execution(proof.RunID)
		if err != nil {
			t.Fatal(err)
		}
		want, err := qualificationObservation(r.Receipt)
		if err != nil || !equalJSON(want, proof.Observation) {
			t.Fatal("completed summary not derived from sealed receipt", err)
		}
		if proof.Case == "guard-loss" && !slices.Contains(proof.Observation.Loss.Reasons, "terminal_observation_unavailable") {
			t.Fatal("summary used pre-seal execution snapshot")
		}
	}
	forged := slices.Clone(q.Proofs)
	observation := *forged[0].Observation
	observation.Scope = "invented"
	forged[0].Observation = &observation
	if _, err := trial.Complete(nil, forged); err == nil {
		t.Fatal("caller forged observation summary")
	}
	q.Proofs[0].Observation = nil
	if _, err := canonicalQualification(q); err == nil {
		t.Fatal("completed v2 record omitted its required observation")
	}
}

func TestQualificationEvidenceBindsExactTerminalFrame(t *testing.T) {
	trial, _, proofs := qualificationFixture(t, nil)
	r, err := trial.store.Execution(proofs[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range []bool{false, true} {
		snapshot := r.Receipt.Snapshot
		if changed {
			snapshot.Scope = "invented"
		}
		data, err := qualificationEvidenceBytes(snapshot, map[string]string{"fixture": "synthetic-unit-only"})
		if err != nil {
			t.Fatal(err)
		}
		if err := validateQualificationEvidence(data, r.Receipt); (err != nil) != changed {
			t.Fatal("evidence did not bind the sealed frame", err)
		}
	}
	data := []byte(strings.Repeat(" ", MaxQualificationEvidenceBytes+1))
	if _, err := trial.RecordEvidence(r.ID, data); err == nil {
		t.Fatal("oversized evidence accepted")
	}
}

func TestQualificationEvidenceRejectsSubstitutedFrameAtBothEntryPoints(t *testing.T) {
	trial, _, proofs := qualificationFixture(t, nil)
	r, err := trial.store.Execution(proofs[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := r.Receipt.Snapshot
	snapshot.Scope = "invented"
	data, err := qualificationEvidenceBytes(snapshot, map[string]string{"fixture": "synthetic-unit-only"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trial.RecordEvidence(r.ID, data); err == nil {
		t.Fatal("publisher accepted substituted terminal frame")
	}
	// Bypass the publisher as a corruption fixture, including a matching digest:
	// Complete must independently bind the typed evidence to the sealed receipt.
	if err := trial.store.publish("qualification-evidence-"+r.ID+".json", data, true); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	proofs[0].EvidenceDigest = hex.EncodeToString(digest[:])
	if _, err := trial.Complete(nil, proofs); err == nil {
		t.Fatal("completion accepted substituted terminal frame with matching digest")
	}
}

func TestQualificationEvidenceStrictAndConsistentlyBounded(t *testing.T) {
	trial, _, proofs := qualificationFixture(t, nil)
	r, err := trial.store.Execution(proofs[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := trial.store.read("qualification-evidence-"+r.ID+".json", MaxQualificationEvidenceBytes)
	if err != nil {
		t.Fatal(err)
	}
	for name, invalid := range map[string][]byte{
		"truncated": data[:len(data)-1],
		"duplicate": []byte(strings.Replace(string(data), `"version":1`, `"version":1,"version":1`, 1)),
		"unknown":   []byte(strings.Replace(string(data), `"version":1`, `"version":1,"unknown":true`, 1)),
	} {
		if _, err := trial.RecordEvidence(r.ID, invalid); err == nil {
			t.Fatal("invalid evidence published", name)
		}
	}
	// JSON whitespace remains evidence bytes. Exercise the same >128 KiB frame
	// at initial publication, idempotent publication and final completion.
	large := append(slices.Clone(data), []byte(strings.Repeat(" ", 129<<10))...)
	if err := trial.store.root.Remove("qualification-evidence-" + r.ID + ".json"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		proofs[0].EvidenceDigest, err = trial.RecordEvidence(r.ID, large)
		if err != nil {
			t.Fatal("valid large evidence refused", err)
		}
	}
	if _, err := trial.Complete(nil, proofs); err != nil {
		t.Fatal("completion has a different evidence limit", err)
	}
}

func TestQualificationMaximumPlanFitsRecordBound(t *testing.T) {
	trial, _, proofs := qualificationFixture(t, nil)
	q, err := trial.Complete(nil, proofs)
	if err != nil {
		t.Fatal(err)
	}
	base := q.Proofs[slices.IndexFunc(q.Proofs, func(p QualificationProof) bool { return p.Case == "enforcement" })]
	for i := range 32 {
		client := QualifiedClient{Dependency: egress.Dependency{Provider: "anthropic", Client: egress.ClientCLI,
			Backend: "direct", AuthMode: "oauth-file", Version: "v1"}, Features: []string{}, MCPProjection: fmt.Sprintf("%064x", i+1)}
		q.Coverage = append(q.Coverage, client)
		start := fmt.Sprintf("%032x", 1000+i*3)
		for j, name := range []string{"provider-start", "provider-resume", "mcp"} {
			proof := base
			proof.Case, proof.Client, proof.RunID = name, &client, fmt.Sprintf("%032x", 1000+i*3+j)
			if name == "provider-resume" {
				proof.ResumesRunID = start
			}
			q.Proofs = append(q.Proofs, proof)
		}
	}
	q, err = canonicalQualification(q)
	if err != nil {
		t.Fatal(err)
	}
	q.ID = trial.store.qualificationID(q)
	data, err := json.Marshal(q)
	if err != nil || len(q.Proofs) != 105 || len(data) > maxQualificationBytes {
		t.Fatal("maximum declared plan exceeds published record bound", len(data), err)
	}
	t.Logf("32-client, 105-proof canonical record: %d bytes", len(data))
}

func TestQualificationDiscoveryRefusesAnUnauthenticatedRecord(t *testing.T) {
	trial, _, proofs := qualificationFixture(t, nil)
	current, err := trial.Complete(nil, proofs)
	if err != nil {
		t.Fatal(err)
	}
	forged := current
	forged.CompletedAt = forged.CompletedAt.Add(time.Second)
	forged.ID = strings.Repeat("f", 64)
	data, _ := json.Marshal(forged)
	if err := trial.store.publish("qualification-"+forged.ID+".json", data, false); err != nil {
		t.Fatal(err)
	}
	if _, err := trial.store.Qualification(forged.ID); err == nil {
		t.Fatal("forged record became launch authority")
	}
	// Discovery refuses instead of silently skipping: a reader must never see a
	// shorter index and conclude that no qualification exists.
	if _, err := trial.store.Qualifications(context.Background()); err == nil {
		t.Fatal("untrusted record silently ignored during discovery")
	}
}
