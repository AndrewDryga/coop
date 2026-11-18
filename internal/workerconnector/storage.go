package workerconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"

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
// failure — a measurement that could not be taken, an object that does not satisfy its own
// contract — because the hello's storage field is OPTIONAL and a measurement problem must never
// stop a worker from polling.
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

// LiveReports keeps the hello's two slow or constant reads off the poll path. The poll loop is one
// goroutine that also starts commands, delivers results and renews leases, and /v1/storage can
// wait up to the private API's one-minute timeout while the daemon re-measures the state tree or a
// job source is removed. So Storage never waits: it returns the last report and refreshes it in
// the background, one refresh at a time. Capabilities are a constant of the running daemon: once
// it answers, that answer holds for the connection.
type LiveReports struct {
	api API

	mu           sync.Mutex
	storage      *workerproto.Storage
	refreshing   bool
	capabilities []workerproto.Capability
}

func NewLiveReports(api API) *LiveReports { return &LiveReports{api: api} }

// Storage returns the last storage report, nil until the first one arrives, and starts a refresh
// unless one is already running. The refresh stops when ctx ends.
func (r *LiveReports) Storage(ctx context.Context) *workerproto.Storage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.refreshing {
		r.refreshing = true
		go func() {
			storage := LiveStorage(ctx, r.api)
			r.mu.Lock()
			defer r.mu.Unlock()
			r.refreshing = false
			if storage != nil || ctx.Err() == nil {
				r.storage = storage
			}
		}()
	}
	return r.storage
}

// Capabilities reads the daemon's capabilities until it answers once, then keeps that answer.
func (r *LiveReports) Capabilities(ctx context.Context) []workerproto.Capability {
	r.mu.Lock()
	cached := r.capabilities
	r.mu.Unlock()
	if cached != nil {
		return cached
	}
	capabilities, answered := readCapabilities(ctx, r.api)
	if answered {
		r.mu.Lock()
		r.capabilities = capabilities
		r.mu.Unlock()
	}
	return capabilities
}
