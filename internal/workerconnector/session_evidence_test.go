package workerconnector

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

func evidenceFixture(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("../workerproto/testdata/session_evidence.json")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(body))
}

// The evidence read is a plain owner-private GET, like the networking reads beside it: the
// connector chooses the path and forwards the daemon's answer verbatim. It holds no projection,
// no destination and no task — the daemon already decided what this session discloses.
func TestExecutorMapsTheSessionEvidenceCommand(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{response: json.RawMessage(evidenceFixture(t))}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))
	command.Kind, command.Payload = "get_session_evidence", json.RawMessage(`{"coop_session_id":"coop/session-1"}`)
	result, err := executor.Execute(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	want := "/v1/sessions/coop%2Fsession-1/evidence"
	if result.State != "succeeded" || len(api.requests) != 1 || api.requests[0].Method != "GET" || api.requests[0].Path != want {
		t.Fatalf("evidence requests = %+v (result %+v), want one GET %s", api.requests, result, want)
	}
	// A read carries no idempotency key and no body: it changes nothing.
	if api.requests[0].IdempotencyKey != "" || len(api.requests[0].Body) != 0 {
		t.Fatalf("a read sent a mutation: %+v", api.requests[0])
	}
	if string(result.Resource) != evidenceFixture(t) {
		t.Fatalf("the connector rewrote the daemon's answer: %s", result.Resource)
	}
}

// A daemon answer that does not satisfy the evidence contract is a definite failure the
// controller records as "not captured". Forwarding it would make a control plane guess whether a
// missing section meant an empty network or a daemon that answered something else entirely.
func TestExecutorRefusesSessionEvidenceOutsideItsContract(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	for name, body := range map[string]string{
		"unsupported version": strings.Replace(evidenceFixture(t), `"version":1`, `"version":2`, 1),
		"unknown field":       strings.Replace(evidenceFixture(t), `"version":1,`, `"version":1,"host_path":"/home/coop",`, 1),
		"leaked destination under a withheld projection": strings.Replace(evidenceFixture(t),
			`"destination":null,"destination_withheld":true`, `"destination":"api.example.com","destination_withheld":true`, 1),
		"not an object": `[]`,
	} {
		api := &fakeAPI{response: json.RawMessage(body)}
		executor, err := NewExecutor(ExecutorConfig{
			API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
		})
		if err != nil {
			t.Fatal(err)
		}
		command := createCommand(now.Add(time.Minute))
		command.Kind, command.Payload = "get_session_evidence", json.RawMessage(`{"coop_session_id":"coop/session-1"}`)
		result, err := executor.Execute(context.Background(), command)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if result.State != "failed" || !strings.Contains(string(result.Error), "invalid_session_evidence") {
			t.Fatalf("%s produced %+v", name, result)
		}
	}

	api := &fakeAPI{}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))
	command.Kind, command.Payload = "get_session_evidence", json.RawMessage(`{"coop_session_id":""}`)
	result, err := executor.Execute(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "failed" || len(api.requests) != 0 {
		t.Fatalf("an evidence read with no identity = %+v (requests %d)", result, len(api.requests))
	}
}

// A worker only advertises the evidence capability once the daemon behind it proves it. Without
// that proof a control plane cannot tell an older worker's silence from a session that genuinely
// observed nothing, and the two render differently.
func TestSessionEvidenceCapabilityRequiresLiveSessionDaemonProof(t *testing.T) {
	configured := []workerproto.Capability{
		{Name: "responder-state", Version: "1"},
		{Name: sessionEvidenceCapabilityName, Version: "999"},
	}
	api := &capabilityAPI{response: json.RawMessage(
		`{"repository_freshness_receipt_versions":[2],"repository_source_selector_versions":[1],"session_evidence_versions":[1]}`)}
	got := LiveCapabilities(context.Background(), api, configured)
	if len(got) != 4 || got[0].Name != "responder-state" || got[3].Name != sessionEvidenceCapabilityName ||
		got[3].Version != sessionEvidenceCapabilityVersion {
		t.Fatalf("live capabilities = %+v", got)
	}
	for name, scripted := range map[string]*capabilityAPI{
		"old daemon":        {err: errors.New("404")},
		"no evidence proof": {response: json.RawMessage(`{"repository_freshness_receipt_versions":[2]}`)},
		"other version":     {response: json.RawMessage(`{"repository_freshness_receipt_versions":[2],"session_evidence_versions":[2]}`)},
	} {
		t.Run(name, func(t *testing.T) {
			for _, capability := range LiveCapabilities(context.Background(), scripted, configured) {
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
