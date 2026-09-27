package workerconnector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The evidence read is a plain owner-private GET, like the networking reads beside it: the
// connector chooses the path and forwards the daemon's answer verbatim. It holds no projection,
// no destination and no task — the daemon already decided what this session discloses.
// A daemon answer that does not satisfy the evidence contract is a definite failure the
// controller records as "not captured". Forwarding it would make a control plane guess whether a
// missing section meant an empty network or a daemon that answered something else entirely.
// A worker only advertises the evidence capability once the daemon behind it proves it. Without
// that proof a control plane cannot tell an older worker's silence from a session that genuinely
// observed nothing, and the two render differently.
func TestSessionEvidenceCapabilityRequiresLiveSessionDaemonProof(t *testing.T) {
	api := &capabilityAPI{response: json.RawMessage(
		`{"repository_freshness_receipt_versions":[2],"session_evidence_versions":[1]}`)}
	got := LiveCapabilities(context.Background(), api)
	if len(got) != 2 || got[1].Name != sessionEvidenceCapabilityName ||
		got[1].Version != sessionEvidenceCapabilityVersion {
		t.Fatalf("live capabilities = %+v", got)
	}
	for name, scripted := range map[string]*capabilityAPI{
		"old daemon":        {err: errors.New("404")},
		"no evidence proof": {response: json.RawMessage(`{"repository_freshness_receipt_versions":[2]}`)},
		"other version":     {response: json.RawMessage(`{"repository_freshness_receipt_versions":[2],"session_evidence_versions":[2]}`)},
	} {
		t.Run(name, func(t *testing.T) {
			for _, capability := range LiveCapabilities(context.Background(), scripted) {
				if capability.Name == sessionEvidenceCapabilityName {
					t.Fatalf("unproven evidence capability advertised: %+v", capability)
				}
			}
		})
	}
}

// The `network` event is how one sealed filtered run's refusals reach a control plane that is not
// watching a terminal. The daemon already applied the policy's destination projection when it
// appended the event, so this boundary bounds and types the fields and adds no disclosure — but a
// free-form field the daemon never promised must still not cross.
func TestPublicActivityExportsBoundedNetworkRefusals(t *testing.T) {
	raw := json.RawMessage(`{"version":1,"run_id":"run-7f3a","denials":[` +
		`{"destination":"name withheld","basis":"tls","count":3},` +
		`{"destination":"blocked.example","basis":"dns","count":1,"peer":"203.0.113.9"}],` +
		`"omitted_destinations":4,"alerts":["collector degraded"],"allowed_traffic":"1 connection, 10 B sent",` +
		`"detail_truncated":true,"evidence_id":"evt-0031","internal_note":"do not export"}`)
	payload, ok := publicActivityPayload("network", raw)
	if !ok {
		t.Fatal("a valid network event was rejected")
	}
	body := string(payload)
	for _, want := range []string{`"run_id":"run-7f3a"`, `"basis":"tls"`, `"count":3`, `"omitted_destinations":4`,
		`"detail_truncated":true`, `"evidence_id":"evt-0031"`, `"allowed_traffic":"1 connection, 10 B sent"`,
		`"destination":"name withheld"`, `"destination":"blocked.example"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("lost %s: %s", want, body)
		}
	}
	for _, forbidden := range []string{"internal_note", "do not export", "203.0.113.9"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("exported %q: %s", forbidden, body)
		}
	}
	if !operatorActivityEvent("network") {
		t.Fatal("the network event is not on the operator allowlist")
	}

	// A version this connector does not speak is not a network event it may reinterpret.
	if _, ok := publicActivityPayload("network", json.RawMessage(`{"version":2,"run_id":"run-1"}`)); ok {
		t.Fatal("an unknown network event version was exported")
	}

	// Bounded by construction: a box that denies a thousand names costs one bounded event.
	denials := strings.Repeat(`{"destination":"name withheld","basis":"dns","count":1},`, maximumNetworkDenials+8)
	alerts := strings.Repeat(`"noisy",`, maximumNetworkAlerts+4)
	raw = json.RawMessage(`{"version":1,"run_id":"run-1","denials":[` + strings.TrimSuffix(denials, ",") +
		`],"alerts":[` + strings.TrimSuffix(alerts, ",") + `],"allowed_traffic":"quiet"}`)
	payload, ok = publicActivityPayload("network", raw)
	if !ok {
		t.Fatal("a large network event was rejected instead of bounded")
	}
	var value struct {
		Denials []map[string]any `json:"denials"`
		Alerts  []string         `json:"alerts"`
	}
	if err := json.Unmarshal(payload, &value); err != nil {
		t.Fatal(err)
	}
	if len(value.Denials) != maximumNetworkDenials || len(value.Alerts) != maximumNetworkAlerts {
		t.Fatalf("network event was not bounded: %d denials, %d alerts", len(value.Denials), len(value.Alerts))
	}
}
