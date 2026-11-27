package workerconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

type sessionCapabilities struct {
	ServiceRelayVersions               []int `json:"service_relay_versions,omitempty"`
	FleetWorkspaceInputVersions        []int `json:"fleet_workspace_input_versions,omitempty"`
	ProjectServiceBudgetVersions       []int `json:"project_service_budget_versions,omitempty"`
	ModelProxyVersions                 []int `json:"model_proxy_versions,omitempty"`
	JobSpecVersions                    []int `json:"job_spec_versions"`
	ProjectRuntimeVersions             []int `json:"project_runtime_versions,omitempty"`
	JobPresetVersions                  []int `json:"job_preset_versions,omitempty"`
	ControllerToolsVersions            []int `json:"controller_tools_versions"`
	RepositoryFreshnessReceiptVersions []int `json:"repository_freshness_receipt_versions"`
	// SessionEvidenceVersions proves the daemon serves the session evidence read this connector
	// forwards an api_request GET to.
	SessionEvidenceVersions []int `json:"session_evidence_versions,omitempty"`
	// ReviewCommitMessageVersions proves a review accepts the controller's commit message.
	ReviewCommitMessageVersions   []int `json:"review_commit_message_versions,omitempty"`
	ExistingPRPublicationVersions []int `json:"existing_pr_publication_versions,omitempty"`
	// CheckpointUpstreamVersions proves restore reads a checkpoint's upstream_revision.
	CheckpointUpstreamVersions []int `json:"checkpoint_upstream_versions,omitempty"`
	// FastTargetVersions proves a job target may carry +fast.
	FastTargetVersions []int `json:"fast_target_versions,omitempty"`
}

// LiveCapabilities adds implementation capabilities only after the exact local
// session daemon proves them, so a hello never claims more than the daemon answers.
func LiveCapabilities(ctx context.Context, api API) []workerproto.Capability {
	result, _ := readCapabilities(ctx, api)
	return result
}

// readCapabilities is LiveCapabilities that also says whether the daemon answered, so a caller
// may keep a proven answer and ask again after a failed one.
func readCapabilities(ctx context.Context, api API) ([]workerproto.Capability, bool) {
	result := []workerproto.Capability{}
	if api == nil {
		return result, false
	}

	raw, err := api.Do(ctx, Request{Method: "GET", Path: "/v1/capabilities"})
	if err != nil {
		return result, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document sessionCapabilities
	if decoder.Decode(&document) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return result, false
	}
	if proves(document.ServiceRelayVersions, 1) {
		result = append(result, workerproto.Capability{Name: "service-relay", Version: "1"})
	}
	if proves(document.FleetWorkspaceInputVersions, 1) {
		result = append(result, workerproto.Capability{Name: "fleet-workspace-input", Version: "1"})
	}
	if proves(document.ModelProxyVersions, 1) {
		result = append(result, workerproto.Capability{Name: "model-proxy", Version: "1"})
	}
	if proves(document.ProjectServiceBudgetVersions, 1) {
		result = append(result, workerproto.Capability{Name: "project-service-budget", Version: "1"})
	}
	if proves(document.ProjectRuntimeVersions, 1) {
		result = append(result, workerproto.Capability{Name: "project-runtime", Version: "1"})
	}
	if proves(document.JobPresetVersions, 1) {
		result = append(result, workerproto.Capability{Name: "job-preset", Version: "1"})
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
	if proves(document.ReviewCommitMessageVersions, 1) {
		result = append(result, workerproto.Capability{Name: "review-commit-message", Version: "1"})
	}
	if proves(document.ExistingPRPublicationVersions, 1) {
		result = append(result, workerproto.Capability{Name: "existing-pr-publication", Version: "1"})
	}
	if proves(document.CheckpointUpstreamVersions, 1) {
		result = append(result, workerproto.Capability{Name: "checkpoint-upstream", Version: "1"})
	}
	if proves(document.FastTargetVersions, 1) {
		result = append(result, workerproto.Capability{Name: "fast-target", Version: "1"})
	}
	return result, true
}

// proves accepts only the exact single version this connector speaks. A daemon publishing a
// different or additional version is a different contract, not a superset of this one.
func proves(published []int, version int) bool {
	return len(published) == 1 && published[0] == version
}
