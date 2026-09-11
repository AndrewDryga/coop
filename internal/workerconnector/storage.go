package workerconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// storageReport is only as much of the daemon's storage report as the wire contract needs. The
// envelope decode is TOLERANT — the owner-private report also carries a per-fork inventory and the
// configured budget, and that detail will keep growing — while the object inside is decoded
// strictly and validated, because that is the part a control plane reads. The connector forwards
// it verbatim and derives nothing: two accounts of the same disk would eventually disagree.
type storageReport struct {
	Storage json.RawMessage `json:"storage"`
}

// LiveStorage reads the local session daemon's own storage accounting. It returns nil for every
// failure — an older daemon with no such endpoint, a measurement that could not be taken, an
// object that does not satisfy its own contract — because the hello's storage field is OPTIONAL
// and a measurement problem must never stop a worker from polling.
func LiveStorage(ctx context.Context, api API) *workerproto.Storage {
	if api == nil {
		return nil
	}
	raw, err := api.Do(ctx, Request{Method: "GET", Path: "/v1/storage"})
	if err != nil {
		return nil
	}
	var report storageReport
	if json.Unmarshal(raw, &report) != nil || len(report.Storage) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(report.Storage))
	decoder.DisallowUnknownFields()
	var storage workerproto.Storage
	if decoder.Decode(&storage) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return nil
	}
	if storage.Validate() != nil {
		return nil
	}
	return &storage
}
