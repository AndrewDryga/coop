package workerconnector

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestLiveCapacityUsesOnlyMeasuredDaemonSlots(t *testing.T) {
	want := workerproto.Capacity{State: "eligible", SessionSlotsTotal: 4, SessionSlotsFree: 2,
		TurnSlotsTotal: 4, TurnSlotsFree: 2, WorkspaceSlotsTotal: 4, WorkspaceSlotsFree: 2}
	document, _ := json.Marshal(want)
	api := &capabilityAPI{response: document}
	if got := LiveCapacity(context.Background(), api); got != want {
		t.Fatalf("capacity=%+v, want %+v", got, want)
	}
	if api.request.Method != "GET" || api.request.Path != "/v1/capacity" {
		t.Fatalf("request=%+v", api.request)
	}
	for _, api := range []*capabilityAPI{
		{err: errors.New("unavailable")},
		{response: []byte(`{}`)},
		{response: []byte(`null`)},
		{response: append(append([]byte(nil), document...), []byte(` {}`)...)},
		{response: []byte(`{"state":"eligible","session_slots_free":5,"session_slots_total":4}`)},
		{response: []byte(`{"state":"eligible","unknown":true}`)},
	} {
		if got := LiveCapacity(context.Background(), api); got != (workerproto.Capacity{State: "busy"}) {
			t.Fatalf("unusable measurement advertised capacity: %+v", got)
		}
	}
	if got := LiveCapacity(context.Background(), nil); got.State != "busy" || got.TurnSlotsFree != 0 {
		t.Fatalf("missing daemon advertised capacity: %+v", got)
	}
}
