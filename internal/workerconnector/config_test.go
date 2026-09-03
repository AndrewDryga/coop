package workerconnector

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

	unknown := filepath.Join(directory, "unknown.json")
	if err := os.WriteFile(unknown, []byte(document[:len(document)-2]+`,"provider_credentials":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(unknown, "coop-test", time.Now()); err == nil {
		t.Fatal("unknown authority configuration was accepted")
	}
}
