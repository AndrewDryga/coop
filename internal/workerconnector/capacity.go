package workerconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// LiveCapacity forwards the daemon's admission accounting, never an optimistic
// connector default. A failed read leaves polling/cleanup available but offers no work.
func LiveCapacity(ctx context.Context, api API) workerproto.Capacity {
	unavailable := workerproto.Capacity{State: "busy"}
	if api == nil {
		return unavailable
	}
	raw, err := api.Do(ctx, Request{Method: "GET", Path: "/v1/capacity"})
	if err != nil {
		return unavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var capacity workerproto.Capacity
	if decoder.Decode(&capacity) != nil || decoder.Decode(&struct{}{}) != io.EOF || capacity.Validate() != nil {
		return unavailable
	}
	return capacity
}
