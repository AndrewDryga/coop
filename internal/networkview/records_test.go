package networkview

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

func TestCountersStayExactAcrossJSONAndRefuseAmbiguousValues(t *testing.T) {
	for _, value := range []uint64{0, 1, 1<<53 + 1, ^uint64(0)} {
		data, err := json.Marshal(Count(value))
		if err != nil || data[0] != '"' {
			t.Fatalf("counter became a JSON number: %s %v", data, err)
		}
		var decoded Count
		if err := json.Unmarshal(data, &decoded); err != nil || uint64(decoded) != value {
			t.Fatalf("counter lost precision: %s %v", data, err)
		}
	}
	for _, invalid := range []string{`0`, `1.1`, `"-1"`, `"+1"`, `"01"`, `"1.0"`, `"18446744073709551616"`, `null`, `""`} {
		var value Count
		if err := json.Unmarshal([]byte(invalid), &value); err == nil {
			t.Fatalf("accepted ambiguous counter %s", invalid)
		}
	}
	value := Count(^uint64(0) - 1)
	if Add(&value, 2) || value != Count(^uint64(0)) {
		t.Fatal("counter overflow wrapped or reported exactness")
	}
}

func TestPartialMetricsRemainUnknownAndBadRatesCannotBeSealed(t *testing.T) {
	r := Receipt{Snapshot: Snapshot{Counters: &Counters{SentBytes: Value(0)}}}
	data, err := json.Marshal(r.Snapshot.Project(false))
	if err != nil || !strings.Contains(string(data), `"sent_bytes":"0"`) || !strings.Contains(string(data), `"denied_packets":null`) {
		t.Fatalf("missing source fabricated a zero: %s %v", data, err)
	}
	for _, rate := range []Rate{{SentPerSecond: math.NaN(), WindowMillis: 1}, {ReceivedPerSecond: math.Inf(1), WindowMillis: 1},
		{SentPerSecond: -1, WindowMillis: 1}, {SentPerSecond: 1}} {
		r.Snapshot.Rate = &rate
		r.Digest = "previous"
		if r.SealDigest() == nil || r.Digest != "" {
			t.Fatal("invalid evidence was sealed")
		}
		if _, err := r.Project(false); err == nil {
			t.Fatal("invalid evidence silently projected to a clean receipt")
		}
	}
}

func TestReceiptProjectionDoesNotLeakPrivateEndpointsCandidatesOrDigest(t *testing.T) {
	privateName := "secret-project.internal.example.com"
	privatePeer := "10.42.19.12:443"
	rule := egress.Rule{To: egress.Destination{Domain: privateName}, Protocol: "tls", Ports: []int{443}}
	private := Receipt{Version: Version, ID: "receipt", StartedAt: time.Now(), Finality: "final", Completeness: "partial", DigestScope: "owner-local",
		Snapshot: Snapshot{Version: Version, RunID: "run", Projection: "owner-local", Connections: []Connection{{ID: "flow", Name: privateName, Peer: privatePeer, RuleID: "private-rule"}},
			Denials:  []Denial{{ID: "event", Name: privateName, Candidate: &Candidate{ID: "candidate", Rule: rule}}},
			Counters: &Counters{SentBytes: Value(1<<53 + 1)}, Loss: Loss{Unknown: true}}}
	if err := private.SealDigest(); err != nil {
		t.Fatal(err)
	}
	redacted, err := private.Project(false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{privateName, privatePeer, "private-rule", "candidate", private.Digest} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("redacted receipt leaked %q: %s", secret, data)
		}
	}
	if redacted.Completeness != "partial" || redacted.Finality != "final" || !redacted.Snapshot.Loss.Unknown || redacted.DigestScope != "destinations-withheld" {
		t.Fatal("redaction hid incomplete evidence or changed finality")
	}
	if private.Snapshot.Connections[0].Name != privateName || private.Snapshot.Denials[0].Candidate == nil {
		t.Fatal("projection mutated owner-local receipt")
	}
	exported, err := private.Project(true)
	if err != nil {
		t.Fatal(err)
	}
	if exported.Snapshot.Connections[0].Name != privateName || exported.Snapshot.Denials[0].Candidate == nil || exported.DigestScope != "destinations-included" {
		t.Fatal("explicit destination export lost intended fields")
	}
	exported.Snapshot.Denials[0].Candidate.Rule.Ports[0] = 8443
	if private.Snapshot.Denials[0].Candidate.Rule.Ports[0] != 443 {
		t.Fatal("exported candidate aliases private mutable state")
	}
}
