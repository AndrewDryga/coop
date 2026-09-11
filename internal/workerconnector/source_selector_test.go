package workerconnector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// The selector is placement authority: it must reach the daemon byte for byte, because the
// daemon's own admission is what refuses an unauthorized one. The connector's job is to forward
// it under the same bounds the daemon enforces, never to reinterpret it.
func TestCreateSessionForwardsTheSourceSelectorToTheDaemon(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	for name, testCase := range map[string]struct {
		payload string
		want    string
	}{
		"default": {
			payload: `{"kind":"default"}`,
			want:    `"source":{"kind":"default"}`,
		},
		"branch": {
			payload: `{"kind":"branch","name":"feature/payments"}`,
			want:    `"source":{"kind":"branch","name":"feature/payments"}`,
		},
		"pull request with host evidence": {
			payload: `{"kind":"pull_request","number":514,"expected_head_commit":"` + strings.Repeat("a", 40) + `"}`,
			want:    `"source":{"kind":"pull_request","number":514,"expected_head_commit":"` + strings.Repeat("a", 40) + `"}`,
		},
		"commit": {
			payload: `{"kind":"commit","sha":"` + strings.Repeat("b", 64) + `"}`,
			want:    `"source":{"kind":"commit","sha":"` + strings.Repeat("b", 64) + `"}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakeAPI{response: json.RawMessage(`{"operation":{"id":"operation-create-1","method":"CreateRemoteSession","state":"running"}}`)}
			executor, err := NewExecutor(ExecutorConfig{
				API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
			})
			if err != nil {
				t.Fatal(err)
			}
			command := createCommand(now.Add(time.Minute))
			command.Payload = json.RawMessage(
				`{"external_ref":"episode-1","policy":"work-read-only","policy_digest":"` +
					repeatedDigest("b") + `","source":` + testCase.payload + `}`)
			if _, err := executor.Execute(context.Background(), command); err != nil {
				t.Fatal(err)
			}
			if len(api.requests) != 1 {
				t.Fatalf("issued %d requests", len(api.requests))
			}
			if !strings.Contains(string(api.requests[0].Body), testCase.want) {
				t.Fatalf("create body = %s, want it to carry %s", api.requests[0].Body, testCase.want)
			}
		})
	}
}

// A selector the daemon would refuse never reaches the Unix API: the connector has the same
// bounds, so a malformed placement fails here rather than after a round trip.
func TestCreateSessionRefusesAMalformedSourceSelectorBeforeTheUnixAPI(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	for name, payload := range map[string]string{
		"unknown kind":           `{"kind":"tag","name":"v1"}`,
		"missing kind":           `{"name":"main"}`,
		"branch without a name":  `{"kind":"branch"}`,
		"branch that is a flag":  `{"kind":"branch","name":"--upload-pack=touch"}`,
		"branch with a newline":  `{"kind":"branch","name":"main\nrefs/heads/other"}`,
		"pull request zero":      `{"kind":"pull_request","number":0}`,
		"pull request unbounded": `{"kind":"pull_request","number":2000000000}`,
		"commit abbreviated":     `{"kind":"commit","sha":"0123456"}`,
		"commit uppercase":       `{"kind":"commit","sha":"` + strings.Repeat("A", 40) + `"}`,
		"two kinds at once":      `{"kind":"branch","name":"main","number":3}`,
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakeAPI{response: json.RawMessage(`{"operation":{"id":"operation-create-1","state":"running"}}`)}
			executor, err := NewExecutor(ExecutorConfig{
				API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
			})
			if err != nil {
				t.Fatal(err)
			}
			command := createCommand(now.Add(time.Minute))
			command.Payload = json.RawMessage(
				`{"external_ref":"episode-1","policy":"work-read-only","policy_digest":"` +
					repeatedDigest("b") + `","source":` + payload + `}`)
			result, err := executor.Execute(context.Background(), command)
			if err != nil {
				t.Fatal(err)
			}
			if result.State != "failed" || !strings.Contains(string(result.Error), "invalid_command") {
				t.Fatalf("result = %+v, want a definite invalid_command failure", result)
			}
			if len(api.requests) != 0 {
				t.Fatalf("a malformed selector reached the daemon: %+v", api.requests)
			}
		})
	}
}

// A fence occupies the create's exact ledger identity, so it must present the same request — the
// selector included. The connector passes a create fence through unchanged; this proves the
// selector survives that pass-through byte for byte and that a different one is a different
// request.
func TestFenceCarriesTheSameSourceSelectorAsItsCreate(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	fenceBody := func(t *testing.T, request string) map[string]any {
		t.Helper()
		api := &fakeAPI{response: json.RawMessage(`{"operation":{"id":"operation-fence-1","state":"failed","error_code":"operation_fenced"}}`)}
		executor, err := NewExecutor(ExecutorConfig{
			API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
		})
		if err != nil {
			t.Fatal(err)
		}
		command := createCommand(now.Add(time.Minute))
		command.Kind = "fence_operation"
		command.Payload = json.RawMessage(`{"method":"CreateRemoteSession","request":` + request + `}`)
		if _, err := executor.Execute(context.Background(), command); err != nil {
			t.Fatal(err)
		}
		if len(api.requests) != 1 || api.requests[0].Path != "/v1/operations/fence" {
			t.Fatalf("fence requests = %+v", api.requests)
		}
		var document map[string]any
		if err := json.Unmarshal(api.requests[0].Body, &document); err != nil {
			t.Fatal(err)
		}
		return document
	}
	request := `{"policy":"work-read-only","task":"episode-1","source":{"kind":"branch","name":"feature/payments"}}`
	fenced := fenceBody(t, request)
	if fenced["method"] != "CreateRemoteSession" {
		t.Fatalf("fence method = %v", fenced["method"])
	}
	forwarded, _ := fenced["request"].(map[string]any)
	source, _ := forwarded["source"].(map[string]any)
	if source["kind"] != "branch" || source["name"] != "feature/payments" {
		t.Fatalf("fenced source = %v, want the create's exact selector", source)
	}
}

// A worker only advertises the selector capability once the daemon behind it proves it. A
// configured claim is never trusted: a connector restart must not speak for an older daemon that
// is still serving its socket through a rolling upgrade.
func TestSourceSelectorCapabilityRequiresLiveSessionDaemonProof(t *testing.T) {
	configured := []workerproto.Capability{
		{Name: "responder-state", Version: "1"},
		{Name: repositorySourceSelectorCapabilityName, Version: "999"},
	}
	api := &capabilityAPI{response: json.RawMessage(
		`{"repository_freshness_receipt_versions":[2],"repository_source_selector_versions":[1]}`)}
	got := LiveCapabilities(context.Background(), api, configured)
	if len(got) != 3 || got[0].Name != "responder-state" ||
		got[1].Name != repositoryFreshnessCapabilityName ||
		got[2].Name != repositorySourceSelectorCapabilityName ||
		got[2].Version != repositorySourceSelectorCapabilityVersion {
		t.Fatalf("live capabilities = %+v", got)
	}

	for name, scripted := range map[string]*capabilityAPI{
		"old endpoint":      {err: errors.New("404")},
		"no selector proof": {response: json.RawMessage(`{"repository_freshness_receipt_versions":[2]}`)},
		"other version":     {response: json.RawMessage(`{"repository_freshness_receipt_versions":[2],"repository_source_selector_versions":[2]}`)},
	} {
		t.Run(name, func(t *testing.T) {
			got := LiveCapabilities(context.Background(), scripted, configured)
			for _, capability := range got {
				if capability.Name == repositorySourceSelectorCapabilityName {
					t.Fatalf("unproven selector capability advertised: %+v", got)
				}
			}
		})
	}
}
