package workerconnector

const (
	repositoryFreshnessCapabilityName    = "repository-freshness"
	repositoryFreshnessCapabilityVersion = "2"
	// Session evidence is the daemon's inspection export, read through an api_request GET.
	// It is advertised only on live proof so a controller can tell "this worker's build does not
	// export evidence" from "this session has no network run" — the two render differently.
	sessionEvidenceCapabilityName    = "session-evidence"
	sessionEvidenceCapabilityVersion = "1"
)
