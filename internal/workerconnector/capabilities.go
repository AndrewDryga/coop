package workerconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

type sessionCapabilities struct {
	RepositoryFreshnessReceiptVersions []int `json:"repository_freshness_receipt_versions"`
	// Policies is the daemon's published per-policy network reach. The connector does not consume
	// it — a controller reads it from the API — but this decode is strict, so the field has to be
	// named here or a daemon that publishes one would look like a different document entirely.
	Policies map[string]json.RawMessage `json:"policies,omitempty"`
}

// LiveCapabilities adds implementation capabilities only after the exact local
// session daemon proves them. A connector restart must not speak for an older
// daemon that is still serving its Unix socket during a rolling upgrade.
func LiveCapabilities(ctx context.Context, api API, configured []workerproto.Capability) []workerproto.Capability {
	result := configuredCapabilities(configured)
	if api == nil {
		return result
	}

	raw, err := api.Do(ctx, Request{Method: "GET", Path: "/v1/capabilities"})
	if err != nil {
		return result
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document sessionCapabilities
	if decoder.Decode(&document) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		len(document.RepositoryFreshnessReceiptVersions) != 1 ||
		document.RepositoryFreshnessReceiptVersions[0] != 2 {
		return result
	}

	return append(result, workerproto.Capability{
		Name: repositoryFreshnessCapabilityName, Version: repositoryFreshnessCapabilityVersion,
	})
}
