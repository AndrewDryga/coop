package workerconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

type sessionCapabilities struct {
	JobSpecVersions                    []int `json:"job_spec_versions"`
	ControllerToolsVersions            []int `json:"controller_tools_versions"`
	RepositoryFreshnessReceiptVersions []int `json:"repository_freshness_receipt_versions"`
	// SessionEvidenceVersions proves the daemon serves the session evidence read this connector
	// forwards an api_request GET to. Omitted by an older daemon, which then advertises nothing.
	SessionEvidenceVersions []int `json:"session_evidence_versions,omitempty"`
}

// LiveCapabilities adds implementation capabilities only after the exact local
// session daemon proves them. A connector restart must not speak for an older
// daemon that is still serving its Unix socket during a rolling upgrade.
func LiveCapabilities(ctx context.Context, api API) []workerproto.Capability {
	result := []workerproto.Capability{}
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
	if proves(document.JobSpecVersions, 2) {
		result = append(result, workerproto.Capability{Name: "job-setup", Version: "2"})
	}
	if proves(document.RepositoryFreshnessReceiptVersions, 2) {
		result = append(result, workerproto.Capability{
			Name: repositoryFreshnessCapabilityName, Version: repositoryFreshnessCapabilityVersion,
		})
	}
	if proves(document.SessionEvidenceVersions, workerproto.SessionEvidenceVersion) {
		result = append(result, workerproto.Capability{
			Name: sessionEvidenceCapabilityName, Version: sessionEvidenceCapabilityVersion,
		})
	}
	if proves(document.ControllerToolsVersions, 1) {
		result = append(result, workerproto.Capability{Name: "controller-tools", Version: "1"})
	}
	return result
}

// proves accepts only the exact single version this connector speaks. A daemon publishing a
// different or additional version is a different contract, not a superset of this one.
func proves(published []int, version int) bool {
	return len(published) == 1 && published[0] == version
}
