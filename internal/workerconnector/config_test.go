package workerconnector

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestConnectorConfigurationBuildsOnlyAProtocolValidWorker(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"ca.pem", "worker-identity.pem", "enrollment-token"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	document := `{
  "version": 1,
  "worker_id": "worker-a",
  "workspace_ref": "workspace-main",
  "responder_url": "https://responder.example",
  "ca_file": "` + filepath.Join(directory, "ca.pem") + `",
  "identity_file": "` + filepath.Join(directory, "worker-identity.pem") + `",
  "enrollment_token_file": "` + filepath.Join(directory, "enrollment-token") + `",
  "coop_socket": "/var/run/coop/control.sock",
  "journal_dir": "` + filepath.Join(directory, "journal") + `",
  "sandbox_digest": "` + repeatedDigest("a") + `",
  "policy_digests": {"work-read-only": "` + repeatedDigest("b") + `"},
  "policy_authority_digests": {"work-read-only": "` + repeatedDigest("c") + `"},
  "repositories": [{"ref":"responder","revision":"commit:abc123"}],
  "capabilities": [{"name":"responder-state","version":"1"}],
  "capacity": {"session_slots_free":2,"session_slots_total":2,"turn_slots_free":2,"turn_slots_total":2,"workspace_slots_free":1,"workspace_slots_total":1,"state":"eligible","cooldown_until":null},
  "poll_interval_ms": 1000,
  "request_timeout_ms": 30000
}`
	path := filepath.Join(directory, "worker.json")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}

	configuration, err := LoadConfig(path, "coop-test", time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Hello.ID != "worker-a" || configuration.Hello.BuildVersion != "coop-test" || configuration.PollInterval != time.Second {
		t.Fatalf("configuration = %+v", configuration)
	}
	if configuration.Hello.PolicyAuthorityDigests["work-read-only"] != repeatedDigest("c") {
		t.Fatalf("worker authority digests = %+v", configuration.Hello.PolicyAuthorityDigests)
	}
	if got := configuration.Hello.Capabilities; len(got) != 1 ||
		got[0].Name != "responder-state" || got[0].Version != "1" {
		t.Fatalf("worker capabilities = %+v", got)
	}

	unknown := filepath.Join(directory, "unknown.json")
	if err := os.WriteFile(unknown, []byte(document[:len(document)-2]+`,"provider_credentials":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(unknown, "coop-test", time.Now()); err == nil {
		t.Fatal("unknown authority configuration was accepted")
	}
}

func TestDocumentedWorkerConfigurationLoads(t *testing.T) {
	path, err := filepath.Abs("../../docs/examples/worker.json")
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := LoadConfig(path, "example-test", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Hello.ID != "worker-a" || configuration.Hello.WorkspaceRef != "workspace-main" ||
		len(configuration.Hello.PolicyDigests) != 1 || len(configuration.Hello.PolicyAuthorityDigests) != 1 ||
		configuration.PollInterval != time.Second || configuration.RenewBefore != time.Hour {
		t.Fatalf("documented worker contract changed: %+v", configuration)
	}
}

func TestWorkerFreshnessCapabilityRequiresLiveSessionDaemonProof(t *testing.T) {
	configured := []workerproto.Capability{
		{Name: "responder-state", Version: "1"},
		{Name: repositoryFreshnessCapabilityName, Version: "999"},
	}

	api := &capabilityAPI{response: json.RawMessage(`{"repository_freshness_receipt_versions":[2]}`)}
	got := LiveCapabilities(context.Background(), api, configured)
	if len(got) != 2 || got[0].Name != "responder-state" ||
		got[1].Name != repositoryFreshnessCapabilityName ||
		got[1].Version != repositoryFreshnessCapabilityVersion {
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
			got := LiveCapabilities(context.Background(), scripted, configured)
			if len(got) != 1 || got[0].Name != "responder-state" {
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

func (a *capabilityAPI) Do(_ context.Context, request Request) (json.RawMessage, error) {
	a.request = request
	return a.response, a.err
}
