package workerconnector

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestWorkerFreshnessCapabilityRequiresLiveSessionDaemonProof(t *testing.T) {
	api := &capabilityAPI{response: json.RawMessage(`{"repository_freshness_receipt_versions":[2]}`)}
	got := LiveCapabilities(context.Background(), api)
	if len(got) != 1 || got[0].Name != repositoryFreshnessCapabilityName ||
		got[0].Version != repositoryFreshnessCapabilityVersion {
		t.Fatalf("live capabilities = %+v", got)
	}
	if api.request.Method != "GET" || api.request.Path != "/v1/capabilities" || len(api.request.Body) != 0 {
		t.Fatalf("capability request = %+v", api.request)
	}

	for name, scripted := range map[string]*capabilityAPI{
		"old endpoint": {err: errors.New("404")},
		"malformed":    {response: json.RawMessage(`{"repository_freshness_receipt_versions":[1]}`)},
	} {
		t.Run(name, func(t *testing.T) {
			got := LiveCapabilities(context.Background(), scripted)
			if len(got) != 0 {
				t.Fatalf("unproven capabilities = %+v", got)
			}
		})
	}
}

type capabilityAPI struct {
	request  Request
	response json.RawMessage
	err      error
}

func TestControllerToolsCapabilityRequiresExactLiveProof(t *testing.T) {
	for _, test := range []struct {
		body string
		want int
	}{
		{`{"controller_tools_versions":[1]}`, 1},
		{`{}`, 0},
		{`{"controller_tools_versions":[2]}`, 0},
		{`{"controller_tools_versions":[1,2]}`, 0},
	} {
		got := LiveCapabilities(context.Background(), &capabilityAPI{response: json.RawMessage(test.body)})
		if len(got) != test.want || (test.want == 1 && (got[0].Name != "controller-tools" || got[0].Version != "1")) {
			t.Fatalf("capability for %s = %+v", test.body, got)
		}
	}
}

func TestJobSetupCapabilityRequiresExactLiveDaemonProof(t *testing.T) {
	for _, test := range []struct {
		body string
		want bool
	}{
		{`{"job_spec_versions":[2]}`, true},
		{`{}`, false},
		{`{"job_spec_versions":[1]}`, false},
		{`{"job_spec_versions":[1,2]}`, false},
		{`{"job_spec_versions":[2],"unknown":true}`, false},
	} {
		found := false
		for _, capability := range LiveCapabilities(context.Background(), &capabilityAPI{response: json.RawMessage(test.body)}) {
			found = found || capability.Name == "job-setup" && capability.Version == "2"
		}
		if found != test.want {
			t.Fatalf("job setup capability for %s = %t, want %t", test.body, found, test.want)
		}
	}
}

func (a *capabilityAPI) Do(_ context.Context, request Request) (json.RawMessage, error) {
	a.request = request
	return a.response, a.err
}
