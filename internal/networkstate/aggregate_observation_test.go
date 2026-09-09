package networkstate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/networkview"
)

func TestInspectionAggregateObservationProvisionalIdentityAndProjection(t *testing.T) {
	base := time.Now().UTC()
	s, evidence, record := observedFixture(t, func(snapshot *networkview.Snapshot) {
		snapshot.Sequence, snapshot.Availability, snapshot.AsOf = 1, "available", base
		snapshot.Counters = &networkview.Counters{SentBytes: networkview.Value(7)}
		snapshot.Connections = []networkview.Connection{{
			ID: "flow-a", DestinationID: "opaque-a", State: "open", Transport: "tcp",
			Name: "private.example", Peer: "10.42.19.12:443", RuleID: "private-rule",
		}}
	})
	got, err := evidence.Inspect(record.ID, base, false)
	if err != nil {
		t.Fatal(err)
	}
	r := got.AggregateObservation.Receipt
	if got.Receipt != nil || got.AggregateObservation.Revision != record.Revision ||
		r.ID != record.ID || !r.StartedAt.Equal(record.StartedAt) || r.SessionID != record.SessionID ||
		r.Finality != "provisional" ||
		r.Completeness != "partial" || r.Digest == "" || r.DigestScope != "destinations-withheld" {
		t.Fatalf("invalid provisional aggregate: %+v", got.AggregateObservation)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private.example", "10.42.19.12:443", "private-rule", record.Project, record.Endpoint} {
		if private != "" && strings.Contains(string(data), private) {
			t.Fatalf("aggregate observation leaked %q", private)
		}
	}
	retained, err := s.Execution(record.ID)
	if err != nil || !equalJSON(retained, record) {
		t.Fatalf("inspection changed retained execution: %v", err)
	}
	if got.AggregateObservation.Receipt.Snapshot.Counters.ReceivedBytes != nil {
		t.Fatal("unmeasured bytes became zero")
	}
}

func TestInspectionAggregateObservationUnknownAndSealed(t *testing.T) {
	s, record := executionFixture(t)
	evidence := fixtureEvidence(t, s)
	got, err := evidence.Inspect(record.ID, time.Now().UTC(), false)
	if err != nil || got.AggregateObservation.Receipt.Completeness != "unknown" {
		t.Fatal("no samples did not remain unknown", err)
	}
	record, err = s.SealExecution(context.Background(), record.ID, record.Revision, "launch_failed")
	if err != nil {
		t.Fatal(err)
	}
	got, err = evidence.Inspect(record.ID, time.Now().UTC(), false)
	if err != nil {
		t.Fatal(err)
	}
	want, err := record.Receipt.Project(false)
	if err != nil || got.Receipt == nil || !equalJSON(got.AggregateObservation.Receipt, want) {
		t.Fatalf("sealed aggregate differs from exact projection: %v", err)
	}
	got.AggregateObservation.Receipt.Snapshot.Loss.Reasons = append(got.AggregateObservation.Receipt.Snapshot.Loss.Reasons, "mutated")
	if !equalJSON(*got.Receipt, want) {
		t.Fatal("aggregate mutation reached ordinary receipt")
	}
}
