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
	// RepositorySourceSelectorVersions is the daemon's independent proof that it resolves the
	// generic source selector. A daemon that publishes freshness but not this one advertises
	// only freshness, so a partially upgraded fleet never receives selector-bound work.
	RepositorySourceSelectorVersions []int `json:"repository_source_selector_versions"`
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
	if decoder.Decode(&document) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return result
	}
	if proves(document.RepositoryFreshnessReceiptVersions, 2) {
		result = append(result, workerproto.Capability{
			Name: repositoryFreshnessCapabilityName, Version: repositoryFreshnessCapabilityVersion,
		})
	}
	if proves(document.RepositorySourceSelectorVersions, 1) {
		result = append(result, workerproto.Capability{
			Name: repositorySourceSelectorCapabilityName, Version: repositorySourceSelectorCapabilityVersion,
		})
	}
	return result
}

// proves accepts only the exact single version this connector speaks. A daemon publishing a
// different or additional version is a different contract, not a superset of this one.
func proves(published []int, version int) bool {
	return len(published) == 1 && published[0] == version
}
