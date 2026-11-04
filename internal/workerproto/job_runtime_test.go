package workerproto

import (
	"encoding/json"
	"strings"
	"testing"
)

// Work jobs previously discarded project tooling and service declarations, so the same change
// could pass source review without ever reaching the database its tests needed.
func TestJobRuntimeIsFrozenAuthorityAndOldJobsKeepTheirDigest(t *testing.T) {
	spec := validJobSpec()
	spec.RepositoryReadOnly = false
	oldDigest, err := spec.Digest()
	if err != nil {
		t.Fatal(err)
	}
	document, _ := json.Marshal(spec)
	withRuntime := strings.TrimSuffix(string(document), "}") + `,"runtime":{"version":1,"dockerfile":".agent/Dockerfile","compose":"compose.test.yml","environment":{"PGHOST":"project-db"}}}`
	decoded, err := DecodeJobSpec([]byte(withRuntime))
	if err != nil {
		t.Fatalf("project runtime was rejected: %v", err)
	}
	digest, err := decoded.Digest()
	if err != nil || digest == oldDigest {
		t.Fatalf("runtime did not bind the job digest: %s %s %v", oldDigest, digest, err)
	}
	reencoded, _ := json.Marshal(decoded)
	if !strings.Contains(string(reencoded), `"runtime":{"version":1,"dockerfile":".agent/Dockerfile","compose":"compose.test.yml","environment":{"PGHOST":"project-db"}}`) {
		t.Fatalf("runtime lost during round trip: %s", reencoded)
	}
	without, err := DecodeJobSpec(document)
	if err != nil {
		t.Fatal(err)
	}
	retainedDigest, _ := without.Digest()
	if retainedDigest != oldDigest {
		t.Fatalf("retained job digest changed: %s != %s", retainedDigest, oldDigest)
	}
}

func TestJobRuntimeRefusesIncompleteUnknownAndUnsafeAuthority(t *testing.T) {
	spec := validJobSpec()
	spec.RepositoryReadOnly = false
	document, _ := json.Marshal(spec)
	prefix := strings.TrimSuffix(string(document), "}") + `,"runtime":`
	for _, runtime := range []string{
		`null`, `{}`, `{"version":1,"dockerfile":".agent/Dockerfile"}`,
		`{"version":1,"dockerfile":"","compose":"","environment":{},"unknown":true}`,
		`{"version":2,"dockerfile":"","compose":"","environment":{}}`,
		`{"version":1,"dockerfile":"../Dockerfile","compose":"","environment":{}}`,
		`{"version":1,"dockerfile":"/Dockerfile","compose":"","environment":{}}`,
		`{"version":1,"dockerfile":".agent/../Dockerfile","compose":"","environment":{}}`,
		`{"version":1,"dockerfile":".agent\\Dockerfile","compose":"","environment":{}}`,
		`{"version":1,"dockerfile":".git/config","compose":"","environment":{}}`,
		`{"version":1,"dockerfile":"","compose":"a//b.yml","environment":{}}`,
		`{"version":1,"dockerfile":"","compose":"./compose.yml","environment":{}}`,
		`{"version":1,"dockerfile":"","compose":"compose.yml\n","environment":{}}`,
	} {
		if _, err := DecodeJobSpec([]byte(prefix + runtime + "}")); err == nil {
			t.Errorf("accepted runtime %s", runtime)
		}
	}
	for _, field := range []string{`"repository_read_only":true`, `"source":null`} {
		var object map[string]any
		_ = json.Unmarshal(document, &object)
		if strings.HasPrefix(field, `"source"`) {
			object["source"] = nil
		} else {
			object["repository_read_only"] = true
		}
		object["runtime"] = map[string]any{"version": 1, "dockerfile": "", "compose": "", "environment": map[string]string{}}
		encoded, _ := json.Marshal(object)
		if _, err := DecodeJobSpec(encoded); err == nil {
			t.Errorf("runtime accepted outside writable source: %s", field)
		}
	}
}

func TestJobRuntimeEnvironmentIsRequiredLiteralAndDigestBound(t *testing.T) {
	spec := validJobSpec()
	spec.RepositoryReadOnly = false
	document, _ := json.Marshal(spec)
	prefix := strings.TrimSuffix(string(document), "}") + `,"runtime":{"version":1,"dockerfile":"","compose":""`
	for _, environment := range []string{
		``, `,"environment":null`, `,"environment":[]`,
		`,"environment":{"COOP_EGRESS":"open"}`,
		`,"environment":{"PGHOST":"db\n"}`,
		`,"environment":{"PGHOST":true}`,
	} {
		if _, err := DecodeJobSpec([]byte(prefix + environment + "}}")); err == nil {
			t.Errorf("accepted invalid runtime environment %q", environment)
		}
	}
	first, err := DecodeJobSpec([]byte(prefix + `,"environment":{"PGHOST":"project-db"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := DecodeJobSpec([]byte(prefix + `,"environment":{"PGHOST":"other-db"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, _ := first.Digest()
	secondDigest, _ := second.Digest()
	if firstDigest == secondDigest {
		t.Fatal("runtime environment did not bind the job digest")
	}
}

func TestProjectDefaultsBudgetIncludesTransportEscaping(t *testing.T) {
	values := map[string]string{}
	for _, key := range []string{"A", "B", "C", "D", "E", "F", "G", "H"} {
		values[key] = "x" + strings.Repeat("<>&\u2028", 340) + "x"
	}
	spec := validJobSpec()
	spec.RepositoryReadOnly = false
	spec.Runtime = &JobRuntime{Version: 1, Environment: values}
	data, _ := json.Marshal(spec)
	if _, err := DecodeJobSpec(data); err != nil {
		t.Fatalf("project defaults below64KiB were refused: %v", err)
	}
	values["I"] = "x" + strings.Repeat("<>&\u2028", 340) + "x"
	data, _ = json.Marshal(spec)
	if _, err := DecodeJobSpec(data); err == nil {
		t.Fatal("project defaults above64KiB encoded must be refused before host transport")
	}
	spec.Runtime = nil
	spec.Environment = values
	data, _ = json.Marshal(spec)
	if _, err := DecodeJobSpec(data); err != nil {
		t.Fatalf("controller environment inherited the project-default limit: %v", err)
	}
}
