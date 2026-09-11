package workerconnector

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

const liveStorageReport = `{"storage":{"version":1,"measured_at":"2026-09-11T04:05:06Z",` +
	`"capacity_bytes":536870912000,"free_bytes":107374182400,"reserve_bytes":26843545600,` +
	`"high_watermark_bytes":510027366400,"low_watermark_bytes":483183820800,` +
	`"disposable_bytes":8589934592,"protected_bytes":21474836480,"unattributed_bytes":null,` +
	`"allocation":"open","refusal_reason":null},` +
	`"budget":{"reserve_bytes":26843545600},"forks":[{"name":"remote-1"}]}`

// The daemon owns the measurement; the connector forwards it. Re-deriving any of these numbers in
// the connector would give the control plane a second, disagreeing account of the same disk.
func TestLiveStorageForwardsTheDaemonsOwnAccountingVerbatim(t *testing.T) {
	api := &capabilityAPI{response: json.RawMessage(liveStorageReport)}

	storage := LiveStorage(context.Background(), api)
	if storage == nil {
		t.Fatal("a measured daemon published no storage")
	}
	if err := storage.Validate(); err != nil {
		t.Fatalf("forwarded storage is invalid: %v", err)
	}
	if storage.DisposableBytes != 8589934592 || storage.ProtectedBytes != 21474836480 ||
		storage.Allocation != workerproto.StorageAllocationOpen {
		t.Fatalf("forwarded storage = %+v", storage)
	}
	if storage.UnattributedBytes != nil {
		t.Fatalf("an unknown unattributed total arrived as %d", *storage.UnattributedBytes)
	}
	if api.request.Method != "GET" || api.request.Path != "/v1/storage" || len(api.request.Body) != 0 {
		t.Fatalf("storage request = %+v", api.request)
	}
}

// The field is optional for a reason: a daemon too old to answer, a measurement that failed, or an
// object that does not satisfy its own contract must leave the poll exactly as it was.
func TestLiveStorageIsAbsentRatherThanWrong(t *testing.T) {
	for name, scripted := range map[string]*capabilityAPI{
		"no endpoint":      {err: errors.New("404 not_found")},
		"no measurement":   {response: json.RawMessage(`{"budget":{"reserve_bytes":1}}`)},
		"null measurement": {response: json.RawMessage(`{"storage":null}`)},
		"not json":         {response: json.RawMessage(`not json`)},
		"unknown field": {response: json.RawMessage(`{"storage":{"version":1,"measured_at":"2026-09-11T04:05:06Z",` +
			`"capacity_bytes":1,"free_bytes":1,"reserve_bytes":0,"high_watermark_bytes":2,"low_watermark_bytes":1,` +
			`"disposable_bytes":0,"protected_bytes":0,"unattributed_bytes":null,"allocation":"open",` +
			`"refusal_reason":null,"secret_path":"/home/me/.ssh"}}`)},
		"incoherent": {response: json.RawMessage(`{"storage":{"version":1,"measured_at":"2026-09-11T04:05:06Z",` +
			`"capacity_bytes":100,"free_bytes":200,"reserve_bytes":0,"high_watermark_bytes":90,` +
			`"low_watermark_bytes":80,"disposable_bytes":0,"protected_bytes":0,"unattributed_bytes":null,` +
			`"allocation":"open","refusal_reason":null}}`)},
	} {
		t.Run(name, func(t *testing.T) {
			if storage := LiveStorage(context.Background(), scripted); storage != nil {
				t.Fatalf("published %+v from an unusable report", storage)
			}
		})
	}
	if storage := LiveStorage(context.Background(), nil); storage != nil {
		t.Fatalf("published %+v without a daemon", storage)
	}
}
